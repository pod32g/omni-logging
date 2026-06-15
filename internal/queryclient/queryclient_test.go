package queryclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/store"
)

func sampleResult() store.SearchResult {
	ts := time.Date(2026, 6, 14, 15, 4, 5, 0, time.UTC)
	return store.SearchResult{
		Events: []model.LogEvent{
			{ID: "a", Timestamp: ts, Service: "checkout", Level: model.LevelError, Message: "boom"},
			{ID: "b", Timestamp: ts, Service: "auth", Level: model.LevelInfo, Message: "ok"},
		},
		Count: 2, Total: 2,
	}
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/search" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(sampleResult())
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearch(t *testing.T) {
	srv := newTestServer(t)
	c := &Client{ServerURL: srv.URL, Token: "tok", HTTP: srv.Client()}
	res, err := c.Search(context.Background(), map[string]string{"q": "level=error"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 2 || len(res.Events) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestSearch_Unauthorized(t *testing.T) {
	srv := newTestServer(t)
	c := &Client{ServerURL: srv.URL, Token: "wrong", HTTP: srv.Client()}
	if _, err := c.Search(context.Background(), nil); err == nil {
		t.Fatal("expected error for bad token")
	}
}

func TestFormatNDJSON(t *testing.T) {
	var b strings.Builder
	if err := WriteNDJSON(&b, sampleResult().Events); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("ndjson lines = %d, want 2", len(lines))
	}
	var e model.LogEvent
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil || e.ID != "a" {
		t.Fatalf("first ndjson line bad: %v (%q)", err, lines[0])
	}
}

func TestFormatTable(t *testing.T) {
	var b strings.Builder
	if err := WriteTable(&b, sampleResult().Events); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"TIMESTAMP", "LEVEL", "SERVICE", "MESSAGE", "checkout", "boom", "error"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatJSON(t *testing.T) {
	var b strings.Builder
	res := sampleResult()
	if err := WriteJSON(&b, res); err != nil {
		t.Fatal(err)
	}
	var got store.SearchResult
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("json output not valid: %v", err)
	}
	if got.Total != 2 {
		t.Fatalf("json total = %d, want 2", got.Total)
	}
}
