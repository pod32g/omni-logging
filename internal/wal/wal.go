// Package wal is a small, dependency-free, segment-based write-ahead log. It
// stores opaque byte payloads as an ordered, sequence-numbered, append-only log
// and supports crash recovery: a torn final record (from a process crash or
// power loss mid-write) is detected via CRC and discarded, and a checkpoint marks
// how far consumers have durably processed so applied segments can be reclaimed.
//
// It knows nothing about log events; callers serialize their own payloads. The
// ingest path uses it to make accepted events survive a crash before they reach
// the store.
package wal

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	headerSize        = 16       // u64 seq + u32 len + u32 crc
	defaultSegmentMax = 8 << 20  // 8 MiB
	maxRecordBytes    = 64 << 20 // guard against a garbage length field
	segmentPrefix     = "wal-"
	segmentSuffix     = ".log"
	checkpointName    = "checkpoint"
)

// Options configures a WAL.
type Options struct {
	Dir             string // directory holding segment files (created if needed)
	MaxSegmentBytes int64  // rotate to a new segment past this size (default 8 MiB)
	SyncOnAppend    bool   // fsync after every Append (default false: rely on the OS page cache + periodic Sync)
}

type segInfo struct {
	startSeq uint64
	path     string
}

// WAL is an append-only, sequence-numbered log split across segment files.
type WAL struct {
	dir          string
	maxSegment   int64
	syncOnAppend bool

	mu         sync.Mutex
	nextSeq    uint64
	checkpoint uint64
	segments   []segInfo // sorted ascending by startSeq; last is active
	active     *os.File
	activeSize int64
}

// Open opens or creates a WAL in opts.Dir, recovering the sequence counter and
// checkpoint and truncating any torn tail record left by a crash.
func Open(opts Options) (*WAL, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("wal: Dir is required")
	}
	if opts.MaxSegmentBytes <= 0 {
		opts.MaxSegmentBytes = defaultSegmentMax
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: mkdir: %w", err)
	}

	w := &WAL{dir: opts.Dir, maxSegment: opts.MaxSegmentBytes, syncOnAppend: opts.SyncOnAppend}
	w.checkpoint = readCheckpoint(opts.Dir)

	segs, err := listSegments(opts.Dir)
	if err != nil {
		return nil, err
	}
	w.segments = segs

	// Scan every segment to find the highest valid seq; remember a torn tail in
	// the last (active) segment so it can be truncated for clean future appends.
	var maxSeq uint64
	var lastTorn bool
	var lastEnd int64
	for i, s := range segs {
		end, torn, serr := scanSegment(s.path, func(seq uint64, _ []byte) error {
			if seq > maxSeq {
				maxSeq = seq
			}
			return nil
		})
		if serr != nil {
			return nil, serr
		}
		if i == len(segs)-1 {
			lastTorn, lastEnd = torn, end
		}
	}

	if len(segs) > 0 {
		last := segs[len(segs)-1]
		if lastTorn {
			if err := os.Truncate(last.path, lastEnd); err != nil {
				return nil, fmt.Errorf("wal: truncate torn tail: %w", err)
			}
		}
		f, err := os.OpenFile(last.path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("wal: open active segment: %w", err)
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		w.active = f
		w.activeSize = st.Size()
	}

	w.nextSeq = maxSeq + 1
	if w.checkpoint+1 > w.nextSeq {
		w.nextSeq = w.checkpoint + 1
	}
	return w, nil
}

// Append writes one record and returns its sequence number. The record is in the
// OS page cache on return (surviving a process crash); call Sync for durability
// against power loss.
func (w *WAL) Append(payload []byte) (uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	seq := w.nextSeq
	rec := encodeRecord(seq, payload)

	if w.active != nil && w.activeSize > 0 && w.activeSize+int64(len(rec)) > w.maxSegment {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	if w.active == nil {
		if err := w.openNewSegmentLocked(seq); err != nil {
			return 0, err
		}
	}
	if _, err := w.active.Write(rec); err != nil {
		return 0, fmt.Errorf("wal: write record: %w", err)
	}
	w.activeSize += int64(len(rec))
	w.nextSeq++
	if w.syncOnAppend {
		if err := w.active.Sync(); err != nil {
			return 0, err
		}
	}
	return seq, nil
}

func (w *WAL) rotateLocked() error {
	if w.active != nil {
		if err := w.active.Sync(); err != nil {
			return err
		}
		if err := w.active.Close(); err != nil {
			return err
		}
		w.active = nil
	}
	return nil
}

func (w *WAL) openNewSegmentLocked(startSeq uint64) error {
	path := filepath.Join(w.dir, fmt.Sprintf("%s%020d%s", segmentPrefix, startSeq, segmentSuffix))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("wal: create segment: %w", err)
	}
	w.active = f
	w.activeSize = 0
	w.segments = append(w.segments, segInfo{startSeq: startSeq, path: path})
	syncDir(w.dir)
	return nil
}

