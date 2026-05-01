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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/controller"
)

// TestController_E2E_ScaleUp verifies the full scale-up flow:
//
//  1. A Spanner instance starts at 1000 PU in the Spanner emulator.
//  2. A workload is configured in the Monitoring Emulator so that at 1000 PU the
//     CPU utilization is 0.80 (80%), which exceeds the target of 40%.
//  3. The controller syncs the status (via the syncer) and decides to scale up.
//  4. The test waits until the Spanner emulator instance PU increases.
func TestController_E2E_ScaleUp(t *testing.T) {
	const (
		projectID         = "e2e-project"
		instanceID        = "e2e-instance"
		initPU            = 1000
		referenceCPU      = 0.80
		syncInterval      = 3 * time.Second
		scaleUpInterval   = 1 * time.Second
		scaleDownInterval = 55 * time.Minute
	)
	targetCPUVal := 40
	targetCPU := &targetCPUVal // percent

	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	// Configure dynamic (workload) mode in the monitoring emulator.
	// At 1000 PU with cpu=0.80: workload = 0.80 * 1000 = 800.
	// After scale-up to e.g. 2000 PU: cpu = 800 / 2000 = 0.40 → at target.
	body, _ := json.Marshal(map[string]interface{}{
		"high_priority": map[string]interface{}{
			"cpu_utilization":            referenceCPU,
			"reference_processing_units": initPU,
		},
	})
	adminPUT(t, fmt.Sprintf("/workload/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/workload/%s/%s", projectID, instanceID)) })

	// Create Spanner instance in the emulator.
	createSpannerInstance(t, projectID, instanceID, initPU)

	// Start envtest with a manager (needed for controller).
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

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: k8sscheme.Scheme})
	if err != nil {
		t.Fatalf("failed to create manager: %v", err)
	}

	reconciler := controller.NewSpannerAutoscalerReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("e2e-controller"),
		logf.Log.WithName("e2e"),
		controller.WithSpannerEndpoint(spannerEmulatorAddr()),
		controller.WithMetricsEndpoint(monitoringGRPCAddr()),
		controller.WithSyncInterval(syncInterval),
		controller.WithScaleUpInterval(scaleUpInterval),
		controller.WithScaleDownInterval(scaleDownInterval),
	)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("failed to setup controller: %v", err)
	}

	mgrCtx, mgrCancel := context.WithCancel(context.Background())
	t.Cleanup(mgrCancel)
	t.Cleanup(reconciler.StopAll) // runs before mgrCancel (LIFO); stops syncer goroutines
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	k8sClient := mgr.GetClient()
	ctx := context.Background()
	nn := types.NamespacedName{Namespace: "default", Name: "e2e-sa"}

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
					Min: 100,
					Max: 10000,
				},
				ScaledownStepSize: intstr.FromInt(2000),
				ScaleupStepSize:   intstr.FromInt(1000),
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: targetCPU,
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("failed to create SpannerAutoscaler: %v", err)
	}

	// Wait for the controller to scale up the Spanner instance.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var updated spannerv1beta1.SpannerAutoscaler
		if err := k8sClient.Get(ctx, nn, &updated); err != nil {
			time.Sleep(time.Second)
			continue
		}
		if updated.Status.CurrentProcessingUnits > initPU {
			t.Logf("scale-up succeeded: PU %d → %d (CPU=%d%%)",
				initPU, updated.Status.CurrentProcessingUnits,
				updated.Status.CurrentHighPriorityCPUUtilization)
			return
		}
		time.Sleep(time.Second)
	}

	var updated spannerv1beta1.SpannerAutoscaler
	k8sClient.Get(ctx, nn, &updated) //nolint:errcheck
	t.Errorf("controller did not scale up within timeout: PU=%d (want >%d), CPU=%d%%",
		updated.Status.CurrentProcessingUnits, initPU,
		updated.Status.CurrentHighPriorityCPUUtilization)
}
