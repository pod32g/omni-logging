// Package omnilog is a small client for shipping logs to an Omni-logging
// server. It has no dependencies beyond the standard library, so adding it to
// an application does not drag a tree in behind it.
//
// The client batches in the background: Send never blocks on the network, and a
// server that is slow or down costs the calling application nothing but a
// bounded queue. That property is the whole point — a logging call inside a
// request handler must not be able to stall the request.
package omnilog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event is one log record. Only Message is required; anything left zero is
// filled in by the server or omitted.
type Event struct {
	Timestamp  time.Time      `json:"timestamp,omitzero"`
	Level      string         `json:"level,omitempty"`
	Service    string         `json:"service,omitempty"`
	Source     string         `json:"source,omitempty"`
	Message    string         `json:"message"`
	Attributes map[string]any `json:"-"` // flattened into the JSON object
}

// MarshalJSON flattens attributes to top level, which is the shape the server's
// ingest endpoint expects: unknown keys become searchable attributes.
func (e Event) MarshalJSON() ([]byte, error) {
	out := map[string]any{"message": e.Message}
	if !e.Timestamp.IsZero() {
		out["timestamp"] = e.Timestamp.UTC().Format(time.RFC3339Nano)
	}
	for _, kv := range []struct{ k, v string }{
		{"level", e.Level}, {"service", e.Service}, {"source", e.Source},
	} {
		if kv.v != "" {
			out[kv.k] = kv.v
		}
	}
	for k, v := range e.Attributes {
		// Reserved keys are not overwritten by an attribute of the same name,
		// so a stray "message" attribute cannot replace the real message.
		if _, taken := out[k]; !taken {
			out[k] = v
		}
	}
	return json.Marshal(out)
}

// Options configures a Client.
type Options struct {
	// ServerURL is the base URL, e.g. http://localhost:8080.
	ServerURL string
	// APIKey is sent as X-Api-Key when the server has ingest keys configured.
	APIKey string
	// Service and Source default onto every event that does not set its own.
	Service string
	Source  string

	// BatchSize and FlushInterval control how events are grouped. A batch is
	// sent when either is reached.
	BatchSize     int
	FlushInterval time.Duration

	// QueueSize bounds how many events may be waiting. Once full, Send drops
	// rather than blocks: an application must not stall because its logging
	// backend is unwell. Dropped counts them so the loss is visible.
	QueueSize int

	// Compress gzips each batch. Log lines compress very well.
	Compress bool

	// HTTPClient is used for delivery; a sane default is supplied.
	HTTPClient *http.Client

	// OnError is called when a batch cannot be delivered. Leave nil to ignore
	// failures silently — the default, because a logging client that writes to
	// stderr on every failure can amplify an outage.
	OnError func(error)
}

func (o *Options) withDefaults() {
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = 2 * time.Second
	}
	if o.QueueSize <= 0 {
		o.QueueSize = 10000
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
}

// Client ships events to a server in the background.
type Client struct {
	opts    Options
	url     string
	ch      chan Event
	wg      sync.WaitGroup
	closing chan struct{}
	once    sync.Once

	dropped atomic.Int64
	sent    atomic.Int64
	failed  atomic.Int64
}

// New creates a Client and starts its background sender. Call Close to flush.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.ServerURL) == "" {
		return nil, errors.New("omnilog: ServerURL is required")
	}
	opts.withDefaults()
	c := &Client{
		opts:    opts,
		url:     strings.TrimRight(opts.ServerURL, "/") + "/api/v1/ingest",
		ch:      make(chan Event, opts.QueueSize),
		closing: make(chan struct{}),
	}
	c.wg.Add(1)
	go c.run()
	return c, nil
}

// Send queues an event. It never blocks: if the queue is full the event is
// dropped and counted, because blocking here would push a logging backend's
// problems into the caller's request path.
func (c *Client) Send(e Event) {
	if e.Service == "" {
		e.Service = c.opts.Service
	}
	if e.Source == "" {
		e.Source = c.opts.Source
	}
	select {
	case c.ch <- e:
	default:
		c.dropped.Add(1)
	}
}

// Log is the convenience form of Send.
func (c *Client) Log(level, message string, attrs map[string]any) {
	c.Send(Event{Level: level, Message: message, Attributes: attrs})
}

// Stats reports delivery counters, so an application can alert on its own
// logging pipeline rather than discovering the loss later.
type Stats struct {
	Sent    int64
	Failed  int64
	Dropped int64
}

// Stats returns a snapshot of delivery counters.
func (c *Client) Stats() Stats {
	return Stats{Sent: c.sent.Load(), Failed: c.failed.Load(), Dropped: c.dropped.Load()}
}

// Close flushes buffered events and stops the sender. It is safe to call twice.
func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.closing)
		close(c.ch)
	})
	c.wg.Wait()
	return nil
}

func (c *Client) run() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]Event, 0, c.opts.BatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		c.post(batch)
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-c.ch:
			if !ok {
				flush() // the queue is closed: send what is left and stop
				return
			}
			batch = append(batch, e)
			if len(batch) >= c.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// post delivers one batch as NDJSON.
func (c *Client) post(batch []Event) {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, e := range batch {
		if err := enc.Encode(e); err != nil {
			c.reportError(fmt.Errorf("omnilog: encode event: %w", err))
			return
		}
	}

	payload, encoding, err := maybeCompress(body.Bytes(), c.opts.Compress)
	if err != nil {
		c.reportError(err)
		return
	}

	// Deliberately context.Background with the client's own timeout: a batch
	// must not be cancelled because the request that produced the log ended.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		c.reportError(err)
		return
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	if c.opts.APIKey != "" {
		req.Header.Set("X-Api-Key", c.opts.APIKey)
	}

	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		c.failed.Add(int64(len(batch)))
		c.reportError(fmt.Errorf("omnilog: send batch: %w", err))
		return
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode/100 != 2 {
		c.failed.Add(int64(len(batch)))
		c.reportError(fmt.Errorf("omnilog: server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet))))
		return
	}
	c.sent.Add(int64(len(batch)))
}

func (c *Client) reportError(err error) {
	if c.opts.OnError != nil {
		c.opts.OnError(err)
	}
}
