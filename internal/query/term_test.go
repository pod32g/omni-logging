package query

import (
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// TestTermMatchesTokens covers free-text matching in live tail. It has to agree
// with the store, which evaluates terms through FTS5 as whole tokens — a
// substring search would stream every "error" event for the query "err" while
// /search returned nothing for it.
func TestTermMatchesTokens(t *testing.T) {
	e := model.LogEvent{
		Message:    "connection refused by upstream",
		Service:    "checkout-api",
		Source:     "host-7",
		Attributes: map[string]any{"user_id": 42, "region": "eu-west"},
	}

	for _, tc := range []struct {
		term string
		want bool
		why  string
	}{
		{"connection", true, "whole token in the message"},
		{"REFUSED", true, "matching is case-insensitive"},
		{"connection refused", true, "adjacent tokens form a phrase"},
		{"refused connection", false, "phrase order matters, as in FTS5"},
		{"checkout", true, "service is part of the indexed text"},
		{"api", true, "punctuation splits checkout-api into two tokens"},
		{"42", true, "attribute values are indexed"},
		{"region", true, "attribute keys are indexed"},
		{"conn", true, "a partly-typed word matches as a prefix"},
		{"upstrea", true, "prefix matching mirrors the trailing * on the FTS phrase"},
		{"check", true, "prefix works across the token split in checkout-api"},
		{"err", false, "a prefix must still match from the start of a token"},
		{"nnection", false, "a mid-token substring is not a match"},
		{"connection refu", true, "in a phrase, only the final token is a prefix"},
		{"conn refused", false, "an earlier token of a phrase must match in full"},
		{"absent", false, "unrelated term"},
		{"  ", false, "a term with no tokens matches nothing"},
	} {
		if got := termMatches(e, tc.term); got != tc.want {
			t.Errorf("termMatches(%q) = %v, want %v (%s)", tc.term, got, tc.want, tc.why)
		}
	}
}

func TestParseRelativeRejectsOverflow(t *testing.T) {
	// A duration is a 64-bit nanosecond count; unchecked multiplication here
	// wraps around and produces a negative window.
	for _, s := range []string{"999999999999d", "99999999999999999h", "9223372036854775807s"} {
		if d, err := ParseRelative(s); err == nil {
			t.Errorf("ParseRelative(%q) = %v, want an error", s, d)
		}
	}
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"15m", 15 * time.Minute},
		{"6h", 6 * time.Hour},
		{"7d", 7 * 24 * time.Hour},
		{"0m", 0},
	} {
		got, err := ParseRelative(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseRelative(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

// TestBuildRejectsOverflowingWindow makes sure the clamp is reachable from the
// request path, not just the parser.
func TestBuildRejectsOverflowingWindow(t *testing.T) {
	_, err := Params{Last: "999999999999d"}.Build(time.Now())
	if err == nil {
		t.Fatal("Build accepted an overflowing relative window")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v, want it to explain the limit", err)
	}
}
