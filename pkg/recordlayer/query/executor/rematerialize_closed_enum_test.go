package executor

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The INSERT…SELECT copy of a composite value into the target column's own
// message type reads the source as Java reads it (RFC-257 ws-j-design.md 4g and
// 4h). A source row holding a closed enum's undeclared number keeps it as an
// unknown field (the read of Java-written bytes); the copy re-marshals it and
// re-parses it into the target type, where protobuf-go's own parse would put
// the number back into the field. The copy must leave the field unset and the
// number unknown, as the source was read. The stored bytes are the same either
// way (the save rewrites a held number as Java writes it, pinned by the
// conformance spec "a record Go holds with an undeclared number is saved,
// updated and deleted as Java reads it"); this pins the copy's own output.
func TestRematerialize_ClosedEnumUndeclaredNumberCopiesAsRead(t *testing.T) {
	t.Parallel()
	opt := func(name string, n int32, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(n),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: typ.Enum(),
		}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	enumField := func() *descriptorpb.FieldDescriptorProto {
		return opt("color", 2, descriptorpb.FieldDescriptorProto_TYPE_ENUM, ".remat.Color")
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("remat_closed_enum_test.proto"), Package: proto.String("remat"),
		Syntax: proto.String("proto2"), // proto2: Color is closed
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Color"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("RED"), Number: proto.Int32(1)},
				{Name: proto.String("BLUE"), Number: proto.Int32(2)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Src"), Field: []*descriptorpb.FieldDescriptorProto{enumField()}},
			{Name: proto.String("Dst"), Field: []*descriptorpb.FieldDescriptorProto{enumField()}},
			{Name: proto.String("Row"), Field: []*descriptorpb.FieldDescriptorProto{
				opt("d", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".remat.Dst"),
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := dynamicpb.NewMessage(fd.Messages().ByName("Src"))
	undeclared := protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 9)
	src.SetUnknown(undeclared) // the number 9, read as protobuf-java reads it

	column := fd.Messages().ByName("Row").Fields().ByName("d")
	got, err := rematerializeProtoScalar(column, protoreflect.ValueOfMessage(src))
	if err != nil {
		t.Fatal(err)
	}
	dst := got.Message()
	if dst.Descriptor().FullName() != "remat.Dst" {
		t.Fatalf("copied into %s, want the column's type remat.Dst", dst.Descriptor().FullName())
	}
	color := dst.Descriptor().Fields().ByName("color")
	if dst.Has(color) {
		t.Errorf("the copy holds color %d; the source read it unset, as Java does", dst.Get(color).Enum())
	}
	if !bytes.Equal(dst.GetUnknown(), undeclared) {
		t.Errorf("the copy's unknown fields are %x, want the number kept as %x", dst.GetUnknown(), undeclared)
	}
}
