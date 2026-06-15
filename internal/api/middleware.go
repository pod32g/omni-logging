package api

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// metricsMiddleware records per-request count and duration, labeled by method
// and response status code only (no path label, to bound cardinality).
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		code := strconv.Itoa(rec.status)
		s.httpReqs.With(r.Method, code).Inc()
		s.httpDur.With(r.Method, code).Observe(time.Since(start).Seconds())
	})
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
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

// requireIngestKey guards ingest endpoints. When no keys are configured, auth is
// disabled (dev mode). The key is read from X-Api-Key or a Bearer token.
func (s *Server) requireIngestKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.IngestKeys) == 0 {
			next(w, r)
			return
		}
		provided := r.Header.Get("X-Api-Key")
		if provided == "" {
			provided = bearer(r)
		}
		for _, k := range s.cfg.IngestKeys {
			if constantTimeEqual(provided, k) {
				next(w, r)
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
