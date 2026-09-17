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
	"github.com/mercari/spanner-autoscaler/internal/scaling"
	webhookv1beta1 "github.com/mercari/spanner-autoscaler/internal/webhook/v1beta1"
)

// maxRecommendCombinations bounds the grid so a typo in a candidate list
// cannot explode into an hours-long search.
const maxRecommendCombinations = 20000

// SearchSpace lists the candidate values tried for each configuration parameter.
// An empty slice keeps the base configuration's value for that parameter; a
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
	// MaxGapMinutes, when non-nil, caps the candidate's data-gap minutes.
	// Callers set it to the base replay's own gap time: a candidate that
	// enables a metric the recording does not contain turns every tick into a
	// gap and would otherwise sail through the constraints on no data.
	MaxGapMinutes *float64
}

// Candidate is one evaluated configuration.
type Candidate struct {
	// Overrides maps parameter names to the value this candidate applied on top
	// of the base configuration (only the parameters present in the SearchSpace).
	Overrides map[string]string `json:"overrides"`
	Summary   Summary           `json:"summary"`
	Feasible  bool              `json:"feasible"`
	// InfeasibleReasons lists, for an infeasible candidate, which
	// constraints it broke and by how much.
	InfeasibleReasons []string `json:"infeasibleReasons,omitempty"`
	Error             string   `json:"error,omitempty"`
	// Autoscaler is the base configuration with this candidate's overrides
	// applied, so callers can re-run the candidate (e.g. to chart its full
	// time series). Excluded from JSON output.
	Autoscaler *spannerv1beta1.SpannerAutoscaler `json:"-"`
}

// Override parameter names used as Overrides keys, in display order.
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

// CurrentParameterValues renders the base configuration's value for every parameter the
// search space can override, keyed by the Override* names, so outputs can show
// "current → recommended" for each parameter — including the ones a candidate did
// not touch. Interval values are rendered as their effective durations
// (defaultScaleUpInterval / defaultScaleDownInterval when the spec leaves
// them unset), so a candidate that sets the same duration explicitly reads as
// "keep" rather than as a change.
func CurrentParameterValues(sa *spannerv1beta1.SpannerAutoscaler, defaultScaleUpInterval, defaultScaleDownInterval time.Duration) map[string]string {
	sc := sa.Spec.ScaleConfig
	current := map[string]string{
		OverrideMinPU:             fmt.Sprintf("%d", sc.ProcessingUnits.Min),
		OverrideScaledownStepSize: sc.ScaledownStepSize.String(),
		OverrideScaleupStepSize:   sc.ScaleupStepSize.String(),
		OverrideScaledownInterval: DurationValueOr(sc.ScaledownInterval, defaultScaleDownInterval).Duration.String(),
		OverrideScaleupInterval:   DurationValueOr(sc.ScaleupInterval, defaultScaleUpInterval).Duration.String(),
	}
	switch {
	case len(sc.ScaledownAllowedTimes) > 0:
		current[OverrideScaledownAllowedTimes] = strings.Join(sc.ScaledownAllowedTimes, ";")
	case len(sc.ScaledownNotAllowedTimes) > 0:
		// The spec restricts scale-down through the complementary field;
		// rendering "none" here would claim scale-down is unrestricted, and a
		// candidate that switches to an allow-list must count as a change.
		current[OverrideScaledownAllowedTimes] = "notAllowedTimes:" + strings.Join(sc.ScaledownNotAllowedTimes, ";")
	default:
		current[OverrideScaledownAllowedTimes] = "none"
	}
	if t := sc.TargetCPUUtilization.HighPriority; t != nil {
		current[OverrideTargetHighPriorityCPU] = fmt.Sprintf("%d", *t)
	} else {
		current[OverrideTargetHighPriorityCPU] = "none"
	}
	if t := sc.TargetCPUUtilization.Total; t != nil {
		current[OverrideTargetTotalCPU] = fmt.Sprintf("%d", *t)
	} else {
		current[OverrideTargetTotalCPU] = "none"
	}
	return current
}

// CurrentParameterDisplay is CurrentParameterValues for human-readable
// output: an interval the spec leaves unset renders as
// "controller default (<value>)" so nobody mistakes the effective value for
// an explicit setting. Comparisons must keep using CurrentParameterValues.
func CurrentParameterDisplay(sa *spannerv1beta1.SpannerAutoscaler, defaultScaleUpInterval, defaultScaleDownInterval time.Duration) map[string]string {
	display := CurrentParameterValues(sa, defaultScaleUpInterval, defaultScaleDownInterval)
	if sa.Spec.ScaleConfig.ScaledownInterval == nil {
		display[OverrideScaledownInterval] = "controller default (" + display[OverrideScaledownInterval] + ")"
	}
	if sa.Spec.ScaleConfig.ScaleupInterval == nil {
		display[OverrideScaleupInterval] = "controller default (" + display[OverrideScaleupInterval] + ")"
	}
	return display
}

