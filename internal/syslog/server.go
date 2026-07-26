package syslog

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

const (
	// maxDatagramBytes bounds a single UDP message. RFC5426 suggests 65535 is
	// the practical ceiling for IPv4/IPv6 datagrams.
	maxDatagramBytes = 64 << 10
	// maxLineBytes bounds one TCP-framed message, so a sender that never emits
	// a newline cannot grow the read buffer without limit.
	maxLineBytes = 1 << 20
	// connIdleTimeout drops a TCP sender that has gone quiet, so abandoned
	// connections do not accumulate against maxConns.
	connIdleTimeout = 10 * time.Minute
	// maxConns caps concurrent TCP senders. The listener is unauthenticated, so
	// without a cap anyone reachable on the port could exhaust file descriptors.
	maxConns = 512
)

// Sink accepts a parsed event. It returns false when the event was refused
// (the ingest buffer is full), which the server counts as a drop.
type Sink func(model.LogEvent) bool

// Options configures a Server. An empty address disables that listener.
type Options struct {
	UDPAddr string
	TCPAddr string
	Sink    Sink
	Now     func() time.Time
	Logger  *slog.Logger
}

// Server accepts syslog messages over UDP and/or TCP.
//
// The protocol carries no credentials, so there is nothing to authenticate
// against: exposure is controlled entirely by which address you bind. Both
// listeners are off unless configured, and the documentation is explicit that
// they belong on a trusted network.
type Server struct {
	opts Options

	mu        sync.Mutex
	udpConn   net.PacketConn
	tcpLn     net.Listener
	closed    bool
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	received  atomic.Int64
	dropped   atomic.Int64
	malformed atomic.Int64
}

// New creates a Server. It does not listen until Start is called.
func New(opts Options) (*Server, error) {
	if opts.Sink == nil {
		return nil, errors.New("syslog: Sink is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.UDPAddr == "" && opts.TCPAddr == "" {
		return nil, errors.New("syslog: at least one of UDPAddr or TCPAddr is required")
	}
	return &Server{opts: opts, conns: map[net.Conn]struct{}{}}, nil
}

// Start binds the configured listeners and serves in the background. Binding
// happens synchronously so a port conflict is reported to the caller rather
// than lost in a goroutine.
func (s *Server) Start() error {
	if s.opts.UDPAddr != "" {
		conn, err := net.ListenPacket("udp", s.opts.UDPAddr)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.udpConn = conn
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveUDP(conn)
		s.opts.Logger.Info("syslog listening", "proto", "udp", "addr", s.opts.UDPAddr)
	}
	if s.opts.TCPAddr != "" {
		ln, err := net.Listen("tcp", s.opts.TCPAddr)
		if err != nil {
			s.Stop()
			return err
		}
		s.mu.Lock()
		s.tcpLn = ln
		s.mu.Unlock()
		s.wg.Add(1)
		go s.serveTCP(ln)
		s.opts.Logger.Info("syslog listening", "proto", "tcp", "addr", s.opts.TCPAddr)
	}
	return nil
}

// Addrs reports the bound addresses, which is how a caller discovers the real
// port when binding to :0.
func (s *Server) Addrs() (udp, tcp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.udpConn != nil {
		udp = s.udpConn.LocalAddr().String()
	}
	if s.tcpLn != nil {
		tcp = s.tcpLn.Addr().String()
	}
	return udp, tcp
}

// Stop closes the listeners and waits for in-flight handlers to finish.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	if s.udpConn != nil {
		s.udpConn.Close()
	}
	if s.tcpLn != nil {
		s.tcpLn.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Metrics is a snapshot of collector activity.
type Metrics struct {
	Received  int64 `json:"received"`
	Dropped   int64 `json:"dropped"`
	Malformed int64 `json:"malformed"`
}

// Metrics returns a snapshot of collector counters.
func (s *Server) Metrics() Metrics {
	return Metrics{
		Received:  s.received.Load(),
		Dropped:   s.dropped.Load(),
		Malformed: s.malformed.Load(),
	}
}

func (s *Server) serveUDP(conn net.PacketConn) {
	defer s.wg.Done()
	buf := make([]byte, maxDatagramBytes)
	for {
		n, _, err := conn.ReadFrom(buf)
		if n > 0 {
			s.handle(string(buf[:n]))
		}
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.opts.Logger.Warn("syslog: udp read failed", "error", err)
			return
		}
	}
}

