# M6 — Versioned DB migrations + schema-version guard

*Roadmap M6 · Effort M · Depends on v1. Required before any later schema change
(M3 WAL/dedup tables, M20 schema tuning, M21 index dimension).*

## Goal

Replace the implicit `CREATE TABLE IF NOT EXISTS` bootstrap with an explicit,
ordered, versioned migration runner keyed on `PRAGMA user_version`, plus a
`schema_migrations` audit table. The server **refuses to start** against a
database whose version is newer than the binary understands (prevents an old
binary from corrupting a DB written by a newer one).

## Decisions

- **`PRAGMA user_version` is the authoritative version integer.** It lives in the
  SQLite file header, is atomic, and set transactionally alongside the DDL of the
  migration that bumps it — so a failed migration rolls back both the schema change
  and the version bump.
- **`schema_migrations(version, name, applied_at)`** records each applied migration
  for observability (the brief's "schema_version table"), created by the runner.
- **Migration #1 == the current v1 schema, verbatim and idempotent**
  (`CREATE ... IF NOT EXISTS`). Existing production DBs sit at `user_version=0` with
  the tables already present; applying migration #1 is a safe no-op that establishes
  the baseline and advances them to version 1 without touching data. This is the
  critical no-data-loss property.
- **One transaction per migration** (standard): migrations N apply in order, each
  atomic; a mid-list failure leaves the DB at the last fully-applied version.

## Components (`internal/store/sqlite/migrate.go`)

```go
type migration struct { version int; name string; stmts []string }
var migrations = []migration{ {1, "initial schema", []string{ /* logs, indexes, fts */ }} }

func latestSchemaVersion() int           // = last migration's version
func migrate(ctx, db *sql.DB) error      // called by Open instead of Exec(schema)
func userVersion(ctx, db) (int, error)
func setUserVersion(ctx, tx, n) error    // PRAGMA user_version = n (int formatted; trusted)
func (d *DB) SchemaVersion(ctx) (int, error)   // exported, for diagnostics
```

`migrate`:
1. ensure `schema_migrations` exists.
2. read current `user_version`; if `current > latest` → error "database schema
   version %d is newer than this binary supports (%d); upgrade omnilog".
3. for each migration with `version > current`, in order: BEGIN; exec each stmt;
   insert into `schema_migrations`; `PRAGMA user_version = version`; COMMIT.

`Open` calls `migrate(ctx, sqldb)` in place of the old `schema` Exec. The `schema`
const moves into migration #1.

## Testing (TDD)

- Fresh `:memory:`/file DB → `SchemaVersion == latest`; tables usable (Append/Search).
- Idempotent reopen → no error, version stable, `schema_migrations` row count stable,
  data preserved.
- **Legacy upgrade**: hand-build a DB with the old DDL + a data row + `user_version=0`,
  close, `Open` → migrates to latest, row preserved, version == latest.
- **Newer-DB guard**: set `user_version = latest+1`, `Open` → error mentioning
  "newer"/"upgrade"; DB untouched.

## Definition of done

`gofmt`/`vet`/`go test ./...` green; existing sqlite tests still pass unchanged
(they exercise the migrated schema); verified that an existing-file DB upgrades
cleanly. No data-loss risk; README/architecture note updated.
