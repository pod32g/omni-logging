package forward

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// flakyServer refuses the first n requests with the given status, then accepts,
// recording every batch ID it saw.
type flakyServer struct {
	mu       sync.Mutex
	failures int
	status   int
	seen     []string
	bodies   []string
	srv      *httptest.Server
}

func newFlakyServer(t *testing.T, failures, status int) *flakyServer {
	t.Helper()
	f := &flakyServer{failures: failures, status: status}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failures > 0 {
			f.failures--
			http.Error(w, "nope", f.status)
			return
		}
		f.seen = append(f.seen, r.Header.Get("X-Batch-Id"))
		f.bodies = append(f.bodies, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *flakyServer) accepted() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...), append([]string(nil), f.bodies...)
}

func newForwarder(t *testing.T, srvURL, spoolDir string) *Forwarder {
	t.Helper()
	f, err := New(Options{
		ServerURL: srvURL, Files: []string{filepath.Join(t.TempDir(), "unused.log")},
		SpoolDir: spoolDir, Logger: quiet(),
		Client: &http.Client{Timeout: 2 * time.Second},
		// The retry *schedule* is covered by TestRetryPolicy; these tests are
		// about the spool's behaviour, so they must not wait out real backoff.
		RetryBackoff: time.Millisecond, RetryMaxBackoff: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestSpoolRetriesUntilAccepted covers the core promise: with a spool the
// forwarder keeps trying rather than giving up after a fixed budget, which is
// what turned a transient outage into lost logs.
func TestSpoolRetriesUntilAccepted(t *testing.T) {
	// More failures than the bounded retry budget used without a spool.
	srv := newFlakyServer(t, maxPostAttempts+3, http.StatusServiceUnavailable)
	dir := t.TempDir()
	f := newForwarder(t, srv.srv.URL, dir)

	f.deliver(context.Background(), []string{"line one", "line two"})

	ids, bodies := srv.accepted()
	if len(ids) != 1 {
		t.Fatalf("server accepted %d batches, want 1 after the retries", len(ids))
	}
	if bodies[0] != "line one\nline two" {
		t.Errorf("delivered body = %q", bodies[0])
	}
	if ids[0] == "" {
		t.Error("the batch was delivered without an X-Batch-Id, so a retry could not be recognised")
	}
}

// TestSpoolRetryReusesTheBatchID: a retry must look like the same batch, or
// server-side de-duplication cannot work.
func TestSpoolRetryReusesTheBatchID(t *testing.T) {
	var (
		mu  sync.Mutex
		ids []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get("X-Batch-Id"))
		n := len(ids)
		mu.Unlock()
		if n < 3 {
			http.Error(w, "later", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := newForwarder(t, srv.URL, t.TempDir())
	f.deliver(context.Background(), []string{"x"})

	mu.Lock()
	defer mu.Unlock()
	if len(ids) < 3 {
		t.Fatalf("expected at least 3 attempts, saw %d", len(ids))
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("attempt %d used batch ID %q, want the original %q", i, id, ids[0])
		}
	}
}

// TestSpoolSurvivesRestart is the whole point of the feature: a batch that
// could not be delivered before the process died must be delivered after it
// comes back.
func TestSpoolSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	// First run: the server is unreachable, and the context is cancelled while
	// the batch is still queued.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	f1 := newForwarder(t, down.URL, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	f1.deliver(ctx, []string{"survive me", "and me"})
	cancel()
	if err := f1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	down.Close()

	// Second run against a healthy server: the pending batch is replayed.
	up := newFlakyServer(t, 0, 0)
	f2 := newForwarder(t, up.srv.URL, dir)
	if err := f2.drainSpool(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}

	_, bodies := up.accepted()
	if len(bodies) != 1 {
		t.Fatalf("replayed %d batches after restart, want 1", len(bodies))
	}
	if bodies[0] != "survive me\nand me" {
		t.Errorf("replayed body = %q", bodies[0])
	}
}

// TestSpoolAckStopsReplay: once delivered, a batch must not be sent again on
// the next start.
func TestSpoolAckStopsReplay(t *testing.T) {
	dir := t.TempDir()
	srv := newFlakyServer(t, 0, 0)

	f1 := newForwarder(t, srv.srv.URL, dir)
	f1.deliver(context.Background(), []string{"once"})
	f1.Close()

	f2 := newForwarder(t, srv.srv.URL, dir)
	if err := f2.drainSpool(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, bodies := srv.accepted()
	if len(bodies) != 1 {
		t.Fatalf("batch was delivered %d times; an acknowledged batch must not replay", len(bodies))
	}
}

// TestSpoolDeadLetterOnPermanentRejection: data the server will never accept
// must remain recoverable by hand rather than vanish into a log line.
func TestSpoolDeadLetterOnPermanentRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	f := newForwarder(t, srv.URL, dir)
	f.deliver(context.Background(), []string{"rejected line"})

	path := filepath.Join(dir, "dead-letter.ndjson")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dead-letter file not written: %v", err)
	}
	var rec deadLetterRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec); err != nil {
		t.Fatalf("dead-letter record is not valid JSON: %v (%s)", err, data)
	}
	if len(rec.Lines) != 1 || rec.Lines[0] != "rejected line" {
		t.Errorf("dead-letter lines = %v, want the original line", rec.Lines)
	}
	if rec.Batch == "" || rec.Reason == "" {
		t.Errorf("dead-letter record lacks context: %+v", rec)
	}

	// A dead-lettered batch is settled, so it must not replay forever.
	f2 := newForwarder(t, srv.URL, dir)
	if err := f2.drainSpool(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if lines := countLines(after); lines != 1 {
		t.Fatalf("dead-letter has %d records after a restart; a settled batch must not replay", lines)
	}
}

// TestWithoutSpoolStillGivesUp documents the behaviour the spool replaces: no
// spool means a bounded retry budget and then loss.
func TestWithoutSpoolStillGivesUp(t *testing.T) {
	srv := newFlakyServer(t, 1000, http.StatusServiceUnavailable)
	f := newForwarder(t, srv.srv.URL, "") // no spool
	if f.spool != nil {
		t.Fatal("no spool should have been created")
	}

	done := make(chan struct{})
	go func() {
		f.deliver(context.Background(), []string{"doomed"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("delivery without a spool must give up rather than retry forever")
	}
}

func countLines(b []byte) int {
	n := 0
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}
