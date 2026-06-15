# M4 — Structured logging + ingest rate limiting & per-key quotas

*Roadmap M4 · Effort M · Depends on M2. Replaces the all-or-nothing buffer-full
429 with fair per-key admission control.*

## Delivered
- **`internal/admission`**: a per-key token-bucket rate limiter + daily
  event/byte quotas (UTC-day reset), injectable clock, race-tested. `Allow(key,
  bytes)` returns a `Decision{Allowed, Reason}` (rate / bytes_quota /
  events_quota) and consumes a rate token on success; `Record(key, events,
  bytes)` attributes actual usage. Zero limits = disabled (Allow always true).
- **Ingest integration**: `requireIngestKey` puts the matched key in the request
  context; both ingest handlers call `admit()` before processing (429 + JSON
  `{error, reason}` on rejection) and `recordUsage()` after, attributing to the
  key. A new `rejected` counter + `omnilog_ingest_rejected_total` metric.
- **Request IDs**: a `requestIDMiddleware` assigns/echoes `X-Request-Id` and
  threads it through the context; the access log includes `request_id`.
- **Configurable log level**: `log_level` (config/env), applied to slog at
  startup. Config + env: `rate_limit_per_sec`, `rate_burst`, `daily_quota_events`,
  `daily_quota_bytes`, `log_level` (and `OMNILOG_*` equivalents).

## Scope notes
Limits are global defaults enforced **independently per key** (each key has its
own bucket/counters). Per-key *custom* limits are a follow-up. Rate limiting is
per-request (anti-flood); volume is bounded by the daily byte/event quotas.

## Testing
Limiter unit tests (rate refill, byte/event quotas, daily reset, per-key
isolation, disabled) under `-race`; an API integration test driving the full
X-Api-Key → context → admit → 429 path and asserting the reason + request-ID
header. Full `-race` suite green.
