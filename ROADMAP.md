# Omni-logging — Roadmap

Post-v1 milestones. **v1 (M1) is shipped**: HTTP ingest (NDJSON/JSON/raw), SQLite+FTS5 storage with retention, the query language (free-text + field/attr filters + time ranges), search + stats (histogram & facets), SSE live tail, the embedded web UI, the file forwarder, minimal auth, Docker, and a self-hosted CI/CD pipeline that deploys to the target box.

> **How to read this.** Milestones are grouped into six themes that tell a rough product story, roughly in the order a single maintainer would tackle them. **IDs are stable references** (dependencies point to them), not a strict execution order — the real order is given by each milestone's *Depends on* line and effort. Effort: S (days), M (1–2 weeks), L (3–6 weeks), XL (multi-week / epic).

**54 milestones** across 6 themes.

## Suggested build order (priority tiers)

Themes group *what* a milestone is; these tiers suggest *when* to do it. Ordered
for turning v1 into a useful, safe tool, respecting dependencies and favoring
high-leverage/low-effort wins. Tilted toward a self-hosted, single-user setup
(multi-tenancy / RBAC / SSO / licensing are pushed down; collectors, analytics,
alerting, data-safety, and UI comfort move up).

**P0 — Now** (foundation, data-safety, daily-use comfort; v1-only deps)
M3 durable write path · M19 store-interface hardening + benchmark · M26 richer query + export · M2 metrics/probes · M6 DB migrations · **M55 dark mode**

**P1 — Next** (capability + easy onramps)
M27 aggregations · M28 saved searches/jobs · M14 parsing framework · M15 grok extraction · M12 syslog · M10 compression/gRPC · M11 OTLP · M9 forwarder spool · M47 tail-hub hardening · M4 quotas · M5 hot-reload · M7 backup/DR · M39 OpenAPI+CLI · M40 SDKs · M8 release automation · M20 read-concurrency

**P2 — Later** (scale, dashboards, alerting, ecosystem, light auth)
M29 dashboards · M30–M32 alerting + anomaly · M49 object-store · M21/M22/M23/M24 tiering & cost · M25 ClickHouse · M13 Fluent/Beats · M16 redaction · M17 schema-on-read · M18 k8s autodiscovery · M50 replay · M41 export · M42 importers · M43 Grafana · M44/M45 packaging · M48 e2e/chaos · M51 query-lang spec · M52 trace correlation · M33/M34/M36/M37 identity/RBAC/audit/SSO

**P3 — Eventually** (heavy scale & enterprise)
M46 HA/clustering · M35 multi-tenancy · M38 data protection/compliance · M53 metering/licensing · M54 accessibility + i18n

