// A thin client over the omnilog API.
//
// The admin token lives in localStorage and travels in the Authorization
// header — never in a query string, which would leak it into browser history,
// Referer headers and proxy access logs. The one exception is the live-tail
// EventSource, which cannot set headers; that is why /api/v1/tail accepts a
// token parameter and nothing else does.

export class Unauthorized extends Error {
  constructor() {
    super("unauthorized");
    this.name = "Unauthorized";
  }
}

export function getToken(): string {
  try {
    return localStorage.getItem("omnilog_token") ?? "";
  } catch {
    return "";
  }
}

export function setToken(t: string) {
  try {
    localStorage.setItem("omnilog_token", t);
  } catch {
    /* private browsing; the session simply will not persist */
  }
}

function authHeaders(extra?: Record<string, string>): Record<string, string> {
  const h: Record<string, string> = { ...extra };
  const t = getToken();
  if (t) h["Authorization"] = "Bearer " + t;
  return h;
}

export async function apiGet<T>(path: string): Promise<T> {
  const res = await fetch(path, { headers: authHeaders() });
  if (res.status === 401) throw new Unauthorized();
  if (!res.ok) throw new Error(`request failed: ${res.status}`);
  return (await res.json()) as T;
}

/** Returns the raw Response so callers can read validation error bodies. */
export async function apiSend(
  method: string,
  path: string,
  body?: unknown,
): Promise<Response> {
  const res = await fetch(path, {
    method,
    headers: authHeaders({ "Content-Type": "application/json" }),
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 401) throw new Unauthorized();
  return res;
}

/**
 * Download all matches. The response is buffered and saved from a Blob URL
 * rather than navigating an <a> at the endpoint, which is what keeps the token
 * in a header instead of the URL.
 */
export async function download(url: string, filename: string): Promise<void> {
  const res = await fetch(url, { headers: authHeaders() });
  if (res.status === 401) throw new Unauthorized();
  if (!res.ok) throw new Error(`export failed: ${res.status}`);
  const blob = await res.blob();
  const objURL = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = objURL;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Deferred: revoking synchronously can cancel the download in some browsers.
  setTimeout(() => URL.revokeObjectURL(objURL), 0);
}

export interface SearchParams {
  q: string;
  range: string;
  order: string;
}

export function searchURL(base: string, p: SearchParams): string {
  const u = new URLSearchParams();
  if (p.q) u.set("q", p.q);
  if (p.range) u.set("last", p.range);
  if (p.order) u.set("order", p.order);
  return `${base}?${u.toString()}`;
}

/** Histogram bucket width appropriate to the selected range. */
export function bucketFor(range: string): string {
  switch (range) {
    case "15m": return "30s";
    case "1h": return "1m";
    case "6h": return "5m";
    case "24h": return "30m";
    case "168h": return "6h";
    default: return "1h";
  }
}
