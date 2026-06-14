package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureServer records raw ingest bodies it receives.
type captureServer struct {
	mu    sync.Mutex
	lines []string
	key   string
}

func (c *captureServer) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/ingest/raw" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	c.mu.Lock()
	c.key = r.Header.Get("X-Api-Key")
	c.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	for _, l := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if l != "" {
			c.mu.Lock()
			c.lines = append(c.lines, l)
			c.mu.Unlock()
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (c *captureServer) got() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
}

func TestForwarder_TailsAndPosts(t *testing.T) {
	cap := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("old line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fwd, err := New(Options{
		ServerURL:     srv.URL,
		APIKey:        "devkey",
		Service:       "test-svc",
		Files:         []string{path},
		FromStart:     true,
		FlushInterval: 50 * time.Millisecond,
		PollInterval:  20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fwd.Run(ctx); close(done) }()

	// Append more lines after the forwarder has started.
	time.Sleep(60 * time.Millisecond)
	appendLine(t, path, "new line one\n")
	appendLine(t, path, "new line two\n")

	// Wait for delivery.
	deadline := time.After(3 * time.Second)
	for {
		if len(cap.got()) >= 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out; received %v", cap.got())
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	<-done

	got := cap.got()
	want := map[string]bool{"old line": false, "new line one": false, "new line two": false}
	for _, l := range got {
		if _, ok := want[l]; ok {
			want[l] = true
		}
	}
	for line, seen := range want {
		if !seen {
			t.Errorf("missing forwarded line %q (got %v)", line, got)
		}
	}
	if cap.key != "devkey" {
		t.Errorf("API key = %q, want devkey", cap.key)
	}
}

func appendLine(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(Options{Files: []string{"x"}}); err == nil {
		t.Error("expected error when ServerURL missing")
	}
	if _, err := New(Options{ServerURL: "http://x"}); err == nil {
		t.Error("expected error when no files given")
	}
}
