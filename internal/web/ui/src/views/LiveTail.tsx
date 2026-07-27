import { useEffect, useRef, useState } from "react";
import type { LogEvent } from "../types";
import { getToken } from "../api";
import { fmtNum } from "../format";
import { EventList } from "../components/EventList";

const MAX_ROWS = 500;

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

  const esRef = useRef<EventSource | null>(null);
  const epsWindow = useRef<number[]>([]);
  const listRef = useRef<HTMLDivElement>(null);

  // The stream is opened only while the view is visible and unpaused, so
  // leaving the tab does not keep a subscriber (and its buffer) alive server-side.
  useEffect(() => {
    if (!active || paused) {
      esRef.current?.close();
      esRef.current = null;
      return;
    }
    const p = new URLSearchParams();
    if (applied) p.set("q", applied);
    // EventSource cannot set headers, which is why this endpoint alone accepts
    // the token as a parameter.
    const t = getToken();
    if (t) p.set("token", t);

    const es = new EventSource(`/api/v1/tail?${p.toString()}`);
    es.onmessage = (msg) => {
      let e: LogEvent;
      try {
        e = JSON.parse(msg.data) as LogEvent;
      } catch {
        return;
      }
      epsWindow.current.push(Date.now());
      setStreamed((n) => n + 1);
      setEvents((prev) => [e, ...prev].slice(0, MAX_ROWS));
    };
    es.onerror = () => { /* the browser reconnects on its own */ };
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
        <button className={`live${paused ? " paused" : ""}`} id="tail-toggle">
          <i className="dot dot-live" />
          <span>{paused ? "PAUSED" : "LIVE"}</span>
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
