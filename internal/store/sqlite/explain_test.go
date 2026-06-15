package sqlite

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/query"
)

var updateGolden = flag.Bool("update", false, "update golden EXPLAIN QUERY PLAN files")

// TestExplainQueryPlan locks in the query plan for each representative query
// shape. If a change makes a query stop using its index (e.g. a regression while
// tuning in M20), the golden plan no longer matches and this test fails. To
// intentionally update the baselines: go test ./internal/store/sqlite -run
// TestExplainQueryPlan -update, then review the diff.
func TestExplainQueryPlan(t *testing.T) {
	db := newTestDB(t)
	seed(t, db) // give the planner a populated schema

	base := time.Date(2026, 6, 14, 15, 0, 0, 0, time.UTC)
	timeRange, _ := query.Parse("")
	timeRange.From = base
	timeRange.To = base.Add(time.Hour)
	timeRange.Normalize()

	cases := []struct {
		name  string
		build func() (string, []any)
	}{
		{"search_freetext", func() (string, []any) { return searchSQL(parseN("timeout")) }},
		{"search_level", func() (string, []any) { return searchSQL(parseN("level=error")) }},
		{"search_attr", func() (string, []any) { return searchSQL(parseN("attr.user_id=42")) }},
		{"search_timerange", func() (string, []any) { return searchSQL(timeRange) }},
		{"count_level", func() (string, []any) { return countSQL(parseN("level=error")) }},
		{"histogram", func() (string, []any) { return histogramSQL(parseN(""), time.Minute.Nanoseconds()) }},
		{"facet_level", func() (string, []any) { return facetSQL(parseN(""), "level") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := tc.build()
			plan := queryPlan(t, db, sql, args)
			golden := filepath.Join("testdata", tc.name+".plan")
			if *updateGolden {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(plan), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if plan != string(want) {
				t.Fatalf("query plan changed for %s:\n--- got ---\n%s\n--- want ---\n%s\n(run -update if intentional)", tc.name, plan, want)
			}
		})
	}
}

// queryPlan runs EXPLAIN QUERY PLAN and returns the joined detail lines.
func queryPlan(t *testing.T, db *DB, sql string, args []any) string {
	t.Helper()
	rows, err := db.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+sql, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, strings.TrimSpace(detail))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return strings.Join(lines, "\n") + "\n"
}

// parseN parses an expression and normalizes it the way Search/Stats do.
func parseN(expr string) query.Query {
	q, err := query.Parse(expr)
	if err != nil {
		panic(err)
	}
	q.Normalize()
	return q
}