// DescribeChanges renders only the overrides that differ from the current
// configuration, in canonical key order — search grids attach an override for
// every dimension, and repeating values that equal the current setting reads
// as if the candidate changed them.
func DescribeChanges(current, overrides map[string]string) string {
	parts := make([]string, 0, len(overrides))
	for _, key := range OverrideKeys {
		if v, ok := overrides[key]; ok && v != current[key] {
			parts = append(parts, key+"="+v)
		}
	}
	if len(parts) == 0 {
		return "(no change vs current)"
	}
	return strings.Join(parts, " ")
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

	// Equal-cost candidates are common (e.g. two pacing values that behave
	// identically on this workload); prefer the one that changes the fewest
	// parameters against the current configuration.
	current := CurrentParameterValues(base.Autoscaler,
		cmp.Or(base.ScaleUpInterval, DefaultScaleUpInterval),
		cmp.Or(base.ScaleDownInterval, DefaultScaleDownInterval))
	slices.SortStableFunc(candidates, func(a, b Candidate) int {
		if a.Feasible != b.Feasible {
			if a.Feasible {
				return -1
			}
			return 1
		}
		if c := cmp.Compare(a.Summary.SimPUHours, b.Summary.SimPUHours); c != 0 {
			return c
		}
		return cmp.Compare(changedParameterCount(current, a), changedParameterCount(current, b))
	})
	return candidates, nil
}

// RecommendedIndex selects the candidate the conclusion should propose,
// plus an optional alternative that saves more at higher risk.
//
// Only feasible candidates cheaper than the base replay are considered.
// When maxChanges > 0, the recommendation is restricted to candidates that
// change at most that many parameters against the current configuration —
// staged adoption: move one setting, observe, iterate. When no eligible
// candidate fits the change budget, nothing is recommended and keepReason
// says so. Within the pool, candidates whose
// savings are within savingsTolerancePt percentage points of the pool's best
// count as equal on cost, and the least risky of them is recommended — see
// lessRisky: the gentlest scale-down first, then the measured risk counters.
//
// When the best unrestricted candidate saves more than the recommendation by
// over savingsTolerancePt, its index is returned as furtherIdx so callers
// can present it as the option to try after the recommendation has proven
// out.
//
// Both indexes are -1 when nothing should change; keepReason then explains
// why: no feasible candidate, or none cheaper than the current
// configuration's own replay.
func RecommendedIndex(base Summary, current map[string]string, candidates []Candidate, savingsTolerancePt float64, maxChanges int) (recommendedIdx, furtherIdx int, keepReason string) {
	eligible := func(c *Candidate) bool {
		return c.Feasible && c.Summary.SimPUHours < base.SimPUHours
	}

	cheapest := -1
	cheapestCost := 0.0
	for i := range candidates {
		if !candidates[i].Feasible {
			continue
		}
		if cheapest == -1 || candidates[i].Summary.SimPUHours < cheapestCost {
			cheapest = i
			cheapestCost = candidates[i].Summary.SimPUHours
		}
	}
	if cheapest == -1 {
		return -1, -1, "no candidate satisfies the constraints — keep the current configuration, or relax the constraints / widen the search space"
	}
	if !eligible(&candidates[cheapest]) {
		return -1, -1, "every candidate that satisfies the constraints costs at least as much as the current configuration — keep the current configuration"
	}

	inPool := func(c *Candidate) bool {
		return eligible(c) && (maxChanges <= 0 || changedParameterCount(current, *c) <= maxChanges)
	}
	poolBest := -1
	poolBestCost := 0.0
	for i := range candidates {
		if !inPool(&candidates[i]) {
			continue
		}
		if poolBest == -1 || candidates[i].Summary.SimPUHours < poolBestCost {
			poolBest = i
			poolBestCost = candidates[i].Summary.SimPUHours
		}
	}
	if poolBest == -1 {
		return -1, -1, fmt.Sprintf("every candidate cheaper than the current configuration changes more than %d parameter(s) — keep the current configuration, or allow more changes per step", maxChanges)
	}

	minSaved := candidates[poolBest].Summary.PUHoursSavedPercent - savingsTolerancePt
	recommendedIdx = poolBest
	for i := range candidates {
		c := &candidates[i]
		if !inPool(c) || c.Summary.PUHoursSavedPercent < minSaved {
			continue
		}
		if lessRisky(current, base.SpecMinPU, c, &candidates[recommendedIdx]) {
			recommendedIdx = i
		}
	}

	if cheapest != recommendedIdx &&
		candidates[cheapest].Summary.PUHoursSavedPercent > candidates[recommendedIdx].Summary.PUHoursSavedPercent+savingsTolerancePt {
		return recommendedIdx, cheapest, ""
	}
	return recommendedIdx, -1, ""
}

