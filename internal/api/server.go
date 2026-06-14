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
	Now      func() time.Time // injectable clock (defaults to time.Now)
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
}

// New creates a Server from its dependencies.
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Server{
		cfg:      d.Config,
		store:    d.Store,
		ingestor: d.Ingestor,
		hub:      d.Hub,
		ui:       d.UI,
		logger:   d.Logger,
		now:      d.Now,
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
	mux.HandleFunc("GET /api/v1/tail", s.requireAdmin(tail.Handler(s.hub, s.now)))
	mux.HandleFunc("GET /api/v1/healthz", s.handleHealth)

	if s.ui != nil {
		mux.Handle("/", http.FileServerFS(s.ui))
	}

	return recoverMiddleware(s.logger, logMiddleware(s.logger, mux))
}
