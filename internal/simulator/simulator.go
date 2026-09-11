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

// Package simulator replays recorded Cloud Spanner metrics against a
// candidate SpannerAutoscaler configuration and reports how the autoscaler
// would have scaled the instance, at what cost, and with what CPU headroom.
//
// The decision logic is not reimplemented: every tick calls internal/scaling,
// the same package the controller uses, so a simulated run exercises exactly
// the production behavior (desired-PU computation, step sizes, cooldown
// intervals, scale-down windows, and scheduled scaling).
//
// The counterfactual CPU utilization is derived with the same workload model
// used by the development monitoring emulator: the recorded workload
// (cpu × processing units) is assumed to be independent of the instance
// size, so cpu_sim = workload / pu_sim. That assumption breaks down when the
// recorded CPU was high enough that the workload itself was throttled by the
// instance size; ticks whose recorded CPU exceeds Config.LowConfidenceCPU
// are therefore counted separately in the summary.
package simulator

import (
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/cron"
	"github.com/mercari/spanner-autoscaler/internal/scaling"
)

// Controller-level defaults mirrored from NewSpannerAutoscalerReconciler.
const (
	DefaultScaleUpInterval   = 60 * time.Second
	DefaultScaleDownInterval = 55 * time.Minute

	// DefaultLowConfidenceCPU is the recorded CPU percentage above which the
	// linear workload model is considered unreliable (the real workload may
	// have been throttled by the instance size at that point).
	DefaultLowConfidenceCPU = 50.0
)

// Point is one recorded sample of the target instance, typically at 1-minute
// resolution. CPU values are percentages in [0, 100]; a nil CPU value means
// the metric was not recorded at that timestamp.
type Point struct {
	Time            time.Time `json:"time"`
	ProcessingUnits int       `json:"processingUnits"`
	HighPriorityCPU *float64  `json:"highPriorityCPU,omitempty"`
	TotalCPU        *float64  `json:"totalCPU,omitempty"`
}

// Config describes one simulation run.
type Config struct {
	// Autoscaler is the candidate configuration; only Spec is read.
	Autoscaler *spannerv1beta1.SpannerAutoscaler
	// Schedules are the SpannerAutoscaleSchedules attached to the autoscaler.
	Schedules []*spannerv1beta1.SpannerAutoscaleSchedule

	// InitialPU is the processing units the simulated instance starts with.
	// 0 means "start from the first recorded point's actual PU".
	InitialPU int

	// ScaleUpInterval / ScaleDownInterval are the controller-level default
	// cooldowns applied when the spec does not override them. Zero values
	// fall back to DefaultScaleUpInterval / DefaultScaleDownInterval.
	ScaleUpInterval   time.Duration
	ScaleDownInterval time.Duration

	// LowConfidenceCPU is the recorded CPU percentage above which a tick is
	// counted as low-confidence (see the package comment). Zero falls back
	// to DefaultLowConfidenceCPU.
	LowConfidenceCPU float64
}

// scheduleRuntime is one SpannerAutoscaleSchedule prepared for replay.
type scheduleRuntime struct {
	name         string
	schedule     interface{ Next(time.Time) time.Time }
	duration     time.Duration
	additionalPU int
	maxPUPolicy  spannerv1beta1.MaxPUPolicy
}

