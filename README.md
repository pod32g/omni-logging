# Omni-logging

A self-contained **centralized logging system** — conceptually similar to Splunk —
that ships as a single Go binary with the web UI embedded. Apps ship logs over
HTTP; logs are stored and full-text indexed in SQLite; you search, filter,
aggregate, and live-tail through a web UI and a JSON API. Zero external services.

## Features (v1)

- **HTTP ingestion** — POST structured (NDJSON / JSON) or raw text logs.
- **Syslog collector** — optional RFC5424/RFC3164 listener over UDP and TCP, so containers, daemons and network gear can ship logs with no agent and no code changes. Off by default.
- **Storage + full-text index** — SQLite with FTS5; time/field indexes; retention.
- **Search** — free-text, field filters (`level=error service=api`), time ranges.
- **Aggregations** — a piped analytics stage (`| stats count by service`, `timechart`, `top`, `rare`), plus the counts-over-time histogram and field facets.
- **Alerting** — scheduled rules over any search or aggregation, with threshold conditions and webhook/Slack notifications on state transitions.
- **Live tail** — real-time streaming of matching events (SSE), seeded with the last 50 matching events so the pane is useful the moment it opens rather than blank until the next log arrives.
- **Web UI** — search, histogram, facets, expandable rows, live tail, paginated results + export, and a light/dark/system theme toggle.
- **Forwarder** — `omnilog forward` tails files and ships them to the server.
- **CLI query** — `omnilog query` searches a server from the terminal (table/JSON/NDJSON, `--follow` live tail).
- **OpenAPI** — a versioned 3.1 contract at `/openapi.json` with a self-hosted reference UI at `/docs` (no CDN; works air-gapped).
- **Settings page** — edit retention, rate limits, quotas, log level, and ingest keys live (persisted in the DB, applied without a restart) via the UI or `GET`/`PUT /api/v1/config`. `PUT` merges: fields you omit keep their current value, so clearing one takes an explicit zero (e.g. `"ingest_keys": []`). The admin token is browser-side only and not editable from the UI.
- **Minimal auth** — per-source ingest API keys + an admin token for query/UI.
- **Admission control** — per-key token-bucket rate limits + daily event/byte quotas (`rate_limit_per_sec`, `rate_burst`, `daily_quota_events`, `daily_quota_bytes`; `0` = off). Rejections return `429 {reason}` and increment `omnilog_ingest_rejected_total`.
  `rate_limit_per_sec` counts **requests, not events** — one request may carry a whole batch. Use the daily quotas to bound event and byte volume.

## Quick start

```sh
# build (UI is embedded)
make build

# run the server
./omnilog serve --addr :8080 --db ./omni.db --admin-token secret --ingest-key devkey

# send some logs
curl -XPOST localhost:8080/api/v1/ingest -H 'X-Api-Key: devkey' \
  -H 'Content-Type: application/x-ndjson' \
  --data-binary $'{"service":"api","level":"error","message":"boom"}\n'

# forward a file
./omnilog forward --file /var/log/app.log --service app \
  --server http://localhost:8080 --api-key devkey

# open the UI
open http://localhost:8080
```

## Sending logs from your services

See **[`docs/INTEGRATION.md`](docs/INTEGRATION.md)** for the full guide — HTTP
ingest (curl/Go/Python/Node snippets), the file forwarder, and how to wire up
dockerized services. The short version:

```sh
# structured (NDJSON): unknown keys become searchable attributes
curl -XPOST http://HOST:8080/api/v1/ingest -H 'X-Api-Key: devkey' \
  -H 'Content-Type: application/x-ndjson' \
  --data-binary $'{"service":"api","level":"error","message":"boom","status":500}\n'

# tail existing files into the server
omnilog forward --server http://HOST:8080 --api-key devkey --service api --file /var/log/app.log
```

## Query language

The search box and the `q` parameter accept a small Splunk-like expression:

