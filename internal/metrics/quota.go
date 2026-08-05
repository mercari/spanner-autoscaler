package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/golang/protobuf/ptypes/timestamp"
	"google.golang.org/api/iterator"
)

const (
	spannerService                 = "spanner.googleapis.com"
	quotaLimitMetricType           = "serviceruntime.googleapis.com/quota/limit"
	quotaAllocationUsageMetricType = "serviceruntime.googleapis.com/quota/allocation/usage"
	regionalConfigPrefix           = "regional-"
	customConfigPrefix             = "custom-"
	defaultRegionalQuotaMetric     = "spanner.googleapis.com/nodes"
	defaultRegionalLimitName       = "SpannerNodesPerProject"
)

var (
	ErrUnsupportedInstanceConfig = errors.New("unsupported spanner instance configuration")
	ErrQuotaNoData               = errors.New("quota metrics not found")
	ErrQuotaMalformedResponse    = errors.New("quota metrics response is malformed")
)

// quotaTarget describes what to look for in Cloud Monitoring's quota time series.
// location is empty for multi/dual-region configs, where it is discovered from
// the limit query and reused for the usage query. limitName is empty for non-
// regional configs and is filled in from the limit time series's label.
type quotaTarget struct {
	configID    string
	quotaMetric string
	location    string
	limitName   string
}

// GetQuota implements Client.
func (c *client) GetQuota(ctx context.Context, instanceConfig string, now time.Time) (*Quota, error) {
	target, err := resolveQuotaTarget(instanceConfig)
	if err != nil {
		return nil, err
	}

	log := c.log.WithValues(
		"instance-id", c.instanceID,
		"project-id", c.projectID,
		"instance-config", instanceConfig,
		"config-id", target.configID,
		"quota-metric", target.quotaMetric,
	)

	// For regional, target.location is already the pre-derived region and is
	// used as the filter for both queries. For multi/dual-region it is empty
	// here, the limit query accepts any location, and we adopt the location
	// returned by Monitoring before issuing the usage query.
	limit, err := c.findQuotaTimeSeries(ctx, quotaLimitMetricType, target, target.location, now)
	if err != nil {
		log.Info("Could not get Spanner quota limit", "error", err)
		return nil, err
	}
	if limit.limitName != "" {
		target.limitName = limit.limitName
	}
	if target.location == "" {
		target.location = limit.location
	}

	usage, err := c.findQuotaTimeSeries(ctx, quotaAllocationUsageMetricType, target, target.location, now)
	if err != nil {
		log.Info("Could not get Spanner quota usage", "error", err)
		return nil, err
	}

	return &Quota{
		LimitNodes:  limit.value,
		UsageNodes:  usage.value,
		QuotaMetric: target.quotaMetric,
		LimitName:   target.limitName,
		Location:    target.location,
	}, nil
}

type quotaTimeSeries struct {
	value     int64
	location  string
	limitName string
}

func (c *client) findQuotaTimeSeries(ctx context.Context, metricType string, target quotaTarget, location string, now time.Time) (*quotaTimeSeries, error) {
	nowUTC := now.UTC()
	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   fmt.Sprintf("projects/%s", c.projectID),
		Filter: quotaFilter(metricType),
		Interval: &monitoringpb.TimeInterval{
			StartTime: &timestamp.Timestamp{Seconds: nowUTC.Add(-c.quotaTerm).Unix()},
			EndTime:   &timestamp.Timestamp{Seconds: nowUTC.Unix()},
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:  &duration.Duration{Seconds: int64(c.quotaTerm.Seconds())},
			PerSeriesAligner: monitoringpb.Aggregation_ALIGN_NEXT_OLDER,
		},
		View:     monitoringpb.ListTimeSeriesRequest_FULL,
		PageSize: 1000,
	}

	it := c.monitoringMetricClient.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		if !matchesQuotaTarget(ts, target, location) {
			continue
		}
		value, err := quotaPointValue(ts.GetPoints())
		if err != nil {
			return nil, err
		}
		resource := ts.GetResource()
		metric := ts.GetMetric()
		return &quotaTimeSeries{
			value:     value,
			location:  resource.GetLabels()["location"],
			limitName: metric.GetLabels()["limit_name"],
		}, nil
	}

	return nil, fmt.Errorf("%w: metric_type=%s quota_metric=%s location=%s", ErrQuotaNoData, metricType, target.quotaMetric, location)
}

func quotaFilter(metricType string) string {
	return fmt.Sprintf("metric.type = %q AND resource.type = %q AND resource.labels.service = %q", metricType, "consumer_quota", spannerService)
}

// matchesQuotaTarget filters time series by the target's quota_metric and
// (when known) limit_name labels. locationFilter restricts the resource
// location label; pass "" to accept any location (used by the limit query for
// multi/dual-region configs where the location is discovered, not pre-derived).
func matchesQuotaTarget(ts *monitoringpb.TimeSeries, target quotaTarget, locationFilter string) bool {
	metricLabels := ts.GetMetric().GetLabels()
	if metricLabels["quota_metric"] != target.quotaMetric {
		return false
	}
	if target.limitName != "" {
		if got := metricLabels["limit_name"]; got != "" && got != target.limitName {
			return false
		}
	}
	if locationFilter != "" && ts.GetResource().GetLabels()["location"] != locationFilter {
		return false
	}
	return true
}

func quotaPointValue(points []*monitoringpb.Point) (int64, error) {
	if len(points) == 0 || points[0].GetValue() == nil {
		return 0, ErrQuotaMalformedResponse
	}
	value := points[0].GetValue()
	switch v := value.GetValue().(type) {
	case *monitoringpb.TypedValue_Int64Value:
		return v.Int64Value, nil
	case *monitoringpb.TypedValue_DoubleValue:
		if math.Trunc(v.DoubleValue) != v.DoubleValue {
			return 0, ErrQuotaMalformedResponse
		}
		return int64(v.DoubleValue), nil
	default:
		return 0, ErrQuotaMalformedResponse
	}
}

func resolveQuotaTarget(instanceConfig string) (quotaTarget, error) {
	configID := instanceConfig
	if i := strings.LastIndex(instanceConfig, "/"); i >= 0 {
		configID = instanceConfig[i+1:]
	}
	if configID == "" || strings.HasPrefix(configID, customConfigPrefix) {
		return quotaTarget{configID: configID}, fmt.Errorf("%w: %s", ErrUnsupportedInstanceConfig, instanceConfig)
	}

	if strings.HasPrefix(configID, regionalConfigPrefix) {
		location := strings.TrimPrefix(configID, regionalConfigPrefix)
		if location == "" {
			return quotaTarget{configID: configID}, fmt.Errorf("%w: %s", ErrUnsupportedInstanceConfig, instanceConfig)
		}
		return quotaTarget{
			configID:    configID,
			quotaMetric: defaultRegionalQuotaMetric,
			location:    location,
			limitName:   defaultRegionalLimitName,
		}, nil
	}

	return quotaTarget{
		configID:    configID,
		quotaMetric: "spanner.googleapis.com/nodes_" + strings.ReplaceAll(configID, "-", "_"),
	}, nil
}
