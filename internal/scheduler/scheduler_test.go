package scheduler

import (
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// TestJobRun_PropagatesMaxPUPolicy verifies that Job.Run copies the
// schedule's maxPUPolicy onto the ActiveSchedule status entry, normalizing
// an unset policy to the Exceed default.
func TestJobRun_PropagatesMaxPUPolicy(t *testing.T) {
	tests := []struct {
		name       string
		specPolicy spannerv1beta1.MaxPUPolicy
		wantPolicy spannerv1beta1.MaxPUPolicy
	}{
		{name: "empty policy is normalized to Exceed", specPolicy: "", wantPolicy: spannerv1beta1.MaxPUPolicyExceed},
		{name: "Exceed is propagated as-is", specPolicy: spannerv1beta1.MaxPUPolicyExceed, wantPolicy: spannerv1beta1.MaxPUPolicyExceed},
		{name: "Cap is propagated as-is", specPolicy: spannerv1beta1.MaxPUPolicyCap, wantPolicy: spannerv1beta1.MaxPUPolicyCap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := spannerv1beta1.AddToScheme(scheme); err != nil {
				t.Fatalf("failed to add scheme: %v", err)
			}

			schedNN := types.NamespacedName{Namespace: "default", Name: "test-schedule"}
			saNN := types.NamespacedName{Namespace: "default", Name: "test-sa"}

			sched := &spannerv1beta1.SpannerAutoscaleSchedule{
				ObjectMeta: metav1.ObjectMeta{Namespace: schedNN.Namespace, Name: schedNN.Name},
				Spec: spannerv1beta1.SpannerAutoscaleScheduleSpec{
					TargetResource:            saNN.Name,
					AdditionalProcessingUnits: 2000,
					MaxPUPolicy:               tt.specPolicy,
					Schedule: spannerv1beta1.Schedule{
						Cron:     "* * * * *",
						Duration: "1h",
					},
				},
			}
			sa := &spannerv1beta1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Namespace: saNN.Namespace, Name: saNN.Name},
			}

			fc := fakeclient.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(sched, sa).
				WithStatusSubresource(&spannerv1beta1.SpannerAutoscaler{}).
				Build()

			job := Job{
				ScheduleName:   schedNN,
				AutoscalerName: saNN,
				Cron:           sched.Spec.Schedule.Cron,
				Log:            logr.Discard(),
				CtrlClient:     fc,
			}
			job.Run()

			var got spannerv1beta1.SpannerAutoscaler
			if err := fc.Get(t.Context(), saNN, &got); err != nil {
				t.Fatalf("failed to get autoscaler: %v", err)
			}
			if len(got.Status.CurrentlyActiveSchedules) != 1 {
				t.Fatalf("expected 1 active schedule, got %d", len(got.Status.CurrentlyActiveSchedules))
			}
			as := got.Status.CurrentlyActiveSchedules[0]
			if as.MaxPUPolicy != tt.wantPolicy {
				t.Errorf("MaxPUPolicy = %q, want %q", as.MaxPUPolicy, tt.wantPolicy)
			}
			if as.AdditionalPU != 2000 {
				t.Errorf("AdditionalPU = %d, want 2000", as.AdditionalPU)
			}
		})
	}
}