- **Free text** — `timeout payments` (AND-combined, full-text via FTS5). Terms match **whole tokens, with the last one as a prefix**: `conn` finds `connection`, but `nnection` finds nothing. In a quoted phrase only the final token is a prefix (`"connection refu"` matches, `"conn refused"` does not).
- **Field filters** — `level=error service=checkout-api source=node-1 message=…` (also `raw`)
- **Attribute filters** — `attr.user_id=42` (or bare `user_id=42`)
- **Negation** — `level!=debug`
- **Comparison** — `attr.status>=500`, `attr.latency_ms<10` (numeric when both sides are numbers, else lexical)
- **Wildcard** — `service=checkout*` (`*` glob)
- **Exists** — `attr.request_id=*` (field present / attribute non-null)
- **In set** — `level=(error,warn,fatal)`
- **Regex** — `message=~timeout|refused` (RE2)
- **Quoted phrases** — `"connection refused"`
- **Time range** — `last=15m` (`s/m/h/d`) or absolute `from`/`to` (RFC3339 / unix)

Filters are AND-combined. (Cross-field OR-grouping with parentheses is planned with
the query-language spec; `IN` covers the common same-field OR case today.)

### Aggregations

Append a `|` stage to turn a filter into a table. The filter half applies
exactly as it does for a plain search, so you narrow first and aggregate second.

```
level=error | stats count by service
| stats count, avg(attr.latency_ms), max(attr.latency_ms) by service, level
| timechart span=5m count by level
service=api | top 5 attr.status
| rare source
```

- **Commands** — `stats`, `timechart` (with `span=1m`), `top [N]`, `rare [N]`
- **Functions** — `count`, `sum(f)`, `avg(f)`, `min(f)`, `max(f)`, `dc(f)` (distinct count)
- **Grouping** — `by a, b`; commas are optional. Fields are named exactly as in
  filters, so `service` and `attr.user_id` mean the same thing on both sides.
- Numeric functions coerce like the comparison operators do: non-numeric text
  counts as `0`, matching `CAST(... AS REAL)`.

Results come back from `GET /api/v1/aggregate` as `{columns, rows,
group_columns}`, and the UI renders the same table automatically when it sees a
`|` in the query. Groups are capped (`truncated: true` says so) so grouping by a
high-cardinality field returns a readable table instead of a row per event.

Example: `level=(error,fatal) service=checkout* attr.status>=500 timeout last=1h`

