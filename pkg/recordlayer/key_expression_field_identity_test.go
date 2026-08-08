package recordlayer

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// FieldKeyExpression.resolveFieldDescriptor is the single name-consuming
// resolution point behind all five live evaluation paths (Evaluate,
// EvaluateFlat, EvaluateScalar, EvaluateInt64, PackDirect). Two independent
// facts have to hold there — one about which FIELD name selects, one about
// which MESSAGE name must not — and either can break without disturbing the
// other, so each gets its own pin.
//
// A third property of the same helper, that a field absent from the
// descriptor is an ERROR rather than Java's null result, is a deliberate
// divergence pinned separately by
// TestKeyExpressionFastPath_UnknownFieldErrorsEverywhere.
//
//  1. The FIELD name is what selects the descriptor, and the field's
//     declaration index is not. Java resolves this step by name on every
//     evaluation — FieldKeyExpression.evaluateMessage calls
//     recordDescriptor.findFieldByName(fieldName) (FieldKeyExpression.java:183).
//     A key expression is declared against a name in stored metadata, so the
//     name IS the identity here. This is the mirror image of the nested-value
//     descent pin in the cascades values package, where the ordinal is the
//     identity and the name must not decide.
//
//  2. The MESSAGE name is NOT the identity of a descriptor, so it must not key
//     the descriptor cache. Two distinct descriptors sharing a full name is
//     ordinary (a generated type versus a dynamicpb type built from stored
//     metadata; two evolutions of one message), and handing a FieldDescriptor
//     from one to the other is rejected by protoreflect with a panic.

// probeFile builds a standalone proto2 file descriptor holding one message
// named probe.Rec with the given fields.
func probeFile(t *testing.T, fileName string, fields ...*descriptorpb.FieldDescriptorProto) protoreflect.MessageDescriptor {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(fileName),
		Package: proto.String("probe"),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name:  proto.String("Rec"),
			Field: fields,
		}},
	}
	file, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build %s: %v", fileName, err)
	}
	return file.Messages().Get(0)
}

func probeStringField(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
	}
}

func probeInt64Field(name string, number int32) *descriptorpb.FieldDescriptorProto {
	return &descriptorpb.FieldDescriptorProto{
		Name:   proto.String(name),
		Number: proto.Int32(number),
		Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:   descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
	}
}

// allFivePaths reads one scalar field through every live evaluation path and
// returns what each produced, so a pin can assert all five agree. A path that
// declines reports (nil, false).
type pathReadings struct {
	evaluate any
	flat     any
	scalar   any
	int64Val any
	packed   tuple.Tuple
	packedOK bool
}

