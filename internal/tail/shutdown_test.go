package tail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandlerEndsOnClosing covers prompt shutdown of live-tail streams.
// http.Server.Shutdown waits for handlers to return but never cancels their
// request contexts, so without an explicit signal an idle SSE client keeps
// graceful shutdown blocked until its timeout expires.
func TestHandlerEndsOnClosing(t *testing.T) {
	hub := NewHub()
	closing := make(chan struct{})
	h := Handler(Options{Hub: hub, Now: time.Now, Closing: closing})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tail", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h(rr, req)
	}()

	// Let the handler subscribe before signalling.
	deadline := time.Now().Add(2 * time.Second)
	for hub.SubscriberCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler never subscribed")
		}
		time.Sleep(time.Millisecond)
	}

	close(closing)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end after the shutdown signal; it would hold graceful shutdown open")
	}

	if body := rr.Body.String(); !strings.Contains(body, "shutting down") {
		t.Errorf("stream should tell the client why it ended, got: %q", body)
	}
	if hub.SubscriberCount() != 0 {
		t.Errorf("subscriber was not released: %d still registered", hub.SubscriberCount())
	}
}

// TestHandlerWithNilClosing: a nil channel blocks forever in select, so servers
// that do not wire the signal keep their previous behaviour.
func TestHandlerWithNilClosing(t *testing.T) {
	hub := NewHub()
	h := Handler(Options{Hub: hub, Now: time.Now})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tail", nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h(httptest.NewRecorder(), req)
	}()

	select {
	case <-done:
		t.Fatal("stream ended without a shutdown signal or a cancelled request")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not end when the client disconnected")
	}
}
