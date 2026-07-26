package otlp

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

var now = time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

// --- a tiny protobuf encoder, so the decoder is tested against real wire bytes
// rather than against bytes produced by the same logic that reads them.

type pb struct{ buf []byte }

func (p *pb) varintField(field int, v uint64) {
	p.tag(field, wireVarint)
	p.buf = binary.AppendUvarint(p.buf, v)
}

func (p *pb) fixed64Field(field int, v uint64) {
	p.tag(field, wireI64)
	p.buf = binary.LittleEndian.AppendUint64(p.buf, v)
}

func (p *pb) bytesField(field int, b []byte) {
	p.tag(field, wireBytes)
	p.buf = binary.AppendUvarint(p.buf, uint64(len(b)))
	p.buf = append(p.buf, b...)
}

func (p *pb) stringField(field int, s string) { p.bytesField(field, []byte(s)) }

func (p *pb) tag(field, wire int) {
	p.buf = binary.AppendUvarint(p.buf, uint64(field<<3|wire))
}

func anyString(s string) []byte {
	var v pb
	v.stringField(fAnyString, s)
	return v.buf
}

func anyInt(n int64) []byte {
	var v pb
	v.varintField(fAnyInt, uint64(n))
	return v.buf
}

func keyValue(key string, value []byte) []byte {
	var kv pb
	kv.stringField(fKeyValueKey, key)
	kv.bytesField(fKeyValueValue, value)
	return kv.buf
}

// buildExport assembles a realistic ExportLogsServiceRequest.
func buildExport() []byte {
	var rec pb
	rec.fixed64Field(fLogTimeUnixNano, uint64(now.UnixNano()))
	rec.fixed64Field(fLogObservedTimeUnixNano, uint64(now.Add(time.Second).UnixNano()))
	rec.varintField(fLogSeverityNumber, 17) // ERROR
	rec.stringField(fLogSeverityText, "Error")
	rec.bytesField(fLogBody, anyString("connection refused"))
	rec.bytesField(fLogAttributes, keyValue("http.status", anyInt(500)))
	rec.bytesField(fLogAttributes, keyValue("component", anyString("db")))
	traceID, _ := hex.DecodeString("0102030405060708090a0b0c0d0e0f10")
	rec.bytesField(fLogTraceID, traceID)

	var scope pb
	scope.stringField(fScopeName, "my.instrumentation")

	var scopeLogs pb
	scopeLogs.bytesField(fScopeLogsScope, scope.buf)
	scopeLogs.bytesField(fScopeLogsLogRecords, rec.buf)

	var resource pb
	resource.bytesField(fResourceAttributes, keyValue("service.name", anyString("checkout-api")))
	resource.bytesField(fResourceAttributes, keyValue("host.name", anyString("node-7")))

	var resourceLogs pb
	resourceLogs.bytesField(fResourceLogsResource, resource.buf)
	resourceLogs.bytesField(fResourceLogsScopeLogs, scopeLogs.buf)

	var req pb
	req.bytesField(fLogsDataResourceLogs, resourceLogs.buf)
	return req.buf
}

func TestDecodeProtobuf(t *testing.T) {
	events, err := DecodeProtobuf(buildExport(), now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("decoded %d events, want 1", len(events))
	}
	e := events[0]

	if e.Message != "connection refused" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Service != "checkout-api" {
		t.Errorf("service = %q, want it from the service.name resource attribute", e.Service)
	}
	if e.Source != "node-7" {
		t.Errorf("source = %q, want it from host.name", e.Source)
	}
	if e.Level != model.LevelError {
		t.Errorf("level = %q, want error for severity 17", e.Level)
	}
	if !e.Timestamp.Equal(now) {
		t.Errorf("timestamp = %s, want %s", e.Timestamp, now)
	}
	if e.Attributes["http.status"] != int64(500) {
		t.Errorf("http.status = %#v, want int64(500)", e.Attributes["http.status"])
	}
	if e.Attributes["component"] != "db" {
		t.Errorf("component = %#v", e.Attributes["component"])
	}
	if e.Attributes["trace_id"] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("trace_id = %#v", e.Attributes["trace_id"])
	}
	// Resource attributes are kept too, so they stay searchable.
	if e.Attributes["service.name"] != "checkout-api" {
		t.Errorf("resource attributes were dropped: %#v", e.Attributes)
	}
	if e.ID == "" {
		t.Error("event was not normalized")
	}
}

