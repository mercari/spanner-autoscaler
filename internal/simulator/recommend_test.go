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
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestRecommendRanksCheapestFeasibleFirst(t *testing.T) {
	// Over-provisioned recording: flat 5000 PU carrying a workload of 400
	// (8% CPU). Lower minimums must win as long as CPU stays under target.
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 24*60, 5000, 400)

	base := Config{Autoscaler: newAutoscaler(5000, 10000, 30)}
	space := SearchSpace{
		MinPUs:             []int{1000, 3000, 5000},
		ScaledownStepSizes: []intstr.IntOrString{intstr.FromString("30%")},
		ScaledownIntervals: []metav1.Duration{{Duration: 30 * time.Minute}},
	}

	candidates, err := Recommend(base, space, Constraints{}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("got %d candidates; want 3", len(candidates))
	}

	// All candidates keep CPU under target here, so ranking is purely by cost:
	// the lowest minimum first.
	if got := candidates[0].Overrides[OverrideMinPU]; got != "1000" {
		t.Errorf("top candidate minPU = %s; want 1000", got)
	}
	for i, c := range candidates {
		if !c.Feasible {
			t.Errorf("candidate %d (%s) infeasible; want feasible", i, DescribeOverrides(c.Overrides))
		}
		if i > 0 && c.Summary.SimPUHours < candidates[i-1].Summary.SimPUHours {
			t.Errorf("candidates not sorted by SimPUHours: %f after %f",
				c.Summary.SimPUHours, candidates[i-1].Summary.SimPUHours)
		}
	}
	// Every candidate must carry the fixed-dimension overrides too.
	if got := candidates[0].Overrides[OverrideScaledownStepSize]; got != "30%" {
		t.Errorf("top candidate scaledownStepSize = %q; want 30%%", got)
	}
	// The candidate keeps its resolved configuration so callers can re-run it.
	top := candidates[0]
	if top.Autoscaler == nil || top.Autoscaler.Spec.ScaleConfig.ProcessingUnits.Min != 1000 {
		t.Errorf("top candidate Autoscaler = %+v; want the override applied (min 1000)", top.Autoscaler)
	}
	if base.Autoscaler.Spec.ScaleConfig.ProcessingUnits.Min != 5000 {
		t.Errorf("base autoscaler min = %d; candidates must not mutate the base config",
			base.Autoscaler.Spec.ScaleConfig.ProcessingUnits.Min)
	}
}

func TestRecommendConstraintFiltersCandidates(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 6*60, 5000, 400)

	base := Config{Autoscaler: newAutoscaler(5000, 10000, 30)}
	space := SearchSpace{MinPUs: []int{1000, 5000}}

	// The min-1000 candidate settles at 2000 PU (the desired-PU rounding
	// stops there for target 30), where the simulated CPU is 20% — above a
	// 15% p99 cap. The min-5000 candidate stays at 8%.
	p99Cap := 15.0
	candidates, err := Recommend(base, space, Constraints{MaxSimCPUP99: &p99Cap}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates; want 2", len(candidates))
	}
	if got := candidates[0].Overrides[OverrideMinPU]; !candidates[0].Feasible || got != "5000" {
		t.Errorf("first candidate = %s feasible=%t; want feasible minPU=5000",
			DescribeOverrides(candidates[0].Overrides), candidates[0].Feasible)
	}
	if candidates[1].Feasible {
		t.Errorf("candidate %s should be infeasible under p99 cap %.0f",
			DescribeOverrides(candidates[1].Overrides), p99Cap)
	}
}

