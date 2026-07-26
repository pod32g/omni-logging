package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

func evt(msg string) model.LogEvent {
	e := model.LogEvent{Message: msg, Raw: msg}
	e.Normalize(time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC))
	return e
}

func run(t *testing.T, spec Spec, e model.LogEvent) model.LogEvent {
	t.Helper()
	p, err := Compile(spec)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if err := p.Apply(&e); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return e
}

// TestGrokExtractsNginxAccessLog covers the case the feature exists for:
// turning an unstructured access-log line into fields you can filter on.
func TestGrokExtractsNginxAccessLog(t *testing.T) {
	line := `192.168.1.10 - - [26/Jul/2026:12:00:01 +0000] "GET /v1/checkout?x=1 HTTP/1.1" 500 1234`
	spec := Spec{
		Name:    "nginx",
		Enabled: true,
		Stages: []StageSpec{{
			Type:    StageGrok,
			Pattern: `%{IP:client} \S+ \S+ \[%{HTTPDATE:ts}\] "%{HTTPVERB:method} %{NOTSPACE:path} %{HTTPVER}" %{INT:status} %{INT:bytes}`,
		}},
	}
	got := run(t, spec, evt(line))

	for k, want := range map[string]any{
		"client": "192.168.1.10",
		"method": "GET",
		"path":   "/v1/checkout?x=1",
		"status": float64(500), // numeric, so attr.status>=500 works
		"bytes":  float64(1234),
	} {
		if got.Attributes[k] != want {
			t.Errorf("attribute %s = %#v, want %#v", k, got.Attributes[k], want)
		}
	}
}

// TestGrokPromotesFirstClassFields: a capture named after a real field lands
// there, so "%{LOGLEVEL:level}" does what it obviously means.
func TestGrokPromotesFirstClassFields(t *testing.T) {
	spec := Spec{
		Name: "app", Enabled: true,
		Stages: []StageSpec{{
			Type:    StageGrok,
			Pattern: `%{LOGLEVEL:level} %{LOGGER:service} %{GREEDYDATA:message}`,
		}},
	}
	got := run(t, spec, evt("ERROR checkout-api connection refused"))

	if got.Level != model.LevelError {
		t.Errorf("level = %q, want error", got.Level)
	}
	if got.Service != "checkout-api" {
		t.Errorf("service = %q", got.Service)
	}
	if got.Message != "connection refused" {
		t.Errorf("message = %q, want the remainder", got.Message)
	}
	if _, leaked := got.Attributes["level"]; leaked {
		t.Error("level was also written as an attribute; it belongs on the field only")
	}
}

func TestTimestampStage(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		layouts     []string
		want        time.Time
	}{
		{"iso8601", "2026-07-26T10:30:00Z", nil, time.Date(2026, 7, 26, 10, 30, 0, 0, time.UTC)},
		{"httpdate", "26/Jul/2026:10:30:00 +0000", nil, time.Date(2026, 7, 26, 10, 30, 0, 0, time.UTC)},
		{"epoch seconds", "1785000000", nil, time.Unix(1785000000, 0).UTC()},
		{"custom layout", "26-07-2026 10:30", []string{"02-01-2006 15:04"}, time.Date(2026, 7, 26, 10, 30, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := evt("x")
			e.Attributes = map[string]any{"ts": tc.value}
			spec := Spec{Name: "t", Enabled: true, Stages: []StageSpec{
				{Type: StageTimestamp, Field: "attr.ts", Layouts: tc.layouts},
			}}
			got := run(t, spec, e)
			if !got.Timestamp.Equal(tc.want) {
				t.Errorf("timestamp = %s, want %s", got.Timestamp, tc.want)
			}
		})
	}
}

