package omnilog_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/sdk/omnilog"
)

// receiver stands in for the server's ingest endpoint and records what arrived.
type receiver struct {
	mu      sync.Mutex
	records []map[string]any
	keys    []string
	fail    int // respond 500 this many times first
	srv     *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		if r.fail > 0 {
			r.fail--
			r.mu.Unlock()
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		r.keys = append(r.keys, req.Header.Get("X-Api-Key"))
		r.mu.Unlock()

		var body io.Reader = req.Body
		if strings.EqualFold(req.Header.Get("Content-Encoding"), "gzip") {
			gz, err := gzip.NewReader(req.Body)
			if err != nil {
				http.Error(w, "bad gzip", http.StatusBadRequest)
				return
			}
			defer gz.Close()
			body = gz
		}

		sc := bufio.NewScanner(body)
		r.mu.Lock()
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				r.mu.Unlock()
				http.Error(w, "not NDJSON", http.StatusBadRequest)
				return
			}
			r.records = append(r.records, m)
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.records...)
}

func (r *receiver) waitFor(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := r.all()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d records, got %d", n, len(got))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newClient(t *testing.T, rec *receiver, mutate func(*omnilog.Options)) *omnilog.Client {
	t.Helper()
	opts := omnilog.Options{
		ServerURL:     rec.srv.URL,
		Service:       "test-svc",
		BatchSize:     10,
		FlushInterval: 20 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := omnilog.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestClientSendsNDJSON(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, func(o *omnilog.Options) { o.APIKey = "devkey" })

	c.Log("error", "boom", map[string]any{"status": 500, "path": "/pay"})
	c.Log("info", "fine", nil)

	got := rec.waitFor(t, 2)
	if got[0]["message"] != "boom" || got[0]["level"] != "error" {
		t.Errorf("first record = %v", got[0])
	}
	if got[0]["service"] != "test-svc" {
		t.Errorf("the client default service was not applied: %v", got[0])
	}
	// Attributes are flattened to top level, which is the shape the server's
	// ingest endpoint turns into searchable attributes.
	if got[0]["status"] != float64(500) || got[0]["path"] != "/pay" {
		t.Errorf("attributes were not flattened: %v", got[0])
	}

	rec.mu.Lock()
	key := rec.keys[0]
	rec.mu.Unlock()
	if key != "devkey" {
		t.Errorf("API key header = %q", key)
	}
}

// TestSendNeverBlocks is the property that makes this safe to call from a
// request handler: a stuck server must cost the caller nothing.
func TestSendNeverBlocks(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // never responds until the test says so
	}))
	defer srv.Close()
	defer close(block)

	c, err := omnilog.New(omnilog.Options{
		ServerURL: srv.URL, QueueSize: 8, BatchSize: 1,
		FlushInterval: time.Millisecond,
		HTTPClient:    &http.Client{Timeout: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			c.Log("info", "spam", nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Send blocked while the server was unresponsive")
	}
	if c.Stats().Dropped == 0 {
		t.Error("overflow should be counted as dropped so the loss is visible")
	}
}

func TestClientCompression(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, func(o *omnilog.Options) { o.Compress = true })
	c.Log("info", "compressed", map[string]any{"n": 1})

	got := rec.waitFor(t, 1)
	if got[0]["message"] != "compressed" {
		t.Errorf("record = %v", got[0])
	}
}

// TestCloseFlushes: events already queued must not be lost at shutdown.
func TestCloseFlushes(t *testing.T) {
	rec := newReceiver(t)
	c, err := omnilog.New(omnilog.Options{
		ServerURL: rec.srv.URL, BatchSize: 1000, FlushInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		c.Log("info", "queued", nil)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if got := rec.all(); len(got) != 5 {
		t.Fatalf("Close delivered %d of 5 queued events", len(got))
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close must be safe to call twice: %v", err)
	}
}

func TestDeliveryFailureIsReported(t *testing.T) {
	rec := newReceiver(t)
	rec.fail = 1

	var (
		mu   sync.Mutex
		errs []error
	)
	c := newClient(t, rec, func(o *omnilog.Options) {
		o.OnError = func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }
	})
	c.Log("info", "first", nil)

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(errs)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a failed delivery was not reported to OnError")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if c.Stats().Failed == 0 {
		t.Error("failed deliveries should be counted")
	}
}

func TestNewRequiresServerURL(t *testing.T) {
	if _, err := omnilog.New(omnilog.Options{}); err == nil {
		t.Fatal("expected an error without a server URL")
	}
}

// --- slog handler ----------------------------------------------------------

func TestSlogHandler(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, nil)
	log := slog.New(omnilog.NewHandler(c, nil))

	log.Error("payment failed", "status", 402, "retryable", true)
	log.Info("ok", slog.String("path", "/health"))

	got := rec.waitFor(t, 2)
	if got[0]["message"] != "payment failed" || got[0]["level"] != "error" {
		t.Errorf("record = %v", got[0])
	}
	// slog attributes become searchable attributes with no extra configuration.
	if got[0]["status"] != float64(402) || got[0]["retryable"] != true {
		t.Errorf("slog attributes were not carried: %v", got[0])
	}
	if got[0]["timestamp"] == nil {
		t.Error("the record's own time was not sent")
	}
}

