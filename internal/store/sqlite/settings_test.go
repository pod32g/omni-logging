package sqlite

import (
	"context"
	"testing"
)

func TestSettings_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if v, _ := db.SchemaVersion(ctx); v != latestSchemaVersion() {
		t.Fatalf("schema version = %d, want %d", v, latestSchemaVersion())
	}

	got, err := db.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings (empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh settings = %v, want empty", got)
	}

	if err := db.PutSettings(ctx, map[string]string{"mutable": `{"retention_days":7}`, "x": "y"}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	got, _ = db.GetSettings(ctx)
	if got["mutable"] != `{"retention_days":7}` || got["x"] != "y" {
		t.Fatalf("round-trip = %v", got)
	}

	// Upsert overwrites a key and leaves others intact.
	if err := db.PutSettings(ctx, map[string]string{"x": "z"}); err != nil {
		t.Fatalf("PutSettings upsert: %v", err)
	}
	got, _ = db.GetSettings(ctx)
	if got["x"] != "z" || got["mutable"] != `{"retention_days":7}` {
		t.Fatalf("after upsert = %v", got)
	}
}
