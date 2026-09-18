//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mercari/spanner-autoscaler/internal/metrics"
)

func TestMetricsClient_GetInstanceMetrics_Static(t *testing.T) {
	const (
		projectID  = "metrics-project"
		instanceID = "metrics-instance"
		wantCPU    = 45 // 0.45 * 100
	)

	body, _ := json.Marshal(map[string]float64{"high_priority": 0.45})
	adminPUT(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID)) })

	ctx := context.Background()
	c, err := metrics.NewClient(ctx, projectID, instanceID,
		metrics.WithEndpoint(monitoringGRPCAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create metrics client: %v", err)
	}

	got, err := c.GetInstanceMetrics(ctx, metrics.MetricTypeHighPriority, time.Now(), nil)
	if err != nil {
		t.Fatalf("GetInstanceMetrics() error: %v", err)
	}

	if got.CurrentHighPriorityCPUUtilization != wantCPU {
		t.Errorf("CurrentHighPriorityCPUUtilization = %d, want %d",
			got.CurrentHighPriorityCPUUtilization, wantCPU)
	}
}

// TestMetricsClient_GetInstanceMetrics_MultiRegion regression-tests the fix in
// PR #263 (aggregate CPU per region and take the max instead of summing
// across regions) end-to-end against the real gRPC boundary, using the
// monitoring emulator's per-region static mode (high_priority_regions).
func TestMetricsClient_GetInstanceMetrics_MultiRegion(t *testing.T) {
	const (
		projectID  = "multiregion-project"
		instanceID = "multiregion-instance"
	)
	// Mirrors the evidence table in PR #263's description: a leader region
	// and two followers, none of which alone exceeds 100%, but whose naive
	// sum (0.53) is nowhere close to any single region's real value (0.31).
	regions := map[string]float64{
		"asia-northeast1": 0.31,
		"asia-northeast2": 0.14,
		"asia-northeast3": 0.08,
	}
	const (
		wantMax = 31 // busiest region: 0.31 * 100
		wantSum = 53 // naive sum across regions: 0.53 * 100 (the bug PR #263 fixed)
	)

	body, _ := json.Marshal(map[string]any{"high_priority_regions": regions})
	adminPUT(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID)) })

	ctx := context.Background()

	// The subtests below drive the emulator directly with hand-built
	// requests mirroring the two shapes internal/metrics/metrics.go can
	// send - the pre-#263 shape (bug) and the #263 shape (fix) - rather
	// than going through metrics.Client, which would couple this test's
	// outcome to whether PR #263 happens to be present on top of the
	// branch this test runs on. Either way, this proves the emulator now
	// actually implements Cloud Monitoring's aggregation semantics instead
	// of ignoring them - which is what made this regression untestable
	// before this change (see PR #263's "Testing" section).
	rawClient, err := monitoring.NewMetricClient(ctx,
		option.WithEndpoint(monitoringGRPCAddr()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		t.Fatalf("failed to create raw monitoring client: %v", err)
	}
	t.Cleanup(func() { rawClient.Close() }) //nolint:errcheck

	filter := fmt.Sprintf(`metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND
		metric.label.priority = "high" AND
		resource.label.instance_id = "%s"`, instanceID)
	now := time.Now()
	interval := &monitoringpb.TimeInterval{
		StartTime: timestamppb.New(now.Add(-10 * time.Minute)),
		EndTime:   timestamppb.New(now),
	}

	listSeries := func(t *testing.T, req *monitoringpb.ListTimeSeriesRequest) []*monitoringpb.TimeSeries {
		t.Helper()
		it := rawClient.ListTimeSeries(ctx, req)
		var series []*monitoringpb.TimeSeries
		for {
			ts, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return series
			}
			if err != nil {
				t.Fatalf("ListTimeSeries() error: %v", err)
			}
			series = append(series, ts)
		}
	}

	t.Run("pre-#263 request shape reproduces the inflated sum", func(t *testing.T) {
		series := listSeries(t, &monitoringpb.ListTimeSeriesRequest{
			Name:     fmt.Sprintf("projects/%s", projectID),
			Filter:   filter,
			Interval: interval,
			Aggregation: &monitoringpb.Aggregation{
				AlignmentPeriod:    durationpb.New(60 * time.Second),
				PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
				CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			},
			View: monitoringpb.ListTimeSeriesRequest_FULL,
		})
		if len(series) != 1 {
			t.Fatalf("got %d series, want exactly 1", len(series))
		}
		got := int(series[0].GetPoints()[0].GetValue().GetDoubleValue() * 100)
		if got != wantSum {
			t.Errorf("pre-#263 shape = %d, want %d (the bug: sum across regions)", got, wantSum)
		}
	})

	t.Run("#263 request shape returns the busiest region", func(t *testing.T) {
		series := listSeries(t, &monitoringpb.ListTimeSeriesRequest{
			Name:     fmt.Sprintf("projects/%s", projectID),
			Filter:   filter,
			Interval: interval,
			Aggregation: &monitoringpb.Aggregation{
				AlignmentPeriod:    durationpb.New(60 * time.Second),
				PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
				CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
				GroupByFields:      []string{"resource.label.location"},
			},
			SecondaryAggregation: &monitoringpb.Aggregation{
				AlignmentPeriod:    durationpb.New(60 * time.Second),
				PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
				CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_MAX,
			},
			View: monitoringpb.ListTimeSeriesRequest_FULL,
		})
		if len(series) != 1 {
			t.Fatalf("got %d series, want exactly 1", len(series))
		}
		got := int(series[0].GetPoints()[0].GetValue().GetDoubleValue() * 100)
		if got != wantMax {
			t.Errorf("#263 shape = %d, want %d (the fix: max across regions)", got, wantMax)
		}
	})

	t.Run("grouping without a secondary reduction yields one series per region", func(t *testing.T) {
		series := listSeries(t, &monitoringpb.ListTimeSeriesRequest{
			Name:     fmt.Sprintf("projects/%s", projectID),
			Filter:   filter,
			Interval: interval,
			Aggregation: &monitoringpb.Aggregation{
				AlignmentPeriod:    durationpb.New(60 * time.Second),
				PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
				CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
				GroupByFields:      []string{"resource.label.location"},
			},
			View: monitoringpb.ListTimeSeriesRequest_FULL,
		})
		if len(series) != len(regions) {
			t.Errorf("got %d series, want %d (one per region) - this is the shape "+
				"GetInstanceMetrics's \"expected a single aggregated time series\" "+
				"check guards against", len(series), len(regions))
		}
	})
}

