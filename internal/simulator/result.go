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
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"time"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// Google-recommended limits for one compute-capacity change: scale by at
// most 2x (up) / half (down) of the current processing units per operation,
// and leave at least 10 minutes between operations — 30 minutes preferred —
// so Spanner can rebalance between resizes.
const (
	GuidelineMaxPUChangeFactor = 2.0
	GuidelineMinScaleGap       = 10 * time.Minute
	GuidelinePreferredScaleGap = 30 * time.Minute
)

// Result is the outcome of one simulation run.
type Result struct {
	Summary Summary    `json:"summary"`
	Events  []Event    `json:"events"`
	Points  []SimPoint `json:"points"`
}

// Event is one simulated processing-units change.
type Event struct {
	Time   time.Time `json:"time"`
	FromPU int       `json:"fromPU"`
	ToPU   int       `json:"toPU"`
}

// SimPoint pairs one recorded point with the simulated state at that tick.
// SimPU is the processing units in effect after the tick's decision. Gap
// marks ticks where required metrics were missing and no decision was taken.
type SimPoint struct {
	Time           time.Time `json:"time"`
	ActualPU       int       `json:"actualPU"`
	SimPU          int       `json:"simPU"`
	ActualHighCPU  *float64  `json:"actualHighCPU,omitempty"`
	ActualTotalCPU *float64  `json:"actualTotalCPU,omitempty"`
	SimHighCPU     *float64  `json:"simHighCPU,omitempty"`
	SimTotalCPU    *float64  `json:"simTotalCPU,omitempty"`
	Gap            bool      `json:"gap,omitzero"`
}

// CPUStats summarizes a simulated CPU utilization series (percentages).
type CPUStats struct {
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
}

// Summary aggregates one simulation run.
type Summary struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	DataPoints int       `json:"dataPoints"`
	GapMinutes float64   `json:"gapMinutes"`

	// ActualPUHours integrates the recorded processing units over the run;
	// SimPUHours integrates the simulated ones. PUHoursSavedPercent is the
	// relative reduction (negative when the simulation costs more).
	ActualPUHours       float64 `json:"actualPUHours"`
	SimPUHours          float64 `json:"simPUHours"`
	PUHoursSavedPercent float64 `json:"puHoursSavedPercent"`

	ScaleUps   int `json:"scaleUps"`
	ScaleDowns int `json:"scaleDowns"`

	SimHighPriorityCPU *CPUStats `json:"simHighPriorityCPU,omitempty"`
	SimTotalCPU        *CPUStats `json:"simTotalCPU,omitempty"`

	// TargetExceededMinutes counts time where at least one configured
	// simulated CPU metric was above its target.
	TargetExceededMinutes float64 `json:"targetExceededMinutes"`
	// LowConfidenceMinutes counts time where the *recorded* CPU was above
	// Config.LowConfidenceCPU, i.e. where the linear workload model may not
	// hold (see the package comment).
	LowConfidenceMinutes float64 `json:"lowConfidenceMinutes"`
	// MinPinnedMinutes counts time the simulated PU sat on the effective
	// minimum (spec min raised by active schedules).
	MinPinnedMinutes float64 `json:"minPinnedMinutes"`

	// PU-change guideline indicators (see GuidelineMaxPUChangeFactor and
	// friends): ScaleStepViolations counts scale events that changed PU
	// beyond 2x/half in one operation; ScaleGapsUnder10Min and
	// ScaleGapsUnder30Min count consecutive scale events closer together
	// than the hard minimum / preferred gap.
	ScaleStepViolations int `json:"scaleStepViolations"`
	ScaleGapsUnder10Min int `json:"scaleGapsUnder10Min"`
	ScaleGapsUnder30Min int `json:"scaleGapsUnder30Min"`
}

// aggregator accumulates the summary while the replay loop runs.
type aggregator struct {
	flags            spannerv1beta1.CPUMetricFlags
	lowConfidenceCPU float64
	targetHigh       int
	targetTotal      int

	dataPoints    int
	gapMinutes    float64
	actualPUHours float64
	simPUHours    float64

	simHighValues  []float64
	simTotalValues []float64

	targetExceededMinutes float64
	lowConfidenceMinutes  float64
	minPinnedMinutes      float64
}

func newAggregator(flags spannerv1beta1.CPUMetricFlags, lowConfidenceCPU float64, targetHigh, targetTotal int) *aggregator {
	return &aggregator{
		flags:            flags,
		lowConfidenceCPU: lowConfidenceCPU,
		targetHigh:       targetHigh,
		targetTotal:      targetTotal,
	}
}

