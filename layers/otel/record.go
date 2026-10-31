package otel

import (
	"encoding/hex"
	"strconv"
	"sync"

	"fdb.dev/pkg/recordlayer"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

var (
	descriptorOnce     sync.Once
	envelopeDescriptor protoreflect.MessageDescriptor
	descriptorError    error
)

func envelopeType() (protoreflect.MessageDescriptor, error) {
	descriptorOnce.Do(func() {
		field := func(name string, n int32, t descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
			return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: t.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
		}
		union := field("envelope", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE)
		union.TypeName = proto.String(".otelbench.Envelope")
		fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{Name: proto.String("otelbench.proto"), Package: proto.String("otelbench"), Syntax: proto.String("proto2"), MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Envelope"), Field: []*descriptorpb.FieldDescriptorProto{field("tenant", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64), field("trace_id", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING), field("span_id", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING), field("start_ns", 4, descriptorpb.FieldDescriptorProto_TYPE_INT64), field("otlp", 5, descriptorpb.FieldDescriptorProto_TYPE_BYTES), field("ordinal", 6, descriptorpb.FieldDescriptorProto_TYPE_INT64)}}, {Name: proto.String("RecordTypeUnion"), Field: []*descriptorpb.FieldDescriptorProto{union}}}}, nil)
		descriptorError = err
		if err == nil {
			envelopeDescriptor = fd.Messages().ByName("Envelope")
		}
	})
	return envelopeDescriptor, descriptorError
}

func metadata(c Config) (*recordlayer.RecordMetaData, *recordlayer.Index, error) {
	d, err := envelopeType()
	if err != nil {
		return nil, nil, err
	}
	idx := recordlayer.NewIndex("typed_scoped_eq_v2", recordlayer.Field("start_ns"))
	idx.Type = indexType
	idx.SetOption("hash_bytes", strconv.Itoa(c.HashBytes))
	idx.SetOption("order", c.Order)
	if c.PostingValueFormat == "tuple" {
		idx.SetOption("value_format", "tuple")
	}
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(d.ParentFile()).SetSplitLongRecords(true)
	b.GetRecordType("Envelope").SetPrimaryKey(recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("trace_id"), recordlayer.Field("span_id")))
	b.AddIndex("Envelope", idx)
	b.AddIndex("Envelope", recordlayer.NewIndex(spanIndexName, recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("span_id"), recordlayer.Field("start_ns"))))
	b.AddIndex("Envelope", recordlayer.NewIndex(timeIndexName, recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("start_ns"), recordlayer.Field("trace_id"))))
	md, err := b.Build()
	return md, idx, err
}

