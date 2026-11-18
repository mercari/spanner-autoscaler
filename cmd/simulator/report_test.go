package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

func TestSeriesPathsLiftPenAtGaps(t *testing.T) {
	bs := []tsBucket{
		{Has: true, Min: 1, Max: 2, Mean: 1.5},
		{Has: true, Min: 1, Max: 3, Mean: 2},
		{}, // gap
		{Has: true, Min: 4, Max: 5, Mean: 4.5},
		{Has: true, Min: 4, Max: 6, Mean: 5},
	}
	xAt := func(i int) float64 { return float64(i) }
	yAt := func(v float64) float64 { return v }

	band, line := seriesPaths(bs, xAt, yAt)
	// One closed envelope per contiguous run: the band must not bridge the gap.
	if moves, closes := strings.Count(band, "M"), strings.Count(band, "Z"); moves != 2 || closes != 2 {
		t.Errorf("band = %q; want two closed subpaths (M×2, Z×2), got M×%d Z×%d", band, moves, closes)
	}
	if moves := strings.Count(line, "M"); moves != 2 {
		t.Errorf("line = %q; want two subpaths (M×2), got M×%d", line, moves)
	}

	band, line = seriesPaths([]tsBucket{{}, {}}, xAt, yAt)
	if band != "" || line != "" {
		t.Errorf("all-gap series = (%q, %q); want empty paths", band, line)
	}
}

func TestBuildResultChartsTargets(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := func(targets ...int) *simulator.Result {
		result := &simulator.Result{}
		for i, target := range targets {
			result.Points = append(result.Points, simulator.SimPoint{
				Time:          start.Add(time.Duration(i) * time.Minute),
				SimPU:         1000,
				SimHighCPU:    new(60.0),
				TargetHighCPU: target,
			})
		}
		return result
	}
	cpuChart := func(t *testing.T, result *simulator.Result) lineChartView {
		t.Helper()
		charts := buildResultCharts(result, "pu", "cpu")
		if len(charts) != 2 {
			t.Fatalf("got %d charts; want 2", len(charts))
		}
		return charts[1]
	}

	t.Run("constant target is a reference line", func(t *testing.T) {
		chart := cpuChart(t, points(50, 50, 50))
		if len(chart.Refs) != 1 || chart.Refs[0].Label != "high-pri target 50%" {
			t.Errorf("Refs = %+v; want one line labeled high-pri target 50%%", chart.Refs)
		}
		if len(chart.Series) != 1 {
			t.Errorf("got %d series; want only the simulated CPU", len(chart.Series))
		}
	})

	t.Run("overridden target is a stepped series", func(t *testing.T) {
		// A gap tick (target 0) must not count as a third target value.
		chart := cpuChart(t, points(50, 70, 0, 50))
		if len(chart.Refs) != 0 {
			t.Errorf("Refs = %+v; want none", chart.Refs)
		}
		if len(chart.Series) != 2 || !chart.Series[1].Ref || chart.Series[1].Band != "" || chart.Series[1].Line == "" {
			t.Errorf("Series = %+v; want the simulated CPU and a reference series without a band", chart.Series)
		}
		if len(chart.Legend) != 2 || chart.Legend[1].Name != "high-pri target" {
			t.Errorf("Legend = %+v; want the target listed", chart.Legend)
		}
	})
}
