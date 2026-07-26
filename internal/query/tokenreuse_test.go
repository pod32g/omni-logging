package query

import (
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

func matchBenchEvent() model.LogEvent {
	return model.LogEvent{
		Timestamp: time.Now(),
		Message:   "connection refused by upstream while handling request for user session token validation",
		Raw:       "2026-07-26T08:00:00Z ERROR connection refused by upstream id=abc123 retry=3",
		Service:   "checkout-api",
		Source:    "host-7",
		Attributes: map[string]any{
			"status": 500, "region": "eu-west", "user_id": 4242,
			"trace_id": "abcdef0123456789", "retry": 3, "path": "/v1/checkout/session",
		},
	}
}

// TestMatchesTokenizesOncePerCall pins the cost model of multi-term matching.
// Matches runs inside the tail hub's fan-out, which the ingest batch writer
// calls synchronously, so tokenizing per term rather than per event turned a
// zero-allocation filter into hundreds of allocations per event and slowed
// matching by more than an order of magnitude.
func TestMatchesTokenizesOncePerCall(t *testing.T) {
	e := matchBenchEvent()

	allocsFor := func(expr string) float64 {
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("Parse(%q): %v", expr, err)
		}
		q.Normalize()
		return testing.AllocsPerRun(200, func() { _ = q.Matches(e) })
	}

	one := allocsFor("connection")
	five := allocsFor("connection refused upstream request session")
	t.Logf("allocs/op: 1 term = %.0f, 5 terms = %.0f", one, five)

	// Tokenizing once per call means extra terms add only the small per-term
	// tokenization of the term itself, not another full pass over the event.
	// Re-tokenizing per term made this ratio track the term count (~5x).
	if five > one*2 {
		t.Fatalf("5-term match costs %.0f allocs vs %.0f for 1 term: the event is being re-tokenized per term", five, one)
	}
}

// TestMatchesAgreesWithSingleTermPath guards the refactor: the shared-token path
// used by Matches must return the same answers as the per-event helper.
func TestMatchesAgreesWithSingleTermPath(t *testing.T) {
	e := matchBenchEvent()
	for _, term := range []string{
		"connection", "refused", "upstream", "checkout", "api", "500",
		"eu", "west", "absent", "conn", "sessions",
	} {
		q, err := Parse(term)
		if err != nil {
			t.Fatal(err)
		}
		q.Normalize()
		if got, want := q.Matches(e), termMatches(e, term); got != want {
			t.Errorf("term %q: Matches=%v but termMatches=%v", term, got, want)
		}
	}

	// A multi-term query is the AND of its terms.
	q, err := Parse("connection refused checkout")
	if err != nil {
		t.Fatal(err)
	}
	q.Normalize()
	if !q.Matches(e) {
		t.Error("expected all three terms to match")
	}
	q2, _ := Parse("connection refused absent")
	q2.Normalize()
	if q2.Matches(e) {
		t.Error("a query containing a non-matching term must not match")
	}
}
