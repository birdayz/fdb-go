package values

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// nestedFixtureMessage builds a dynamic message
//
//	message Outer { message Inner { int64 sub = 1; int64 arr_e = 2 (repeated); } Inner rec = 1; }
//
// with rec.sub = 42 — the runtime shape of a STRUCT column (the executor
// flows nested records as raw proto messages).
func nestedFixtureMessage(t *testing.T) protoreflect.Message {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    protoStr("fixture.proto"),
		Syntax:  protoStr("proto2"),
		Package: protoStr("fx"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: protoStr("Outer"),
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: protoStr("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: protoStr("sub"), Number: protoInt32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()},
					{Name: protoStr("arr_e"), Number: protoInt32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()},
				},
			}},
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: protoStr("rec"), Number: protoInt32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: protoStr(".fx.Outer.Inner"), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("fixture descriptor: %v", err)
	}
	outer := fd.Messages().ByName("Outer")
	inner := outer.Messages().ByName("Inner")
	im := dynamicpb.NewMessage(inner)
	im.Set(inner.Fields().ByName("sub"), protoreflect.ValueOfInt64(42))
	lst := im.NewField(inner.Fields().ByName("arr_e")).List()
	lst.Append(protoreflect.ValueOfInt64(7))
	lst.Append(protoreflect.ValueOfInt64(8))
	im.Set(inner.Fields().ByName("arr_e"), protoreflect.ValueOfList(lst))
	om := dynamicpb.NewMessage(outer)
	om.Set(outer.Fields().ByName("rec"), protoreflect.ValueOfMessage(im))
	return om
}

func protoStr(s string) *string { return &s }
func protoInt32(i int32) *int32 { return &i }

// TestFieldValue_DescendProtoMessage pins the Java-parity STRUCT descent
// (FieldValue.eval → MessageHelpers.getFieldOnMessage): a fused
// multi-accessor path whose intermediate value is a raw proto message
// descends by field name (case-insensitive — SQL identifiers are UPPER,
// proto fields lower), converting scalars to the row-value domain and
// keeping repeated fields as lists for a downstream Explode. The
// unnest-residual class-2 runtime substrate.
func TestFieldValue_DescendProtoMessage(t *testing.T) {
	t.Parallel()
	om := nestedFixtureMessage(t)
	fused := &FieldValue{
		Field: "SUB",
		Typ:   NotNullLong,
		Resolved: &FieldPath{
			Accessors:      []ResolvedAccessor{{Field: "ROOT", Ordinal: 0}, {Field: "REC", Ordinal: 0}, {Field: "SUB", Ordinal: 0}},
			FrontierPinned: true,
		},
	}
	got, err := fused.descendResolvedPath(om.Interface())
	if err != nil {
		t.Fatalf("descend: %v", err)
	}
	if got != int64(42) {
		t.Fatalf("rec.sub = %v (%T), want int64 42", got, got)
	}

	// A repeated leaf stays a list (the Explode's collection shape).
	fusedArr := &FieldValue{
		Field: "ARR_E",
		Typ:   NotNullLong,
		Resolved: &FieldPath{
			Accessors:      []ResolvedAccessor{{Field: "ROOT", Ordinal: 0}, {Field: "REC", Ordinal: 0}, {Field: "ARR_E", Ordinal: 1}},
			FrontierPinned: true,
		},
	}
	got, err = fusedArr.descendResolvedPath(om.Interface())
	if err != nil {
		t.Fatalf("descend arr: %v", err)
	}
	arr, isArr := got.([]any)
	if !isArr || len(arr) != 2 || arr[0] != int64(7) || arr[1] != int64(8) {
		t.Fatalf("rec.arr_e = %v (%T), want [7 8]", got, got)
	}

	// A missing field on a PINNED path is LOUD, never silent NULL.
	fusedMiss := &FieldValue{
		Field: "NOPE",
		Typ:   NotNullLong,
		Resolved: &FieldPath{
			Accessors:      []ResolvedAccessor{{Field: "ROOT", Ordinal: 0}, {Field: "REC", Ordinal: 0}, {Field: "NOPE", Ordinal: 9}},
			FrontierPinned: true,
		},
	}
	if _, err := fusedMiss.descendResolvedPath(om.Interface()); err == nil {
		t.Fatal("a missing proto field on a pinned path must be LOUD (OrdinalResolutionError), got nil")
	}
}

// TestFieldValue_DescendDefaultArm pins the descend default arm: a nested step
// whose current value is NEITHER an OrdinalRow NOR a proto.Message (a scalar
// descended past a leaf, or a bare name-keyed map). The RFC-retired name model
// would have name-read the map here; the ordinal engine must NOT — a PINNED
// path is LOUD (OrdinalResolutionError), an UNPINNED path is SQL NULL, and in
// NEITHER case does a map key matching the field name get silently read.
func TestFieldValue_DescendDefaultArm(t *testing.T) {
	t.Parallel()

	descend := func(pinned bool, root any) (any, error) {
		fv := &FieldValue{
			Field: "SUB",
			Typ:   NotNullLong,
			Resolved: &FieldPath{
				// 2 accessors → one descent step lands `cur` on `root`.
				Accessors:      []ResolvedAccessor{{Field: "ROOT", Ordinal: 0}, {Field: "SUB", Ordinal: 0}},
				FrontierPinned: pinned,
			},
		}
		return fv.descendResolvedPath(root)
	}

	// A name-keyed map whose key MATCHES the descent field — the exact bait the
	// deleted name model would have taken. The default arm must never read it.
	nameBait := map[string]any{"SUB": int64(777)}

	// (1) Unpinned scalar → NULL.
	if got, err := descend(false, int64(99)); err != nil || got != nil {
		t.Fatalf("unpinned descent into a scalar must be (nil, nil), got (%v, %v)", got, err)
	}
	// (2) Unpinned name-keyed map → NULL, NOT the matching key's 777.
	if got, err := descend(false, nameBait); err != nil || got != nil {
		t.Fatalf("unpinned descent into a name-keyed map must be NULL (no silent name read), got (%v, %v)", got, err)
	}
	// (3) Pinned scalar → LOUD.
	var ore *OrdinalResolutionError
	if _, err := descend(true, int64(99)); !errors.As(err, &ore) {
		t.Fatalf("pinned descent into a scalar must be LOUD OrdinalResolutionError, got %v", err)
	}
	// (4) Pinned name-keyed map → LOUD, never the matching key's 777.
	if _, err := descend(true, nameBait); !errors.As(err, &ore) {
		t.Fatalf("pinned descent into a name-keyed map must be LOUD (no silent name read), got %v", err)
	}
}
