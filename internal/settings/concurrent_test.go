package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

// memStore is a minimal Store that records what was last persisted.
type memStore struct {
	mu sync.Mutex
	kv map[string]string
}

func newMemStore() *memStore { return &memStore{kv: map[string]string{}} }

func (m *memStore) GetSettings(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.kv {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) PutSettings(_ context.Context, kv map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range kv {
		m.kv[k] = v
	}
	return nil
}

// TestConcurrentApplyJSONDoesNotLoseUpdates covers the read-modify-write that
// merging introduced. Each writer touches a different field, so with a correct
// merge every change must survive; decoding over an unsynchronized snapshot let
// concurrent updates overwrite one another with stale values.
func TestConcurrentApplyJSONDoesNotLoseUpdates(t *testing.T) {
	store := newMemStore()
	m := NewManager(Mutable{
		RetentionDays:    1,
		RateLimitPerSec:  1,
		RateBurst:        1,
		DailyQuotaEvents: 1,
		DailyQuotaBytes:  1,
		LogLevel:         "info",
		IngestKeys:       []string{"k"},
	}, store)

	patches := []string{
		`{"retention_days":30}`,
		`{"rate_limit_per_sec":25}`,
		`{"rate_burst":50}`,
		`{"daily_quota_events":1000}`,
		`{"daily_quota_bytes":2000}`,
	}

	var wg sync.WaitGroup
	for _, p := range patches {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			if _, _, err := m.ApplyJSON(context.Background(), []byte(body)); err != nil {
				t.Errorf("ApplyJSON(%s): %v", body, err)
			}
		}(p)
	}
	wg.Wait()

	got := m.Current()
	for _, c := range []struct {
		name string
		got  any
		want any
	}{
		{"retention_days", got.RetentionDays, 30},
		{"rate_limit_per_sec", got.RateLimitPerSec, float64(25)},
		{"rate_burst", got.RateBurst, 50},
		{"daily_quota_events", got.DailyQuotaEvents, int64(1000)},
		{"daily_quota_bytes", got.DailyQuotaBytes, int64(2000)},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s = %v, want %v — a concurrent update was lost", c.name, c.got, c.want)
		}
	}
	if len(got.IngestKeys) != 1 {
		t.Errorf("ingest keys = %v, want the original preserved", got.IngestKeys)
	}
}

// TestApplyPersistsWhatIsInMemory covers store/memory divergence: whatever ends
// up current must be exactly what was written to the store, even when applies
// race.
func TestApplyPersistsWhatIsInMemory(t *testing.T) {
	store := newMemStore()
	m := NewManager(Mutable{RetentionDays: 1, LogLevel: "info"}, store)

	var wg sync.WaitGroup
	for i := 1; i <= 12; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"retention_days":%d}`, n)
			if _, _, err := m.ApplyJSON(context.Background(), []byte(body)); err != nil {
				t.Errorf("ApplyJSON: %v", err)
			}
		}(i)
	}
	wg.Wait()

	kv, err := store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var persisted Mutable
	if err := json.Unmarshal([]byte(kv[mutableKey]), &persisted); err != nil {
		t.Fatalf("persisted settings unreadable: %v", err)
	}
	if persisted.RetentionDays != m.Current().RetentionDays {
		t.Fatalf("store has retention_days=%d but memory has %d: the two diverged",
			persisted.RetentionDays, m.Current().RetentionDays)
	}
}

func TestApplyJSONRejectsMalformedInput(t *testing.T) {
	m := NewManager(Mutable{RetentionDays: 7, LogLevel: "info"}, newMemStore())
	if _, _, err := m.ApplyJSON(context.Background(), []byte(`{nope`)); err == nil {
		t.Fatal("expected malformed JSON to be rejected")
	}
	if m.Current().RetentionDays != 7 {
		t.Fatal("a rejected document must not change the current settings")
	}
	if _, _, err := m.ApplyJSON(context.Background(), []byte(`{"retention_days":-5}`)); err == nil {
		t.Fatal("expected validation to reject a negative retention")
	}
	if m.Current().RetentionDays != 7 {
		t.Fatal("a failed validation must not change the current settings")
	}
}
