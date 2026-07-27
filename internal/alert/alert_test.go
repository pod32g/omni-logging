package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/query"
	"github.com/pod32g/omni-logging/internal/store"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// fakeRunner answers queries from canned results and records what it was asked.
type fakeRunner struct {
	search    store.SearchResult
	agg       store.AggResult
	searchErr error
	aggErr    error

	mu      sync.Mutex
	lastQ   query.Query
	nSearch int
	nAgg    int
}

func (f *fakeRunner) Search(_ context.Context, q query.Query) (store.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQ, f.nSearch = q, f.nSearch+1
	return f.search, f.searchErr
}

func (f *fakeRunner) Aggregate(_ context.Context, q query.Query) (store.AggResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQ, f.nAgg = q, f.nAgg+1
	return f.agg, f.aggErr
}

func baseRule() Rule {
	return Rule{
		ID: "r1", Name: "errors", Query: "level=error",
		Window: 5 * time.Minute, Interval: time.Minute,
		Cond: Condition{Op: OpGT, Value: 10}, Enabled: true,
	}
}

func TestConditionMatch(t *testing.T) {
	for _, tc := range []struct {
		op   Op
		thr  float64
		v    float64
		want bool
	}{
		{OpGT, 10, 11, true}, {OpGT, 10, 10, false},
		{OpGTE, 10, 10, true}, {OpLT, 10, 9, true},
		{OpLTE, 10, 10, true}, {OpEQ, 10, 10, true},
		{OpNE, 10, 11, true}, {OpNE, 10, 10, false},
	} {
		c := Condition{Op: tc.op, Value: tc.thr}
		if got := c.Match(tc.v); got != tc.want {
			t.Errorf("%v %s: got %v, want %v", tc.v, c, got, tc.want)
		}
	}
}

