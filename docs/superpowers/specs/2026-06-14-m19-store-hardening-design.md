# M19 — Store-interface hardening + benchmarking baseline

*Roadmap M19 · Effort M · Depends on v1, M3. Establishes the baseline numbers and
the query-plan golden files every later storage/perf milestone (M20, M21, M23…)
is measured against. **No query behavior changes** — that is M20.*

## Goals

1. **Finalize the `store.Store` contract** — document the guarantees the
   conformance suite (M3) already enforces, so a new backend knows exactly what
   "correct" means: append idempotency by ID, newest-first default ordering,
   `Total` ignoring the limit, purge semantics, ping, error handling.
2. **Benchmark harness** — reproducible `go test -bench` baselines for append and
   search/stats, so M20's read-concurrency/FTS work has numbers to beat.
3. **Golden `EXPLAIN QUERY PLAN`** — capture the query plan for each representative
   query shape into committed golden files, with a regression test. If a future
   change (e.g. M20) makes a query stop using an index, the golden test flags it.

## Changes

### Tighten the contract (`internal/store/store.go`)
Expand the `Store` interface doc comments to state, per method: Append is
idempotent per `LogEvent.ID` (re-appending replaces, including the full-text
index — relied on by WAL replay); Search returns newest-first unless
`Order == OrderOldest`, `Count` = returned, `Total` = matches ignoring `Limit`;
Stats buckets by `Interval`; Purge removes `ts < cutoff` and cleans the FTS index;
Ping verifies reachability; all methods honor context cancellation. No signature
changes (the interface is already minimal and backend-agnostic).

### Extract SQL builders (`internal/store/sqlite/search.go`)
Refactor the inline SQL strings in `Search`/`count`/`Stats`/`facet` into small
pure helpers — `searchSQL`, `countSQL`, `histogramSQL`, `facetSQL` — each
returning `(sql string, args []any)`. `Search`/`Stats` call them (behavior
identical, guarded by the existing tests). This makes the exact executed SQL
available to the golden-plan test without duplicating query construction.

### Benchmarks (`internal/store/sqlite/bench_test.go`)
`BenchmarkAppend` (batch insert), `BenchmarkSearchFreeText`,
`BenchmarkSearchLevelFilter`, `BenchmarkSearchAttr`, `BenchmarkStats`, over a
store seeded with N rows via a shared helper. Reported with `go test -bench`.

### Golden query plans (`internal/store/sqlite/explain_test.go` + `testdata/`)
For each shape (free-text, level filter, attr filter, time range, histogram,
facet) run `EXPLAIN QUERY PLAN` on the builder's SQL and compare to a committed
`testdata/<name>.plan` file. A `-update` flag regenerates them. The test asserts
the plan is stable (catches a lost index / new full scan). Plans are normalized
(row ids stripped) to reduce cross-version noise; if SQLite changes the wording,
regenerate with `-update` and review the diff.

### Baseline doc
Record the captured numbers + how to reproduce in the spec's appendix / a short
`docs/` note, so M20 can show the before/after.

## Testing (TDD / regression)
The existing conformance + sqlite tests are the behavior guard: the SQL-builder
refactor must keep them all green (no behavior change). New: golden-plan test
(fails if a plan regresses), benchmarks (smoke-run in CI via `-bench . -benchtime=1x`
is optional; primarily run manually for baselines).

## Baseline numbers (M19, the bar M20+ must beat)

Reproduce: `go test ./internal/store/sqlite -bench 'BenchmarkAppend|BenchmarkSearch|BenchmarkStats' -benchmem -run '^$'`
(SQLite `:memory:`, 20k rows for searches; Apple-silicon dev machine — relative,
not absolute, numbers are what matter).

| Benchmark | ns/op | note |
|---|---:|---|
| Append (100-event batch) | ~3.4 ms | ≈34 µs/event |
| SearchLevelFilter | ~5.9 ms | uses `idx_logs_level` |
| **SearchFreeText** | **~58 ms** | ~10× slower — the FTS `id`-join (`sqlite_autoindex_logs_1 (id=?)` per match); the M20 target |
| SearchAttr | ~8.5 ms | `json_extract` over an `idx_logs_ts` scan |
| Stats (histogram+facets) | ~6.7 ms | covering-index scans |

Golden plans in `internal/store/sqlite/testdata/*.plan` capture the index usage
behind these numbers; the `search_freetext` plan shows the TEXT-`id` FTS join that
M20 removes.

## Definition of done
`gofmt`/`vet`/`go test ./...` green (incl. golden plans); `go test -bench` runs
and produces append/search/stats numbers; golden plan files committed; the
`Store` contract documented. No behavior change; no new third-party dependency.
