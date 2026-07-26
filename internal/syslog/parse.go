// Package syslog parses RFC5424 and RFC3164 syslog messages into the canonical
// LogEvent, and serves them over UDP and TCP. It exists so network gear, Linux
// daemons, and containers can ship logs with no agent and no application
// changes — a Docker service only needs `logging: driver: syslog`.
package syslog

import (
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// facilityNames maps the syslog facility number to its conventional name. The
// name is far more useful as a searchable attribute than the raw number.
var facilityNames = [...]string{
	"kern", "user", "mail", "daemon", "auth", "syslog", "lpr", "news",
	"uucp", "cron", "authpriv", "ftp", "ntp", "audit", "alert", "clock",
	"local0", "local1", "local2", "local3", "local4", "local5", "local6", "local7",
}

// severityNames maps the syslog severity number to its conventional name.
var severityNames = [...]string{
	"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug",
}

// nilValue is RFC5424's explicit "no value" marker.
const nilValue = "-"

// Parse turns one syslog message into a LogEvent, accepting either RFC5424 or
// RFC3164 framing. now supplies the receive time and the year for RFC3164
// timestamps, which carry no year of their own.
//
// A message that cannot be parsed is not discarded: it becomes an event whose
// Message is the raw line, because a malformed log line is still evidence and
// dropping it silently is the worst possible outcome for a logging system.
func Parse(line string, now time.Time) model.LogEvent {
	line = strings.TrimRight(line, "\r\n\x00")
	e := model.LogEvent{Raw: line}

	pri, rest, ok := parsePRI(line)
	if !ok {
		// No priority header at all: keep the line as an unstructured message.
		e.Message = strings.TrimSpace(line)
		e.Normalize(now)
		return e
	}

	facility, severity := pri/8, pri%8
	e.Attributes = map[string]any{
		"syslog_facility": facilityName(facility),
		"syslog_severity": severityName(severity),
	}
	e.Level = model.ParseLevel(strconv.Itoa(severity))

	if isRFC5424(rest) {
		parse5424(&e, rest, now)
	} else {
		parse3164(&e, rest, now)
	}
	e.Normalize(now)
	return e
}

func facilityName(f int) string {
	if f >= 0 && f < len(facilityNames) {
		return facilityNames[f]
	}
	return strconv.Itoa(f)
}

func severityName(s int) string {
	if s >= 0 && s < len(severityNames) {
		return severityNames[s]
	}
	return strconv.Itoa(s)
}

// parsePRI reads the leading "<N>" priority header.
func parsePRI(line string) (pri int, rest string, ok bool) {
	if len(line) < 3 || line[0] != '<' {
		return 0, line, false
	}
	end := strings.IndexByte(line, '>')
	if end < 2 || end > 4 { // "<0>" .. "<191>"
		return 0, line, false
	}
	n, err := strconv.Atoi(line[1:end])
	if err != nil || n < 0 || n > 191 {
		return 0, line, false
	}
	return n, line[end+1:], true
}

// isRFC5424 reports whether the remainder after the PRI starts with a version
// digit followed by a space, which is what distinguishes 5424 from 3164.
func isRFC5424(rest string) bool {
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	return i > 0 && i < len(rest) && rest[i] == ' '
}

// parse5424 handles "VERSION SP TIMESTAMP SP HOSTNAME SP APP-NAME SP PROCID SP
// MSGID SP STRUCTURED-DATA [SP MSG]".
func parse5424(e *model.LogEvent, rest string, now time.Time) {
	fields := make([]string, 0, 7)
	remainder := rest
	for len(fields) < 6 {
		sp := strings.IndexByte(remainder, ' ')
		if sp < 0 {
			fields = append(fields, remainder)
			remainder = ""
			break
		}
		fields = append(fields, remainder[:sp])
		remainder = remainder[sp+1:]
	}
	if len(fields) < 6 {
		// Too short to be a real 5424 message; keep what we have as the message.
		e.Message = strings.TrimSpace(rest)
		return
	}

	if ts := unNil(fields[1]); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Timestamp = t.UTC()
		}
	}
	e.Source = unNil(fields[2])
	e.Service = unNil(fields[3])
	if procID := unNil(fields[4]); procID != "" {
		e.Attributes["procid"] = procID
	}
	if msgID := unNil(fields[5]); msgID != "" {
		e.Attributes["msgid"] = msgID
	}

	sd, msg := splitStructuredData(remainder)
	for k, v := range parseStructuredData(sd) {
		e.Attributes[k] = v
	}
	e.Message = strings.TrimSpace(stripBOM(msg))
}

