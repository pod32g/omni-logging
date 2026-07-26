package alert

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/store"
)

// memStore is an in-memory alert.Store that records state writes.
type memStore struct {
	mu       sync.Mutex
	rules    []Rule
	channels []Channel
	saved    []Rule
}

func (m *memStore) ListRules(context.Context) ([]Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Rule(nil), m.rules...), nil
}

func (m *memStore) ListChannels(context.Context) ([]Channel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Channel(nil), m.channels...), nil
}

func (m *memStore) SaveRuleState(_ context.Context, r Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, r)
	for i := range m.rules {
		if m.rules[i].ID == r.ID {
			m.rules[i].State = r.State
			m.rules[i].StateFrom = r.StateFrom
			m.rules[i].LastValue = r.LastValue
			m.rules[i].LastError = r.LastError
			m.rules[i].LastEval = r.LastEval
		}
	}
	return nil
}

func (m *memStore) lastState() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.saved) == 0 {
		return StateUnknown
	}
	return m.saved[len(m.saved)-1].State
}

// recorder counts notifications delivered to a test channel.
type recorder struct {
	mu    sync.Mutex
	notes []Notification
	srv   *httptest.Server
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var n Notification
		_ = decode(body, &n)
		r.mu.Lock()
		r.notes = append(r.notes, n)
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.notes)
}

func (r *recorder) states() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]State, 0, len(r.notes))
	for _, n := range r.notes {
		out = append(out, n.State)
	}
	return out
}

func decode(b []byte, v any) error { return json.Unmarshal(b, v) }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// schedFixture wires a scheduler over a controllable clock and runner.
func schedFixture(t *testing.T, rule Rule) (*Scheduler, *memStore, *fakeRunner, *recorder, *time.Time) {
	t.Helper()
	rec := newRecorder(t)
	ch := Channel{ID: "c1", Name: "hook", Type: ChannelWebhook, URL: rec.srv.URL}
	rule.Channels = []string{ch.ID}
	st := &memStore{rules: []Rule{rule}, channels: []Channel{ch}}
	runner := &fakeRunner{}
	clock := now

	s := NewScheduler(SchedulerOptions{
		Store: st, Runner: runner, Logger: quietLogger(),
		Now: func() time.Time { return clock },
	})
	return s, st, runner, rec, &clock
}

// TestSchedulerNotifiesOnlyOnTransitions is the property that decides whether
// alerts get read or filtered: a rule that stays broken must not notify on
// every interval.
func TestSchedulerNotifiesOnlyOnTransitions(t *testing.T) {
	rule := baseRule()
	s, st, runner, rec, clock := schedFixture(t, rule)

	// Below threshold: no notification, state ok.
	runner.search = store.SearchResult{Total: 1}
	s.RunDue(context.Background())
	if rec.count() != 0 {
		t.Fatalf("notified while healthy: %+v", rec.states())
	}
	if st.lastState() != StateOK {
		t.Fatalf("state = %s, want ok", st.lastState())
	}

	// Crosses the threshold: exactly one firing notification.
	runner.search = store.SearchResult{Total: 99}
	*clock = clock.Add(time.Minute)
	s.RunDue(context.Background())
	if rec.count() != 1 {
		t.Fatalf("expected one firing notification, got %d", rec.count())
	}

	// Still broken over several intervals: still just the one notification.
	for i := 0; i < 3; i++ {
		*clock = clock.Add(time.Minute)
		s.RunDue(context.Background())
	}
	if rec.count() != 1 {
		t.Fatalf("a continuously firing rule notified %d times; it must notify once per transition", rec.count())
	}

	// Recovers: one resolved notification.
	runner.search = store.SearchResult{Total: 0}
	*clock = clock.Add(time.Minute)
	s.RunDue(context.Background())
	if got := rec.states(); len(got) != 2 || got[0] != StateFiring || got[1] != StateOK {
		t.Fatalf("notification states = %v, want [firing ok]", got)
	}
}

// TestSchedulerRespectsInterval: a rule must not be evaluated more often than
// its interval, however often the scheduler ticks.
func TestSchedulerRespectsInterval(t *testing.T) {
	rule := baseRule()
	rule.Interval = 5 * time.Minute
	rule.Window = 5 * time.Minute
	s, _, runner, _, clock := schedFixture(t, rule)
	runner.search = store.SearchResult{Total: 0}

	s.RunDue(context.Background()) // first run always happens
	for i := 0; i < 5; i++ {
		*clock = clock.Add(30 * time.Second)
		s.RunDue(context.Background())
	}
	runner.mu.Lock()
	n := runner.nSearch
	runner.mu.Unlock()
	if n != 1 {
		t.Fatalf("evaluated %d times inside one interval, want 1", n)
	}

	*clock = clock.Add(5 * time.Minute)
	s.RunDue(context.Background())
	runner.mu.Lock()
	n = runner.nSearch
	runner.mu.Unlock()
	if n != 2 {
		t.Fatalf("evaluated %d times after the interval elapsed, want 2", n)
	}
}

func TestSchedulerSkipsDisabledRules(t *testing.T) {
	rule := baseRule()
	rule.Enabled = false
	s, _, runner, rec, _ := schedFixture(t, rule)
	runner.search = store.SearchResult{Total: 999}

	s.RunDue(context.Background())
	runner.mu.Lock()
	n := runner.nSearch
	runner.mu.Unlock()
	if n != 0 || rec.count() != 0 {
		t.Fatalf("a disabled rule was evaluated (%d) or notified (%d)", n, rec.count())
	}
}

// TestSchedulerEvaluationErrorGoesToUnknown: "we could not tell" must not be
// recorded as "fine", and must not send a resolved notification.
func TestSchedulerEvaluationErrorGoesToUnknown(t *testing.T) {
	rule := baseRule()
	s, st, runner, rec, clock := schedFixture(t, rule)

	runner.search = store.SearchResult{Total: 99}
	s.RunDue(context.Background())
	if rec.count() != 1 {
		t.Fatalf("expected the initial firing notification, got %d", rec.count())
	}

	runner.searchErr = context.DeadlineExceeded
	*clock = clock.Add(time.Minute)
	s.RunDue(context.Background())

	if st.lastState() != StateUnknown {
		t.Fatalf("state after an error = %s, want unknown", st.lastState())
	}
	if rec.count() != 1 {
		t.Fatalf("an evaluation error sent a notification; it must not resolve an alert it could not evaluate")
	}
	st.mu.Lock()
	last := st.saved[len(st.saved)-1]
	st.mu.Unlock()
	if last.LastError == "" {
		t.Error("the failure reason was not recorded on the rule")
	}
}

// TestSchedulerMissingChannelIsSkipped: a dangling channel reference must not
// stop the other channels or the evaluation.
func TestSchedulerMissingChannelIsSkipped(t *testing.T) {
	rule := baseRule()
	s, st, runner, rec, _ := schedFixture(t, rule)

	st.mu.Lock()
	st.rules[0].Channels = []string{"does-not-exist", "c1"}
	st.mu.Unlock()

	runner.search = store.SearchResult{Total: 99}
	s.RunDue(context.Background())

	if rec.count() != 1 {
		t.Fatalf("the surviving channel got %d notifications, want 1", rec.count())
	}
	if st.lastState() != StateFiring {
		t.Errorf("state = %s, want firing", st.lastState())
	}
}

func TestSchedulerStartStop(t *testing.T) {
	rule := baseRule()
	s, _, runner, _, _ := schedFixture(t, rule)
	runner.search = store.SearchResult{Total: 0}

	s.Start()
	done := make(chan struct{})
	go func() { s.Stop(); s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung")
	}
}
