# M39 — Public OpenAPI spec + CLI query tool

*Roadmap M39 · Effort M · Depends on v1. Unblocks SDKs (M40) and the Grafana
plugin (M43) — a stable, documented contract.*

## Delivered
- **OpenAPI 3.1 contract**, hand-written and embedded (`internal/api/openapi.json`,
  `go:embed`), served unauthenticated at **`GET /openapi.json`**; a reference UI at
  **`GET /docs`** (Redoc via CDN, consistent with the UI's web-font CDN use). Covers
  ingest (structured + raw), search, stats, export, tail, health/ready, metrics,
  with `LogEvent`/`SearchResult`/`StatsResult`/`IngestResponse` schemas and the two
  security schemes (ingest key, admin bearer).
- **`omnilog query` CLI**: terminal search against a server's API with
  `table | json | ndjson` output and `--follow` (SSE live tail). Backed by a thin,
  tested `internal/queryclient` package (HTTP client + formatters); the token falls
  back to `OMNILOG_ADMIN_TOKEN`.

## Testing
`queryclient` unit tests (Search via httptest incl. auth failure; table/json/ndjson
formatters); an API test that `/openapi.json` is valid JSON `3.1.0` with the
expected paths and `/docs` renders. Runtime smoke: `omnilog query` table/ndjson +
`/openapi.json`/`/docs` against a live server. Full suite green.
