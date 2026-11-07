package otel

import (
	"strings"
	"testing"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func logFidelityUnknown(m proto.Message, label string) {
	var unknown []byte
	for _, value := range []string{label, label + "-again"} {
		unknown = protowire.AppendTag(unknown, 536870911, protowire.BytesType)
		unknown = protowire.AppendString(unknown, value)
	}
	unknown = protowire.AppendTag(unknown, 4242, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 123)
	m.ProtoReflect().SetUnknown(unknown)
}

func logFidelityFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	var out []*logspb.ResourceLogs
	for i, name := range []string{"absent", "present_empty", "resource_only", "scope_only", "populated", "zero_ids"} {
		rec := &logspb.LogRecord{
			TimeUnixNano:           uint64(10 + i),
			ObservedTimeUnixNano:   uint64(100 + i),
			TraceId:                qaBytes(t, qaID(0xf200, 16)),
			SpanId:                 qaBytes(t, qaID(0xf201, 8)),
			Flags:                  0x80000101,
			SeverityNumber:         logspb.SeverityNumber(777),
			SeverityText:           "future severity",
			EventName:              "fidelity." + name,
			Body:                   exArray(exStr("body"), exInt(17)),
			DroppedAttributesCount: 13,
			Attributes: []*commonpb.KeyValue{
				exKV("case", exStr(name)),
				exKV("duplicate", exStr("first")),
				exKV("duplicate", exStr("last")),
				exKV("bool", exBool(true)),
				exKV("oversize", exStr(strings.Repeat("x", maxIndexedValueBytes+1))),
			},
		}
		logFidelityUnknown(rec, "record-"+name)
		logFidelityUnknown(rec.Body, "body-"+name)
		logFidelityUnknown(rec.Attributes[0], "attribute-"+name)
		logFidelityUnknown(rec.Attributes[0].Value, "value-"+name)
		sl := &logspb.ScopeLogs{SchemaUrl: "https://scope/" + name, LogRecords: []*logspb.LogRecord{rec}}
		rl := &logspb.ResourceLogs{SchemaUrl: "https://resource/" + name, ScopeLogs: []*logspb.ScopeLogs{sl}}
		logFidelityUnknown(sl, "scope-logs-"+name)
		logFidelityUnknown(rl, "resource-logs-"+name)
		if name == "present_empty" || name == "resource_only" || name == "populated" {
			rl.Resource = &resourcepb.Resource{}
		}
		if name == "present_empty" || name == "scope_only" || name == "populated" {
			sl.Scope = &commonpb.InstrumentationScope{}
		}
		if name == "populated" {
			rl.Resource.Attributes = []*commonpb.KeyValue{exKV("resource-key", exBool(true))}
			rl.Resource.DroppedAttributesCount = 7
			sl.Scope.Name, sl.Scope.Version = "scope", "version"
			sl.Scope.Attributes = []*commonpb.KeyValue{exKV("scope-key", exInt(5))}
			sl.Scope.DroppedAttributesCount = 11
			logFidelityUnknown(rl.Resource, "resource")
			logFidelityUnknown(sl.Scope, "instrumentation-scope")
		}
		if name == "zero_ids" {
			rec.TraceId, rec.SpanId = make([]byte, 16), make([]byte, 8)
			rec.TimeUnixNano, rec.ObservedTimeUnixNano = 0, 0
			rec.SeverityNumber = logspb.SeverityNumber(-77)
		}
		out = append(out, rl)
	}
	return out
}

func requireLogFidelity(t *testing.T, got LogRecord, want *logspb.ResourceLogs) {
	t.Helper()
	actual, err := got.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(actual, want) {
		t.Fatalf("OTLP envelope changed:\n got %v\nwant %v", actual, want)
	}
}

func TestLogOTLPConstructedAccessorIsDetached(t *testing.T) {
	t.Parallel()
	l := LogRecord{Body: exStr("body"), Attributes: []*commonpb.KeyValue{exKV("key", exStr("value"))}}
	got, err := l.OTLP()
	if err != nil {
		t.Fatal(err)
	}
	got.ScopeLogs[0].LogRecords[0].Body.Value = &commonpb.AnyValue_StringValue{StringValue: "changed"}
	got.ScopeLogs[0].LogRecords[0].Attributes[0].Value = exStr("changed")
	if l.Body.GetStringValue() != "body" || l.Attributes[0].Value.GetStringValue() != "value" {
		t.Fatal("OTLP accessor aliased a constructed record")
	}
	_, err = (LogRecord{TraceID: "not hex"}).OTLP()
	requireTyped[*AdmissionError](t, "OTLP identity encoding", err)
}

func TestLogOTLPFidelitySingleRecordEnvelopes(t *testing.T) {
	t.Parallel()
	in := logFidelityFixture(t)
	// Put two records and two scopes in one wrapper to catch retaining siblings.
	rl := in[4]
	rl.ScopeLogs[0].LogRecords = append(rl.ScopeLogs[0].LogRecords, in[0].ScopeLogs[0].LogRecords[0])
	rl.ScopeLogs = append(rl.ScopeLogs, in[1].ScopeLogs[0])
	logs := LogRecordsFromResourceLogs([]*logspb.ResourceLogs{nil, rl, {ScopeLogs: []*logspb.ScopeLogs{nil}}})
	if len(logs) != 3 {
		t.Fatalf("converted %d logs, want three", len(logs))
	}
	for i, indexes := range [][2]int{{0, 0}, {0, 1}, {1, 0}} {
		want := proto.Clone(rl).(*logspb.ResourceLogs)
		scope := want.ScopeLogs[indexes[0]]
		scope.LogRecords = []*logspb.LogRecord{scope.LogRecords[indexes[1]]}
		want.ScopeLogs = []*logspb.ScopeLogs{scope}
		requireLogFidelity(t, logs[i], want)
	}
}
