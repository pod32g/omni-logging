// Package logship ships slog records to an omnilog server over HTTP
// (POST /api/v1/ingest, NDJSON, X-Api-Key), non-blocking and best-effort.
//
// When omnilog ships its OWN logs to itself, a minimum level (typically WARN)
// must be set so routine per-request Info logs — including the ingest requests
// this shipper generates — are never shipped, which would otherwise form a
// feedback loop (ingest → request log → ship → ingest → …).
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config configures the shipper. Zero batch/flush/buffer fields use defaults.
type Config struct {
	URL      string
	APIKey   string
	Service  string
	MinLevel slog.Level // records below this level are not shipped (loop guard)

	BatchSize     int
	FlushInterval time.Duration
	BufferSize    int
	Timeout       time.Duration
}

type shipper struct {
	endpoint  string
	apiKey    string
	service   string
	client    *http.Client
	batchSize int
	flush     time.Duration
	ch        chan map[string]any
	dropped   atomic.Int64
	done      chan struct{}
	wg        sync.WaitGroup
	once      sync.Once
}

// Handler is an slog.Handler that enqueues records (at/above MinLevel) for
// shipping.
type Handler struct {
	s        *shipper
	minLevel slog.Level
	attrs    []slog.Attr
}

var _ slog.Handler = (*Handler)(nil)

// NewHandler validates cfg, starts the worker, and returns a Handler.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.URL == "" || cfg.APIKey == "" {
		return nil, errors.New("logship: url and api_key are required")
	}
	if cfg.Service == "" {
		cfg.Service = "omnilog"
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 2 * time.Second
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1024
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	s := &shipper{
		endpoint:  strings.TrimRight(cfg.URL, "/") + "/api/v1/ingest",
		apiKey:    cfg.APIKey,
		service:   cfg.Service,
		client:    &http.Client{Timeout: cfg.Timeout},
		batchSize: cfg.BatchSize,
		flush:     cfg.FlushInterval,
		ch:        make(chan map[string]any, cfg.BufferSize),
		done:      make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return &Handler{s: s, minLevel: cfg.MinLevel}, nil
}

func (h *Handler) Dropped() int64 { return h.s.dropped.Load() }

func (h *Handler) Close(ctx context.Context) error {
	h.s.once.Do(func() { close(h.s.done) })
	done := make(chan struct{})
	go func() { h.s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.minLevel
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	rec := make(map[string]any, 4+len(h.attrs)+r.NumAttrs())
	rec["time"] = r.Time.UTC().Format(time.RFC3339Nano)
	rec["level"] = strings.ToLower(r.Level.String())
	rec["service"] = h.s.service
	rec["message"] = r.Message
	for _, a := range h.attrs {
		putAttr(rec, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		putAttr(rec, a)
		return true
	})
	select {
	case h.s.ch <- rec:
	default:
		h.s.dropped.Add(1)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &Handler{s: h.s, minLevel: h.minLevel, attrs: merged}
}

func (h *Handler) WithGroup(string) slog.Handler { return h }

func putAttr(m map[string]any, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	m[a.Key] = a.Value.Resolve().Any()
}

func (s *shipper) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.flush)
	defer ticker.Stop()
	batch := make([]map[string]any, 0, s.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.post(batch)
		batch = batch[:0]
	}
	for {
		select {
		case rec := <-s.ch:
			batch = append(batch, rec)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.done:
			for {
				select {
				case rec := <-s.ch:
					batch = append(batch, rec)
					if len(batch) >= s.batchSize {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}

func (s *shipper) post(batch []map[string]any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, rec := range batch {
		if err := enc.Encode(rec); err != nil {
			continue
		}
	}
	body := buf.Bytes()
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequest(http.MethodPost, s.endpoint, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		req.Header.Set("X-Api-Key", s.apiKey)
		resp, err := s.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return
			}
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	s.dropped.Add(int64(len(batch)))
}
