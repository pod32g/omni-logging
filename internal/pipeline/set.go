package pipeline

import (
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/pod32g/omni-logging/internal/model"
)

// Set is the collection of pipelines applied to each event, swappable at
// runtime so editing a pipeline takes effect without a restart.
//
// Reads are on the ingest hot path and vastly outnumber writes, so the compiled
// pipelines are held in an atomic pointer and replaced wholesale rather than
// guarded by a mutex taken per event.
type Set struct {
	compiled atomic.Pointer[[]*Pipeline]
	logger   *slog.Logger

	applied atomic.Int64
	failed  atomic.Int64

	// warnOnce keeps a pipeline that fails on every single event from
	// producing one log line per event.
	warnOnce sync.Map // pipeline ID -> struct{}
}

// NewSet creates an empty Set, which passes events through untouched.
func NewSet(logger *slog.Logger) *Set {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Set{logger: logger}
	empty := []*Pipeline{}
	s.compiled.Store(&empty)
	return s
}

// Replace compiles and installs a new set of pipelines. It is all-or-nothing:
// if any spec fails to compile the current set is left untouched, because a
// half-applied configuration is harder to reason about than a rejected one.
func (s *Set) Replace(specs []Spec) error {
	next := make([]*Pipeline, 0, len(specs))
	for _, spec := range specs {
		if !spec.Enabled {
			continue
		}
		p, err := Compile(spec)
		if err != nil {
			return err
		}
		next = append(next, p)
	}
	// Stable order so two pipelines with the same Order stay in a predictable
	// sequence rather than whatever the store happened to return.
	sort.SliceStable(next, func(i, j int) bool { return next[i].Spec.Order < next[j].Spec.Order })
	s.compiled.Store(&next)
	s.warnOnce.Range(func(k, _ any) bool { s.warnOnce.Delete(k); return true })
	return nil
}

// Len reports how many pipelines are currently active.
func (s *Set) Len() int { return len(*s.compiled.Load()) }

// Apply runs every matching pipeline over the event, in order. The event is
// modified in place.
//
// A pipeline failure never rejects the event: losing a log line because an
// enrichment rule was wrong is a far worse outcome than storing it unenriched.
func (s *Set) Apply(e *model.LogEvent) {
	pipelines := *s.compiled.Load()
	if len(pipelines) == 0 {
		return
	}
	for _, p := range pipelines {
		if !p.Matches(*e) {
			continue
		}
		if err := p.Apply(e); err != nil {
			s.failed.Add(1)
			if _, warned := s.warnOnce.LoadOrStore(p.Spec.ID, struct{}{}); !warned {
				s.logger.Warn("pipeline failed; the event is stored unenriched",
					"pipeline", p.Spec.Name, "error", err)
			}
			continue
		}
		s.applied.Add(1)
	}
}

// Metrics is a snapshot of pipeline activity.
type Metrics struct {
	Pipelines int   `json:"pipelines"`
	Applied   int64 `json:"applied"`
	Failed    int64 `json:"failed"`
}

// Metrics returns a snapshot of pipeline activity.
func (s *Set) Metrics() Metrics {
	return Metrics{Pipelines: s.Len(), Applied: s.applied.Load(), Failed: s.failed.Load()}
}

// TestResult is what a dry run reports: the event as it would be stored, plus
// which pipelines touched it.
type TestResult struct {
	Event   model.LogEvent `json:"event"`
	Matched []string       `json:"matched"`
	Errors  []string       `json:"errors,omitempty"`
}

// Test runs a set of specs over one event without installing them, so a pattern
// can be checked against a real line before it is saved.
func Test(specs []Spec, e model.LogEvent) (TestResult, error) {
	res := TestResult{}
	compiled := make([]*Pipeline, 0, len(specs))
	for _, spec := range specs {
		p, err := Compile(spec)
		if err != nil {
			return res, err
		}
		compiled = append(compiled, p)
	}
	sort.SliceStable(compiled, func(i, j int) bool { return compiled[i].Spec.Order < compiled[j].Spec.Order })

	for _, p := range compiled {
		if !p.Matches(e) {
			continue
		}
		res.Matched = append(res.Matched, p.Spec.Name)
		if err := p.Apply(&e); err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
	}
	res.Event = e
	return res, nil
}
