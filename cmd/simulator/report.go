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
	"html"
	"io"
	"math"
	"strings"
	"time"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

// The report is a single self-contained HTML file: inline CSS/SVG/JS, no
// external resources, light and dark mode from the same validated palette
// (categorical blue/orange pass the palette validator in both modes; the
// recorded series uses the de-emphasis gray of the emphasis form, with
// identity carried by the legend and tooltip, not color alone).

const reportCSS = `
:root {
  color-scheme: light;
  --surface-1: #fcfcfb; --surface-2: #f3f2ef;
  --text-primary: #0b0b0b; --text-secondary: #52514e;
  --grid: #e7e6e2; --ref: #a5a49d;
  --series-1: #2a78d6; --series-2: #eb6834; --context: #8f8e88;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    color-scheme: dark;
    --surface-1: #1a1a19; --surface-2: #232322;
    --text-primary: #ffffff; --text-secondary: #c3c2b7;
    --grid: #2c2c2a; --ref: #6b6a64;
    --series-1: #3987e5; --series-2: #d95926; --context: #8a8a84;
  }
}
:root[data-theme="dark"] {
  color-scheme: dark;
  --surface-1: #1a1a19; --surface-2: #232322;
  --text-primary: #ffffff; --text-secondary: #c3c2b7;
  --grid: #2c2c2a; --ref: #6b6a64;
  --series-1: #3987e5; --series-2: #d95926; --context: #8a8a84;
}
* { box-sizing: border-box; }
body { margin: 0 auto; padding: 24px 20px 48px; max-width: 1040px;
  background: var(--surface-1); color: var(--text-primary);
  font: 14px/1.5 -apple-system, "Segoe UI", Roboto, "Noto Sans", sans-serif; }
h1 { font-size: 20px; margin: 0 0 4px; }
h2 { font-size: 15px; margin: 28px 0 8px; }
.sub { color: var(--text-secondary); margin: 0 0 16px; }
.kpis { display: flex; flex-wrap: wrap; gap: 12px; margin: 16px 0; }
.kpi { background: var(--surface-2); border-radius: 8px; padding: 10px 14px; min-width: 150px; }
.kpi .label { color: var(--text-secondary); font-size: 12px; }
.kpi .value { font-size: 22px; font-weight: 600; }
.kpi .note { color: var(--text-secondary); font-size: 12px; }
figure.chart { margin: 8px 0 4px; position: relative; }
.legend { display: flex; gap: 16px; margin: 0 0 4px; color: var(--text-secondary); font-size: 12px; }
.legend .key { display: inline-block; width: 14px; height: 0; border-top: 3px solid; vertical-align: middle; margin-right: 5px; border-radius: 2px; }
svg { max-width: 100%; height: auto; display: block; }
svg text { fill: var(--text-secondary); font-size: 11px; }
.gridline { stroke: var(--grid); stroke-width: 1; }
.refline { stroke: var(--ref); stroke-width: 1; }
.crosshair { stroke: var(--ref); stroke-width: 1; visibility: hidden; }
.tooltip { position: absolute; pointer-events: none; visibility: hidden;
  background: var(--surface-2); color: var(--text-primary); border-radius: 6px;
  padding: 6px 10px; font-size: 12px; box-shadow: 0 2px 8px rgba(0,0,0,.25); max-width: 340px; z-index: 2; }
.tooltip .t { color: var(--text-secondary); margin-bottom: 2px; }
.tooltip .row { display: flex; align-items: baseline; gap: 6px; }
.tooltip .key { display: inline-block; width: 12px; border-top: 3px solid; border-radius: 2px; flex: none; }
.tooltip .v { font-weight: 600; }
.tooltip .n { color: var(--text-secondary); }
table { border-collapse: collapse; font-size: 13px; }
th, td { text-align: right; padding: 3px 10px; border-bottom: 1px solid var(--grid); }
th:first-child, td:first-child { text-align: left; }
details { margin: 12px 0; }
summary { cursor: pointer; color: var(--text-secondary); }
.verdict { background: var(--surface-2); border-radius: 8px; padding: 10px 14px; margin: 8px 0; }
.verdict b { font-weight: 600; }
.infeasible { color: var(--text-secondary); }
.reason { color: var(--text-secondary); font-size: 12px; }
`

