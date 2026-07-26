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
