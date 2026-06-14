# Omni-logging

A self-contained **centralized logging system** — conceptually similar to Splunk —
that ships as a single Go binary with the web UI embedded. Apps ship logs over
HTTP; logs are stored and full-text indexed in SQLite; you search, filter,
aggregate, and live-tail through a web UI and a JSON API. Zero external services.

> Status: **v1 shipped.** See the design spec in
> [`docs/superpowers/specs/2026-06-14-omni-logging-design.md`](docs/superpowers/specs/2026-06-14-omni-logging-design.md)
> and the post-v1 plan in [`ROADMAP.md`](ROADMAP.md) (53 milestones across 6 themes).

## Features (v1)

- **HTTP ingestion** — POST structured (NDJSON / JSON) or raw text logs.
- **Storage + full-text index** — SQLite with FTS5; time/field indexes; retention.
- **Search** — free-text, field filters (`level=error service=api`), time ranges.
- **Aggregations** — counts-over-time histogram and field facets.
- **Live tail** — real-time streaming of matching events (SSE).
- **Web UI** — search, histogram, facets, expandable rows, live tail.
- **Forwarder** — `omnilog forward` tails files and ships them to the server.
- **Minimal auth** — per-source ingest API keys + an admin token for query/UI.

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

## Query language

The search box and the `q` parameter accept a small Splunk-like expression:

- **Free text** — `timeout payments` (AND-combined, full-text via FTS5)
- **Field filters** — `level=error service=checkout-api source=node-1`
- **Attribute filters** — `attr.user_id=42` (or bare `user_id=42`)
- **Negation** — `level!=debug`
- **Quoted phrases** — `"connection refused"`
- **Time range** — `last=15m` (`s/m/h/d`) or absolute `from`/`to` (RFC3339 / unix)

Example: `level=error service=checkout-api timeout last=1h`

## Architecture

Single Go binary, packages under `internal/`:

| Package | Responsibility |
|---|---|
| `model` | Canonical `LogEvent`, level/timestamp normalization, ULID |
| `query` | Query-language parser, params builder, in-memory matcher |
| `store` + `store/sqlite` | `Store` interface; SQLite + FTS5 implementation |
| `ingest` | Buffered batch writer + HTTP ingest handlers |
| `tail` | In-memory pub/sub hub + SSE handler |
| `api` | Router, auth middleware, search/stats/health handlers |
| `web` | Embedded single-page UI (vanilla JS/CSS, no build step) |
| `forward` | File-tailing forwarder client |

The web UI is hand-written vanilla JS/CSS embedded via `go:embed`, so the whole
project builds with a single `go build` — no Node toolchain required. See the
design spec in
[`docs/superpowers/specs/2026-06-14-omni-logging-design.md`](docs/superpowers/specs/2026-06-14-omni-logging-design.md).

## Deployment & CI/CD

A GitHub Actions workflow ([`.github/workflows/cicd.yml`](.github/workflows/cicd.yml))
runs on a **self-hosted runner that lives on the deploy target** (the deploy target),
so the deploy runs local `docker` commands — no SSH hop, no stored credentials.

- **`build`** — builds the image (`docker compose build`) on every push/PR; gates deploy. Fork PRs from outside the repo are not run on the self-hosted runner.
- **`deploy`** — runs only on `main`. Because omni-logging is **stateful** (SQLite + WAL), the deploy is hardened: online `VACUUM INTO` backup → stop-first recreate → health wait → external smoke test → `PRAGMA integrity_check` → auto-heal from the latest backup if the check fails. Deploys are serialized (`concurrency: deploy-omnilog`).

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

TBD