// reportJS drives the crosshair tooltip on line charts and the per-mark
// tooltip on scatter dots. All user-derived strings are inserted with
// textContent only.
const reportJS = `
function fmtVal(v, unit) {
  const s = unit === "%" ? v.toFixed(1) : Math.round(v).toLocaleString("en-US");
  return s + (unit === "%" ? "%" : "");
}
function makeTooltip(fig) {
  const tip = document.createElement("div");
  tip.className = "tooltip";
  fig.appendChild(tip);
  return tip;
}
document.querySelectorAll("figure.chart[data-kind=line]").forEach(fig => {
  const data = JSON.parse(fig.querySelector("script[type='application/json']").textContent);
  const svg = fig.querySelector("svg");
  const plot = data.plot;
  const tip = makeTooltip(fig);
  const cross = svg.querySelector(".crosshair");
  function hide() { tip.style.visibility = "hidden"; cross.style.visibility = "hidden"; }
  svg.addEventListener("pointerleave", hide);
  svg.addEventListener("pointermove", e => {
    const rect = svg.getBoundingClientRect();
    const sx = (e.clientX - rect.left) * (plot.w / rect.width);
    if (sx < plot.x0 || sx > plot.x1 || data.t.length === 0) { hide(); return; }
    const frac = (sx - plot.x0) / (plot.x1 - plot.x0);
    const i = Math.max(0, Math.min(data.t.length - 1, Math.round(frac * (data.t.length - 1))));
    const px = plot.x0 + (data.t.length === 1 ? 0 : i / (data.t.length - 1) * (plot.x1 - plot.x0));
    cross.setAttribute("x1", px); cross.setAttribute("x2", px);
    cross.style.visibility = "visible";
    tip.replaceChildren();
    const t = document.createElement("div"); t.className = "t";
    t.textContent = new Date(data.t[i]).toISOString().slice(0, 16).replace("T", " ") + " UTC";
    tip.appendChild(t);
    data.series.forEach(s => {
      if (s.min[i] === null) return;
      const row = document.createElement("div"); row.className = "row";
      const key = document.createElement("span"); key.className = "key"; key.style.borderTopColor = s.color;
      const v = document.createElement("span"); v.className = "v";
      v.textContent = Math.abs(s.max[i] - s.min[i]) > (data.unit === "%" ? 0.05 : 0.5)
        ? fmtVal(s.min[i], data.unit) + "–" + fmtVal(s.max[i], data.unit)
        : fmtVal(s.mean[i], data.unit);
      const n = document.createElement("span"); n.className = "n"; n.textContent = s.name;
      row.append(key, v, n); tip.appendChild(row);
    });
    tip.style.visibility = "visible";
    const fr = fig.getBoundingClientRect();
    let left = e.clientX - fr.left + 14;
    if (left + tip.offsetWidth > fr.width) left = e.clientX - fr.left - tip.offsetWidth - 14;
    tip.style.left = Math.max(0, left) + "px";
    tip.style.top = (e.clientY - fr.top + 12) + "px";
  });
});
document.querySelectorAll("figure.chart[data-kind=scatter]").forEach(fig => {
  const tip = makeTooltip(fig);
  function show(g, cx, cy) {
    const info = JSON.parse(g.dataset.info);
    tip.replaceChildren();
    const t = document.createElement("div"); t.className = "t"; t.textContent = info.label;
    tip.appendChild(t);
    info.lines.forEach(l => {
      const row = document.createElement("div"); row.textContent = l;
      tip.appendChild(row);
    });
    tip.style.visibility = "visible";
    const fr = fig.getBoundingClientRect();
    let left = cx - fr.left + 14;
    if (left + tip.offsetWidth > fr.width) left = cx - fr.left - tip.offsetWidth - 14;
    tip.style.left = Math.max(0, left) + "px";
    tip.style.top = (cy - fr.top + 12) + "px";
  }
  fig.querySelectorAll("g.dot").forEach(g => {
    g.addEventListener("pointerenter", e => show(g, e.clientX, e.clientY));
    g.addEventListener("pointerleave", () => { tip.style.visibility = "hidden"; });
    g.addEventListener("focus", () => {
      const r = g.getBoundingClientRect();
      show(g, r.left + r.width / 2, r.top + r.height / 2);
    });
    g.addEventListener("blur", () => { tip.style.visibility = "hidden"; });
  });
});
`

