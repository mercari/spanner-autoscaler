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
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

func runRecommend(args []string) error {
	fs := flag.NewFlagSet("recommend", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	configPath := fs.String("config", "", "path to the base YAML manifests (required); candidates are derived from it")
	minPUs := fs.String("min-pu", "", "comma-separated candidate values for spec.scaleConfig.processingUnits.min (e.g. \"2000,5000,10000\")")
	stepSizes := fs.String("scaledown-step-size", "", "comma-separated candidate scaledownStepSize values, int or percent (e.g. \"2000,10%,30%\")")
	intervals := fs.String("scaledown-interval", "", "comma-separated candidate scaledownInterval values (e.g. \"30m,55m\")")
	windows := fs.String("scaledown-allowed-times", "", "'|'-separated candidate scaledownAllowedTimes; use ';' between cron expressions inside one candidate and the literal \"none\" for no restriction (e.g. \"* 10-23 * * *|* 13-23 * * *|none\")")
	highTargets := fs.String("target-high-cpu", "", "comma-separated candidate targetCPUUtilization.highPriority values (e.g. \"30,40\")")
	totalTargets := fs.String("target-total-cpu", "", "comma-separated candidate targetCPUUtilization.total values")
	maxExceeded := fs.Float64("max-exceeded-minutes", 0, "constraint: maximum minutes the simulated CPU may spend above its target")
	maxP99 := fs.Float64("max-p99-cpu", 0, "constraint: maximum allowed p99 of every simulated CPU metric in percent (0 = no cap)")
	top := fs.Int("top", 10, "number of candidates to print (text format)")
	showInfeasible := fs.Bool("show-infeasible", false, "also list candidates that violate the constraints (text format)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("-config is required")
	}

	space, err := buildSearchSpace(*minPUs, *stepSizes, *intervals, *windows, *highTargets, *totalTargets)
	if err != nil {
		return err
	}

	constraints := simulator.Constraints{MaxTargetExceededMinutes: *maxExceeded}
	if *maxP99 > 0 {
		constraints.MaxSimCPUP99 = maxP99
	}

	points, err := common.loadPoints()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	sa, schedules, err := simulator.LoadManifests(data)
	if err != nil {
		return fmt.Errorf("%s: %w", *configPath, err)
	}
	base := simulator.Config{
		Autoscaler:        sa,
		Schedules:         schedules,
		InitialPU:         common.initialPU,
		ScaleUpInterval:   common.scaleUpInterval,
		ScaleDownInterval: common.scaleDownInterval,
		LowConfidenceCPU:  common.lowConfidenceCPU,
	}

	// The unmodified base config is the reference row for the ranking.
	baseResult, err := simulator.Run(base, points)
	if err != nil {
		return fmt.Errorf("%s: %w", *configPath, err)
	}

	candidates, err := simulator.Recommend(base, space, constraints, points)
	if err != nil {
		return err
	}

	switch common.format {
	case "json":
		return json.NewEncoder(os.Stdout).Encode(struct {
			Base       simulator.Summary     `json:"base"`
			Candidates []simulator.Candidate `json:"candidates"`
		}{baseResult.Summary, candidates})
	case "text":
		writeRecommendTable(baseResult.Summary, candidates, &common, *top, *showInfeasible)
		return nil
	default:
		return fmt.Errorf("unknown format %q", common.format)
	}
}

func buildSearchSpace(minPUs, stepSizes, intervals, windows, highTargets, totalTargets string) (simulator.SearchSpace, error) {
	var space simulator.SearchSpace
	var err error

	if space.MinPUs, err = parseIntList(minPUs); err != nil {
		return space, fmt.Errorf("-min-pu: %w", err)
	}
	for part := range splitList(stepSizes, ",") {
		space.ScaledownStepSizes = append(space.ScaledownStepSizes, intstr.Parse(part))
	}
	if space.ScaledownIntervals, err = simulator.ParseScaledownIntervals(intervals); err != nil {
		return space, fmt.Errorf("-scaledown-interval: %w", err)
	}
	for alternative := range splitList(windows, "|") {
		if alternative == "none" {
			space.ScaledownAllowedTimes = append(space.ScaledownAllowedTimes, nil)
			continue
		}
		var exprs []string
		for expr := range splitList(alternative, ";") {
			exprs = append(exprs, expr)
		}
		space.ScaledownAllowedTimes = append(space.ScaledownAllowedTimes, exprs)
	}
	if space.TargetHighPriorityCPUs, err = parseIntList(highTargets); err != nil {
		return space, fmt.Errorf("-target-high-cpu: %w", err)
	}
	if space.TargetTotalCPUs, err = parseIntList(totalTargets); err != nil {
		return space, fmt.Errorf("-target-total-cpu: %w", err)
	}
	return space, nil
}

// splitList yields the trimmed non-empty elements of s separated by sep.
func splitList(s, sep string) func(func(string) bool) {
	return func(yield func(string) bool) {
		if s == "" {
			return
		}
		for part := range strings.SplitSeq(s, sep) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !yield(part) {
				return
			}
		}
	}
}