func TestRecommendScaleupDimensions(t *testing.T) {
	// 30 minutes idle, then a sustained spike: an uncapped scale-up absorbs
	// it in one jump while a 1000-PU step needs several minutes above target.
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 30, 1000, 50)
	spike := constantWorkloadPoints(start.Add(30*time.Minute), 90, 1000, 900)
	points = append(points, spike...)

	base := Config{Autoscaler: newAutoscaler(1000, 10000, 30)}
	space := SearchSpace{
		ScaleupStepSizes: []intstr.IntOrString{intstr.FromInt(0), intstr.FromInt(1000)},
		ScaleupIntervals: []metav1.Duration{{Duration: time.Minute}},
	}

	// Allow exactly one minute above target: only the uncapped candidate
	// (a single spike tick before the jump) stays feasible.
	candidates, err := Recommend(base, space, Constraints{MaxTargetExceededMinutes: 1}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates; want 2", len(candidates))
	}

	first := candidates[0]
	if !first.Feasible || first.Overrides[OverrideScaleupStepSize] != "0" {
		t.Errorf("first candidate = %s feasible=%t; want feasible scaleupStepSize=0",
			DescribeOverrides(first.Overrides), first.Feasible)
	}
	if got := first.Overrides[OverrideScaleupInterval]; got != "1m0s" {
		t.Errorf("scaleupInterval override = %q; want 1m0s", got)
	}
	second := candidates[1]
	if second.Feasible {
		t.Errorf("stepped candidate %s should exceed the 1-minute budget (got %.0f minutes)",
			DescribeOverrides(second.Overrides), second.Summary.TargetExceededMinutes)
	}
	if second.Summary.TargetExceededMinutes <= first.Summary.TargetExceededMinutes {
		t.Errorf("stepped candidate exceeded %.0f minutes; want more than uncapped %.0f",
			second.Summary.TargetExceededMinutes, first.Summary.TargetExceededMinutes)
	}
}

func TestRecommendHighPriorityCPUGuidelineOnTarget(t *testing.T) {
	// The workload is tiny, so simulated CPU never approaches the ceiling —
	// a target above the guideline must still be infeasible, because such a
	// config would let the autoscaler hold utilization above the
	// recommended maximum.
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 60, 1000, 50)

	base := Config{Autoscaler: newAutoscaler(1000, 10000, 40)}
	space := SearchSpace{TargetHighPriorityCPUs: []int{40, 70}}

	candidates, err := Recommend(base, space, Constraints{
		MaxHighPriorityCPU: RecommendedHighPriorityCPURegional,
	}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates; want 2", len(candidates))
	}
	if got := candidates[0].Overrides[OverrideTargetHighPriorityCPU]; !candidates[0].Feasible || got != "40" {
		t.Errorf("first candidate = %s feasible=%t; want feasible target 40",
			DescribeOverrides(candidates[0].Overrides), candidates[0].Feasible)
	}
	if candidates[1].Feasible {
		t.Errorf("target-70 candidate must be infeasible under the regional 65%% guideline")
	}
	if reasons := candidates[1].InfeasibleReasons; len(reasons) == 0 || !strings.Contains(strings.Join(reasons, ";"), "recommended 65") {
		t.Errorf("InfeasibleReasons = %v; want a reason naming the recommended 65%% ceiling", reasons)
	}
}

func TestRecommendHighPriorityCPUGuidelineOnSimP99(t *testing.T) {
	// min == max pins the instance at 1000 PU, so the simulated CPU sits at
	// a constant 50%: within the regional 65% ceiling but above the
	// multi-region 45% one.
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 60, 1000, 500)
	base := Config{Autoscaler: newAutoscaler(1000, 1000, 60)}

	for _, tc := range []struct {
		limit    int
		feasible bool
	}{
		{RecommendedHighPriorityCPURegional, true},
		{RecommendedHighPriorityCPUMultiRegion, false},
	} {
		candidates, err := Recommend(base, SearchSpace{}, Constraints{MaxHighPriorityCPU: tc.limit}, points)
		if err != nil {
			t.Fatalf("Recommend(limit=%d): %v", tc.limit, err)
		}
		if len(candidates) != 1 || candidates[0].Feasible != tc.feasible {
			t.Errorf("limit=%d: candidates = %+v; want single candidate with feasible=%t",
				tc.limit, candidates, tc.feasible)
		}
	}
}

