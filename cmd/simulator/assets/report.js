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