**Pagination & export.** `/api/v1/search` returns a `next_cursor`; pass it as
`?after=<cursor>` for stable keyset paging (the UI's *Load more*). `/api/v1/export`
streams **all** matches (beyond the search cap) as `?format=ndjson|csv|json`.

## Architecture

Single Go binary, packages under `internal/`:

| Package | Responsibility |
|---|---|
| `model` | Canonical `LogEvent`, level/timestamp normalization, ULID |
| `query` | Query-language parser, params builder, in-memory matcher |
| `store` + `store/sqlite` | `Store` interface; SQLite + FTS5; versioned migrations (`PRAGMA user_version`) |
| `ingest` | Durable accept (WAL) + buffered batch writer + HTTP ingest handlers |
| `wal` | Segment write-ahead log: crash-safe accept, CRC, checkpoint, replay |
| `tail` | In-memory pub/sub hub + SSE handler |
| `api` | Router, auth + metrics middleware, search/stats/health/metrics handlers |
| `metrics` | Tiny Prometheus-text registry (counters/gauges/histograms), no deps |
| `web` | Embedded single-page UI (vanilla JS/CSS, no build step) |
| `forward` | File-tailing forwarder client |
| `alert` | Rule evaluation, scheduling and notification delivery |
| `syslog` | RFC5424/RFC3164 parser + UDP/TCP collector |

The web UI is hand-written vanilla JS/CSS embedded via `go:embed`, so the whole
project builds with a single `go build` — no Node toolchain required. See the
design spec in
[`docs/superpowers/specs/2026-06-14-omni-logging-design.md`](docs/superpowers/specs/2026-06-14-omni-logging-design.md).

## Observability

The server exposes Prometheus metrics and split health probes. Health probes are
unauthenticated and intentionally minimal. Metrics are loopback-only by default;
set `--metrics-public` or `OMNILOG_METRICS_PUBLIC=true` only on a trusted network.

| Endpoint | Purpose |
|---|---|
| `GET /metrics` | Prometheus text exposition: ingest counters, store query latency, live-tail subscribers/drops, HTTP request count/latency, `omnilog_build_info` (loopback-only by default). |
| `GET /api/v1/healthz` | **Liveness** — process is up (always `200`; used by the container HEALTHCHECK and the deploy probe). |
| `GET /api/v1/readyz` | **Readiness** — `200` only when the backend store is reachable, else `503`. |
| `GET /api/v1/status` | **Operational snapshot** — version, live-tail subscribers, ingest counters. Behind the admin token: unlike liveness, these numbers describe traffic shape. Powers the UI's Settings → Server status panel. |

> **Reverse proxies and `/metrics`.** The loopback check looks at the immediate
> peer address. Behind a proxy — including the `netviz-sidecar` in
> [`docker-compose.yml`](docker-compose.yml) — scrapes arrive from the proxy's
> address, not loopback, so `/metrics` returns `404`. Either scrape from inside
> the container's network namespace, or set `OMNILOG_METRICS_PUBLIC=true` and
> restrict access at the proxy.

Metrics are emitted by a small in-repo registry (no `client_golang` dependency), so
the binary stays self-contained. Example scrape config:

```yaml
scrape_configs:
  - job_name: omnilog
    static_configs: [{ targets: ['HOST:8080'] }]
```

## Deployment & CI/CD

A GitHub Actions workflow ([`.github/workflows/cicd.yml`](.github/workflows/cicd.yml))
runs on a **self-hosted runner that lives on the deploy target**,
so the deploy runs local `docker` commands — no SSH hop, no stored credentials.

- **`build`** — builds the image (`docker compose build`) on every push/PR; gates deploy. Fork PRs from outside the repo are not run on the self-hosted runner.
- **`deploy`** — runs only on `main`. Because omni-logging is **stateful** (SQLite + WAL), the deploy is hardened: online `VACUUM INTO` backup → stop-first recreate → health wait → external smoke test → `PRAGMA integrity_check` → auto-heal from the latest backup if the check fails. Deploys are serialized (`concurrency: deploy-omnilog`).

Ingestion is **durable**: each accepted event is written to an on-disk
write-ahead log (`<db dir>/wal`, override with `--wal-dir` / `OMNILOG_WAL_DIR`)
before the request is acked, then committed to the store in batches. After a
commit the WAL checkpoint advances and applied segments are reclaimed. On startup
the WAL is replayed into the store, so events accepted before a crash are never
lost. Replay is idempotent (ULID `INSERT OR REPLACE`).

The schema is managed by **versioned migrations** keyed on `PRAGMA user_version`
(audited in a `schema_migrations` table). On startup the server applies any pending
migrations in order, each in its own transaction, and **refuses to start** against a
database written by a newer binary — so a rollback can never silently corrupt data.

The binary self-validates so the distroless image needs no extra tools:

```sh
omnilog backup --db /data/omni.db --out /data/backups/snap.db   # WAL-safe snapshot
omnilog integrity --db /data/omni.db                            # PRAGMA integrity_check
omnilog healthcheck --url http://localhost:8080/api/v1/healthz  # container HEALTHCHECK
```

Run locally with Compose: `docker compose up --build -d` (UI on `:8080`,
data in the `omnilog-data` volume). Set `OMNILOG_ADMIN_TOKEN` / `OMNILOG_INGEST_KEYS`
in a `.env` file to enable auth.

## Alerting

An alert rule is a query plus a window plus a threshold — the query is any
search expression, including an aggregation stage, so anything you can search
for you can alert on.

```sh
# a channel to notify
curl -XPOST localhost:8080/api/v1/alerts/channels -H 'Content-Type: application/json' \
  -d '{"name":"ops","type":"slack","url":"https://hooks.slack.com/services/..."}'

# fire when any service logs more than 50 errors in 5 minutes
curl -XPOST localhost:8080/api/v1/alerts -H 'Content-Type: application/json' -d '{
  "name": "error spike",
  "query": "level=error | stats count by service",
  "window_seconds": 300,
  "interval_seconds": 60,
  "condition": {"op": "gt", "value": 50},
  "channels": ["<channel-id>"],
  "enabled": true
}'

# see what it would do right now, without firing or changing its state
curl -XPOST localhost:8080/api/v1/alerts/<rule-id>/test
```

- **Conditions** — `gt`, `gte`, `lt`, `lte`, `eq`, `ne` against a threshold.
  A plain query compares the number of matching events; an aggregating query
  compares its first measure and reports **which groups** breached, so the
  notification names the service rather than only the symptom.
- **Notifications fire on transitions**, not on every evaluation: a rule that
  stays broken for an hour sends one firing message and one resolved message.
- **A failed evaluation goes to `unknown`**, never to `ok` — "we could not tell"
  is not the same as "fine", and it does not send a resolved notification.
- **Dead-man's switch** — because an empty aggregate compares as zero,
  `service=heartbeat | stats count` with `lt 1` alerts when a service goes quiet.
- **Channels** are `webhook` (receives the full JSON payload) or `slack`
  (receives `{"text": ...}`). Redirects are deliberately not followed, so a
  payload cannot be diverted to a host you did not configure.

> Alert channels make the server issue outbound HTTP requests to URLs you
> supply. The endpoints are behind the admin token for that reason; treat the
> ability to create a channel as equivalent to outbound request access.

### Syslog collector

Off unless you bind it. Enable with `--syslog-udp` / `--syslog-tcp` (or
`OMNILOG_SYSLOG_UDP_ADDR` / `OMNILOG_SYSLOG_TCP_ADDR`):

```sh
omnilog serve --syslog-udp :5514 --syslog-tcp :5514
```

Point Docker services at it with no application changes:

```yaml
services:
  my-service:
    logging:
      driver: syslog
      options:
        syslog-address: "udp://omnilog:5514"
        tag: "my-service"
```

Both RFC5424 and the classic RFC3164 format are parsed, over both RFC6587 TCP
framings (octet-counted and newline-delimited). The syslog severity becomes the
event level, the app-name/tag becomes `service`, the hostname becomes `source`,
and RFC5424 structured data is flattened into searchable attributes
(`sdid.param`), alongside `syslog_facility` and `syslog_severity`. A message
that fails to parse is still stored with its raw text rather than dropped.

> **Syslog carries no credentials.** There is nothing to authenticate, so
> exposure is controlled entirely by the bind address — the ingest keys do not
> apply to this listener. Bind it to a private interface or a container network,
> never to a public one. Ports below 1024 (like the conventional 514) need
> privileges the distroless container deliberately does not have; use a high
> port and remap it if you need 514 externally.

### Optional hardening

Everything here is **off by default** — the server runs open over plain HTTP so a
fresh or homelab install is never locked out of itself. Turn these on when the
deployment warrants it:

| Setting | Flag / env | Effect when unset (default) |
|---|---|---|
| Admin token | `--admin-token` / `OMNILOG_ADMIN_TOKEN` | Query, export, tail, status and config endpoints are open. |
| Ingest keys | `--ingest-key` / `OMNILOG_INGEST_KEYS` | Ingestion is unauthenticated. |
| TLS | `--tls-cert` + `--tls-key` | Plain HTTP. TLS 1.2 is the floor when enabled. |
| HSTS | `--hsts` / `OMNILOG_HSTS=true` | No `Strict-Transport-Security` header. |

> **On HSTS:** browsers cache the policy for a year, so enabling it pins the
> origin to HTTPS for every client that has visited it — reverting to plain HTTP
> then requires clearing browser state. It only takes effect when TLS is also
> configured, and it stays opt-in for exactly this reason.

## Development

```sh
make test      # run the full Go test suite
make vet       # go vet
make build     # build the single binary (UI is embedded)
make run       # build and run locally on :8080
make docker    # build the container image
```

## License

[MIT](LICENSE) © pod32g
