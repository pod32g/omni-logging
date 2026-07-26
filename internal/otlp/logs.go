package otlp

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// Field numbers from the OTLP logs proto (opentelemetry/proto/logs/v1/logs.proto
// and common/v1/common.proto). Named rather than inlined so a future field
// addition is a one-line change against a readable reference.
const (
	// LogsData
	fLogsDataResourceLogs = 1
	// ResourceLogs
	fResourceLogsResource  = 1
	fResourceLogsScopeLogs = 2
	// Resource
	fResourceAttributes = 1
	// ScopeLogs
	fScopeLogsScope      = 1
	fScopeLogsLogRecords = 2
	// InstrumentationScope
	fScopeName = 1
	// LogRecord
	fLogTimeUnixNano         = 1
	fLogSeverityNumber       = 2
	fLogSeverityText         = 3
	fLogBody                 = 5
	fLogAttributes           = 6
	fLogTraceID              = 9
	fLogSpanID               = 10
	fLogObservedTimeUnixNano = 11
	// KeyValue
	fKeyValueKey   = 1
	fKeyValueValue = 2
	// AnyValue
	fAnyString = 1
	fAnyBool   = 2
	fAnyInt    = 3
	fAnyDouble = 4
	fAnyArray  = 5
	fAnyKvlist = 6
	fAnyBytes  = 7
	// ArrayValue / KeyValueList
	fArrayValues  = 1
	fKvlistValues = 1
)

// resourceServiceKeys are the resource attributes that identify the emitting
// service, in the order the OTel semantic conventions prefer.
var resourceServiceKeys = []string{"service.name", "service_name", "k8s.deployment.name", "k8s.container.name"}

// resourceHostKeys identify the emitting host.
var resourceHostKeys = []string{"host.name", "host", "k8s.pod.name", "k8s.node.name"}

// DecodeProtobuf converts an OTLP ExportLogsServiceRequest (or a bare LogsData,
// which has the same shape) into events.
func DecodeProtobuf(b []byte, now time.Time) ([]model.LogEvent, error) {
	var out []model.LogEvent
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field != fLogsDataResourceLogs || wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		body, err := r.bytes()
		if err != nil {
			return nil, err
		}
		events, err := decodeResourceLogs(body, now, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

func decodeResourceLogs(b []byte, now time.Time, depth int) ([]model.LogEvent, error) {
	if depth > maxNestingDepth {
		return nil, fmt.Errorf("otlp: message nests too deeply")
	}
	var (
		resourceAttrs = map[string]any{}
		scopes        [][]byte
	)
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		payload, err := r.bytes()
		if err != nil {
			return nil, err
		}
		switch field {
		case fResourceLogsResource:
			if err := decodeResource(payload, resourceAttrs, depth+1); err != nil {
				return nil, err
			}
		case fResourceLogsScopeLogs:
			scopes = append(scopes, payload)
		}
	}

	var out []model.LogEvent
	for _, scope := range scopes {
		events, err := decodeScopeLogs(scope, resourceAttrs, now, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, events...)
	}
	return out, nil
}

func decodeResource(b []byte, attrs map[string]any, depth int) error {
	if depth > maxNestingDepth {
		return fmt.Errorf("otlp: message nests too deeply")
	}
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return err
		}
		if field != fResourceAttributes || wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return err
			}
			continue
		}
		kv, err := r.bytes()
		if err != nil {
			return err
		}
		k, v, err := decodeKeyValue(kv, depth+1)
		if err != nil {
			return err
		}
		if k != "" {
			attrs[k] = v
		}
	}
	return nil
}

func decodeScopeLogs(b []byte, resourceAttrs map[string]any, now time.Time, depth int) ([]model.LogEvent, error) {
	if depth > maxNestingDepth {
		return nil, fmt.Errorf("otlp: message nests too deeply")
	}
	var (
		scopeName string
		records   [][]byte
	)
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		payload, err := r.bytes()
		if err != nil {
			return nil, err
		}
		switch field {
		case fScopeLogsScope:
			scopeName = decodeScopeName(payload)
		case fScopeLogsLogRecords:
			records = append(records, payload)
		}
	}

	out := make([]model.LogEvent, 0, len(records))
	for _, rec := range records {
		e, err := decodeLogRecord(rec, resourceAttrs, scopeName, now, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func decodeScopeName(b []byte) string {
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return ""
		}
		if field == fScopeName && wire == wireBytes {
			v, err := r.bytes()
			if err != nil {
				return ""
			}
			return string(v)
		}
		if err := r.skip(wire); err != nil {
			return ""
		}
	}
	return ""
}

func decodeLogRecord(b []byte, resourceAttrs map[string]any, scopeName string, now time.Time, depth int) (model.LogEvent, error) {
	e := model.LogEvent{Attributes: map[string]any{}}
	var (
		timeNano     uint64
		observedNano uint64
		sevNumber    uint64
		sevText      string
	)

	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return e, err
		}
		switch {
		case field == fLogTimeUnixNano && wire == wireI64:
			if timeNano, err = r.fixed64(); err != nil {
				return e, err
			}
		case field == fLogObservedTimeUnixNano && wire == wireI64:
			if observedNano, err = r.fixed64(); err != nil {
				return e, err
			}
		case field == fLogSeverityNumber && wire == wireVarint:
			if sevNumber, err = r.varint(); err != nil {
				return e, err
			}
		case field == fLogSeverityText && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return e, err
			}
			sevText = string(v)
		case field == fLogBody && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return e, err
			}
			body, err := decodeAnyValue(v, depth+1)
			if err != nil {
				return e, err
			}
			e.Message = valueToString(body)
		case field == fLogAttributes && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return e, err
			}
			k, val, err := decodeKeyValue(v, depth+1)
			if err != nil {
				return e, err
			}
			if k != "" {
				e.Attributes[k] = val
			}
		case field == fLogTraceID && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return e, err
			}
			if len(v) > 0 {
				e.Attributes["trace_id"] = hex.EncodeToString(v)
			}
		case field == fLogSpanID && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return e, err
			}
			if len(v) > 0 {
				e.Attributes["span_id"] = hex.EncodeToString(v)
			}
		default:
			if err := r.skip(wire); err != nil {
				return e, err
			}
		}
	}

	finishEvent(&e, resourceAttrs, scopeName, timeNano, observedNano, sevNumber, sevText, now)
	return e, nil
}

