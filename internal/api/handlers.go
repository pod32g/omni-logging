package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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
	s.queryDur.With("search").Observe(float64(res.TookMs) / 1000)
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
	s.queryDur.With("stats").Observe(float64(res.TookMs) / 1000)
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
		After:    v.Get("after"),
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

// handleReady is the readiness probe: it reports 200 only when the backend
// store is reachable, else 503. Unlike liveness, a 503 here tells an
// orchestrator/load balancer to stop routing traffic until the store recovers.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "reason": "store unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

// handleMetrics renders the Prometheus text exposition for all collectors.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.metrics.WriteProm(w); err != nil {
		s.logger.Error("metrics render failed", "error", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
