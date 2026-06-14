package query

import (
	"fmt"
	"strings"

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
	var actual string
	switch f.Field {
	case FieldLevel:
		actual = string(e.Level)
	case FieldService:
		actual = e.Service
	case FieldSource:
		actual = e.Source
	case FieldAttr:
		v, ok := e.Attributes[f.Attr]
		if !ok {
			// Missing attribute: == never matches, != always matches.
			return f.Negate
		}
		actual = stringify(v)
	}
	eq := strings.EqualFold(actual, f.Value)
	if f.Negate {
		return !eq
	}
	return eq
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
