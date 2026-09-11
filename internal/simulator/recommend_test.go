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
}

func TestRecommendConstraintFiltersCandidates(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 6*60, 5000, 400)

	base := Config{Autoscaler: newAutoscaler(5000, 10000, 30)}
	space := SearchSpace{MinPUs: []int{1000, 5000}}

	// The min-1000 candidate settles at 2000 PU (the desired-PU rounding
	// stops there for target 30), where the simulated CPU is 20% — above a
	// 15% p99 cap. The min-5000 candidate stays at 8%.
	cap := 15.0
	candidates, err := Recommend(base, space, Constraints{MaxSimCPUP99: &cap}, points)
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
			DescribeOverrides(candidates[1].Overrides), cap)
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

func TestFillGuidelineStepCandidates(t *testing.T) {
	space := SearchSpace{
		ScaleupStepSizes: []intstr.IntOrString{intstr.FromInt(5000)},
	}
	space.FillGuidelineStepCandidates()

	if len(space.ScaledownStepSizes) != 5 || space.ScaledownStepSizes[4] != intstr.FromString("50%") {
		t.Errorf("ScaledownStepSizes = %v; want 5 percent candidates up to 50%%", space.ScaledownStepSizes)
	}
	// A dimension the caller already filled must be left untouched.
	if len(space.ScaleupStepSizes) != 1 || space.ScaleupStepSizes[0] != intstr.FromInt(5000) {
		t.Errorf("ScaleupStepSizes = %v; want the caller's single candidate preserved", space.ScaleupStepSizes)
	}
	want := []metav1.Duration{{Duration: GuidelineMinScaleGap}, {Duration: GuidelinePreferredScaleGap}}
	for _, got := range [][]metav1.Duration{space.ScaledownIntervals, space.ScaleupIntervals} {
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("intervals = %v; want %v", got, want)
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
