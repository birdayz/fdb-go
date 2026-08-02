package executor

import (
	"bytes"
	"math"
	"testing"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// TestPackedDedupKey_CompositeLossless pins RFC-180 C1/C3: composite slots
// encode via the continuation codec, not a rendered string — the %T:%v form
// collided on []any{"a b"} vs []any{"a","b"} (both rendered "[a b]"),
// merging distinct rows under DISTINCT/GROUP BY. RED pre-fix.
func TestPackedDedupKey_CompositeLossless(t *testing.T) {
	t.Parallel()
	a, errA := packedDedupKey([]any{[]any{"a b"}})
	b, errB := packedDedupKey([]any{[]any{"a", "b"}})
	if errA != nil || errB != nil {
		t.Fatalf("composite slots must encode: %v %v", errA, errB)
	}
	if a == b {
		t.Fatal("distinct composite rows must produce distinct dedup keys")
	}
	// Equal composites still collide (same key) — dedup correctness.
	c, _ := packedDedupKey([]any{[]any{"a", "b"}})
	if b != c {
		t.Fatal("equal composite rows must produce equal dedup keys")
	}

	// The recursive encoding is an implementation detail, not a BYTES value.
	// Its exact bytes must not let a user forge the composite's outer type.
	encodedComposite, err := appendDistinctValue(nil, []any{"a", "b"})
	if err != nil {
		t.Fatalf("encode composite directly: %v", err)
	}
	bytesKey := mustPackedDedupKey(t, []any{encodedComposite})
	if b == bytesKey {
		t.Fatal("ARRAY value must not collide with a BYTES value containing its recursive encoding")
	}
}

func TestPackedDedupKeyCanonicalizesNaNsButPreservesSignedZero(t *testing.T) {
	t.Parallel()
	nan64A := math.Float64frombits(0x7ff8000000000001)
	nan64B := math.Float64frombits(0xfff123456789abcd)
	key64A, err := packedDedupKey([]any{nan64A})
	if err != nil {
		t.Fatalf("pack first float64 NaN: %v", err)
	}
	key64B, err := packedDedupKey([]any{nan64B})
	if err != nil {
		t.Fatalf("pack second float64 NaN: %v", err)
	}
	if key64A != key64B {
		t.Fatal("float64 NaN sign/payload variants must be one DISTINCT value")
	}

	nan32A := math.Float32frombits(0x7fc00001)
	nan32B := math.Float32frombits(0xffd23456)
	key32A, err := packedDedupKey([]any{nan32A})
	if err != nil {
		t.Fatalf("pack first float32 NaN: %v", err)
	}
	key32B, err := packedDedupKey([]any{nan32B})
	if err != nil {
		t.Fatalf("pack second float32 NaN: %v", err)
	}
	if key32A != key32B {
		t.Fatal("float32 NaN sign/payload variants must be one DISTINCT value")
	}

	negZero := math.Copysign(0, -1)
	negZeroKey, err := packedDedupKey([]any{negZero})
	if err != nil {
		t.Fatalf("pack -0.0: %v", err)
	}
	posZeroKey, err := packedDedupKey([]any{float64(0)})
	if err != nil {
		t.Fatalf("pack +0.0: %v", err)
	}
	if negZeroKey == posZeroKey {
		t.Fatal("signed zeros must remain distinct DISTINCT values")
	}
}

func TestPackedDedupKeyNestedArraysCanonicalizeNaNsButPreserveSignedZero(t *testing.T) {
	t.Parallel()

	nan32A := math.Float32frombits(0x7fc00001)
	nan32B := math.Float32frombits(0xffd23456)
	nan64A := math.Float64frombits(0x7ff8000000000001)
	nan64B := math.Float64frombits(0xfff123456789abcd)
	left := []any{[]any{nan32A, []any{int64(7), nan64A}}}
	right := []any{[]any{nan32B, []any{int64(7), nan64B}}}
	leftKey := mustPackedDedupKey(t, left)
	rightKey := mustPackedDedupKey(t, right)
	if leftKey != rightKey {
		t.Fatal("NaN sign/payload variants must canonicalize recursively inside ARRAY values")
	}

	// The continuation codec has the opposite job: it must reconstruct the exact
	// raw values and therefore must not inherit DISTINCT's NaN normalization.
	leftContinuation, err := appendContValue(nil, left[0])
	if err != nil {
		t.Fatalf("encode first continuation value: %v", err)
	}
	rightContinuation, err := appendContValue(nil, right[0])
	if err != nil {
		t.Fatalf("encode second continuation value: %v", err)
	}
	if bytes.Equal(leftContinuation, rightContinuation) {
		t.Fatal("continuation encoding must preserve nested NaN sign/payload bits")
	}

	zeroCases := []struct {
		name  string
		minus []any
		plus  []any
	}{
		{
			name:  "float32",
			minus: []any{[]any{math.Float32frombits(0x80000000)}},
			plus:  []any{[]any{float32(0)}},
		},
		{
			name:  "float64 deeply nested",
			minus: []any{[]any{[]any{math.Copysign(0, -1)}}},
			plus:  []any{[]any{[]any{float64(0)}}},
		},
	}
	for _, test := range zeroCases {
		t.Run(test.name, func(t *testing.T) {
			minusKey := mustPackedDedupKey(t, test.minus)
			plusKey := mustPackedDedupKey(t, test.plus)
			if minusKey == plusKey {
				t.Fatal("nested signed zeros must remain distinct DISTINCT values")
			}
		})
	}
}

func TestPackedDedupKeyProtoCanonicalizesNaNsRecursively(t *testing.T) {
	t.Parallel()

	schema := newDistinctFloatProtoSchema(t, "executor.distinct.nested")
	nan32A := math.Float32frombits(0x7fc00001)
	nan32B := math.Float32frombits(0xffd23456)
	nan64A := math.Float64frombits(0x7ff8000000000001)
	nan64B := math.Float64frombits(0xfff123456789abcd)
	left := schema.newMessage(nan32A, nan64A, false)
	right := schema.newMessage(nan32B, nan64B, true)

	// Pin non-mutation: canonicalization happens on a clone, never on the row's
	// live STRUCT value. Check both widths in an ordinary nested field and in a
	// scalar-valued map before and after key construction.
	leftNested := left.Get(schema.root.Fields().ByName("nested")).Message()
	leftFloatBefore := math.Float32bits(float32(leftNested.Get(schema.nested.Fields().ByName("f")).Float()))
	leftDoubleBefore := math.Float64bits(leftNested.Get(schema.nested.Fields().ByName("d")).Float())
	leftMapBefore := math.Float32bits(float32(left.Get(schema.root.Fields().ByName("float_map")).Map().
		Get(protoreflect.ValueOfString("nan").MapKey()).Float()))

	leftKey := mustPackedDedupKey(t, []any{[]any{left}})
	rightKey := mustPackedDedupKey(t, []any{[]any{right}})
	if leftKey != rightKey {
		t.Fatal("nested/repeated/map/oneof protobuf NaN variants must be one DISTINCT value")
	}
	if got := math.Float32bits(float32(leftNested.Get(schema.nested.Fields().ByName("f")).Float())); got != leftFloatBefore {
		t.Fatalf("DISTINCT key mutated nested float32 input: got bits %#x, want %#x", got, leftFloatBefore)
	}
	if got := math.Float64bits(leftNested.Get(schema.nested.Fields().ByName("d")).Float()); got != leftDoubleBefore {
		t.Fatalf("DISTINCT key mutated nested float64 input: got bits %#x, want %#x", got, leftDoubleBefore)
	}
	if got := math.Float32bits(float32(left.Get(schema.root.Fields().ByName("float_map")).Map().
		Get(protoreflect.ValueOfString("nan").MapKey()).Float())); got != leftMapBefore {
		t.Fatalf("DISTINCT key mutated map float32 input: got bits %#x, want %#x", got, leftMapBefore)
	}

	// Deterministic protobuf serialization must make the key stable despite map
	// iteration order. The two messages above deliberately inserted map entries
	// in opposite orders; repeated calls also exercise clone + traversal afresh.
	for i := 0; i < 100; i++ {
		if got := mustPackedDedupKey(t, []any{[]any{left}}); got != leftKey {
			t.Fatalf("protobuf DISTINCT key changed on repetition %d", i)
		}
	}

	leftContinuation, err := appendContValue(nil, left)
	if err != nil {
		t.Fatalf("encode first proto continuation value: %v", err)
	}
	rightContinuation, err := appendContValue(nil, right)
	if err != nil {
		t.Fatalf("encode second proto continuation value: %v", err)
	}
	if bytes.Equal(leftContinuation, rightContinuation) {
		t.Fatal("continuation encoding must preserve protobuf NaN sign/payload bits")
	}

	minusZero := schema.newMessage(math.Float32frombits(0x80000000), math.Copysign(0, -1), false)
	plusZero := schema.newMessage(float32(0), float64(0), true)
	if mustPackedDedupKey(t, []any{minusZero}) == mustPackedDedupKey(t, []any{plusZero}) {
		t.Fatal("signed zeros inside nested/repeated/map/oneof protobuf fields must remain distinct")
	}
}

func TestPackedDedupKeyProtoCanonicalAcrossGeneratedAndDynamicRepresentations(t *testing.T) {
	t.Parallel()

	nan32A := math.Float32frombits(0x7fc00001)
	nan64A := math.Float64frombits(0x7ff8000000000001)
	generated := &gen.TypedRecord{ValFloat: &nan32A, ValDouble: &nan64A}

	nan32B := math.Float32frombits(0xffd23456)
	nan64B := math.Float64frombits(0xfff123456789abcd)
	dynamic := dynamicpb.NewMessage(generated.ProtoReflect().Descriptor())
	dynamic.Set(dynamic.Descriptor().Fields().ByName("val_float"), protoreflect.ValueOfFloat32(nan32B))
	dynamic.Set(dynamic.Descriptor().Fields().ByName("val_double"), protoreflect.ValueOfFloat64(nan64B))

	if mustPackedDedupKey(t, []any{generated}) != mustPackedDedupKey(t, []any{dynamic}) {
		t.Fatal("generated and dynamic representations of the same protobuf STRUCT/NaNs must share a DISTINCT key")
	}
	if math.Float32bits(*generated.ValFloat) != math.Float32bits(nan32A) ||
		math.Float64bits(*generated.ValDouble) != math.Float64bits(nan64A) {
		t.Fatal("protobuf DISTINCT key construction mutated the generated input")
	}
	if got := math.Float32bits(float32(dynamic.Get(dynamic.Descriptor().Fields().ByName("val_float")).Float())); got != math.Float32bits(nan32B) {
		t.Fatalf("protobuf DISTINCT key construction mutated the dynamic input: got bits %#x", got)
	}

	negative32 := math.Float32frombits(0x80000000)
	negative64 := math.Copysign(0, -1)
	negative := &gen.TypedRecord{ValFloat: &negative32, ValDouble: &negative64}
	positive := dynamicpb.NewMessage(generated.ProtoReflect().Descriptor())
	positive.Set(positive.Descriptor().Fields().ByName("val_float"), protoreflect.ValueOfFloat32(0))
	positive.Set(positive.Descriptor().Fields().ByName("val_double"), protoreflect.ValueOfFloat64(0))
	if mustPackedDedupKey(t, []any{negative}) == mustPackedDedupKey(t, []any{positive}) {
		t.Fatal("generated/dynamic normalization must not collapse protobuf signed zeros")
	}
}

func TestPackedDedupKeyTypedNilProtoFailsLoudly(t *testing.T) {
	t.Parallel()

	var typedNil *gen.TypedRecord
	if _, err := packedDedupKey([]any{typedNil}); err == nil {
		t.Fatal("typed-nil protobuf STRUCT must return an error, not panic or encode ambiguously")
	}
}

func mustPackedDedupKey(t testing.TB, slots []any) string {
	t.Helper()
	key, err := packedDedupKey(slots)
	if err != nil {
		t.Fatalf("packedDedupKey: %v", err)
	}
	return key
}

type distinctFloatProtoSchema struct {
	root   protoreflect.MessageDescriptor
	nested protoreflect.MessageDescriptor
}

func newDistinctFloatProtoSchema(t testing.TB, packageName string) distinctFloatProtoSchema {
	t.Helper()

	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	floatType := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
	doubleType := descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	stringType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	messageType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	field := func(
		name string,
		number int32,
		label descriptorpb.FieldDescriptorProto_Label,
		fieldType descriptorpb.FieldDescriptorProto_Type,
		typeName string,
		oneof *int32,
	) *descriptorpb.FieldDescriptorProto {
		result := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(number),
			Label:  label.Enum(),
			Type:   fieldType.Enum(),
		}
		if typeName != "" {
			result.TypeName = proto.String(typeName)
		}
		result.OneofIndex = oneof
		return result
	}
	mapEntry := func(name string, valueType descriptorpb.FieldDescriptorProto_Type, valueTypeName string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name:    proto.String(name),
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			Field: []*descriptorpb.FieldDescriptorProto{
				field("key", 1, optional, stringType, "", nil),
				field("value", 2, optional, valueType, valueTypeName, nil),
			},
		}
	}

	nestedName := "." + packageName + ".Nested"
	rootName := "." + packageName + ".Root"
	floatChoice := int32(0)
	doubleChoice := int32(1)
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("distinct_nested_float_test.proto"),
		Package: proto.String(packageName),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Nested"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("f", 1, optional, floatType, "", nil),
					field("d", 2, optional, doubleType, "", nil),
					field("fs", 3, repeated, floatType, "", nil),
					field("ds", 4, repeated, doubleType, "", nil),
				},
			},
			{
				Name: proto.String("Root"),
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: proto.String("float_choice")},
					{Name: proto.String("double_choice")},
				},
				Field: []*descriptorpb.FieldDescriptorProto{
					field("nested", 1, optional, messageType, nestedName, nil),
					field("children", 2, repeated, messageType, nestedName, nil),
					field("float_map", 3, repeated, messageType, rootName+".FloatMapEntry", nil),
					field("double_map", 4, repeated, messageType, rootName+".DoubleMapEntry", nil),
					field("nested_map", 5, repeated, messageType, rootName+".NestedMapEntry", nil),
					field("choice_float", 6, optional, floatType, "", &floatChoice),
					field("choice_double", 7, optional, doubleType, "", &doubleChoice),
				},
				NestedType: []*descriptorpb.DescriptorProto{
					mapEntry("FloatMapEntry", floatType, ""),
					mapEntry("DoubleMapEntry", doubleType, ""),
					mapEntry("NestedMapEntry", messageType, nestedName),
				},
			},
		},
	}
	fileDescriptor, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("build DISTINCT float test descriptor: %v", err)
	}
	return distinctFloatProtoSchema{
		root:   fileDescriptor.Messages().ByName("Root"),
		nested: fileDescriptor.Messages().ByName("Nested"),
	}
}