// TestEvaluateCountUsesTotal: a non-aggregating rule compares how many events
// matched, not how many rows happened to be returned.
func TestEvaluateCountUsesTotal(t *testing.T) {
	r := &fakeRunner{search: store.SearchResult{Total: 42, Count: 1}}
	ev, err := Evaluate(context.Background(), r, baseRule(), now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Value != 42 || !ev.Firing {
		t.Fatalf("evaluation = %+v, want value 42 firing", ev)
	}
	if r.nSearch != 1 || r.nAgg != 0 {
		t.Errorf("expected exactly one Search and no Aggregate, got %d/%d", r.nSearch, r.nAgg)
	}
}

// TestEvaluateWindowOverridesTheQuery: the rule's window must win, or a saved
// query with an absolute range would freeze the alert on stale data forever.
func TestEvaluateWindowOverridesTheQuery(t *testing.T) {
	rule := baseRule()
	rule.Query = "level=error"
	rule.Window = 15 * time.Minute
	r := &fakeRunner{search: store.SearchResult{Total: 0}}

	if _, err := Evaluate(context.Background(), r, rule, now); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastQ.From.Equal(now.Add(-15 * time.Minute)) {
		t.Errorf("query From = %s, want the rule window start", r.lastQ.From)
	}
	if !r.lastQ.To.Equal(now) {
		t.Errorf("query To = %s, want now", r.lastQ.To)
	}
}

// TestEvaluateAggregationReportsBreachingGroups: knowing which service broke is
// the difference between an actionable alert and a shrug.
func TestEvaluateAggregationReportsBreachingGroups(t *testing.T) {
	rule := baseRule()
	rule.Query = "level=error | stats count by service"
	r := &fakeRunner{agg: store.AggResult{
		Columns:      []string{"service", "count"},
		GroupColumns: 1,
		Rows: [][]any{
			{"api", int64(50)},
			{"web", int64(12)},
			{"db", int64(3)},
		},
	}}

	ev, err := Evaluate(context.Background(), r, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Firing {
		t.Fatal("expected firing")
	}
	if ev.Value != 50 {
		t.Errorf("value = %v, want the most extreme breaching value 50", ev.Value)
	}
	if len(ev.Groups) != 2 {
		t.Fatalf("groups = %+v, want the two above the threshold", ev.Groups)
	}
	if ev.Groups[0].Labels["service"] != "api" {
		t.Errorf("first group = %+v", ev.Groups[0])
	}
	if got := ev.Groups[0].Label(); got != "service=api" {
		t.Errorf("label = %q", got)
	}
}

// TestEvaluateAggregationNotFiring reports the extreme value even when nothing
// breached, so a notification-free evaluation still records something useful.
func TestEvaluateAggregationNotFiring(t *testing.T) {
	rule := baseRule()
	rule.Query = "| stats count by service"
	r := &fakeRunner{agg: store.AggResult{
		Columns: []string{"service", "count"}, GroupColumns: 1,
		Rows: [][]any{{"api", int64(4)}, {"web", int64(9)}},
	}}
	ev, err := Evaluate(context.Background(), r, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Firing {
		t.Fatal("nothing crossed the threshold; must not fire")
	}
	if ev.Value != 9 {
		t.Errorf("value = %v, want the highest observed 9", ev.Value)
	}
}

// TestEvaluateEmptyAggregateIsZero supports the dead-man's switch: "count < 1"
// should fire when a service stops logging entirely.
func TestEvaluateEmptyAggregateIsZero(t *testing.T) {
	rule := baseRule()
	rule.Query = "service=heartbeat | stats count"
	rule.Cond = Condition{Op: OpLT, Value: 1}
	r := &fakeRunner{agg: store.AggResult{Columns: []string{"count"}, GroupColumns: 0}}

	ev, err := Evaluate(context.Background(), r, rule, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Firing || ev.Value != 0 {
		t.Fatalf("evaluation = %+v, want firing at 0 for an absent service", ev)
	}
}

func TestEvaluateRejectsBadQuery(t *testing.T) {
	rule := baseRule()
	rule.Query = "| stats bogus(x)"
	if _, err := Evaluate(context.Background(), &fakeRunner{}, rule, now); err == nil {
		t.Fatal("expected an error for an unparseable query")
	}
}

func TestRuleValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Rule)
	}{
		{"no name", func(r *Rule) { r.Name = "" }},
		{"no query", func(r *Rule) { r.Query = "" }},
		{"bad op", func(r *Rule) { r.Cond.Op = "sideways" }},
		{"interval too small", func(r *Rule) { r.Interval = time.Second }},
		{"window too small", func(r *Rule) { r.Window = time.Second; r.Interval = MinInterval }},
		{"window below interval", func(r *Rule) { r.Window = time.Minute; r.Interval = 5 * time.Minute }},
	} {
		r := baseRule()
		tc.mut(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", tc.name)
		}
	}
	valid := baseRule()
	if err := valid.Validate(); err != nil {
		t.Errorf("valid rule rejected: %v", err)
	}
}

func TestChannelValidate(t *testing.T) {
	for _, c := range []Channel{
		{Name: "", Type: ChannelWebhook, URL: "https://x"},
		{Name: "n", Type: "carrier-pigeon", URL: "https://x"},
		{Name: "n", Type: ChannelWebhook, URL: "ftp://x"},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("expected %+v to be rejected", c)
		}
	}
	ok := Channel{Name: "ops", Type: ChannelSlack, URL: "https://hooks.example/x"}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid channel rejected: %v", err)
	}
}

// --- notification -----------------------------------------------------------

