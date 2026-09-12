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
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

// The reports are single self-contained HTML files: inline CSS/SVG/JS, no
// external resources, light and dark mode from the same validated palette
// (categorical blue/orange pass the palette validator in both modes; the
// recorded series uses the de-emphasis gray of the emphasis form, with
// identity carried by the legend and tooltip, not color alone).
//
// Rendering is html/template driven: Go computes view models (coordinates,
// paths, formatted labels) and the templates own all markup. Chart geometry
// is fixed, so the plot-box coordinates live directly in the template text.

// ---- view models ----

type legendItem struct {
	Name  string
	Color template.CSS
}

type kpiView struct{ Label, Value, Note string }

type verdictView struct {
	Label  string
	MinPU  string
	Pinned string
	Floor  string // empty when the run never pinned with usable data
	Lower  string
	Raise  string
}

type conclusionRow struct {
	Parameter, Current, Recommended string
	Changed                         bool
}

type conclusionView struct {
	None   bool
	Reason string
	Rows   []conclusionRow
	Effect string
}

type tickView struct {
	Pos      string // formatted coordinate of the tick
	LabelPos string // formatted coordinate of its label
	Label    string
}

type refView struct {
	Y, LabelY, Label string
}

type seriesView struct {
	Color      template.CSS
	Band, Line string // SVG path data; Band may be empty
}

type bucketRowView struct {
	Time  string
	Cells []string
}

type lineChartView struct {
	Title, Unit   string
	Legend        []legendItem
	YTicks        []tickView
	XTicks        []tickView
	Refs          []refView
	Series        []seriesView
	DataJSON      template.JS
	BucketHeaders []string
	BucketRows    []bucketRowView
}

type dotView struct {
	CX, CY string
	Color  template.CSS
	Info   string // JSON consumed by the tooltip script
}

type scatterView struct {
	Legend []legendItem
	YTicks []tickView
	XTicks []tickView
	Dots   []dotView
}

type eventView struct{ Time, From, To, Delta string }

type candidateRowView struct {
	Rank, Label, Error                 string
	Infeasible, Recommended            bool
	Saved, DSaved, Exceeded, DExceeded string
	Gaps, Steps, P99, Status           string
}

type simulatePage struct {
	CSS     template.CSS
	JS      template.JS
	Period  string
	Points  int
	KPIs    []kpiView
	Verdict verdictView
	Charts  []lineChartView
	Events  []eventView
}

type recommendPage struct {
	CSS        template.CSS
	JS         template.JS
	Sub        string
	Conclusion conclusionView
	KPIs       []kpiView
	Charts     []lineChartView
	Scatter    scatterView
	Verdicts   []verdictView
	Rows       []candidateRowView
}

// ---- page builders ----

// writeSimulateHTML renders the full report for one simulation run: KPI row,
// the recorded-vs-simulated PU timeline (emphasis form: the simulation in the
// accent hue, the recording as gray context), the simulated CPU timeline with
// target reference lines, the min-PU assessment, and the scale-event table.
func writeSimulateHTML(w io.Writer, result *simulator.Result, targetHigh, targetTotal int) error {
	s := result.Summary
	days := max(s.End.Sub(s.Start).Hours()/24, 1)

	page := simulatePage{
		CSS:    reportCSS,
		JS:     reportJS,
		Period: s.Start.UTC().Format("2006-01-02 15:04") + " .. " + s.End.UTC().Format("2006-01-02 15:04"),
		Points: s.DataPoints,
		KPIs: []kpiView{
			{"PU-hours saved", fmt.Sprintf("%.1f%%", s.PUHoursSavedPercent),
				fmt.Sprintf("recorded %s → simulated %s", commaInt(int(s.ActualPUHours)), commaInt(int(s.SimPUHours)))},
			{"above target", fmt.Sprintf("%.0f min", s.TargetExceededMinutes),
				fmt.Sprintf("%.1f min/day", s.TargetExceededMinutes/days)},
			{"pinned at min PU", fmt.Sprintf("%.0f%%", s.MinPinnedPercent),
				fmt.Sprintf("workload floor p95 %s PU", commaInt(s.RequiredPUAtMinP95))},
			{"scale events", fmt.Sprintf("%d ↑ / %d ↓", s.ScaleUps, s.ScaleDowns),
				fmt.Sprintf("%d steps >2x, %d gaps <10m", s.ScaleStepViolations, s.ScaleGapsUnder10Min)},
			{"low confidence", fmt.Sprintf("%.0f min", s.LowConfidenceMinutes),
				"recorded CPU above the model threshold"},
		},
		Verdict: buildVerdict("this config", s),
	}

	page.Charts = buildResultCharts(result, targetHigh, targetTotal,
		"Processing units — simulated vs recorded", "Simulated CPU utilization")

	for _, e := range result.Events {
		page.Events = append(page.Events, eventView{
			Time:  e.Time.UTC().Format("2006-01-02 15:04"),
			From:  commaInt(e.FromPU),
			To:    commaInt(e.ToPU),
			Delta: fmt.Sprintf("%+d", e.ToPU-e.FromPU),
		})
	}

	return reportTemplates.ExecuteTemplate(w, "simulate", page)
}

