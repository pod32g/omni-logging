package alert

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Store persists rules and channels.
type Store interface {
	ListRules(ctx context.Context) ([]Rule, error)
	SaveRuleState(ctx context.Context, r Rule) error
	ListChannels(ctx context.Context) ([]Channel, error)
}

// tickInterval is how often the scheduler wakes to look for due rules. Each
// rule still runs on its own interval; this only bounds the scheduling
// granularity.
const tickInterval = 5 * time.Second

// Scheduler evaluates due rules and notifies on state transitions.
//
// Notifications are sent on transitions only — entering firing, and returning
// to ok — not on every evaluation. A rule that stays broken for an hour
// produces two messages, not one per interval, which is the difference between
// an alert people read and one they filter away.
type Scheduler struct {
	store    Store
	runner   Runner
	notifier *Notifier
	logger   *slog.Logger
	now      func() time.Time

	mu   sync.Mutex
	next map[string]time.Time // rule ID -> earliest next evaluation

	wg   sync.WaitGroup
	stop chan struct{}
	once sync.Once
}

// SchedulerOptions configures a Scheduler.
type SchedulerOptions struct {
	Store    Store
	Runner   Runner
	Notifier *Notifier
	Logger   *slog.Logger
	Now      func() time.Time
}

// NewScheduler creates a Scheduler.
func NewScheduler(o SchedulerOptions) *Scheduler {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Notifier == nil {
		o.Notifier = NewNotifier(nil)
	}
	return &Scheduler{
		store:    o.Store,
		runner:   o.Runner,
		notifier: o.Notifier,
		logger:   o.Logger,
		now:      o.Now,
		next:     map[string]time.Time{},
		stop:     make(chan struct{}),
	}
}

// Start runs the scheduler until Stop is called.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go s.run()
}

// Stop halts the scheduler and waits for the current pass to finish.
func (s *Scheduler) Stop() {
	s.once.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *Scheduler) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.RunDue(context.Background())
		}
	}
}

// RunDue evaluates every rule whose interval has elapsed. It is exported so a
// test can drive the scheduler deterministically instead of waiting on wall
// time.
func (s *Scheduler) RunDue(ctx context.Context) {
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		s.logger.Error("alert: listing rules failed", "error", err)
		return
	}
	channels, err := s.store.ListChannels(ctx)
	if err != nil {
		s.logger.Error("alert: listing channels failed", "error", err)
		return
	}
	byID := make(map[string]Channel, len(channels))
	for _, c := range channels {
		byID[c.ID] = c
	}

	now := s.now()
	for _, rule := range rules {
		if !rule.Enabled || !s.due(rule, now) {
			continue
		}
		s.evaluateAndNotify(ctx, rule, byID, now)
	}
}

// due reports whether a rule's interval has elapsed, and claims the slot.
func (s *Scheduler) due(r Rule, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at, ok := s.next[r.ID]; ok && now.Before(at) {
		return false
	}
	s.next[r.ID] = now.Add(r.Interval)
	return true
}

func (s *Scheduler) evaluateAndNotify(ctx context.Context, rule Rule, channels map[string]Channel, now time.Time) {
	ev, err := Evaluate(ctx, s.runner, rule, now)

	prev := rule.State
	rule.LastEval = now
	if err != nil {
		// A failing rule goes to unknown and keeps its error, rather than
		// silently reporting ok — "we could not tell" is not the same as "fine".
		rule.State = StateUnknown
		rule.LastError = err.Error()
		s.logger.Warn("alert: evaluation failed", "rule", rule.Name, "error", err)
		s.persist(ctx, rule)
		return
	}
	rule.LastError = ""
	rule.LastValue = ev.Value
	if ev.Firing {
		rule.State = StateFiring
	} else {
		rule.State = StateOK
	}
	if rule.State != prev {
		rule.StateFrom = now
	}
	s.persist(ctx, rule)

	// Notify only on a transition into or out of firing. Coming back from
	// unknown to ok is not a resolution anyone asked about, so it stays quiet.
	transitionedToFiring := rule.State == StateFiring && prev != StateFiring
	transitionedToOK := rule.State == StateOK && prev == StateFiring
	if !transitionedToFiring && !transitionedToOK {
		return
	}

	severity := rule.Severity
	if severity == "" {
		// Rules stored before severity existed carry none; treat them as the
		// default rather than sending an empty string downstream, where it
		// would fall through every severity-matching route.
		severity = DefaultSeverity
	}
	note := Notification{
		Rule:      rule.Name,
		RuleID:    rule.ID,
		State:     rule.State,
		Severity:  severity,
		Value:     ev.Value,
		Condition: rule.Cond.String(),
		Query:     rule.Query,
		Window:    rule.Window.String(),
		At:        now,
		Groups:    ev.Groups,
	}
	s.dispatch(ctx, rule, channels, note)
}

// dispatch delivers to every configured channel. One failing channel must not
// stop the others, and a delivery failure is logged rather than retried: the
// next transition will notify again, and a retry storm against a broken webhook
// helps nobody.
func (s *Scheduler) dispatch(ctx context.Context, rule Rule, channels map[string]Channel, note Notification) {
	if len(rule.Channels) == 0 {
		s.logger.Info("alert: state changed but the rule has no channels",
			"rule", rule.Name, "state", note.State)
		return
	}
	for _, id := range rule.Channels {
		ch, ok := channels[id]
		if !ok {
			s.logger.Warn("alert: rule references a channel that no longer exists",
				"rule", rule.Name, "channel_id", id)
			continue
		}
		if err := s.notifier.Send(ctx, ch, note); err != nil {
			s.logger.Error("alert: notification failed",
				"rule", rule.Name, "channel", ch.Name, "error", err)
			continue
		}
		s.logger.Info("alert: notified", "rule", rule.Name, "channel", ch.Name, "state", note.State)
	}
}

func (s *Scheduler) persist(ctx context.Context, rule Rule) {
	if err := s.store.SaveRuleState(ctx, rule); err != nil {
		s.logger.Error("alert: persisting rule state failed", "rule", rule.Name, "error", err)
	}
}