func TestMetricsClient_GetInstanceMetrics_NotFound(t *testing.T) {
	ctx := context.Background()
	c, err := metrics.NewClient(ctx, "no-project", "no-instance",
		metrics.WithEndpoint(monitoringGRPCAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create metrics client: %v", err)
	}

	_, err = c.GetInstanceMetrics(ctx, metrics.MetricTypeHighPriority, time.Now(), nil)
	if err == nil {
		t.Fatal("GetInstanceMetrics() expected error for unconfigured instance, got nil")
	}
}

// TestMetricsClient_GetInstanceMetrics_WindowAggregates verifies the
// metricWindows plumbing end-to-end against the monitoring emulator: the
// emulator synthesizes a per-minute point series over the requested interval
// and the client aggregates it into min/avg/max per window.
func TestMetricsClient_GetInstanceMetrics_WindowAggregates(t *testing.T) {
	const (
		projectID  = "metrics-window-project"
		instanceID = "metrics-window-instance"
	)

	body, _ := json.Marshal(map[string]float64{"high_priority": 0.55})
	adminPUT(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID), body)
	t.Cleanup(func() { adminDELETE(t, fmt.Sprintf("/metrics/%s/%s", projectID, instanceID)) })

	ctx := t.Context()
	c, err := metrics.NewClient(ctx, projectID, instanceID,
		metrics.WithEndpoint(monitoringGRPCAddr()),
	)
	if err != nil {
		t.Fatalf("failed to create metrics client: %v", err)
	}

	windows := []time.Duration{15 * time.Minute, time.Hour}
	got, err := c.GetInstanceMetrics(ctx, metrics.MetricTypeHighPriority, time.Now(), windows)
	if err != nil {
		t.Fatalf("GetInstanceMetrics() error: %v", err)
	}

	if got.CurrentHighPriorityCPUUtilization != 55 {
		t.Errorf("CurrentHighPriorityCPUUtilization = %d, want 55", got.CurrentHighPriorityCPUUtilization)
	}
	if len(got.WindowAggregates) != len(windows) {
		t.Fatalf("WindowAggregates = %+v, want one entry per requested window %v", got.WindowAggregates, windows)
	}
	for i, agg := range got.WindowAggregates {
		if agg.Window != windows[i] {
			t.Errorf("WindowAggregates[%d].Window = %s, want %s", i, agg.Window, windows[i])
		}
		// Static mode yields a constant series, so min == avg == max == 55.
		if agg.Min != 55 || agg.Avg != 55 || agg.Max != 55 {
			t.Errorf("WindowAggregates[%d] = %+v, want min/avg/max all 55", i, agg)
		}
	}
}
