// Package observability exposes Prometheus metrics describing spanner-autoscaler
// runtime behavior. The collectors are registered with controller-runtime's
// global metrics registry from cmd/main.go and served via the same /metrics
// endpoint as the standard controller_runtime_* / workqueue_* metrics.
//
// Every business metric carries the four identity labels (namespace, name,
// project_id, instance_id). This is deliberate over an info-metric/join model:
// downstream tools that scrape the endpoint flat (notably the Datadog Agent's
// OpenMetrics check) do not need to perform a PromQL join to attribute a
// time-series to the underlying Spanner instance. Cardinality is bounded by
// the number of SpannerAutoscaler resources because (namespace,name) and
// (project_id,instance_id) are 1:1 in practice.
package observability

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

const (
	namespaceLabel  = "namespace"
	nameLabel       = "name"
	projectIDLabel  = "project_id"
	instanceIDLabel = "instance_id"

	typeLabel      = "type"
	directionLabel = "direction"
	driverLabel    = "driver"
	reasonLabel    = "reason"
	resultLabel    = "result"

	cpuTypeHighPriority = "high_priority"
	cpuTypeTotal        = "total"

	directionUp   = "up"
	directionDown = "down"

	DriverCPUHighPriority = "cpu_high_priority"
	DriverCPUTotal        = "cpu_total"
	DriverSchedule        = "schedule"

	// Skip reasons for scale_skipped_total.
	SkipReasonSame              = "same"
	SkipReasonScaleUpInterval   = "scale_up_interval"
	SkipReasonScaleDownInterval = "scale_down_interval"
	SkipReasonScaleDownWindow   = "scale_down_window"
	SkipReasonInstanceNotReady  = "instance_not_ready"
	SkipReasonCPUNotReady       = "cpu_not_ready"

	// Schedule deactivation reasons for schedule_deactivations_total.
	ScheduleDeactivationExpired      = "expired"
	ScheduleDeactivationUnregistered = "unregistered"

	resultSuccess = "success"
	resultError   = "error"
)

// identityLabels is the ordered label set attached to every business metric.
var identityLabels = []string{namespaceLabel, nameLabel, projectIDLabel, instanceIDLabel}

// Labels groups the four identity labels for a single SpannerAutoscaler resource.
// Constructed via LabelsForAutoscaler or LabelsFor.
type Labels struct {
	Namespace  string
	Name       string
	ProjectID  string
	InstanceID string
}

// LabelsForAutoscaler extracts the identity labels from a SpannerAutoscaler.
func LabelsForAutoscaler(sa *spannerv1beta1.SpannerAutoscaler) Labels {
	return Labels{
		Namespace:  sa.Namespace,
		Name:       sa.Name,
		ProjectID:  sa.Spec.TargetInstance.ProjectID,
		InstanceID: sa.Spec.TargetInstance.InstanceID,
	}
}

// LabelsFor builds Labels from a NamespacedName plus the GCP coordinates.
// Used by call sites that only hold the NamespacedName (e.g. the syncer),
// rather than the full SpannerAutoscaler object.
func LabelsFor(nn types.NamespacedName, projectID, instanceID string) Labels {
	return Labels{
		Namespace:  nn.Namespace,
		Name:       nn.Name,
		ProjectID:  projectID,
		InstanceID: instanceID,
	}
}

// values returns the identity label values in the order declared by identityLabels.
func (l Labels) values() []string {
	return []string{l.Namespace, l.Name, l.ProjectID, l.InstanceID}
}

// partialMatch returns a prometheus.Labels map keyed by the identity labels
// only. Used with vector.DeletePartialMatch to remove every series for this
// resource regardless of any metric-specific labels (type, direction, etc.).
func (l Labels) partialMatch() prometheus.Labels {
	return prometheus.Labels{
		namespaceLabel:  l.Namespace,
		nameLabel:       l.Name,
		projectIDLabel:  l.ProjectID,
		instanceIDLabel: l.InstanceID,
	}
}

// withExtra appends additional label values to the identity values, in the
// caller-supplied order. The order must match the vector's label declaration.
func (l Labels) withExtra(extra ...string) []string {
	return append(l.values(), extra...)
}

