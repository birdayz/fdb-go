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
// Loop loop = 4; oneof choice { Holder ha = 5; Holder hb = 6 };
// map<string, Holder> mh = 7; Node tree = 8 } with Holder { map<string, int64>
// hm = 1 }, a recursive Node { map<string, Node> kids = 1 }, and a recursive
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
	inOneof := func(f *descriptorpb.FieldDescriptorProto) *descriptorpb.FieldDescriptorProto {
		f.OneofIndex = proto.Int32(0)
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
	messageEntry := func(name, valueType string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				field("key", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
				field("value", 2, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, valueType),
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
					inOneof(field("ha", 5, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Holder")),
					inOneof(field("hb", 6, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Holder")),
					field("mh", 7, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Rec.MhEntry"),
					field("tree", 8, optional, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Node"),
				},
				OneofDecl:  []*descriptorpb.OneofDescriptorProto{{Name: proto.String("choice")}},
				NestedType: []*descriptorpb.DescriptorProto{entry("MEntry"), messageEntry("MhEntry", ".wiremap.Holder")},
			},
			{
				Name:       proto.String("Holder"),
				Field:      []*descriptorpb.FieldDescriptorProto{field("hm", 1, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Holder.HmEntry")},
				NestedType: []*descriptorpb.DescriptorProto{entry("HmEntry")},
			},
			{
				Name:       proto.String("Node"),
				Field:      []*descriptorpb.FieldDescriptorProto{field("kids", 1, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremap.Node.KidsEntry")},
				NestedType: []*descriptorpb.DescriptorProto{messageEntry("KidsEntry", ".wiremap.Node")},
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
	return &FDBStoredRecord[proto.Message]{Record: msg, wire: newRecordWire(testRecordType(md), raw)}
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
	// Build's reach answers every type the record types reach, recursive ones
	// included, and is not written after: a type outside it is answered
	// without being stored.
	msgs := file.Messages()
	reach := newMapReach(rec)
	for name, want := range map[protoreflect.Name]bool{"Rec": true, "Holder": true, "Node": true, "Loop": false} {
		v, ok := reach[msgs.ByName(name)]
		if !ok || v != want {
			t.Errorf("reach[%s] = %t, %t; want %t, true", name, v, ok, want)
		}
	}
	size := len(reach)
	if reach.reaches(msgs.ByName("Loop").Fields().ByName("next").Message()) {
		t.Error("Loop reaches no map")
	}
	outside := newMapReach(msgs.ByName("Loop"))
	if len(outside) != 1 || !reach.reaches(rec) || mapReach(nil).reaches(msgs.ByName("Holder")) != true || len(reach) != size {
		t.Errorf("reach grew or answered wrongly: %d types, %d before; Loop's own reach %v", len(reach), size, outside)
	}
	if newRecordWire(testRecordType(file.Messages().ByName("Loop")), raw) != nil {
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
	out, err := serializeUnion(msg, testRecordType(rec))
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
	again, err := serializeUnion(msg, testRecordType(rec))
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
	rt := testRecordType(rec)
	got, err := serializeUnion(vtMapRecord{msg}, rt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := serializeUnion(msg, rt)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("serialized %x, want %x (%v)", got, want, err)
	}
}

// testRecordType is a record type over md as Build makes one: union field 1,
// its map reach computed.
func testRecordType(md protoreflect.MessageDescriptor) *RecordType {
	reach := newMapReach(md)
	return &RecordType{Name: string(md.Name()), Descriptor: md, unionFieldNumber: 1, reachesMap: reach.reaches(md), mapReach: reach}
}

// writtenKeys is the map keys of field num in inner, a Rec's bytes, in order,
// and of the hm field of each Holder element when num is 3.
func writtenKeys(t *testing.T, inner []byte, num protowire.Number) []string {
	t.Helper()
	var keys []string
	for len(inner) > 0 {
		n, typ, k := protowire.ConsumeTag(inner)
		size := protowire.ConsumeFieldValue(n, typ, inner[k:])
		value := inner[k : k+size]
		inner = inner[k+size:]
		if n != num {
			continue
		}
		body, _ := protowire.ConsumeBytes(value)
		if num == 3 {
			keys = append(keys, writtenKeys(t, body, 1)...)
			continue
		}
		_, _, tk := protowire.ConsumeTag(body)
		key, _ := protowire.ConsumeString(body[tk:])
		keys = append(keys, key)
	}
	return keys
}

// A record that replaces a stored one writes each map as Java's load-then-save
// of the stored one writes it (measured by the JVM spec "Map entries are
// maintained in the record's wire order"): the stored entries in their order,
// each unchanged key's every entry, a key written twice included, with the
// value that entry held; a changed key once, in its first position; the new
// keys after, in key order. A new record's maps are in key order. Maps below
// the top level are TestSerializeUnionOverKeepsNestedMapOrders'.
func TestSerializeUnionOverKeepsTheStoredMapOrder(t *testing.T) {
	t.Parallel()
	file := wireMapFile(t)
	rec := file.Messages().ByName("Rec")
	rt := testRecordType(rec)
	prior := func(entries ...wireEntry) []byte {
		var raw []byte
		for _, e := range entries {
			raw = append(raw, mapEntryBytes(2, e)...)
		}
		return protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), raw)
	}
	message := func(entries map[string]int64) *dynamicpb.Message {
		msg := dynamicpb.NewMessage(rec)
		m := msg.Mutable(rec.Fields().ByName("m")).Map()
		for k, v := range entries {
			m.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfInt64(v))
		}
		return msg
	}
	stored := prior(kv("c", 1), kv("a", 2), kv("b", 3))
	for _, c := range []struct {
		name    string
		prior   []byte
		entries map[string]int64
		want    []string
	}{
		{"unchanged", stored, map[string]int64{"a": 2, "b": 3, "c": 1}, []string{"c", "a", "b"}},
		{"a value changed", stored, map[string]int64{"a": 9, "b": 3, "c": 1}, []string{"c", "a", "b"}},
		{"keys added", stored, map[string]int64{"a": 2, "b": 3, "c": 1, "e": 5, "d": 4}, []string{"c", "a", "b", "d", "e"}},
		{"a key removed", stored, map[string]int64{"b": 3, "c": 1}, []string{"c", "b"}},
		{"a key written twice", prior(kv("x", 1), kv("y", 2), kv("x", 3)), map[string]int64{"x": 3, "y": 2}, []string{"x", "y", "x"}},
		{"a key written twice, another key changed", prior(kv("x", 1), kv("y", 2), kv("x", 3)), map[string]int64{"x": 3, "y": 7}, []string{"x", "y", "x"}},
		{"a key written twice, its value changed", prior(kv("x", 1), kv("y", 2), kv("x", 3)), map[string]int64{"x": 5, "y": 2}, []string{"x", "y"}},
		{"a key written twice, its value changed to its first", prior(kv("x", 1), kv("y", 2), kv("x", 3)), map[string]int64{"x": 1, "y": 2}, []string{"x", "y"}},
		{"a new record", nil, map[string]int64{"b": 1, "a": 2}, []string{"a", "b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, err := serializeUnionOver(message(c.entries), rt, unionInner(c.prior, 1))
			if err != nil {
				t.Fatal(err)
			}
			if got := writtenKeys(t, unionInner(out, 1), 2); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("written keys %v, want %v", got, c.want)
			}
			// The bytes decode to the message, and the saved record is
			// evaluated in the order it was written.
			back := decodedRecord(t, rec, unionInner(out, 1))
			if !proto.Equal(back.Record, message(c.entries)) {
				t.Fatal("the reordered bytes decode to another message")
			}
			var want [][]any
			for _, k := range c.want {
				want = append(want, []any{k})
			}
			if got := evaluateTuples(t, NestFanOut("m", Field("key")), back); !reflect.DeepEqual(got, want) {
				t.Fatalf("evaluated %v, want %v", got, want)
			}
		})
	}

	// Each entry is written with the value it held, and an entry stored
	// without a value is written back with its default, as Java writes it.
	t.Run("the entries' values", func(t *testing.T) {
		t.Parallel()
		inner := append(append(mapEntryBytes(2, kv("x", 1)), mapEntryBytes(2, wireEntry{key: "k"})...), mapEntryBytes(2, kv("x", 3))...)
		msg := dynamicpb.NewMessage(rec)
		if err := proto.Unmarshal(inner, msg); err != nil {
			t.Fatal(err)
		}
		out, err := serializeUnionOver(msg, rt, inner)
		if err != nil {
			t.Fatal(err)
		}
		want := append(append(mapEntryBytes(2, kv("x", 1)), mapEntryBytes(2, kv("k", 0))...), mapEntryBytes(2, kv("x", 3))...)
		if got := unionInner(out, 1); !bytes.Equal(got, want) {
			t.Fatalf("written %x, want %x", got, want)
		}
	})

	t.Run("a map in a repeated message", func(t *testing.T) {
		t.Parallel()
		holder := func(entries ...wireEntry) []byte {
			var body []byte
			for _, e := range entries {
				body = append(body, mapEntryBytes(1, e)...)
			}
			return protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), body)
		}
		inner := append(holder(kv("z", 1), kv("y", 2)), holder(kv("q", 3), kv("p", 4))...)
		priorUnion := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), inner)
		msg := dynamicpb.NewMessage(rec)
		if err := proto.Unmarshal(inner, msg); err != nil {
			t.Fatal(err)
		}
		first := msg.Mutable(rec.Fields().ByName("h")).List().Get(0).Message()
		first.Mutable(first.Descriptor().Fields().ByName("hm")).Map().Set(protoreflect.ValueOfString("a").MapKey(), protoreflect.ValueOfInt64(0))
		out, err := serializeUnionOver(msg, rt, unionInner(priorUnion, 1))
		if err != nil {
			t.Fatal(err)
		}
		if got, want := writtenKeys(t, unionInner(out, 1), 3), []string{"z", "y", "a", "q", "p"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("written keys %v, want %v", got, want)
		}
	})

	// An unchanged element keeps its own stored order wherever it moved in the
	// list, as Java's element does: it is matched to the stored element with
	// its content, not to the one at its position.
	t.Run("elements of a repeated message moved", func(t *testing.T) {
		t.Parallel()
		holder := func(keys ...string) []byte {
			return protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), holderBytes(keys...))
		}
		inner := append(holder("z", "y"), holder("q", "p")...)
		h := rec.Fields().ByName("h")
		for _, c := range []struct {
			name   string
			change func(list protoreflect.List, stored []protoreflect.Message)
			want   []string
		}{
			{"one inserted before them", func(list protoreflect.List, stored []protoreflect.Message) {
				fresh := list.NewElement().Message()
				fresh.Mutable(fresh.Descriptor().Fields().ByName("hm")).Map().Set(protoreflect.ValueOfString("b").MapKey(), protoreflect.ValueOfInt64(1))
				fresh.Mutable(fresh.Descriptor().Fields().ByName("hm")).Map().Set(protoreflect.ValueOfString("a").MapKey(), protoreflect.ValueOfInt64(1))
				list.Truncate(0)
				list.Append(protoreflect.ValueOfMessage(fresh))
				for _, m := range stored {
					list.Append(protoreflect.ValueOfMessage(m))
				}
			}, []string{"a", "b", "z", "y", "q", "p"}},
			{"the first removed", func(list protoreflect.List, stored []protoreflect.Message) {
				list.Truncate(0)
				list.Append(protoreflect.ValueOfMessage(stored[1]))
			}, []string{"q", "p"}},
			{"the two swapped", func(list protoreflect.List, stored []protoreflect.Message) {
				list.Set(0, protoreflect.ValueOfMessage(stored[1]))
				list.Set(1, protoreflect.ValueOfMessage(stored[0]))
			}, []string{"q", "p", "z", "y"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				msg := dynamicpb.NewMessage(rec)
				if err := proto.Unmarshal(inner, msg); err != nil {
					t.Fatal(err)
				}
				list := msg.Mutable(h).List()
				stored := []protoreflect.Message{list.Get(0).Message(), list.Get(1).Message()}
				c.change(list, stored)
				out, err := serializeUnionOver(msg, rt, inner)
				if err != nil {
					t.Fatal(err)
				}
				if got := writtenKeys(t, unionInner(out, 1), 3); !reflect.DeepEqual(got, c.want) {
					t.Fatalf("written keys %v, want %v", got, c.want)
				}
			})
		}
	})
}

// Two members of one oneof in the bytes: the decoder keeps the member written
// last, from after the last switch, and a later occurrence of it merges; the
// map entries read back are those of the kept occurrences only, in order.
func TestMapEntriesOfAOneofMemberAreTheSurvivingOccurrences(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	member := func(num protowire.Number, entries ...wireEntry) []byte {
		var body []byte
		for _, e := range entries {
			body = append(body, mapEntryBytes(1, e)...)
		}
		return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), body)
	}
	join := func(parts ...[]byte) []byte {
		var out []byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	for _, c := range []struct {
		name string
		raw  []byte
		want [][]any
	}{
		{"a switch away and back", join(member(5, kv("z", 1)), member(6, kv("y", 2)), member(5, kv("q", 3), kv("p", 4))), [][]any{{"q"}, {"p"}}},
		{"two occurrences merged", join(member(5, kv("z", 1)), member(5, kv("y", 2))), [][]any{{"z"}, {"y"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			decoded := decodedRecord(t, rec, c.raw)
			got := evaluateTuples(t, NestFanOut("ha", NestFanOut("hm", Field("key"))), decoded)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("%v, want %v", got, c.want)
			}
		})
	}
}

// Test-side wire readers, independent of the code under test: the bodies of
// field num's occurrences in raw, and a map entry's string key and value bytes.
func occurrences(raw []byte, num protowire.Number) [][]byte {
	var out [][]byte
	for len(raw) > 0 {
		n, typ, k := protowire.ConsumeTag(raw)
		size := protowire.ConsumeFieldValue(n, typ, raw[k:])
		if n == num && typ == protowire.BytesType {
			body, _ := protowire.ConsumeBytes(raw[k : k+size])
			out = append(out, body)
		}
		raw = raw[k+size:]
	}
	return out
}

func entryKey(entry []byte) string {
	for _, b := range occurrences(entry, 1) {
		return string(b)
	}
	return ""
}

func entryValue(entry []byte) []byte {
	var v []byte
	for _, b := range occurrences(entry, 2) {
		v = b
	}
	return v
}

// keysOf is the keys of map field num's entries in raw, in order.
func keysOf(raw []byte, num protowire.Number) []string {
	var keys []string
	for _, e := range occurrences(raw, num) {
		keys = append(keys, entryKey(e))
	}
	return keys
}

// valueOfKey is the value bytes of the last entry of map field num keyed key.
func valueOfKey(raw []byte, num protowire.Number, key string) []byte {
	var v []byte
	for _, e := range occurrences(raw, num) {
		if entryKey(e) == key {
			v = entryValue(e)
		}
	}
	return v
}

func stringMapEntry(num protowire.Number, key string, value []byte) []byte {
	body := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), key)
	body = protowire.AppendBytes(protowire.AppendTag(body, 2, protowire.BytesType), value)
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), body)
}

