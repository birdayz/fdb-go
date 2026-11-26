package rlwire

import (
	"bytes"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func wireMapHolderFile(t *testing.T) protoreflect.FileDescriptor {
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
	entry := &descriptorpb.DescriptorProto{
		Name: proto.String("HmEntry"),
		Field: []*descriptorpb.FieldDescriptorProto{
			field("key", 1, optional, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
			field("value", 2, optional, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
		},
		Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("wire_map_holder.proto"),
		Package: proto.String("wiremapholder"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name:       proto.String("Holder"),
				Field:      []*descriptorpb.FieldDescriptorProto{field("hm", 1, repeated, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".wiremapholder.Holder.HmEntry")},
				NestedType: []*descriptorpb.DescriptorProto{entry},
			},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func mapEntryBytes(fieldNum protowire.Number, key string, value int64) []byte {
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendString(body, key)
	body = protowire.AppendTag(body, 2, protowire.VarintType)
	body = protowire.AppendVarint(body, uint64(value))
	out := protowire.AppendTag(nil, fieldNum, protowire.BytesType)
	return protowire.AppendBytes(out, body)
}

func TestAppendEntryFieldMatchesTheMapMarshal(t *testing.T) {
	t.Parallel()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	type T = descriptorpb.FieldDescriptorProto_Type
	kinds := []struct {
		name string
		typ  T
		vals []protoreflect.Value
		key  bool
	}{
		{"bool", descriptorpb.FieldDescriptorProto_TYPE_BOOL, []protoreflect.Value{protoreflect.ValueOfBool(false), protoreflect.ValueOfBool(true)}, true},
		{"enum", descriptorpb.FieldDescriptorProto_TYPE_ENUM, []protoreflect.Value{protoreflect.ValueOfEnum(0), protoreflect.ValueOfEnum(1), protoreflect.ValueOfEnum(-1)}, false},
		{"int32", descriptorpb.FieldDescriptorProto_TYPE_INT32, []protoreflect.Value{protoreflect.ValueOfInt32(0), protoreflect.ValueOfInt32(-5)}, true},
		{"int64", descriptorpb.FieldDescriptorProto_TYPE_INT64, []protoreflect.Value{protoreflect.ValueOfInt64(0), protoreflect.ValueOfInt64(-5)}, true},
		{"uint32", descriptorpb.FieldDescriptorProto_TYPE_UINT32, []protoreflect.Value{protoreflect.ValueOfUint32(0), protoreflect.ValueOfUint32(1 << 31)}, true},
		{"uint64", descriptorpb.FieldDescriptorProto_TYPE_UINT64, []protoreflect.Value{protoreflect.ValueOfUint64(0), protoreflect.ValueOfUint64(1 << 63)}, true},
		{"sint32", descriptorpb.FieldDescriptorProto_TYPE_SINT32, []protoreflect.Value{protoreflect.ValueOfInt32(0), protoreflect.ValueOfInt32(-5)}, true},
		{"sint64", descriptorpb.FieldDescriptorProto_TYPE_SINT64, []protoreflect.Value{protoreflect.ValueOfInt64(0), protoreflect.ValueOfInt64(-5)}, true},
		{"fixed32", descriptorpb.FieldDescriptorProto_TYPE_FIXED32, []protoreflect.Value{protoreflect.ValueOfUint32(0), protoreflect.ValueOfUint32(7)}, true},
		{"sfixed32", descriptorpb.FieldDescriptorProto_TYPE_SFIXED32, []protoreflect.Value{protoreflect.ValueOfInt32(0), protoreflect.ValueOfInt32(-5)}, true},
		{"float", descriptorpb.FieldDescriptorProto_TYPE_FLOAT, []protoreflect.Value{protoreflect.ValueOfFloat32(0), protoreflect.ValueOfFloat32(-1.5)}, false},
		{"fixed64", descriptorpb.FieldDescriptorProto_TYPE_FIXED64, []protoreflect.Value{protoreflect.ValueOfUint64(0), protoreflect.ValueOfUint64(7)}, true},
		{"sfixed64", descriptorpb.FieldDescriptorProto_TYPE_SFIXED64, []protoreflect.Value{protoreflect.ValueOfInt64(0), protoreflect.ValueOfInt64(-5)}, true},
		{"double", descriptorpb.FieldDescriptorProto_TYPE_DOUBLE, []protoreflect.Value{protoreflect.ValueOfFloat64(0), protoreflect.ValueOfFloat64(-1.5)}, false},
		{"string", descriptorpb.FieldDescriptorProto_TYPE_STRING, []protoreflect.Value{protoreflect.ValueOfString(""), protoreflect.ValueOfString("s")}, true},
		{"bytes", descriptorpb.FieldDescriptorProto_TYPE_BYTES, []protoreflect.Value{protoreflect.ValueOfBytes(nil), protoreflect.ValueOfBytes([]byte("b"))}, false},
	}
	msg := &descriptorpb.DescriptorProto{Name: proto.String("Rec")}
	field := func(name string, number int32, typ T, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: optional, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	number := int32(1)
	addMap := func(name string, keyTyp, valueTyp T) {
		entry := string(name[0]-'a'+'A') + name[1:] + "Entry"
		typeName := func(typ T) string {
			if typ == descriptorpb.FieldDescriptorProto_TYPE_ENUM {
				return ".entrykinds.E"
			}
			return ""
		}
		msg.NestedType = append(msg.NestedType, &descriptorpb.DescriptorProto{
			Name:    proto.String(entry),
			Field:   []*descriptorpb.FieldDescriptorProto{field("key", 1, keyTyp, typeName(keyTyp)), field("value", 2, valueTyp, typeName(valueTyp))},
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
		})
		f := field(name, number, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".entrykinds.Rec."+entry)
		f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		msg.Field = append(msg.Field, f)
		number++
	}
	for _, k := range kinds {
		addMap("v"+k.name, descriptorpb.FieldDescriptorProto_TYPE_INT32, k.typ)
		if k.key {
			addMap("k"+k.name, k.typ, descriptorpb.FieldDescriptorProto_TYPE_INT32)
		}
	}
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("entry_kinds.proto"), Package: proto.String("entrykinds"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{msg},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("E"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("Z"), Number: proto.Int32(0)},
				{Name: proto.String("O"), Number: proto.Int32(1)},
				{Name: proto.String("N"), Number: proto.Int32(-1)},
			},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := file.Messages().ByName("Rec")
	check := func(fd protoreflect.FieldDescriptor, key protoreflect.MapKey, value protoreflect.Value) {
		t.Helper()
		m := dynamicpb.NewMessage(rec)
		m.Mutable(fd).Map().Set(key, value)
		marshaled, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		_, _, n := protowire.ConsumeTag(marshaled)
		want, _ := protowire.ConsumeBytes(marshaled[n:])
		got, err := AppendEntryField(nil, fd.MapKey(), key.Value())
		if err != nil {
			t.Fatal(err)
		}
		if got, err = AppendEntryField(got, fd.MapValue(), value); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s %v=%v: AppendEntryField wrote %x, the map marshal %x", fd.Name(), key, value, got, want)
		}
	}
	checked := 0
	for _, k := range kinds {
		for _, v := range k.vals {
			check(rec.Fields().ByName(protoreflect.Name("v"+k.name)), protoreflect.ValueOfInt32(3).MapKey(), v)
			checked++
			if k.key {
				check(rec.Fields().ByName(protoreflect.Name("k"+k.name)), v.MapKey(), protoreflect.ValueOfInt32(3))
				checked++
			}
		}
	}
	if checked != 57 {
		t.Fatalf("checked %d entries, want 57", checked)
	}
}

func testPartialUnmarshal(b []byte, m proto.Message) error {
	return proto.UnmarshalOptions{
		RecursionLimit: 100,
		Resolver:       new(protoregistry.Types),
		AllowPartial:   true,
	}.Unmarshal(b, m)
}

func TestElementPriorsMatching(t *testing.T) {
	t.Parallel()
	holder := wireMapHolderFile(t).Messages().ByName("Holder")
	body := func(keys ...string) []byte {
		var b []byte
		for _, k := range keys {
			b = append(b, mapEntryBytes(1, k, 1)...)
		}
		return b
	}
	canonical := func(b []byte) []byte {
		m := dynamicpb.NewMessage(holder)
		if err := proto.Unmarshal(b, m); err != nil {
			t.Fatal(err)
		}
		out, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a1, a2, b := body("y", "x"), body("x", "y"), body("z")

	current := [][]byte{canonical(b), canonical(a2)}
	for _, c := range []struct {
		cells int
		want  [][]byte
	}{
		{MaxLCSCells, [][]byte{b, a2}},
		{0, [][]byte{b, a1}},
	} {
		got, err := ElementPriorsWithin(holder, current, [][]byte{a1, b, a2}, c.cells, testPartialUnmarshal)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("cells %d: priors %x, want %x", c.cells, got, c.want)
		}
	}

	stored := body("y", "x")
	got, err := ElementPriors(holder, [][]byte{canonical(stored), canonical(stored)}, [][]byte{stored}, testPartialUnmarshal)
	if err != nil {
		t.Fatal(err)
	}
	if want := [][]byte{stored, nil}; !reflect.DeepEqual(got, want) {
		t.Errorf("an inserted equal element: priors %x, want %x", got, want)
	}
}

func TestNilPartialUnmarshalReturnsError(t *testing.T) {
	t.Parallel()
	holder := wireMapHolderFile(t).Messages().ByName("Holder")
	raw := mapEntryBytes(1, "k", 1)
	m := dynamicpb.NewMessage(holder)
	if err := testPartialUnmarshal(raw, m); err != nil {
		t.Fatal(err)
	}
	reach := NewMapReach(holder)
	if _, err := ElementPriorsWithin(holder, [][]byte{raw}, [][]byte{raw}, MaxLCSCells, nil); err == nil {
		t.Fatal("ElementPriorsWithin accepted nil partialUnmarshal")
	}
	if _, err := RewriteMaps(holder, raw, raw, reach, nil); err == nil {
		t.Fatal("RewriteMaps accepted nil partialUnmarshal")
	}
	if _, err := MarshalMapRecord(m, raw, reach, nil); err == nil {
		t.Fatal("MarshalMapRecord accepted nil partialUnmarshal")
	}
	w := NewRecordWire(true, reach, raw, nil)
	if _, ok := w.MapEntries(m, m.ProtoReflect(), holder.Fields().ByName("hm")); ok || w.err == nil {
		t.Fatalf("RecordWire.MapEntries accepted nil partialUnmarshal (ok=%t, err=%v)", ok, w.err)
	}
}
