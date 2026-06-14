# Omni-logging — v1 Design Spec

**Date:** 2026-06-14
**Repository:** `git@github.com:pod32g/omni-logging.git`
**Status:** Approved baseline for v1 implementation

## 1. Summary

Omni-logging is a self-contained centralized log platform, conceptually similar to
Splunk. Applications (or a small bundled forwarder) ship logs over HTTP; logs are
stored and full-text indexed in SQLite; users search, filter, aggregate, and
live-tail through a web UI and a JSON API.

It ships as a **single Go binary** (`omnilog`) with the web UI embedded, so it
runs anywhere with **zero external services**.

## 2. Goals & non-goals

### v1 goals
- Ingest structured/unstructured logs over HTTP at reasonable throughput.
- Durable storage with a full-text index and time/field indexes.
- A query language supporting free-text, field filters, and time ranges.
- Aggregations for a timeline histogram and field facets.
- Real-time live tail.
- A web UI for search + live tail.
- A minimal file-tailing forwarder so "centralized" collection is real.
- Minimal auth: per-source ingest API keys + an admin token for query/UI.

### Explicit non-goals for v1 (designed-for, not built)
- Alerting and notifications.
- Saved searches and chart dashboards.
- Clustering / HA / sharding.
- Multi-tenant RBAC and user management.
- Ingest-time regex parsing / field-extraction pipelines (v1 accepts structured
  JSON; unstructured lines are stored whole and full-text searchable).
- TLS termination is optional; production deployments may sit behind a proxy.

## 3. Architecture

Single binary with cobra subcommands: `serve`, `forward`, `version`.

```
 sources / apps ─┐
 omnilog forward ─┼─ HTTP POST /api/v1/ingest (API key) ─▶ [ingest: validate → normalize → buffered channel]
                  │                                               │
                                                          [batch writer] ─▶ SQLite (logs + FTS5)
                                                                 │  └────────▶ [tail hub] ─SSE─▶ Web UI live tail
 Web UI / clients ─ GET /api/v1/search, /search/stats, /tail ─▶ [query engine] ─▶ SQLite
```

### Data flow
1. A source POSTs a batch of events (NDJSON or JSON array) with an API key.
2. The ingest handler validates and normalizes each record to a `LogEvent` and
   pushes it onto a buffered channel. Malformed records are reported per-record;
   if the buffer is full the request is rejected with `429` (never silently
   dropped).
3. A batch writer drains the channel and writes events to SQLite inside a
   transaction (structured columns + FTS5 row), then publishes each event to the
   tail hub.
4. The query engine answers `/search` and `/search/stats` from SQLite.
5. UI clients subscribe to `/tail` (SSE) for matching new events.

## 4. Components

Each component lives behind a clear interface so it can be understood, tested,
and replaced independently.

### 4.1 Canonical model (`internal/model`)
```go
type Level string // debug, info, warn, error, fatal (normalized)

type LogEvent struct {
    ID         string            // ULID, server-assigned
    Timestamp  time.Time         // event time (client-supplied, defaults to received_at)
    ReceivedAt time.Time         // server receipt time
    Source     string            // host / origin
    Service    string            // logical service name
    Level      Level
    Message    string            // human-readable message / raw line
    Attributes map[string]any    // arbitrary structured key-values
    Raw        string            // original payload when unstructured
}
```
Severity parsing normalizes common spellings (`WARNING`→`warn`, `err`/`error`,
numeric syslog levels, etc.). Unknown levels default to `info`.

### 4.2 Ingestion (`internal/ingest`)
- `POST /api/v1/ingest` — `Content-Type: application/x-ndjson` or
  `application/json` (array). Each line/element is a JSON object mapped onto
  `LogEvent` (unknown keys fold into `Attributes`).
- `POST /api/v1/ingest/raw` — `text/plain`; each line becomes a `LogEvent` with
  `Message`/`Raw` set, `service`/`source` from query params or headers.
- Validation: timestamp parsing (RFC3339 / unix), size limits, max batch size.
- Response: `{ "accepted": N, "rejected": M, "errors": [{index, reason}] }`.
- Backpressure: bounded channel; full → `429 Too Many Requests`.

### 4.3 Store (`internal/store`, impl `internal/store/sqlite`)
Interface:
```go
type Store interface {
    Append(ctx, []LogEvent) error
    Search(ctx, Query) (SearchResult, error)
    Stats(ctx, Query) (StatsResult, error)   // histogram + facets
    Purge(ctx, olderThan time.Time) (int64, error)
    Close() error
}
```
SQLite schema:
- `logs(id TEXT PK, ts INTEGER, received_at INTEGER, source TEXT, service TEXT,
  level TEXT, message TEXT, attributes TEXT /*json*/, raw TEXT)`
- `logs_fts` — FTS5 virtual table over `message` + flattened `attributes`,
  contentless/external-content linked to `logs` by rowid.
- Indexes on `ts`, `service`, `level`, `source`.
- Pragmas: WAL journaling, `synchronous=NORMAL`, busy timeout.
- Writes are batched in a single transaction per drain cycle for throughput.
- A background retention job calls `Purge` on an interval (configurable days).

