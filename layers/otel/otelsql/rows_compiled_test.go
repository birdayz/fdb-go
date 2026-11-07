package otelsql

import (
	"bytes"
	"math"
	"sort"
	"strings"
	"testing"

	"fdb.dev/layers/otel/otelsql/otelv1pb"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func templateMetaData(t *testing.T) *recordlayer.RecordMetaData {
	t.Helper()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(TemplateDDL())
	if err != nil {
		t.Fatal(err)
	}
	return tmpl.Underlying()
}

// templateTables are the template's tables, each with its compiled type.
var templateTables = map[string]proto.Message{
	"SPANS": &otelv1pb.SPANS{}, "LOGS": &otelv1pb.LOGS{}, "MESSAGES": &otelv1pb.MESSAGES{},
	"MESSAGE_TRACES": &otelv1pb.MESSAGE_TRACES{}, "STORE_META": &otelv1pb.STORE_META{},
}

// otelv1pb is generated from the records descriptor the template's DDL
// compiles to. A template change without regeneration fails here (and would
// fail OpenTenantStoreCompiled's evolution check at run time).
func TestCompiledTypesMatchTemplateDescriptor(t *testing.T) {
	t.Parallel()
	md := templateMetaData(t)
	want := protodesc.ToFileDescriptorProto(md.GetUnionDescriptor().ParentFile())
	got := protodesc.ToFileDescriptorProto(otelv1pb.File_OTEL_V1)
	got.Options.GoPackage = nil
	for _, f := range []*descriptorpb.FileDescriptorProto{want, got} {
		if f.Options != nil && proto.Size(f.Options) == 0 {
			f.Options = nil
		}
	}
	if !proto.Equal(want, got) {
		t.Fatal("otelv1pb is stale: regenerate with bazelisk run //layers/otel/otelsql/cmd/genotelv1 -- \"$PWD/layers/otel/otelsql/otelv1pb\"")
	}
	var tables []string
	for name := range md.RecordTypes() {
		tables = append(tables, name)
	}
	sort.Strings(tables)
	if len(tables) != len(templateTables) {
		t.Fatalf("template tables %v, compiled %d", tables, len(templateTables))
	}
	for _, name := range tables {
		m, ok := templateTables[name]
		if !ok {
			t.Fatalf("table %s has no compiled type", name)
		}
		if got, want := m.ProtoReflect().Descriptor().FullName(), md.GetRecordType(name).Descriptor.FullName(); got != want {
			t.Errorf("table %s: compiled %s, catalog %s", name, got, want)
		}
	}
}

func kvStr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func kvInt(k string, v int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: v}}}
}

// fixtureAttributes has every value kind, each cap's edge, a repeated key, a
// nil entry and a GenAI message key.
func fixtureAttributes() []*commonpb.KeyValue {
	return []*commonpb.KeyValue{
		kvStr("s", "v"), kvStr("empty", ""), kvStr("prompt", strings.Repeat("l", MaxIndexedStringBytes+1)),
		kvStr("longest", strings.Repeat("m", MaxAttributeValueBytes)), kvStr("too_long", strings.Repeat("n", MaxAttributeValueBytes+1)),
		kvStr(strings.Repeat("k", MaxAttributeKeyBytes+1), "dropped key"), kvStr(strings.Repeat("k", MaxAttributeKeyBytes), "longest key"),
		kvInt("i", -7), kvStr("dup", "first"),
		{Key: "d", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 2.5}}},
		{Key: "nan", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: math.NaN()}}},
		{Key: "b", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
		{Key: "raw", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1, 2}}}},
		{Key: "raw_empty", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{}}},
		{Key: "arr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}}}}}},
		{Key: "kv", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kvStr("a", "b")}}}}},
		{Key: "none", Value: &commonpb.AnyValue{}},
		{Key: "nil"},
		nil,
		kvStr("gen_ai.input.messages", "[]"),
		kvInt("dup", 2),
	}
}

