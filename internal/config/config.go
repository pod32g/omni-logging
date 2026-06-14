// Package config loads server configuration from layered sources: built-in
// defaults, an optional YAML file, and environment variables (in increasing
// precedence). Command-line flags, applied by the caller, take final priority.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all server settings.
type Config struct {
	Addr            string   `yaml:"addr"`
	DBPath          string   `yaml:"db_path"`
	RetentionDays   int      `yaml:"retention_days"`
	AdminToken      string   `yaml:"admin_token"`
	IngestKeys      []string `yaml:"ingest_keys"`
	BufferSize      int      `yaml:"buffer_size"`
	BatchSize       int      `yaml:"batch_size"`
	FlushIntervalMS int      `yaml:"flush_interval_ms"`
	TLSCert         string   `yaml:"tls_cert"`
	TLSKey          string   `yaml:"tls_key"`
}

// Default returns the baseline configuration.
func Default() Config {
	return Config{
		Addr:            ":8080",
		DBPath:          "omni.db",
		RetentionDays:   14,
		BufferSize:      10000,
		BatchSize:       500,
		FlushIntervalMS: 500,
	}
}

// Load builds a Config from defaults, then an optional YAML file, then
// environment variables. An empty path skips the file layer.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	cfg.applyEnv()
	return cfg, nil
}

// applyEnv overlays OMNILOG_* environment variables onto the config.
func (c *Config) applyEnv() {
	if v := os.Getenv("OMNILOG_ADDR"); v != "" {
		c.Addr = v
	}
	if v := os.Getenv("OMNILOG_DB"); v != "" {
		c.DBPath = v
	}
	if v := os.Getenv("OMNILOG_ADMIN_TOKEN"); v != "" {
		c.AdminToken = v
	}
	if v := os.Getenv("OMNILOG_INGEST_KEYS"); v != "" {
		c.IngestKeys = splitCSV(v)
	}
	if v := os.Getenv("OMNILOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.RetentionDays = n
		}
	}
	if v := os.Getenv("OMNILOG_TLS_CERT"); v != "" {
		c.TLSCert = v
	}
	if v := os.Getenv("OMNILOG_TLS_KEY"); v != "" {
		c.TLSKey = v
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TLSEnabled reports whether both a cert and key are configured.
func (c Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }
