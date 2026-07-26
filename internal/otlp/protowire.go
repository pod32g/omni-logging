// Package otlp receives OpenTelemetry logs over OTLP/HTTP, in both the
// protobuf and JSON encodings, and converts them to the canonical LogEvent.
//
// The protobuf encoding is decoded by hand rather than by generating code from
// the OTLP .proto files. That keeps the binary's dependency tree as it is —
// one SQLite driver and a YAML parser — and the subset of the wire format OTLP
// logs actually use is small and stable. Protobuf is not optional here: it is
// what every OTLP exporter sends by default, so a JSON-only receiver would be
// half a feature.
package otlp

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Protobuf wire types.
const (
	wireVarint = 0
	wireI64    = 1
	wireBytes  = 2
	wireI32    = 5
)

// maxNestingDepth stops a crafted payload from recursing until the stack gives
// out. OTLP logs nest at most a handful of levels.
const maxNestingDepth = 16

// reader walks a protobuf message.
type reader struct {
	buf []byte
	pos int
}

func newReader(b []byte) *reader { return &reader{buf: b} }

func (r *reader) done() bool { return r.pos >= len(r.buf) }

// tag reads the next field number and wire type.
func (r *reader) tag() (field int, wire int, err error) {
	v, err := r.varint()
	if err != nil {
		return 0, 0, err
	}
	field = int(v >> 3)
	wire = int(v & 0x7)
	if field <= 0 {
		return 0, 0, fmt.Errorf("otlp: invalid field number %d", field)
	}
	return field, wire, nil
}

func (r *reader) varint() (uint64, error) {
	var (
		v     uint64
		shift uint
	)
	for {
		if r.pos >= len(r.buf) {
			return 0, fmt.Errorf("otlp: truncated varint")
		}
		b := r.buf[r.pos]
		r.pos++
		v |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return v, nil
		}
		shift += 7
		if shift > 63 {
			return 0, fmt.Errorf("otlp: varint overflows 64 bits")
		}
	}
}

func (r *reader) bytes() ([]byte, error) {
	n, err := r.varint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(r.buf)-r.pos) {
		return nil, fmt.Errorf("otlp: length-delimited field runs past the end of the message")
	}
	start := r.pos
	r.pos += int(n)
	return r.buf[start:r.pos], nil
}

func (r *reader) fixed64() (uint64, error) {
	if r.pos+8 > len(r.buf) {
		return 0, fmt.Errorf("otlp: truncated 64-bit field")
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *reader) fixed32() (uint32, error) {
	if r.pos+4 > len(r.buf) {
		return 0, fmt.Errorf("otlp: truncated 32-bit field")
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

// skip advances past a field whose contents we do not care about. Being able to
// skip unknown fields is what makes this decoder forward-compatible with newer
// OTLP versions rather than brittle against them.
func (r *reader) skip(wire int) error {
	switch wire {
	case wireVarint:
		_, err := r.varint()
		return err
	case wireI64:
		_, err := r.fixed64()
		return err
	case wireBytes:
		_, err := r.bytes()
		return err
	case wireI32:
		_, err := r.fixed32()
		return err
	}
	return fmt.Errorf("otlp: unsupported wire type %d", wire)
}

func float64FromBits(v uint64) float64 { return math.Float64frombits(v) }
