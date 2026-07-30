package executor

// A continuation token is wire-visible state. The same cursor position must
// encode to the SAME bytes every time: tokens are compared for equality, are
// asserted against in tests and goldens, and a deterministic-simulation replay
// of an identical run must produce an identical token stream. A *dynamicpb.Message
// STRUCT slot broke that — its default marshal iterates fields in Go map order —
// so appendContProtoMessage pins the encoding with
// proto.MarshalOptions{Deterministic: true}.
//
// The property being pinned is byte-stability, not a canonical encoding.
// Deterministic:true marshals in order.LegacyFieldOrder (extensions first, then
// non-oneof fields before oneof fields, then by field number) and sorts map
// entries by key; upstream disclaims that the result is canonical across
// languages or stable across releases. These tests therefore assert that Go
// re-emits its own bytes, never that those bytes are the ones Java would write.

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// multiFieldDynamicMessageDesc builds a five-field descriptor on spread-out field
// numbers. The field COUNT is what makes this usable as a detector: with a single
// field (as unregisteredDynamicMessage has) every iteration order is trivially
// ascending and the nondeterminism is unobservable.
func multiFieldDynamicMessageDesc(tb testing.TB) protoreflect.MessageDescriptor {
	tb.Helper()
	str := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(num),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   typ.Enum(),
		}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("continuation_determinism_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("contdet"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Struct"),
			Field: []*descriptorpb.FieldDescriptorProto{
				str("s_a", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64),
				str("s_b", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING),
				str("s_c", 5, descriptorpb.FieldDescriptorProto_TYPE_INT32),
				str("s_d", 7, descriptorpb.FieldDescriptorProto_TYPE_BOOL),
				str("s_e", 9, descriptorpb.FieldDescriptorProto_TYPE_STRING),
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		tb.Fatalf("build synthetic file descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

// newMultiFieldDynamicMessage returns a freshly built, fully populated dynamic
// message. Freshness matters: dynamicpb holds fields in a Go map, so each new
// message can range them in a different order.
func newMultiFieldDynamicMessage(md protoreflect.MessageDescriptor) *dynamicpb.Message {
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("s_a"), protoreflect.ValueOfInt64(0x0123456789abcdef))
	m.Set(md.Fields().ByName("s_b"), protoreflect.ValueOfString("beta-value"))
	m.Set(md.Fields().ByName("s_c"), protoreflect.ValueOfInt32(1337))
	m.Set(md.Fields().ByName("s_d"), protoreflect.ValueOfBool(true))
	m.Set(md.Fields().ByName("s_e"), protoreflect.ValueOfString("epsilon-value"))
	return m
}

// TestAppendContValue_DynamicStructSlotDeterministic pins that a dynamicpb STRUCT
// slot encodes to byte-identical continuation bytes across repeats.
//
// The loop is the detector, not padding: dynamicpb's field map is never grown or
// deleted from, so ranging it returns a ROTATION of insertion order — one of five
// here, chosen per range. A single comparison would agree with the
// Deterministic:true encoding one time in five even with the fix reverted;
// looping makes a false pass (1/5)^n.
func TestAppendContValue_DynamicStructSlotDeterministic(t *testing.T) {
	t.Parallel()
	md := multiFieldDynamicMessageDesc(t)

	// Guard the premise: this value must take the proto.Message arm of
	// appendContValue AND be flagged dynamic (flag 1), which is the arm whose
	// marshal was nondeterministic.
	var probe any = newMultiFieldDynamicMessage(md)
	if _, ok := probe.(proto.Message); !ok {
		t.Fatalf("dynamicpb.Message no longer satisfies proto.Message — this test targets the wrong arm")
	}
	if _, ok := probe.(*dynamicpb.Message); !ok {
		t.Fatalf("probe is %T, want *dynamicpb.Message — flag-1 is the nondeterministic representation", probe)
	}

	first, err := appendContValue(nil, newMultiFieldDynamicMessage(md))
	if err != nil {
		t.Fatalf("appendContValue: %v", err)
	}

	const iterations = 256
	for i := 0; i < iterations; i++ {
		got, err := appendContValue(nil, newMultiFieldDynamicMessage(md))
		if err != nil {
			t.Fatalf("appendContValue iteration %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("continuation encoding of a dynamic STRUCT slot is not byte-stable at iteration %d:\n first=%x\n   got=%x", i, first, got)
		}
	}

	// And the payload is specifically the Deterministic:true encoding, not merely
	// some arbitrary encoding that happened to repeat. (This says nothing about
	// matching Java: it pins that the marshal option is actually in effect.)
	want, err := proto.MarshalOptions{Deterministic: true}.Marshal(newMultiFieldDynamicMessage(md))
	if err != nil {
		t.Fatalf("deterministic marshal: %v", err)
	}
	if !bytes.Contains(first, want) {
		t.Fatalf("continuation bytes do not carry the Deterministic:true payload:\n token=%x\n want payload=%x", first, want)
	}
}
