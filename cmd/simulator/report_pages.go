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
	"strings"

	"github.com/mercari/spanner-autoscaler/internal/simulator"
)

// writeSimulateHTML renders the full report for one simulation run: KPI row,
// the recorded-vs-simulated PU timeline (emphasis form: the simulation in the
// accent hue, the recording as gray context), the simulated CPU timeline with
// target reference lines, the min-PU assessment, and the scale-event table.
func writeSimulateHTML(w io.Writer, name string, result *simulator.Result, targetHigh, targetTotal int) {
	s := result.Summary
	writePageHead(w, "Simulation report: "+name)
	fmt.Fprintf(w, "<h1>Simulation report</h1>\n<p class=\"sub\">%s — %s .. %s (%d points)</p>\n",
		html.EscapeString(name),
		s.Start.UTC().Format("2006-01-02 15:04"), s.End.UTC().Format("2006-01-02 15:04"), s.DataPoints)

	days := max(s.End.Sub(s.Start).Hours()/24, 1)
	fmt.Fprint(w, "<div class=\"kpis\">\n")
	writeKPI(w, "PU-hours saved", fmt.Sprintf("%.1f%%", s.PUHoursSavedPercent),
		fmt.Sprintf("recorded %s → simulated %s", commaInt(int(s.ActualPUHours)), commaInt(int(s.SimPUHours))))
	writeKPI(w, "above target", fmt.Sprintf("%.0f min", s.TargetExceededMinutes),
		fmt.Sprintf("%.1f min/day", s.TargetExceededMinutes/days))
	writeKPI(w, "pinned at min PU", fmt.Sprintf("%.0f%%", s.MinPinnedPercent),
		fmt.Sprintf("workload floor p95 %s PU", commaInt(s.RequiredPUAtMinP95)))
	writeKPI(w, "scale events", fmt.Sprintf("%d ↑ / %d ↓", s.ScaleUps, s.ScaleDowns),
		fmt.Sprintf("%d steps >2x, %d gaps <10m", s.ScaleStepViolations, s.ScaleGapsUnder10Min))
	writeKPI(w, "low confidence", fmt.Sprintf("%.0f min", s.LowConfidenceMinutes),
		"recorded CPU above the model threshold")
	fmt.Fprint(w, "</div>\n")

	writeMinPUVerdict(w, "this config", s)

	times, simPU := downsample(result.Points, buckets, func(p simulator.SimPoint) (float64, bool) {
		return float64(p.SimPU), true
	})
	_, actualPU := downsample(result.Points, buckets, func(p simulator.SimPoint) (float64, bool) {
		return float64(p.ActualPU), true
	})
	writeLineChart(w, "Processing units — simulated vs recorded", "PU", times, []tsSeries{
		{Name: "simulated", Color: "var(--series-1)", Buckets: simPU},
		{Name: "recorded", Color: "var(--context)", Buckets: actualPU},
	}, nil)

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
		writeLineChart(w, "Simulated CPU utilization", "%", times, cpuSeries, refs)
	}

	fmt.Fprintf(w, "<details><summary>scale events (%d)</summary><table>\n<tr><th>time (UTC)</th><th>from</th><th>to</th><th>change</th></tr>\n", len(result.Events))
	for _, e := range result.Events {
		fmt.Fprintf(w, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%+d</td></tr>\n",
			e.Time.UTC().Format("2006-01-02 15:04"), commaInt(e.FromPU), commaInt(e.ToPU), e.ToPU-e.FromPU)
	}
	fmt.Fprint(w, "</table></details>\n")

	writePageFoot(w)
}

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

// ---- recommend report ----

const (
	scatterH  = 400
	scatterY0 = 16.0
	scatterY1 = scatterH - 34.0
)

