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
	"bytes"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// newAutoscaler builds a minimal high-priority-target autoscaler the way the
// defaulting webhook would leave it (scaledownStepSize defaulted to 2000).
func newAutoscaler(minPU, maxPU, targetHighCPU int) *spannerv1beta1.SpannerAutoscaler {
	return &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ComputeType: spannerv1beta1.ComputeTypePU,
				ProcessingUnits: spannerv1beta1.ScaleConfigPUs{
					Min: minPU,
					Max: maxPU,
				},
				ScaledownStepSize: intstr.FromInt(2000),
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: new(targetHighCPU),
				},
			},
		},
	}
}

// constantWorkloadPoints generates n one-minute points where the recorded
// instance runs at actualPU with the CPU that the given workload
// (cpu% × PU / 100) produces at that size.
func constantWorkloadPoints(start time.Time, n int, actualPU int, workload float64) []Point {
	points := make([]Point, 0, n)
	for i := range n {
		cpu := workload / float64(actualPU) * 100
		points = append(points, Point{
			Time:            start.Add(time.Duration(i) * time.Minute),
			ProcessingUnits: actualPU,
			HighPriorityCPU: new(cpu),
		})
	}
	return points
}

func TestRunScaleUp(t *testing.T) {
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	// Workload 900 PU-equivalents: 90% CPU at 1000 PU.
	points := constantWorkloadPoints(start, 10, 1000, 900)

	result, err := Run(Config{Autoscaler: newAutoscaler(1000, 10000, 30)}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// 90% at 1000 PU with target 30 → required 3000, rounded up one unit → 4000.
	if len(result.Events) != 1 {
		t.Fatalf("events = %+v; want exactly one scale-up", result.Events)
	}
	e := result.Events[0]
	if e.FromPU != 1000 || e.ToPU != 4000 || !e.Time.Equal(start) {
		t.Errorf("event = %+v; want 1000→4000 at %v", e, start)
	}
	// After the scale-up the scale-down cooldown (55m default) holds 4000.
	if got := result.Points[len(result.Points)-1].SimPU; got != 4000 {
		t.Errorf("final SimPU = %d; want 4000", got)
	}
	if result.Summary.ScaleUps != 1 || result.Summary.ScaleDowns != 0 {
		t.Errorf("summary scaleUps/scaleDowns = %d/%d; want 1/0", result.Summary.ScaleUps, result.Summary.ScaleDowns)
	}
	// The recorded CPU (90%) is above the low-confidence threshold for every
	// tick, so the whole run must be flagged.
	if result.Summary.LowConfidenceMinutes == 0 {
		t.Error("LowConfidenceMinutes = 0; want > 0 for a 90% CPU recording")
	}
}

func TestRunScaledownWindowAndStepSize(t *testing.T) {
	// An over-provisioned instance idling at single-digit
	// CPU, a scale-down window opening at 13:00, and a fixed step size with a
	// shortened scale-down interval.
	sa := newAutoscaler(1000, 10000, 40)
	sa.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(1000)
	sa.Spec.ScaleConfig.ScaledownInterval = &metav1.Duration{Duration: 10 * time.Minute}
	sa.Spec.ScaleConfig.ScaledownAllowedTimes = []string{"* 13-23 * * *"}

	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Workload 400: 8% CPU at 5000 PU.
	points := constantWorkloadPoints(start, 121, 5000, 400)

	result, err := Run(Config{Autoscaler: sa}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	windowOpen := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	for _, e := range result.Events {
		if e.Time.Before(windowOpen) {
			t.Errorf("scale event %+v before the 13:00 window opened", e)
		}
	}

	// 5000 → 4000 → 3000 → 2000, one step per scaledownInterval. At 2000 PU
	// the simulated CPU is 20%, whose desired PU rounds back up to 2000, so
	// the ramp stops there.
	wantEvents := []Event{
		{Time: windowOpen, FromPU: 5000, ToPU: 4000},
		{Time: windowOpen.Add(10 * time.Minute), FromPU: 4000, ToPU: 3000},
		{Time: windowOpen.Add(20 * time.Minute), FromPU: 3000, ToPU: 2000},
	}
	if len(result.Events) != len(wantEvents) {
		t.Fatalf("events = %+v; want %+v", result.Events, wantEvents)
	}
	for i, want := range wantEvents {
		got := result.Events[i]
		if !got.Time.Equal(want.Time) || got.FromPU != want.FromPU || got.ToPU != want.ToPU {
			t.Errorf("event[%d] = %+v; want %+v", i, got, want)
		}
	}

	if got := result.Points[len(result.Points)-1].SimPU; got != 2000 {
		t.Errorf("final SimPU = %d; want 2000", got)
	}
	// The simulation must be cheaper than the recorded flat 5000 PU.
	if result.Summary.SimPUHours >= result.Summary.ActualPUHours {
		t.Errorf("SimPUHours = %f >= ActualPUHours = %f; want savings",
			result.Summary.SimPUHours, result.Summary.ActualPUHours)
	}
	if result.Summary.TargetExceededMinutes != 0 {
		t.Errorf("TargetExceededMinutes = %f; want 0", result.Summary.TargetExceededMinutes)
	}
}

func TestRunScheduleRaisesMinimum(t *testing.T) {
	sa := newAutoscaler(1000, 10000, 30)
	schedule := &spannerv1beta1.SpannerAutoscaleSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "morning-batch", Namespace: "default"},
		Spec: spannerv1beta1.SpannerAutoscaleScheduleSpec{
			TargetResource:            "test",
			AdditionalProcessingUnits: 2000,
			Schedule: spannerv1beta1.Schedule{
				Cron:     "0 9 * * *",
				Duration: "2h",
			},
		},
	}

	start := time.Date(2026, 9, 1, 8, 50, 0, 0, time.UTC)
	// Workload 50: 5% CPU at 1000 PU — CPU alone never asks for more PU.
	points := constantWorkloadPoints(start, 145, 1000, 50) // 08:50–11:14

	result, err := Run(Config{
		Autoscaler: sa,
		Schedules:  []*spannerv1beta1.SpannerAutoscaleSchedule{schedule},
	}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	nineAM := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	elevenAM := nineAM.Add(2 * time.Hour)

	puAt := func(at time.Time) int {
		for _, p := range result.Points {
			if p.Time.Equal(at) {
				return p.SimPU
			}
		}
		t.Fatalf("no point at %v", at)
		return 0
	}

	if got := puAt(nineAM.Add(-time.Minute)); got != 1000 {
		t.Errorf("SimPU before schedule = %d; want 1000", got)
	}
	// min 1000 + additional 2000 = 3000 while the schedule is active.
	if got := puAt(nineAM); got != 3000 {
		t.Errorf("SimPU at schedule start = %d; want 3000", got)
	}
	if got := puAt(elevenAM); got != 3000 {
		t.Errorf("SimPU at schedule end (inclusive) = %d; want 3000", got)
	}
	// The entry expires strictly after EndTime; the scale-down back to the
	// spec minimum happens on the next tick (cooldown elapsed long ago).
	if got := puAt(elevenAM.Add(time.Minute)); got != 1000 {
		t.Errorf("SimPU after schedule expiry = %d; want 1000", got)
	}
}

func TestMinPUSignals(t *testing.T) {
	// A grossly over-provisioned floor: pinned at min 5000 the whole run
	// while the workload only needs 400/40% → 1000 PU (rounded up to 2000).
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 120, 5000, 400)

	result, err := Run(Config{Autoscaler: newAutoscaler(5000, 10000, 40)}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := result.Summary
	if s.SpecMinPU != 5000 {
		t.Errorf("SpecMinPU = %d; want 5000", s.SpecMinPU)
	}
	if s.MinPinnedPercent != 100 {
		t.Errorf("MinPinnedPercent = %.1f; want 100", s.MinPinnedPercent)
	}
	if s.RequiredPUAtMinP95 != 2000 {
		t.Errorf("RequiredPUAtMinP95 = %d; want 2000 (workload 400 at target 40, rounded up)", s.RequiredPUAtMinP95)
	}
	assessment := s.AssessMinPU()
	if !strings.Contains(assessment.Lower, "possible down to ~2000") {
		t.Errorf("AssessMinPU().Lower = %q; want a lower-possible verdict around 2000", assessment.Lower)
	}
	if !strings.Contains(assessment.Raise, "not indicated") {
		t.Errorf("AssessMinPU().Raise = %q; want not-indicated (no overshoot)", assessment.Raise)
	}

	// The spike scenario: the whole overshoot is observed while sitting at
	// the min, so the raise/pre-scale hint must fire.
	spikePoints := constantWorkloadPoints(start, 10, 1000, 900)
	spikeResult, err := Run(Config{Autoscaler: newAutoscaler(1000, 10000, 30)}, spikePoints)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ss := spikeResult.Summary
	if ss.TargetExceededAtMinMinutes == 0 || ss.TargetExceededAtMinMinutes != ss.TargetExceededMinutes {
		t.Errorf("TargetExceededAtMinMinutes = %.0f (total %.0f); want all overshoot attributed to the min",
			ss.TargetExceededAtMinMinutes, ss.TargetExceededMinutes)
	}
	if raise := ss.AssessMinPU().Raise; !strings.Contains(raise, "consider raising or pre-scaling") {
		t.Errorf("AssessMinPU().Raise = %q; want the raise/pre-scale verdict", raise)
	}
}

func TestRunGapHoldsPU(t *testing.T) {
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 3, 1000, 900)
	// Second point loses its metric: the simulator must hold PU and mark a gap.
	points[1].HighPriorityCPU = nil

	result, err := Run(Config{Autoscaler: newAutoscaler(1000, 10000, 30)}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Points[1].Gap {
		t.Error("point[1].Gap = false; want true")
	}
	if result.Points[1].SimPU != result.Points[0].SimPU {
		t.Errorf("SimPU changed across a gap: %d → %d", result.Points[0].SimPU, result.Points[1].SimPU)
	}
	if result.Summary.GapMinutes == 0 {
		t.Error("GapMinutes = 0; want > 0")
	}
}

func TestRunMissingSpanCountsAsGap(t *testing.T) {
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 2, 1000, 50)
	// A one-hour hole in the recording: the next two points resume at +61m.
	points = append(points,
		constantWorkloadPoints(start.Add(61*time.Minute), 2, 1000, 50)...)

	result, err := Run(Config{Autoscaler: newAutoscaler(1000, 10000, 30)}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := result.Summary
	// The missing 59 minutes count as gap time, not as observation.
	if s.GapMinutes != 59 {
		t.Errorf("GapMinutes = %.1f; want 59", s.GapMinutes)
	}
	// PU-hours cover only the four observed minutes at 1000 PU.
	want := 1000.0 * 4 / 60
	if diff := s.SimPUHours - want; diff < -0.01 || diff > 0.01 {
		t.Errorf("SimPUHours = %.2f; want %.2f (observed minutes only)", s.SimPUHours, want)
	}
	if s.TargetExceededMinutes != 0 {
		t.Errorf("TargetExceededMinutes = %.1f; want 0", s.TargetExceededMinutes)
	}
}

func TestRunScheduleIgnoresOtherNamespace(t *testing.T) {
	sa := newAutoscaler(1000, 10000, 30)
	schedule := &spannerv1beta1.SpannerAutoscaleSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "morning-batch", Namespace: "other"},
		Spec: spannerv1beta1.SpannerAutoscaleScheduleSpec{
			TargetResource:            "test",
			AdditionalProcessingUnits: 2000,
			Schedule: spannerv1beta1.Schedule{
				Cron:     "0 9 * * *",
				Duration: "2h",
			},
		},
	}

	start := time.Date(2026, 9, 1, 8, 50, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 30, 1000, 50)

	result, err := Run(Config{
		Autoscaler: sa,
		Schedules:  []*spannerv1beta1.SpannerAutoscaleSchedule{schedule},
	}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The controller only binds schedules in the autoscaler's own namespace;
	// a name match across namespaces must not raise the minimum.
	if len(result.Events) != 0 {
		t.Errorf("events = %+v; want none for a schedule in another namespace", result.Events)
	}
}

