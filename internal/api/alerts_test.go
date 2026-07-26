package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/model"
)

func doJSON(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	out := map[string]any{}
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
	}
	return rr, out
}

// TestAlertRuleLifecycle walks create → list → update → delete through the API.
func TestAlertRuleLifecycle(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	h := srv.Handler()

	rr, created := doJSON(t, h, http.MethodPost, "/api/v1/alerts", `{
		"name":"too many errors","query":"level=error",
		"window_seconds":300,"interval_seconds":60,
		"condition":{"op":"gt","value":10},"enabled":true}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rr.Code, rr.Body.String())
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("create did not return an ID")
	}
	if created["state"] != "unknown" {
		t.Errorf("a new rule should start unknown, got %v", created["state"])
	}

	rr, listed := doJSON(t, h, http.MethodGet, "/api/v1/alerts", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d", rr.Code)
	}
	rules, _ := listed["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("list returned %d rules, want 1", len(rules))
	}

	rr, updated := doJSON(t, h, http.MethodPut, "/api/v1/alerts/"+id, `{
		"name":"renamed","query":"level=error",
		"window_seconds":600,"interval_seconds":60,
		"condition":{"op":"gte","value":5},"enabled":false}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rr.Code, rr.Body.String())
	}
	if updated["name"] != "renamed" || updated["window_seconds"].(float64) != 600 {
		t.Errorf("update did not apply: %+v", updated)
	}

	rr, _ = doJSON(t, h, http.MethodDelete, "/api/v1/alerts/"+id, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rr.Code)
	}
	rr, _ = doJSON(t, h, http.MethodDelete, "/api/v1/alerts/"+id, "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("deleting a missing rule = %d, want 404", rr.Code)
	}
}

// TestAlertRuleValidation: a rule whose query does not parse must be rejected at
// write time, not fail silently on every evaluation afterwards.
func TestAlertRuleValidation(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	h := srv.Handler()

	for _, tc := range []struct{ name, body string }{
		{"unparseable query", `{"name":"x","query":"| stats bogus(f)","window_seconds":300,"interval_seconds":60,"condition":{"op":"gt","value":1}}`},
		{"missing name", `{"name":"","query":"level=error","window_seconds":300,"interval_seconds":60,"condition":{"op":"gt","value":1}}`},
		{"bad op", `{"name":"x","query":"level=error","window_seconds":300,"interval_seconds":60,"condition":{"op":"sideways","value":1}}`},
		{"window below interval", `{"name":"x","query":"level=error","window_seconds":30,"interval_seconds":60,"condition":{"op":"gt","value":1}}`},
		{"interval too small", `{"name":"x","query":"level=error","window_seconds":300,"interval_seconds":1,"condition":{"op":"gt","value":1}}`},
	} {
		rr, _ := doJSON(t, h, http.MethodPost, "/api/v1/alerts", tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", tc.name, rr.Code, rr.Body.String())
		}
	}
}

// TestAlertTestEndpointEvaluatesWithoutSideEffects: the whole point of a dry run
// is that it tells you what would happen without doing it.
func TestAlertTestEndpointEvaluatesWithoutSideEffects(t *testing.T) {
	srv, db := newServer(t, config.Default())
	h := srv.Handler()
	for i := 0; i < 3; i++ {
		seedEvent(t, db, "kaboom", model.LevelError)
	}

	_, created := doJSON(t, h, http.MethodPost, "/api/v1/alerts", `{
		"name":"errors","query":"level=error",
		"window_seconds":3600,"interval_seconds":60,
		"condition":{"op":"gt","value":2},"enabled":true}`)
	id := created["id"].(string)

	rr, ev := doJSON(t, h, http.MethodPost, "/api/v1/alerts/"+id+"/test", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("test = %d: %s", rr.Code, rr.Body.String())
	}
	if ev["firing"] != true {
		t.Errorf("3 errors against '> 2' should fire: %+v", ev)
	}
	if ev["value"].(float64) != 3 {
		t.Errorf("value = %v, want 3", ev["value"])
	}

	// The dry run must not have advanced the stored state.
	_, listed := doJSON(t, h, http.MethodGet, "/api/v1/alerts", "")
	rule := listed["rules"].([]any)[0].(map[string]any)
	if rule["state"] != "unknown" {
		t.Errorf("a dry run changed the persisted state to %v", rule["state"])
	}
}

func TestAlertChannelLifecycle(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	h := srv.Handler()

	rr, created := doJSON(t, h, http.MethodPost, "/api/v1/alerts/channels",
		`{"name":"ops","type":"slack","url":"https://hooks.example/abc"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create channel = %d: %s", rr.Code, rr.Body.String())
	}
	id := created["id"].(string)

	rr, listed := doJSON(t, h, http.MethodGet, "/api/v1/alerts/channels", "")
	if rr.Code != http.StatusOK || len(listed["channels"].([]any)) != 1 {
		t.Fatalf("list channels = %d %v", rr.Code, listed)
	}

	// A bad URL scheme is refused rather than stored to fail later.
	rr, _ = doJSON(t, h, http.MethodPost, "/api/v1/alerts/channels",
		`{"name":"bad","type":"webhook","url":"ftp://nope"}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid channel URL = %d, want 400", rr.Code)
	}

	rr, _ = doJSON(t, h, http.MethodDelete, "/api/v1/alerts/channels/"+id, "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete channel = %d", rr.Code)
	}
}

// TestAlertEndpointsRequireAdmin: alert rules can reach out to arbitrary URLs,
// so they must sit behind the admin guard like the rest of the query surface.
func TestAlertEndpointsRequireAdmin(t *testing.T) {
	cfg := config.Default()
	cfg.AdminToken = "sec"
	srv, _ := newServer(t, cfg)
	h := srv.Handler()

	for _, path := range []string{"/api/v1/alerts", "/api/v1/alerts/channels"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated GET %s = %d, want 401", path, rr.Code)
		}
	}
}
