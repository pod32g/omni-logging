package tail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/pod32g/omni-logging/internal/query"
)

// heartbeatInterval keeps proxies and clients from timing out an idle stream.
const heartbeatInterval = 20 * time.Second

// Handler returns an http.Handler that streams matching events as
// Server-Sent Events. The query is taken from the same parameters as /search
// (q, from, to, last). now is injected for testability.
func Handler(hub *Hub, now func() time.Time) http.HandlerFunc {
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

		sub := hub.Subscribe(q, 256)
		defer sub.Close()

		// Initial comment so clients know the stream is open.
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			case e, ok := <-sub.C:
				if !ok {
					return
				}
				data, err := json.Marshal(e)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	}
}
