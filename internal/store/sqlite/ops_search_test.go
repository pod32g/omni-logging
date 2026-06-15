package sqlite

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

func idSet(events []model.LogEvent) map[string]bool {
	m := map[string]bool{}
	for _, e := range events {
		m[e.ID] = true
	}
	return m
}

func TestSearch_Operators(t *testing.T) {
	db := newTestDB(t)
	seed(t, db) // a(error,checkout,status504,uid42) b(warn,checkout,uid7) c(info,auth,uid42) d(error,auth) e(debug,worker)

	cases := []struct {
		expr string
		want []string
	}{
		{"attr.status>=504", []string{"a"}},
		{"attr.status>504", []string{}},
		{"attr.status<600", []string{"a"}},
		{"service=checkout*", []string{"a", "b"}},
		{"service=auth*", []string{"c", "d"}},
		{"attr.status=*", []string{"a"}},                // exists
		{"attr.user_id=*", []string{"a", "b", "c"}},     // exists
		{"level=(error,warn)", []string{"a", "b", "d"}}, // IN
		{"message=~payments", []string{"a", "b"}},       // regex
		{"message=~^issued", []string{"c"}},
		{"message=*timeout*", []string{"a"}}, // message wildcard
	}
	for _, tc := range cases {
		got := idSet(search(t, db, tc.expr))
		if len(got) != len(tc.want) {
			t.Errorf("%q -> %v, want %v", tc.expr, got, tc.want)
			continue
		}
		for _, id := range tc.want {
			if !got[id] {
				t.Errorf("%q missing %s (got %v)", tc.expr, id, got)
			}
		}
	}
}

func TestSearch_KeysetPagination(t *testing.T) {
	db := newTestDB(t)
	seed(t, db) // 5 events; newest-first order: e, d, c, b, a

	var all []string
	cursor := ""
	pages := 0
	for {
		q, _ := query.Parse("")
		q.Limit = 2
		if cursor != "" {
			ts, id, err := query.DecodeCursor(cursor)
			if err != nil {
				t.Fatalf("DecodeCursor: %v", err)
			}
			q.AfterTS, q.AfterID = ts, id
		}
		res, err := db.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		all = append(all, ids(res.Events)...)
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		if res.NextCursor == "" || len(res.Events) == 0 {
			break
		}
		cursor = res.NextCursor
	}

	want := []string{"e", "d", "c", "b", "a"}
	if len(all) != len(want) {
		t.Fatalf("paged ids = %v, want %v (no dupes/gaps)", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("paged order = %v, want %v", all, want)
		}
	}
}

// TestSearchMatcherParity asserts the SQL store (search) and the in-memory
// matcher (live tail) return identical results for the same query — they must
// agree per the Store contract, or a user watching a tail sees rows that vanish
// when they freeze and search. Covers case sensitivity and numeric coercion.
func TestSearchMatcherParity(t *testing.T) {
	db := newTestDB(t)
	base := time.Date(2026, 6, 14, 15, 0, 0, 0, time.UTC)
	events := []model.LogEvent{
		mk("a", base.Add(1*time.Minute), "Checkout-API", "node-1", model.LevelError, "Upstream TIMEOUT calling payments", map[string]any{"status": float64(504), "code": "ABC"}),
		mk("b", base.Add(2*time.Minute), "checkout-api", "node-1", model.LevelWarn, "slow upstream payments", map[string]any{"status": "not-a-number", "user_id": float64(7)}),
		mk("c", base.Add(3*time.Minute), "auth-svc", "node-2", model.LevelInfo, "issued access token", map[string]any{"status": float64(200)}),
		mk("d", base.Add(4*time.Minute), "auth-svc", "node-2", model.LevelError, "rate limit exceeded", nil),
		mk("e", base.Add(5*time.Minute), "worker", "node-3", model.LevelDebug, "flushed events", nil),
	}
	if err := db.Append(context.Background(), events); err != nil {
		t.Fatalf("Append: %v", err)
	}

	exprs := []string{
		"service=checkout-api", "service=Checkout-API", // case-sensitive eq
		"attr.code=ABC", "attr.code=abc", // case-sensitive attr eq
		"attr.status>=500", "attr.status>504", "attr.status<300", // numeric compare
		"attr.status!=200",                         // != incl. missing attr
		"level=(error,warn)", "level=(ERROR,WARN)", // IN, level normalized
		"service=checkout*", "service=CHECK*", // glob (ASCII case-insensitive)
		"attr.status=*", "attr.user_id=*", // exists
		"message=~payments", "message=~TIMEOUT", "message=~timeout", // regex case-sensitive
	}
	for _, expr := range exprs {
		q, err := query.Parse(expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
		sqlIDs := idSet(search(t, db, expr))
		matchIDs := map[string]bool{}
		for _, e := range events {
			if q.Matches(e) {
				matchIDs[e.ID] = true
			}
		}
		if !sameSet(sqlIDs, matchIDs) {
			t.Errorf("parity mismatch for %q:\n  sql     = %v\n  matcher = %v", expr, keys(sqlIDs), keys(matchIDs))
		}
	}
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestStream_ReturnsAllMatchesInOrder(t *testing.T) {
	db := newTestDB(t)
	seed(t, db)

	var got []string
	q, _ := query.Parse("")
	if err := db.Stream(context.Background(), q, func(e model.LogEvent) error {
		got = append(got, e.ID)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Newest-first, ignoring any limit.
	want := []string{"e", "d", "c", "b", "a"}
	if len(got) != len(want) {
		t.Fatalf("stream got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stream order = %v, want %v", got, want)
		}
	}

	// Filtered stream.
	got = nil
	qf, _ := query.Parse("level=error")
	db.Stream(context.Background(), qf, func(e model.LogEvent) error { got = append(got, e.ID); return nil })
	sort.Strings(got)
	if len(got) != 2 || got[0] != "a" || got[1] != "d" {
		t.Fatalf("filtered stream = %v, want [a d]", got)
	}
}
