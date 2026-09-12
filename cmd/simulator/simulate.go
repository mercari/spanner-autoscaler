/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

// commonFlags are the simulation parameters shared by simulate and compare.
type commonFlags struct {
	metricsPath       string
	initialPU         int
	scaleUpInterval   time.Duration
	scaleDownInterval time.Duration
	lowConfidenceCPU  float64
	nodeHourPrice     float64
	format            string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.metricsPath, "metrics", "", "path to the metrics CSV (required; produce one with 'fetch')")
	fs.IntVar(&c.initialPU, "initial-pu", 0, "processing units the simulation starts with (default: first recorded value)")
	fs.DurationVar(&c.scaleUpInterval, "scale-up-interval", simulator.DefaultScaleUpInterval, "controller-level default scale-up cooldown (overridden by spec.scaleConfig.scaleupInterval)")
	fs.DurationVar(&c.scaleDownInterval, "scale-down-interval", simulator.DefaultScaleDownInterval, "controller-level default scale-down cooldown (overridden by spec.scaleConfig.scaledownInterval)")
	fs.Float64Var(&c.lowConfidenceCPU, "low-confidence-cpu", simulator.DefaultLowConfidenceCPU, "recorded CPU %% above which the workload model is flagged as unreliable")
	fs.Float64Var(&c.nodeHourPrice, "node-hour-price", 0, "price per 1000 PU per hour; when > 0, cost figures are printed alongside PU-hours")
	fs.StringVar(&c.format, "format", "text", "output format: text or json")
}

func (c *commonFlags) loadPoints() ([]simulator.Point, error) {
	if c.metricsPath == "" {
		return nil, fmt.Errorf("-metrics is required")
	}
	f, err := os.Open(c.metricsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return simulator.LoadCSV(f)
}

func (c *commonFlags) simulate(configPath string, points []simulator.Point) (*simulator.Result, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	sa, schedules, err := simulator.LoadManifests(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	result, err := simulator.Run(simulator.Config{
		Autoscaler:        sa,
		Schedules:         schedules,
		InitialPU:         c.initialPU,
		ScaleUpInterval:   c.scaleUpInterval,
		ScaleDownInterval: c.scaleDownInterval,
		LowConfidenceCPU:  c.lowConfidenceCPU,
	}, points)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	return result, nil
}

func (c *commonFlags) cost(puHours float64) float64 {
	return puHours / 1000 * c.nodeHourPrice
}

// configTargets re-reads the manifest for the CPU targets so the HTML report
// can draw them as reference lines.
func configTargets(configPath string) (targetHigh, targetTotal int, err error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return 0, 0, err
	}
	sa, _, err := simulator.LoadManifests(data)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", configPath, err)
	}
	if t := sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority; t != nil {
		targetHigh = *t
	}
	if t := sa.Spec.ScaleConfig.TargetCPUUtilization.Total; t != nil {
		targetTotal = *t
	}
	return targetHigh, targetTotal, nil
}

