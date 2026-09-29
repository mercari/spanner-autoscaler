package metrics

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/go-logr/logr"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/golang/protobuf/ptypes/timestamp"
	"golang.org/x/oauth2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// MetricType represents which Cloud Monitoring metric to query.
type MetricType int

const (
	// MetricTypeHighPriority queries cpu/utilization_by_priority with priority=high (default).
	MetricTypeHighPriority MetricType = iota
	// MetricTypeTotal queries cpu/utilization (all priorities combined).
	MetricTypeTotal
)

const metricsFilterFormatHighPriority = `
		metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND
		metric.label.priority = "high" AND
		resource.label.instance_id = "%s"
`

const metricsFilterFormatTotal = `
		metric.type = "spanner.googleapis.com/instance/cpu/utilization" AND
		resource.label.instance_id = "%s"
`

// InstanceMetrics represents metrics of Spanner instance.
type InstanceMetrics struct {
	// CurrentHighPriorityCPUUtilization is set when MetricTypeHighPriority is used.
	CurrentHighPriorityCPUUtilization int
	// CurrentTotalCPUUtilization is set when MetricTypeTotal is used.
	CurrentTotalCPUUtilization int

	// WindowAggregates holds the min/avg/max of the 1-minute CPU series over
	// each requested window, for the queried metric type. A requested window
	// is omitted while the returned series does not yet cover it (see
	// GetInstanceMetrics).
	WindowAggregates []WindowAggregate
}

// WindowAggregate is the aggregation of the 1-minute CPU utilization series
// over one time window. Values are integer percentages, truncated the same
// way as the current CPU value (int(fraction * 100)).
type WindowAggregate struct {
	// Window is the requested aggregation window.
	Window time.Duration
	// Min/Avg/Max of the 1-minute CPU utilization within the window, percent.
	Min int
	Avg int
	Max int
}

// Client is a client for manipulation of InstanceMetrics.
type Client interface {
	// GetInstanceMetrics gets the instance metrics for the given metric type.
	// The now parameter is the reference time for the query window
	// (StartTime = now - term, EndTime = now). Callers that issue several
	// GetInstanceMetrics calls whose results need to align should pass the
	// same now to each call so the underlying Cloud Monitoring queries hit
	// the same alignment window.
	//
	// windows, when non-empty, requests min/avg/max aggregates of the
	// 1-minute series over each window. The query term is extended to cover
	// the largest window; each window is anchored at the newest returned
	// point (which typically lags now by a couple of minutes of ingestion
	// delay) and is only reported once the series holds a full window's worth
	// of 1-minute points, so that e.g. a "sustained for 15 minutes" condition
	// can never be satisfied by a shorter series.
	GetInstanceMetrics(ctx context.Context, metricType MetricType, now time.Time, windows []time.Duration) (*InstanceMetrics, error)
}

// client is a client for Stackdriver Monitoring.
type client struct {
	monitoringMetricClient *monitoring.MetricClient

	projectID  string
	instanceID string
	term       time.Duration

	endpoint    string
	tokenSource oauth2.TokenSource

	log logr.Logger
}

var _ Client = (*client)(nil)

type Option func(*client)

func WithEndpoint(endpoint string) Option {
	return func(c *client) {
		c.endpoint = endpoint
	}
}

func WithTerm(term time.Duration) Option {
	return func(c *client) {
		c.term = term
	}
}

func WithTokenSource(ts oauth2.TokenSource) Option {
	return func(c *client) {
		c.tokenSource = ts
	}
}

func WithLog(log logr.Logger) Option {
	return func(c *client) {
		c.log = log.WithName("metrics")
	}
}

// NewClient returns a new Client.
func NewClient(ctx context.Context, projectID, instanceID string, opts ...Option) (Client, error) {
	c := &client{
		projectID:  projectID,
		instanceID: instanceID,
		term:       10 * time.Minute,
		log:        logr.Discard(),
	}

	for _, opt := range opts {
		opt(c)
	}

	var options []option.ClientOption

	if c.endpoint != "" {
		options = append(options,
			option.WithEndpoint(c.endpoint),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)
	} else if c.tokenSource != nil {
		options = append(options, option.WithTokenSource(c.tokenSource))
	}

	monitoringMetricClient, err := monitoring.NewMetricClient(ctx, options...)
	if err != nil {
		return nil, err
	}

	c.monitoringMetricClient = monitoringMetricClient

	return c, nil
}

