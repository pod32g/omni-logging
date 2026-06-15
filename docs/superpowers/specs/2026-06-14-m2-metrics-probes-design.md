# M2 — Self-monitoring: Prometheus metrics + readiness/liveness probes

*Roadmap M2 · Effort M · Depends on v1. Unblocks M4 (quotas), M47 (tail hub), and
much of P1's reliability/scale work.*

## Goal

Expose a `/metrics` endpoint in Prometheus text-exposition format covering ingest,
store query latency, the live-tail hub, and HTTP requests; and split the v1 health
stub into a real **liveness** probe (process up) and **readiness** probe (backend
reachable) so a load balancer / orchestrator can tell "alive but not ready" from
"dead".

## Constraints / decisions

- **Hand-rolled metrics, no `prometheus/client_golang`.** The project's identity is
  a single pure-Go binary with a tiny dependency tree (only `modernc.org/sqlite`,
  `yaml.v3`, `uuid`). Pulling in client_golang's large transitive tree contradicts
  that. A minimal in-repo `internal/metrics` package emits the same wire format
  Prometheus scrapes. This is the metrics foundation every later milestone reuses.
- **Endpoints stay unauthenticated**, consistent with the existing open
  `/api/v1/healthz`. Health/readiness must be reachable by infra probes; `/metrics`
  is an operational endpoint scraped on a trusted network. (A future auth posture
  can be added without changing the surface.)
- **Back-compat:** `GET /api/v1/healthz` keeps its current shape and always-200
  behavior — the container HEALTHCHECK and the CI/CD deploy probe both depend on it.
  Readiness is **added** alongside, not a breaking rename.

## API surface

| Method/path            | Auth | Meaning |
|------------------------|------|---------|
| `GET /api/v1/healthz`  | open | **Liveness** — process is up. Always 200 (unchanged). |
| `GET /api/v1/readyz`   | open | **Readiness** — store reachable. 200 when ready, 503 + JSON reason when not. |
| `GET /metrics`         | open | Prometheus text exposition (`text/plain; version=0.0.4`). |

`/metrics` and `/readyz` are registered next to `healthz` in `Server.Handler()`;
the more specific patterns coexist with the `/` UI file server under Go 1.22 mux.

## `internal/metrics` package

A small, dependency-free registry that renders the Prometheus text format.

```go
type Registry struct { ... }
func NewRegistry() *Registry

// Observed collectors (updated by the app during requests):
func (r *Registry) NewCounter(name, help string, labelNames ...string) *CounterVec
func (r *Registry) NewHistogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec

// Pull collectors (read live values from existing atomics at scrape time, so we
// don't double-bookkeep the ingest/tail counters that already exist):
func (r *Registry) NewCounterFunc(name, help string, f func() float64)
func (r *Registry) NewGaugeFunc(name, help string, f func() float64)

func (r *Registry) WriteProm(w io.Writer) error   // full exposition

type CounterVec   struct{...}; func (v *CounterVec) With(lvs ...string) *Counter
type Counter      struct{...}; func (c *Counter) Inc(); func (c *Counter) Add(f float64)
type HistogramVec struct{...}; func (v *HistogramVec) With(lvs ...string) *Histogram
type Histogram    struct{...}; func (h *Histogram) Observe(v float64)
```

Format rules implemented: one `# HELP`/`# TYPE` per metric family; labels sorted by
name; label values escaped (`\`, `"`, `\n`); histograms emit cumulative `_bucket{le=...}`
series plus `_sum` and `_count`, including a `le="+Inf"` bucket. Counters/Add reject
negative deltas. All operations are goroutine-safe (atomics + a mutex guarding the
vec child maps).

## Metrics exposed

- `omnilog_ingest_received_total`, `omnilog_ingest_written_total`,
  `omnilog_ingest_dropped_total` — CounterFunc reading `Ingestor` atomics.
- `omnilog_ingest_queued` — GaugeFunc (`len(ch)`).
- `omnilog_tail_subscribers` — GaugeFunc (`Hub.SubscriberCount()`).
- `omnilog_tail_dropped_total` — CounterFunc (new aggregate drop counter on `Hub`).
- `omnilog_store_query_duration_seconds{op="search|stats"}` — Histogram observed in
  the search/stats handlers from `res.TookMs` (keeps the store package metrics-free).
- `omnilog_http_requests_total{method,code}` and
  `omnilog_http_request_duration_seconds{method,code}` — observed by a new metrics
  middleware. No `path` label (raw paths would be unbounded). `method` is normalized
  to the standard HTTP verb set (anything else → `"other"`) because the endpoints are
  unauthenticated and the raw request method is attacker-controlled — without this an
  attacker could grow the series set without bound. Recording runs in a `defer` so a
  panicking handler is still counted as a `500` before `recoverMiddleware` responds.
- `omnilog_build_info{version="..."}` gauge = 1 — handy constant for dashboards.

## Wiring

- `internal/metrics.Registry` is created in `cmd/omnilog` `runServe`, populated with
  the pull collectors (ingestor/hub), and passed via `api.Deps.Metrics`.
- `api.Server` holds the registry; `Handler()` adds the metrics middleware around the
  mux and registers `/metrics`, `/readyz`.
- `store.Store` gains `Ping(ctx context.Context) error`; sqlite implements it as
  `db.PingContext`. Readiness pings the store with a short timeout. (This is the first
  step of the M19 interface hardening.)

## Testing (TDD)

- `internal/metrics`: table-driven exposition tests — counter, counter-with-labels,
  histogram bucket math + `+Inf` + sum/count, label escaping/sorting, negative-Add
  guard, func collectors, concurrent `Inc`/`Observe` under `-race`.
- `internal/api`: `/metrics` returns 200 `text/plain` and contains expected family
  names after a search + an ingest; `/readyz` returns 200 with a live store and 503
  when the store is closed; `/healthz` unchanged; HTTP counters increment.
- `internal/store/sqlite`: `Ping` succeeds on an open DB.
- `internal/tail`: `Hub` aggregate dropped counter increments when a sub buffer fills.

## Definition of done

`gofmt`/`go vet`/`go test ./...` green (incl. `-race` on metrics); binary builds;
`/metrics`, `/readyz`, `/healthz` verified against a running server; README documents
the three endpoints. No new third-party dependency; no internal host details.
