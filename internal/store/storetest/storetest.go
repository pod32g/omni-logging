// Package storetest provides a backend-agnostic conformance suite for the
// store.Store contract. Any backend (SQLite today, others later) runs the same
// suite to prove it honors the interface — append/search/stats/purge/ping
// semantics, ordering, and the ULID idempotency that crash recovery relies on.
//
// Usage from a backend's _test.go:
//
//	func TestConformance(t *testing.T) {
//		storetest.Run(t, func(t *testing.T) store.Store {
//			db, _ := sqlite.Open(":memory:")
//			t.Cleanup(func() { db.Close() })
//			return db
//		})
//	}
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

// Run executes the full Store contract. newStore must return a fresh, empty
// store for each call (and arrange its own cleanup).
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("PingOnFreshStore", func(t *testing.T) {
		s := newStore(t)
		if err := s.Ping(context.Background()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})

	t.Run("EmptyAppendIsNoop", func(t *testing.T) {
		s := newStore(t)
		if err := s.Append(context.Background(), nil); err != nil {
			t.Fatalf("Append(nil): %v", err)
		}
		if got := totalFor(t, s, ""); got != 0 {
			t.Fatalf("empty store total = %d, want 0", got)
		}
	})

	t.Run("AppendAndSearchByLevel", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		got := searchIDs(t, s, "level=error")
		// Newest first: d (4m) then a (1m).
		if len(got) != 2 || got[0] != "d" || got[1] != "a" {
			t.Fatalf("level=error = %v, want [d a]", got)
		}
	})

	t.Run("IdempotentAppendByID", func(t *testing.T) {
		s := newStore(t)
		base := time.Date(2026, 6, 14, 15, 0, 0, 0, time.UTC)
		first := mk("dup", base, "svc", "h", model.LevelInfo, "first version", nil)
		if err := s.Append(context.Background(), []model.LogEvent{first}); err != nil {
			t.Fatalf("Append first: %v", err)
		}
		// Re-append the same ID with a different message: must replace, not duplicate.
		second := mk("dup", base, "svc", "h", model.LevelInfo, "second version", nil)
		if err := s.Append(context.Background(), []model.LogEvent{second}); err != nil {
			t.Fatalf("Append second: %v", err)
		}
		res := searchAll(t, s, "")
		if len(res) != 1 {
			t.Fatalf("after re-appending same ID, count = %d, want 1 (idempotent)", len(res))
		}
		if res[0].Message != "second version" {
			t.Fatalf("re-append message = %q, want %q", res[0].Message, "second version")
		}
		// The full-text index must be idempotent too: re-applying an event (as
		// crash recovery does) must not leave a duplicate FTS row that doubles
		// free-text results, nor leave the superseded text searchable.
		if got := searchIDs(t, s, "version"); len(got) != 1 {
			t.Fatalf("free-text after re-append = %v, want exactly 1 (no FTS duplicate)", got)
		}
		if got := searchIDs(t, s, "second"); len(got) != 1 {
			t.Fatalf("free-text for new text = %v, want 1", got)
		}
		if got := searchIDs(t, s, "first"); len(got) != 0 {
			t.Fatalf("free-text for superseded text = %v, want 0", got)
		}
	})

	t.Run("FreeTextSearch", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		if got := searchIDs(t, s, "timeout"); len(got) != 1 || got[0] != "a" {
			t.Fatalf("free-text timeout = %v, want [a]", got)
		}
		if got := searchIDs(t, s, "service=checkout-api payments"); len(got) != 2 {
			t.Fatalf("service+text = %v, want 2", got)
		}
	})

	t.Run("AttributeFilter", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		if got := searchIDs(t, s, "attr.user_id=42"); len(got) != 2 {
			t.Fatalf("attr.user_id=42 = %v, want 2", got)
		}
		if got := searchIDs(t, s, "attr.status=504"); len(got) != 1 || got[0] != "a" {
			t.Fatalf("attr.status=504 = %v, want [a]", got)
		}
		if got := searchIDs(t, s, "attr.user_id!=42"); len(got) != 3 {
			t.Fatalf("attr.user_id!=42 = %v, want 3 (incl. missing attr)", got)
		}
	})

	t.Run("TimeRangeOrderLimit", func(t *testing.T) {
		s := newStore(t)
		base := seed(t, s)
		q, _ := query.Parse("")
		q.From = base.Add(2 * time.Minute)
		q.To = base.Add(4 * time.Minute)
		q.Order = query.OrderOldest
		q.Limit = 2
		res, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if res.Total != 3 {
			t.Errorf("Total = %d, want 3", res.Total)
		}
		got := idsOf(res.Events)
		if len(got) != 2 || got[0] != "b" || got[1] != "c" {
			t.Fatalf("oldest/limit = %v, want [b c]", got)
		}
	})

	t.Run("StatsHistogramAndFacets", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		q, _ := query.Parse("")
		q.Interval = time.Minute
		res, err := s.Stats(context.Background(), q)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if res.Total != 5 {
			t.Errorf("stats total = %d, want 5", res.Total)
		}
		if len(res.Histogram) != 5 {
			t.Errorf("histogram buckets = %d, want 5", len(res.Histogram))
		}
		levels := facetMap(res.Facets["level"])
		if levels["error"] != 2 || levels["warn"] != 1 || levels["info"] != 1 || levels["debug"] != 1 {
			t.Errorf("level facets = %v", levels)
		}
	})

	t.Run("StreamReturnsAllMatches", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		var got []string
		q, _ := query.Parse("level=error")
		if err := s.Stream(context.Background(), q, func(e model.LogEvent) error {
			got = append(got, e.ID)
			return nil
		}); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		// d (4m) then a (1m), newest-first.
		if len(got) != 2 || got[0] != "d" || got[1] != "a" {
			t.Fatalf("Stream(level=error) = %v, want [d a]", got)
		}
	})

	t.Run("PaginationIsStableAndComplete", func(t *testing.T) {
		s := newStore(t)
		seed(t, s)
		seen := map[string]bool{}
		cursor, pages := "", 0
		for {
			q, _ := query.Parse("")
			q.Limit = 2
			if cursor != "" {
				ts, id, _ := query.DecodeCursor(cursor)
				q.AfterTS, q.AfterID = ts, id
			}
			res, err := s.Search(context.Background(), q)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			for _, e := range res.Events {
				if seen[e.ID] {
					t.Fatalf("duplicate id %s across pages", e.ID)
				}
				seen[e.ID] = true
			}
			pages++
			if res.NextCursor == "" || len(res.Events) == 0 || pages > 10 {
				break
			}
			cursor = res.NextCursor
		}
		if len(seen) != 5 {
			t.Fatalf("paged %d unique events, want 5", len(seen))
		}
	})

	t.Run("PurgeRemovesAndCleansFTS", func(t *testing.T) {
		s := newStore(t)
		base := seed(t, s)
		n, err := s.Purge(context.Background(), base.Add(3*time.Minute))
		if err != nil {
			t.Fatalf("Purge: %v", err)
		}
		if n != 2 {
			t.Fatalf("purged %d, want 2", n)
		}
		if got := searchIDs(t, s, "timeout"); len(got) != 0 {
			t.Fatalf("after purge, free-text for a purged term = %v, want none (FTS not cleaned)", got)
		}
		if got := searchIDs(t, s, ""); len(got) != 3 {
			t.Fatalf("after purge, remaining = %v, want 3", got)
		}
	})
}