func writePageHead(w io.Writer, title string) {
	fmt.Fprintf(w, "<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n<title>%s</title>\n<style>%s</style>\n</head>\n<body>\n", html.EscapeString(title), reportCSS)
}

func writePageFoot(w io.Writer) {
	fmt.Fprintf(w, "<script>%s</script>\n</body>\n</html>\n", reportJS)
}

// ---- time-series chart ----

// tsBucket is one downsampled slot of a time series. Min/max keep the 1-minute
// spikes honest at any zoom; the mean line carries the trend.
type tsBucket struct {
	Min, Max, Mean float64
	Has            bool
}

type tsSeries struct {
	Name    string
	Color   string // resolved CSS variable, e.g. var(--series-1)
	Buckets []tsBucket
}

type refLine struct {
	Y     float64
	Label string
}

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
	buckets := make([]tsBucket, n)
	counts := make([]int, n)
	for _, p := range points {
		v, ok := value(p)
		if !ok {
			continue
		}
		i := min(int((p.Time.UnixMilli()-start)*int64(n)/span), n-1)
		b := &buckets[i]
		if !b.Has {
			*b = tsBucket{Min: v, Max: v, Has: true}
		} else {
			b.Min = min(b.Min, v)
			b.Max = max(b.Max, v)
		}
		b.Mean += v
		counts[i]++
	}
	for i := range buckets {
		if counts[i] > 0 {
			buckets[i].Mean /= float64(counts[i])
		}
	}
	return times, buckets
}

const (
	chartW  = 960
	chartH  = 280
	plotX0  = 68.0
	plotX1  = chartW - 20.0
	plotY0  = 14.0
	plotY1  = chartH - 30.0
	buckets = 440
)

// writeLineChart renders one figure: legend, SVG (grid, axes, per-series
// min-max band at 10% opacity plus a 2px mean line), reference lines, and the
// embedded JSON that powers the crosshair tooltip.
func writeLineChart(w io.Writer, title, unit string, times []int64, series []tsSeries, refs []refLine) {
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

	fmt.Fprintf(w, "<h2>%s</h2>\n<figure class=\"chart\" data-kind=\"line\">\n", html.EscapeString(title))

	fmt.Fprint(w, "<div class=\"legend\">")
	for _, s := range series {
		fmt.Fprintf(w, "<span><span class=\"key\" style=\"border-top-color:%s\"></span>%s</span>", s.Color, html.EscapeString(s.Name))
	}
	fmt.Fprint(w, "</div>\n")

	fmt.Fprintf(w, "<svg viewBox=\"0 0 %d %d\" role=\"img\" aria-label=\"%s\">\n", chartW, chartH, html.EscapeString(title))

	// Horizontal gridlines + y tick labels.
	for _, t := range ticks {
		y := yAt(t)
		fmt.Fprintf(w, "<line class=\"gridline\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\"/>\n", plotX0, y, plotX1, y)
		fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"end\">%s</text>\n", plotX0-6, y+4, formatTick(t, unit))
	}
	// X tick labels.
	for _, i := range xTickIndexes(len(times)) {
		fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"middle\">%s</text>\n",
			xAt(i), plotY1+18, time.UnixMilli(times[i]).UTC().Format("01-02"))
	}
	fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"end\">UTC</text>\n", plotX1, plotY1+18)

	// Reference lines with direct labels.
	for _, r := range refs {
		y := yAt(r.Y)
		fmt.Fprintf(w, "<line class=\"refline\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\"/>\n", plotX0, y, plotX1, y)
		fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"end\">%s</text>\n", plotX1, y-4, html.EscapeString(r.Label))
	}

	for _, s := range series {
		band, line := seriesPaths(s.Buckets, xAt, yAt)
		if band != "" {
			fmt.Fprintf(w, "<path d=\"%s\" fill=\"%s\" fill-opacity=\"0.1\" stroke=\"none\"/>\n", band, s.Color)
		}
		if line != "" {
			fmt.Fprintf(w, "<path d=\"%s\" fill=\"none\" stroke=\"%s\" stroke-width=\"2\" stroke-linejoin=\"round\" stroke-linecap=\"round\"/>\n", line, s.Color)
		}
	}

	fmt.Fprintf(w, "<line class=\"crosshair\" x1=\"0\" y1=\"%.1f\" x2=\"0\" y2=\"%.1f\"/>\n", plotY0, plotY1)
	fmt.Fprint(w, "</svg>\n")

	writeChartJSON(w, unit, times, series)
	fmt.Fprint(w, "</figure>\n")

	writeBucketTable(w, unit, times, series)
}

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

func writeChartJSON(w io.Writer, unit string, times []int64, series []tsSeries) {
	type jsSeries struct {
		Name  string     `json:"name"`
		Color string     `json:"color"`
		Min   []*float64 `json:"min"`
		Max   []*float64 `json:"max"`
		Mean  []*float64 `json:"mean"`
	}
	payload := struct {
		T      []int64            `json:"t"`
		Unit   string             `json:"unit"`
		Series []jsSeries         `json:"series"`
		Plot   map[string]float64 `json:"plot"`
	}{
		T:    times,
		Unit: unit,
		Plot: map[string]float64{"x0": plotX0, "x1": plotX1, "w": chartW},
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
	// </script> cannot appear in the marshaled output: JSON strings here are
	// only series names we control, but escape defensively anyway.
	fmt.Fprintf(w, "<script type=\"application/json\">%s</script>\n",
		strings.ReplaceAll(string(buf), "</", "<\\/"))
}

// writeBucketTable is the table view backing the chart: the same downsampled
// buckets, so every plotted value is reachable without hover.
func writeBucketTable(w io.Writer, unit string, times []int64, series []tsSeries) {
	fmt.Fprint(w, "<details><summary>table view (downsampled buckets)</summary><table>\n<tr><th>time (UTC)</th>")
	for _, s := range series {
		fmt.Fprintf(w, "<th>%s (min)</th><th>%s (max)</th>", html.EscapeString(s.Name), html.EscapeString(s.Name))
	}
	fmt.Fprint(w, "</tr>\n")
	for i, t := range times {
		fmt.Fprintf(w, "<tr><td>%s</td>", time.UnixMilli(t).UTC().Format("2006-01-02 15:04"))
		for _, s := range series {
			b := s.Buckets[i]
			if !b.Has {
				fmt.Fprint(w, "<td></td><td></td>")
				continue
			}
			fmt.Fprintf(w, "<td>%s</td><td>%s</td>", formatTick(b.Min, unit), formatTick(b.Max, unit))
		}
		fmt.Fprint(w, "</tr>\n")
	}
	fmt.Fprint(w, "</table></details>\n")
}

// ---- helpers ----

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
	s := fmt.Sprintf("%d", v)
	if v < 0 {
		return "-" + commaInt(-v)
	}
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

func writeKPI(w io.Writer, label, value, note string) {
	fmt.Fprintf(w, "<div class=\"kpi\"><div class=\"label\">%s</div><div class=\"value\">%s</div><div class=\"note\">%s</div></div>\n",
		html.EscapeString(label), html.EscapeString(value), html.EscapeString(note))
}

func writeMinPUVerdict(w io.Writer, label string, s simulator.Summary) {
	a := s.AssessMinPU()
	fmt.Fprintf(w, "<div class=\"verdict\"><b>min PU assessment (%s)</b> — min %s, pinned %.0f%% of the run",
		html.EscapeString(label), commaInt(s.SpecMinPU), s.MinPinnedPercent)
	if s.RequiredPUAtMinP95 > 0 {
		fmt.Fprintf(w, ", workload floor while pinned (p95) = %s PU", commaInt(s.RequiredPUAtMinP95))
	}
	fmt.Fprintf(w, "<br>lower? %s<br>raise? %s</div>\n", html.EscapeString(a.Lower), html.EscapeString(a.Raise))
}
