package model

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maxFutureTimestampSkew = 24 * time.Hour

// LogEvent is the canonical, normalized representation of a single log record
// once it has been accepted by the system.
type LogEvent struct {
	ID         string         `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`   // event time (client-supplied or received_at)
	ReceivedAt time.Time      `json:"received_at"` // server receipt time
	Source     string         `json:"source"`      // host / origin
	Service    string         `json:"service"`     // logical service name
	Level      Level          `json:"level"`
	Message    string         `json:"message"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Raw        string         `json:"raw,omitempty"` // original payload when unstructured
}

// reserved holds the JSON keys that map onto first-class LogEvent fields.
// Any other key in an incoming object folds into Attributes.
var reserved = map[string]bool{
	"id": true, "timestamp": true, "time": true, "ts": true, "@timestamp": true,
	"received_at": true, "source": true, "host": true, "hostname": true,
	"service": true, "logger": true, "level": true, "severity": true, "lvl": true,
	"message": true, "msg": true, "raw": true, "attributes": true,
}

// Normalize fills in derived/default fields on a LogEvent that was built from
// untrusted input: it assigns an ID, defaults timestamps, and normalizes the
// level. now is injected for testability.
func (e *LogEvent) Normalize(now time.Time) {
	if e.ID == "" {
		e.ID = newIDAt(now)
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = now
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = e.ReceivedAt
	}
	if !e.Level.Valid() {
		e.Level = ParseLevel(string(e.Level))
	}
}

// EventFromJSON parses one JSON log object into a normalized LogEvent. Known
// keys (with common aliases) map onto first-class fields; everything else is
// collected into Attributes. now is used to default missing timestamps and to
// generate the ID.
func EventFromJSON(data []byte, now time.Time) (LogEvent, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return LogEvent{}, fmt.Errorf("invalid json object: %w", err)
	}

	e := LogEvent{Attributes: map[string]any{}}

	get := func(keys ...string) (json.RawMessage, bool) {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				return v, true
			}
		}
		return nil, false
	}

	// Canonical IDs are always assigned by the server. The store uses IDs for
	// WAL replay idempotency, so accepting caller-controlled IDs here would let a
	// producer replace an existing historical event.
	if v, ok := get("source", "host", "hostname"); ok {
		e.Source = jsonToString(v)
	}
	if v, ok := get("service", "logger"); ok {
		e.Service = jsonToString(v)
	}
	if v, ok := get("level", "severity", "lvl"); ok {
		e.Level = ParseLevel(jsonToString(v))
	}
	if v, ok := get("message", "msg"); ok {
		e.Message = jsonToString(v)
	}
	if v, ok := get("raw"); ok {
		e.Raw = jsonToString(v)
	}
	if v, ok := get("timestamp", "time", "ts", "@timestamp"); ok {
		t, err := parseTimestamp(v)
		if err != nil {
			return LogEvent{}, fmt.Errorf("invalid timestamp: %w", err)
		}
		if t.After(now.Add(maxFutureTimestampSkew)) {
			return LogEvent{}, fmt.Errorf("invalid timestamp: more than %s in the future", maxFutureTimestampSkew)
		}
		e.Timestamp = t
	}

	// An explicit "attributes" object merges in first.
	if v, ok := raw["attributes"]; ok {
		var attrs map[string]any
		if err := json.Unmarshal(v, &attrs); err == nil {
			for k, val := range attrs {
				e.Attributes[k] = val
			}
		}
	}
	// Any remaining non-reserved top-level keys become attributes too.
	for k, v := range raw {
		if reserved[k] {
			continue
		}
		var val any
		if err := json.Unmarshal(v, &val); err == nil {
			e.Attributes[k] = val
		}
	}
	if len(e.Attributes) == 0 {
		e.Attributes = nil
	}

	e.Normalize(now)
	return e, nil
}

// jsonToString renders a JSON value as a plain string: JSON strings are
// unquoted, everything else keeps its compact JSON form.
func jsonToString(v json.RawMessage) string {
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(v))
}

// parseTimestamp accepts RFC3339 strings or numeric unix time in
// seconds / milliseconds / nanoseconds (auto-detected by magnitude).
func parseTimestamp(v json.RawMessage) (time.Time, error) {
	// String form first.
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return time.Time{}, nil
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC(), nil
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return unixAuto(n), nil
		}
		return time.Time{}, fmt.Errorf("unrecognized time format %q", s)
	}
	// Numeric form.
	var n float64
	if err := json.Unmarshal(v, &n); err == nil {
		return unixAuto(int64(n)), nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time value")
}

// unixAuto interprets an integer as seconds, milliseconds, or nanoseconds
// based on its magnitude (a heuristic that works for any plausible recent date).
func unixAuto(n int64) time.Time {
	switch {
	case n >= 1e18: // nanoseconds
		return time.Unix(0, n).UTC()
	case n >= 1e15: // microseconds
		return time.Unix(0, n*1e3).UTC()
	case n >= 1e12: // milliseconds
		return time.Unix(0, n*1e6).UTC()
	default: // seconds
		return time.Unix(n, 0).UTC()
	}
}
