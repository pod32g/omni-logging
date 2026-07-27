package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Notification is the payload delivered on a state transition.
type Notification struct {
	Rule      string     `json:"rule"`
	RuleID    string     `json:"rule_id"`
	State     State      `json:"state"`              // firing or ok (resolved)
	Severity  Severity   `json:"severity,omitempty"` // how bad, independent of state
	Value     float64    `json:"value"`
	Condition string     `json:"condition"`
	Query     string     `json:"query"`
	Window    string     `json:"window"`
	At        time.Time  `json:"at"`
	Groups    []GroupHit `json:"groups,omitempty"`
}

// Text renders the notification as a single human line, used by chat channels
// and by any webhook consumer that would rather not parse the payload.
func (n Notification) Text() string {
	var b strings.Builder
	if n.State == StateFiring {
		b.WriteString("🔴 FIRING: ")
	} else {
		b.WriteString("✅ RESOLVED: ")
	}
	fmt.Fprintf(&b, "%s — %g %s over %s", n.Rule, n.Value, n.Condition, n.Window)
	if len(n.Groups) > 0 {
		b.WriteString("\n")
		for i, g := range n.Groups {
			if i == 5 {
				fmt.Fprintf(&b, "\n…and %d more", len(n.Groups)-5)
				break
			}
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "  • %s: %g", g.Label(), g.Value)
		}
	}
	fmt.Fprintf(&b, "\nquery: %s", n.Query)
	return b.String()
}

// omniNotifyEvent is Omni-Notify's ingest schema (POST /api/v1/events).
//
// Omni-Notify deduplicates on a fingerprint it derives as
// sha256(type | source | event_id | sorted labels) unless one is supplied.
// That single fact drives the whole mapping below, so it is worth stating
// plainly: **labels must be identical between a rule's firing and resolved
// events**. If a label carried the observed value, or which groups broke, the
// resolve would fingerprint differently from the firing, Omni-Notify would see
// a resolve for something that never fired, suppress it, and the alert would
// stay active there forever.
//
// So labels carry identity only. Everything that varies between evaluations
// goes in annotations, which are still matchable for routing but are not part
// of the fingerprint.
type omniNotifyEvent struct {
	EventID     string            `json:"event_id"`
	Type        string            `json:"type"`
	Source      string            `json:"source"`
	Status      string            `json:"status"`
	Severity    string            `json:"severity,omitempty"`
	Title       string            `json:"title"`
	Summary     string            `json:"summary,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Timestamp   time.Time         `json:"timestamp"`
}

// omniNotifySource identifies this server as the producing system. Omni-Notify
// routes can match on it, so it is a constant rather than the hostname: a
// route written against it must not break because the container moved.
const omniNotifySource = "omni-logging"

// toOmniNotify maps a notification onto an Omni-Notify event.
func toOmniNotify(note Notification) omniNotifyEvent {
	status := "resolved"
	if note.State == StateFiring {
		status = "firing"
	}

	severity := string(note.Severity)
	if severity == "" {
		severity = string(DefaultSeverity)
	}

	e := omniNotifyEvent{
		// The rule ID, not its name: a rule renamed while firing must still
		// resolve against the event it opened.
		EventID:  note.RuleID,
		Type:     "alert",
		Source:   omniNotifySource,
		Status:   status,
		Severity: severity,
		Title:    note.Rule,
		Summary: fmt.Sprintf("%g %s over %s",
			note.Value, note.Condition, note.Window),
		Description: note.Text(),
		// Identity only — see the type comment.
		Labels: map[string]string{
			"rule_id": note.RuleID,
			"rule":    note.Rule,
		},
		Annotations: map[string]string{
			"query":     note.Query,
			"condition": note.Condition,
			"window":    note.Window,
			"value":     strconv.FormatFloat(note.Value, 'g', -1, 64),
		},
		Timestamp: note.At,
	}
	if len(note.Groups) > 0 {
		e.Annotations["groups"] = strconv.Itoa(len(note.Groups))
		e.Annotations["top_group"] = note.Groups[0].Label()
	}
	return e
}

// omniNotifyEventsPath is where Omni-Notify ingests events.
const omniNotifyEventsPath = "/api/v1/events"

// omniNotifyEventsURL accepts either a base URL ("http://notify:8088") or the
// full endpoint, because both are the obvious thing to paste and guessing
// wrong costs the operator a failed delivery with a 404 to interpret.
func omniNotifyEventsURL(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if strings.HasSuffix(trimmed, omniNotifyEventsPath) {
		return trimmed
	}
	return trimmed + omniNotifyEventsPath
}

// notifyTimeout bounds a single delivery attempt.
const notifyTimeout = 10 * time.Second

// Notifier delivers notifications to channels.
type Notifier struct {
	client *http.Client
}

// NewNotifier creates a Notifier. The HTTP client deliberately refuses
// redirects: a channel URL is operator-supplied, and silently following a
// redirect would send the payload somewhere the operator never configured.
func NewNotifier(client *http.Client) *Notifier {
	if client == nil {
		client = &http.Client{
			Timeout: notifyTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &Notifier{client: client}
}

// Send delivers one notification to one channel.
func (n *Notifier) Send(ctx context.Context, ch Channel, note Notification) error {
	var (
		body []byte
		err  error
	)
	url := ch.URL
	switch ch.Type {
	case ChannelSlack:
		body, err = json.Marshal(map[string]string{"text": note.Text()})
	case ChannelOmniNotify:
		body, err = json.Marshal(toOmniNotify(note))
		url = omniNotifyEventsURL(ch.URL)
	default:
		body, err = json.Marshal(note)
	}
	if err != nil {
		return fmt.Errorf("alert: encode notification: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alert: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "omnilog-alerts")
	if ch.Token != "" {
		req.Header.Set("Authorization", "Bearer "+ch.Token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("alert: deliver to %q: %w", ch.Name, err)
	}
	defer resp.Body.Close()
	// Read a bounded amount so a chatty endpoint cannot hand us an unbounded body.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("alert: channel %q returned %d: %s",
			ch.Name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
