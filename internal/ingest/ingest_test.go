package ingest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/tail"
	"github.com/pod32g/omni-logging/internal/wal"
)

func mkEvent(msg string) model.LogEvent {
	e := model.LogEvent{Service: "t", Level: model.LevelInfo, Message: msg}
	e.Normalize(time.Now())
	return e
}

func total(t *testing.T, db *sqlite.DB) int64 {
	t.Helper()
	q, _ := query.Parse("")
	res, err := db.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return res.Total
}

// TestIngest_WALCrashRecovery enqueues events (which the WAL persists before
// acking) but never flushes — simulating a crash before the batch writer runs —
// then recovers them into the store from the WAL on "restart".
func TestIngest_WALCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	db := newStore(t)

	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	ing := New(db, nil, Options{WAL: w}) // not Start()ed: nothing reaches the store
	for i := 0; i < 5; i++ {
		if !ing.Enqueue(mkEvent(fmt.Sprintf("event-%d", i))) {
			t.Fatalf("enqueue %d rejected", i)
		}
	}
	if got := total(t, db); got != 0 {
		t.Fatalf("store should be empty before recovery, got %d", got)
	}
	w.Close() // crash

	// Restart on the same (durable) store and WAL directory.
	w2, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen wal: %v", err)
	}
	t.Cleanup(func() { w2.Close() })
	ing2 := New(db, nil, Options{WAL: w2})

	n, err := ing2.Recover(context.Background())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 5 {
		t.Fatalf("recovered %d events, want 5", n)
	}
	if got := total(t, db); got != 5 {
		t.Fatalf("after recovery store has %d, want 5", got)
	}

	// Recovery is idempotent (checkpoint advanced; ULID INSERT OR REPLACE).
	n2, err := ing2.Recover(context.Background())
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second recover replayed %d, want 0", n2)
	}
	if got := total(t, db); got != 5 {
		t.Fatalf("after second recovery store has %d, want 5 (not duplicated)", got)
	}
}

// TestIngest_WALBackpressureNotWALd verifies events rejected for backpressure are
// not written to the WAL (otherwise an unacked event could be "recovered").
func TestIngest_WALBackpressureNotWALd(t *testing.T) {
	dir := t.TempDir()
	db := newStore(t)
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	ing := New(db, nil, Options{WAL: w, BufferSize: 1}) // not started: fills at 1
	accepted := 0
	for i := 0; i < 3; i++ {
		if ing.Enqueue(mkEvent("x")) {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d, want 1", accepted)
	}
	count := 0
	if err := w.Replay(func(seq uint64, p []byte) error { count++; return nil }); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 1 {
		t.Fatalf("WAL has %d records, want 1 (rejected events must not be WAL'd)", count)
	}
}

// TestIngest_WALNormalFlowCheckpoints verifies the batch writer advances the WAL
// checkpoint after committing, so a later recovery replays nothing.
func TestIngest_WALNormalFlowCheckpoints(t *testing.T) {
	dir := t.TempDir()
	db := newStore(t)
	w, err := wal.Open(wal.Options{Dir: dir})
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	ing := New(db, nil, Options{WAL: w, FlushInterval: 5 * time.Millisecond})
	ing.Start()
	for i := 0; i < 3; i++ {
		ing.Enqueue(mkEvent(fmt.Sprintf("n%d", i)))
	}
	ing.Stop() // drains and flushes

	if w.Checkpoint() != 3 {
		t.Fatalf("checkpoint = %d, want 3", w.Checkpoint())
	}
	if got := total(t, db); got != 3 {
		t.Fatalf("store has %d, want 3", got)
	}
	ing2 := New(db, nil, Options{WAL: w})
	if n, _ := ing2.Recover(context.Background()); n != 0 {
		t.Fatalf("recover after clean flush replayed %d, want 0", n)
	}
}

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
