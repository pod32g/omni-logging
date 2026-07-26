package alert

import (
	"context"
	"fmt"
	"time"

	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

// Runner is the slice of the store an evaluation needs. Narrow on purpose: an
// alert should be able to run against anything that can answer a query.
type Runner interface {
	Search(ctx context.Context, q query.Query) (store.SearchResult, error)
	Aggregate(ctx context.Context, q query.Query) (store.AggResult, error)
}

// evalTimeout bounds one evaluation so a pathological rule cannot occupy the
// scheduler indefinitely.
const evalTimeout = 30 * time.Second

// Evaluate runs a rule once against the window ending at now.
//
// A rule whose query has no aggregation compares the number of matching events.
// A rule that aggregates compares the first measure column, and reports every
// group that met the condition — so "error rate by service" tells you which
// service, not merely that some service crossed the line.
func Evaluate(ctx context.Context, r Runner, rule Rule, now time.Time) (Evaluation, error) {
	q, err := query.Params{Q: rule.Query}.Build(now)
	if err != nil {
		return Evaluation{}, fmt.Errorf("alert %q: invalid query: %w", rule.Name, err)
	}
	// The rule's own window always wins over anything the expression said, so a
	// stale absolute range in a saved query cannot freeze the alert on old data.
	q.From, q.To = now.Add(-rule.Window), now
	q.Normalize()

	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()

	if q.IsAggregation() {
		return evaluateAggregation(ctx, r, rule, q, now)
	}
	return evaluateCount(ctx, r, rule, q, now)
}

func evaluateCount(ctx context.Context, r Runner, rule Rule, q query.Query, now time.Time) (Evaluation, error) {
	// Only the count matters, so ask for as few rows as the store will return.
	q.Limit = 1
	res, err := r.Search(ctx, q)
	if err != nil {
		return Evaluation{}, fmt.Errorf("alert %q: %w", rule.Name, err)
	}
	v := float64(res.Total)
	return Evaluation{At: now, Value: v, Firing: rule.Cond.Match(v)}, nil
}

func evaluateAggregation(ctx context.Context, r Runner, rule Rule, q query.Query, now time.Time) (Evaluation, error) {
	res, err := r.Aggregate(ctx, q)
	if err != nil {
		return Evaluation{}, fmt.Errorf("alert %q: %w", rule.Name, err)
	}

	measureAt := res.GroupColumns
	if measureAt >= len(res.Columns) {
		return Evaluation{}, fmt.Errorf("alert %q: query produces no measure to compare", rule.Name)
	}

	ev := Evaluation{At: now}
	seen := false
	for _, row := range res.Rows {
		if measureAt >= len(row) {
			continue
		}
		v, ok := toFloat(row[measureAt])
		if !ok {
			continue
		}
		// Report the most extreme value in the direction the condition cares
		// about, so the single number in a notification is the worst case rather
		// than whichever group happened to sort last.
		if !seen || moreExtreme(rule.Cond.Op, v, ev.Value) {
			ev.Value = v
			seen = true
		}
		if rule.Cond.Match(v) {
			ev.Firing = true
			ev.Groups = append(ev.Groups, GroupHit{
				Labels: labelsFor(res.Columns, row, res.GroupColumns),
				Value:  v,
			})
		}
	}
	if !seen {
		// An empty aggregate compares as zero, so "count < 1" works as a
		// dead-man's switch for a service that has stopped logging entirely.
		ev.Value = 0
		ev.Firing = rule.Cond.Match(0)
	}
	return ev, nil
}

// moreExtreme reports whether a is further along the condition's direction than
// b, so the reported value is the worst case rather than an arbitrary row.
func moreExtreme(op Op, a, b float64) bool {
	switch op {
	case OpGT, OpGTE, OpNE:
		return a > b
	case OpLT, OpLTE:
		return a < b
	}
	return false
}

// labelsFor pairs the group columns of a row with their column names.
func labelsFor(cols []string, row []any, groupCols int) map[string]string {
	if groupCols == 0 {
		return nil
	}
	labels := make(map[string]string, groupCols)
	for i := 0; i < groupCols && i < len(row) && i < len(cols); i++ {
		labels[cols[i]] = fmt.Sprintf("%v", row[i])
	}
	return labels
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}
