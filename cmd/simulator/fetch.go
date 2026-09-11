/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"time"

	monitoring "cloud.google.com/go/monitoring/apiv3/v2"
	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

// The CPU filters mirror internal/metrics exactly so fetched values match
// what the controller's metrics client observes. The processing-units series
// is additionally required by the simulator to reconstruct the workload.
const (
	fetchFilterHighPriorityCPU = `metric.type = "spanner.googleapis.com/instance/cpu/utilization_by_priority" AND metric.label.priority = "high" AND resource.label.instance_id = "%s"`
	fetchFilterTotalCPU        = `metric.type = "spanner.googleapis.com/instance/cpu/utilization" AND resource.label.instance_id = "%s"`
	fetchFilterProcessingUnits = `metric.type = "spanner.googleapis.com/instance/processing_units" AND resource.label.instance_id = "%s"`
)

func runFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	project := fs.String("project", "", "GCP project ID (required)")
	instance := fs.String("instance", "", "Spanner instance ID (required)")
	startFlag := fs.String("start", "", "start of the range, RFC3339 (required)")
	endFlag := fs.String("end", "", "end of the range, RFC3339 (default: now)")
	out := fs.String("out", "", "output CSV path (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *project == "" || *instance == "" || *startFlag == "" {
		return errors.New("-project, -instance and -start are required")
	}

	start, err := time.Parse(time.RFC3339, *startFlag)
	if err != nil {
		return fmt.Errorf("invalid -start: %w", err)
	}
	end := time.Now().UTC()
	if *endFlag != "" {
		if end, err = time.Parse(time.RFC3339, *endFlag); err != nil {
			return fmt.Errorf("invalid -end: %w", err)
		}
	}
	if !end.After(start) {
		return errors.New("-end must be after -start")
	}

	ctx := context.Background()
	client, err := monitoring.NewMetricClient(ctx)
	if err != nil {
		return fmt.Errorf("creating Cloud Monitoring client (are Application Default Credentials configured?): %w", err)
	}
	defer client.Close()

	points, err := fetchPoints(ctx, client, *project, *instance, start, end)
	if err != nil {
		return err
	}

	w := os.Stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if err := simulator.WriteCSV(w, points); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "fetched %d points (%s .. %s)\n", len(points), start.Format(time.RFC3339), end.Format(time.RFC3339))
	return nil
}

// fetchPoints downloads the three per-minute series and joins them on their
// aligned timestamps. Rows without a processing-units sample are dropped
// (the simulator cannot reconstruct the workload without the instance size);
// missing CPU samples stay as empty cells.
func fetchPoints(ctx context.Context, client *monitoring.MetricClient, project, instance string, start, end time.Time) ([]simulator.Point, error) {
	byTime := map[time.Time]*simulator.Point{}
	upsert := func(t time.Time) *simulator.Point {
		p, ok := byTime[t]
		if !ok {
			p = &simulator.Point{Time: t}
			byTime[t] = p
		}
		return p
	}

	series := []struct {
		name   string
		filter string
		assign func(p *simulator.Point, v float64)
	}{
		{"high-priority cpu", fetchFilterHighPriorityCPU, func(p *simulator.Point, v float64) {
			cpu := v * 100
			p.HighPriorityCPU = &cpu
		}},
		{"total cpu", fetchFilterTotalCPU, func(p *simulator.Point, v float64) {
			cpu := v * 100
			p.TotalCPU = &cpu
		}},
		{"processing units", fetchFilterProcessingUnits, func(p *simulator.Point, v float64) {
			p.ProcessingUnits = int(math.Round(v))
		}},
	}

	for _, s := range series {
		if err := listSeries(ctx, client, project, fmt.Sprintf(s.filter, instance), start, end, func(t time.Time, v float64) {
			s.assign(upsert(t), v)
		}); err != nil {
			return nil, fmt.Errorf("fetching %s: %w", s.name, err)
		}
	}

	points := make([]simulator.Point, 0, len(byTime))
	for _, p := range slices.SortedFunc(maps.Values(byTime), func(a, b *simulator.Point) int {
		return a.Time.Compare(b.Time)
	}) {
		if p.ProcessingUnits <= 0 {
			continue
		}
		points = append(points, *p)
	}
	if len(points) == 0 {
		return nil, errors.New("no points fetched; check project/instance and the time range")
	}
	return points, nil
}

// listSeries issues a ListTimeSeries call with the same alignment the
// controller uses (ALIGN_MEAN over 60s, REDUCE_SUM across series) and streams
// every point of every returned series into visit.
func listSeries(ctx context.Context, client *monitoring.MetricClient, project, filter string, start, end time.Time, visit func(time.Time, float64)) error {
	req := &monitoringpb.ListTimeSeriesRequest{
		Name:   "projects/" + project,
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: timestamppb.New(start.UTC()),
			EndTime:   timestamppb.New(end.UTC()),
		},
		Aggregation: &monitoringpb.Aggregation{
			AlignmentPeriod:    durationpb.New(60 * time.Second),
			PerSeriesAligner:   monitoringpb.Aggregation_ALIGN_MEAN,
			CrossSeriesReducer: monitoringpb.Aggregation_REDUCE_SUM,
		},
		View: monitoringpb.ListTimeSeriesRequest_FULL,
	}

	it := client.ListTimeSeries(ctx, req)
	for {
		ts, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, p := range ts.GetPoints() {
			visit(p.GetInterval().GetEndTime().AsTime().UTC(), typedValue(p.GetValue()))
		}
	}
}

func typedValue(v *monitoringpb.TypedValue) float64 {
	switch t := v.GetValue().(type) {
	case *monitoringpb.TypedValue_DoubleValue:
		return t.DoubleValue
	case *monitoringpb.TypedValue_Int64Value:
		return float64(t.Int64Value)
	default:
		return 0
	}
}
