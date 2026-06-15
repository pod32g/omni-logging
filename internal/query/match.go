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

func (f Filter) matches(e model.LogEvent) bool {
	actual, present := f.actual(e)

	switch f.Op {
	case OpExists:
		return present && actual != ""
	case OpNeq:
		// A missing attribute satisfies !=.
		return !present || !strings.EqualFold(actual, f.Value)
	}

	if !present {
		return false // every remaining operator requires a value
	}

	switch f.Op {
	case OpEq:
		return strings.EqualFold(actual, f.Value)
	case OpIn:
		for _, v := range f.Values {
			if strings.EqualFold(actual, v) {
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
		return compareMatch(f.Op, actual, f.Value)
	default:
		return strings.EqualFold(actual, f.Value)
	}
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

// compareMatch evaluates a comparison operator. If both operands parse as
// numbers it compares numerically, otherwise lexically.
func compareMatch(op Op, actual, want string) bool {
	af, aerr := strconv.ParseFloat(actual, 64)
	wf, werr := strconv.ParseFloat(want, 64)
	if aerr == nil && werr == nil {
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
	c := strings.Compare(strings.ToLower(actual), strings.ToLower(want))
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
	regexCache[pattern] = re
	regexCacheMu.Unlock()
	return re, nil
}

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
