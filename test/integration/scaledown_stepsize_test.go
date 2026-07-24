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
	webhookv1beta1 "github.com/mercari/spanner-autoscaler/internal/webhook/v1beta1"
)

// TestController_ScaledownStepSize_Percent observes, end-to-end via the
// emulators, the scale-down step size when scaledownStepSize is set to "10%".
//
// It contrasts two paths:
//   - applyWebhookDefault=true:  apply the mutating webhook Default() before
//     creation, exactly like production, to check whether "10%" is preserved
//     rather than being overwritten with 2000.
//   - applyWebhookDefault=false: pass "10%" straight to the controller without
//     going through Default(), to observe the step-size resolution logic alone.
//
// In every case the instance starts at initPU under a low CPU load so it scales
// down, and the observed PU sequence (and each step delta) is recorded.
func TestController_ScaledownStepSize_Percent(t *testing.T) {
	cases := []struct {
		name                string
		projectID           string
		instanceID          string
		saName              string
		initPU              int
		maxPU               int
		applyWebhookDefault bool
	}{
		{
			name:                "with_webhook_default_production_path",
			projectID:           "sd-def-project",
			instanceID:          "sd-def-instance",
			saName:              "sd-def-sa",
			initPU:              10000,
			maxPU:               10000,
			applyWebhookDefault: true,
		},
		{
			name:                "without_webhook_default_direct_10pct",
			projectID:           "sd-raw-project",
			instanceID:          "sd-raw-instance",
			saName:              "sd-raw-sa",
			initPU:              10000,
			maxPU:               10000,
			applyWebhookDefault: false,
		},
		{
			// Post-fix check: even on the production-equivalent path (after the
			// webhook Default() is applied), "10%" is preserved and the instance
			// scales down by roughly 10% of the current PU from 35000.
			name:                "from_35000_with_webhook_default",
			projectID:           "sd-35k-project",
			instanceID:          "sd-35k-instance",
			saName:              "sd-35k-sa",
			initPU:              35000,
			maxPU:               35000,
			applyWebhookDefault: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			seq, effective := runScaledownScenario(t, tc.projectID, tc.instanceID, tc.saName, tc.initPU, tc.maxPU, tc.applyWebhookDefault)
			t.Logf("[%s] effective scaledownStepSize on the object = %q", tc.name, effective)
			t.Logf("[%s] observed PU sequence = %v", tc.name, seq)
			for i := 1; i < len(seq); i++ {
				t.Logf("[%s] step %d: %d -> %d (delta=%d)",
					tc.name, i, seq[i-1], seq[i], seq[i-1]-seq[i])
			}
		})
	}
}

func runScaledownScenario(t *testing.T, projectID, instanceID, saName string, initPU, maxPU int, applyWebhookDefault bool) (seq []int, effectiveStepSize string) {
	t.Helper()

	const (
		referenceCPU      = 0.10 // Workload = 0.10 * initPU; cpu = Workload / PU
		syncInterval      = 1 * time.Second
		scaleUpInterval   = 1 * time.Second
		scaleDownInterval = 1 * time.Second
	)
	targetCPUVal := 40
	targetCPU := &targetCPUVal

	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	// Configure a low CPU load on the total metric to trigger scale-down.
	body, _ := json.Marshal(map[string]interface{}{
		"total": map[string]interface{}{
			"cpu_utilization":            referenceCPU,
			"reference_processing_units": initPU,
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

	skipValidation := true
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
		mgr.GetEventRecorderFor("sd-controller"),
		logf.Log.WithName("sd"),
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
	t.Cleanup(reconciler.StopAll)
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	k8sClient := mgr.GetClient()
	ctx := context.Background()
	nn := types.NamespacedName{Namespace: "default", Name: saName}

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
					Max: maxPU,
				},
				// User intent: scale down by 10% of the current PU per step.
				ScaledownStepSize: intstr.FromString("10%"),
				ScaleupStepSize:   intstr.FromInt(1000),
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					Total: targetCPU,
				},
			},
		},
	}

	// In production the mutating webhook Default() always runs before creation.
	// This envtest environment does not register the webhook, so call it
	// explicitly here to reproduce the same state as production.
	if applyWebhookDefault {
		d := &webhookv1beta1.SpannerAutoscalerCustomDefaulter{}
		if err := d.Default(ctx, sa); err != nil {
			t.Fatalf("webhook Default() failed: %v", err)
		}
	}
	effectiveStepSize = sa.Spec.ScaleConfig.ScaledownStepSize.String()

	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("failed to create SpannerAutoscaler: %v", err)
	}

	// Observe the PU sequence. status.CurrentProcessingUnits reflects the
	// instance's actual PU in the emulator (populated by the syncer); record it
	// whenever the value changes.
	seq = []int{}
	last := -1
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		var updated spannerv1beta1.SpannerAutoscaler
		if err := k8sClient.Get(ctx, nn, &updated); err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		cur := updated.Status.CurrentProcessingUnits
		if cur != 0 && cur != last {
			seq = append(seq, cur)
			last = cur
			t.Logf("[%s] observed PU=%d (totalCPU=%d%%)", saName, cur, updated.Status.CurrentTotalCPUUtilization)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return seq, effectiveStepSize
}
