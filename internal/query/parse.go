package query

import (
	"fmt"
	"regexp"
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
	"message": FieldMessage,
	"msg":     FieldMessage,
	"raw":     FieldRaw,
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
		handled, err := parseTimeDirective(&q.Time, tok.text)
		if err != nil {
			return Query{}, err
		}
		if handled {
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

// timeKeys are the reserved keys that set the query's time range rather than
// filtering a field. They are reserved *before* the attribute fallback, so
// `last=15m` is a time window and not the attribute filter attr.last="15m" —
// which is what it silently became before, matching nothing and reporting no
// error. An event genuinely carrying one of these as an attribute is still
// reachable with the explicit `attr.` prefix.
var timeKeys = map[string]bool{"last": true, "from": true, "to": true}

// parseTimeDirective recognises `last=`, `from=` and `to=`, recording them on
// spec without resolving. It reports whether the token was consumed.
func parseTimeDirective(spec *TimeSpec, tok string) (bool, error) {
	op, key, val, ok := splitOp(tok)
	if !ok {
		return false, nil
	}
	lower := strings.ToLower(strings.TrimSpace(key))
	if !timeKeys[lower] {
		return false, nil
	}
	if op != "=" {
		// Ranges are expressed with from/to, so `last>1h` has no meaning. Saying
		// so beats treating it as an attribute comparison that matches nothing.
		return false, fmt.Errorf("time bound %q only supports '='; use from=/to= for a range, or attr.%s%s… to filter an attribute", lower, lower, op)
	}
	val = strings.Trim(strings.TrimSpace(val), `"`)
	if val == "" {
		return false, fmt.Errorf("empty value for %q", lower)
	}

	// Validate now, while the offending token is still in hand. Resolution
	// happens in Build, which has a clock.
	switch lower {
	case "last":
		if _, err := ParseRelative(val); err != nil {
			return false, err
		}
		if spec.Last != "" {
			return false, fmt.Errorf("last= given more than once")
		}
		spec.Last = val
	case "from":
		if _, err := ParseTime(val); err != nil {
			return false, err
		}
		if spec.From != "" {
			return false, fmt.Errorf("from= given more than once")
		}
		spec.From = val
	case "to":
		if _, err := ParseTime(val); err != nil {
			return false, err
		}
		if spec.To != "" {
			return false, fmt.Errorf("to= given more than once")
		}
		spec.To = val
	}
	return true, nil
}

// splitOp finds the comparison operator in a filter token and returns the
// operator string, the key (before it), the raw value (after it), and whether an
// operator was found. The key must be non-empty (so a leading operator char,
// e.g. ">foo", is treated as a bare term, not a filter).
func splitOp(tok string) (op, key, val string, ok bool) {
	for i := 0; i < len(tok); i++ {
		if i == 0 {
			continue // key must be non-empty
		}
		switch tok[i] {
		case '!':
			if i+1 < len(tok) && tok[i+1] == '=' {
				return "!=", tok[:i], tok[i+2:], true
			}
		case '>':
			if i+1 < len(tok) && tok[i+1] == '=' {
				return ">=", tok[:i], tok[i+2:], true
			}
			return ">", tok[:i], tok[i+1:], true
		case '<':
			if i+1 < len(tok) && tok[i+1] == '=' {
				return "<=", tok[:i], tok[i+2:], true
			}
			return "<", tok[:i], tok[i+1:], true
		case '=':
			if i+1 < len(tok) && tok[i+1] == '~' {
				return "=~", tok[:i], tok[i+2:], true
			}
			return "=", tok[:i], tok[i+1:], true
		}
	}
	return "", "", "", false
}

// parseFilter recognizes `key OP value` for all supported operators. It returns
// isFilter false for a bare term (no operator).
func parseFilter(tok string) (Filter, bool, error) {
	op, key, val, ok := splitOp(tok)
	if !ok {
		return Filter{}, false, nil
	}
	key = strings.TrimSpace(key)
	val = strings.Trim(strings.TrimSpace(val), `"`)
	if key == "" {
		return Filter{}, false, nil
	}

	f := Filter{}
	lower := strings.ToLower(key)
	switch {
	case knownFields[lower] != "":
		f.Field = knownFields[lower]
	case strings.HasPrefix(lower, "attr."):
		f.Field = FieldAttr
		f.Attr = key[len("attr."):]
	default:
		f.Field = FieldAttr
		f.Attr = key
	}
	if f.Field == FieldAttr && f.Attr == "" {
		return Filter{}, false, fmt.Errorf("empty attribute key in filter %q", tok)
	}

	switch op {
	case "!=":
		f.Op, f.Value = OpNeq, val
	case ">":
		f.Op, f.Value = OpGt, val
	case ">=":
		f.Op, f.Value = OpGte, val
	case "<":
		f.Op, f.Value = OpLt, val
	case "<=":
		f.Op, f.Value = OpLte, val
	case "=~":
		if _, err := regexp.Compile(val); err != nil {
			return Filter{}, false, fmt.Errorf("invalid regex in filter %q: %w", tok, err)
		}
		f.Op, f.Value = OpRegex, val
	case "=":
		switch {
		case val == "*":
			f.Op = OpExists
		case strings.HasPrefix(val, "(") && strings.HasSuffix(val, ")"):
			f.Op = OpIn
			f.Values = splitList(val[1 : len(val)-1])
		case strings.Contains(val, "*"):
			f.Op, f.Value = OpLike, val
		default:
			f.Op, f.Value = OpEq, val
		}
	}
	return f, true, nil
}

// splitList splits a comma-separated IN list, trimming spaces and quotes.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
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

// MaxRelative bounds a relative window. time.Duration is a 64-bit nanosecond
// count that tops out near 292 years, so an unchecked "99999999999d" silently
// overflows into a negative duration and yields a nonsense time bound. A
// century is far past any real retention window.
const MaxRelative = 100 * 365 * 24 * time.Hour

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

	var scale time.Duration
	switch unit {
	case 's':
		scale = time.Second
	case 'm':
		scale = time.Minute
	case 'h':
		scale = time.Hour
	case 'd':
		scale = 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid relative time unit in %q (use s/m/h/d)", s)
	}

	// Compare before multiplying so the check itself cannot overflow.
	if int64(n) > int64(MaxRelative/scale) {
		return 0, fmt.Errorf("relative time %q is too large (max %s)", s, MaxRelative)
	}
	return time.Duration(n) * scale, nil
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
