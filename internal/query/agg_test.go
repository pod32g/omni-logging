package query

import (
	"strings"
	"testing"
	"time"
)

func TestSplitPipeline(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"level=error", []string{"level=error"}},
		{"level=error | stats count", []string{"level=error ", " stats count"}},
		{"| stats count", []string{"", " stats count"}},
		// A '|' inside quotes belongs to the value, not the pipeline.
		{`message="a|b" | stats count`, []string{`message="a|b" `, " stats count"}},
	} {
		got := SplitPipeline(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("SplitPipeline(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitPipeline(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseAggregationStats(t *testing.T) {
	a, err := ParseAggregation("stats count, avg(attr.latency_ms) by service, level")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Kind != KindStats {
		t.Errorf("kind = %q", a.Kind)
	}
	if len(a.Exprs) != 2 {
		t.Fatalf("exprs = %+v, want 2", a.Exprs)
	}
	if a.Exprs[0].Func != FuncCount || a.Exprs[0].Alias != "count" {
		t.Errorf("first measure = %+v", a.Exprs[0])
	}
	if a.Exprs[1].Func != FuncAvg || a.Exprs[1].Ref.Attr != "latency_ms" {
		t.Errorf("second measure = %+v", a.Exprs[1])
	}
	if a.Exprs[1].Alias != "avg(latency_ms)" {
		t.Errorf("alias = %q", a.Exprs[1].Alias)
	}
	if len(a.GroupBy) != 2 || a.GroupBy[0].Field != FieldService || a.GroupBy[1].Field != FieldLevel {
		t.Errorf("group by = %+v", a.GroupBy)
	}
	want := []string{"service", "level", "count", "avg(latency_ms)"}
	if cols := a.Columns(); strings.Join(cols, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", cols, want)
	}
}

// TestParseAggregationSpaceOrComma: measures and group fields may be separated
// either way, so a user does not have to remember which.
func TestParseAggregationSpaceOrComma(t *testing.T) {
	withCommas, err := ParseAggregation("stats count, max(attr.x) by service, source")
	if err != nil {
		t.Fatal(err)
	}
	withSpaces, err := ParseAggregation("stats count max(attr.x) by service source")
	if err != nil {
		t.Fatal(err)
	}
	if len(withCommas.Exprs) != len(withSpaces.Exprs) || len(withCommas.GroupBy) != len(withSpaces.GroupBy) {
		t.Fatalf("comma form %+v differs from space form %+v", withCommas, withSpaces)
	}
}

func TestParseAggregationTimechart(t *testing.T) {
	a, err := ParseAggregation("timechart span=5m count by level")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Span != 5*time.Minute {
		t.Errorf("span = %s, want 5m", a.Span)
	}
	if cols := a.Columns(); cols[0] != "bucket" {
		t.Errorf("columns = %v, want the time bucket first", cols)
	}

	// span defaults rather than erroring.
	d, err := ParseAggregation("timechart count")
	if err != nil {
		t.Fatal(err)
	}
	if d.Span != time.Minute {
		t.Errorf("default span = %s, want 1m", d.Span)
	}

	if _, err := ParseAggregation("timechart count by a, b"); err == nil {
		t.Error("timechart should reject more than one 'by' field")
	}
	if _, err := ParseAggregation("stats span=5m count"); err == nil {
		t.Error("span= should be rejected on stats")
	}
}

func TestParseAggregationTopRare(t *testing.T) {
	a, err := ParseAggregation("top 5 attr.status")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.Kind != KindTop || a.Limit != 5 {
		t.Errorf("parsed = %+v", a)
	}
	if len(a.GroupBy) != 1 || a.GroupBy[0].Attr != "status" {
		t.Errorf("group by = %+v", a.GroupBy)
	}
	if len(a.Exprs) != 1 || a.Exprs[0].Func != FuncCount {
		t.Errorf("top must imply a count: %+v", a.Exprs)
	}

	d, err := ParseAggregation("rare service")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindRare || d.Limit != DefaultTopLimit {
		t.Errorf("parsed = %+v, want the default limit", d)
	}
}

func TestParseAggregationErrors(t *testing.T) {
	for _, tc := range []struct{ expr, why string }{
		{"nonsense count", "unknown command"},
		{"stats", "no measure"},
		{"stats bogus(x)", "unknown function"},
		{"stats sum()", "missing field"},
		{"top", "missing field"},
		{"top 0 service", "non-positive count"},
		{"timechart span=0m count", "zero span"},
		{"timechart span=notaduration count", "bad span"},
	} {
		if _, err := ParseAggregation(tc.expr); err == nil {
			t.Errorf("ParseAggregation(%q) succeeded, want an error (%s)", tc.expr, tc.why)
		}
	}
}

// TestBuildParsesPipeline: the filter half and the aggregation half must both
// survive the request-parameter path.
func TestBuildParsesPipeline(t *testing.T) {
	q, err := Params{Q: "level=error boom | stats count by service", Last: "1h"}.Build(time.Now())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !q.IsAggregation() {
		t.Fatal("aggregation stage was not parsed")
	}
	if len(q.Filters) != 1 || q.Filters[0].Field != FieldLevel {
		t.Errorf("filters lost: %+v", q.Filters)
	}
	if len(q.Terms) != 1 || q.Terms[0] != "boom" {
		t.Errorf("free-text term lost: %+v", q.Terms)
	}
	if q.From.IsZero() {
		t.Error("time window lost")
	}
	if q.Agg.Limit != MaxGroups {
		t.Errorf("limit = %d, want the group cap after Normalize", q.Agg.Limit)
	}
}

func TestBuildWithoutPipeline(t *testing.T) {
	q, err := Params{Q: "level=error"}.Build(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.IsAggregation() {
		t.Fatal("a plain query must not produce an aggregation")
	}
}

func TestBuildRejectsMultipleStages(t *testing.T) {
	_, err := Params{Q: "| stats count | stats count"}.Build(time.Now())
	if err == nil {
		t.Fatal("expected an error for two aggregation stages")
	}
	if !strings.Contains(err.Error(), "one aggregation") {
		t.Errorf("error = %v, want it to explain the limit", err)
	}
}