func (s *Server) serveTCP(ln net.Listener) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if s.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			s.opts.Logger.Warn("syslog: accept failed", "error", err)
			return
		}
		if !s.trackConn(conn) {
			// At the connection cap: refuse rather than run out of descriptors.
			s.opts.Logger.Warn("syslog: connection limit reached, refusing sender",
				"remote", conn.RemoteAddr().String(), "limit", maxConns)
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrackConn(conn)
			s.serveConn(conn)
		}()
	}
}

func (s *Server) trackConn(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.conns) >= maxConns {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrackConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	c.Close()
}

// serveConn reads messages from one TCP sender. Both RFC6587 framings are
// accepted: octet-counted ("123 <PRI>...") and newline-delimited, chosen per
// message by looking at whether it starts with a digit.
func (s *Server) serveConn(conn net.Conn) {
	r := bufio.NewReaderSize(conn, 64<<10)
	for {
		// Deliberately time.Now, not opts.Now. A socket deadline is an absolute
		// wall-clock instant the runtime compares against the real clock, whereas
		// opts.Now is the injectable *event* clock used for parsing timestamps.
		// Feeding the event clock in here makes every read time out immediately
		// whenever the two differ.
		if err := conn.SetReadDeadline(time.Now().Add(connIdleTimeout)); err != nil {
			return
		}
		msg, err := readFrame(r)
		if msg != "" {
			s.handle(msg)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !s.isClosed() && !errors.Is(err, net.ErrClosed) {
				s.opts.Logger.Debug("syslog: connection ended", "error", err)
			}
			return
		}
	}
}

// readFrame reads one message using whichever RFC6587 framing the sender used.
func readFrame(r *bufio.Reader) (string, error) {
	first, err := r.Peek(1)
	if err != nil {
		return "", err
	}
	if first[0] >= '0' && first[0] <= '9' {
		return readOctetCounted(r)
	}
	line, err := readLineLimited(r)
	return strings.TrimRight(line, "\r\n"), err
}

// readOctetCounted reads the "<length> <message>" framing.
func readOctetCounted(r *bufio.Reader) (string, error) {
	var digits []byte
	for len(digits) < 10 {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == ' ' {
			break
		}
		if b < '0' || b > '9' {
			// Not actually octet-counted; treat what we have as a line.
			_ = r.UnreadByte()
			line, lerr := readLineLimited(r)
			return string(digits) + strings.TrimRight(line, "\r\n"), lerr
		}
		digits = append(digits, b)
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil || n < 0 || n > maxLineBytes {
		return "", errors.New("syslog: implausible frame length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// readLineLimited reads one newline-terminated message, refusing to buffer more
// than maxLineBytes so a sender that never sends a newline cannot exhaust memory.
func readLineLimited(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		chunk, err := r.ReadString('\n')
		b.WriteString(chunk)
		if err != nil {
			return b.String(), err
		}
		if strings.HasSuffix(chunk, "\n") {
			return b.String(), nil
		}
		if b.Len() > maxLineBytes {
			return b.String(), errors.New("syslog: message exceeds the size limit")
		}
	}
}

// handle parses one message and offers it to the sink.
func (s *Server) handle(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	e := Parse(raw, s.opts.Now())
	if e.Message == "" && e.Raw != "" {
		s.malformed.Add(1)
	}
	s.received.Add(1)
	if !s.opts.Sink(e) {
		s.dropped.Add(1)
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}
