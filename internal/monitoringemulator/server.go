package monitoringemulator

import (
	"context"
	"errors"
	"fmt"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	spanneradmin "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
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
type MetricServiceServer struct {
	monitoringpb.UnimplementedMetricServiceServer
	staticStore        *StaticStore
	workloadStore      *WorkloadStore
	scenarioStore      *ScenarioStore
	spannerAdminClient *spanneradmin.InstanceAdminClient // nil → dynamic mode unavailable
}

func NewMetricServiceServer(
	staticStore *StaticStore,
	workloadStore *WorkloadStore,
	scenarioStore *ScenarioStore,
	spannerAdminClient *spanneradmin.InstanceAdminClient,
) *MetricServiceServer {
	return &MetricServiceServer{
		staticStore:        staticStore,
		workloadStore:      workloadStore,
		scenarioStore:      scenarioStore,
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
	instanceID, err := extractInstanceID(req.GetFilter())
	if err != nil {
		return nil, err
	}

	// Priority 1: ScenarioStore (time-based scenario mode)
	if step, ok := s.scenarioStore.Get(projectID, instanceID); ok {
		if step.Workload != nil {
			cpu, err := s.calcCPUFromWorkload(ctx, projectID, instanceID,
				step.Workload.CPUUtilization*float64(step.Workload.ReferenceProcessingUnits))
			if err != nil {
				return nil, err
			}
			return buildResponse(cpu), nil
		}
		return buildResponse(*step.CPUUtilization), nil
	}

	// Priority 2: WorkloadStore (dynamic mode)
	if entry, ok := s.workloadStore.Get(projectID, instanceID); ok {
		cpu, err := s.calcCPUFromWorkload(ctx, projectID, instanceID, entry.Workload)
		if err != nil {
			return nil, err
		}
		return buildResponse(cpu), nil
	}

	// Priority 3: StaticStore (static mode)
	if cpu, ok := s.staticStore.Get(projectID, instanceID); ok {
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
