package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
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

const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// hstsValue is sent only when TLS is on AND hsts is explicitly enabled in the
// config. It stays opt-in on purpose: HSTS is sticky in browsers, so turning it
// on by default would lock a plain-HTTP fallback out of an already-visited
// origin — a bad trade for a self-hosted deployment.
const hstsValue = "max-age=31536000; includeSubDomains"

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	hsts := s.cfg.HSTS && s.cfg.TLSEnabled()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		if hsts {
			w.Header().Set("Strict-Transport-Security", hstsValue)
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// metricsMiddleware records per-request count and duration, labeled by method
// and response status code only (no path label, to bound cardinality). The
// method is normalized to a fixed allowlist so an attacker cannot grow the
// series set unboundedly by sending arbitrary HTTP methods (these endpoints are
// unauthenticated by design). Recording runs in a defer so a request that
// panics past this layer is still counted.
//
// Recovery sits INSIDE this middleware, so an ordinary handler panic has already
// been converted to a 500 by the time the status is read here. The one panic
// that still reaches this defer is http.ErrAbortHandler, raised deliberately by
// the export path to cut a response short; that request already sent its status
// line, so it is recorded as the status actually sent rather than as a 500.
func (s *Server) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		method := normalizeMethod(r.Method)
		defer func() {
			p := recover()
			status := rec.status
			if p != nil && p != http.ErrAbortHandler {
				status = http.StatusInternalServerError
			}
			code := strconv.Itoa(status)
			s.httpReqs.With(method, code).Inc()
			s.httpDur.With(method, code).Observe(time.Since(start).Seconds())
			if p != nil {
				panic(p) // ErrAbortHandler must reach net/http to abort the response
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
// the server. http.ErrAbortHandler is passed through untouched: it is not a
// bug report but a handler deliberately aborting the connection (see the
// export path), and net/http gives it the silent-close treatment it wants.
func recoverMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler {
				panic(rec)
			}
			logger.Error("panic in handler", "error", rec, "path", r.URL.Path)
			http.Error(w, "internal server error", http.StatusInternalServerError)
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

// probePaths are hit on a fixed schedule by orchestrators and container health
// checks. Logging them at info drowns real traffic, so they drop to debug.
var probePaths = map[string]bool{"/api/v1/healthz": true, "/api/v1/readyz": true}

// logMiddleware logs one line per request with method, path, status, duration.
// The line is emitted from a defer so a request that panics past this point is
// still logged (with the status the recovery layer set, or 500).
func logMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			level := slog.LevelInfo
			if probePaths[r.URL.Path] {
				level = slog.LevelDebug
			}
			logger.Log(r.Context(), level, "request",
				"request_id", requestIDFromCtx(r.Context()),
				"method", r.Method, "path", r.URL.Path,
				"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rec, r)
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
