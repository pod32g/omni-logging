// Package query defines the search query model, a small query-language parser,
// and an in-memory matcher used by live tail. The same Query value drives both
// SQL execution (in the store) and in-memory matching (in tail).
package query

import (
	"time"
)

// Order controls result sort direction by timestamp.
type Order string

const (
	OrderNewest Order = "newest" // most recent first (default)
	OrderOldest Order = "oldest"
)

// Field identifies which part of an event a filter applies to.
type Field string

const (
	FieldLevel   Field = "level"
	FieldService Field = "service"
	FieldSource  Field = "source"
	FieldMessage Field = "message"
	FieldRaw     Field = "raw"
	FieldAttr    Field = "attr" // attribute lookup; Filter.Attr holds the key
)

// Op is a filter comparison operator.
type Op string

const (
	OpEq     Op = "="      // equals (case-insensitive for fields)
	OpNeq    Op = "!="     // not equals (a missing attribute satisfies this)
	OpGt     Op = ">"      // greater than (numeric when both sides parse as numbers)
	OpGte    Op = ">="     // greater than or equal
	OpLt     Op = "<"      // less than
	OpLte    Op = "<="     // less than or equal
	OpLike   Op = "like"   // glob wildcard (value contains '*')
	OpExists Op = "exists" // field present / attribute non-null (value '*')
	OpIn     Op = "in"     // value in a set: key=(a,b,c)
	OpRegex  Op = "regex"  // RE2 match: key=~pattern
)

// Filter is a single structured constraint, e.g. level=error,
// attr.status>=500, service=checkout*, or level=(error,warn). Filters are
// AND-combined (see the package docs on OR-grouping).
type Filter struct {
	Field  Field
	Attr   string   // attribute key when Field == FieldAttr
	Op     Op       // comparison operator
	Value  string   // operand for most operators
	Values []string // operands for OpIn
}

// Query is a fully parsed search request.
type Query struct {
	Terms    []string // free-text terms, AND-combined
	Filters  []Filter
	Time     TimeSpec      // time directives written in the expression, unresolved
	From, To time.Time     // inclusive lower / upper bound on event time (zero = unbounded)
	Limit    int           // max events to return
	Order    Order         // sort direction
	Interval time.Duration // histogram bucket width for Stats

	// Keyset pagination cursor: when AfterID is set, results continue strictly
	// after (AfterTS, AfterID) in the query's sort order. Stable under concurrent
	// ingest (unlike OFFSET).
	AfterTS time.Time
	AfterID string

	// Agg is the piped aggregation stage, if the expression had one. When set,
	// the query produces a table of grouped measures instead of events; the
	// filter half still applies exactly as it does for a plain search.
	Agg *Aggregation
}

// TimeSpec holds the time directives written in a query expression —
// `last=15m`, `from=…`, `to=…` — exactly as typed. Parse records them but does
// not resolve them, because resolving a relative window needs a clock and Parse
// deliberately has none. Build turns them into From/To.
//
// They stay inert until something resolves them, which is what makes it safe
// for callers that parse an expression without a clock (ingest pipelines) to
// ignore time entirely.
type TimeSpec struct {
	Last string // relative window, e.g. "15m"
	From string // absolute lower bound (RFC3339 or unix seconds)
	To   string // absolute upper bound
}

// IsZero reports whether the expression named no time bound at all.
func (t TimeSpec) IsZero() bool { return t.Last == "" && t.From == "" && t.To == "" }

// HasLowerBound reports whether the expression pinned the start of the range,
// by either an absolute from or a relative window.
func (t TimeSpec) HasLowerBound() bool { return t.From != "" || t.Last != "" }

// IsAggregation reports whether this query produces a table rather than events.
func (q Query) IsAggregation() bool { return q.Agg != nil }

// DefaultLimit and MaxLimit bound how many events a single search returns.
const (
	DefaultLimit = 100
	MaxLimit     = 5000
)

// Normalize fills in sane defaults and clamps out-of-range values so the rest
// of the system can trust the Query.
func (q *Query) Normalize() {
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	if q.Order != OrderOldest {
		q.Order = OrderNewest
	}
	if q.Agg != nil {
		q.Agg.Normalize()
	}
}