// writeRecommendHTML renders the candidate ranking visually: the conclusion
// block up front (current → recommended per parameter), a savings-vs-risk scatter
// (feasible candidates in the accent hue, infeasible as gray context, the
// base config as the orange reference), the min-PU assessments, and the full
// candidate table with rejection reasons as the table view.
// topResult, when non-nil, is a full re-run of the top feasible candidate; its
// PU/CPU timelines are embedded so the recommendation can be judged from the
// simulated behavior, not only from aggregate numbers.
func writeRecommendHTML(w io.Writer, current map[string]string, base simulator.Summary, candidates []simulator.Candidate, topResult *simulator.Result, topTargetHigh, topTargetTotal int) error {
	feasibleCount := 0
	for _, c := range candidates {
		if c.Feasible {
			feasibleCount++
		}
	}

	page := recommendPage{
		CSS: reportCSS,
		JS:  reportJS,
		Sub: fmt.Sprintf("%d candidates (%d feasible) against %d recorded points, %s .. %s",
			len(candidates), feasibleCount, base.DataPoints,
			base.Start.UTC().Format("2006-01-02"), base.End.UTC().Format("2006-01-02")),
		Conclusion: buildConclusion(current, base, candidates),
		KPIs: []kpiView{
			{"base PU-hours saved", fmt.Sprintf("%.1f%%", base.PUHoursSavedPercent), "replay of the current config vs recorded"},
			{"base above target", fmt.Sprintf("%.0f min", base.TargetExceededMinutes), "risk reference for the deltas"},
			{"feasible candidates", fmt.Sprintf("%d / %d", feasibleCount, len(candidates)), "under the given constraints"},
		},
		Scatter:  buildScatter(base, candidates),
		Verdicts: []verdictView{buildVerdict("base", base)},
	}
	if topResult != nil {
		page.Charts = buildResultCharts(topResult, topTargetHigh, topTargetTotal,
			"Processing units — recommended candidate (simulated) vs recorded",
			"CPU utilization — recommended candidate (simulated)")
	}
	if idx, _ := recommendedIndex(base, candidates); idx >= 0 {
		page.Verdicts = append(page.Verdicts, buildVerdict("recommended candidate", candidates[idx].Summary))
	}

	// Mark the row the conclusion recommends (if any), so the two sections
	// cross-reference without comparing values by hand.
	recIdx, _ := recommendedIndex(base, candidates)
	for i, c := range candidates {
		row := candidateRowView{
			Rank:  strconv.Itoa(i + 1),
			Label: simulator.DescribeOverrides(c.Overrides),
			Error: c.Error,
		}
		if c.Error == "" {
			cs := c.Summary
			row.Infeasible = !c.Feasible
			row.Saved = fmt.Sprintf("%.1f%%", cs.PUHoursSavedPercent)
			row.DSaved = fmt.Sprintf("%+.1f", cs.PUHoursSavedPercent-base.PUHoursSavedPercent)
			row.Exceeded = fmt.Sprintf("%.0f min", cs.TargetExceededMinutes)
			row.DExceeded = fmt.Sprintf("%+.0f", cs.TargetExceededMinutes-base.TargetExceededMinutes)
			row.Gaps = strconv.Itoa(cs.ScaleGapsUnder10Min)
			row.Steps = strconv.Itoa(cs.ScaleStepViolations)
			row.P99 = formatP99(cs.SimHighPriorityCPU)
			row.Status = "feasible"
			if !c.Feasible {
				row.Status = "infeasible: " + strings.Join(c.InfeasibleReasons, "; ")
			} else if i == recIdx {
				row.Recommended = true
				row.Status = "recommended"
			}
		}
		page.Rows = append(page.Rows, row)
	}

	return reportTemplates.ExecuteTemplate(w, "recommend", page)
}

