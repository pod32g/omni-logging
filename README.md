# Omni-logging

A self-contained **centralized logging system** — conceptually similar to Splunk —
that ships as a single Go binary with the web UI embedded. Apps ship logs over
HTTP; logs are stored and full-text indexed in SQLite; you search, filter,
aggregate, and live-tail through a web UI and a JSON API. Zero external services.

## Features (v1)

- **HTTP ingestion** — POST structured (NDJSON / JSON) or raw text logs, optionally gzip/deflate compressed.
- **OTLP receiver** — OpenTelemetry logs over **both** transports: OTLP/HTTP at `/v1/logs` (protobuf and JSON encodings) and OTLP/gRPC on an optional `:4317` listener, so an OTel SDK or Collector can point straight at it either way. The gRPC service is implemented directly on HTTP/2 rather than via `grpc-go`, keeping the dependency tree small.
- **Syslog collector** — optional RFC5424/RFC3164 listener over UDP and TCP, so containers, daemons and network gear can ship logs with no agent and no code changes. Off by default.
- **Parsing pipelines** — ordered grok/regex/timestamp stages applied at ingest, turning unstructured text into searchable fields. Scoped with the ordinary query language, editable at runtime, testable against a sample line before you save.
- **Storage + full-text index** — SQLite with FTS5; time/field indexes; retention.
- **Search** — free-text, field filters (`level=error service=api`), time ranges.
- **Aggregations** — a piped analytics stage (`| stats count by service`, `timechart`, `top`, `rare`), plus the counts-over-time histogram and field facets.
- **Alerting** — scheduled rules over any search or aggregation, with threshold conditions, per-rule severity, and notifications on state transitions via webhook, Slack, or [Omni-Notify](https://github.com/pod32g/omni-notify) (which handles dedup, routing and delivery to Discord/Telegram/SMTP).
- **Live tail** — real-time streaming of matching events (SSE), seeded with the last 50 matching events so the pane is useful the moment it opens rather than blank until the next log arrives.
- **Web UI** — search, histogram, facets, expandable rows, live tail, paginated results + export, and a light/dark/system theme toggle.
- **Forwarder** — `omnilog forward` tails files and ships them to the server, with an optional durable spool for at-least-once delivery across restarts and outages.
- **Client SDKs** — dependency-free Go, Python and JavaScript clients with native `slog` / `logging` / pino integrations, so an app can emit directly without the forwarder ([`sdk/`](sdk/)).
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
| `forward` | File-tailing forwarder client (durable spool) |
| `pipeline` | Grok/regex extraction, timestamp parsing, ingest-time transforms |
| `alert` | Rule evaluation, scheduling and notification delivery |
| `syslog` | RFC5424/RFC3164 parser + UDP/TCP collector |
| `otlp` | OpenTelemetry logs receiver: HTTP (protobuf + JSON) and gRPC, both hand-rolled |

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

## Durable forwarding

By default the forwarder is best-effort: a batch it cannot deliver is retried a
few times and then lost. Point it at a spool directory to make delivery
at-least-once instead:

```sh
omnilog forward --server http://HOST:8080 --api-key devkey \
  --service api --file /var/log/app.log --spool-dir /var/lib/omnilog-forward
```

- Every batch is written to a CRC-checked on-disk queue **before** it is sent,
  and removed only once the server accepts it. A restart, a crash or a server
  outage resumes where it left off.
- Transient failures (network, `429`, `5xx`) retry indefinitely with capped
  backoff, because the data is safe on disk — giving up would discard something
  still queued. Without a spool the retry budget stays bounded, since the batch
  exists only in memory.
- A batch the server refuses **permanently** (a bad API key, a malformed
  request) goes to `dead-letter.ndjson` in the spool directory with its lines
  and the reason, so it can be inspected and replayed by hand rather than
  disappearing into a log line.
- Each batch carries an `X-Batch-Id`, reused across retries. The server
  remembers recently-seen batch IDs and answers a repeat with
  `{"duplicate": true}` instead of storing the events twice.

> Delivery is **at-least-once**, not exactly-once. Event IDs are assigned by the
> server so a producer cannot overwrite existing history, which means they
> cannot double as a de-duplication key; the batch ID handles the common case
> (a retry seconds later) and its window is in-memory and time-bounded. A
> re-send hours later, or after a server restart, can still duplicate.

The spool reuses `internal/wal` rather than introducing a second append-only
log: CRC-checked records, torn-tail recovery, segment rotation and a checkpoint
are exactly what "keep this until it is acknowledged" needs.

## Client SDKs

Apps can emit directly, without the file forwarder in between. See
[`sdk/`](sdk/) for all three.

```go
client, _ := omnilog.New(omnilog.Options{ServerURL: "http://logs:8080", Service: "api"})
defer client.Close()
slog.SetDefault(slog.New(omnilog.NewHandler(client, nil)))
slog.Error("payment failed", "status", 402)   // -> attr.status>=400
```

```python
logging.getLogger().addHandler(
    omnilog.OmnilogHandler(server_url="http://logs:8080", service="api"))
```

```js
const log = new Omnilog({ serverUrl: 'http://logs:8080', service: 'api' });
log.error('payment failed', { status: 402 });
```

All three batch on a background worker so a logging call never blocks on the
network, drop-and-count rather than block when the queue fills, expose
`sent`/`failed`/`dropped` counters, and optionally gzip. None of them has a
dependency outside its language's standard library.

## Compression

Every ingest endpoint accepts a `Content-Encoding` of `gzip`, `x-gzip` or
`deflate`, decompressed transparently before parsing. Log lines compress
extremely well, so this is mostly free bandwidth:

```sh
gzip -c batch.ndjson | curl -XPOST http://HOST:8080/api/v1/ingest \
  -H 'X-Api-Key: devkey' -H 'Content-Type: application/x-ndjson' \
  -H 'Content-Encoding: gzip' --data-binary @-

# the forwarder can do it for you
omnilog forward --server http://HOST:8080 --file /var/log/app.log --compress
```

An encoding the server cannot decode is answered with `415` naming it, rather
than failing later on bytes the parser cannot read.

> Decompression is bounded at 64 MiB of **output**. A size cap on the
> compressed bytes is not a memory bound: a few kilobytes of gzip can expand to
> gigabytes, so the limit has to apply to what comes out.

## OpenTelemetry (OTLP)

Both OTLP transports are supported: **HTTP** on the main port and **gRPC** on
its own.

### OTLP/HTTP

`/v1/logs` is the standard OTLP/HTTP path, so pointing an SDK or Collector at
the server needs no special configuration:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://HOST:8080
export OTEL_EXPORTER_OTLP_HEADERS="x-api-key=devkey"
```

Both the **protobuf** and **JSON** encodings are accepted, gzipped or not. An
unrecognised `Content-Type` is treated as protobuf, since that is what
exporters default to and some omit the header.

### OTLP/gRPC

Off by default. Give it an address and it listens on the conventional port:

```sh
omnilog serve --otlp-grpc :4317          # or OMNILOG_OTLP_GRPC_ADDR=:4317
```

```sh
export OTEL_EXPORTER_OTLP_PROTOCOL=grpc
export OTEL_EXPORTER_OTLP_ENDPOINT=http://HOST:4317
export OTEL_EXPORTER_OTLP_HEADERS="x-api-key=devkey"
```

It is a second listener rather than a route on the main server because gRPC
requires HTTP/2, and the main server only speaks HTTP/2 when TLS is configured.
The gRPC port serves **h2c** (cleartext HTTP/2), which is what a collector's
`insecure` channel expects, so it works without certificates. Per-message
`gzip` is accepted and advertised via `grpc-accept-encoding`.

**Ingest keys apply here exactly as they do to `/v1/logs`** — sent as gRPC
metadata, either `x-api-key` or `authorization: Bearer`. Turning this listener
on cannot open an unauthenticated way in past keys you have already configured.
Keys are read live, so one added in the UI takes effect without a restart.

> **This is real gRPC, without `grpc-go`.** gRPC is HTTP/2 plus three
> conventions: a `/package.Service/Method` path, messages prefixed with a
> compression flag and a big-endian length, and the call's outcome in HTTP
> trailers rather than the HTTP status. Since this package already decodes OTLP
> protobuf by hand, implementing those directly costs two modules
> (`golang.org/x/net`, `golang.org/x/text`) where `grpc-go` would have cost
> twelve. The test suite verifies the wire format with a real HTTP/2 client,
> and it is checked against a stock `grpc-go` + upstream-OTLP client, which
> connects without knowing the difference.

Mapping: `service.name` and `host.name` resource attributes become the event's
service and source, the OTLP severity number becomes the level (falling back to
`severityText` when the number is absent), the body becomes the message, and
trace/span IDs become searchable attributes. Resource attributes are kept as
attributes too. Rejected records are reported in the response's
`partialSuccess` so a collector retries those rather than resending everything.

> The protobuf decoder is written by hand against the OTLP wire format rather
> than generated from the `.proto` files. That keeps the dependency tree as it
> is — one SQLite driver and a YAML parser — and unknown fields are skipped, so
> a newer OTLP version does not break the receiver.

## Parsing pipelines

Raw text becomes searchable fields at ingest time. A pipeline is a match
expression plus ordered stages:

```sh
curl -XPOST localhost:8080/api/v1/pipelines -H 'Content-Type: application/json' -d '{
  "name": "nginx access",
  "match": "service=nginx",
  "enabled": true,
  "stages": [
    {"type":"grok","pattern":"%{IP:client} \\S+ \\S+ \\[%{HTTPDATE:ts}\\] \"%{HTTPVERB:method} %{NOTSPACE:path} %{HTTPVER}\" %{INT:status} %{INT:bytes}"},
    {"type":"timestamp","field":"attr.ts"},
    {"type":"remove","fields":["ts"]}
  ]
}'
```

That line then answers `attr.status>=500`, `| stats count by attr.method`, and
anything else the query language can express.

- **Stages** — `grok`, `regex`, `timestamp`, `level`, `service`, `rename`,
  `remove`, `set`.
- **Match** is the ordinary search expression, so `service=nginx` means the same
  thing here as in the search box. Empty applies to every event.
- **Captures named after a first-class field land there**: `%{LOGLEVEL:level}`
  sets the event level, not an attribute. Everything else becomes an attribute,
  with numeric-looking values stored as numbers so comparisons work.
- **A stage that does not match is skipped, not fatal.** A pattern failing on a
  particular line is normal; losing the line would be far worse than storing it
  unenriched. Set `"fail_pipeline": true` on a stage to opt out of that.
- **Test before you save** — `POST /api/v1/pipelines/test` runs a sample line
  through unsaved pipelines and shows the resulting event. Grok is unforgiving
  enough that writing a pattern without trying it is guesswork.
- Pipelines compile on save, so a bad pattern is a `400` then, not a silent
  failure on every event afterwards. `GET /api/v1/pipelines` also returns the
  built-in grok pattern names.

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
- **Severity** — `critical`, `error`, `warning` (default), `info`, `debug`. It
  is independent of state: severity is how bad the rule is, state is whether it
  is firing right now. Downstream routing matches on it.
- **Channels** are `webhook` (receives the full JSON payload), `slack`
  (receives `{"text": ...}`) or `omni-notify` (see below). Redirects are
  deliberately not followed, so a payload cannot be diverted to a host you did
  not configure.

> Alert channels make the server issue outbound HTTP requests to URLs you
> supply. The endpoints are behind the admin token for that reason; treat the
> ability to create a channel as equivalent to outbound request access.

### Omni-Notify

[Omni-Notify](https://github.com/pod32g/omni-notify) is a notification router:
it deduplicates, routes and delivers to Discord, Telegram, Slack, SMTP and
webhooks, with a durable queue and retries — and it deliberately does not
evaluate rules. That is the complementary half of what this server does, so an
`omni-notify` channel hands off at exactly that seam rather than growing a
provider zoo and a retry queue here.

```sh
curl -XPOST localhost:8080/api/v1/alerts/channels -H 'Content-Type: application/json' \
  -d '{"name":"homelab","type":"omni-notify",
       "url":"http://omni-notify:8088","token":"<OMNI_NOTIFY_API_TOKEN>"}'
