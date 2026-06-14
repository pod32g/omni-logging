package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

// whereClause is the SQL fragment + args that select the events matching a
// query. It optionally requires a join against the FTS table.
type whereClause struct {
	sql     string // e.g. "WHERE logs.level = ? AND ..."
	args    []any
	needFTS bool // true when free-text terms require the logs_fts join
}

// buildWhere translates a query.Query into a SQL WHERE clause with bound
// parameters. User input is never interpolated into SQL — only into bound
// values and (parameterized) JSON paths.
func buildWhere(q query.Query) whereClause {
	var conds []string
	var args []any
	needFTS := false

	if len(q.Terms) > 0 {
		needFTS = true
		conds = append(conds, "logs_fts MATCH ?")
		args = append(args, ftsMatchExpr(q.Terms))
	}
	if !q.From.IsZero() {
		conds = append(conds, "logs.ts >= ?")
		args = append(args, q.From.UnixNano())
	}
	if !q.To.IsZero() {
		conds = append(conds, "logs.ts <= ?")
		args = append(args, q.To.UnixNano())
	}

	for _, f := range q.Filters {
		switch f.Field {
		case query.FieldLevel:
			conds = append(conds, eqCond("logs.level", f.Negate))
			args = append(args, strings.ToLower(f.Value))
		case query.FieldService:
			conds = append(conds, eqCond("logs.service", f.Negate))
			args = append(args, f.Value)
		case query.FieldSource:
			conds = append(conds, eqCond("logs.source", f.Negate))
			args = append(args, f.Value)
		case query.FieldAttr:
			path := jsonPath(f.Attr)
			if f.Negate {
				// Missing attribute should satisfy "!=".
				conds = append(conds,
					"(json_extract(logs.attributes, ?) IS NULL OR CAST(json_extract(logs.attributes, ?) AS TEXT) != ?)")
				args = append(args, path, path, f.Value)
			} else {
				conds = append(conds, "CAST(json_extract(logs.attributes, ?) AS TEXT) = ?")
				args = append(args, path, f.Value)
			}
		}
	}

	w := ""
	if len(conds) > 0 {
		w = "WHERE " + strings.Join(conds, " AND ")
	}
	return whereClause{sql: w, args: args, needFTS: needFTS}
}

func eqCond(col string, negate bool) string {
	if negate {
		return col + " != ?"
	}
	return col + " = ?"
}

// jsonPath builds a safe JSON path for an attribute key. The key is bound as a
// parameter value (not concatenated into SQL), so the only escaping needed is
// for the JSON path grammar's quote character.
func jsonPath(key string) string {
	return `$."` + strings.ReplaceAll(key, `"`, `""`) + `"`
}

// ftsMatchExpr builds an FTS5 MATCH expression that ANDs all terms together.
// Each term is wrapped as a quoted string so phrases and punctuation are safe.
func ftsMatchExpr(terms []string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(parts, " AND ")
}

// fromClause returns the table expression, joining FTS only when needed.
func fromClause(needFTS bool) string {
	if needFTS {
		return "FROM logs JOIN logs_fts ON logs_fts.id = logs.id"
	}
	return "FROM logs"
}

// Search executes a query and returns matching events plus the total count.
func (d *DB) Search(ctx context.Context, q query.Query) (store.SearchResult, error) {
	q.Normalize()
	start := time.Now()
	w := buildWhere(q)

	order := "DESC"
	if q.Order == query.OrderOldest {
		order = "ASC"
	}

	sqlStr := fmt.Sprintf(
		"SELECT logs.id, logs.ts, logs.received_at, logs.source, logs.service, logs.level, logs.message, logs.attributes, logs.raw %s %s ORDER BY logs.ts %s, logs.id %s LIMIT ?",
		fromClause(w.needFTS), w.sql, order, order)

	rows, err := d.db.QueryContext(ctx, sqlStr, append(w.args, q.Limit)...)
	if err != nil {
		return store.SearchResult{}, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var events []model.LogEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return store.SearchResult{}, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return store.SearchResult{}, err
	}

	total, err := d.count(ctx, w)
	if err != nil {
		return store.SearchResult{}, err
	}

	return store.SearchResult{
		Events: events,
		Count:  len(events),
		Total:  total,
		TookMs: time.Since(start).Milliseconds(),
	}, nil
}

