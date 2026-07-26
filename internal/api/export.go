package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

// maxConcurrentExports bounds how many exports may stream at once. Exports run
// long by design and each holds a read connection for its whole duration, so
// leaving them unbounded lets a few downloads crowd out interactive searches.
const maxConcurrentExports = 2

// handleExport streams all events matching the query (ignoring the search limit)
// as NDJSON, CSV, or a JSON array, for downloads decoupled from the UI cap.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q, err := s.buildQuery(r)
	if err != nil {
		http.Error(w, "invalid query: "+err.Error(), http.StatusBadRequest)
		return
	}

	format := r.URL.Query().Get("format")
	if format == "" {
		format = "ndjson"
	}
	if format != "ndjson" && format != "csv" && format != "json" {
		http.Error(w, "unsupported format (use ndjson, csv, or json)", http.StatusBadRequest)
		return
	}

	select {
	case s.exportSlots <- struct{}{}:
		defer func() { <-s.exportSlots }()
	default:
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many concurrent exports; retry shortly", http.StatusTooManyRequests)
		return
	}

	flusher, _ := w.(http.Flusher)

	switch format {
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="omnilog-export.ndjson"`)
		s.finishExport(r, "ndjson", s.exportNDJSON(w, r, q, flusher))
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="omnilog-export.csv"`)
		s.finishExport(r, "csv", s.exportCSV(w, r, q, flusher))
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="omnilog-export.json"`)
		s.finishExport(r, "json", s.exportJSON(w, r, q, flusher))
	}
}

// finishExport decides how a failed export ends. Because the 200 and the first
// rows are already on the wire, there is no status code left to change — so
// rather than let the client save a silently truncated file that looks
// complete, abort the connection. net/http closes it without a terminating
// chunk, which every HTTP client reports as a failed transfer.
func (s *Server) finishExport(r *http.Request, format string, err error) {
	if err == nil {
		return
	}
	if r.Context().Err() != nil {
		// The client hung up or the request deadline passed; nothing to report.
		s.logger.Debug("export abandoned by client", "format", format, "error", err)
		return
	}
	s.logger.Error("export failed mid-stream; aborting the response",
		"format", format, "request_id", requestIDFromCtx(r.Context()), "error", err)
	panic(http.ErrAbortHandler)
}

func (s *Server) exportNDJSON(w http.ResponseWriter, r *http.Request, q query.Query, flusher http.Flusher) error {
	enc := json.NewEncoder(w)
	n := 0
	return s.store.Stream(r.Context(), q, func(e model.LogEvent) error {
		if err := enc.Encode(e); err != nil {
			return err
		}
		n++
		if flusher != nil && n%500 == 0 {
			flusher.Flush()
		}
		return nil
	})
}

func (s *Server) exportJSON(w http.ResponseWriter, r *http.Request, q query.Query, flusher http.Flusher) error {
	first := true
	if _, err := w.Write([]byte("[")); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	err := s.store.Stream(r.Context(), q, func(e model.LogEvent) error {
		if !first {
			if _, werr := w.Write([]byte(",")); werr != nil {
				return werr
			}
		}
		first = false
		return enc.Encode(e) // trailing newline is harmless inside the array
	})
	if err != nil {
		// Leave the array unterminated: a truncated stream must not parse as a
		// complete document.
		return err
	}
	_, err = w.Write([]byte("]"))
	return err
}

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request, q query.Query, flusher http.Flusher) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"timestamp", "level", "service", "source", "message", "attributes"})
	n := 0
	err := s.store.Stream(r.Context(), q, func(e model.LogEvent) error {
		attrs := ""
		if len(e.Attributes) > 0 {
			b, merr := json.Marshal(e.Attributes)
			if merr != nil {
				return merr
			}
			attrs = string(b)
		}
		if werr := cw.Write([]string{
			csvSafeCell(e.Timestamp.UTC().Format(time.RFC3339Nano)),
			csvSafeCell(string(e.Level)), csvSafeCell(e.Service), csvSafeCell(e.Source),
			csvSafeCell(e.Message), csvSafeCell(attrs),
		}); werr != nil {
			return werr
		}
		n++
		if n%500 == 0 {
			cw.Flush()
			if flusher != nil {
				flusher.Flush()
			}
		}
		return nil
	})
	cw.Flush()
	if err != nil {
		return err
	}
	return cw.Error()
}

// csvSafeCell prevents spreadsheet applications from interpreting untrusted
// log values as formulas while preserving the visible value for human review.
func csvSafeCell(s string) string {
	if s == "" {
		return s
	}
	if strings.ContainsRune("=+-@\t\r\n", rune(s[0])) {
		return "'" + s
	}
	return s
}
