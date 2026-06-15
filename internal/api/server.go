// Package api wires the store, ingestor, and live-tail hub into an HTTP handler
// that serves the JSON API and the embedded web UI.
package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/metrics"
	"github.com/pod32g/omni-logging/internal/settings"
	"github.com/pod32g/omni-logging/internal/store"
	"github.com/pod32g/omni-logging/internal/tail"
)

// Deps are the collaborators an API server needs.
type Deps struct {
	Config   config.Config
	Store    store.Store
	Ingestor *ingest.Ingestor
	Hub      *tail.Hub
	UI       fs.FS // embedded web assets
	Logger   *slog.Logger
	Now      func() time.Time  // injectable clock (defaults to time.Now)
	Metrics  *metrics.Registry // metrics registry (created if nil)
	Version  string            // build version, surfaced as omnilog_build_info
	Settings *settings.Manager // runtime-mutable config (nil = static cfg only)
}

// Server holds API dependencies and builds the HTTP handler.
type Server struct {
	cfg      config.Config
	store    store.Store
	ingestor *ingest.Ingestor
	hub      *tail.Hub
	ui       fs.FS
	logger   *slog.Logger
	now      func() time.Time
	settings *settings.Manager
	version  string

	metrics  *metrics.Registry
	httpReqs *metrics.CounterVec
	httpDur  *metrics.HistogramVec
	queryDur *metrics.HistogramVec
}

// latencyBuckets are the default duration buckets (seconds) for histograms.
var latencyBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}

// New creates a Server from its dependencies.
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Metrics == nil {
		d.Metrics = metrics.NewRegistry()
	}
	s := &Server{
		cfg:      d.Config,
		store:    d.Store,
		ingestor: d.Ingestor,
		hub:      d.Hub,
		ui:       d.UI,
		logger:   d.Logger,
		now:      d.Now,
		settings: d.Settings,
		version:  d.Version,
		metrics:  d.Metrics,
	}
	s.registerMetrics(d.Version)
	return s
}

// registerMetrics wires the metric collectors. Existing counters (ingest, tail)
// are exposed via function-backed collectors that read live values at scrape
// time, avoiding double-bookkeeping.
func (s *Server) registerMetrics(version string) {
	reg := s.metrics
	if version == "" {
		version = "unknown"
	}
	reg.NewGauge("omnilog_build_info", "Build information; value is always 1.", "version").With(version).Set(1)

	s.httpReqs = reg.NewCounter("omnilog_http_requests_total", "Total HTTP requests served.", "method", "code")
	s.httpDur = reg.NewHistogram("omnilog_http_request_duration_seconds", "HTTP request duration in seconds.", latencyBuckets, "method", "code")
	s.queryDur = reg.NewHistogram("omnilog_store_query_duration_seconds", "Store query duration in seconds.", latencyBuckets, "op")

	if s.ingestor != nil {
		reg.NewCounterFunc("omnilog_ingest_received_total", "Events accepted into the ingest buffer.", func() float64 { return float64(s.ingestor.Metrics().Received) })
		reg.NewCounterFunc("omnilog_ingest_written_total", "Events written durably to the store.", func() float64 { return float64(s.ingestor.Metrics().Written) })
		reg.NewCounterFunc("omnilog_ingest_dropped_total", "Events rejected because the ingest buffer was full.", func() float64 { return float64(s.ingestor.Metrics().Dropped) })
		reg.NewCounterFunc("omnilog_ingest_rejected_total", "Requests refused by admission control (rate limit / quota).", func() float64 { return float64(s.ingestor.Metrics().Rejected) })
		reg.NewGaugeFunc("omnilog_ingest_queued", "Events currently buffered awaiting a write.", func() float64 { return float64(s.ingestor.Metrics().Queued) })
	}
	if s.hub != nil {
		reg.NewGaugeFunc("omnilog_tail_subscribers", "Active live-tail subscribers.", func() float64 { return float64(s.hub.SubscriberCount()) })
		reg.NewCounterFunc("omnilog_tail_dropped_total", "Events dropped because a subscriber buffer was full.", func() float64 { return float64(s.hub.DroppedTotal()) })
		reg.NewCounterFunc("omnilog_tail_evicted_total", "Subscribers evicted for being too slow.", func() float64 { return float64(s.hub.EvictedTotal()) })
	}
}

// Handler returns the fully wired HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	if s.ingestor != nil {
		mux.HandleFunc("POST /api/v1/ingest", s.requireIngestKey(s.ingestor.Handler()))
		mux.HandleFunc("POST /api/v1/ingest/raw", s.requireIngestKey(s.ingestor.RawHandler()))
	}
	mux.HandleFunc("GET /api/v1/search", s.requireAdmin(s.handleSearch))
	mux.HandleFunc("GET /api/v1/search/stats", s.requireAdmin(s.handleStats))
	mux.HandleFunc("GET /api/v1/export", s.requireAdmin(s.handleExport))
	mux.HandleFunc("GET /api/v1/tail", s.requireAdmin(tail.Handler(s.hub, s.now)))
	mux.HandleFunc("GET /api/v1/healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/readyz", s.handleReady)
	metricsHandler := http.Handler(http.HandlerFunc(s.handleMetrics))
	if !s.cfg.MetricsPublic {
		metricsHandler = loopbackOnly(metricsHandler)
	}
	mux.Handle("GET /metrics", metricsHandler)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /docs", s.handleDocs)
	mux.HandleFunc("GET /api/v1/config", s.requireAdmin(s.handleConfigGet))
	mux.HandleFunc("PUT /api/v1/config", s.requireAdmin(s.handleConfigPut))

	if s.ui != nil {
		mux.Handle("/", http.FileServerFS(s.ui))
	}

	return requestIDMiddleware(securityHeaders(recoverMiddleware(s.logger, s.metricsMiddleware(logMiddleware(s.logger, mux)))))
}
