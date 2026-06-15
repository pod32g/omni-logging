package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// migration is a single ordered, versioned schema change. stmts are executed in
// order within one transaction; applying it advances PRAGMA user_version to
// version. Migrations must be append-only: never edit or reorder a released
// migration — add a new one.
type migration struct {
	version int
	name    string
	stmts   []string
}

// migrations is the ordered list of schema changes. Migration 1 is the verbatim
// v1 schema, kept idempotent (IF NOT EXISTS) so it is a safe no-op on databases
// created by the pre-migration bootstrap (which sit at user_version 0 with the
// tables already present) — it establishes the baseline and advances them to 1
// without touching data.
var migrations = []migration{
	{
		version: 1,
		name:    "initial schema",
		stmts: []string{
			`CREATE TABLE IF NOT EXISTS logs (
				id          TEXT PRIMARY KEY,
				ts          INTEGER NOT NULL,
				received_at INTEGER NOT NULL,
				source      TEXT,
				service     TEXT,
				level       TEXT,
				message     TEXT,
				attributes  TEXT,
				raw         TEXT
			)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_ts      ON logs(ts)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_service ON logs(service)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_level   ON logs(level)`,
			`CREATE INDEX IF NOT EXISTS idx_logs_source  ON logs(source)`,
			`CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(id UNINDEXED, text)`,
		},
	},
}

// latestSchemaVersion is the highest version this binary knows how to produce.
func latestSchemaVersion() int {
	return migrations[len(migrations)-1].version
}

// migrate brings db up to latestSchemaVersion, applying each pending migration
// in its own transaction. It refuses to operate on a database whose version is
// newer than this binary understands.
func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	current, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	latest := latestSchemaVersion()
	if current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); upgrade omnilog", current, latest)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, s := range m.stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO schema_migrations (version, name, applied_at) VALUES (?, ?, strftime('%s','now'))`,
		m.version, m.name); err != nil {
		return err
	}
	// PRAGMA user_version cannot be parameterized; the value is a trusted int
	// from our own migration list. It is set inside the transaction so the
	// schema change and the version bump commit atomically.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

func userVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// SchemaVersion reports the database's current schema version.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	return userVersion(ctx, d.db)
}
