package otlp

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// maxBodyBytes caps one OTLP export request.
const maxBodyBytes = 32 << 20

// Sink accepts a decoded event, returning false when it was refused because the
// ingest buffer is full.
type Sink func(model.LogEvent) bool

// Options configures the OTLP handler.
type Options struct {
	Sink   Sink
	Now    func() time.Time
	Logger *slog.Logger
}

// partialSuccess is the OTLP response shape. The spec requires a body even on
// full success, and requires rejected counts to be reported rather than the
// request simply failing — a collector uses this to decide what to retry.
type exportResponse struct {
	PartialSuccess *partialSuccess `json:"partialSuccess,omitempty"`
}

type partialSuccess struct {
	RejectedLogRecords int64  `json:"rejectedLogRecords"`
	ErrorMessage       string `json:"errorMessage,omitempty"`
}

// Handler returns an http.Handler for POST /v1/logs, accepting both the
// protobuf and JSON encodings and gzip content-encoding.
func Handler(opts Options) http.HandlerFunc {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(w, r)
		if err != nil {
			http.Error(w, err.Error(), statusForReadError(err))
			return
		}

		ct := r.Header.Get("Content-Type")
		var events []model.LogEvent
		switch {
		case strings.Contains(ct, "application/json"):
			events, err = DecodeJSON(body, now())
		default:
			// protobuf is the default OTLP encoding, and some exporters omit the
			// header entirely, so it is what an unrecognised content type gets.
			events, err = DecodeProtobuf(body, now())
		}
		if err != nil {
			logger.Warn("otlp: could not decode export request", "content_type", ct, "error", err)
			writeExport(w, http.StatusBadRequest, exportResponse{
				PartialSuccess: &partialSuccess{ErrorMessage: err.Error()},
			})
			return
		}

		var rejected int64
		for _, e := range events {
			if !opts.Sink(e) {
				rejected++
			}
		}

		if rejected > 0 {
			// The spec's partial-success path: tell the collector exactly how
			// many records did not land so it can retry those rather than
			// resending everything or silently losing them.
			writeExport(w, http.StatusOK, exportResponse{PartialSuccess: &partialSuccess{
				RejectedLogRecords: rejected,
				ErrorMessage:       "ingest buffer full",
			}})
			return
		}
		writeExport(w, http.StatusOK, exportResponse{})
	}
}

var errTooLarge = errors.New("otlp: request body too large")

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return nil, errors.New("otlp: invalid gzip body")
		}
		defer gz.Close()
		// Bound the decompressed size too: the cap above limits what arrives on
		// the wire, not what it expands to.
		reader = io.LimitReader(gz, maxBodyBytes)
	}
	b, err := io.ReadAll(reader)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, errTooLarge
		}
		return nil, errors.New("otlp: could not read the request body")
	}
	return b, nil
}

func statusForReadError(err error) int {
	if errors.Is(err, errTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func writeExport(w http.ResponseWriter, status int, resp exportResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}
