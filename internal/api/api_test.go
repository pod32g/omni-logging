package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/admission"
	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/model"
	settingspkg "github.com/pod32g/omni-logging/internal/settings"
	"github.com/pod32g/omni-logging/internal/store"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/tail"
)

func TestAdmissionRateLimitThroughAPI(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	hub := tail.NewHub()
	// Burst of 1, effectively no refill within the test.
	lim := admission.New(admission.Limits{RatePerSec: 0.0001, Burst: 1}, time.Now)
	ing := ingest.New(db, hub, ingest.Options{FlushInterval: 5 * time.Millisecond, Limiter: lim})
	ing.Start()
	t.Cleanup(func() { ing.Stop() })

	cfg := config.Default()
	cfg.IngestKeys = []string{"devkey"}
	srv := New(Deps{Config: cfg, Store: db, Ingestor: ing, Hub: hub})
	h := srv.Handler()

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"message":"x"}`))
		req.Header.Set("X-Api-Key", "devkey")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := post(); rr.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	rr := post()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"reason":"rate"`) {
		t.Fatalf("expected rate-limit reason, got %s", rr.Body.String())
	}
	// The request ID is echoed on every response.
	if rr.Header().Get("X-Request-Id") == "" {
		t.Error("missing X-Request-Id response header")
	}
}

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

func TestSearchPaginationEndpoint(t *testing.T) {
	srv, db := newServer(t, config.Default())
	for i := 0; i < 5; i++ {
		seedEvent(t, db, "page event", model.LevelInfo)
	}
	h := srv.Handler()

	page := func(after string) store.SearchResult {
		u := "/api/v1/search?q=page&limit=2"
		if after != "" {
			u += "&after=" + after
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, u, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rr.Code, rr.Body.String())
		}
		var res store.SearchResult
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return res
	}

	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		res := page(cursor)
		for _, e := range res.Events {
			if seen[e.ID] {
				t.Fatalf("duplicate %s across pages", e.ID)
			}
			seen[e.ID] = true
		}
		pages++
		if res.NextCursor == "" || len(res.Events) == 0 || pages > 10 {
			break
		}
		cursor = res.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d unique events, want 5", len(seen))
	}
}

func TestExportEndpoint(t *testing.T) {
	srv, db := newServer(t, config.Default())
	for i := 0; i < 7; i++ {
		seedEvent(t, db, "export me", model.LevelError)
	}
	h := srv.Handler()

	// NDJSON: one JSON object per line, all 7 (beyond a small limit param).
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/export?q=export&format=ndjson&limit=2", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("ndjson status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("ndjson content-type = %q", ct)
	}
	lines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if len(lines) != 7 {
		t.Fatalf("ndjson lines = %d, want 7 (export ignores the search cap)", len(lines))
	}
	var first model.LogEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("ndjson line not valid JSON: %v", err)
	}

	// CSV: header + 7 rows.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/export?q=export&format=csv", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Content-Type") != "text/csv" {
		t.Fatalf("csv status/type = %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	csvLines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if len(csvLines) != 8 || !strings.HasPrefix(csvLines[0], "timestamp,level,service") {
		t.Fatalf("csv = %d lines, header=%q", len(csvLines), csvLines[0])
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

func TestMetricsEndpoint(t *testing.T) {
	srv, db := newServer(t, config.Default())
	seedEvent(t, db, "hello", model.LevelInfo)
	h := srv.Handler()

	// Drive a search so the store-query histogram and HTTP counters record.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/search?q=hello", nil))

	mrr := httptest.NewRecorder()
	h.ServeHTTP(mrr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if mrr.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", mrr.Code)
	}
	if ct := mrr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("/metrics content-type = %q, want text/plain", ct)
	}
	body := mrr.Body.String()
	for _, want := range []string{
		"omnilog_http_requests_total",
		"omnilog_store_query_duration_seconds",
		"omnilog_ingest_received_total",
		"omnilog_tail_subscribers",
		"omnilog_build_info",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics missing %q in:\n%s", want, body)
		}
	}
}

func renderMetrics(t *testing.T, srv *Server) string {
	t.Helper()
	var b strings.Builder
	if err := srv.metrics.WriteProm(&b); err != nil {
		t.Fatalf("WriteProm: %v", err)
	}
	return b.String()
}

