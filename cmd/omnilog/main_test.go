package main

import (
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
}