func TestRunSparsePointsSkipExpiredSchedule(t *testing.T) {
	sa := newAutoscaler(1000, 10000, 30)
	schedule := &spannerv1beta1.SpannerAutoscaleSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "morning-batch", Namespace: "default"},
		Spec: spannerv1beta1.SpannerAutoscaleScheduleSpec{
			TargetResource:            "test",
			AdditionalProcessingUnits: 2000,
			Schedule: spannerv1beta1.Schedule{
				Cron:     "0 9 * * *",
				Duration: "2h",
			},
		},
	}

	// Only two points, jumping clean over the 09:00-11:00 window: the fire
	// observed at 12:00 already ended and must not activate.
	points := []Point{
		{Time: time.Date(2026, 9, 1, 8, 50, 0, 0, time.UTC), ProcessingUnits: 1000, HighPriorityCPU: new(5.0)},
		{Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), ProcessingUnits: 1000, HighPriorityCPU: new(5.0)},
	}

	result, err := Run(Config{
		Autoscaler: sa,
		Schedules:  []*spannerv1beta1.SpannerAutoscaleSchedule{schedule},
	}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Events) != 0 {
		t.Errorf("events = %+v; want none for a schedule window that ended before the tick", result.Events)
	}
	if got := result.Points[1].SimPU; got != 1000 {
		t.Errorf("SimPU at 12:00 = %d; want 1000", got)
	}
}

