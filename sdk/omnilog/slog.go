package omnilog

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"time"
)

// Handler is an slog.Handler that ships records to an Omni-logging server, so
// an application emits structured logs with one line of setup:
//
//	client, _ := omnilog.New(omnilog.Options{ServerURL: "http://logs:8080", Service: "api"})
//	defer client.Close()
//	slog.SetDefault(slog.New(omnilog.NewHandler(client, nil)))
//
// Every slog attribute becomes a searchable attribute on the event, so
// slog.Int("status", 500) is queryable as attr.status>=500 without any
// further configuration.
type Handler struct {
	client *Client
	opts   HandlerOptions

	// preformatted holds attributes already qualified with the group prefix that
	// was in effect when they were added. slog scopes an attribute by the groups
	// open at the time of the WithAttrs call, so qualifying them later — when
	// more groups may have opened — would file them under the wrong prefix.
	preformatted map[string]any
	// groups is the prefix applied to attributes arriving from here on.
	groups []string
}

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	// Level is the minimum level shipped. nil means every record.
	Level slog.Leveler
	// AddSource records the calling file and line as attributes.
	AddSource bool
	// Fallback also writes each record to another handler, which is how you
	// keep local stderr output while shipping. nil disables it.
	Fallback slog.Handler
}

// NewHandler creates a Handler writing to client.
func NewHandler(client *Client, opts *HandlerOptions) *Handler {
	h := &Handler{client: client, preformatted: map[string]any{}}
	if opts != nil {
		h.opts = *opts
	}
	return h
}

// Enabled reports whether a record at this level is shipped.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	min := slog.LevelInfo
	if h.opts.Level != nil {
		min = h.opts.Level.Level()
	}
	if level < min {
		// Still let the fallback decide for itself: a local handler may well
		// want debug output that is not worth shipping.
		return h.opts.Fallback != nil && h.opts.Fallback.Enabled(ctx, level)
	}
	return true
}

// Handle ships one record.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h.opts.Fallback != nil && h.opts.Fallback.Enabled(ctx, r.Level) {
		// Best-effort: a failing local handler must not stop the record from
		// being shipped, which is the whole reason for having both.
		_ = h.opts.Fallback.Handle(ctx, r)
	}

	min := slog.LevelInfo
	if h.opts.Level != nil {
		min = h.opts.Level.Level()
	}
	if r.Level < min {
		return nil
	}

	attrs := make(map[string]any, len(h.preformatted)+r.NumAttrs())
	for k, v := range h.preformatted {
		attrs[k] = v
	}
	r.Attrs(func(a slog.Attr) bool {
		flattenAttr(attrs, h.groups, a)
		return true
	})
	if h.opts.AddSource && r.PC != 0 {
		if f := callerFrame(r.PC); f.File != "" {
			attrs["source_file"] = f.File
			attrs["source_line"] = f.Line
		}
	}
	if len(attrs) == 0 {
		attrs = nil
	}

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	h.client.Send(Event{
		Timestamp:  ts,
		Level:      levelName(r.Level),
		Message:    r.Message,
		Attributes: attrs,
	})
	return nil
}

// WithAttrs returns a handler with additional attributes on every record.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := h.clone()
	// Qualify now, with the groups currently open.
	for _, a := range attrs {
		flattenAttr(next.preformatted, h.groups, a)
	}
	if h.opts.Fallback != nil {
		next.opts.Fallback = h.opts.Fallback.WithAttrs(attrs)
	}
	return next
}

// WithGroup returns a handler that nests subsequent attributes under name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := h.clone()
	next.groups = append(next.groups, name)
	if h.opts.Fallback != nil {
		next.opts.Fallback = h.opts.Fallback.WithGroup(name)
	}
	return next
}

// clone copies the handler's state. Handlers are shared across goroutines and a
// derived handler must never mutate its parent, so this is a deep copy of both
// the attribute map and the group stack.
func (h *Handler) clone() *Handler {
	next := &Handler{
		client:       h.client,
		opts:         h.opts,
		preformatted: make(map[string]any, len(h.preformatted)),
		groups:       slices.Clone(h.groups),
	}
	for k, v := range h.preformatted {
		next.preformatted[k] = v
	}
	return next
}

// flattenAttr writes one slog attribute into the flat attribute map, joining
// group names with dots. The event model is flat and searchable by
// "attr.http.status"; preserving nesting would make that path unreachable.
func flattenAttr(out map[string]any, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if len(groups) > 0 {
		key = joinDot(groups, a.Key)
	}
	if a.Value.Kind() == slog.KindGroup {
		nested := append(slices.Clone(groups), a.Key)
		for _, sub := range a.Value.Group() {
			flattenAttr(out, nested, sub)
		}
		return
	}
	out[key] = a.Value.Any()
}

func joinDot(groups []string, key string) string {
	var b bytes.Buffer
	for _, g := range groups {
		b.WriteString(g)
		b.WriteByte('.')
	}
	b.WriteString(key)
	return b.String()
}

// levelName maps an slog level onto the server's level vocabulary. slog levels
// are open-ended integers, so anything above Error is treated as fatal.
func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	case l < slog.LevelError+4:
		return "error"
	default:
		return "fatal"
	}
}

// maybeCompress gzips a payload when requested, returning the content encoding.
func maybeCompress(body []byte, compress bool) ([]byte, string, error) {
	if !compress {
		return body, "", nil
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(body); err != nil {
		return nil, "", fmt.Errorf("omnilog: compress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, "", fmt.Errorf("omnilog: compress: %w", err)
	}
	return buf.Bytes(), "gzip", nil
}

// callerFrame resolves a program counter to its source location.
func callerFrame(pc uintptr) runtime.Frame {
	fs := runtime.CallersFrames([]uintptr{pc})
	f, _ := fs.Next()
	return f
}
