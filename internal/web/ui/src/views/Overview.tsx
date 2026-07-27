import { useCallback, useEffect, useState } from "react";
import type { AlertRule, LogEvent, SearchResult, StatsResult } from "../types";
import { apiGet, bucketFor, Unauthorized } from "../api";
import { fmtClock, fmtNum, fmtTime } from "../format";
import { Histogram } from "../components/Histogram";

interface Props {
  active: boolean;
  theme: string;
  onUnauthorized: () => void;
  onSearch: (query: string, range: string) => void;
}

function levelCount(stats: StatsResult | null, lvl: string): number {
  return stats?.facets?.level?.find((f) => f.value === lvl)?.count ?? 0;
}

export function Overview({ active, theme, onUnauthorized, onSearch }: Props) {
  const [range, setRange] = useState("1h");
  const [stats, setStats] = useState<StatsResult | null>(null);
  const [rules, setRules] = useState<AlertRule[]>([]);
  const [errors, setErrors] = useState<LogEvent[]>([]);

  const load = useCallback(async () => {
    try {
      const [s, a, e] = await Promise.all([
        apiGet<StatsResult>(`/api/v1/search/stats?last=${range}&interval=${bucketFor(range)}`),
        apiGet<{ rules: AlertRule[] | null }>("/api/v1/alerts").catch(() => ({ rules: [] })),
        apiGet<SearchResult>(
          `/api/v1/search?q=${encodeURIComponent("level=(error,fatal)")}&last=${range}&limit=8`,
        ).catch<SearchResult>(() => ({ events: [], count: 0, total: 0 })),
      ]);
      setStats(s);
      setRules(a.rules ?? []);
      setErrors(e.events ?? []);
    } catch (err) {
      if (err instanceof Unauthorized) onUnauthorized();
      else console.error(err);
    }
  }, [range, onUnauthorized]);

  useEffect(() => {
    if (active) load();
  }, [active, load]);

  const errCount = levelCount(stats, "error") + levelCount(stats, "fatal");
  const warnCount = levelCount(stats, "warn");
  const firing = rules.filter((r) => r.state === "firing").length;
  const services = stats?.facets?.service ?? [];
  const maxSvc = Math.max(1, ...services.map((s) => s.count));
  const hist = stats?.histogram ?? [];

  const tile = (
    k: string,
    v: string,
    sub: string,
    sev?: "error" | "warn",
    onClick?: () => void,
  ) => (
    <div
      key={k}
      className={`tile${sev ? " sev-" + sev : ""}${onClick ? " clickable" : ""}`}
      onClick={onClick}
    >
      <div className="tile-k">{k}</div>
      <div className="tile-v">{v}</div>
      {sub && <div className="tile-sub">{sub}</div>}
    </div>
  );

  return (
    <section className="view" id="view-dash">
      <div className="querybar">
        <div className="fb-label">Overview</div>
        <div className="fb-spacer" />
        <div className="select">
          <svg className="ico" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" /><path d="M12 7v5l3 2" /></svg>
          <select id="dash-range" value={range} onChange={(e) => setRange(e.target.value)}>
            <option value="15m">Last 15 minutes</option>
            <option value="1h">Last 1 hour</option>
            <option value="6h">Last 6 hours</option>
            <option value="24h">Last 24 hours</option>
            <option value="168h">Last 7 days</option>
          </select>
        </div>
        <button className="chip" id="dash-refresh" onClick={load}>Refresh</button>
      </div>

      <div className="dash" id="dash">
        <div className="tiles" id="dash-tiles">
          {tile("Events", fmtNum(stats?.total), "in this range", undefined, () => onSearch("", range))}
          {tile("Errors", fmtNum(errCount), errCount ? "needs a look" : "all clear", errCount ? "error" : undefined, () =>
            onSearch("level=(error,fatal)", range),
          )}
          {tile("Warnings", fmtNum(warnCount), "", warnCount ? "warn" : undefined, () => onSearch("level=warn", range))}
          {tile("Services", fmtNum(services.length), "reporting")}
          {tile("Alerts firing", fmtNum(firing), `${rules.length} rules`, firing ? "error" : undefined)}
        </div>

        <div className="card">
          <div className="card-head">
            <h3>Events over time</h3>
            <span className="card-link">
              {hist.length ? `${fmtClock(hist[0].start)} – ${fmtClock(hist[hist.length - 1].start)}` : ""}
            </span>
          </div>
          <Histogram
            className="chart chart-lg"
            buckets={hist}
            theme={theme}
            onSelect={(from, to) =>
              onSearch(`from=${from.toISOString()} to=${to.toISOString()}`, range)
            }
          />
        </div>

        <div className="dash-grid">
          <div className="card">
            <div className="card-head">
              <h3>Busiest services</h3>
              <span className="card-link">click to search</span>
            </div>
            <div className="barlist" id="dash-services">
              {services.length === 0 && <span className="hint">No events in this range.</span>}
              {services.slice(0, 8).map((f) => (
                <div key={f.value} className="bl-row" onClick={() => onSearch(`service=${f.value}`, range)}>
                  <div className="bl-top">
                    <span className="bl-name">{f.value || "—"}</span>
                    <span className="bl-count">{fmtNum(f.count)}</span>
                  </div>
                  <div className="bl-bar"><i style={{ width: `${Math.round((f.count / maxSvc) * 100)}%` }} /></div>
                </div>
              ))}
            </div>
          </div>

          <div className="card">
            <div className="card-head"><h3>Alert rules</h3></div>
            <div className="rules" id="dash-alerts">
              {rules.length === 0 && <span className="hint">No alert rules yet.</span>}
              {rules.slice(0, 8).map((r) => (
                <div key={r.id} className="rule">
                  <span className={`rule-state ${r.state ?? "unknown"}`} title={`state: ${r.state ?? "unknown"}`} />
                  <div className="rule-main">
                    <div className="rule-name">{r.name}</div>
                    <div className="rule-query">{r.query}</div>
                  </div>
                </div>
              ))}
            </div>
          </div>
        </div>

        <div className="card">
          <div className="card-head">
            <h3>Recent errors</h3>
            <span className="card-link">click to open in search</span>
          </div>
          <div className="mini-rows" id="dash-errors">
            {errors.length === 0 && <span className="hint">No errors in this range.</span>}
            {errors.map((e) => (
              <div
                key={e.id}
                className={`mini-row lvl-${(e.level ?? "info").toLowerCase()}`}
                onClick={() => onSearch(`level=(error,fatal) service=${e.service ?? ""}`, range)}
              >
                <span className="mini-ts">{fmtTime(e.timestamp).slice(0, 8)}</span>
                <span className="mini-msg" title={e.message}>
                  {e.service || "—"} · {e.message || e.raw || ""}
                </span>
              </div>
            ))}
          </div>
        </div>
      </div>
    </section>
  );
}
