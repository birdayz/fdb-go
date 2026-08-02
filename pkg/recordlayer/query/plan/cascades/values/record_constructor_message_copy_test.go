package values

// Shape pins for copyFieldsByNumber's mismatch handling.
//
// copyFieldsByNumber joins two descriptors on FIELD NUMBER — the wire identity
// — and nothing upstream guarantees the two fields agree on kind or
// cardinality. protoreflect's Set/Mutable PANIC in exactly that case, and a
// panic in library code is never the right failure (design principle 4). These
// tests construct the mismatching shapes directly and require a typed
// *ProtoTypeError instead.

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// oneFieldMessage builds a standalone dynamic message descriptor with a single
// field at number 1 of the given kind and cardinality.
func oneFieldMessage(t *testing.T, file, msg string, kind descriptorpb.FieldDescriptorProto_Type, repeated bool) protoreflect.MessageDescriptor {
	t.Helper()
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if repeated {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(file),
		Syntax:  proto.String("proto2"),
		Package: proto.String("copytest"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String(msg),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name:   proto.String("f1"),
				Number: proto.Int32(1),
				Label:  label.Enum(),
				Type:   kind.Enum(),
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build %s: %v", file, err)
	}
	return fd.Messages().Get(0)
}

// TestCopyFieldsByNumberKindMismatchIsAnError pins the SCALAR KIND mismatch:
// field 1 is an int64 in the source and a string in the target. Before the
// shape check this reached dst.Set and panicked
// ("proto: copytest.Dst.f1: assigning invalid type").
func TestCopyFieldsByNumberKindMismatchIsAnError(t *testing.T) {
	t.Parallel()

	srcMD := oneFieldMessage(t, "copy_src_kind.proto", "Src", descriptorpb.FieldDescriptorProto_TYPE_INT64, false)
	dstMD := oneFieldMessage(t, "copy_dst_kind.proto", "Dst", descriptorpb.FieldDescriptorProto_TYPE_STRING, false)

	src := dynamicpb.NewMessage(srcMD)
	src.Set(srcMD.Fields().ByNumber(1), protoreflect.ValueOfInt64(7))
	dst := dynamicpb.NewMessage(dstMD)

	err := copyFieldsByNumber(dst, src)
	if err == nil {
		t.Fatal("copyFieldsByNumber returned nil for an int64-vs-string mismatch at field 1; " +
			"the shape check in copyFieldsByNumber is gone and protoreflect.Set will panic instead")
	}
	var pte *ProtoTypeError
	if !errors.As(err, &pte) {
		t.Fatalf("want *ProtoTypeError, got %T: %v", err, err)
	}
}

// TestCopyFieldsByNumberCardinalityMismatchIsAnError pins the OTHER direction:
// same kind, but repeated on one side only. This one never reached dst.Set —
// it hit dst.Mutable(tfd).List() on a singular target, a different panic — so
// it is a genuinely independent arm of the guard and a fix that satisfied only
// the kind check would still crash here.
func TestCopyFieldsByNumberCardinalityMismatchIsAnError(t *testing.T) {
	t.Parallel()

	srcMD := oneFieldMessage(t, "copy_src_card.proto", "Src", descriptorpb.FieldDescriptorProto_TYPE_INT64, true)
	dstMD := oneFieldMessage(t, "copy_dst_card.proto", "Dst", descriptorpb.FieldDescriptorProto_TYPE_INT64, false)

	src := dynamicpb.NewMessage(srcMD)
	src.Mutable(srcMD.Fields().ByNumber(1)).List().Append(protoreflect.ValueOfInt64(7))
	dst := dynamicpb.NewMessage(dstMD)

	err := copyFieldsByNumber(dst, src)
	if err == nil {
		t.Fatal("copyFieldsByNumber returned nil for a repeated-vs-singular mismatch at field 1; " +
			"the cardinality half of the shape check is gone and dst.Mutable().List() will panic instead")
	}
	var pte *ProtoTypeError
	if !errors.As(err, &pte) {
		t.Fatalf("want *ProtoTypeError, got %T: %v", err, err)
	}
}

// TestCopyFieldsByNumberMatchingShapeStillCopies is the counterweight: the
// guard must reject only genuine mismatches. A test that merely proved
// "returns an error" would be satisfied by a function that rejects everything.
func TestCopyFieldsByNumberMatchingShapeStillCopies(t *testing.T) {
	t.Parallel()

	srcMD := oneFieldMessage(t, "copy_src_ok.proto", "Src", descriptorpb.FieldDescriptorProto_TYPE_INT64, false)
	dstMD := oneFieldMessage(t, "copy_dst_ok.proto", "Dst", descriptorpb.FieldDescriptorProto_TYPE_INT64, false)

	src := dynamicpb.NewMessage(srcMD)
	src.Set(srcMD.Fields().ByNumber(1), protoreflect.ValueOfInt64(7))
	dst := dynamicpb.NewMessage(dstMD)

	if err := copyFieldsByNumber(dst, src); err != nil {
		t.Fatalf("wire-compatible copy rejected: %v", err)
	}
	if got := dst.Get(dstMD.Fields().ByNumber(1)).Int(); got != 7 {
		t.Fatalf("value not copied: got %d, want 7", got)
	}
}
