// Package sqlite implements store.Store on top of SQLite with an FTS5
// full-text index. It uses the pure-Go modernc.org/sqlite driver so the binary
// builds and runs without CGO.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"

	_ "modernc.org/sqlite"
)

// DB is a SQLite-backed store.Store.
type DB struct {
	db *sql.DB
}

// Open opens (creating if needed) a SQLite database at path and runs the
// versioned migrations up to the latest known schema version. Use ":memory:"
// for an ephemeral store (tests).
func Open(path string) (*DB, error) {
	dsn := path
	if path != ":memory:" {
		// Per-connection pragmas for concurrency and durability tradeoffs.
		dsn = fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)", path)
	}

	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection avoids "database is locked" churn and keeps an
	// in-memory database alive for the process lifetime. This is plenty for a
	// single-node logging server; reads still proceed concurrently via WAL.
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	sqldb.SetConnMaxLifetime(0)
	sqldb.SetConnMaxIdleTime(0)

	if err := migrate(context.Background(), sqldb); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db: sqldb}, nil
}

// Close closes the underlying database.
func (d *DB) Close() error { return d.db.Close() }

// Ping verifies the database connection is alive. It powers the readiness probe.
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Append writes a batch of events in a single transaction, updating both the
// structured table and the full-text index.
func (d *DB) Append(ctx context.Context, events []model.LogEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// UPSERT (rather than INSERT OR REPLACE) so a re-appended event updates the
	// existing row IN PLACE, preserving its rowid. We key the FTS row by that
	// same rowid and delete-then-insert it, so re-applying an event (as crash
	// recovery does) is fully idempotent — no duplicate FTS rows. RETURNING gives
	// us the stable rowid for both the insert and the update case.
	insLog, err := tx.PrepareContext(ctx,
		`INSERT INTO logs (id, ts, received_at, source, service, level, message, attributes, raw)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   ts=excluded.ts, received_at=excluded.received_at, source=excluded.source,
		   service=excluded.service, level=excluded.level, message=excluded.message,
		   attributes=excluded.attributes, raw=excluded.raw
		 RETURNING rowid`)
	if err != nil {
		return err
	}
	defer insLog.Close()

	delFTS, err := tx.PrepareContext(ctx, `DELETE FROM logs_fts WHERE rowid = ?`)
	if err != nil {
		return err
	}
	defer delFTS.Close()

	insFTS, err := tx.PrepareContext(ctx, `INSERT INTO logs_fts (rowid, id, text) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insFTS.Close()

	for _, e := range events {
		attrsJSON := "{}"
		if len(e.Attributes) > 0 {
			b, err := json.Marshal(e.Attributes)
			if err != nil {
				return fmt.Errorf("marshal attributes for %s: %w", e.ID, err)
			}
			attrsJSON = string(b)
		}
		var rowid int64
		if err := insLog.QueryRowContext(ctx,
			e.ID, e.Timestamp.UnixNano(), e.ReceivedAt.UnixNano(),
			e.Source, e.Service, string(e.Level), e.Message, attrsJSON, e.Raw,
		).Scan(&rowid); err != nil {
			return fmt.Errorf("upsert log %s: %w", e.ID, err)
		}
		// Clear any prior FTS row for this event (no-op for a brand-new rowid).
		if _, err := delFTS.ExecContext(ctx, rowid); err != nil {
			return fmt.Errorf("clear fts %s: %w", e.ID, err)
		}
		if _, err := insFTS.ExecContext(ctx, rowid, e.ID, ftsText(e)); err != nil {
			return fmt.Errorf("insert fts %s: %w", e.ID, err)
		}
	}
	return tx.Commit()
}

// ftsText builds the searchable text blob for an event: message, raw, service,
// source, and every attribute key and value.
func ftsText(e model.LogEvent) string {
	var b strings.Builder
	b.WriteString(e.Message)
	b.WriteByte(' ')
	b.WriteString(e.Raw)
	b.WriteByte(' ')
	b.WriteString(e.Service)
	b.WriteByte(' ')
	b.WriteString(e.Source)
	for k, v := range e.Attributes {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte(' ')
		fmt.Fprintf(&b, "%v", v)
	}
	return b.String()
}

// Purge deletes events with an event time strictly older than olderThan.
func (d *DB) Purge(ctx context.Context, olderThan time.Time) (int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	cutoff := olderThan.UnixNano()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM logs_fts WHERE id IN (SELECT id FROM logs WHERE ts < ?)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM logs WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
