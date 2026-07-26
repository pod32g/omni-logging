package otlp

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// This file speaks gRPC directly rather than through grpc-go.
//
// That sounds bolder than it is. gRPC is HTTP/2 plus three conventions: the
// path is /package.Service/Method, each message on the wire is prefixed with a
// compression flag and a big-endian length, and the call's outcome arrives in
// HTTP trailers rather than the HTTP status (which is always 200). This package
// already decodes OTLP protobuf by hand, so the only thing grpc-go would add is
// twelve modules of machinery for a single unary method.
//
// The result is wire-compatible: a stock OpenTelemetry gRPC exporter, grpcurl,
// or any generated client talks to it without knowing the difference.

// LogsServiceExportMethod is the gRPC path an OTLP logs exporter posts to.
const LogsServiceExportMethod = "/opentelemetry.proto.collector.logs.v1.LogsService/Export"

// gRPC status codes, from the canonical set. Only the ones this service can
// actually return are named.
const (
	codeOK                = 0
	codeInvalidArgument   = 3
	codeResourceExhausted = 8
	codeUnimplemented     = 12
	codeInternal          = 13
	codeUnauthenticated   = 16
)

// maxRecvMsgBytes caps a single gRPC message after decompression. gRPC's own
// default is 4 MiB; this matches the HTTP ingest cap so the two paths accept
// the same payloads rather than one silently refusing what the other takes.
const maxRecvMsgBytes = maxBodyBytes

// frameHeaderLen is the compressed flag (1) plus the big-endian length (4).
const frameHeaderLen = 5

// GRPCOptions configures the gRPC OTLP handler.
type GRPCOptions struct {
	Options

	// Keys returns the currently valid ingest keys. Nil, or an empty result,
	// disables authentication — the same dev-mode behaviour as HTTP ingest, and
	// deliberately so: the two ingest paths must not disagree about whether a
	// key is required, or opening the gRPC port would quietly bypass the keys
	// guarding /v1/logs.
	Keys func() []string
}

// GRPCHandler returns an http.Handler implementing the OTLP LogsService over
// gRPC. It must be served over HTTP/2 — see GRPCServer, which arranges that for
// both cleartext (h2c) and TLS.
func GRPCHandler(opts GRPCOptions) http.Handler {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// gRPC requires HTTP/2. Anything else cannot receive trailers, so the
		// call could never complete; say so in a way a human reading curl output
		// can act on.
		if r.ProtoMajor != 2 {
			http.Error(w, "gRPC requires HTTP/2; this port speaks h2c (cleartext HTTP/2) or HTTP/2 over TLS", http.StatusHTTPVersionNotSupported)
			return
		}
		if r.Method != http.MethodPost {
			writeGRPCStatus(w, codeUnimplemented, "gRPC calls are POST")
			return
		}
		// The spec's own status for a non-gRPC content type is HTTP 415, not a
		// gRPC status, because the peer has not proven it speaks gRPC at all.
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/grpc") {
			http.Error(w, "expected content-type application/grpc", http.StatusUnsupportedMediaType)
			return
		}
		if r.URL.Path != LogsServiceExportMethod {
			writeGRPCStatus(w, codeUnimplemented, "unknown method "+r.URL.Path)
			return
		}
		if !grpcAuthorized(r, opts.Keys) {
			writeGRPCStatus(w, codeUnauthenticated, "invalid or missing ingest key")
			return
		}

		frames, err := readFrames(r.Body, r.Header.Get("Grpc-Encoding"))
		if err != nil {
			var re *recvError
			if errors.As(err, &re) {
				writeGRPCStatus(w, re.code, re.msg)
				return
			}
			writeGRPCStatus(w, codeInternal, "could not read the request stream")
			return
		}

		// Export is a unary method, so a well-behaved client sends exactly one
		// message. Extra frames are decoded rather than refused: they carry real
		// records, and dropping data to make a point about framing helps nobody.
		var (
			rejected int64
			total    int
		)
		for _, f := range frames {
			events, derr := DecodeProtobuf(f, now())
			if derr != nil {
				logger.Warn("otlp/grpc: could not decode export request", "error", derr)
				writeGRPCStatus(w, codeInvalidArgument, derr.Error())
				return
			}
			total += len(events)
			for _, e := range events {
				if !opts.Sink(e) {
					rejected++
				}
			}
		}

		// Partial success is reported in the response message with an OK status,
		// exactly as over HTTP: the call succeeded, some records did not land,
		// and the collector needs the count to retry only those.
		body := encodeExportResponse(rejected, "ingest buffer full")
		writeGRPCMessage(w, body)
		if rejected > 0 && int64(total) == rejected {
			logger.Warn("otlp/grpc: every record was refused", "records", total)
		}
		setGRPCTrailer(w, codeOK, "")
	})
}

// grpcAuthorized checks the ingest key. gRPC metadata is just HTTP/2 headers,
// so the same x-api-key an HTTP client sends works unchanged, as does an
// authorization: Bearer header for clients that only expose that knob.
func grpcAuthorized(r *http.Request, keys func() []string) bool {
	if keys == nil {
		return true
	}
	valid := keys()
	if len(valid) == 0 {
		return true // dev mode: no keys configured
	}
	provided := r.Header.Get("X-Api-Key")
	if provided == "" {
		if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
			provided = a[7:]
		}
	}
	for _, k := range valid {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(k)) == 1 {
			return true
		}
	}
	return false
}