func TestNotifierWebhookAndSlack(t *testing.T) {
	type capture struct {
		contentType string
		body        []byte
	}
	got := make(chan capture, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		got <- capture{r.Header.Get("Content-Type"), b}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := NewNotifier(nil)
	note := Notification{
		Rule: "errors", State: StateFiring, Value: 50,
		Condition: "> 10", Query: "level=error", Window: "5m", At: now,
		Groups: []GroupHit{{Labels: map[string]string{"service": "api"}, Value: 50}},
	}

	// A webhook receives the structured payload.
	if err := n.Send(context.Background(), Channel{Name: "hook", Type: ChannelWebhook, URL: srv.URL}, note); err != nil {
		t.Fatalf("webhook send: %v", err)
	}
	c := <-got
	var decoded Notification
	if err := json.Unmarshal(c.body, &decoded); err != nil {
		t.Fatalf("webhook payload is not the notification JSON: %v (%s)", err, c.body)
	}
	if decoded.Rule != "errors" || decoded.Value != 50 || len(decoded.Groups) != 1 {
		t.Errorf("decoded payload = %+v", decoded)
	}

	// Slack receives a text field it can render.
	if err := n.Send(context.Background(), Channel{Name: "slack", Type: ChannelSlack, URL: srv.URL}, note); err != nil {
		t.Fatalf("slack send: %v", err)
	}
	c = <-got
	var slack map[string]string
	if err := json.Unmarshal(c.body, &slack); err != nil {
		t.Fatalf("slack payload: %v", err)
	}
	if !strings.Contains(slack["text"], "FIRING") || !strings.Contains(slack["text"], "service=api") {
		t.Errorf("slack text lacks the essentials: %q", slack["text"])
	}
}

func TestNotifierReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no thanks", http.StatusForbidden)
	}))
	defer srv.Close()

	err := NewNotifier(nil).Send(context.Background(),
		Channel{Name: "hook", Type: ChannelWebhook, URL: srv.URL}, Notification{Rule: "x"})
	if err == nil {
		t.Fatal("a non-2xx response must be reported")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should name the status: %v", err)
	}
}

// TestNotifierDoesNotFollowRedirects: a channel URL is operator-supplied, and
// quietly following a redirect would deliver the payload somewhere else.
func TestNotifierDoesNotFollowRedirects(t *testing.T) {
	var elsewhereHit bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	err := NewNotifier(nil).Send(context.Background(),
		Channel{Name: "hook", Type: ChannelWebhook, URL: redirector.URL}, Notification{Rule: "x"})
	if err == nil {
		t.Error("a redirect should surface as a non-2xx, not be followed silently")
	}
	if elsewhereHit {
		t.Fatal("the notification was delivered to the redirect target")
	}
}

func TestNotificationText(t *testing.T) {
	n := Notification{
		Rule: "errors", State: StateOK, Value: 2, Condition: "> 10",
		Query: "level=error", Window: "5m",
	}
	if txt := n.Text(); !strings.Contains(txt, "RESOLVED") {
		t.Errorf("resolved notification should say so: %q", txt)
	}
	// Long group lists are truncated so a chat message stays readable.
	n.State = StateFiring
	for i := 0; i < 9; i++ {
		n.Groups = append(n.Groups, GroupHit{Labels: map[string]string{"service": "s"}, Value: float64(i)})
	}
	if txt := n.Text(); !strings.Contains(txt, "and 4 more") {
		t.Errorf("expected the group list to be truncated: %q", txt)
	}
}

// --- Omni-Notify channel ----------------------------------------------------

// omniNotifyReceiver stands in for Omni-Notify's event API.
type omniNotifyReceiver struct {
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	auths  []string
	events []map[string]any
}

func newOmniNotifyReceiver(t *testing.T) *omniNotifyReceiver {
	t.Helper()
	r := &omniNotifyReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Omni-Notify rejects anything without a bearer token, so reproduce that
		// here: a test that accepted unauthenticated posts would not notice the
		// header going missing.
		if req.Header.Get("Authorization") == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var ev map[string]any
		if err := json.NewDecoder(req.Body).Decode(&ev); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.paths = append(r.paths, req.URL.Path)
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.events = append(r.events, ev)
		r.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *omniNotifyReceiver) last() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return nil
	}
	return r.events[len(r.events)-1]
}

func omniNote(state State) Notification {
	return Notification{
		Rule: "checkout errors", RuleID: "rule-123",
		State: state, Severity: SeverityCritical, Value: 50,
		Condition: "> 10", Query: "level=error service=checkout", Window: "5m", At: now,
		Groups: []GroupHit{{Labels: map[string]string{"service": "checkout"}, Value: 50}},
	}
}