// TestStageFailureDoesNotDiscardTheEvent: a pattern that does not fit a
// particular line is normal. Losing the line would be much worse than storing
// it unenriched.
func TestStageFailureDoesNotDiscardTheEvent(t *testing.T) {
	spec := Spec{Name: "strict", Enabled: true, Stages: []StageSpec{
		{Type: StageGrok, Pattern: `%{IP:client} %{GREEDYDATA:rest}`},
		{Type: StageSet, To: "marker", Value: "reached"},
	}}
	got := run(t, spec, evt("this line has no IP in it"))

	if got.Message != "this line has no IP in it" {
		t.Errorf("message was altered: %q", got.Message)
	}
	if got.Attributes["marker"] != "reached" {
		t.Error("a non-matching stage stopped the later stages from running")
	}
}

// TestFailPipelineStopsProcessing: opting in must actually abort the pipeline.
func TestFailPipelineStopsProcessing(t *testing.T) {
	spec := Spec{Name: "strict", Enabled: true, Stages: []StageSpec{
		{Type: StageGrok, Pattern: `%{IP:client}`, FailPipeline: true},
		{Type: StageSet, To: "marker", Value: "reached"},
	}}
	p, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	e := evt("no ip here")
	if err := p.Apply(&e); err == nil {
		t.Fatal("expected the pipeline to fail")
	}
	if _, ok := e.Attributes["marker"]; ok {
		t.Error("stages after the failure still ran")
	}
}

// TestMatchSelectsEvents: the match expression is the ordinary query language,
// so a pipeline can be scoped without a second syntax to learn.
func TestMatchSelectsEvents(t *testing.T) {
	spec := Spec{Name: "nginx only", Enabled: true, Match: "service=nginx",
		Stages: []StageSpec{{Type: StageSet, To: "touched", Value: "yes"}}}
	p, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	nginx := evt("x")
	nginx.Service = "nginx"
	other := evt("x")
	other.Service = "api"

	if !p.Matches(nginx) {
		t.Error("a matching event was skipped")
	}
	if p.Matches(other) {
		t.Error("a non-matching event was selected")
	}
}

func TestRenameRemoveSet(t *testing.T) {
	e := evt("x")
	e.Attributes = map[string]any{"old": "v", "junk": "drop me"}
	spec := Spec{Name: "tidy", Enabled: true, Stages: []StageSpec{
		{Type: StageRename, Field: "attr.old", To: "new"},
		{Type: StageRemove, Fields: []string{"junk"}},
		{Type: StageSet, To: "env", Value: "prod"},
	}}
	got := run(t, spec, e)

	if got.Attributes["new"] != "v" {
		t.Errorf("rename target = %v", got.Attributes["new"])
	}
	if _, ok := got.Attributes["old"]; ok {
		t.Error("rename left the source attribute behind")
	}
	if _, ok := got.Attributes["junk"]; ok {
		t.Error("remove did not delete the attribute")
	}
	if got.Attributes["env"] != "prod" {
		t.Errorf("set = %v", got.Attributes["env"])
	}
}

func TestCompileRejectsBadSpecs(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"no stages", Spec{Name: "x"}},
		{"unknown stage", Spec{Name: "x", Stages: []StageSpec{{Type: "teleport"}}}},
		{"unknown grok pattern", Spec{Name: "x", Stages: []StageSpec{{Type: StageGrok, Pattern: "%{NOSUCHPATTERN:f}"}}}},
		{"bad regex", Spec{Name: "x", Stages: []StageSpec{{Type: StageRegex, Pattern: "([unclosed"}}}},
		{"bad match", Spec{Name: "x", Match: `unterminated="`, Stages: []StageSpec{{Type: StageSet, To: "a", Value: "b"}}}},
		{"aggregation in match", Spec{Name: "x", Match: "| stats count", Stages: []StageSpec{{Type: StageSet, To: "a", Value: "b"}}}},
		{"rename without target", Spec{Name: "x", Stages: []StageSpec{{Type: StageRename, Field: "a"}}}},
	} {
		if _, err := Compile(tc.spec); err == nil {
			t.Errorf("%s: expected a compile error", tc.name)
		}
	}
}

