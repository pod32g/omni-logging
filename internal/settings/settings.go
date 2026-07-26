// Package settings manages the runtime-mutable subset of server configuration:
// it overlays persisted overrides on the startup config, applies validated
// changes live (persisting them and firing hot-apply hooks), and exposes
// thread-safe getters for the components that read settings on the fly.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Mutable is the set of settings editable at runtime (via the admin API/UI).
// The admin token is deliberately NOT here — it is not editable from the UI.
type Mutable struct {
	RetentionDays    int      `json:"retention_days"`
	RateLimitPerSec  float64  `json:"rate_limit_per_sec"`
	RateBurst        int      `json:"rate_burst"`
	DailyQuotaEvents int64    `json:"daily_quota_events"`
	DailyQuotaBytes  int64    `json:"daily_quota_bytes"`
	LogLevel         string   `json:"log_level"`
	IngestKeys       []string `json:"ingest_keys"`
}

// Store persists settings as opaque key/value pairs.
type Store interface {
	GetSettings(ctx context.Context) (map[string]string, error)
	PutSettings(ctx context.Context, kv map[string]string) error
}

const mutableKey = "mutable"

// Manager holds the current mutable settings and applies changes live.
type Manager struct {
	// applyMu serializes whole apply operations (read → merge → validate →
	// persist → swap). mu alone is not enough: a merging update is a
	// read-modify-write, so without this two concurrent partial updates can
	// each read the same base and the second can drop the first's changes.
	// It also keeps the store and the in-memory copy from diverging, which
	// interleaved applies could otherwise cause.
	applyMu sync.Mutex

	mu    sync.RWMutex
	cur   Mutable
	store Store
	hooks []func(Mutable)
}

// NewManager creates a Manager seeded with base (the effective startup config).
func NewManager(base Mutable, store Store) *Manager {
	base.IngestKeys = normalizeKeys(base.IngestKeys)
	return &Manager{cur: base, store: store}
}

// Load overlays any persisted overrides onto the base config.
func (m *Manager) Load(ctx context.Context) error {
	kv, err := m.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	raw, ok := kv[mutableKey]
	if !ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Unmarshal over the current base so absent JSON fields keep their base value.
	if err := json.Unmarshal([]byte(raw), &m.cur); err != nil {
		return fmt.Errorf("settings: parse persisted overrides: %w", err)
	}
	m.cur.IngestKeys = normalizeKeys(m.cur.IngestKeys)
	return nil
}

// OnChange registers a hook invoked (with the new settings) after each Apply.
func (m *Manager) OnChange(fn func(Mutable)) {
	m.mu.Lock()
	m.hooks = append(m.hooks, fn)
	m.mu.Unlock()
}

// Current returns a copy of the current settings.
func (m *Manager) Current() Mutable {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur.clone()
}

// IngestKeys returns a copy of the current ingest keys.
func (m *Manager) IngestKeys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.cur.IngestKeys...)
}

// RetentionDays returns the current retention setting.
func (m *Manager) RetentionDays() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cur.RetentionDays
}

// ErrInvalidJSON marks a settings document that could not be parsed, so a
// caller can tell malformed input from input that failed validation.
var ErrInvalidJSON = errors.New("invalid settings JSON")

// Apply validates next, makes it current, persists it, and fires change hooks.
// next fully replaces the current settings.
func (m *Manager) Apply(ctx context.Context, next Mutable) error {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	_, _, err := m.applyLocked(ctx, next)
	return err
}

// ApplyJSON merges a settings document over the current values and applies the
// result, returning the settings before and after. Fields absent from the
// document keep their current value. The whole read-merge-persist sequence runs
// under applyMu, so concurrent partial updates cannot lose each other.
func (m *Manager) ApplyJSON(ctx context.Context, raw []byte) (prev, next Mutable, err error) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	next = m.Current()
	if uerr := json.Unmarshal(raw, &next); uerr != nil {
		return Mutable{}, Mutable{}, fmt.Errorf("%w: %s", ErrInvalidJSON, uerr)
	}
	return m.applyLocked(ctx, next)
}

// applyLocked persists and installs next. Caller holds applyMu.
func (m *Manager) applyLocked(ctx context.Context, next Mutable) (prev, applied Mutable, err error) {
	next.IngestKeys = normalizeKeys(next.IngestKeys)
	next.LogLevel = strings.ToLower(strings.TrimSpace(next.LogLevel))
	if verr := Validate(next); verr != nil {
		return Mutable{}, Mutable{}, verr
	}
	raw, merr := json.Marshal(next)
	if merr != nil {
		return Mutable{}, Mutable{}, merr
	}
	if perr := m.store.PutSettings(ctx, map[string]string{mutableKey: string(raw)}); perr != nil {
		return Mutable{}, Mutable{}, perr
	}

	m.mu.Lock()
	prev = m.cur.clone()
	m.cur = next
	hooks := make([]func(Mutable), len(m.hooks))
	copy(hooks, m.hooks)
	snapshot := next.clone()
	m.mu.Unlock()

	for _, fn := range hooks {
		fn(snapshot)
	}
	return prev, snapshot, nil
}

// Validate checks a settings value for sanity.
func Validate(m Mutable) error {
	if m.RetentionDays < 0 {
		return fmt.Errorf("retention_days must be >= 0")
	}
	if m.RateLimitPerSec < 0 {
		return fmt.Errorf("rate_limit_per_sec must be >= 0")
	}
	if m.RateBurst < 0 {
		return fmt.Errorf("rate_burst must be >= 0")
	}
	if m.DailyQuotaEvents < 0 || m.DailyQuotaBytes < 0 {
		return fmt.Errorf("daily quotas must be >= 0")
	}
	switch strings.ToLower(strings.TrimSpace(m.LogLevel)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error")
	}
	return nil
}

func (m Mutable) clone() Mutable {
	m.IngestKeys = append([]string(nil), m.IngestKeys...)
	return m
}

// normalizeKeys trims, drops empties, and de-duplicates ingest keys (stable order).
func normalizeKeys(keys []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}
