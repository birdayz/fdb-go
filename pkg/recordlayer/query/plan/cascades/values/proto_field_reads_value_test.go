package values

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestProtoFieldByNameReadsAsAQuery pins protoFieldByName to a query's read of
// a record (ProtoFieldReadsValue): a proto3 field at its default reads NULL, an
// unset proto2 field with an explicit default reads NULL too (Java's query
// reads a copy of the record in the plan's type, which declares no default),
// and a repeated field reads its list. The live-JVM comparisons are
// conformance/null_standin_conformance_test.go (the readers) and "WS-J an
// unset field with a declared default reads as the target reads it" (SQL).
func TestProtoFieldByNameReadsAsAQuery(t *testing.T) {
	t.Parallel()
	message := func(t *testing.T, syntax string) protoreflect.MessageDescriptor {
		t.Helper()
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
		field := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
			return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: label, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}
		}
		repeated := field("r", 3)
		repeated.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		fields := []*descriptorpb.FieldDescriptorProto{field("a", 1), repeated}
		if syntax == "proto2" {
			withDefault := field("d", 2)
			withDefault.DefaultValue = proto.String("7")
			fields = append(fields, withDefault)
		}
		fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name: proto.String("reads_value_" + syntax + ".proto"), Package: proto.String("readsvalue"), Syntax: proto.String(syntax),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Rec"), Field: fields}},
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return fd.Messages().ByName("Rec")
	}
	for _, c := range []struct {
		syntax string
		set    map[string]int64
		field  string
		want   any
	}{
		{"proto2", nil, "a", nil},
		{"proto2", map[string]int64{"a": 0}, "a", int64(0)},
		{"proto2", nil, "d", nil},
		{"proto2", map[string]int64{"d": 0}, "d", int64(0)},
		{"proto3", nil, "a", nil},
		{"proto3", map[string]int64{"a": 0}, "a", nil},
		{"proto3", map[string]int64{"a": 5}, "a", int64(5)},
	} {
		desc := message(t, c.syntax)
		msg := dynamicpb.NewMessage(desc)
		for name, v := range c.set {
			msg.Set(desc.Fields().ByName(protoreflect.Name(name)), protoreflect.ValueOfInt64(v))
		}
		got, found := protoFieldByName(msg, c.field)
		if !found || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s %v: protoFieldByName(%q) = (%#v, %t), want (%#v, true)", c.syntax, c.set, c.field, got, found, c.want)
		}
	}
	for _, syntax := range []string{"proto2", "proto3"} {
		got, found := protoFieldByName(dynamicpb.NewMessage(message(t, syntax)), "r")
		if list, ok := got.([]any); !found || !ok || len(list) != 0 {
			t.Fatalf("%s: an empty repeated field read (%#v, %t), want the empty list", syntax, got, found)
		}
	}
}