// --- A. State gauges ---------------------------------------------------------

var (
	currentProcessingUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_current_processing_units",
		Help: "Current Spanner processing units observed in SpannerAutoscaler.status.",
	}, identityLabels)

	desiredProcessingUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_desired_processing_units",
		Help: "Desired Spanner processing units computed in the latest reconcile.",
	}, identityLabels)

	minProcessingUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_min_processing_units",
		Help: "Lower bound of processing units configured by spec.scaleConfig.processingUnits.min.",
	}, identityLabels)

	maxProcessingUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_max_processing_units",
		Help: "Upper bound of processing units configured by spec.scaleConfig.processingUnits.max.",
	}, identityLabels)

	effectiveMinProcessingUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_effective_min_processing_units",
		Help: "Effective lower bound including AdditionalPU from currently active schedules.",
	}, identityLabels)

	effectiveMaxProcessingUnits = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_effective_max_processing_units",
		Help: "Effective upper bound including AdditionalPU from currently active schedules.",
	}, identityLabels)

	cpuUtilization = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_cpu_utilization",
		Help: "Current Spanner CPU utilization percentage (0-100) by metric type.",
	}, append(identityLabels, typeLabel))

	cpuUtilizationTarget = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_cpu_utilization_target",
		Help: "Configured target Spanner CPU utilization percentage (0-100) by metric type.",
	}, append(identityLabels, typeLabel))

	instanceReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_instance_ready",
		Help: "1 when Spanner instance state is ready, 0 otherwise.",
	}, identityLabels)

	activeSchedules = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_active_schedules",
		Help: "Number of SpannerAutoscaleSchedule entries currently in effect.",
	}, identityLabels)

	activeScheduleAdditionalPU = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_active_schedule_additional_pu",
		Help: "Sum of AdditionalPU contributed by currently active schedules.",
	}, identityLabels)

	lastScaleTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_last_scale_timestamp_seconds",
		Help: "Unix timestamp of the last successful processing-units update.",
	}, identityLabels)

	lastSyncTimestamp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "spanner_autoscaler_last_sync_timestamp_seconds",
		Help: "Unix timestamp of the last successful Cloud Monitoring sync.",
	}, identityLabels)
)

// --- B. Schedule counters ----------------------------------------------------

var (
	scheduleActivationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spanner_autoscaler_schedule_activations_total",
		Help: "Number of times a SpannerAutoscaleSchedule cron has fired and added an ActiveSchedule entry.",
	}, identityLabels)

	scheduleDeactivationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spanner_autoscaler_schedule_deactivations_total",
		Help: "Number of times an ActiveSchedule entry has been removed, labeled by reason (expired|unregistered).",
	}, append(identityLabels, reasonLabel))
)

// --- C. Scaling event counters and histogram ---------------------------------

var (
	scaleEventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spanner_autoscaler_scale_events_total",
		Help: "Number of processing-units updates, labeled by direction (up|down) and driver (cpu_high_priority|cpu_total|schedule).",
	}, append(identityLabels, directionLabel, driverLabel))

	scaleSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spanner_autoscaler_scale_skipped_total",
		Help: "Number of reconciles in which scaling was skipped, labeled by reason.",
	}, append(identityLabels, reasonLabel))

	scalePUDelta = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "spanner_autoscaler_scale_pu_delta",
		Help:    "Absolute processing-units change per scale event, labeled by direction.",
		Buckets: []float64{100, 200, 500, 1000, 2000, 5000, 10000, 20000},
	}, append(identityLabels, directionLabel))
)

// --- D. Operational counters and histograms ---------------------------------

var (
	instanceUpdateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spanner_autoscaler_instance_update_total",
		Help: "Spanner UpdateInstance API call count, labeled by result (success|error).",
	}, append(identityLabels, resultLabel))

	instanceUpdateDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "spanner_autoscaler_instance_update_duration_seconds",
		Help:    "Spanner UpdateInstance API call latency in seconds.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60},
	}, identityLabels)

	metricsFetchTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "spanner_autoscaler_metrics_fetch_total",
		Help: "Cloud Monitoring GetInstanceMetrics call count, labeled by result (success|error).",
	}, append(identityLabels, resultLabel))

	metricsFetchDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "spanner_autoscaler_metrics_fetch_duration_seconds",
		Help:    "Cloud Monitoring GetInstanceMetrics call latency in seconds.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, identityLabels)
)

