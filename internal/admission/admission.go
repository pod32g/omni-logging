// Package admission provides per-ingest-key admission control: a token-bucket
// rate limit plus daily event/byte quotas. It replaces the v1 all-or-nothing
// buffer-full 429 with fair, per-key limits so one noisy source cannot starve
// others or overrun the store.
package admission

import (
	"sync"
	"time"
)

// Limits configures admission control. A zero value disables all limits.
type Limits struct {
	RatePerSec  float64 // per-key request token refill per second (0 = no rate limit)
	Burst       int     // token-bucket capacity (defaults to max(1, ceil(RatePerSec)))
	DailyEvents int64   // max accepted events per key per UTC day (0 = unlimited)
	DailyBytes  int64   // max ingested bytes per key per UTC day (0 = unlimited)
}

// Decision is the outcome of an admission check.
type Decision struct {
	Allowed bool
	Reason  string // "", "rate", "events_quota", or "bytes_quota"
}

// Limiter enforces Limits independently per key.
type Limiter struct {
	limits Limits
	burst  float64
	now    func() time.Time

	mu   sync.Mutex
	keys map[string]*keyState
}

type keyState struct {
	tokens float64
	last   time.Time
	day    string // UTC date the counters below belong to
	events int64
	bytes  int64
}

// New creates a Limiter. now is injectable for testing (defaults to time.Now).
func New(limits Limits, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	burst := float64(limits.Burst)
	if burst < 1 {
		burst = 1
		if limits.RatePerSec > 1 {
			burst = limits.RatePerSec
		}
	}
	return &Limiter{limits: limits, burst: burst, now: now, keys: map[string]*keyState{}}
}

// Enabled reports whether any limit is active.
func (l *Limiter) Enabled() bool {
	return l.limits.RatePerSec > 0 || l.limits.DailyEvents > 0 || l.limits.DailyBytes > 0
}

// Allow decides whether a request from key carrying up to bytes may proceed. It
// applies the rate token bucket and the daily quotas, consuming a rate token on
// success. It does NOT add to the daily counters — call Record afterwards with
// the actual accepted counts.
func (l *Limiter) Allow(key string, bytes int64) Decision {
	if !l.Enabled() {
		return Decision{Allowed: true}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.stateLocked(key)

	if l.limits.RatePerSec > 0 && st.tokens < 1 {
		return Decision{Reason: "rate"}
	}
	if l.limits.DailyBytes > 0 && st.bytes+bytes > l.limits.DailyBytes {
		return Decision{Reason: "bytes_quota"}
	}
	if l.limits.DailyEvents > 0 && st.events >= l.limits.DailyEvents {
		return Decision{Reason: "events_quota"}
	}
	if l.limits.RatePerSec > 0 {
		st.tokens--
	}
	return Decision{Allowed: true}
}

// Record adds accepted events and ingested bytes to the key's daily usage.
func (l *Limiter) Record(key string, events, bytes int64) {
	if !l.Enabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.stateLocked(key)
	st.events += events
	st.bytes += bytes
}

// stateLocked returns the key's state, refilling its bucket and resetting its
// daily counters when the UTC day rolls over. Caller holds l.mu.
func (l *Limiter) stateLocked(key string) *keyState {
	now := l.now()
	st := l.keys[key]
	if st == nil {
		st = &keyState{tokens: l.burst, last: now, day: day(now)}
		l.keys[key] = st
	}
	if l.limits.RatePerSec > 0 {
		elapsed := now.Sub(st.last).Seconds()
		if elapsed > 0 {
			st.tokens += elapsed * l.limits.RatePerSec
			if st.tokens > l.burst {
				st.tokens = l.burst
			}
			st.last = now
		}
	}
	if d := day(now); st.day != d {
		st.day = d
		st.events = 0
		st.bytes = 0
	}
	return st
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }
