package plugin

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var update = flag.Bool("update", false, "rewrite golden files")

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func anyKV(k string, v *commonpb.AnyValue) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: v}
}

func idBytes(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

const (
	goldenTraceID = "0102030405060708090a0b0c0d0e0f10"
	rootSpanID    = "1111111111111111"
	childSpanID   = "2222222222222222"
	otherSpanID   = "0333333333333333"
)

// fullTrace sets every OTLP trace field (fieldCensus checks that) across
// two single-span envelopes as the reader returns them, plus a third span
// from a resource without attributes.
func fullTrace() []*tracepb.ResourceSpans {
	allValues := []*commonpb.KeyValue{
		str("http.method", "GET"),
		anyKV("ok", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}),
		anyKV("count", &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -42}}),
		anyKV("ratio", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.25}}),
		anyKV("nan", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}}),
		anyKV("inf", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(1)}}),
		anyKV("neginf", &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(-1)}}),
		anyKV("blob", &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0xde, 0xad, 0xbe, 0xef}}}),
		anyKV("list", &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
			{Value: &commonpb.AnyValue_StringValue{StringValue: "a"}},
			{Value: &commonpb.AnyValue_IntValue{IntValue: 2}},
			{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1}}},
		}}}}),
		anyKV("map", &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
			str("inner", "x"),
			anyKV("deep", &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}),
		}}}}),
		anyKV("empty", &commonpb.AnyValue{}),
	}
	resource := &resourcepb.Resource{
		Attributes: []*commonpb.KeyValue{
			str("service.name", "checkout"),
			str("service.namespace.name", "shop-alt"),
			str("host.name", "h1"),
		},
		DroppedAttributesCount: 3,
		EntityRefs: []*commonpb.EntityRef{{
			SchemaUrl: "https://opentelemetry.io/schemas/1.30.0", Type: "service",
			IdKeys: []string{"service.name"}, DescriptionKeys: []string{"host.name"},
		}},
	}
	scope := &commonpb.InstrumentationScope{
		Name: "io.otel.http", Version: "1.2.3",
		Attributes:             []*commonpb.KeyValue{str("scope.attr", "s")},
		DroppedAttributesCount: 4,
	}
	child := &tracepb.Span{
		TraceId: idBytes(goldenTraceID), SpanId: idBytes(childSpanID), ParentSpanId: idBytes(rootSpanID),
		TraceState: "vendor=a", Flags: 0x301, Name: "GET /cart", Kind: tracepb.Span_SPAN_KIND_CLIENT,
		StartTimeUnixNano: 1_700_000_000_500_000_000, EndTimeUnixNano: 1_700_000_000_750_250_000,
		Attributes: allValues, DroppedAttributesCount: 5,
		Events: []*tracepb.Span_Event{{
			TimeUnixNano: 1_700_000_000_600_000_000, Name: "exception",
			Attributes: []*commonpb.KeyValue{str("exception.type", "Boom")}, DroppedAttributesCount: 6,
		}},
		DroppedEventsCount: 7,
		Links: []*tracepb.Span_Link{{
			TraceId: idBytes("ffeeddccbbaa99887766554433221100"), SpanId: idBytes("aaaaaaaaaaaaaaaa"),
			TraceState: "vendor=b", Attributes: []*commonpb.KeyValue{str("link.kind", "follows")},
			DroppedAttributesCount: 8, Flags: 0x101,
		}},
		DroppedLinksCount: 9,
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "boom"},
	}
	root := &tracepb.Span{
		TraceId: idBytes(goldenTraceID), SpanId: idBytes(rootSpanID),
		Name: "checkout", Kind: tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: 1_700_000_000_000_000_000, EndTimeUnixNano: 1_700_000_001_000_000_000,
		Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
	}
	// Same start as child: the span ID breaks the tie (0333... sorts first).
	other := &tracepb.Span{
		TraceId: idBytes(goldenTraceID), SpanId: idBytes(otherSpanID), ParentSpanId: idBytes(rootSpanID),
		Name: "background", Kind: tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: 1_700_000_000_500_000_000, EndTimeUnixNano: 1_700_000_000_500_000_000,
	}
	envelope := func(r *resourcepb.Resource, s *tracepb.Span) *tracepb.ResourceSpans {
		return &tracepb.ResourceSpans{
			Resource: r, SchemaUrl: "https://opentelemetry.io/schemas/1.26.0",
			ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope, Spans: []*tracepb.Span{s}, SchemaUrl: "https://opentelemetry.io/schemas/1.27.0"}},
		}
	}
	// Span-ID order, as the reader returns them; the frame sorts by start.
	return []*tracepb.ResourceSpans{
		envelope(resource, root),
		envelope(resource, child),
		{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{other}}}},
	}
}

