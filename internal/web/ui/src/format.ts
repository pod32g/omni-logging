// Formatting shared across views.

export function fmtNum(n: number | undefined | null): string {
  return (n ?? 0).toLocaleString("en-US");
}

const p2 = (n: number) => String(n).padStart(2, "0");

/** Full local timestamp — used in detail panes and aggregation cells. */
export function fmtTs(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return (
    `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())} ` +
    `${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}.` +
    String(d.getMilliseconds()).padStart(3, "0")
  );
}

/**
 * Rows show a time only. Repeating the date on every one of 200 rows costs a
 * column and says nothing; it moves to a separator when the day changes.
 */
export function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return (
    `${p2(d.getHours())}:${p2(d.getMinutes())}:${p2(d.getSeconds())}.` +
    String(d.getMilliseconds()).padStart(3, "0")
  );
}

export function fmtClock(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  return `${p2(d.getHours())}:${p2(d.getMinutes())}`;
}

export function dayKey(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return `${d.getFullYear()}-${p2(d.getMonth() + 1)}-${p2(d.getDate())}`;
}

export function dayLabel(iso: string): string {
  const key = dayKey(iso);
  const today = dayKey(new Date().toISOString());
  const yesterday = dayKey(new Date(Date.now() - 86400000).toISOString());
  if (key === today) return `Today · ${key}`;
  if (key === yesterday) return `Yesterday · ${key}`;
  return key;
}

export function fmtDuration(sec: number): string {
  if (!sec) return "—";
  if (sec % 3600 === 0) return `${sec / 3600}h`;
  if (sec % 60 === 0) return `${sec / 60}m`;
  return `${sec}s`;
}

/** Aggregation cell: timestamps local, numbers grouped, everything else text. */
export function fmtCell(v: string | number | null): string {
  if (v == null || v === "") return "—";
  if (typeof v === "number") {
    return Number.isInteger(v) ? fmtNum(v) : v.toFixed(2);
  }
  if (/^\d{4}-\d{2}-\d{2}T/.test(v) && !isNaN(new Date(v).getTime())) {
    return fmtTs(v);
  }
  return String(v);
}