// GroupEquivalent partitions candidate indexes into groups whose simulated
// outcomes are identical (same cost, risk counters, and scale activity), in
// the candidates' existing order. Search grids routinely contain parameter
// combinations that behave identically on a given recording; displays use
// the groups to show one representative row per outcome.
func GroupEquivalent(candidates []Candidate) [][]int {
	type outcome struct {
		simPUHours, exceeded    float64
		gaps, steps, ups, downs int
		feasible                bool
		err                     string
	}
	index := map[outcome]int{}
	var groups [][]int
	for i := range candidates {
		c := &candidates[i]
		key := outcome{
			simPUHours: c.Summary.SimPUHours,
			exceeded:   c.Summary.TargetExceededMinutes,
			gaps:       c.Summary.ScaleGapsUnder10Min,
			steps:      c.Summary.ScaleStepViolations,
			ups:        c.Summary.ScaleUps,
			downs:      c.Summary.ScaleDowns,
			feasible:   c.Feasible,
			err:        c.Error,
		}
		if gi, ok := index[key]; ok {
			groups[gi] = append(groups[gi], i)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, []int{i})
	}
	return groups
}

// lessRisky orders near-equal-cost candidates. The first key is the
// scale-down rate — the PU the configuration can shed per minute (resolved
// step size at the base minimum divided by the scale-down interval), so a
// larger step at a long interval still counts as gentler than a small step
// fired every few minutes. Frequent downsizing carries costs the simulation
// cannot measure — split rebalancing and tail latency during resizes — so
// the gentler configuration wins even when its measured overshoot is
// slightly higher. Ties fall through to the measured risk: minutes above
// target, scale gaps under ten minutes, changed parameters, and finally
// cost.
func lessRisky(current map[string]string, refPU int, a, b *Candidate) bool {
	if sa, sb := scaledownRate(refPU, a), scaledownRate(refPU, b); sa != sb {
		return sa < sb
	}
	if a.Summary.TargetExceededMinutes != b.Summary.TargetExceededMinutes {
		return a.Summary.TargetExceededMinutes < b.Summary.TargetExceededMinutes
	}
	if a.Summary.ScaleGapsUnder10Min != b.Summary.ScaleGapsUnder10Min {
		return a.Summary.ScaleGapsUnder10Min < b.Summary.ScaleGapsUnder10Min
	}
	if ca, cb := changedParameterCount(current, *a), changedParameterCount(current, *b); ca != cb {
		return ca < cb
	}
	return a.Summary.SimPUHours < b.Summary.SimPUHours
}

// scaledownRate is the candidate's scale-down speed in PU per minute: the
// scaledownStepSize resolved at a shared reference PU (so percentage and
// fixed steps compare on one scale) divided by the effective scale-down
// interval. A spec that leaves the interval unset resolves against
// DefaultScaleDownInterval; a candidate without a resolved configuration
// compares as neutral (0).
func scaledownRate(refPU int, c *Candidate) float64 {
	if c.Autoscaler == nil || refPU <= 0 {
		return 0
	}
	step := scaling.ResolveStepSize(&c.Autoscaler.Spec.ScaleConfig.ScaledownStepSize, refPU, scaling.StepDirectionScaledown)
	interval := DurationValueOr(c.Autoscaler.Spec.ScaleConfig.ScaledownInterval, DefaultScaleDownInterval).Duration
	if interval <= 0 {
		return float64(step)
	}
	return float64(step) / interval.Minutes()
}

