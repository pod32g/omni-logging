package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file adds the piped aggregation stage that turns a filter into an
// analytic query:
//
//	level=error | stats count by service
//	| timechart span=5m count by level
//	service=api | top 5 attr.status
//
// The filter half is unchanged; everything after the first '|' is parsed here.

// AggFunc is an aggregation function.
type AggFunc string

const (
	FuncCount    AggFunc = "count"
	FuncSum      AggFunc = "sum"
	FuncAvg      AggFunc = "avg"
	FuncMin      AggFunc = "min"
	FuncMax      AggFunc = "max"
	FuncDistinct AggFunc = "dc" // distinct count
)

// AggKind is the pipeline command.
type AggKind string

const (
	KindStats     AggKind = "stats"
	KindTimechart AggKind = "timechart"
	KindTop       AggKind = "top"
	KindRare      AggKind = "rare"
)

// FieldRef names either a first-class column or an attribute key.
type FieldRef struct {
	Field Field  // FieldAttr when Attr is set
	Attr  string // attribute key when Field == FieldAttr
	Name  string // label as written, used as the output column name
}

// AggExpr is one output measure, e.g. count or avg(attr.latency_ms).
type AggExpr struct {
	Func  AggFunc
	Ref   FieldRef // unused for a bare count
	Alias string   // output column name
}

// Aggregation is a parsed pipeline stage.
type Aggregation struct {
	Kind    AggKind
	Exprs   []AggExpr
	GroupBy []FieldRef
	Span    time.Duration // timechart bucket width
	Limit   int           // top/rare row count
}

// DefaultTopLimit is how many rows top/rare return when no count is given.
const DefaultTopLimit = 10

// MaxGroups bounds how many groups an aggregation returns. Grouping by a
// high-cardinality field (a request id, say) would otherwise try to build a row
// per event and return a result no one can read.
const MaxGroups = 1000

// defaultTimechartSpan is used when a timechart omits span=.
const defaultTimechartSpan = time.Minute

// SplitPipeline splits an expression on top-level '|', leaving any '|' inside a
// quoted string alone so a regex like message=~a|b survives when quoted.
func SplitPipeline(expr string) []string {
	var (
		parts   []string
		b       strings.Builder
		inQuote bool
	)
	for _, r := range expr {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == '|' && !inQuote:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	parts = append(parts, b.String())
	return parts
}

// ParseAggregation parses one pipeline stage such as "stats count by service".
func ParseAggregation(stage string) (*Aggregation, error) {
	fields := strings.Fields(stage)
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty pipeline stage")
	}
	switch AggKind(strings.ToLower(fields[0])) {
	case KindStats:
		return parseStats(KindStats, fields[1:])
	case KindTimechart:
		return parseStats(KindTimechart, fields[1:])
	case KindTop:
		return parseTopRare(KindTop, fields[1:])
	case KindRare:
		return parseTopRare(KindRare, fields[1:])
	default:
		return nil, fmt.Errorf("unknown pipeline command %q (use stats, timechart, top, or rare)", fields[0])
	}
}

