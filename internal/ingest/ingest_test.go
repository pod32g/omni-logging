package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/tail"
)

func newStore(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestIngest_NDJSON_RoundTrip(t *testing.T) {
	db := newStore(t)
	ing := New(db, nil, Options{FlushInterval: 10 * time.Millisecond})
	ing.Start()

	body := strings.Join([]string{
		`{"service":"api","level":"error","message":"boom one"}`,
		`{"service":"api","level":"info","message":"hello two"}`,
		`not-json`,
	}, "\n")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	rr := httptest.NewRecorder()
	ing.Handler()(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"accepted":2`) || !strings.Contains(rr.Body.String(), `"rejected":1`) {
		t.Fatalf("unexpected response: %s", rr.Body.String())
	}

	ing.Stop() // flush

	q, _ := query.Parse("boom")
	res, err := db.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 1 || res.Events[0].Message != "boom one" {
		t.Fatalf("search after ingest = %+v", res.Events)
	}
}

func TestIngest_JSONArray(t *testing.T) {
	db := newStore(t)
	ing := New(db, nil, Options{FlushInterval: 10 * time.Millisecond})
	ing.Start()
	defer ing.Stop()

	body := `[{"service":"a","message":"x"},{"service":"b","message":"y"}]`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	rr := httptest.NewRecorder()
	ing.Handler()(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"accepted":2`) {
		t.Fatalf("array ingest failed: %d %s", rr.Code, rr.Body.String())
	}
}

func TestIngest_Raw(t *testing.T) {
	db := newStore(t)
	ing := New(db, nil, Options{FlushInterval: 10 * time.Millisecond})
	ing.Start()

	body := "line one\nline two\n\nline three\n"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/raw?service=nginx&level=warn", strings.NewReader(body))
	rr := httptest.NewRecorder()
	ing.RawHandler()(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"accepted":3`) {
		t.Fatalf("raw ingest = %d %s", rr.Code, rr.Body.String())
	}

	ing.Stop()
	q, _ := query.Parse("service=nginx")
	res, _ := db.Search(context.Background(), q)
	if res.Total != 3 {
		t.Fatalf("raw events stored = %d, want 3", res.Total)
	}
	for _, e := range res.Events {
		if e.Level != model.LevelWarn {
			t.Errorf("level = %q, want warn", e.Level)
		}
	}
}

func TestIngest_Backpressure429(t *testing.T) {
	db := newStore(t)
	// Buffer of 1 and no Start(): nothing drains, so the buffer fills immediately.
	ing := New(db, nil, Options{BufferSize: 1})

	body := strings.Join([]string{
		`{"message":"1"}`, `{"message":"2"}`, `{"message":"3"}`,
	}, "\n")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	rr := httptest.NewRecorder()
	ing.Handler()(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"accepted":1`) {
		t.Fatalf("expected 1 accepted before overflow: %s", rr.Body.String())
	}
}

func TestIngest_BroadcastsToHub(t *testing.T) {
	db := newStore(t)
	hub := tail.NewHub()
	q, _ := query.Parse("level=error")
	sub := hub.Subscribe(q, 8)
	defer sub.Close()

	ing := New(db, hub, Options{FlushInterval: 5 * time.Millisecond})
	ing.Start()
	defer ing.Stop()

	body := `{"level":"error","message":"streamed"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	ing.Handler()(httptest.NewRecorder(), req)

	select {
	case e := <-sub.C:
		if e.Message != "streamed" {
			t.Fatalf("tail got %q, want streamed", e.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected event broadcast to tail hub")
	}
}