func TestRunTrailingGapNotDoubleCounted(t *testing.T) {
	// Two points one hour apart: 59 minutes are missing between them, and
	// nothing is missing after the recording ends. The last point must not
	// reuse the previous gap as its own duration.
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	points := []Point{
		{Time: start, ProcessingUnits: 1000, HighPriorityCPU: new(5.0)},
		{Time: start.Add(time.Hour), ProcessingUnits: 1000, HighPriorityCPU: new(5.0)},
	}

	result, err := Run(Config{Autoscaler: newAutoscaler(1000, 10000, 30)}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := result.Summary
	if s.GapMinutes != 59 {
		t.Errorf("GapMinutes = %.1f; want 59 (no phantom span after the last point)", s.GapMinutes)
	}
	want := 1000.0 * 2 / 60
	if diff := s.SimPUHours - want; diff < -0.01 || diff > 0.01 {
		t.Errorf("SimPUHours = %.2f; want %.2f (two observed minutes)", s.SimPUHours, want)
	}
}

func TestLoadCSVRejectsNonFiniteValues(t *testing.T) {
	header := "time,processing_units,high_priority_cpu,total_cpu\n"
	for _, row := range []string{
		"2026-09-01T09:00:00Z,1000,NaN,",
		"2026-09-01T09:00:00Z,1000,+Inf,",
		"2026-09-01T09:00:00Z,1000,-5,",
		"2026-09-01T09:00:00Z,-1000,5,",
	} {
		if _, err := LoadCSV(strings.NewReader(header + row + "\n")); err == nil {
			t.Errorf("LoadCSV(%q) = nil error; want rejection", row)
		}
	}
	// A plain valid row still loads.
	if _, err := LoadCSV(strings.NewReader(header + "2026-09-01T09:00:00Z,1000,5,\n")); err != nil {
		t.Errorf("LoadCSV(valid row): %v", err)
	}
}

func TestRunCountsWindowGapsAfterWarmUp(t *testing.T) {
	sa := newAutoscaler(1000, 10000, 70)
	sa.Spec.ScaleConfig.MetricWindows = []string{"10m"}
	sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
		// Never triggers; it exists so the 10m window is evaluated each tick.
		{When: "cpu.highPriority.min10m >= 101", ScaleUp: intstr.FromString("25%")},
	}

	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 40, 1000, 50)

	// Baseline: warm-up alone (the first ticks before the window holds ten
	// samples) must not count as CEL errors.
	result, err := Run(Config{Autoscaler: sa}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Summary.CELErrors != 0 {
		t.Fatalf("CELErrors = %d; want 0 for warm-up only", result.Summary.CELErrors)
	}

	// A mid-replay metric outage empties the window after warm-up completed;
	// production would skip the rules the same way, and the summary must
	// report it instead of staying at zero.
	for i := 20; i < 25; i++ {
		points[i].HighPriorityCPU = nil
	}
	result, err = Run(Config{Autoscaler: sa}, points)
	if err != nil {
		t.Fatalf("Run (with gap): %v", err)
	}
	if result.Summary.CELErrors == 0 {
		t.Error("CELErrors = 0; want > 0 for a window emptied by a post-warm-up gap")
	}
}

