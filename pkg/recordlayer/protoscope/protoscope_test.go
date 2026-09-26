package protoscope

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func enumOf(name string, values ...string) *descriptorpb.EnumDescriptorProto {
	e := &descriptorpb.EnumDescriptorProto{Name: proto.String(name)}
	for i, v := range values {
		e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v), Number: proto.Int32(int32(i))})
	}
	return e
}

func enumField(name string, n int32, typeName string) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name: proto.String(name), Number: proto.Int32(n), TypeName: proto.String(typeName),
		Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
	}
}

// A file with no collision is not touched, byte for byte: the rewrite exists
// only for files protoc would refuse.
func TestScopeLeavesACleanFileAlone(t *testing.T) {
	t.Parallel()
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("clean.proto"), Package: proto.String("p"), Syntax: proto.String("proto2"),
		EnumType:    []*descriptorpb.EnumDescriptorProto{enumOf("E1", "A", "B"), enumOf("E2", "C")},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("M"), Field: []*descriptorpb.FieldDescriptorProto{enumField("f", 1, ".p.E1")}}},
	}
	before := proto.Clone(fdp)
	ScopeEnumValuesAsJava(fdp)
	if !proto.Equal(before, fdp) {
		t.Fatalf("a file with no colliding value was rewritten:\n%v", fdp)
	}
}

// A nil file is left to the caller's build to refuse: the record layer's
// metadata load passes a metadata with no records file through here, and its
// refusal ("new metadata does not build") must still be the answer.
func TestScopeLeavesANilFileToTheCaller(t *testing.T) {
	t.Parallel()
	ScopeEnumValuesAsJava(nil)
}

// Every collision protobuf-go refuses and protobuf-java accepts, in a package,
// at file level and inside a message, against another enum's value and against
// a message name, referenced from a field, a nested field and an extension:
// the rewritten file builds, each field's enum keeps its own values, and
// JavaFullName answers the name Java reads.
func TestScopeBuildsWhatJavaAccepts(t *testing.T) {
	t.Parallel()
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("shared.proto"), Package: proto.String("p"), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			enumOf("E1", "X", "Y"),
			enumOf("E2", "Y", "Z"),
			enumOf("E3", "M"), // M is also a message of this scope
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("M"),
				Field: []*descriptorpb.FieldDescriptorProto{
					enumField("a", 1, ".p.E1"), enumField("b", 2, ".p.E2"), enumField("c", 3, ".p.E3"),
					enumField("d", 4, ".p.M.N1"), enumField("e", 5, ".p.M.N2"),
				},
				EnumType:       []*descriptorpb.EnumDescriptorProto{enumOf("N1", "K"), enumOf("N2", "K")},
				ExtensionRange: []*descriptorpb.DescriptorProto_ExtensionRange{{Start: proto.Int32(100), End: proto.Int32(200)}},
			},
		},
		Extension: []*descriptorpb.FieldDescriptorProto{{
			Name: proto.String("x"), Number: proto.Int32(100), Extendee: proto.String(".p.M"), TypeName: proto.String(".p.E2"),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
		}},
	}
	if _, err := protodesc.NewFile(proto.Clone(fdp).(*descriptorpb.FileDescriptorProto), nil); err == nil {
		t.Fatal("the fixture builds without the rewrite, so it tests nothing")
	}
	ScopeEnumValuesAsJava(fdp)
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("the rewritten file does not build: %v", err)
	}
	m := fd.Messages().ByName("M")
	for field, want := range map[string]struct {
		java   string
		values []string
	}{
		"a": {"p.E1", []string{"X", "Y"}}, "b": {"p.E2", []string{"Y", "Z"}}, "c": {"p.E3", []string{"M"}},
		"d": {"p.M.N1", []string{"K"}}, "e": {"p.M.N2", []string{"K"}},
	} {
		ed := m.Fields().ByName(protoreflectName(field)).Enum()
		if got := string(JavaFullName(ed)); got != want.java {
			t.Errorf("field %s: JavaFullName %s, want %s", field, got, want.java)
		}
		if ed.Values().Len() != len(want.values) {
			t.Fatalf("field %s: %d values, want %v", field, ed.Values().Len(), want.values)
		}
		for i, v := range want.values {
			if got := ed.Values().Get(i); string(got.Name()) != v || int(got.Number()) != i {
				t.Errorf("field %s value %d: %s=%d, want %s=%d", field, i, got.Name(), got.Number(), v, i)
			}
		}
	}
	if got := string(JavaFullName(fd.Extensions().ByName("x").Enum())); got != "p.E2" {
		t.Errorf("the extension's enum reads as %s, want p.E2", got)
	}
}

func protoreflectName(s string) protoreflect.Name { return protoreflect.Name(s) }
