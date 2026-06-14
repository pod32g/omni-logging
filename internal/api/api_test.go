package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/store"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/tail"
)

func newServer(t *testing.T, cfg config.Config) (*Server, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	hub := tail.NewHub()
	ing := ingest.New(db, hub, ingest.Options{FlushInterval: 5 * time.Millisecond})
	ing.Start()
	t.Cleanup(func() { ing.Stop() })

	srv := New(Deps{Config: cfg, Store: db, Ingestor: ing, Hub: hub})
	return srv, db
}

func seedEvent(t *testing.T, db store.Store, msg string, lvl model.Level) {
	t.Helper()
	e := model.LogEvent{Service: "api", Level: lvl, Message: msg}
	e.Normalize(time.Now())
	if err := db.Append(context.Background(), []model.LogEvent{e}); err != nil {
		t.Fatalf("Append: %v", err)
	}
}

func TestSearchEndpoint(t *testing.T) {
	srv, db := newServer(t, config.Default())
	seedEvent(t, db, "hello world", model.LevelInfo)
	seedEvent(t, db, "boom error", model.LevelError)

	h := srv.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=level=error", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var res store.SearchResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Total != 1 || len(res.Events) != 1 || res.Events[0].Message != "boom error" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestStatsEndpoint(t *testing.T) {
	srv, db := newServer(t, config.Default())
	seedEvent(t, db, "a", model.LevelError)
	seedEvent(t, db, "b", model.LevelInfo)

	h := srv.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/search/stats?interval=1m", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var res store.StatsResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Total != 2 {
		t.Fatalf("stats total = %d, want 2", res.Total)
	}
}

func TestAdminAuth(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "s3cret"
	srv, _ := newServer(t, cfg)
	h := srv.Handler()

	// No token -> 401.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/search", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rr.Code)
	}

	// Bearer token -> 200.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer token: status = %d, want 200", rr.Code)
	}

	// Query param token (for EventSource) -> 200.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/search?token=s3cret", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("query token: status = %d, want 200", rr.Code)
	}
}

func TestIngestAuth(t *testing.T) {
	cfg := config.Default()
	cfg.IngestKeys = []string{"devkey"}
	srv, _ := newServer(t, cfg)
	h := srv.Handler()

	body := `{"message":"x"}`

	// Wrong key -> 401.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "nope")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401", rr.Code)
	}

	// Correct key -> 200.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "devkey")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("correct key: status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
}

func TestHealthEndpoint(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/healthz", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Fatalf("health = %d %s", rr.Code, rr.Body.String())
	}
}
