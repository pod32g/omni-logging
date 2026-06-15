package query

import (
	"reflect"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

func TestParse_FiltersAndTerms(t *testing.T) {
	q, err := Parse(`level=error service=checkout-api timeout attr.user_id=42 host!=node-9 "connection refused"`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if len(q.Filters) != 4 {
		t.Fatalf("got %d filters, want 4: %+v", len(q.Filters), q.Filters)
	}
	want := []Filter{
		{Field: FieldLevel, Op: OpEq, Value: "error"},
		{Field: FieldService, Op: OpEq, Value: "checkout-api"},
		{Field: FieldAttr, Attr: "user_id", Op: OpEq, Value: "42"},
		{Field: FieldSource, Op: OpNeq, Value: "node-9"},
	}
	for i, w := range want {
		if !reflect.DeepEqual(q.Filters[i], w) {
			t.Errorf("filter[%d] = %+v, want %+v", i, q.Filters[i], w)
		}
	}

	if len(q.Terms) != 2 || q.Terms[0] != "timeout" || q.Terms[1] != "connection refused" {
		t.Errorf("terms = %v, want [timeout, \"connection refused\"]", q.Terms)
	}
}

func TestParse_BareKeyIsAttribute(t *testing.T) {
	q, err := Parse(`request_id=abc123`)
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	if len(q.Filters) != 1 || q.Filters[0].Field != FieldAttr || q.Filters[0].Attr != "request_id" {
		t.Fatalf("expected attribute filter on request_id, got %+v", q.Filters)
	}
}

func TestParse_UnterminatedQuote(t *testing.T) {
	if _, err := Parse(`level=error "oops`); err == nil {
		t.Error("expected error for unterminated quote")
	}
}

func TestParseRelative(t *testing.T) {
	cases := map[string]time.Duration{
		"30s": 30 * time.Second,
		"15m": 15 * time.Minute,
		"2h":  2 * time.Hour,
		"7d":  7 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseRelative(in)
		if err != nil || got != want {
			t.Errorf("ParseRelative(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseRelative("10x"); err == nil {
		t.Error("expected error for bad unit")
	}
}

func TestBuild_RelativeWindowAndDefaults(t *testing.T) {
	now := time.Date(2026, 6, 14, 15, 57, 0, 0, time.UTC)
	p := Params{Q: "level=error boom", Last: "15m"}
	q, err := p.Build(now)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if !q.From.Equal(now.Add(-15 * time.Minute)) {
		t.Errorf("From = %v, want %v", q.From, now.Add(-15*time.Minute))
	}
	if q.Limit != DefaultLimit {
		t.Errorf("Limit = %d, want default %d", q.Limit, DefaultLimit)
	}
	if q.Order != OrderNewest {
		t.Errorf("Order = %q, want newest", q.Order)
	}
}

func TestBuild_ClampsLimit(t *testing.T) {
	now := time.Now()
	q, err := (Params{Limit: "999999"}).Build(now)
	if err != nil {
		t.Fatalf("Build error: %v", err)
	}
	if q.Limit != MaxLimit {
		t.Errorf("Limit = %d, want clamped to %d", q.Limit, MaxLimit)
	}
}

func TestMatches(t *testing.T) {
	base := model.LogEvent{
		Timestamp: time.Date(2026, 6, 14, 15, 50, 0, 0, time.UTC),
		Service:   "checkout-api",
		Source:    "node-1",
		Level:     model.LevelError,
		Message:   "upstream request timeout calling payments",
		Attributes: map[string]any{
			"user_id": float64(42),
			"status":  float64(504),
		},
	}

	mustMatch := func(expr string, want bool) {
		t.Helper()
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
		if got := q.Matches(base); got != want {
			t.Errorf("Matches(%q) = %v, want %v", expr, got, want)
		}
	}

	mustMatch("level=error", true)
	mustMatch("level=info", false)
	mustMatch("level!=info", true)
	mustMatch("service=checkout-api timeout", true)
	mustMatch("timeout payments", true)     // multiple terms, AND
	mustMatch("timeout nonexistent", false) // one term missing
	mustMatch("attr.user_id=42", true)      // numeric attr stringified
	mustMatch("attr.user_id=99", false)
	mustMatch("attr.missing=1", false)   // missing attr, == fails
	mustMatch("attr.missing!=1", true)   // missing attr, != passes
	mustMatch(`"request timeout"`, true) // phrase substring
}

func TestMatches_TimeBounds(t *testing.T) {
	e := model.LogEvent{Timestamp: time.Date(2026, 6, 14, 15, 50, 0, 0, time.UTC)}
	q := Query{From: time.Date(2026, 6, 14, 15, 55, 0, 0, time.UTC)}
	if q.Matches(e) {
		t.Error("event before From should not match")
	}
	q2 := Query{To: time.Date(2026, 6, 14, 15, 45, 0, 0, time.UTC)}
	if q2.Matches(e) {
		t.Error("event after To should not match")
	}
}
