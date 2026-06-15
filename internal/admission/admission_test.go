package admission

import (
	"testing"
	"time"
)

func TestDisabledAllowsEverything(t *testing.T) {
	l := New(Limits{}, time.Now)
	if l.Enabled() {
		t.Fatal("zero limits should be disabled")
	}
	for i := 0; i < 100; i++ {
		if d := l.Allow("k", 1<<20); !d.Allowed {
			t.Fatalf("disabled limiter rejected: %+v", d)
		}
	}
}

func TestRateLimitTokenBucket(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	l := New(Limits{RatePerSec: 1, Burst: 2}, func() time.Time { return now })

	if d := l.Allow("k", 0); !d.Allowed {
		t.Fatalf("req 1 should pass: %+v", d)
	}
	if d := l.Allow("k", 0); !d.Allowed {
		t.Fatalf("req 2 should pass (burst): %+v", d)
	}
	if d := l.Allow("k", 0); d.Allowed || d.Reason != "rate" {
		t.Fatalf("req 3 should be rate-limited, got %+v", d)
	}
	// One second later → one token refilled.
	now = now.Add(time.Second)
	if d := l.Allow("k", 0); !d.Allowed {
		t.Fatalf("after refill should pass: %+v", d)
	}
}

func TestByteQuota(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	l := New(Limits{DailyBytes: 100}, func() time.Time { return now })

	if d := l.Allow("k", 60); !d.Allowed {
		t.Fatalf("first 60 bytes should pass: %+v", d)
	}
	l.Record("k", 0, 60)
	if d := l.Allow("k", 60); d.Allowed || d.Reason != "bytes_quota" {
		t.Fatalf("60+60 > 100 should be bytes_quota, got %+v", d)
	}
	if d := l.Allow("k", 40); !d.Allowed {
		t.Fatalf("60+40 == 100 should pass: %+v", d)
	}
}

func TestEventQuota(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	l := New(Limits{DailyEvents: 5}, func() time.Time { return now })

	if d := l.Allow("k", 0); !d.Allowed {
		t.Fatalf("under quota should pass: %+v", d)
	}
	l.Record("k", 5, 0) // now at quota
	if d := l.Allow("k", 0); d.Allowed || d.Reason != "events_quota" {
		t.Fatalf("at quota should be events_quota, got %+v", d)
	}
}

func TestDailyReset(t *testing.T) {
	now := time.Date(2026, 6, 14, 23, 0, 0, 0, time.UTC)
	l := New(Limits{DailyBytes: 100, DailyEvents: 5}, func() time.Time { return now })
	l.Record("k", 5, 100)
	if d := l.Allow("k", 1); d.Allowed {
		t.Fatalf("exhausted should reject: %+v", d)
	}
	// Next UTC day → counters reset.
	now = now.Add(2 * time.Hour)
	if d := l.Allow("k", 50); !d.Allowed {
		t.Fatalf("after daily reset should pass: %+v", d)
	}
}

func TestPerKeyIsolation(t *testing.T) {
	now := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)
	l := New(Limits{RatePerSec: 1, Burst: 1, DailyEvents: 2}, func() time.Time { return now })
	l.Allow("a", 0)     // consume a's token
	l.Record("a", 2, 0) // exhaust a's event quota
	if d := l.Allow("a", 0); d.Allowed {
		t.Fatalf("key a should be limited: %+v", d)
	}
	if d := l.Allow("b", 0); !d.Allowed {
		t.Fatalf("key b should be unaffected by a: %+v", d)
	}
}
