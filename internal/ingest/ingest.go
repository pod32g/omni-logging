// Package ingest accepts log events over HTTP, normalizes them, and writes them
// to the store in batches. A bounded buffer provides backpressure: when the
// buffer is full, callers are rejected (429) rather than events being dropped
// silently.
package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/store"
	"github.com/pod32g/omni-logging/internal/tail"
	"github.com/pod32g/omni-logging/internal/wal"
)

// Options configures an Ingestor.
type Options struct {
	BufferSize    int           // capacity of the in-memory queue
	BatchSize     int           // max events written per transaction
	FlushInterval time.Duration // max time a partial batch waits before writing
	Now           func() time.Time
	Logger        *slog.Logger
	WAL           *wal.WAL // durable write-ahead log; nil = in-memory only (v1 behavior)
}

// queued is one buffered event plus its WAL sequence number (0 when no WAL).
type queued struct {
	e   model.LogEvent
	seq uint64
}

func (o *Options) withDefaults() {
	if o.BufferSize <= 0 {
		o.BufferSize = 10000
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 500 * time.Millisecond
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Ingestor buffers events and writes them to the store in batches, optionally
// broadcasting each written event to a live-tail hub. When a WAL is configured,
// accepted events are persisted to it before Enqueue returns, so they survive a
// process crash and are replayed into the store on restart.
type Ingestor struct {
	store store.Store
	hub   *tail.Hub
	opts  Options
	wal   *wal.WAL

	ch chan queued
	wg sync.WaitGroup

	enqMu  sync.Mutex // serializes Enqueue (and WAL appends) and guards closed
	closed bool

	// checkpointStalled is set by the batch writer if a store write ever fails:
	// once set, the WAL checkpoint is never advanced again, so every event since
	// the failure is replayed (and idempotently re-applied) on the next restart
	// rather than being skipped by a checkpoint that jumped over a gap.
	checkpointStalled bool

	received atomic.Int64
	written  atomic.Int64
	dropped  atomic.Int64
}

// New creates an Ingestor. hub may be nil to disable live-tail broadcasting.
func New(s store.Store, hub *tail.Hub, opts Options) *Ingestor {
	opts.withDefaults()
	return &Ingestor{
		store: s,
		hub:   hub,
		opts:  opts,
		wal:   opts.WAL,
		ch:    make(chan queued, opts.BufferSize),
	}
}

// Start launches the background batch writer. Call Stop to drain and shut down.
func (i *Ingestor) Start() {
	i.wg.Add(1)
	go i.run()
}

// Enqueue durably accepts an event and buffers it for the batch writer. It
// returns false (backpressure) if the buffer is full — in which case the event
// is NOT written to the WAL, so an unacked event is never recovered. When a WAL
// is configured the event is appended to it (surviving a crash) before being
// queued. Enqueues are serialized so the WAL stays a single ordered log and the
// capacity check below cannot race a second producer.
func (i *Ingestor) Enqueue(e model.LogEvent) bool {
	i.enqMu.Lock()
	if i.closed || len(i.ch) >= cap(i.ch) {
		i.enqMu.Unlock()
		i.dropped.Add(1)
		return false
	}
	var seq uint64
	if i.wal != nil {
		payload, err := json.Marshal(e)
		if err != nil {
			i.enqMu.Unlock()
			i.dropped.Add(1)
			i.opts.Logger.Error("ingest: marshal event for WAL", "error", err)
			return false
		}
		seq, err = i.wal.Append(payload)
		if err != nil {
			i.enqMu.Unlock()
			i.dropped.Add(1)
			i.opts.Logger.Error("ingest: WAL append failed", "error", err)
			return false
		}
	}
	// A slot is guaranteed: we checked len < cap under the lock, no other
	// producer can add (lock held), and the consumer only frees slots.
	i.ch <- queued{e: e, seq: seq}
	i.enqMu.Unlock()
	i.received.Add(1)
	return true
}

// Recover replays any WAL records past the checkpoint into the store (idempotent
// via ULID INSERT OR REPLACE) and advances the checkpoint. Call once before
// Start. It returns the number of events recovered. A no-op when no WAL is set.
func (i *Ingestor) Recover(ctx context.Context) (int, error) {
	if i.wal == nil {
		return 0, nil
	}
	var (
		batch  []model.LogEvent
		maxSeq uint64
		count  int
	)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := i.store.Append(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	err := i.wal.Replay(func(seq uint64, payload []byte) error {
		var e model.LogEvent
		if uerr := json.Unmarshal(payload, &e); uerr != nil {
			i.opts.Logger.Error("ingest: skipping malformed WAL record on replay", "seq", seq, "error", uerr)
			return nil
		}
		batch = append(batch, e)
		maxSeq = seq
		count++
		if len(batch) >= i.opts.BatchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	if err := flush(); err != nil {
		return count, err
	}
	if maxSeq > 0 {
		if err := i.wal.SetCheckpoint(maxSeq); err != nil {
			return count, err
		}
	}
	return count, nil
}

// Stop closes the queue and waits for all buffered events to be written.
func (i *Ingestor) Stop() {
	i.enqMu.Lock()
	if !i.closed {
		i.closed = true
		close(i.ch)
	}
	i.enqMu.Unlock()
	i.wg.Wait()
}

// run drains the queue, writing batches when BatchSize is reached or
// FlushInterval elapses.
func (i *Ingestor) run() {
	defer i.wg.Done()
	ticker := time.NewTicker(i.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]model.LogEvent, 0, i.opts.BatchSize)
	var maxSeq uint64
	flush := func() {
		if len(batch) == 0 {
			return
		}
		i.writeBatch(batch, maxSeq)
		batch = batch[:0]
		maxSeq = 0
	}

	for {
		select {
		case q, ok := <-i.ch:
			if !ok {
				flush() // queue closed: write the remainder and exit
				return
			}
			batch = append(batch, q.e)
			if q.seq > maxSeq {
				maxSeq = q.seq
			}
			if len(batch) >= i.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (i *Ingestor) writeBatch(batch []model.LogEvent, maxSeq uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Ensure the WAL is durable before relying on the checkpoint we are about to
	// advance, so a power loss can never leave the checkpoint ahead of the WAL.
	if i.wal != nil {
		if err := i.wal.Sync(); err != nil {
			i.opts.Logger.Error("ingest: WAL sync failed", "error", err)
			i.checkpointStalled = true
		}
	}
	if err := i.store.Append(ctx, batch); err != nil {
		// A write failure is serious and must be visible, not swallowed. The
		// events remain in the WAL past the checkpoint, so they are recovered on
		// restart; we stall the checkpoint so it never skips this gap.
		i.opts.Logger.Error("ingest: batch write failed", "count", len(batch), "error", err)
		i.checkpointStalled = true
		return
	}
	i.written.Add(int64(len(batch)))
	if i.wal != nil && maxSeq > 0 && !i.checkpointStalled {
		if err := i.wal.SetCheckpoint(maxSeq); err != nil {
			i.opts.Logger.Error("ingest: WAL checkpoint failed", "error", err)
			i.checkpointStalled = true
		}
	}
	if i.hub != nil {
		i.hub.Publish(batch...)
	}
}

// Metrics is a snapshot of ingest counters.
type Metrics struct {
	Received int64 `json:"received"`
	Written  int64 `json:"written"`
	Dropped  int64 `json:"dropped"`
	Queued   int64 `json:"queued"`
}

// Metrics returns a snapshot of ingest activity.
func (i *Ingestor) Metrics() Metrics {
	return Metrics{
		Received: i.received.Load(),
		Written:  i.written.Load(),
		Dropped:  i.dropped.Load(),
		Queued:   int64(len(i.ch)),
	}
}
