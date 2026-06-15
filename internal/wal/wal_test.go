package wal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

func open(t *testing.T, opts Options) *WAL {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	w, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// collect replays the WAL into a slice of (seq, payload-as-string).
func collect(t *testing.T, w *WAL) []string {
	t.Helper()
	var out []string
	if err := w.Replay(func(seq uint64, payload []byte) error {
		out = append(out, fmt.Sprintf("%d:%s", seq, payload))
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	return out
}

func TestAppendReplayRoundTrip(t *testing.T) {
	w := open(t, Options{})
	for i, p := range []string{"alpha", "beta", "gamma"} {
		seq, err := w.Append([]byte(p))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if seq != uint64(i+1) {
			t.Fatalf("seq = %d, want %d", seq, i+1)
		}
	}
	got := collect(t, w)
	want := []string{"1:alpha", "2:beta", "3:gamma"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("replay = %v, want %v", got, want)
	}
}

func TestCheckpointSkipsApplied(t *testing.T) {
	w := open(t, Options{})
	for _, p := range []string{"a", "b", "c"} {
		w.Append([]byte(p))
	}
	if err := w.SetCheckpoint(2); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if w.Checkpoint() != 2 {
		t.Fatalf("Checkpoint = %d, want 2", w.Checkpoint())
	}
	got := collect(t, w)
	if fmt.Sprint(got) != fmt.Sprint([]string{"3:c"}) {
		t.Fatalf("replay after checkpoint = %v, want [3:c]", got)
	}
}

func TestReopenRecoversSeqAndReplaysUnapplied(t *testing.T) {
	dir := t.TempDir()
	w := open(t, Options{Dir: dir})
	for _, p := range []string{"a", "b", "c"} {
		w.Append([]byte(p))
	}
	w.SetCheckpoint(1)
	w.Close()

	w2 := open(t, Options{Dir: dir})
	if got := collect(t, w2); fmt.Sprint(got) != fmt.Sprint([]string{"2:b", "3:c"}) {
		t.Fatalf("replay after reopen = %v, want [2:b 3:c]", got)
	}
	// New appends continue the sequence.
	seq, _ := w2.Append([]byte("d"))
	if seq != 4 {
		t.Fatalf("post-reopen Append seq = %d, want 4", seq)
	}
}

func TestTornTailRecordIgnored(t *testing.T) {
	dir := t.TempDir()
	w := open(t, Options{Dir: dir})
	w.Append([]byte("good1"))
	w.Append([]byte("good2"))
	w.Sync()
	w.Close()

	// Simulate a crash mid-write: append a partial/garbage record to the segment.
	seg := segmentFiles(t, dir)[0]
	f, err := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	// A truncated header (fewer than 16 bytes) — an interrupted append.
	f.Write([]byte{0, 0, 0, 0, 0})
	f.Close()

	w2 := open(t, Options{Dir: dir})
	if got := collect(t, w2); fmt.Sprint(got) != fmt.Sprint([]string{"1:good1", "2:good2"}) {
		t.Fatalf("replay with torn tail = %v, want [1:good1 2:good2]", got)
	}
	// The torn tail must be truncated so new appends are clean and contiguous.
	seq, _ := w2.Append([]byte("good3"))
	if seq != 3 {
		t.Fatalf("post-recovery Append seq = %d, want 3", seq)
	}
	if got := collect(t, w2); fmt.Sprint(got) != fmt.Sprint([]string{"1:good1", "2:good2", "3:good3"}) {
		t.Fatalf("replay after clean append = %v", got)
	}
}

func TestCrcCorruptionStopsReplay(t *testing.T) {
	dir := t.TempDir()
	w := open(t, Options{Dir: dir})
	w.Append([]byte("rec1"))
	w.Append([]byte("rec2"))
	w.Append([]byte("rec3"))
	w.Sync()
	w.Close()

	// Flip a byte inside the second record's payload region.
	seg := segmentFiles(t, dir)[0]
	data, _ := os.ReadFile(seg)
	// header is 16 bytes; rec1 payload "rec1" (4) -> rec2 header starts at 20,
	// rec2 payload starts at 36. Corrupt a payload byte of rec2.
	data[36] ^= 0xFF
	os.WriteFile(seg, data, 0o644)

	w2 := open(t, Options{Dir: dir})
	got := collect(t, w2)
	if fmt.Sprint(got) != fmt.Sprint([]string{"1:rec1"}) {
		t.Fatalf("replay should stop at corruption, got %v, want [1:rec1]", got)
	}
}

func TestRotationAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	w := open(t, Options{Dir: dir, MaxSegmentBytes: 40}) // tiny -> frequent rotation
	for i := 0; i < 10; i++ {
		if _, err := w.Append([]byte(fmt.Sprintf("payload-%d", i))); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if n := len(segmentFiles(t, dir)); n < 2 {
		t.Fatalf("expected multiple segments, got %d", n)
	}
	if got := collect(t, w); len(got) != 10 {
		t.Fatalf("replay across segments = %d records, want 10", len(got))
	}
}

func TestSegmentDeletionAfterCheckpoint(t *testing.T) {
	dir := t.TempDir()
	w := open(t, Options{Dir: dir, MaxSegmentBytes: 40})
	for i := 0; i < 10; i++ {
		w.Append([]byte(fmt.Sprintf("payload-%d", i)))
	}
	before := len(segmentFiles(t, dir))
	// Checkpoint past everything; fully-applied non-active segments are removed.
	w.SetCheckpoint(10)
	after := len(segmentFiles(t, dir))
	if after >= before {
		t.Fatalf("expected segment deletion after checkpoint: before=%d after=%d", before, after)
	}
	// Replay now yields nothing (all applied).
	if got := collect(t, w); len(got) != 0 {
		t.Fatalf("replay after full checkpoint = %v, want none", got)
	}
}

func TestConcurrentAppends(t *testing.T) {
	w := open(t, Options{})
	const goroutines, each = 8, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := w.Append([]byte(fmt.Sprintf("g%d-i%d", g, i))); err != nil {
					t.Errorf("Append: %v", err)
				}
			}
		}(g)
	}
	wg.Wait()

	// All records present, and seqs are unique and contiguous 1..N.
	var seqs []uint64
	if err := w.Replay(func(seq uint64, payload []byte) error {
		seqs = append(seqs, seq)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(seqs) != goroutines*each {
		t.Fatalf("record count = %d, want %d", len(seqs), goroutines*each)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d (non-contiguous/duplicate)", i, s, i+1)
		}
	}
}

// segmentFiles lists the WAL segment files in dir, sorted.
func segmentFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "wal-*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(matches)
	return matches
}
