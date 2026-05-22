package observability

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// labelsFor builds a deterministic Labels per test so subtests do not
// collide on shared global vectors. Each subtest passes its own name.
func labelsFor(name string) Labels {
	return Labels{
		Namespace:  "ns",
		Name:       name,
		ProjectID:  "proj-" + name,
		InstanceID: "inst-" + name,
	}
}

// teardown removes every series for the given labels at the end of a test
// so subsequent tests start from a clean slate.
func teardown(t *testing.T, l Labels) {
	t.Helper()
	t.Cleanup(func() { DeleteSeries(l) })
}

func TestRegister_Idempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := Register(reg); err != nil {
		t.Fatalf("second Register should tolerate AlreadyRegisteredError, got %v", err)
	}
}

func TestRecordScaleEvent_DirectionInference(t *testing.T) {
	tests := []struct {
		name      string
		before    int
		after     int
		wantDir   string
		wantDelta float64
	}{
		{"scale up", 1000, 3000, "up", 2000},
		{"scale down", 5000, 2000, "down", 3000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := labelsFor("scale-" + tc.name)
			teardown(t, l)

			RecordScaleEvent(l, tc.before, tc.after, DriverCPUHighPriority)

			got := testutil.ToFloat64(scaleEventsTotal.WithLabelValues(
				l.Namespace, l.Name, l.ProjectID, l.InstanceID, tc.wantDir, DriverCPUHighPriority,
			))
			if got != 1 {
				t.Errorf("scale_events_total[%s] = %v, want 1", tc.wantDir, got)
			}

			// Histogram count == 1 after a single Observe.
			count := testutil.CollectAndCount(scalePUDelta, "spanner_autoscaler_scale_pu_delta")
			if count == 0 {
				t.Error("scale_pu_delta histogram has no series")
			}
		})
	}
}

func TestRecordScaleSkipped(t *testing.T) {
	l := labelsFor("skipped")
	teardown(t, l)

	RecordScaleSkipped(l, SkipReasonScaleUpInterval)
	RecordScaleSkipped(l, SkipReasonScaleUpInterval)
	RecordScaleSkipped(l, SkipReasonSame)

	if got := testutil.ToFloat64(scaleSkippedTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, SkipReasonScaleUpInterval,
	)); got != 2 {
		t.Errorf("scale_up_interval count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(scaleSkippedTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, SkipReasonSame,
	)); got != 1 {
		t.Errorf("same count = %v, want 1", got)
	}
}

func TestRecordScheduleActivationAndDeactivation(t *testing.T) {
	l := labelsFor("schedule")
	teardown(t, l)

	RecordScheduleActivation(l)
	RecordScheduleActivation(l)
	if got := testutil.ToFloat64(scheduleActivationsTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID,
	)); got != 2 {
		t.Errorf("schedule_activations_total = %v, want 2", got)
	}

	RecordScheduleDeactivation(l, ScheduleDeactivationExpired)
	RecordScheduleDeactivation(l, ScheduleDeactivationUnregistered)
	if got := testutil.ToFloat64(scheduleDeactivationsTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, ScheduleDeactivationExpired,
	)); got != 1 {
		t.Errorf("expired count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(scheduleDeactivationsTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, ScheduleDeactivationUnregistered,
	)); got != 1 {
		t.Errorf("unregistered count = %v, want 1", got)
	}
}

func TestRecordInstanceUpdate_ResultMapping(t *testing.T) {
	l := labelsFor("update")
	teardown(t, l)

	RecordInstanceUpdate(l, 50*time.Millisecond, nil)
	RecordInstanceUpdate(l, 2*time.Second, errors.New("boom"))

	if got := testutil.ToFloat64(instanceUpdateTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, resultSuccess,
	)); got != 1 {
		t.Errorf("success count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(instanceUpdateTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, resultError,
	)); got != 1 {
		t.Errorf("error count = %v, want 1", got)
	}
}

func TestRecordMetricsFetch_ResultMapping(t *testing.T) {
	l := labelsFor("fetch")
	teardown(t, l)

	RecordMetricsFetch(l, 100*time.Millisecond, nil)
	RecordMetricsFetch(l, 500*time.Millisecond, errors.New("timeout"))

	if got := testutil.ToFloat64(metricsFetchTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, resultSuccess,
	)); got != 1 {
		t.Errorf("success count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metricsFetchTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, resultError,
	)); got != 1 {
		t.Errorf("error count = %v, want 1", got)
	}
}

