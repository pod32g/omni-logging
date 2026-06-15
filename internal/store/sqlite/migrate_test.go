package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

func TestMigrate_FreshDBIsAtLatest(t *testing.T) {
	db := newTestDB(t)
	v, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v != latestSchemaVersion() {
		t.Fatalf("fresh DB version = %d, want %d", v, latestSchemaVersion())
	}
	// The schema is actually usable.
	seed(t, db)
	if got := ids(search(t, db, "level=error")); len(got) != 2 {
		t.Fatalf("seeded schema unusable: %v", got)
	}
}

func TestMigrate_IdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "omni.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	seed(t, db)
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	v, _ := db2.SchemaVersion(context.Background())
	if v != latestSchemaVersion() {
		t.Fatalf("reopened version = %d, want %d", v, latestSchemaVersion())
	}
	// Data survived the reopen, and migrations were not re-recorded.
	if got := ids(search(t, db2, "")); len(got) != 5 {
		t.Fatalf("after reopen, total = %v, want 5", got)
	}
	var n int
	if err := db2.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if n != latestSchemaVersion() {
		t.Fatalf("schema_migrations rows = %d, want %d (no duplicate applies)", n, latestSchemaVersion())
	}
}

// TestMigrate_LegacyV0Upgrade simulates a database created by the pre-M6 code:
// tables exist (created via CREATE TABLE IF NOT EXISTS) and user_version == 0.
// Opening it must upgrade in place to the latest version without data loss.
func TestMigrate_LegacyV0Upgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	const legacyDDL = `
CREATE TABLE IF NOT EXISTS logs (
	id TEXT PRIMARY KEY, ts INTEGER NOT NULL, received_at INTEGER NOT NULL,
	source TEXT, service TEXT, level TEXT, message TEXT, attributes TEXT, raw TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(id UNINDEXED, text);`
	if _, err := raw.Exec(legacyDDL); err != nil {
		t.Fatalf("legacy DDL: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO logs (id, ts, received_at, source, service, level, message, attributes, raw)
		VALUES ('legacy1', 1, 1, 'h', 'svc', 'error', 'old row', '{}', '')`); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 0"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open legacy: %v", err)
	}
	defer db.Close()

	if v, _ := db.SchemaVersion(context.Background()); v != latestSchemaVersion() {
		t.Fatalf("legacy upgraded to version %d, want %d", v, latestSchemaVersion())
	}
	got := search(t, db, "level=error")
	if len(got) != 1 || got[0].ID != "legacy1" {
		t.Fatalf("legacy data lost after migration: %v", ids(got))
	}
}

// TestMigrate_RebuildsFTSRowids simulates a v1 database whose FTS rows have
// rowids that do NOT match logs.rowid (as the pre-M3 INSERT produced), then
// verifies the v2 migration rebuilds the FTS index with aligned rowids so the
// fast rowid join returns correct results.
func TestMigrate_RebuildsFTSRowids(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1fts.db")

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	const v1DDL = `
CREATE TABLE IF NOT EXISTS logs (
	id TEXT PRIMARY KEY, ts INTEGER NOT NULL, received_at INTEGER NOT NULL,
	source TEXT, service TEXT, level TEXT, message TEXT, attributes TEXT, raw TEXT
);
CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(id UNINDEXED, text);`
	if _, err := raw.Exec(v1DDL); err != nil {
		t.Fatalf("v1 DDL: %v", err)
	}
	// A logs row gets rowid 1; give its FTS row a deliberately different rowid (100).
	if _, err := raw.Exec(`INSERT INTO logs (id, ts, received_at, source, service, level, message, attributes, raw)
		VALUES ('e1', 1, 1, 'h', 'svc', 'error', 'database connection refused', '{}', '')`); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO logs_fts (rowid, id, text) VALUES (100, 'e1', 'database connection refused svc h')`); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if v, _ := db.SchemaVersion(context.Background()); v != latestSchemaVersion() {
		t.Fatalf("version = %d, want %d", v, latestSchemaVersion())
	}
	// Free-text search (which uses the FTS join) must find the row.
	if got := ids(search(t, db, "refused")); len(got) != 1 || got[0] != "e1" {
		t.Fatalf("free-text after FTS rebuild = %v, want [e1]", got)
	}
	// The FTS rowid must now equal the logs rowid.
	var logRowid, ftsRowid int64
	db.db.QueryRow("SELECT rowid FROM logs WHERE id='e1'").Scan(&logRowid)
	db.db.QueryRow("SELECT rowid FROM logs_fts WHERE id='e1'").Scan(&ftsRowid)
	if logRowid != ftsRowid {
		t.Fatalf("after rebuild fts.rowid=%d, logs.rowid=%d (want equal)", ftsRowid, logRowid)
	}
}

func TestMigrate_RefusesNewerDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Pretend a newer binary wrote this DB.
	if _, err := db.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", latestSchemaVersion()+1)); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	db.Close()

	_, err = Open(path)
	if err == nil {
		t.Fatal("expected Open to refuse a newer-versioned DB")
	}
}

// ensure the model/query imports are used even if a future edit drops a case.
var _ = model.LevelError
var _ = query.OrderNewest
