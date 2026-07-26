package wal

import (
	"fmt"
	"testing"
)

// TestSyncOnAppendRoundTrip exercises the SyncOnAppend option, which had no
// coverage at all: nothing in the tree enables it, so its append path (and the
// rollback that guards a failed sync) was never run.
func TestSyncOnAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(Options{Dir: dir, SyncOnAppend: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var seqs []uint64
	for i := 0; i < 5; i++ {
		seq, aerr := w.Append([]byte(fmt.Sprintf(`{"n":%d}`, i)))
		if aerr != nil {
			t.Fatalf("Append %d: %v", i, aerr)
		}
		seqs = append(seqs, seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("sequence numbers not contiguous: %v", seqs)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Everything appended must survive a reopen and replay in order.
	w2, err := Open(Options{Dir: dir, SyncOnAppend: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer w2.Close()

	var got []uint64
	if err := w2.Replay(func(seq uint64, payload []byte) error {
		got = append(got, seq)
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != len(seqs) {
		t.Fatalf("replayed %d records, want %d", len(got), len(seqs))
	}
	for i := range got {
		if got[i] != seqs[i] {
			t.Fatalf("replayed seq %d = %d, want %d", i, got[i], seqs[i])
		}
	}
}

// TestRotationClearsActiveSegment covers rotateLocked retiring the old handle.
// Leaving a closed file installed as the active segment would make every later
// append fail against a descriptor that is already gone.
func TestRotationClearsActiveSegment(t *testing.T) {
	dir := t.TempDir()
	// A tiny segment cap forces a rotation on nearly every append.
	w, err := Open(Options{Dir: dir, MaxSegmentBytes: 64})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer w.Close()

	for i := 0; i < 10; i++ {
		if _, aerr := w.Append([]byte(fmt.Sprintf(`{"padding":"%020d"}`, i))); aerr != nil {
			t.Fatalf("append %d after rotation: %v", i, aerr)
		}
	}
	if len(w.segments) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(w.segments))
	}

	n := 0
	if err := w.Replay(func(uint64, []byte) error { n++; return nil }); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if n != 10 {
		t.Fatalf("replayed %d records across rotations, want 10", n)
	}
}