// allCollectors returns every collector defined in this package. Used by
// Register and by DeleteSeries to iterate vectors uniformly.
func allCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		currentProcessingUnits,
		desiredProcessingUnits,
		minProcessingUnits,
		maxProcessingUnits,
		effectiveMinProcessingUnits,
		effectiveMaxProcessingUnits,
		cpuUtilization,
		cpuUtilizationTarget,
		instanceReady,
		activeSchedules,
		activeScheduleAdditionalPU,
		lastScaleTimestamp,
		lastSyncTimestamp,
		scheduleActivationsTotal,
		scheduleDeactivationsTotal,
		scaleEventsTotal,
		scaleSkippedTotal,
		scalePUDelta,
		instanceUpdateTotal,
		instanceUpdateDurationSeconds,
		metricsFetchTotal,
		metricsFetchDurationSeconds,
	}
}

// allVectors returns every vector as a deleter, so DeleteSeries can erase a
// resource's series uniformly. Plain prometheus.Collector lacks the
// DeletePartialMatch method, so we need this narrower interface.
type partialDeleter interface {
	DeletePartialMatch(prometheus.Labels) int
}

func allVectors() []partialDeleter {
	return []partialDeleter{
		currentProcessingUnits,
		desiredProcessingUnits,
		minProcessingUnits,
		maxProcessingUnits,
		effectiveMinProcessingUnits,
		effectiveMaxProcessingUnits,
		cpuUtilization,
		cpuUtilizationTarget,
		instanceReady,
		activeSchedules,
		activeScheduleAdditionalPU,
		lastScaleTimestamp,
		lastSyncTimestamp,
		scheduleActivationsTotal,
		scheduleDeactivationsTotal,
		scaleEventsTotal,
		scaleSkippedTotal,
		scalePUDelta,
		instanceUpdateTotal,
		instanceUpdateDurationSeconds,
		metricsFetchTotal,
		metricsFetchDurationSeconds,
	}
}

// Register installs every collector in reg. Tolerates AlreadyRegisteredError
// so repeated registration (e.g. across test cases that share the global
// controller-runtime registry) is a no-op rather than a panic.
func Register(reg prometheus.Registerer) error {
	for _, c := range allCollectors() {
		if err := reg.Register(c); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
				continue
			}
			return err
		}
	}
	return nil
}

// RecordState writes every state gauge based on the current SpannerAutoscaler.
// Intended to be called once per Reconcile (after Status mutations and before
// returning) so dashboards reflect the snapshot the controller acted on.
func RecordState(sa *spannerv1beta1.SpannerAutoscaler) {
	l := LabelsForAutoscaler(sa)
	vals := l.values()

	currentProcessingUnits.WithLabelValues(vals...).Set(float64(sa.Status.CurrentProcessingUnits))
	desiredProcessingUnits.WithLabelValues(vals...).Set(float64(sa.Status.DesiredProcessingUnits))
	minProcessingUnits.WithLabelValues(vals...).Set(float64(sa.Spec.ScaleConfig.ProcessingUnits.Min))
	maxProcessingUnits.WithLabelValues(vals...).Set(float64(sa.Spec.ScaleConfig.ProcessingUnits.Max))
	effectiveMinProcessingUnits.WithLabelValues(vals...).Set(float64(sa.Status.DesiredMinPUs))
	effectiveMaxProcessingUnits.WithLabelValues(vals...).Set(float64(sa.Status.DesiredMaxPUs))

	// Emit a CPU gauge only for metric types the spec actually activates.
	// This avoids reporting a stale 0% for the unused mode in single-metric
	// configurations (syncer also leaves the unused field at 0).
	if p := sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority; p != nil {
		cpuUtilization.WithLabelValues(l.withExtra(cpuTypeHighPriority)...).Set(float64(sa.Status.CurrentHighPriorityCPUUtilization))
		cpuUtilizationTarget.WithLabelValues(l.withExtra(cpuTypeHighPriority)...).Set(float64(*p))
	}
	if p := sa.Spec.ScaleConfig.TargetCPUUtilization.Total; p != nil {
		cpuUtilization.WithLabelValues(l.withExtra(cpuTypeTotal)...).Set(float64(sa.Status.CurrentTotalCPUUtilization))
		cpuUtilizationTarget.WithLabelValues(l.withExtra(cpuTypeTotal)...).Set(float64(*p))
	}

	var ready float64
	if sa.Status.InstanceState == spannerv1beta1.InstanceStateReady {
		ready = 1
	}
	instanceReady.WithLabelValues(vals...).Set(ready)

	var addPUSum int
	for _, as := range sa.Status.CurrentlyActiveSchedules {
		addPUSum += as.AdditionalPU
	}
	activeSchedules.WithLabelValues(vals...).Set(float64(len(sa.Status.CurrentlyActiveSchedules)))
	activeScheduleAdditionalPU.WithLabelValues(vals...).Set(float64(addPUSum))

	if !sa.Status.LastScaleTime.IsZero() {
		lastScaleTimestamp.WithLabelValues(vals...).Set(float64(sa.Status.LastScaleTime.Unix()))
	}
	if !sa.Status.LastSyncTime.IsZero() {
		lastSyncTimestamp.WithLabelValues(vals...).Set(float64(sa.Status.LastSyncTime.Unix()))
	}
}

