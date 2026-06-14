package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seed inserts a small, deterministic dataset and returns the base time.
func seed(t *testing.T, db *DB) time.Time {
	t.Helper()
	base := time.Date(2026, 6, 14, 15, 0, 0, 0, time.UTC)
	events := []model.LogEvent{
		mk("a", base.Add(1*time.Minute), "checkout-api", "node-1", model.LevelError, "upstream request timeout calling payments", map[string]any{"user_id": float64(42), "status": float64(504)}),
		mk("b", base.Add(2*time.Minute), "checkout-api", "node-1", model.LevelWarn, "slow upstream payments", map[string]any{"user_id": float64(7)}),
		mk("c", base.Add(3*time.Minute), "auth-svc", "node-2", model.LevelInfo, "issued access token", map[string]any{"user_id": float64(42)}),
		mk("d", base.Add(4*time.Minute), "auth-svc", "node-2", model.LevelError, "rate limit exceeded", nil),
		mk("e", base.Add(5*time.Minute), "worker", "node-3", model.LevelDebug, "flushed events to index", nil),
	}
	if err := db.Append(context.Background(), events); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return base
}

func mk(id string, ts time.Time, svc, src string, lvl model.Level, msg string, attrs map[string]any) model.LogEvent {
	e := model.LogEvent{
		ID: id, Timestamp: ts, ReceivedAt: ts,
		Service: svc, Source: src, Level: lvl, Message: msg, Attributes: attrs,
	}
	return e
}

func search(t *testing.T, db *DB, expr string) []model.LogEvent {
	t.Helper()
	q, err := query.Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	res, err := db.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%q): %v", expr, err)
	}
	return res.Events
}

func ids(events []model.LogEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func TestSearch_LevelFilter(t *testing.T) {
	db := newTestDB(t)
	seed(t, db)

	got := ids(search(t, db, "level=error"))
	// Newest first: d (4m) before a (1m).
	if len(got) != 2 || got[0] != "d" || got[1] != "a" {
		t.Fatalf("level=error returned %v, want [d a]", got)
	}
}

func TestSearch_FreeTextAndService(t *testing.T) {
	db := newTestDB(t)
	seed(t, db)

	if got := ids(search(t, db, "timeout")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("free-text timeout = %v, want [a]", got)
	}
	if got := ids(search(t, db, "service=checkout-api payments")); len(got) != 2 {
		t.Fatalf("service+text = %v, want 2 results", got)
	}
	if got := ids(search(t, db, "level=error nonexistentword")); len(got) != 0 {
		t.Fatalf("text+filter no match = %v, want empty", got)
	}
}

func TestSearch_AttributeFilter(t *testing.T) {
	db := newTestDB(t)
	seed(t, db)

	got := ids(search(t, db, "attr.user_id=42"))
	if len(got) != 2 { // a and c
		t.Fatalf("attr.user_id=42 = %v, want 2", got)
	}
	if got := ids(search(t, db, "attr.status=504")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("attr.status=504 = %v, want [a]", got)
	}
	// Negation: events without user_id=42 (includes those missing the attr).
	if got := ids(search(t, db, "attr.user_id!=42")); len(got) != 3 {
		t.Fatalf("attr.user_id!=42 = %v, want 3", got)
	}
}

func TestSearch_TimeRangeOrderingAndLimit(t *testing.T) {
	db := newTestDB(t)
	base := seed(t, db)

	q, _ := query.Parse("")
	q.From = base.Add(2 * time.Minute)
	q.To = base.Add(4 * time.Minute)
	q.Order = query.OrderOldest
	q.Limit = 2
	res, err := db.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 3 { // b, c, d in range
		t.Errorf("Total = %d, want 3", res.Total)
	}
	got := ids(res.Events)
	if len(got) != 2 || got[0] != "b" || got[1] != "c" { // oldest first, limited to 2
		t.Fatalf("time range oldest/limit = %v, want [b c]", got)
	}
}

func TestStats_HistogramAndFacets(t *testing.T) {
	db := newTestDB(t)
	seed(t, db)

	q, _ := query.Parse("")
	q.Interval = time.Minute
	res, err := db.Stats(context.Background(), q)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if res.Total != 5 {
		t.Errorf("stats Total = %d, want 5", res.Total)
	}
	if len(res.Histogram) != 5 { // five distinct 1-minute buckets
		t.Errorf("histogram buckets = %d, want 5", len(res.Histogram))
	}

	levels := facetMap(res.Facets["level"])
	if levels["error"] != 2 || levels["warn"] != 1 || levels["info"] != 1 || levels["debug"] != 1 {
		t.Errorf("level facets = %v", levels)
	}
	services := facetMap(res.Facets["service"])
	if services["checkout-api"] != 2 || services["auth-svc"] != 2 || services["worker"] != 1 {
		t.Errorf("service facets = %v", services)
	}
}

func facetMap(facets []store.Facet) map[string]int64 {
	m := map[string]int64{}
	for _, f := range facets {
		m[f.Value] = f.Count
	}
	return m
}

func TestPurge(t *testing.T) {
	db := newTestDB(t)
	base := seed(t, db)

	n, err := db.Purge(context.Background(), base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 2 { // a (1m) and b (2m) are strictly older than 3m
		t.Fatalf("purged %d, want 2", n)
	}
	// Full-text index must be purged too: searching for a purged term yields nothing.
	if got := ids(search(t, db, "timeout")); len(got) != 0 {
		t.Fatalf("after purge, timeout search = %v, want empty (fts not cleaned)", got)
	}
	if got := ids(search(t, db, "")); len(got) != 3 {
		t.Fatalf("after purge, total = %v, want 3 remaining", got)
	}
}
