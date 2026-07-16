package monitoringemulator

import (
	"context"
	"testing"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBuildQuotaResponse(t *testing.T) {
	store := NewQuotaStore()
	store.Set("proj", "asia-northeast1", QuotaEntry{
		QuotaMetric: "spanner.googleapis.com/nodes",
		LimitName:   "SpannerNodesPerProject",
		LimitNodes:  100,
		UsageNodes:  1,
	})

	limitResp := buildQuotaResponse("proj", MetricKindQuotaLimit, store)
	if len(limitResp.GetTimeSeries()) != 1 {
		t.Fatalf("limit time series count = %d, want 1", len(limitResp.GetTimeSeries()))
	}
	limitTS := limitResp.GetTimeSeries()[0]
	if got := limitTS.GetMetric().GetType(); got != "serviceruntime.googleapis.com/quota/limit" {
		t.Errorf("limit metric type = %q", got)
	}
	if got := limitTS.GetMetric().GetLabels()["quota_metric"]; got != "spanner.googleapis.com/nodes" {
		t.Errorf("quota_metric = %q", got)
	}
	if got := limitTS.GetMetric().GetLabels()["limit_name"]; got != "SpannerNodesPerProject" {
		t.Errorf("limit_name = %q", got)
	}
	if got := limitTS.GetResource().GetLabels()["location"]; got != "asia-northeast1" {
		t.Errorf("location = %q", got)
	}
	if got := limitTS.GetPoints()[0].GetValue().GetInt64Value(); got != 100 {
		t.Errorf("limit value = %d, want 100", got)
	}

	usageResp := buildQuotaResponse("proj", MetricKindQuotaUsage, store)
	if len(usageResp.GetTimeSeries()) != 1 {
		t.Fatalf("usage time series count = %d, want 1", len(usageResp.GetTimeSeries()))
	}
	usageTS := usageResp.GetTimeSeries()[0]
	if got := usageTS.GetMetric().GetType(); got != "serviceruntime.googleapis.com/quota/allocation/usage" {
		t.Errorf("usage metric type = %q", got)
	}
	if got := usageTS.GetMetric().GetLabels()["limit_name"]; got != "" {
		t.Errorf("usage limit_name = %q, want empty", got)
	}
	if got := usageTS.GetPoints()[0].GetValue().GetInt64Value(); got != 1 {
		t.Errorf("usage value = %d, want 1", got)
	}
}

func TestListTimeSeries_QuotaModeReturnsError(t *testing.T) {
	staticStore := NewStaticStore()
	workloadStore := NewWorkloadStore()
	scenarioStore := NewScenarioStore()
	quotaStore := NewQuotaStore()
	quotaModeStore := NewQuotaModeStore()
	quotaStore.Set("proj", "asia-northeast1", QuotaEntry{
		QuotaMetric: "spanner.googleapis.com/nodes",
		LimitName:   "SpannerNodesPerProject",
		LimitNodes:  100,
		UsageNodes:  1,
	})

	srv := NewMetricServiceServer(staticStore, workloadStore, scenarioStore, quotaStore, quotaModeStore, nil)

	limitReq := &monitoringpb.ListTimeSeriesRequest{
		Name:   "projects/proj",
		Filter: `metric.type = "serviceruntime.googleapis.com/quota/limit" AND resource.type = "consumer_quota"`,
	}
	usageReq := &monitoringpb.ListTimeSeriesRequest{
		Name:   "projects/proj",
		Filter: `metric.type = "serviceruntime.googleapis.com/quota/allocation/usage" AND resource.type = "consumer_quota"`,
	}

	// No mode set yet → success
	if _, err := srv.ListTimeSeries(context.Background(), limitReq); err != nil {
		t.Fatalf("ListTimeSeries(limit) before mode = %v, want nil", err)
	}

	// Mode set → returns the configured error
	quotaModeStore.Set(codes.PermissionDenied, "permission denied")
	_, err := srv.ListTimeSeries(context.Background(), limitReq)
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("ListTimeSeries(limit) under mode code = %v, want PermissionDenied; err=%v", status.Code(err), err)
	}
	_, err = srv.ListTimeSeries(context.Background(), usageReq)
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("ListTimeSeries(usage) under mode code = %v, want PermissionDenied; err=%v", status.Code(err), err)
	}

	// Clear → success again
	quotaModeStore.Clear()
	if _, err := srv.ListTimeSeries(context.Background(), limitReq); err != nil {
		t.Errorf("ListTimeSeries(limit) after clear = %v, want nil", err)
	}
}
