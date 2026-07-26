// Package pipeline applies ordered parse/transform stages to every event
// between raw ingest and the batch writer. It is the seam unstructured text
// becomes structured at: a grok pattern promotes captures into attributes, a
// timestamp stage moves a parsed time onto the event, and so on.
package pipeline

import (
	"fmt"
	"regexp"
	"sync"
)

// grokPatterns is the built-in pattern library. It is deliberately small and
// focused on what actually shows up in application and web-server logs, rather
// than a port of every pattern in existence — an unused pattern is a
// maintenance cost and another way for a name to collide.
var grokPatterns = map[string]string{
	// primitives
	"WORD":       `\b\w+\b`,
	"NOTSPACE":   `\S+`,
	"SPACE":      `\s*`,
	"DATA":       `.*?`,
	"GREEDYDATA": `.*`,
	"INT":        `(?:[+-]?(?:[0-9]+))`,
	"NUMBER":     `(?:[+-]?(?:[0-9]+(?:\.[0-9]+)?))`,
	"BASE16NUM":  `(?:0[xX])?[0-9a-fA-F]+`,
	"UUID":       `[A-Fa-f0-9]{8}-(?:[A-Fa-f0-9]{4}-){3}[A-Fa-f0-9]{12}`,
	"QS":         `"(?:[^"\\]|\\.)*"`,

	// network
	"IPV4":     `(?:(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(?:25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)`,
	"IPV6":     `(?:[A-Fa-f0-9]{0,4}:){2,7}[A-Fa-f0-9]{0,4}`,
	"IP":       `(?:%{IPV6}|%{IPV4})`,
	"HOSTNAME": `\b(?:[0-9A-Za-z][0-9A-Za-z-]{0,62})(?:\.(?:[0-9A-Za-z][0-9A-Za-z-]{0,62}))*\b`,
	"IPORHOST": `(?:%{IP}|%{HOSTNAME})`,
	"PORT":     `\b(?:[0-9]{1,5})\b`,

	// http
	"URIPATH":    `(?:/[^\s?#]*)`,
	"URIPARAM":   `\?\S*`,
	"URIPROTO":   `[A-Za-z]+(?:\+[A-Za-z+]+)?`,
	"HTTPVERB":   `\b(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|TRACE|CONNECT)\b`,
	"HTTPVER":    `HTTP/[0-9.]+`,
	"HTTPSTATUS": `\b[1-5][0-9]{2}\b`,

	// time
	"YEAR":              `(?:\d{4})`,
	"MONTHNUM":          `(?:0?[1-9]|1[0-2])`,
	"MONTHDAY":          `(?:0?[1-9]|[12][0-9]|3[01])`,
	"HOUR":              `(?:2[0123]|[01]?[0-9])`,
	"MINUTE":            `(?:[0-5][0-9])`,
	"SECOND":            `(?:(?:[0-5]?[0-9]|60)(?:[:.,][0-9]+)?)`,
	"TIME":              `%{HOUR}:%{MINUTE}:%{SECOND}`,
	"ISO8601_TIMEZONE":  `(?:Z|[+-]%{HOUR}(?::?%{MINUTE})?)`,
	"TIMESTAMP_ISO8601": `%{YEAR}-%{MONTHNUM}-%{MONTHDAY}[T ]%{TIME}%{ISO8601_TIMEZONE}?`,
	"MONTH":             `\b(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]*\b`,
	"SYSLOGTIMESTAMP":   `%{MONTH} +%{MONTHDAY} %{TIME}`,
	"HTTPDATE":          `%{MONTHDAY}/%{MONTH}/%{YEAR}:%{TIME} [+-][0-9]{4}`,

	// logging
	"LOGLEVEL": `(?:[Aa]lert|ALERT|[Tt]race|TRACE|[Dd]ebug|DEBUG|[Nn]otice|NOTICE|[Ii]nfo|INFO|[Ww]arn(?:ing)?|WARN(?:ING)?|[Ee]rr(?:or)?|ERR(?:OR)?|[Cc]rit(?:ical)?|CRIT(?:ICAL)?|[Ff]atal|FATAL|[Ee]merg(?:ency)?|EMERG(?:ENCY)?)`,
	"LOGGER":   `[A-Za-z0-9_.$/-]+`,
}

// maxGrokDepth bounds pattern expansion so a self-referential definition fails
// with an error instead of recursing until the stack gives out.
const maxGrokDepth = 20

// grokRef matches "%{NAME}" and "%{NAME:field}".
var grokRef = regexp.MustCompile(`%\{(\w+)(?::([\w.]+))?\}`)

