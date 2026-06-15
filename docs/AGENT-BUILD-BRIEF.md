# Build brief: implement roadmap P0 + P1

You are a fresh Claude Code (ultracode) instance. Your job: implement the **P0**
then **P1** milestones from [`ROADMAP.md`](../ROADMAP.md) for this repo,
`pod32g/omni-logging`. This brief is self-contained — read it fully before starting.

## What this project is
A self-contained centralized logging system (Splunk-like) shipped as **one Go
binary** with the web UI embedded. v1 is shipped and deployed. Packages:
`internal/{model,query,store,store/sqlite,ingest,tail,api,config,forward,web}`
and `cmd/omnilog` (subcommands: serve, forward, backup, integrity, healthcheck,
version). The web UI is hand-written vanilla JS/CSS in `internal/web/dist`,
embedded via `go:embed`. Read `ROADMAP.md` for each milestone's full description,
effort, and dependencies.

## HARD CONSTRAINTS (a fresh instance cannot infer these — do not violate)
1. **This is a PUBLIC repo.** Never commit internal hostnames, IPs, or absolute
   home paths in any file (docs, workflows, comments). Use generic placeholders
   (`HOST`, `the deploy target`) and derive paths from `$HOME` at runtime.
2. **Commit identity:** the repo's local git identity is
   `pod32g <3311662+pod32g@users.noreply.github.com>` (already set). **Do NOT add
   a `Co-Authored-By: Claude` trailer** — it creates a phantom GitHub contributor.
3. **CI/CD auto-deploys.** `.github/workflows/cicd.yml` runs on every push: `build`
   gates, and **`deploy` runs on `main`** to a self-hosted runner that deploys
   live. Docs-only pushes are skipped (`paths-ignore: **.md, docs/**, LICENSE`).
   → **Work on feature branches**, verify locally, and treat every merge to
   `main` as a real production deploy. Never commit secrets; `.env` is gitignored.
4. **Storage is stateful (SQLite + WAL).** The deploy backs up + integrity-checks
   on every release. Do not change the on-disk schema without a migration
   (see M6) and the store conformance suite (see M3/M19). Don't risk data loss.
5. **UI is embedded.** Changes under `internal/web/dist` require a `go build` to
   take effect (and a deploy to land on the box).

## Engineering workflow (ultracode)
- Use the superpowers skills: **brainstorming → writing-plans → test-driven-
  development → verification-before-completion** for each milestone or coherent group.
- **TDD**: write table-driven tests first; mirror the existing test style in each package.
- Before every commit: `gofmt -w .` && `go vet ./...` && `go test ./...` must pass.
- Use the **Workflow tool** to parallelize *independent* milestones, but respect
  the dependency order below — never start a milestone before its deps are merged.
- Land each milestone (or small group) as its own branch → verify → merge to `main`
  → confirm the deploy is green (`gh run watch`) and the box stays healthy.

## Build order

### P0 (do first; mostly v1-only deps)
1. **M2 — Prometheus metrics + readiness/liveness probes** (M). Add a metrics
   registry (hand-rolled or `prometheus/client_golang`); expose `/metrics` and
   readiness in `internal/api`. Instrument ingest (`internal/ingest` already has
   counters), store query latency, tail subscribers. Unblocks much of P1.
2. **M6 — Versioned DB migrations + schema guard** (M). Add a `schema_version`
   table and an ordered migration runner in `internal/store/sqlite`; refuse to
   start on an unknown/newer version. Needed before any schema change below.
3. **M3 — Durable write path: segment WAL + crash recovery + store conformance
   suite** (L, careful). Harden `Append` durability and add a reusable
   `store` conformance test suite (table-driven, run against sqlite now, any
   backend later). This is the riskiest; pair with M6 and the backup/integrity
   tooling that already exists (`omnilog backup|integrity`).
4. **M19 — Store-interface hardening + benchmarking baseline** (M; dep M3).
   Tighten the `store.Store` interface, run the conformance suite against it, add
   `go test -bench` baselines for append/search. Unblocks all scale work.
5. **M26 — Richer query operators, pagination & export** (L; dep v1). Extend
   `internal/query` (comparison ops, wildcards, exists, regex) + the `/api/v1/search`
   handler (cursor pagination, CSV/NDJSON export) + the UI results view. Unblocks
   aggregations/dashboards/alerting.
6. **M55 — Dark mode & theming** (S; dep v1). The UI already uses CSS custom
   properties (`internal/web/dist/styles.css`, `:root`). Add a `dark` variable
   set, a light/dark/system toggle in the app bar (`index.html` + `app.js`),
   persist to `localStorage`, honor `prefers-color-scheme`. Rebuild to embed.

### P1 (after P0; respect deps)
- **M4** rate limiting & per-key quotas (M; dep M2) → **M5** config hot-reload &
  zero-downtime shutdown (M; dep M4) → **M7** backup/restore/DR (L; dep M5;
  builds on existing `backup`/`integrity`).
- **M47** tail-hub hardening (M; dep M2) — `internal/tail`: per-subscriber bounded
  buffers, slow-consumer eviction, metrics. Unblocks redaction/alerts/export.
- **M20** SQLite read-concurrency, FTS & cost-guard tuning (L; dep M19).
- **M27** aggregation pipeline — stats/timechart/top/rare (XL; dep M26) → **M28**
  saved searches, query history & async jobs + caching (L; dep M26,M27).
- **M14** ingest-time parsing pipeline framework (L; dep M5) → **M15** grok/regex
  extraction + timestamp/multiline (L; dep M14).
- **M10** compression + gRPC ingest (L; dep v1) → **M11** OTLP logs receiver (L;
  dep M10). **M12** syslog collector RFC3164/5424 over UDP/TCP/TLS (M; dep v1).
- **M9** durable forwarder spool + at-least-once + dead-letter (L; dep M3) —
  extend `internal/forward`.
- **M39** public OpenAPI spec + CLI query tool (M; dep v1) → **M40** client SDKs +
  framework appenders Go/Python/JS (L; dep M39).
- **M8** finish CI/CD: multi-arch signed release automation (M; dep v1; CI/CD
  scaffold already exists).

## Definition of done (per milestone)
- New/changed behavior covered by tests; full suite + vet + gofmt green.
- Docs updated (README/INTEGRATION/ROADMAP as relevant; docs-only = no deploy).
- Merged to `main`; deploy run green; `omnilog healthcheck` passes on the box.
- No internal host details, secrets, or Claude co-author trailers introduced.

When P0 and P1 are complete and verified, summarize what shipped and what remains
(P2/P3) and stop.
