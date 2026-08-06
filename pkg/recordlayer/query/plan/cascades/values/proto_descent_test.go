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

// TestFieldValue_DescendProtoMessage pins the STRUCT descent: a fused
// multi-accessor path whose intermediate value is a raw proto message
// descends by field name (case-insensitive — SQL identifiers are UPPER,
// proto fields lower), converting scalars to the row-value domain and
// keeping repeated fields as lists for a downstream Explode. The
// unnest-residual class-2 runtime substrate.
//
// Descending by NAME is the DIVERGENCE this step carries on the `.Field`
// ratchet (RFC-197's `boundary` bucket), not the port, and this comment used to
// call it "Java-parity … FieldValue.eval → MessageHelpers.getFieldOnMessage".
// Java descends by ORDINAL: FieldValue.eval calls
// MessageHelpers.getFieldValueForFieldOrdinals (FieldValue.java:169), which
// bounds-checks and indexes getFields().get(ordinal) (MessageHelpers.java:170-175);
// the name-taking getFieldOnMessage(msg, String) overload exists in
// MessageHelpers but FieldValue.eval is not a caller. See the comment on the
// proto.Message arm of descendResolvedPath for the full citation, and the
// ordinal test below for why the conversion is not a local edit.
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

// TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal is a NEGATIVE
// result, pinned because a decision rests on it: the nested proto descent is
// deliberately NOT converted to Java's ordinal form, and this is the fact that
// makes the conversion unsafe rather than merely unreviewed.
//
// Java descends this step by ordinal (FieldValue.java:169 →
// MessageHelpers.java:170-175). Go descends by name, and the debt entry on
// protoFieldByName says the fix is to resolve the path to field NUMBERS once at
// the boundary. That reads like a local edit. It is not, and the reason is
// mechanical: ResolvedAccessor.Ordinal is NOT the proto descriptor's
// declaration index on the producers that actually reach this arm.
//
// The struct-descent producers mint the LOUD sentinel -1 on purpose —
// `unnest_seed.go` and `unnest_gather.go` both build their suffix accessors as
// `ResolvedAccessor{Field: strings.ToUpper(seg), Ordinal: -1}`, on the stated
// grounds that "a struct materializes as a proto message, not a positional row,
// so the ordinal is never consulted". The remaining producer,
// `expr/expr.go`'s fuseNestedAccessors, copies semantic.NestedAccessor.Ordinal,
// which is a position in the SQL struct type's declared field list — equal to
// the emitted descriptor's declaration index only by convention, exactly the
// equality Java itself only holds by convention (Type.Record.fromDescriptor
// preserves descriptor order into an insertion-ordered map; a caller passing a
// differently-ordered map would silently diverge).
//
// So converting this arm to `getFields().get(ordinal)` today would index at -1.
// If a future change makes every producer mint a true declaration index, THIS
// test is what has to be updated, and updating it is the signal that the
// boundary bucket's two entries have become retirable.
func TestFieldValue_DescendProtoMessage_MustNotConsultTheOrdinal(t *testing.T) {
	t.Parallel()
	om := nestedFixtureMessage(t)

	// The exact accessor shape unnest_seed.go / unnest_gather.go mint for a
	// struct descent: the name carries the resolution, the ordinal is -1.
	sentinel := &FieldValue{
		Field: "SUB",
		Typ:   NotNullLong,
		Resolved: &FieldPath{
			Accessors: []ResolvedAccessor{
				{Field: "ROOT", Ordinal: 0},
				{Field: "REC", Ordinal: -1},
				{Field: "SUB", Ordinal: -1},
			},
			FrontierPinned: true,
		},
	}
	got, err := sentinel.descendResolvedPath(om.Interface())
	if err != nil {
		t.Fatalf("descend with the -1 struct-descent sentinel: %v. The nested proto arm must "+
			"resolve by NAME; the producers that reach it mint Ordinal -1 deliberately", err)
	}
	if got != int64(42) {
		t.Fatalf("rec.sub = %v (%T), want int64 42 — read by name, with the ordinal ignored", got, got)
	}

	// And the ordinal must not merely be tolerated, it must be IGNORED: a
	// wrong-but-in-range ordinal still reads the field the name selects. `sub`
	// is declaration index 0 of Inner and `arr_e` is 1, so an ordinal descent
	// would return the list [7 8] here instead of 42.
	wrongOrdinal := &FieldValue{
		Field: "SUB",
		Typ:   NotNullLong,
		Resolved: &FieldPath{
			Accessors: []ResolvedAccessor{
				{Field: "ROOT", Ordinal: 0},
				{Field: "REC", Ordinal: 0},
				{Field: "SUB", Ordinal: 1},
			},
			FrontierPinned: true,
		},
	}
	got, err = wrongOrdinal.descendResolvedPath(om.Interface())
	if err != nil {
		t.Fatalf("descend with a wrong in-range ordinal: %v", err)
	}
	if got != int64(42) {
		t.Fatalf("rec.sub = %v (%T), want int64 42. Reading [7 8] means this arm has started "+
			"consulting the ordinal — which is a WRONG-COLUMN read on every producer that "+
			"does not mint a descriptor declaration index", got, got)
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
