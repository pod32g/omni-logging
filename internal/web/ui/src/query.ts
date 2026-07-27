// Client-side reading of the query expression.
//
// None of this decides anything — the server parses the expression and its
// answer is authoritative. These helpers only drive UI affordances, which is
// why they can afford to be approximate.

export const LEVELS = ["fatal", "error", "warn", "info", "debug"] as const;

/**
 * Does the query set its own time range? The leading (^|\s) is what keeps
 * attr.last= out of it: there the key is preceded by a dot, not a boundary.
 */
export function hasTimeDirective(q: string): boolean {
  return /(^|\s)(last|from|to)=/i.test(q);
}

/** Does the query carry an aggregation stage? A '|' inside quotes is a value. */
export function hasPipeline(q: string): boolean {
  let inQuote = false;
  for (const ch of q) {
    if (ch === '"') inQuote = !inQuote;
    else if (ch === "|" && !inQuote) return true;
  }
  return false;
}

export function includesTerm(q: string, term: string): boolean {
  return q.split(/\s+/).includes(term);
}

export function addTerm(q: string, term: string): string {
  return includesTerm(q, term) ? q : `${q} ${term}`.trim();
}

/** A filter chip is a toggle, so clicking an active one removes it. */
export function toggleTerm(q: string, term: string): string {
  const parts = q.split(/\s+/).filter(Boolean);
  const i = parts.indexOf(term);
  if (i >= 0) parts.splice(i, 1);
  else parts.push(term);
  return parts.join(" ");
}

/** Replace any time bounds with an explicit window. */
export function withRange(q: string, from: Date, to: Date): string {
  const parts = q.split(/\s+/).filter((t) => t && !/^(last|from|to)=/i.test(t));
  parts.push(`from=${from.toISOString()}`, `to=${to.toISOString()}`);
  return parts.join(" ");
}

/** Fill gaps so the histogram is contiguous rather than a few wide blocks. */
export function fillBuckets<T extends { start: string; count: number }>(hist: T[]): { start: string; count: number }[] {
  if (hist.length < 2) return hist.map((b) => ({ start: b.start, count: b.count }));
  const starts = hist.map((b) => new Date(b.start).getTime());
  let step = Infinity;
  for (let i = 1; i < starts.length; i++) {
    step = Math.min(step, starts[i] - starts[i - 1]);
  }
  if (!isFinite(step) || step <= 0) {
    return hist.map((b) => ({ start: b.start, count: b.count }));
  }
  const counts = new Map(starts.map((t, i) => [t, hist[i].count]));
  const out: { start: string; count: number }[] = [];
  const end = starts[starts.length - 1];
  for (let t = starts[0]; t <= end && out.length < 1000; t += step) {
    out.push({ start: new Date(t).toISOString(), count: counts.get(t) ?? 0 });
  }
  return out;
}
