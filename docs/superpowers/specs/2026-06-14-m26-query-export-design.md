# M26 — Richer query operators, pagination & export

*Roadmap M26 · Effort L · Depends on v1. Unblocks M27 (aggregations), dashboards,
alerting.*

Three pillars: (1) richer filter operators, (2) keyset pagination, (3) streaming
export decoupled from the UI cap.

## Scope decision

**In scope:** comparison (`>`, `>=`, `<`, `<=`), wildcard (glob `*`), exists
(`field=*`), `IN` (`field=(a,b,c)`), and regex (`field=~re`) operators — in the
parser, the SQL builder, and the in-memory tail matcher; keyset pagination;
NDJSON/CSV/JSON streaming export.

**Deferred (documented):** full parenthesized **OR-grouping across fields** — it
requires replacing the flat AND filter list with a boolean expression tree
(parser + matcher + SQL all rewritten), a large/risky change best done with the
M51 query-language spec. `IN` already covers the common same-field OR case
(`level=(error,warn)`). Filters remain AND-combined; this is called out in the
README and query docs.

## Operator grammar (token = `key OP value`, whitespace-separated)

| Syntax | Meaning |
|---|---|
| `level=error` | equals (case-insensitive for fields) |
| `level!=error` | not equals (a missing attr satisfies `!=`) |
| `attr.status>=500`, `>`, `<`, `<=` | comparison (numeric when the value parses as a number, else lexical) |
| `service=checkout*`, `=*foo*` | glob wildcard (`*` → SQL `LIKE %`, escaped) |
| `attr.user_id=*` | exists (field present / attr non-null) |
| `level=(error,warn,fatal)` | IN list |
| `message=~timeout\|refused` | regex (RE2 via a registered SQLite `REGEXP` function) |

`Filter` gains `Op Op` and `Values []string`, replacing the boolean `Negate`.
Quoted values with spaces still go through the tokenizer's quoted-token path.

## SQL (`internal/store/sqlite`)

`buildWhere` emits the right predicate per `Op`, always via bound parameters:
- fields → `col <op> ?`; attrs → `json_extract(attributes, path)` (CAST AS REAL for
  numeric comparison, AS TEXT otherwise).
- wildcard → `... LIKE ? ESCAPE '\'` (glob `*`→`%`, literal `%`/`_`/`\` escaped).
- exists → field `IS NOT NULL AND <> ''`; attr `json_extract(...) IS NOT NULL`.
- IN → `... IN (?,?,…)`.
- regex → `... REGEXP ?`; a deterministic `REGEXP` scalar function is registered on
  the driver (Go `regexp`, compiled-pattern cache).

The in-memory matcher (`internal/query/match.go`) mirrors every operator so live
tail filters identically (numeric compare, glob, RE2, IN, exists). Golden
EXPLAIN plans regenerated where shapes change.

## Keyset pagination

`Query` gains a cursor `AfterTS time.Time` + `AfterID string`. For newest-first,
the predicate is `(ts < AfterTS) OR (ts = AfterTS AND id < AfterID)` (mirrored for
oldest-first). `SearchResult` gains `NextCursor string` (opaque
`base64("<unixnano>|<id>")`, empty when no more). The `/api/v1/search` handler
accepts `?after=<cursor>` and returns `next_cursor`. Pagination is stable under
concurrent ingest (keyset, not OFFSET). `Total` is still the full match count.

## Export (`/api/v1/export`)

A new admin endpoint streams **all** matches (ignoring the UI `MaxLimit`) as
`?format=ndjson|csv|json`. Backed by a new `Store.Stream(ctx, q, fn func(LogEvent)
error) error` that iterates matches newest/oldest without buffering them all in
memory (one `rows.Next()` loop). The handler sets `Content-Disposition` and writes
incrementally with a flush. CSV columns: `timestamp,level,service,source,message,
attributes(JSON)`. `Stream` is added to the `Store` interface + conformance suite.

## UI (`internal/web/dist`)

Results view: a **Load more** button that fetches the next page via `next_cursor`
and appends rows; **Export** buttons (NDJSON/CSV) that open `/api/v1/export` with
the current query + token. Minimal CSS, reusing existing custom properties.

## Testing (TDD)

- `internal/query`: table-driven parse tests for every operator + error cases;
  matcher tests for each operator incl. numeric vs lexical compare, glob, regex,
  IN, exists, missing-attr semantics.
- `internal/store/sqlite`: search tests per operator against seeded data; keyset
  pagination (stable, no dupes/gaps across pages); `Stream` returns all matches in
  order; REGEXP function correctness; conformance suite gains a `Stream` case.
- `internal/api`: `/api/v1/search?after=` returns the next page + cursor;
  `/api/v1/export` returns NDJSON and CSV with the right rows and headers, beyond
  `MaxLimit`.

## Definition of done

`gofmt`/`vet`/`go test -race ./...` green; operators/pagination/export verified
against a running binary; UI rebuilt + embedded; README/query docs updated
(incl. the OR-grouping deferral). No new third-party dependency.
