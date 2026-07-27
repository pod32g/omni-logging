// Shapes returned by the omnilog API. Only the fields the UI actually reads are
// declared; the server may send more.

export type Level = "fatal" | "error" | "warn" | "info" | "debug";

export interface LogEvent {
  id: string;
  timestamp: string;
  received_at?: string;
  source?: string;
  service?: string;
  level?: string;
  message?: string;
  raw?: string;
  attributes?: Record<string, unknown>;
}

export interface SearchResult {
  events: LogEvent[] | null;
  count: number;
  total: number;
  total_capped?: boolean;
  next_cursor?: string;
  took_ms?: number;
}

export interface Bucket {
  start: string;
  count: number;
}

export interface Facet {
  value: string;
  count: number;
}

export interface StatsResult {
  total: number;
  took_ms?: number;
  histogram: Bucket[] | null;
  facets: { level?: Facet[]; service?: Facet[] } | null;
}

export interface AggResult {
  columns: string[];
  rows: (string | number | null)[][] | null;
  group_columns: number;
  truncated?: boolean;
  took_ms?: number;
}

export type RuleState = "ok" | "firing" | "unknown";

export interface AlertRule {
  id: string;
  name: string;
  query: string;
  window_seconds: number;
  interval_seconds: number;
  condition: { op: string; value: number };
  severity?: string;
  channels: string[];
  enabled: boolean;
  state?: RuleState;
  last_value?: number;
  last_error?: string;
}

export interface Channel {
  id: string;
  name: string;
  type: "webhook" | "slack" | "omni-notify";
  url: string;
  token?: string;
}

export interface ServerConfig {
  retention_days?: number;
  rate_limit_per_sec?: number;
  rate_burst?: number;
  daily_quota_events?: number;
  daily_quota_bytes?: number;
  log_level?: string;
  ingest_keys?: string[];
}

export interface ServerStatus {
  version?: string;
  subscribers?: number;
  ingest?: Record<string, number>;
}

export type ViewName = "dash" | "search" | "tail" | "alerts" | "settings";