func (d *DB) count(ctx context.Context, w whereClause) (int64, error) {
	sqlStr := fmt.Sprintf("SELECT COUNT(*) %s %s", fromClause(w.needFTS), w.sql)
	var n int64
	if err := d.db.QueryRowContext(ctx, sqlStr, w.args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count query: %w", err)
	}
	return n, nil
}

// scanEvent reads a single logs row into a LogEvent.
func scanEvent(rows *sql.Rows) (model.LogEvent, error) {
	var (
		e            model.LogEvent
		ts, received int64
		level        string
		attrsJSON    string
	)
	if err := rows.Scan(&e.ID, &ts, &received, &e.Source, &e.Service, &level, &e.Message, &attrsJSON, &e.Raw); err != nil {
		return model.LogEvent{}, fmt.Errorf("scan event: %w", err)
	}
	e.Timestamp = time.Unix(0, ts).UTC()
	e.ReceivedAt = time.Unix(0, received).UTC()
	e.Level = model.Level(level)
	if attrsJSON != "" && attrsJSON != "{}" {
		_ = json.Unmarshal([]byte(attrsJSON), &e.Attributes)
	}
	return e, nil
}

// Stats computes the histogram and level/service facets for a query.
func (d *DB) Stats(ctx context.Context, q query.Query) (store.StatsResult, error) {
	q.Normalize()
	start := time.Now()
	w := buildWhere(q)

	interval := q.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	bucketNanos := interval.Nanoseconds()

	res := store.StatsResult{Facets: map[string][]store.Facet{}}

	// Histogram: integer-divide ts into buckets.
	histSQL := fmt.Sprintf(
		"SELECT (logs.ts / %d) * %d AS bucket, COUNT(*) %s %s GROUP BY bucket ORDER BY bucket ASC",
		bucketNanos, bucketNanos, fromClause(w.needFTS), w.sql)
	hrows, err := d.db.QueryContext(ctx, histSQL, w.args...)
	if err != nil {
		return store.StatsResult{}, fmt.Errorf("histogram query: %w", err)
	}
	defer hrows.Close()
	for hrows.Next() {
		var bucket, c int64
		if err := hrows.Scan(&bucket, &c); err != nil {
			return store.StatsResult{}, err
		}
		res.Histogram = append(res.Histogram, store.Bucket{Start: time.Unix(0, bucket).UTC(), Count: c})
		res.Total += c
	}
	if err := hrows.Err(); err != nil {
		return store.StatsResult{}, err
	}

	// Facets for level and service.
	for _, field := range []string{"level", "service"} {
		facets, err := d.facet(ctx, w, field)
		if err != nil {
			return store.StatsResult{}, err
		}
		res.Facets[field] = facets
	}

	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
}

// facet returns the top values and counts for a column under the given filter.
func (d *DB) facet(ctx context.Context, w whereClause, col string) ([]store.Facet, error) {
	sqlStr := fmt.Sprintf(
		"SELECT logs.%s AS v, COUNT(*) AS c %s %s GROUP BY v ORDER BY c DESC LIMIT 20",
		col, fromClause(w.needFTS), w.sql)
	rows, err := d.db.QueryContext(ctx, sqlStr, w.args...)
	if err != nil {
		return nil, fmt.Errorf("facet %s: %w", col, err)
	}
	defer rows.Close()

	var facets []store.Facet
	for rows.Next() {
		var v sql.NullString
		var c int64
		if err := rows.Scan(&v, &c); err != nil {
			return nil, err
		}
		facets = append(facets, store.Facet{Value: v.String, Count: c})
	}
	return facets, rows.Err()
}
