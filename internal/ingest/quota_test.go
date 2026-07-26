package ingest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/admission"
)

// chunkedBody is a body with no known length, which is what an HTTP client
// sends when it streams. Content-Length is -1 for these requests.
type chunkedBody struct{ r io.Reader }

func (c chunkedBody) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c chunkedBody) Close() error               { return nil }

func chunkedRequest(path, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Body = chunkedBody{r: strings.NewReader(body)}
	req.ContentLength = -1
	return req
}

// TestChunkedUploadsCountTowardByteQuota covers quota accounting for streamed
// requests. Both the pre-check and the recorded usage derived their byte count
// from Content-Length, which is -1 (clamped to 0) for a chunked upload — so
// chunked traffic accrued no usage and daily_quota_bytes never applied to it.
func TestChunkedUploadsCountTowardByteQuota(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"structured", "/api/v1/ingest", `{"message":"one"}` + "\n" + `{"message":"two"}` + "\n"},
		{"raw", "/api/v1/ingest/raw", "line one\nline two\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore(t)
			lim := admission.New(admission.Limits{DailyBytes: 1 << 20}, time.Now)
			ing := New(db, nil, Options{FlushInterval: time.Hour, Limiter: lim})
			ing.Start()
			defer ing.Stop()

			handler := ing.Handler()
			if tc.path == "/api/v1/ingest/raw" {
				handler = ing.RawHandler()
			}

			req := chunkedRequest(tc.path, tc.body)
			req = req.WithContext(WithIngestKey(req.Context(), "k"))
			rr := httptest.NewRecorder()
			handler(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
			}

			// Charge a request of exactly the remaining allowance: it should be
			// short by the bytes this upload consumed.
			if d := lim.Allow("k", 1<<20); d.Allowed {
				t.Fatal("chunked upload was recorded as 0 bytes; daily_quota_bytes does not apply to streamed requests")
			}
		})
	}
}

// TestClientIPHandlesIPv6 covers the source fallback for raw ingest. Cutting
// RemoteAddr at the first ':' turns "[::1]:54321" into "[".
func TestClientIPHandlesIPv6(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.10:34567", "192.0.2.10"},
		{"[2001:db8::1]:34567", "2001:db8::1"},
		{"[::1]:8080", "::1"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest/raw", nil)
		req.RemoteAddr = tc.remote
		if got := clientIP(req); got != tc.want {
			t.Errorf("clientIP(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

// TestOversizeBodyIsRejected: the body is now consumed as a stream, so both the
// whole-body cap and the per-line cap have to be enforced during the read
// rather than by buffering everything first. Both are size failures (413).
func TestOversizeBodyIsRejected(t *testing.T) {
	// A small body cap keeps this cheap; the production default is 32 MiB.
	const bodyCap = 64 << 10
	oneLine := strings.Repeat("x", defaultMaxLineBytes+1024)
	manyLines := strings.Repeat(strings.Repeat("y", 255)+"\n", (bodyCap/256)+16)

	for _, tc := range []struct{ name, body string }{
		{"single line over the line cap", oneLine},
		{"many lines over the body cap", manyLines},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore(t)
			ing := New(db, nil, Options{FlushInterval: time.Hour, BufferSize: 1 << 16, MaxBodyBytes: bodyCap})
			ing.Start()
			defer ing.Stop()

			req := chunkedRequest("/api/v1/ingest/raw", tc.body)
			rr := httptest.NewRecorder()
			ing.RawHandler()(rr, req)
			if rr.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("oversize body = %d, want 413 (%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestPartialJSONArrayReportsWhatWasAccepted: streaming the array means records
// before a syntax error are already enqueued, so the response has to say so
// rather than imply the whole request was a no-op.
func TestPartialJSONArrayReportsWhatWasAccepted(t *testing.T) {
	db := newStore(t)
	ing := New(db, nil, Options{FlushInterval: time.Hour})
	ing.Start()
	defer ing.Stop()

	body := `[{"message":"good one"},{"message":"good two"},{oops}]`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(body))
	rr := httptest.NewRecorder()
	ing.Handler()(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed array = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"accepted":2`) {
		t.Fatalf("response should report the records already accepted: %s", rr.Body.String())
	}
}

// TestPrettyPrintedJSONIsAccepted covers multi-line records. Splitting the body
// purely on newlines rejected a valid, pretty-printed application/json object as
// several broken fragments — and returned 200 while doing it.
func TestPrettyPrintedJSONIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		accepted int
		rejected int
	}{
		{"pretty single object", "{\n  \"service\": \"api\",\n  \"message\": \"hi\"\n}", 1, 0},
		{"pretty array", "[\n  {\"service\":\"api\",\"message\":\"a\"},\n  {\"service\":\"api\",\"message\":\"b\"}\n]", 2, 0},
		{"compact single object", `{"service":"api","message":"hi"}`, 1, 0},
		{"two pretty objects back to back", "{\n \"message\": \"a\"\n}\n{\n \"message\": \"b\"\n}", 2, 0},
		{"pretty object then ndjson line", "{\n \"message\": \"a\"\n}\n{\"message\":\"b\"}", 2, 0},
		// Per-record resilience must survive: a bad record in the middle is
		// rejected on its own without swallowing the records after it.
		{"bad line between good ones", "{\"message\":\"a\"}\nnot-json\n{\"message\":\"b\"}", 2, 1},
		{"trailing truncated object", "{\"message\":\"a\"}\n{\"message\":", 1, 1},
		{"blank lines between records", "{\"message\":\"a\"}\n\n\n{\"message\":\"b\"}\n", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore(t)
			ing := New(db, nil, Options{FlushInterval: time.Hour})
			ing.Start()
			defer ing.Stop()

			rr := httptest.NewRecorder()
			ing.Handler()(rr, httptest.NewRequest(http.MethodPost, "/api/v1/ingest", strings.NewReader(tc.body)))

			var got struct {
				Accepted int `json:"accepted"`
				Rejected int `json:"rejected"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response %q: %v", rr.Body.String(), err)
			}
			if got.Accepted != tc.accepted || got.Rejected != tc.rejected {
				t.Fatalf("accepted=%d rejected=%d, want %d/%d (%s)",
					got.Accepted, got.Rejected, tc.accepted, tc.rejected, rr.Body.String())
			}
		})
	}
}
