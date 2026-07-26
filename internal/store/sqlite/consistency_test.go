package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// TestFreeTextAgreesWithLiveTail runs the same free-text queries through both
// execution paths — the store's FTS5 index and the in-memory matcher live tail
// uses — and requires them to agree on every event. The two are meant to be
// interchangeable; when the matcher did a substring search instead of matching
// tokens, a term like "err" behaved completely differently in the tail view
// than in search results.
func TestFreeTextAgreesWithLiveTail(t *testing.T) {
	d := newTestDB(t)
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	events := []model.LogEvent{
		{Message: "connection refused by upstream", Service: "checkout-api", Source: "host-1"},
		{Message: "error while handling request", Service: "billing", Source: "host-2"},
		{Message: "request completed", Service: "checkout-api", Source: "host-3",
			Attributes: map[string]any{"status": 500, "region": "eu-west"}},
		{Message: "timeout contacting payments", Service: "billing", Source: "host-1"},
		{Raw: "raw unstructured erroneous text", Service: "legacy", Source: "host-9"},
	}
	for i := range events {
		events[i].Normalize(base.Add(time.Duration(i) * time.Second))
	}
	if err := d.Append(context.Background(), events); err != nil {
		t.Fatalf("Append: %v", err)
	}

	terms := []string{
		"connection", "refused", "connection refused", "refused connection",
		"error", "err", "erroneous", "request", "checkout", "api",
		"timeout", "500", "region", "eu", "west", "host", "nonexistent",
	}

	for _, term := range terms {
		q, err := query.Parse(term)
		if err != nil {
			t.Fatalf("Parse(%q): %v", term, err)
		}
		q.Normalize()

		res, err := d.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("Search(%q): %v", term, err)
		}
		fromSearch := map[string]bool{}
		for _, e := range res.Events {
			fromSearch[e.ID] = true
		}

		for _, e := range events {
			matched := q.Matches(e)
			if matched != fromSearch[e.ID] {
				t.Errorf("term %q: live tail matches=%v but search returned=%v for %q (service %q)",
					term, matched, fromSearch[e.ID], e.Message, e.Service)
			}
		}
	}
}
