import { useCallback, useEffect, useState } from "react";
import type { ServerConfig, ServerStatus } from "../types";
import { apiGet, apiSend, getToken, setToken, Unauthorized } from "../api";
import { fmtNum } from "../format";

const KEYS_HELP: [string, string][] = [
  ["⌘K", "Command palette"],
  ["/", "Focus the search box"],
  ["j / k", "Move down / up the results"],
  ["enter", "Expand the selected event"],
  ["g then o s t a", "Go to overview, search, tail, alerts"],
  ["esc", "Close the palette or collapse a row"],
];

interface Props {
  active: boolean;
  theme: string;
  onTheme: (t: string) => void;
  onUnauthorized: () => void;
}

export function Settings({ active, theme, onTheme, onUnauthorized }: Props) {
  const [cfg, setCfg] = useState<ServerConfig>({});
  const [keys, setKeys] = useState<string[]>([]);
  const [newKey, setNewKey] = useState("");
  const [status, setStatus] = useState<ServerStatus | null>(null);
  const [msg, setMsg] = useState("");
  const [msgOk, setMsgOk] = useState(true);
  const [adminToken, setAdminToken] = useState(getToken());

  const load = useCallback(async () => {
    setAdminToken(getToken());
    try {
      const c = await apiGet<ServerConfig>("/api/v1/config");
      setCfg(c);
      setKeys(c.ingest_keys ?? []);
    } catch (e) {
      if (e instanceof Unauthorized) onUnauthorized();
      else console.error(e);
    }
    try {
      // Counters come from /api/v1/status, not the liveness probe: /healthz
      // deliberately reports nothing but {"status":"ok"} so an unauthenticated
      // caller cannot read traffic volumes off it.
      setStatus(await apiGet<ServerStatus>("/api/v1/status"));
    } catch {
      /* status is best-effort */
    }
  }, [onUnauthorized]);

  useEffect(() => { if (active) load(); }, [active, load]);

  async function save() {
    try {
      const res = await apiSend("PUT", "/api/v1/config", { ...cfg, ingest_keys: keys });
      if (!res.ok) {
        setMsg("Error: " + (await res.text()).trim());
        setMsgOk(false);
        return;
      }
      const saved = (await res.json()) as ServerConfig;
      setCfg(saved);
      setKeys(saved.ingest_keys ?? []);
      setMsg("Saved.");
      setMsgOk(true);
      setTimeout(() => setMsg(""), 2500);
    } catch (e) {
      if (!(e instanceof Unauthorized)) { setMsg("Error: " + (e as Error).message); setMsgOk(false); }
    }
  }

  const num = (k: keyof ServerConfig, label: string, step?: string) => (
    <div className="set-row">
      <label htmlFor={`cfg-${k}`}>{label}</label>
      <input
        id={`cfg-${k}`} type="number" min={0} step={step}
        value={(cfg[k] as number) ?? 0}
        onChange={(e) =>
          setCfg({ ...cfg, [k]: step ? parseFloat(e.target.value) || 0 : parseInt(e.target.value, 10) || 0 })
        }
      />
    </div>
  );

  return (
    <section className="view" id="view-settings">
      <div className="settings">
        <div className="settings-col">
          <div className="card">
            <h3>Appearance</h3>
            <div className="set-row">
              <label>Theme</label>
              <div className="seg" id="theme-seg">
                {["light", "dark", "system"].map((t) => (
                  <button key={t} className={theme === t ? "is-on" : ""} data-theme-set={t} onClick={() => onTheme(t)}>
                    {t[0].toUpperCase() + t.slice(1)}
                  </button>
                ))}
              </div>
            </div>
          </div>

          <div className="card">
            <h3>Server configuration</h3>
            <p className="hint">Changes apply live and persist across restarts.</p>
            {num("retention_days", "Retention (days, 0 = keep forever)")}
            {num("rate_limit_per_sec", "Rate limit (requests/sec per key, 0 = off)", "any")}
            {num("rate_burst", "Rate burst")}
            {num("daily_quota_events", "Daily event quota per key (0 = off)")}
            {num("daily_quota_bytes", "Daily byte quota per key (0 = off)")}
            <div className="set-row">
              <label htmlFor="cfg-loglevel">Log level</label>
              <select id="cfg-loglevel" value={cfg.log_level ?? "info"} onChange={(e) => setCfg({ ...cfg, log_level: e.target.value })}>
                {["debug", "info", "warn", "error"].map((l) => <option key={l} value={l}>{l}</option>)}
              </select>
            </div>
            <div className="set-row">
              <label>Ingest keys</label>
              <div className="keys" id="cfg-keys">
                {keys.length === 0 && <span className="hint">No ingest keys — ingestion is open (dev mode).</span>}
                {keys.map((k, i) => (
                  <span key={k} className="key-chip">
                    <code>{k}</code>
                    <button className="key-x" title="Remove key" onClick={() => setKeys(keys.filter((_, j) => j !== i))}>×</button>
                  </span>
                ))}
              </div>
            </div>
            <div className="set-row add-key">
              <input
                id="cfg-key-new" value={newKey} placeholder="add an ingest key"
                onChange={(e) => setNewKey(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    e.preventDefault();
                    if (newKey.trim() && !keys.includes(newKey.trim())) setKeys([...keys, newKey.trim()]);
                    setNewKey("");
                  }
                }}
              />
              <button
                className="chip" id="cfg-key-add"
                onClick={() => {
                  if (newKey.trim() && !keys.includes(newKey.trim())) setKeys([...keys, newKey.trim()]);
                  setNewKey("");
                }}
              >
                Add
              </button>
            </div>
            <div className="set-actions">
              <button className="btn-primary" id="cfg-save" onClick={save}>Save changes</button>
              <span className={`cfg-msg ${msgOk ? "ok" : "err"}`} id="cfg-msg">{msg}</span>
            </div>
          </div>
        </div>

        <div className="settings-col">
          <div className="card">
            <h3>Connection</h3>
            <p className="hint">
              The admin token is stored in your browser to authenticate this UI — it is not a server
              setting and cannot be changed here.
            </p>
            <div className="set-row">
              <label htmlFor="cfg-admintoken">Admin token</label>
              <input id="cfg-admintoken" type="password" value={adminToken} placeholder="admin token"
                     onChange={(e) => setAdminToken(e.target.value)} />
            </div>
            <div className="set-actions">
              <button className="chip" id="cfg-token-save" onClick={() => { setToken(adminToken.trim()); load(); }}>
                Save token
              </button>
            </div>
          </div>

          <div className="card">
            <h3>Keyboard</h3>
            <div className="keys-help">
              {KEYS_HELP.map(([k, d]) => (
                <div key={k} style={{ display: "contents" }}>
                  <span className="kbd">{k}</span>
                  <span>{d}</span>
                </div>
              ))}
            </div>
          </div>

          <div className="card">
            <h3>Server status</h3>
            <div className="status-grid" id="cfg-status">
              {!status && <span className="hint">Loading…</span>}
              {status && (
                <>
                  <div className="st-k">Version</div><div className="st-v">{status.version ?? "—"}</div>
                  <div className="st-k">Subscribers</div><div className="st-v">{status.subscribers ?? 0}</div>
                  {status.ingest &&
                    Object.entries(status.ingest).map(([k, v]) => (
                      <div key={k} style={{ display: "contents" }}>
                        <div className="st-k">{k[0].toUpperCase() + k.slice(1)}</div>
                        <div className="st-v">{fmtNum(v)}</div>
                      </div>
                    ))}
                </>
              )}
            </div>
            <div className="set-links">
              <a href="/docs" target="_blank" rel="noopener">API docs</a> ·{" "}
              <a href="/metrics" target="_blank" rel="noopener">Metrics</a> ·{" "}
              <a href="/openapi.json" target="_blank" rel="noopener">OpenAPI</a>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}