### 4.4 Query engine (`internal/query`)
A small query language parsed into a `Query` struct:
- Free-text terms → FTS5 `MATCH`.
- Field filters: `level=error`, `service=api`, `source=host1`,
  `attr.user_id=42`. Supports `!=` and quoted values.
- Time range: absolute `from`/`to` (RFC3339/unix) or relative `last=15m`
  (`s/m/h/d`).
- `limit` (default 100, capped), `order` (newest/oldest).

Endpoints:
- `GET /api/v1/search?q=...&from=...&to=...&limit=...&order=...`
  → `{ events: [...], count, took_ms }`.
- `GET /api/v1/search/stats?q=...&interval=1m` →
  `{ histogram: [{bucket_ts, count}], facets: { level: {...}, service: {...} } }`.

Parser is pure and table-driven-testable; execution maps the `Query` to
parameterized SQL (no string interpolation of user input).

### 4.5 Live tail (`internal/tail`)
- In-memory pub/sub hub. Each subscriber registers a compiled filter (reuses the
  query matcher).
- `GET /api/v1/tail?q=...` → `text/event-stream` (SSE). Server pushes each newly
  ingested matching event. Heartbeat comments keep the connection alive.
- Slow consumers are dropped (bounded per-subscriber buffer) rather than blocking
  ingestion.

### 4.6 API layer (`internal/api`)
- `net/http` + a lightweight router (chi or stdlib `http.ServeMux` with Go 1.22+
  patterns — default to stdlib to avoid deps).
- Middleware: request logging, panic recovery, API-key auth (ingest),
  admin-token/basic auth (query + UI), CORS for local dev.
- Serves the embedded web UI at `/` and the JSON API under `/api/v1`.

### 4.7 Web UI (`web/` source → embedded in `internal/web`)
- React + Vite, built to static assets, embedded via Go `embed`.
- Pages/views:
  - **Search**: query bar, time-range picker, results table with expandable JSON
    rows, a counts-over-time histogram, and level/service facets.
  - **Live tail**: streaming view with the same query bar and pause/clear.
- Talks only to the JSON API. Designed in Paper first, then implemented to match.

### 4.8 Forwarder (`internal/forward`, `omnilog forward`)
- `omnilog forward --file /path/log --service x --server http://host:port --api-key K`
- Tails one or more files (follows rotation), batches lines, POSTs to
  `/api/v1/ingest/raw`. Backoff + retry on failure; resumes from last offset.

### 4.9 Config (`internal/config`)
- Layered: defaults → config file (YAML) → env vars → flags.
- Keys: listen address, DB path, retention days, ingest buffer size, batch size/
  flush interval, admin token, ingest API keys, TLS cert/key (optional).

## 5. Error handling
- **Ingest:** per-record validation errors returned in the batch response;
  buffer-full → `429`; malformed body → `400`. No silent drops.
- **Store:** transactional batch writes; failures bubble up and are logged with
  context; WAL enables concurrent reads during writes.
- **Query:** parser returns precise errors (`400`); result size bounded by
  `limit`; very large time scans are allowed but limited by `limit`/pagination.
- **Tail:** per-subscriber bounded buffers; slow clients dropped, never block
  ingestion.
- **Auth:** `401` on missing/invalid credentials; constant-time key comparison.

## 6. Testing strategy (TDD)
- Unit (table-driven): query parser, severity/timestamp normalization, filter
  matcher, config layering.
- Store: against a temp on-disk SQLite DB — append/search/stats/purge, FTS
  behavior, ordering, limits.
- Integration: ingest → search round trip; ingest → tail broadcast received.
- API: handlers via `net/http/httptest`, including auth failures and partial
  ingest.
- Forwarder: tail a temp file, assert POSTed payloads via a stub server.

## 7. Repo layout
```
cmd/omnilog/main.go            # entry + cobra subcommands (serve, forward, version)
internal/
  config/                      # layered config
  model/                       # LogEvent, Level, Query types
  ingest/                      # HTTP ingest handlers, validation, buffer
  store/                       # Store interface + shared types
    sqlite/                    # SQLite + FTS5 implementation
  query/                       # query parser + matcher
  tail/                        # pub/sub hub + SSE handler
  api/                         # router, middleware, wiring
  web/                         # go:embed of built UI (dist)
  forward/                     # file-tailing forwarder client
web/                           # React + Vite UI source
docs/                          # spec + docs
testdata/                      # sample logs / fixtures
README.md  Makefile  Dockerfile  go.mod  .gitignore
```

## 8. Milestone definition (v1 "done")
- `omnilog serve` runs; UI loads; `omnilog forward` ships a file's lines.
- End-to-end: forward/POST logs → search by text/field/time → see histogram &
  facets → live-tail matching new events.
- Retention purges old logs.
- Tests green; README documents run/usage; Dockerfile builds.

## 9. Future (post-v1, designed-for)
Alerting, saved searches & dashboards, RBAC, columnar store (ClickHouse) behind
the `Store` interface, ingest parsing pipelines, clustering.
