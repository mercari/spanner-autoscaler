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
	"slices"
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
	scaleupStepSizes := fs.String("scaleup-step-size", "", "comma-separated candidate scaleupStepSize values, int or percent; 0 means no per-step cap (e.g. \"0,5000,20%\")")
	scaleupIntervals := fs.String("scaleup-interval", "", "comma-separated candidate scaleupInterval values (e.g. \"60s,3m\")")
	highTargets := fs.String("target-high-cpu", "", "comma-separated candidate targetCPUUtilization.highPriority values (e.g. \"30,40\")")
	totalTargets := fs.String("target-total-cpu", "", "comma-separated candidate targetCPUUtilization.total values")
	maxExceeded := fs.Float64("max-exceeded-minutes", 0, "constraint: maximum minutes the simulated CPU may spend above its target")
	maxP99 := fs.Float64("max-p99-cpu", 0, "constraint: maximum allowed p99 of every simulated CPU metric in percent (0 = no cap)")
	instanceConfig := fs.String("instance-config", "regional", "instance configuration deciding the Google-recommended high-priority CPU ceiling: regional (65%), multi-region (45% per region), or none to disable the guideline")
	puChangeGuideline := fs.String("pu-change-guideline", "base", "how to enforce the recommended PU-change limits (at most 2x/half per operation, >=10m between operations): base = no worse than the base config's replay, strict = zero violations, none = unconstrained")
	auto := fs.Bool("auto", false, "auto-generate candidates for the search dimensions left empty: -min-pu from the recorded workload's required-PU percentiles, and step sizes / intervals within the PU-change guideline")
	maxChanges := fs.Int("max-changes", 1, "recommend a candidate that changes at most this many parameters at once (0 = no limit); the best multi-change candidate still appears as a further option")
	savingsTolerance := fs.Float64("savings-tolerance", 2.0, "treat feasible candidates whose savings are within this many percentage points of the best as equal on cost and recommend the least risky of them (gentlest scale-down first); a candidate that saves more than this appears as a further option (0 always recommends the cheapest)")
	top := fs.Int("top", 6, "number of candidates to print (text format)")
	showInfeasible := fs.Bool("show-infeasible", false, "also list candidates that violate the constraints (text format)")
	htmlPath := fs.String("html", "", "also write a self-contained HTML report (savings-vs-risk scatter and candidate table) to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return fmt.Errorf("-config is required")
	}

	space, err := buildSearchSpace(searchSpaceFlags{
		minPUs:           *minPUs,
		stepSizes:        *stepSizes,
		intervals:        *intervals,
		windows:          *windows,
		scaleupStepSizes: *scaleupStepSizes,
		scaleupIntervals: *scaleupIntervals,
		highTargets:      *highTargets,
		totalTargets:     *totalTargets,
	})
	if err != nil {
		return err
	}

	constraints := simulator.Constraints{MaxTargetExceededMinutes: *maxExceeded}
	if *maxP99 > 0 {
		constraints.MaxSimCPUP99 = maxP99
	}
	switch *instanceConfig {
	case "regional":
		constraints.MaxHighPriorityCPU = simulator.RecommendedHighPriorityCPURegional
	case "multi-region":
		constraints.MaxHighPriorityCPU = simulator.RecommendedHighPriorityCPUMultiRegion
	case "none":
		// Guideline disabled.
	default:
		return fmt.Errorf("unknown -instance-config %q (want regional, multi-region, or none)", *instanceConfig)
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

	if *auto {
		space.FillGuidelineStepCandidates(sa, common.scaleUpInterval, common.scaleDownInterval)
		if len(space.MinPUs) == 0 {
			space.MinPUs = simulator.MinPUCandidates(sa, points)
			fmt.Fprintf(os.Stderr, "auto-generated -min-pu candidates from the workload's required-PU percentiles: %v\n", space.MinPUs)
		}
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

	zero := 0
	switch *puChangeGuideline {
	case "base":
		constraints.MaxScaleStepViolations = &baseResult.Summary.ScaleStepViolations
		constraints.MaxShortScaleGaps = &baseResult.Summary.ScaleGapsUnder10Min
	case "strict":
		constraints.MaxScaleStepViolations = &zero
		constraints.MaxShortScaleGaps = &zero
	case "none":
		// Guideline disabled.
	default:
		return fmt.Errorf("unknown -pu-change-guideline %q (want base, strict, or none)", *puChangeGuideline)
	}

	candidates, err := simulator.Recommend(base, space, constraints, points)
	if err != nil {
		return err
	}

	current := simulator.CurrentParameterValues(sa, common.scaleUpInterval, common.scaleDownInterval)
	displayCurrent := simulator.CurrentParameterDisplay(sa, common.scaleUpInterval, common.scaleDownInterval)

	if *htmlPath != "" {
		// Re-run the recommended candidate to chart its full time series in
		// the report — the aggregate numbers alone do not show how the PU and
		// CPU would have moved under the recommended configuration.
		var topResult *simulator.Result
		var topHigh, topTotal int
		if idx, _, _ := simulator.RecommendedIndex(baseResult.Summary, current, candidates, *savingsTolerance, *maxChanges); idx >= 0 && candidates[idx].Autoscaler != nil {
			best := &candidates[idx]
			topConfig := base
			topConfig.Autoscaler = best.Autoscaler
			if topResult, err = simulator.Run(topConfig, points); err != nil {
				return err
			}
			if t := best.Autoscaler.Spec.ScaleConfig.TargetCPUUtilization.HighPriority; t != nil {
				topHigh = *t
			}
			if t := best.Autoscaler.Spec.ScaleConfig.TargetCPUUtilization.Total; t != nil {
				topTotal = *t
			}
		}

		f, err := os.Create(*htmlPath)
		if err != nil {
			return err
		}
		if err := writeRecommendHTML(f, current, displayCurrent, baseResult.Summary, candidates, *savingsTolerance, *maxChanges, topResult, topHigh, topTotal); err != nil {
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
		return json.NewEncoder(os.Stdout).Encode(struct {
			Base       simulator.Summary     `json:"base"`
			Candidates []simulator.Candidate `json:"candidates"`
		}{baseResult.Summary, candidates})
	case "text":
		if constraints.MaxHighPriorityCPU > 0 {
			fmt.Printf("guideline: high-priority CPU target and simulated p99 must stay <= %d%% (%s instance configuration)\n",
				constraints.MaxHighPriorityCPU, *instanceConfig)
		}
		if constraints.MaxScaleStepViolations != nil {
			fmt.Printf("guideline: at most %d scale events beyond 2x/half and %d gaps < 10m allowed (-pu-change-guideline %s)\n",
				*constraints.MaxScaleStepViolations, *constraints.MaxShortScaleGaps, *puChangeGuideline)
		} else {
			fmt.Println("guideline: PU-change pacing checks are disabled (-pu-change-guideline none); check the STEP>2X and GAP<10M columns before adopting a candidate")
		}
		writeRecommendTable(current, displayCurrent, baseResult.Summary, candidates, *savingsTolerance, *maxChanges, &common, *top, *showInfeasible)
		return nil
	default:
		return fmt.Errorf("unknown format %q", common.format)
	}
}

// searchSpaceFlags carries the raw CLI list values for buildSearchSpace.
type searchSpaceFlags struct {
	minPUs           string
	stepSizes        string
	intervals        string
	windows          string
	scaleupStepSizes string
	scaleupIntervals string
	highTargets      string
	totalTargets     string
}

func buildSearchSpace(flags searchSpaceFlags) (simulator.SearchSpace, error) {
	var space simulator.SearchSpace
	var err error

	if space.MinPUs, err = parseIntList(flags.minPUs); err != nil {
		return space, fmt.Errorf("-min-pu: %w", err)
	}
	for part := range splitList(flags.stepSizes, ",") {
		space.ScaledownStepSizes = append(space.ScaledownStepSizes, intstr.Parse(part))
	}
	if space.ScaledownIntervals, err = simulator.ParseDurations(flags.intervals); err != nil {
		return space, fmt.Errorf("-scaledown-interval: %w", err)
	}
	for alternative := range splitList(flags.windows, "|") {
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
	for part := range splitList(flags.scaleupStepSizes, ",") {
		space.ScaleupStepSizes = append(space.ScaleupStepSizes, intstr.Parse(part))
	}
	if space.ScaleupIntervals, err = simulator.ParseDurations(flags.scaleupIntervals); err != nil {
		return space, fmt.Errorf("-scaleup-interval: %w", err)
	}
	if space.TargetHighPriorityCPUs, err = parseIntList(flags.highTargets); err != nil {
		return space, fmt.Errorf("-target-high-cpu: %w", err)
	}
	if space.TargetTotalCPUs, err = parseIntList(flags.totalTargets); err != nil {
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

// recommendationLines renders the conclusion: for the recommended candidate,
// every parameter as "current → recommended" (unchanged parameters marked (keep), so the
// min PU decision is always shown), followed by the effect of adopting it.
// current is compared on effective values while displayCurrent carries the
// human-readable form (unset intervals as "controller default (...)").
// A nil top yields keepReason as the conclusion.
func recommendationLines(current, displayCurrent map[string]string, base simulator.Summary, top *simulator.Candidate, keepReason string) []string {
	if top == nil {
		return []string{keepReason}
	}
	var lines []string
	for _, key := range simulator.OverrideKeys {
		cur, ok := current[key]
		if !ok {
			continue
		}
		if next, changed := top.Overrides[key]; changed && next != cur {
			lines = append(lines, fmt.Sprintf("%s: %s -> %s", key, displayCurrent[key], next))
		} else {
			lines = append(lines, fmt.Sprintf("%s: %s (keep)", key, displayCurrent[key]))
		}
	}
	s := top.Summary
	lines = append(lines, fmt.Sprintf("effect: saves %.1f%% PU-hours (%+.1f vs current), above target %.0fm (%+.0f), gaps<10m %d (%+d)",
		s.PUHoursSavedPercent, s.PUHoursSavedPercent-base.PUHoursSavedPercent,
		s.TargetExceededMinutes, s.TargetExceededMinutes-base.TargetExceededMinutes,
		s.ScaleGapsUnder10Min, s.ScaleGapsUnder10Min-base.ScaleGapsUnder10Min))
	return lines
}

// groupRank returns the 1-based position of idx's outcome group, matching the
// RANK column of the candidate table.
func groupRank(candidates []simulator.Candidate, idx int) int {
	for rank, g := range simulator.GroupEquivalent(candidates) {
		if slices.Contains(g, idx) {
			return rank + 1
		}
	}
	return 0
}

// furtherOptionLine describes the cheapest candidate that was passed over for
// a safer or smaller change.
func furtherOptionLine(current map[string]string, recommended, further simulator.Candidate, rank int) string {
	return fmt.Sprintf("further option (rank %d): %s — saves %+.1f pt more with %+.0f min more above target; consider it after the recommendation has proven out",
		rank, simulator.DescribeChanges(current, further.Overrides),
		further.Summary.PUHoursSavedPercent-recommended.Summary.PUHoursSavedPercent,
		further.Summary.TargetExceededMinutes-recommended.Summary.TargetExceededMinutes)
}

func writeRecommendTable(current, displayCurrent map[string]string, base simulator.Summary, candidates []simulator.Candidate, savingsTolerance float64, maxChanges int, common *commonFlags, top int, showInfeasible bool) {
	feasibleCount := 0
	for _, c := range candidates {
		if c.Feasible {
			feasibleCount++
		}
	}
	fmt.Printf("Evaluated %d candidates (%d feasible) against %d recorded points\n",
		len(candidates), feasibleCount, base.DataPoints)
	if feasibleCount == 0 && !showInfeasible {
		// When nothing passes the constraints, show the cheapest rejected
		// candidates and their reasons so the constraints can be revisited.
		showInfeasible = true
		fmt.Println("no candidate satisfies the constraints — showing the cheapest infeasible ones with the reasons they were rejected")
	}
	fmt.Println()

	tw := tabwriter.NewWriter(os.Stdout, 2, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "RANK\tOVERRIDES\tPU-HOURS\tSAVED%\tP95 HI-CPU\tP99 HI-CPU\t>TARGET MIN\tUPS\tDOWNS\tSTEP>2X\tGAP<10M")
	fmt.Fprintf(tw, "-\t(recorded)\t%.1f\t\t\t\t\t\t\t\t\n", base.ActualPUHours)
	fmt.Fprintf(tw, "-\t(base)\t%.1f\t%.1f\t%s\t%s\t%.0f\t%d\t%d\t%d\t%d\n",
		base.SimPUHours, base.PUHoursSavedPercent,
		formatP95(base.SimHighPriorityCPU), formatP99(base.SimHighPriorityCPU),
		base.TargetExceededMinutes, base.ScaleUps, base.ScaleDowns,
		base.ScaleStepViolations, base.ScaleGapsUnder10Min)

	type rejected struct {
		rank    int
		reasons []string
	}
	var rejections []rejected

	recIdx, furtherIdx, keepReason := simulator.RecommendedIndex(base, current, candidates, savingsTolerance, maxChanges)

	// One row per distinct outcome: grids routinely contain parameter
	// combinations that behave identically on the recording, and repeating
	// them hides the rows worth reading. Ranks are group positions, so they
	// stay stable whether or not infeasible rows are shown.
	groups := simulator.GroupEquivalent(candidates)
	shown := 0
	for rank, g := range groups {
		rep := g[0]
		if slices.Contains(g, recIdx) {
			rep = recIdx
		}
		c := candidates[rep]
		if !c.Feasible && !showInfeasible {
			continue
		}
		shown++
		if shown > top {
			fmt.Fprintf(tw, "\t... %d more distinct outcomes (raise -top or use -format json)\t\t\t\t\t\t\t\t\t\n", len(groups)-rank)
			break
		}
		label := simulator.DescribeChanges(current, c.Overrides)
		if len(g) > 1 {
			label += fmt.Sprintf(" (+%d equivalent)", len(g)-1)
		}
		if c.Error != "" {
			fmt.Fprintf(tw, "%d\t%s\tERROR: %s\t\t\t\t\t\t\t\t\n", rank+1, label, c.Error)
			continue
		}
		marker := ""
		switch {
		case !c.Feasible:
			marker = " [infeasible]"
			rejections = append(rejections, rejected{rank + 1, c.InfeasibleReasons})
		case rep == recIdx:
			marker = " [recommended]"
		}
		// Deltas are against the (base) reference row, so a row reads as
		// "what adopting this candidate changes", not just absolute numbers.
		fmt.Fprintf(tw, "%d\t%s%s\t%.1f\t%.1f (%+.1f)\t%s\t%s\t%.0f (%+.0f)\t%d\t%d\t%d\t%d (%+d)\n",
			rank+1, label, marker,
			c.Summary.SimPUHours,
			c.Summary.PUHoursSavedPercent, c.Summary.PUHoursSavedPercent-base.PUHoursSavedPercent,
			formatP95(c.Summary.SimHighPriorityCPU), formatP99(c.Summary.SimHighPriorityCPU),
			c.Summary.TargetExceededMinutes, c.Summary.TargetExceededMinutes-base.TargetExceededMinutes,
			c.Summary.ScaleUps, c.Summary.ScaleDowns,
			c.Summary.ScaleStepViolations,
			c.Summary.ScaleGapsUnder10Min, c.Summary.ScaleGapsUnder10Min-base.ScaleGapsUnder10Min)
	}
	tw.Flush()

	if len(rejections) > 0 {
		fmt.Println("\nwhy infeasible:")
		for _, r := range rejections {
			fmt.Printf("  %d: %s\n", r.rank, strings.Join(r.reasons, "; "))
		}
	}

	var best *simulator.Candidate
	if recIdx >= 0 {
		best = &candidates[recIdx]
	}
	fmt.Println("\nrecommended configuration:")
	for _, line := range recommendationLines(current, displayCurrent, base, best, keepReason) {
		fmt.Printf("  %s\n", line)
	}
	if furtherIdx >= 0 {
		fmt.Printf("  %s\n", furtherOptionLine(current, *best, candidates[furtherIdx], groupRank(candidates, furtherIdx)))
	}

	printMinPUAssessment("base", base)
	if best != nil {
		printMinPUAssessment("recommended candidate", best.Summary)
	}

	if base.LowConfidenceMinutes > 0 {
		fmt.Printf("\nnote: %.0f minutes of the recording are above %.0f%% CPU; the workload model is less reliable there (see -low-confidence-cpu)\n",
			base.LowConfidenceMinutes, common.lowConfidenceCPU)
	}
}

// printMinPUAssessment surfaces whether processingUnits.min should move,
// which the ranking table alone does not answer.
func printMinPUAssessment(label string, s simulator.Summary) {
	fmt.Printf("\nmin PU assessment (%s): min %d, pinned %.0f%% of the run", label, s.SpecMinPU, s.MinPinnedPercent)
	if s.RequiredPUAtMinP95 > 0 {
		fmt.Printf(", workload floor while pinned (p95) = %d PU", s.RequiredPUAtMinP95)
	}
	fmt.Println()
	assessment := s.AssessMinPU()
	fmt.Printf("  lower? %s\n", assessment.Lower)
	fmt.Printf("  raise? %s\n", assessment.Raise)
}

func formatP99(s *simulator.CPUStats) string {
	if s == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", s.P99)
}
