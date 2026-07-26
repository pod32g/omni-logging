package ingest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBatchDedupeSuppressesRetries covers the server half of at-least-once
// delivery: a client that re-sends a batch because the acknowledgement was lost
// must not have every event in it stored twice.
func TestBatchDedupeSuppressesRetries(t *testing.T) {
	db := newStore(t)
	ing := New(db, nil, Options{FlushInterval: 5 * time.Millisecond})
	ing.Start()
	defer ing.Stop()

	send := func(batchID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
		if batchID != "" {
			req.Header.Set("X-Batch-Id", batchID)
		}
		rr := httptest.NewRecorder()
		ing.Handler()(rr, req)
		return rr
	}

	body := `{"message":"only once"}`
	if rr := send("batch-1", body); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"accepted":1`) {
		t.Fatalf("first delivery = %d %s", rr.Code, rr.Body.String())
	}
	rr := send("batch-1", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("a retry must be answered 200 so the client stops retrying, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"duplicate":true`) {
		t.Fatalf("retry response should say it was a duplicate: %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"accepted":1`) {
		t.Fatalf("the retry was ingested again: %s", rr.Body.String())
	}
	if m := ing.Metrics(); m.Duplicates != 1 {
		t.Errorf("duplicates counter = %d, want 1", m.Duplicates)
	}

	// A different batch ID is genuinely new data, not a retry.
	if rr := send("batch-2", body); !strings.Contains(rr.Body.String(), `"accepted":1`) {
		t.Fatalf("a new batch ID must be accepted: %s", rr.Body.String())
	}
	// No batch ID at all keeps the old behaviour.
	if rr := send("", body); !strings.Contains(rr.Body.String(), `"accepted":1`) {
		t.Fatalf("a request without a batch ID must be accepted: %s", rr.Body.String())
	}
}

func TestBatchDedupeExpires(t *testing.T) {
	clock := time.Now()
	d := newBatchDeduper(func() time.Time { return clock })

	if d.Seen("a") {
		t.Fatal("first sighting must not be a duplicate")
	}
	if !d.Seen("a") {
		t.Fatal("an immediate repeat must be a duplicate")
	}
	// Past the window, the ID is forgotten: the guarantee is at-least-once, and
	// the window only suppresses the duplicate a retry produces.
	clock = clock.Add(dedupeTTL + time.Second)
	if d.Seen("a") {
		t.Fatal("an entry older than the TTL should have expired")
	}
	if d.Seen("") {
		t.Fatal("an empty batch ID is never a duplicate")
	}
}

// TestBatchDedupeIsBounded: memory must stay bounded even if a client mints a
// new batch ID for every request.
func TestBatchDedupeIsBounded(t *testing.T) {
	clock := time.Now()
	d := newBatchDeduper(func() time.Time { return clock })
	for i := 0; i < maxDedupeEntries+5000; i++ {
		d.Seen(fmt.Sprintf("id-%d", i))
		clock = clock.Add(time.Millisecond)
	}
	d.mu.Lock()
	n := len(d.seen)
	d.mu.Unlock()
	if n > maxDedupeEntries {
		t.Fatalf("dedupe set grew to %d entries, above the %d cap", n, maxDedupeEntries)
	}
}
