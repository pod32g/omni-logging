package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

// refExpr renders a field reference as a SQL expression, plus any bound args it
// needs. Attribute lookups bind the JSON path as a parameter rather than
// splicing it into the statement, exactly as the filter builder does.
func refExpr(r query.FieldRef) (string, []any) {
	if r.Field == query.FieldAttr {
		return "CAST(json_extract(logs.attributes, ?) AS TEXT)", []any{jsonPath(r.Attr)}
	}
	if col := columnFor(r.Field); col != "" {
		return col, nil
	}
	// An unknown reference yields NULL rather than an error, so a typo groups
	// everything under one empty bucket instead of failing the whole query.
	return "NULL", nil
}

// numericRefExpr renders a reference for arithmetic aggregation. SQLite's CAST
// to REAL yields 0 for non-numeric text, matching how the comparison operators
// in the filter half coerce.
func numericRefExpr(r query.FieldRef) (string, []any) {
	if r.Field == query.FieldAttr {
		return "CAST(json_extract(logs.attributes, ?) AS REAL)", []any{jsonPath(r.Attr)}
	}
	if col := columnFor(r.Field); col != "" {
		return "CAST(" + col + " AS REAL)", nil
	}
	return "NULL", nil
}

// aggSelect renders one measure as a SQL aggregate expression.
func aggSelect(e query.AggExpr) (string, []any) {
	switch e.Func {
	case query.FuncCount:
		return "COUNT(*)", nil
	case query.FuncDistinct:
		expr, args := refExpr(e.Ref)
		return "COUNT(DISTINCT " + expr + ")", args
	case query.FuncSum, query.FuncAvg, query.FuncMin, query.FuncMax:
		expr, args := numericRefExpr(e.Ref)
		return strings.ToUpper(string(e.Func)) + "(" + expr + ")", args
	}
	return "COUNT(*)", nil
}

// aggregateSQL builds the grouped statement and its bound args.
//
// Argument order matters and is fragile to reorder: SQLite binds positionally,
// so the SELECT list's args must precede the WHERE clause's, and the trailing
// LIMIT arg must come last.
func aggregateSQL(q query.Query) (string, []any) {
	a := q.Agg
	var (
		selects []string
		groups  []string
		args    []any
	)

	if a.Kind == query.KindTimechart {
		span := a.Span.Nanoseconds()
		// Integer-divide the timestamp into buckets. span is an int64 from a
		// validated duration, never user text spliced in raw.
		bucket := fmt.Sprintf("(logs.ts / %d) * %d", span, span)
		selects = append(selects, bucket+" AS bucket")
		groups = append(groups, "bucket")
	}
	for i, g := range a.GroupBy {
		expr, gargs := refExpr(g)
		alias := fmt.Sprintf("g%d", i)
		selects = append(selects, expr+" AS "+alias)
		args = append(args, gargs...)
		groups = append(groups, alias)
	}
	for i, e := range a.Exprs {
		expr, eargs := aggSelect(e)
		selects = append(selects, fmt.Sprintf("%s AS m%d", expr, i))
		args = append(args, eargs...)
	}

	w := buildWhere(q)
	args = append(args, w.args...)

	sb := &strings.Builder{}
	fmt.Fprintf(sb, "SELECT %s %s %s", strings.Join(selects, ", "), fromClause(w.needFTS), w.sqlStr())
	if len(groups) > 0 {
		fmt.Fprintf(sb, " GROUP BY %s", strings.Join(groups, ", "))
	}
	sb.WriteString(" ORDER BY " + orderClause(a))
	sb.WriteString(" LIMIT ?")
	args = append(args, a.Limit+1) // one extra row tells us the cap was hit

	return sb.String(), args
}

// orderClause decides row order: time for a timechart, ascending count for
// rare, and descending count otherwise so the largest groups come first.
func orderClause(a *query.Aggregation) string {
	switch a.Kind {
	case query.KindTimechart:
		return "bucket ASC"
	case query.KindRare:
		return "m0 ASC"
	default:
		return "m0 DESC"
	}
}

// Aggregate runs a piped aggregation and returns a table of results.
func (d *DB) Aggregate(ctx context.Context, q query.Query) (store.AggResult, error) {
	if q.Agg == nil {
		return store.AggResult{}, fmt.Errorf("aggregate: query has no aggregation stage")
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	q.Normalize()
	start := time.Now()

	sqlStr, args := aggregateSQL(q)
	rows, err := d.ro.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return store.AggResult{}, fmt.Errorf("aggregate query: %w", err)
	}
	defer rows.Close()

	cols := q.Agg.Columns()
	groupCols := len(q.Agg.GroupBy)
	if q.Agg.Kind == query.KindTimechart {
		groupCols++ // the time bucket is a label, not a measure
	}
	res := store.AggResult{Columns: cols, GroupColumns: groupCols}
	for rows.Next() {
		if len(res.Rows) >= q.Agg.Limit {
			// The extra row we asked for exists, so there were more groups than
			// we are willing to return.
			res.Truncated = true
			break
		}
		row, serr := scanAggRow(rows, len(cols), q.Agg)
		if serr != nil {
			return store.AggResult{}, serr
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return store.AggResult{}, err
	}
	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
}

// scanAggRow reads one grouped row, converting the time bucket back into a
// timestamp and leaving everything else as its natural JSON type.
func scanAggRow(rows *sql.Rows, n int, a *query.Aggregation) ([]any, error) {
	raw := make([]any, n)
	ptrs := make([]any, n)
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("scan aggregate row: %w", err)
	}

	out := make([]any, n)
	for i, v := range raw {
		switch {
		case i == 0 && a.Kind == query.KindTimechart:
			if ns, ok := v.(int64); ok {
				out[i] = time.Unix(0, ns).UTC()
				continue
			}
			out[i] = v
		case v == nil:
			// A missing attribute groups as an empty label rather than a null,
			// which renders as a blank table cell instead of "<nil>".
			out[i] = ""
		default:
			if b, ok := v.([]byte); ok {
				out[i] = string(b)
				continue
			}
			out[i] = v
		}
	}
	return out, nil
}
