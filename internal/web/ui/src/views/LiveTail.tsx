import { useEffect, useRef, useState } from "react";
import type { LogEvent } from "../types";
import { getToken } from "../api";
import { fmtNum } from "../format";
import { EventList } from "../components/EventList";

const MAX_ROWS = 500;

/** What the stream is actually doing, as opposed to what we wish it were doing. */
type Status = "connecting" | "live" | "reconnecting" | "paused";

interface Props {
  active: boolean;
  onFilter: (term: string) => void;
}

export function LiveTail({ active, onFilter }: Props) {
  const [filter, setFilter] = useState("");
  const [applied, setApplied] = useState("");
  const [paused, setPaused] = useState(false);
  const [events, setEvents] = useState<LogEvent[]>([]);
  const [streamed, setStreamed] = useState(0);
  const [eps, setEps] = useState(0);
  const [autoScroll, setAutoScroll] = useState(true);
  const [status, setStatus] = useState<Status>("connecting");

  const esRef = useRef<EventSource | null>(null);
  const epsWindow = useRef<number[]>([]);
  const listRef = useRef<HTMLDivElement>(null);
  // IDs already on screen. A reconnect can redeliver events — the server
  // resumes from Last-Event-ID, but a proxy that drops the header would replay
  // the whole backfill — and a duplicate id becomes a duplicate React key,
  // which leaves orphaned rows in the DOM and breaks the MAX_ROWS cap.
  const seen = useRef<Set<string>>(new Set());

  // The stream is opened only while the view is visible and unpaused, so
  // leaving the tab does not keep a subscriber (and its buffer) alive server-side.
  useEffect(() => {
    if (!active || paused) {
      esRef.current?.close();
      esRef.current = null;
      setStatus(paused ? "paused" : "connecting");
      return;
    }
    setStatus("connecting");
    const p = new URLSearchParams();
    if (applied) p.set("q", applied);
    // EventSource cannot set headers, which is why this endpoint alone accepts
    // the token as a parameter.
    const t = getToken();
    if (t) p.set("token", t);

    const es = new EventSource(`/api/v1/tail?${p.toString()}`);
    es.onopen = () => setStatus("live");
    es.onmessage = (msg) => {
      let e: LogEvent;
      try {
        e = JSON.parse(msg.data) as LogEvent;
      } catch {
        return;
      }
      // A redelivered event is not new traffic: counting it would inflate the
      // rate and duplicate the row.
      if (e.id && seen.current.has(e.id)) return;
      if (e.id) seen.current.add(e.id);

      setStatus("live");
      epsWindow.current.push(Date.now());
      setStreamed((n) => n + 1);
      setEvents((prev) => {
        const next = [e, ...prev].slice(0, MAX_ROWS);
        // Keep the id set bounded to what is actually on screen, or it grows
        // for the lifetime of the tab.
        if (seen.current.size > MAX_ROWS * 2) {
          seen.current = new Set(next.map((x) => x.id).filter(Boolean) as string[]);
        }
        return next;
      });
    };
    // EventSource retries on its own, but silently — saying "LIVE" through an
    // outage is how a dropped stream looks like no logs rather than no
    // connection.
    es.onerror = () => {
      setStatus(es.readyState === EventSource.CLOSED ? "connecting" : "reconnecting");
    };
    esRef.current = es;
    return () => {
      es.close();
      esRef.current = null;
    };
  }, [active, paused, applied]);

  useEffect(() => {
    const id = setInterval(() => {
      const cutoff = Date.now() - 1000;
      epsWindow.current = epsWindow.current.filter((t) => t >= cutoff);
      setEps(epsWindow.current.length);
    }, 1000);
    return () => clearInterval(id);
  }, []);

  useEffect(() => {
    if (autoScroll && listRef.current) listRef.current.scrollTop = 0;
  }, [events, autoScroll]);

  return (
    <section className="view" id="view-tail">
      <div className="querybar">
        <div className="field">
          <svg className="ico" viewBox="0 0 24 24"><circle cx="11" cy="11" r="7" /><path d="M21 21l-4.3-4.3" /></svg>
          <input
            id="tail-q"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                setEvents([]);
                seen.current = new Set();
                setApplied(filter.trim());
              }
            }}
            autoComplete="off"
            spellCheck={false}
            placeholder="service=checkout-api level!=debug"
          />
          <span className="kbd">enter</span>
        </div>
        <button className="chip" id="tail-pause" onClick={() => setPaused((p) => !p)}>
          <i className="pause" />
          <span>{paused ? "Resume" : "Pause"}</span>
        </button>
        <button
          className={`live${status === "paused" ? " paused" : ""}${status === "reconnecting" || status === "connecting" ? " reconnecting" : ""}`}
          id="tail-toggle"
          title={
            status === "live"
              ? "Connected"
              : status === "paused"
                ? "Paused — the stream is closed"
                : "The connection dropped; retrying"
          }
        >
          <i className="dot dot-live" />
          <span>
            {status === "paused" ? "PAUSED" : status === "live" ? "LIVE" : "RECONNECTING"}
          </span>
        </button>
      </div>

      <div className="stream-stats">
        <div className="stat"><strong id="eps">{fmtNum(eps)}</strong><span>events/sec</span></div>
        <div className="divider" />
        <div className="stat"><strong id="streamed">{fmtNum(streamed)}</strong><span>streamed this session</span></div>
        <div className="spacer" />
        <label className="toggle">
          <span>Auto-scroll</span>
          <input type="checkbox" id="autoscroll" checked={autoScroll} onChange={(e) => setAutoScroll(e.target.checked)} />
          <i className="switch" />
        </label>
      </div>

      <div className="col-header">
        <span>Time</span><span>Level</span><span>Service</span><span>Message</span><span />
      </div>
      <EventList events={events} onFilter={onFilter} listRef={listRef} />
      {events.length === 0 && (
        <div className="empty" id="tail-empty">
          No matching events yet — recent history appears here, then new events stream in live.
        </div>
      )}
    </section>
  );
}