// --- helpers ---------------------------------------------------------------

func seed(t *testing.T, s store.Store) time.Time {
	t.Helper()
	base := time.Date(2026, 6, 14, 15, 0, 0, 0, time.UTC)
	events := []model.LogEvent{
		mk("a", base.Add(1*time.Minute), "checkout-api", "node-1", model.LevelError, "upstream request timeout calling payments", map[string]any{"user_id": float64(42), "status": float64(504)}),
		mk("b", base.Add(2*time.Minute), "checkout-api", "node-1", model.LevelWarn, "slow upstream payments", map[string]any{"user_id": float64(7)}),
		mk("c", base.Add(3*time.Minute), "auth-svc", "node-2", model.LevelInfo, "issued access token", map[string]any{"user_id": float64(42)}),
		mk("d", base.Add(4*time.Minute), "auth-svc", "node-2", model.LevelError, "rate limit exceeded", nil),
		mk("e", base.Add(5*time.Minute), "worker", "node-3", model.LevelDebug, "flushed events to index", nil),
	}
	if err := s.Append(context.Background(), events); err != nil {
		t.Fatalf("Append seed: %v", err)
	}
	return base
}

func mk(id string, ts time.Time, svc, src string, lvl model.Level, msg string, attrs map[string]any) model.LogEvent {
	return model.LogEvent{
		ID: id, Timestamp: ts, ReceivedAt: ts,
		Service: svc, Source: src, Level: lvl, Message: msg, Attributes: attrs,
	}
}

func searchAll(t *testing.T, s store.Store, expr string) []model.LogEvent {
	t.Helper()
	q, err := query.Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%q): %v", expr, err)
	}
	return res.Events
}

func searchIDs(t *testing.T, s store.Store, expr string) []string {
	return idsOf(searchAll(t, s, expr))
}

func totalFor(t *testing.T, s store.Store, expr string) int64 {
	t.Helper()
	q, _ := query.Parse(expr)
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return res.Total
}

func idsOf(events []model.LogEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func facetMap(facets []store.Facet) map[string]int64 {
	m := map[string]int64{}
	for _, f := range facets {
		m[f.Value] = f.Count
	}
	return m
}
