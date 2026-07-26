package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// aggFixture seeds a small, deliberately uneven data set so group ordering and
// numeric aggregation are both observable.
func aggFixture(t *testing.T) (*DB, time.Time) {
	t.Helper()
	d := newTestDB(t)
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	rows := []struct {
		svc     string
		level   model.Level
		latency float64
		offset  time.Duration
	}{
		{"api", model.LevelError, 100, 0},
		{"api", model.LevelError, 300, 30 * time.Second},
		{"api", model.LevelInfo, 50, time.Minute},
		{"web", model.LevelError, 200, 90 * time.Second},
		{"web", model.LevelInfo, 10, 2 * time.Minute},
		{"db", model.LevelInfo, 20, 2*time.Minute + 30*time.Second},
	}
	events := make([]model.LogEvent, 0, len(rows))
	for i, r := range rows {
		e := model.LogEvent{
			Service:    r.svc,
			Level:      r.level,
			Message:    fmt.Sprintf("event %d", i),
			Attributes: map[string]any{"latency_ms": r.latency},
		}
		e.Normalize(base.Add(r.offset))
		events = append(events, e)
	}
	if err := d.Append(context.Background(), events); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return d, base
}

func aggregate(t *testing.T, d *DB, expr string) (cols []string, rows [][]any, truncated bool) {
	t.Helper()
	q, err := query.Params{Q: expr}.Build(time.Now())
	if err != nil {
		t.Fatalf("Build(%q): %v", expr, err)
	}
	res, err := d.Aggregate(context.Background(), q)
	if err != nil {
		t.Fatalf("Aggregate(%q): %v", expr, err)
	}
	return res.Columns, res.Rows, res.Truncated
}

func TestAggregateStatsCountByService(t *testing.T) {
	d, _ := aggFixture(t)
	cols, rows, _ := aggregate(t, d, "| stats count by service")

	if len(cols) != 2 || cols[0] != "service" || cols[1] != "count" {
		t.Fatalf("columns = %v", cols)
	}
	// Ordered by count descending: api 3, web 2, db 1.
	want := []struct {
		svc   string
		count int64
	}{{"api", 3}, {"web", 2}, {"db", 1}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i][0] != w.svc {
			t.Errorf("row %d service = %v, want %s", i, rows[i][0], w.svc)
		}
		if rows[i][1] != w.count {
			t.Errorf("row %d count = %v (%T), want %d", i, rows[i][1], rows[i][1], w.count)
		}
	}
}

// TestAggregateAppliesTheFilter: the filter half must still narrow the input,
// otherwise an aggregation silently reports on the whole database.
func TestAggregateAppliesTheFilter(t *testing.T) {
	d, _ := aggFixture(t)
	_, rows, _ := aggregate(t, d, "level=error | stats count by service")

	total := int64(0)
	for _, r := range rows {
		n, ok := r[1].(int64)
		if !ok {
			t.Fatalf("count is %T, want int64", r[1])
		}
		total += n
	}
	if total != 3 {
		t.Fatalf("level=error aggregated %d events, want 3", total)
	}
}

func TestAggregateNumericFunctions(t *testing.T) {
	d, _ := aggFixture(t)
	cols, rows, _ := aggregate(t, d,
		"service=api | stats count, sum(attr.latency_ms), avg(attr.latency_ms), min(attr.latency_ms), max(attr.latency_ms)")

	if len(rows) != 1 {
		t.Fatalf("rows = %v, want a single ungrouped row", rows)
	}
	want := map[string]float64{
		"count":           3,
		"sum(latency_ms)": 450,
		"avg(latency_ms)": 150,
		"min(latency_ms)": 50,
		"max(latency_ms)": 300,
	}
	for i, c := range cols {
		exp, ok := want[c]
		if !ok {
			t.Errorf("unexpected column %q", c)
			continue
		}
		var got float64
		switch v := rows[0][i].(type) {
		case int64:
			got = float64(v)
		case float64:
			got = v
		default:
			t.Errorf("column %q is %T, want a number", c, rows[0][i])
			continue
		}
		if got != exp {
			t.Errorf("%s = %v, want %v", c, got, exp)
		}
	}
}

