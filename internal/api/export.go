package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/query"
)

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
	flusher, _ := w.(http.Flusher)

	switch format {
	case "ndjson":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="omnilog-export.ndjson"`)
		s.exportNDJSON(w, r, q, flusher)
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="omnilog-export.csv"`)
		s.exportCSV(w, r, q, flusher)
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="omnilog-export.json"`)
		s.exportJSON(w, r, q, flusher)
	default:
		http.Error(w, "unsupported format (use ndjson, csv, or json)", http.StatusBadRequest)
	}
}

func (s *Server) exportNDJSON(w http.ResponseWriter, r *http.Request, q query.Query, flusher http.Flusher) {
	enc := json.NewEncoder(w)
	n := 0
	err := s.store.Stream(r.Context(), q, func(e model.LogEvent) error {
		if err := enc.Encode(e); err != nil {
			return err
		}
		n++
		if flusher != nil && n%500 == 0 {
			flusher.Flush()
		}
		return nil
	})
	if err != nil {
		s.logger.Error("export ndjson failed", "error", err)
	}
}

func (s *Server) exportJSON(w http.ResponseWriter, r *http.Request, q query.Query, flusher http.Flusher) {
	first := true
	w.Write([]byte("["))
	enc := json.NewEncoder(w)
	err := s.store.Stream(r.Context(), q, func(e model.LogEvent) error {
		if !first {
			w.Write([]byte(","))
		}
		first = false
		return enc.Encode(e) // trailing newline is harmless inside the array
	})
	if err != nil {
		s.logger.Error("export json failed", "error", err)
	}
	w.Write([]byte("]"))
}

func (s *Server) exportCSV(w http.ResponseWriter, r *http.Request, q query.Query, flusher http.Flusher) {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"timestamp", "level", "service", "source", "message", "attributes"})
	n := 0
	err := s.store.Stream(r.Context(), q, func(e model.LogEvent) error {
		attrs := ""
		if len(e.Attributes) > 0 {
			b, _ := json.Marshal(e.Attributes)
			attrs = string(b)
		}
		if err := cw.Write([]string{
			e.Timestamp.UTC().Format(time.RFC3339Nano),
			string(e.Level), e.Service, e.Source, e.Message, attrs,
		}); err != nil {
			return err
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
		s.logger.Error("export csv failed", "error", err)
	}
}
