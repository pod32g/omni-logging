"use strict";

// ---------- small helpers ----------
const $ = (sel) => document.querySelector(sel);
const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};
const SVGNS = "http://www.w3.org/2000/svg";
function svgPath(cls, d) {
  const svg = document.createElementNS(SVGNS, "svg");
  svg.setAttribute("class", cls);
  svg.setAttribute("viewBox", "0 0 24 24");
  const path = document.createElementNS(SVGNS, "path");
  path.setAttribute("d", d);
  svg.appendChild(path);
  return svg;
}

const LEVELS = ["error", "warn", "info", "debug", "fatal"];
const LEVEL_COLOR = {
  error: "#DC2626", warn: "#D97706", info: "#2563EB", debug: "#9AA3B2", fatal: "#7C2D12",
};

function token() { return localStorage.getItem("omnilog_token") || ""; }
function setToken(t) { localStorage.setItem("omnilog_token", t); }

// Fetch JSON from the API, attaching the admin token and surfacing 401s.
async function api(path) {
  const headers = {};
  const t = token();
  if (t) headers["Authorization"] = "Bearer " + t;
  const res = await fetch(path, { headers });
  if (res.status === 401) {
    $("#token-bar").classList.add("show");
    throw new Error("unauthorized");
  }
  if (!res.ok) throw new Error("request failed: " + res.status);
  return res.json();
}

function fmtTs(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ` +
         `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}
function fmtNum(n) { return (n || 0).toLocaleString("en-US"); }

// ---------- view switching ----------
const views = { search: $("#view-search"), tail: $("#view-tail") };
document.querySelectorAll(".nav-item").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll(".nav-item").forEach((b) => b.classList.remove("is-active"));
    btn.classList.add("is-active");
    const v = btn.dataset.view;
    views.search.hidden = v !== "search";
    views.tail.hidden = v !== "tail";
    if (v === "tail") startTail(); else stopTail();
  });
});

// ---------- token bar ----------
$("#token-save").addEventListener("click", () => {
  setToken($("#token-input").value.trim());
  $("#token-bar").classList.remove("show");
  runSearch();
});
$("#token-btn").addEventListener("click", () => $("#token-bar").classList.toggle("show"));

// ---------- SEARCH ----------
const rowsEl = $("#rows");

function buildSearchURL(base) {
  const q = $("#q").value.trim();
  const range = $("#range").value;
  const order = $("#order-chip").dataset.order;
  const p = new URLSearchParams();
  if (q) p.set("q", q);
  if (range) p.set("last", range);
  p.set("order", order);
  return base + "?" + p.toString();
}

