package metrics

import (
	"slices"
	"strings"
	"testing"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/golang/protobuf/ptypes/timestamp"
)

func TestBuildListTimeSeriesRequest(t *testing.T) {
	now := time.Date(2026, 9, 15, 9, 8, 0, 0, time.FixedZone("JST", 9*60*60))
	term := 10 * time.Minute

	tests := []struct {
		name           string
		metricType     MetricType
		wantFilterHas  []string
		wantFilterOmit []string
	}{
		{
			name:       "high priority",
			metricType: MetricTypeHighPriority,
			wantFilterHas: []string{
				`metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority"`,
				`metric.label.priority = "high"`,
				`resource.label.instance_id = "my-instance"`,
			},
			wantFilterOmit: []string{"resource.label.location"},
		},
		{
			name:       "total",
			metricType: MetricTypeTotal,
			wantFilterHas: []string{
				`metric.type = "spanner.googleapis.com/instance/cpu/utilization"`,
				`resource.label.instance_id = "my-instance"`,
			},
			wantFilterOmit: []string{"metric.label.priority", "resource.label.location"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &client{projectID: "my-project", instanceID: "my-instance", term: term}

			req := c.buildListTimeSeriesRequest(tt.metricType, now, nil)

			if got, want := req.GetName(), "projects/my-project"; got != want {
				t.Errorf("Name = %q, want %q", got, want)
			}
			for _, want := range tt.wantFilterHas {
				if !strings.Contains(req.GetFilter(), want) {
					t.Errorf("Filter = %q, want it to contain %q", req.GetFilter(), want)
				}
			}
			for _, unwanted := range tt.wantFilterOmit {
				if strings.Contains(req.GetFilter(), unwanted) {
					t.Errorf("Filter = %q, want it to omit %q", req.GetFilter(), unwanted)
				}
			}

			if got, want := req.GetInterval().GetEndTime().GetSeconds(), now.UTC().Unix(); got != want {
				t.Errorf("Interval.EndTime = %d, want %d", got, want)
			}
			if got, want := req.GetInterval().GetStartTime().GetSeconds(), now.UTC().Add(-term).Unix(); got != want {
				t.Errorf("Interval.StartTime = %d, want %d", got, want)
			}

			agg := req.GetAggregation()
			if got, want := agg.GetAlignmentPeriod().GetSeconds(), int64(60); got != want {
				t.Errorf("Aggregation.AlignmentPeriod = %ds, want %ds", got, want)
			}
			if got, want := agg.GetPerSeriesAligner(), monitoringpb.Aggregation_ALIGN_MEAN; got != want {
				t.Errorf("Aggregation.PerSeriesAligner = %v, want %v", got, want)
			}
			if got, want := agg.GetCrossSeriesReducer(), monitoringpb.Aggregation_REDUCE_SUM; got != want {
				t.Errorf("Aggregation.CrossSeriesReducer = %v, want %v", got, want)
			}
			// Without this grouping the per-region series of a multi-region instance are summed.
			if got, want := agg.GetGroupByFields(), []string{"resource.label.location"}; !slices.Equal(got, want) {
				t.Errorf("Aggregation.GroupByFields = %v, want %v", got, want)
			}

			sec := req.GetSecondaryAggregation()
			if sec == nil {
				t.Fatal("SecondaryAggregation is nil, want the per-region groups reduced to a single series")
			}
			if got, want := sec.GetAlignmentPeriod().GetSeconds(), int64(60); got != want {
				t.Errorf("SecondaryAggregation.AlignmentPeriod = %ds, want %ds", got, want)
			}
			if got, want := sec.GetPerSeriesAligner(), monitoringpb.Aggregation_ALIGN_MEAN; got != want {
				t.Errorf("SecondaryAggregation.PerSeriesAligner = %v, want %v", got, want)
			}
			if got, want := sec.GetCrossSeriesReducer(), monitoringpb.Aggregation_REDUCE_MAX; got != want {
				t.Errorf("SecondaryAggregation.CrossSeriesReducer = %v, want %v", got, want)
			}
			if got := sec.GetGroupByFields(); len(got) != 0 {
				t.Errorf("SecondaryAggregation.GroupByFields = %v, want empty so every region collapses into one series", got)
			}

			if got, want := req.GetView(), monitoringpb.ListTimeSeriesRequest_FULL; got != want {
				t.Errorf("View = %v, want %v", got, want)
			}
		})
	}
}

// makePoints builds a newest-first 1-minute point series ending at end, one
// point per value (values[0] is the newest).
func makePoints(end time.Time, values ...float64) []*monitoringpb.Point {
	points := make([]*monitoringpb.Point, len(values))
	for i, v := range values {
		t := end.Add(-time.Duration(i) * time.Minute)
		points[i] = &monitoringpb.Point{
			Interval: &monitoringpb.TimeInterval{
				StartTime: &timestamp.Timestamp{Seconds: t.Add(-time.Minute).Unix()},
				EndTime:   &timestamp.Timestamp{Seconds: t.Unix()},
			},
			Value: &monitoringpb.TypedValue{
				Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: v},
			},
		}
	}
	return points
}

func TestWindowAggregates(t *testing.T) {
	end := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	t.Run("full window anchored at newest point", func(t *testing.T) {
		// 5 points, newest first: 60%, 55%, 50%, 45%, 40%.
		points := makePoints(end, 0.60, 0.55, 0.50, 0.45, 0.40)
		got := windowAggregates(points, []time.Duration{3 * time.Minute})
		want := []WindowAggregate{{Window: 3 * time.Minute, Min: 50, Avg: 55, Max: 60}}
		if !slices.Equal(got, want) {
			t.Errorf("windowAggregates() = %+v, want %+v", got, want)
		}
	})

	t.Run("window with too few points is omitted", func(t *testing.T) {
		points := makePoints(end, 0.60, 0.55)
		if got := windowAggregates(points, []time.Duration{3 * time.Minute}); len(got) != 0 {
			t.Errorf("windowAggregates() = %+v, want empty for an uncovered window", got)
		}
	})

	t.Run("multiple windows one series", func(t *testing.T) {
		points := makePoints(end, 0.60, 0.20, 0.20, 0.20, 0.20)
		got := windowAggregates(points, []time.Duration{2 * time.Minute, 5 * time.Minute})
		want := []WindowAggregate{
			{Window: 2 * time.Minute, Min: 20, Avg: 40, Max: 60},
			{Window: 5 * time.Minute, Min: 20, Avg: 28, Max: 60},
		}
		if !slices.Equal(got, want) {
			t.Errorf("windowAggregates() = %+v, want %+v", got, want)
		}
	})

	t.Run("no windows requested", func(t *testing.T) {
		if got := windowAggregates(makePoints(end, 0.60), nil); got != nil {
			t.Errorf("windowAggregates() = %+v, want nil", got)
		}
	})
}

func TestBuildListTimeSeriesRequest_WindowTerm(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	c := &client{projectID: "my-project", instanceID: "my-instance", term: 10 * time.Minute}

	req := c.buildListTimeSeriesRequest(MetricTypeHighPriority, now, []time.Duration{15 * time.Minute, time.Hour})
	start := time.Unix(req.GetInterval().GetStartTime().GetSeconds(), 0)
	// The largest window (1h) plus the base term (10m) for ingestion delay.
	if want := now.Add(-(time.Hour + 10*time.Minute)); !start.Equal(want) {
		t.Errorf("StartTime = %s, want %s", start, want)
	}
}