func TestCSVRoundTrip(t *testing.T) {
	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	points := []Point{
		{Time: start, ProcessingUnits: 1000, HighPriorityCPU: new(12.5), TotalCPU: new(20.25)},
		{Time: start.Add(time.Minute), ProcessingUnits: 2000, HighPriorityCPU: new(6.25)},
		{Time: start.Add(2 * time.Minute), ProcessingUnits: 2000},
	}

	var buf bytes.Buffer
	if err := WriteCSV(&buf, points); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	loaded, err := LoadCSV(&buf)
	if err != nil {
		t.Fatalf("LoadCSV: %v", err)
	}

	if len(loaded) != len(points) {
		t.Fatalf("loaded %d points; want %d", len(loaded), len(points))
	}
	for i, want := range points {
		got := loaded[i]
		if !got.Time.Equal(want.Time) || got.ProcessingUnits != want.ProcessingUnits {
			t.Errorf("point[%d] = %+v; want %+v", i, got, want)
		}
		if (got.HighPriorityCPU == nil) != (want.HighPriorityCPU == nil) ||
			(got.HighPriorityCPU != nil && *got.HighPriorityCPU != *want.HighPriorityCPU) {
			t.Errorf("point[%d].HighPriorityCPU = %v; want %v", i, got.HighPriorityCPU, want.HighPriorityCPU)
		}
		if (got.TotalCPU == nil) != (want.TotalCPU == nil) ||
			(got.TotalCPU != nil && *got.TotalCPU != *want.TotalCPU) {
			t.Errorf("point[%d].TotalCPU = %v; want %v", i, got.TotalCPU, want.TotalCPU)
		}
	}
}

