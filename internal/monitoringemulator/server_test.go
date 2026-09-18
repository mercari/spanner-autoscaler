package monitoringemulator

import (
	"testing"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestSeriesRange(t *testing.T) {
	end := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	t.Run("interval determines point count", func(t *testing.T) {
		gotEnd, minutes := seriesRange(&monitoringpb.TimeInterval{
			StartTime: timestamppb.New(end.Add(-25 * time.Minute)),
			EndTime:   timestamppb.New(end),
		})
		if !gotEnd.Equal(end) {
			t.Errorf("seriesRange() end = %s, want %s", gotEnd, end)
		}
		if minutes != 25 {
			t.Errorf("seriesRange() minutes = %d, want 25", minutes)
		}
	})

	t.Run("point count is capped", func(t *testing.T) {
		_, minutes := seriesRange(&monitoringpb.TimeInterval{
			StartTime: timestamppb.New(end.Add(-72 * time.Hour)),
			EndTime:   timestamppb.New(end),
		})
		if minutes != maxSeriesMinutes {
			t.Errorf("seriesRange() minutes = %d, want %d", minutes, maxSeriesMinutes)
		}
	})

	t.Run("at least one point", func(t *testing.T) {
		_, minutes := seriesRange(&monitoringpb.TimeInterval{
			StartTime: timestamppb.New(end),
			EndTime:   timestamppb.New(end),
		})
		if minutes != 1 {
			t.Errorf("seriesRange() minutes = %d, want 1", minutes)
		}
	})
}

func TestBuildResponse_PointSeries(t *testing.T) {
	end := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

	resp := buildResponse(end, 15, 0.42)
	if len(resp.GetTimeSeries()) != 1 {
		t.Fatalf("TimeSeries count = %d, want 1", len(resp.GetTimeSeries()))
	}
	points := resp.GetTimeSeries()[0].GetPoints()
	if len(points) != 15 {
		t.Fatalf("point count = %d, want 15", len(points))
	}
	for i, p := range points {
		wantEnd := end.Add(-time.Duration(i) * time.Minute)
		if got := p.GetInterval().GetEndTime().AsTime(); !got.Equal(wantEnd) {
			t.Errorf("point %d EndTime = %s, want %s (newest first, 1-minute apart)", i, got, wantEnd)
		}
		if got := p.GetValue().GetDoubleValue(); got != 0.42 {
			t.Errorf("point %d value = %v, want 0.42", i, got)
		}
	}
}

// TestListTimeSeries_ScenarioHistory verifies that scenario mode reconstructs
// the historical step values across the requested interval, so the metrics
// client's window aggregation sees the actual step transitions (the basis for
// testing "CPU sustained for N minutes" scaling rules end to end).
func TestListTimeSeries_ScenarioHistory(t *testing.T) {
	store := NewScenarioStore()
	high := func(v float64) *ScenarioMetric { return &ScenarioMetric{CPUUtilization: &v} }
	if err := store.Set("p", "i", []ScenarioStep{
		{Duration: Duration{10 * time.Minute}, HighPriority: high(0.2)},
		{Duration: Duration{10 * time.Minute}, HighPriority: high(0.8)},
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewMetricServiceServer(NewStaticStore(), NewWorkloadStore(), store, nil)

	// 12 minutes after scenario start: the active step is the second (0.8),
	// which has been active for 2 minutes; the 10 minutes before that were
	// the first step (0.2).
	end := time.Now().Add(12 * time.Minute)
	resp, err := srv.ListTimeSeries(t.Context(), &monitoringpb.ListTimeSeriesRequest{
		Name: "projects/p",
		Filter: `metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND
			metric.label.priority = "high" AND resource.label.instance_id = "i"`,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(end.Add(-12 * time.Minute)),
			EndTime:   timestamppb.New(end),
		},
	})
	if err != nil {
		t.Fatalf("ListTimeSeries() error: %v", err)
	}
	if len(resp.GetTimeSeries()) != 1 {
		t.Fatalf("TimeSeries count = %d, want 1", len(resp.GetTimeSeries()))
	}
	points := resp.GetTimeSeries()[0].GetPoints()
	if len(points) != 12 {
		t.Fatalf("point count = %d, want 12", len(points))
	}
	// Newest first: points 0..2 fall in the second step (0.8). The remaining
	// points fall in the first step (0.2); the exact boundary point depends
	// on sub-minute offsets of the scenario start, so allow one point of
	// slack around it.
	if got := points[0].GetValue().GetDoubleValue(); got != 0.8 {
		t.Errorf("newest point = %v, want 0.8 (second step)", got)
	}
	if got := points[11].GetValue().GetDoubleValue(); got != 0.2 {
		t.Errorf("oldest point = %v, want 0.2 (first step)", got)
	}
	var highCount int
	for _, p := range points {
		if p.GetValue().GetDoubleValue() == 0.8 {
			highCount++
		}
	}
	if highCount < 2 || highCount > 3 {
		t.Errorf("points in second step = %d, want 2 or 3", highCount)
	}
}