// GetInstanceMetrics implements Client.
// https://cloud.google.com/monitoring/custom-metrics/reading-metrics#monitoring_read_timeseries_fields-go
func (c *client) GetInstanceMetrics(ctx context.Context, metricType MetricType, now time.Time, windows []time.Duration) (*InstanceMetrics, error) {
	log := c.log.WithValues("instance-id", c.instanceID, "project-id", c.projectID)

	log.V(1).Info("getting monitoring time series data")

	it := c.monitoringMetricClient.ListTimeSeries(ctx, c.buildListTimeSeriesRequest(metricType, now, windows))

	var series []*monitoringpb.TimeSeries
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			log.Error(err, "unable to get metrics list time series response with iterator")
			return nil, err
		}
		series = append(series, ts)
	}

	if len(series) == 0 {
		log.V(1).Info("could not get any time series metrics")
		return nil, errors.New("no such spanner instance metrics")
	}
	if len(series) > 1 {
		err := fmt.Errorf("expected a single aggregated time series, got %d", len(series))
		log.Error(err, "aggregation did not reduce the metric to a single time series")
		return nil, err
	}

	resp := series[0]

	log.V(1).Info("got time series data points", "points", resp.GetPoints())

	// monitoringpb.Point.GetValue().GetDoubleValue() for CPU is in [0, 1].
	cpuPercent, err := firstPointAsPercent(resp.GetPoints())
	if err != nil {
		return nil, err
	}

	result := &InstanceMetrics{
		WindowAggregates: windowAggregates(resp.GetPoints(), windows),
	}
	switch metricType {
	case MetricTypeTotal:
		result.CurrentTotalCPUUtilization = cpuPercent
	default: // MetricTypeHighPriority
		result.CurrentHighPriorityCPUUtilization = cpuPercent
	}
	return result, nil
}

func (c *client) buildListTimeSeriesRequest(metricType MetricType, now time.Time, windows []time.Duration) *monitoringpb.ListTimeSeriesRequest {
	var filter string
	switch metricType {
	case MetricTypeTotal:
		filter = fmt.Sprintf(metricsFilterFormatTotal, c.instanceID)
	default: // MetricTypeHighPriority
		filter = fmt.Sprintf(metricsFilterFormatHighPriority, c.instanceID)
	}

	// The base term exists to absorb ingestion delay (the newest point lags
	// now by a couple of minutes). Window aggregation is anchored at the
	// newest point, so the term must additionally cover the largest window.
	term := c.term
	for _, w := range windows {
		term = max(term, w+c.term)
	}

	nowUTC := now.UTC()

	return &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", c.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: &timestamp.Timestamp{
				Seconds: nowUTC.Add(-term).Unix(),
			},
			EndTime: &timestamp.Timestamp{
				Seconds: nowUTC.Unix(),
			},
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    &duration.Duration{Seconds: 60},
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
			GroupByFields:      []string{"resource.label.location"},
		},
		// Each region of a multi-region instance carries the full compute capacity, so the busiest region is the constraint.
		SecondaryAggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    &duration.Duration{Seconds: 60},
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_MAX,
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}
}

func firstPointAsPercent(points []*monitoringpb.Point) (percent int, err error) {
	if len(points) == 0 {
		return 0, errors.New("invalid points")
	}

	return int(points[0].GetValue().GetDoubleValue() * 100), nil
}

// windowAggregates computes min/avg/max over the 1-minute point series for
// each requested window. Every window is anchored at the newest point in the
// series (not at the request time, which the newest point lags by the metric
// ingestion delay): a window covers the points strictly newer than
// (newest - window). A window is skipped while it holds fewer than a full
// window's worth of 1-minute points — evaluating "sustained for 15 minutes"
// over a shorter series would report false sustainment right after instance
// creation or across an ingestion gap.
//
// The simulator reimplements these exact semantics over its replayed series
// (internal/simulator windowState.windowMetrics); keep the two in sync.
func windowAggregates(points []*monitoringpb.Point, windows []time.Duration) []WindowAggregate {
	if len(windows) == 0 || len(points) == 0 {
		return nil
	}

	// Cloud Monitoring returns points newest-first, but ordering is not
	// documented as part of the contract; scan for the newest explicitly.
	newest := time.Unix(points[0].GetInterval().GetEndTime().GetSeconds(), 0)
	for _, p := range points[1:] {
		if t := time.Unix(p.GetInterval().GetEndTime().GetSeconds(), 0); t.After(newest) {
			newest = t
		}
	}

	aggregates := make([]WindowAggregate, 0, len(windows))
	for _, w := range windows {
		cutoff := newest.Add(-w)
		var values []float64
		for _, p := range points {
			t := time.Unix(p.GetInterval().GetEndTime().GetSeconds(), 0)
			if !t.After(cutoff) {
				continue
			}
			values = append(values, p.GetValue().GetDoubleValue()*100)
		}
		if expected := int(w / time.Minute); len(values) < expected {
			continue
		}
		var sum float64
		for _, v := range values {
			sum += v
		}
		aggregates = append(aggregates, WindowAggregate{
			Window: w,
			Min:    int(slices.Min(values)),
			Avg:    int(sum / float64(len(values))),
			Max:    int(slices.Max(values)),
		})
	}
	return aggregates
}
