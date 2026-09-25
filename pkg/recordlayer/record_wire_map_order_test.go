package recordlayer

import (
	"bytes"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// wireMapFile: Rec { id = 1; map<string, int64> m = 2; repeated Holder h = 3;
// Loop loop = 4 } with Holder { map<string, int64> hm = 1 } and a recursive
// Loop { Loop next = 1; int64 v = 2 } that reaches no map.
func wireMapFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, number int32, label *descriptorpb.FieldDescriptorProto_Label, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	entry := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				field("key", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
				field("value", 2, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
			},
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
		}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("wire_map_order.proto"),
		Package: proto.String("wiremap"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Rec"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
					field("m", 2, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Rec.MEntry"),
					field("h", 3, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Holder"),
					field("loop", 4, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Loop"),
				},
				NestedType: []*descriptorpb.DescriptorProto{entry("MEntry")},
			},
			{
				Name:       proto.String("Holder"),
				Field:      []*descriptorpb.FieldDescriptorProto{field("hm", 1, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Holder.HmEntry")},
				NestedType: []*descriptorpb.DescriptorProto{entry("HmEntry")},
			},
			{
				Name: proto.String("Loop"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("next", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Loop"),
					field("v", 2, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
				},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

type wireEntry struct {
	key      string
	value    int64
	hasValue bool
}

// mapEntryBytes is one map entry on the wire, its value omitted when !hasValue.
func mapEntryBytes(fieldNum protowire.Number, e wireEntry) []byte {
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendString(body, e.key)
	if e.hasValue {
		body = protowire.AppendTag(body, 2, protowire.VarintType)
		body = protowire.AppendVarint(body, uint64(e.value))
	}
	out := protowire.AppendTag(nil, fieldNum, protowire.BytesType)
	return protowire.AppendBytes(out, body)
}

func kv(key string, value int64) wireEntry { return wireEntry{key, value, true} }

// decodedRecord decodes raw as a Rec, with its wire, as the store decodes it.
func decodedRecord(t *testing.T, md protoreflect.MessageDescriptor, raw []byte) *FDBStoredRecord[proto.Message] {
	t.Helper()
	msg := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(raw, msg); err != nil {
		t.Fatal(err)
	}
	return &FDBStoredRecord[proto.Message]{Record: msg, wire: newRecordWire(md, raw)}
}

func evaluateTuples(t *testing.T, expr KeyExpression, rec *FDBStoredRecord[proto.Message]) [][]any {
	t.Helper()
	got, err := expr.Evaluate(rec, rec.Record)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A record decoded from bytes visits its map entries in the bytes' order,
// duplicates of a key included, as a DynamicMessage holds them; the same
// message with no wire is visited in key order.
func TestMapEntriesAreEvaluatedInWireOrder(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	expr := NestFanOut("m", Concat(Field("key"), Field("value")))

	var raw []byte
	for _, e := range []wireEntry{kv("x", 1), kv("b", 5), kv("a", 5), kv("x", 3)} {
		raw = append(raw, mapEntryBytes(2, e)...)
	}
	decoded := decodedRecord(t, rec, raw)
	want := [][]any{{"x", int64(1)}, {"b", int64(5)}, {"a", int64(5)}, {"x", int64(3)}}
	if got := evaluateTuples(t, expr, decoded); !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded: %v, want %v", got, want)
	}

	inMemory := &FDBStoredRecord[proto.Message]{Record: decoded.Record}
	want = [][]any{{"a", int64(5)}, {"b", int64(5)}, {"x", int64(3)}}
	if got := evaluateTuples(t, expr, inMemory); !reflect.DeepEqual(got, want) {
		t.Fatalf("in memory: %v, want %v", got, want)
	}
}

// A loaded message changed after it was loaded no longer holds what its bytes
// hold, and is evaluated in key order, as a message Go is about to save.
func TestMapEntriesOfAChangedMessageAreEvaluatedInKeyOrder(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	expr := NestFanOut("m", Field("key"))
	raw := append(mapEntryBytes(2, kv("b", 1)), mapEntryBytes(2, kv("a", 2))...)
	for _, c := range []struct {
		name   string
		change func(protoreflect.Map)
		want   [][]any
	}{
		{"unchanged", func(protoreflect.Map) {}, [][]any{{"b"}, {"a"}}},
		{"a value changed", func(m protoreflect.Map) {
			m.Set(protoreflect.ValueOfString("a").MapKey(), protoreflect.ValueOfInt64(9))
		}, [][]any{{"a"}, {"b"}}},
		{"an entry added", func(m protoreflect.Map) {
			m.Set(protoreflect.ValueOfString("c").MapKey(), protoreflect.ValueOfInt64(3))
		}, [][]any{{"a"}, {"b"}, {"c"}}},
		{"an entry removed", func(m protoreflect.Map) {
			m.Clear(protoreflect.ValueOfString("a").MapKey())
		}, [][]any{{"b"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			decoded := decodedRecord(t, rec, raw)
			c.change(decoded.Record.ProtoReflect().Mutable(rec.Fields().ByName("m")).Map())
			if got := evaluateTuples(t, expr, decoded); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%v, want %v", got, c.want)
			}
		})
	}
}

// A map inside the i-th element of a repeated message field is read from that
// element's bytes, and a message that reaches a map only through a recursive
// type terminates.
func TestMapEntriesInNestedMessagesAreEvaluatedInWireOrder(t *testing.T) {
	t.Parallel()
	file := wireMapFile(t)
	rec := file.Messages().ByName("Rec")
	holder := func(entries ...wireEntry) []byte {
		var body []byte
		for _, e := range entries {
			body = append(body, mapEntryBytes(1, e)...)
		}
		return protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), body)
	}
	raw := append(holder(kv("z", 1), kv("y", 2)), holder(kv("q", 3), kv("p", 4))...)
	decoded := decodedRecord(t, rec, raw)
	got := evaluateTuples(t, NestFanOut("h", NestFanOut("hm", Field("key"))), decoded)
	if want := [][]any{{"z"}, {"y"}, {"q"}, {"p"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	if messageReachesMap(file.Messages().ByName("Loop")) {
		t.Fatal("Loop reaches no map")
	}
	if !messageReachesMap(rec) || !messageReachesMap(file.Messages().ByName("Holder")) {
		t.Fatal("Rec and Holder reach a map")
	}
	if newRecordWire(file.Messages().ByName("Loop"), raw) != nil {
		t.Fatal("a type that reaches no map keeps no wire")
	}
}

// An entry whose bytes lack its value holds the value's default, as the Go map
// the decoder filled does, so the wire order and key order read one entry
// alike.
func TestMapEntryWithoutValueReadsTheDefault(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	raw := append(mapEntryBytes(2, wireEntry{key: "k"}), mapEntryBytes(2, kv("j", 2))...)
	decoded := decodedRecord(t, rec, raw)
	got := evaluateTuples(t, NestFanOut("m", Concat(Field("key"), Field("value"))), decoded)
	if want := [][]any{{"k", int64(0)}, {"j", int64(2)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}

// A type that reaches a map is written with its map entries in key order, the
// order Go evaluated them in, whatever order the map was filled in.
func TestSerializeUnionWritesMapEntriesInKeyOrder(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	msg := dynamicpb.NewMessage(rec)
	m := msg.Mutable(rec.Fields().ByName("m")).Map()
	for _, k := range []string{"d", "b", "c", "a", "e"} {
		m.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfInt64(1))
	}
	out, err := serializeUnion(msg, &RecordType{Name: "Rec", Descriptor: rec, unionFieldNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, _, n := protowire.ConsumeTag(out)
	inner, _ := protowire.ConsumeBytes(out[n:])
	var keys []string
	for len(inner) > 0 {
		_, _, n := protowire.ConsumeTag(inner)
		body, k := protowire.ConsumeBytes(inner[n:])
		inner = inner[n+k:]
		_, _, t1 := protowire.ConsumeTag(body)
		key, _ := protowire.ConsumeString(body[t1:])
		keys = append(keys, key)
	}
	if want := []string{"a", "b", "c", "d", "e"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("written keys %v, want %v", keys, want)
	}
	again, err := serializeUnion(msg, &RecordType{Name: "Rec", Descriptor: rec, unionFieldNumber: 1})
	if err != nil || !bytes.Equal(out, again) {
		t.Fatalf("not byte-stable: %v", err)
	}
}

// vtMapRecord is a message that offers vtproto's marshal, as a generated type
// with a map does; its MarshalVT writes a map in Go's iteration order, so the
// serializer must not call it for a type that reaches a map.
type vtMapRecord struct{ *dynamicpb.Message }

func (vtMapRecord) SizeVT() int { panic("SizeVT called for a type that reaches a map") }

func (vtMapRecord) MarshalToSizedBufferVT([]byte) (int, error) {
	panic("MarshalToSizedBufferVT called for a type that reaches a map")
}

func (vtMapRecord) MarshalVT() ([]byte, error) {
	panic("MarshalVT called for a type that reaches a map")
}

func TestSerializeUnionDoesNotUseVTMarshalForAMapType(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	msg := dynamicpb.NewMessage(rec)
	m := msg.Mutable(rec.Fields().ByName("m")).Map()
	for _, k := range []string{"b", "a"} {
		m.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfInt64(1))
	}
	rt := &RecordType{Name: "Rec", Descriptor: rec, unionFieldNumber: 1}
	got, err := serializeUnion(vtMapRecord{msg}, rt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := serializeUnion(msg, rt)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("serialized %x, want %x (%v)", got, want, err)
	}
}
