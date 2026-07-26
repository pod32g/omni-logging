// Package forward implements a lightweight log forwarder: it tails one or more
// files and ships new lines to an Omni-logging server's raw ingest endpoint.
package forward

import (
	"bufio"
	"bytes"
	"compress/gzip"
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

	// SpoolDir turns best-effort tailing into at-least-once delivery: each
	// batch is written to a durable on-disk queue before it is sent, retried
	// until the server accepts it, and only then dropped from the queue. A
	// batch the server refuses permanently is written to a dead-letter file.
	// Empty keeps the previous behaviour, where a batch that cannot be
	// delivered is lost.
	SpoolDir string

	// RetryBackoff is the first retry delay; it doubles up to RetryMaxBackoff.
	// Exposed mainly so tests do not have to wait out the production schedule.
	RetryBackoff    time.Duration
	RetryMaxBackoff time.Duration

	// Compress gzips each batch before sending. Log lines compress extremely
	// well, so this is mostly free bandwidth on a busy host.
	Compress bool
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
	if o.RetryBackoff <= 0 {
		o.RetryBackoff = baseBackoff
	}
	if o.RetryMaxBackoff <= 0 {
		o.RetryMaxBackoff = maxBackoff
	}
}

// Forwarder tails files and forwards their lines.
type Forwarder struct {
	opts   Options
	rawURL string
	spool  *spool // nil when SpoolDir is unset
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

	f := &Forwarder{opts: opts, rawURL: base.String()}
	if opts.SpoolDir != "" {
		sp, serr := openSpool(opts.SpoolDir)
		if serr != nil {
			return nil, serr
		}
		f.spool = sp
	}
	return f, nil
}

// Close releases the spool. Undelivered batches stay on disk for the next run.
func (f *Forwarder) Close() error {
	if f.spool == nil {
		return nil
	}
	return f.spool.Close()
}

// Run tails all files and forwards lines until ctx is cancelled.
func (f *Forwarder) Run(ctx context.Context) error {
	// Anything left in the spool from a previous run goes first, so restart
	// order matches ingest order.
	if err := f.drainSpool(ctx); err != nil {
		return err
	}

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
		f.deliver(c, batch)
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
func (f *Forwarder) backoffFor(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := f.opts.RetryBackoff << (attempt - 1)
	if d > f.opts.RetryMaxBackoff || d <= 0 {
		return f.opts.RetryMaxBackoff
	}
	return d
}

// deliveryOutcome is how a single POST attempt ended.
type deliveryOutcome int

const (
	delivered deliveryOutcome = iota // the server accepted it
	transient                        // worth retrying (network, 429, 5xx)
	permanent                        // the server will never accept it
	abandoned                        // shutting down; leave it queued
)

// attempt performs one POST. The batch ID goes in a header so a retry is
// recognisable as the same batch rather than as new data.
func (f *Forwarder) attempt(ctx context.Context, b spooledBatch) (deliveryOutcome, error) {
	body := []byte(strings.Join(b.Lines, "\n"))
	encoding := ""
	if f.opts.Compress {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, werr := gz.Write(body); werr != nil {
			return transient, werr
		}
		if cerr := gz.Close(); cerr != nil {
			return transient, cerr
		}
		body, encoding = buf.Bytes(), "gzip"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.rawURL, bytes.NewReader(body))
	if err != nil {
		return transient, err
	}
	req.Header.Set("Content-Type", "text/plain")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	if b.ID != "" {
		req.Header.Set("X-Batch-Id", b.ID)
	}
	if f.opts.APIKey != "" {
		req.Header.Set("X-Api-Key", f.opts.APIKey)
	}
	resp, err := f.opts.Client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return abandoned, err
		}
		return transient, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		return delivered, nil
	case retryable(resp.StatusCode):
		return transient, fmt.Errorf("server returned %d", resp.StatusCode)
	default:
		return permanent, fmt.Errorf("server returned %d", resp.StatusCode)
	}
}

