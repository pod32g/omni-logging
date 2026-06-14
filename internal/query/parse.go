package query

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// knownFields maps query-language keys (and aliases) to structured filter
// fields. Any key not listed here is treated as an attribute filter.
var knownFields = map[string]Field{
	"level":   FieldLevel,
	"service": FieldService,
	"source":  FieldSource,
	"host":    FieldSource,
}

// Parse turns a query expression into filters and free-text terms. It does not
// set time bounds, limit, or order — those come from request parameters via
// the Builder. Examples:
//
//	level=error service=checkout-api timeout
//	"connection refused" attr.user_id=42 level!=debug
func Parse(expr string) (Query, error) {
	tokens, err := tokenize(expr)
	if err != nil {
		return Query{}, err
	}

	var q Query
	for _, tok := range tokens {
		if tok.quoted {
			q.Terms = append(q.Terms, tok.text)
			continue
		}
		f, isFilter, err := parseFilter(tok.text)
		if err != nil {
			return Query{}, err
		}
		if isFilter {
			q.Filters = append(q.Filters, f)
		} else if tok.text != "" {
			q.Terms = append(q.Terms, tok.text)
		}
	}
	return q, nil
}

// parseFilter recognizes `key=value` and `key!=value`. It returns isFilter
// false for a bare term (no operator).
func parseFilter(tok string) (Filter, bool, error) {
	neg := false
	idx := -1
	if i := strings.Index(tok, "!="); i >= 0 {
		idx, neg = i, true
	} else if i := strings.Index(tok, "="); i >= 0 {
		idx = i
	}
	if idx <= 0 { // no operator, or empty key
		return Filter{}, false, nil
	}

	key := strings.TrimSpace(tok[:idx])
	val := tok[idx:]
	if neg {
		val = strings.TrimPrefix(val, "!")
	}
	val = strings.TrimSpace(strings.TrimPrefix(val, "="))
	val = strings.Trim(val, `"`)
	if key == "" {
		return Filter{}, false, nil
	}

	f := Filter{Negate: neg, Value: val}
	lower := strings.ToLower(key)
	switch {
	case knownFields[lower] != "":
		f.Field = knownFields[lower]
	case strings.HasPrefix(lower, "attr."):
		f.Field = FieldAttr
		f.Attr = key[len("attr."):]
	default:
		// Bare key -> attribute filter (e.g. user_id=42).
		f.Field = FieldAttr
		f.Attr = key
	}
	if f.Field == FieldAttr && f.Attr == "" {
		return Filter{}, false, fmt.Errorf("empty attribute key in filter %q", tok)
	}
	return f, true, nil
}

type token struct {
	text   string
	quoted bool
}

// tokenize splits an expression on whitespace, treating double-quoted spans as
// single tokens. An unterminated quote is an error.
func tokenize(expr string) ([]token, error) {
	var tokens []token
	var b strings.Builder
	inQuote := false
	flush := func(quoted bool) {
		if b.Len() > 0 || quoted {
			tokens = append(tokens, token{text: b.String(), quoted: quoted})
			b.Reset()
		}
	}
	for _, r := range expr {
		switch {
		case r == '"':
			if inQuote {
				flush(true)
				inQuote = false
			} else {
				flush(false)
				inQuote = true
			}
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			flush(false)
		default:
			b.WriteRune(r)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated quote in query")
	}
	flush(false)
	return tokens, nil
}

// ParseRelative parses a relative duration like "15m", "2h", "7d", "30s".
func ParseRelative(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty relative time")
	}
	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid relative time %q", s)
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid relative time unit in %q (use s/m/h/d)", s)
	}
}

// ParseTime parses an absolute time bound: RFC3339 or unix seconds.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (use RFC3339 or unix seconds)", s)
}
