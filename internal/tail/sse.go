package tail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// heartbeatInterval keeps proxies and clients from timing out an idle stream.
const heartbeatInterval = 20 * time.Second

// DefaultBackfillLimit is how many recent matching events a new stream replays
// before going live.
const DefaultBackfillLimit = 50

// BackfillFunc returns the most recent events matching q, newest first. It is a
// function rather than a store.Store so this package stays independent of the
// persistence layer.
type BackfillFunc func(ctx context.Context, q query.Query, limit int) ([]model.LogEvent, error)

// Options configures the SSE handler.
type Options struct {
	Hub *Hub
	Now func() time.Time // injectable clock (defaults to time.Now)

	// Closing, when non-nil, is closed as the process starts shutting down.
	// Streams end as soon as it fires: net/http's Shutdown waits for handlers
	// to return but does not cancel their request contexts, so without this an
	// idle live-tail client would hold graceful shutdown open for its full
	// timeout.
	Closing <-chan struct{}

	// Backfill, when set, seeds a new stream with recent matching events.
	// Without it a live tail shows nothing at all until the next matching event
	// happens to be ingested, which on a quiet system looks indistinguishable
	// from a broken stream.
	Backfill      BackfillFunc
	BackfillLimit int // defaults to DefaultBackfillLimit
}

// Handler returns an http.Handler that streams matching events as
// Server-Sent Events. The query is taken from the same parameters as /search
// (q, from, to, last).
func Handler(opts Options) http.HandlerFunc {
	hub, now, closing := opts.Hub, opts.Now, opts.Closing
	if now == nil {
		now = time.Now
	}
	limit := opts.BackfillLimit
	if limit <= 0 {
		limit = DefaultBackfillLimit
	}
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		params := query.Params{
			Q:    r.URL.Query().Get("q"),
			From: r.URL.Query().Get("from"),
			To:   r.URL.Query().Get("to"),
			Last: r.URL.Query().Get("last"),
		}
		q, err := params.Build(now())
		if err != nil {
			http.Error(w, "invalid query: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Live tail follows new events forward; ignore historical bounds.
		q.From, q.To = time.Time{}, time.Time{}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

		// Subscribe BEFORE reading history. The other order would drop anything
		// ingested between the read and the subscription; this way such events
		// queue in the subscriber buffer and are de-duplicated below instead.
		sub := hub.Subscribe(q, 256)
		defer sub.Close()

		// Initial comment so clients know the stream is open.
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ctx := r.Context()
		replayed := map[string]struct{}{}
		if opts.Backfill != nil {
			events, berr := opts.Backfill(ctx, q, limit)
			if berr != nil {
				// History is a convenience; a failure here must not cost the
				// client its live stream.
				fmt.Fprint(w, ": history unavailable\n\n")
				flusher.Flush()
			} else {
				// Search returns newest first; emit oldest first so the pane
				// fills top-down the way a log file reads.
				for i := len(events) - 1; i >= 0; i-- {
					if !writeEvent(w, flusher, events[i]) {
						return
					}
					replayed[events[i].ID] = struct{}{}
				}
				if len(events) > 0 {
					fmt.Fprint(w, ": end of history\n\n")
					flusher.Flush()
				}
			}
		}

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-closing:
				fmt.Fprint(w, ": server shutting down\n\n")
				flusher.Flush()
				return
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case e, ok := <-sub.C:
				if !ok {
					if sub.Evicted() {
						// Tell the client why the stream ended; it will reconnect.
						fmt.Fprint(w, ": evicted (slow consumer; reconnecting)\n\n")
						flusher.Flush()
					}
					return
				}
				// An event ingested between subscribing and reading history
				// arrives on both paths; send it once.
				if len(replayed) > 0 {
					if _, dup := replayed[e.ID]; dup {
						delete(replayed, e.ID)
						continue
					}
				}
				if !writeEvent(w, flusher, e) {
					return
				}
			}
		}
	}
}

// writeEvent emits one event as an SSE data frame. It reports false when the
// event could not be serialized into a frame at all, which should not happen for
// a stored event and leaves the stream in an unknown state if it does.
func writeEvent(w http.ResponseWriter, flusher http.Flusher, e model.LogEvent) bool {
	data, err := json.Marshal(e)
	if err != nil {
		return true // skip this one; the stream itself is still fine
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