async function runSearch() {
  try {
    const [res, stats] = await Promise.all([
      api(buildSearchURL("/api/v1/search") + "&limit=200"),
      api(buildSearchURL("/api/v1/search/stats") + "&interval=" + bucketFor($("#range").value)),
    ]);
    renderResults(res);
    renderStats(stats);
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

// Pick a histogram bucket width appropriate to the selected range.
function bucketFor(range) {
  switch (range) {
    case "15m": return "30s";
    case "1h": return "1m";
    case "6h": return "5m";
    case "24h": return "30m";
    case "168h": return "6h";
    default: return "1h";
  }
}

function renderResults(res) {
  rowsEl.replaceChildren();
  $("#match-count").textContent = fmtNum(res.total) + " matching events";
  $("#match-sub").textContent = res.count < res.total ? `showing ${fmtNum(res.count)}` : "";
  $("#search-empty").hidden = (res.events && res.events.length > 0);
  (res.events || []).forEach((e) => rowsEl.appendChild(renderRow(e)));
}

function renderRow(e) {
  const lvl = (e.level || "info").toLowerCase();
  const row = el("div", `row lvl-${lvl}`);

  const line = el("div", "row-line");
  line.appendChild(el("span", "row-ts", fmtTs(e.timestamp)));
  const lvlCell = el("span", "row-level");
  lvlCell.appendChild(el("span", `badge ${lvl}`, lvl));
  line.appendChild(lvlCell);
  line.appendChild(el("span", "row-svc", e.service || "—"));
  line.appendChild(el("span", "row-msg", e.message || e.raw || ""));
  line.appendChild(svgPath("chev", "M18 15l-6-6-6 6"));
  row.appendChild(line);

  const detail = el("div", "row-detail");
  const chips = el("div", "attr-chips");
  const attrs = e.attributes || {};
  const meta = { source: e.source, ...attrs };
  Object.keys(meta).forEach((k) => {
    if (meta[k] == null || meta[k] === "") return;
    const chip = el("span", "attr-chip");
    chip.appendChild(el("b", null, k + "="));
    chip.appendChild(document.createTextNode(String(meta[k])));
    chips.appendChild(chip);
  });
  if (chips.children.length) detail.appendChild(chips);
  const jb = el("div", "json-block");
  jb.appendChild(el("pre", null, JSON.stringify(e, null, 2)));
  detail.appendChild(jb);
  row.appendChild(detail);

  line.addEventListener("click", () => row.classList.toggle("open"));
  return row;
}

function renderStats(stats) {
  $("#hist-count").textContent = fmtNum(stats.total);
  $("#hist-took").textContent = `events · ${(stats.took_ms || 0)}ms`;
  renderBars(stats.histogram || []);
  renderFacets(stats.facets || {});
  const h = stats.histogram || [];
  $("#hist-sub").textContent = h.length
    ? `${fmtTs(h[0].start)} – ${fmtTs(h[h.length - 1].start)}`
    : "no data in range";
}

// fillBuckets inserts zero-count buckets into gaps so the histogram renders as
// contiguous bars rather than a few wide blocks when data is sparse.
function fillBuckets(hist) {
  if (hist.length < 2) return hist;
  const starts = hist.map((b) => new Date(b.start).getTime());
  let step = Infinity;
  for (let i = 1; i < starts.length; i++) step = Math.min(step, starts[i] - starts[i - 1]);
  if (!isFinite(step) || step <= 0) return hist;
  const counts = new Map(hist.map((b) => [new Date(b.start).getTime(), b.count]));
  const out = [];
  const end = starts[starts.length - 1];
  for (let t = starts[0]; t <= end && out.length < 1000; t += step) {
    out.push({ start: new Date(t).toISOString(), count: counts.get(t) || 0 });
  }
  return out;
}

function renderBars(rawHist) {
  const hist = fillBuckets(rawHist);
  const bars = $("#bars");
  bars.replaceChildren();
  const max = Math.max(1, ...hist.map((b) => b.count));
  hist.forEach((b) => {
    const bar = el("div", "bar");
    const norm = el("div", "norm");
    norm.style.height = Math.round((b.count / max) * 92) + "px";
    bar.title = `${fmtTs(b.start)} · ${fmtNum(b.count)} events`;
    bar.appendChild(norm);
    bars.appendChild(bar);
  });
}

function renderFacets(facets) {
  const levelsEl = $("#facet-levels");
  levelsEl.replaceChildren();
  const levelMap = {};
  (facets.level || []).forEach((f) => (levelMap[f.value] = f.count));
  const maxLevel = Math.max(1, ...Object.values(levelMap));
  LEVELS.forEach((lvl) => {
    if (levelMap[lvl] == null) return;
    levelsEl.appendChild(facetRow(lvl, levelMap[lvl], maxLevel, LEVEL_COLOR[lvl], false, "level=" + lvl));
  });

  const svcEl = $("#facet-services");
  svcEl.replaceChildren();
  const svc = facets.service || [];
  const maxSvc = Math.max(1, ...svc.map((f) => f.count));
  svc.slice(0, 8).forEach((f) => {
    if (!f.value) return;
    svcEl.appendChild(facetRow(f.value, f.count, maxSvc, null, true, "service=" + f.value));
  });
}

function facetRow(name, count, max, color, mono, queryFrag) {
  const f = el("div", "facet");
  const top = el("div", "facet-top");
  if (color) {
    const sw = el("span", "facet-swatch");
    sw.style.background = color;
    top.appendChild(sw);
  }
  top.appendChild(el("span", "facet-name" + (mono ? " mono" : ""), name));
  top.appendChild(el("span", "facet-count", fmtNum(count)));
  f.appendChild(top);
  const bar = el("div", "facet-bar");
  const fill = el("i");
  fill.style.width = Math.round((count / max) * 100) + "%";
  fill.style.background = color || "#2348E0";
  bar.appendChild(fill);
  f.appendChild(bar);
  f.addEventListener("click", () => {
    const q = $("#q");
    if (!q.value.includes(queryFrag)) q.value = (q.value + " " + queryFrag).trim();
    runSearch();
  });
  return f;
}

$("#search-form").addEventListener("submit", (e) => { e.preventDefault(); runSearch(); });
$("#search-btn").addEventListener("click", runSearch);
$("#range").addEventListener("change", runSearch);
$("#order-chip").addEventListener("click", () => {
  const c = $("#order-chip");
  const next = c.dataset.order === "newest" ? "oldest" : "newest";
  c.dataset.order = next;
  c.querySelector("span").textContent = next === "newest" ? "Newest first" : "Oldest first";
  runSearch();
});

// ---------- LIVE TAIL ----------
let es = null, paused = false, streamed = 0, epsWindow = [];
const streamRows = $("#stream-rows");

function startTail() {
  if (paused) return;
  stopTail();
  const q = $("#tail-q").value.trim();
  const p = new URLSearchParams();
  if (q) p.set("q", q);
  if (token()) p.set("token", token());
  es = new EventSource("/api/v1/tail?" + p.toString());
  es.onmessage = (msg) => {
    let e; try { e = JSON.parse(msg.data); } catch { return; }
    addStreamRow(e);
  };
  es.onerror = () => { /* browser auto-reconnects */ };
}
function stopTail() { if (es) { es.close(); es = null; } }

function addStreamRow(e) {
  $("#tail-empty").hidden = true;
  streamed++;
  $("#streamed").textContent = fmtNum(streamed);
  epsWindow.push(Date.now());

  const row = renderRow(e);
  row.classList.add("fresh");
  setTimeout(() => row.classList.remove("fresh"), 1200);
  streamRows.insertBefore(row, streamRows.firstChild);
  while (streamRows.children.length > 500) streamRows.removeChild(streamRows.lastChild);
  if ($("#autoscroll").checked) streamRows.scrollTop = 0;
}

setInterval(() => {
  const cutoff = Date.now() - 1000;
  epsWindow = epsWindow.filter((t) => t >= cutoff);
  $("#eps").textContent = fmtNum(epsWindow.length);
}, 1000);

$("#tail-pause").addEventListener("click", () => {
  paused = !paused;
  const toggle = $("#tail-toggle");
  $("#tail-pause").querySelector("span").textContent = paused ? "Resume" : "Pause";
  toggle.classList.toggle("paused", paused);
  toggle.querySelector("span").textContent = paused ? "PAUSED" : "LIVE";
  if (paused) stopTail(); else startTail();
});
$("#tail-q").addEventListener("keydown", (e) => { if (e.key === "Enter") startTail(); });

// ---------- boot ----------
runSearch();
