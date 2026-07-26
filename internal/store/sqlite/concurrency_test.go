package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// TestStreamDoesNotBlockAppend pins the reason reads and writes use separate
// pools. An export streams to the client at the client's pace, so it holds its
// database connection for as long as the download takes; when that connection
// was the only one, ingestion stalled behind every export.
func TestStreamDoesNotBlockAppend(t *testing.T) {
	d, err := Open(t.TempDir() + "/concurrency.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	now := time.Now()
	seed := make([]model.LogEvent, 0, 200)
	for i := 0; i < 200; i++ {
		e := model.LogEvent{Message: "seeded event", Service: "svc"}
		e.Normalize(now)
		seed = append(seed, e)
	}
	if err := d.Append(context.Background(), seed); err != nil {
		t.Fatalf("seed Append: %v", err)
	}

	var (
		started  = make(chan struct{})
		appendIn = make(chan time.Duration, 1)
	)
	go func() {
		<-started
		e := model.LogEvent{Message: "written during export"}
		e.Normalize(time.Now())
		t0 := time.Now()
		if err := d.Append(context.Background(), []model.LogEvent{e}); err != nil {
			t.Errorf("Append during export: %v", err)
		}
		appendIn <- time.Since(t0)
	}()

	// A deliberately slow consumer: 200 rows x 5ms is ~1s of held connection.
	n := 0
	err = d.Stream(context.Background(), query.Query{}, func(model.LogEvent) error {
		if n == 0 {
			close(started)
		}
		n++
		time.Sleep(5 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	waited := <-appendIn
	if waited > 200*time.Millisecond {
		t.Fatalf("ingest Append waited %v for a %d-row export to finish; reads and writes are sharing a connection again", waited, n)
	}
}

// TestSearchTotalIsCapped covers the early-stopping count: past MaxExactCount a
// search must report the cap and flag it rather than walk the whole match set.
func TestSearchTotalIsCapped(t *testing.T) {
	d := newTestDB(t)
	seedCount(t, d, 25)

	res, err := d.Search(context.Background(), query.Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Total != 25 || res.TotalCapped {
		t.Fatalf("small result set: total=%d capped=%v, want 25/false", res.Total, res.TotalCapped)
	}

	// Rather than insert 50k rows, verify the cap arithmetic directly against
	// the same statement Search uses.
	sqlStr, args := countSQL(query.Query{})
	args[len(args)-1] = 10 // stand in for MaxExactCount+1
	var raw int64
	if err := d.ro.QueryRowContext(context.Background(), sqlStr, args...).Scan(&raw); err != nil {
		t.Fatalf("capped count query: %v", err)
	}
	if raw != 10 {
		t.Fatalf("count did not stop at the cap: got %d, want 10", raw)
	}
}

func seedCount(t *testing.T, d *DB, n int) {
	t.Helper()
	now := time.Now()
	events := make([]model.LogEvent, 0, n)
	for i := 0; i < n; i++ {
		e := model.LogEvent{Message: "event", Service: "svc"}
		e.Normalize(now.Add(time.Duration(i) * time.Millisecond))
		events = append(events, e)
	}
	if err := d.Append(context.Background(), events); err != nil {
		t.Fatalf("Append: %v", err)
	}
}
