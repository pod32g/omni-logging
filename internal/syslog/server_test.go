package syslog

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// collector is a Sink that records what the server produced.
type collector struct {
	mu     sync.Mutex
	events []model.LogEvent
	refuse bool
}

func (c *collector) sink(e model.LogEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse {
		return false
	}
	c.events = append(c.events, e)
	return true
}

func (c *collector) snapshot() []model.LogEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]model.LogEvent(nil), c.events...)
}

// waitFor polls until the collector holds n events or the deadline passes.
func (c *collector) waitFor(t *testing.T, n int) []model.LogEvent {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := c.snapshot()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d events, got %d: %+v", n, len(got), got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func startServer(t *testing.T, c *collector) *Server {
	t.Helper()
	s, err := New(Options{
		UDPAddr: "127.0.0.1:0",
		TCPAddr: "127.0.0.1:0",
		Sink:    c.sink,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Stop)
	return s
}

func TestServerUDP(t *testing.T) {
	c := &collector{}
	s := startServer(t, c)
	udpAddr, _ := s.Addrs()

	conn, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`<34>1 2026-07-26T10:00:00Z web nginx - - - upstream timed out`)); err != nil {
		t.Fatal(err)
	}

	got := c.waitFor(t, 1)
	if got[0].Service != "nginx" || got[0].Source != "web" {
		t.Errorf("parsed event = %+v", got[0])
	}
	if got[0].Message != "upstream timed out" {
		t.Errorf("message = %q", got[0].Message)
	}
}

// TestServerTCPNewlineFramed covers the framing most senders use.
func TestServerTCPNewlineFramed(t *testing.T) {
	c := &collector{}
	s := startServer(t, c)
	_, tcpAddr := s.Addrs()

	conn, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < 3; i++ {
		fmt.Fprintf(conn, "<14>Jul 26 10:00:0%d host app: message %d\n", i, i)
	}

	got := c.waitFor(t, 3)
	for i, e := range got[:3] {
		if e.Service != "app" {
			t.Errorf("event %d service = %q", i, e.Service)
		}
		if want := fmt.Sprintf("message %d", i); e.Message != want {
			t.Errorf("event %d message = %q, want %q", i, e.Message, want)
		}
	}
}

// TestServerTCPOctetCounted covers RFC6587's length-prefixed framing, which
// rsyslog and several appliances use and which newline-splitting would mangle
// for any message containing an embedded newline.
func TestServerTCPOctetCounted(t *testing.T) {
	c := &collector{}
	s := startServer(t, c)
	_, tcpAddr := s.Addrs()

	conn, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for _, msg := range []string{
		`<14>1 2026-07-26T10:00:00Z host app - - - first`,
		`<14>1 2026-07-26T10:00:01Z host app - - - second with
an embedded newline`,
	} {
		fmt.Fprintf(conn, "%d %s", len(msg), msg)
	}

	got := c.waitFor(t, 2)
	if got[0].Message != "first" {
		t.Errorf("first message = %q", got[0].Message)
	}
	if got[1].Message != "second with\nan embedded newline" {
		t.Errorf("second message = %q, want the embedded newline preserved", got[1].Message)
	}
}

// TestServerCountsDrops: when the ingest buffer refuses an event the collector
// must account for it rather than lose it silently.
func TestServerCountsDrops(t *testing.T) {
	c := &collector{refuse: true}
	s := startServer(t, c)
	udpAddr, _ := s.Addrs()

	conn, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`<14>Jul 26 10:00:00 host app: dropped`)); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for s.Metrics().Dropped == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("drop was not counted: %+v", s.Metrics())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if m := s.Metrics(); m.Received != 1 {
		t.Errorf("received = %d, want 1", m.Received)
	}
}

// TestServerStopIsIdempotentAndUnblocks: Stop must be safe to call twice and
// must not hang waiting on an open connection.
func TestServerStopIsIdempotentAndUnblocks(t *testing.T) {
	c := &collector{}
	s, err := New(Options{
		UDPAddr: "127.0.0.1:0",
		TCPAddr: "127.0.0.1:0",
		Sink:    c.sink,
		Now:     func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	_, tcpAddr := s.Addrs()

	conn, err := net.Dial("tcp", tcpAddr) // deliberately left open
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	done := make(chan struct{})
	go func() { s.Stop(); s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung with a connection still open")
	}
}

// TestServerRequiresSinkAndAddress guards the constructor's contract.
func TestServerRequiresSinkAndAddress(t *testing.T) {
	if _, err := New(Options{UDPAddr: "127.0.0.1:0"}); err == nil {
		t.Error("expected an error without a Sink")
	}
	if _, err := New(Options{Sink: func(model.LogEvent) bool { return true }}); err == nil {
		t.Error("expected an error when neither listener is configured")
	}
}

// TestServerPortConflictIsReported: binding failures must surface to the
// caller, not vanish into a goroutine.
func TestServerPortConflictIsReported(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	s, err := New(Options{
		TCPAddr: busy.Addr().String(),
		Sink:    func(model.LogEvent) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		s.Stop()
		t.Fatal("Start succeeded on an address already in use")
	}
}
