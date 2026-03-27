//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
	"github.com/mercari/spanner-autoscaler/internal/syncer"
)

func TestSyncer_SyncResource(t *testing.T) {
	const (
		projectID  = "syncer-project"
		instanceID = "syncer-instance"
		initPU     = 1000
		wantCPU    = 60 // 0.60 * 100
	)

	// Configure static CPU utilization in the monitoring emulator.
	body, _ := json.Marshal(map[string]float64{"cpu_utilization": 0.60})
	adminPUT(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID)) })

	// Create Spanner instance in the emulator.
	createSpannerInstance(t, projectID, instanceID, initPU)

	// Start envtest and create K8s client.
	k8sClient := startEnvtest(t)

	ctx := context.Background()
	nn := types.NamespacedName{Namespace: "default", Name: "syncer-sa"}

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
				TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
					HighPriority: 40,
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, sa); err != nil {
		t.Fatalf("failed to create SpannerAutoscaler: %v", err)
	}

	spannerClient, err := spanner.NewClient(ctx, projectID, instanceID,
		spanner.WithEndpoint(spannerEmulatorAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create spanner client: %v", err)
	}

	metricsClient, err := metrics.NewClient(ctx, projectID, instanceID,
		metrics.WithEndpoint(monitoringGRPCAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create metrics client: %v", err)
	}

	creds := syncer.NewADCCredentials()
	recorder := record.NewFakeRecorder(100)
	s, err := syncer.New(ctx, k8sClient, nn, creds, recorder, spannerClient, metricsClient,
		syncer.WithInterval(2*time.Second),
	)
	if err != nil {
		t.Fatalf("failed to create syncer: %v", err)
	}

	go s.Start()
	t.Cleanup(s.Stop)

	// Wait for the syncer to update status.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var updated spannerv1beta1.SpannerAutoscaler
		if err := k8sClient.Get(ctx, nn, &updated); err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if updated.Status.CurrentProcessingUnits == initPU &&
			updated.Status.CurrentHighPriorityCPUUtilization == wantCPU {
			return // success
		}
		time.Sleep(500 * time.Millisecond)
	}

	var updated spannerv1beta1.SpannerAutoscaler
	k8sClient.Get(ctx, nn, &updated) //nolint:errcheck
	t.Errorf("syncer did not update status within timeout: PU=%d (want %d), CPU=%d (want %d)",
		updated.Status.CurrentProcessingUnits, initPU,
		updated.Status.CurrentHighPriorityCPUUtilization, wantCPU,
	)
}
