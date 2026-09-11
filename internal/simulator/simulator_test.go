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
	advice := s.MinPUAdvice()
	if len(advice) != 1 {
		t.Fatalf("MinPUAdvice = %v; want exactly the lower-min hint", advice)
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
	hasRaiseHint := false
	for _, a := range ss.MinPUAdvice() {
		if strings.Contains(a, "spikes start from the min") {
			hasRaiseHint = true
		}
	}
	if !hasRaiseHint {
		t.Errorf("MinPUAdvice = %v; want the spikes-start-from-min hint", ss.MinPUAdvice())
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