func runSimulate(args []string) error {
	fs := flag.NewFlagSet("simulate", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	configPath := fs.String("config", "", "path to YAML manifests holding one SpannerAutoscaler and its SpannerAutoscaleSchedules (required)")
	pointsCSV := fs.String("points-csv", "", "also write the per-tick recorded-vs-simulated series to this CSV path")
	htmlPath := fs.String("html", "", "also write a self-contained HTML report with charts to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("-config is required")
	}

	points, err := common.loadPoints()
	if err != nil {
		return err
	}
	result, err := common.simulate(*configPath, points)
	if err != nil {
		return err
	}

	if *pointsCSV != "" {
		f, err := os.Create(*pointsCSV)
		if err != nil {
			return err
		}
		if err := result.WritePointsCSV(f); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	if *htmlPath != "" {
		targetHigh, targetTotal, err := configTargets(*configPath)
		if err != nil {
			return err
		}
		f, err := os.Create(*htmlPath)
		if err != nil {
			return err
		}
		if err := writeSimulateHTML(f, *configPath, result, targetHigh, targetTotal); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote HTML report to %s\n", *htmlPath)
	}

	switch common.format {
	case "json":
		// Points are omitted here to keep the report small; use -points-csv
		// for the full series.
		return json.NewEncoder(os.Stdout).Encode(struct {
			Config  string            `json:"config"`
			Summary simulator.Summary `json:"summary"`
			Events  []simulator.Event `json:"events"`
		}{*configPath, result.Summary, result.Events})
	case "text":
		writeTextReport(os.Stdout, *configPath, result, &common)
		return nil
	default:
		return fmt.Errorf("unknown format %q", common.format)
	}
}

func writeTextReport(w io.Writer, name string, result *simulator.Result, common *commonFlags) {
	s := result.Summary
	fmt.Fprintf(w, "Simulation of %s\n", name)
	fmt.Fprintf(w, "  period:            %s .. %s (%d points, %.0f gap minutes)\n",
		s.Start.Format(time.RFC3339), s.End.Format(time.RFC3339), s.DataPoints, s.GapMinutes)
	fmt.Fprintf(w, "  PU-hours:          recorded %.1f -> simulated %.1f (%.1f%% saved)\n",
		s.ActualPUHours, s.SimPUHours, s.PUHoursSavedPercent)
	if common.nodeHourPrice > 0 {
		fmt.Fprintf(w, "  cost:              recorded %.2f -> simulated %.2f (price %.4f per 1000 PU-hour)\n",
			common.cost(s.ActualPUHours), common.cost(s.SimPUHours), common.nodeHourPrice)
	}
	fmt.Fprintf(w, "  scale events:      %d up / %d down\n", s.ScaleUps, s.ScaleDowns)
	fmt.Fprintf(w, "  PU-change guide:   %d steps beyond 2x/half, %d gaps < 10m, %d gaps < 30m\n",
		s.ScaleStepViolations, s.ScaleGapsUnder10Min, s.ScaleGapsUnder30Min)
	if s.SimHighPriorityCPU != nil {
		fmt.Fprintf(w, "  sim high-pri CPU:  %s\n", formatCPUStats(s.SimHighPriorityCPU))
	}
	if s.SimTotalCPU != nil {
		fmt.Fprintf(w, "  sim total CPU:     %s\n", formatCPUStats(s.SimTotalCPU))
	}
	days := s.End.Sub(s.Start).Hours() / 24
	perDay := ""
	if days >= 1 {
		perDay = fmt.Sprintf(" (%.0f/day)", s.TargetExceededMinutes/days)
	}
	fmt.Fprintf(w, "  above target:      %.0f minutes%s\n", s.TargetExceededMinutes, perDay)
	fmt.Fprintf(w, "  low confidence:    %.0f minutes (recorded CPU >= %.0f%%)\n", s.LowConfidenceMinutes, common.lowConfidenceCPU)
	fmt.Fprintf(w, "  pinned at min PU:  %.0f minutes (%.0f%% of the run)\n", s.MinPinnedMinutes, s.MinPinnedPercent)
	assessment := s.AssessMinPU()
	fmt.Fprintf(w, "  min PU (lower?):   %s\n", assessment.Lower)
	fmt.Fprintf(w, "  min PU (raise?):   %s\n", assessment.Raise)
	if len(result.Events) > 0 {
		fmt.Fprintf(w, "  first events:\n")
		for i, e := range result.Events {
			if i == 10 {
				fmt.Fprintf(w, "    ... %d more (use -format json or -points-csv for the full list)\n", len(result.Events)-i)
				break
			}
			fmt.Fprintf(w, "    %s  %6d -> %6d\n", e.Time.Format(time.RFC3339), e.FromPU, e.ToPU)
		}
	}
}

func formatCPUStats(s *simulator.CPUStats) string {
	return fmt.Sprintf("mean %.1f%%  p50 %.1f%%  p95 %.1f%%  p99 %.1f%%  max %.1f%%", s.Mean, s.P50, s.P95, s.P99, s.Max)
}

// stringSlice is a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string { return fmt.Sprint(*s) }

func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func runCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	var configPaths stringSlice
	fs.Var(&configPaths, "config", "path to a configuration to replay; repeat the flag to compare several")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(configPaths) == 0 {
		return fmt.Errorf("at least one -config is required")
	}

	points, err := common.loadPoints()
	if err != nil {
		return err
	}

	type row struct {
		Config  string            `json:"config"`
		Summary simulator.Summary `json:"summary"`
	}
	rows := make([]row, 0, len(configPaths))
	for _, path := range configPaths {
		result, err := common.simulate(path, points)
		if err != nil {
			return err
		}
		rows = append(rows, row{Config: path, Summary: result.Summary})
	}

	switch common.format {
	case "json":
		return json.NewEncoder(os.Stdout).Encode(rows)
	case "text":
		tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "CONFIG\tPU-HOURS\tSAVED%\tUPS\tDOWNS\tP95 HI-CPU\tP95 TOTAL-CPU\t>TARGET MIN\tLOW-CONF MIN")
		fmt.Fprintf(tw, "(recorded)\t%.1f\t\t\t\t\t\t\t\n", rows[0].Summary.ActualPUHours)
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%.1f\t%.1f\t%d\t%d\t%s\t%s\t%.0f\t%.0f\n",
				r.Config,
				r.Summary.SimPUHours,
				r.Summary.PUHoursSavedPercent,
				r.Summary.ScaleUps,
				r.Summary.ScaleDowns,
				formatP95(r.Summary.SimHighPriorityCPU),
				formatP95(r.Summary.SimTotalCPU),
				r.Summary.TargetExceededMinutes,
				r.Summary.LowConfidenceMinutes,
			)
		}
		return tw.Flush()
	default:
		return fmt.Errorf("unknown format %q", common.format)
	}
}

func formatP95(s *simulator.CPUStats) string {
	if s == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", s.P95)
}
