// Package model defines the canonical log event and shared value types used
// across ingestion, storage, query, and tailing.
package model

import "strings"

// Level is a normalized log severity. Incoming logs use many spellings and
// numeric codes; ParseLevel folds them into this small fixed set.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
	LevelFatal Level = "fatal"
)

// levelRank orders severities from least to most severe. Used for filtering
// and display ordering. Unknown levels are treated as info.
var levelRank = map[Level]int{
	LevelDebug: 0,
	LevelInfo:  1,
	LevelWarn:  2,
	LevelError: 3,
	LevelFatal: 4,
}

// Levels returns all known levels in ascending severity order.
func Levels() []Level {
	return []Level{LevelDebug, LevelInfo, LevelWarn, LevelError, LevelFatal}
}

// Rank returns the severity rank of the level (higher is more severe).
func (l Level) Rank() int { return levelRank[l] }

// Valid reports whether l is one of the known normalized levels.
func (l Level) Valid() bool {
	_, ok := levelRank[l]
	return ok
}

// ParseLevel normalizes a free-form severity string into a Level. It accepts
// common textual spellings (e.g. "WARNING", "err", "critical") and numeric
// syslog severities (0-7). Anything unrecognized normalizes to LevelInfo.
func ParseLevel(s string) Level {
	// Numeric values follow the syslog severity scale: 0=emergency .. 7=debug.
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug", "dbg", "verbose", "7":
		return LevelDebug
	case "info", "information", "informational", "notice", "5", "6":
		return LevelInfo
	case "warn", "warning", "4":
		return LevelWarn
	case "err", "error", "3":
		return LevelError
	case "fatal", "crit", "critical", "alert", "emerg", "emergency", "panic", "0", "1", "2":
		return LevelFatal
	default:
		return LevelInfo
	}
}
