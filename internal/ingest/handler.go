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

// defaultMaxLineBytes is the largest single NDJSON or raw log line accepted.
// Bodies are consumed incrementally, so per-request memory is bounded by this
// rather than by the whole-body cap. Raise it with Options.MaxLineBytes if a
// producer emits genuinely huge single lines (before the body was streamed, the
// effective ceiling was the whole-body cap).
const defaultMaxLineBytes = 1 << 20 // 1 MiB

// peekBufBytes only has to be large enough to look at the first byte.
const peekBufBytes = 64 << 10

// recordError describes why a single record in a batch was rejected.
type recordError struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// ingestResponse is the JSON body returned from ingest endpoints.
type ingestResponse struct {
	Accepted  int           `json:"accepted"`
	Rejected  int           `json:"rejected"`
	Duplicate bool          `json:"duplicate,omitempty"` // a retry of an already-accepted batch
	Errors    []recordError `json:"errors,omitempty"`
}

// duplicateBatch reports whether this request repeats a batch already accepted,
// answering 200 so the client stops retrying. A retry means the acknowledgement
// was lost, not the data, so re-ingesting would duplicate every event in it.
func (i *Ingestor) duplicateBatch(w http.ResponseWriter, r *http.Request) bool {
	id := r.Header.Get("X-Batch-Id")
	if id == "" || !i.dedupe.Seen(id) {
		return false
	}
	i.duplicates.Add(1)
	writeJSON(w, http.StatusOK, ingestResponse{Duplicate: true})
	return true
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
		if i.duplicateBatch(w, r) {
			return
		}
		counter := &countingReader{r: http.MaxBytesReader(w, r.Body, i.opts.MaxBodyBytes)}
		br := bufio.NewReaderSize(counter, peekBufBytes)
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
			// NDJSON form. Lines are accumulated until they form one complete JSON
			// value, so a pretty-printed object spanning several lines is accepted
			// (it is valid application/json, and splitting purely on newlines
			// rejected it as several broken fragments). A line that can never
			// become valid is still rejected on its own, which keeps one bad
			// record from swallowing the rest of the batch.
			scanner := bufio.NewScanner(br)
			scanner.Buffer(make([]byte, 0, 64*1024), i.opts.MaxLineBytes)
			var pending []byte
			idx := 0
			take := func() {
				emit(idx, pending)
				pending = pending[:0]
				idx++
			}
			for scanner.Scan() {
				line := scanner.Bytes()
				if len(pending) == 0 && len(bytes.TrimSpace(line)) == 0 {
					continue // blank separator between records
				}
				pending = append(pending, line...)
				pending = append(pending, '\n')
				if len(pending) > i.opts.MaxLineBytes {
					resp.Rejected++
					resp.Errors = append(resp.Errors, recordError{Index: idx, Error: "record exceeds the line size limit"})
					pending = pending[:0]
					idx++
					continue
				}
				// Both complete and hopeless buffers are handed to emit; emit
				// reports the parse error per record for the latter.
				if classifyJSON(pending) != jsonIncomplete {
					take()
				}
			}
			if serr := scanner.Err(); serr != nil {
				i.recordUsage(key, resp.Accepted, counter.n)
				http.Error(w, "error reading body: "+serr.Error(), readErrStatus(serr))
				return
			}
			if len(bytes.TrimSpace(pending)) > 0 {
				take() // truncated trailing value: reported as a rejected record
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
		if i.duplicateBatch(w, r) {
			return
		}
		service := firstNonEmpty(r.URL.Query().Get("service"), r.Header.Get("X-Service"))
		source := firstNonEmpty(r.URL.Query().Get("source"), r.Header.Get("X-Source"), clientIP(r))
		level := model.ParseLevel(firstNonEmpty(r.URL.Query().Get("level"), "info"))
		now := i.opts.Now()

		counter := &countingReader{r: http.MaxBytesReader(w, r.Body, i.opts.MaxBodyBytes)}
		scanner := bufio.NewScanner(counter)
		scanner.Buffer(make([]byte, 0, 64*1024), i.opts.MaxLineBytes)

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

// jsonState classifies a partially accumulated record.
type jsonState int

const (
	jsonComplete   jsonState = iota // exactly one JSON value, nothing trailing
	jsonIncomplete                  // a valid prefix; more lines are needed
	jsonInvalid                     // cannot become a single valid value
)

// classifyJSON distinguishes "still arriving" from "broken". That distinction is
// what lets multi-line records be reassembled without a malformed line
// swallowing everything after it: only a genuinely truncated value keeps
// accumulating. A buffer holding more than one value (two records jammed onto
// one line, or trailing garbage) counts as invalid, matching how a whole-line
// unmarshal used to treat it.
func classifyJSON(b []byte) jsonState {
	dec := json.NewDecoder(bytes.NewReader(b))
	var raw json.RawMessage
	switch err := dec.Decode(&raw); {
	case err == nil:
		if _, terr := dec.Token(); terr == io.EOF {
			return jsonComplete
		}
		return jsonInvalid
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return jsonIncomplete
	default:
		return jsonInvalid
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
