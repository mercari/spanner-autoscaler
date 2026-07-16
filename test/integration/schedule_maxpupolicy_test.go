//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/controller"
)

// TestController_E2E_ScheduleMaxPUPolicyCap verifies that a schedule with
// maxPUPolicy: Cap raises only the lower bound of the autoscaling range:
// under sustained CPU pressure that always wants more capacity, the instance
// must never be scaled beyond spec.processingUnits.max even while the
// schedule is active.
func TestController_E2E_ScheduleMaxPUPolicyCap(t *testing.T) {
	const (
		projectID         = "cappolicy-project"
		instanceID        = "cappolicy-instance"
		initPU            = 60000
		minPU             = 1000
		maxPU             = 60000
		additionalPU      = 20000
		syncInterval      = 1 * time.Second
		scaleUpInterval   = 1 * time.Second
		scaleDownInterval = 55 * time.Minute
		activateWithin    = 90 * time.Second
		observeFor        = 90 * time.Second
	)
	targetCPUVal := 40
	targetCPU := &targetCPUVal
	skipValidation := true

	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	// Sustained heavy workload: cpu = 72000/PU, i.e. 120% at 60000 PU — the
	// CPU-driven calculation always wants more than max, so only the upper
	// bound decides how far the controller scales.
	body, _ := json.Marshal(map[string]interface{}{
		"high_priority": map[string]interface{}{
			"cpu_utilization":            0.90,
			"reference_processing_units": 80000,
		},
	})
	adminPUT(t, fmt.Sprintf("/workload/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/workload/%s/%s", projectID, instanceID)) })

	createSpannerInstance(t, projectID, instanceID, initPU)

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	t.Cleanup(func() { testEnv.Stop() }) //nolint:errcheck

	if err := spannerv1alpha1.AddToScheme(k8sscheme.Scheme); err != nil {
		t.Fatalf("add v1alpha1 scheme: %v", err)
	}
	if err := spannerv1beta1.AddToScheme(k8sscheme.Scheme); err != nil {
		t.Fatalf("add v1beta1 scheme: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     k8sscheme.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: ctrlconfig.Controller{SkipNameValidation: &skipValidation},
	})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	reconciler := controller.NewSpannerAutoscalerReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("cappolicy-controller"),
		logf.Log.WithName("cappolicy"),
		controller.WithSpannerEndpoint(spannerEmulatorAddr()),
		controller.WithMetricsEndpoint(monitoringGRPCAddr()),
		controller.WithSyncInterval(syncInterval),
		controller.WithScaleUpInterval(scaleUpInterval),
		controller.WithScaleDownInterval(scaleDownInterval),
	)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup controller: %v", err)
	}

	scheduleReconciler := controller.NewSpannerAutoscaleScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("cappolicy-schedule-controller"),
	)
	if err := scheduleReconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup schedule controller: %v", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	t.Cleanup(mgrCancel)
	t.Cleanup(reconciler.StopAll)
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	k8sClient := mgr.GetClient()
	ctx := context.Background()
	nn := types.NamespacedName{Namespace: "default", Name: "cappolicy-sa"}

	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nn.Name,
			Namespace: nn.Namespace,
		},
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			TargetInstance: spannerv1beta1.TargetInstance{
				ProjectID:  projectID,
				InstanceID: instanceID,
			},
			Authentication: spannerv1beta1.Authentication{
				Type: spannerv1beta1.AuthTypeADC,
			},
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ComputeType: spannerv1beta1.ComputeTypePU,
				ProcessingUnits: spannerv1beta1.ScaleConfigPUs{
					Min: minPU,
					Max: maxPU,
				},
				ScaledownStepSize: intstr.FromInt(2000),
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: targetCPU,
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("failed to create SpannerAutoscaler: %v", err)
	}

	sched := &spannerv1beta1.SpannerAutoscaleSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cappolicy-schedule",
			Namespace: nn.Namespace,
		},
		Spec: spannerv1beta1.SpannerAutoscaleScheduleSpec{
			TargetResource:            nn.Name,
			AdditionalProcessingUnits: additionalPU,
			MaxPUPolicy:               spannerv1beta1.MaxPUPolicyCap,
			Schedule: spannerv1beta1.Schedule{
				Cron:     "* * * * *",
				Duration: "5m",
			},
		},
	}
	if err := k8sClient.Create(ctx, sched); err != nil {
		t.Fatalf("failed to create SpannerAutoscaleSchedule: %v", err)
	}

	// Phase 1: wait until the schedule's cron fires and the ActiveSchedule
	// entry lands in status, so the "never exceeds max" assertion below is
	// exercised with the schedule actually in effect.
	activated := false
	deadline := time.Now().Add(activateWithin)
	for time.Now().Before(deadline) {
		var cur spannerv1beta1.SpannerAutoscaler
		if err := k8sClient.Get(ctx, nn, &cur); err == nil && len(cur.Status.CurrentlyActiveSchedules) > 0 {
			as := cur.Status.CurrentlyActiveSchedules[0]
			t.Logf("schedule activated: additionalPU=%d maxPUPolicy=%q", as.AdditionalPU, as.MaxPUPolicy)
			if as.MaxPUPolicy != spannerv1beta1.MaxPUPolicyCap {
				t.Errorf("ActiveSchedule.MaxPUPolicy = %q, want %q", as.MaxPUPolicy, spannerv1beta1.MaxPUPolicyCap)
			}
			activated = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !activated {
		t.Fatalf("schedule did not activate within %s", activateWithin)
	}

	// Phase 2: with the Cap schedule active and CPU pressure sustained,
	// the instance must stay at spec max; the lower bound must reflect the
	// schedule (min + additionalPU) and the upper bound must not move.
	var (
		maxSeenCurrent int
		maxSeenDesired int
		maxSeenRange   int
		lastLogged     string
	)
	wantDesiredMin := minPU + additionalPU // 21000, well below max: no clamping expected here
	deadline = time.Now().Add(observeFor)
	for time.Now().Before(deadline) {
		var cur spannerv1beta1.SpannerAutoscaler
		if err := k8sClient.Get(ctx, nn, &cur); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if cur.Status.CurrentProcessingUnits > maxSeenCurrent {
			maxSeenCurrent = cur.Status.CurrentProcessingUnits
		}
		if cur.Status.DesiredProcessingUnits > maxSeenDesired {
			maxSeenDesired = cur.Status.DesiredProcessingUnits
		}
		if cur.Status.DesiredMaxPUs > maxSeenRange {
			maxSeenRange = cur.Status.DesiredMaxPUs
		}
		if len(cur.Status.CurrentlyActiveSchedules) > 0 && cur.Status.DesiredMinPUs != wantDesiredMin {
			t.Errorf("desiredMinPUs = %d, want %d while Cap schedule is active", cur.Status.DesiredMinPUs, wantDesiredMin)
		}
		snapshot := fmt.Sprintf("currentPU=%d desiredPU=%d desiredMinPUs=%d desiredMaxPUs=%d activeSchedules=%d",
			cur.Status.CurrentProcessingUnits,
			cur.Status.DesiredProcessingUnits,
			cur.Status.DesiredMinPUs,
			cur.Status.DesiredMaxPUs,
			len(cur.Status.CurrentlyActiveSchedules))
		if snapshot != lastLogged {
			t.Logf("%s %s", time.Now().Format("15:04:05"), snapshot)
			lastLogged = snapshot
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Logf("observed maxima: currentPU=%d desiredPU=%d desiredMaxPUs=%d (spec max %d)",
		maxSeenCurrent, maxSeenDesired, maxSeenRange, maxPU)

	if maxSeenCurrent > maxPU {
		t.Errorf("instance PU exceeded spec max with maxPUPolicy=Cap: got %d, max %d", maxSeenCurrent, maxPU)
	}
	if maxSeenDesired > maxPU {
		t.Errorf("desired PU exceeded spec max with maxPUPolicy=Cap: got %d, max %d", maxSeenDesired, maxPU)
	}
	if maxSeenRange > maxPU {
		t.Errorf("status.desiredMaxPUs exceeded spec max with maxPUPolicy=Cap: got %d, max %d", maxSeenRange, maxPU)
	}
}
