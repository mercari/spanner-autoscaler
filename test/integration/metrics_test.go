//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mercari/spanner-autoscaler/internal/metrics"
)

func TestMetricsClient_GetInstanceMetrics_Static(t *testing.T) {
	const (
		projectID  = "metrics-project"
		instanceID = "metrics-instance"
		wantCPU    = 45 // 0.45 * 100
	)

	body, _ := json.Marshal(map[string]float64{"cpu_utilization": 0.45})
	adminPUT(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID)) })

	ctx := context.Background()
	c, err := metrics.NewClient(ctx, projectID, instanceID,
		metrics.WithEndpoint(monitoringGRPCAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create metrics client: %v", err)
	}

	got, err := c.GetInstanceMetrics(ctx, metrics.MetricTypeHighPriority)
	if err != nil {
		t.Fatalf("GetInstanceMetrics() error: %v", err)
	}

	if got.CurrentHighPriorityCPUUtilization != wantCPU {
		t.Errorf("CurrentHighPriorityCPUUtilization = %d, want %d",
			got.CurrentHighPriorityCPUUtilization, wantCPU)
	}
}

func TestMetricsClient_GetInstanceMetrics_NotFound(t *testing.T) {
	ctx := context.Background()
	c, err := metrics.NewClient(ctx, "no-project", "no-instance",
		metrics.WithEndpoint(monitoringGRPCAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create metrics client: %v", err)
	}

	_, err = c.GetInstanceMetrics(ctx, metrics.MetricTypeHighPriority)
	if err == nil {
		t.Fatal("GetInstanceMetrics() expected error for unconfigured instance, got nil")
	}
}