func TestHTTPMetrics_MethodNormalized(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	h := srv.Handler()
	// An unknown but syntactically valid method must not create a per-method
	// time series (cardinality DoS): it is collapsed to method="other".
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("WEIRDVERB", "/api/v1/healthz", nil))

	out := renderMetrics(t, srv)
	if strings.Contains(out, `method="WEIRDVERB"`) {
		t.Fatalf("unknown method leaked as a label:\n%s", out)
	}
	if !strings.Contains(out, `method="other"`) {
		t.Fatalf("unknown method not normalized to other:\n%s", out)
	}
}

func TestMetricsMiddleware_RecordsPanicAs500(t *testing.T) {
	srv := New(Deps{})
	h := srv.metricsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	func() {
		defer func() { _ = recover() }() // swallow the re-panic
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	}()

	out := renderMetrics(t, srv)
	if !strings.Contains(out, `omnilog_http_requests_total{code="500",method="GET"} 1`) {
		t.Fatalf("panic not recorded as a 500 request:\n%s", out)
	}
}

func TestReadyz(t *testing.T) {
	srv, db := newServer(t, config.Default())
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz healthy status = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}

	db.Close() // simulate backend loss
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after store close status = %d, want 503", rr.Code)
	}
}

func TestConfigEndpointAndLiveIngestKeys(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	hub := tail.NewHub()
	ing := ingest.New(db, hub, ingest.Options{FlushInterval: 5 * time.Millisecond})
	ing.Start()
	t.Cleanup(func() { ing.Stop() })

	mgr := settingspkg.NewManager(settingspkg.Mutable{RetentionDays: 14}, db)
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	cfg := config.Default()
	cfg.AdminToken = "sec"
	srv := New(Deps{Config: cfg, Store: db, Ingestor: ing, Hub: hub, Settings: mgr})
	h := srv.Handler()

	// GET without admin token -> 401.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/config", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("config GET without token = %d, want 401", rr.Code)
	}

	// GET with token -> current settings.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("config GET = %d (%s)", rr.Code, rr.Body.String())
	}
	var got settingspkg.Mutable
	json.Unmarshal(rr.Body.Bytes(), &got)
	if got.RetentionDays != 14 {
		t.Fatalf("retention = %d, want 14", got.RetentionDays)
	}

	// Before edit, no ingest keys configured -> ingest is open (dev mode).
	if code := ingestStatus(t, h, ""); code != http.StatusOK {
		t.Fatalf("ingest with no keys = %d, want 200 (open)", code)
	}

	// PUT new settings (adds an ingest key + retention + log level).
	body := `{"retention_days":30,"log_level":"warn","ingest_keys":["abc"]}`
	rr = httptest.NewRecorder()
	preq := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(body))
	preq.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rr, preq)
	if rr.Code != http.StatusOK {
		t.Fatalf("config PUT = %d (%s)", rr.Code, rr.Body.String())
	}

	// The new ingest key is live immediately: right key 200, wrong/none 401.
	if code := ingestStatus(t, h, "abc"); code != http.StatusOK {
		t.Fatalf("ingest with new key = %d, want 200", code)
	}
	if code := ingestStatus(t, h, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("ingest with wrong key = %d, want 401", code)
	}

	// And it persisted (a fresh manager loads retention 30).
	m2 := settingspkg.NewManager(settingspkg.Mutable{RetentionDays: 14}, db)
	m2.Load(context.Background())
	if m2.Current().RetentionDays != 30 {
		t.Fatalf("persisted retention = %d, want 30", m2.Current().RetentionDays)
	}

	// Invalid PUT -> 400.
	rr = httptest.NewRecorder()
	bad := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"retention_days":-5}`))
	bad.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rr, bad)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid PUT = %d, want 400", rr.Code)
	}
}

func ingestStatus(t *testing.T, h http.Handler, key string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(`{"message":"x"}`))
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

func TestOpenAPIAndDocs(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("/openapi.json status = %d", rr.Code)
	}
	var spec map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &spec); err != nil {
		t.Fatalf("/openapi.json is not valid JSON: %v", err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("openapi version = %v, want 3.1.0", spec["openapi"])
	}
	if _, ok := spec["paths"].(map[string]any)["/api/v1/search"]; !ok {
		t.Fatal("spec missing /api/v1/search path")
	}

	drr := httptest.NewRecorder()
	h.ServeHTTP(drr, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if drr.Code != http.StatusOK || !strings.Contains(drr.Body.String(), "redoc") {
		t.Fatalf("/docs status=%d body lacks redoc", drr.Code)
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
