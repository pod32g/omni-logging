package api

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
)

// maxDecompressedBytes bounds what a compressed body may expand to.
//
// This is the whole reason decompression is centralised rather than left to
// each handler: a request-size cap applied to the compressed bytes is not a
// memory bound at all, because a few kilobytes of gzip can expand to gigabytes.
// The handlers' own MaxBytesReader then applies to the decompressed stream,
// which is what they actually read.
// It is a var rather than a const only so a test can lower it: building a body
// that actually exceeds 64 MiB costs seconds, and the bound is the behaviour
// under test, not the specific number.
var maxDecompressedBytes int64 = 64 << 20 // 64 MiB

// decompressingBody wraps the decompressor so closing the request body also
// releases the decompressor's resources.
type decompressingBody struct {
	io.Reader
	closers []io.Closer
}

func (d *decompressingBody) Close() error {
	var firstErr error
	for _, c := range d.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// decompressRequests transparently decodes a compressed ingest body, so a
// producer can halve its egress without the handlers knowing anything about it.
//
// The Content-Encoding header is removed once the body is unwrapped, so any
// handler doing its own encoding check (the OTLP receiver does) sees plain
// bytes and does not try to decompress twice.
func decompressRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		if enc == "" || enc == "identity" || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}

		var (
			reader io.Reader
			closer io.Closer
		)
		switch enc {
		case "gzip", "x-gzip":
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, "invalid gzip body", http.StatusBadRequest)
				return
			}
			reader, closer = gz, gz
		case "deflate":
			fl := flate.NewReader(r.Body)
			reader, closer = fl, fl
		default:
			// Naming an encoding we cannot decode is better answered explicitly
			// than by letting the parser fail on bytes it cannot read.
			http.Error(w, "unsupported Content-Encoding: "+enc, http.StatusUnsupportedMediaType)
			return
		}

		body := &decompressingBody{
			Reader:  io.LimitReader(reader, maxDecompressedBytes),
			closers: []io.Closer{closer, r.Body},
		}
		r.Body = body
		r.Header.Del("Content-Encoding")
		// Content-Length described the compressed bytes and is now wrong;
		// leaving it would mislead anything sizing a buffer from it.
		r.Header.Del("Content-Length")
		r.ContentLength = -1

		next.ServeHTTP(w, r)
	})
}
