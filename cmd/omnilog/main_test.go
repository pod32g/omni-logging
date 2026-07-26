package main

import (
	"crypto/tls"
	"net/http"
	"testing"
	"time"
)

func TestNewHTTPServerHasResourceLimits(t *testing.T) {
	srv := newHTTPServer(":8080", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 5*time.Second || srv.ReadTimeout != 30*time.Second || srv.IdleTimeout != 60*time.Second {
		t.Fatalf("unexpected timeouts: header=%s read=%s idle=%s", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 64<<10 {
		t.Fatalf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 64<<10)
	}
	// Stated rather than inherited, so a toolchain default cannot lower it.
	if srv.TLSConfig == nil || srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS MinVersion is not pinned to 1.2: %+v", srv.TLSConfig)
	}
	// WriteTimeout must stay unset: exports and live tail stream for a long time.
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %s, want unset so streaming responses are not cut off", srv.WriteTimeout)
	}
}