func compiledFixtureSpans() []Span {
	all := fixtureAttributes()
	rich := func(exclude bool, refs []MessageRef) Span {
		return Span{
			Resource:          &resourcepb.Resource{Attributes: append([]*commonpb.KeyValue{kvStr("service.name", "svc"), kvStr("service.name", "")}, all...)},
			ResourceSchemaURL: "https://res", Scope: &commonpb.InstrumentationScope{Name: "lib", Version: "1", Attributes: all},
			ScopeSchemaURL: "https://scope", OTLP: []byte("envelope"), Refs: refs, ExcludeMessageAttributes: exclude,
			Span: &tracepb.Span{
				TraceId: []byte{9, 9}, SpanId: []byte{8}, ParentSpanId: []byte{7}, TraceState: "ts", Flags: 3, Name: "op",
				Kind: tracepb.Span_SPAN_KIND_SERVER, StartTimeUnixNano: 10, EndTimeUnixNano: 20, Attributes: all,
				DroppedAttributesCount: 1, DroppedEventsCount: 2, DroppedLinksCount: 3,
				Events: []*tracepb.Span_Event{{Name: "e", TimeUnixNano: 11, Attributes: all, DroppedAttributesCount: 4}, {}},
				Links:  []*tracepb.Span_Link{{TraceId: []byte{5}, SpanId: []byte{6}, TraceState: "lt", Flags: 1, Attributes: all, DroppedAttributesCount: 5}, {}},
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "bad"},
			},
		}
	}
	return []Span{
		{Span: &tracepb.Span{TraceId: []byte{1}, SpanId: []byte{2}}},
		{Span: &tracepb.Span{}, OTLP: []byte{}},
		rich(false, nil),
		rich(true, []MessageRef{{Writer: 1, Seq: 2, Attribute: 1, Newest: true, Ordinals: []int{0, 3}}, {Writer: 1 << 56, Seq: 256, Attribute: 4, Ordinals: []int{256}}}),
		{Span: &tracepb.Span{Status: &tracepb.Status{}}, Scope: &commonpb.InstrumentationScope{}, Resource: &resourcepb.Resource{}},
	}
}

// BuildCompiled's records are Build's: byte-for-byte under deterministic
// marshaling, and equal as messages once the compiled bytes are read into the
// catalog's dynamic descriptor. The other tables' compiled records read back
// through the catalog's descriptors as the same bytes, with no unknown field.
func TestBuildCompiledIsBuild(t *testing.T) {
	t.Parallel()
	md := templateMetaData(t)
	rows, err := NewSpanRows(md)
	if err != nil {
		t.Fatal(err)
	}
	det := proto.MarshalOptions{Deterministic: true}
	for i, s := range compiledFixtureSpans() {
		dyn, err := rows.Build(s)
		if err != nil {
			t.Fatal(err)
		}
		cmp, err := BuildCompiled(s)
		if err != nil {
			t.Fatal(err)
		}
		dynBytes, err := det.Marshal(dyn)
		if err != nil {
			t.Fatal(err)
		}
		cmpBytes, err := det.Marshal(cmp)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(dynBytes, cmpBytes) {
			t.Errorf("span %d: compiled bytes differ from Build's", i)
		}
		back := readBack(t, md, "SPANS", cmp)
		if !proto.Equal(back, dyn) {
			t.Errorf("span %d: compiled record reads back as another row", i)
		}
	}
	if _, err := BuildCompiled(Span{}); err == nil {
		t.Error("BuildCompiled of a nil span succeeded")
	}
	if _, err := rows.Build(Span{}); err == nil {
		t.Error("Build of a nil span succeeded")
	}

	for i, l := range fixtureLogs() {
		r, err := BuildLogCompiled(l)
		if err != nil {
			t.Fatal(err)
		}
		readBack(t, md, "LOGS", r)
		if i == 0 && r.TRACE_ID != nil {
			t.Error("uncorrelated log has a trace_id")
		}
	}
	for _, p := range []MessagePart{
		{
			Writer: 3, Seq: 1, Part: 0, Text: &MessageText{Role: "user", Content: "hello"}, Raw: []byte("hello"), ContentHash: MessageContentHash("any", []byte("hello")),
			Encoding: "any", RawBytes: 5, Parts: 2, UnindexedTokens: 1, Large: true,
		},
		{Writer: 3, Seq: 1, Part: 1, Text: &MessageText{Role: "user", Content: "world", TokenOffset: 7}},
		{Writer: 19, Seq: 4, Part: 0, Raw: []byte{}, Encoding: "json"},
	} {
		r, err := BuildMessageCompiled(p)
		if err != nil {
			t.Fatal(err)
		}
		readBack(t, md, "MESSAGES", r)
	}
	readBack(t, md, "MESSAGE_TRACES", BuildMessageTraceCompiled(3, 1, []byte{0, 1}))
	n, s := int64(7), "fulltext"
	for _, m := range []StoreMeta{{Name: StoreMetaMessageWriter, Int: &n}, {Name: StoreMetaContinuationHMAC, Bytes: []byte{0, 1}}, {Name: StoreMetaLayout, Str: &s}} {
		r, err := BuildStoreMetaCompiled(m)
		if err != nil {
			t.Fatal(err)
		}
		readBack(t, md, "STORE_META", r)
	}
}

