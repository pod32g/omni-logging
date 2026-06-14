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
	FieldAttr    Field = "attr" // attribute lookup; Filter.Attr holds the key
)

// Filter is a single structured constraint, e.g. level=error or
// attr.user_id=42 or service!=worker.
type Filter struct {
	Field  Field
	Attr   string // attribute key when Field == FieldAttr
	Negate bool   // true for the != operator
	Value  string
}

// Query is a fully parsed search request.
type Query struct {
	Terms    []string // free-text terms, AND-combined
	Filters  []Filter
	From, To time.Time     // inclusive lower / upper bound on event time (zero = unbounded)
	Limit    int           // max events to return
	Order    Order         // sort direction
	Interval time.Duration // histogram bucket width for Stats
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
