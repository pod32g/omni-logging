package tail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

func evt(id, msg string, ts time.Time) model.LogEvent {
	return model.LogEvent{ID: id, Message: msg, Timestamp: ts, Level: model.LevelInfo}
}

// runStream drives the handler until ctx is cancelled and returns what it wrote.
func runStream(t *testing.T, opts Options, url string, drive func(cancel func())) string {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Handler(opts)(rr, req)
	}()
	drive(cancel)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return")
	}
	return rr.Body.String()
}

// waitForSubscriber blocks until the handler has registered with the hub.
func waitForSubscriber(t *testing.T, hub *Hub) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for hub.SubscriberCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestBackfillSeedsTheStream covers the reason backfill exists: without it a
// live tail shows nothing at all until the next matching event is ingested,
// which on a quiet system is indistinguishable from a broken stream.
func TestBackfillSeedsTheStream(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	history := []model.LogEvent{ // newest first, as Search returns
		evt("c", "third", base.Add(2*time.Minute)),
		evt("b", "second", base.Add(time.Minute)),
		evt("a", "first", base),
	}
	hub := NewHub()
	opts := Options{
		Hub: hub,
		Now: time.Now,
		Backfill: func(context.Context, query.Query, int) ([]model.LogEvent, error) {
			return history, nil
		},
	}

	body := runStream(t, opts, "/api/v1/tail", func(cancel func()) {
		waitForSubscriber(t, hub)
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	for _, want := range []string{"first", "second", "third", ": end of history"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q:\n%s", want, body)
		}
	}
	// Oldest first, so the pane fills top-down the way a log file reads.
	if i, j := strings.Index(body, "first"), strings.Index(body, "third"); i > j {
		t.Errorf("history is newest-first; expected oldest first:\n%s", body)
	}
}

// TestBackfillDoesNotDuplicateLiveEvents covers the seam between history and
// live streaming. The handler subscribes before reading history so nothing is
// lost in between, which means an event can arrive on both paths.
func TestBackfillDoesNotDuplicateLiveEvents(t *testing.T) {
	shared := evt("dup", "appears in both", time.Now())
	hub := NewHub()
	opts := Options{
		Hub: hub,
		Now: time.Now,
		Backfill: func(context.Context, query.Query, int) ([]model.LogEvent, error) {
			// Publish while "reading history" so the same event is also queued
			// on the subscriber.
			hub.Publish(shared)
			return []model.LogEvent{shared}, nil
		},
	}

	body := runStream(t, opts, "/api/v1/tail", func(cancel func()) {
		waitForSubscriber(t, hub)
		time.Sleep(50 * time.Millisecond)
		hub.Publish(evt("later", "only live", time.Now()))
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if n := strings.Count(body, `"appears in both"`); n != 1 {
		t.Fatalf("event sent %d times, want exactly 1:\n%s", n, body)
	}
	if !strings.Contains(body, "only live") {
		t.Fatalf("de-duplication swallowed a genuinely new event:\n%s", body)
	}
}

// TestBackfillFailureKeepsStreamAlive: history is a convenience, so a store
// error must not cost the client its live stream.
func TestBackfillFailureKeepsStreamAlive(t *testing.T) {
	hub := NewHub()
	opts := Options{
		Hub: hub,
		Now: time.Now,
		Backfill: func(context.Context, query.Query, int) ([]model.LogEvent, error) {
			return nil, context.DeadlineExceeded
		},
	}

	body := runStream(t, opts, "/api/v1/tail", func(cancel func()) {
		waitForSubscriber(t, hub)
		time.Sleep(50 * time.Millisecond)
		hub.Publish(evt("live", "still streaming", time.Now()))
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if !strings.Contains(body, ": history unavailable") {
		t.Errorf("client was not told history was unavailable:\n%s", body)
	}
	if !strings.Contains(body, "still streaming") {
		t.Fatalf("a backfill failure killed the live stream:\n%s", body)
	}
}

// TestBackfillRespectsTheQuery: history must be filtered by the same query as
// the live stream, otherwise opening a filtered tail dumps unrelated events.
func TestBackfillRespectsTheQuery(t *testing.T) {
	var gotQuery query.Query
	var gotLimit int
	hub := NewHub()
	opts := Options{
		Hub:           hub,
		Now:           time.Now,
		BackfillLimit: 7,
		Backfill: func(_ context.Context, q query.Query, limit int) ([]model.LogEvent, error) {
			gotQuery, gotLimit = q, limit
			return nil, nil
		},
	}

	runStream(t, opts, "/api/v1/tail?q=level%3Derror+boom", func(cancel func()) {
		waitForSubscriber(t, hub)
		time.Sleep(50 * time.Millisecond)
		cancel()
	})

	if gotLimit != 7 {
		t.Errorf("backfill limit = %d, want the configured 7", gotLimit)
	}
	if len(gotQuery.Filters) != 1 || gotQuery.Filters[0].Field != query.FieldLevel {
		t.Errorf("backfill lost the level filter: %+v", gotQuery.Filters)
	}
	if len(gotQuery.Terms) != 1 || gotQuery.Terms[0] != "boom" {
		t.Errorf("backfill lost the free-text term: %+v", gotQuery.Terms)
	}
}

// TestNoBackfillConfigured keeps the previous behaviour available: without a
// backfill function the stream is purely forward-looking.
func TestNoBackfillConfigured(t *testing.T) {
	hub := NewHub()
	body := runStream(t, Options{Hub: hub, Now: time.Now}, "/api/v1/tail", func(cancel func()) {
		waitForSubscriber(t, hub)
		time.Sleep(30 * time.Millisecond)
		cancel()
	})
	if strings.Contains(body, "data:") {
		t.Fatalf("stream emitted history without a backfill function:\n%s", body)
	}
	if !strings.Contains(body, ": connected") {
		t.Fatalf("stream never opened:\n%s", body)
	}
}

// runStreamWithHeaders is runStream with request headers, so a reconnect can be
// simulated the way a browser makes one.
func runStreamWithHeaders(t *testing.T, opts Options, url string, headers map[string]string, drive func(cancel func())) string {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Handler(opts)(rr, req)
	}()
	drive(cancel)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return")
	}
	return rr.Body.String()
}

// TestStreamEmitsEventIDs: without an id: line the browser has nothing to send
// back as Last-Event-ID, so every reconnect replays the entire backfill.
func TestStreamEmitsEventIDs(t *testing.T) {
	hub := NewHub()
	now := time.Now()
	opts := Options{
		Hub: hub,
		Backfill: func(context.Context, query.Query, int) ([]model.LogEvent, error) {
			return []model.LogEvent{evt("01B", "second", now), evt("01A", "first", now)}, nil
		},
	}
	body := runStream(t, opts, "/api/v1/tail", func(cancel func()) {
		waitForSubscriber(t, hub)
		cancel()
	})
	for _, want := range []string{"id: 01A\n", "id: 01B\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q; the client cannot resume without it\n%s", want, body)
		}
	}
}

// TestReconnectResumesInsteadOfReplaying is the fix for what a dropped stream
// looked like from the UI: the browser reconnects automatically, the server
// replayed all 50 backfill events, and the pane filled with duplicates of rows
// already on screen.
func TestReconnectResumesInsteadOfReplaying(t *testing.T) {
	hub := NewHub()
	now := time.Now()
	history := []model.LogEvent{ // newest first, as search returns
		evt("01D", "newest", now),
		evt("01C", "third", now),
		evt("01B", "second", now),
		evt("01A", "oldest", now),
	}
	opts := Options{
		Hub: hub,
		Backfill: func(context.Context, query.Query, int) ([]model.LogEvent, error) {
			return history, nil
		},
	}

	// A reconnect saying "I already have everything up to 01B".
	body := runStreamWithHeaders(t, opts, "/api/v1/tail",
		map[string]string{"Last-Event-ID": "01B"},
		func(cancel func()) { waitForSubscriber(t, hub); cancel() })

	for _, gone := range []string{"oldest", "second"} {
		if strings.Contains(body, gone) {
			t.Errorf("replayed %q, which the client already had\n%s", gone, body)
		}
	}
	for _, want := range []string{"third", "newest"} {
		if !strings.Contains(body, want) {
			t.Errorf("dropped %q, which the client had not seen\n%s", want, body)
		}
	}
}

// TestFirstConnectStillGetsFullHistory: the resume logic must not starve a
// genuinely new stream, which sends no Last-Event-ID.
func TestFirstConnectStillGetsFullHistory(t *testing.T) {
	hub := NewHub()
	now := time.Now()
	opts := Options{
		Hub: hub,
		Backfill: func(context.Context, query.Query, int) ([]model.LogEvent, error) {
			return []model.LogEvent{evt("01B", "second", now), evt("01A", "first", now)}, nil
		},
	}
	body := runStream(t, opts, "/api/v1/tail", func(cancel func()) {
		waitForSubscriber(t, hub)
		cancel()
	})
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(body, want) {
			t.Errorf("a fresh stream should replay %q\n%s", want, body)
		}
	}
}
