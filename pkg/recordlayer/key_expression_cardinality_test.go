package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen"
)

// buildCardTestMessage builds a dynamic message descriptor with an int32
// "id" and a repeated int32 "arr" (the plain-repeated array shape Go writes),
// returning a constructor that sets arr to the given values (nil = unset).
func buildCardTestMessage(t *testing.T) (protoreflect.MessageDescriptor, func(arr []int32, set bool) proto.Message) {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("card_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("cardtest"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Rec"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{
					Name:   proto.String("id"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
				},
				{
					Name:   proto.String("arr"),
					Number: proto.Int32(2),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
				},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build file descriptor: %v", err)
	}
	md := fd.Messages().Get(0)
	return md, func(arr []int32, set bool) proto.Message {
		m := dynamicpb.NewMessage(md)
		m.Set(md.Fields().ByName("id"), protoreflect.ValueOfInt32(1))
		if set {
			list := m.NewField(md.Fields().ByName("arr")).List()
			for _, v := range arr {
				list.Append(protoreflect.ValueOfInt32(v))
			}
			m.Set(md.Fields().ByName("arr"), protoreflect.ValueOfList(list))
		}
		return m
	}
}

// TestCardinalityKeyExpression_PlainRepeated pins the evaluator over a plain
// repeated array field (Go's write shape): populated arrays count their
// elements, while an empty/unset array yields a NULL key (empty ==
// wire-indistinguishable from NULL on Go-written records, RFC-143 §3a) —
// consistent with the scalar CardinalityValue.
func TestCardinalityKeyExpression_PlainRepeated(t *testing.T) {
	t.Parallel()
	_, mk := buildCardTestMessage(t)
	expr := CardinalityExpr(FieldConcatenate("arr"))

	cases := []struct {
		name string
		arr  []int32
		set  bool
		want any // nil means NULL key
	}{
		{"two elements", []int32{10, 20}, true, int64(2)},
		{"one element", []int32{10}, true, int64(1)},
		{"empty array → NULL (§3a)", []int32{}, true, nil},
		{"unset array → NULL", nil, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := expr.Evaluate(nil, mk(tc.arr, tc.set))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(got) != 1 || len(got[0]) != 1 {
				t.Fatalf("want single key element, got %v", got)
			}
			if got[0][0] != tc.want {
				t.Fatalf("cardinality key = %v (%T), want %v", got[0][0], got[0][0], tc.want)
			}
		})
	}
}

// TestCardinalityKeyExpression_NullableWrapper pins the cross-engine §3a fast
// path: a Java-written nullable array is a wrapper message
// { repeated R values = 1; }, and the argument key expression is
// field("arr").nest(field("values", Concatenate)). The evaluator descends the
// wrapper and counts the inner "values" field; a PRESENT wrapper with zero
// elements is a non-null empty array → 0 (distinct from NULL), while an ABSENT
// wrapper → NULL. This is how Java distinguishes NULL from an empty array.
func TestCardinalityKeyExpression_NullableWrapper(t *testing.T) {
	t.Parallel()
	// Build: Rec { optional Wrapper arr = 1; }  Wrapper { repeated int32 values = 1; }
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("card_wrap_test.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("cardwraptest"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Wrapper"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:   proto.String("values"),
					Number: proto.Int32(1),
					Label:  descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
					Type:   descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
				}},
			},
			{
				Name: proto.String("Rec"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name:     proto.String("arr"),
					Number:   proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String(".cardwraptest.Wrapper"),
				}},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build file descriptor: %v", err)
	}
	recMD := fd.Messages().ByName("Rec")
	wrapMD := fd.Messages().ByName("Wrapper")
	arrFD := recMD.Fields().ByName("arr")
	valuesFD := wrapMD.Fields().ByName("values")

	mk := func(present bool, vals []int32) proto.Message {
		m := dynamicpb.NewMessage(recMD)
		if present {
			w := dynamicpb.NewMessage(wrapMD)
			list := w.NewField(valuesFD).List()
			for _, v := range vals {
				list.Append(protoreflect.ValueOfInt32(v))
			}
			w.Set(valuesFD, protoreflect.ValueOfList(list))
			m.Set(arrFD, protoreflect.ValueOfMessage(w))
		}
		return m
	}

	// The Java wrapper-shaped argument: field("arr").nest(field("values", Concatenate)).
	expr := CardinalityExpr(Nest("arr", FieldConcatenate("values")))

	cases := []struct {
		name    string
		present bool
		vals    []int32
		want    any
	}{
		{"wrapper present, two values", true, []int32{1, 2}, int64(2)},
		{"wrapper present, empty → 0 (non-null)", true, []int32{}, int64(0)},
		{"wrapper absent → NULL", false, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := expr.Evaluate(nil, mk(tc.present, tc.vals))
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(got) != 1 || len(got[0]) != 1 {
				t.Fatalf("want single key element, got %v", got)
			}
			if got[0][0] != tc.want {
				t.Fatalf("wrapper cardinality = %v (%T), want %v", got[0][0], got[0][0], tc.want)
			}
		})
	}
}