// Sync flushes the active segment to stable storage.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active != nil {
		return w.active.Sync()
	}
	return nil
}

// Checkpoint returns the last sequence number marked applied.
func (w *WAL) Checkpoint() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checkpoint
}

// SetCheckpoint persists that all records up to seq have been durably processed
// and deletes any non-active segment fully covered by it. Regressions are ignored.
func (w *WAL) SetCheckpoint(seq uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if seq <= w.checkpoint {
		return nil
	}
	w.checkpoint = seq
	if err := writeCheckpoint(w.dir, seq); err != nil {
		return err
	}
	w.pruneSegmentsLocked()
	return nil
}

func (w *WAL) pruneSegmentsLocked() {
	var kept []segInfo
	for i, s := range w.segments {
		isLast := i == len(w.segments)-1
		if !isLast {
			lastSeqInSeg := w.segments[i+1].startSeq - 1
			if lastSeqInSeg <= w.checkpoint {
				_ = os.Remove(s.path)
				continue
			}
		}
		kept = append(kept, s)
	}
	w.segments = kept
}

// Replay calls fn for every record with seq greater than the checkpoint, in
// order. It is intended for startup recovery (no concurrent appends). A scan
// stops at the first torn/corrupt record in a segment (the tail is untrustworthy).
func (w *WAL) Replay(fn func(seq uint64, payload []byte) error) error {
	w.mu.Lock()
	segs := append([]segInfo(nil), w.segments...)
	cp := w.checkpoint
	w.mu.Unlock()

	for _, s := range segs {
		if _, _, err := scanSegment(s.path, func(seq uint64, payload []byte) error {
			if seq <= cp {
				return nil
			}
			return fn(seq, payload)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the active segment.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active != nil {
		err := w.active.Close()
		w.active = nil
		return err
	}
	return nil
}

// --- encoding / scanning ---------------------------------------------------

func encodeRecord(seq uint64, payload []byte) []byte {
	buf := make([]byte, headerSize+len(payload))
	binary.BigEndian.PutUint64(buf[0:8], seq)
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(payload)))
	binary.BigEndian.PutUint32(buf[12:16], crc32.ChecksumIEEE(payload))
	copy(buf[16:], payload)
	return buf
}

// scanSegment reads valid records from a segment, invoking visit for each. It
// returns the byte offset just past the last valid record and whether the scan
// stopped early on a torn/corrupt record.
func scanSegment(path string, visit func(seq uint64, payload []byte) error) (endOff int64, torn bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false, fmt.Errorf("wal: open segment: %w", err)
	}
	defer f.Close()

	r := bufio.NewReader(f)
	header := make([]byte, headerSize)
	for {
		if _, e := io.ReadFull(r, header); e != nil {
			if e == io.EOF {
				return endOff, false, nil // clean end
			}
			return endOff, true, nil // partial header: torn tail
		}
		plen := binary.BigEndian.Uint32(header[8:12])
		crc := binary.BigEndian.Uint32(header[12:16])
		if plen > maxRecordBytes {
			return endOff, true, nil // implausible length: garbage
		}
		payload := make([]byte, plen)
		if _, e := io.ReadFull(r, payload); e != nil {
			return endOff, true, nil // short payload: torn tail
		}
		if crc32.ChecksumIEEE(payload) != crc {
			return endOff, true, nil // corruption: stop, tail untrustworthy
		}
		seq := binary.BigEndian.Uint64(header[0:8])
		if visit != nil {
			if verr := visit(seq, payload); verr != nil {
				return endOff, false, verr
			}
		}
		endOff += int64(headerSize) + int64(plen)
	}
}

// --- segment listing / checkpoint persistence ------------------------------

func listSegments(dir string) ([]segInfo, error) {
	matches, err := filepath.Glob(filepath.Join(dir, segmentPrefix+"*"+segmentSuffix))
	if err != nil {
		return nil, err
	}
	var segs []segInfo
	for _, p := range matches {
		base := filepath.Base(p)
		num := strings.TrimSuffix(strings.TrimPrefix(base, segmentPrefix), segmentSuffix)
		start, perr := strconv.ParseUint(num, 10, 64)
		if perr != nil {
			continue // ignore files that don't fit the naming scheme
		}
		segs = append(segs, segInfo{startSeq: start, path: p})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].startSeq < segs[j].startSeq })
	return segs, nil
}

func readCheckpoint(dir string) uint64 {
	data, err := os.ReadFile(filepath.Join(dir, checkpointName))
	if err != nil || len(data) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(data[:8])
}

func writeCheckpoint(dir string, seq uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	tmp := filepath.Join(dir, checkpointName+".tmp")
	if err := os.WriteFile(tmp, b[:], 0o644); err != nil {
		return fmt.Errorf("wal: write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, checkpointName)); err != nil {
		return fmt.Errorf("wal: rename checkpoint: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir best-effort fsyncs a directory so file creations/renames are durable.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