func TestOmniNotifyChannelPayload(t *testing.T) {
	rec := newOmniNotifyReceiver(t)
	ch := Channel{Name: "notify", Type: ChannelOmniNotify, URL: rec.srv.URL, Token: "tok"}

	if err := NewNotifier(nil).Send(context.Background(), ch, omniNote(StateFiring)); err != nil {
		t.Fatalf("send: %v", err)
	}
	ev := rec.last()

	for k, want := range map[string]any{
		"event_id": "rule-123",
		"type":     "alert",
		"source":   "omni-logging",
		"status":   "firing",
		"severity": "critical",
		"title":    "checkout errors",
	} {
		if ev[k] != want {
			t.Errorf("%s = %#v, want %#v", k, ev[k], want)
		}
	}
	if ev["summary"] == "" || ev["description"] == "" {
		t.Errorf("summary/description should be populated: %#v", ev)
	}

	// The bearer token must actually be sent; Omni-Notify 401s without it.
	rec.mu.Lock()
	auth := rec.auths[0]
	path := rec.paths[0]
	rec.mu.Unlock()
	if auth != "Bearer tok" {
		t.Errorf("Authorization = %q", auth)
	}
	if path != "/api/v1/events" {
		t.Errorf("posted to %q, want the events endpoint", path)
	}
}

// TestOmniNotifyLabelsAreStableAcrossTransitions is the property the whole
// mapping is built around. Omni-Notify fingerprints on
// sha256(type|source|event_id|sorted labels); if a label differed between
// firing and resolved, the resolve would look like a resolve for something
// that never fired, be suppressed, and the alert would stay active forever.
func TestOmniNotifyLabelsAreStableAcrossTransitions(t *testing.T) {
	rec := newOmniNotifyReceiver(t)
	ch := Channel{Name: "notify", Type: ChannelOmniNotify, URL: rec.srv.URL, Token: "tok"}
	n := NewNotifier(nil)

	firing := omniNote(StateFiring)
	if err := n.Send(context.Background(), ch, firing); err != nil {
		t.Fatal(err)
	}
	firingEvent := rec.last()

	// The resolve reports a different value and different groups, as a real
	// recovery would.
	resolved := omniNote(StateOK)
	resolved.Value = 0
	resolved.Groups = nil
	if err := n.Send(context.Background(), ch, resolved); err != nil {
		t.Fatal(err)
	}
	resolvedEvent := rec.last()

	if firingEvent["status"] != "firing" || resolvedEvent["status"] != "resolved" {
		t.Fatalf("statuses = %v / %v", firingEvent["status"], resolvedEvent["status"])
	}
	if firingEvent["event_id"] != resolvedEvent["event_id"] {
		t.Errorf("event_id changed between transitions: %v -> %v",
			firingEvent["event_id"], resolvedEvent["event_id"])
	}

	fl, _ := json.Marshal(firingEvent["labels"])
	rl, _ := json.Marshal(resolvedEvent["labels"])
	if string(fl) != string(rl) {
		t.Errorf("labels differ between firing and resolved, so the resolve would\n"+
			"fingerprint differently and be suppressed:\n  firing:   %s\n  resolved: %s", fl, rl)
	}

	// The value *did* change, and must therefore live in annotations, which are
	// matchable but outside the fingerprint.
	fa, _ := firingEvent["annotations"].(map[string]any)
	ra, _ := resolvedEvent["annotations"].(map[string]any)
	if fa["value"] == ra["value"] {
		t.Errorf("the observed value should differ between transitions: %v", fa["value"])
	}
	if _, leaked := firingEvent["labels"].(map[string]any)["value"]; leaked {
		t.Error("the observed value is in labels; it must be in annotations or dedup breaks")
	}
}

func TestOmniNotifyAcceptsBaseOrFullURL(t *testing.T) {
	for _, suffix := range []string{"", "/", "/api/v1/events", "/api/v1/events/"} {
		rec := newOmniNotifyReceiver(t)
		ch := Channel{Name: "n", Type: ChannelOmniNotify, URL: rec.srv.URL + suffix, Token: "tok"}
		if err := NewNotifier(nil).Send(context.Background(), ch, omniNote(StateFiring)); err != nil {
			t.Fatalf("URL suffix %q: %v", suffix, err)
		}
		rec.mu.Lock()
		got := rec.paths[0]
		rec.mu.Unlock()
		if got != "/api/v1/events" {
			t.Errorf("URL suffix %q posted to %q", suffix, got)
		}
	}
}

