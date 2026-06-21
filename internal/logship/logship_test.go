package logship

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestShipsWarnNotInfo(t *testing.T) {
	var mu sync.Mutex
	var recs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sc := bufio.NewScanner(r.Body)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(bytes.TrimSpace(sc.Bytes()), &m) == nil {
				recs = append(recs, m)
			}
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	h, err := NewHandler(Config{URL: srv.URL, APIKey: "k", Service: "omnilog",
		MinLevel: slog.LevelWarn, FlushInterval: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(h)
	// This Info simulates the per-request ingest log that would cause a loop —
	// it must NOT be shipped.
	log.Info("request", "path", "/api/v1/ingest", "code", 200)
	log.Warn("ingest buffer full", "dropped", 12)
	log.Error("store unavailable")
	if err := h.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(recs) != 2 {
		t.Fatalf("want 2 shipped (warn+error), got %d: %+v", len(recs), recs)
	}
	for _, r := range recs {
		if r["level"] == "info" {
			t.Fatalf("info must not be shipped (loop guard): %+v", r)
		}
		if r["service"] != "omnilog" {
			t.Fatalf("bad service: %+v", r)
		}
	}
}