// deliver spools a batch (when durable delivery is enabled) and sends it,
// acknowledging the spool entry only once the server has accepted it.
func (f *Forwarder) deliver(ctx context.Context, lines []string) {
	b := spooledBatch{ID: newBatchID(), Lines: append([]string(nil), lines...)}

	var seq uint64
	if f.spool != nil {
		var err error
		seq, err = f.spool.Add(b)
		if err != nil {
			// Durability is compromised, so say so plainly and still try to
			// send: refusing to send as well would lose the batch for certain.
			f.opts.Logger.Error("forward: could not spool batch durably; sending anyway",
				"lines", len(b.Lines), "error", err)
		}
	}

	outcome, err := f.post(ctx, b)
	f.settle(b, seq, outcome, err)
}

// settle records the end state of a batch: acknowledged, dead-lettered, or left
// in the spool for the next attempt.
func (f *Forwarder) settle(b spooledBatch, seq uint64, outcome deliveryOutcome, err error) {
	switch outcome {
	case delivered:
		if f.spool != nil && seq > 0 {
			if aerr := f.spool.Ack(seq); aerr != nil {
				// Failing to advance the checkpoint only means the batch is
				// re-sent later; the server's batch-ID dedup absorbs that.
				f.opts.Logger.Warn("forward: could not acknowledge spooled batch", "error", aerr)
			}
		}
	case permanent:
		f.opts.Logger.Error("forward: server refused the batch permanently",
			"lines", len(b.Lines), "batch", b.ID, "error", err)
		if f.spool != nil {
			rec := deadLetterRecord{At: time.Now().UTC(), Batch: b.ID, Reason: err.Error(), Lines: b.Lines}
			if derr := f.spool.DeadLetter(rec); derr != nil {
				f.opts.Logger.Error("forward: could not write the dead-letter record", "error", derr)
			} else {
				f.opts.Logger.Warn("forward: batch written to the dead-letter file",
					"path", f.spool.DeadLetterPath(), "lines", len(b.Lines))
			}
			if seq > 0 {
				// It is recorded elsewhere now, so stop retrying it.
				_ = f.spool.Ack(seq)
			}
		}
	case abandoned:
		if f.spool != nil {
			f.opts.Logger.Info("forward: batch left in the spool for the next run",
				"lines", len(b.Lines), "batch", b.ID)
		} else {
			f.opts.Logger.Error("forward: dropping batch (shutting down, no spool configured)",
				"lines", len(b.Lines), "error", err)
		}
	case transient:
		// Only reachable without a spool, where the retry budget is finite.
		f.opts.Logger.Error("forward: dropping batch after retries (no spool configured)",
			"lines", len(b.Lines), "attempts", maxPostAttempts, "error", err)
	}
}

// drainSpool re-sends everything a previous run left undelivered, oldest first.
func (f *Forwarder) drainSpool(ctx context.Context) error {
	if f.spool == nil {
		return nil
	}
	pending := 0
	err := f.spool.Pending(func(seq uint64, b spooledBatch) error {
		pending++
		outcome, perr := f.post(ctx, b)
		f.settle(b, seq, outcome, perr)
		if outcome == abandoned {
			return ctx.Err()
		}
		return nil
	})
	if pending > 0 {
		f.opts.Logger.Info("forward: replayed spooled batches from a previous run", "batches", pending)
	}
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("forward: replaying the spool: %w", err)
	}
	return nil
}

// post delivers one batch, retrying transient failures with capped backoff.
//
// With a spool the retry budget is unbounded: the batch is on disk, so giving
// up would discard data that is still safely queued. Without one the batch
// exists only in memory, so it retries a bounded number of times and is then
// lost — which is why the spool exists.
func (f *Forwarder) post(ctx context.Context, b spooledBatch) (deliveryOutcome, error) {
	var lastErr error
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			select {
			case <-time.After(f.backoffFor(attempt - 1)):
			case <-ctx.Done():
				return abandoned, lastErr
			}
		}
		outcome, err := f.attempt(ctx, b)
		lastErr = err
		switch outcome {
		case delivered:
			f.opts.Logger.Debug("forward: sent batch", "lines", len(b.Lines), "batch", b.ID)
			return delivered, nil
		case permanent:
			return permanent, err
		case abandoned:
			return abandoned, err
		}
		if f.spool == nil && attempt >= maxPostAttempts {
			return transient, err
		}
		f.opts.Logger.Warn("forward: retrying batch",
			"lines", len(b.Lines), "attempt", attempt, "spooled", f.spool != nil, "error", err)
	}
}