func TestAggregateDistinctCount(t *testing.T) {
	d, _ := aggFixture(t)
	_, rows, _ := aggregate(t, d, "| stats dc(service)")
	if len(rows) != 1 || rows[0][0] != int64(3) {
		t.Fatalf("dc(service) = %v, want 3", rows)
	}
}

func TestAggregateTimechart(t *testing.T) {
	d, base := aggFixture(t)
	cols, rows, _ := aggregate(t, d, "| timechart span=1m count")

	if cols[0] != "bucket" {
		t.Fatalf("columns = %v", cols)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %v, want 3 one-minute buckets", rows)
	}
	// Ascending in time, and the bucket is a real timestamp.
	first, ok := rows[0][0].(time.Time)
	if !ok {
		t.Fatalf("bucket is %T, want time.Time", rows[0][0])
	}
	if first.After(base) {
		t.Errorf("first bucket %s is after the first event %s", first, base)
	}
	for i := 1; i < len(rows); i++ {
		prev := rows[i-1][0].(time.Time)
		cur := rows[i][0].(time.Time)
		if !cur.After(prev) {
			t.Errorf("buckets are not ascending: %s then %s", prev, cur)
		}
	}
	// Counts: 2 in the first minute, 2 in the second, 2 in the third.
	total := int64(0)
	for _, r := range rows {
		total += r[1].(int64)
	}
	if total != 6 {
		t.Errorf("timechart total = %d, want 6", total)
	}
}

func TestAggregateTopAndRare(t *testing.T) {
	d, _ := aggFixture(t)

	_, top, _ := aggregate(t, d, "| top 2 service")
	if len(top) != 2 {
		t.Fatalf("top 2 returned %d rows", len(top))
	}
	if top[0][0] != "api" {
		t.Errorf("top service = %v, want api", top[0][0])
	}

	_, rare, _ := aggregate(t, d, "| rare 1 service")
	if len(rare) != 1 || rare[0][0] != "db" {
		t.Errorf("rare service = %v, want db", rare)
	}
}

// TestAggregateGroupCapIsReported: grouping by a high-cardinality field must
// return a bounded, explicitly-truncated table rather than a row per event.
func TestAggregateGroupCapIsReported(t *testing.T) {
	d := newTestDB(t)
	base := time.Now()
	events := make([]model.LogEvent, 0, 30)
	for i := 0; i < 30; i++ {
		e := model.LogEvent{Service: fmt.Sprintf("svc-%02d", i), Message: "x"}
		e.Normalize(base.Add(time.Duration(i) * time.Millisecond))
		events = append(events, e)
	}
	if err := d.Append(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	q, err := query.Params{Q: "| top 10 service"}.Build(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Aggregate(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 10 {
		t.Fatalf("rows = %d, want the requested 10", len(res.Rows))
	}
	if !res.Truncated {
		t.Error("more groups existed than were returned; Truncated must say so")
	}
}

// TestAggregateMissingAttributeGroupsAsEmpty: a typo or an attribute only some
// events carry must not produce a null that renders as "<nil>".
func TestAggregateMissingAttributeGroupsAsEmpty(t *testing.T) {
	d, _ := aggFixture(t)
	_, rows, _ := aggregate(t, d, "| stats count by attr.nonexistent")
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one group", rows)
	}
	if rows[0][0] != "" {
		t.Errorf("missing attribute grouped as %#v, want an empty label", rows[0][0])
	}
}

func TestAggregateRejectsQueryWithoutStage(t *testing.T) {
	d, _ := aggFixture(t)
	q, err := query.Params{Q: "level=error"}.Build(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Aggregate(context.Background(), q); err == nil {
		t.Fatal("expected an error when the query has no aggregation stage")
	}
}

// TestAggregateFreeTextUsesTheIndex: an aggregation over a free-text filter
// must join the FTS table like a search does.
func TestAggregateFreeTextUsesTheIndex(t *testing.T) {
	d, _ := aggFixture(t)
	_, rows, _ := aggregate(t, d, "event | stats count")
	if len(rows) != 1 || rows[0][0] != int64(6) {
		t.Fatalf("free-text aggregation = %v, want 6", rows)
	}
	_, none, _ := aggregate(t, d, "zzzz | stats count")
	if len(none) != 1 || none[0][0] != int64(0) {
		t.Fatalf("non-matching free-text aggregation = %v, want 0", none)
	}
}
