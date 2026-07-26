package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/pod32g/omni-logging/internal/alert"
	"github.com/pod32g/omni-logging/internal/query"
)

// AlertStore is the persistence an API server needs for alerting. It is an
// interface so the api package stays independent of the concrete backend, the
// way it already is for events.
type AlertStore interface {
	alert.Store
	GetRule(ctx context.Context, id string) (alert.Rule, error)
	PutRule(ctx context.Context, r alert.Rule) (alert.Rule, error)
	DeleteRule(ctx context.Context, id string) error
	GetChannel(ctx context.Context, id string) (alert.Channel, error)
	PutChannel(ctx context.Context, c alert.Channel) (alert.Channel, error)
	DeleteChannel(ctx context.Context, id string) error
}

// ruleDTO is the JSON shape of a rule. Durations cross the wire as seconds
// rather than as Go's nanosecond integers, which no other client would guess.
type ruleDTO struct {
	ID              string          `json:"id,omitempty"`
	Name            string          `json:"name"`
	Query           string          `json:"query"`
	WindowSeconds   int64           `json:"window_seconds"`
	IntervalSeconds int64           `json:"interval_seconds"`
	Condition       alert.Condition `json:"condition"`
	Channels        []string        `json:"channels"`
	Enabled         bool            `json:"enabled"`

	State     alert.State `json:"state,omitempty"`
	StateFrom time.Time   `json:"state_since,omitzero"`
	LastEval  time.Time   `json:"last_eval,omitzero"`
	LastValue float64     `json:"last_value"`
	LastError string      `json:"last_error,omitempty"`
	CreatedAt time.Time   `json:"created_at,omitzero"`
	UpdatedAt time.Time   `json:"updated_at,omitzero"`
}

