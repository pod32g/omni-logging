package settings

import (
	"context"
	"reflect"
	"sync"
	"testing"
)

type fakeStore struct {
	mu sync.Mutex
	kv map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{kv: map[string]string{}} }

func (f *fakeStore) GetSettings(context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.kv {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) PutSettings(_ context.Context, kv map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range kv {
		f.kv[k] = v
	}
	return nil
}

func base() Mutable {
	return Mutable{RetentionDays: 14, LogLevel: "info", IngestKeys: []string{"k1"}}
}

func TestManager_LoadOverlay(t *testing.T) {
	st := newFakeStore()
	st.kv["mutable"] = `{"retention_days":7}` // partial override
	m := NewManager(base(), st)
	if err := m.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cur := m.Current()
	if cur.RetentionDays != 7 {
		t.Fatalf("retention = %d, want 7 (overlaid)", cur.RetentionDays)
	}
	if cur.LogLevel != "info" {
		t.Fatalf("log level = %q, want info (from base)", cur.LogLevel)
	}
}

func TestManager_ApplyPersistsAndFiresHooks(t *testing.T) {
	st := newFakeStore()
	m := NewManager(base(), st)
	var got Mutable
	var fired int
	m.OnChange(func(n Mutable) { got = n; fired++ })

	next := base()
	next.RetentionDays = 30
	next.RateLimitPerSec = 100
	if err := m.Apply(context.Background(), next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if m.Current().RetentionDays != 30 {
		t.Fatalf("current retention = %d, want 30", m.Current().RetentionDays)
	}
	if fired != 1 || got.RetentionDays != 30 {
		t.Fatalf("hook fired=%d got=%+v", fired, got)
	}
	if _, ok := st.kv["mutable"]; !ok {
		t.Fatal("Apply did not persist")
	}
	// A fresh manager loads the persisted value.
	m2 := NewManager(base(), st)
	m2.Load(context.Background())
	if m2.Current().RetentionDays != 30 {
		t.Fatalf("reloaded retention = %d, want 30", m2.Current().RetentionDays)
	}
}

func TestManager_ApplyValidation(t *testing.T) {
	st := newFakeStore()
	m := NewManager(base(), st)
	bad := base()
	bad.RetentionDays = -1
	if err := m.Apply(context.Background(), bad); err == nil {
		t.Fatal("expected validation error for negative retention")
	}
	if m.Current().RetentionDays != 14 {
		t.Fatalf("current changed despite invalid Apply: %d", m.Current().RetentionDays)
	}

	badLvl := base()
	badLvl.LogLevel = "loud"
	if err := m.Apply(context.Background(), badLvl); err == nil {
		t.Fatal("expected validation error for bad log level")
	}
}

func TestManager_IngestKeysNormalized(t *testing.T) {
	st := newFakeStore()
	m := NewManager(base(), st)
	next := base()
	next.IngestKeys = []string{" a ", "", "a", "b", "  "}
	if err := m.Apply(context.Background(), next); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := m.IngestKeys(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("normalized keys = %v, want [a b]", got)
	}
}

func TestManager_ConcurrentReadsAndApply(t *testing.T) {
	st := newFakeStore()
	m := NewManager(base(), st)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = m.Current()
				_ = m.IngestKeys()
				_ = m.RetentionDays()
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n := base()
			n.RetentionDays = i + 1
			_ = m.Apply(context.Background(), n)
		}(i)
	}
	wg.Wait()
}
