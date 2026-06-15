package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// benchSeed inserts n synthetic events spread across services/levels/time and
// returns the store. It establishes the M19 baseline dataset that later storage
// and performance milestones (M20+) are measured against.
func benchSeed(b *testing.B, n int) *DB {
	b.Helper()
	db, err := Open(":memory:")
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	services := []string{"checkout-api", "auth-svc", "worker", "gateway", "billing"}
	levels := model.Levels()
	base := time.Date(2026, 6, 14, 0, 0, 0, 0, time.UTC)

	const batchSize = 500
	batch := make([]model.LogEvent, 0, batchSize)
	for i := 0; i < n; i++ {
		e := model.LogEvent{
			ID:         model.NewID(),
			Timestamp:  base.Add(time.Duration(i) * time.Second),
			ReceivedAt: base.Add(time.Duration(i) * time.Second),
			Service:    services[i%len(services)],
			Source:     fmt.Sprintf("node-%d", i%8),
			Level:      levels[i%len(levels)],
			Message:    fmt.Sprintf("request %d to upstream returned status %d for user", i, 200+(i%5)*100),
			Attributes: map[string]any{"user_id": float64(i % 1000), "status": float64(200 + (i%5)*100)},
		}
		batch = append(batch, e)
		if len(batch) == batchSize {
			if err := db.Append(context.Background(), batch); err != nil {
				b.Fatalf("Append: %v", err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if err := db.Append(context.Background(), batch); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
	return db
}

func BenchmarkAppend(b *testing.B) {
	db, err := Open(":memory:")
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { db.Close() })

	const batchSize = 100
	mkBatch := func(off int) []model.LogEvent {
		batch := make([]model.LogEvent, batchSize)
		for j := range batch {
			batch[j] = model.LogEvent{
				ID:        model.NewID(),
				Timestamp: time.Unix(int64(off+j), 0),
				Service:   "bench", Level: model.LevelInfo,
				Message:    fmt.Sprintf("event %d processed in pipeline", off+j),
				Attributes: map[string]any{"n": float64(off + j)},
			}
		}
		return batch
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := db.Append(context.Background(), mkBatch(i*batchSize)); err != nil {
			b.Fatalf("Append: %v", err)
		}
	}
	b.ReportMetric(float64(batchSize), "events/op")
}

func benchSearch(b *testing.B, expr string) {
	db := benchSeed(b, 20000)
	q, err := query.Parse(expr)
	if err != nil {
		b.Fatalf("Parse: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Search(context.Background(), q); err != nil {
			b.Fatalf("Search: %v", err)
		}
	}
}

func BenchmarkSearchLevelFilter(b *testing.B) { benchSearch(b, "level=error") }
func BenchmarkSearchFreeText(b *testing.B)    { benchSearch(b, "upstream") }
func BenchmarkSearchAttr(b *testing.B)        { benchSearch(b, "attr.status=500") }

func BenchmarkStats(b *testing.B) {
	db := benchSeed(b, 20000)
	q, _ := query.Parse("")
	q.Interval = time.Hour
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Stats(context.Background(), q); err != nil {
			b.Fatalf("Stats: %v", err)
		}
	}
}
