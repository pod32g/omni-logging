import { useState } from "react";
import type { LogEvent } from "../types";
import { fmtTime } from "../format";

interface Props {
  event: LogEvent;
  isCursor?: boolean;
  onFilter: (term: string) => void;
}

export function EventRow({ event, isCursor, onFilter }: Props) {
  const [open, setOpen] = useState(false);
  const lvl = (event.level ?? "info").toLowerCase();
  const message = event.message || event.raw || "";
  const meta: Record<string, unknown> = { source: event.source, ...(event.attributes ?? {}) };

  return (
    <div
      className={`row lvl-${lvl}${open ? " open" : ""}${isCursor ? " is-cursor" : ""}`}
      data-testid="event-row"
    >
      <div className="row-line" onClick={() => setOpen((o) => !o)}>
        <span className="row-ts">{fmtTime(event.timestamp)}</span>
        <span className="row-level">{lvl}</span>
        <span className="row-svc" title={event.service}>{event.service || "—"}</span>
        <span className="row-msg" title={message}>{message}</span>
        <svg className="chev" viewBox="0 0 24 24"><path d="M18 15l-6-6-6 6" /></svg>
      </div>
      {open && (
        <div className="row-detail">
          <div className="attr-chips">
            {Object.entries(meta)
              .filter(([, v]) => v != null && v !== "")
              .map(([k, v]) => (
                <span
                  key={k}
                  className="attr-chip"
                  title="Filter by this"
                  onClick={(e) => {
                    e.stopPropagation();
                    // Clicking narrows the search, which is the reason to have
                    // expanded the row in the first place.
                    onFilter(`${k === "source" ? "source" : "attr." + k}=${String(v)}`);
                  }}
                >
                  <b>{k}=</b>
                  {String(v)}
                </span>
              ))}
          </div>
          <div className="json-block">
            <pre>{JSON.stringify(event, null, 2)}</pre>
          </div>
        </div>
      )}
    </div>
  );
}
