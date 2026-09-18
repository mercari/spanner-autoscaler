package main

import (
	"strings"
	"testing"
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