// Run replays points (sorted by time internally) against cfg and returns the
// simulation result.
func Run(cfg Config, points []Point) (*Result, error) {
	if cfg.Autoscaler == nil {
		return nil, errors.New("config: autoscaler is required")
	}
	if len(points) == 0 {
		return nil, errors.New("no metric points")
	}

	flags := cfg.Autoscaler.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags()
	if flags == 0 {
		return nil, errors.New("config: no targetCPUUtilization metric configured")
	}

	scaleUpInterval := cfg.ScaleUpInterval
	if scaleUpInterval == 0 {
		scaleUpInterval = DefaultScaleUpInterval
	}
	scaleDownInterval := cfg.ScaleDownInterval
	if scaleDownInterval == 0 {
		scaleDownInterval = DefaultScaleDownInterval
	}
	lowConfidenceCPU := cfg.LowConfidenceCPU
	if lowConfidenceCPU == 0 {
		lowConfidenceCPU = DefaultLowConfidenceCPU
	}

	schedules, err := prepareSchedules(cfg.Autoscaler, cfg.Schedules)
	if err != nil {
		return nil, err
	}

	points = slices.Clone(points)
	slices.SortFunc(points, func(a, b Point) int { return a.Time.Compare(b.Time) })

	simPU := cfg.InitialPU
	if simPU == 0 {
		simPU = points[0].ProcessingUnits
	}
	if simPU <= 0 {
		return nil, fmt.Errorf("initial processing units must be positive (got %d); set Config.InitialPU or fix the first data point", simPU)
	}

	// sa holds the simulated resource state fed into internal/scaling each
	// tick, playing the role the real Status fields play in the controller.
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.Autoscaler.Name,
			Namespace: cfg.Autoscaler.Namespace,
		},
		Spec: *cfg.Autoscaler.Spec.DeepCopy(),
	}

	result := &Result{
		Points: make([]SimPoint, 0, len(points)),
	}

	var active []spannerv1beta1.ActiveSchedule
	// Start the cron scan just before the first tick so a schedule firing
	// exactly at the first timestamp still activates.
	prevTick := points[0].Time.Add(-time.Minute)

	var targetHigh, targetTotal int
	if t := sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority; t != nil {
		targetHigh = *t
	}
	if t := sa.Spec.ScaleConfig.TargetCPUUtilization.Total; t != nil {
		targetTotal = *t
	}
	agg := newAggregator(flags, lowConfidenceCPU, targetHigh, targetTotal, sa.Spec.ScaleConfig.ProcessingUnits.Min)

	for i, p := range points {
		now := p.Time
		dt := tickDuration(points, i)

		// Expired entries are dropped the same way the scheduler's cleanup
		// does: an entry stays active through its EndTime and is removed on
		// the first tick strictly after it.
		active = slices.DeleteFunc(active, func(as spannerv1beta1.ActiveSchedule) bool {
			return as.EndTime.Time.Before(now)
		})
		active = fireSchedules(schedules, active, prevTick, now)
		prevTick = now

		simHigh, simTotal, ok := simulatedCPU(flags, p, simPU)
		sp := SimPoint{
			Time:           now,
			ActualPU:       p.ProcessingUnits,
			ActualHighCPU:  p.HighPriorityCPU,
			ActualTotalCPU: p.TotalCPU,
			SimHighCPU:     simHigh,
			SimTotalCPU:    simTotal,
		}

		if !ok {
			// Data gap: hold the current PU, take no decision (in production
			// a failed metrics sync leaves status stale and no scaling
			// happens either).
			sp.SimPU = simPU
			sp.Gap = true
			result.Points = append(result.Points, sp)
			agg.observeGap(p, simPU, dt)
			continue
		}

		sa.Status.CurrentProcessingUnits = simPU
		sa.Status.CurrentlyActiveSchedules = active
		setStatusCPU(sa, flags, simHigh, simTotal)

		minPU, maxPU, _, _ := scaling.DesiredPURange(*sa)
		sa.Status.DesiredMinPUs = minPU
		sa.Status.DesiredMaxPUs = maxPU

		desired := scaling.DesiredProcessingUnits(*sa)
		decision, err := scaling.Decide(sa, desired, now, scaleUpInterval, scaleDownInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid scale-down time restriction configuration: %w", err)
		}

		puBefore := simPU
		if decision == scaling.DecisionScale {
			result.Events = append(result.Events, Event{
				Time:   now,
				FromPU: simPU,
				ToPU:   desired,
			})
			simPU = desired
			sa.Status.LastScaleTime = metav1.Time{Time: now}
		}

		sp.SimPU = simPU
		result.Points = append(result.Points, sp)
		agg.observe(p, sp, minPU, puBefore, dt)
	}

	result.Summary = agg.summary(points[0].Time, points[len(points)-1].Time, result.Events)
	return result, nil
}

// prepareSchedules parses cron expressions and durations of the schedules
// that target the autoscaler. A schedule whose TargetResource names a
// different autoscaler is skipped.
func prepareSchedules(sa *spannerv1beta1.SpannerAutoscaler, schedules []*spannerv1beta1.SpannerAutoscaleSchedule) ([]scheduleRuntime, error) {
	prepared := make([]scheduleRuntime, 0, len(schedules))
	for _, sas := range schedules {
		if sas.Spec.TargetResource != "" && sa.Name != "" && sas.Spec.TargetResource != sa.Name {
			continue
		}
		parsed, err := cron.Parse(sas.Spec.Schedule.Cron)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: invalid cron %q: %w", sas.Name, sas.Spec.Schedule.Cron, err)
		}
		duration, err := time.ParseDuration(sas.Spec.Schedule.Duration)
		if err != nil {
			return nil, fmt.Errorf("schedule %q: invalid duration %q: %w", sas.Name, sas.Spec.Schedule.Duration, err)
		}
		name := sas.Name
		if sas.Namespace != "" {
			name = sas.Namespace + "/" + sas.Name
		}
		prepared = append(prepared, scheduleRuntime{
			name:         name,
			schedule:     parsed,
			duration:     duration,
			additionalPU: sas.Spec.AdditionalProcessingUnits,
			maxPUPolicy:  sas.Spec.MaxPUPolicy.Normalized(),
		})
	}
	return prepared, nil
}

