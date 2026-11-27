package values

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// TestFieldTypeNullabilityIsJavas: Java types a field that is not an array
// nullable unless it is required (Type.Record.Field.fromDescriptor,
// `!isRequired()`), whatever its presence: a proto3 scalar without explicit
// presence is nullable, as is a proto3 `optional` and a proto2 `optional`; a
// proto2 `required` is not. A repeated field is a non-nullable array.
func TestFieldTypeNullabilityIsJavas(t *testing.T) {
	t.Parallel()
	file := func(syntax string, fields ...*descriptorpb.FieldDescriptorProto) protoreflect.MessageDescriptor {
		t.Helper()
		message := &descriptorpb.DescriptorProto{Name: proto.String("M"), Field: fields}
		for _, f := range fields {
			if f.GetProto3Optional() {
				message.OneofDecl = []*descriptorpb.OneofDescriptorProto{{Name: proto.String("_" + f.GetName())}}
			}
		}
		f, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name: proto.String("nullability_" + syntax + ".proto"), Package: proto.String("nullability" + syntax),
			Syntax:      proto.String(syntax),
			MessageType: []*descriptorpb.DescriptorProto{message},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return f.Messages().ByName("M")
	}
	field := func(name string, number int32, label descriptorpb.FieldDescriptorProto_Label, optional bool) *descriptorpb.FieldDescriptorProto {
		fd := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(number), Label: label.Enum(),
			Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
		}
		if optional {
			fd.Proto3Optional = proto.Bool(true)
			fd.OneofIndex = proto.Int32(0)
		}
		return fd
	}
	p3 := file("proto3",
		field("implicit", 1, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL, false),
		field("repeated", 2, descriptorpb.FieldDescriptorProto_LABEL_REPEATED, false),
		field("explicit", 3, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL, true),
	)
	p2 := file("proto2",
		field("optional", 1, descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL, false),
		field("required", 2, descriptorpb.FieldDescriptorProto_LABEL_REQUIRED, false),
	)
	for _, c := range []struct {
		name     string
		fd       protoreflect.FieldDescriptor
		nullable bool
	}{
		{"a proto3 scalar without explicit presence", p3.Fields().ByName("implicit"), true},
		{"a proto3 repeated field (an array)", p3.Fields().ByName("repeated"), false},
		{"a proto3 optional", p3.Fields().ByName("explicit"), true},
		{"a proto2 optional", p2.Fields().ByName("optional"), true},
		{"a proto2 required", p2.Fields().ByName("required"), false},
	} {
		if c.fd == nil {
			t.Fatalf("%s: no such field", c.name)
		}
		if got := FieldTypeForProtoField(c.fd); got.IsNullable() != c.nullable {
			t.Errorf("%s: %v nullable %v, want %v", c.name, got, got.IsNullable(), c.nullable)
		}
	}
	// The array's element is NOT NULL too: a repeated field never holds a NULL.
	if array, ok := FieldTypeForProtoField(p3.Fields().ByName("repeated")).(*ArrayType); !ok || array.ElementType == nil || array.ElementType.IsNullable() {
		t.Errorf("the repeated field's element: %v, want a NOT NULL element", FieldTypeForProtoField(p3.Fields().ByName("repeated")))
	}
}