func frameJSON(t *testing.T, f *data.Frame) []byte {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		t.Fatal(err)
	}
	return append(out.Bytes(), '\n')
}

func TestTraceFrameGolden(t *testing.T) {
	t.Parallel()
	frame, err := traceFrame(fullTrace())
	if err != nil {
		t.Fatal(err)
	}
	got := frameJSON(t, frame)
	path := filepath.Join("testdata", "trace_frame.golden.json")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("trace frame differs from %s (run with -update to rewrite):\n%s", path, got)
	}
}

// Tempo's field set, in Tempo's order and types (trace_transform.go).
func TestTraceFrameFieldSet(t *testing.T) {
	t.Parallel()
	frame, err := traceFrame(fullTrace())
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name string
		typ  data.FieldType
	}{
		{"traceID", data.FieldTypeString},
		{"spanID", data.FieldTypeString},
		{"parentSpanID", data.FieldTypeString},
		{"operationName", data.FieldTypeString},
		{"serviceName", data.FieldTypeString},
		{"serviceNamespace", data.FieldTypeString},
		{"kind", data.FieldTypeString},
		{"statusCode", data.FieldTypeInt64},
		{"statusMessage", data.FieldTypeString},
		{"instrumentationLibraryName", data.FieldTypeString},
		{"instrumentationLibraryVersion", data.FieldTypeString},
		{"traceState", data.FieldTypeString},
		{"serviceTags", data.FieldTypeJSON},
		{"startTime", data.FieldTypeFloat64},
		{"duration", data.FieldTypeFloat64},
		{"logs", data.FieldTypeJSON},
		{"references", data.FieldTypeJSON},
		{"tags", data.FieldTypeJSON},
	}
	if len(frame.Fields) != len(want) {
		t.Fatalf("%d fields, want %d", len(frame.Fields), len(want))
	}
	for i, w := range want {
		if f := frame.Fields[i]; f.Name != w.name || f.Type() != w.typ {
			t.Errorf("field %d: %s %s, want %s %s", i, f.Name, f.Type(), w.name, w.typ)
		}
	}
	if frame.Name != "Trace" || frame.Meta == nil || frame.Meta.PreferredVisualization != data.VisTypeTrace {
		t.Fatalf("frame meta: %q %+v", frame.Name, frame.Meta)
	}
	spanIDs := frame.Fields[1]
	if got := []string{spanIDs.At(0).(string), spanIDs.At(1).(string), spanIDs.At(2).(string)}; !slices.Equal(got, []string{rootSpanID, otherSpanID, childSpanID}) {
		t.Fatalf("row order %v: want start time, then span ID", got)
	}
	child := 2
	checks := map[string]any{
		"parentSpanID": rootSpanID, "serviceName": "checkout", "serviceNamespace": "shop-alt", "kind": "client",
		"statusCode": int64(2), "statusMessage": "boom", "instrumentationLibraryName": "io.otel.http",
		"instrumentationLibraryVersion": "1.2.3", "traceState": "vendor=a", "startTime": 1_700_000_000_500.0, "duration": 250.25,
	}
	for name, v := range checks {
		f, _ := frame.FieldByName(name)
		if got := f.At(child); got != v {
			t.Errorf("%s = %v, want %v", name, got, v)
		}
	}
	if f, _ := frame.FieldByName("serviceName"); f.At(1) != resourceNoServiceName {
		t.Errorf("attribute-less resource: serviceName %v", f.At(1))
	}
}

// fieldCensus lists every field of the OTLP trace messages and which ones
// fullTrace leaves unset; only the profiles dictionary indexes may be unset.
func TestTraceFixtureCoversEveryOTLPField(t *testing.T) {
	t.Parallel()
	set := map[protoreflect.FullName]bool{}
	for _, rs := range fullTrace() {
		walkSet(rs.ProtoReflect(), set)
	}
	all := map[protoreflect.FullName]bool{}
	allFields((&tracepb.ResourceSpans{}).ProtoReflect().Descriptor(), all)
	if len(all) < 40 {
		t.Fatalf("census found only %d fields; the walk is broken", len(all))
	}
	// Dictionary indexes of the profiles signal; trace data never sets them.
	notInTraces := map[protoreflect.FullName]bool{
		"opentelemetry.proto.common.v1.AnyValue.string_value_strindex": true,
		"opentelemetry.proto.common.v1.KeyValue.key_strindex":          true,
	}
	for name := range all {
		if !set[name] && !notInTraces[name] {
			t.Errorf("fullTrace does not set %s", name)
		}
	}
	for name := range notInTraces {
		if !all[name] {
			t.Errorf("%s is no longer an OTLP field; drop it from notInTraces", name)
		}
	}
}

