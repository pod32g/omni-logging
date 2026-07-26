package syslog_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/ingest"
	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store/sqlite"
	"github.com/pod32g/omni-logging/internal/syslog"
	"github.com/pod32g/omni-logging/internal/tail"
)

// TestSyslogEndToEnd drives a message all the way from the wire to a search
// result. Syslog deliberately reuses the Ingestor rather than writing to the
// store directly, so it inherits the WAL, batching, retention and live tail —
// this test is what pins that wiring.
func TestSyslogEndToEnd(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	hub := tail.NewHub()
	ing := ingest.New(db, hub, ingest.Options{FlushInterval: 10 * time.Millisecond})
	ing.Start()
	t.Cleanup(ing.Stop)

	// A live-tail subscriber must see syslog traffic like any other event.
	q, _ := query.Parse("")
	q.Normalize()
	sub := hub.Subscribe(q, 16)
	t.Cleanup(sub.Close)

	srv, err := syslog.New(syslog.Options{
		UDPAddr: "127.0.0.1:0",
		TCPAddr: "127.0.0.1:0",
		Sink:    ing.Enqueue,
	})
	if err != nil {
		t.Fatalf("New collector: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start collector: %v", err)
	}
	t.Cleanup(srv.Stop)
	udpAddr, tcpAddr := srv.Addrs()

	// One message over each transport.
	uc, err := net.Dial("udp", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer uc.Close()
	fmt.Fprint(uc, `<11>1 2026-07-26T10:00:00Z web nginx - - - upstream connection refused`)

	tc, err := net.Dial("tcp", tcpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	fmt.Fprint(tc, "<14>Jul 26 10:00:01 db postgres[99]: checkpoint complete\n")

	// Wait for both to land in the store.
	deadline := time.Now().Add(5 * time.Second)
	var total int64
	for {
		all, _ := query.Parse("")
		all.Normalize()
		res, serr := db.Search(context.Background(), all)
		if serr != nil {
			t.Fatalf("Search: %v", serr)
		}
		total = res.Total
		if total >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d events reached the store", total)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The severity must survive as a searchable level, and the app-name as service.
	errq, _ := query.Parse("level=error service=nginx")
	errq.Normalize()
	res, err := db.Search(context.Background(), errq)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 1 {
		t.Fatalf("level=error service=nginx matched %d, want 1", res.Total)
	}
	if got := res.Events[0].Attributes["syslog_facility"]; got != "user" {
		t.Errorf("facility attribute = %v, want user", got)
	}

	// Free-text search must reach the message body through the FTS index.
	ftq, _ := query.Parse("checkpoint")
	ftq.Normalize()
	res, err = db.Search(context.Background(), ftq)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 1 || res.Events[0].Service != "postgres" {
		t.Fatalf("free-text search for a syslog message returned %+v", res.Events)
	}

	// And the live-tail hub must have been fed too.
	select {
	case e := <-sub.C:
		if e.Message == "" {
			t.Error("live tail received an empty syslog event")
		}
	case <-time.After(2 * time.Second):
		t.Error("syslog events never reached the live-tail hub")
	}
}
