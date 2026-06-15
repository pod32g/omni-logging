# M3 — Durable write path: segment WAL + crash recovery + store conformance suite

*Roadmap M3 · Effort L (careful) · Depends on v1, M2. Closes the v1 gap where a
crash between "ingest returns 200" and "batch committed to the store" silently
loses accepted events.*

## The gap

Today `Ingestor.Enqueue` puts an event on an in-memory channel and the HTTP
handler returns `200 accepted`. A background batch writer later commits it. If the
process crashes (panic, OOM, SIGKILL) before the commit, every event still in the
channel — already acked to the client — is lost. We need accepted events to
survive a process crash.

## Design overview

Add an on-disk **write-ahead log** that each accepted event is appended to
*before* `Enqueue` returns. The in-memory channel still feeds the batch writer for
low-latency store writes and live-tail; the WAL is the durability backstop. After
a batch is committed to the store, the WAL **checkpoint** advances past it, which
lets fully-applied segments be deleted. On startup, the WAL is **replayed** from
the checkpoint into the store, recovering anything accepted-but-not-committed.
Replay is idempotent because events carry ULIDs and `Append` is `INSERT OR
REPLACE` keyed on `id` — re-applying an already-stored event is a harmless
overwrite ("ULID-based dedup on replay").

### Crash-safety argument

- **Process crash** (panic/OOM/kill): WAL data written before ack lives in the OS
  page cache and survives process death, so replay recovers it. No fsync needed
  for this case.
- **Power loss / kernel panic**: covered by periodic fsync (`Sync` on each batch
  flush by default; configurable). The window of loss is bounded by the flush
  interval.
- **Checkpoint ordering**: the checkpoint only advances *after* a successful
  `store.Append`, so `checkpoint <= durably-applied` always holds. Replaying
  `(checkpoint, end]` may re-apply a few already-stored events — safe (idempotent).

## `internal/wal` package (new, byte-oriented, no log-domain knowledge)

```go
type Options struct { Dir string; MaxSegmentBytes int64; SyncOnAppend bool }
type WAL struct { ... }
func Open(opts Options) (*WAL, error)
func (w *WAL) Append(payload []byte) (seq uint64, err error)
func (w *WAL) Sync() error
func (w *WAL) Checkpoint() uint64
func (w *WAL) SetCheckpoint(seq uint64) error
func (w *WAL) Replay(fn func(seq uint64, payload []byte) error) error
func (w *WAL) Close() error
```

- **Segments**: append-only files `wal-<startSeq>.log` (20-digit zero-padded),
  rotated when they exceed `MaxSegmentBytes` (default 8 MiB).
- **Record**: `[u64 seq | u32 payloadLen | u32 crc32(payload) | payload]`
  (16-byte header, big-endian). CRC detects a torn final write from a crash.
- **Checkpoint**: `dir/checkpoint` = last-applied seq, 8-byte big-endian, written
  via temp-file + atomic rename.
- **Open/recovery**: read checkpoint; scan segments in order validating CRC;
  `nextSeq = maxValidSeq + 1`; **truncate the active segment** at the end of the
  last valid record so a torn tail never corrupts future appends.
- **Replay**: iterate records with `seq > checkpoint`, in order, stopping a
  segment scan at the first CRC failure / short read (the tail is untrustworthy).
- **SetCheckpoint**: persist, then delete non-active segments fully `<= checkpoint`.
- Appends are serialized by a mutex (a WAL is a single sequential log).

## Ingest integration (`internal/ingest`)

- `Options` gains `WAL *wal.WAL` (nil → in-memory only, the v1 behavior; used for
  `:memory:` DBs and tests that don't need durability).
- `Enqueue(e)` (when WAL set): marshal `e` → JSON; under the enqueue mutex,
  fast-reject (429) if the channel is full *before* touching the WAL; else
  `wal.Append(payload)` then non-blocking channel send (a slot is guaranteed since
  we checked capacity under the lock and the consumer only frees slots). This keeps
  the WAL and the in-flight set consistent: only accepted events are WAL'd.
- Batch writer `flush()`: `wal.Sync()` → `store.Append(batch)` →
  `wal.SetCheckpoint(maxSeq)` → publish to hub.
- `Recover(ctx)` (called once before `Start`): `wal.Replay` → batch into the store
  → advance checkpoint. Returns the count recovered (logged at startup).
- Marshal/unmarshal uses the existing `model.LogEvent` JSON tags (round-trips ID,
  timestamps, attributes).

## Store conformance suite (`internal/store/storetest`)

`func Run(t *testing.T, newStore func(t *testing.T) store.Store)` runs the
backend-agnostic contract as subtests: append+search by level/free-text/attr,
**idempotent append by ID** (the dedup contract), time-range/order/limit, stats
histogram+facets, purge (incl. FTS cleanup), ping, empty-append no-op. The sqlite
package calls `storetest.Run` against `:memory:`; M19 and any future backend reuse
it unchanged.

## Wiring (`cmd/omnilog`, `internal/config`)

- `config.WALDir` (yaml `wal_dir`, env `OMNILOG_WAL_DIR`). Default: for a file DB,
  `<dir(DBPath)>/wal`; for `:memory:`, empty (disabled). Lives on the same
  persistent volume as the DB so it survives container restarts.
- `runServe`: open the WAL (if enabled), pass to `ingest.Options.WAL`, call
  `ing.Recover(ctx)` before `ing.Start()`, log the recovered count.

## Testing (TDD)

- `internal/wal`: append/replay round-trip; checkpoint skips applied; reopen
  recovers nextSeq and replays unapplied; **torn-tail record ignored + file
  truncated**; CRC corruption stops replay at the bad record; rotation across
  segments; segment deletion after checkpoint; concurrent appends (`-race`).
- `internal/ingest`: **crash recovery** — enqueue events, abandon without flushing
  (simulating a crash), open a fresh ingestor on the same WAL+store, `Recover`,
  assert the events are in the store; idempotent double-recover; 429 still fires
  when the channel is full and the over-limit event is *not* WAL'd.
- `internal/store/storetest` + sqlite invocation.

## Definition of done

`gofmt`/`vet`/`go test -race ./...` green; a real binary survives a simulated
crash (kill -9 mid-ingest) and recovers acked events on restart; the deploy's
backup/integrity path is unaffected (WAL lives beside the DB on the volume; replay
is idempotent). No data loss; no new third-party dependency.
