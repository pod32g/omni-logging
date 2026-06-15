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
	Events []model.LogEvent `json:"events"`
	Count  int              `json:"count"` // number of events returned
	Total  int64            `json:"total"` // total matches ignoring the limit
	TookMs int64            `json:"took_ms"`
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

// Store persists and queries log events.
type Store interface {
	// Append durably writes a batch of events (structured row + full-text index).
	Append(ctx context.Context, events []model.LogEvent) error
	// Search returns matching events ordered per the query, plus the total count.
	Search(ctx context.Context, q query.Query) (SearchResult, error)
	// Stats returns the histogram and facets for a query.
	Stats(ctx context.Context, q query.Query) (StatsResult, error)
	// Purge deletes events older than the cutoff and returns how many were removed.
	Purge(ctx context.Context, olderThan time.Time) (int64, error)
	// Ping verifies the backend is reachable; it powers the readiness probe.
	Ping(ctx context.Context) error
	// Close releases resources.
	Close() error
}
