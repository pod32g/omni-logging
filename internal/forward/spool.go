package forward

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/wal"
)

// The spool is built on internal/wal rather than a second append-only log:
// the WAL already provides CRC-checked records, torn-tail recovery, segment
// rotation and a checkpoint, which is exactly what "keep this batch until the
// server acknowledges it" needs.

// spooledBatch is one pending POST. The ID travels with it so a retry reuses
// the same value and the server can recognise the duplicate.
type spooledBatch struct {
	ID    string   `json:"id"`
	Lines []string `json:"lines"`
}

// deadLetterRecord is what gets written when a batch is refused permanently.
type deadLetterRecord struct {
	At     time.Time `json:"at"`
	Batch  string    `json:"batch_id"`
	Reason string    `json:"reason"`
	Status int       `json:"status,omitempty"`
	Lines  []string  `json:"lines"`
}

// spool is a durable queue of batches awaiting delivery, plus a dead-letter
// file for batches the server will never accept.
type spool struct {
	mu   sync.Mutex
	wal  *wal.WAL
	dead *os.File
	dir  string
}

// openSpool prepares the spool directory. A batch is durable once Add returns.
func openSpool(dir string) (*spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("spool: mkdir: %w", err)
	}
	w, err := wal.Open(wal.Options{Dir: filepath.Join(dir, "pending")})
	if err != nil {
		return nil, fmt.Errorf("spool: open pending log: %w", err)
	}
	return &spool{wal: w, dir: dir}, nil
}

// Add appends a batch and returns its sequence number. The record is in the OS
// page cache on return; Sync makes it survive power loss too.
func (s *spool) Add(b spooledBatch) (uint64, error) {
	payload, err := json.Marshal(b)
	if err != nil {
		return 0, fmt.Errorf("spool: encode batch: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.wal.Append(payload)
	if err != nil {
		return 0, fmt.Errorf("spool: append: %w", err)
	}
	if err := s.wal.Sync(); err != nil {
		// The record may only be in the page cache. Report it rather than
		// implying a durability guarantee that was not met.
		return seq, fmt.Errorf("spool: sync: %w", err)
	}
	return seq, nil
}

// Ack marks every batch up to seq as delivered, allowing the log to reclaim
// space. Called only after the server has accepted the batch.
func (s *spool) Ack(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wal.SetCheckpoint(seq)
}

// Pending replays every batch that has not been acknowledged, oldest first.
// Used at startup to resume delivery after a restart or an outage.
func (s *spool) Pending(fn func(seq uint64, b spooledBatch) error) error {
	return s.wal.Replay(func(seq uint64, payload []byte) error {
		var b spooledBatch
		if err := json.Unmarshal(payload, &b); err != nil {
			// A record we cannot decode can never be delivered; skip it rather
			// than wedging the queue behind it forever.
			return nil
		}
		return fn(seq, b)
	})
}

// DeadLetter records a batch the server refused permanently, so the lines are
// still recoverable by hand instead of vanishing into a log line.
func (s *spool) DeadLetter(rec deadLetterRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead == nil {
		f, err := os.OpenFile(filepath.Join(s.dir, "dead-letter.ndjson"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("spool: open dead-letter: %w", err)
		}
		s.dead = f
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := s.dead.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("spool: write dead-letter: %w", err)
	}
	return s.dead.Sync()
}

// DeadLetterPath is where permanently-refused batches are written.
func (s *spool) DeadLetterPath() string {
	return filepath.Join(s.dir, "dead-letter.ndjson")
}

// Close releases the spool's file handles.
func (s *spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	if s.dead != nil {
		firstErr = s.dead.Close()
		s.dead = nil
	}
	if err := s.wal.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// newBatchID mints an identifier for a batch. It is a ULID like every other ID
// in the system, and it is generated once per batch — a retry must reuse it, or
// the server cannot tell a retry from new data.
func newBatchID() string { return model.NewID() }
