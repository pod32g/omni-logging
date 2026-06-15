package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

// whereClause is the set of conditions + args that select the events matching a
// query. It optionally requires a join against the FTS table.
type whereClause struct {
	conds   []string
	args    []any
	needFTS bool // true when free-text terms require the logs_fts join
}

// sqlStr renders the conditions as a SQL WHERE clause (empty when none).
func (w whereClause) sqlStr() string {
	if len(w.conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(w.conds, " AND ")
}

// buildWhere translates a query.Query into SQL conditions with bound parameters.
// User input is never interpolated into SQL — only into bound values and
// (parameterized) JSON paths. The keyset cursor is NOT included here so count and
// stats see the full match set; searchSQL adds it.
func buildWhere(q query.Query) whereClause {
	w := whereClause{}

	if len(q.Terms) > 0 {
		w.needFTS = true
		w.conds = append(w.conds, "logs_fts MATCH ?")
		w.args = append(w.args, ftsMatchExpr(q.Terms))
	}
	if !q.From.IsZero() {
		w.conds = append(w.conds, "logs.ts >= ?")
		w.args = append(w.args, q.From.UnixNano())
	}
	if !q.To.IsZero() {
		w.conds = append(w.conds, "logs.ts <= ?")
		w.args = append(w.args, q.To.UnixNano())
	}

	for _, f := range q.Filters {
		cond, fargs := filterCond(f)
		if cond == "" {
			continue
		}
		w.conds = append(w.conds, cond)
		w.args = append(w.args, fargs...)
	}
	return w
}

// columnFor maps a structured field to its SQL column, or "" for attributes.
func columnFor(field query.Field) string {
	switch field {
	case query.FieldLevel:
		return "logs.level"
	case query.FieldService:
		return "logs.service"
	case query.FieldSource:
		return "logs.source"
	case query.FieldMessage:
		return "logs.message"
	case query.FieldRaw:
		return "logs.raw"
	}
	return ""
}

// filterCond builds the SQL predicate + args for one filter.
func filterCond(f query.Filter) (string, []any) {
	isAttr := f.Field == query.FieldAttr
	path := ""
	col := columnFor(f.Field)
	if isAttr {
		path = jsonPath(f.Attr)
	}
	// norm lowercases level values to match the normalized storage.
	norm := func(v string) string {
		if f.Field == query.FieldLevel {
			return strings.ToLower(v)
		}
		return v
	}
	// textExpr / realExpr return the column expression plus the JSON-path args it
	// needs (one per json_extract occurrence) for attributes.
	textExpr := func() (string, []any) {
		if isAttr {
			return "CAST(json_extract(logs.attributes, ?) AS TEXT)", []any{path}
		}
		return col, nil
	}

	switch f.Op {
	case query.OpEq:
		e, a := textExpr()
		return e + " = ?", append(a, norm(f.Value))
	case query.OpNeq:
		if isAttr {
			return "(json_extract(logs.attributes, ?) IS NULL OR CAST(json_extract(logs.attributes, ?) AS TEXT) != ?)",
				[]any{path, path, f.Value}
		}
		return col + " != ?", []any{norm(f.Value)}
	case query.OpIn:
		e, a := textExpr()
		ph := strings.TrimSuffix(strings.Repeat("?,", len(f.Values)), ",")
		for _, v := range f.Values {
			a = append(a, norm(v))
		}
		return e + " IN (" + ph + ")", a
	case query.OpExists:
		if isAttr {
			return "json_extract(logs.attributes, ?) IS NOT NULL", []any{path}
		}
		return "(" + col + " IS NOT NULL AND " + col + " != '')", nil
	case query.OpLike:
		e, a := textExpr()
		return e + " LIKE ? ESCAPE '\\'", append(a, globToLike(f.Value))
	case query.OpRegex:
		e, a := textExpr()
		return e + " REGEXP ?", append(a, f.Value)
	case query.OpGt, query.OpGte, query.OpLt, query.OpLte:
		sym := compareSym(f.Op)
		if n, err := strconv.ParseFloat(f.Value, 64); err == nil {
			if isAttr {
				return "CAST(json_extract(logs.attributes, ?) AS REAL) " + sym + " ?", []any{path, n}
			}
			return "CAST(" + col + " AS REAL) " + sym + " ?", []any{n}
		}
		e, a := textExpr()
		return e + " " + sym + " ?", append(a, norm(f.Value))
	}
	return "", nil
}

func compareSym(op query.Op) string {
	switch op {
	case query.OpGt:
		return ">"
	case query.OpGte:
		return ">="
	case query.OpLt:
		return "<"
	case query.OpLte:
		return "<="
	}
	return "="
}

// globToLike converts a glob (only '*' is special) to a SQL LIKE pattern,
// escaping LIKE's own metacharacters with a backslash (ESCAPE '\').
func globToLike(glob string) string {
	var b strings.Builder
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteByte('%')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// keysetCond returns the pagination predicate (and args) for a cursor, in the
// query's sort order, or ("", nil) when there is no cursor.
func keysetCond(q query.Query) (string, []any) {
	if q.AfterID == "" {
		return "", nil
	}
	ts := q.AfterTS.UnixNano()
	if q.Order == query.OrderOldest {
		return "(logs.ts > ? OR (logs.ts = ? AND logs.id > ?))", []any{ts, ts, q.AfterID}
	}
	return "(logs.ts < ? OR (logs.ts = ? AND logs.id < ?))", []any{ts, ts, q.AfterID}
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

// searchSQL builds the event-selection statement and its bound args for an
// already-normalized query. It is separated from Search so the exact SQL can be
// inspected (e.g. EXPLAIN QUERY PLAN in tests) without duplicating construction.
func searchSQL(q query.Query) (string, []any) {
	w := buildWhere(q)
	if cond, cargs := keysetCond(q); cond != "" {
		w.conds = append(w.conds, cond)
		w.args = append(w.args, cargs...)
	}
	order := "DESC"
	if q.Order == query.OrderOldest {
		order = "ASC"
	}
	sqlStr := fmt.Sprintf(
		"SELECT logs.id, logs.ts, logs.received_at, logs.source, logs.service, logs.level, logs.message, logs.attributes, logs.raw %s %s ORDER BY logs.ts %s, logs.id %s LIMIT ?",
		fromClause(w.needFTS), w.sqlStr(), order, order)
	return sqlStr, append(w.args, q.Limit)
}

// countSQL builds the total-count statement (ignoring the limit/cursor) and args.
func countSQL(q query.Query) (string, []any) {
	w := buildWhere(q)
	return fmt.Sprintf("SELECT COUNT(*) %s %s", fromClause(w.needFTS), w.sqlStr()), w.args
}

// Search executes a query and returns matching events plus the total count.
func (d *DB) Search(ctx context.Context, q query.Query) (store.SearchResult, error) {
	q.Normalize()
	start := time.Now()

	sqlStr, args := searchSQL(q)
	rows, err := d.db.QueryContext(ctx, sqlStr, args...)
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

	total, err := d.count(ctx, q)
	if err != nil {
		return store.SearchResult{}, err
	}

	// A full page implies there may be more; hand back a cursor to continue.
	next := ""
	if len(events) == q.Limit && len(events) > 0 {
		last := events[len(events)-1]
		next = query.EncodeCursor(last.Timestamp, last.ID)
	}

	return store.SearchResult{
		Events:     events,
		Count:      len(events),
		Total:      total,
		TookMs:     time.Since(start).Milliseconds(),
		NextCursor: next,
	}, nil
}

// streamSQL is searchSQL without the LIMIT, for exports.
func streamSQL(q query.Query) (string, []any) {
	w := buildWhere(q)
	if cond, cargs := keysetCond(q); cond != "" {
		w.conds = append(w.conds, cond)
		w.args = append(w.args, cargs...)
	}
	order := "DESC"
	if q.Order == query.OrderOldest {
		order = "ASC"
	}
	return fmt.Sprintf(
		"SELECT logs.id, logs.ts, logs.received_at, logs.source, logs.service, logs.level, logs.message, logs.attributes, logs.raw %s %s ORDER BY logs.ts %s, logs.id %s",
		fromClause(w.needFTS), w.sqlStr(), order, order), w.args
}

// Stream invokes fn for every matching event without buffering them all.
func (d *DB) Stream(ctx context.Context, q query.Query, fn func(model.LogEvent) error) error {
	q.Normalize()
	sqlStr, args := streamSQL(q)
	rows, err := d.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return fmt.Errorf("stream query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (d *DB) count(ctx context.Context, q query.Query) (int64, error) {
	sqlStr, args := countSQL(q)
	var n int64
	if err := d.db.QueryRowContext(ctx, sqlStr, args...).Scan(&n); err != nil {
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

	interval := q.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	bucketNanos := interval.Nanoseconds()

	res := store.StatsResult{Facets: map[string][]store.Facet{}}

	// Histogram: integer-divide ts into buckets.
	histSQL, histArgs := histogramSQL(q, bucketNanos)
	hrows, err := d.db.QueryContext(ctx, histSQL, histArgs...)
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
		facets, err := d.facet(ctx, q, field)
		if err != nil {
			return store.StatsResult{}, err
		}
		res.Facets[field] = facets
	}

	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
}

// histogramSQL builds the time-bucketed count statement and its args.
func histogramSQL(q query.Query, bucketNanos int64) (string, []any) {
	w := buildWhere(q)
	return fmt.Sprintf(
		"SELECT (logs.ts / %d) * %d AS bucket, COUNT(*) %s %s GROUP BY bucket ORDER BY bucket ASC",
		bucketNanos, bucketNanos, fromClause(w.needFTS), w.sqlStr()), w.args
}

// facetSQL builds the top-values statement for a column and its args. col is a
// fixed internal column name (never user input).
func facetSQL(q query.Query, col string) (string, []any) {
	w := buildWhere(q)
	return fmt.Sprintf(
		"SELECT logs.%s AS v, COUNT(*) AS c %s %s GROUP BY v ORDER BY c DESC LIMIT 20",
		col, fromClause(w.needFTS), w.sqlStr()), w.args
}

// facet returns the top values and counts for a column under the given filter.
func (d *DB) facet(ctx context.Context, q query.Query, col string) ([]store.Facet, error) {
	sqlStr, args := facetSQL(q, col)
	rows, err := d.db.QueryContext(ctx, sqlStr, args...)
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
