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
		if isAttr && isBoolLiteral(f.Value) {
			cond, args := attrBoolCond(path, f.Value)
			return cond, args
		}
		e, a := textExpr()
		return e + " = ?", append(a, norm(f.Value))
	case query.OpNeq:
		if isAttr {
			if isBoolLiteral(f.Value) {
				cond, args := attrBoolCond(path, f.Value)
				// A missing attribute satisfies != , as it does for text.
				return "(json_extract(logs.attributes, ?) IS NULL OR NOT " + cond + ")",
					append([]any{path}, args...)
			}
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

// isBoolLiteral reports whether a filter value is written as a boolean.
func isBoolLiteral(v string) bool {
	return strings.EqualFold(v, "true") || strings.EqualFold(v, "false")
}

// attrBoolCond compares a JSON boolean attribute.
//
// SQLite has no boolean type: json_extract turns JSON true into the integer 1,
// so the obvious "CAST(... AS TEXT) = 'true'" never matches and attr.flag=true
// silently returns nothing. json_type reports the real JSON type, so match on
// that — and also accept the literal string "true", since a producer may send
// the value quoted. The in-memory matcher applies the same rule, so live tail
// and search cannot disagree about a boolean.
func attrBoolCond(path, value string) (string, []any) {
	want := "false"
	if strings.EqualFold(value, "true") {
		want = "true"
	}
	return "(json_type(logs.attributes, ?) = ? OR lower(CAST(json_extract(logs.attributes, ?) AS TEXT)) = ?)",
		[]any{path, want, path, want}
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
// A trailing '*' makes the final token of each phrase a prefix match, so typing
// "conn" finds "connection". Whole-token-only matching reads as broken to
// anyone typing into a search box — they get nothing until the word is complete
// — and the in-memory matcher mirrors this (see query.containsTokens).
func ftsMatchExpr(terms []string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"*`
	}
	return strings.Join(parts, " AND ")
}

// fromClause returns the table expression, joining FTS only when needed. The
// join is on the integer rowid (logs_fts.rowid == logs.rowid, guaranteed by
// Append and the v2 migration) rather than the TEXT id — an integer rowid lookup
// is markedly faster than the TEXT primary-key lookup the v1 join used.
func fromClause(needFTS bool) string {
	if needFTS {
		return "FROM logs JOIN logs_fts ON logs_fts.rowid = logs.rowid"
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

// MaxExactCount bounds the total-match count a search reports. Counting an
// unbounded match set means visiting every matching row, which on a large
// database costs far more than the page of results the caller actually wants.
// Past this many matches the count stops early and SearchResult.TotalCapped is
// set, so a UI can render "50,000+" instead of paying for an exact number.
const MaxExactCount = 50000

// countSQL builds the total-count statement (ignoring the limit/cursor) and
// args. The inner LIMIT lets SQLite stop once the cap is exceeded; we ask for
// one row past the cap so the caller can tell "exactly the cap" from "more".
func countSQL(q query.Query) (string, []any) {
	w := buildWhere(q)
	return fmt.Sprintf("SELECT COUNT(*) FROM (SELECT 1 %s %s LIMIT ?)",
			fromClause(w.needFTS), w.sqlStr()),
		append(w.args, MaxExactCount+1)
}

// readTimeout bounds the worst case of an interactive read so a single broad
// query (a huge COUNT or a wide FTS scan) cannot stall the server indefinitely.
// Exports (Stream) are deliberately exempt — they are expected to run long.
const readTimeout = 30 * time.Second

// Search executes a query and returns matching events plus the total count.
func (d *DB) Search(ctx context.Context, q query.Query) (store.SearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	q.Normalize()
	start := time.Now()

	sqlStr, args := searchSQL(q)
	rows, err := d.ro.QueryContext(ctx, sqlStr, args...)
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

	total, capped, err := d.count(ctx, q)
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
		Events:      events,
		Count:       len(events),
		Total:       total,
		TotalCapped: capped,
		TookMs:      time.Since(start).Milliseconds(),
		NextCursor:  next,
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

// Stream invokes fn for every matching event without buffering them all. It
// runs on the read pool, so however long the consumer takes, the write
// connection stays free for ingestion.
func (d *DB) Stream(ctx context.Context, q query.Query, fn func(model.LogEvent) error) error {
	q.Normalize()
	sqlStr, args := streamSQL(q)
	rows, err := d.ro.QueryContext(ctx, sqlStr, args...)
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

// count returns the number of matches, stopping at MaxExactCount. capped
// reports that the real total is higher than the returned value.
func (d *DB) count(ctx context.Context, q query.Query) (n int64, capped bool, err error) {
	sqlStr, args := countSQL(q)
	if err := d.ro.QueryRowContext(ctx, sqlStr, args...).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("count query: %w", err)
	}
	if n > MaxExactCount {
		return MaxExactCount, true, nil
	}
	return n, false, nil
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
//
// Unlike Search, this is NOT bounded by MaxExactCount and cannot be: every
// matching row has to be visited to be placed in a time bucket, and the facet
// counts are exact by definition. readTimeout is the only ceiling on a broad
// query here, so on a large database Stats is the expensive half of a UI search
// (the web UI issues it in parallel with every /search call). StatsResult.Total
// is therefore always exact, never capped.
func (d *DB) Stats(ctx context.Context, q query.Query) (store.StatsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
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
	hrows, err := d.ro.QueryContext(ctx, histSQL, histArgs...)
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
	rows, err := d.ro.QueryContext(ctx, sqlStr, args...)
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