// RecordScaleEvent records a successful processing-units change. before/after
// are the PU values; direction is derived from their relation. driver should
// be one of the Driver* constants (computed at the call site, since the
// scaling logic itself does not currently surface a per-metric breakdown).
func RecordScaleEvent(l Labels, before, after int, driver string) {
	direction := directionUp
	delta := after - before
	if delta < 0 {
		direction = directionDown
		delta = -delta
	}
	scaleEventsTotal.WithLabelValues(l.withExtra(direction, driver)...).Inc()
	scalePUDelta.WithLabelValues(l.withExtra(direction)...).Observe(float64(delta))
}

// RecordScaleSkipped records a reconcile in which the controller chose not to
// change processing units, labeled by one of the SkipReason* constants.
func RecordScaleSkipped(l Labels, reason string) {
	scaleSkippedTotal.WithLabelValues(l.withExtra(reason)...).Inc()
}

// RecordScheduleActivation increments the schedule activation counter when a
// cron fires and an ActiveSchedule entry is added.
func RecordScheduleActivation(l Labels) {
	scheduleActivationsTotal.WithLabelValues(l.values()...).Inc()
}

// RecordScheduleDeactivation increments the schedule deactivation counter,
// labeled by one of the ScheduleDeactivation* constants.
func RecordScheduleDeactivation(l Labels, reason string) {
	scheduleDeactivationsTotal.WithLabelValues(l.withExtra(reason)...).Inc()
}

// RecordInstanceUpdate records the latency and result of a Spanner
// UpdateInstance call. err == nil maps to result="success".
func RecordInstanceUpdate(l Labels, duration time.Duration, err error) {
	instanceUpdateDurationSeconds.WithLabelValues(l.values()...).Observe(duration.Seconds())
	instanceUpdateTotal.WithLabelValues(l.withExtra(resultFor(err))...).Inc()
}

// RecordMetricsFetch records the latency and result of a Cloud Monitoring
// GetInstanceMetrics call. err == nil maps to result="success".
func RecordMetricsFetch(l Labels, duration time.Duration, err error) {
	metricsFetchDurationSeconds.WithLabelValues(l.values()...).Observe(duration.Seconds())
	metricsFetchTotal.WithLabelValues(l.withExtra(resultFor(err))...).Inc()
}

// DeleteSeries removes every series associated with this resource across all
// vectors. Call from the Reconcile delete branch so that deleted resources do
// not leave stale time-series exposed on /metrics indefinitely.
func DeleteSeries(l Labels) {
	match := l.partialMatch()
	for _, v := range allVectors() {
		v.DeletePartialMatch(match)
	}
}

func resultFor(err error) string {
	if err == nil {
		return resultSuccess
	}
	return resultError
}
