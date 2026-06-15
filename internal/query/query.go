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
	From, To time.Time     // inclusive lower / upper bound on event time (zero = unbounded)
	Limit    int           // max events to return
	Order    Order         // sort direction
	Interval time.Duration // histogram bucket width for Stats

	// Keyset pagination cursor: when AfterID is set, results continue strictly
	// after (AfterTS, AfterID) in the query's sort order. Stable under concurrent
	// ingest (unlike OFFSET).
	AfterTS time.Time
	AfterID string
}

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
}
