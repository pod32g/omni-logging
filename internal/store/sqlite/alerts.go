package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pod32g/omni-logging/internal/alert"
	"github.com/pod32g/omni-logging/internal/model"
)

// --- rules ------------------------------------------------------------------

// ListRules returns every alert rule, oldest first.
func (d *DB) ListRules(ctx context.Context) ([]alert.Rule, error) {
	rows, err := d.ro.QueryContext(ctx, `
		SELECT id, name, query, window_sec, interval_sec, cond_op, cond_value,
		       channels, enabled, state, state_since, last_eval, last_value, last_error,
		       created_at, updated_at
		FROM alert_rules ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()

	var out []alert.Rule
	for rows.Next() {
		r, serr := scanRule(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule returns one rule by ID.
func (d *DB) GetRule(ctx context.Context, id string) (alert.Rule, error) {
	row := d.ro.QueryRowContext(ctx, `
		SELECT id, name, query, window_sec, interval_sec, cond_op, cond_value,
		       channels, enabled, state, state_since, last_eval, last_value, last_error,
		       created_at, updated_at
		FROM alert_rules WHERE id = ?`, id)
	r, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return alert.Rule{}, alert.ErrNotFound
	}
	return r, err
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface{ Scan(...any) error }

func scanRule(s scanner) (alert.Rule, error) {
	var (
		r                             alert.Rule
		windowSec, intervalSec        int64
		channelsJSON                  string
		stateSince, lastEval, created int64
		updated                       int64
		lastError                     sql.NullString
	)
	if err := s.Scan(&r.ID, &r.Name, &r.Query, &windowSec, &intervalSec,
		&r.Cond.Op, &r.Cond.Value, &channelsJSON, &r.Enabled, &r.State,
		&stateSince, &lastEval, &r.LastValue, &lastError,
		&created, &updated); err != nil {
		return alert.Rule{}, err
	}
	r.Window = time.Duration(windowSec) * time.Second
	r.Interval = time.Duration(intervalSec) * time.Second
	r.LastError = lastError.String
	r.StateFrom = unixOrZero(stateSince)
	r.LastEval = unixOrZero(lastEval)
	r.CreatedAt = unixOrZero(created)
	r.UpdatedAt = unixOrZero(updated)
	if channelsJSON != "" {
		_ = json.Unmarshal([]byte(channelsJSON), &r.Channels)
	}
	return r, nil
}

func unixOrZero(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// PutRule inserts or replaces a rule, assigning an ID and timestamps on create.
// Evaluation state is preserved across an update so editing a rule's name does
// not silently re-fire an alert that was already firing.
func (d *DB) PutRule(ctx context.Context, r alert.Rule) (alert.Rule, error) {
	now := time.Now().UTC()
	if r.ID == "" {
		r.ID = model.NewID()
		r.CreatedAt = now
		if r.State == "" {
			r.State = alert.StateUnknown
		}
	} else if existing, err := d.GetRule(ctx, r.ID); err == nil {
		r.CreatedAt = existing.CreatedAt
		r.State = existing.State
		r.StateFrom = existing.StateFrom
		r.LastEval = existing.LastEval
		r.LastValue = existing.LastValue
		r.LastError = existing.LastError
	} else if !errors.Is(err, alert.ErrNotFound) {
		return alert.Rule{}, err
	}
	r.UpdatedAt = now
	if r.State == "" {
		r.State = alert.StateUnknown
	}

	channels, err := json.Marshal(r.Channels)
	if err != nil {
		return alert.Rule{}, err
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO alert_rules (id, name, query, window_sec, interval_sec, cond_op, cond_value,
		                         channels, enabled, state, state_since, last_eval, last_value,
		                         last_error, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, query=excluded.query, window_sec=excluded.window_sec,
		  interval_sec=excluded.interval_sec, cond_op=excluded.cond_op,
		  cond_value=excluded.cond_value, channels=excluded.channels,
		  enabled=excluded.enabled, updated_at=excluded.updated_at`,
		r.ID, r.Name, r.Query, int64(r.Window/time.Second), int64(r.Interval/time.Second),
		string(r.Cond.Op), r.Cond.Value, string(channels), r.Enabled, string(r.State),
		nanosOrZero(r.StateFrom), nanosOrZero(r.LastEval), r.LastValue, r.LastError,
		nanosOrZero(r.CreatedAt), nanosOrZero(r.UpdatedAt)); err != nil {
		return alert.Rule{}, fmt.Errorf("save alert rule: %w", err)
	}
	return r, nil
}

// SaveRuleState persists only the fields the scheduler owns, so it cannot race
// a concurrent edit of the rule's definition.
func (d *DB) SaveRuleState(ctx context.Context, r alert.Rule) error {
	_, err := d.db.ExecContext(ctx, `
		UPDATE alert_rules
		SET state = ?, state_since = ?, last_eval = ?, last_value = ?, last_error = ?
		WHERE id = ?`,
		string(r.State), nanosOrZero(r.StateFrom), nanosOrZero(r.LastEval),
		r.LastValue, r.LastError, r.ID)
	if err != nil {
		return fmt.Errorf("save alert state: %w", err)
	}
	return nil
}

// DeleteRule removes a rule.
func (d *DB) DeleteRule(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM alert_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return alert.ErrNotFound
	}
	return nil
}

func nanosOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// --- channels ---------------------------------------------------------------

// ListChannels returns every notification channel.
func (d *DB) ListChannels(ctx context.Context) ([]alert.Channel, error) {
	rows, err := d.ro.QueryContext(ctx,
		`SELECT id, name, type, url, created_at FROM alert_channels ORDER BY created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	defer rows.Close()

	var out []alert.Channel
	for rows.Next() {
		c, serr := scanChannel(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChannel returns one channel by ID.
func (d *DB) GetChannel(ctx context.Context, id string) (alert.Channel, error) {
	row := d.ro.QueryRowContext(ctx,
		`SELECT id, name, type, url, created_at FROM alert_channels WHERE id = ?`, id)
	c, err := scanChannel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return alert.Channel{}, alert.ErrNotFound
	}
	return c, err
}

func scanChannel(s scanner) (alert.Channel, error) {
	var (
		c       alert.Channel
		created int64
	)
	if err := s.Scan(&c.ID, &c.Name, &c.Type, &c.URL, &created); err != nil {
		return alert.Channel{}, err
	}
	c.CreatedAt = unixOrZero(created)
	return c, nil
}

// PutChannel inserts or replaces a notification channel.
func (d *DB) PutChannel(ctx context.Context, c alert.Channel) (alert.Channel, error) {
	if c.ID == "" {
		c.ID = model.NewID()
		c.CreatedAt = time.Now().UTC()
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO alert_channels (id, name, type, url, created_at) VALUES (?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, type=excluded.type, url=excluded.url`,
		c.ID, c.Name, string(c.Type), c.URL, nanosOrZero(c.CreatedAt)); err != nil {
		return alert.Channel{}, fmt.Errorf("save alert channel: %w", err)
	}
	return c, nil
}

// DeleteChannel removes a channel. Rules referencing it keep the dangling ID;
// the scheduler logs and skips it rather than failing the whole evaluation.
func (d *DB) DeleteChannel(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM alert_channels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete alert channel: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return alert.ErrNotFound
	}
	return nil
}
