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

// Hub fans out published events to matching subscribers.
type Hub struct {
	mu   sync.RWMutex
	subs map[*Subscriber]struct{}
}

// Subscriber receives events matching its query via channel C.
type Subscriber struct {
	hub     *Hub
	q       query.Query
	C       chan model.LogEvent
	dropped int64 // events skipped because the buffer was full
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[*Subscriber]struct{}{}}
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

// Publish delivers events to all matching subscribers. Delivery is
// non-blocking: if a subscriber's buffer is full, the event is dropped for that
// subscriber and counted. Publish never blocks the caller (the ingest path).
func (h *Hub) Publish(events ...model.LogEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		for _, e := range events {
			if !s.q.Matches(e) {
				continue
			}
			select {
			case s.C <- e:
			default:
				atomic.AddInt64(&s.dropped, 1)
			}
		}
	}
}

// SubscriberCount returns the number of active subscribers.
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
