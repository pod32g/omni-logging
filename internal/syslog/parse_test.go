package syslog

import (
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

func TestParseRFC5424(t *testing.T) {
	// The example from RFC5424 §6.5, with structured data.
	line := `<34>1 2003-10-11T22:14:15.003Z mymachine.example.com su - ID47 [exampleSDID@32473 iut="3" eventSource="Apple"] 'su root' failed for lonvick`
	e := Parse(line, now)

	if e.Source != "mymachine.example.com" {
		t.Errorf("source = %q", e.Source)
	}
	if e.Service != "su" {
		t.Errorf("service = %q", e.Service)
	}
	if e.Message != "'su root' failed for lonvick" {
		t.Errorf("message = %q", e.Message)
	}
	// PRI 34 = facility 4 (auth), severity 2 (crit) -> fatal.
	if e.Level != model.LevelFatal {
		t.Errorf("level = %q, want fatal for severity 2", e.Level)
	}
	if e.Attributes["syslog_facility"] != "auth" || e.Attributes["syslog_severity"] != "crit" {
		t.Errorf("facility/severity attributes = %v / %v",
			e.Attributes["syslog_facility"], e.Attributes["syslog_severity"])
	}
	if e.Attributes["msgid"] != "ID47" {
		t.Errorf("msgid = %v", e.Attributes["msgid"])
	}
	if _, ok := e.Attributes["procid"]; ok {
		t.Error("a '-' procid must not become an attribute")
	}
	if got := e.Attributes["exampleSDID@32473.iut"]; got != "3" {
		t.Errorf("structured data iut = %v, want 3", got)
	}
	if got := e.Attributes["exampleSDID@32473.eventSource"]; got != "Apple" {
		t.Errorf("structured data eventSource = %v", got)
	}
	if !e.Timestamp.Equal(time.Date(2003, 10, 11, 22, 14, 15, 3e6, time.UTC)) {
		t.Errorf("timestamp = %s", e.Timestamp)
	}
}

func TestParseRFC5424StructuredDataEdgeCases(t *testing.T) {
	// A ']' and an escaped quote inside a value must not end the SD block, and
	// multiple elements must all be captured.
	line := `<14>1 2026-07-26T10:00:00Z host app 42 - [a x="brack]et" y="quo\"te"][b@1 z="1"] the message`
	e := Parse(line, now)

	if got := e.Attributes["a.x"]; got != "brack]et" {
		t.Errorf(`a.x = %v, want "brack]et" — a ']' inside a quoted value ended the block early`, got)
	}
	if got := e.Attributes["a.y"]; got != `quo"te` {
		t.Errorf("a.y = %v, want an unescaped quote", got)
	}
	if got := e.Attributes["b@1.z"]; got != "1" {
		t.Errorf("b@1.z = %v, want 1 (second element lost)", got)
	}
	if e.Message != "the message" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Attributes["procid"] != "42" {
		t.Errorf("procid = %v", e.Attributes["procid"])
	}
}

func TestParseRFC5424NilStructuredData(t *testing.T) {
	e := Parse(`<14>1 2026-07-26T10:00:00Z host app - - - just a message`, now)
	if e.Message != "just a message" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Source != "host" || e.Service != "app" {
		t.Errorf("source/service = %q / %q", e.Source, e.Service)
	}
}

func TestParseRFC5424StripsBOM(t *testing.T) {
	e := Parse("<14>1 2026-07-26T10:00:00Z host app - - - \ufeffwith a BOM", now)
	if e.Message != "with a BOM" {
		t.Errorf("message = %q, want the BOM stripped", e.Message)
	}
}

func TestParseRFC3164(t *testing.T) {
	// The classic BSD example, whose timestamp carries no year. The clock is set
	// just after the message date: syslog messages arrive close to when they are
	// generated, and TestParseRFC3164YearRollover covers the other case.
	october := time.Date(2026, 10, 11, 23, 0, 0, 0, time.UTC)
	e := Parse(`<34>Oct 11 22:14:15 mymachine su[1234]: 'su root' failed for lonvick`, october)

	if e.Source != "mymachine" {
		t.Errorf("source = %q", e.Source)
	}
	if e.Service != "su" {
		t.Errorf("service = %q, want the tag without its pid", e.Service)
	}
	if e.Attributes["procid"] != "1234" {
		t.Errorf("procid = %v, want it split out of the tag", e.Attributes["procid"])
	}
	if e.Message != "'su root' failed for lonvick" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Timestamp.Year() != october.Year() {
		t.Errorf("year = %d, want the receive year %d", e.Timestamp.Year(), october.Year())
	}
	if e.Timestamp.Month() != time.October || e.Timestamp.Day() != 11 {
		t.Errorf("timestamp = %s", e.Timestamp)
	}
}

// TestParseRFC3164YearRollover: a December message received in January belongs
// to the previous year, not eleven months in the future.
func TestParseRFC3164YearRollover(t *testing.T) {
	january := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	e := Parse(`<14>Dec 31 23:59:00 host app: end of year`, january)
	if e.Timestamp.Year() != 2025 {
		t.Fatalf("year = %d, want 2025 (a Dec message seen in Jan is from last year)", e.Timestamp.Year())
	}
}

func TestParseRFC3164NoTag(t *testing.T) {
	e := Parse(`<13>Jul 26 10:00:00 host a message with: a colon far into the text`, now)
	if e.Source != "host" {
		t.Errorf("source = %q", e.Source)
	}
	// "a message with" contains spaces, so it is not a tag.
	if e.Service != "" {
		t.Errorf("service = %q, want empty when there is no tag", e.Service)
	}
	if !strings.Contains(e.Message, "a colon") {
		t.Errorf("message = %q", e.Message)
	}
}

func TestParseSeverityMapping(t *testing.T) {
	// facility 1 (user) x 8 + severity.
	for _, tc := range []struct {
		pri   int
		level model.Level
		name  string
	}{
		{8, model.LevelFatal, "emerg"},
		{9, model.LevelFatal, "alert"},
		{10, model.LevelFatal, "crit"},
		{11, model.LevelError, "err"},
		{12, model.LevelWarn, "warning"},
		{13, model.LevelInfo, "notice"},
		{14, model.LevelInfo, "info"},
		{15, model.LevelDebug, "debug"},
	} {
		e := Parse("<"+itoa(tc.pri)+">Jul 26 10:00:00 host app: msg", now)
		if e.Level != tc.level {
			t.Errorf("pri %d: level = %q, want %q", tc.pri, e.Level, tc.level)
		}
		if e.Attributes["syslog_severity"] != tc.name {
			t.Errorf("pri %d: severity name = %v, want %q", tc.pri, e.Attributes["syslog_severity"], tc.name)
		}
		if e.Attributes["syslog_facility"] != "user" {
			t.Errorf("pri %d: facility = %v, want user", tc.pri, e.Attributes["syslog_facility"])
		}
	}
}

// TestParseMalformedIsKeptNotDropped: a logging system must not silently
// discard evidence just because it is misformatted.
func TestParseMalformedIsKeptNotDropped(t *testing.T) {
	for _, line := range []string{
		"this has no priority header at all",
		"<not-a-number> nonsense",
		"<999> priority out of range",
		"<14>",
	} {
		e := Parse(line, now)
		if e.Raw != line {
			t.Errorf("raw = %q, want the original line %q", e.Raw, line)
		}
		if e.Message == "" && e.Raw == "" {
			t.Errorf("line %q produced an empty event", line)
		}
		if e.ID == "" || e.Timestamp.IsZero() {
			t.Errorf("line %q was not normalized: %+v", line, e)
		}
	}
}

func TestParseKeepsRawAndNormalizes(t *testing.T) {
	line := `<14>1 2026-07-26T10:00:00Z host app - - - hello`
	e := Parse(line, now)
	if e.Raw != line {
		t.Errorf("raw not preserved: %q", e.Raw)
	}
	if e.ID == "" {
		t.Error("event was not assigned an ID")
	}
	if e.ReceivedAt.IsZero() {
		t.Error("received_at was not set")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
