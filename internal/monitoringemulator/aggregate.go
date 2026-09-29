package monitoringemulator

import (
	"fmt"
	"maps"
	"slices"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
)

// locationGroupByField is the only GroupByFields value internal/metrics/metrics.go
// ever sends.
const locationGroupByField = "resource.label.location"

// aggregate reproduces Cloud Monitoring's two-stage aggregation over a
// region-name -> CPU-utilization map. It supports exactly what
// internal/metrics/metrics.go sends: CrossSeriesReducer REDUCE_SUM,
// REDUCE_MAX, or REDUCE_NONE (no reduction, every series preserved), and
// GroupByFields either empty or [locationGroupByField]. Anything else is an
// error, so a future change to the real request shape fails loudly here
// instead of silently mis-simulating it.
//
// Without a secondary aggregation, one value per primary group is returned
// (this is what surfaces as more than one TimeSeries when the request
// groups by location without reducing the groups back down). With a
// secondary aggregation, the group results are further reduced into a
// single value.
func aggregate(regions map[string]float64, primary, secondary *monitoringpb.Aggregation) ([]float64, error) {
	groups, err := groupRegions(regions, primary.GetGroupByFields())
	if err != nil {
		return nil, fmt.Errorf("primary aggregation: %w", err)
	}

	reduced := make(map[string][]float64, len(groups))
	for key, values := range groups {
		vs, err := reduceCrossSeries(values, primary.GetCrossSeriesReducer())
		if err != nil {
			return nil, fmt.Errorf("primary aggregation: %w", err)
		}
		reduced[key] = vs
	}

	// Sort by group key so the result order is deterministic.
	var values []float64
	for _, key := range slices.Sorted(maps.Keys(reduced)) {
		values = append(values, reduced[key]...)
	}

	if secondary == nil {
		return values, nil
	}
	if len(secondary.GetGroupByFields()) > 0 {
		return nil, fmt.Errorf("secondary aggregation: GroupByFields is not supported by the emulator")
	}

	final, err := reduceCrossSeries(values, secondary.GetCrossSeriesReducer())
	if err != nil {
		return nil, fmt.Errorf("secondary aggregation: %w", err)
	}
	return final, nil
}

// groupRegions groups region CPU values by the requested GroupByFields.
// Empty fields put every region in one group (the group key "" is
// arbitrary); [locationGroupByField] puts each region in its own
// single-member group, keyed by the region name itself.
func groupRegions(regions map[string]float64, groupByFields []string) (map[string][]float64, error) {
	switch {
	case len(groupByFields) == 0:
		return map[string][]float64{"": slices.Collect(maps.Values(regions))}, nil
	case len(groupByFields) == 1 && groupByFields[0] == locationGroupByField:
		groups := make(map[string][]float64, len(regions))
		for region, value := range regions {
			groups[region] = []float64{value}
		}
		return groups, nil
	default:
		return nil, fmt.Errorf("unsupported GroupByFields %v: the emulator only supports %q or none", groupByFields, locationGroupByField)
	}
}

// reduceCrossSeries reduces values with the given reducer. REDUCE_NONE (the
// zero value) means no cross-series reduction in Cloud Monitoring, so every
// value is preserved (sorted for determinism); collapsing it to a sum would
// hide exactly the request-shape bugs this emulator exists to expose.
func reduceCrossSeries(values []float64, reducer monitoringpb.Aggregation_Reducer) ([]float64, error) {
	switch reducer {
	case monitoringpb.Aggregation_REDUCE_NONE:
		return slices.Sorted(slices.Values(values)), nil
	case monitoringpb.Aggregation_REDUCE_SUM:
		var sum float64
		for _, v := range values {
			sum += v
		}
		return []float64{sum}, nil
	case monitoringpb.Aggregation_REDUCE_MAX:
		if len(values) == 0 {
			return nil, fmt.Errorf("REDUCE_MAX over zero values")
		}
		return []float64{slices.Max(values)}, nil
	default:
		return nil, fmt.Errorf("unsupported CrossSeriesReducer %v: the emulator only supports REDUCE_NONE, REDUCE_SUM, and REDUCE_MAX", reducer)
	}
}
