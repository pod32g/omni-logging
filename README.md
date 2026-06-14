# Omni-logging

A self-contained **centralized logging system** — conceptually similar to Splunk —
that ships as a single Go binary with the web UI embedded. Apps ship logs over
HTTP; logs are stored and full-text indexed in SQLite; you search, filter,
aggregate, and live-tail through a web UI and a JSON API. Zero external services.

> Status: **v1 in development.** See the design spec in
> [`docs/superpowers/specs/2026-06-14-omni-logging-design.md`](docs/superpowers/specs/2026-06-14-omni-logging-design.md).

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

## Development

```sh
make test      # Go tests
make ui        # build the web UI into the embed dir
make build     # build the binary with embedded UI
make run       # run locally
```

## License

TBD
