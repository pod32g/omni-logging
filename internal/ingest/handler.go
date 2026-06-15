package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/pod32g/omni-logging/internal/model"
)

// maxBodyBytes caps a single ingest request to protect memory.
const maxBodyBytes = 32 << 20 // 32 MiB

// recordError describes why a single record in a batch was rejected.
type recordError struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// ingestResponse is the JSON body returned from ingest endpoints.
type ingestResponse struct {
	Accepted int           `json:"accepted"`
	Rejected int           `json:"rejected"`
	Errors   []recordError `json:"errors,omitempty"`
}

// Handler accepts structured logs as either a JSON array of objects or NDJSON
// (one JSON object per line). Malformed records are reported per-record;
// well-formed records are enqueued. If the buffer fills mid-batch, the response
// is 429 and the overflow is reported as rejected.
func (i *Ingestor) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := i.admit(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		now := i.opts.Now()

		var resp ingestResponse
		overflow := false

		emit := func(idx int, raw []byte) {
			raw = bytes.TrimSpace(raw)
			if len(raw) == 0 {
				return
			}
			e, perr := model.EventFromJSON(raw, now)
			if perr != nil {
				resp.Rejected++
				resp.Errors = append(resp.Errors, recordError{Index: idx, Error: perr.Error()})
				return
			}
			if overflow || !i.Enqueue(e) {
				overflow = true
				resp.Rejected++
				resp.Errors = append(resp.Errors, recordError{Index: idx, Error: "ingest buffer full"})
				return
			}
			resp.Accepted++
		}

		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			// JSON array form.
			var arr []json.RawMessage
			if err := json.Unmarshal(trimmed, &arr); err != nil {
				http.Error(w, "invalid JSON array: "+err.Error(), http.StatusBadRequest)
				return
			}
			for idx, raw := range arr {
				emit(idx, raw)
			}
		} else {
			// NDJSON form (also handles a single object).
			scanner := bufio.NewScanner(bytes.NewReader(body))
			scanner.Buffer(make([]byte, 0, 64*1024), maxBodyBytes)
			idx := 0
			for scanner.Scan() {
				emit(idx, scanner.Bytes())
				idx++
			}
			if err := scanner.Err(); err != nil {
				http.Error(w, "error reading body: "+err.Error(), http.StatusBadRequest)
				return
			}
		}

		i.recordUsage(key, resp.Accepted, int64(len(body)))
		status := http.StatusOK
		if overflow {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, resp)
	}
}

// RawHandler accepts plain text, one log line per line of the body. service and
// source come from query params (?service=, ?source=) or headers
// (X-Service, X-Source); source falls back to the remote address.
func (i *Ingestor) RawHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := i.admit(w, r)
		if !ok {
			return
		}
		service := firstNonEmpty(r.URL.Query().Get("service"), r.Header.Get("X-Service"))
		source := firstNonEmpty(r.URL.Query().Get("source"), r.Header.Get("X-Source"), clientIP(r))
		level := model.ParseLevel(firstNonEmpty(r.URL.Query().Get("level"), "info"))
		now := i.opts.Now()

		body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), maxBodyBytes)

		var resp ingestResponse
		overflow := false
		for scanner.Scan() {
			line := strings.TrimRight(scanner.Text(), "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			e := model.LogEvent{
				Service: service, Source: source, Level: level,
				Message: line, Raw: line,
			}
			e.Normalize(now)
			if overflow || !i.Enqueue(e) {
				overflow = true
				resp.Rejected++
				continue
			}
			resp.Accepted++
		}
		if err := scanner.Err(); err != nil {
			http.Error(w, "error reading body: "+err.Error(), http.StatusBadRequest)
			return
		}

		cl := r.ContentLength
		if cl < 0 {
			cl = 0
		}
		i.recordUsage(key, resp.Accepted, cl)
		status := http.StatusOK
		if overflow {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func clientIP(r *http.Request) string {
	if host, _, found := strings.Cut(r.RemoteAddr, ":"); found {
		return host
	}
	return r.RemoteAddr
}
