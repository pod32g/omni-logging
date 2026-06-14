package api

import (
	"encoding/json"
	"net/http"

	"github.com/pod32g/omni-logging/internal/query"
)

// handleSearch executes a search and returns matching events plus the total.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q, err := s.buildQuery(r)
	if err != nil {
		http.Error(w, "invalid query: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.store.Search(r.Context(), q)
	if err != nil {
		s.logger.Error("search failed", "error", err)
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleStats returns the histogram and facets for a query.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	q, err := s.buildQuery(r)
	if err != nil {
		http.Error(w, "invalid query: "+err.Error(), http.StatusBadRequest)
		return
	}
	res, err := s.store.Stats(r.Context(), q)
	if err != nil {
		s.logger.Error("stats failed", "error", err)
		http.Error(w, "stats failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// buildQuery parses request parameters into a normalized query.Query.
func (s *Server) buildQuery(r *http.Request) (query.Query, error) {
	v := r.URL.Query()
	p := query.Params{
		Q:        v.Get("q"),
		From:     v.Get("from"),
		To:       v.Get("to"),
		Last:     v.Get("last"),
		Limit:    v.Get("limit"),
		Order:    v.Get("order"),
		Interval: v.Get("interval"),
	}
	return p.Build(s.now())
}

// handleHealth reports liveness and ingest metrics. It requires no auth so it
// can be used as a load-balancer health check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"status":      "ok",
		"subscribers": s.hub.SubscriberCount(),
	}
	if s.ingestor != nil {
		resp["ingest"] = s.ingestor.Metrics()
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