func toRuleDTO(r alert.Rule) ruleDTO {
	return ruleDTO{
		ID: r.ID, Name: r.Name, Query: r.Query,
		WindowSeconds:   int64(r.Window / time.Second),
		IntervalSeconds: int64(r.Interval / time.Second),
		Condition:       r.Cond, Channels: r.Channels, Enabled: r.Enabled,
		State: r.State, StateFrom: r.StateFrom, LastEval: r.LastEval,
		LastValue: r.LastValue, LastError: r.LastError,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (d ruleDTO) toRule() alert.Rule {
	if d.Channels == nil {
		d.Channels = []string{}
	}
	return alert.Rule{
		ID: d.ID, Name: d.Name, Query: d.Query,
		Window:   time.Duration(d.WindowSeconds) * time.Second,
		Interval: time.Duration(d.IntervalSeconds) * time.Second,
		Cond:     d.Condition, Channels: d.Channels, Enabled: d.Enabled,
	}
}

// handleAlertsList returns every rule.
func (s *Server) handleAlertsList(w http.ResponseWriter, r *http.Request) {
	rules, err := s.alerts.ListRules(r.Context())
	if err != nil {
		s.logger.Error("list alert rules failed", "error", err)
		http.Error(w, "could not list alert rules", http.StatusInternalServerError)
		return
	}
	out := make([]ruleDTO, 0, len(rules))
	for _, rule := range rules {
		out = append(out, toRuleDTO(rule))
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

// handleAlertCreate creates a rule.
func (s *Server) handleAlertCreate(w http.ResponseWriter, r *http.Request) {
	dto, ok := decodeJSON[ruleDTO](w, r)
	if !ok {
		return
	}
	dto.ID = "" // a create never adopts a caller-supplied ID
	rule := dto.toRule()
	if err := s.validateRule(&rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.alerts.PutRule(r.Context(), rule)
	if err != nil {
		s.logger.Error("create alert rule failed", "error", err)
		http.Error(w, "could not save the alert rule", http.StatusInternalServerError)
		return
	}
	s.logger.Info("alert rule created", "rule", saved.Name, "id", saved.ID)
	writeJSON(w, http.StatusCreated, toRuleDTO(saved))
}

// handleAlertUpdate replaces a rule's definition, preserving its firing state.
func (s *Server) handleAlertUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.alerts.GetRule(r.Context(), id); err != nil {
		s.alertNotFound(w, err, "alert rule")
		return
	}
	dto, ok := decodeJSON[ruleDTO](w, r)
	if !ok {
		return
	}
	dto.ID = id
	rule := dto.toRule()
	if err := s.validateRule(&rule); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.alerts.PutRule(r.Context(), rule)
	if err != nil {
		s.logger.Error("update alert rule failed", "error", err)
		http.Error(w, "could not save the alert rule", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, toRuleDTO(saved))
}

// handleAlertDelete removes a rule.
func (s *Server) handleAlertDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.alerts.DeleteRule(r.Context(), r.PathValue("id")); err != nil {
		s.alertNotFound(w, err, "alert rule")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAlertTest evaluates a rule immediately and returns what it saw, without
// changing the rule's state or notifying. Being able to answer "what would this
// fire on right now" is the difference between a rule you trust and one you
// hope about.
func (s *Server) handleAlertTest(w http.ResponseWriter, r *http.Request) {
	rule, err := s.alerts.GetRule(r.Context(), r.PathValue("id"))
	if err != nil {
		s.alertNotFound(w, err, "alert rule")
		return
	}
	ev, err := alert.Evaluate(r.Context(), s.store, rule, s.now())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ev)
}

// --- channels ---------------------------------------------------------------

func (s *Server) handleChannelsList(w http.ResponseWriter, r *http.Request) {
	channels, err := s.alerts.ListChannels(r.Context())
	if err != nil {
		s.logger.Error("list alert channels failed", "error", err)
		http.Error(w, "could not list channels", http.StatusInternalServerError)
		return
	}
	if channels == nil {
		channels = []alert.Channel{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
}

func (s *Server) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	ch, ok := decodeJSON[alert.Channel](w, r)
	if !ok {
		return
	}
	ch.ID = ""
	if err := ch.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	saved, err := s.alerts.PutChannel(r.Context(), ch)
	if err != nil {
		s.logger.Error("create alert channel failed", "error", err)
		http.Error(w, "could not save the channel", http.StatusInternalServerError)
		return
	}
	s.logger.Info("alert channel created", "channel", saved.Name, "type", saved.Type)
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.alerts.DeleteChannel(r.Context(), r.PathValue("id")); err != nil {
		s.alertNotFound(w, err, "channel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleChannelTest sends a sample notification so a channel can be verified
// when it is configured rather than when an incident happens.
func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	ch, err := s.alerts.GetChannel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.alertNotFound(w, err, "channel")
		return
	}
	note := alert.Notification{
		Rule:      "Test notification",
		State:     alert.StateFiring,
		Value:     1,
		Condition: "> 0",
		Query:     "(test)",
		Window:    "5m",
		At:        s.now(),
	}
	if err := alert.NewNotifier(nil).Send(r.Context(), ch, note); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- helpers ----------------------------------------------------------------

// validateRule checks both the rule's own shape and that its query parses, so a
// typo is rejected at write time rather than failing silently every interval.
func (s *Server) validateRule(rule *alert.Rule) error {
	if err := rule.Validate(); err != nil {
		return err
	}
	// Parenthesised because a composite literal in an if-statement header is
	// ambiguous with the block brace.
	if _, err := (query.Params{Q: rule.Query}).Build(s.now()); err != nil {
		return err
	}
	return nil
}

func (s *Server) alertNotFound(w http.ResponseWriter, err error, what string) {
	if errors.Is(err, alert.ErrNotFound) {
		http.Error(w, what+" not found", http.StatusNotFound)
		return
	}
	s.logger.Error("alert store operation failed", "error", err)
	http.Error(w, "alert store error", http.StatusInternalServerError)
}

// decodeJSON reads a bounded JSON body, reporting a 400 on failure.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "could not read the request body", http.StatusBadRequest)
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return v, false
	}
	return v, true
}
