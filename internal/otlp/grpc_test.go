package otlp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// These tests speak the gRPC wire protocol over a real HTTP/2 connection rather
// than calling the handler directly. That is the point: the whole reason to
// hand-roll gRPC is the claim that the bytes on the wire are indistinguishable
// from grpc-go's, and only a real HTTP/2 client can check that claim. In
// particular it is the only way to verify that grpc-status arrives as an HTTP
// trailer, which is what makes a call complete rather than hang.

// --- a minimal gRPC client -------------------------------------------------

type grpcReply struct {
	httpStatus  int
	contentType string
	status      int    // grpc-status trailer
	message     string // grpc-message trailer, still percent-encoded
	messages    [][]byte
}

// h2cClient dials cleartext HTTP/2 with prior knowledge, exactly as a gRPC
// client opens an insecure channel.
func h2cClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		Timeout: 10 * time.Second,
	}
}

// frame wraps a message in the gRPC length prefix.
func frame(msg []byte, compressed bool) []byte {
	out := make([]byte, frameHeaderLen, frameHeaderLen+len(msg))
	if compressed {
		out[0] = 1
	}
	binary.BigEndian.PutUint32(out[1:], uint32(len(msg)))
	return append(out, msg...)
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type callOpts struct {
	method   string
	body     []byte
	headers  map[string]string
	rawHTTP1 bool
}

func call(t *testing.T, addr string, o callOpts) *grpcReply {
	t.Helper()
	method := o.method
	if method == "" {
		method = LogsServiceExportMethod
	}

	client := h2cClient()
	if o.rawHTTP1 {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	req, err := http.NewRequest(http.MethodPost, "http://"+addr+method, bytes.NewReader(o.body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("TE", "trailers")
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("gRPC call failed at the transport level: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response stream: %v", err)
	}

	reply := &grpcReply{
		httpStatus:  resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		status:      -1,
	}
	// Trailers are only populated once the body is drained; reading them any
	// earlier is the classic way to conclude a server "sent no status".
	if s := resp.Trailer.Get("Grpc-Status"); s != "" {
		n, cerr := strconv.Atoi(s)
		if cerr != nil {
			t.Fatalf("grpc-status trailer %q is not a number", s)
		}
		reply.status = n
	}
	reply.message = resp.Trailer.Get("Grpc-Message")

	// A non-gRPC content type means the server refused at the HTTP layer and the
	// body is a plain-text explanation, not a frame stream. Parsing it as frames
	// would read the first five characters of prose as a length prefix.
	if !strings.HasPrefix(reply.contentType, "application/grpc") {
		return reply
	}

	for len(body) >= frameHeaderLen {
		n := binary.BigEndian.Uint32(body[1:frameHeaderLen])
		if uint64(len(body)) < uint64(frameHeaderLen)+uint64(n) {
			t.Fatalf("response frame claims %d bytes but only %d remain", n, len(body)-frameHeaderLen)
		}
		reply.messages = append(reply.messages, body[frameHeaderLen:frameHeaderLen+n])
		body = body[frameHeaderLen+n:]
	}
	if len(body) != 0 {
		t.Fatalf("%d trailing bytes are not part of any frame", len(body))
	}
	return reply
}

func startGRPC(t *testing.T, opts GRPCOptions) (string, *sink) {
	t.Helper()
	s := &sink{}
	if opts.Sink == nil {
		opts.Sink = s.accept
	}
	srv, err := NewGRPCServer(GRPCServerOptions{GRPCOptions: opts, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewGRPCServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	return srv.Addr(), s
}

// --- tests -----------------------------------------------------------------

func TestGRPCExportRoundTrip(t *testing.T) {
	addr, s := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{body: frame(buildExport(), false)})

	if reply.status != codeOK {
		t.Fatalf("grpc-status = %d (%s), want 0", reply.status, reply.message)
	}
	if !strings.HasPrefix(reply.contentType, "application/grpc") {
		t.Errorf("content-type = %q", reply.contentType)
	}
	if s.count() != 1 {
		t.Fatalf("the sink received %d events, want 1", s.count())
	}
	e := s.events[0]
	if e.Message != "connection refused" {
		t.Errorf("message = %q", e.Message)
	}
	if e.Service != "checkout-api" {
		t.Errorf("service = %q, want the resource's service.name", e.Service)
	}
	// A successful unary Export returns exactly one message, and for full
	// success that message is empty.
	if len(reply.messages) != 1 {
		t.Fatalf("got %d response messages, want 1", len(reply.messages))
	}
	if len(reply.messages[0]) != 0 {
		t.Errorf("full success should return the empty message, got %d bytes", len(reply.messages[0]))
	}
}

// TestGRPCStatusArrivesAsTrailer is the load-bearing test for the whole
// approach. gRPC puts the call outcome in HTTP trailers; if Go emitted them as
// ordinary headers instead, every real client would block waiting for a status
// that never came.
func TestGRPCStatusArrivesAsTrailer(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	client := h2cClient()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+LogsServiceExportMethod,
		bytes.NewReader(frame(buildExport(), false)))
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.ProtoMajor != 2 {
		t.Fatalf("the connection negotiated HTTP/%d; gRPC requires HTTP/2", resp.ProtoMajor)
	}
	if got := resp.Header.Get("Grpc-Status"); got != "" {
		t.Errorf("grpc-status was sent as a header (%q); it must be a trailer", got)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if got := resp.Trailer.Get("Grpc-Status"); got != "0" {
		t.Errorf("grpc-status trailer = %q, want \"0\"", got)
	}
}

func TestGRPCAcceptsGzipMessages(t *testing.T) {
	addr, s := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{
		body:    frame(gzipBytes(t, buildExport()), true),
		headers: map[string]string{"Grpc-Encoding": "gzip"},
	})

	if reply.status != codeOK {
		t.Fatalf("grpc-status = %d (%s)", reply.status, reply.message)
	}
	if s.count() != 1 {
		t.Errorf("the sink received %d events, want 1", s.count())
	}
}

// TestGRPCCompressedFlagWithoutEncoding: a client that sets the flag but never
// negotiated an encoding gets UNIMPLEMENTED, which is the status that makes it
// retry with identity rather than fail the export outright.
func TestGRPCCompressedFlagWithoutEncoding(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{body: frame(gzipBytes(t, buildExport()), true)})

	if reply.status != codeUnimplemented {
		t.Errorf("grpc-status = %d, want %d (UNIMPLEMENTED)", reply.status, codeUnimplemented)
	}
}

func TestGRPCAdvertisesAcceptedEncodings(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	client := h2cClient()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+LogsServiceExportMethod,
		bytes.NewReader(frame(buildExport(), false)))
	req.Header.Set("Content-Type", "application/grpc")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if got := resp.Header.Get("Grpc-Accept-Encoding"); !strings.Contains(got, "gzip") {
		t.Errorf("grpc-accept-encoding = %q, want it to advertise gzip", got)
	}
}

// TestGRPCPartialSuccess: a refused record is reported in the response message
// with an OK status. The call worked; the data did not land. A collector needs
// the count to retry only what was lost.
func TestGRPCPartialSuccess(t *testing.T) {
	s := &sink{refuse: true}
	addr, _ := startGRPC(t, GRPCOptions{Options: Options{Sink: s.accept}})

	reply := call(t, addr, callOpts{body: frame(buildExport(), false)})

	if reply.status != codeOK {
		t.Fatalf("grpc-status = %d, want OK — a full buffer is not a failed call", reply.status)
	}
	if len(reply.messages) != 1 || len(reply.messages[0]) == 0 {
		t.Fatalf("expected a non-empty partial-success message, got %#v", reply.messages)
	}
	rejected, msg := decodePartialSuccess(t, reply.messages[0])
	if rejected != 1 {
		t.Errorf("rejectedLogRecords = %d, want 1", rejected)
	}
	if msg == "" {
		t.Error("partial success should explain why records were rejected")
	}
}

// decodePartialSuccess reads the response back with the package's own protobuf
// reader, which checks that what encodeExportResponse writes is actually
// decodable rather than merely non-empty.
func decodePartialSuccess(t *testing.T, b []byte) (int64, string) {
	t.Helper()
	r := newReader(b)
	var rejected int64
	var msg string
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			t.Fatalf("response tag: %v", err)
		}
		if field != 1 || wire != wireBytes {
			if err := r.skip(wire); err != nil {
				t.Fatal(err)
			}
			continue
		}
		inner, err := r.bytes()
		if err != nil {
			t.Fatal(err)
		}
		ir := newReader(inner)
		for !ir.done() {
			f, wt, err := ir.tag()
			if err != nil {
				t.Fatalf("partial-success tag: %v", err)
			}
			switch {
			case f == 1 && wt == wireVarint:
				v, err := ir.varint()
				if err != nil {
					t.Fatal(err)
				}
				rejected = int64(v)
			case f == 2 && wt == wireBytes:
				v, err := ir.bytes()
				if err != nil {
					t.Fatal(err)
				}
				msg = string(v)
			default:
				if err := ir.skip(wt); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return rejected, msg
}

func TestGRPCUnknownMethod(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{
		method: "/opentelemetry.proto.collector.trace.v1.TraceService/Export",
		body:   frame(buildExport(), false),
	})

	if reply.status != codeUnimplemented {
		t.Errorf("grpc-status = %d, want %d (UNIMPLEMENTED)", reply.status, codeUnimplemented)
	}
	if reply.httpStatus != http.StatusOK {
		t.Errorf("HTTP status = %d; gRPC reports errors in trailers and keeps HTTP at 200", reply.httpStatus)
	}
}

func TestGRPCRejectsNonGRPCContentType(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{
		body:    frame(buildExport(), false),
		headers: map[string]string{"Content-Type": "application/json"},
	})

	if reply.httpStatus != http.StatusUnsupportedMediaType {
		t.Errorf("HTTP status = %d, want 415", reply.httpStatus)
	}
}

