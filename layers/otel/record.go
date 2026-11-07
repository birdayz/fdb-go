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

// Message storage layouts, part of a run's stored layout identity.
const (
	messagesNone     = ""
	messagesDeltas   = "deltas"      // Message records; spans carry manifests
	messagesFullText = "fulltext_v2" // plus the message TEXT index and span back references
)

// Index subspace keys of the message layout are small integers so every
// index key carries two bytes of prefix instead of the index name.
const (
	messageHashIndexName  = "message_hash_v1"
	messageLargeIndexName = "message_large_v1"
	messageTextIndexName  = "message_text_v1"
	envelopeRefsIndexName = "envelope_refs_v1"
	messageTextIndexKey   = int64(20)
	envelopeRefsIndexKey  = int64(22)
	messageHashIndexKey   = int64(23)
	messageLargeIndexKey  = int64(24)
	// Subspace keys of the span indexes in directory_v1 runs.
	eqIndexKey   = int64(1)
	spanIndexKey = int64(2)
	timeIndexKey = int64(3)
	// messageTraceTypeKey leads every MessageTrace primary key.
	messageTraceTypeKey = "mt"
)

var (
	descriptorOnce  sync.Once
	recordFiles     [2]protoreflect.FileDescriptor
	descriptorError error
)

// recordFile describes Envelope plus message records/references when enabled;
// optional envelope fidelity is separate from the producer’s OTLP namespace.
func recordFile(messages string) (protoreflect.FileDescriptor, error) {
	if messages != messagesNone && messageTokenizerRegistrationError != nil {
		return nil, wrap("config", "message tokenizer registration", messageTokenizerRegistrationError)
	}
	descriptorOnce.Do(func() {
		field := func(name string, n int32, t descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
			return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: t.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
		}
		repeatedField := func(name string, n int32, t descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
			f := field(name, n, t)
			f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			return f
		}
		message := func(name, typeName string, n int32, repeated bool) *descriptorpb.FieldDescriptorProto {
			f := field(name, n, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE)
			f.TypeName = proto.String(typeName)
			if repeated {
				f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
			}
			return f
		}
		const (
			i64   = descriptorpb.FieldDescriptorProto_TYPE_INT64
			str   = descriptorpb.FieldDescriptorProto_TYPE_STRING
			bytes = descriptorpb.FieldDescriptorProto_TYPE_BYTES
		)
		envelope := func(withMessages bool) *descriptorpb.DescriptorProto {
			d := &descriptorpb.DescriptorProto{Name: proto.String("Envelope"), Field: []*descriptorpb.FieldDescriptorProto{field("tenant", 1, i64), field("trace_id", 2, str), field("span_id", 3, str), field("start_ns", 4, i64), field("otlp", 5, bytes), field("ordinal", 6, i64), field("fidelity", 9, bytes)}}
			if withMessages {
				// The messages this span references, packed (messages.go).
				d.Field = append(d.Field, message("ref_chunks", ".otelbench.RefChunk", 8, true))
			}
			return d
		}
		base := &descriptorpb.FileDescriptorProto{Name: proto.String("otelbench.proto"), Package: proto.String("otelbench"), Syntax: proto.String("proto2"), MessageType: []*descriptorpb.DescriptorProto{envelope(false), {Name: proto.String("RecordTypeUnion"), Field: []*descriptorpb.FieldDescriptorProto{message("envelope", ".otelbench.Envelope", 1, false)}}}}
		withMessages := &descriptorpb.FileDescriptorProto{Name: proto.String("otelbench.proto"), Package: proto.String("otelbench"), Syntax: proto.String("proto2"), MessageType: []*descriptorpb.DescriptorProto{
			envelope(true),
			{Name: proto.String("RefChunk"), Field: []*descriptorpb.FieldDescriptorProto{field("chunk", 1, i64), field("refs", 2, bytes)}},
			// A message occurs in a trace (messages.go).
			{Name: proto.String("MessageTrace"), Field: []*descriptorpb.FieldDescriptorProto{field("tenant", 1, i64), field("writer", 2, i64), field("seq", 3, i64), field("trace_id", 4, str)}},
			// One part of a stored message (messages.go); part 0 carries the header.
			{Name: proto.String("Message"), Field: []*descriptorpb.FieldDescriptorProto{field("tenant", 1, i64), field("writer", 2, i64), field("seq", 3, i64), field("part", 4, i64), field("hash", 5, bytes), field("role", 6, str), field("text", 7, str), field("token_offset", 8, i64), field("raw", 9, bytes), field("encoding", 10, str), field("raw_bytes", 11, i64), field("parts", 12, i64), field("unindexed_tokens", 13, i64), repeatedField("large", 14, i64), field("shard", 15, i64)}},
			{Name: proto.String("RecordTypeUnion"), Field: []*descriptorpb.FieldDescriptorProto{message("envelope", ".otelbench.Envelope", 1, false), message("message", ".otelbench.Message", 2, false), message("message_trace", ".otelbench.MessageTrace", 3, false)}},
		}}
		for i, fdp := range []*descriptorpb.FileDescriptorProto{base, withMessages} {
			fd, err := protodesc.NewFile(fdp, nil)
			if err != nil {
				descriptorError = err
				return
			}
			recordFiles[i] = fd
		}
	})
	if descriptorError != nil {
		return nil, descriptorError
	}
	if messages == messagesNone {
		return recordFiles[0], nil
	}
	return recordFiles[1], nil
}