func holderBytes(keys ...string) []byte {
	var raw []byte
	for _, k := range keys {
		raw = append(raw, mapEntryBytes(1, kv(k, 1))...)
	}
	return raw
}

// resave decodes inner as a rec, applies change, and serializes it over inner.
func resave(t *testing.T, rec protoreflect.MessageDescriptor, inner []byte, change func(protoreflect.Message)) []byte {
	t.Helper()
	msg := dynamicpb.NewMessage(rec)
	if err := proto.Unmarshal(inner, msg); err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(msg)
	}
	out, err := serializeUnionOver(msg, testRecordType(rec), inner)
	if err != nil {
		t.Fatal(err)
	}
	return unionInner(out, 1)
}

// A save keeps the stored order of maps below the top level: in a singular
// message, in a map value (each value's map by its own), and in two maps of a
// recursive type that a path of unquoted keys would name alike. A key written
// twice with message values keeps both entries while unchanged, each value's
// map in its own order; changed, it is written once, its map in the order of
// the value that stood (the last).
func TestSerializeUnionOverKeepsNestedMapOrders(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	fields := rec.Fields()

	t.Run("a map in a singular message", func(t *testing.T) {
		t.Parallel()
		inner := protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), holderBytes("z", "y"))
		out := resave(t, rec, inner, func(m protoreflect.Message) {
			ha := m.Mutable(fields.ByName("ha")).Message()
			ha.Mutable(ha.Descriptor().Fields().ByName("hm")).Map().Set(protoreflect.ValueOfString("a").MapKey(), protoreflect.ValueOfInt64(1))
		})
		got := keysOf(occurrences(out, 5)[0], 1)
		if want := []string{"z", "y", "a"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ha.hm written %v, want %v", got, want)
		}
	})

	t.Run("maps in map values", func(t *testing.T) {
		t.Parallel()
		inner := append(stringMapEntry(7, "k2", holderBytes("q", "p")), stringMapEntry(7, "k1", holderBytes("z", "y"))...)
		out := resave(t, rec, inner, func(m protoreflect.Message) {
			mh := m.Mutable(fields.ByName("mh")).Map()
			holder := fields.ByName("mh").MapValue().Message()
			k1 := mh.Mutable(protoreflect.ValueOfString("k1").MapKey()).Message()
			k1.Mutable(holder.Fields().ByName("hm")).Map().Set(protoreflect.ValueOfString("c").MapKey(), protoreflect.ValueOfInt64(1))
			k0 := dynamicpb.NewMessage(holder)
			hm := k0.Mutable(holder.Fields().ByName("hm")).Map()
			for _, k := range []string{"b", "a"} {
				hm.Set(protoreflect.ValueOfString(k).MapKey(), protoreflect.ValueOfInt64(1))
			}
			mh.Set(protoreflect.ValueOfString("k0").MapKey(), protoreflect.ValueOfMessage(k0))
		})
		if got, want := keysOf(out, 7), []string{"k2", "k1", "k0"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("mh written %v, want %v", got, want)
		}
		for key, want := range map[string][]string{"k2": {"q", "p"}, "k1": {"z", "y", "c"}, "k0": {"a", "b"}} {
			if got := keysOf(valueOfKey(out, 7, key), 1); !reflect.DeepEqual(got, want) {
				t.Errorf("mh[%s].hm written %v, want %v", key, got, want)
			}
		}
	})

	t.Run("two maps a path of unquoted keys would name alike", func(t *testing.T) {
		t.Parallel()
		// tree.kids["a"].kids["b"].kids and tree.kids[`a}/1{b`].kids, the same
		// keys in opposite orders: a map-instance path of unquoted keys names
		// both /8/1{a}/1{b}/1.
		leaf := func(keys ...string) []byte {
			var raw []byte
			for _, k := range keys {
				raw = append(raw, stringMapEntry(1, k, nil)...)
			}
			return raw
		}
		tree := append(stringMapEntry(1, "a", stringMapEntry(1, "b", leaf("z", "y"))), stringMapEntry(1, "a}/1{b", leaf("y", "z"))...)
		inner := protowire.AppendBytes(protowire.AppendTag(nil, 8, protowire.BytesType), tree)
		out := resave(t, rec, inner, nil)
		gotTree := occurrences(out, 8)[0]
		if got, want := keysOf(valueOfKey(valueOfKey(gotTree, 1, "a"), 1, "b"), 1), []string{"z", "y"}; !reflect.DeepEqual(got, want) {
			t.Errorf(`tree.kids["a"].kids["b"].kids written %v, want %v`, got, want)
		}
		if got, want := keysOf(valueOfKey(gotTree, 1, "a}/1{b"), 1), []string{"y", "z"}; !reflect.DeepEqual(got, want) {
			t.Errorf("tree.kids[`a}/1{b`].kids written %v, want %v", got, want)
		}
	})

	t.Run("a key written twice with message values", func(t *testing.T) {
		t.Parallel()
		inner := append(stringMapEntry(7, "k", holderBytes("y", "x")), stringMapEntry(7, "k", holderBytes("x", "y"))...)
		// Unchanged: both entries, each value's map in its own order.
		out := resave(t, rec, inner, nil)
		entries := occurrences(out, 7)
		if len(entries) != 2 {
			t.Fatalf("mh written %v, want [k k]", keysOf(out, 7))
		}
		for i, want := range [][]string{{"y", "x"}, {"x", "y"}} {
			if got := keysOf(entryValue(entries[i]), 1); !reflect.DeepEqual(got, want) {
				t.Errorf("mh entry %d's hm written %v, want %v", i, got, want)
			}
		}
		// Changed: once, its map in the standing (last) value's order.
		out = resave(t, rec, inner, func(m protoreflect.Message) {
			holder := fields.ByName("mh").MapValue().Message()
			k := m.Mutable(fields.ByName("mh")).Map().Mutable(protoreflect.ValueOfString("k").MapKey()).Message()
			k.Mutable(holder.Fields().ByName("hm")).Map().Set(protoreflect.ValueOfString("a").MapKey(), protoreflect.ValueOfInt64(1))
		})
		if got, want := keysOf(out, 7), []string{"k"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("mh written %v, want %v", got, want)
		}
		if got, want := keysOf(valueOfKey(out, 7, "k"), 1), []string{"x", "y", "a"}; !reflect.DeepEqual(got, want) {
			t.Fatalf(`mh["k"].hm written %v, want the standing value's order %v`, got, want)
		}
	})
}

// An occurrence of a oneof member with a wire type the member does not have is
// an unknown field to the decoder, not a switch of member: the entries of the
// member written before it still stand.
func TestMapEntriesOfAOneofSurviveAWrongWireTypeOccurrence(t *testing.T) {
	t.Parallel()
	rec := wireMapFile(t).Messages().ByName("Rec")
	raw := protowire.AppendBytes(protowire.AppendTag(nil, 5, protowire.BytesType), holderBytes("z", "y"))
	raw = protowire.AppendVarint(protowire.AppendTag(raw, 6, protowire.VarintType), 1) // hb as a varint
	decoded := decodedRecord(t, rec, raw)
	got := evaluateTuples(t, NestFanOut("ha", NestFanOut("hm", Field("key"))), decoded)
	if want := [][]any{{"z"}, {"y"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v, want %v (the stored order)", got, want)
	}
}
