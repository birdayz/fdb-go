package otelsql

import (
	"bytes"
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

// otelv1pb is generated from the records descriptor the template's DDL
// compiles to. A template change without regeneration fails here (and would
// fail OpenTenantStoreCompiled's evolution check at run time).
func TestCompiledTypesMatchTemplateDescriptor(t *testing.T) {
	t.Parallel()
	want := protodesc.ToFileDescriptorProto(templateMetaData(t).GetUnionDescriptor().ParentFile())
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
}

func kvStr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func compiledFixtureSpans() []Span {
	long := strings.Repeat("l", MaxIndexedStringBytes+1)
	all := []*commonpb.KeyValue{
		kvStr("s", "v"), kvStr("empty", ""), kvStr("prompt", long), kvStr(strings.Repeat("k", MaxIndexedStringBytes+1), "dropped key"),
		{Key: "i", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -7}}},
		{Key: "d", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 2.5}}},
		{Key: "b", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
		{Key: "raw", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{1, 2}}}},
		{Key: "raw_empty", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{}}},
		{Key: "arr", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{{Value: &commonpb.AnyValue_IntValue{IntValue: 1}}}}}}},
		{Key: "kv", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{kvStr("a", "b")}}}}},
		{Key: "none", Value: &commonpb.AnyValue{}},
		{Key: "nil"},
	}
	return []Span{
		{Span: &tracepb.Span{TraceId: []byte{1}, SpanId: []byte{2}}},
		{Span: &tracepb.Span{}, OTLP: []byte{}},
		{
			Resource:          &resourcepb.Resource{Attributes: append([]*commonpb.KeyValue{kvStr("service.name", "svc"), kvStr("service.name", "")}, all...)},
			ResourceSchemaURL: "https://res", Scope: &commonpb.InstrumentationScope{Name: "lib", Version: "1", Attributes: all},
			ScopeSchemaURL: "https://scope", OTLP: []byte("envelope"),
			Span: &tracepb.Span{
				TraceId: []byte{9, 9}, SpanId: []byte{8}, ParentSpanId: []byte{7}, TraceState: "ts", Flags: 3, Name: "op",
				Kind: tracepb.Span_SPAN_KIND_SERVER, StartTimeUnixNano: 10, EndTimeUnixNano: 20, Attributes: all,
				DroppedAttributesCount: 1, DroppedEventsCount: 2, DroppedLinksCount: 3,
				Events: []*tracepb.Span_Event{{Name: "e", TimeUnixNano: 11, Attributes: all, DroppedAttributesCount: 4}, {}},
				Links:  []*tracepb.Span_Link{{TraceId: []byte{5}, SpanId: []byte{6}, TraceState: "lt", Flags: 1, Attributes: all, DroppedAttributesCount: 5}, {}},
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "bad"},
			},
		},
		{Span: &tracepb.Span{Status: &tracepb.Status{}}, Scope: &commonpb.InstrumentationScope{}, Resource: &resourcepb.Resource{}},
	}
}

// BuildCompiled's records are Build's: byte-for-byte under deterministic
// marshaling, and equal as messages once the compiled bytes are read into the
// catalog's dynamic descriptor.
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
		back := dynamicpb.NewMessage(md.GetRecordType("SPANS").Descriptor)
		if err := proto.Unmarshal(cmpBytes, back); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(back, dyn) {
			t.Errorf("span %d: compiled record reads back as another row", i)
		}
	}
	if _, err := BuildCompiled(Span{}); err == nil {
		t.Error("BuildCompiled of a nil span succeeded")
	}
}

// The compiled types bind to the template's metadata (WithLocalFileDescriptor)
// and are then the SPANS record type's descriptor.
func TestCompiledTypesBindToTemplateMetaData(t *testing.T) {
	t.Parallel()
	p, err := templateMetaData(t).ToProto()
	if err != nil {
		t.Fatal(err)
	}
	md, err := recordlayer.RecordMetaDataFromProto(p, recordlayer.WithLocalFileDescriptor(otelv1pb.File_OTEL_V1))
	if err != nil {
		t.Fatal(err)
	}
	if rt, err := md.GetRecordTypeForDescriptor((&otelv1pb.SPANS{}).ProtoReflect().Descriptor()); err != nil || rt.Name != "SPANS" {
		t.Fatalf("SPANS record type for the compiled descriptor: %v, %v", rt, err)
	}
}
