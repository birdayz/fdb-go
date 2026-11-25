package executor

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// PositionalTypeForDescriptor must carry the DECLARED column type on every
// field, not a placeholder.
//
// This is the axis that was unprobed, and it is unprobed-ness — not absence of
// tests — that let a wrong-answer bug ship. The function had thorough coverage
// of the two properties it was written for (names are UPPER-cased, ordinals are
// declaration order) and none at all of the third property every type-directed
// consumer silently assumes: that the FieldType means something. Every field
// came back UnknownType, so a planner rule asking "is this column a DOUBLE?"
// against a sargable match candidate's layout got "cannot tell" and fell
// through to its permissive default — while the same rule, asking the same
// question about the same table via the plain full-scan leaf (which types its
// fields), got "yes". The ordering-claim termination therefore fired on
// `ORDER BY d` and did NOT fire on `WHERE d > 5.0 ORDER BY d`.
//
// The declared types are asserted for every kind the mapping resolves, so the
// test fails on a partial revert as loudly as on a total one.
func TestPositionalTypeForDescriptorCarriesDeclaredFieldTypes(t *testing.T) {
	t.Parallel()

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("positional_field_types_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("ptftest"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Row"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
				{Name: proto.String("n32"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()},
				{Name: proto.String("d"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()},
				{Name: proto.String("e"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_FLOAT.Enum()},
				{Name: proto.String("s"), Number: proto.Int32(5), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("b"), Number: proto.Int32(6), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()},
				{Name: proto.String("flag"), Number: proto.Int32(7), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()},
				// Repeated: the slot holds an exact non-null ARRAY whose element
				// is the declared non-null DOUBLE. It must not collapse either to
				// the scalar DOUBLE or to an Unknown placeholder.
				{Name: proto.String("ds"), Number: proto.Int32(8), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build synthetic file descriptor: %v", err)
	}
	desc := fd.Messages().Get(0)

	rt := PositionalTypeForDescriptor(desc)
	if rt == nil {
		t.Fatal("PositionalTypeForDescriptor returned nil")
	}
	want := []struct {
		name string
		code values.TypeCode
	}{
		{"id", values.TypeCodeLong},
		{"n32", values.TypeCodeInt},
		{"d", values.TypeCodeDouble},
		{"e", values.TypeCodeFloat},
		{"s", values.TypeCodeString},
		{"b", values.TypeCodeBytes},
		{"flag", values.TypeCodeBoolean},
		{"ds", values.TypeCodeArray},
	}
	if len(rt.Fields) != len(want) {
		t.Fatalf("layout has %d fields, want %d: %s", len(rt.Fields), len(want), rt.String())
	}
	for i, w := range want {
		f := rt.Fields[i]
		if f.Name != w.name || f.Ordinal != i {
			t.Errorf("field %d = {Name:%q Ordinal:%d}, want {Name:%q Ordinal:%d}", i, f.Name, f.Ordinal, w.name, i)
		}
		if f.FieldType == nil || f.FieldType.Code() != w.code {
			t.Errorf("field %q typed %v, want %v — a stored column's layout must carry its "+
				"declared type; UnknownType here makes every type-directed planner rule "+
				"fall through to its permissive default on the sargable access path while "+
				"the full-scan path decides the opposite (layout: %s)",
				f.Name, f.FieldType, w.code, rt.String())
		}
	}
	ds, ok := rt.Fields[7].FieldType.(*values.ArrayType)
	if !ok || ds == nil || ds.IsNullable() || ds.ElementType == nil ||
		ds.ElementType.Code() != values.TypeCodeDouble || ds.ElementType.IsNullable() {
		t.Fatalf("field DS typed %v, want ARRAY<DOUBLE NOT NULL> NOT NULL", rt.Fields[7].FieldType)
	}

	// The layout must be identical to the one the plain full-scan leaf derives,
	// because both describe the same stored columns and feed the same planner
	// decisions. Asserting equality against the single authority is what stops
	// the two from drifting apart again.
	for i := 0; i < desc.Fields().Len(); i++ {
		fd := desc.Fields().Get(i)
		got := rt.Fields[i].FieldType
		auth := values.FieldTypeForProtoField(fd)
		if got == nil || auth == nil || got.Code() != auth.Code() {
			t.Errorf("field %q: layout says %v, values.FieldTypeForProtoField says %v — "+
				"the row layout must not derive column types independently of the single authority",
				rt.Fields[i].Name, got, auth)
		}
	}
}

// A nil descriptor field must not panic, and must be Unknown rather than
// silently claiming a type.
func TestFieldTypeForProtoFieldNilIsUnknown(t *testing.T) {
	t.Parallel()
	var fd protoreflect.FieldDescriptor
	if got := values.FieldTypeForProtoField(fd); got == nil || got.Code() != values.TypeCodeUnknown {
		t.Fatalf("FieldTypeForProtoField(nil) = %v, want UNKNOWN", got)
	}
}