// envelopeType is the base layout's Envelope, used by the fixture corpus.
func envelopeType() (protoreflect.MessageDescriptor, error) {
	fd, err := recordFile(messagesNone)
	if err != nil {
		return nil, err
	}
	return fd.Messages().ByName("Envelope"), nil
}

func metadata(c Config) (*recordlayer.RecordMetaData, *recordlayer.Index, error) {
	fd, err := recordFile(c.Messages)
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
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd).SetSplitLongRecords(true)
	b.GetRecordType("Envelope").SetPrimaryKey(recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("trace_id"), recordlayer.Field("span_id")))
	spanIdx := recordlayer.NewIndex(spanIndexName, recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("span_id"), recordlayer.Field("start_ns")))
	timeIdx := recordlayer.NewIndex(timeIndexName, recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("start_ns"), recordlayer.Field("trace_id")))
	if c.KeyLayout == keyLayoutDirectory {
		// Integer subspace keys, as the message indexes have; tuple-layout
		// runs keep their index names.
		idx.SetSubspaceKey(eqIndexKey)
		spanIdx.SetSubspaceKey(spanIndexKey)
		timeIdx.SetSubspaceKey(timeIndexKey)
	}
	b.AddIndex("Envelope", idx)
	b.AddIndex("Envelope", spanIdx)
	b.AddIndex("Envelope", timeIdx)
	if c.Messages != messagesNone {
		b.GetRecordType("Message").SetPrimaryKey(recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("writer"), recordlayer.Field("seq"), recordlayer.Field("part")))
		// The string record type key keeps markers apart from the other
		// records, whose primary keys start with the integer tenant.
		b.GetRecordType("MessageTrace").SetRecordTypeKey(messageTraceTypeKey).SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("tenant"), recordlayer.Field("writer"), recordlayer.Field("seq"), recordlayer.Field("trace_id")))
		// (tenant, digest) -> message; only part 0 has a digest, and a unique
		// index ignores the null digests of other parts.
		dedup := recordlayer.NewIndex(messageHashIndexName, recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("hash")))
		dedup.SetOption(recordlayer.IndexOptionUnique, "true")
		dedup.SetSubspaceKey(messageHashIndexKey)
		b.AddIndex("Message", dedup)
	}
	if c.Messages == messagesFullText {
		text := recordlayer.NewIndex(messageTextIndexName, recordlayer.GroupBy(recordlayer.Field("text"), recordlayer.Field("tenant"), recordlayer.Field("role"), recordlayer.Field("shard")))
		text.Type = recordlayer.IndexTypeText
		text.SetOption(recordlayer.IndexOptionTextTokenizerName, messageTokenizerName)
		text.SetSubspaceKey(messageTextIndexKey)
		b.AddIndex("Message", text)
		// (tenant, trace_id, start_ns, chunk, span_id) -> packed refs, chunked at 16KB,
		// so all spans and references of a trace occupy one range.
		refs := recordlayer.NewIndex(envelopeRefsIndexName, recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.Field("trace_id"), recordlayer.Field("start_ns"), recordlayer.NestFanOut("ref_chunks", recordlayer.Concat(recordlayer.Field("chunk"), recordlayer.Field("refs")))), 4))
		refs.SetSubspaceKey(envelopeRefsIndexKey)
		b.AddIndex("Envelope", refs)
		// (tenant) -> multi-part messages: only their part 0 has a large
		// element, so other messages add no entry.
		large := recordlayer.NewIndex(messageLargeIndexName, recordlayer.Concat(recordlayer.Field("tenant"), recordlayer.FanOut("large")))
		large.SetSubspaceKey(messageLargeIndexKey)
		b.AddIndex("Message", large)
	}
	md, err := b.Build()
	return md, idx, err
}

func toRecord(s Span) (proto.Message, error) {
	d, err := envelopeType()
	if err != nil {
		return nil, err
	}
	_, wire, err := FixtureOTLP(s)
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

// FixtureOTLP is a fixture span's single-span ResourceSpans and its
// deterministic wire bytes, the envelope every backend stores.
func FixtureOTLP(s Span) (*tracepb.ResourceSpans, []byte, error) {
	tr, err := hex.DecodeString(s.Trace)
	if err != nil {
		return nil, nil, wrap("admission", "trace identity", err)
	}
	id, err := hex.DecodeString(s.ID)
	if err != nil {
		return nil, nil, wrap("admission", "span identity", err)
	}
	if len(tr) != 16 || len(id) != 8 {
		return nil, nil, fail("admission", "identity", "expected 16-byte trace and 8-byte span identifiers")
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
				return nil, nil, wrap("admission", "integer attribute", e)
			}
			v.Value = &commonpb.AnyValue_IntValue{IntValue: n}
		default:
			return nil, nil, failf("admission", "OTLP encode", "unsupported fixture type %q", a.Type)
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
			return nil, nil, failf("admission", "OTLP encode", "unsupported fixture scope %q", a.Scope)
		}
	}
	wire, err := proto.MarshalOptions{Deterministic: true}.Marshal(otlp)
	if err != nil {
		return nil, nil, err
	}
	return otlp, wire, nil
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
