# M47 — Live tail hub hardening

*Roadmap M47 · Effort M · Depends on v1, M2. Unblocks redaction/streaming alerts/export.*

Promotes the in-memory tail hub to an observable fan-out layer:
- **Bounded buffers** (already per-subscriber channel capacity; SSE uses 256).
- **Slow-consumer eviction**: a subscriber that drops more than the hub threshold
  (`DefaultMaxDrops`, configurable via `NewHubLimit`) is evicted — its channel
  closed and removed — so one stuck client can't accumulate unboundedly. Eviction
  happens after the publish read-lock is released (collect-then-evict) to avoid a
  send-on-closed-channel race. The SSE handler emits a `: evicted` comment and
  returns; the browser `EventSource` reconnects.
- **Metrics**: `omnilog_tail_subscribers` (gauge), `omnilog_tail_dropped_total`,
  and new `omnilog_tail_evicted_total` (counters).

TDD: eviction past threshold (channel closed, count 0, Evicted() true, metric);
no eviction under the default threshold; existing drop/unsubscribe tests unchanged.
Verified with `-race`.