func (schema distinctFloatProtoSchema) newMessage(f32 float32, f64 float64, reverseMaps bool) *dynamicpb.Message {
	fillNested := func(message protoreflect.Message) {
		message.Set(schema.nested.Fields().ByName("f"), protoreflect.ValueOfFloat32(f32))
		message.Set(schema.nested.Fields().ByName("d"), protoreflect.ValueOfFloat64(f64))
		message.Mutable(schema.nested.Fields().ByName("fs")).List().Append(protoreflect.ValueOfFloat32(f32))
		message.Mutable(schema.nested.Fields().ByName("ds")).List().Append(protoreflect.ValueOfFloat64(f64))
	}

	root := dynamicpb.NewMessage(schema.root)
	fillNested(root.Mutable(schema.root.Fields().ByName("nested")).Message())
	child := dynamicpb.NewMessage(schema.nested)
	fillNested(child)
	root.Mutable(schema.root.Fields().ByName("children")).List().Append(protoreflect.ValueOfMessage(child))

	keys := []string{"nan", "finite"}
	if reverseMaps {
		keys[0], keys[1] = keys[1], keys[0]
	}
	floatMap := root.Mutable(schema.root.Fields().ByName("float_map")).Map()
	doubleMap := root.Mutable(schema.root.Fields().ByName("double_map")).Map()
	nestedMap := root.Mutable(schema.root.Fields().ByName("nested_map")).Map()
	for _, key := range keys {
		mapKey := protoreflect.ValueOfString(key).MapKey()
		mapF32, mapF64 := f32, f64
		if key == "finite" {
			mapF32, mapF64 = 1.25, -9.5
		}
		floatMap.Set(mapKey, protoreflect.ValueOfFloat32(mapF32))
		doubleMap.Set(mapKey, protoreflect.ValueOfFloat64(mapF64))
		nestedValue := dynamicpb.NewMessage(schema.nested)
		fillNested(nestedValue)
		nestedMap.Set(mapKey, protoreflect.ValueOfMessage(nestedValue))
	}
	root.Set(schema.root.Fields().ByName("choice_float"), protoreflect.ValueOfFloat32(f32))
	root.Set(schema.root.Fields().ByName("choice_double"), protoreflect.ValueOfFloat64(f64))
	return root
}