func TestLoadManifests(t *testing.T) {
	manifests := []byte(`
apiVersion: v1
kind: Namespace
metadata:
  name: ignored
---
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerAutoscaler
metadata:
  name: test
  namespace: default
spec:
  targetInstance:
    projectId: my-project
    instanceId: my-instance
  scaleConfig:
    processingUnits:
      min: 1000
      max: 10000
    targetCPUUtilization:
      highPriority: 30
---
apiVersion: spanner.mercari.com/v1beta1
kind: SpannerAutoscaleSchedule
metadata:
  name: morning-batch
  namespace: default
spec:
  targetResource: test
  additionalProcessingUnits: 2000
  schedule:
    cron: "0 9 * * *"
    duration: "2h"
`)

	sa, schedules, err := LoadManifests(manifests)
	if err != nil {
		t.Fatalf("LoadManifests: %v", err)
	}
	if sa.Name != "test" {
		t.Errorf("autoscaler name = %q; want %q", sa.Name, "test")
	}
	// The production defaulting webhook must have been applied.
	if got := sa.Spec.ScaleConfig.ScaledownStepSize; got != intstr.FromInt(2000) {
		t.Errorf("defaulted ScaledownStepSize = %v; want 2000", got)
	}
	if len(schedules) != 1 || schedules[0].Name != "morning-batch" {
		t.Errorf("schedules = %+v; want one named morning-batch", schedules)
	}
}

