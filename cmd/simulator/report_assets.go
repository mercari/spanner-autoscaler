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

import "html/template"

// reportCSS defines the palette slots as CSS custom properties for both
// modes; the chart markup only ever references roles (var(--series-1), …).
const reportCSS template.CSS = `
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
.conclusion { border-left: 4px solid var(--series-1); background: var(--surface-2); border-radius: 8px; padding: 12px 16px; margin: 14px 0; }
.conclusion table { margin: 8px 0 6px; }
.conclusion td.changed { font-weight: 600; }
.conclusion .effect { color: var(--text-secondary); }
.infeasible { color: var(--text-secondary); }
.reason { color: var(--text-secondary); font-size: 12px; }
`

// reportJS drives the crosshair tooltip on line charts and the per-mark
// tooltip on scatter dots. All data-derived strings are inserted with
// textContent only.
const reportJS template.JS = `
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