func TestRecommendPUChangeGuideline(t *testing.T) {
	// Same spike shape as TestRecommendScaleupDimensions: after 30 idle
	// minutes the workload jumps so that 4000 PU are needed.
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 30, 1000, 50)
	points = append(points, constantWorkloadPoints(start.Add(30*time.Minute), 90, 1000, 900)...)

	base := Config{Autoscaler: newAutoscaler(1000, 10000, 30)}
	space := SearchSpace{
		// Uncapped: one 1000→4000 jump (4x, a step violation, no gaps).
		// 100% with a 10m interval: 1000→2000→4000, guideline-compliant.
		ScaleupStepSizes: []intstr.IntOrString{intstr.FromInt(0), intstr.FromString("100%")},
		ScaleupIntervals: []metav1.Duration{{Duration: 10 * time.Minute}},
	}

	zero := 0
	candidates, err := Recommend(base, space, Constraints{
		MaxTargetExceededMinutes: 60,
		MaxScaleStepViolations:   &zero,
		MaxShortScaleGaps:        &zero,
	}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates; want 2", len(candidates))
	}

	first := candidates[0]
	if !first.Feasible || first.Overrides[OverrideScaleupStepSize] != "100%" {
		t.Errorf("first candidate = %s feasible=%t (violations=%d); want feasible scaleupStepSize=100%%",
			DescribeOverrides(first.Overrides), first.Feasible, first.Summary.ScaleStepViolations)
	}
	if first.Summary.ScaleStepViolations != 0 || first.Summary.ScaleGapsUnder10Min != 0 {
		t.Errorf("compliant candidate counters = %d step / %d gap; want 0/0",
			first.Summary.ScaleStepViolations, first.Summary.ScaleGapsUnder10Min)
	}
	second := candidates[1]
	if second.Feasible || second.Summary.ScaleStepViolations == 0 {
		t.Errorf("uncapped candidate = feasible=%t violations=%d; want infeasible with >=1 step violation",
			second.Feasible, second.Summary.ScaleStepViolations)
	}
}

func TestMinPUCandidates(t *testing.T) {
	// Two workload levels: 60 quiet minutes needing 2000 PU (workload 400 at
	// target 40 → required 1000, rounded up one unit) and 60 busy minutes
	// needing 8000 PU (workload 2800 → required 7000, rounded up).
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 60, 20000, 400)
	points = append(points, constantWorkloadPoints(start.Add(time.Hour), 60, 20000, 2800)...)

	sa := newAutoscaler(20000, 30000, 40)
	got := MinPUCandidates(sa, points)

	// p50 lands on the quiet level (2000), p75..p99 on the busy level (8000),
	// plus the current minimum — deduplicated and sorted.
	want := []int{2000, 8000, 20000}
	if !slices.Equal(got, want) {
		t.Errorf("MinPUCandidates = %v; want %v", got, want)
	}

	// Percentile candidates above the configured maximum are clamped to it.
	sa = newAutoscaler(3000, 5000, 40)
	if got, want := MinPUCandidates(sa, points), []int{2000, 3000, 5000}; !slices.Equal(got, want) {
		t.Errorf("MinPUCandidates with max 5000 = %v; want %v", got, want)
	}

	if got := MinPUCandidates(newAutoscaler(1000, 2000, 40), nil); got != nil {
		t.Errorf("MinPUCandidates with no points = %v; want nil", got)
	}
}

func TestFillGuidelineStepCandidates(t *testing.T) {
	sa := newAutoscaler(1000, 10000, 30)
	sa.Spec.ScaleConfig.ScaledownInterval = &metav1.Duration{Duration: 55 * time.Minute}
	space := SearchSpace{
		ScaleupStepSizes: []intstr.IntOrString{intstr.FromInt(5000)},
	}
	space.FillGuidelineStepCandidates(sa, time.Minute, 55*time.Minute)

	// Four conservative percentages plus the spec's current value (2000).
	if len(space.ScaledownStepSizes) != 5 || space.ScaledownStepSizes[4] != intstr.FromInt(2000) {
		t.Errorf("ScaledownStepSizes = %v; want 5/10/15/20%% plus the current 2000", space.ScaledownStepSizes)
	}
	// A dimension the caller already filled must be left untouched.
	if len(space.ScaleupStepSizes) != 1 || space.ScaleupStepSizes[0] != intstr.FromInt(5000) {
		t.Errorf("ScaleupStepSizes = %v; want the caller's single candidate preserved", space.ScaleupStepSizes)
	}
	// Scaledown intervals: the two guideline gaps plus the spec's current 55m;
	// scaleup intervals: the guideline gaps plus the controller default (the
	// spec leaves the interval nil).
	wantDown := []metav1.Duration{{Duration: GuidelineMinScaleGap}, {Duration: GuidelinePreferredScaleGap}, {Duration: 55 * time.Minute}}
	if !slices.Equal(space.ScaledownIntervals, wantDown) {
		t.Errorf("ScaledownIntervals = %v; want %v", space.ScaledownIntervals, wantDown)
	}
	wantUp := []metav1.Duration{{Duration: GuidelineMinScaleGap}, {Duration: GuidelinePreferredScaleGap}, {Duration: time.Minute}}
	if !slices.Equal(space.ScaleupIntervals, wantUp) {
		t.Errorf("ScaleupIntervals = %v; want %v", space.ScaleupIntervals, wantUp)
	}

	// scaleupStepSize is not searched automatically: an empty dimension stays
	// empty, keeping the configuration's current value.
	empty := SearchSpace{}
	empty.FillGuidelineStepCandidates(sa, time.Minute, 55*time.Minute)
	if len(empty.ScaleupStepSizes) != 0 {
		t.Errorf("ScaleupStepSizes = %v; want none auto-generated", empty.ScaleupStepSizes)
	}
}