// changedParameterCount counts the overrides that differ from the current
// configuration's value.
func changedParameterCount(current map[string]string, c Candidate) int {
	changed := 0
	for key, value := range c.Overrides {
		if cur, ok := current[key]; ok && cur != value {
			changed++
		}
	}
	return changed
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

	c := Candidate{Overrides: overrides, Autoscaler: sa}

	// Reject combinations the admission webhook would refuse (min above max,
	// invalid PU values or step sizes, out-of-range targets) before spending a
	// replay on them; a recommendation nobody can apply is worthless.
	if err := webhookv1beta1.ValidateSpec(sa); err != nil {
		c.Error = err.Error()
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
	if m := constraints.MaxGapMinutes; m != nil && s.GapMinutes > *m {
		reasons = append(reasons, fmt.Sprintf("data gaps %.0fm > %.0fm in the base replay — a metric this candidate enables is missing from the recording", s.GapMinutes, *m))
	}
	return reasons
}

// minPUCandidatePercentiles are the points of the required-PU distribution
// that become auto-generated minimum candidates: from "covers typical load"
// (p50) up to "covers almost every observed minute" (p99).
var minPUCandidatePercentiles = []float64{50, 75, 90, 95, 99}

// MinPUCandidates derives spec.processingUnits.min candidates from the
// recorded workload instead of requiring the caller to guess them. For every
// point it computes the PU the workload needed to stay on the configuration's
// targets (workload / target, PU-independent), takes percentiles of that
// distribution, and returns them as valid PU values together with the current
// minimum — deduplicated, sorted, and clamped to the configured maximum. The
// candidates are heuristic starting points; Recommend still evaluates each
// one against the full recording and the constraints.
func MinPUCandidates(sa *spannerv1beta1.SpannerAutoscaler, points []Point) []int {
	flags := sa.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags()
	var targetHigh, targetTotal int
	if t := sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority; t != nil {
		targetHigh = *t
	}
	if t := sa.Spec.ScaleConfig.TargetCPUUtilization.Total; t != nil {
		targetTotal = *t
	}

	required := make([]float64, 0, len(points))
	for _, p := range points {
		if req := requiredPU(flags, targetHigh, targetTotal, p); req > 0 {
			required = append(required, float64(req))
		}
	}
	if len(required) == 0 {
		return nil
	}
	slices.Sort(required)

	maxPU := sa.Spec.ScaleConfig.ProcessingUnits.Max
	candidates := []int{sa.Spec.ScaleConfig.ProcessingUnits.Min}
	for _, q := range minPUCandidatePercentiles {
		// Nearest-rank percentile returns an element of required, and
		// requiredPU already rounds to valid PU values.
		c := max(int(percentile(required, q)), 100)
		if maxPU > 0 {
			c = min(c, maxPU)
		}
		candidates = append(candidates, c)
	}
	slices.Sort(candidates)
	return slices.Compact(candidates)
}

// FillGuidelineStepCandidates populates the scale-down step-size and the
// interval dimensions that are still empty with conservative candidates:
// scale-down steps of 5-20% (larger steps shed capacity too fast to
// recommend unless the instance is essentially idle — list them explicitly
// to search them) and the PU-change guideline's minimum / preferred gaps as
// intervals. scaleupStepSize is intentionally not searched: capping the
// upward step saves next to nothing (the instance still reaches the desired
// PU, only later) while delaying spike response, so the current value is
// kept unless the caller lists candidates explicitly.
// Each auto-filled dimension also
// keeps the configuration's effective current value as a candidate —
// defaultScaleUpInterval / defaultScaleDownInterval stand in when the spec
// leaves the interval unset — so combinations that change only the other
// parameters stay in the search. Dimensions the caller already filled are
// left untouched.
func (s *SearchSpace) FillGuidelineStepCandidates(sa *spannerv1beta1.SpannerAutoscaler, defaultScaleUpInterval, defaultScaleDownInterval time.Duration) {
	sc := sa.Spec.ScaleConfig
	if len(s.ScaledownStepSizes) == 0 {
		for _, p := range []string{"5%", "10%", "15%", "20%"} {
			s.ScaledownStepSizes = append(s.ScaledownStepSizes, intstr.FromString(p))
		}
		s.ScaledownStepSizes = appendMissingStep(s.ScaledownStepSizes, sc.ScaledownStepSize)
	}
	guidelineIntervals := []metav1.Duration{
		{Duration: GuidelineMinScaleGap},
		{Duration: GuidelinePreferredScaleGap},
	}
	if len(s.ScaledownIntervals) == 0 {
		s.ScaledownIntervals = appendMissingInterval(guidelineIntervals, DurationValueOr(sc.ScaledownInterval, defaultScaleDownInterval))
	}
	if len(s.ScaleupIntervals) == 0 {
		s.ScaleupIntervals = appendMissingInterval(guidelineIntervals, DurationValueOr(sc.ScaleupInterval, defaultScaleUpInterval))
	}
}

// DurationValueOr resolves a spec interval to its effective value: the spec's
// own duration, or the controller-level default when the spec leaves it nil.
func DurationValueOr(spec *metav1.Duration, def time.Duration) metav1.Duration {
	if spec != nil {
		return *spec
	}
	return metav1.Duration{Duration: def}
}

func appendMissingStep(candidates []intstr.IntOrString, current intstr.IntOrString) []intstr.IntOrString {
	if slices.Contains(candidates, current) {
		return candidates
	}
	return append(candidates, current)
}

func appendMissingInterval(candidates []metav1.Duration, current metav1.Duration) []metav1.Duration {
	out := slices.Clone(candidates)
	if slices.Contains(out, current) {
		return out
	}
	return append(out, current)
}

// buildDimensions converts the search space into per-parameter override lists. A
// parameter with no candidates contributes a single no-op so the cartesian product
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

// cartesian expands the per-parameter override lists into every combination.
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