// recvError carries a gRPC status out of frame decoding.
type recvError struct {
	code int
	msg  string
}

func (e *recvError) Error() string { return e.msg }

// readFrames decodes the length-prefixed gRPC message stream.
func readFrames(body io.Reader, encoding string) ([][]byte, error) {
	// Bound the whole stream, not just each message: many small frames would
	// otherwise add up to an unbounded read.
	r := io.LimitReader(body, maxRecvMsgBytes+frameHeaderLen)

	var out [][]byte
	var hdr [frameHeaderLen]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil // clean end of stream
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil, &recvError{codeInvalidArgument, "truncated gRPC frame header"}
			}
			return nil, err
		}
		compressed := hdr[0]
		length := binary.BigEndian.Uint32(hdr[1:])
		if length > maxRecvMsgBytes {
			return nil, &recvError{codeResourceExhausted,
				fmt.Sprintf("message of %d bytes exceeds the %d byte limit", length, maxRecvMsgBytes)}
		}

		msg := make([]byte, length)
		if _, err := io.ReadFull(r, msg); err != nil {
			return nil, &recvError{codeInvalidArgument, "truncated gRPC message body"}
		}

		switch compressed {
		case 0:
			// identity
		case 1:
			var derr error
			if msg, derr = decompressFrame(msg, encoding); derr != nil {
				return nil, derr
			}
		default:
			return nil, &recvError{codeInvalidArgument,
				fmt.Sprintf("invalid compressed-flag %d", compressed)}
		}
		out = append(out, msg)
	}
}

// decompressFrame expands one compressed message. Only gzip is advertised, so
// anything else is UNIMPLEMENTED — the status that makes a gRPC client retry
// with identity encoding rather than give up.
func decompressFrame(msg []byte, encoding string) ([]byte, error) {
	if !strings.EqualFold(encoding, "gzip") {
		return nil, &recvError{codeUnimplemented,
			fmt.Sprintf("grpc-encoding %q is not supported; use identity or gzip", encoding)}
	}
	zr, err := gzip.NewReader(bytes.NewReader(msg))
	if err != nil {
		return nil, &recvError{codeInvalidArgument, "message is flagged compressed but is not valid gzip"}
	}
	defer zr.Close()
	// Read one byte past the cap so a body that expands to exactly the limit is
	// distinguishable from one that overruns it.
	out, err := io.ReadAll(io.LimitReader(zr, maxRecvMsgBytes+1))
	if err != nil {
		return nil, &recvError{codeInvalidArgument, "could not decompress the message"}
	}
	if len(out) > maxRecvMsgBytes {
		return nil, &recvError{codeResourceExhausted, "decompressed message exceeds the size limit"}
	}
	return out, nil
}

// writeGRPCMessage writes response headers and one length-prefixed message.
func writeGRPCMessage(w http.ResponseWriter, msg []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Accept-Encoding", "identity, gzip")
	// Trailers must be announced before the body: the HTTP/2 writer needs to
	// know which headers to hold back for the trailing HEADERS frame, and a
	// gRPC client treats a call with no grpc-status as failed.
	h.Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)

	var hdr [frameHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
	_, _ = w.Write(hdr[:])
	_, _ = w.Write(msg)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeGRPCStatus ends a call with an error and no response message.
func writeGRPCStatus(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/grpc")
	h.Set("Grpc-Accept-Encoding", "identity, gzip")
	h.Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK) // gRPC reports failures in trailers, not here
	setGRPCTrailer(w, code, msg)
}

func setGRPCTrailer(w http.ResponseWriter, code int, msg string) {
	h := w.Header()
	h.Set("Grpc-Status", itoa(code))
	if msg != "" {
		h.Set("Grpc-Message", percentEncode(msg))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// percentEncode escapes a grpc-message value. The spec restricts it to printable
// ASCII, so anything outside that range — including the UTF-8 in a Go error
// string — has to be escaped or the client sees a mangled header.
func percentEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c <= 0x7E && c != '%' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}

// encodeExportResponse builds an ExportLogsServiceResponse.
//
//	message ExportLogsServiceResponse { ExportLogsPartialSuccess partial_success = 1; }
//	message ExportLogsPartialSuccess { int64 rejected_log_records = 1; string error_message = 2; }
//
// Full success is the empty message — zero bytes — which is what the spec asks
// for and what exporters expect.
func encodeExportResponse(rejected int64, errMsg string) []byte {
	if rejected == 0 {
		return nil
	}
	var inner []byte
	inner = appendTag(inner, 1, wireVarint)
	inner = appendVarint(inner, uint64(rejected))
	if errMsg != "" {
		inner = appendTag(inner, 2, wireBytes)
		inner = appendVarint(inner, uint64(len(errMsg)))
		inner = append(inner, errMsg...)
	}

	var out []byte
	out = appendTag(out, 1, wireBytes)
	out = appendVarint(out, uint64(len(inner)))
	return append(out, inner...)
}

func appendTag(b []byte, field, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
