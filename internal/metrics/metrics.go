package metrics

import (
	"context"
	"errors"
	"fmt"
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
	utilclock "k8s.io/utils/clock"
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
}

// Client is a client for manipulation of InstanceMetrics.
type Client interface {
	// GetInstanceMetrics gets the instance metrics for the given metric type.
	GetInstanceMetrics(ctx context.Context, metricType MetricType) (*InstanceMetrics, error)
}

// client is a client for Stackdriver Monitoring.
type client struct {
	monitoringMetricClient *monitoring.MetricClient

	projectID  string
	instanceID string
	term       time.Duration

	endpoint    string
	tokenSource oauth2.TokenSource

	clock utilclock.Clock
	log   logr.Logger
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

func WithClock(clock utilclock.Clock) Option {
	return func(c *client) {
		c.clock = clock
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
		clock:      utilclock.RealClock{},
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
func (c *client) GetInstanceMetrics(ctx context.Context, metricType MetricType) (*InstanceMetrics, error) {
	log := c.log.WithValues("instance-id", c.instanceID, "project-id", c.projectID)

	log.V(1).Info("getting monitoring time series data")

	var filter string
	switch metricType {
	case MetricTypeTotal:
		filter = fmt.Sprintf(metricsFilterFormatTotal, c.instanceID)
	default: // MetricTypeHighPriority
		filter = fmt.Sprintf(metricsFilterFormatHighPriority, c.instanceID)
	}

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", c.projectID),
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: &timestamp.Timestamp{
				Seconds: c.clock.Now().UTC().Add(-c.term).Unix(),
			},
			EndTime: &timestamp.Timestamp{
				Seconds: c.clock.Now().UTC().Unix(),
			},
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    &duration.Duration{Seconds: 60},
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}

	it := c.monitoringMetricClient.ListTimeSeries(ctx, req)

	resp, err := it.Next()
	if err == iterator.Done {
		log.V(1).Info("could not get any time series metrics")
		return nil, errors.New("no such spanner instance metrics")
	}
	if err != nil {
		log.Error(err, "unable to get metrics list time series response with iterator")
		return nil, err
	}

	log.V(1).Info("got time series data points", "points", resp.GetPoints())

	// monitoringpb.Point.GetValue().GetDoubleValue() for CPU is in [0, 1].
	cpuPercent, err := firstPointAsPercent(resp.GetPoints())
	if err != nil {
		return nil, err
	}

	result := &InstanceMetrics{}
	switch metricType {
	case MetricTypeTotal:
		result.CurrentTotalCPUUtilization = cpuPercent
	default: // MetricTypeHighPriority
		result.CurrentHighPriorityCPUUtilization = cpuPercent
	}
	return result, nil
}

func firstPointAsPercent(points []*monitoringpb.Point) (percent int, err error) {
	if len(points) == 0 {
		return 0, errors.New("invalid points")
	}

	return int(points[0].GetValue().GetDoubleValue() * 100), nil
}