// buildResultCharts renders one run as the two report timelines: simulated
// versus recorded processing units, and the simulated CPU with its targets
// as reference lines.
func buildResultCharts(result *simulator.Result, targetHigh, targetTotal int, puTitle, cpuTitle string) []lineChartView {
	times, simPU := downsample(result.Points, buckets, func(p simulator.SimPoint) (float64, bool) {
		return float64(p.SimPU), true
	})
	_, actualPU := downsample(result.Points, buckets, func(p simulator.SimPoint) (float64, bool) {
		return float64(p.ActualPU), true
	})
	charts := []lineChartView{buildLineChart(puTitle, "PU", times, []tsSeries{
		{Name: "simulated", Color: "var(--series-1)", Buckets: simPU},
		{Name: "recorded", Color: "var(--context)", Buckets: actualPU},
	}, nil)}

	var cpuSeries []tsSeries
	var refs []refLine
	if _, hp := downsample(result.Points, buckets, simHighCPU); hasData(hp) {
		cpuSeries = append(cpuSeries, tsSeries{Name: "high-priority (simulated)", Color: "var(--series-1)", Buckets: hp})
		if targetHigh > 0 {
			refs = append(refs, refLine{Y: float64(targetHigh), Label: fmt.Sprintf("high-pri target %d%%", targetHigh)})
		}
	}
	if _, tt := downsample(result.Points, buckets, simTotalCPU); hasData(tt) {
		cpuSeries = append(cpuSeries, tsSeries{Name: "total (simulated)", Color: "var(--series-2)", Buckets: tt})
		if targetTotal > 0 {
			refs = append(refs, refLine{Y: float64(targetTotal), Label: fmt.Sprintf("total target %d%%", targetTotal)})
		}
	}
	if len(cpuSeries) > 0 {
		charts = append(charts, buildLineChart(cpuTitle, "%", times, cpuSeries, refs))
	}
	return charts
}

func buildVerdict(label string, s simulator.Summary) verdictView {
	a := s.AssessMinPU()
	v := verdictView{
		Label:  label,
		MinPU:  commaInt(s.SpecMinPU),
		Pinned: fmt.Sprintf("%.0f%%", s.MinPinnedPercent),
		Lower:  a.Lower,
		Raise:  a.Raise,
	}
	if s.RequiredPUAtMinP95 > 0 {
		v.Floor = commaInt(s.RequiredPUAtMinP95)
	}
	return v
}

func buildConclusion(current map[string]string, base simulator.Summary, candidates []simulator.Candidate) conclusionView {
	idx, keepReason := recommendedIndex(base, candidates)
	if idx < 0 {
		return conclusionView{None: true, Reason: keepReason}
	}
	top := &candidates[idx]
	view := conclusionView{}
	for _, key := range simulator.OverrideKeys {
		cur, ok := current[key]
		if !ok {
			continue
		}
		row := conclusionRow{Parameter: key, Current: cur, Recommended: cur}
		if next, changed := top.Overrides[key]; changed && next != cur {
			row.Recommended = next
			row.Changed = true
		}
		view.Rows = append(view.Rows, row)
	}
	s := top.Summary
	view.Effect = fmt.Sprintf("effect: saves %.1f%% PU-hours (%+.1f vs current) · above target %.0f min (%+.0f) · gaps<10m %d (%+d)",
		s.PUHoursSavedPercent, s.PUHoursSavedPercent-base.PUHoursSavedPercent,
		s.TargetExceededMinutes, s.TargetExceededMinutes-base.TargetExceededMinutes,
		s.ScaleGapsUnder10Min, s.ScaleGapsUnder10Min-base.ScaleGapsUnder10Min)
	return view
}