// TestCardinalityKeyExpression_NilMessage pins the null-message fallback: no
// record → NULL key (mirrors Java's null Key.Evaluated).
func TestCardinalityKeyExpression_NilMessage(t *testing.T) {
	t.Parallel()
	expr := CardinalityExpr(FieldConcatenate("arr"))
	got, err := expr.Evaluate(nil, nil)
	if err != nil {
		t.Fatalf("Evaluate(nil): %v", err)
	}
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != nil {
		t.Fatalf("nil message → NULL key, got %v", got)
	}
}

// TestCardinalityKeyExpression_NoDuplicatesColumnSize pins the structural
// contract matching Java's CardinalityFunctionKeyExpression: column size 1 and
// createsDuplicates() == false (overriding FunctionKeyExpression's default
// true).
func TestCardinalityKeyExpression_NoDuplicatesColumnSize(t *testing.T) {
	t.Parallel()
	expr := CardinalityExpr(FieldConcatenate("arr"))
	if got := expr.ColumnSize(); got != 1 {
		t.Fatalf("ColumnSize = %d, want 1", got)
	}
	if createsDuplicates(expr) {
		t.Fatalf("createsDuplicates(cardinality) = true, want false (Java overrides to false)")
	}
	// A plain FunctionKeyExpression must still report true (unchanged).
	if !createsDuplicates(FunctionExpr("some_other_fn", FieldConcatenate("arr"))) {
		t.Fatalf("createsDuplicates(plain function) = false, want true")
	}
}

// TestCardinalityKeyExpression_WireRoundTrip is the wire-compat pin: a
// cardinality index key serialises to the Function proto (field 9, name
// "cardinality") byte-identically to Java, and deserialises back to a
// CardinalityFunctionKeyExpression (not a bare FunctionKeyExpression) so its
// fast paths and createsDuplicates==false survive a catalog round-trip.
func TestCardinalityKeyExpression_WireRoundTrip(t *testing.T) {
	t.Parallel()
	expr := CardinalityExpr(FieldConcatenate("arr"))

	pb := expr.ToKeyExpression()
	if pb.GetFunction() == nil {
		t.Fatalf("ToKeyExpression did not produce a Function proto: %+v", pb)
	}
	if pb.GetFunction().GetName() != FunctionNameCardinality {
		t.Fatalf("Function.name = %q, want %q", pb.GetFunction().GetName(), FunctionNameCardinality)
	}
	// The argument must be the Concatenate field (Java's
	// field("arr", Concatenate)).
	argField := pb.GetFunction().GetArguments().GetField()
	if argField == nil || argField.GetFieldName() != "arr" {
		t.Fatalf("Function.arguments is not field(arr): %+v", pb.GetFunction().GetArguments())
	}

	// Round-trip the bytes and re-deserialise.
	raw, err := proto.Marshal(pb)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pb2 := &gen.KeyExpression{}
	if err := proto.Unmarshal(raw, pb2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	back, err := KeyExpressionFromProto(pb2)
	if err != nil {
		t.Fatalf("KeyExpressionFromProto: %v", err)
	}
	card, ok := back.(*CardinalityFunctionKeyExpression)
	if !ok {
		t.Fatalf("deserialised type = %T, want *CardinalityFunctionKeyExpression", back)
	}
	if card.Name() != FunctionNameCardinality {
		t.Fatalf("round-tripped name = %q, want %q", card.Name(), FunctionNameCardinality)
	}
	// createsDuplicates must survive the round-trip.
	if createsDuplicates(card) {
		t.Fatalf("round-tripped createsDuplicates = true, want false")
	}
	// The re-serialised proto must be byte-identical (wire stability).
	raw2, err := proto.Marshal(card.ToKeyExpression())
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(raw) != string(raw2) {
		t.Fatalf("re-serialised bytes differ:\n first=%x\nsecond=%x", raw, raw2)
	}
}
