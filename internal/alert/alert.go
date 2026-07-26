// Package alert evaluates saved queries on a schedule and notifies when their
// results cross a threshold. A rule is a query plus a window plus a condition;
// everything else — grouping, filtering, aggregation — is the existing query
// language, so an alert is exactly a search you already know how to write.
package alert

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned by a Store when a rule or channel ID does not exist.
var ErrNotFound = errors.New("alert: not found")

// Op is a threshold comparison.
type Op string

const (
	OpGT  Op = "gt"
	OpGTE Op = "gte"
	OpLT  Op = "lt"
	OpLTE Op = "lte"
	OpEQ  Op = "eq"
	OpNE  Op = "ne"
)

// Condition compares an observed value against a threshold.
type Condition struct {
	Op    Op      `json:"op"`
	Value float64 `json:"value"`
}

// Match reports whether v satisfies the condition.
func (c Condition) Match(v float64) bool {
	switch c.Op {
	case OpGT:
		return v > c.Value
	case OpGTE:
		return v >= c.Value
	case OpLT:
		return v < c.Value
	case OpLTE:
		return v <= c.Value
	case OpEQ:
		return v == c.Value
	case OpNE:
		return v != c.Value
	}
	return false
}

func (c Condition) String() string {
	sym := map[Op]string{OpGT: ">", OpGTE: ">=", OpLT: "<", OpLTE: "<=", OpEQ: "==", OpNE: "!="}[c.Op]
	return fmt.Sprintf("%s %g", sym, c.Value)
}

// State is where a rule sits in its firing lifecycle.
type State string

const (
	StateOK      State = "ok"
	StateFiring  State = "firing"
	StateUnknown State = "unknown" // never evaluated, or the last evaluation errored
)

// Bounds on rule timing. An interval far below the ingest flush interval would
// evaluate the same data repeatedly; a window shorter than the interval would
// leave gaps where events are never examined by the rule at all.
const (
	MinInterval = 10 * time.Second
	MinWindow   = 10 * time.Second
	MaxWindow   = 30 * 24 * time.Hour
)

// Rule is a scheduled query with a threshold.
type Rule struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Query    string        `json:"query"` // filter, optionally with an aggregation stage
	Window   time.Duration `json:"-"`     // how far back each evaluation looks
	Interval time.Duration `json:"-"`     // how often to evaluate
	Cond     Condition     `json:"condition"`
	Channels []string      `json:"channels"` // channel IDs notified on transitions
	Enabled  bool          `json:"enabled"`

	// State is maintained by the scheduler, not the caller.
	State     State     `json:"state"`
	StateFrom time.Time `json:"state_since,omitzero"`
	LastEval  time.Time `json:"last_eval,omitzero"`
	LastValue float64   `json:"last_value"`
	LastError string    `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Validate checks a rule is coherent enough to schedule.
func (r *Rule) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("alert: name is required")
	}
	if strings.TrimSpace(r.Query) == "" {
		return fmt.Errorf("alert: query is required")
	}
	switch r.Cond.Op {
	case OpGT, OpGTE, OpLT, OpLTE, OpEQ, OpNE:
	default:
		return fmt.Errorf("alert: condition op must be one of gt, gte, lt, lte, eq, ne")
	}
	if r.Interval < MinInterval {
		return fmt.Errorf("alert: interval must be at least %s", MinInterval)
	}
	if r.Window < MinWindow || r.Window > MaxWindow {
		return fmt.Errorf("alert: window must be between %s and %s", MinWindow, MaxWindow)
	}
	if r.Window < r.Interval {
		// Otherwise each run examines a slice shorter than the gap between runs,
		// so events falling between windows are never evaluated by this rule.
		return fmt.Errorf("alert: window (%s) must be at least the interval (%s), or events between runs are never examined", r.Window, r.Interval)
	}
	return nil
}

// ChannelType is a notification transport.
type ChannelType string

const (
	ChannelWebhook ChannelType = "webhook" // POST the full JSON payload
	ChannelSlack   ChannelType = "slack"   // POST {"text": ...} to an incoming webhook
)

// Channel is a notification destination.
type Channel struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Type      ChannelType `json:"type"`
	URL       string      `json:"url"`
	CreatedAt time.Time   `json:"created_at,omitzero"`
}

// Validate checks a channel is usable.
func (c *Channel) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("alert: channel name is required")
	}
	switch c.Type {
	case ChannelWebhook, ChannelSlack:
	default:
		return fmt.Errorf("alert: channel type must be webhook or slack")
	}
	if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") {
		return fmt.Errorf("alert: channel URL must be an http(s) URL")
	}
	return nil
}

// GroupHit identifies one aggregation group that met the condition, so a
// notification can say *which* service broke rather than only that something did.
type GroupHit struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
}

// Label renders a group's labels compactly for a message line.
func (g GroupHit) Label() string {
	if len(g.Labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(g.Labels))
	for k, v := range g.Labels {
		parts = append(parts, k+"="+v)
	}
	// Stable order so repeated notifications read the same.
	sortStrings(parts)
	return strings.Join(parts, " ")
}

// Evaluation is the outcome of running a rule once.
type Evaluation struct {
	At     time.Time  `json:"at"`
	Value  float64    `json:"value"` // the value compared against the threshold
	Firing bool       `json:"firing"`
	Groups []GroupHit `json:"groups,omitempty"` // groups that met the condition
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