// TestSlogGroupsFlattenToDottedKeys: the event model is flat and searched as
// attr.http.status, so a group has to become a dotted prefix rather than a
// nested object the query language cannot reach into.
func TestSlogGroupsFlattenToDottedKeys(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, nil)
	log := slog.New(omnilog.NewHandler(c, nil)).
		WithGroup("http").
		With("method", "GET")

	log.Info("request", slog.Int("status", 200), slog.Group("timing", slog.Int("ms", 12)))

	got := rec.waitFor(t, 1)
	for k, want := range map[string]any{
		"http.method":    "GET",
		"http.status":    float64(200),
		"http.timing.ms": float64(12),
	} {
		if got[0][k] != want {
			t.Errorf("%s = %#v, want %#v (record: %v)", k, got[0][k], want, got[0])
		}
	}
}

func TestSlogLevelMapping(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, nil)
	h := omnilog.NewHandler(c, &omnilog.HandlerOptions{Level: slog.LevelDebug})
	log := slog.New(h)

	log.Debug("d")
	log.Info("i")
	log.Warn("w")
	log.Error("e")
	log.Log(context.Background(), slog.LevelError+4, "f")

	got := rec.waitFor(t, 5)
	want := []string{"debug", "info", "warn", "error", "fatal"}
	for i, w := range want {
		if got[i]["level"] != w {
			t.Errorf("record %d level = %v, want %s", i, got[i]["level"], w)
		}
	}
}

func TestSlogLevelFiltering(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, nil)
	log := slog.New(omnilog.NewHandler(c, &omnilog.HandlerOptions{Level: slog.LevelWarn}))

	log.Info("dropped")
	log.Warn("kept")

	got := rec.waitFor(t, 1)
	time.Sleep(80 * time.Millisecond) // let anything else arrive
	if all := rec.all(); len(all) != 1 {
		t.Fatalf("shipped %d records, want only the warn: %v", len(all), all)
	}
	if got[0]["message"] != "kept" {
		t.Errorf("wrong record shipped: %v", got[0])
	}
}

// TestSlogFallbackKeepsLocalOutput: shipping logs must not mean losing them
// locally, which is the common reason people avoid a network handler.
func TestSlogFallbackKeepsLocalOutput(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, nil)

	var local bytes.Buffer
	log := slog.New(omnilog.NewHandler(c, &omnilog.HandlerOptions{
		Fallback: slog.NewTextHandler(&local, nil),
	}))
	log.With("svc", "x").Info("both places")

	rec.waitFor(t, 1)
	if !strings.Contains(local.String(), "both places") {
		t.Errorf("the fallback handler received nothing: %q", local.String())
	}
	if !strings.Contains(local.String(), "svc=x") {
		t.Errorf("WithAttrs did not reach the fallback: %q", local.String())
	}
}

// TestSlogHandlerIsConcurrencySafe: slog handlers are shared across goroutines,
// and a derived handler must not mutate its parent.
func TestSlogHandlerIsConcurrencySafe(t *testing.T) {
	rec := newReceiver(t)
	c := newClient(t, rec, nil)
	base := slog.New(omnilog.NewHandler(c, nil))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			base.With("worker", n).WithGroup("g").Info("concurrent", "i", n)
		}(i)
	}
	wg.Wait()

	got := rec.waitFor(t, 20)
	for _, r := range got[:20] {
		if _, ok := r["worker"]; !ok {
			t.Fatalf("record lost its attribute: %v", r)
		}
	}
}