func TestRecommendedIndex(t *testing.T) {
	base := Summary{SimPUHours: 10000, PUHoursSavedPercent: 8}
	current := map[string]string{OverrideScaledownStepSize: "10%"}
	mk := func(saved, exceeded float64, step string) Candidate {
		return Candidate{
			Feasible:  true,
			Overrides: map[string]string{OverrideScaledownStepSize: step},
			Summary: Summary{
				// Higher savings = lower cost against the same recording.
				SimPUHours:            10000 * (108 - saved) / 100,
				PUHoursSavedPercent:   saved,
				TargetExceededMinutes: exceeded,
			},
		}
	}
	candidates := []Candidate{
		mk(19.4, 694, "20%"), // cheapest but riskiest
		mk(18.9, 500, "10%"), // within 1pt of the best, far less risk
		mk(10.0, 470, "10%"), // outside the tolerance window
	}

	// Unlimited changes: the safer near-equal candidate wins the tolerance
	// window; the cheapest is equivalent by definition, so no further option.
	recIdx, furtherIdx, reason := RecommendedIndex(base, current, candidates, 1.0, 0)
	if recIdx != 1 || furtherIdx != -1 || reason != "" {
		t.Errorf("RecommendedIndex(tolerance 1.0) = (%d, %d, %q); want the safer near-equal candidate (1) and no further option", recIdx, furtherIdx, reason)
	}

	// Tolerance 0, unlimited changes: always the cheapest, no alternative.
	recIdx, furtherIdx, _ = RecommendedIndex(base, current, candidates, 0, 0)
	if recIdx != 0 || furtherIdx != -1 {
		t.Errorf("RecommendedIndex(tolerance 0) = (%d, %d); want (0, -1)", recIdx, furtherIdx)
	}

	// A one-change budget: the cheapest candidate now changes two parameters
	// and saves well beyond the tolerance, so it is excluded from the
	// recommendation pool but surfaces as the further option.
	candidates[0].Summary.PUHoursSavedPercent = 21.0
	candidates[0].Summary.SimPUHours = 10000 * (108 - 21.0) / 100
	candidates[0].Overrides[OverrideMinPU] = "15000"
	current[OverrideMinPU] = "20000"
	recIdx, furtherIdx, _ = RecommendedIndex(base, current, candidates, 1.0, 1)
	if recIdx != 1 || furtherIdx != 0 {
		t.Errorf("RecommendedIndex(maxChanges 1) = (%d, %d); want the single-change candidate (1) with the two-change one (0) as the further option", recIdx, furtherIdx)
	}

	// Nothing cheaper than base → keep the current configuration.
	expensive := []Candidate{mk(-2, 100, "10%")}
	expensive[0].Summary.SimPUHours = 11000
	if recIdx, furtherIdx, reason = RecommendedIndex(base, current, expensive, 1.0, 1); recIdx != -1 || furtherIdx != -1 || reason == "" {
		t.Errorf("RecommendedIndex(costlier than base) = (%d, %d, %q); want keep-current", recIdx, furtherIdx, reason)
	}

	// No feasible candidate at all.
	infeasible := []Candidate{{Feasible: false}}
	if recIdx, _, reason = RecommendedIndex(base, current, infeasible, 1.0, 1); recIdx != -1 || reason == "" {
		t.Errorf("RecommendedIndex(no feasible) = (%d, %q); want keep-current with a reason", recIdx, reason)
	}
}