func TestRunScalingRule_SustainedCPU(t *testing.T) {
	// Built-in logic sees 55% against a 70% target and never scales; the
	// trigger rule fires once the CPU has stayed >= 50% for 15 minutes.
	sa := newAutoscaler(1000, 10000, 70)
	sa.Spec.ScaleConfig.MetricWindows = []string{"15m"}
	sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
		{When: "cpu.highPriority.min15m >= 50", ScaleUp: intstr.FromString("100%")},
	}

	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	// Workload 550: 55% CPU at 1000 PU.
	points := constantWorkloadPoints(start, 30, 1000, 550)

	result, err := Run(Config{Autoscaler: sa}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The window needs 15 one-minute samples, so the rule can first fire at
	// the 15th tick (start + 14m). Doubling to 2000 halves the CPU to 27.5%,
	// which immediately drops min15m below 50, so it fires exactly once.
	if len(result.Events) != 1 {
		t.Fatalf("events = %+v; want exactly one rule-driven scale-up", result.Events)
	}
	e := result.Events[0]
	wantTime := start.Add(14 * time.Minute)
	if e.FromPU != 1000 || e.ToPU != 2000 || !e.Time.Equal(wantTime) {
		t.Errorf("event = %+v; want 1000→2000 at %v (after 15 sustained minutes)", e, wantTime)
	}
	if result.Summary.CELErrors != 0 {
		t.Errorf("CELErrors = %d; want 0 (window warm-up is not an error)", result.Summary.CELErrors)
	}
}

func TestRunScaledownCondition_GatesUntilWindowProvesQuiet(t *testing.T) {
	// An oversized instance at 8% CPU. The gate allows scale-down only once
	// the last 15 minutes prove quiet; during the window warm-up it fails
	// closed, so the first scale-down shifts from the first tick to the 15th.
	sa := newAutoscaler(1000, 10000, 40)
	sa.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(1000)
	sa.Spec.ScaleConfig.MetricWindows = []string{"15m"}
	sa.Spec.ScaleConfig.ScaledownCondition = "cpu.highPriority.max15m < 10"

	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	// Workload 400: 8% CPU at 5000 PU.
	points := constantWorkloadPoints(start, 20, 5000, 400)

	result, err := Run(Config{Autoscaler: sa}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(result.Events) == 0 {
		t.Fatal("no scale-down happened; the gate should open once the window is quiet")
	}
	e := result.Events[0]
	wantTime := start.Add(14 * time.Minute)
	if e.FromPU != 5000 || e.ToPU != 4000 || !e.Time.Equal(wantTime) {
		t.Errorf("first event = %+v; want 5000→4000 at %v (gate closed during warm-up)", e, wantTime)
	}
	if result.Summary.CELErrors != 0 {
		t.Errorf("CELErrors = %d; want 0 (window warm-up is not an error)", result.Summary.CELErrors)
	}
}

func TestRunScalingRule_BrokenExpressionIsCounted(t *testing.T) {
	// A rule referencing a window that is not declared cannot compile; the
	// replay must skip it fail-safe (no scaling) and report it via CELErrors.
	sa := newAutoscaler(1000, 10000, 70)
	sa.Spec.ScaleConfig.MetricWindows = []string{"15m"}
	sa.Spec.ScaleConfig.ScalingRules = []spannerv1beta1.ScalingRule{
		{When: "cpu.highPriority.min30m >= 50", ScaleUp: intstr.FromString("100%")},
	}

	start := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	points := constantWorkloadPoints(start, 20, 1000, 550)

	result, err := Run(Config{Autoscaler: sa}, points)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Events) != 0 {
		t.Errorf("events = %+v; want none (broken rule is skipped)", result.Events)
	}
	if result.Summary.CELErrors == 0 {
		t.Error("CELErrors = 0; want > 0 for a rule that cannot compile")
	}
}