// captureName sanitises a target field into something Go's regexp accepts as a
// capture-group name, keeping a map from the safe name back to the written one.
var nonWordRun = regexp.MustCompile(`\W+`)

// CompiledGrok is a grok pattern ready to run against a line.
type CompiledGrok struct {
	re      *regexp.Regexp
	names   map[string]string // capture-group name -> attribute name as written
	pattern string
}

var (
	grokCacheMu sync.RWMutex
	grokCache   = map[string]*CompiledGrok{}
)

// maxGrokCache bounds how many distinct patterns are kept compiled.
const maxGrokCache = 512

// CompileGrok expands a grok expression into a regexp, caching the result.
// Patterns come from configuration, not from request data, but the cache is
// bounded anyway so a misbehaving config cannot grow memory without limit.
func CompileGrok(pattern string) (*CompiledGrok, error) {
	grokCacheMu.RLock()
	if c, ok := grokCache[pattern]; ok {
		grokCacheMu.RUnlock()
		return c, nil
	}
	grokCacheMu.RUnlock()

	names := map[string]string{}
	expanded, err := expandGrok(pattern, names, 0)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(expanded)
	if err != nil {
		return nil, fmt.Errorf("grok %q: %w", pattern, err)
	}
	c := &CompiledGrok{re: re, names: names, pattern: pattern}

	grokCacheMu.Lock()
	if len(grokCache) >= maxGrokCache {
		grokCache = map[string]*CompiledGrok{}
	}
	grokCache[pattern] = c
	grokCacheMu.Unlock()
	return c, nil
}

// expandGrok recursively replaces %{NAME:field} references with their regex.
func expandGrok(pattern string, names map[string]string, depth int) (string, error) {
	if depth > maxGrokDepth {
		return "", fmt.Errorf("grok: pattern nests more than %d levels deep (is a pattern self-referential?)", maxGrokDepth)
	}
	var outerErr error
	out := grokRef.ReplaceAllStringFunc(pattern, func(ref string) string {
		m := grokRef.FindStringSubmatch(ref)
		name, field := m[1], m[2]
		body, ok := grokPatterns[name]
		if !ok {
			outerErr = fmt.Errorf("grok: unknown pattern %%{%s}", name)
			return ref
		}
		sub, err := expandGrok(body, names, depth+1)
		if err != nil {
			outerErr = err
			return ref
		}
		if field == "" {
			return "(?:" + sub + ")"
		}
		safe := safeCaptureName(field, names)
		names[safe] = field
		return "(?P<" + safe + ">" + sub + ")"
	})
	if outerErr != nil {
		return "", outerErr
	}
	return out, nil
}

// safeCaptureName turns a written field name into a unique, regexp-legal
// capture name. Dots are common in field names ("http.status") but illegal in
// a Go capture group.
func safeCaptureName(field string, names map[string]string) string {
	safe := nonWordRun.ReplaceAllString(field, "_")
	if safe == "" || (safe[0] >= '0' && safe[0] <= '9') {
		safe = "f_" + safe
	}
	base := safe
	for i := 2; ; i++ {
		if existing, taken := names[safe]; !taken || existing == field {
			return safe
		}
		safe = fmt.Sprintf("%s_%d", base, i)
	}
}

// Match runs the pattern against s, returning the captured fields keyed by the
// names as written in the pattern. ok is false when the pattern did not match.
func (c *CompiledGrok) Match(s string) (fields map[string]string, ok bool) {
	m := c.re.FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	fields = map[string]string{}
	for i, capture := range c.re.SubexpNames() {
		if capture == "" || i >= len(m) {
			continue
		}
		name, known := c.names[capture]
		if !known {
			name = capture
		}
		// An optional group that did not participate contributes nothing, so a
		// missing field is absent rather than present-and-empty.
		if m[i] == "" {
			continue
		}
		fields[name] = m[i]
	}
	return fields, true
}

// Pattern returns the grok expression this was compiled from.
func (c *CompiledGrok) Pattern() string { return c.pattern }

// PatternNames lists the built-in pattern names, for documentation and for the
// UI's pattern picker.
func PatternNames() []string {
	out := make([]string, 0, len(grokPatterns))
	for name := range grokPatterns {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ensure the library itself compiles; a typo in a pattern should fail loudly at
// startup rather than the first time someone references it.
func init() {
	for name := range grokPatterns {
		if _, err := CompileGrok("%{" + name + "}"); err != nil {
			panic("pipeline: built-in grok pattern " + name + " is invalid: " + err.Error())
		}
	}
	grokCacheMu.Lock()
	grokCache = map[string]*CompiledGrok{}
	grokCacheMu.Unlock()
}
