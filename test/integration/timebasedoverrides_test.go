//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/controller"
)

// TestController_TimeBasedOverrides checks against the emulators that a Deny
// override holds an idle instance, and that replacing it with an override
// that sets a larger step size releases it with that step.
func TestController_TimeBasedOverrides(t *testing.T) {
	const (
		projectID  = "tbo-project"
		instanceID = "tbo-instance"
		initPU     = 10000
	)

	logf.SetLogger(zap.New(zap.UseDevMode(true)))

	body, _ := json.Marshal(map[string]any{
		"total": map[string]any{
			"cpu_utilization":            0.05,
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
		mgr.GetEventRecorderFor("tbo-controller"),
		logf.Log.WithName("tbo"),
		controller.WithSpannerEndpoint(spannerEmulatorAddr()),
		controller.WithMetricsEndpoint(monitoringGRPCAddr()),
		controller.WithSyncInterval(time.Second),
		controller.WithScaleUpInterval(time.Second),
		controller.WithScaleDownInterval(time.Second),
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
	nn := types.NamespacedName{Namespace: "default", Name: "tbo-sa"}

	sa := &spannerv1beta1.SpannerAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
		Spec: spannerv1beta1.SpannerAutoscalerSpec{
			TargetInstance: spannerv1beta1.TargetInstance{ProjectID: projectID, InstanceID: instanceID},
			Authentication: spannerv1beta1.Authentication{Type: spannerv1beta1.AuthTypeADC},
			ScaleConfig: spannerv1beta1.ScaleConfig{
				ComputeType:          spannerv1beta1.ComputeTypePU,
				ProcessingUnits:      spannerv1beta1.ScaleConfigPUs{Min: 1000, Max: initPU},
				ScaledownStepSize:    intstr.FromInt(1000),
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{Total: new(40)},
				TimeBasedOverrides: []spannerv1beta1.TimeBasedOverride{{
					Name:            "hold",
					Times:           []string{"* * * * *"},
					ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
				}},
			},
		},
	}
	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("failed to create SpannerAutoscaler: %v", err)
	}

	// The manager client reads from its cache, which may lag behind writes.
	get := func() spannerv1beta1.SpannerAutoscaler {
		t.Helper()
		var got spannerv1beta1.SpannerAutoscaler
		var err error
		for range 50 {
			if err = k8sClient.Get(ctx, nn, &got); err == nil {
				return got
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("get SpannerAutoscaler: %v", err)
		return got
	}

	// The Deny override must hold the idle instance at its current size.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got := get()
		if cur := got.Status.CurrentProcessingUnits; cur != 0 && cur != initPU {
			t.Fatalf("PU changed to %d while the Deny override applied", cur)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := get().Status.CurrentTimeBasedOverride; got != "hold" {
		t.Fatalf("status.currentTimeBasedOverride = %q; want %q", got, "hold")
	}

	// Replace it with an override that allows scale down with a 5000 PU step.
	updated := false
	for range 50 {
		got := get()
		got.Spec.ScaleConfig.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{{
			Name:              "release",
			Times:             []string{"* * * * *"},
			ScaledownStepSize: new(intstr.FromInt(5000)),
		}}
		if err := k8sClient.Update(ctx, &got); err != nil {
			t.Logf("retrying update: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		updated = true
		break
	}
	if !updated {
		t.Fatal("could not update the overrides")
	}

	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got := get()
		if cur := got.Status.CurrentProcessingUnits; cur != initPU && cur != 0 {
			if cur != initPU-5000 {
				t.Fatalf("first scale-down went to %d; want %d (the override step size)", cur, initPU-5000)
			}
			if got.Status.CurrentTimeBasedOverride != "release" {
				t.Errorf("status.currentTimeBasedOverride = %q; want %q", got.Status.CurrentTimeBasedOverride, "release")
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("instance did not scale down after the Deny override was replaced")
}
