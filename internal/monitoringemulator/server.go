package monitoringemulator

import (
	"context"
	"errors"
	"fmt"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	spanneradmin "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// MetricServiceServer implements the Cloud Monitoring MetricService gRPC interface.
// Only ListTimeSeries is implemented; all other methods return Unimplemented via
// the embedded UnimplementedMetricServiceServer.
//
// Three modes are supported, checked in priority order:
//
//  1. Scenario mode: steps through a time-based sequence of CPU values
//     loaded from a YAML file or set via the admin API
//     (PUT /scenario/{project}/{instance}).
//
//  2. Dynamic mode: calculates CPU utilization from a constant workload and the
//     current Spanner instance processing units, queried live from the Spanner
//     Emulator (PUT /workload/{project}/{instance}).
//
//  3. Static mode: returns a fixed CPU utilization value set via the admin API
//     (PUT /metrics/{project}/{instance}).
//
// If none is configured for the requested instance, an empty TimeSeries
// list is returned, which causes metrics.Client.GetInstanceMetrics to return
// "no such spanner instance metrics".
//
// Each mode supports independent values per CPU metric type (highPriority / total),
// enabling dual CPU scaling mode testing with different values for each metric.
type MetricServiceServer struct {
	monitoringpb.UnimplementedMetricServiceServer
	staticStore        *StaticStore
	workloadStore      *WorkloadStore
	scenarioStore      *ScenarioStore
	quotaStore         *QuotaStore
	quotaModeStore     *QuotaModeStore
	spannerAdminClient *spanneradmin.InstanceAdminClient // nil → dynamic mode unavailable
}

func NewMetricServiceServer(
	staticStore *StaticStore,
	workloadStore *WorkloadStore,
	scenarioStore *ScenarioStore,
	quotaStore *QuotaStore,
	quotaModeStore *QuotaModeStore,
	spannerAdminClient *spanneradmin.InstanceAdminClient,
) *MetricServiceServer {
	return &MetricServiceServer{
		staticStore:        staticStore,
		workloadStore:      workloadStore,
		scenarioStore:      scenarioStore,
		quotaStore:         quotaStore,
		quotaModeStore:     quotaModeStore,
		spannerAdminClient: spannerAdminClient,
	}
}

func (s *MetricServiceServer) ListTimeSeries(
	ctx context.Context,
	req *monitoringpb.ListTimeSeriesRequest,
) (*monitoringpb.ListTimeSeriesResponse, error) {
	projectID, err := extractProjectID(req.GetName())
	if err != nil {
		return nil, err
	}

	kind := extractMetricKind(req.GetFilter())
	if kind == MetricKindUnknown {
		return nil, fmt.Errorf("unsupported metric type in filter: %s", req.GetFilter())
	}
	if kind == MetricKindQuotaLimit || kind == MetricKindQuotaUsage {
		if s.quotaModeStore != nil {
			if mode, ok := s.quotaModeStore.Get(); ok {
				return nil, status.Error(mode.Code, mode.Message)
			}
		}
		return buildQuotaResponse(projectID, kind, s.quotaStore), nil
	}

	instanceID, err := extractInstanceID(req.GetFilter())
	if err != nil {
		return nil, err
	}

	// Priority 1: ScenarioStore (time-based scenario mode)
	if step, ok := s.scenarioStore.Get(projectID, instanceID); ok {
		metric := step.metricFor(kind)
		if metric == nil {
			// Scenario registered but no value configured for this metric kind.
			return &monitoringpb.ListTimeSeriesResponse{}, nil
		}
		if metric.Workload != nil {
			cpu, err := s.calcCPUFromWorkload(ctx, projectID, instanceID,
				metric.Workload.CPUUtilization*float64(metric.Workload.ReferenceProcessingUnits))
			if err != nil {
				return nil, err
			}
			return buildResponse(cpu), nil
		}
		return buildResponse(*metric.CPUUtilization), nil
	}

	// Priority 2: WorkloadStore (dynamic mode)
	if params, ok := s.workloadStore.Get(projectID, instanceID, kind); ok {
		cpu, err := s.calcCPUFromWorkload(ctx, projectID, instanceID, params.Workload)
		if err != nil {
			return nil, err
		}
		return buildResponse(cpu), nil
	}

	// Priority 3: StaticStore (static mode)
	if cpu, ok := s.staticStore.Get(projectID, instanceID, kind); ok {
		return buildResponse(cpu), nil
	}

	// None configured: empty response causes "no such spanner instance metrics" in the caller.
	return &monitoringpb.ListTimeSeriesResponse{}, nil
}

// calcCPUFromWorkload queries the Spanner Emulator for the current processing
// units of the instance and computes:
//
//	cpu = workload / current_processing_units
func (s *MetricServiceServer) calcCPUFromWorkload(
	ctx context.Context, projectID, instanceID string, workload float64,
) (float64, error) {
	if s.spannerAdminClient == nil {
		return 0, errors.New("dynamic mode requires SPANNER_EMULATOR_HOST to be set")
	}
	resp, err := s.spannerAdminClient.GetInstance(ctx, &instancepb.GetInstanceRequest{
		Name: fmt.Sprintf("projects/%s/instances/%s", projectID, instanceID),
	})
	if err != nil {
		return 0, fmt.Errorf("failed to get instance from spanner emulator: %w", err)
	}
	currentPU := float64(resp.GetProcessingUnits())
	if currentPU == 0 {
		return 0, errors.New("instance has 0 processing units")
	}
	return workload / currentPU, nil
}

func buildResponse(cpuUtilization float64) *monitoringpb.ListTimeSeriesResponse {
	now := time.Now()
	return &monitoringpb.ListTimeSeriesResponse{
		TimeSeries: []*monitoringpb.TimeSeries{
			{
				Points: []*monitoringpb.Point{
					{
						Interval: &monitoringpb.TimeInterval{
							StartTime: timestamppb.New(now.Add(-time.Minute)),
							EndTime:   timestamppb.New(now),
						},
						Value: &monitoringpb.TypedValue{
							Value: &monitoringpb.TypedValue_DoubleValue{
								DoubleValue: cpuUtilization,
							},
						},
					},
				},
			},
		},
	}
}

func buildQuotaResponse(projectID string, kind MetricKind, store *QuotaStore) *monitoringpb.ListTimeSeriesResponse {
	if store == nil {
		return &monitoringpb.ListTimeSeriesResponse{}
	}

	now := time.Now()
	entries := store.List(projectID)
	resp := &monitoringpb.ListTimeSeriesResponse{
		TimeSeries: make([]*monitoringpb.TimeSeries, 0, len(entries)),
	}
	for location, entry := range entries {
		metricLabels := map[string]string{
			"quota_metric": entry.QuotaMetric,
		}
		value := entry.UsageNodes
		metricType := "serviceruntime.googleapis.com/quota/allocation/usage"
		if kind == MetricKindQuotaLimit {
			metricType = "serviceruntime.googleapis.com/quota/limit"
			metricLabels["limit_name"] = entry.LimitName
			value = entry.LimitNodes
		}
		resp.TimeSeries = append(resp.TimeSeries, &monitoringpb.TimeSeries{
			Metric: &metricpb.Metric{
				Type:   metricType,
				Labels: metricLabels,
			},
			Resource: &monitoredrespb.MonitoredResource{
				Type: "consumer_quota",
				Labels: map[string]string{
					"project_id": projectID,
					"service":    "spanner.googleapis.com",
					"location":   location,
				},
			},
			Points: []*monitoringpb.Point{
				{
					Interval: &monitoringpb.TimeInterval{
						StartTime: timestamppb.New(now),
						EndTime:   timestamppb.New(now),
					},
					Value: &monitoringpb.TypedValue{
						Value: &monitoringpb.TypedValue_Int64Value{
							Int64Value: value,
						},
					},
				},
			},
		})
	}
	return resp
}