func readAllFivePaths(t *testing.T, f *FieldKeyExpression, m protoreflect.Message) pathReadings {
	t.Helper()
	var out pathReadings

	rows, err := f.Evaluate(nil, m.Interface())
	if err != nil {
		t.Fatalf("Evaluate: unexpected error: %v", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("Evaluate: want exactly one single-column tuple, got %v", rows)
	}
	out.evaluate = rows[0][0]

	flat, err := f.EvaluateFlat(nil, m.Interface())
	if err != nil {
		t.Fatalf("EvaluateFlat: unexpected error: %v", err)
	}
	if len(flat) != 1 {
		t.Fatalf("EvaluateFlat: want one column, got %v", flat)
	}
	out.flat = flat[0]

	sc, err := f.EvaluateScalar(nil, m.Interface())
	if err != nil {
		t.Fatalf("EvaluateScalar: unexpected error: %v", err)
	}
	out.scalar = sc

	if iv, ok, err := f.EvaluateInt64(nil, m.Interface()); err != nil {
		t.Fatalf("EvaluateInt64: unexpected error: %v", err)
	} else if ok {
		out.int64Val = iv
	}

	pk := tuple.GetPacker()
	pk.Reset()
	out.packedOK = f.PackDirect(pk, nil, m.Interface())
	if out.packedOK {
		var buf []byte
		bytes := pk.AppendInto(&buf, nil)
		got, uerr := tuple.Unpack(bytes)
		if uerr != nil {
			t.Fatalf("PackDirect: unpack: %v", uerr)
		}
		out.packed = got
	}
	tuple.PutPacker(pk)
	return out
}

// TestFieldKeyExpression_ResolvesByName_NotByDeclarationIndex pins fact 1: the
// field NAME selects the descriptor. The message is built so that name order
// and declaration order disagree — "a" sits at declaration index 1 (field
// number 2) behind a decoy at index 0 — so any path that substituted the
// declaration index for the name would read the decoy and be caught.
func TestFieldKeyExpression_ResolvesByName_NotByDeclarationIndex(t *testing.T) {
	t.Parallel()
	md := probeFile(t, "byname.proto",
		probeStringField("decoy_at_index_zero", 1),
		probeStringField("a", 2),
		probeStringField("also_not_a", 3),
	)
	m := dynamicpb.NewMessage(md)
	m.Set(md.Fields().ByName("decoy_at_index_zero"), protoreflect.ValueOfString("WRONG-index-0"))
	m.Set(md.Fields().ByName("a"), protoreflect.ValueOfString("RIGHT"))
	m.Set(md.Fields().ByName("also_not_a"), protoreflect.ValueOfString("WRONG-index-2"))

	// Sanity: the shape only has teeth while the named field is NOT at index 0.
	if md.Fields().ByName("a").Index() == 0 {
		t.Fatal("probe is vacuous: field \"a\" must not be the first declared field")
	}

	got := readAllFivePaths(t, Field("a").(*FieldKeyExpression), m)
	for label, v := range map[string]any{
		"Evaluate":       got.evaluate,
		"EvaluateFlat":   got.flat,
		"EvaluateScalar": got.scalar,
	} {
		if v != "RIGHT" {
			t.Errorf("%s read %v; the field NAME must select the descriptor, "+
				"never its declaration index (Java: findFieldByName, "+
				"FieldKeyExpression.java:183)", label, v)
		}
	}
	if !got.packedOK || len(got.packed) != 1 || got.packed[0] != "RIGHT" {
		t.Errorf("PackDirect packed %v (ok=%v); want the value of the field NAMED a",
			got.packed, got.packedOK)
	}

	// EvaluateInt64 is the one path that reads the descriptor's KIND to decide
	// whether to answer at all, so drive it against an integer field whose
	// declaration index likewise is not zero.
	mdInt := probeFile(t, "bynameint.proto",
		probeInt64Field("decoy_at_index_zero", 1),
		probeInt64Field("n", 2),
	)
	mi := dynamicpb.NewMessage(mdInt)
	mi.Set(mdInt.Fields().ByName("decoy_at_index_zero"), protoreflect.ValueOfInt64(111))
	mi.Set(mdInt.Fields().ByName("n"), protoreflect.ValueOfInt64(222))
	iv, ok, err := Field("n").(*FieldKeyExpression).EvaluateInt64(nil, mi.Interface())
	if err != nil || !ok || iv != 222 {
		t.Errorf("EvaluateInt64 = (%d, %v, %v); want (222, true, nil) — the field "+
			"NAMED n, not the one at declaration index 0", iv, ok, err)
	}
}

// TestFieldKeyExpression_DescriptorCacheKeysOnIdentity_NotOnMessageName pins
// fact 2. Both descriptors are named probe.Rec and both declare "a" and "b",
// but they number and order them differently, so a cache keyed on the message
// FULL NAME hands the second message a FieldDescriptor belonging to the first.
// protoreflect answers that with a panic, so every path here would crash on a
// live index write.
func TestFieldKeyExpression_DescriptorCacheKeysOnIdentity_NotOnMessageName(t *testing.T) {
	t.Parallel()
	first := probeFile(t, "cachefirst.proto", probeStringField("a", 1), probeStringField("b", 2))
	second := probeFile(t, "cachesecond.proto", probeStringField("b", 1), probeStringField("a", 2))

	if first.FullName() != second.FullName() {
		t.Fatalf("probe is vacuous: the two descriptors must share a full name, got %q and %q",
			first.FullName(), second.FullName())
	}
	if first == second {
		t.Fatal("probe is vacuous: the two descriptors must be distinct instances")
	}

	m1 := dynamicpb.NewMessage(first)
	m1.Set(first.Fields().ByName("a"), protoreflect.ValueOfString("A-from-first"))
	m1.Set(first.Fields().ByName("b"), protoreflect.ValueOfString("B-from-first"))
	m2 := dynamicpb.NewMessage(second)
	m2.Set(second.Fields().ByName("a"), protoreflect.ValueOfString("A-from-second"))
	m2.Set(second.Fields().ByName("b"), protoreflect.ValueOfString("B-from-second"))

	// One expression instance, warmed on `first`, then used on `second` — the
	// shape a shared metadata key expression meets when a store reads records
	// through a descriptor built from stored metadata.
	expr := Field("a").(*FieldKeyExpression)

	warm := readAllFivePaths(t, expr, m1)
	if warm.evaluate != "A-from-first" {
		t.Fatalf("warm-up read %v; want A-from-first", warm.evaluate)
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic reusing the cached descriptor across two descriptors "+
				"sharing the full name %q: %v — the cache must key on descriptor "+
				"IDENTITY, never on the message name", first.FullName(), p)
		}
	}()
	got := readAllFivePaths(t, expr, m2)
	for label, v := range map[string]any{
		"Evaluate":       got.evaluate,
		"EvaluateFlat":   got.flat,
		"EvaluateScalar": got.scalar,
	} {
		if v != "A-from-second" {
			t.Errorf("%s read %v after the cache was warmed on a different "+
				"descriptor of the same name; want A-from-second", label, v)
		}
	}
	if !got.packedOK || len(got.packed) != 1 || got.packed[0] != "A-from-second" {
		t.Errorf("PackDirect packed %v (ok=%v); want A-from-second", got.packed, got.packedOK)
	}
}