func walkSet(m protoreflect.Message, set map[protoreflect.FullName]bool) {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		set[fd.FullName()] = true
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.IsList() {
			l := m.Get(fd).List()
			for j := 0; j < l.Len(); j++ {
				walkSet(l.Get(j).Message(), set)
			}
		} else {
			walkSet(m.Get(fd).Message(), set)
		}
	}
}

func allFields(md protoreflect.MessageDescriptor, out map[protoreflect.FullName]bool) {
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if out[fd.FullName()] {
			continue
		}
		out[fd.FullName()] = true
		if fd.Kind() == protoreflect.MessageKind {
			allFields(fd.Message(), out)
		}
	}
}

// Every populated field occurrence of the fixture reaches the frame: clearing
// any one of them changes the frame. Nothing in an OTLP span vanishes.
func TestTraceFrameLosesNoField(t *testing.T) {
	t.Parallel()
	base, err := traceFrame(fullTrace())
	if err != nil {
		t.Fatal(err)
	}
	baseJSON := frameJSON(t, base)
	n := countOccurrences(fullTrace())
	if n < 100 {
		t.Fatalf("only %d field occurrences; the walk is broken", n)
	}
	skipped := 0
	for i := 0; i < n; i++ {
		rss := fullTrace()
		name, empty := clearOccurrence(rss, i)
		if empty {
			// An empty message reads as an absent one (the "empty" AnyValue).
			skipped++
			continue
		}
		f, err := traceFrame(rss)
		if err != nil {
			t.Fatalf("clearing %s: %v", name, err)
		}
		if bytes.Equal(frameJSON(t, f), baseJSON) {
			t.Errorf("clearing occurrence %d (%s) does not change the frame", i, name)
		}
	}
	if skipped != 1 {
		t.Fatalf("%d empty messages skipped, want the one empty AnyValue", skipped)
	}
}

func countOccurrences(rss []*tracepb.ResourceSpans) int {
	n := 0
	for _, rs := range rss {
		visit(rs.ProtoReflect(), func(protoreflect.Message, protoreflect.FieldDescriptor) bool { n++; return false })
	}
	return n
}

// clearOccurrence clears the i-th populated field occurrence and names it;
// empty reports a singular message field that held an empty message.
func clearOccurrence(rss []*tracepb.ResourceSpans, i int) (name protoreflect.FullName, empty bool) {
	k := 0
	for _, rs := range rss {
		stop := visit(rs.ProtoReflect(), func(m protoreflect.Message, fd protoreflect.FieldDescriptor) bool {
			if k == i {
				if fd.Kind() == protoreflect.MessageKind && !fd.IsList() {
					empty = countOccurrencesIn(m.Get(fd).Message()) == 0
				}
				m.Clear(fd)
				name = fd.FullName()
				return true
			}
			k++
			return false
		})
		if stop {
			break
		}
	}
	return name, empty
}

func countOccurrencesIn(m protoreflect.Message) int {
	n := 0
	visit(m, func(protoreflect.Message, protoreflect.FieldDescriptor) bool { n++; return false })
	return n
}

// visit calls fn for every populated field, parents before children, in
// field-number order; fn returning true stops the walk.
func visit(m protoreflect.Message, fn func(protoreflect.Message, protoreflect.FieldDescriptor) bool) bool {
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if !m.Has(fd) {
			continue
		}
		if fn(m, fd) {
			return true
		}
		if fd.Kind() != protoreflect.MessageKind {
			continue
		}
		if fd.IsList() {
			l := m.Get(fd).List()
			for j := 0; j < l.Len(); j++ {
				if visit(l.Get(j).Message(), fn) {
					return true
				}
			}
		} else if visit(m.Get(fd).Message(), fn) {
			return true
		}
	}
	return false
}

func TestAttrValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    *commonpb.AnyValue
		want any
	}{
		{nil, ""},
		{&commonpb.AnyValue{}, ""},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("hi")}}, "aGk="},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.Inf(-1)}}, "-Inf"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 7}}, int64(7)},
	}
	for _, c := range cases {
		if got := attrValue(c.v); got != c.want {
			t.Errorf("%v: %v, want %v", c.v, got, c.want)
		}
	}
}

func TestTraceFrameEmpty(t *testing.T) {
	t.Parallel()
	f, err := traceFrame(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := f.RowLen(); n != 0 || len(f.Fields) != 18 {
		t.Fatalf("empty trace: %d rows, %d fields", n, len(f.Fields))
	}
}