func TestScaledownRateOrdersGentlerFirst(t *testing.T) {
	base := Summary{SimPUHours: 10000, PUHoursSavedPercent: 8, SpecMinPU: 20000}
	current := map[string]string{OverrideScaledownStepSize: "10%", OverrideScaledownInterval: "30m0s"}

	mk := func(saved float64, step string, interval time.Duration) Candidate {
		sa := newAutoscaler(20000, 60000, 40)
		sa.Spec.ScaleConfig.ScaledownStepSize = intstr.Parse(step)
		sa.Spec.ScaleConfig.ScaledownInterval = &metav1.Duration{Duration: interval}
		return Candidate{
			Feasible:   true,
			Autoscaler: sa,
			Overrides:  map[string]string{OverrideScaledownStepSize: step, OverrideScaledownInterval: interval.String()},
			Summary:    Summary{SimPUHours: 10000 * (108 - saved) / 100, PUHoursSavedPercent: saved},
		}
	}
	candidates := []Candidate{
		mk(15.5, "10%", 10*time.Minute), // cheapest; rate 2000/10 = 200 PU/min
		mk(15.2, "20%", 30*time.Minute), // near-equal; rate 4000/30 ≈ 133 PU/min — gentler
	}
	recIdx, _, _ := RecommendedIndex(base, current, candidates, 2.0, 0)
	if recIdx != 1 {
		t.Errorf("RecommendedIndex = %d; want 1 (a larger step at a long interval is gentler than a small step fired often)", recIdx)
	}
}

func TestDescribeChangesAndDisplay(t *testing.T) {
	current := map[string]string{
		OverrideMinPU:             "20000",
		OverrideScaledownStepSize: "10%",
		OverrideScaleupInterval:   "1m0s",
	}
	overrides := map[string]string{
		OverrideMinPU:             "15000",
		OverrideScaledownStepSize: "10%",  // equals current — must not appear
		OverrideScaleupInterval:   "1m0s", // equals current — must not appear
	}
	if got, want := DescribeChanges(current, overrides), "minPU=15000"; got != want {
		t.Errorf("DescribeChanges = %q; want %q", got, want)
	}
	if got, want := DescribeChanges(current, map[string]string{OverrideMinPU: "20000"}), "(no change vs current)"; got != want {
		t.Errorf("DescribeChanges(no diff) = %q; want %q", got, want)
	}

	sa := newAutoscaler(20000, 60000, 40) // intervals left nil in the spec
	display := CurrentParameterDisplay(sa, time.Minute, 55*time.Minute)
	if got, want := display[OverrideScaleupInterval], "controller default (1m0s)"; got != want {
		t.Errorf("display scaleupInterval = %q; want %q", got, want)
	}
	if got, want := display[OverrideScaledownInterval], "controller default (55m0s)"; got != want {
		t.Errorf("display scaledownInterval = %q; want %q", got, want)
	}
	// The comparison map keeps plain effective values.
	values := CurrentParameterValues(sa, time.Minute, 55*time.Minute)
	if got, want := values[OverrideScaleupInterval], "1m0s"; got != want {
		t.Errorf("values scaleupInterval = %q; want %q", got, want)
	}
}

func TestCurrentParameterValuesNotAllowedTimesAndAbsentTargets(t *testing.T) {
	sa := newAutoscaler(1000, 10000, 30) // total CPU target left unset
	sa.Spec.ScaleConfig.ScaledownNotAllowedTimes = []string{"0 9 * * *", "0 21 * * *"}

	values := CurrentParameterValues(sa, time.Minute, 55*time.Minute)
	// A not-allowed-times restriction must not render as "none" (unrestricted),
	// and switching to an allow-list must register as a change.
	if got, want := values[OverrideScaledownAllowedTimes], "notAllowedTimes:0 9 * * *;0 21 * * *"; got != want {
		t.Errorf("scaledownAllowedTimes = %q; want %q", got, want)
	}
	if got, want := values[OverrideTargetTotalCPU], "none"; got != want {
		t.Errorf("targetTotalCPU = %q; want %q", got, want)
	}
	if got, want := values[OverrideTargetHighPriorityCPU], "30"; got != want {
		t.Errorf("targetHighPriorityCPU = %q; want %q", got, want)
	}
	// Enabling an absent target counts toward the change budget.
	c := Candidate{Overrides: map[string]string{OverrideTargetTotalCPU: "80"}}
	if got := changedParameterCount(values, c); got != 1 {
		t.Errorf("changedParameterCount(enable total target) = %d; want 1", got)
	}
}