// ---- line-chart geometry ----

// tsBucket is one downsampled slot of a time series. Min/max keep the 1-minute
// spikes visible after downsampling; the mean line shows the trend.
type tsBucket struct {
	Min, Max, Mean float64
	Has            bool
}

type tsSeries struct {
	Name    string
	Color   template.CSS
	Buckets []tsBucket
}

type refLine struct {
	Y     float64
	Label string
}

// The fixed plot box; the same values are hardcoded in the template markup.
const (
	plotX0    = 68.0
	plotX1    = 940.0
	plotY0    = 14.0
	plotY1    = 250.0
	scatterY0 = 16.0
	scatterY1 = 366.0
	buckets   = 440
)

// downsample splits points into n uniform time buckets and aggregates value(p)
// per bucket. Returned bucket midpoints are shared by every series of a chart.
func downsample(points []simulator.SimPoint, n int, value func(simulator.SimPoint) (float64, bool)) ([]int64, []tsBucket) {
	if len(points) == 0 {
		return nil, nil
	}
	start := points[0].Time.UnixMilli()
	end := points[len(points)-1].Time.UnixMilli()
	span := max(end-start, 1)

	times := make([]int64, n)
	for i := range n {
		times[i] = start + span*int64(2*i+1)/int64(2*n)
	}
	bs := make([]tsBucket, n)
	counts := make([]int, n)
	for _, p := range points {
		v, ok := value(p)
		if !ok {
			continue
		}
		i := min(int((p.Time.UnixMilli()-start)*int64(n)/span), n-1)
		b := &bs[i]
		if !b.Has {
			*b = tsBucket{Min: v, Max: v, Has: true}
		} else {
			b.Min = min(b.Min, v)
			b.Max = max(b.Max, v)
		}
		b.Mean += v
		counts[i]++
	}
	for i := range bs {
		if counts[i] > 0 {
			bs[i].Mean /= float64(counts[i])
		}
	}
	return times, bs
}

func buildLineChart(title, unit string, times []int64, series []tsSeries, refs []refLine) lineChartView {
	var yMax float64
	for _, s := range series {
		for _, b := range s.Buckets {
			if b.Has {
				yMax = max(yMax, b.Max)
			}
		}
	}
	for _, r := range refs {
		yMax = max(yMax, r.Y)
	}
	if yMax == 0 {
		yMax = 1
	}
	ticks := niceTicks(yMax * 1.05)
	yTop := ticks[len(ticks)-1]

	xAt := func(i int) float64 {
		if len(times) <= 1 {
			return plotX0
		}
		return plotX0 + float64(i)/float64(len(times)-1)*(plotX1-plotX0)
	}
	yAt := func(v float64) float64 { return plotY1 - v/yTop*(plotY1-plotY0) }

	view := lineChartView{Title: title, Unit: unit}
	for _, s := range series {
		view.Legend = append(view.Legend, legendItem{Name: s.Name, Color: s.Color})
	}
	for _, t := range ticks {
		y := yAt(t)
		view.YTicks = append(view.YTicks, tickView{Pos: coord(y), LabelPos: coord(y + 4), Label: formatTick(t, unit)})
	}
	for _, i := range xTickIndexes(len(times)) {
		view.XTicks = append(view.XTicks, tickView{
			Pos:   coord(xAt(i)),
			Label: time.UnixMilli(times[i]).UTC().Format("01-02"),
		})
	}
	for _, r := range refs {
		y := yAt(r.Y)
		view.Refs = append(view.Refs, refView{Y: coord(y), LabelY: coord(y - 4), Label: r.Label})
	}
	for _, s := range series {
		band, line := seriesPaths(s.Buckets, xAt, yAt)
		view.Series = append(view.Series, seriesView{Color: s.Color, Band: band, Line: line})
	}
	view.DataJSON = chartJSON(unit, times, series)

	for _, s := range series {
		view.BucketHeaders = append(view.BucketHeaders, s.Name+" (min)", s.Name+" (max)")
	}
	for i, t := range times {
		row := bucketRowView{Time: time.UnixMilli(t).UTC().Format("2006-01-02 15:04")}
		for _, s := range series {
			b := s.Buckets[i]
			if !b.Has {
				row.Cells = append(row.Cells, "", "")
				continue
			}
			row.Cells = append(row.Cells, formatTick(b.Min, unit), formatTick(b.Max, unit))
		}
		view.BucketRows = append(view.BucketRows, row)
	}
	return view
}