func (a *aggregator) observe(p Point, sp SimPoint, effectiveMinPU int, dt time.Duration) {
	a.observeCommon(p, sp.SimPU, dt)

	minutes := dt.Minutes()

	exceeded := false
	if a.flags&spannerv1beta1.CPUMetricFlagHighPriority != 0 && sp.SimHighCPU != nil {
		a.simHighValues = append(a.simHighValues, *sp.SimHighCPU)
		if *sp.SimHighCPU > float64(a.targetHigh) {
			exceeded = true
		}
	}
	if a.flags&spannerv1beta1.CPUMetricFlagTotal != 0 && sp.SimTotalCPU != nil {
		a.simTotalValues = append(a.simTotalValues, *sp.SimTotalCPU)
		if *sp.SimTotalCPU > float64(a.targetTotal) {
			exceeded = true
		}
	}
	if exceeded {
		a.targetExceededMinutes += minutes
	}

	if sp.SimPU <= effectiveMinPU {
		a.minPinnedMinutes += minutes
	}
}

func (a *aggregator) observeGap(p Point, simPU int, dt time.Duration) {
	a.observeCommon(p, simPU, dt)
	a.gapMinutes += dt.Minutes()
}

func (a *aggregator) observeCommon(p Point, simPU int, dt time.Duration) {
	a.dataPoints++
	hours := dt.Hours()
	a.actualPUHours += float64(p.ProcessingUnits) * hours
	a.simPUHours += float64(simPU) * hours

	if (p.HighPriorityCPU != nil && *p.HighPriorityCPU >= a.lowConfidenceCPU) ||
		(p.TotalCPU != nil && *p.TotalCPU >= a.lowConfidenceCPU) {
		a.lowConfidenceMinutes += dt.Minutes()
	}
}

func (a *aggregator) summary(start, end time.Time, events []Event) Summary {
	s := Summary{
		Start:                 start,
		End:                   end,
		DataPoints:            a.dataPoints,
		GapMinutes:            a.gapMinutes,
		ActualPUHours:         a.actualPUHours,
		SimPUHours:            a.simPUHours,
		TargetExceededMinutes: a.targetExceededMinutes,
		LowConfidenceMinutes:  a.lowConfidenceMinutes,
		MinPinnedMinutes:      a.minPinnedMinutes,
	}
	if a.actualPUHours > 0 {
		s.PUHoursSavedPercent = (a.actualPUHours - a.simPUHours) / a.actualPUHours * 100
	}
	for i, e := range events {
		if e.ToPU > e.FromPU {
			s.ScaleUps++
		} else {
			s.ScaleDowns++
		}
		if float64(e.ToPU) > float64(e.FromPU)*GuidelineMaxPUChangeFactor ||
			float64(e.ToPU) < float64(e.FromPU)/GuidelineMaxPUChangeFactor {
			s.ScaleStepViolations++
		}
		if i > 0 {
			if gap := e.Time.Sub(events[i-1].Time); gap < GuidelineMinScaleGap {
				s.ScaleGapsUnder10Min++
				s.ScaleGapsUnder30Min++
			} else if gap < GuidelinePreferredScaleGap {
				s.ScaleGapsUnder30Min++
			}
		}
	}
	if len(a.simHighValues) > 0 {
		s.SimHighPriorityCPU = cpuStats(a.simHighValues)
	}
	if len(a.simTotalValues) > 0 {
		s.SimTotalCPU = cpuStats(a.simTotalValues)
	}
	return s
}

func cpuStats(values []float64) *CPUStats {
	sorted := slices.Clone(values)
	slices.Sort(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}

	return &CPUStats{
		Mean: sum / float64(len(sorted)),
		P50:  percentile(sorted, 50),
		P95:  percentile(sorted, 95),
		P99:  percentile(sorted, 99),
		Max:  sorted[len(sorted)-1],
	}
}

// percentile returns the nearest-rank percentile of an ascending-sorted
// non-empty slice.
func percentile(sorted []float64, q float64) float64 {
	rank := int(math.Ceil(q / 100 * float64(len(sorted))))
	rank = max(rank, 1)
	return sorted[rank-1]
}

// WritePointsCSV writes the per-tick series (recorded vs simulated) so the
// run can be charted or diffed externally.
func (r *Result) WritePointsCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"time", "actual_pu", "sim_pu", "actual_high_priority_cpu", "actual_total_cpu", "sim_high_priority_cpu", "sim_total_cpu", "gap"}); err != nil {
		return err
	}
	formatCPU := func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', 2, 64)
	}
	for _, p := range r.Points {
		record := []string{
			p.Time.UTC().Format(time.RFC3339),
			strconv.Itoa(p.ActualPU),
			strconv.Itoa(p.SimPU),
			formatCPU(p.ActualHighCPU),
			formatCPU(p.ActualTotalCPU),
			formatCPU(p.SimHighCPU),
			formatCPU(p.SimTotalCPU),
			strconv.FormatBool(p.Gap),
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("writing points csv: %w", err)
	}
	return nil
}