func TestRecommendedIndexKeepsCurrentWhenOverChangeBudget(t *testing.T) {
	base := Summary{SimPUHours: 10000, PUHoursSavedPercent: 0}
	current := map[string]string{OverrideMinPU: "20000", OverrideScaledownStepSize: "10%"}
	// The only candidate cheaper than base changes two parameters.
	candidates := []Candidate{{
		Feasible: true,
		Overrides: map[string]string{
			OverrideMinPU:             "15000",
			OverrideScaledownStepSize: "20%",
		},
		Summary: Summary{SimPUHours: 9000, PUHoursSavedPercent: 10},
	}}

	recIdx, furtherIdx, reason := RecommendedIndex(base, current, candidates, 1.0, 1)
	if recIdx != -1 || furtherIdx != -1 {
		t.Errorf("RecommendedIndex = (%d, %d); want keep-current when nothing fits the change budget", recIdx, furtherIdx)
	}
	if !strings.Contains(reason, "changes more than 1 parameter") {
		t.Errorf("keepReason = %q; want it to explain the change budget", reason)
	}
}

func TestRecommendGapConstraintRejectsMissingMetric(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// High-priority CPU only: enabling a total-CPU target turns every tick
	// into a data gap.
	points := constantWorkloadPoints(start, 60, 5000, 400)

	base := Config{Autoscaler: newAutoscaler(5000, 10000, 30)}
	space := SearchSpace{TargetTotalCPUs: []int{80}}
	constraints := Constraints{MaxGapMinutes: new(0.0)}

	candidates, err := Recommend(base, space, constraints, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d; want 1", len(candidates))
	}
	c := candidates[0]
	if c.Feasible {
		t.Fatalf("candidate feasible = true; want infeasible when the enabled metric is absent from the recording")
	}
	found := slices.ContainsFunc(c.InfeasibleReasons, func(r string) bool {
		return strings.Contains(r, "data gaps")
	})
	if !found {
		t.Errorf("InfeasibleReasons = %v; want a data-gap reason", c.InfeasibleReasons)
	}
}

func TestRecommendNoneAllowedTimesClearsBlocklist(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 10, 5000, 400)

	sa := newAutoscaler(5000, 10000, 30)
	sa.Spec.ScaleConfig.ScaledownNotAllowedTimes = []string{"0 9 * * *"}
	space := SearchSpace{ScaledownAllowedTimes: [][]string{{}}} // "none" = unrestricted

	candidates, err := Recommend(Config{Autoscaler: sa}, space, Constraints{}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d; want 1", len(candidates))
	}
	c := candidates[0]
	if got := c.Overrides[OverrideScaledownAllowedTimes]; got != "none" {
		t.Errorf("override = %q; want %q", got, "none")
	}
	// The "none" candidate claims unrestricted scale-down; the blocklist must
	// not survive into the replayed spec.
	if got := c.Autoscaler.Spec.ScaleConfig.ScaledownNotAllowedTimes; len(got) != 0 {
		t.Errorf("ScaledownNotAllowedTimes = %v; want cleared for the none candidate", got)
	}
}

func TestGroupEquivalent(t *testing.T) {
	mkSummary := func(cost float64) Summary { return Summary{SimPUHours: cost} }
	candidates := []Candidate{
		{Feasible: true, Summary: mkSummary(100)},
		{Feasible: true, Summary: mkSummary(100)}, // same outcome as 0
		{Feasible: true, Summary: mkSummary(200)},
		{Feasible: false, Summary: mkSummary(100)}, // same numbers, different feasibility
	}
	groups := GroupEquivalent(candidates)
	want := [][]int{{0, 1}, {2}, {3}}
	if len(groups) != len(want) {
		t.Fatalf("groups = %v; want %v", groups, want)
	}
	for i := range want {
		if !slices.Equal(groups[i], want[i]) {
			t.Errorf("group %d = %v; want %v", i, groups[i], want[i])
		}
	}
}

func TestRecommendRejectsInvalidRange(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 10, 5000, 400)

	base := Config{Autoscaler: newAutoscaler(5000, 10000, 30)}
	space := SearchSpace{MinPUs: []int{20000}} // above max 10000

	candidates, err := Recommend(base, space, Constraints{}, points)
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Error == "" || candidates[0].Feasible {
		t.Errorf("candidates = %+v; want a single infeasible candidate with an error", candidates)
	}
}

func TestRecommendGridLimit(t *testing.T) {
	base := Config{Autoscaler: newAutoscaler(1000, 10000, 30)}
	minPUs := make([]int, maxRecommendCombinations+1)
	for i := range minPUs {
		minPUs[i] = 1000
	}
	if _, err := Recommend(base, SearchSpace{MinPUs: minPUs}, Constraints{}, []Point{{}}); err == nil {
		t.Fatal("Recommend accepted an oversized grid; want an error")
	}
}
