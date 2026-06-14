package model

import (
	"testing"
	"time"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want Level
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"trace", LevelDebug},
		{"info", LevelInfo},
		{"INFORMATION", LevelInfo},
		{"notice", LevelInfo},
		{"warn", LevelWarn},
		{"WARNING", LevelWarn},
		{"err", LevelError},
		{"error", LevelError},
		{"critical", LevelFatal},
		{"fatal", LevelFatal},
		{"panic", LevelFatal},
		{"0", LevelFatal}, // syslog emergency
		{"3", LevelError}, // syslog error
		{"4", LevelWarn},  // syslog warning
		{"6", LevelInfo},  // syslog info
		{"7", LevelDebug}, // syslog debug
		{"", LevelInfo},   // empty defaults to info
		{"weird", LevelInfo},
	}
	for _, c := range cases {
		if got := ParseLevel(c.in); got != c.want {
			t.Errorf("ParseLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLevelRankOrdering(t *testing.T) {
	if !(LevelDebug.Rank() < LevelInfo.Rank() &&
		LevelInfo.Rank() < LevelWarn.Rank() &&
		LevelWarn.Rank() < LevelError.Rank() &&
		LevelError.Rank() < LevelFatal.Rank()) {
		t.Fatalf("level ranks are not strictly increasing")
	}
}

func TestNewIDSortableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if len(id) != 26 {
			t.Fatalf("ULID length = %d, want 26 (%q)", len(id), id)
		}
		if seen[id] {
			t.Fatalf("duplicate ULID generated: %q", id)
		}
		seen[id] = true
	}

	// Later timestamps must sort lexicographically after earlier ones.
	early := newIDAt(time.Unix(1_000_000, 0))
	late := newIDAt(time.Unix(2_000_000, 0))
	if !(early < late) {
		t.Fatalf("expected %q < %q for time ordering", early, late)
	}
}

func TestEventFromJSON_StructuredAndAliases(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	data := []byte(`{
		"msg": "boom",
		"severity": "ERROR",
		"host": "node-1",
		"logger": "checkout-api",
		"time": "2026-06-14T11:59:00Z",
		"user_id": 42,
		"nested": {"k": "v"}
	}`)

	e, err := EventFromJSON(data, now)
	if err != nil {
		t.Fatalf("EventFromJSON error: %v", err)
	}
	if e.Message != "boom" {
		t.Errorf("Message = %q, want boom", e.Message)
	}
	if e.Level != LevelError {
		t.Errorf("Level = %q, want error", e.Level)
	}
	if e.Source != "node-1" {
		t.Errorf("Source = %q, want node-1", e.Source)
	}
	if e.Service != "checkout-api" {
		t.Errorf("Service = %q, want checkout-api", e.Service)
	}
	if !e.Timestamp.Equal(time.Date(2026, 6, 14, 11, 59, 0, 0, time.UTC)) {
		t.Errorf("Timestamp = %v, want 11:59:00Z", e.Timestamp)
	}
	if !e.ReceivedAt.Equal(now) {
		t.Errorf("ReceivedAt = %v, want %v", e.ReceivedAt, now)
	}
	if e.ID == "" {
		t.Error("ID was not assigned")
	}
	// Non-reserved keys must land in attributes.
	if got, ok := e.Attributes["user_id"]; !ok || got.(float64) != 42 {
		t.Errorf("Attributes[user_id] = %v, want 42", got)
	}
	if _, ok := e.Attributes["nested"]; !ok {
		t.Errorf("nested attribute missing: %#v", e.Attributes)
	}
}

func TestEventFromJSON_DefaultsAndNumericTime(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	// Numeric millisecond timestamp, missing level -> info default.
	e, err := EventFromJSON([]byte(`{"message":"hi","timestamp":1750000000000}`), now)
	if err != nil {
		t.Fatalf("EventFromJSON error: %v", err)
	}
	if e.Level != LevelInfo {
		t.Errorf("missing level should default to info, got %q", e.Level)
	}
	want := time.Unix(0, 1750000000000*1e6).UTC()
	if !e.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", e.Timestamp, want)
	}

	// No timestamp at all -> defaults to received_at (now).
	e2, _ := EventFromJSON([]byte(`{"message":"hi"}`), now)
	if !e2.Timestamp.Equal(now) {
		t.Errorf("missing timestamp should default to now, got %v", e2.Timestamp)
	}
}

func TestEventFromJSON_Invalid(t *testing.T) {
	now := time.Now()
	if _, err := EventFromJSON([]byte(`not json`), now); err == nil {
		t.Error("expected error for invalid json")
	}
	if _, err := EventFromJSON([]byte(`{"timestamp":"yesterday"}`), now); err == nil {
		t.Error("expected error for unparseable timestamp")
	}
}
