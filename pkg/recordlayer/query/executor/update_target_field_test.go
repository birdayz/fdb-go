package executor

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestUpdateTargetField drives the executor's field-path walk over Order
// (order_id, flower{type, color}, …): a column, a field of a NULL struct
// (created, as Java's transformMessage builds the nested message), and each
// refusal, which must be loud rather than an assignment to whatever field an
// ordinal names.
func TestUpdateTargetField(t *testing.T) {
	t.Parallel()
	tr := func(names []string, ordinals ...int) expressions.UpdateTransform {
		return expressions.UpdateTransform{FieldNames: names, FieldOrdinals: ordinals}
	}
	msg := (&gen.Order{}).ProtoReflect()
	owner, fd, err := updateTargetField(msg, tr([]string{"price"}, 2))
	if err != nil || owner != msg || string(fd.Name()) != "price" {
		t.Fatalf("a column: %v, %v", fd, err)
	}
	if msg.Has(msg.Descriptor().Fields().ByName("flower")) {
		t.Fatal("flower set before the walk")
	}
	owner, fd, err = updateTargetField(msg, tr([]string{"flower", "color"}, 1, 1))
	if err != nil || string(fd.Name()) != "color" || string(owner.Descriptor().Name()) != "Flower" {
		t.Fatalf("a field of a NULL struct: %v, %v", fd, err)
	}
	if !msg.Has(msg.Descriptor().Fields().ByName("flower")) {
		t.Error("the NULL struct was not created")
	}
	for _, c := range []struct {
		name string
		tr   expressions.UpdateTransform
		want string
	}{
		{"no path", tr(nil), "0 ordinals for 0 names"},
		{"an ordinal outside", tr([]string{"x"}, 99), "ordinal 99 outside"},
		{"another field at the ordinal", tr([]string{"quantity"}, 2), `at ordinal 2 is field "price"`},
		{"a field of a scalar", tr([]string{"price", "x"}, 2, 0), "is not a struct"},
		{"a field of an array", tr([]string{"tags", "x"}, 3, 0), "is not a struct"},
	} {
		if _, _, err := updateTargetField((&gen.Order{}).ProtoReflect(), c.tr); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error containing %q", c.name, err, c.want)
		}
	}
}

// TestUpdateTargetFieldDescendsOnlyAStruct pins that the walk descends a
// struct and nothing else that is a proto message: a UUID column and a
// nullable array's wrapper message are messages on the wire and not structs
// in SQL, so a path through either is refused, as the resolver and the plan
// refuse it.
func TestUpdateTargetFieldDescendsOnlyAStruct(t *testing.T) {
	t.Parallel()
	msgField := func(name string, num int32, typ string) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:  descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(typ),
		}
	}
	scalar := func(name string, num int32, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num), Label: label.Enum(),
			Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
		}
	}
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("update_target_field_descend.proto"), Package: proto.String("com.apple.foundationdb.record"),
		Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("UUID"), Field: []*descriptorpb.FieldDescriptorProto{
				scalar("most_significant_bits", 1, opt), scalar("least_significant_bits", 2, opt),
			}},
			{Name: proto.String("Wrapper"), Field: []*descriptorpb.FieldDescriptorProto{
				scalar(values.WrappedArrayValuesFieldName, 1, descriptorpb.FieldDescriptorProto_LABEL_REPEATED),
			}},
			{Name: proto.String("S"), Field: []*descriptorpb.FieldDescriptorProto{scalar("x", 1, opt)}},
			{Name: proto.String("R"), Field: []*descriptorpb.FieldDescriptorProto{
				msgField("u", 1, ".com.apple.foundationdb.record.UUID"),
				msgField("a", 2, ".com.apple.foundationdb.record.Wrapper"),
				msgField("s", 3, ".com.apple.foundationdb.record.S"),
			}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := file.Messages().ByName("R")
	tr := func(names []string, ordinals ...int) expressions.UpdateTransform {
		return expressions.UpdateTransform{FieldNames: names, FieldOrdinals: ordinals}
	}
	// The control: the same walk descends the struct.
	if _, fd, err := updateTargetField(dynamicpb.NewMessage(record), tr([]string{"s", "x"}, 2, 0)); err != nil || string(fd.Name()) != "x" {
		t.Fatalf("a field of a struct: %v, %v", fd, err)
	}
	for _, c := range []struct {
		name string
		tr   expressions.UpdateTransform
	}{
		{"a field of a UUID", tr([]string{"u", "most_significant_bits"}, 0, 0)},
		{"a field of a nullable array's wrapper", tr([]string{"a", values.WrappedArrayValuesFieldName}, 1, 0)},
	} {
		if _, _, err := updateTargetField(dynamicpb.NewMessage(record), c.tr); err == nil || !strings.Contains(err.Error(), "is not a struct") {
			t.Errorf("%s: %v, want \"is not a struct\"", c.name, err)
		}
	}
}