// splitStructuredData separates the STRUCTURED-DATA element from the message.
// It respects quoting and escaping so a ']' inside a parameter value does not
// terminate the block early.
func splitStructuredData(s string) (sd, msg string) {
	s = strings.TrimLeft(s, " ")
	if strings.HasPrefix(s, nilValue) {
		return "", strings.TrimPrefix(s[1:], " ")
	}
	if !strings.HasPrefix(s, "[") {
		return "", s
	}
	var (
		inQuote bool
		escaped bool
		depth   int
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case inQuote:
			// Brackets inside a quoted value are literal.
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				// RFC5424 allows several elements back to back ("[a ...][b ...]").
				// Stopping at the first ']' would leave the rest of them sitting
				// in the message text.
				if i+1 < len(s) && s[i+1] == '[' {
					continue
				}
				return s[:i+1], strings.TrimPrefix(s[i+1:], " ")
			}
		}
	}
	return s, "" // unterminated: treat the whole remainder as structured data
}

// parseStructuredData flattens SD elements into "sdid.param" attribute keys.
func parseStructuredData(sd string) map[string]any {
	out := map[string]any{}
	for _, elem := range splitElements(sd) {
		id, params := splitElementID(elem)
		if id == "" {
			continue
		}
		if len(params) == 0 {
			out[id] = true // an element with no parameters is still a signal
			continue
		}
		for k, v := range params {
			out[id+"."+k] = v
		}
	}
	return out
}

// splitElements breaks "[a ...][b ...]" into its bracketed elements.
func splitElements(sd string) []string {
	var (
		elems   []string
		start   = -1
		inQuote bool
		escaped bool
	)
	for i := 0; i < len(sd); i++ {
		c := sd[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			inQuote = !inQuote
		case inQuote:
		case c == '[':
			if start < 0 {
				start = i + 1
			}
		case c == ']':
			if start >= 0 {
				elems = append(elems, sd[start:i])
				start = -1
			}
		}
	}
	return elems
}

// splitElementID separates an element's SD-ID from its key="value" parameters.
func splitElementID(elem string) (id string, params map[string]string) {
	elem = strings.TrimSpace(elem)
	sp := strings.IndexByte(elem, ' ')
	if sp < 0 {
		return elem, nil
	}
	id = elem[:sp]
	params = map[string]string{}

	rest := elem[sp+1:]
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			return id, params
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			return id, params
		}
		key := rest[:eq]
		rest = rest[eq+1:]
		if !strings.HasPrefix(rest, `"`) {
			return id, params
		}
		value, remainder, ok := readQuoted(rest)
		if !ok {
			return id, params
		}
		params[key] = value
		rest = remainder
	}
}

// readQuoted reads a `"..."` value, honoring RFC5424's backslash escapes.
func readQuoted(s string) (value, rest string, ok bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", s, false
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			b.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == '"':
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), "", false
}

// rfc3164Layouts are the timestamp forms seen in the wild. The classic form
// carries no year, so the receive year is assumed.
var rfc3164Layouts = []string{
	"Jan _2 15:04:05",
	"Jan 02 15:04:05",
	"Jan _2 2006 15:04:05",
}

// parse3164 handles "TIMESTAMP HOSTNAME TAG[PID]: MSG". Every part is optional
// in practice, so each step degrades to leaving the rest as the message.
func parse3164(e *model.LogEvent, rest string, now time.Time) {
	rest = strings.TrimLeft(rest, " ")

	// Some senders emit an RFC3339 timestamp despite otherwise using 3164.
	if sp := strings.IndexByte(rest, ' '); sp > 0 {
		if t, err := time.Parse(time.RFC3339Nano, rest[:sp]); err == nil {
			e.Timestamp = t.UTC()
			rest = strings.TrimLeft(rest[sp+1:], " ")
		}
	}

	if e.Timestamp.IsZero() && len(rest) >= 15 {
		stamp := rest[:15]
		for _, layout := range rfc3164Layouts {
			t, err := time.Parse(layout, stamp)
			if err != nil {
				continue
			}
			if t.Year() == 0 {
				t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
				// A December message received in January belongs to last year.
				if t.Sub(now) > 24*time.Hour {
					t = t.AddDate(-1, 0, 0)
				}
			}
			e.Timestamp = t.UTC()
			rest = strings.TrimLeft(rest[15:], " ")
			break
		}
	}

	// HOSTNAME
	if sp := strings.IndexByte(rest, ' '); sp > 0 {
		e.Source = rest[:sp]
		rest = strings.TrimLeft(rest[sp+1:], " ")
	}

	// TAG[PID]: — the colon terminates the tag, but only if it is close by;
	// otherwise a message containing a colon would be mistaken for a tag.
	if colon := strings.IndexByte(rest, ':'); colon > 0 && colon <= 48 && !strings.Contains(rest[:colon], " ") {
		tag := rest[:colon]
		if open := strings.IndexByte(tag, '['); open > 0 && strings.HasSuffix(tag, "]") {
			e.Attributes["procid"] = tag[open+1 : len(tag)-1]
			tag = tag[:open]
		}
		e.Service = tag
		rest = strings.TrimLeft(rest[colon+1:], " ")
	}

	e.Message = strings.TrimSpace(rest)
}

func unNil(s string) string {
	if s == nilValue {
		return ""
	}
	return s
}

func stripBOM(s string) string { return strings.TrimPrefix(s, "\ufeff") }
