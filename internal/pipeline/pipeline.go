package pipeline

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// ErrNotFound is returned by a store when a pipeline ID does not exist.
var ErrNotFound = errors.New("pipeline: not found")

// StageType names a transform.
type StageType string

const (
	StageGrok      StageType = "grok"      // extract named captures from a field
	StageRegex     StageType = "regex"     // same, with a raw RE2 pattern
	StageTimestamp StageType = "timestamp" // parse a field into the event time
	StageLevel     StageType = "level"     // set the event level from a field
	StageService   StageType = "service"   // set the service from a field
	StageRename    StageType = "rename"    // move an attribute
	StageRemove    StageType = "remove"    // drop attributes
	StageSet       StageType = "set"       // set an attribute to a literal
)

// StageSpec is the serialized form of one stage.
type StageSpec struct {
	Type StageType `json:"type"`
	// Field is the input, naming a first-class field (message, raw, service,
	// source, level) or an attribute. Defaults to "message".
	Field   string   `json:"field,omitempty"`
	Pattern string   `json:"pattern,omitempty"` // grok/regex
	To      string   `json:"to,omitempty"`      // rename target, or set target
	Value   string   `json:"value,omitempty"`   // set literal
	Fields  []string `json:"fields,omitempty"`  // remove targets
	Layouts []string `json:"layouts,omitempty"` // timestamp formats to try
	// OnFailure controls what happens when a stage cannot do its job. Ignoring
	// failure is the default because a parse rule that does not match a
	// particular line is normal, not an error.
	FailPipeline bool `json:"fail_pipeline,omitempty"`
}

