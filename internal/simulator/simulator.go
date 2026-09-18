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
// This is trace-driven simulation — a backtest, not a forecast: the input is
// the actually observed workload, and what gets simulated is the
// counterfactual ("what would this configuration have done against that
// workload"). The simulated PU trace and the simulated CPU values are
// synthetic outputs that were never observed in production.
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

	// Rebuild the metric-window aggregates each tick from the simulated CPU
	// series, mirroring what the controller's metrics client computes from
	// the Cloud Monitoring point series, so CEL scaling rules and gate
	// conditions replay exactly as they would run in production.
	ws := newWindowState(sa.Spec.ScaleConfig.MetricWindows)
	celErrors := 0
	countCELError := func(err error) {
		// The warm-up period before a window has enough data is expected on
		// every replay (production skips evaluation the same way); only count
		// errors that would persist.
		if err != nil && !errors.Is(err, scaling.ErrWindowDataNotReady) {
			celErrors++
		}
	}

	for i, p := range points {
		now := p.Time
		dt := tickDuration(points, i)
		// Points are one-minute aligned; a larger gap to the next point means
		// missing samples, not sixty minutes of the current observation.
		// Attribute at most one minute to this tick and the rest to gap time.
		if dt > time.Minute {
			agg.observeMissingSpan((dt - time.Minute).Minutes())
			dt = time.Minute
		}

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
		ws.add(now, simHigh, simTotal)
		sa.Status.CurrentCPUWindowMetrics = ws.windowMetrics(flags)

		minPU, maxPU, _, _ := scaling.DesiredPURange(*sa)
		sa.Status.DesiredMinPUs = minPU
		sa.Status.DesiredMaxPUs = maxPU

		builtinDesired := scaling.DesiredProcessingUnits(*sa)
		desired, ruleOutcomes := scaling.EvaluateScalingRules(sa, builtinDesired, now)
		for _, oc := range ruleOutcomes {
			countCELError(oc.Err)
		}
		decision, gates, err := scaling.Decide(sa, desired, now, scaleUpInterval, scaleDownInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid scale-down time restriction configuration: %w", err)
		}
		for _, gate := range gates {
			countCELError(gate.Err)
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
	result.Summary.CELErrors = celErrors
	return result, nil
}

// cpuSample is one tick's simulated CPU observation retained for window
// aggregation. CPU values are percentages; nil means the metric was not
// simulated at that tick.
type cpuSample struct {
	t     time.Time
	high  *float64
	total *float64
}

// windowState keeps the recent simulated CPU samples needed to compute the
// spec.scaleConfig.metricWindows aggregates at every tick.
type windowState struct {
	windows   []string
	durations []time.Duration
	maxWindow time.Duration
	samples   []cpuSample // ascending by time
}

func newWindowState(specWindows []string) *windowState {
	ws := &windowState{}
	ws.windows, ws.durations = scaling.ValidMetricWindowDurations(specWindows)
	for _, d := range ws.durations {
		ws.maxWindow = max(ws.maxWindow, d)
	}
	return ws
}

func (ws *windowState) add(t time.Time, high, total *float64) {
	if len(ws.durations) == 0 {
		return
	}
	ws.samples = append(ws.samples, cpuSample{t: t, high: high, total: total})
	// Drop samples older than the largest window (anchored at the newest
	// sample, matching the production aggregation).
	cutoff := t.Add(-ws.maxWindow)
	firstKept := 0
	for firstKept < len(ws.samples) && !ws.samples[firstKept].t.After(cutoff) {
		firstKept++
	}
	ws.samples = ws.samples[firstKept:]
}

// windowMetrics computes the status window aggregates from the retained
// samples with the same semantics as the production metrics client: each
// window is anchored at the newest sample, covers samples strictly newer
// than (newest - window), and is only reported once it holds a full
// window's worth of 1-minute samples for the metric. CPU percentages are
// truncated to integers exactly like the status current* CPU fields.
func (ws *windowState) windowMetrics(flags spannerv1beta1.CPUMetricFlags) []spannerv1beta1.CPUWindowMetric {
	if len(ws.durations) == 0 || len(ws.samples) == 0 {
		return nil
	}
	newest := ws.samples[len(ws.samples)-1].t

	var metrics []spannerv1beta1.CPUWindowMetric
	for i, d := range ws.durations {
		cutoff := newest.Add(-d)
		expected := int(d / time.Minute)

		var highs, totals []float64
		for _, s := range ws.samples {
			if !s.t.After(cutoff) {
				continue
			}
			if s.high != nil {
				highs = append(highs, *s.high)
			}
			if s.total != nil {
				totals = append(totals, *s.total)
			}
		}
		if flags&spannerv1beta1.CPUMetricFlagHighPriority != 0 && len(highs) >= expected {
			metrics = append(metrics, windowMetric(spannerv1beta1.CPUMetricTypeHighPriority, ws.windows[i], highs))
		}
		if flags&spannerv1beta1.CPUMetricFlagTotal != 0 && len(totals) >= expected {
			metrics = append(metrics, windowMetric(spannerv1beta1.CPUMetricTypeTotal, ws.windows[i], totals))
		}
	}
	return metrics
}

func windowMetric(metric spannerv1beta1.CPUMetricType, window string, values []float64) spannerv1beta1.CPUWindowMetric {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return spannerv1beta1.CPUWindowMetric{
		Metric: metric,
		Window: window,
		Min:    int(slices.Min(values)),
		Avg:    int(sum / float64(len(values))),
		Max:    int(slices.Max(values)),
	}
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
		// The controller only binds schedules to autoscalers in the same
		// namespace.
		if sas.Namespace != "" && sa.Namespace != "" && sas.Namespace != sa.Namespace {
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
		endTime := lastFire.Add(sr.duration)
		// Sparse points can jump past an entire schedule window; a fire whose
		// window already ended must not activate at the current tick.
		if endTime.Before(now) {
			continue
		}
		entry := spannerv1beta1.ActiveSchedule{
			ScheduleName: sr.name,
			AdditionalPU: sr.additionalPU,
			EndTime:      metav1.Time{Time: endTime},
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
// next point, or one aligned minute for the last point. The last point must
// not reuse a larger previous gap — that would invent a missing span after
// the recording ends.
func tickDuration(points []Point, i int) time.Duration {
	switch {
	case i+1 < len(points):
		return points[i+1].Time.Sub(points[i].Time)
	case i > 0:
		return min(points[i].Time.Sub(points[i-1].Time), time.Minute)
	default:
		return time.Minute
	}
}
