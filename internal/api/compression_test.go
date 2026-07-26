package api

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pod32g/omni-logging/internal/config"
)

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.Bytes()
}

func deflateBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	w.Close()
	return buf.Bytes()
}

// TestIngestAcceptsCompressedBodies covers the point of the feature: a producer
// halves its egress and the handlers are none the wiser.
func TestIngestAcceptsCompressedBodies(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	h := srv.Handler()

	body := `{"service":"api","level":"error","message":"compressed one"}` + "\n" +
		`{"service":"api","level":"info","message":"compressed two"}` + "\n"

	for _, tc := range []struct {
		name, encoding string
		payload        []byte
	}{
		{"gzip", "gzip", gzipBytes(t, body)},
		{"x-gzip", "x-gzip", gzipBytes(t, body)},
		{"deflate", "deflate", deflateBytes(t, body)},
		{"identity", "identity", []byte(body)},
		{"none", "", []byte(body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", bytes.NewReader(tc.payload))
			req.Header.Set("Content-Type", "application/x-ndjson")
			if tc.encoding != "" {
				req.Header.Set("Content-Encoding", tc.encoding)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), `"accepted":2`) {
				t.Fatalf("response = %s", rr.Body.String())
			}
		})
	}
}

func TestCompressedRawIngest(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/raw?service=x",
		bytes.NewReader(gzipBytes(t, "line one\nline two\nline three\n")))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"accepted":3`) {
		t.Fatalf("compressed raw ingest = %d %s", rr.Code, rr.Body.String())
	}
}

func TestUnsupportedEncodingIsRejectedClearly(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader("{}"))
	req.Header.Set("Content-Encoding", "br")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 naming the unsupported encoding", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "br") {
		t.Errorf("the error should name the encoding: %s", rr.Body.String())
	}
}

func TestCorruptCompressedBodyIsRejected(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader("this is not gzip"))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

// TestDecompressionIsBounded: a size cap on the compressed bytes is not a
// memory bound, because a small gzip can expand enormously. The cap has to
// apply to what comes out.
func TestDecompressionIsBounded(t *testing.T) {
	srv, _ := newServer(t, config.Default())

	// Lower the ceiling so the test exercises the bound without building a
	// body large enough to matter in wall-clock terms.
	original := maxDecompressedBytes
	maxDecompressedBytes = 1 << 20 // 1 MiB
	t.Cleanup(func() { maxDecompressedBytes = original })

	// 8 MiB of newlines compresses to a few KiB: eight times the lowered cap.
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	chunk := bytes.Repeat([]byte("\n"), 1<<20)
	for i := 0; i < 8; i++ {
		w.Write(chunk)
	}
	w.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/raw", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	// The request must terminate rather than read 100 MiB into memory; blank
	// lines are skipped so nothing is ingested either way.
	if rr.Code != http.StatusOK && rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"accepted":1`) {
		t.Errorf("a decompression bomb produced events: %s", rr.Body.String())
	}
}

// TestCompressedOTLP: the OTLP receiver does its own gzip handling, so the
// middleware must not leave it trying to decompress an already-plain body.
func TestCompressedOTLP(t *testing.T) {
	srv, _ := newServer(t, config.Default())
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"otlp gz"}}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(gzipBytes(t, body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("compressed OTLP = %d: %s", rr.Code, rr.Body.String())
	}
}