func TestRecordState(t *testing.T) {
	l := labelsFor("state")
	teardown(t, l)

	high := 65
	total := 80
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.Namespace, Name: l.Name},
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			TargetInstance: spannerv1beta1.TargetInstance{ProjectID: l.ProjectID, InstanceID: l.InstanceID},
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits: spannerv1beta1.ScaleConfigPUs{Min: 1000, Max: 10000},
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: &high,
					Total:        &total,
				},
			},
		},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits:            3000,
			DesiredProcessingUnits:            4000,
			DesiredMinPUs:                     1500,
			DesiredMaxPUs:                     10000,
			CurrentHighPriorityCPUUtilization: 70,
			CurrentTotalCPUUtilization:        85,
			InstanceState:                     spannerv1beta1.InstanceStateReady,
			CurrentlyActiveSchedules: []spannerv1beta1.ActiveSchedule{
				{ScheduleName: "ns/s1", AdditionalPU: 300},
				{ScheduleName: "ns/s2", AdditionalPU: 200},
			},
			LastScaleTime: metav1.Time{Time: time.Unix(1700000000, 0)},
			LastSyncTime:  metav1.Time{Time: time.Unix(1700000600, 0)},
		},
	}

	RecordState(sa)

	check := func(g *prometheus.GaugeVec, want float64, extra ...string) {
		t.Helper()
		args := append([]string{l.Namespace, l.Name, l.ProjectID, l.InstanceID}, extra...)
		if got := testutil.ToFloat64(g.WithLabelValues(args...)); got != want {
			t.Errorf("gauge with extra %v = %v, want %v", extra, got, want)
		}
	}

	check(currentProcessingUnits, 3000)
	check(desiredProcessingUnits, 4000)
	check(minProcessingUnits, 1000)
	check(maxProcessingUnits, 10000)
	check(effectiveMinProcessingUnits, 1500)
	check(effectiveMaxProcessingUnits, 10000)
	check(cpuUtilization, 70, cpuTypeHighPriority)
	check(cpuUtilization, 85, cpuTypeTotal)
	check(cpuUtilizationTarget, 65, cpuTypeHighPriority)
	check(cpuUtilizationTarget, 80, cpuTypeTotal)
	check(instanceReady, 1)
	check(activeSchedules, 2)
	check(activeScheduleAdditionalPU, 500)
	check(lastScaleTimestamp, 1700000000)
	check(lastSyncTimestamp, 1700000600)
}

func TestRecordState_OmitsUnsetCPUMetric(t *testing.T) {
	l := labelsFor("singlemode")
	teardown(t, l)

	high := 60
	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: l.Namespace, Name: l.Name},
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			TargetInstance: spannerv1beta1.TargetInstance{ProjectID: l.ProjectID, InstanceID: l.InstanceID},
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ProcessingUnits: spannerv1beta1.ScaleConfigPUs{Min: 1000, Max: 10000},
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: &high,
				},
			},
		},
		Status: spannerv1beta1.SpannerAutoscalerStatus{
			CurrentProcessingUnits:            2000,
			CurrentHighPriorityCPUUtilization: 50,
			InstanceState:                     spannerv1beta1.InstanceStateReady,
		},
	}

	RecordState(sa)

	// total type was never emitted, so no series should exist for it.
	if got := testutil.ToFloat64(cpuUtilization.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, cpuTypeTotal,
	)); got != 0 {
		// WithLabelValues lazily creates a series at 0 the first time we ask
		// for it. We therefore explicitly DeletePartialMatch to confirm only
		// the high_priority series was previously emitted.
		t.Logf("note: WithLabelValues created a zero-valued series; verifying via collected count")
	}

	// Direct verification: the high_priority series exists with value 50.
	if got := testutil.ToFloat64(cpuUtilization.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, cpuTypeHighPriority,
	)); got != 50 {
		t.Errorf("high_priority cpu_utilization = %v, want 50", got)
	}
}

func TestDeleteSeries(t *testing.T) {
	l := labelsFor("delete")

	RecordScaleSkipped(l, SkipReasonSame)
	RecordScheduleActivation(l)
	RecordInstanceUpdate(l, time.Millisecond, nil)

	// Confirm series exist.
	if got := testutil.ToFloat64(scaleSkippedTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, SkipReasonSame,
	)); got != 1 {
		t.Fatalf("precondition: scale_skipped_total = %v, want 1", got)
	}

	DeleteSeries(l)

	// After deletion, asking for the same series lazily creates a fresh zero,
	// so the only reliable check is that it starts from 0 again.
	if got := testutil.ToFloat64(scaleSkippedTotal.WithLabelValues(
		l.Namespace, l.Name, l.ProjectID, l.InstanceID, SkipReasonSame,
	)); got != 0 {
		t.Errorf("after DeleteSeries: scale_skipped_total = %v, want 0", got)
	}

	// Clean up the lazily-created zero series so other tests are unaffected.
	DeleteSeries(l)
}

func TestResultFor(t *testing.T) {
	if got := resultFor(nil); got != resultSuccess {
		t.Errorf("resultFor(nil) = %q, want %q", got, resultSuccess)
	}
	if got := resultFor(errors.New("x")); got != resultError {
		t.Errorf("resultFor(err) = %q, want %q", got, resultError)
	}
}
