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

package simulator

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// CSV column names understood by LoadCSV / written by WritePointsCSV's input
// counterpart WriteCSV. CPU values are percentages in [0, 100].
const (
	columnTime            = "time"
	columnProcessingUnits = "processing_units"
	columnHighPriorityCPU = "high_priority_cpu"
	columnTotalCPU        = "total_cpu"
)

// LoadCSV reads metric points from a CSV stream with a header line.
// Required columns: time (RFC3339) and processing_units. Optional columns:
// high_priority_cpu and total_cpu (percent, empty cell = not recorded).
// Column order is free; unknown columns are ignored.
func LoadCSV(r io.Reader) ([]Point, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true

	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("reading csv header: %w", err)
	}

	col := make(map[string]int, len(header))
	for i, name := range header {
		col[strings.TrimSpace(strings.ToLower(name))] = i
	}
	timeIdx, ok := col[columnTime]
	if !ok {
		return nil, fmt.Errorf("csv header: missing required column %q", columnTime)
	}
	puIdx, ok := col[columnProcessingUnits]
	if !ok {
		return nil, fmt.Errorf("csv header: missing required column %q", columnProcessingUnits)
	}
	highIdx, hasHigh := col[columnHighPriorityCPU]
	totalIdx, hasTotal := col[columnTotalCPU]

	var points []Point
	for line := 2; ; line++ {
		record, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csv line %d: %w", line, err)
		}

		t, err := time.Parse(time.RFC3339, strings.TrimSpace(record[timeIdx]))
		if err != nil {
			return nil, fmt.Errorf("csv line %d: invalid %s: %w", line, columnTime, err)
		}

		pu, err := parseCSVFloat(record[puIdx])
		if err != nil || pu == nil {
			return nil, fmt.Errorf("csv line %d: invalid %s %q", line, columnProcessingUnits, record[puIdx])
		}

		p := Point{Time: t, ProcessingUnits: int(math.Round(*pu))}
		if hasHigh {
			if p.HighPriorityCPU, err = parseCSVFloat(record[highIdx]); err != nil {
				return nil, fmt.Errorf("csv line %d: invalid %s %q", line, columnHighPriorityCPU, record[highIdx])
			}
		}
		if hasTotal {
			if p.TotalCPU, err = parseCSVFloat(record[totalIdx]); err != nil {
				return nil, fmt.Errorf("csv line %d: invalid %s %q", line, columnTotalCPU, record[totalIdx])
			}
		}
		points = append(points, p)
	}
	if len(points) == 0 {
		return nil, fmt.Errorf("csv contains no data rows")
	}
	return points, nil
}

// WriteCSV writes points in the format LoadCSV reads, so fetched metrics can
// be stored and replayed.
func WriteCSV(w io.Writer, points []Point) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{columnTime, columnProcessingUnits, columnHighPriorityCPU, columnTotalCPU}); err != nil {
		return err
	}
	formatCPU := func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', 4, 64)
	}
	for _, p := range points {
		record := []string{
			p.Time.UTC().Format(time.RFC3339),
			strconv.Itoa(p.ProcessingUnits),
			formatCPU(p.HighPriorityCPU),
			formatCPU(p.TotalCPU),
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("writing csv: %w", err)
	}
	return nil
}

func parseCSVFloat(s string) (*float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, err
	}
	// ParseFloat accepts NaN, infinities, and negatives; none is a valid
	// measurement, and NaN in particular compares false against every
	// threshold, so it would silently pass the simulation's constraints.
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil, fmt.Errorf("not a finite non-negative number")
	}
	return &v, nil
}
