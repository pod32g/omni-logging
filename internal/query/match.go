package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/pod32g/omni-logging/internal/model"
)

// Matches reports whether an event satisfies the query. It is used by live
// tail to decide which newly ingested events to push to a subscriber. The
// semantics mirror the store's SQL execution: all filters and all free-text
// terms must match (AND), within the time bounds.
func (q Query) Matches(e model.LogEvent) bool {
	if !q.From.IsZero() && e.Timestamp.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && e.Timestamp.After(q.To) {
		return false
	}
	for _, f := range q.Filters {
		if !f.matches(e) {
			return false
		}
	}
	for _, term := range q.Terms {
		if !termMatches(e, term) {
			return false
		}
	}
	return true
}

// matches mirrors the store's SQL execution exactly (search.go filterCond) so
// live tail and search never disagree: equality/IN/lexical-comparison are
// case-sensitive (SQLite's default BINARY collation), level values are
// lowercase-normalized on both sides, glob/LIKE is ASCII case-insensitive, and
// numeric comparison coerces like CAST(... AS REAL).
func (f Filter) matches(e model.LogEvent) bool {
	actual, present := f.actual(e)
	want := f.norm(f.Value)

	switch f.Op {
	case OpExists:
		// Attribute: present with any value (matches json_extract IS NOT NULL).
		// Field: non-empty (matches "col IS NOT NULL AND col != ''").
		if f.Field == FieldAttr {
			return present
		}
		return actual != ""
	case OpNeq:
		// A missing attribute satisfies !=.
		return !present || actual != want
	}

	if !present {
		return false // every remaining operator requires a value
	}

	switch f.Op {
	case OpEq:
		return actual == want
	case OpIn:
		for _, v := range f.Values {
			if actual == f.norm(v) {
				return true
			}
		}
		return false
	case OpLike:
		return globMatch(f.Value, actual)
	case OpRegex:
		re, err := compileRegex(f.Value)
		return err == nil && re.MatchString(actual)
	case OpGt, OpGte, OpLt, OpLte:
		return compareMatch(f.Op, actual, want)
	default:
		return actual == want
	}
}

// norm lowercases level values to match the normalized storage (the SQL builder
// does the same).
func (f Filter) norm(v string) string {
	if f.Field == FieldLevel {
		return strings.ToLower(v)
	}
	return v
}

// actual returns the event's value for the filter's field and whether it is
// present (attributes may be absent).
func (f Filter) actual(e model.LogEvent) (string, bool) {
	switch f.Field {
	case FieldLevel:
		return string(e.Level), true
	case FieldService:
		return e.Service, true
	case FieldSource:
		return e.Source, true
	case FieldMessage:
		return e.Message, true
	case FieldRaw:
		return e.Raw, true
	case FieldAttr:
		v, ok := e.Attributes[f.Attr]
		if !ok {
			return "", false
		}
		return stringify(v), true
	}
	return "", false
}

// compareMatch evaluates a comparison operator, mirroring the SQL builder: when
// the query value is numeric, both sides are compared as numbers (the actual
// value coerced like CAST(... AS REAL), i.e. 0 for non-numeric text); otherwise
// a case-sensitive lexical comparison (SQLite's default BINARY collation).
func compareMatch(op Op, actual, want string) bool {
	if wf, err := strconv.ParseFloat(want, 64); err == nil {
		af, _ := strconv.ParseFloat(actual, 64) // 0 on failure, like CAST AS REAL
		switch op {
		case OpGt:
			return af > wf
		case OpGte:
			return af >= wf
		case OpLt:
			return af < wf
		case OpLte:
			return af <= wf
		}
	}
	c := strings.Compare(actual, want)
	switch op {
	case OpGt:
		return c > 0
	case OpGte:
		return c >= 0
	case OpLt:
		return c < 0
	case OpLte:
		return c <= 0
	}
	return false
}

// globMatch reports whether s matches a glob pattern whose only metacharacter is
// '*' (matching any run of characters). Matching is case-insensitive.
func globMatch(pattern, s string) bool {
	re, err := compileRegex("(?i)^" + globToRegex(pattern) + "$")
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func globToRegex(pattern string) string {
	var b strings.Builder
	for _, part := range strings.Split(pattern, "*") {
		// Rebuild with quoted literals between '*' wildcards.
		b.WriteString(regexp.QuoteMeta(part))
		b.WriteString(".*")
	}
	// Drop the trailing ".*" added after the last segment.
	out := b.String()
	return strings.TrimSuffix(out, ".*")
}

var (
	regexCacheMu sync.RWMutex
	regexCache   = map[string]*regexp.Regexp{}
)

// compileRegex compiles and caches an RE2 pattern.
func compileRegex(pattern string) (*regexp.Regexp, error) {
	regexCacheMu.RLock()
	re, ok := regexCache[pattern]
	regexCacheMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	regexCacheMu.Lock()
	if len(regexCache) >= maxRegexCache {
		regexCache = map[string]*regexp.Regexp{} // bound memory from many distinct patterns
	}
	regexCache[pattern] = re
	regexCacheMu.Unlock()
	return re, nil
}

const maxRegexCache = 1024

// termMatches does a case-insensitive substring search across the searchable
// text of an event: message, raw, service, source, and attribute values.
func termMatches(e model.LogEvent, term string) bool {
	needle := strings.ToLower(term)
	if strings.Contains(strings.ToLower(e.Message), needle) ||
		strings.Contains(strings.ToLower(e.Raw), needle) ||
		strings.Contains(strings.ToLower(e.Service), needle) ||
		strings.Contains(strings.ToLower(e.Source), needle) {
		return true
	}
	for k, v := range e.Attributes {
		if strings.Contains(strings.ToLower(k), needle) ||
			strings.Contains(strings.ToLower(stringify(v)), needle) {
			return true
		}
	}
	return false
}

// stringify renders an attribute value as a string for comparison/search.
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