// fireSchedules activates every schedule whose cron fires in (prev, now],
// upserting by schedule name with EndTime = fireTime + duration — the same
// semantics as scheduler.Job.Run.
func fireSchedules(schedules []scheduleRuntime, active []spannerv1beta1.ActiveSchedule, prev, now time.Time) []spannerv1beta1.ActiveSchedule {
	for _, sr := range schedules {
		var lastFire time.Time
		for next := sr.schedule.Next(prev); !next.IsZero() && !next.After(now); next = sr.schedule.Next(next) {
			lastFire = next
		}
		if lastFire.IsZero() {
			continue
		}
		entry := spannerv1beta1.ActiveSchedule{
			ScheduleName: sr.name,
			AdditionalPU: sr.additionalPU,
			EndTime:      metav1.Time{Time: lastFire.Add(sr.duration)},
			MaxPUPolicy:  sr.maxPUPolicy,
		}
		if idx := slices.IndexFunc(active, func(as spannerv1beta1.ActiveSchedule) bool {
			return as.ScheduleName == sr.name
		}); idx >= 0 {
			active[idx] = entry
		} else {
			active = append(active, entry)
		}
	}
	return active
}

// simulatedCPU converts the recorded point into the CPU percentages the
// metrics client would have observed at simPU, using the workload model
// cpu_sim = (cpu_recorded × pu_recorded) / pu_sim. ok is false when the data
// required by the configured metric flags is missing at this point.
func simulatedCPU(flags spannerv1beta1.CPUMetricFlags, p Point, simPU int) (high, total *float64, ok bool) {
	if p.ProcessingUnits <= 0 || simPU <= 0 {
		return nil, nil, false
	}
	scale := func(cpu *float64) *float64 {
		if cpu == nil {
			return nil
		}
		v := *cpu * float64(p.ProcessingUnits) / float64(simPU)
		return &v
	}
	high = scale(p.HighPriorityCPU)
	total = scale(p.TotalCPU)

	if flags&spannerv1beta1.CPUMetricFlagHighPriority != 0 && high == nil {
		return nil, nil, false
	}
	if flags&spannerv1beta1.CPUMetricFlagTotal != 0 && total == nil {
		return nil, nil, false
	}
	return high, total, true
}

// setStatusCPU populates the status CPU fields the way the syncer does,
// truncating to integer percent exactly like the production metrics client
// (int(fraction * 100)).
func setStatusCPU(sa *spannerv1beta1.SpannerAutoscaler, flags spannerv1beta1.CPUMetricFlags, high, total *float64) {
	sa.Status.CurrentHighPriorityCPUUtilization = 0
	sa.Status.CurrentTotalCPUUtilization = 0
	switch flags {
	case spannerv1beta1.CPUMetricFlagHighPriority | spannerv1beta1.CPUMetricFlagTotal:
		sa.Status.CurrentHighPriorityCPUUtilization = int(*high)
		sa.Status.CurrentTotalCPUUtilization = int(*total)
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeBoth
	case spannerv1beta1.CPUMetricFlagTotal:
		sa.Status.CurrentTotalCPUUtilization = int(*total)
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeTotal
	case spannerv1beta1.CPUMetricFlagHighPriority:
		sa.Status.CurrentHighPriorityCPUUtilization = int(*high)
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority
	}
}

// tickDuration returns how long point i's PU stays in effect: the gap to the
// next point, or the previous gap for the last point (one minute for a
// single-point series).
func tickDuration(points []Point, i int) time.Duration {
	switch {
	case i+1 < len(points):
		return points[i+1].Time.Sub(points[i].Time)
	case i > 0:
		return points[i].Time.Sub(points[i-1].Time)
	default:
		return time.Minute
	}
}