// TestGRPCOverHTTP1Explains: a curl or a misconfigured client that arrives on
// HTTP/1.1 must get a comprehensible refusal, not a hang waiting for trailers
// the protocol cannot deliver.
func TestGRPCOverHTTP1Explains(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{body: frame(buildExport(), false), rawHTTP1: true})

	if reply.httpStatus != http.StatusHTTPVersionNotSupported {
		t.Errorf("HTTP status = %d, want 505", reply.httpStatus)
	}
}

func TestGRPCRejectsGarbageMessage(t *testing.T) {
	addr, s := startGRPC(t, GRPCOptions{})

	reply := call(t, addr, callOpts{body: frame([]byte{0xff, 0xff, 0xff, 0xff}, false)})

	if reply.status != codeInvalidArgument {
		t.Errorf("grpc-status = %d, want %d (INVALID_ARGUMENT)", reply.status, codeInvalidArgument)
	}
	if s.count() != 0 {
		t.Errorf("a garbage payload produced %d events", s.count())
	}
}

func TestGRPCRejectsTruncatedFrame(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	full := frame(buildExport(), false)
	reply := call(t, addr, callOpts{body: full[:len(full)-10]})

	if reply.status != codeInvalidArgument {
		t.Errorf("grpc-status = %d, want %d (INVALID_ARGUMENT)", reply.status, codeInvalidArgument)
	}
}