// writeRecommendHTML renders the candidate ranking visually: a savings-vs-risk
// scatter (feasible in the accent hue, infeasible as gray context, the base
// config as the orange reference), the min-PU assessments, and the full
// candidate table with rejection reasons as the table view.
func writeRecommendHTML(w io.Writer, name string, base simulator.Summary, candidates []simulator.Candidate) {
	writePageHead(w, "Recommendation report: "+name)

	feasibleCount := 0
	for _, c := range candidates {
		if c.Feasible {
			feasibleCount++
		}
	}
	fmt.Fprintf(w, "<h1>Recommendation report</h1>\n<p class=\"sub\">%s — %d candidates (%d feasible) against %d recorded points, %s .. %s</p>\n",
		html.EscapeString(name), len(candidates), feasibleCount, base.DataPoints,
		base.Start.UTC().Format("2006-01-02"), base.End.UTC().Format("2006-01-02"))

	fmt.Fprint(w, "<div class=\"kpis\">\n")
	writeKPI(w, "base PU-hours saved", fmt.Sprintf("%.1f%%", base.PUHoursSavedPercent), "replay of the current config vs recorded")
	writeKPI(w, "base above target", fmt.Sprintf("%.0f min", base.TargetExceededMinutes), "risk reference for the deltas")
	writeKPI(w, "feasible candidates", fmt.Sprintf("%d / %d", feasibleCount, len(candidates)), "under the given constraints")
	fmt.Fprint(w, "</div>\n")

	writeScatter(w, base, candidates)

	writeMinPUVerdict(w, "base", base)
	for _, c := range candidates {
		if c.Feasible {
			writeMinPUVerdict(w, "top candidate", c.Summary)
			break
		}
	}

	fmt.Fprint(w, "<h2>All candidates</h2>\n<table>\n<tr><th>candidate</th><th>saved%</th><th>Δ saved</th><th>above target</th><th>Δ</th><th>gaps&lt;10m</th><th>steps&gt;2x</th><th>p99 hi-CPU</th><th>status</th></tr>\n")
	for _, c := range candidates {
		if c.Error != "" {
			fmt.Fprintf(w, "<tr class=\"infeasible\"><td>%s</td><td colspan=\"7\"></td><td>error: %s</td></tr>\n",
				html.EscapeString(simulator.DescribeOverrides(c.Overrides)), html.EscapeString(c.Error))
			continue
		}
		cs := c.Summary
		status := "feasible"
		class := ""
		if !c.Feasible {
			status = "infeasible: " + strings.Join(c.InfeasibleReasons, "; ")
			class = " class=\"infeasible\""
		}
		fmt.Fprintf(w, "<tr%s><td>%s</td><td>%.1f%%</td><td>%+.1f</td><td>%.0f min</td><td>%+.0f</td><td>%d</td><td>%d</td><td>%s</td><td class=\"reason\">%s</td></tr>\n",
			class, html.EscapeString(simulator.DescribeOverrides(c.Overrides)),
			cs.PUHoursSavedPercent, cs.PUHoursSavedPercent-base.PUHoursSavedPercent,
			cs.TargetExceededMinutes, cs.TargetExceededMinutes-base.TargetExceededMinutes,
			cs.ScaleGapsUnder10Min, cs.ScaleStepViolations,
			formatP99(cs.SimHighPriorityCPU), html.EscapeString(status))
	}
	fmt.Fprint(w, "</table>\n")

	writePageFoot(w)
}

func writeScatter(w io.Writer, base simulator.Summary, candidates []simulator.Candidate) {
	type dot struct {
		x, y  float64
		color string
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
		color := "var(--context)"
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

	fmt.Fprint(w, "<h2>Savings vs risk</h2>\n<figure class=\"chart\" data-kind=\"scatter\">\n")
	fmt.Fprint(w, "<div class=\"legend\">")
	for _, l := range []struct{ name, color string }{
		{"feasible candidate", "var(--series-1)"},
		{"infeasible candidate", "var(--context)"},
		{"current config (base)", "var(--series-2)"},
	} {
		fmt.Fprintf(w, "<span><span class=\"key\" style=\"border-top-color:%s\"></span>%s</span>", l.color, l.name)
	}
	fmt.Fprint(w, "</div>\n")

	fmt.Fprintf(w, "<svg viewBox=\"0 0 %d %d\" role=\"img\" aria-label=\"savings versus risk scatter\">\n", chartW, scatterH)
	for _, t := range yTicks {
		if t > yTop {
			break
		}
		y := yAt(t)
		fmt.Fprintf(w, "<line class=\"gridline\" x1=\"%.1f\" y1=\"%.1f\" x2=\"%.1f\" y2=\"%.1f\"/>\n", plotX0, y, plotX1, y)
		fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"end\">%s</text>\n", plotX0-6, y+4, formatTick(t, "%"))
	}
	for _, t := range xTicks {
		x := xAt(t)
		fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"middle\">%s</text>\n", x, scatterY1+18, commaInt(int(t)))
	}
	fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\" text-anchor=\"end\">minutes above target (risk) →</text>\n", plotX1, scatterY1+32)
	fmt.Fprintf(w, "<text x=\"%.1f\" y=\"%.1f\">PU-hours saved ↑</text>\n", plotX0, scatterY0-2)

	for _, d := range dots {
		info, _ := json.Marshal(d.info)
		fmt.Fprintf(w, "<g class=\"dot\" tabindex=\"0\" data-info=\"%s\">", html.EscapeString(string(info)))
		fmt.Fprintf(w, "<circle cx=\"%.1f\" cy=\"%.1f\" r=\"14\" fill=\"transparent\"/>", xAt(d.x), yAt(d.y))
		fmt.Fprintf(w, "<circle cx=\"%.1f\" cy=\"%.1f\" r=\"5\" fill=\"%s\" stroke=\"var(--surface-1)\" stroke-width=\"2\"/>", xAt(d.x), yAt(d.y), d.color)
		fmt.Fprint(w, "</g>\n")
	}
	fmt.Fprint(w, "</svg>\n</figure>\n")
}
