package query

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

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
	// Tokenize once for the whole query, not once per term: this runs inside the
	// tail hub's fan-out, which the ingest batch writer calls synchronously, so
	// per-term re-tokenization showed up directly as ingest throughput.
	if len(q.Terms) > 0 {
		toks := ftsTokens(e)
		for _, term := range q.Terms {
			if !termMatchesTokens(toks, term) {
				return false
			}
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
		if !present {
			return true
		}
		if boolEqual(actual, want) {
			return false
		}
		return actual != want
	}

	if !present {
		return false // every remaining operator requires a value
	}

	switch f.Op {
	case OpEq:
		if boolEqual(actual, want) {
			return true
		}
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

// compareString renders an attribute the way SQLite's json_extract does, which
// is what a filter is actually compared against. The only divergence from
// stringify is bool: SQLite has no boolean type and yields 1/0, so rendering
// Go's true as "true" here would make attr.flag=1 match in search and not in
// live tail. Free-text matching still uses stringify, because the FTS text the
// store indexes is built with %v.
func compareString(v any) string {
	switch b := v.(type) {
	case bool:
		if b {
			return "1"
		}
		return "0"
	}
	return stringify(v)
}

// boolEqual matches a boolean the way the store does. SQLite has no boolean
// type, so a JSON true reads back as 1; accepting both spellings on both sides
// is what keeps live tail and search from disagreeing about attr.flag=true.
func boolEqual(actual, want string) bool {
	w := strings.ToLower(want)
	if w != "true" && w != "false" {
		return false
	}
	a := strings.ToLower(actual)
	switch w {
	case "true":
		return a == "true" || a == "1"
	default:
		return a == "false" || a == "0"
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
		return compareString(v), true
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

// termMatches reports whether a free-text term matches an event, mirroring the
// store's FTS5 execution rather than doing a naive substring search.
//
// The store wraps each term as a quoted FTS5 string, which matches a contiguous
// run of whole tokens. Substring matching would disagree with it in both
// directions — searching "err" would stream every "error" event into live tail
// while returning nothing from search — so we tokenize the same way FTS5's
// default tokenizer does (runs of letters and digits, case-folded) and look for
// the term's tokens as a contiguous subsequence.
// It tokenizes the event on each call; Matches goes through termMatchesTokens
// so a multi-term query pays for tokenization only once.
func termMatches(e model.LogEvent, term string) bool {
	return termMatchesTokens(ftsTokens(e), term)
}

// termMatchesTokens reports whether term matches an already-tokenized event.
func termMatchesTokens(have []string, term string) bool {
	want := ftsTokenize(term)
	if len(want) == 0 {
		return false // FTS5 matches nothing for a term with no tokens
	}
	return containsTokens(have, want)
}

// ftsTokens builds the token stream for the event's searchable text, in the
// same order the store concatenates it (see sqlite.ftsText): message, raw,
// service, source, then every attribute key and value.
func ftsTokens(e model.LogEvent) []string {
	toks := ftsTokenize(e.Message)
	toks = append(toks, ftsTokenize(e.Raw)...)
	toks = append(toks, ftsTokenize(e.Service)...)
	toks = append(toks, ftsTokenize(e.Source)...)
	for k, v := range e.Attributes {
		toks = append(toks, ftsTokenize(k)...)
		toks = append(toks, ftsTokenize(stringify(v))...)
	}
	return toks
}

// ftsTokenize splits text into lowercase alphanumeric tokens, approximating FTS5's
// default unicode61 tokenizer (which breaks on anything that is not a letter or
// a digit).
func ftsTokenize(s string) []string {
	var (
		toks []string
		b    strings.Builder
	)
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return toks
}

// containsTokens reports whether want appears as a contiguous run inside have,
// which is how FTS5 evaluates a quoted multi-word phrase.
// The last token matches as a prefix, mirroring the trailing '*' the store
// appends to each FTS5 phrase: a partly-typed word ("conn") has to find
// "connection", or a live-tail filter goes silent until the word is finished.
// Only the final token is a prefix — earlier tokens of a phrase must match in
// full, which is exactly how FTS5 evaluates `"a b"*`.
func containsTokens(have, want []string) bool {
	if len(want) > len(have) {
		return false
	}
	last := len(want) - 1
	for i := 0; i+len(want) <= len(have); i++ {
		match := true
		for j := 0; j < last; j++ {
			if have[i+j] != want[j] {
				match = false
				break
			}
		}
		if match && strings.HasPrefix(have[i+last], want[last]) {
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
