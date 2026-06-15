// Package tail provides an in-memory publish/subscribe hub for live tailing.
// Newly ingested events are published to the hub, which fans them out to every
// subscriber whose filter matches. Slow subscribers are dropped rather than
// allowed to block ingestion.
package tail

import (
	"sync"
	"sync/atomic"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// DefaultMaxDrops is the per-subscriber dropped-event count past which a slow
// consumer is evicted (its stream closed) rather than dropping forever.
const DefaultMaxDrops = 10000

// Hub fans out published events to matching subscribers.
type Hub struct {
	mu           sync.RWMutex
	subs         map[*Subscriber]struct{}
	maxDrops     int64        // evict a subscriber after this many drops (0 = never)
	droppedTotal atomic.Int64 // aggregate events dropped across all subscribers
	evictedTotal atomic.Int64 // subscribers evicted for being too slow
}

// Subscriber receives events matching its query via channel C.
type Subscriber struct {
	hub     *Hub
	q       query.Query
	C       chan model.LogEvent
	dropped int64       // events skipped because the buffer was full
	evicted atomic.Bool // set when the hub evicted this subscriber for slowness
}

// NewHub creates an empty hub with the default slow-consumer eviction threshold.
func NewHub() *Hub { return NewHubLimit(DefaultMaxDrops) }

// NewHubLimit creates an empty hub that evicts a subscriber after maxDrops
// dropped events (maxDrops <= 0 disables eviction).
func NewHubLimit(maxDrops int64) *Hub {
	return &Hub{subs: map[*Subscriber]struct{}{}, maxDrops: maxDrops}
}

// Subscribe registers a subscriber for events matching q. buffer is the channel
// capacity; once full, further events for this subscriber are dropped. The
// caller must call Close (or the returned Subscriber's Close) when done.
func (h *Hub) Subscribe(q query.Query, buffer int) *Subscriber {
	if buffer < 1 {
		buffer = 1
	}
	s := &Subscriber{hub: h, q: q, C: make(chan model.LogEvent, buffer)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Close unregisters the subscriber and closes its channel.
func (s *Subscriber) Close() {
	s.hub.mu.Lock()
	if _, ok := s.hub.subs[s]; ok {
		delete(s.hub.subs, s)
		close(s.C)
	}
	s.hub.mu.Unlock()
}

// Dropped reports how many events were dropped for this subscriber.
func (s *Subscriber) Dropped() int64 { return atomic.LoadInt64(&s.dropped) }

// Evicted reports whether the hub evicted this subscriber for being too slow.
func (s *Subscriber) Evicted() bool { return s.evicted.Load() }

// Publish delivers events to all matching subscribers. Delivery is
// non-blocking: if a subscriber's buffer is full, the event is dropped for that
// subscriber and counted. Publish never blocks the caller (the ingest path). A
// subscriber that drops more than the hub's threshold is evicted (its stream
// closed) so a single stuck client cannot accumulate unboundedly.
func (h *Hub) Publish(events ...model.LogEvent) {
	var toEvict []*Subscriber
	h.mu.RLock()
	for s := range h.subs {
		for _, e := range events {
			if !s.q.Matches(e) {
				continue
			}
			select {
			case s.C <- e:
			default:
				d := atomic.AddInt64(&s.dropped, 1)
				h.droppedTotal.Add(1)
				if h.maxDrops > 0 && d == h.maxDrops {
					toEvict = append(toEvict, s)
				}
			}
		}
	}
	h.mu.RUnlock()

	for _, s := range toEvict {
		h.evict(s)
	}
}

// evict removes a slow subscriber and closes its channel. The reader observes
// the close and ends its stream; the client reconnects.
func (h *Hub) evict(s *Subscriber) {
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		s.evicted.Store(true)
		delete(h.subs, s)
		close(s.C)
		h.evictedTotal.Add(1)
	}
	h.mu.Unlock()
}

// DroppedTotal returns the aggregate number of events dropped across all
// subscribers because their buffers were full.
func (h *Hub) DroppedTotal() int64 { return h.droppedTotal.Load() }

// EvictedTotal returns the number of subscribers evicted for being too slow.
func (h *Hub) EvictedTotal() int64 { return h.evictedTotal.Load() }

// SubscriberCount returns the number of active subscribers.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
