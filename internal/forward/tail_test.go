package forward

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// collect runs tail against path and returns the lines it emits within window.
func collect(t *testing.T, opts Options, path string, window time.Duration, write func()) []string {
	t.Helper()
	opts.withDefaults()
	f := &Forwarder{opts: opts}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan string, 64)
	go f.tail(ctx, path, out)

	write()

	var got []string
	deadline := time.After(window)
	for {
		select {
		case line := <-out:
			got = append(got, line)
		case <-deadline:
			return got
		}
	}
}

// TestTailDoesNotShipPartialLines covers a writer caught mid-line. Emitting
// whatever happens to be in the file at poll time turns one log line into two
// events: the fragment, then its remainder.
func TestTailDoesNotShipPartialLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// FromStart pins the initial offset at 0. The default ("begin at EOF")
	// would race the writer below for what counts as the starting point.
	opts := Options{PollInterval: 10 * time.Millisecond, FromStart: true}
	got := collect(t, opts, path, 300*time.Millisecond, func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		f.WriteString("first half ")
		time.Sleep(60 * time.Millisecond) // long enough for several polls
		f.WriteString("second half\n")
		time.Sleep(60 * time.Millisecond)
	})

	if len(got) != 1 {
		t.Fatalf("emitted %d lines %q, want exactly one complete line", len(got), got)
	}
	if got[0] != "first half second half" {
		t.Fatalf("line = %q, want the reassembled whole line", got[0])
	}
}

// TestTailDetectsRotationByIdentity covers a rotated file whose replacement
// grows past the old read offset between polls. A size-based check sees a file
// that is merely "bigger" and resumes at the stale offset, skipping everything
// written before it.
func TestTailDetectsRotationByIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	// Start with a long-ish file so the replacement can exceed its size.
	if err := os.WriteFile(path, []byte("old line one\nold line two\nold line three\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := collect(t, Options{PollInterval: 10 * time.Millisecond}, path, 300*time.Millisecond, func() {
		time.Sleep(50 * time.Millisecond)
		// Rotate: move the original aside and drop a new, larger file in place.
		if err := os.Rename(path, filepath.Join(dir, "app.log.1")); err != nil {
			t.Error(err)
			return
		}
		fresh := "fresh one\nfresh two\nfresh three\nfresh four\nfresh five\n"
		if err := os.WriteFile(path, []byte(fresh), 0o644); err != nil {
			t.Error(err)
			return
		}
		time.Sleep(100 * time.Millisecond)
	})

	if len(got) != 5 {
		t.Fatalf("got %d lines %q, want all 5 lines of the rotated-in file", len(got), got)
	}
	if got[0] != "fresh one" {
		t.Fatalf("first line = %q, want to start at the top of the new file", got[0])
	}
}

func TestRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{200, false}, {400, false}, {401, false}, {413, false},
		{429, true}, {500, true}, {502, true}, {503, true},
	} {
		if got := retryable(tc.status); got != tc.want {
			t.Errorf("retryable(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}

	f := &Forwarder{}
	f.opts.withDefaults()
	if f.backoffFor(1) != baseBackoff {
		t.Errorf("first backoff = %v, want %v", f.backoffFor(1), baseBackoff)
	}
	if f.backoffFor(2) != 2*baseBackoff {
		t.Errorf("second backoff = %v, want %v", f.backoffFor(2), 2*baseBackoff)
	}
	if got := f.backoffFor(50); got != maxBackoff {
		t.Errorf("backoff must saturate at %v, got %v", maxBackoff, got)
	}
}
