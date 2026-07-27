package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Params is the raw, stringly-typed search input as it arrives from an HTTP
// request. Build resolves it into a normalized Query.
type Params struct {
	Q        string // query expression
	From     string // absolute lower bound (RFC3339 / unix seconds)
	To       string // absolute upper bound
	Last     string // relative window (e.g. "15m"); used when From is empty
	Limit    string
	Order    string
	Interval string // histogram bucket (e.g. "1m"); Stats only
	After    string // keyset pagination cursor (opaque, from a prior result)
}

// Build parses Params into a Query, resolving relative times against now.
// Unspecified bounds are left zero (unbounded). Invalid values return an error
// so the caller can respond with 400.
func (p Params) Build(now time.Time) (Query, error) {
	// Everything before the first '|' is the filter; the rest is the pipeline.
	segments := SplitPipeline(p.Q)
	q, err := Parse(segments[0])
	if err != nil {
		return Query{}, err
	}
	for _, stage := range segments[1:] {
		if strings.TrimSpace(stage) == "" {
			continue
		}
		if q.Agg != nil {
			return Query{}, fmt.Errorf("only one aggregation stage is supported")
		}
		agg, aerr := ParseAggregation(stage)
		if aerr != nil {
			return Query{}, aerr
		}
		q.Agg = agg
	}

	// Time bounds: a bound written in the expression wins over the request
	// parameter. The expression is the more specific statement — someone who
	// types "last=5m" into the search box means it, and having the range picker
	// silently override it would be the same class of surprise as the bug that
	// made the expression form do nothing at all.
	//
	// The two bounds are decided independently, but each is decided wholly by
	// one source: if the expression names any lower bound, the request's From
	// and Last are both ignored, so a `from=` in the query cannot be
	// half-overridden by a `last=` on the request.
	toExpr := q.Time.To
	if toExpr == "" {
		toExpr = p.To
	}
	if toExpr != "" {
		t, err := ParseTime(toExpr)
		if err != nil {
			return Query{}, err
		}
		q.To = t
	}

	fromExpr, lastExpr := q.Time.From, q.Time.Last
	if !q.Time.HasLowerBound() {
		fromExpr, lastExpr = p.From, p.Last
	}
	switch {
	case fromExpr != "":
		t, err := ParseTime(fromExpr)
		if err != nil {
			return Query{}, err
		}
		q.From = t
	case lastExpr != "":
		d, err := ParseRelative(lastExpr)
		if err != nil {
			return Query{}, err
		}
		// Anchor the window to the upper bound when one was given, so
		// "from=… to=…"-style ranges and "last=" compose predictably.
		upper := q.To
		if upper.IsZero() {
			upper = now
		}
		q.From = upper.Add(-d)
	}

	if p.Limit != "" {
		n, err := strconv.Atoi(p.Limit)
		if err != nil {
			return Query{}, err
		}
		q.Limit = n
	}
	if Order(p.Order) == OrderOldest {
		q.Order = OrderOldest
	}
	if p.Interval != "" {
		d, err := ParseRelative(p.Interval)
		if err != nil {
			return Query{}, err
		}
		q.Interval = d
	}
	if p.After != "" {
		ts, id, err := DecodeCursor(p.After)
		if err != nil {
			return Query{}, err
		}
		q.AfterTS, q.AfterID = ts, id
	}

	q.Normalize()
	return q, nil
}
