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

const LEVELS = ["fatal", "error", "warn", "info", "debug"];
// Read from the stylesheet so the palette lives in exactly one place. Severity
// is the only thing in this UI that carries colour.
function levelColor(lvl) {
  return getComputedStyle(document.documentElement).getPropertyValue("--" + lvl).trim() || "#8A8A8A";
}

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

// apiSend performs a non-GET request (returns the raw Response so callers can
// read validation error bodies).
async function apiSend(method, path, body) {
  const headers = { "Content-Type": "application/json" };
  const t = token();
  if (t) headers["Authorization"] = "Bearer " + t;
  const res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  if (res.status === 401) {
    $("#token-bar").classList.add("show");
    throw new Error("unauthorized");
  }
  return res;
}

function fmtTs(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ` +
         `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}
function fmtNum(n) { return (n || 0).toLocaleString("en-US"); }

// Rows show a time only. Repeating the date on all 200 of them costs a column
// and tells you nothing — it moves to a separator whenever the day changes.
function fmtTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (n, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}
function fmtClock(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (n) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}`;
}
function dayKey(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return "";
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}
function dayLabel(iso) {
  const key = dayKey(iso);
  const today = dayKey(new Date().toISOString());
  const yest = dayKey(new Date(Date.now() - 86400000).toISOString());
  if (key === today) return "Today · " + key;
  if (key === yest) return "Yesterday · " + key;
  return key;
}

// ---------- view switching ----------
const views = {
  dash: $("#view-dash"), search: $("#view-search"), tail: $("#view-tail"),
  alerts: $("#view-alerts"), settings: $("#view-settings"),
};
let searchLoaded = false;

function navTo(v) {
  if (!views[v]) return;
  document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("is-active", b.dataset.view === v));
  Object.entries(views).forEach(([name, elm]) => { elm.hidden = name !== v; });
  if (v === "tail") startTail(); else stopTail();
  if (v === "settings") loadSettings();
  if (v === "alerts") loadAlerts();
  if (v === "dash") loadDash();
  if (v === "search" && !searchLoaded) { searchLoaded = true; runSearch(); }
}
document.querySelectorAll(".nav-item").forEach((btn) => {
  btn.addEventListener("click", () => navTo(btn.dataset.view));
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
let searchBase = "";   // the /api/v1/search URL of the current query (no limit/after)
let searchCursor = ""; // keyset cursor for the next page, "" when exhausted

function buildSearchURL(base) {
  const q = $("#q").value.trim();
  syncRangeOverride();
  const range = $("#range").value;
  const order = $("#order-chip").dataset.order;
  const p = new URLSearchParams();
  if (q) p.set("q", q);
  if (range) p.set("last", range);
  p.set("order", order);
  return base + "?" + p.toString();
}

// hasTimeDirective reports whether the query sets its own time range, in which
// case the server ignores the range picker (the expression is the more specific
// statement). The leading (^|\s) is what keeps attr.last= out of it — there the
// key is preceded by a dot, not a boundary.
//
// Advisory only: this drives a label, never the request. The server decides.
function hasTimeDirective(q) {
  return /(^|\s)(last|from|to)=/i.test(q);
}

// syncRangeOverride marks the range picker as overridden, so a query saying
// last=24h while the picker reads "Last 1 hour" does not look like a bug.
function syncRangeOverride() {
  const overridden = hasTimeDirective($("#q").value);
  $("#range-override").hidden = !overridden;
  $("#range-select").classList.toggle("is-overridden", overridden);
  $("#range-select").title = overridden
    ? "The query sets its own time range, so this picker is ignored."
    : "";
}

// hasPipeline reports whether the query carries an aggregation stage. A '|'
// inside quotes belongs to a value, so it is skipped — the same rule the server
// applies when it splits the expression.
function hasPipeline(q) {
  let inQuote = false;
  for (const ch of q) {
    if (ch === '"') inQuote = !inQuote;
    else if (ch === "|" && !inQuote) return true;
  }
  return false;
}

async function runSearch() {
  const expr = $("#q").value.trim();
  if (hasPipeline(expr)) return runAggregation();

  showAggregation(false);
  try {
    searchBase = buildSearchURL("/api/v1/search");
    const [res, stats] = await Promise.all([
      api(searchBase + "&limit=200"),
      api(buildSearchURL("/api/v1/search/stats") + "&interval=" + bucketFor($("#range").value)),
    ]);
    renderResults(res, false);
    renderStats(stats);
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

// showAggregation swaps the results pane between the event list and the table.
// The event-list chrome goes with it: an aggregation has no timestamp/level
// columns, no sort order, and its rows are not the events the export buttons
// would download — leaving them visible would promise something untrue.
function showAggregation(on) {
  $("#agg-wrap").hidden = !on;
  $("#agg-note").hidden = true;
  rowsEl.hidden = on;
  $("#col-header").hidden = on;
  $("#export-ndjson").hidden = on;
  $("#export-csv").hidden = on;
  $("#order-chip").hidden = on;
  $("#load-more").hidden = on || !searchCursor;
  if (on) $("#search-empty").hidden = true;
}

async function runAggregation() {
  showAggregation(true);
  try {
    // The histogram still describes the filter half, so it stays useful.
    const [res, stats] = await Promise.all([
      api(buildSearchURL("/api/v1/aggregate")),
      api(buildSearchURL("/api/v1/search/stats") + "&interval=" + bucketFor($("#range").value)),
    ]);
    renderAggregation(res);
    renderStats(stats);
  } catch (e) {
    if (e.message !== "unauthorized") {
      console.error(e);
      renderAggError(e.message);
    }
  }
}

function renderAggError(msg) {
  $("#agg-table").replaceChildren();
  const note = $("#agg-note");
  note.textContent = msg;
  note.hidden = false;
  $("#match-count").textContent = "query error";
  $("#match-sub").textContent = "";
}

// fmtCell renders one aggregation value: timestamps as local time, numbers
// grouped, everything else as text.
function fmtCell(v) {
  if (v == null || v === "") return "—";
  if (typeof v === "number") return Number.isInteger(v) ? fmtNum(v) : v.toFixed(2);
  if (typeof v === "string" && /^\d{4}-\d{2}-\d{2}T/.test(v)) {
    const d = new Date(v);
    if (!isNaN(d)) return fmtTs(v);
  }
  return String(v);
}

function renderAggregation(res) {
  const cols = res.columns || [];
  const rows = res.rows || [];
  const table = $("#agg-table");
  table.replaceChildren();

  const head = el("tr");
  cols.forEach((c) => head.appendChild(el("th", null, c)));
  table.appendChild(head);

  // The server states where the measures start, so a null measure cannot be
  // mistaken for a group label.
  const firstMeasure = res.group_columns || 0;
  let max = 0;
  rows.forEach((r) => {
    const v = r[firstMeasure];
    if (typeof v === "number" && v > max) max = v;
  });

  rows.forEach((r) => {
    const tr = el("tr");
    r.forEach((v, i) => {
      const isNum = typeof v === "number";
      const td = el("td", isNum ? "num" : "label");
      if (isNum && i === firstMeasure && max > 0) {
        const bar = el("span", "barfill");
        bar.style.width = Math.max(2, Math.round((v / max) * 60)) + "px";
        td.appendChild(bar);
      }
      td.appendChild(document.createTextNode(fmtCell(v)));
      tr.appendChild(td);
    });
    table.appendChild(tr);
  });

  $("#match-count").textContent = fmtNum(rows.length) + (rows.length === 1 ? " row" : " rows");
  $("#match-sub").textContent = `${res.took_ms || 0}ms`;

  const note = $("#agg-note");
  if (res.truncated) {
    note.textContent = "Showing the largest groups only — more groups matched than can be returned. Narrow the query or group by a lower-cardinality field.";
    note.hidden = false;
  } else if (!rows.length) {
    note.textContent = "No matching events in this time range.";
    note.hidden = false;
  } else {
    note.hidden = true;
  }
}


// loadMore appends the next keyset page to the current results.
async function loadMore() {
  if (!searchCursor) return;
  try {
    const res = await api(searchBase + "&limit=200&after=" + encodeURIComponent(searchCursor));
    renderResults(res, true);
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

// download triggers an export of all matches in the given format. The admin
// token goes in the Authorization header (never the URL query string, which
// would leak it into browser history, Referer headers, and proxy access logs),
// so we fetch the export and save it from a Blob URL rather than navigating an
// <a> to the endpoint. The whole response is buffered in memory before saving —
// fine for an admin UI, and the cost of keeping the token out of the URL.
async function download(format) {
  const url = buildSearchURL("/api/v1/export") + "&format=" + format;
  const headers = {};
  const t = token();
  if (t) headers["Authorization"] = "Bearer " + t;
  try {
    const res = await fetch(url, { headers });
    if (res.status === 401) {
      $("#token-bar").classList.add("show");
      return;
    }
    if (!res.ok) throw new Error("export failed: " + res.status);
    const blob = await res.blob();
    const objURL = URL.createObjectURL(blob);
    const a = el("a");
    a.href = objURL;
    a.download = "omnilog-export." + format;
    document.body.appendChild(a);
    a.click();
    a.remove();
    // Defer revocation so the browser has started the download before the URL
    // is invalidated (revoking synchronously can cancel it in some browsers).
    setTimeout(() => URL.revokeObjectURL(objURL), 0);
  } catch (e) {
    console.error(e);
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

let lastDay = "";   // day of the most recently appended row, for separators

function renderResults(res, append) {
  if (!append) { rowsEl.replaceChildren(); lastDay = ""; }
  (res.events || []).forEach((e) => {
    const day = dayKey(e.timestamp);
    if (day && day !== lastDay) {
      rowsEl.appendChild(el("div", "day-sep", dayLabel(e.timestamp)));
      lastDay = day;
    }
    rowsEl.appendChild(renderRow(e));
  });
  const shown = rowsEl.children.length;
  // The server stops counting past a cap rather than walking an unbounded
  // match set, so a capped total is a lower bound: show it as "50,000+".
  const total = fmtNum(res.total) + (res.total_capped ? "+" : "");
  $("#match-count").textContent = total + " matching events";
  $("#match-sub").textContent = (res.total_capped || shown < res.total) ? `showing ${fmtNum(shown)}` : "";
  $("#search-empty").hidden = shown > 0;
  searchCursor = res.next_cursor || "";
  $("#load-more").hidden = !searchCursor;
}

function renderRow(e) {
  const lvl = (e.level || "info").toLowerCase();
  const row = el("div", `row lvl-${lvl}`);
  row.dataset.ts = e.timestamp || "";

  const line = el("div", "row-line");
  line.appendChild(el("span", "row-ts", fmtTime(e.timestamp)));
  line.appendChild(el("span", "row-level", lvl));
  const svc = el("span", "row-svc", e.service || "—");
  svc.title = e.service || "";
  line.appendChild(svc);
  const msg = el("span", "row-msg", e.message || e.raw || "");
  msg.title = e.message || e.raw || "";
  line.appendChild(msg);
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
    // Clicking narrows the search by that field, which is the whole reason to
    // have expanded the row.
    chip.title = "Filter by this";
    chip.addEventListener("click", (ev) => {
      ev.stopPropagation();
      const frag = (k === "source" ? "source=" : "attr." + k + "=") + String(meta[k]);
      addToQuery(frag);
    });
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
  $("#hist-axis-l").textContent = h.length ? fmtClock(h[0].start) : "";
  $("#hist-axis-r").textContent = h.length ? fmtClock(h[h.length - 1].start) : "";
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

let histBuckets = [];  // the rendered buckets, so the brush can map x -> time

function renderBars(rawHist, host, keep) {
  const hist = fillBuckets(rawHist);
  const bars = host || $("#bars");
  bars.replaceChildren();
  const max = Math.max(1, ...hist.map((b) => b.count));
  hist.forEach((b) => {
    const bar = el("div", "bar");
    const norm = el("div", "norm");
    // Percentage, not pixels: a bar is bounded by its container by construction.
    // This was a pixel constant that had to be kept in step with the .bars
    // height in CSS, and when the CSS shrank to 46px the 62px bars overflowed
    // upward and collided with the header above them.
    norm.style.height = (b.count > 0 ? Math.max(4, Math.round((b.count / max) * 100)) : 0) + "%";
    bar.title = `${fmtTs(b.start)} · ${fmtNum(b.count)} events`;
    bar.appendChild(norm);
    bars.appendChild(bar);
  });
  if (keep !== false) histBuckets = hist;
  return hist;
}

// The sidebar of facet bars is gone; counts live inline above the results as
// toggle chips. Same information, a fraction of the space, and the results get
// the full width — which is what you are actually here to read.
function renderFacets(facets) {
  const levelsEl = $("#facet-levels");
  levelsEl.replaceChildren();
  const levelMap = {};
  (facets.level || []).forEach((f) => (levelMap[f.value] = f.count));
  LEVELS.forEach((lvl) => {
    if (levelMap[lvl] == null) return;
    levelsEl.appendChild(filterChip(lvl, levelMap[lvl], levelColor(lvl), "level=" + lvl));
  });

  const svcEl = $("#facet-services");
  svcEl.replaceChildren();
  (facets.service || []).slice(0, 5).forEach((f) => {
    if (!f.value) return;
    svcEl.appendChild(filterChip(f.value, f.count, null, "service=" + f.value));
  });
}

function filterChip(name, count, color, queryFrag) {
  const c = el("button", "lvl-chip");
  if (color) {
    const sw = el("i");
    sw.style.background = color;
    c.appendChild(sw);
  }
  c.appendChild(document.createTextNode(name));
  c.appendChild(el("b", null, fmtNum(count)));
  // A chip is a toggle: clicking an active filter takes it back off, so you can
  // undo without editing the query text by hand.
  if (queryIncludes(queryFrag)) c.classList.add("is-on");
  c.addEventListener("click", () => toggleQueryFrag(queryFrag));
  return c;
}

function queryIncludes(frag) {
  return $("#q").value.split(/\s+/).includes(frag);
}

function addToQuery(frag) {
  const q = $("#q");
  if (!queryIncludes(frag)) q.value = (q.value + " " + frag).trim();
  navTo("search");
  runSearch();
}

function toggleQueryFrag(frag) {
  const q = $("#q");
  const parts = q.value.split(/\s+/).filter(Boolean);
  const i = parts.indexOf(frag);
  if (i >= 0) parts.splice(i, 1); else parts.push(frag);
  q.value = parts.join(" ");
  runSearch();
}

$("#search-form").addEventListener("submit", (e) => { e.preventDefault(); runSearch(); });
$("#search-btn").addEventListener("click", runSearch);
$("#range").addEventListener("change", runSearch);
$("#q").addEventListener("input", syncRangeOverride);
syncRangeOverride();
$("#load-more").addEventListener("click", loadMore);
$("#export-ndjson").addEventListener("click", () => download("ndjson"));
$("#export-csv").addEventListener("click", () => download("csv"));
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

// ---------- theme (light / dark / system) ----------
const THEME_ORDER = ["system", "light", "dark"];
function currentTheme() { return document.documentElement.dataset.theme || "system"; }
function setTheme(t) {
  document.documentElement.dataset.theme = t;
  try { localStorage.setItem("omnilog_theme", t); } catch (e) { /* ignore */ }
  $("#theme-toggle").title = "Theme: " + t + " (click to change)";
  reflectThemeSeg();
}
function reflectThemeSeg() {
  const cur = currentTheme();
  document.querySelectorAll("#theme-seg button").forEach((b) => b.classList.toggle("is-on", b.dataset.themeSet === cur));
}
$("#theme-toggle").addEventListener("click", () => {
  const next = THEME_ORDER[(THEME_ORDER.indexOf(currentTheme()) + 1) % THEME_ORDER.length];
  setTheme(next);
});
// Sync the tooltip with the theme applied by the no-flash head script.
setTheme(currentTheme());

// ---------- SETTINGS ----------
let settingsKeys = [];

async function loadSettings() {
  reflectThemeSeg();
  $("#cfg-admintoken").value = token();
  try {
    const cfg = await api("/api/v1/config");
    $("#cfg-retention").value = cfg.retention_days ?? 0;
    $("#cfg-rate").value = cfg.rate_limit_per_sec ?? 0;
    $("#cfg-burst").value = cfg.rate_burst ?? 0;
    $("#cfg-qevents").value = cfg.daily_quota_events ?? 0;
    $("#cfg-qbytes").value = cfg.daily_quota_bytes ?? 0;
    $("#cfg-loglevel").value = cfg.log_level || "info";
    settingsKeys = cfg.ingest_keys || [];
    renderKeys();
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
  loadStatus();
}

function renderKeys() {
  const c = $("#cfg-keys");
  c.replaceChildren();
  if (!settingsKeys.length) {
    c.appendChild(el("span", "hint", "No ingest keys — ingestion is open (dev mode)."));
    return;
  }
  settingsKeys.forEach((k, i) => {
    const chip = el("span", "key-chip");
    chip.appendChild(el("code", null, k));
    const x = el("button", "key-x", "×");
    x.title = "Remove key";
    x.addEventListener("click", () => { settingsKeys.splice(i, 1); renderKeys(); });
    chip.appendChild(x);
    c.appendChild(chip);
  });
}

// Operational counters come from /api/v1/status, not the liveness probe:
// /api/v1/healthz deliberately reports nothing but {"status":"ok"} so an
// unauthenticated caller cannot read traffic volumes off it.
async function loadStatus() {
  try {
    const h = await api("/api/v1/status");
    const g = $("#cfg-status");
    g.replaceChildren();
    const add = (k, v) => { g.appendChild(el("div", "st-k", k)); g.appendChild(el("div", "st-v", String(v))); };
    add("Version", h.version || "—");
    add("Subscribers", h.subscribers ?? 0);
    if (h.ingest) {
      add("Received", fmtNum(h.ingest.received));
      add("Written", fmtNum(h.ingest.written));
      add("Dropped", fmtNum(h.ingest.dropped));
      add("Rejected", fmtNum(h.ingest.rejected));
      add("Queued", fmtNum(h.ingest.queued));
    }
  } catch (e) { if (e.message !== "unauthorized") console.error(e); }
}

async function saveSettings() {
  const num = (sel) => parseInt($(sel).value, 10) || 0;
  const body = {
    retention_days: num("#cfg-retention"),
    rate_limit_per_sec: parseFloat($("#cfg-rate").value) || 0,
    rate_burst: num("#cfg-burst"),
    daily_quota_events: num("#cfg-qevents"),
    daily_quota_bytes: num("#cfg-qbytes"),
    log_level: $("#cfg-loglevel").value,
    ingest_keys: settingsKeys,
  };
  const msg = $("#cfg-msg");
  try {
    const res = await apiSend("PUT", "/api/v1/config", body);
    if (!res.ok) {
      msg.textContent = "Error: " + (await res.text()).trim();
      msg.className = "cfg-msg err";
      return;
    }
    const cfg = await res.json();
    settingsKeys = cfg.ingest_keys || [];
    renderKeys();
    msg.textContent = "Saved.";
    msg.className = "cfg-msg ok";
    setTimeout(() => { msg.textContent = ""; }, 2500);
  } catch (e) {
    if (e.message !== "unauthorized") { msg.textContent = "Error: " + e.message; msg.className = "cfg-msg err"; }
  }
}

$("#cfg-save").addEventListener("click", saveSettings);
$("#cfg-key-add").addEventListener("click", () => {
  const v = $("#cfg-key-new").value.trim();
  if (v && !settingsKeys.includes(v)) { settingsKeys.push(v); renderKeys(); }
  $("#cfg-key-new").value = "";
});
$("#cfg-key-new").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); $("#cfg-key-add").click(); } });
$("#cfg-token-save").addEventListener("click", () => { setToken($("#cfg-admintoken").value.trim()); loadSettings(); });
document.querySelectorAll("#theme-seg button").forEach((b) => b.addEventListener("click", () => setTheme(b.dataset.themeSet)));

// ---------- boot ----------
navTo("dash");

// ---------- ALERTS ----------
// Rules are edited in a single inline form rather than a modal: the list, the
// editor and the channel picker all need to be visible at once when you are
// deciding whether a threshold is sensible.
let alRules = [];
let alChannels = [];
let alEditingID = null;      // null = the editor is creating a new rule
let alSelectedChannels = new Set();

const AL_OPS = { gt: ">", gte: "≥", lt: "<", lte: "≤", eq: "=", ne: "≠" };

async function loadAlerts() {
  try {
    const [rules, chans] = await Promise.all([
      api("/api/v1/alerts"),
      api("/api/v1/alerts/channels"),
    ]);
    alRules = rules.rules || [];
    alChannels = chans.channels || [];
    renderRules();
    renderChannels();
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

function fmtDuration(sec) {
  if (!sec) return "—";
  if (sec % 3600 === 0) return sec / 3600 + "h";
  if (sec % 60 === 0) return sec / 60 + "m";
  return sec + "s";
}

function renderRules() {
  const host = $("#al-rules");
  host.replaceChildren();
  if (!alRules.length) {
    host.appendChild(el("span", "hint", "No alert rules yet."));
    return;
  }
  alRules.forEach((r) => {
    const row = el("div", "rule");

    const dot = el("span", "rule-state " + (r.state || "unknown"));
    dot.title = "state: " + (r.state || "unknown");
    row.appendChild(dot);

    const main = el("div", "rule-main");
    const title = el("div", "rule-name", r.name);
    if (!r.enabled) title.appendChild(el("span", "rule-off", "disabled"));
    main.appendChild(title);
    main.appendChild(el("div", "rule-query mono", r.query));

    const meta = `${AL_OPS[r.condition.op] || r.condition.op} ${r.condition.value}` +
      ` · window ${fmtDuration(r.window_seconds)} · every ${fmtDuration(r.interval_seconds)}` +
      ` · last ${r.last_value ?? 0}`;
    main.appendChild(el("div", "rule-meta", meta));
    if (r.last_error) main.appendChild(el("div", "rule-err", r.last_error));
    row.appendChild(main);

    const actions = el("div", "rule-actions");
    const edit = el("button", "chip", "Edit");
    edit.addEventListener("click", () => openRuleEditor(r));
    const del = el("button", "chip", "Delete");
    del.addEventListener("click", () => deleteRule(r));
    actions.appendChild(edit);
    actions.appendChild(del);
    row.appendChild(actions);

    host.appendChild(row);
  });
}

function renderChannels() {
  const host = $("#al-chan-list");
  host.replaceChildren();
  if (!alChannels.length) {
    host.appendChild(el("span", "hint", "No channels yet — a rule with no channel changes state silently."));
  }
  alChannels.forEach((c) => {
    const row = el("div", "chan");
    row.appendChild(el("span", "chan-type", c.type));
    const main = el("div", "chan-main");
    const name = el("div", "chan-name", c.name);
    // The token reads back masked, so its presence is all the UI can show —
    // and all it should: this list is served without auth when no admin token
    // is set.
    if (c.token) name.appendChild(el("span", "chan-auth", " 🔒 authenticated"));
    main.appendChild(name);
    main.appendChild(el("div", "chan-url mono", c.url));
    row.appendChild(main);

    const actions = el("div", "rule-actions");
    const test = el("button", "chip", "Test");
    test.addEventListener("click", () => testChannel(c, test));
    const del = el("button", "chip", "Delete");
    del.addEventListener("click", () => deleteChannel(c));
    actions.appendChild(test);
    actions.appendChild(del);
    row.appendChild(actions);
    host.appendChild(row);
  });
  renderChannelPicker();
}

function renderChannelPicker() {
  const host = $("#al-channels");
  host.replaceChildren();
  if (!alChannels.length) {
    host.appendChild(el("span", "hint", "Add a channel to be notified."));
    return;
  }
  alChannels.forEach((c) => {
    const chip = el("button", "chip" + (alSelectedChannels.has(c.id) ? " is-on" : ""), c.name);
    chip.type = "button";
    chip.addEventListener("click", () => {
      if (alSelectedChannels.has(c.id)) alSelectedChannels.delete(c.id);
      else alSelectedChannels.add(c.id);
      renderChannelPicker();
    });
    host.appendChild(chip);
  });
}

function openRuleEditor(rule) {
  alEditingID = rule ? rule.id : null;
  alSelectedChannels = new Set(rule ? rule.channels || [] : []);
  $("#al-editor-title").textContent = rule ? "Edit rule" : "New rule";
  $("#al-name").value = rule ? rule.name : "";
  $("#al-query").value = rule ? rule.query : "level=error";
  $("#al-window").value = rule ? rule.window_seconds : 300;
  $("#al-interval").value = rule ? rule.interval_seconds : 60;
  $("#al-op").value = rule ? rule.condition.op : "gt";
  $("#al-value").value = rule ? rule.condition.value : 10;
  $("#al-severity").value = (rule && rule.severity) || "warning";
  $("#al-enabled").checked = rule ? rule.enabled : true;
  $("#al-editor-msg").textContent = "";
  $("#al-dryrun-out").hidden = true;
  $("#al-editor").hidden = false;
  renderChannelPicker();
  $("#al-name").focus();
}

function ruleFromEditor() {
  return {
    name: $("#al-name").value.trim(),
    query: $("#al-query").value.trim(),
    window_seconds: parseInt($("#al-window").value, 10) || 0,
    interval_seconds: parseInt($("#al-interval").value, 10) || 0,
    condition: { op: $("#al-op").value, value: parseFloat($("#al-value").value) || 0 },
    severity: $("#al-severity").value,
    channels: [...alSelectedChannels],
    enabled: $("#al-enabled").checked,
  };
}

async function saveRule() {
  const msg = $("#al-editor-msg");
  const body = ruleFromEditor();
  const method = alEditingID ? "PUT" : "POST";
  const path = alEditingID ? "/api/v1/alerts/" + alEditingID : "/api/v1/alerts";
  try {
    const res = await apiSend(method, path, body);
    if (!res.ok) {
      // The server validates the query too, so this is where a bad expression
      // surfaces — at save time rather than silently every interval.
      msg.textContent = (await res.text()).trim();
      msg.className = "cfg-msg err";
      return;
    }
    msg.textContent = "Saved.";
    msg.className = "cfg-msg ok";
    $("#al-editor").hidden = true;
    alEditingID = null;
    await loadAlerts();
  } catch (e) {
    if (e.message !== "unauthorized") { msg.textContent = e.message; msg.className = "cfg-msg err"; }
  }
}

async function dryRunRule() {
  const out = $("#al-dryrun-out");
  const msg = $("#al-editor-msg");
  if (!alEditingID) {
    msg.textContent = "Save the rule first, then test it.";
    msg.className = "cfg-msg err";
    return;
  }
  try {
    const res = await apiSend("POST", "/api/v1/alerts/" + alEditingID + "/test");
    const body = await res.json();
    out.textContent = JSON.stringify(body, null, 2);
    out.hidden = false;
    msg.textContent = body.firing ? "Would fire now." : "Would not fire now.";
    msg.className = "cfg-msg " + (body.firing ? "err" : "ok");
  } catch (e) {
    if (e.message !== "unauthorized") { msg.textContent = e.message; msg.className = "cfg-msg err"; }
  }
}

async function deleteRule(rule) {
  try {
    await apiSend("DELETE", "/api/v1/alerts/" + rule.id);
    if (alEditingID === rule.id) { $("#al-editor").hidden = true; alEditingID = null; }
    await loadAlerts();
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

async function addChannel() {
  const msg = $("#al-chan-msg");
  const body = {
    name: $("#al-chan-name").value.trim(),
    type: $("#al-chan-type").value,
    url: $("#al-chan-url").value.trim(),
  };
  const token = $("#al-chan-token").value.trim();
  if (token) body.token = token;
  try {
    const res = await apiSend("POST", "/api/v1/alerts/channels", body);
    if (!res.ok) {
      msg.textContent = (await res.text()).trim();
      msg.className = "cfg-msg err";
      return;
    }
    $("#al-chan-name").value = "";
    $("#al-chan-url").value = "";
    $("#al-chan-token").value = "";
    msg.textContent = "Added.";
    msg.className = "cfg-msg ok";
    await loadAlerts();
  } catch (e) {
    if (e.message !== "unauthorized") { msg.textContent = e.message; msg.className = "cfg-msg err"; }
  }
}

async function testChannel(chan, btn) {
  const msg = $("#al-chan-msg");
  const original = btn.textContent;
  btn.textContent = "Sending…";
  try {
    const res = await apiSend("POST", "/api/v1/alerts/channels/" + chan.id + "/test");
    const body = await res.json();
    msg.textContent = body.ok ? `Delivered to ${chan.name}.` : `Failed: ${body.error}`;
    msg.className = "cfg-msg " + (body.ok ? "ok" : "err");
  } catch (e) {
    if (e.message !== "unauthorized") { msg.textContent = e.message; msg.className = "cfg-msg err"; }
  } finally {
    btn.textContent = original;
  }
}

async function deleteChannel(chan) {
  try {
    await apiSend("DELETE", "/api/v1/alerts/channels/" + chan.id);
    alSelectedChannels.delete(chan.id);
    await loadAlerts();
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

// Omni-Notify rejects every call without a token, so make the field's necessity
// visible at the moment the type is chosen rather than at the first failed
// delivery.
function syncChannelTypeHints() {
  const isNotify = $("#al-chan-type").value === "omni-notify";
  $("#al-chan-token-hint").hidden = !isNotify;
  $("#al-chan-token").placeholder = isNotify ? "OMNI_NOTIFY_API_TOKEN (required)" : "bearer token (optional)";
  $("#al-chan-url").placeholder = isNotify
    ? "http://omni-notify:8088"
    : "https://hooks.slack.com/services/...";
}
$("#al-chan-type").addEventListener("change", syncChannelTypeHints);
syncChannelTypeHints();

$("#al-new").addEventListener("click", () => openRuleEditor(null));
$("#al-cancel").addEventListener("click", () => { $("#al-editor").hidden = true; alEditingID = null; });
$("#al-save").addEventListener("click", saveRule);
$("#al-dryrun").addEventListener("click", dryRunRule);
$("#al-chan-add").addEventListener("click", addChannel);


// ---------- histogram brush: drag to narrow the time range ----------
// Dragging writes from=/to= into the query rather than holding a hidden bit of
// state, so the selected window is visible, editable and shareable as a URL —
// and the query's own bounds beat the range picker server-side.
(function initBrush() {
  const wrap = $("#bars-wrap");
  if (!wrap) return;
  let startX = null, box = null;

  const xToTime = (x) => {
    if (!histBuckets.length) return null;
    const r = wrap.getBoundingClientRect();
    const frac = Math.min(1, Math.max(0, (x - r.left) / r.width));
    const i = Math.min(histBuckets.length - 1, Math.floor(frac * histBuckets.length));
    return new Date(histBuckets[i].start).getTime();
  };
  const step = () => {
    if (histBuckets.length < 2) return 60000;
    return new Date(histBuckets[1].start).getTime() - new Date(histBuckets[0].start).getTime();
  };

  wrap.addEventListener("mousedown", (e) => {
    if (!histBuckets.length) return;
    startX = e.clientX;
    box = el("div", "brush");
    wrap.appendChild(box);
    e.preventDefault();
  });
  window.addEventListener("mousemove", (e) => {
    if (startX == null || !box) return;
    const r = wrap.getBoundingClientRect();
    const a = Math.max(r.left, Math.min(startX, e.clientX));
    const b = Math.min(r.right, Math.max(startX, e.clientX));
    box.style.left = (a - r.left) + "px";
    box.style.width = (b - a) + "px";
  });
  window.addEventListener("mouseup", (e) => {
    if (startX == null) return;
    const moved = Math.abs(e.clientX - startX);
    const from = xToTime(Math.min(startX, e.clientX));
    const to = xToTime(Math.max(startX, e.clientX));
    if (box) { box.remove(); box = null; }
    startX = null;
    // A click is not a drag: below a few pixels the user was probably just
    // clicking, and snapping the range to one bucket would be a surprise.
    if (moved < 4 || from == null || to == null) return;
    setQueryRange(new Date(from), new Date(to + step()));
  });
})();

// setQueryRange rewrites the query's time bounds, dropping whatever was there.
function setQueryRange(from, to) {
  const q = $("#q");
  const parts = q.value.split(/\s+/).filter((t) => t && !/^(last|from|to)=/i.test(t));
  parts.push("from=" + from.toISOString(), "to=" + to.toISOString());
  q.value = parts.join(" ");
  syncRangeOverride();
  runSearch();
}

// ---------- wrap toggle ----------
$("#wrap-toggle").addEventListener("click", () => {
  const on = rowsEl.classList.toggle("wrap");
  $("#wrap-toggle").classList.toggle("is-on", on);
});

// ---------- OVERVIEW ----------
async function loadDash() {
  const range = $("#dash-range").value;
  try {
    const [stats, alerts, errs] = await Promise.all([
      api(`/api/v1/search/stats?last=${range}&interval=${bucketFor(range)}`),
      api("/api/v1/alerts").catch(() => ({ rules: [] })),
      api(`/api/v1/search?q=${encodeURIComponent("level=(error,fatal)")}&last=${range}&limit=8`).catch(() => ({ events: [] })),
    ]);
    renderDashTiles(stats, alerts);
    renderBars(stats.histogram || [], $("#dash-bars"), false);
    const h = stats.histogram || [];
    $("#dash-hist-sub").textContent = h.length ? `${fmtClock(h[0].start)} – ${fmtClock(h[h.length - 1].start)}` : "no data";
    renderDashServices(stats.facets || {});
    renderDashAlerts(alerts.rules || []);
    renderDashErrors(errs.events || []);
  } catch (e) {
    if (e.message !== "unauthorized") console.error(e);
  }
}

function levelCount(facets, lvl) {
  const f = (facets.level || []).find((x) => x.value === lvl);
  return f ? f.count : 0;
}

function renderDashTiles(stats, alerts) {
  const host = $("#dash-tiles");
  host.replaceChildren();
  const facets = stats.facets || {};
  const errors = levelCount(facets, "error") + levelCount(facets, "fatal");
  const warns = levelCount(facets, "warn");
  const firing = (alerts.rules || []).filter((r) => r.state === "firing").length;

  const tile = (k, v, sub, sev, onClick) => {
    const t = el("div", "tile" + (sev ? " sev-" + sev : "") + (onClick ? " clickable" : ""));
    t.appendChild(el("div", "tile-k", k));
    t.appendChild(el("div", "tile-v", v));
    if (sub) t.appendChild(el("div", "tile-sub", sub));
    if (onClick) t.addEventListener("click", onClick);
    host.appendChild(t);
  };

  const range = $("#dash-range").value;
  const goSearch = (frag) => () => { $("#q").value = frag; $("#range").value = range; navTo("search"); runSearch(); };

  tile("Events", fmtNum(stats.total) + (stats.total_capped ? "+" : ""), "in this range", null, goSearch(""));
  tile("Errors", fmtNum(errors), errors ? "needs a look" : "all clear", errors ? "error" : null, goSearch("level=(error,fatal)"));
  tile("Warnings", fmtNum(warns), "", warns ? "warn" : null, goSearch("level=warn"));
  tile("Services", fmtNum((facets.service || []).length), "reporting", null, null);
  tile("Alerts firing", fmtNum(firing), (alerts.rules || []).length + " rules", firing ? "error" : null, () => navTo("alerts"));
}

function renderDashServices(facets) {
  const host = $("#dash-services");
  host.replaceChildren();
  const svc = (facets.service || []).slice(0, 8);
  if (!svc.length) { host.appendChild(el("span", "hint", "No events in this range.")); return; }
  const max = Math.max(1, ...svc.map((f) => f.count));
  svc.forEach((f) => {
    const row = el("div", "bl-row");
    const top = el("div", "bl-top");
    top.appendChild(el("span", "bl-name", f.value || "—"));
    top.appendChild(el("span", "bl-count", fmtNum(f.count)));
    row.appendChild(top);
    const bar = el("div", "bl-bar");
    const fill = el("i");
    fill.style.width = Math.round((f.count / max) * 100) + "%";
    bar.appendChild(fill);
    row.appendChild(bar);
    row.addEventListener("click", () => {
      $("#q").value = "service=" + f.value;
      $("#range").value = $("#dash-range").value;
      navTo("search"); runSearch();
    });
    host.appendChild(row);
  });
}

function renderDashAlerts(rules) {
  const host = $("#dash-alerts");
  host.replaceChildren();
  if (!rules.length) {
    host.appendChild(el("span", "hint", "No alert rules yet."));
    return;
  }
  rules.slice(0, 8).forEach((r) => {
    const row = el("div", "rule");
    const dot = el("span", "rule-state " + (r.state || "unknown"));
    dot.title = "state: " + (r.state || "unknown");
    row.appendChild(dot);
    const main = el("div", "rule-main");
    main.appendChild(el("div", "rule-name", r.name));
    main.appendChild(el("div", "rule-query", r.query));
    row.appendChild(main);
    row.addEventListener("click", () => navTo("alerts"));
    row.style.cursor = "pointer";
    host.appendChild(row);
  });
}

function renderDashErrors(events) {
  const host = $("#dash-errors");
  host.replaceChildren();
  if (!events.length) {
    host.appendChild(el("span", "hint", "No errors in this range."));
    return;
  }
  events.forEach((e) => {
    const lvl = (e.level || "info").toLowerCase();
    const row = el("div", "mini-row lvl-" + lvl);
    row.appendChild(el("span", "mini-ts", fmtTime(e.timestamp).slice(0, 8)));
    const msg = el("span", "mini-msg", `${e.service || "—"} · ${e.message || e.raw || ""}`);
    msg.title = e.message || "";
    row.appendChild(msg);
    row.addEventListener("click", () => {
      $("#q").value = "level=(error,fatal) service=" + (e.service || "");
      navTo("search"); runSearch();
    });
    host.appendChild(row);
  });
}

$("#dash-range").addEventListener("change", loadDash);
$("#dash-refresh").addEventListener("click", loadDash);

// ---------- keyboard ----------
// Log triage is a keyboard activity: you scan, you narrow, you scan again.
let cursorIdx = -1;

function visibleRows() {
  const host = views.search.hidden ? streamRows : rowsEl;
  return Array.from(host.querySelectorAll(".row"));
}
function moveCursor(delta) {
  const rows = visibleRows();
  if (!rows.length) return;
  rows.forEach((r) => r.classList.remove("is-cursor"));
  cursorIdx = Math.max(0, Math.min(rows.length - 1, cursorIdx + delta));
  const row = rows[cursorIdx];
  row.classList.add("is-cursor");
  row.scrollIntoView({ block: "nearest" });
}
function typingInField(t) {
  return t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT");
}

let gPending = false;
document.addEventListener("keydown", (e) => {
  const inField = typingInField(e.target);

  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault(); openPalette(); return;
  }
  if (e.key === "Escape") {
    if (!$("#palette").hidden) { closePalette(); return; }
    const open = document.querySelector(".row.open");
    if (open) open.classList.remove("open");
    if (inField) e.target.blur();
    return;
  }
  if (inField) return;

  if (gPending) {
    gPending = false;
    const map = { o: "dash", s: "search", t: "tail", a: "alerts", c: "settings" };
    if (map[e.key]) { e.preventDefault(); navTo(map[e.key]); }
    return;
  }
  switch (e.key) {
    case "/":
      e.preventDefault(); navTo("search"); $("#q").focus(); $("#q").select(); break;
    case "g": gPending = true; setTimeout(() => { gPending = false; }, 900); break;
    case "j": e.preventDefault(); moveCursor(1); break;
    case "k": e.preventDefault(); moveCursor(-1); break;
    case "Enter": {
      const rows = visibleRows();
      if (rows[cursorIdx]) { e.preventDefault(); rows[cursorIdx].classList.toggle("open"); }
      break;
    }
  }
});

// ---------- command palette ----------
let palItems = [], palSel = 0;

function paletteActions() {
  const acts = [
    { kind: "go", label: "Overview", run: () => navTo("dash") },
    { kind: "go", label: "Search", run: () => navTo("search") },
    { kind: "go", label: "Live tail", run: () => navTo("tail") },
    { kind: "go", label: "Alerts", run: () => navTo("alerts") },
    { kind: "go", label: "Settings", run: () => navTo("settings") },
    { kind: "filter", label: "Only errors", run: () => { $("#q").value = "level=(error,fatal)"; navTo("search"); runSearch(); } },
    { kind: "filter", label: "Only warnings", run: () => { $("#q").value = "level=warn"; navTo("search"); runSearch(); } },
    { kind: "filter", label: "Clear the query", run: () => { $("#q").value = ""; navTo("search"); runSearch(); } },
    { kind: "act", label: "Toggle message wrapping", run: () => $("#wrap-toggle").click() },
    { kind: "act", label: "Toggle theme", run: () => $("#theme-toggle").click() },
    { kind: "act", label: "Export matches as NDJSON", run: () => download("ndjson") },
    { kind: "act", label: "Export matches as CSV", run: () => download("csv") },
  ];
  return acts;
}

function openPalette() {
  $("#palette").hidden = false;
  $("#pal-input").value = "";
  renderPalette("");
  $("#pal-input").focus();
}
function closePalette() { $("#palette").hidden = true; }

function renderPalette(term) {
  const t = term.trim().toLowerCase();
  palItems = paletteActions().filter((a) => !t || a.label.toLowerCase().includes(t));
  // Anything typed is also offered as a query, so the palette doubles as a
  // quick way to run a search without leaving the keyboard.
  if (t) {
    palItems.unshift({
      kind: "search", label: `Search for “${term.trim()}”`,
      run: () => { $("#q").value = term.trim(); navTo("search"); runSearch(); },
    });
  }
  palSel = 0;
  const list = $("#pal-list");
  list.replaceChildren();
  if (!palItems.length) {
    list.appendChild(el("div", "pal-empty", "Nothing matches."));
    return;
  }
  palItems.forEach((it, i) => {
    const row = el("div", "pal-item" + (i === palSel ? " is-sel" : ""));
    row.appendChild(el("span", "pal-kind", it.kind));
    row.appendChild(el("span", "pal-label", it.label));
    row.addEventListener("click", () => { closePalette(); it.run(); });
    list.appendChild(row);
  });
}

function palMove(d) {
  if (!palItems.length) return;
  palSel = (palSel + d + palItems.length) % palItems.length;
  Array.from($("#pal-list").children).forEach((c, i) => c.classList.toggle("is-sel", i === palSel));
  const sel = $("#pal-list").children[palSel];
  if (sel) sel.scrollIntoView({ block: "nearest" });
}

$("#pal-input").addEventListener("input", (e) => renderPalette(e.target.value));
$("#pal-input").addEventListener("keydown", (e) => {
  if (e.key === "ArrowDown") { e.preventDefault(); palMove(1); }
  else if (e.key === "ArrowUp") { e.preventDefault(); palMove(-1); }
  else if (e.key === "Enter") {
    e.preventDefault();
    const it = palItems[palSel];
    if (it) { closePalette(); it.run(); }
  }
});
$("#palette").addEventListener("click", (e) => { if (e.target.id === "palette") closePalette(); });
$("#palette-btn").addEventListener("click", openPalette);


// ---------- histogram collapse ----------
// Remembered, because whether you want the chart is a working style rather than
// a per-search decision.
(function initHistCollapse() {
  const panel = $("#hist-panel"), btn = $("#hist-collapse");
  if (!panel || !btn) return;
  const apply = (on) => {
    panel.classList.toggle("collapsed", on);
    btn.textContent = on ? "show" : "hide";
    btn.title = on ? "Show the chart" : "Collapse the chart";
  };
  apply(localStorage.getItem("omnilog_hist_collapsed") === "1");
  btn.addEventListener("click", () => {
    const on = !panel.classList.contains("collapsed");
    apply(on);
    try { localStorage.setItem("omnilog_hist_collapsed", on ? "1" : "0"); } catch (e) { /* ignore */ }
  });
})();
