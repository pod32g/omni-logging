import { useCallback, useEffect, useRef, useState } from "react";
import type { AggResult, Facet, LogEvent, SearchResult, StatsResult } from "../types";
import { apiGet, bucketFor, download, searchURL, Unauthorized } from "../api";
import { hasPipeline, hasTimeDirective, toggleTerm, withRange, addTerm } from "../query";
import { fmtCell, fmtNum } from "../format";
import { Histogram } from "../components/Histogram";
import { EventList } from "../components/EventList";
import { FilterBar } from "../components/FilterBar";

const PAGE = 200;

export interface SearchHandle {
  setQuery: (q: string, range?: string) => void;
}

interface Props {
  active: boolean;
  theme: string;
  onUnauthorized: () => void;
  handleRef: React.MutableRefObject<SearchHandle | null>;
}

export function Search({ active, theme, onUnauthorized, handleRef }: Props) {
  const [query, setQuery] = useState("");
  const [draft, setDraft] = useState("");
  const [range, setRange] = useState("1h");
  const [order, setOrder] = useState("newest");
  const [wrap, setWrap] = useState(false);

  const [events, setEvents] = useState<LogEvent[]>([]);
  const [result, setResult] = useState<SearchResult | null>(null);
  const [stats, setStats] = useState<StatsResult | null>(null);
  const [agg, setAgg] = useState<AggResult | null>(null);
  const [aggError, setAggError] = useState("");
  const [cursor, setCursor] = useState("");
  const [hover, setHover] = useState("");
  const [cursorIdx, setCursorIdx] = useState(-1);
  const [loaded, setLoaded] = useState(false);

  const listRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const isAgg = hasPipeline(query);

  const run = useCallback(
    async (q: string, r: string, o: string) => {
      const p = { q, range: r, order: o };
      try {
        if (hasPipeline(q)) {
          const [a, s] = await Promise.all([
            apiGet<AggResult>(searchURL("/api/v1/aggregate", p)),
            apiGet<StatsResult>(searchURL("/api/v1/search/stats", p) + "&interval=" + bucketFor(r)),
          ]);
          setAgg(a);
          setAggError("");
          setStats(s);
          setEvents([]);
          setResult(null);
          setCursor("");
        } else {
          const [res, s] = await Promise.all([
            apiGet<SearchResult>(searchURL("/api/v1/search", p) + `&limit=${PAGE}`),
            apiGet<StatsResult>(searchURL("/api/v1/search/stats", p) + "&interval=" + bucketFor(r)),
          ]);
          setEvents(res.events ?? []);
          setResult(res);
          setCursor(res.next_cursor ?? "");
          setStats(s);
          setAgg(null);
          setAggError("");
        }
        setCursorIdx(-1);
        setLoaded(true);
      } catch (e) {
        if (e instanceof Unauthorized) onUnauthorized();
        else if (hasPipeline(q)) setAggError((e as Error).message);
        else console.error(e);
      }
    },
    [onUnauthorized],
  );

  // Load once the view is first shown, rather than at boot — the Overview is
  // the landing page, and a search nobody asked for is a wasted query.
  useEffect(() => {
    if (active && !loaded) run(query, range, order);
  }, [active, loaded, query, range, order, run]);

  const apply = useCallback(
    (q: string, r = range, o = order) => {
      setQuery(q);
      setDraft(q);
      setRange(r);
      setOrder(o);
      run(q, r, o);
    },
    [range, order, run],
  );

  // Lets the shell drive the view: palette actions, overview click-throughs.
  useEffect(() => {
    handleRef.current = { setQuery: (q, r) => apply(q, r ?? range) };
  }, [apply, handleRef, range]);

  // j/k/Enter over the result list while this view has focus.
  useEffect(() => {
    if (!active) return;
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (t && ["INPUT", "TEXTAREA", "SELECT"].includes(t.tagName)) return;
      if (e.key === "j" || e.key === "k") {
        e.preventDefault();
        setCursorIdx((i) => {
          const next = Math.max(0, Math.min(events.length - 1, i + (e.key === "j" ? 1 : -1)));
          const rows = listRef.current?.querySelectorAll<HTMLElement>(".row");
          rows?.[next]?.scrollIntoView({ block: "nearest" });
          return next;
        });
      } else if (e.key === "Enter" && cursorIdx >= 0) {
        e.preventDefault();
        const rows = listRef.current?.querySelectorAll<HTMLElement>(".row-line");
        rows?.[cursorIdx]?.click();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [active, events.length, cursorIdx]);

  // Focus the box when the shell asks for it (the "/" shortcut).
  useEffect(() => {
    const onFocus = () => inputRef.current?.focus();
    window.addEventListener("omnilog:focus-search", onFocus);
    return () => window.removeEventListener("omnilog:focus-search", onFocus);
  }, []);

  async function loadMore() {
    if (!cursor) return;
    try {
      const res = await apiGet<SearchResult>(
        searchURL("/api/v1/search", { q: query, range, order }) +
          `&limit=${PAGE}&after=${encodeURIComponent(cursor)}`,
      );
      setEvents((prev) => [...prev, ...(res.events ?? [])]);
      setResult(res);
      setCursor(res.next_cursor ?? "");
    } catch (e) {
      if (e instanceof Unauthorized) onUnauthorized();
      else console.error(e);
    }
  }

  const overridden = hasTimeDirective(draft);
  const levels: Facet[] = stats?.facets?.level ?? [];
  const services: Facet[] = stats?.facets?.service ?? [];
  const total = result ? fmtNum(result.total) + (result.total_capped ? "+" : "") : "0";

  return (
    <section className="view" id="view-search">
      <div className="querybar">
        <form
          className="field"
          id="search-form"
          onSubmit={(e) => {
            e.preventDefault();
            apply(draft);
          }}
        >
          <svg className="ico" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4.3-4.3" /></svg>
          <input
            id="q"
            ref={inputRef}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            autoComplete="off"
            spellCheck={false}
            placeholder="level=error service=checkout-api timeout"
          />
          <span className="kbd">/</span>
        </form>
        <div
          className={`select${overridden ? " is-overridden" : ""}`}
          id="range-select"
          title={overridden ? "The query sets its own time range, so this picker is ignored." : ""}
        >
          <svg className="ico" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></svg>
          <select id="range" value={range} onChange={(e) => apply(draft, e.target.value)}>
            <option value="15m">Last 15 minutes</option>
            <option value="1h">Last 1 hour</option>
            <option value="6h">Last 6 hours</option>
            <option value="24h">Last 24 hours</option>
            <option value="168h">Last 7 days</option>
            <option value="">All time</option>
          </select>
        </div>
        <button className="btn-primary" id="search-btn" onClick={() => apply(draft)}>Search</button>
        {overridden && <span className="range-override" id="range-override">query sets the range</span>}
      </div>

      <FilterBar
        levels={levels}
        services={services}
        query={draft}
        wrap={wrap}
        onToggleTerm={(t) => apply(toggleTerm(draft, t))}
        onToggleWrap={() => setWrap((w) => !w)}
      />

      <div className="hist-panel" id="hist-panel">
        <div className="hist-head">
          <div className="hist-title">Events over time</div>
          <div className="hist-sub" id="hist-sub">{hover}</div>
          <div className="fb-spacer" />
          <div className="hist-total">
            <strong id="hist-count">{fmtNum(stats?.total)}</strong>
            <span id="hist-took">events · {stats?.took_ms ?? 0}ms</span>
          </div>
        </div>
        <div id="bars-wrap">
          <Histogram
            buckets={stats?.histogram ?? []}
            theme={theme}
            onHover={setHover}
            onSelect={(from, to) => apply(withRange(draft, from, to))}
          />
        </div>
        <div className="hist-foot">
          <span className="hist-axis" />
          <span className="hist-hint">drag to zoom a time range</span>
          <span className="hist-axis" />
        </div>
      </div>

      <div className="results-toolbar">
        <div className="rt-left">
          <strong id="match-count">
            {isAgg
              ? `${fmtNum(agg?.rows?.length ?? 0)} ${agg?.rows?.length === 1 ? "row" : "rows"}`
              : `${total} matching events`}
          </strong>
          <span id="match-sub">
            {isAgg
              ? `${agg?.took_ms ?? 0}ms`
              : result && (result.total_capped || events.length < result.total)
                ? `showing ${fmtNum(events.length)}`
                : ""}
          </span>
        </div>
        {!isAgg && (
          <div className="rt-right">
            <button
              className="chip"
              id="export-ndjson"
              title="Download all matches as NDJSON"
              onClick={() =>
                download(searchURL("/api/v1/export", { q: query, range, order }) + "&format=ndjson", "omnilog-export.ndjson")
              }
            >
              Export NDJSON
            </button>
            <button
              className="chip"
              id="export-csv"
              title="Download all matches as CSV"
              onClick={() =>
                download(searchURL("/api/v1/export", { q: query, range, order }) + "&format=csv", "omnilog-export.csv")
              }
            >
              Export CSV
            </button>
            <button
              className="chip"
              id="order-chip"
              onClick={() => apply(draft, range, order === "newest" ? "oldest" : "newest")}
            >
              <span>{order === "newest" ? "Newest first" : "Oldest first"}</span>
              <svg className="ico-sm" viewBox="0 0 24 24"><path d="M6 9l6 6 6-6" /></svg>
            </button>
          </div>
        )}
      </div>

      {isAgg ? (
        <>
          <div className="agg-wrap" id="agg-wrap">
            <table className="agg" id="agg-table">
              <thead>
                <tr>{(agg?.columns ?? []).map((c) => <th key={c}>{c}</th>)}</tr>
              </thead>
              <tbody>
                {(agg?.rows ?? []).map((r, i) => {
                  const firstMeasure = agg?.group_columns ?? 0;
                  const max = Math.max(
                    1,
                    ...(agg?.rows ?? []).map((rr) => (typeof rr[firstMeasure] === "number" ? (rr[firstMeasure] as number) : 0)),
                  );
                  return (
                    <tr key={i}>
                      {r.map((v, j) => (
                        <td key={j} className={typeof v === "number" ? "num" : "label"}>
                          {typeof v === "number" && j === firstMeasure && (
                            <span className="barfill" style={{ width: Math.max(2, Math.round((v / max) * 60)) }} />
                          )}
                          {fmtCell(v)}
                        </td>
                      ))}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          {(aggError || agg?.truncated || !(agg?.rows ?? []).length) && (
            <div className="hint agg-note" id="agg-note">
              {aggError
                ? aggError
                : agg?.truncated
                  ? "Showing the largest groups only — more groups matched than can be returned. Narrow the query or group by a lower-cardinality field."
                  : "No matching events in this time range."}
            </div>
          )}
        </>
      ) : (
        <>
          <div className="col-header" id="col-header">
            <span>Time</span><span>Level</span><span>Service</span><span>Message</span><span />
          </div>
          <EventList
            events={events}
            cursorIdx={cursorIdx}
            wrap={wrap}
            listRef={listRef}
            onFilter={(t) => apply(addTerm(draft, t))}
          />
          {cursor && <button className="load-more" id="load-more" onClick={loadMore}>Load more</button>}
          {loaded && events.length === 0 && (
            <div className="empty" id="search-empty">No matching events. Adjust your query or time range.</div>
          )}
        </>
      )}
    </section>
  );
}
