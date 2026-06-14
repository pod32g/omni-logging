package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

func TestBackupTo_AndReopen(t *testing.T) {
	src := newTestDB(t)
	seed(t, src)

	dst := filepath.Join(t.TempDir(), "snapshot.db")
	if err := src.BackupTo(context.Background(), dst); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}

	// The snapshot must be a usable, complete database.
	bak, err := Open(dst)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer bak.Close()

	got := ids(search(t, bak, "level=error"))
	if len(got) != 2 {
		t.Fatalf("backup search level=error = %v, want 2 rows", got)
	}
	ok, problems, err := bak.IntegrityCheck(context.Background())
	if err != nil || !ok {
		t.Fatalf("backup integrity = ok:%v problems:%v err:%v", ok, problems, err)
	}
}

func TestIntegrityCheck_OK(t *testing.T) {
	db := newTestDB(t)
	seed(t, db)
	ok, problems, err := db.IntegrityCheck(context.Background())
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok, got problems: %v", problems)
	}
}
