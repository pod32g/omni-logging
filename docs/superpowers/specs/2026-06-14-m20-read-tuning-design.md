# M20 — SQLite read concurrency, FTS & cost-guard tuning

*Roadmap M20 · Effort L · Depends on M19. Measured against the M19 baselines.*

## Delivered
- **Fast FTS join.** The free-text search join switched from the slow TEXT-id join
  (`logs_fts.id = logs.id`, a TEXT primary-key lookup per match) to an integer
  rowid join (`logs_fts.rowid = logs.rowid`). M3 already wrote aligned rowids;
  **migration v2** (a Go-backed data migration) rebuilds the FTS index so any
  pre-M3 rows align too — idempotent, recomputing the searchable text exactly as
  Append does. Golden plan now shows `SEARCH logs USING INTEGER PRIMARY KEY
  (rowid=?)`. Benchmark: free-text **~58ms → ~25ms (2.3x)** on the 20k-row set.
- **Cost guard.** Interactive reads (Search/Stats) run under a `readTimeout`
  (30s) so a single broad query (huge COUNT / wide FTS) cannot stall the server.
  Exports (Stream) are exempt — they are expected to run long.
- **Migration machinery** extended to support Go data-migration steps (`fn`),
  reused by future schema changes.

## Deferred (documented)
- **Multi-connection read pool** (removing `MaxOpenConns(1)`). For a single-node,
  largely single-user tool the contention win is modest, and a read/write pool
  split needs careful `:memory:` shared-cache handling (test-heavy). Left as a
  follow-up; the cost-guard already bounds the worst case.
- **Bounded/optional COUNT(*)** — the read timeout bounds its worst case; making
  Total approximate would change UI semantics, so deferred.

## Testing
Migration v2 rebuild test (misaligned FTS rowids → aligned, free-text still
correct); golden plan regenerated; full `-race` suite green; benchmark recorded.
