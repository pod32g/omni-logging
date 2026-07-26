package queryclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// TestFollowOutlivesTheRequestTimeout guards the streaming client. Reusing the
// one-shot client meant its end-to-end Timeout applied to reading the SSE body,
// so `omnilog query --follow` died with "context deadline exceeded" after 30
// seconds no matter how healthy the stream was.
func TestFollowOutlivesTheRequestTimeout(t *testing.T) {
	const gap = 40 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server response is not flushable")
			return
		}
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()
		// Streams until the client goes away, like the real /api/v1/tail.
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
			fmt.Fprintf(w, "data: {\"message\":\"event-%d\"}\n\n", i)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	// A stream that runs well past the one-shot client's 30s budget is not
	// practical to test directly, so shorten the budget instead: the streaming
	// path must not be governed by it at all.
	c := &Client{ServerURL: srv.URL}
	if c.streamClient().Timeout != 0 {
		t.Fatalf("streamClient has a timeout of %v; a live tail must not be deadlined", c.streamClient().Timeout)
	}
	if c.httpClient().Timeout == 0 {
		t.Fatal("the one-shot client should still carry a timeout")
	}

	var got atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- c.Follow(ctx, map[string]string{}, func(model.LogEvent) { got.Add(1) })
	}()

	// Well past where a 30s-style end-to-end deadline would bite, scaled down.
	deadline := time.After(time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("Follow returned early after %d events: %v", got.Load(), err)
		case <-deadline:
			if n := got.Load(); n < 5 {
				t.Fatalf("received %d events, want the stream to still be flowing", n)
			}
			return
		}
	}
}