> **Top 3 to start:** M3 (don't lose logs), M26 (powerful search), M27 (aggregations/charts) — plus M55 (dark mode) as a quick comfort win.


## Production Hardening

_Make the single-node v1 operable and durable: self-observability, durable writes with crash recovery, admission control, config hot-reload, backup/restore, CI/CD, and versioned migrations. These are the table-stakes operational features that everything else depends on._

### M2 — Self-monitoring: Prometheus metrics + readiness/liveness probes
*Effort: M · 1–2 wks · Depends on: v1*

Expose a /metrics endpoint covering ingest, batch writer, query, tail, and store internals, and split the v1 health stub into real liveness and readiness probes that reflect backend state. This is the observability foundation every other reliability and scale decision depends on.

### M3 — Durable write path: segment WAL + crash recovery + store conformance suite
*Effort: L · 3–6 wks · Depends on: v1, M2*

Put a crash-safe write-ahead log in front of the batch writer so accepted events survive a process crash, add ULID-based dedup on replay, and harden the Store interface with a backend-agnostic conformance test suite. This closes the v1 gap where a crash mid-buffer silently loses accepted events.

### M4 — Structured logging + ingest rate limiting & per-key quotas
*Effort: M · 1–2 wks · Depends on: v1, M2*

Standardize structured operational logging with request IDs and configurable levels, and add real admission control: per-ingest-key token-bucket rate limits and daily byte/event quotas, replacing the all-or-nothing buffer-full 429.

### M5 — Graceful config hot-reload & zero-downtime shutdown
*Effort: M · 1–2 wks · Depends on: v1, M4*

Reload mutable configuration (ingest keys, rate limits/quotas, retention, log level) on SIGHUP or an admin endpoint without dropping connections, and harden startup/shutdown so deploys never lose buffered logs.

### M6 — Versioned DB migrations + schema-version guard
*Effort: M · 1–2 wks · Depends on: v1*

Replace the implicit CREATE TABLE IF NOT EXISTS bootstrap with an explicit, ordered, versioned migration runner keyed on PRAGMA user_version, plus a Migrator seam so non-SQLite backends plug in their own scheme. This makes every future schema change safe on existing databases.

### M7 — Backup, restore & disaster recovery for the SQLite store
*Effort: L · 3–6 wks · Depends on: v1, M5*

Add first-class online backup and restore to the Store interface using SQLite's online backup API, with scheduled snapshots, optional S3 push, and integrity verification, so a single-node deployment is durable against disk loss and operator mistakes.

### M8 — CI/CD pipeline + multi-arch signed release automation
*Effort: M · 1–2 wks · Depends on: v1*

Stand up GitHub Actions for test/lint/vuln gates on every push and a tag-triggered release producing reproducible, multi-arch binaries and signed container images. This is the foundation every distribution artifact consumes.

### M47 — Live tail hub hardening (standalone real-time subscription layer)
*Effort: M · 1–2 wks · Depends on: v1, M2*

Promote the in-memory tail hub to a hardened, observable real-time fan-out layer with per-subscriber bounded buffers, slow-consumer eviction, and metrics, before redaction, streaming alerts, and export come to depend on it.

### M48 — End-to-end, upgrade-compatibility & chaos test harness
*Effort: L · 3–6 wks · Depends on: M6, M8, M19*

An integration/e2e harness distinct from the perf benchmark: cross-version migration tests, rolling-upgrade and multi-protocol-receiver coverage, and fault injection (disk-full, killed mid-write, network partition) to prove the durability/auto-heal claims.


## Collection & Parsing

_Broaden how logs get in and turn raw lines into structured data: durable forwarder delivery, compression/gRPC/OTLP/syslog/Fluent/Beats receivers, a Kubernetes collector, and a configurable ingest-time parsing pipeline (grok, timestamps, multiline, redaction, sampling, enrichment) plus schema-on-read._

### M9 — Durable forwarder spool with at-least-once delivery + dead-letter
*Effort: L · 3–6 wks · Depends on: v1, M3*

Harden the file-tailing forwarder into a reliable agent: an on-disk spool buffers batches across restarts and outages, retries until acked, dedupes via client batch IDs, and routes permanently-rejected records to a dead-letter file. This turns best-effort tailing into at-least-once delivery.

### M10 — Compression + gRPC ingest transport
*Effort: L · 3–6 wks · Depends on: v1*

Add transparent gzip/zstd request decompression to the HTTP ingest endpoints and a parallel gRPC ingest service sharing the same validate/normalize/buffer path, widening the throughput ceiling without changing the LogEvent model or Store seam.

### M11 — OTLP/OpenTelemetry logs receiver
*Effort: L · 3–6 wks · Depends on: v1, M10*

Implement the OTLP logs receiver over both gRPC and HTTP (protobuf and JSON, gzip-aware) mapping the OTLP log data model onto LogEvent, so any OpenTelemetry SDK or Collector can export directly. This plugs Omni-logging into the dominant observability ecosystem.

### M12 — Syslog collector (RFC3164/RFC5424 over UDP/TCP/TLS)
*Effort: M · 1–2 wks · Depends on: v1*

Add a built-in syslog server so network gear, Linux daemons, and appliances can ship logs with no agent, parsing both classic and structured syslog into the canonical LogEvent.

### M13 — Fluent Forward + Beats/Lumberjack ingestion
*Effort: M · 1–2 wks · Depends on: v1*

Add server-side receivers for the two most widely deployed shippers' wire protocols (Fluentd/Fluent Bit forward and Elastic Beats Lumberjack v2), letting existing fleets point at Omni-logging without re-instrumenting hosts.

### M14 — Ingest-time parsing pipeline framework
*Effort: L · 3–6 wks · Depends on: v1, M5*

Introduce a configurable, ordered pipeline of parse/transform stages every event passes through after raw ingest and before the batch writer. This is the foundational seam all later parsing features (grok, timestamps, multiline, redaction, sampling, enrichment) plug into.

### M15 — Grok/regex field extraction + timestamp & multiline assembly
*Effort: L · 3–6 wks · Depends on: M14*

Pipeline stages that turn unstructured text into structured fields: named grok/regex patterns promoting captures into first-class fields and Attributes, pluggable timestamp-format parsing, and multiline assembly that joins stack traces into a single event before extraction.

### M16 — Redaction, sampling, drop & enrichment stages
*Effort: M · 1–2 wks · Depends on: M14, M15, M47*

Compliance- and cost-oriented pipeline stages: mask/hash PII, drop noisy events by query predicate, probabilistically sample high-volume sources, and enrich events via CSV/JSON lookup tables and offline GeoIP resolution.

### M17 — Schema-on-read extraction & derived fields at query time
*Effort: L · 3–6 wks · Depends on: v1, M15*

Let users define regex extractions and computed/derived fields that apply at search time over already-stored events, so fields can be added retroactively without reindexing, complementing ingest-time parsing.

### M18 — Container & Kubernetes log autodiscovery agent
*Effort: XL · multi-wk · Depends on: M9, M15*

Extend the durable forwarder into a node agent that autodiscovers and tails container logs (Docker/CRI JSON files) and enriches each line with Kubernetes metadata, so one DaemonSet collects a whole cluster.

### M50 — Data reprocessing / replay pipeline
*Effort: L · 3–6 wks · Depends on: M14, M15, M22, M49*

Replay already-stored or S3-archived raw events back through the (possibly updated) ingest-time parsing pipeline to re-extract fields and re-apply redaction/enrichment, without re-shipping from sources.


## Scale & Storage

_Take the Store seam from one SQLite file to Splunk-scale economics: hardened multi-backend interface, per-index retention, hot/warm tiering, S3 cold archival with rehydration, rollups, performance tuning, compression, and a ClickHouse backend._

### M19 — Store interface hardening for multi-backend & benchmarking baseline
*Effort: M · 1–2 wks · Depends on: v1, M3*

Finalize the backend-agnostic Store contract and stand up a reproducible benchmark/load-testing harness with golden EXPLAIN QUERY PLAN files, establishing the baseline numbers every later storage and performance milestone is measured against.

### M20 — SQLite read concurrency, FTS & cost-guard tuning
*Effort: L · 3–6 wks · Depends on: M19*

Lift the single-connection read ceiling, tune the schema for real query shapes, and bound the worst case so a single broad query cannot stall the server. Removes the MaxOpenConns(1) bottleneck, the TEXT-id FTS join, and the always-on full COUNT(*).

### M21 — Per-index retention, tiering policies & rollups
*Effort: L · 3–6 wks · Depends on: M3, M19*

Introduce a first-class index/dataset concept with per-index retention, size caps, and compression, replacing the single global retention knob, and precompute time-bucketed rollups so histograms and facets over long ranges answer from small tables.

### M22 — Hot/warm tiering & cold S3 archival with rehydration
*Effort: XL · multi-wk · Depends on: M21, M49*

Implement a tiered local store where recent data stays in a fast hot tier and aged partitions roll to a compressed warm tier, then export sealed segments to S3-compatible object storage as a cold tier that rehydrates on demand. This is the mechanism for cheap months-to-years retention on one node.

### M23 — Compression & cost-per-GB observability
*Effort: L · 3–6 wks · Depends on: M20, M19*

Cut storage cost and make it visible: compress large text payloads transparently, measure ingested-vs-stored bytes and compression ratio, and attribute cost per service/source to enable capacity planning toward Splunk-scale.

### M24 — Adaptive backpressure & cardinality controls
*Effort: M · 1–2 wks · Depends on: M19, M23*

Tune the ingest path under sustained load and protect memory from cardinality explosions: latency/saturation metrics, adaptive batch sizing, high-cardinality guards on facets and the FTS blob, and high/low-water-mark 429 behavior.

### M25 — ClickHouse Store backend behind the interface
*Effort: XL · multi-wk · Depends on: M19, M21*

Provide a ClickHouse implementation of the hardened Store interface as the high-throughput columnar option for large deployments, translating the query language and stats to MergeTree partitioning, TTL-based tiering, and native S3 cold storage, while keeping the SQLite path intact for small users.

### M46 — Horizontal fan-out + leader election & replication (HA)
*Effort: XL · multi-wk · Depends on: M2, M7, M19, M20, M21, M22*

Take Omni-logging from single-node toward Splunk-scale and real HA: a stateless aggregator that fans search/stats/tail across multiple ingest+store nodes and merges results, then leader election plus per-shard replication and failover so a node loss neither loses data nor double-runs maintenance jobs.

### M49 — Shared object-store abstraction & archival manifest
*Effort: M · 1–2 wks · Depends on: M19*

Factor a single S3/object-store client and a versioned archival manifest format used by both cold tiering and outbound export, so they don't diverge into two incompatible implementations.


## Analytics & Alerting

_Turn search into an analytics and detection engine: richer query operators, pagination/export, an aggregation pipeline, saved searches, async jobs and caching, dashboards with charts, and a full alerting stack from rules and channels through dedup, streaming detection, and anomaly/pattern mining._

### M26 — Richer query operators, pagination & export
*Effort: L · 3–6 wks · Depends on: v1*

Extend the AND-only filter grammar with comparison/regex/wildcard/exists/IN operators and OR-grouping, add keyset pagination for deep stable result browsing, and stream full results to CSV/JSON/NDJSON downloads decoupled from the UI cap.

### M27 — Aggregation pipeline (stats/timechart/top/rare)
*Effort: XL · multi-wk · Depends on: M26*

Introduce a piped command stage after the search filter (... | stats count by service) with group-by and aggregation functions, turning Omni-logging from a filter tool into an analytics engine. This is the single biggest gap versus Splunk and the foundation for dashboards and alerting.

### M28 — Saved searches, query history & async jobs with caching
*Effort: L · 3–6 wks · Depends on: M26, M27*

Persist named queries and visualization definitions with an automatic recent-query history, run heavy searches/aggregations as cancellable background jobs, and content-address-cache identical results so dashboards and re-runs stay responsive. This is the reuse and performance layer dashboards and alerts build on.

### M29 — Dashboards: charts, builder, variables, sharing & scheduled reports
*Effort: XL · multi-wk · Depends on: M27, M28*

Add a dependency-free client-side charting layer and a grid dashboard builder with variables, a shared time picker, and drill-down, then make dashboards/panels shareable via signed read-only links and embeddable iframes, with server-side scheduled PDF/email reports reusing the embed render path.

### M30 — Alert rules, scheduler & notification channels
*Effort: L · 3–6 wks · Depends on: M28*

Add a persisted alert-rule store and a scheduled evaluation loop running threshold/ratio conditions against the query engine, plus pluggable notification channels (webhook, Slack, email, PagerDuty) with templated payloads and reliable retrying delivery for firing/resolved transitions.

### M31 — Alert dedup, grouping, silences & streaming detection
*Effort: M · 1–2 wks · Depends on: M30, M47*

Make alerting production-survivable with per-group fan-out, re-notify throttling, silences/maintenance windows, and resolve-hold; and add sub-second detection by evaluating selected rules against the in-memory tail hub with sliding-window counters that reconcile against periodic SQLite queries.

### M32 — Alerting web UI + anomaly detection & pattern clustering
*Effort: XL · multi-wk · Depends on: M30, M31*

Add UI screens to create/edit/triage alert rules, channels, history, and silences, and move beyond static thresholds with dependency-free seasonal/EWMA baselining and Drain-style online log-pattern clustering to surface novel patterns and volume anomalies automatically.

### M51 — Query language specification, versioning & linter
*Effort: M · 1–2 wks · Depends on: M26*

A formal grammar for the query/aggregation language as a first-class public contract: versioning/compatibility policy, deprecation handling, and a query linter/validator (sibling to the OpenAPI spec).

### M52 — Trace correlation & log-derived metrics
*Effort: L · 3–6 wks · Depends on: M11, M27*

Use the trace_id/span_id captured via OTLP to pivot from a log line to its distributed trace, and add a log-to-metric derivation path (counters/gauges extracted from log streams) for unified observability.


## Enterprise & Multi-tenancy

_Make Omni-logging sellable into organizations: real user identity, RBAC with scoped tokens, multi-tenant org/index isolation, tamper-evident audit logging, SSO/OIDC, and data-protection/compliance primitives (masking, erasure, secrets hygiene)._

### M33 — Identity & user management foundation
*Effort: L · 3–6 wks · Depends on: v1*

Replace the single static admin token with real user identities: a users table, argon2id passwords and/or token login, signed server-side sessions with CSRF, and admin CLI/UI to manage accounts. This is the substrate every RBAC, audit, and tenancy feature builds on.

### M34 — RBAC: roles, permissions & scoped API tokens
*Effort: L · 3–6 wks · Depends on: M33*

Introduce a role/permission model (predefined plus custom roles) enforced in middleware, and replace flat ingest/admin keys with managed, hashed-at-rest API tokens carrying explicit scopes, expiry, and revocation. Coarse all-or-nothing access is the biggest gap versus Splunk.

### M35 — Multi-tenant isolation (orgs & indexes)
*Effort: XL · multi-wk · Depends on: M34, M21*

Add a tenancy layer of organizations and named indexes so logs are partitioned and every read, write, stat, and tail is constrained to the caller's authorized tenants, enforced at the query layer rather than just the UI. This is the architectural fork that lets one deployment serve multiple teams/customers.

### M36 — Tamper-evident audit logging
*Effort: M · 1–2 wks · Depends on: M34*

Record every security-relevant and data-access action (logins, searches, ingest-key use, role/token/retention changes, exports) to an append-only, hash-chained, queryable audit trail surfaced in the UI for users with audit:read. SOC2/GDPR and enterprise buyers require a defensible who-did-what trail.

### M37 — SSO via OIDC (SAML follow-on)
*Effort: L · 3–6 wks · Depends on: M33, M34*

Allow enterprises to authenticate through their IdP (Okta, Entra, Google, Auth0) using OIDC Authorization Code + PKCE, mapping IdP groups/claims onto Omni-logging roles and tenants, with local break-glass admin and a documented SAML next step.

### M38 — Data protection: field masking, secrets handling & compliance
*Effort: L · 3–6 wks · Depends on: M34, M35, M36*

Add role-aware data masking of sensitive fields, hardened secrets handling for tokens/keys, and the compliance primitives (GDPR erasure, retention proofs, encryption-at-rest guidance) needed for SOC2/GDPR. Logs are where PII and secrets leak; this turns a compliance blocker into a compliance story.

### M53 — Usage metering, ingest-volume accounting & licensing primitives
*Effort: M · 1–2 wks · Depends on: M33, M35*

Per-tenant/per-day ingest-volume metering with a license model (volume- or node-based), enforcement/grace behavior, and exportable usage reports for chargeback and commercial licensing.


## Ecosystem & Distribution

_Meet users where they already are and make deployment frictionless: OpenAPI spec and CLI query tool, client SDKs and logging-framework appenders, outbound export/forward sinks, importers/migration helpers, a Grafana datasource plugin, OS packaging, Helm chart, docs/seed data, and self-update/Terraform._

### M39 — Public OpenAPI spec + CLI query tool
*Effort: M · 1–2 wks · Depends on: v1*

Publish a versioned OpenAPI 3.1 document for the ingest/search/stats/tail API, serve it from the binary with embedded docs, and ship an omnilog query subcommand for terminal searches with table/JSON/NDJSON output and SSE follow mode. A stable contract is what every SDK, plugin, and integration depends on.

### M40 — Client SDKs + logging-framework appenders (Go/Python/JS)
*Effort: L · 3–6 wks · Depends on: v1, M39*

Ship thin, dependency-light client libraries plus native handlers for the dominant logging frameworks (slog, structlog, pino/winston) so apps emit to Omni-logging with one line of config, removing the forwarder as a hard dependency.

### M41 — Outbound export & forward pipeline (S3, Kafka, webhooks)
*Effort: L · 3–6 wks · Depends on: v1, M47, M49*

Add an outbound side to Omni-logging: continuously archive ingested events to object storage for cheap retention and fan out a filtered stream to Kafka and HTTP webhooks, turning Omni-logging from a sink into a routable node in a larger pipeline.

### M42 — Importers & migration helpers (files, Splunk/Elastic)
*Effort: M · 1–2 wks · Depends on: v1, M41, M49*

Provide one-shot import tooling to backfill historical data from log files and from existing Splunk/Elasticsearch deployments, with auto-detection of common formats, so teams can evaluate Omni-logging against their real data and convert an evaluation into a migration.

### M43 — Grafana data source plugin
*Effort: M · 1–2 wks · Depends on: v1, M39*

Build a Grafana backend datasource plugin that queries Omni-logging's search/stats API, letting teams reuse Grafana dashboards and Explore for log search and timeseries against Omni-logging. Grafana is where most ops teams already live.

### M44 — OS packaging, Helm chart & deployment docs
*Effort: L · 3–6 wks · Depends on: M8, M6*

Make Omni-logging installable as a first-class OS service and on Kubernetes: hardened systemd units, .deb/.rpm/Homebrew/Scoop packages, an official Helm chart, generated config-reference docs, getting-started tutorials, and a seed-data generator for instant demos.

### M45 — Self-update channel + Terraform module
*Effort: L · 3–6 wks · Depends on: M8, M44, M6*

Provide a built-in updater for binary installs and a Terraform module for cloud VM deployments, so both hobbyist and infra-as-code users stay current without manual steps, with migration-aware version-skew safety.

### M54 — UI accessibility (WCAG 2.1 AA) & internationalization
*Effort: M · 1–2 wks · Depends on: M29*

Make the web UI keyboard-navigable and screen-reader compatible to WCAG 2.1 AA (focus management for the dashboard builder and charts), and extract user-facing strings for i18n/localization.

### M55 — Dark mode & theming (light / dark / system)
*Effort: S · days · Depends on: v1*

A dark theme plus a light/dark/system toggle persisted in the browser, honoring `prefers-color-scheme`. The UI already drives every color through CSS custom properties, so this is a second variable set and a small toggle — high comfort payoff for an interface people stare at for hours. Prioritized **P0**.


## Cross-cutting sequencing notes

These are coordination concerns that span several milestones — worth deciding early even though they aren't milestones themselves:

- **Index/org dimension is a retrofit risk.** M21 introduces a per-`index` dimension and M35 adds `org_id`/scoped tokens, but M4 (per-key quotas), M14 (pipeline config), and M34 (token scoping) key off `source`/`service` and predate it. Either land index-awareness earlier or plan the retrofit of those config/scoping models.
- **Two layers of masking.** M16 redacts at *ingest* (irreversible) while M38 masks at *query/render* time (role-based). Decide explicitly what is dropped at ingest vs. retained-but-masked before building either.
- **One search-projection seam, three editors.** M17 (derived fields), M35 (tenancy filtering), and M38 (query-time masking) all mutate `Store.Search` projection. Design that seam once so they compose instead of fighting.
- **Schema-on-read vs. the query engine.** M17's virtual fields should be referenceable by the richer operators (M26) and aggregations (M27); thread the field catalog through the query AST in one pass rather than two.
