import { useCallback, useEffect, useState } from "react";
import type { AlertRule, Channel } from "../types";
import { apiGet, apiSend, Unauthorized } from "../api";
import { fmtDuration } from "../format";

const OPS: Record<string, string> = { gt: ">", gte: "≥", lt: "<", lte: "≤", eq: "=", ne: "≠" };

const BLANK: AlertRule = {
  id: "", name: "", query: "level=error", window_seconds: 300, interval_seconds: 60,
  condition: { op: "gt", value: 10 }, severity: "warning", channels: [], enabled: true,
};

interface Props {
  active: boolean;
  onUnauthorized: () => void;
}

export function Alerts({ active, onUnauthorized }: Props) {
  const [rules, setRules] = useState<AlertRule[]>([]);
  const [channels, setChannels] = useState<Channel[]>([]);
  const [editing, setEditing] = useState<AlertRule | null>(null);
  const [editorMsg, setEditorMsg] = useState("");
  const [dryRun, setDryRun] = useState("");
  const [chanMsg, setChanMsg] = useState("");
  const [chan, setChan] = useState({ name: "", type: "webhook", url: "", token: "" });

  const load = useCallback(async () => {
    try {
      const [r, c] = await Promise.all([
        apiGet<{ rules: AlertRule[] | null }>("/api/v1/alerts"),
        apiGet<{ channels: Channel[] | null }>("/api/v1/alerts/channels"),
      ]);
      setRules(r.rules ?? []);
      setChannels(c.channels ?? []);
    } catch (e) {
      if (e instanceof Unauthorized) onUnauthorized();
      else console.error(e);
    }
  }, [onUnauthorized]);

  useEffect(() => { if (active) load(); }, [active, load]);

  async function saveRule() {
    if (!editing) return;
    const method = editing.id ? "PUT" : "POST";
    const path = editing.id ? `/api/v1/alerts/${editing.id}` : "/api/v1/alerts";
    try {
      const res = await apiSend(method, path, editing);
      if (!res.ok) {
        // The server validates the query too, so a bad expression surfaces here
        // rather than failing silently on every interval.
        setEditorMsg((await res.text()).trim());
        return;
      }
      setEditing(null);
      setEditorMsg("");
      setDryRun("");
      load();
    } catch (e) {
      if (!(e instanceof Unauthorized)) setEditorMsg((e as Error).message);
    }
  }

  async function testRule() {
    if (!editing?.id) { setEditorMsg("Save the rule before testing it."); return; }
    try {
      const res = await apiSend("POST", `/api/v1/alerts/${editing.id}/test`);
      setDryRun(JSON.stringify(await res.json(), null, 2));
    } catch (e) {
      if (!(e instanceof Unauthorized)) setEditorMsg((e as Error).message);
    }
  }

  async function addChannel() {
    const body: Record<string, string> = { name: chan.name.trim(), type: chan.type, url: chan.url.trim() };
    if (chan.token.trim()) body.token = chan.token.trim();
    try {
      const res = await apiSend("POST", "/api/v1/alerts/channels", body);
      if (!res.ok) { setChanMsg((await res.text()).trim()); return; }
      setChan({ name: "", type: chan.type, url: "", token: "" });
      setChanMsg("Added.");
      load();
    } catch (e) {
      if (!(e instanceof Unauthorized)) setChanMsg((e as Error).message);
    }
  }

  async function testChannel(c: Channel) {
    setChanMsg("Sending…");
    try {
      const res = await apiSend("POST", `/api/v1/alerts/channels/${c.id}/test`);
      const body = (await res.json()) as { ok: boolean; error?: string };
      setChanMsg(body.ok ? `Delivered to ${c.name}.` : `Failed: ${body.error}`);
    } catch (e) {
      if (!(e instanceof Unauthorized)) setChanMsg((e as Error).message);
    }
  }

  const isNotify = chan.type === "omni-notify";
  const set = (patch: Partial<AlertRule>) => setEditing((r) => (r ? { ...r, ...patch } : r));

  return (
    <section className="view" id="view-alerts">
      <div className="settings">
        <div className="settings-col">
          <div className="card">
            <h3>Alert rules</h3>
            <p className="hint">
              A rule is a query, a window and a threshold. Any search works, including an aggregation
              like <code>level=error | stats count by service</code>. Notifications are sent when a
              rule changes state, not on every check.
            </p>
            <div className="rules" id="al-rules">
              {rules.length === 0 && <span className="hint">No alert rules yet.</span>}
              {rules.map((r) => (
                <div key={r.id} className="rule">
                  <span className={`rule-state ${r.state ?? "unknown"}`} title={`state: ${r.state ?? "unknown"}`} />
                  <div className="rule-main">
                    <div className="rule-name">
                      {r.name}
                      {!r.enabled && <span className="rule-off">disabled</span>}
                    </div>
                    <div className="rule-query">{r.query}</div>
                    <div className="rule-meta">
                      {OPS[r.condition.op]} {r.condition.value} · window {fmtDuration(r.window_seconds)} ·
                      every {fmtDuration(r.interval_seconds)} · {r.severity ?? "warning"} · last {r.last_value ?? 0}
                    </div>
                    {r.last_error && <div className="rule-err">{r.last_error}</div>}
                  </div>
                  <div className="rule-actions">
                    <button className="chip" onClick={() => { setEditing(r); setDryRun(""); setEditorMsg(""); }}>Edit</button>
                    <button
                      className="chip"
                      onClick={async () => {
                        await apiSend("DELETE", `/api/v1/alerts/${r.id}`);
                        load();
                      }}
                    >
                      Delete
                    </button>
                  </div>
                </div>
              ))}
            </div>
            <div className="set-actions">
              <button className="chip" id="al-new" onClick={() => { setEditing({ ...BLANK }); setDryRun(""); setEditorMsg(""); }}>
                New rule
              </button>
            </div>
          </div>

          {editing && (
            <div className="card" id="al-editor">
              <h3 id="al-editor-title">{editing.id ? "Edit rule" : "New rule"}</h3>
              <div className="set-row">
                <label htmlFor="al-name">Name</label>
                <input id="al-name" value={editing.name} onChange={(e) => set({ name: e.target.value })} placeholder="error spike" />
              </div>
              <div className="set-row">
                <label htmlFor="al-query">Query</label>
                <input id="al-query" className="mono" value={editing.query} onChange={(e) => set({ query: e.target.value })} />
              </div>
              <div className="set-row">
                <label htmlFor="al-window">Window (seconds) — how far back each check looks</label>
                <input id="al-window" type="number" min={10} value={editing.window_seconds}
                       onChange={(e) => set({ window_seconds: parseInt(e.target.value, 10) || 0 })} />
              </div>
              <div className="set-row">
                <label htmlFor="al-interval">Interval (seconds) — how often to check</label>
                <input id="al-interval" type="number" min={10} value={editing.interval_seconds}
                       onChange={(e) => set({ interval_seconds: parseInt(e.target.value, 10) || 0 })} />
              </div>
              <div className="set-row">
                <label>Fire when the value is</label>
                <div className="cond-row">
                  <select id="al-op" value={editing.condition.op}
                          onChange={(e) => set({ condition: { ...editing.condition, op: e.target.value } })}>
                    {Object.entries(OPS).map(([k, v]) => <option key={k} value={k}>{v}</option>)}
                  </select>
                  <input id="al-value" type="number" step="any" value={editing.condition.value}
                         onChange={(e) => set({ condition: { ...editing.condition, value: parseFloat(e.target.value) || 0 } })} />
                </div>
              </div>
              <div className="set-row">
                <label htmlFor="al-severity">Severity</label>
                <select id="al-severity" value={editing.severity ?? "warning"} onChange={(e) => set({ severity: e.target.value })}>
                  {["critical", "error", "warning", "info", "debug"].map((s) => <option key={s} value={s}>{s}</option>)}
                </select>
              </div>
              <div className="set-row">
                <label>Notify these channels</label>
                <div className="keys" id="al-channels">
                  {channels.length === 0 && <span className="hint">Add a channel to be notified.</span>}
                  {channels.map((c) => (
                    <span
                      key={c.id}
                      className={`chan-pick${editing.channels.includes(c.id) ? " is-on" : ""}`}
                      onClick={() =>
                        set({
                          channels: editing.channels.includes(c.id)
                            ? editing.channels.filter((x) => x !== c.id)
                            : [...editing.channels, c.id],
                        })
                      }
                    >
                      {c.name}
                    </span>
                  ))}
                </div>
              </div>
              <div className="set-row">
                <label htmlFor="al-enabled">Enabled</label>
                <input id="al-enabled" type="checkbox" checked={editing.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
              </div>
              <div className="set-actions">
                <button className="btn-primary" id="al-save" onClick={saveRule}>Save rule</button>
                <button className="chip" id="al-dryrun" onClick={testRule}>Test now</button>
                <button className="chip" id="al-cancel" onClick={() => setEditing(null)}>Cancel</button>
                <span className="cfg-msg err" id="al-editor-msg">{editorMsg}</span>
              </div>
              {dryRun && <pre className="dryrun" id="al-dryrun-out">{dryRun}</pre>}
            </div>
          )}
        </div>

        <div className="settings-col">
          <div className="card">
            <h3>Notification channels</h3>
            <p className="hint">
              A <strong>webhook</strong> receives the full JSON payload; <strong>slack</strong> receives
              a rendered <code>{'{"text"}'}</code>; <strong>omni-notify</strong> posts an event to an{" "}
              <a href="https://github.com/pod32g/omni-notify">Omni-Notify</a> server, which handles
              deduplication, routing and delivery to Discord/Telegram/SMTP. The server makes outbound
              requests to these URLs.
            </p>
            <div className="chans" id="al-chan-list">
              {channels.map((c) => (
                <div key={c.id} className="chan">
                  <span className="chan-type">{c.type}</span>
                  <div className="chan-main">
                    <div className="chan-name">
                      {c.name}
                      {c.token && <span className="chan-auth">🔒 authenticated</span>}
                    </div>
                    <div className="chan-url">{c.url}</div>
                  </div>
                  <div className="rule-actions">
                    <button className="chip" onClick={() => testChannel(c)}>Test</button>
                    <button
                      className="chip"
                      onClick={async () => { await apiSend("DELETE", `/api/v1/alerts/channels/${c.id}`); load(); }}
                    >
                      Delete
                    </button>
                  </div>
                </div>
              ))}
            </div>
            <div className="set-row">
              <label htmlFor="al-chan-name">Name</label>
              <input id="al-chan-name" value={chan.name} onChange={(e) => setChan({ ...chan, name: e.target.value })} placeholder="ops" />
            </div>
            <div className="set-row">
              <label htmlFor="al-chan-type">Type</label>
              <select id="al-chan-type" value={chan.type} onChange={(e) => setChan({ ...chan, type: e.target.value })}>
                <option value="webhook">webhook</option>
                <option value="slack">slack</option>
                <option value="omni-notify">omni-notify</option>
              </select>
            </div>
            <div className="set-row">
              <label htmlFor="al-chan-url">URL</label>
              <input
                id="al-chan-url" className="mono" value={chan.url}
                onChange={(e) => setChan({ ...chan, url: e.target.value })}
                placeholder={isNotify ? "http://omni-notify:8088" : "https://hooks.slack.com/services/..."}
              />
            </div>
            <div className="set-row" id="al-chan-token-row">
              <label htmlFor="al-chan-token">Token</label>
              <input
                id="al-chan-token" type="password" className="mono" value={chan.token}
                onChange={(e) => setChan({ ...chan, token: e.target.value })}
                placeholder={isNotify ? "OMNI_NOTIFY_API_TOKEN (required)" : "bearer token (optional)"}
              />
            </div>
            {isNotify && (
              <p className="hint" id="al-chan-token-hint">
                Sent as <code>Authorization: Bearer</code>. Omni-Notify requires one — its{" "}
                <code>OMNI_NOTIFY_API_TOKEN</code>. Stored server-side and never read back.
              </p>
            )}
            <div className="set-actions">
              <button className="btn-primary" id="al-chan-add" onClick={addChannel}>Add channel</button>
              <span className="cfg-msg" id="al-chan-msg">{chanMsg}</span>
            </div>
          </div>
        </div>
      </div>
    </section>
  );
}
