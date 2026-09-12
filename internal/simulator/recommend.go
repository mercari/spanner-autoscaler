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

package simulator

import (
	"cmp"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// maxRecommendCombinations bounds the grid so a typo in a candidate list
// cannot explode into an hours-long search.
const maxRecommendCombinations = 20000

// SearchSpace lists the candidate values tried for each configuration knob.
// An empty slice keeps the base configuration's value for that knob; a
// non-empty slice REPLACES it with each candidate in turn (include the
// current value explicitly if it should stay in the running).
type SearchSpace struct {
	MinPUs                 []int
	ScaledownStepSizes     []intstr.IntOrString
	ScaledownIntervals     []metav1.Duration
	ScaledownAllowedTimes  [][]string
	ScaleupStepSizes       []intstr.IntOrString
	ScaleupIntervals       []metav1.Duration
	TargetHighPriorityCPUs []int
	TargetTotalCPUs        []int
}

// Google-recommended maximum high-priority CPU utilization for Cloud Spanner
// (https://cloud.google.com/spanner/docs/cpu-utilization#recommended-max):
// 65% for regional instances, 45% per region for multi-region / dual-region
// configurations.
const (
	RecommendedHighPriorityCPURegional    = 65
	RecommendedHighPriorityCPUMultiRegion = 45
)

// Constraints filter candidates before ranking.
type Constraints struct {
	// MaxTargetExceededMinutes is the maximum tolerated time with a
	// simulated CPU metric above its target.
	MaxTargetExceededMinutes float64
	// MaxSimCPUP99, when non-nil, requires the p99 of every simulated CPU
	// metric to stay at or below this percentage.
	MaxSimCPUP99 *float64
	// MaxHighPriorityCPU, when > 0, applies the Google-recommended
	// high-priority CPU ceiling (RecommendedHighPriorityCPURegional /
	// RecommendedHighPriorityCPUMultiRegion): a candidate is infeasible when
	// its effective targetCPUUtilization.highPriority exceeds the ceiling —
	// such a config would let the autoscaler hold utilization above the
	// recommended maximum — or when the simulated high-priority CPU p99
	// exceeds it.
	MaxHighPriorityCPU int
	// MaxScaleStepViolations / MaxShortScaleGaps, when non-nil, cap the
	// PU-change guideline counters of the simulated trace: scale events
	// beyond GuidelineMaxPUChangeFactor per operation, and consecutive scale
	// events closer together than GuidelineMinScaleGap. Callers typically
	// set these to 0 (strict) or to the base config's own counts (do not
	// regress) — the latter tolerates violations caused by fixed schedules
	// that every candidate shares.
	MaxScaleStepViolations *int
	MaxShortScaleGaps      *int
}

// Candidate is one evaluated configuration.
type Candidate struct {
	// Overrides maps knob names to the value this candidate applied on top
	// of the base configuration (only the knobs present in the SearchSpace).
	Overrides map[string]string `json:"overrides"`
	Summary   Summary           `json:"summary"`
	Feasible  bool              `json:"feasible"`
	// InfeasibleReasons lists, for an infeasible candidate, which
	// constraints it broke and by how much.
	InfeasibleReasons []string `json:"infeasibleReasons,omitempty"`
	Error             string   `json:"error,omitempty"`
}

// Override knob names used as Overrides keys, in display order.
const (
	OverrideMinPU                 = "minPU"
	OverrideScaledownStepSize     = "scaledownStepSize"
	OverrideScaledownInterval     = "scaledownInterval"
	OverrideScaledownAllowedTimes = "scaledownAllowedTimes"
	OverrideScaleupStepSize       = "scaleupStepSize"
	OverrideScaleupInterval       = "scaleupInterval"
	OverrideTargetHighPriorityCPU = "targetHighPriorityCPU"
	OverrideTargetTotalCPU        = "targetTotalCPU"
)

// OverrideKeys is the canonical display order of Candidate.Overrides keys.
var OverrideKeys = []string{
	OverrideMinPU,
	OverrideScaledownStepSize,
	OverrideScaledownInterval,
	OverrideScaledownAllowedTimes,
	OverrideScaleupStepSize,
	OverrideScaleupInterval,
	OverrideTargetHighPriorityCPU,
	OverrideTargetTotalCPU,
}

// CurrentKnobValues renders the base configuration's value for every knob the
// search space can override, keyed by the Override* names, so outputs can show
// "current → recommended" for each knob — including the ones a candidate did
// not touch.
func CurrentKnobValues(sa *spannerv1beta1.SpannerAutoscaler) map[string]string {
	sc := sa.Spec.ScaleConfig
	current := map[string]string{
		OverrideMinPU:             fmt.Sprintf("%d", sc.ProcessingUnits.Min),
		OverrideScaledownStepSize: sc.ScaledownStepSize.String(),
		OverrideScaleupStepSize:   sc.ScaleupStepSize.String(),
	}
	if sc.ScaledownInterval != nil {
		current[OverrideScaledownInterval] = sc.ScaledownInterval.Duration.String()
	} else {
		current[OverrideScaledownInterval] = "controller default"
	}
	if sc.ScaleupInterval != nil {
		current[OverrideScaleupInterval] = sc.ScaleupInterval.Duration.String()
	} else {
		current[OverrideScaleupInterval] = "controller default"
	}
	if len(sc.ScaledownAllowedTimes) > 0 {
		current[OverrideScaledownAllowedTimes] = strings.Join(sc.ScaledownAllowedTimes, ";")
	} else {
		current[OverrideScaledownAllowedTimes] = "none"
	}
	if t := sc.TargetCPUUtilization.HighPriority; t != nil {
		current[OverrideTargetHighPriorityCPU] = fmt.Sprintf("%d", *t)
	}
	if t := sc.TargetCPUUtilization.Total; t != nil {
		current[OverrideTargetTotalCPU] = fmt.Sprintf("%d", *t)
	}
	return current
}

// DescribeOverrides renders a candidate's overrides in canonical key order.
func DescribeOverrides(overrides map[string]string) string {
	parts := make([]string, 0, len(overrides))
	for _, key := range OverrideKeys {
		if v, ok := overrides[key]; ok {
			parts = append(parts, key+"="+v)
		}
	}
	if len(parts) == 0 {
		return "(base)"
	}
	return strings.Join(parts, " ")
}

// override mutates a copy of the base spec and records itself for display.
type override struct {
	key   string
	value string
	apply func(sa *spannerv1beta1.SpannerAutoscaler)
}

// Recommend replays every combination of the search space against points and
// returns the candidates, feasible ones first, each group ordered by
// ascending simulated PU-hours (i.e. cheapest safe configuration first).
// Runs are independent and executed in parallel.
func Recommend(base Config, space SearchSpace, constraints Constraints, points []Point) ([]Candidate, error) {
	if base.Autoscaler == nil {
		return nil, fmt.Errorf("config: autoscaler is required")
	}

	dimensions := buildDimensions(space)
	total := 1
	for _, dim := range dimensions {
		total *= len(dim)
	}
	if total > maxRecommendCombinations {
		return nil, fmt.Errorf("search space has %d combinations; the limit is %d", total, maxRecommendCombinations)
	}

	combos := cartesian(dimensions)
	candidates := make([]Candidate, len(combos))

	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	for i, combo := range combos {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			candidates[i] = evaluate(base, combo, constraints, points)
		}()
	}
	wg.Wait()

	slices.SortStableFunc(candidates, func(a, b Candidate) int {
		if a.Feasible != b.Feasible {
			if a.Feasible {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Summary.SimPUHours, b.Summary.SimPUHours)
	})
	return candidates, nil
}

func evaluate(base Config, combo []override, constraints Constraints, points []Point) Candidate {
	sa := base.Autoscaler.DeepCopy()
	overrides := make(map[string]string, len(combo))
	for _, o := range combo {
		if o.apply == nil {
			continue
		}
		o.apply(sa)
		overrides[o.key] = o.value
	}

	c := Candidate{Overrides: overrides}

	if minPU, maxPU := sa.Spec.ScaleConfig.ProcessingUnits.Min, sa.Spec.ScaleConfig.ProcessingUnits.Max; minPU > maxPU {
		c.Error = fmt.Sprintf("min PU %d exceeds max PU %d", minPU, maxPU)
		return c
	}

	cfg := base
	cfg.Autoscaler = sa
	result, err := Run(cfg, points)
	if err != nil {
		c.Error = err.Error()
		return c
	}

	c.Summary = result.Summary
	c.InfeasibleReasons = infeasibleReasons(sa, result.Summary, constraints)
	c.Feasible = len(c.InfeasibleReasons) == 0
	return c
}

// infeasibleReasons reports every constraint the candidate breaks, with the
// actual value against the allowed one, so an infeasible row explains itself.
func infeasibleReasons(sa *spannerv1beta1.SpannerAutoscaler, s Summary, constraints Constraints) []string {
	var reasons []string
	if s.TargetExceededMinutes > constraints.MaxTargetExceededMinutes {
		reasons = append(reasons, fmt.Sprintf("above target %.0fm > %.0fm allowed",
			s.TargetExceededMinutes, constraints.MaxTargetExceededMinutes))
	}
	if p := constraints.MaxSimCPUP99; p != nil {
		if s.SimHighPriorityCPU != nil && s.SimHighPriorityCPU.P99 > *p {
			reasons = append(reasons, fmt.Sprintf("p99 high-priority CPU %.1f%% > %.1f%%", s.SimHighPriorityCPU.P99, *p))
		}
		if s.SimTotalCPU != nil && s.SimTotalCPU.P99 > *p {
			reasons = append(reasons, fmt.Sprintf("p99 total CPU %.1f%% > %.1f%%", s.SimTotalCPU.P99, *p))
		}
	}
	if limit := constraints.MaxHighPriorityCPU; limit > 0 {
		if t := sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority; t != nil && *t > limit {
			reasons = append(reasons, fmt.Sprintf("high-priority CPU target %d%% > recommended %d%%", *t, limit))
		}
		if s.SimHighPriorityCPU != nil && s.SimHighPriorityCPU.P99 > float64(limit) {
			reasons = append(reasons, fmt.Sprintf("p99 high-priority CPU %.1f%% > recommended %d%%", s.SimHighPriorityCPU.P99, limit))
		}
	}
	if m := constraints.MaxScaleStepViolations; m != nil && s.ScaleStepViolations > *m {
		reasons = append(reasons, fmt.Sprintf("steps beyond 2x/half %d > %d allowed", s.ScaleStepViolations, *m))
	}
	if m := constraints.MaxShortScaleGaps; m != nil && s.ScaleGapsUnder10Min > *m {
		reasons = append(reasons, fmt.Sprintf("scale gaps <10m %d > %d allowed", s.ScaleGapsUnder10Min, *m))
	}
	return reasons
}

// FillGuidelineStepCandidates populates the step-size and interval dimensions
// that are still empty with candidates that respect the PU-change guideline:
// percentage steps (which express the 2x/half rule naturally at any instance
// size — down to 50%, up to 100% of the current PU) and the guideline's
// minimum / preferred gaps as intervals. Dimensions the caller already filled
// are left untouched.
func (s *SearchSpace) FillGuidelineStepCandidates() {
	if len(s.ScaledownStepSizes) == 0 {
		for _, p := range []string{"10%", "20%", "30%", "40%", "50%"} {
			s.ScaledownStepSizes = append(s.ScaledownStepSizes, intstr.FromString(p))
		}
	}
	if len(s.ScaleupStepSizes) == 0 {
		for _, p := range []string{"25%", "50%", "100%"} {
			s.ScaleupStepSizes = append(s.ScaleupStepSizes, intstr.FromString(p))
		}
	}
	guidelineIntervals := []metav1.Duration{
		{Duration: GuidelineMinScaleGap},
		{Duration: GuidelinePreferredScaleGap},
	}
	if len(s.ScaledownIntervals) == 0 {
		s.ScaledownIntervals = guidelineIntervals
	}
	if len(s.ScaleupIntervals) == 0 {
		s.ScaleupIntervals = guidelineIntervals
	}
}

// buildDimensions converts the search space into per-knob override lists. A
// knob with no candidates contributes a single no-op so the cartesian product
// keeps the base value.
func buildDimensions(space SearchSpace) [][]override {
	noop := []override{{}}

	dim := func(overrides []override) []override {
		if len(overrides) == 0 {
			return noop
		}
		return overrides
	}

	var minPUs []override
	for _, v := range space.MinPUs {
		minPUs = append(minPUs, override{
			key:   OverrideMinPU,
			value: fmt.Sprintf("%d", v),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ProcessingUnits.Min = v
			},
		})
	}

	var steps []override
	for _, v := range space.ScaledownStepSizes {
		steps = append(steps, override{
			key:   OverrideScaledownStepSize,
			value: v.String(),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaledownStepSize = v
			},
		})
	}

	var intervals []override
	for _, v := range space.ScaledownIntervals {
		intervals = append(intervals, override{
			key:   OverrideScaledownInterval,
			value: v.Duration.String(),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaledownInterval = &metav1.Duration{Duration: v.Duration}
			},
		})
	}

	var windows []override
	for _, v := range space.ScaledownAllowedTimes {
		value := "none"
		if len(v) > 0 {
			value = strings.Join(v, ";")
		}
		windows = append(windows, override{
			key:   OverrideScaledownAllowedTimes,
			value: value,
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaledownAllowedTimes = v
				// The two restriction styles are mutually exclusive; replacing
				// the allowlist drops any blocklist from the base config.
				if len(v) > 0 {
					sa.Spec.ScaleConfig.ScaledownNotAllowedTimes = nil
				}
			},
		})
	}

	var upSteps []override
	for _, v := range space.ScaleupStepSizes {
		upSteps = append(upSteps, override{
			key:   OverrideScaleupStepSize,
			value: v.String(),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaleupStepSize = v
			},
		})
	}

	var upIntervals []override
	for _, v := range space.ScaleupIntervals {
		upIntervals = append(upIntervals, override{
			key:   OverrideScaleupInterval,
			value: v.Duration.String(),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.ScaleupInterval = &metav1.Duration{Duration: v.Duration}
			},
		})
	}

	var highTargets []override
	for _, v := range space.TargetHighPriorityCPUs {
		highTargets = append(highTargets, override{
			key:   OverrideTargetHighPriorityCPU,
			value: fmt.Sprintf("%d", v),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority = &v
			},
		})
	}

	var totalTargets []override
	for _, v := range space.TargetTotalCPUs {
		totalTargets = append(totalTargets, override{
			key:   OverrideTargetTotalCPU,
			value: fmt.Sprintf("%d", v),
			apply: func(sa *spannerv1beta1.SpannerAutoscaler) {
				sa.Spec.ScaleConfig.TargetCPUUtilization.Total = &v
			},
		})
	}

	return [][]override{
		dim(minPUs),
		dim(steps),
		dim(intervals),
		dim(windows),
		dim(upSteps),
		dim(upIntervals),
		dim(highTargets),
		dim(totalTargets),
	}
}

// cartesian expands the per-knob override lists into every combination.
func cartesian(dimensions [][]override) [][]override {
	combos := [][]override{{}}
	for _, dim := range dimensions {
		next := make([][]override, 0, len(combos)*len(dim))
		for _, combo := range combos {
			for _, o := range dim {
				extended := make([]override, len(combo), len(combo)+1)
				copy(extended, combo)
				next = append(next, append(extended, o))
			}
		}
		combos = next
	}
	return combos
}

// ParseDurations converts "30m,55m" style CLI input into candidate interval
// values.
func ParseDurations(s string) ([]metav1.Duration, error) {
	if s == "" {
		return nil, nil
	}
	var out []metav1.Duration
	for part := range strings.SplitSeq(s, ",") {
		d, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", part, err)
		}
		out = append(out, metav1.Duration{Duration: d})
	}
	return out, nil
}