```

The URL is the Omni-Notify **base** URL; `/api/v1/events` is appended if you
leave it off. The token is its `OMNI_NOTIFY_API_TOKEN`, sent as
`Authorization: Bearer`. It is **write-only**: it reads back masked, and posting
the mask back is rejected rather than stored.

Each transition becomes one Omni-Notify event:

| Omni-Notify field | From |
|---|---|
| `event_id` | the rule's ID |
| `type` | always `alert` |
| `source` | always `omni-logging` |
| `status` | `firing` / `resolved` |
| `severity` | the rule's severity |
| `title` / `summary` / `description` | rule name, threshold line, full text with the breaching groups |
| `labels` | `rule`, `rule_id` — **identity only** |
| `annotations` | `query`, `condition`, `window`, `value`, `groups`, `top_group` |

Route them on the Omni-Notify side with `source: omni-logging`, or by
`severity`, or per-rule with `labels.rule`.

> **Why labels carry nothing but identity.** Omni-Notify's dedup key is
> `sha256(type | source | event_id | sorted labels)`. If a label held the
> observed value or which groups broke, the resolved event would fingerprint
> differently from the firing one, Omni-Notify would treat it as a resolve for
> something that never fired, suppress it — and the alert would stay active
> there forever. Everything that varies between evaluations goes in
> annotations, which are still matchable for routing but sit outside the
> fingerprint.

Omni-Notify can also tee **its own logs** back here, so the two compose in both
directions: it ships logs to `/api/v1/ingest`, this server ships alerts to its
event API. That makes its delivery failures alertable like anything else:

```
service=omni-notify level=error "delivery permanently failed" | stats count
```

> Route that one somewhere else. A rule about Omni-Notify being unwell, notified
> *through* Omni-Notify, is the one alert guaranteed not to arrive — give it a
> `slack` or `webhook` channel instead.

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
