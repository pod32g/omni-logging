package ingest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/pod32g/omni-logging/internal/model"
)

// defaultMaxBodyBytes caps a single ingest request to protect memory. Override
// per-Ingestor with Options.MaxBodyBytes.
const defaultMaxBodyBytes = 32 << 20 // 32 MiB

// readBufBytes is the streaming read buffer, and therefore the largest single
// NDJSON line or raw log line accepted. Bodies are consumed incrementally, so
// request memory is bounded by this rather than by the whole-body cap.
const readBufBytes = 1 << 20 // 1 MiB

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

// countingReader tallies the bytes actually read. Quota accounting uses this
// instead of Content-Length, which is -1 for a chunked request and would let
// chunked uploads accumulate no usage at all.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Handler accepts structured logs as either a JSON array of objects or NDJSON
// (one JSON object per line). Malformed records are reported per-record;
// well-formed records are enqueued. If the buffer fills mid-batch, the response
// is 429 and the overflow is reported as rejected.
//
// The body is consumed as a stream rather than buffered whole: a 32 MiB cap
// per request times the number of concurrent producers is a lot of live heap
// for a server whose job is to stay up under load.
func (i *Ingestor) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, ok := i.admit(w, r)
		if !ok {
			return
		}
		counter := &countingReader{r: http.MaxBytesReader(w, r.Body, i.opts.MaxBodyBytes)}
		br := bufio.NewReaderSize(counter, readBufBytes)
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

		first, err := peekFirstNonSpace(br)
		if err != nil && err != io.EOF {
			i.recordUsage(key, resp.Accepted, counter.n)
			http.Error(w, "error reading body: "+err.Error(), readErrStatus(err))
			return
		}

		if first == '[' {
			// JSON array form, decoded element by element.
			dec := json.NewDecoder(br)
			if _, terr := dec.Token(); terr != nil { // consumes '['
				i.recordUsage(key, resp.Accepted, counter.n)
				http.Error(w, "invalid JSON array: "+terr.Error(), readErrStatus(terr))
				return
			}
			idx := 0
			for dec.More() {
				var raw json.RawMessage
				if derr := dec.Decode(&raw); derr != nil {
					// Records before the syntax error were already accepted, so
					// report the partial outcome rather than implying none were.
					resp.Rejected++
					resp.Errors = append(resp.Errors, recordError{Index: idx, Error: "invalid JSON array: " + derr.Error()})
					i.recordUsage(key, resp.Accepted, counter.n)
					writeJSON(w, http.StatusBadRequest, resp)
					return
				}
				emit(idx, raw)
				idx++
			}
		} else {
			// NDJSON form (also handles a single object).
			scanner := bufio.NewScanner(br)
			scanner.Buffer(make([]byte, 0, 64*1024), readBufBytes)
			idx := 0
			for scanner.Scan() {
				emit(idx, scanner.Bytes())
				idx++
			}
			if serr := scanner.Err(); serr != nil {
				i.recordUsage(key, resp.Accepted, counter.n)
				http.Error(w, "error reading body: "+serr.Error(), readErrStatus(serr))
				return
			}
		}

		i.recordUsage(key, resp.Accepted, counter.n)
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

		counter := &countingReader{r: http.MaxBytesReader(w, r.Body, i.opts.MaxBodyBytes)}
		scanner := bufio.NewScanner(counter)
		scanner.Buffer(make([]byte, 0, 64*1024), readBufBytes)

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
			i.recordUsage(key, resp.Accepted, counter.n)
			http.Error(w, "error reading body: "+err.Error(), readErrStatus(err))
			return
		}

		i.recordUsage(key, resp.Accepted, counter.n)
		status := http.StatusOK
		if overflow {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, resp)
	}
}

// peekFirstNonSpace discards leading whitespace and returns the first
// meaningful byte without consuming it.
func peekFirstNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		return b, br.UnreadByte()
	}
}

// readErrStatus maps a body-read failure to a status code. Both the whole-body
// cap and the per-line cap are size problems and report 413; anything else is a
// malformed request.
func readErrStatus(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) || errors.Is(err, bufio.ErrTooLong) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
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

// clientIP returns the peer address without its port. net.SplitHostPort is
// required rather than cutting at the first ':' — an IPv6 RemoteAddr like
// "[::1]:54321" would otherwise yield "[".
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