func parseIntList(s string) ([]int, error) {
	var out []int
	for part := range splitList(s, ",") {
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", part)
		}
		out = append(out, v)
	}
	return out, nil
}

func writeRecommendTable(base simulator.Summary, candidates []simulator.Candidate, common *commonFlags, top int, showInfeasible bool) {
	feasibleCount := 0
	for _, c := range candidates {
		if c.Feasible {
			feasibleCount++
		}
	}
	fmt.Printf("Evaluated %d candidates (%d feasible) against %d recorded points\n\n",
		len(candidates), feasibleCount, base.DataPoints)

	tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tOVERRIDES\tPU-HOURS\tSAVED%\tP95 HI-CPU\tP99 HI-CPU\t>TARGET MIN\tUPS\tDOWNS")
	fmt.Fprintf(tw, "-\t(recorded)\t%.1f\t\t\t\t\t\t\n", base.ActualPUHours)
	fmt.Fprintf(tw, "-\t(base)\t%.1f\t%.1f\t%s\t%s\t%.0f\t%d\t%d\n",
		base.SimPUHours, base.PUHoursSavedPercent,
		formatP95(base.SimHighPriorityCPU), formatP99(base.SimHighPriorityCPU),
		base.TargetExceededMinutes, base.ScaleUps, base.ScaleDowns)

	rank := 0
	for _, c := range candidates {
		if !c.Feasible && !showInfeasible {
			continue
		}
		rank++
		if rank > top {
			fmt.Fprintf(tw, "\t... %d more candidates (raise -top or use -format json)\t\t\t\t\t\t\t\n", len(candidates)-rank+1)
			break
		}
		label := simulator.DescribeOverrides(c.Overrides)
		if c.Error != "" {
			fmt.Fprintf(tw, "%d\t%s\tERROR: %s\t\t\t\t\t\t\n", rank, label, c.Error)
			continue
		}
		marker := ""
		if !c.Feasible {
			marker = " [infeasible]"
		}
		fmt.Fprintf(tw, "%d\t%s%s\t%.1f\t%.1f\t%s\t%s\t%.0f\t%d\t%d\n",
			rank, label, marker,
			c.Summary.SimPUHours, c.Summary.PUHoursSavedPercent,
			formatP95(c.Summary.SimHighPriorityCPU), formatP99(c.Summary.SimHighPriorityCPU),
			c.Summary.TargetExceededMinutes, c.Summary.ScaleUps, c.Summary.ScaleDowns)
	}
	tw.Flush()

	if base.LowConfidenceMinutes > 0 {
		fmt.Printf("\nnote: %.0f minutes of the recording are above %.0f%% CPU; the workload model is less reliable there (see -low-confidence-cpu)\n",
			base.LowConfidenceMinutes, common.lowConfidenceCPU)
	}
}

func formatP99(s *simulator.CPUStats) string {
	if s == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", s.P99)
}
