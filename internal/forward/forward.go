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

// tail follows a single file, emitting each new complete line.
//
// Rotation is detected by file identity rather than by size. Comparing the
// current size against the read offset only catches a replacement file that is
// still shorter than the old one; if the new file grows past the old offset
// between two polls, a size check sees nothing wrong and the reader seeks into
// the middle of unrelated content, skipping everything before it.
func (f *Forwarder) tail(ctx context.Context, path string, out chan<- string) {
	var (
		offset int64
		known  os.FileInfo // identity of the file the offset belongs to
	)
	if fi, err := os.Stat(path); err == nil {
		known = fi
		if !f.opts.FromStart {
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
		switch {
		case known != nil && !os.SameFile(known, fi):
			offset = 0 // rotated: a different file now lives at this path
		case fi.Size() < offset:
			offset = 0 // truncated in place
		}
		known = fi
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
			line, rerr := reader.ReadString('\n')
			// Only a line terminated by '\n' is complete. Without this check a
			// line the writer is still mid-way through gets shipped as a whole
			// event, and its remainder arrives later as a second one.
			if strings.HasSuffix(line, "\n") {
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
			if rerr != nil {
				break // EOF or partial line; re-read from offset next poll
			}
		}
		file.Close()
	}
}

// finalFlushTimeout bounds the last delivery attempt during shutdown.
const finalFlushTimeout = 5 * time.Second

// send batches lines and POSTs them, flushing on size or interval.
func (f *Forwarder) send(ctx context.Context, in <-chan string) {
	ticker := time.NewTicker(f.opts.FlushInterval)
	defer ticker.Stop()
	batch := make([]string, 0, f.opts.Batch)

	flush := func(c context.Context) {
		if len(batch) == 0 {
			return
		}
		f.post(c, batch)
		batch = batch[:0]
	}
	// On shutdown the parent context is already cancelled, so posting with it
	// would fail instantly and throw away everything still buffered. Detach it
	// and give the last batch a short deadline of its own.
	finalFlush := func() {
		if len(batch) == 0 {
			return
		}
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalFlushTimeout)
		defer cancel()
		flush(c)
	}

	for {
		select {
		case line, ok := <-in:
			if !ok {
				finalFlush()
				return
			}
			batch = append(batch, line)
			if len(batch) >= f.opts.Batch {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			finalFlush()
			return
		}
	}
}

// Retry policy. Dropping a batch loses logs permanently, so a transient
// condition — the server restarting, or admission control pushing back with a
// 429 — must not exhaust the attempts in under two seconds the way a fixed
// 3-try/500ms schedule did.
const (
	maxPostAttempts = 6
	baseBackoff     = 500 * time.Millisecond
	maxBackoff      = 30 * time.Second
)

// retryable reports whether an HTTP status is worth another attempt. 429 and
// 5xx are transient; other 4xx (bad key, malformed request) will fail
// identically no matter how many times we resend.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// backoffFor returns the delay before the given attempt, doubling up to a cap.
func backoffFor(attempt int) time.Duration {
	d := baseBackoff << (attempt - 1)
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

// post sends a batch of lines, retrying transient failures with exponential
// backoff. A batch that still cannot be delivered is dropped with a loud error:
// the forwarder holds no durable spool, so this is the one place logs can be
// lost, and it should be visible when it happens.
func (f *Forwarder) post(ctx context.Context, batch []string) {
	body := strings.Join(batch, "\n")
	var lastErr error
	for attempt := 1; attempt <= maxPostAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(backoffFor(attempt - 1)):
			case <-ctx.Done():
				f.opts.Logger.Error("forward: giving up on batch (shutting down)", "lines", len(batch), "error", lastErr)
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
			if ctx.Err() != nil {
				f.opts.Logger.Error("forward: giving up on batch (shutting down)", "lines", len(batch), "error", lastErr)
				return
			}
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			f.opts.Logger.Debug("forward: sent batch", "lines", len(batch))
			return
		}
		lastErr = fmt.Errorf("server returned %d", resp.StatusCode)
		if !retryable(resp.StatusCode) {
			f.opts.Logger.Error("forward: dropping batch, server rejected it permanently",
				"lines", len(batch), "status", resp.StatusCode)
			return
		}
		f.opts.Logger.Warn("forward: retrying batch", "lines", len(batch), "attempt", attempt, "error", lastErr)
	}
	f.opts.Logger.Error("forward: dropping batch after retries", "lines", len(batch), "attempts", maxPostAttempts, "error", lastErr)
}