// seriesPaths returns the min-max envelope (a closed band) and the mean line
// as SVG path data. Gaps in the data lift the pen.
func seriesPaths(bs []tsBucket, xAt func(int) float64, yAt func(float64) float64) (band, line string) {
	var upper, lower, mean strings.Builder
	pen := "M"
	for i, b := range bs {
		if !b.Has {
			pen = "M"
			continue
		}
		x := xAt(i)
		fmt.Fprintf(&mean, "%s%.1f %.1f ", pen, x, yAt(b.Mean))
		fmt.Fprintf(&upper, "%s%.1f %.1f ", pen, x, yAt(b.Max))
		fmt.Fprintf(&lower, "L%.1f %.1f ", x, yAt(b.Min))
		pen = "L"
	}
	if upper.Len() == 0 {
		return "", ""
	}
	// Lower edge walks back right-to-left to close the envelope.
	low := strings.Fields(lower.String())
	var back strings.Builder
	for i := len(low) - 2; i >= 0; i -= 2 {
		fmt.Fprintf(&back, "%s %s ", strings.TrimPrefix(low[i], "L"), low[i+1])
	}
	return upper.String() + "L" + back.String() + "Z", mean.String()
}

// chartJSON is the payload behind the crosshair tooltip. Marked template.JS:
// the content is produced entirely by encoding/json over data this program
// computed.
func chartJSON(unit string, times []int64, series []tsSeries) template.JS {
	type jsSeries struct {
		Name  string       `json:"name"`
		Color template.CSS `json:"color"`
		Min   []*float64   `json:"min"`
		Max   []*float64   `json:"max"`
		Mean  []*float64   `json:"mean"`
	}
	payload := struct {
		T      []int64            `json:"t"`
		Unit   string             `json:"unit"`
		Series []jsSeries         `json:"series"`
		Plot   map[string]float64 `json:"plot"`
	}{
		T:    times,
		Unit: unit,
		Plot: map[string]float64{"x0": plotX0, "x1": plotX1, "w": 960},
	}
	for _, s := range series {
		js := jsSeries{Name: s.Name, Color: s.Color}
		for _, b := range s.Buckets {
			if !b.Has {
				js.Min = append(js.Min, nil)
				js.Max = append(js.Max, nil)
				js.Mean = append(js.Mean, nil)
				continue
			}
			mn, mx, me := b.Min, b.Max, b.Mean
			js.Min = append(js.Min, &mn)
			js.Max = append(js.Max, &mx)
			js.Mean = append(js.Mean, &me)
		}
		payload.Series = append(payload.Series, js)
	}
	buf, _ := json.Marshal(payload)
	return template.JS(strings.ReplaceAll(string(buf), "</", "<\\/")) //nolint:gosec // program-generated JSON, see above
}

// ---- scatter geometry ----