// Spec is the serialized form of a whole pipeline.
type Spec struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Match selects which events this pipeline applies to, written in the same
	// query language as search — so "service=nginx" means the same thing here
	// as it does in the search box. Empty applies to everything.
	Match   string      `json:"match,omitempty"`
	Stages  []StageSpec `json:"stages"`
	Enabled bool        `json:"enabled"`
	// Order breaks ties when several pipelines match; lower runs first.
	Order     int       `json:"order"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Pipeline is a compiled, runnable Spec.
type Pipeline struct {
	Spec   Spec
	match  *query.Query // nil means "everything"
	stages []stage
}

// stage is one compiled transform.
type stage interface {
	apply(e *model.LogEvent) error
	spec() StageSpec
}

// Compile validates a spec and prepares it to run. Compiling at write time is
// what makes a bad pattern a 400 on save rather than a silent failure on every
// event thereafter.
func Compile(s Spec) (*Pipeline, error) {
	p := &Pipeline{Spec: s}
	if strings.TrimSpace(s.Match) != "" {
		// query.Parse handles the filter half only; the pipeline split lives in
		// Params.Build. Check for it explicitly, because a match written as
		// "| stats count" is a user error worth naming rather than silently
		// treating "stats" and "count" as free-text terms.
		if len(query.SplitPipeline(s.Match)) > 1 {
			return nil, fmt.Errorf("pipeline %q: match is a filter, not a pipeline — remove the '|' stage", s.Name)
		}
		q, err := query.Parse(s.Match)
		if err != nil {
			return nil, fmt.Errorf("pipeline %q: match: %w", s.Name, err)
		}
		q.Normalize()
		p.match = &q
	}
	if len(s.Stages) == 0 {
		return nil, fmt.Errorf("pipeline %q: needs at least one stage", s.Name)
	}
	for i, spec := range s.Stages {
		st, err := compileStage(spec)
		if err != nil {
			return nil, fmt.Errorf("pipeline %q stage %d (%s): %w", s.Name, i+1, spec.Type, err)
		}
		p.stages = append(p.stages, st)
	}
	return p, nil
}

// Matches reports whether this pipeline applies to an event.
func (p *Pipeline) Matches(e model.LogEvent) bool {
	if p.match == nil {
		return true
	}
	return p.match.Matches(e)
}

// Apply runs every stage in order. A stage that cannot do its job is skipped
// unless it set FailPipeline: a grok pattern not matching a given line is the
// normal case, not an error, and aborting there would discard the rest of the
// enrichment for every line that happens not to fit.
func (p *Pipeline) Apply(e *model.LogEvent) error {
	for _, st := range p.stages {
		if err := st.apply(e); err != nil {
			if st.spec().FailPipeline {
				return fmt.Errorf("pipeline %q: %s: %w", p.Spec.Name, st.spec().Type, err)
			}
		}
	}
	return nil
}

func compileStage(s StageSpec) (stage, error) {
	if s.Field == "" {
		s.Field = "message"
	}
	switch s.Type {
	case StageGrok:
		g, err := CompileGrok(s.Pattern)
		if err != nil {
			return nil, err
		}
		return &extractStage{s: s, grok: g}, nil
	case StageRegex:
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return nil, err
		}
		return &extractStage{s: s, re: re}, nil
	case StageTimestamp:
		layouts := s.Layouts
		if len(layouts) == 0 {
			layouts = defaultTimeLayouts
		}
		return &timestampStage{s: s, layouts: layouts}, nil
	case StageLevel:
		return &levelStage{s: s}, nil
	case StageService:
		return &serviceStage{s: s}, nil
	case StageRename:
		if s.To == "" {
			return nil, fmt.Errorf("rename needs a 'to' field")
		}
		return &renameStage{s: s}, nil
	case StageRemove:
		if len(s.Fields) == 0 {
			return nil, fmt.Errorf("remove needs at least one field")
		}
		return &removeStage{s: s}, nil
	case StageSet:
		if s.To == "" {
			return nil, fmt.Errorf("set needs a 'to' field")
		}
		return &setStage{s: s}, nil
	}
	return nil, fmt.Errorf("unknown stage type %q", s.Type)
}

// --- field access -----------------------------------------------------------

// readField reads a first-class field or an attribute by name.
func readField(e *model.LogEvent, name string) (string, bool) {
	switch strings.ToLower(name) {
	case "message", "msg":
		return e.Message, e.Message != ""
	case "raw":
		return e.Raw, e.Raw != ""
	case "service":
		return e.Service, e.Service != ""
	case "source", "host":
		return e.Source, e.Source != ""
	case "level":
		return string(e.Level), e.Level != ""
	}
	key := strings.TrimPrefix(name, "attr.")
	if e.Attributes == nil {
		return "", false
	}
	v, ok := e.Attributes[key]
	if !ok {
		return "", false
	}
	return stringify(v), true
}

// writeAttr sets an attribute, converting numeric-looking strings so that
// comparison filters (attr.status>=500) work without the author having to
// think about types.
func writeAttr(e *model.LogEvent, key, value string) {
	if e.Attributes == nil {
		e.Attributes = map[string]any{}
	}
	if n, err := strconv.ParseFloat(value, 64); err == nil && looksNumeric(value) {
		e.Attributes[key] = n
		return
	}
	e.Attributes[key] = value
}

// looksNumeric keeps things like "007" or "1e5" from silently changing shape;
// only plain decimal numbers are converted.
func looksNumeric(s string) bool {
	if s == "" || (len(s) > 1 && s[0] == '0' && s[1] != '.') {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' {
			return false
		}
	}
	return true
}

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

// --- stages -----------------------------------------------------------------

// extractStage promotes named captures into attributes, or onto first-class
// fields when the capture is named after one.
type extractStage struct {
	s    StageSpec
	grok *CompiledGrok
	re   *regexp.Regexp
}

func (x *extractStage) spec() StageSpec { return x.s }

func (x *extractStage) apply(e *model.LogEvent) error {
	src, ok := readField(e, x.s.Field)
	if !ok {
		return fmt.Errorf("field %q is empty", x.s.Field)
	}

	var fields map[string]string
	if x.grok != nil {
		var matched bool
		fields, matched = x.grok.Match(src)
		if !matched {
			return fmt.Errorf("pattern did not match")
		}
	} else {
		m := x.re.FindStringSubmatch(src)
		if m == nil {
			return fmt.Errorf("pattern did not match")
		}
		fields = map[string]string{}
		for i, name := range x.re.SubexpNames() {
			if name == "" || i >= len(m) || m[i] == "" {
				continue
			}
			fields[name] = m[i]
		}
	}

	for name, value := range fields {
		// A capture named after a first-class field lands there rather than in
		// attributes, so "%{LOGLEVEL:level}" does what it obviously means.
		switch strings.ToLower(name) {
		case "message":
			e.Message = value
		case "service":
			e.Service = value
		case "source", "host":
			e.Source = value
		case "level":
			e.Level = model.ParseLevel(value)
		default:
			writeAttr(e, name, value)
		}
	}
	return nil
}

// defaultTimeLayouts are tried in order when a timestamp stage names none.
var defaultTimeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05,000",
	"2006-01-02 15:04:05.000",
	"2006-01-02 15:04:05",
	"02/Jan/2006:15:04:05 -0700", // HTTPDATE
	"Jan _2 15:04:05",            // syslog, no year
	time.RFC1123Z,
}

type timestampStage struct {
	s       StageSpec
	layouts []string
}

func (t *timestampStage) spec() StageSpec { return t.s }

func (t *timestampStage) apply(e *model.LogEvent) error {
	raw, ok := readField(e, t.s.Field)
	if !ok {
		return fmt.Errorf("field %q is empty", t.s.Field)
	}
	raw = strings.TrimSpace(raw)

	// Epoch seconds/millis are common enough to accept without a layout.
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		e.Timestamp = epochToTime(n)
		return nil
	}
	for _, layout := range t.layouts {
		parsed, err := time.Parse(layout, raw)
		if err != nil {
			continue
		}
		if parsed.Year() == 0 {
			// A layout without a year (syslog) means the event year is implied
			// by when it was received.
			parsed = parsed.AddDate(e.ReceivedAt.Year(), 0, 0)
		}
		e.Timestamp = parsed.UTC()
		return nil
	}
	return fmt.Errorf("no layout matched %q", raw)
}

func epochToTime(n int64) time.Time {
	switch {
	case n >= 1e18:
		return time.Unix(0, n).UTC()
	case n >= 1e15:
		return time.Unix(0, n*1e3).UTC()
	case n >= 1e12:
		return time.Unix(0, n*1e6).UTC()
	default:
		return time.Unix(n, 0).UTC()
	}
}

type levelStage struct{ s StageSpec }

func (l *levelStage) spec() StageSpec { return l.s }
func (l *levelStage) apply(e *model.LogEvent) error {
	v, ok := readField(e, l.s.Field)
	if !ok {
		return fmt.Errorf("field %q is empty", l.s.Field)
	}
	e.Level = model.ParseLevel(v)
	return nil
}

type serviceStage struct{ s StageSpec }

func (sv *serviceStage) spec() StageSpec { return sv.s }
func (sv *serviceStage) apply(e *model.LogEvent) error {
	v, ok := readField(e, sv.s.Field)
	if !ok {
		return fmt.Errorf("field %q is empty", sv.s.Field)
	}
	e.Service = v
	return nil
}

type renameStage struct{ s StageSpec }

func (r *renameStage) spec() StageSpec { return r.s }
func (r *renameStage) apply(e *model.LogEvent) error {
	v, ok := readField(e, r.s.Field)
	if !ok {
		return fmt.Errorf("field %q is empty", r.s.Field)
	}
	writeAttr(e, strings.TrimPrefix(r.s.To, "attr."), v)
	if e.Attributes != nil {
		delete(e.Attributes, strings.TrimPrefix(r.s.Field, "attr."))
	}
	return nil
}

type removeStage struct{ s StageSpec }

func (r *removeStage) spec() StageSpec { return r.s }
func (r *removeStage) apply(e *model.LogEvent) error {
	if e.Attributes == nil {
		return nil
	}
	for _, f := range r.s.Fields {
		delete(e.Attributes, strings.TrimPrefix(f, "attr."))
	}
	return nil
}

type setStage struct{ s StageSpec }

func (s *setStage) spec() StageSpec { return s.s }
func (s *setStage) apply(e *model.LogEvent) error {
	writeAttr(e, strings.TrimPrefix(s.s.To, "attr."), s.s.Value)
	return nil
}
