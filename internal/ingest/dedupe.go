package ingest

import (
	"sync"
	"time"
)

// At-least-once delivery means a client will sometimes re-send a batch it
// already delivered — the ack was lost, not the data. Event IDs cannot solve
// this: they are assigned by the server precisely so a producer cannot
// overwrite existing history (see model.EventFromJSON). So retries are
// recognised by a client-supplied *batch* ID instead, which identifies the
// request without granting any control over event identity.
//
// The window is deliberately in-memory and time-bounded: it suppresses the
// duplicate a retry produces seconds or minutes later, not one produced by a
// re-send hours later or after a restart. The durability guarantee stays
// at-least-once; this only removes the common duplicate.

// dedupeTTL is how long a batch ID is remembered.
const dedupeTTL = 10 * time.Minute

// maxDedupeEntries bounds memory. Reaching it drops the oldest half rather than
// refusing new entries, so a burst degrades dedup rather than breaking ingest.
const maxDedupeEntries = 50000

type batchDeduper struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

func newBatchDeduper(now func() time.Time) *batchDeduper {
	if now == nil {
		now = time.Now
	}
	return &batchDeduper{seen: map[string]time.Time{}, now: now}
}

// Seen records a batch ID and reports whether it had already been delivered
// within the window. An empty ID is never treated as a duplicate.
func (d *batchDeduper) Seen(id string) bool {
	if id == "" {
		return false
	}
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()

	if at, ok := d.seen[id]; ok && now.Sub(at) < dedupeTTL {
		return true
	}
	if len(d.seen) >= maxDedupeEntries {
		d.evictLocked(now)
	}
	d.seen[id] = now
	return false
}

// evictLocked drops expired entries, and if that is not enough, the oldest
// half. Caller holds d.mu.
func (d *batchDeduper) evictLocked(now time.Time) {
	for id, at := range d.seen {
		if now.Sub(at) >= dedupeTTL {
			delete(d.seen, id)
		}
	}
	if len(d.seen) < maxDedupeEntries {
		return
	}
	// Still full of live entries: shed the older half by timestamp.
	cutoff := medianAge(d.seen, now)
	for id, at := range d.seen {
		if now.Sub(at) >= cutoff {
			delete(d.seen, id)
		}
	}
}

// medianAge approximates the median entry age, used as an eviction cutoff.
func medianAge(seen map[string]time.Time, now time.Time) time.Duration {
	var oldest, newest time.Duration
	first := true
	for _, at := range seen {
		age := now.Sub(at)
		if first {
			oldest, newest, first = age, age, false
			continue
		}
		if age > oldest {
			oldest = age
		}
		if age < newest {
			newest = age
		}
	}
	return (oldest + newest) / 2
}
