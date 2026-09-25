package recordlayer

import (
	"bytes"
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
)

func varintField(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}

// protobuf-go keeps a closed enum's undeclared number in the field;
// closedEnumsAsJava moves it where protobuf-java's parser puts it, at every
// kind of field that can hold one, and in the unknown-field order Java writes.
func TestClosedEnumsAsJava(t *testing.T) {
	t.Parallel()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	enum := descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum()
	message := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("closed_enums.proto"), Package: proto.String("ce"), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{Name: proto.String("E"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("A"), Number: proto.Int32(1)}, {Name: proto.String("B"), Number: proto.Int32(2)},
			}},
			// A map's enum value type must declare 0 first.
			{Name: proto.String("F"), Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("F0"), Number: proto.Int32(0)}, {Name: proto.String("F1"), Number: proto.Int32(1)},
			}},
		},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("M"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("e"), Number: proto.Int32(1), Label: optional, Type: enum, TypeName: proto.String(".ce.E")},
				{Name: proto.String("r"), Number: proto.Int32(2), Label: repeated, Type: enum, TypeName: proto.String(".ce.E")},
				{Name: proto.String("child"), Number: proto.Int32(3), Label: optional, Type: message, TypeName: proto.String(".ce.M")},
				{Name: proto.String("m"), Number: proto.Int32(4), Label: repeated, Type: message, TypeName: proto.String(".ce.M.MEntry")},
				{Name: proto.String("x"), Number: proto.Int32(5), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
			},
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("MEntry"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("key"), Number: proto.Int32(1), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
					{Name: proto.String("value"), Number: proto.Int32(2), Label: optional, Type: enum, TypeName: proto.String(".ce.F")},
				},
				Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			}},
		}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	md := fd.Messages().ByName("M")
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	entry := protowire.AppendBytes(protowire.AppendTag(nil, 4, protowire.BytesType),
		cat(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "k"), varintField(2, 7)))
	child := protowire.AppendBytes(protowire.AppendTag(nil, 3, protowire.BytesType), varintField(1, 9))
	// e 7, r [A, 8, B], child.e 9, m {"k": 7}, x 5, and unknown fields 1 (as
	// a fixed32, a wire type the field does not take) and 30.
	raw := cat(varintField(1, 7), varintField(2, 1), varintField(2, 8), varintField(2, 2), child, entry, varintField(5, 5),
		protowire.AppendFixed32(protowire.AppendTag(nil, 1, protowire.Fixed32Type), 4), varintField(30, 1))
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(raw, m); err != nil {
		t.Fatal(err)
	}
	if got := m.Get(md.Fields().ByName("e")).Enum(); got != 7 {
		t.Fatalf("protobuf-go's parse no longer keeps the number in the field (e=%d); revisit proto_closed_enums.go", got)
	}
	closedEnumsAsJava(m, newClosedEnumReach(md))

	if m.Has(md.Fields().ByName("e")) {
		t.Error("e is still set")
	}
	r := m.Get(md.Fields().ByName("r")).List()
	if r.Len() != 2 || r.Get(0).Enum() != 1 || r.Get(1).Enum() != 2 {
		t.Errorf("r keeps its declared elements in order; got %d elements", r.Len())
	}
	c := m.Get(md.Fields().ByName("child")).Message()
	if c.Has(md.Fields().ByName("e")) || !bytes.Equal(c.GetUnknown(), varintField(1, 9)) {
		t.Errorf("child: e set %t, unknown %x", c.Has(md.Fields().ByName("e")), c.GetUnknown())
	}
	mp := m.Get(md.Fields().ByName("m")).Map()
	if v := mp.Get(protoreflect.ValueOfString("k").MapKey()); v.Enum() != 0 {
		t.Errorf("the map value reads the default F0, got %d", v.Enum())
	}
	// Java's UnknownFieldSet order: by number, a field's varints before its
	// fixed32s.
	want := cat(varintField(1, 7), protowire.AppendFixed32(protowire.AppendTag(nil, 1, protowire.Fixed32Type), 4), varintField(2, 8), varintField(30, 1))
	if got := m.GetUnknown(); !bytes.Equal(got, want) {
		t.Errorf("unknown fields %x, want %x", got, want)
	}

	// A message with nothing to move is left as it is.
	plain := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(cat(varintField(1, 2), varintField(30, 1)), plain); err != nil {
		t.Fatal(err)
	}
	closedEnumsAsJava(plain, nil)
	if plain.Get(md.Fields().ByName("e")).Enum() != 2 || !bytes.Equal(plain.GetUnknown(), varintField(30, 1)) {
		t.Error("a declared number was moved")
	}
}

// The stored protos Go decodes read as Java reads them: a store header whose
// record-count state Java cannot read is its default, READABLE, and a key
// expression's fan type 7 is a missing fan type.
func TestStoredProtosReadAsJava(t *testing.T) {
	t.Parallel()
	var header gen.DataStoreInfo
	if err := unmarshalVTAsJava(&header, varintField(9, 7)); err != nil {
		t.Fatal(err)
	}
	if header.RecordCountState != nil || header.GetRecordCountState() != gen.DataStoreInfo_READABLE {
		t.Errorf("record count state %v, want unset (READABLE)", header.GetRecordCountState())
	}
	if !bytes.Equal(header.ProtoReflect().GetUnknown(), varintField(9, 7)) {
		t.Errorf("the number is kept as an unknown field: %x", header.ProtoReflect().GetUnknown())
	}

	field := append(protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), "f"), varintField(2, 7)...)
	var f gen.Field
	if err := proto.Unmarshal(field, &f); err != nil {
		t.Fatal(err)
	}
	_, err := KeyExpressionFromProto(&gen.KeyExpression{Field: &f})
	var de *KeyExpressionDeserializationError
	if !errors.As(err, &de) || de.Message != "Serialized Field is missing fan type" {
		t.Errorf("fan type 7 through protobuf-go's parse: %v", err)
	}
}