func toRecord(s Span) (proto.Message, error) {
	d, err := envelopeType()
	if err != nil {
		return nil, err
	}
	tr, err := hex.DecodeString(s.Trace)
	if err != nil {
		return nil, wrap("admission", "trace identity", err)
	}
	id, err := hex.DecodeString(s.ID)
	if err != nil {
		return nil, wrap("admission", "span identity", err)
	}
	if len(tr) != 16 || len(id) != 8 {
		return nil, fail("admission", "identity", "expected 16-byte trace and 8-byte span identifiers")
	}
	otlp := &tracepb.ResourceSpans{Resource: &resourcepb.Resource{}, SchemaUrl: "otelbench-resource-v2", ScopeSpans: []*tracepb.ScopeSpans{{Scope: &commonpb.InstrumentationScope{Name: "otelbench", Version: "2"}, SchemaUrl: "otelbench-scope-v2", Spans: []*tracepb.Span{{TraceId: tr, SpanId: id, Name: "immutable-fixture", StartTimeUnixNano: uint64(s.Start), EndTimeUnixNano: uint64(s.Start + 1000)}}}}}
	for _, a := range s.Attributes {
		v := &commonpb.AnyValue{}
		switch a.Type {
		case "string":
			v.Value = &commonpb.AnyValue_StringValue{StringValue: a.Value}
		case "int":
			n, e := strconv.ParseInt(a.Value, 10, 64)
			if e != nil {
				return nil, wrap("admission", "integer attribute", e)
			}
			v.Value = &commonpb.AnyValue_IntValue{IntValue: n}
		default:
			return nil, failf("admission", "OTLP encode", "unsupported fixture type %q", a.Type)
		}
		kv := &commonpb.KeyValue{Key: a.Key, Value: v}
		switch a.Scope {
		case "span":
			otlp.ScopeSpans[0].Spans[0].Attributes = append(otlp.ScopeSpans[0].Spans[0].Attributes, kv)
		case "resource":
			otlp.Resource.Attributes = append(otlp.Resource.Attributes, kv)
		case "scope":
			otlp.ScopeSpans[0].Scope.Attributes = append(otlp.ScopeSpans[0].Scope.Attributes, kv)
		default:
			return nil, failf("admission", "OTLP encode", "unsupported fixture scope %q", a.Scope)
		}
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(otlp)
	if err != nil {
		return nil, err
	}
	m := dynamicpb.NewMessage(d)
	f := d.Fields()
	m.Set(f.ByName("ordinal"), protoreflect.ValueOfInt64(int64(s.Ordinal)))
	m.Set(f.ByName("tenant"), protoreflect.ValueOfInt64(int64(s.Tenant)))
	m.Set(f.ByName("trace_id"), protoreflect.ValueOfString(s.Trace))
	m.Set(f.ByName("span_id"), protoreflect.ValueOfString(s.ID))
	m.Set(f.ByName("start_ns"), protoreflect.ValueOfInt64(s.Start))
	m.Set(f.ByName("otlp"), protoreflect.ValueOfBytes(wire))
	return m, nil
}

// decodeRecord is the backend extractor, deliberately separate from Expected.
func decodeRecord(m proto.Message) (Span, error) {
	r := m.ProtoReflect()
	f := r.Descriptor().Fields()
	s := Span{Ordinal: int(r.Get(f.ByName("ordinal")).Int()), Tenant: int(r.Get(f.ByName("tenant")).Int()), Trace: r.Get(f.ByName("trace_id")).String(), ID: r.Get(f.ByName("span_id")).String(), Start: r.Get(f.ByName("start_ns")).Int()}
	var otlp tracepb.ResourceSpans
	if err := proto.Unmarshal(r.Get(f.ByName("otlp")).Bytes(), &otlp); err != nil {
		return s, wrap("admission", "OTLP decode", err)
	}
	if len(otlp.ScopeSpans) != 1 || len(otlp.ScopeSpans[0].Spans) != 1 {
		return s, fail("admission", "OTLP decode", "expected one source span")
	}
	sp := otlp.ScopeSpans[0].Spans[0]
	if hex.EncodeToString(sp.TraceId) != s.Trace || hex.EncodeToString(sp.SpanId) != s.ID || int64(sp.StartTimeUnixNano) != s.Start {
		return s, fail("admission", "OTLP decode", "envelope/source identity mismatch")
	}
	collect := func(scope string, kvs []*commonpb.KeyValue) error {
		seen := map[string]bool{}
		for _, kv := range kvs {
			if seen[kv.Key] {
				return fail("admission", "OTLP decode", "duplicate attribute key")
			}
			seen[kv.Key] = true
			a := Attribute{Scope: scope, Key: kv.Key}
			switch v := kv.GetValue().GetValue().(type) {
			case *commonpb.AnyValue_StringValue:
				a.Type = "string"
				a.Value = v.StringValue
			case *commonpb.AnyValue_IntValue:
				a.Type = "int"
				a.Value = strconv.FormatInt(v.IntValue, 10)
			default:
				return fail("admission", "OTLP decode", "unsupported source type")
			}
			s.Attributes = append(s.Attributes, a)
		}
		return nil
	}
	if err := collect("resource", otlp.GetResource().GetAttributes()); err != nil {
		return s, err
	}
	if err := collect("scope", otlp.ScopeSpans[0].GetScope().GetAttributes()); err != nil {
		return s, err
	}
	if err := collect("span", sp.Attributes); err != nil {
		return s, err
	}
	return s, nil
}
