package values

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/recordlayer/protoname"
)

// TestProtoFieldByNameResolvesEscapedNames pins the reverse of the descriptor
// emitter's name escaping.
//
// Every descriptor this engine emits from a SQL identifier runs the name
// through protoname.ToProtoBufCompliantName, which does not merely change
// CASE — it REWRITES characters: `$` becomes `__1`, `.` becomes `__2`, a
// literal `__` becomes `__0`. A reader that only tries the identifier
// verbatim, its lower-case, and a case-insensitive scan cannot reach any of
// those, because no case folding maps `a$b` onto `a__1b`.
//
// The consequence of missing it is the dangerous kind: protoFieldByName's
// found=false is read by FieldValue.descendResolvedPath as "no such field",
// which for an unpinned path returns a silent NULL rather than an error. A
// struct field would simply read as empty. This test is therefore written
// against the ESCAPER rather than a hand-written constant — if the escaping
// table ever changes, the test follows it instead of pinning a stale spelling.
func TestProtoFieldByNameResolvesEscapedNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sqlName string
		want    any
	}{
		{"dollar", "a$b", int64(11)},
		{"dot", "a.b", int64(22)},
		{"double_underscore", "a__b", int64(33)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			escaped, err := protoname.ToProtoBufCompliantName(tc.sqlName)
			if err != nil {
				t.Fatalf("ToProtoBufCompliantName(%q): %v", tc.sqlName, err)
			}
			if escaped == tc.sqlName {
				t.Fatalf("%q is not mangled by the escaper, so it cannot exercise "+
					"the reverse lookup — pick an identifier the escaper rewrites", tc.sqlName)
			}

			md := singleInt64FieldMessage(t, escaped)
			msg := dynamicpb.NewMessage(md)
			msg.Set(md.Fields().Get(0), protoreflect.ValueOfInt64(tc.want.(int64)))

			got, found := protoFieldByName(msg, tc.sqlName)
			if !found {
				t.Fatalf("protoFieldByName(%q) did not find the field stored as %q — "+
					"an escaped field name is unreachable, which surfaces as a silent NULL "+
					"through FieldValue.descendResolvedPath", tc.sqlName, escaped)
			}
			if got != tc.want {
				t.Fatalf("protoFieldByName(%q) = %v, want %v", tc.sqlName, got, tc.want)
			}
		})
	}
}

// TestProtoFieldByNameStillMissesGenuinelyAbsentFields keeps the escaped-name
// attempt from degrading into "find something, anything". A name that is
// absent under BOTH its verbatim and its escaped spelling must still report
// found=false, or the NULL-vs-error distinction above collapses.
func TestProtoFieldByNameStillMissesGenuinelyAbsentFields(t *testing.T) {
	t.Parallel()

	md := singleInt64FieldMessage(t, "present")
	msg := dynamicpb.NewMessage(md)

	if _, found := protoFieldByName(msg, "absent$field"); found {
		t.Fatal("protoFieldByName resolved a field the descriptor does not declare")
	}
}

// singleInt64FieldMessage compiles a one-field message descriptor whose single
// int64 field carries the given (already escaped) proto field name.
func singleInt64FieldMessage(t *testing.T, fieldName string) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:   proto.String("escaped_name_test.proto"),
		Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("M"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String(fieldName),
				Number: proto.Int32(1),
				Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("compiling descriptor with field %q: %v", fieldName, err)
	}
	return fd.Messages().ByName("M")
}