// finishEvent applies the mapping shared by both encodings: resource
// attributes become service/source, severity becomes the level, and the OTLP
// timestamps become the event and receipt times.
func finishEvent(e *model.LogEvent, resourceAttrs map[string]any, scopeName string,
	timeNano, observedNano, sevNumber uint64, sevText string, now time.Time) {

	for k, v := range resourceAttrs {
		if _, taken := e.Attributes[k]; !taken {
			e.Attributes[k] = v
		}
	}
	e.Service = firstAttr(resourceAttrs, resourceServiceKeys)
	e.Source = firstAttr(resourceAttrs, resourceHostKeys)
	if e.Service == "" && scopeName != "" {
		// The instrumentation scope is a weaker signal than service.name, but a
		// better label than nothing.
		e.Service = scopeName
	}

	e.Level = severityToLevel(sevNumber, sevText)

	if timeNano > 0 {
		e.Timestamp = time.Unix(0, int64(timeNano)).UTC()
	} else if observedNano > 0 {
		e.Timestamp = time.Unix(0, int64(observedNano)).UTC()
	}
	if observedNano > 0 {
		e.ReceivedAt = time.Unix(0, int64(observedNano)).UTC()
	}
	if len(e.Attributes) == 0 {
		e.Attributes = nil
	}
	e.Normalize(now)
}

func firstAttr(attrs map[string]any, keys []string) string {
	for _, k := range keys {
		if v, ok := attrs[k]; ok {
			if s := valueToString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// severityToLevel maps an OTLP severity number onto our level set. The number
// is authoritative when present; the text is the fallback, because exporters
// that omit the number usually still send something human-readable.
func severityToLevel(number uint64, text string) model.Level {
	switch {
	case number == 0:
		if text != "" {
			return model.ParseLevel(text)
		}
		return model.LevelInfo
	case number <= 4: // TRACE
		return model.LevelDebug
	case number <= 8: // DEBUG
		return model.LevelDebug
	case number <= 12: // INFO
		return model.LevelInfo
	case number <= 16: // WARN
		return model.LevelWarn
	case number <= 20: // ERROR
		return model.LevelError
	default: // FATAL
		return model.LevelFatal
	}
}

func decodeKeyValue(b []byte, depth int) (string, any, error) {
	if depth > maxNestingDepth {
		return "", nil, fmt.Errorf("otlp: message nests too deeply")
	}
	var (
		key string
		val any
	)
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return "", nil, err
		}
		if wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return "", nil, err
			}
			continue
		}
		payload, err := r.bytes()
		if err != nil {
			return "", nil, err
		}
		switch field {
		case fKeyValueKey:
			key = string(payload)
		case fKeyValueValue:
			if val, err = decodeAnyValue(payload, depth+1); err != nil {
				return "", nil, err
			}
		}
	}
	return key, val, nil
}

func decodeAnyValue(b []byte, depth int) (any, error) {
	if depth > maxNestingDepth {
		return nil, fmt.Errorf("otlp: value nests too deeply")
	}
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		switch {
		case field == fAnyString && wire == wireBytes:
			v, err := r.bytes()
			return string(v), err
		case field == fAnyBool && wire == wireVarint:
			v, err := r.varint()
			return v != 0, err
		case field == fAnyInt && wire == wireVarint:
			v, err := r.varint()
			return int64(v), err
		case field == fAnyDouble && wire == wireI64:
			v, err := r.fixed64()
			return float64FromBits(v), err
		case field == fAnyBytes && wire == wireBytes:
			v, err := r.bytes()
			return hex.EncodeToString(v), err
		case field == fAnyArray && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return nil, err
			}
			return decodeArrayValue(v, depth+1)
		case field == fAnyKvlist && wire == wireBytes:
			v, err := r.bytes()
			if err != nil {
				return nil, err
			}
			return decodeKvlist(v, depth+1)
		default:
			if err := r.skip(wire); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}

func decodeArrayValue(b []byte, depth int) (any, error) {
	var out []any
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field != fArrayValues || wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		payload, err := r.bytes()
		if err != nil {
			return nil, err
		}
		v, err := decodeAnyValue(payload, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func decodeKvlist(b []byte, depth int) (any, error) {
	out := map[string]any{}
	r := newReader(b)
	for !r.done() {
		field, wire, err := r.tag()
		if err != nil {
			return nil, err
		}
		if field != fKvlistValues || wire != wireBytes {
			if err := r.skip(wire); err != nil {
				return nil, err
			}
			continue
		}
		payload, err := r.bytes()
		if err != nil {
			return nil, err
		}
		k, v, err := decodeKeyValue(payload, depth+1)
		if err != nil {
			return nil, err
		}
		if k != "" {
			out[k] = v
		}
	}
	return out, nil
}

// valueToString renders an AnyValue for a field that must be text.
func valueToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", t)
	}
}
