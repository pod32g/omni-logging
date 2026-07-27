import { Fragment } from "react";
import type { LogEvent } from "../types";
import { dayKey, dayLabel } from "../format";
import { EventRow } from "./EventRow";

interface Props {
  events: LogEvent[];
  cursorIdx?: number;
  wrap?: boolean;
  onFilter: (term: string) => void;
  listRef?: React.Ref<HTMLDivElement>;
}

/** The dense event list, with a date separator whenever the day changes. */
export function EventList({ events, cursorIdx, wrap, onFilter, listRef }: Props) {
  let lastDay = "";
  return (
    <div className={`rows${wrap ? " wrap" : ""}`} ref={listRef} id="rows">
      {events.map((e, i) => {
        const day = dayKey(e.timestamp);
        const sep = day && day !== lastDay ? dayLabel(e.timestamp) : null;
        lastDay = day || lastDay;
        return (
          <Fragment key={e.id || `${e.timestamp}-${i}`}>
            {sep && <div className="day-sep">{sep}</div>}
            <EventRow event={e} isCursor={i === cursorIdx} onFilter={onFilter} />
          </Fragment>
        );
      })}
    </div>
  );
}
