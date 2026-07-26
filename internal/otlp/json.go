package otlp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
)

// The OTLP/JSON encoding mirrors the protobuf message names in lowerCamelCase.
// Numeric 64-bit fields are strings on the wire (JSON cannot carry them
// losslessly), which is why the timestamp fields are decoded from json.Number
// and from quoted strings alike.

type jsonLogsData struct {
	ResourceLogs []jsonResourceLogs `json:"resourceLogs"`
	// Some SDKs emit snake_case; accept both rather than silently dropping data.
	ResourceLogsSnake []jsonResourceLogs `json:"resource_logs"`
}

type jsonResourceLogs struct {
	Resource       jsonResource    `json:"resource"`
	ScopeLogs      []jsonScopeLogs `json:"scopeLogs"`
	ScopeLogsSnake []jsonScopeLogs `json:"scope_logs"`
}

type jsonResource struct {
	Attributes []jsonKeyValue `json:"attributes"`
}

type jsonScopeLogs struct {
	Scope           jsonScope       `json:"scope"`
	LogRecords      []jsonLogRecord `json:"logRecords"`
	LogRecordsSnake []jsonLogRecord `json:"log_records"`
}

type jsonScope struct {
	Name string `json:"name"`
}

type jsonLogRecord struct {
	TimeUnixNano         any            `json:"timeUnixNano"`
	TimeUnixNanoSnake    any            `json:"time_unix_nano"`
	ObservedTimeUnixNano any            `json:"observedTimeUnixNano"`
	ObservedSnake        any            `json:"observed_time_unix_nano"`
	SeverityNumber       any            `json:"severityNumber"`
	SeverityNumberSnake  any            `json:"severity_number"`
	SeverityText         string         `json:"severityText"`
	SeverityTextSnake    string         `json:"severity_text"`
	Body                 jsonAnyValue   `json:"body"`
	Attributes           []jsonKeyValue `json:"attributes"`
	TraceID              string         `json:"traceId"`
	TraceIDSnake         string         `json:"trace_id"`
	SpanID               string         `json:"spanId"`
	SpanIDSnake          string         `json:"span_id"`
}

type jsonKeyValue struct {
	Key   string       `json:"key"`
	Value jsonAnyValue `json:"value"`
}

type jsonAnyValue struct {
	StringValue *string     `json:"stringValue"`
	BoolValue   *bool       `json:"boolValue"`
	IntValue    any         `json:"intValue"`
	DoubleValue *float64    `json:"doubleValue"`
	BytesValue  *string     `json:"bytesValue"`
	ArrayValue  *jsonArray  `json:"arrayValue"`
	KvlistValue *jsonKvlist `json:"kvlistValue"`
}

type jsonArray struct {
	Values []jsonAnyValue `json:"values"`
}

type jsonKvlist struct {
	Values []jsonKeyValue `json:"values"`
}

func (v jsonAnyValue) decode() any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.IntValue != nil:
		return anyToInt(v.IntValue)
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BytesValue != nil:
		return *v.BytesValue
	case v.ArrayValue != nil:
		out := make([]any, 0, len(v.ArrayValue.Values))
		for _, item := range v.ArrayValue.Values {
			out = append(out, item.decode())
		}
		return out
	case v.KvlistValue != nil:
		out := map[string]any{}
		for _, kv := range v.KvlistValue.Values {
			if kv.Key != "" {
				out[kv.Key] = kv.Value.decode()
			}
		}
		return out
	}
	return nil
}

// anyToUint64 accepts both the quoted-string and bare-number forms JSON
// producers use for 64-bit fields.
func anyToUint64(v any) uint64 {
	switch t := v.(type) {
	case string:
		n, _ := strconv.ParseUint(t, 10, 64)
		return n
	case float64:
		if t < 0 {
			return 0
		}
		return uint64(t)
	case json.Number:
		n, _ := strconv.ParseUint(t.String(), 10, 64)
		return n
	}
	return 0
}

func anyToInt(v any) int64 {
	switch t := v.(type) {
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case float64:
		return int64(t)
	case json.Number:
		n, _ := strconv.ParseInt(t.String(), 10, 64)
		return n
	}
	return 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func firstNonNil(a, b any) any {
	if a != nil {
		return a
	}
	return b
}

func pickSlice[T any](a, b []T) []T {
	if len(a) > 0 {
		return a
	}
	return b
}

// DecodeJSON converts an OTLP/JSON ExportLogsServiceRequest into events.
func DecodeJSON(b []byte, now time.Time) ([]model.LogEvent, error) {
	var data jsonLogsData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("otlp: invalid JSON: %w", err)
	}

	var out []model.LogEvent
	for _, rl := range pickSlice(data.ResourceLogs, data.ResourceLogsSnake) {
		resourceAttrs := map[string]any{}
		for _, kv := range rl.Resource.Attributes {
			if kv.Key != "" {
				resourceAttrs[kv.Key] = kv.Value.decode()
			}
		}
		for _, sl := range pickSlice(rl.ScopeLogs, rl.ScopeLogsSnake) {
			for _, rec := range pickSlice(sl.LogRecords, sl.LogRecordsSnake) {
				out = append(out, jsonRecordToEvent(rec, resourceAttrs, sl.Scope.Name, now))
			}
		}
	}
	return out, nil
}

func jsonRecordToEvent(rec jsonLogRecord, resourceAttrs map[string]any, scopeName string, now time.Time) model.LogEvent {
	e := model.LogEvent{Attributes: map[string]any{}}
	e.Message = valueToString(rec.Body.decode())

	for _, kv := range rec.Attributes {
		if kv.Key != "" {
			e.Attributes[kv.Key] = kv.Value.decode()
		}
	}
	if id := firstNonEmpty(rec.TraceID, rec.TraceIDSnake); id != "" {
		e.Attributes["trace_id"] = id
	}
	if id := firstNonEmpty(rec.SpanID, rec.SpanIDSnake); id != "" {
		e.Attributes["span_id"] = id
	}

	finishEvent(&e, resourceAttrs, scopeName,
		anyToUint64(firstNonNil(rec.TimeUnixNano, rec.TimeUnixNanoSnake)),
		anyToUint64(firstNonNil(rec.ObservedTimeUnixNano, rec.ObservedSnake)),
		anyToUint64(firstNonNil(rec.SeverityNumber, rec.SeverityNumberSnake)),
		firstNonEmpty(rec.SeverityText, rec.SeverityTextSnake),
		now)
	return e
}
