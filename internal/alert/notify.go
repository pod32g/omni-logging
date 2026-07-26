package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Notification is the payload delivered on a state transition.
type Notification struct {
	Rule      string     `json:"rule"`
	RuleID    string     `json:"rule_id"`
	State     State      `json:"state"` // firing or ok (resolved)
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
	switch ch.Type {
	case ChannelSlack:
		body, err = json.Marshal(map[string]string{"text": note.Text()})
	default:
		body, err = json.Marshal(note)
	}
	if err != nil {
		return fmt.Errorf("alert: encode notification: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ch.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alert: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "omnilog-alerts")

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
