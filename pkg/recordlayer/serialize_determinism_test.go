package recordlayer

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

// buildDeterminismTestMessageDesc builds a descriptor for a record type with
// several scalar fields spread across non-adjacent field numbers. The spread
// (and the number of fields) makes it likely that dynamicpb's default,
// map-order field iteration would emit fields in a different order — and thus
// produce different bytes — on repeated marshals if determinism were off.
func buildDeterminismTestMessageDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("serialize_determinism_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("serdet"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Rec"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:   proto.String("f_a"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				},
				{
					Name:   proto.String("f_b"),
					Number: proto.Int32(3),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				},
				{
					Name:   proto.String("f_c"),
					Number: proto.Int32(5),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
				},
				{
					Name:   proto.String("f_d"),
					Number: proto.Int32(7),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
				},
				{
					Name:   proto.String("f_e"),
					Number: proto.Int32(9),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build file descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

// newDeterminismTestMessage returns a fully populated *dynamicpb.Message for the
// descriptor. Building a fresh message each time is deliberate: dynamicpb keeps
// fields in a Go map, so a fresh message can iterate them in a different order,
// which is exactly the nondeterminism the fix guards against.
func newDeterminismTestMessage(md protoreflect.MessageDescriptor) proto.Message {
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("f_a"), protoreflect.ValueOfInt64(0x0123456789abcdef))
	m.Set(md.Fields().ByName("f_b"), protoreflect.ValueOfString("beta-value"))
	m.Set(md.Fields().ByName("f_c"), protoreflect.ValueOfInt32(1337))
	m.Set(md.Fields().ByName("f_d"), protoreflect.ValueOfBool(true))
	m.Set(md.Fields().ByName("f_e"), protoreflect.ValueOfString("epsilon-value"))
	return m
}

// unwrapUnionInner extracts the inner record bytes from a serializeUnion output
// (a single length-delimited field tagged with unionFieldNumber).
func unwrapUnionInner(t *testing.T, union []byte, unionFieldNumber protowire.Number) []byte {
	t.Helper()
	num, typ, n := protowire.ConsumeTag(union)
	if n < 0 {
		t.Fatalf("consume union tag: %v", protowire.ParseError(n))
	}
	if num != unionFieldNumber {
		t.Fatalf("union field number = %d, want %d", num, unionFieldNumber)
	}
	if typ != protowire.BytesType {
		t.Fatalf("union wire type = %v, want BytesType", typ)
	}
	inner, m := protowire.ConsumeBytes(union[n:])
	if m < 0 {
		t.Fatalf("consume union bytes: %v", protowire.ParseError(m))
	}
	if n+m != len(union) {
		t.Fatalf("union has trailing bytes: consumed %d of %d", n+m, len(union))
	}
	return inner
}

// assertAscendingFields walks a serialized message and fails if the top-level
// field numbers are not in non-decreasing (ascending) order — the canonical
// wire order Java always writes and that Deterministic:true guarantees.
func assertAscendingFields(t *testing.T, msg []byte) {
	t.Helper()
	prev := protowire.Number(0)
	rest := msg
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			t.Fatalf("consume tag: %v", protowire.ParseError(n))
		}
		if num < prev {
			t.Fatalf("field number %d follows %d — not ascending in %x", num, prev, msg)
		}
		prev = num
		rest = rest[n:]
		skip := protowire.ConsumeFieldValue(num, typ, rest)
		if skip < 0 {
			t.Fatalf("consume field value: %v", protowire.ParseError(skip))
		}
		rest = rest[skip:]
	}
}

