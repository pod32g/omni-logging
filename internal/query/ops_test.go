package query

import (
	"reflect"
	"testing"

	"github.com/pod32g/omni-logging/internal/model"
)

func TestParse_Operators(t *testing.T) {
	cases := []struct {
		expr string
		want Filter
	}{
		{"level=error", Filter{Field: FieldLevel, Op: OpEq, Value: "error"}},
		{"level!=error", Filter{Field: FieldLevel, Op: OpNeq, Value: "error"}},
		{"attr.status>=500", Filter{Field: FieldAttr, Attr: "status", Op: OpGte, Value: "500"}},
		{"attr.status>500", Filter{Field: FieldAttr, Attr: "status", Op: OpGt, Value: "500"}},
		{"attr.latency<10", Filter{Field: FieldAttr, Attr: "latency", Op: OpLt, Value: "10"}},
		{"attr.latency<=10", Filter{Field: FieldAttr, Attr: "latency", Op: OpLte, Value: "10"}},
		{"service=checkout*", Filter{Field: FieldService, Op: OpLike, Value: "checkout*"}},
		{"attr.user_id=*", Filter{Field: FieldAttr, Attr: "user_id", Op: OpExists}},
		{"level=(error,warn,fatal)", Filter{Field: FieldLevel, Op: OpIn, Values: []string{"error", "warn", "fatal"}}},
		{"message=~time.*out", Filter{Field: FieldMessage, Op: OpRegex, Value: "time.*out"}},
		{"msg=hello", Filter{Field: FieldMessage, Op: OpEq, Value: "hello"}},
		{"user_id=42", Filter{Field: FieldAttr, Attr: "user_id", Op: OpEq, Value: "42"}}, // bare key -> attr
	}
	for _, tc := range cases {
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if len(q.Filters) != 1 {
			t.Fatalf("Parse(%q) produced %d filters, want 1 (%+v)", tc.expr, len(q.Filters), q)
		}
		if !reflect.DeepEqual(q.Filters[0], tc.want) {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.expr, q.Filters[0], tc.want)
		}
	}
}

func TestMatches_Operators(t *testing.T) {
	e := model.LogEvent{
		Service: "checkout-api", Source: "node-1", Level: model.LevelError,
		Message:    "upstream request timeout",
		Attributes: map[string]any{"status": float64(504), "user_id": float64(42)},
	}
	cases := []struct {
		expr string
		want bool
	}{
		{"level=error", true},
		{"level=info", false},
		{"level=(info,error)", true},
		{"level=(info,warn)", false},
		{"attr.status>=500", true},
		{"attr.status>504", false},
		{"attr.status>=504", true},
		{"attr.status<600", true},
		{"service=checkout*", true},
		{"service=auth*", false},
		{"attr.user_id=*", true},   // exists
		{"attr.missing=*", false},  // exists on absent attr
		{"attr.missing!=42", true}, // != on absent attr is true
		{"message=~time\\w+", true},
		{"message=~^upstream", true},
		{"message=~^downstream", false},
		{"service=*api", true}, // suffix wildcard
	}
	for _, tc := range cases {
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if got := q.Matches(e); got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestParse_InvalidRegexErrors(t *testing.T) {
	if _, err := Parse("message=~(unclosed"); err == nil {
		t.Fatal("expected error for invalid regex")
	}
}
