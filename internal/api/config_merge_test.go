package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-logging/internal/config"
	settingspkg "github.com/pod32g/omni-logging/internal/settings"
)

// TestConfigPutKeepsOmittedFields covers the merge semantics of PUT
// /api/v1/config. Decoding the body into a zero-valued struct made a
// single-field update silently reset everything else — including clearing every
// ingest key, which turns off ingest authentication.
func TestConfigPutKeepsOmittedFields(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "sec"
	srv, db := newServer(t, cfg)

	mgr := settingspkg.NewManager(settingspkg.Mutable{
		RetentionDays:    14,
		RateLimitPerSec:  25,
		RateBurst:        50,
		DailyQuotaEvents: 1000,
		DailyQuotaBytes:  2000,
		LogLevel:         "info",
		IngestKeys:       []string{"key-a", "key-b"},
	}, db)
	srv.settings = mgr
	h := srv.Handler()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"retention_days":30}`))
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rr.Code, rr.Body.String())
	}

	got := mgr.Current()
	if got.RetentionDays != 30 {
		t.Errorf("retention_days = %d, want the updated 30", got.RetentionDays)
	}
	if len(got.IngestKeys) != 2 {
		t.Errorf("ingest keys = %v, want both preserved — omitting the field must not disable ingest auth", got.IngestKeys)
	}
	if got.RateLimitPerSec != 25 || got.RateBurst != 50 {
		t.Errorf("rate limit = %v/%v, want 25/50 preserved", got.RateLimitPerSec, got.RateBurst)
	}
	if got.DailyQuotaEvents != 1000 || got.DailyQuotaBytes != 2000 {
		t.Errorf("quotas = %d/%d, want 1000/2000 preserved", got.DailyQuotaEvents, got.DailyQuotaBytes)
	}
	if got.LogLevel != "info" {
		t.Errorf("log_level = %q, want info preserved", got.LogLevel)
	}

	// Clearing stays possible, but has to be explicit.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/v1/config", strings.NewReader(`{"ingest_keys":[]}`))
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("explicit clear = %d: %s", rr.Code, rr.Body.String())
	}
	if keys := mgr.Current().IngestKeys; len(keys) != 0 {
		t.Fatalf("explicit empty array did not clear the keys: %v", keys)
	}
	if mgr.Current().RetentionDays != 30 {
		t.Fatalf("retention lost during the explicit clear: %d", mgr.Current().RetentionDays)
	}
}

// TestExportConcurrencyIsBounded checks that exports past the cap are refused
// with a retryable status rather than piling onto the read pool.
func TestExportConcurrencyIsBounded(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	for i := 0; i < maxConcurrentExports; i++ {
		srv.exportSlots <- struct{}{}
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/export", nil))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("export beyond the cap = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("a 429 from the export cap should tell the client when to retry")
	}
}

// TestStatusEndpointRequiresAdmin: the operational counters moved off the
// unauthenticated liveness probe, so they must actually be guarded.
func TestStatusEndpointRequiresAdmin(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "sec"
	srv, _ := newServer(t, cfg)
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/v1/status = %d, want 401", rr.Code)
	}

	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer sec")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated /api/v1/status = %d", rr.Code)
	}
	var got statusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got.Status != "ok" || got.Ingest == nil {
		t.Fatalf("status payload missing ingest counters: %s", rr.Body.String())
	}
}
