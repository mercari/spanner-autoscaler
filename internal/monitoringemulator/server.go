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
//
// Each mode supports independent values per CPU metric type (highPriority / total),
// enabling dual CPU scaling mode testing with different values for each metric.
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

	kind := extractMetricKind(req.GetFilter())
	if kind == MetricKindUnknown {
		return nil, fmt.Errorf("unsupported metric type in filter: %s", req.GetFilter())
	}

	end, minutes := seriesRange(req.GetInterval())

	// Priority 1: ScenarioStore (time-based scenario mode)
	if _, ok := s.scenarioStore.Get(projectID, instanceID); ok {
		return s.buildScenarioResponse(ctx, projectID, instanceID, kind, end, minutes)
	}

	// Priority 2: WorkloadStore (dynamic mode)
	if params, ok := s.workloadStore.Get(projectID, instanceID, kind); ok {
		// The historical PU trajectory is not tracked, so the whole series is
		// derived from the current processing units. Good enough for testing:
		// the autoscaler only compares the series against thresholds.
		cpu, err := s.calcCPUFromWorkload(ctx, projectID, instanceID, params.Workload)
		if err != nil {
			return nil, err
		}
		return buildResponse(end, minutes, cpu), nil
	}

	// Priority 3: StaticStore (static mode)
	if entry, ok := s.staticStore.GetEntry(projectID, instanceID); ok {
		regions, ok := entry.regions(kind)
		if !ok {
			return &monitoringpb.ListTimeSeriesResponse{}, nil
		}
		values, err := aggregate(regions, req.GetAggregation(), req.GetSecondaryAggregation())
		if err != nil {
			return nil, err
		}
		return buildResponse(end, minutes, values...), nil
	}

	// None configured: empty response causes "no such spanner instance metrics" in the caller.
	return &monitoringpb.ListTimeSeriesResponse{}, nil
}

// maxSeriesMinutes caps the number of synthesized 1-minute points per
// series, regardless of how wide the requested interval is.
const maxSeriesMinutes = 24 * 60

// seriesRange derives the anchor (newest point time) and the number of
// 1-minute points to synthesize from the request interval. The number of
// points matches what Cloud Monitoring returns for a 60s-aligned query over
// the same interval, so window aggregation in the metrics client sees a fully
// covered window.
func seriesRange(interval *monitoringpb.TimeInterval) (time.Time, int) {
	end := time.Now()
	if ts := interval.GetEndTime(); ts != nil {
		end = ts.AsTime()
	}
	minutes := 1
	if ts := interval.GetStartTime(); ts != nil {
		minutes = int(end.Sub(ts.AsTime()) / time.Minute)
	}
	return end, min(max(minutes, 1), maxSeriesMinutes)
}

// buildScenarioResponse synthesizes the scenario's historical 1-minute series
// over the requested interval by resolving the step active at each minute.
// Steps whose value depends on the workload use the instance's *current*
// processing units for the whole series (the historical PU trajectory is not
// tracked). Minutes whose step has no value for the requested metric kind are
// skipped, mirroring a metric that was not ingested at that time.
func (s *MetricServiceServer) buildScenarioResponse(
	ctx context.Context, projectID, instanceID string, kind MetricKind, end time.Time, minutes int,
) (*monitoringpb.ListTimeSeriesResponse, error) {
	// Lazily fetch the current PU once; only needed for workload-based steps.
	currentPU := 0.0
	points := make([]*monitoringpb.Point, 0, minutes)
	for i := range minutes {
		t := end.Add(-time.Duration(i) * time.Minute)
		step, ok := s.scenarioStore.StepAt(projectID, instanceID, t)
		if !ok {
			break
		}
		metric := step.metricFor(kind)
		if metric == nil {
			continue
		}
		cpu := 0.0
		switch {
		case metric.Workload != nil:
			if currentPU == 0 {
				pu, err := s.currentProcessingUnits(ctx, projectID, instanceID)
				if err != nil {
					return nil, err
				}
				currentPU = pu
			}
			cpu = metric.Workload.CPUUtilization * float64(metric.Workload.ReferenceProcessingUnits) / currentPU
		default:
			cpu = *metric.CPUUtilization
		}
		points = append(points, buildPoint(t, cpu))
	}
	if len(points) == 0 {
		// Scenario registered but no value configured for this metric kind.
		return &monitoringpb.ListTimeSeriesResponse{}, nil
	}
	return &monitoringpb.ListTimeSeriesResponse{
		TimeSeries: []*monitoringpb.TimeSeries{{Points: points}},
	}, nil
}

// calcCPUFromWorkload queries the Spanner Emulator for the current processing
// units of the instance and computes:
//
//	cpu = workload / current_processing_units
func (s *MetricServiceServer) calcCPUFromWorkload(
	ctx context.Context, projectID, instanceID string, workload float64,
) (float64, error) {
	currentPU, err := s.currentProcessingUnits(ctx, projectID, instanceID)
	if err != nil {
		return 0, err
	}
	return workload / currentPU, nil
}

// currentProcessingUnits queries the Spanner Emulator for the instance's
// current processing units.
func (s *MetricServiceServer) currentProcessingUnits(ctx context.Context, projectID, instanceID string) (float64, error) {
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
	return currentPU, nil
}

// buildResponse builds one TimeSeries per given CPU utilization value, each
// holding a constant 1-minute point series over the requested interval
// (minutes points ending at end, newest first). Static mode may pass more
// than one value when the request's aggregation groups regions without a
// secondary reduction; every other mode always passes exactly one.
func buildResponse(end time.Time, minutes int, cpuUtilizations ...float64) *monitoringpb.ListTimeSeriesResponse {
	series := make([]*monitoringpb.TimeSeries, len(cpuUtilizations))
	for i, cpuUtilization := range cpuUtilizations {
		points := make([]*monitoringpb.Point, minutes)
		for j := range minutes {
			points[j] = buildPoint(end.Add(-time.Duration(j)*time.Minute), cpuUtilization)
		}
		series[i] = &monitoringpb.TimeSeries{Points: points}
	}
	return &monitoringpb.ListTimeSeriesResponse{TimeSeries: series}
}

// buildPoint builds one 1-minute-aligned point ending at t.
func buildPoint(t time.Time, cpuUtilization float64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(t.Add(-time.Minute)),
			EndTime:   timestamppb.New(t),
		},
		Value: &monitoringpb.TypedValue{
			Value: &monitoringpb.TypedValue_DoubleValue{
				DoubleValue: cpuUtilization,
			},
		},
	}
}
