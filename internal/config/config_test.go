package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.DBPath != "omni.db" || cfg.RetentionDays != 14 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoad_YAMLThenEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "omni.yaml")
	yaml := "addr: \":9000\"\ndb_path: \"/data/logs.db\"\nretention_days: 30\ningest_keys: [k1, k2]\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("OMNILOG_ADDR", ":7000") // env overrides file
	t.Setenv("OMNILOG_INGEST_KEYS", "a,b,c")
	t.Setenv("OMNILOG_METRICS_PUBLIC", "true")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":7000" {
		t.Errorf("Addr = %q, want :7000 (env overrides file)", cfg.Addr)
	}
	if cfg.DBPath != "/data/logs.db" {
		t.Errorf("DBPath = %q, want from file", cfg.DBPath)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want 30", cfg.RetentionDays)
	}
	if len(cfg.IngestKeys) != 3 {
		t.Errorf("IngestKeys = %v, want 3 from env", cfg.IngestKeys)
	}
	if !cfg.MetricsPublic {
		t.Error("MetricsPublic = false, want true from env")
	}
}

func TestResolveWALDir(t *testing.T) {
	if got := (Config{DBPath: ":memory:"}).ResolveWALDir(); got != "" {
		t.Errorf("memory db WAL dir = %q, want empty (disabled)", got)
	}
	if got := (Config{DBPath: "/data/omni.db"}).ResolveWALDir(); got != "/data/wal" {
		t.Errorf("file db WAL dir = %q, want /data/wal", got)
	}
	if got := (Config{DBPath: "/data/omni.db", WALDir: "/custom/wal"}).ResolveWALDir(); got != "/custom/wal" {
		t.Errorf("explicit WAL dir = %q, want /custom/wal", got)
	}
}

func TestTLSEnabled(t *testing.T) {
	if (Config{}).TLSEnabled() {
		t.Error("empty config should not have TLS enabled")
	}
	if !(Config{TLSCert: "c", TLSKey: "k"}).TLSEnabled() {
		t.Error("cert+key should enable TLS")
	}
}