// parseStats handles "[span=5m] <agg>... [by <field>...]" for stats/timechart.
func parseStats(kind AggKind, args []string) (*Aggregation, error) {
	a := &Aggregation{Kind: kind}
	if kind == KindTimechart {
		a.Span = defaultTimechartSpan
	}

	// Leading span= applies to timechart only.
	for len(args) > 0 && strings.HasPrefix(strings.ToLower(args[0]), "span=") {
		if kind != KindTimechart {
			return nil, fmt.Errorf("span= is only valid on timechart")
		}
		d, err := ParseRelative(args[0][len("span="):])
		if err != nil {
			return nil, fmt.Errorf("invalid span: %w", err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("span must be greater than zero")
		}
		a.Span = d
		args = args[1:]
	}

	// Split the measures from the "by" clause.
	measures, groups := args, []string(nil)
	for i, f := range args {
		if strings.EqualFold(f, "by") {
			measures, groups = args[:i], args[i+1:]
			break
		}
	}
	if len(measures) == 0 {
		return nil, fmt.Errorf("%s needs at least one aggregation, e.g. %s count", kind, kind)
	}

	for _, m := range splitCommaList(measures) {
		expr, err := parseAggExpr(m)
		if err != nil {
			return nil, err
		}
		a.Exprs = append(a.Exprs, expr)
	}
	for _, g := range splitCommaList(groups) {
		a.GroupBy = append(a.GroupBy, resolveRef(g))
	}
	if kind == KindTimechart && len(a.GroupBy) > 1 {
		return nil, fmt.Errorf("timechart supports at most one 'by' field")
	}
	return a, nil
}

// parseTopRare handles "top [N] <field>" and its rare counterpart, which are
// shorthand for a count aggregation ordered in one direction or the other.
func parseTopRare(kind AggKind, args []string) (*Aggregation, error) {
	a := &Aggregation{Kind: kind, Limit: DefaultTopLimit}
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			if n <= 0 {
				return nil, fmt.Errorf("%s count must be greater than zero", kind)
			}
			a.Limit = n
			args = args[1:]
		}
	}
	refs := splitCommaList(args)
	if len(refs) == 0 {
		return nil, fmt.Errorf("%s needs a field, e.g. %s service", kind, kind)
	}
	for _, r := range refs {
		a.GroupBy = append(a.GroupBy, resolveRef(r))
	}
	a.Exprs = []AggExpr{{Func: FuncCount, Alias: "count"}}
	if a.Limit > MaxGroups {
		a.Limit = MaxGroups
	}
	return a, nil
}

// splitCommaList flattens tokens that may be comma-separated, space-separated,
// or both, so "count, avg(x)" and "count avg(x)" parse the same.
func splitCommaList(tokens []string) []string {
	var out []string
	for _, t := range tokens {
		for _, p := range strings.Split(t, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// parseAggExpr parses "count", "count(field)", "avg(field)" and friends.
func parseAggExpr(s string) (AggExpr, error) {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)

	if lower == "count" || lower == "count()" {
		return AggExpr{Func: FuncCount, Alias: "count"}, nil
	}

	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return AggExpr{}, fmt.Errorf("unknown aggregation %q (use count, or func(field) with sum/avg/min/max/dc)", s)
	}
	name := AggFunc(strings.ToLower(strings.TrimSpace(s[:open])))
	arg := strings.TrimSpace(s[open+1 : len(s)-1])
	if arg == "" {
		if name == FuncCount {
			return AggExpr{Func: FuncCount, Alias: "count"}, nil
		}
		return AggExpr{}, fmt.Errorf("%s() needs a field", name)
	}

	switch name {
	case FuncCount, FuncSum, FuncAvg, FuncMin, FuncMax, FuncDistinct:
	default:
		return AggExpr{}, fmt.Errorf("unknown aggregation function %q", name)
	}
	ref := resolveRef(arg)
	return AggExpr{Func: name, Ref: ref, Alias: string(name) + "(" + ref.Name + ")"}, nil
}

// resolveRef maps a written name onto a column or an attribute, using the same
// rules as filter keys so "service" and "attr.user_id" mean the same thing in
// both halves of the query.
func resolveRef(name string) FieldRef {
	name = strings.TrimSpace(name)
	lower := strings.ToLower(name)
	if f, ok := knownFields[lower]; ok {
		return FieldRef{Field: f, Name: lower}
	}
	attr := name
	if strings.HasPrefix(lower, "attr.") {
		attr = name[len("attr."):]
	}
	return FieldRef{Field: FieldAttr, Attr: attr, Name: attr}
}

// Columns returns the output column names in order: the group-by labels (with
// the time bucket first for a timechart), then one per measure.
func (a *Aggregation) Columns() []string {
	cols := make([]string, 0, len(a.GroupBy)+len(a.Exprs)+1)
	if a.Kind == KindTimechart {
		cols = append(cols, "bucket")
	}
	for _, g := range a.GroupBy {
		cols = append(cols, g.Name)
	}
	for _, e := range a.Exprs {
		cols = append(cols, e.Alias)
	}
	return cols
}

// Normalize fills in defaults and clamps the row limit.
func (a *Aggregation) Normalize() {
	if a.Kind == KindTimechart && a.Span <= 0 {
		a.Span = defaultTimechartSpan
	}
	if a.Limit <= 0 || a.Limit > MaxGroups {
		a.Limit = MaxGroups
	}
}
