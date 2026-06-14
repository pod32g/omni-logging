// Package forward implements a lightweight log forwarder: it tails one or more
// files and ships new lines to an Omni-logging server's raw ingest endpoint.
package forward

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Options configures a Forwarder.
type Options struct {
	ServerURL     string   // e.g. http://localhost:8080
	APIKey        string   // sent as X-Api-Key
	Service       string   // logical service name applied to all lines
	Source        string   // origin; defaults to the OS hostname
	Files         []string // files to tail
	Batch         int      // max lines per POST
	FlushInterval time.Duration
	PollInterval  time.Duration
	FromStart     bool // read existing content before following new lines
	Client        *http.Client
	Logger        *slog.Logger
}

func (o *Options) withDefaults() {
	if o.Batch <= 0 {
		o.Batch = 200
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 250 * time.Millisecond
	}
	if o.Source == "" {
		if h, err := os.Hostname(); err == nil {
			o.Source = h
		}
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Forwarder tails files and forwards their lines.
type Forwarder struct {
	opts   Options
	rawURL string
}

// New creates a Forwarder, returning an error if required options are missing.
func New(opts Options) (*Forwarder, error) {
	opts.withDefaults()
	if opts.ServerURL == "" {
		return nil, fmt.Errorf("forward: server URL is required")
	}
	if len(opts.Files) == 0 {
		return nil, fmt.Errorf("forward: at least one file is required")
	}
	base, err := url.Parse(opts.ServerURL)
	if err != nil {
		return nil, fmt.Errorf("forward: invalid server URL: %w", err)
	}
	q := url.Values{}
	if opts.Service != "" {
		q.Set("service", opts.Service)
	}
	if opts.Source != "" {
		q.Set("source", opts.Source)
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/ingest/raw"
	base.RawQuery = q.Encode()

	return &Forwarder{opts: opts, rawURL: base.String()}, nil
}

// Run tails all files and forwards lines until ctx is cancelled.
func (f *Forwarder) Run(ctx context.Context) error {
	lines := make(chan string, f.opts.Batch*4)
	var wg sync.WaitGroup
	for _, path := range f.opts.Files {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			f.tail(ctx, p, lines)
		}(path)
	}
	go func() { wg.Wait(); close(lines) }()

	f.send(ctx, lines)
	return nil
}

// tail follows a single file, emitting each new complete line. It handles
// truncation/rotation by resetting to the start when the file shrinks.
func (f *Forwarder) tail(ctx context.Context, path string, out chan<- string) {
	var offset int64
	if !f.opts.FromStart {
		if fi, err := os.Stat(path); err == nil {
			offset = fi.Size()
		}
	}
	ticker := time.NewTicker(f.opts.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		fi, err := os.Stat(path)
		if err != nil {
			continue // file may not exist yet; keep polling
		}
		if fi.Size() < offset {
			offset = 0 // rotated/truncated
		}
		if fi.Size() == offset {
			continue
		}

		file, err := os.Open(path)
		if err != nil {
			f.opts.Logger.Warn("forward: open failed", "file", path, "error", err)
			continue
		}
		if _, err := file.Seek(offset, io.SeekStart); err != nil {
			file.Close()
			continue
		}
		reader := bufio.NewReader(file)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				offset += int64(len(line))
				if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
					select {
					case out <- trimmed:
					case <-ctx.Done():
						file.Close()
						return
					}
				}
			}
			if err != nil {
				break // EOF or partial line; resume from offset next poll
			}
		}
		file.Close()
	}
}

// send batches lines and POSTs them, flushing on size or interval.
func (f *Forwarder) send(ctx context.Context, in <-chan string) {
	ticker := time.NewTicker(f.opts.FlushInterval)
	defer ticker.Stop()
	batch := make([]string, 0, f.opts.Batch)

	flush := func() {
		if len(batch) == 0 {
			return
		}
		f.post(ctx, batch)
		batch = batch[:0]
	}

	for {
		select {
		case line, ok := <-in:
			if !ok {
				flush()
				return
			}
			batch = append(batch, line)
			if len(batch) >= f.opts.Batch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}

// post sends a batch of lines, retrying a few times with backoff on failure.
func (f *Forwarder) post(ctx context.Context, batch []string) {
	body := strings.Join(batch, "\n")
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.rawURL, bytes.NewReader([]byte(body)))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "text/plain")
		if f.opts.APIKey != "" {
			req.Header.Set("X-Api-Key", f.opts.APIKey)
		}
		resp, err := f.opts.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			f.opts.Logger.Debug("forward: sent batch", "lines", len(batch))
			return
		}
		lastErr = fmt.Errorf("server returned %d", resp.StatusCode)
	}
	f.opts.Logger.Error("forward: failed to send batch after retries", "lines", len(batch), "error", lastErr)
}