// readBack reads m's deterministic bytes as table's catalog row and requires
// every field known and the same bytes back.
func readBack(t *testing.T, md *recordlayer.RecordMetaData, table string, m proto.Message) *dynamicpb.Message {
	t.Helper()
	det := proto.MarshalOptions{Deterministic: true}
	raw, err := det.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	back := dynamicpb.NewMessage(md.GetRecordType(table).Descriptor)
	if err := proto.Unmarshal(raw, back); err != nil {
		t.Fatal(err)
	}
	requireNoUnknown(t, table, back)
	again, err := det.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, again) {
		t.Errorf("%s: catalog row re-marshals to other bytes", table)
	}
	return back
}

func requireNoUnknown(t *testing.T, path string, m protoreflect.Message) {
	t.Helper()
	if len(m.GetUnknown()) > 0 {
		t.Errorf("%s: unknown fields %x", path, m.GetUnknown())
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		if fd.IsList() {
			for i := 0; i < v.List().Len(); i++ {
				requireNoUnknown(t, path+"."+string(fd.Name()), v.List().Get(i).Message())
			}
			return true
		}
		requireNoUnknown(t, path+"."+string(fd.Name()), v.Message())
		return true
	})
}

// The compiled types bind to the template's metadata (WithLocalFileDescriptor)
// and are then each table's record type descriptor, under TemplateName and
// under RevisionTemplateName, which only renames the records file.
func TestCompiledTypesBindToTemplateMetaData(t *testing.T) {
	t.Parallel()
	revision := RevisionTemplateName()
	if revision == TemplateName || revision != RevisionTemplateName() || !strings.HasPrefix(revision, TemplateName+"_") || len(revision) != len(TemplateName)+9 {
		t.Fatalf("revision template name %q", revision)
	}
	for _, name := range []string{TemplateName, revision} {
		tmpl, err := embedded.BuildSchemaTemplateFromDDL(templateDDL(name))
		if err != nil {
			t.Fatal(err)
		}
		records := protodesc.ToFileDescriptorProto(tmpl.Underlying().GetUnionDescriptor().ParentFile())
		if records.GetName() != name {
			t.Fatalf("records file %q under template %s", records.GetName(), name)
		}
		p, err := tmpl.Underlying().ToProto()
		if err != nil {
			t.Fatal(err)
		}
		md, err := recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for table, m := range templateTables {
			if rt, err := md.GetRecordTypeForDescriptor(m.ProtoReflect().Descriptor()); err != nil || rt.Name != table {
				t.Fatalf("%s: %s record type for the compiled descriptor: %v, %v", name, table, rt, err)
			}
		}
	}
}