// TestSerializeUnion_DynamicMessageDeterministic pins the fix in serializeUnion:
// the reflection slow path (hit only by *dynamicpb.Message — the SQL/relational
// row shape, which has no MarshalVT/SizeVT fast path) must serialize a given
// record to byte-identical output on every call and in ascending field-number
// order. Before the fix this path used plain proto.Marshal, which iterates a
// dynamic message's fields in map order and so wrote the same row to different
// bytes across writes — caught by the DST SQL workload's determinism oracle.
func TestSerializeUnion_DynamicMessageDeterministic(t *testing.T) {
	t.Parallel()
	md := buildDeterminismTestMessageDesc(t)

	// Guard the premise: a *dynamicpb.Message must NOT satisfy the VT fast/slow
	// paths, so serializeUnion truly exercises the deterministic reflection path
	// under test. Generated protos implement these interfaces and never get here.
	var probe proto.Message = dynamicpb.NewMessage(md)
	if _, ok := probe.(interface {
		SizeVT() int
		MarshalToSizedBufferVT([]byte) (int, error)
	}); ok {
		t.Fatalf("dynamicpb.Message unexpectedly implements the SizeVT fast path")
	}
	if _, ok := probe.(interface{ MarshalVT() ([]byte, error) }); ok {
		t.Fatalf("dynamicpb.Message unexpectedly implements MarshalVT")
	}

	rt := &RecordType{Name: "Rec", unionFieldNumber: 1}

	// First serialization is the reference.
	first, err := serializeUnion(newDeterminismTestMessage(md), rt)
	if err != nil {
		t.Fatalf("serializeUnion: %v", err)
	}
	if len(first) == 0 {
		t.Fatalf("serializeUnion produced empty output")
	}

	// The inner record bytes must be in ascending field-number order.
	inner := unwrapUnionInner(t, first, rt.unionFieldNumber)
	assertAscendingFields(t, inner)

	// Byte stability across many fresh messages (fresh => fresh map iteration
	// order, the source of the original nondeterminism).
	const iterations = 256
	for i := 0; i < iterations; i++ {
		got, err := serializeUnion(newDeterminismTestMessage(md), rt)
		if err != nil {
			t.Fatalf("serializeUnion iteration %d: %v", i, err)
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("serializeUnion not byte-stable at iteration %d:\n first=%x\n   got=%x", i, first, got)
		}
	}
}

// TestSerializeUnion_DeterministicBeatsPlainMarshal is the differential guard:
// it demonstrates that Deterministic:true is load-bearing for dynamic messages.
// Plain proto.Marshal of a *dynamicpb.Message can vary across marshals (map
// order), whereas the deterministic path serializeUnion uses is stable. To keep
// the test itself deterministic we do not assert plain Marshal *does* vary
// (it may or may not on any given run); instead we assert the deterministic
// inner bytes exactly match proto.MarshalOptions{Deterministic:true}.Marshal —
// i.e. serializeUnion is wrapping the canonical, sorted encoding.
func TestSerializeUnion_DeterministicBeatsPlainMarshal(t *testing.T) {
	t.Parallel()
	md := buildDeterminismTestMessageDesc(t)
	rt := &RecordType{Name: "Rec", unionFieldNumber: 4}

	union, err := serializeUnion(newDeterminismTestMessage(md), rt)
	if err != nil {
		t.Fatalf("serializeUnion: %v", err)
	}
	inner := unwrapUnionInner(t, union, rt.unionFieldNumber)

	want, err := proto.MarshalOptions{Deterministic: true}.Marshal(newDeterminismTestMessage(md))
	if err != nil {
		t.Fatalf("deterministic marshal: %v", err)
	}
	if !bytes.Equal(inner, want) {
		t.Fatalf("serializeUnion inner bytes are not the canonical deterministic encoding:\n inner=%x\n  want=%x", inner, want)
	}

	// And the canonical encoding is itself stable across repeats.
	for i := 0; i < 64; i++ {
		got, err := proto.MarshalOptions{Deterministic: true}.Marshal(newDeterminismTestMessage(md))
		if err != nil {
			t.Fatalf("deterministic marshal iteration %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("deterministic marshal not stable at iteration %d", i)
		}
	}
}