// TestGrokFieldNamesWithDots: dots are natural in field names but illegal as a
// Go capture-group name, so they have to survive the round trip.
func TestGrokFieldNamesWithDots(t *testing.T) {
	spec := Spec{Name: "dots", Enabled: true, Stages: []StageSpec{
		{Type: StageGrok, Pattern: `%{INT:http.status} %{WORD:http.method}`},
	}}
	got := run(t, spec, evt("404 GET"))
	if got.Attributes["http.status"] != float64(404) {
		t.Errorf("http.status = %#v", got.Attributes["http.status"])
	}
	if got.Attributes["http.method"] != "GET" {
		t.Errorf("http.method = %#v", got.Attributes["http.method"])
	}
}

func TestSetApplyIsHotSwappable(t *testing.T) {
	set := NewSet(nil)
	if set.Len() != 0 {
		t.Fatal("a new set should be empty")
	}
	e := evt("hello")
	set.Apply(&e) // must be a no-op, not a panic

	if err := set.Replace([]Spec{{
		Name: "p1", Enabled: true,
		Stages: []StageSpec{{Type: StageSet, To: "a", Value: "1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	set.Apply(&e)
	if e.Attributes["a"] != float64(1) {
		t.Errorf("pipeline was not applied: %#v", e.Attributes)
	}

	// A disabled spec is not installed.
	if err := set.Replace([]Spec{{Name: "p1", Enabled: false,
		Stages: []StageSpec{{Type: StageSet, To: "b", Value: "2"}}}}); err != nil {
		t.Fatal(err)
	}
	if set.Len() != 0 {
		t.Errorf("a disabled pipeline was installed")
	}
}

// TestReplaceIsAllOrNothing: a half-applied configuration is harder to reason
// about than a rejected one.
func TestReplaceIsAllOrNothing(t *testing.T) {
	set := NewSet(nil)
	good := Spec{Name: "good", Enabled: true, Stages: []StageSpec{{Type: StageSet, To: "a", Value: "1"}}}
	if err := set.Replace([]Spec{good}); err != nil {
		t.Fatal(err)
	}
	bad := Spec{Name: "bad", Enabled: true, Stages: []StageSpec{{Type: StageGrok, Pattern: "%{NOPE:x}"}}}
	if err := set.Replace([]Spec{good, bad}); err == nil {
		t.Fatal("expected the replace to fail")
	}
	if set.Len() != 1 {
		t.Fatalf("a failed replace changed the active set (len = %d)", set.Len())
	}
}

func TestPipelineOrder(t *testing.T) {
	set := NewSet(nil)
	err := set.Replace([]Spec{
		{Name: "second", Order: 2, Enabled: true, Stages: []StageSpec{{Type: StageSet, To: "who", Value: "second"}}},
		{Name: "first", Order: 1, Enabled: true, Stages: []StageSpec{{Type: StageSet, To: "who", Value: "first"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := evt("x")
	set.Apply(&e)
	// Both write the same attribute; the higher Order runs later and wins.
	if e.Attributes["who"] != "second" {
		t.Errorf("who = %v, want the higher-Order pipeline to run last", e.Attributes["who"])
	}
}

func TestTestHelperReportsMatchesWithoutInstalling(t *testing.T) {
	specs := []Spec{{Name: "p", Enabled: true, Match: "service=api",
		Stages: []StageSpec{{Type: StageGrok, Pattern: `%{INT:code}`}}}}
	e := evt("500")
	e.Service = "api"

	res, err := Test(specs, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matched) != 1 || res.Matched[0] != "p" {
		t.Errorf("matched = %v", res.Matched)
	}
	if res.Event.Attributes["code"] != float64(500) {
		t.Errorf("dry run did not extract: %#v", res.Event.Attributes)
	}
}

func TestPatternNamesIncludeTheEssentials(t *testing.T) {
	names := strings.Join(PatternNames(), ",")
	for _, want := range []string{"IP", "HTTPDATE", "LOGLEVEL", "TIMESTAMP_ISO8601", "GREEDYDATA"} {
		if !strings.Contains(names, want) {
			t.Errorf("pattern library is missing %s", want)
		}
	}
}
