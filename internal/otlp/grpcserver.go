package otlp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// GRPCServer runs the OTLP gRPC service on its own listener.
//
// It is a separate port rather than a route on the main server because gRPC
// requires HTTP/2, and the main server is HTTP/1.1 unless TLS is configured.
// Serving it here means cleartext gRPC works the way exporters expect it to,
// on the conventional port 4317, whether or not the UI is behind TLS.
type GRPCServer struct {
	addr string
	ln   net.Listener
	srv  *http.Server
	log  *slog.Logger

	mu      sync.Mutex
	started bool
	done    chan struct{}
}

// GRPCServerOptions configures a GRPCServer.
type GRPCServerOptions struct {
	GRPCOptions

	// Addr is the listen address, e.g. ":4317". Required.
	Addr string

	// TLSConfig serves gRPC over TLS. Nil serves h2c (cleartext HTTP/2), which
	// is what a local collector defaults to.
	TLSConfig *tls.Config
}

// NewGRPCServer builds a server. It does not listen until Start.
func NewGRPCServer(opts GRPCServerOptions) (*GRPCServer, error) {
	if opts.Addr == "" {
		return nil, errors.New("otlp/grpc: Addr is required")
	}
	if opts.Sink == nil {
		return nil, errors.New("otlp/grpc: Sink is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	h2 := &http2.Server{
		// A gRPC client holds one connection open and multiplexes calls on it,
		// so the idle timeout has to outlast the gaps between exports rather
		// than the exports themselves.
		IdleTimeout: 5 * time.Minute,
	}
	handler := GRPCHandler(opts.GRPCOptions)

	srv := &http.Server{
		Addr: opts.Addr,
		// h2c upgrades cleartext connections that arrive with HTTP/2 prior
		// knowledge, which is how every gRPC client opens an insecure channel.
		// Requests that are not HTTP/2 fall through to the handler, which
		// explains the problem rather than hanging.
		Handler:   h2c.NewHandler(handler, h2),
		TLSConfig: opts.TLSConfig,
		// No WriteTimeout: it would cut off a long-lived multiplexed connection
		// mid-call. ReadHeaderTimeout still protects against a slowloris.
		ReadHeaderTimeout: 20 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}
	if opts.TLSConfig != nil {
		// Advertise h2 so a TLS client negotiates HTTP/2 in the handshake
		// instead of falling back to HTTP/1.1, which cannot carry gRPC.
		if err := http2.ConfigureServer(srv, h2); err != nil {
			return nil, fmt.Errorf("otlp/grpc: configure http2: %w", err)
		}
	}

	return &GRPCServer{addr: opts.Addr, srv: srv, log: logger, done: make(chan struct{})}, nil
}

// Start binds the listener and serves in the background. Binding happens
// synchronously so a port conflict is reported to the caller rather than
// appearing in the log some time after startup claimed success.
func (s *GRPCServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("otlp/grpc: already started")
	}

	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("otlp/grpc: listen %s: %w", s.addr, err)
	}
	s.ln = ln
	s.started = true

	go func() {
		defer close(s.done)
		var serr error
		if s.srv.TLSConfig != nil {
			serr = s.srv.ServeTLS(ln, "", "") // certificates come from TLSConfig
		} else {
			serr = s.srv.Serve(ln)
		}
		if serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			s.log.Error("otlp/grpc: server stopped", "error", serr)
		}
	}()
	return nil
}

// Addr reports the address actually bound, which is how a test that asked for
// port 0 finds out what it got.
func (s *GRPCServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return s.addr
	}
	return s.ln.Addr().String()
}

// Stop shuts the server down, waiting briefly for in-flight calls.
func (s *GRPCServer) Stop() {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	<-s.done
}