func TestOmniNotifyDefaultsSeverity(t *testing.T) {
	rec := newOmniNotifyReceiver(t)
	ch := Channel{Name: "n", Type: ChannelOmniNotify, URL: rec.srv.URL, Token: "tok"}
	note := omniNote(StateFiring)
	note.Severity = "" // a rule stored before severity existed

	if err := NewNotifier(nil).Send(context.Background(), ch, note); err != nil {
		t.Fatal(err)
	}
	if got := rec.last()["severity"]; got != string(DefaultSeverity) {
		t.Errorf("severity = %v, want the default %q — an empty severity would fall\n"+
			"through every severity-matching route downstream", got, DefaultSeverity)
	}
}

func TestOmniNotifyChannelRequiresToken(t *testing.T) {
	ch := Channel{Name: "n", Type: ChannelOmniNotify, URL: "http://notify:8088"}
	if err := ch.Validate(); err == nil {
		t.Fatal("an omni-notify channel without a token must be rejected at save time")
	}
	ch.Token = "tok"
	if err := ch.Validate(); err != nil {
		t.Fatalf("with a token it should validate: %v", err)
	}
}

// TestChannelTokenIsMasked: the channels endpoint is unauthenticated when no
// admin token is set, so a token returned verbatim would be readable by anyone
// who can reach the port.
func TestChannelTokenIsMasked(t *testing.T) {
	c := Channel{Name: "n", Type: ChannelOmniNotify, URL: "http://notify:8088", Token: "super-secret"}
	m := c.Masked()
	if m.Token == "super-secret" {
		t.Fatal("the token was returned in the clear")
	}
	if m.Token != TokenMask {
		t.Errorf("masked token = %q", m.Token)
	}
	if c.Token != "super-secret" {
		t.Error("Masked must not mutate the receiver")
	}

	// And posting the mask back must be refused rather than stored.
	if err := m.Validate(); err == nil {
		t.Error("saving a channel whose token is the mask must be rejected")
	}
}

func TestRuleValidateSeverity(t *testing.T) {
	base := func() Rule {
		return Rule{Name: "r", Query: "level=error", Interval: time.Minute,
			Window: 5 * time.Minute, Cond: Condition{Op: OpGT, Value: 1}}
	}
	r := base()
	if err := r.Validate(); err != nil {
		t.Fatalf("an empty severity should default rather than fail: %v", err)
	}
	if r.Severity != DefaultSeverity {
		t.Errorf("severity = %q, want the default %q", r.Severity, DefaultSeverity)
	}

	r = base()
	r.Severity = "catastrophic"
	if err := r.Validate(); err == nil {
		t.Error("an unknown severity must be rejected; downstream routing matches on it exactly")
	}

	for _, s := range []Severity{SeverityCritical, SeverityError, SeverityWarning, SeverityInfo, SeverityDebug} {
		r = base()
		r.Severity = s
		if err := r.Validate(); err != nil {
			t.Errorf("severity %q should be valid: %v", s, err)
		}
	}
}

// TestEvaluateWindowBeatsTimeInTheQuery: now that `last=` in an expression
// actually resolves, a rule whose query carries one must still be evaluated over
// the rule's own window. Otherwise a rule written as "level=error last=1h" with
// a 5m window would silently examine twelve times the intended range.
func TestEvaluateWindowBeatsTimeInTheQuery(t *testing.T) {
	rule := baseRule()
	rule.Query = "level=error last=1h"
	rule.Window = 5 * time.Minute
	r := &fakeRunner{search: store.SearchResult{Total: 0}}

	if _, err := Evaluate(context.Background(), r, rule, now); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastQ.From.Equal(now.Add(-5 * time.Minute)) {
		t.Errorf("query From = %s, want the rule's 5m window, not the query's 1h", r.lastQ.From)
	}
}

// TestEvaluateRejectsBadTimeInQuery: a malformed time bound should be reported
// when the rule is saved or evaluated, not swallowed.
func TestEvaluateRejectsBadTimeInQuery(t *testing.T) {
	rule := baseRule()
	rule.Query = "level=error last=banana"
	r := &fakeRunner{search: store.SearchResult{Total: 0}}

	if _, err := Evaluate(context.Background(), r, rule, now); err == nil {
		t.Fatal("an unparseable time bound in a rule query must be an error")
	}
}

