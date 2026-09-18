package monitoringemulator

import (
	"slices"
	"testing"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
)

func sumAgg(groupByFields ...string) *monitoringpb.Aggregation {
	return &monitoringpb.Aggregation{
		CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
		GroupByFields:      groupByFields,
	}
}

func maxAgg() *monitoringpb.Aggregation {
	return &monitoringpb.Aggregation{CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_MAX}
}

func TestAggregate(t *testing.T) {
	threeRegions := map[string]float64{
		"asia-northeast1": 0.31,
		"asia-northeast2": 0.14,
		"asia-northeast3": 0.08,
	}
	oneRegion := map[string]float64{"asia-northeast1": 0.31}

	tests := []struct {
		name      string
		regions   map[string]float64
		primary   *monitoringpb.Aggregation
		secondary *monitoringpb.Aggregation
		want      []float64
		wantErr   bool
	}{
		{
			// The bug PR #263 fixed: summing across regions with no grouping.
			name:    "old buggy shape: sum with no grouping inflates a multi-region instance",
			regions: threeRegions,
			primary: sumAgg(),
			want:    []float64{0.53},
		},
		{
			// The fix in PR #263: group by location, then take the max across regions.
			name:      "fixed shape: group by location then max reduces to the busiest region",
			regions:   threeRegions,
			primary:   sumAgg(locationGroupByField),
			secondary: maxAgg(),
			want:      []float64{0.31},
		},
		{
			name:    "regional instance: sum with no grouping is unaffected",
			regions: oneRegion,
			primary: sumAgg(),
			want:    []float64{0.31},
		},
		{
			name:      "regional instance: grouped and maxed is unaffected",
			regions:   oneRegion,
			primary:   sumAgg(locationGroupByField),
			secondary: maxAgg(),
			want:      []float64{0.31},
		},
		{
			name:    "grouped without a secondary aggregation returns one series per region",
			regions: threeRegions,
			primary: sumAgg(locationGroupByField),
			want:    []float64{0.31, 0.14, 0.08},
		},
		{
			name:    "nil primary aggregation defaults to summing everything",
			regions: threeRegions,
			want:    []float64{0.53},
		},
		{
			name:    "unsupported GroupByFields is an error",
			regions: threeRegions,
			primary: sumAgg("metric.label.database"),
			wantErr: true,
		},
		{
			name:      "unsupported secondary GroupByFields is an error",
			regions:   threeRegions,
			primary:   sumAgg(locationGroupByField),
			secondary: sumAgg(locationGroupByField),
			wantErr:   true,
		},
		{
			name:    "unsupported CrossSeriesReducer is an error",
			regions: threeRegions,
			primary: &monitoringpb.Aggregation{CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_MEAN},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := aggregate(tt.regions, tt.primary, tt.secondary)
			if (err != nil) != tt.wantErr {
				t.Fatalf("aggregate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			slices.Sort(got)
			want := slices.Clone(tt.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("aggregate() = %v, want %v", got, tt.want)
			}
		})
	}
}
