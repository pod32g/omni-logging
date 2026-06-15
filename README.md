# Omni-logging

A self-contained **centralized logging system** — conceptually similar to Splunk —
that ships as a single Go binary with the web UI embedded. Apps ship logs over
HTTP; logs are stored and full-text indexed in SQLite; you search, filter,
aggregate, and live-tail through a web UI and a JSON API. Zero external services.

## Features (v1)

- **HTTP ingestion** — POST structured (NDJSON / JSON) or raw text logs.
- **Storage + full-text index** — SQLite with FTS5; time/field indexes; retention.
- **Search** — free-text, field filters (`level=error service=api`), time ranges.
- **Aggregations** — counts-over-time histogram and field facets.
- **Live tail** — real-time streaming of matching events (SSE).
- **Web UI** — search, histogram, facets, expandable rows, live tail, paginated results + export, and a light/dark/system theme toggle.
- **Forwarder** — `omnilog forward` tails files and ships them to the server.
- **CLI query** — `omnilog query` searches a server from the terminal (table/JSON/NDJSON, `--follow` live tail).
- **OpenAPI** — a versioned 3.1 contract at `/openapi.json` with a reference UI at `/docs`.
- **Settings page** — edit retention, rate limits, quotas, log level, and ingest keys live (persisted in the DB, applied without a restart) via the UI or `GET`/`PUT /api/v1/config`. The admin token is browser-side only and not editable from the UI.
- **Minimal auth** — per-source ingest API keys + an admin token for query/UI.
- **Admission control** — per-key token-bucket rate limits + daily event/byte quotas (`rate_limit_per_sec`, `rate_burst`, `daily_quota_events`, `daily_quota_bytes`; `0` = off). Rejections return `429 {reason}` and increment `omnilog_ingest_rejected_total`.

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

- **Free text** — `timeout payments` (AND-combined, full-text via FTS5)
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

The web UI is hand-written vanilla JS/CSS embedded via `go:embed`, so the whole
project builds with a single `go build` — no Node toolchain required. See the
design spec in
[`docs/superpowers/specs/2026-06-14-omni-logging-design.md`](docs/superpowers/specs/2026-06-14-omni-logging-design.md).

## Observability

The server exposes Prometheus metrics and split health probes (all unauthenticated,
so infra probes and scrapers can reach them):

| Endpoint | Purpose |
|---|---|
| `GET /metrics` | Prometheus text exposition: ingest counters, store query latency, live-tail subscribers/drops, HTTP request count/latency, `omnilog_build_info`. |
| `GET /api/v1/healthz` | **Liveness** — process is up (always `200`; used by the container HEALTHCHECK and the deploy probe). |
| `GET /api/v1/readyz` | **Readiness** — `200` only when the backend store is reachable, else `503`. |

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