// TestDecodeProtobufSkipsUnknownFields: forward compatibility with newer OTLP
// versions matters more than strictness — an added field must not fail the
// whole export.
func TestDecodeProtobufSkipsUnknownFields(t *testing.T) {
	var rec pb
	rec.bytesField(fLogBody, anyString("hello"))
	rec.varintField(99, 12345)          // unknown varint
	rec.stringField(98, "future field") // unknown string
	rec.fixed64Field(97, 42)            // unknown 64-bit

	var scopeLogs pb
	scopeLogs.bytesField(fScopeLogsLogRecords, rec.buf)
	var resourceLogs pb
	resourceLogs.bytesField(fResourceLogsScopeLogs, scopeLogs.buf)
	var req pb
	req.bytesField(fLogsDataResourceLogs, resourceLogs.buf)

	events, err := DecodeProtobuf(req.buf, now)
	if err != nil {
		t.Fatalf("unknown fields should be skipped, got: %v", err)
	}
	if len(events) != 1 || events[0].Message != "hello" {
		t.Fatalf("events = %+v", events)
	}
}

func TestDecodeProtobufRejectsTruncatedInput(t *testing.T) {
	full := buildExport()
	for _, n := range []int{1, len(full) / 3, len(full) / 2, len(full) - 2} {
		if _, err := DecodeProtobuf(full[:n], now); err == nil {
			t.Errorf("truncating to %d bytes should have failed to decode", n)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	body := `{"resourceLogs":[{
	  "resource":{"attributes":[
	    {"key":"service.name","value":{"stringValue":"billing"}},
	    {"key":"host.name","value":{"stringValue":"node-2"}}]},
	  "scopeLogs":[{"scope":{"name":"scope"},"logRecords":[{
	    "timeUnixNano":"1785000000000000000",
	    "severityNumber":13,
	    "severityText":"Warn",
	    "body":{"stringValue":"disk almost full"},
	    "attributes":[
	      {"key":"pct","value":{"doubleValue":91.5}},
	      {"key":"ok","value":{"boolValue":false}},
	      {"key":"tags","value":{"arrayValue":{"values":[{"stringValue":"a"},{"stringValue":"b"}]}}}],
	    "traceId":"abc123"}]}]}]}`

	events, err := DecodeJSON([]byte(body), now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("decoded %d events, want 1", len(events))
	}
	e := events[0]
	if e.Message != "disk almost full" || e.Service != "billing" || e.Source != "node-2" {
		t.Errorf("event = %+v", e)
	}
	if e.Level != model.LevelWarn {
		t.Errorf("level = %q, want warn for severity 13", e.Level)
	}
	if !e.Timestamp.Equal(time.Unix(0, 1785000000000000000).UTC()) {
		t.Errorf("timestamp = %s", e.Timestamp)
	}
	if e.Attributes["pct"] != 91.5 {
		t.Errorf("pct = %#v", e.Attributes["pct"])
	}
	if e.Attributes["ok"] != false {
		t.Errorf("ok = %#v", e.Attributes["ok"])
	}
	if arr, ok := e.Attributes["tags"].([]any); !ok || len(arr) != 2 {
		t.Errorf("tags = %#v", e.Attributes["tags"])
	}
	if e.Attributes["trace_id"] != "abc123" {
		t.Errorf("trace_id = %#v", e.Attributes["trace_id"])
	}
}

// TestSeverityMapping covers the whole OTLP severity range.
func TestSeverityMapping(t *testing.T) {
	for _, tc := range []struct {
		number uint64
		text   string
		want   model.Level
	}{
		{1, "", model.LevelDebug}, {4, "", model.LevelDebug},
		{5, "", model.LevelDebug}, {8, "", model.LevelDebug},
		{9, "", model.LevelInfo}, {12, "", model.LevelInfo},
		{13, "", model.LevelWarn}, {16, "", model.LevelWarn},
		{17, "", model.LevelError}, {20, "", model.LevelError},
		{21, "", model.LevelFatal}, {24, "", model.LevelFatal},
		// With no number, the text is the fallback rather than a silent "info".
		{0, "ERROR", model.LevelError},
		{0, "", model.LevelInfo},
	} {
		if got := severityToLevel(tc.number, tc.text); got != tc.want {
			t.Errorf("severity %d/%q = %q, want %q", tc.number, tc.text, got, tc.want)
		}
	}
}

// --- handler ---------------------------------------------------------------

type sink struct {
	mu     sync.Mutex
	events []model.LogEvent
	refuse bool
}

func (s *sink) accept(e model.LogEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse {
		return false
	}
	s.events = append(s.events, e)
	return true
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func post(t *testing.T, h http.HandlerFunc, body []byte, contentType, encoding string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func TestHandlerAcceptsBothEncodings(t *testing.T) {
	s := &sink{}
	h := Handler(Options{Sink: s.accept, Now: func() time.Time { return now }})

	// protobuf, the default an OTLP exporter sends
	rr := post(t, h, buildExport(), "application/x-protobuf", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("protobuf export = %d: %s", rr.Code, rr.Body.String())
	}
	// ...and with no Content-Type at all, which some exporters do.
	if rr := post(t, h, buildExport(), "", ""); rr.Code != http.StatusOK {
		t.Fatalf("export without a content type = %d", rr.Code)
	}
	// JSON
	body := `{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"json log"}}]}]}]}`
	if rr := post(t, h, []byte(body), "application/json", ""); rr.Code != http.StatusOK {
		t.Fatalf("json export = %d: %s", rr.Code, rr.Body.String())
	}
	if s.count() != 3 {
		t.Fatalf("sink received %d events, want 3", s.count())
	}
}

func TestHandlerAcceptsGzip(t *testing.T) {
	s := &sink{}
	h := Handler(Options{Sink: s.accept, Now: func() time.Time { return now }})

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(buildExport())
	gz.Close()

	if rr := post(t, h, buf.Bytes(), "application/x-protobuf", "gzip"); rr.Code != http.StatusOK {
		t.Fatalf("gzipped export = %d: %s", rr.Code, rr.Body.String())
	}
	if s.count() != 1 {
		t.Fatalf("sink received %d events", s.count())
	}
}

// TestHandlerReportsPartialSuccess: the spec's contract is that rejected
// records are counted in the response, so a collector retries those rather
// than resending everything or losing them silently.
func TestHandlerReportsPartialSuccess(t *testing.T) {
	s := &sink{refuse: true}
	h := Handler(Options{Sink: s.accept, Now: func() time.Time { return now }})

	rr := post(t, h, buildExport(), "application/x-protobuf", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a partial-success body", rr.Code)
	}
	var resp exportResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v (%s)", err, rr.Body.String())
	}
	if resp.PartialSuccess == nil || resp.PartialSuccess.RejectedLogRecords != 1 {
		t.Fatalf("expected 1 rejected record, got %s", rr.Body.String())
	}
}

func TestHandlerRejectsGarbage(t *testing.T) {
	s := &sink{}
	h := Handler(Options{Sink: s.accept, Now: func() time.Time { return now }})

	if rr := post(t, h, []byte("{not json"), "application/json", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON = %d, want 400", rr.Code)
	}
	// A protobuf body that is not decodable must fail rather than be accepted
	// as an empty export.
	if rr := post(t, h, []byte{0xff, 0xff, 0xff}, "application/x-protobuf", ""); rr.Code != http.StatusBadRequest {
		t.Errorf("malformed protobuf = %d, want 400", rr.Code)
	}
	if s.count() != 0 {
		t.Errorf("garbage produced %d events", s.count())
	}
}

func TestHandlerSuccessBodyIsValidJSON(t *testing.T) {
	s := &sink{}
	h := Handler(Options{Sink: s.accept, Now: func() time.Time { return now }})
	rr := post(t, h, buildExport(), "application/x-protobuf", "")
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content type = %q", ct)
	}
	var resp exportResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("success body is not valid JSON: %v", err)
	}
	if resp.PartialSuccess != nil {
		t.Errorf("a full success should not report partial success: %s", rr.Body.String())
	}
}