func buildScatter(base simulator.Summary, candidates []simulator.Candidate) scatterView {
	type dot struct {
		x, y  float64
		color template.CSS
		info  map[string]any
	}
	dots := []dot{{
		x: base.TargetExceededMinutes, y: base.PUHoursSavedPercent, color: "var(--series-2)",
		info: map[string]any{"label": "(base) current config", "lines": []string{
			fmt.Sprintf("saved %.1f%%", base.PUHoursSavedPercent),
			fmt.Sprintf("above target %.0f min", base.TargetExceededMinutes),
		}},
	}}
	for _, c := range candidates {
		if c.Error != "" {
			continue
		}
		cs := c.Summary
		color := template.CSS("var(--context)")
		if c.Feasible {
			color = "var(--series-1)"
		}
		lines := []string{
			fmt.Sprintf("saved %.1f%% (%+.1f vs base)", cs.PUHoursSavedPercent, cs.PUHoursSavedPercent-base.PUHoursSavedPercent),
			fmt.Sprintf("above target %.0f min (%+.0f)", cs.TargetExceededMinutes, cs.TargetExceededMinutes-base.TargetExceededMinutes),
			fmt.Sprintf("gaps<10m %d (%+d), steps>2x %d", cs.ScaleGapsUnder10Min, cs.ScaleGapsUnder10Min-base.ScaleGapsUnder10Min, cs.ScaleStepViolations),
		}
		for _, r := range c.InfeasibleReasons {
			lines = append(lines, "infeasible: "+r)
		}
		dots = append(dots, dot{
			x: cs.TargetExceededMinutes, y: cs.PUHoursSavedPercent, color: color,
			info: map[string]any{"label": simulator.DescribeOverrides(c.Overrides), "lines": lines},
		})
	}

	var xMax, yMax, yMin float64
	for _, d := range dots {
		xMax = max(xMax, d.x)
		yMax = max(yMax, d.y)
		yMin = min(yMin, d.y)
	}
	xTicks := niceTicks(xMax * 1.05)
	yTicks := niceTicks(max(yMax, 1) * 1.1)
	xTop := xTicks[len(xTicks)-1]
	yTop := yTicks[len(yTicks)-1]
	yBottom := min(yMin*1.1, 0.0)

	xAt := func(v float64) float64 { return plotX0 + v/xTop*(plotX1-plotX0) }
	yAt := func(v float64) float64 {
		return scatterY1 - (v-yBottom)/(yTop-yBottom)*(scatterY1-scatterY0)
	}

	view := scatterView{
		Legend: []legendItem{
			{"feasible candidate", "var(--series-1)"},
			{"infeasible candidate", "var(--context)"},
			{"current config (base)", "var(--series-2)"},
		},
	}
	for _, t := range yTicks {
		if t > yTop {
			break
		}
		y := yAt(t)
		view.YTicks = append(view.YTicks, tickView{Pos: coord(y), LabelPos: coord(y + 4), Label: formatTick(t, "%")})
	}
	for _, t := range xTicks {
		view.XTicks = append(view.XTicks, tickView{Pos: coord(xAt(t)), Label: commaInt(int(t))})
	}
	for _, d := range dots {
		info, _ := json.Marshal(d.info)
		view.Dots = append(view.Dots, dotView{
			CX: coord(xAt(d.x)), CY: coord(yAt(d.y)), Color: d.color, Info: string(info),
		})
	}
	return view
}

// ---- helpers ----

func simHighCPU(p simulator.SimPoint) (float64, bool) {
	if p.SimHighCPU == nil {
		return 0, false
	}
	return *p.SimHighCPU, true
}

func simTotalCPU(p simulator.SimPoint) (float64, bool) {
	if p.SimTotalCPU == nil {
		return 0, false
	}
	return *p.SimTotalCPU, true
}

func hasData(bs []tsBucket) bool {
	for _, b := range bs {
		if b.Has {
			return true
		}
	}
	return false
}

func coord(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func niceTicks(maxVal float64) []float64 {
	if maxVal <= 0 {
		return []float64{0, 1}
	}
	rawStep := maxVal / 4
	mag := math.Pow(10, math.Floor(math.Log10(rawStep)))
	var step float64
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		step = m * mag
		if step >= rawStep {
			break
		}
	}
	ticks := []float64{0}
	for v := step; ; v += step {
		ticks = append(ticks, v)
		if v >= maxVal {
			break
		}
	}
	return ticks
}

func formatTick(v float64, unit string) string {
	if unit == "%" {
		if v == math.Trunc(v) {
			return fmt.Sprintf("%.0f%%", v)
		}
		return fmt.Sprintf("%.1f%%", v)
	}
	return commaInt(int(math.Round(v)))
}

func commaInt(v int) string {
	if v < 0 {
		return "-" + commaInt(-v)
	}
	s := strconv.Itoa(v)
	var out strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func xTickIndexes(n int) []int {
	if n == 0 {
		return nil
	}
	count := min(7, n)
	idx := make([]int, 0, count)
	for i := range count {
		idx = append(idx, i*(n-1)/max(count-1, 1))
	}
	return idx
}
