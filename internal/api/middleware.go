package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/model"
)

type ctxKey int

const requestIDKey ctxKey = 0

// requestIDMiddleware assigns each request a request ID (honoring an inbound
// X-Request-Id), echoes it back, and threads it through the context so logs can
// correlate to a single request.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = model.NewID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func requestIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// metricsMiddleware records per-request count and duration, labeled by method
// and response status code only (no path label, to bound cardinality). The
// method is normalized to a fixed allowlist so an attacker cannot grow the
// series set unboundedly by sending arbitrary HTTP methods (these endpoints are
// unauthenticated by design). Recording runs in a defer so a panicking handler
// is still counted (as a 500) before the panic propagates to recoverMiddleware.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		method := normalizeMethod(r.Method)
		defer func() {
			p := recover()
			status := rec.status
			if p != nil {
				status = http.StatusInternalServerError
			}
			code := strconv.Itoa(status)
			s.httpReqs.With(method, code).Inc()
			s.httpDur.With(method, code).Observe(time.Since(start).Seconds())
			if p != nil {
				panic(p) // let recoverMiddleware turn it into the 500 response
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// normalizeMethod collapses any method outside the standard HTTP set to "other"
// to keep the metric label cardinality bounded.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodConnect, http.MethodTrace:
		return m
	default:
		return "other"
	}
}

// recoverMiddleware turns panics into 500s and logs them instead of crashing
// the server.
func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic in handler", "error", rec, "path", r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Flush propagates flushing so SSE streaming keeps working through the wrapper.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logMiddleware logs one line per request with method, path, status, duration.
func logMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"request_id", requestIDFromCtx(r.Context()),
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

// requireIngestKey guards ingest endpoints. When no keys are configured, auth is
// disabled (dev mode). The key is read from X-Api-Key or a Bearer token.
func (s *Server) requireIngestKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keys := s.cfg.IngestKeys
		if s.settings != nil {
			keys = s.settings.IngestKeys() // live: reflects edits without restart
		}
		if len(keys) == 0 {
			next(w, r)
			return
		}
		provided := r.Header.Get("X-Api-Key")
		if provided == "" {
			provided = bearer(r)
		}
		for _, k := range keys {
			if constantTimeEqual(provided, k) {
				next(w, r.WithContext(ingest.WithIngestKey(r.Context(), k)))
				return
			}
		}
		unauthorized(w)
	}
}

// requireAdmin guards query/tail endpoints with the admin token. When no token
// is configured, auth is disabled (dev mode). The token may come from a Bearer
// header, a "token" query parameter (needed for EventSource), or the
// omnilog_token cookie.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			next(w, r)
			return
		}
		provided := bearer(r)
		if provided == "" {
			provided = r.URL.Query().Get("token")
		}
		if provided == "" {
			if c, err := r.Cookie("omnilog_token"); err == nil {
				provided = c.Value
			}
		}
		if constantTimeEqual(provided, s.cfg.AdminToken) {
			next(w, r)
			return
		}
		unauthorized(w)
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