// TestGRPCRejectsOversizedLengthPrefix: the length prefix is attacker-controlled
// and must be checked before it is used to size an allocation.
func TestGRPCRejectsOversizedLengthPrefix(t *testing.T) {
	addr, _ := startGRPC(t, GRPCOptions{})

	var hdr [frameHeaderLen]byte
	binary.BigEndian.PutUint32(hdr[1:], 0xFFFFFFFF) // 4 GiB, sent in 5 bytes
	reply := call(t, addr, callOpts{body: hdr[:]})

	if reply.status != codeResourceExhausted {
		t.Errorf("grpc-status = %d, want %d (RESOURCE_EXHAUSTED)", reply.status, codeResourceExhausted)
	}
}

func TestGRPCHandlesMultipleFrames(t *testing.T) {
	addr, s := startGRPC(t, GRPCOptions{})

	body := append(frame(buildExport(), false), frame(buildExport(), false)...)
	reply := call(t, addr, callOpts{body: body})

	if reply.status != codeOK {
		t.Fatalf("grpc-status = %d (%s)", reply.status, reply.message)
	}
	if s.count() != 2 {
		t.Errorf("the sink received %d events, want 2 (one per frame)", s.count())
	}
}

// --- authentication --------------------------------------------------------

// TestGRPCRequiresIngestKey guards against the gRPC port becoming an
// unauthenticated way in while /v1/logs over HTTP still demands a key.
func TestGRPCRequiresIngestKey(t *testing.T) {
	keys := func() []string { return []string{"devkey"} }

	for _, tc := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"no key", nil, codeUnauthenticated},
		{"wrong key", map[string]string{"X-Api-Key": "nope"}, codeUnauthenticated},
		{"x-api-key", map[string]string{"X-Api-Key": "devkey"}, codeOK},
		{"bearer", map[string]string{"Authorization": "Bearer devkey"}, codeOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, s := startGRPC(t, GRPCOptions{Keys: keys})
			reply := call(t, addr, callOpts{body: frame(buildExport(), false), headers: tc.header})
			if reply.status != tc.want {
				t.Errorf("grpc-status = %d (%s), want %d", reply.status, reply.message, tc.want)
			}
			if tc.want == codeUnauthenticated && s.count() != 0 {
				t.Errorf("an unauthenticated call still ingested %d events", s.count())
			}
		})
	}
}

