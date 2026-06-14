package query

import (
	"strconv"
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
}

// Build parses Params into a Query, resolving relative times against now.
// Unspecified bounds are left zero (unbounded). Invalid values return an error
// so the caller can respond with 400.
func (p Params) Build(now time.Time) (Query, error) {
	q, err := Parse(p.Q)
	if err != nil {
		return Query{}, err
	}

	if p.To != "" {
		t, err := ParseTime(p.To)
		if err != nil {
			return Query{}, err
		}
		q.To = t
	}
	switch {
	case p.From != "":
		t, err := ParseTime(p.From)
		if err != nil {
			return Query{}, err
		}
		q.From = t
	case p.Last != "":
		d, err := ParseRelative(p.Last)
		if err != nil {
			return Query{}, err
		}
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

	q.Normalize()
	return q, nil
}
