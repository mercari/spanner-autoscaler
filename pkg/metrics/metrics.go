package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang/protobuf/ptypes/duration"

	monitoring "cloud.google.com/go/monitoring/apiv3"
	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/golang/protobuf/ptypes/timestamp"
	"go.uber.org/zap"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	monitoringpb "google.golang.org/genproto/googleapis/monitoring/v3"
	"k8s.io/apimachinery/pkg/util/clock"

	"github.com/mercari/spanner-autoscaler/pkg/pointer"
)

const metricsFilterFormat = `
		metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND
		metric.label.priority = "high" AND
		resource.label.instance_id = "%s"
`

// InstanceMetrics represents metrics of Spanner instance.
type InstanceMetrics struct {
	CurrentHighPriorityCPUUtilization *int32
}

// Client is a client for manipulation of InstanceMetrics.
type Client interface {
	// GetInstanceMetrics gets the instance metrics by instance id.
	GetInstanceMetrics(ctx context.Context, instanceID string) (*InstanceMetrics, error)
}

// client is a client for Stackdriver Monitoring.
type client struct {
	monitoringMetricClient *monitoring.MetricClient

	projectID string
	term      time.Duration

	useCredentialsFile bool
	credentialsFile    string
	useCredentialsJSON bool
	credentialsJSON    []byte

	clock clock.Clock
	log   logr.Logger
}

var _ Client = (*client)(nil)

type Option func(*client)

func WithTerm(term time.Duration) Option {
	return func(c *client) {
		c.term = term
	}
}

func WithCredentialsFile(path string) Option {
	return func(c *client) {
		c.useCredentialsFile = true
		c.credentialsFile = path
	}
}

func WithCredentialsJSON(data []byte) Option {
	return func(c *client) {
		c.useCredentialsJSON = true
		c.credentialsJSON = data
	}
}

func WithClock(clock clock.Clock) Option {
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
func NewClient(ctx context.Context, projectID string, opts ...Option) (Client, error) {
	c := &client{
		projectID: projectID,
		term:      10 * time.Minute,
		clock:     clock.RealClock{},
		log:       zapr.NewLogger(zap.NewNop()),
	}

	for _, opt := range opts {
		opt(c)
	}

	var options []option.ClientOption

	if c.useCredentialsFile {
		options = append(options, option.WithCredentialsFile(c.credentialsFile))
	}

	if c.useCredentialsJSON {
		options = append(options, option.WithCredentialsJSON(c.credentialsJSON))
	}

	monitoringMetricClient, err := monitoring.NewMetricClient(ctx, options...)
	if err != nil {
		return nil, err
	}

	c.monitoringMetricClient = monitoringMetricClient

	return c, nil
}

// GetInstanceMetrics implements Client.
// https://cloud.google.com/monitoring/custom-metrics/reading-metrics?hl=ja#monitoring_read_timeseries_fields-go
func (c *client) GetInstanceMetrics(ctx context.Context, instanceID string) (*InstanceMetrics, error) {
	log := c.log.WithValues("instance id", instanceID)

	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", c.projectID),
		Filter: fmt.Sprintf(metricsFilterFormat, instanceID),
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

	for {
		resp, err := it.Next()
		if err == iterator.Done {
			break
		}

		if err != nil {
			log.Error(err, "unable to get metrics list time series response with iterator")
			return nil, err
		}

		// monitoringpb.Point.GetValue().GetDoubleValue() for CPU is in [0, 1].
		cpuPercent, err := firstPointAsPercent(resp.GetPoints())
		if err != nil {
			return nil, err
		}

		return &InstanceMetrics{
			CurrentHighPriorityCPUUtilization: pointer.Int32(cpuPercent),
		}, nil
	}

	return nil, errors.New("no such spanner instance metrics")
}

func firstPointAsPercent(points []*monitoringpb.Point) (percent int32, err error) {
	if len(points) == 0 {
		return 0, errors.New("invalid points")
	}

	return int32(points[0].GetValue().GetDoubleValue() * 100), nil
}