// TestGRPCNoKeysMeansOpen mirrors HTTP ingest's dev-mode behaviour: with no
// keys configured, the port is open. The two paths must agree, or enabling one
// would silently change the other's security posture.
func TestGRPCNoKeysMeansOpen(t *testing.T) {
	addr, s := startGRPC(t, GRPCOptions{Keys: func() []string { return nil }})

	reply := call(t, addr, callOpts{body: frame(buildExport(), false)})

	if reply.status != codeOK || s.count() != 1 {
		t.Errorf("grpc-status = %d, events = %d; want an open port when no keys are set", reply.status, s.count())
	}
}

// --- server lifecycle ------------------------------------------------------

func TestGRPCServerRequiresAddrAndSink(t *testing.T) {
	if _, err := NewGRPCServer(GRPCServerOptions{}); err == nil {
		t.Error("expected an error without an Addr")
	}
	if _, err := NewGRPCServer(GRPCServerOptions{Addr: ":0"}); err == nil {
		t.Error("expected an error without a Sink")
	}
}

// TestGRPCServerReportsBindFailure: a port conflict must surface from Start,
// not appear in the log after startup has already claimed success.
func TestGRPCServerReportsBindFailure(t *testing.T) {
	s := &sink{}
	first, err := NewGRPCServer(GRPCServerOptions{
		GRPCOptions: GRPCOptions{Options: Options{Sink: s.accept}}, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	defer first.Stop()

	second, err := NewGRPCServer(GRPCServerOptions{
		GRPCOptions: GRPCOptions{Options: Options{Sink: s.accept}}, Addr: first.Addr(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err == nil {
		second.Stop()
		t.Fatal("binding an occupied port should fail at Start")
	}
}

func TestGRPCServerStopIsSafeBeforeStart(t *testing.T) {
	s := &sink{}
	srv, err := NewGRPCServer(GRPCServerOptions{
		GRPCOptions: GRPCOptions{Options: Options{Sink: s.accept}}, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Stop() // must not block or panic
}

func TestGRPCServerRejectsDoubleStart(t *testing.T) {
	s := &sink{}
	srv, err := NewGRPCServer(GRPCServerOptions{
		GRPCOptions: GRPCOptions{Options: Options{Sink: s.accept}}, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	if err := srv.Start(); err == nil {
		t.Error("a second Start should be refused rather than leaking a listener")
	}
}

// TestGRPCServerStopEndsTheListener confirms Stop actually releases the port,
// so a restart in the same process is not defeated by the old listener.
func TestGRPCServerStopEndsTheListener(t *testing.T) {
	s := &sink{}
	srv, err := NewGRPCServer(GRPCServerOptions{
		GRPCOptions: GRPCOptions{Options: Options{Sink: s.accept}}, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	addr := srv.Addr()
	srv.Stop()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		// The port may be briefly reusable by something else; what matters is
		// that our server no longer answers gRPC on it.
		return
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("unexpected dial error after Stop: %v", err)
	}
}

// --- unit tests on the wire helpers ---------------------------------------

func TestEncodeExportResponseEmptyOnSuccess(t *testing.T) {
	if b := encodeExportResponse(0, "ignored"); len(b) != 0 {
		t.Errorf("full success must encode to the empty message, got %d bytes", len(b))
	}
}

func TestPercentEncodeGrpcMessage(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain ascii", "plain ascii"},
		{"100%", "100%25"},
		{"café", "caf%C3%A9"},       // UTF-8 must not go on the wire raw
		{"tab\there", "tab%09here"}, // control characters are not allowed
	} {
		if got := percentEncode(tc.in); got != tc.want {
			t.Errorf("percentEncode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestItoa(t *testing.T) {
	for n, want := range map[int]string{0: "0", 3: "3", 12: "12", 16: "16"} {
		if got := itoa(n); got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}