// --- channel test probes ----------------------------------------------------

// TestOmniNotifyProbeIsStateless is the regression test for a button that lied.
// The channel test used to be sent as status=firing with a fixed event_id, so
// Omni-Notify opened an incident nothing would ever resolve and then correctly
// suppressed every later probe as a repeat of the one still active — while the
// API kept returning 202, so the UI reported success and delivered nothing.
func TestOmniNotifyProbeIsStateless(t *testing.T) {
	rec := newOmniNotifyReceiver(t)
	ch := Channel{Name: "n", Type: ChannelOmniNotify, URL: rec.srv.URL, Token: "tok"}

	note := omniNote(StateFiring)
	note.Test = true
	if err := NewNotifier(nil).Send(context.Background(), ch, note); err != nil {
		t.Fatal(err)
	}

	ev := rec.last()
	if status, present := ev["status"]; present {
		t.Errorf("a probe carried status=%v; it must be stateless, or it opens an incident that never resolves", status)
	}
	if ev["type"] != "alert" || ev["source"] != "omni-logging" {
		t.Errorf("routing fields should be unchanged: %#v", ev)
	}
}

// TestOmniNotifyProbesAreDistinct: stateless events dedupe on the route's
// window, so two probes sharing a fingerprint would collapse into one delivery.
// Pressing Test twice must send twice.
func TestOmniNotifyProbesAreDistinct(t *testing.T) {
	rec := newOmniNotifyReceiver(t)
	ch := Channel{Name: "n", Type: ChannelOmniNotify, URL: rec.srv.URL, Token: "tok"}
	n := NewNotifier(nil)

	first := omniNote(StateFiring)
	first.Test = true
	if err := n.Send(context.Background(), ch, first); err != nil {
		t.Fatal(err)
	}
	firstID := rec.last()["event_id"]

	second := first
	second.At = first.At.Add(time.Second) // a later press
	if err := n.Send(context.Background(), ch, second); err != nil {
		t.Fatal(err)
	}
	secondID := rec.last()["event_id"]

	if firstID == secondID {
		t.Errorf("both probes used event_id %v, so the second would be deduped away", firstID)
	}
	if !strings.HasPrefix(firstID.(string), "test-") {
		t.Errorf("probe event_id = %v, want a test- prefix so it is recognisable", firstID)
	}
}

// TestRealAlertsKeepTheirLifecycle: the probe change must not leak into real
// notifications, which depend on firing/resolved pairing to clear.
func TestRealAlertsKeepTheirLifecycle(t *testing.T) {
	rec := newOmniNotifyReceiver(t)
	ch := Channel{Name: "n", Type: ChannelOmniNotify, URL: rec.srv.URL, Token: "tok"}
	n := NewNotifier(nil)

	if err := n.Send(context.Background(), ch, omniNote(StateFiring)); err != nil {
		t.Fatal(err)
	}
	firing := rec.last()
	if firing["status"] != "firing" || firing["event_id"] != "rule-123" {
		t.Errorf("a real alert must still be a stateful incident keyed by rule ID: %#v", firing)
	}

	if err := n.Send(context.Background(), ch, omniNote(StateOK)); err != nil {
		t.Fatal(err)
	}
	resolved := rec.last()
	if resolved["status"] != "resolved" || resolved["event_id"] != firing["event_id"] {
		t.Errorf("the resolve must pair with the firing: %#v", resolved)
	}
}

// TestProbeTextIsNotAnIncident: a probe rendered as FIRING or RESOLVED puts a
// fake incident in the operator's chat history.
func TestProbeTextIsNotAnIncident(t *testing.T) {
	note := omniNote(StateFiring)
	note.Test = true
	got := note.Text()
	if strings.Contains(got, "FIRING") || strings.Contains(got, "RESOLVED") {
		t.Errorf("probe text reads as an incident: %q", got)
	}
	if !strings.Contains(got, "TEST") {
		t.Errorf("probe text should say it is a test: %q", got)
	}
}
