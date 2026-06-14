// Package ingest accepts log events over HTTP, normalizes them, and writes them
// to the store in batches. A bounded buffer provides backpressure: when the
// buffer is full, callers are rejected (429) rather than events being dropped
// silently.
package ingest

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/store"
	"github.com/pod32g/omni-logging/internal/tail"
)

// Options configures an Ingestor.
type Options struct {
	BufferSize    int           // capacity of the in-memory queue
	BatchSize     int           // max events written per transaction
	FlushInterval time.Duration // max time a partial batch waits before writing
	Now           func() time.Time
	Logger        *slog.Logger
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
// broadcasting each written event to a live-tail hub.
type Ingestor struct {
	store store.Store
	hub   *tail.Hub
	opts  Options

	ch   chan model.LogEvent
	done chan struct{}
	wg   sync.WaitGroup

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
		ch:    make(chan model.LogEvent, opts.BufferSize),
		done:  make(chan struct{}),
	}
}

// Start launches the background batch writer. Call Stop to drain and shut down.
func (i *Ingestor) Start() {
	i.wg.Add(1)
	go i.run()
}

// Enqueue attempts to buffer an event without blocking. It returns false if the
// buffer is full (the caller should signal backpressure to the client).
func (i *Ingestor) Enqueue(e model.LogEvent) bool {
	select {
	case i.ch <- e:
		i.received.Add(1)
		return true
	default:
		i.dropped.Add(1)
		return false
	}
}

// Stop closes the queue and waits for all buffered events to be written.
func (i *Ingestor) Stop() {
	close(i.ch)
	i.wg.Wait()
}

// run drains the queue, writing batches when BatchSize is reached or
// FlushInterval elapses.
func (i *Ingestor) run() {
	defer i.wg.Done()
	ticker := time.NewTicker(i.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]model.LogEvent, 0, i.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		i.writeBatch(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-i.ch:
			if !ok {
				flush() // queue closed: write the remainder and exit
				return
			}
			batch = append(batch, e)
			if len(batch) >= i.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (i *Ingestor) writeBatch(batch []model.LogEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := i.store.Append(ctx, batch); err != nil {
		// A write failure is serious and must be visible, not swallowed.
		i.opts.Logger.Error("ingest: batch write failed", "count", len(batch), "error", err)
		return
	}
	i.written.Add(int64(len(batch)))
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
