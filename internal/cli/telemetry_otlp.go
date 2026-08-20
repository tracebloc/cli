package cli

// The OTLP/HTTP JSON wire shape, converted AT THE SEAM — backend#2217.
//
// The emitter and the spool keep the compact `{resource, attributes}` shape:
// it is what a human reading a spool file wants, and older spooled files stay
// readable when this mapping changes. Only delivery speaks OTLP.
//
// The receiver is `common/telemetry/otlp.py::parse_export_logs_request`
// (backend#2213). Two of its rules are load-bearing here and are asserted by
// the tests rather than trusted:
//
//  1. ONE `resourceLogs` ENTRY PER EVENT, never one for the batch.
//     `service.instance.id` is fresh per run and `service.version` legitimately
//     differs between runs, so a drained spool carries several genuinely
//     different resources. Collapsing them attributes every event to whichever
//     run happened to be first.
//  2. int64 IS A STRING in proto3 JSON. The canonical encoding of 41230 is
//     "41230". The receiver takes both, but sending the canonical form means
//     this payload is also readable by any stock OTLP consumer.
//
// No `timeUnixNano`. Events arrive late by design once spooling is in play, and
// the #2213 decision deliberately sends no client-side timing — so the receiver
// stamps arrival and "how late" is knowingly invisible. Adding an event clock
// here would be a contract change, not an implementation detail.

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
)

// spooledEvent is one emitted occurrence, in the shape the spool stores.
type spooledEvent struct {
	Resource   map[string]string `json:"resource"`
	Attributes map[string]any    `json:"attributes"`
}

type otlpAnyValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
}

type otlpAttr struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

type otlpResource struct {
	Attributes []otlpAttr `json:"attributes"`
}

type otlpLogRecord struct {
	Attributes []otlpAttr `json:"attributes"`
}

type otlpScopeLogs struct {
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpExportLogsServiceRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

// anyValue renders one attribute value as an OTLP AnyValue.
//
// BY KIND, NOT BY CONCRETE TYPE, and that is not a style choice. The emitter's
// own `checkAttrValue` accepts every scalar KIND via reflection precisely so an
// idiomatic caller can pass a named type (`type Reason string`, or a
// time.Duration through telemetry.Duration). A `switch v := value.(type)` here
// would match the DYNAMIC type, miss those, and drop at the seam exactly the
// values the emitter went out of its way to admit — a silent hole one layer
// below the check that permits them.
func anyValue(value any) (otlpAnyValue, bool) {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.String:
		s := rv.String()
		return otlpAnyValue{StringValue: &s}, true
	case reflect.Bool:
		b := rv.Bool()
		return otlpAnyValue{BoolValue: &b}, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s := strconv.FormatInt(rv.Int(), 10)
		return otlpAnyValue{IntValue: &s}, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s := strconv.FormatUint(rv.Uint(), 10)
		return otlpAnyValue{IntValue: &s}, true
	case reflect.Float32, reflect.Float64:
		f := rv.Float()
		return otlpAnyValue{DoubleValue: &f}, true
	default:
		// reflect.Invalid (a nil `any`) lands here too. The emitter already
		// omits absent values, so reaching this is a defect upstream rather
		// than a value to guess at — dropped, and the batch still ships.
		return otlpAnyValue{}, false
	}
}

// attrsFromStrings renders the resource layer. Sorted so a payload is
// byte-stable for a given input, which is what lets the tests assert on it.
func attrsFromStrings(m map[string]string) []otlpAttr {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpAttr, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		out = append(out, otlpAttr{Key: k, Value: otlpAnyValue{StringValue: &v}})
	}
	return out
}

func attrsFromAny(m map[string]any) []otlpAttr {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpAttr, 0, len(keys))
	for _, k := range keys {
		value, ok := anyValue(m[k])
		if !ok {
			continue
		}
		out = append(out, otlpAttr{Key: k, Value: value})
	}
	return out
}

// otlpPayload renders a batch as one ExportLogsServiceRequest.
func otlpPayload(events []spooledEvent) ([]byte, error) {
	doc := otlpExportLogsServiceRequest{
		ResourceLogs: make([]otlpResourceLogs, 0, len(events)),
	}
	for _, ev := range events {
		doc.ResourceLogs = append(doc.ResourceLogs, otlpResourceLogs{
			Resource: otlpResource{Attributes: attrsFromStrings(ev.Resource)},
			ScopeLogs: []otlpScopeLogs{{
				LogRecords: []otlpLogRecord{{
					Attributes: attrsFromAny(ev.Attributes),
				}},
			}},
		})
	}
	return json.Marshal(doc)
}
