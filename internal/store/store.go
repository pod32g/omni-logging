// Package store defines the persistence interface for log events and the
// result types returned by search and aggregation. The interface lets the rest
// of the system stay decoupled from the concrete backend (SQLite today; a
// columnar store could be added later).
package store

import (
	"context"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// SearchResult is the outcome of a Search: the (limited) matching events plus
// the total number of matches and how long the query took.
type SearchResult struct {
	Events     []model.LogEvent `json:"events"`
	Count      int              `json:"count"` // number of events returned
	Total      int64            `json:"total"` // total matches ignoring the limit
	TookMs     int64            `json:"took_ms"`
	NextCursor string           `json:"next_cursor,omitempty"` // keyset cursor for the next page (empty = no more)
}

// Bucket is a single histogram column: a count of events in [Start, Start+width).
type Bucket struct {
	Start time.Time `json:"start"`
	Count int64     `json:"count"`
}

// Facet is a value/count pair within a faceted field (e.g. level=error: 1284).
type Facet struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// StatsResult powers the UI's histogram and facet sidebar.
type StatsResult struct {
	Histogram []Bucket           `json:"histogram"`
	Facets    map[string][]Facet `json:"facets"` // keyed by "level", "service"
	Total     int64              `json:"total"`
	TookMs    int64              `json:"took_ms"`
}

// Store persists and queries log events. Implementations must honor the
// contract below (enforced by the backend-agnostic suite in
// internal/store/storetest) so the rest of the system can treat any backend
// identically. All methods must respect context cancellation.
type Store interface {
	// Append durably writes a batch of events (structured row + full-text index).
	// It is idempotent per LogEvent.ID: re-appending an event with an existing ID
	// replaces it everywhere it is indexed (structured row AND full-text index),
	// never creating a duplicate. Crash recovery (the ingest WAL replay) relies on
	// this. An empty batch is a no-op.
	Append(ctx context.Context, events []model.LogEvent) error
	// Search returns matching events plus counts. Results are ordered newest-first
	// by event time (ties broken by ID) unless q.Order == query.OrderOldest, and
	// capped at q.Limit. SearchResult.Count is the number returned; Total is the
	// number of matches ignoring the limit. When q sets a keyset cursor
	// (AfterTS/AfterID) results continue after it; NextCursor carries the cursor
	// for the following page.
	Search(ctx context.Context, q query.Query) (SearchResult, error)
	// Stream invokes fn for every event matching q, in the query's sort order,
	// without buffering the whole result set (powering exports decoupled from the
	// search limit). q.Limit is ignored; q.From/To/filters/cursor are honored.
	Stream(ctx context.Context, q query.Query, fn func(model.LogEvent) error) error
	// Stats returns the time-bucketed histogram (bucket width q.Interval) and the
	// level/service facets for the events matching q.
	Stats(ctx context.Context, q query.Query) (StatsResult, error)
	// Purge deletes events with event time strictly older than olderThan
	// (including their full-text entries) and returns how many were removed.
	Purge(ctx context.Context, olderThan time.Time) (int64, error)
	// Ping verifies the backend is reachable; it powers the readiness probe.
	Ping(ctx context.Context) error
	// Close releases resources.
	Close() error
}
