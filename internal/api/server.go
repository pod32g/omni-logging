// Package api wires the store, ingestor, and live-tail hub into an HTTP handler
// that serves the JSON API and the embedded web UI.
package api

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/pod32g/omni-logging/internal/config"
	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/metrics"
	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
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
	// Closing is closed when the process begins shutting down. Live-tail
	// streams watch it so they end promptly instead of holding graceful
	// shutdown open for its full timeout. nil means "never closes".
	Closing <-chan struct{}
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
	closing  <-chan struct{}

	// exportSlots bounds concurrent exports so a handful of long downloads
	// cannot occupy every read connection and starve interactive searches.
	exportSlots chan struct{}

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
		cfg:         d.Config,
		store:       d.Store,
		ingestor:    d.Ingestor,
		hub:         d.Hub,
		ui:          d.UI,
		logger:      d.Logger,
		now:         d.Now,
		settings:    d.Settings,
		version:     d.Version,
		closing:     d.Closing,
		metrics:     d.Metrics,
		exportSlots: make(chan struct{}, maxConcurrentExports),
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

// backfill seeds a new live-tail stream with the most recent matching events,
// so opening the tail on a quiet system shows recent history instead of an
// empty pane. It runs on the store's read pool, so it cannot delay ingestion.
func (s *Server) backfill(ctx context.Context, q query.Query, limit int) ([]model.LogEvent, error) {
	if s.store == nil {
		return nil, nil
	}
	q.Limit = limit
	q.Order = query.OrderNewest
	q.AfterTS, q.AfterID = time.Time{}, "" // history is independent of any cursor
	res, err := s.store.Search(ctx, q)
	if err != nil {
		s.logger.Warn("live tail backfill failed", "error", err)
		return nil, err
	}
	return res.Events, nil
}

// route is one registered endpoint. Handler installs these on the mux and the
// OpenAPI conformance test walks the same slice, so an endpoint cannot ship
// without a decision about whether it belongs in the published contract.
type route struct {
	Method  string
	Path    string
	handler http.Handler
	// InSpec marks the endpoint as part of the documented API surface. It is
	// false for the contract document itself and the human-facing viewer that
	// renders it — discovery artifacts rather than API operations.
	InSpec bool
}

// routes returns every endpoint this server exposes, in registration order.
func (s *Server) routes() []route {
	rs := make([]route, 0, 16)
	add := func(method, path string, h http.Handler, inSpec bool) {
		rs = append(rs, route{Method: method, Path: path, handler: h, InSpec: inSpec})
	}

	if s.ingestor != nil {
		add("POST", "/api/v1/ingest", s.requireIngestKey(s.ingestor.Handler()), true)
		add("POST", "/api/v1/ingest/raw", s.requireIngestKey(s.ingestor.RawHandler()), true)
	}
	add("GET", "/api/v1/search", s.requireAdmin(s.handleSearch), true)
	add("GET", "/api/v1/search/stats", s.requireAdmin(s.handleStats), true)
	add("GET", "/api/v1/aggregate", s.requireAdmin(s.handleAggregate), true)
	add("GET", "/api/v1/export", s.requireAdmin(s.handleExport), true)
	add("GET", "/api/v1/tail", s.requireAdmin(tail.Handler(tail.Options{
		Hub:      s.hub,
		Now:      s.now,
		Closing:  s.closing,
		Backfill: s.backfill,
	})), true)
	add("GET", "/api/v1/healthz", http.HandlerFunc(s.handleHealth), true)
	add("GET", "/api/v1/readyz", http.HandlerFunc(s.handleReady), true)
	add("GET", "/api/v1/status", s.requireAdmin(s.handleStatus), true)
	add("GET", "/api/v1/config", s.requireAdmin(s.handleConfigGet), true)
	add("PUT", "/api/v1/config", s.requireAdmin(s.handleConfigPut), true)

	metricsHandler := http.Handler(http.HandlerFunc(s.handleMetrics))
	if !s.cfg.MetricsPublic {
		metricsHandler = loopbackOnly(metricsHandler)
	}
	add("GET", "/metrics", metricsHandler, true)

	add("GET", "/openapi.json", http.HandlerFunc(s.handleOpenAPI), false)
	add("GET", "/docs", http.HandlerFunc(s.handleDocs), false)
	add("GET", "/docs.css", http.HandlerFunc(s.handleDocsCSS), false)
	add("GET", "/docs.js", http.HandlerFunc(s.handleDocsJS), false)
	return rs
}

// Handler returns the fully wired HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.Method+" "+rt.Path, rt.handler)
	}
	if s.ui != nil {
		mux.Handle("/", http.FileServerFS(s.ui))
	}

	// Ordering matters: recovery sits innermost so the metrics and access-log
	// layers observe the 500 it writes (and still see a request that aborts the
	// connection, which recovery re-panics).
	return requestIDMiddleware(
		s.securityHeaders(
			logMiddleware(s.logger,
				s.metricsMiddleware(
					recoverMiddleware(s.logger, mux)))))
}
