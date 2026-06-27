package metadata

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/relational/api"
)

func TestIsFieldNullable_ProtoCardinality(t *testing.T) {
	t.Parallel()

	// Demo proto uses proto2 with optional fields — all should be
	// nullable.
	orderMD := gen.File_record_layer_demo_proto.Messages().ByName("Order")
	for i := 0; i < orderMD.Fields().Len(); i++ {
		fd := orderMD.Fields().Get(i)
		got := isFieldNullable(fd)
		want := fd.Cardinality() != protoreflect.Required
		if got != want {
			t.Errorf("Order.%s: isFieldNullable = %v, want %v (cardinality=%v)",
				fd.Name(), got, want, fd.Cardinality())
		}
	}

	// Synthesize a proto2 REQUIRED field and verify we report
	// nullable=false.
	required := descriptorpb.FieldDescriptorProto_LABEL_REQUIRED
	strType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	fd := stringField("req", 1, strType, required)
	md := buildMessageDesc(t, "Msg", []*descriptorpb.FieldDescriptorProto{fd})
	if isFieldNullable(md.Fields().Get(0)) {
		t.Error("proto2 REQUIRED field reported nullable=true, want false")
	}
	// End-to-end: the nullability flag must also propagate through
	// protoFieldToDataType. Guards against a future refactor quietly
	// dropping the nullable argument somewhere in the call chain.
	dt, err := protoFieldToDataType(md.Fields().Get(0))
	if err != nil {
		t.Fatalf("protoFieldToDataType(REQUIRED): %v", err)
	}
	if dt.IsNullable() {
		t.Error("protoFieldToDataType(REQUIRED) produced nullable=true, want false")
	}
}

func TestIsUUIDDescriptor(t *testing.T) {
	t.Parallel()

	uuidMD := (&gen.UUID{}).ProtoReflect().Descriptor()
	if string(uuidMD.FullName()) != uuidFullName {
		t.Fatalf("gen.UUID FullName = %q, want %q (proto regen?)",
			uuidMD.FullName(), uuidFullName)
	}
	if !isUUIDDescriptor(uuidMD) {
		t.Error("isUUIDDescriptor(gen.UUID) = false, want true")
	}

	// Non-UUID message must fail the check.
	orderMD := gen.File_record_layer_demo_proto.Messages().ByName("Order")
	if orderMD == nil {
		t.Fatal("Order descriptor missing from demo proto")
	}
	if isUUIDDescriptor(orderMD) {
		t.Error("isUUIDDescriptor(Order) = true, want false")
	}
}

func TestUnwrapWrappedArray_MatchesJavaPattern(t *testing.T) {
	t.Parallel()

	// Build a synthetic descriptor matching Java's nullable-array
	// wrapper shape: message Wrapper { repeated int32 values = 1; }
	wrapper := buildMessageDesc(t, "Wrapper", []*descriptorpb.FieldDescriptorProto{
		stringField("values", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32,
			descriptorpb.FieldDescriptorProto_LABEL_REPEATED),
	})

	dt, ok := unwrapWrappedArray(wrapper)
	if !ok {
		t.Fatal("unwrapWrappedArray returned ok=false on canonical wrapped-array shape")
	}
	arr, ok := dt.(*api.ArrayType)
	if !ok {
		t.Fatalf("result %T, want *ArrayType", dt)
	}
	if _, ok := arr.ElementType().(*api.IntegerType); !ok {
		t.Errorf("element type %T, want *IntegerType", arr.ElementType())
	}
}

func TestUnwrapWrappedArray_RejectsNonMatching(t *testing.T) {
	t.Parallel()

	// Wrong field name (not "values").
	wrongName := buildMessageDesc(t, "NotWrapper", []*descriptorpb.FieldDescriptorProto{
		stringField("items", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32,
			descriptorpb.FieldDescriptorProto_LABEL_REPEATED),
	})
	if _, ok := unwrapWrappedArray(wrongName); ok {
		t.Error("unwrapWrappedArray accepted wrong field name")
	}

	// Non-repeated field.
	singular := buildMessageDesc(t, "Single", []*descriptorpb.FieldDescriptorProto{
		stringField("values", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32,
			descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
	})
	if _, ok := unwrapWrappedArray(singular); ok {
		t.Error("unwrapWrappedArray accepted non-repeated field")
	}

	// Two fields — not a single-field wrapper.
	twoFields := buildMessageDesc(t, "Two", []*descriptorpb.FieldDescriptorProto{
		stringField("values", 1, descriptorpb.FieldDescriptorProto_TYPE_INT32,
			descriptorpb.FieldDescriptorProto_LABEL_REPEATED),
		stringField("extra", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING,
			descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL),
	})
	if _, ok := unwrapWrappedArray(twoFields); ok {
		t.Error("unwrapWrappedArray accepted two-field message")
	}

	// Regular non-wrapper messages must still be rejected.
	orderMD := gen.File_record_layer_demo_proto.Messages().ByName("Order")
	if _, ok := unwrapWrappedArray(orderMD); ok {
		t.Error("unwrapWrappedArray accepted Order (multi-field real table)")
	}

	// Map field shaped like "map<string,int32> values = 1;" — a
	// repeated synthetic message field named "values" that would
	// otherwise match the wrapper shape. Must be rejected so maps
	// still route to the UnresolvedType("map") path.
	mapMD := buildMapValuesMessage(t)
	if _, ok := unwrapWrappedArray(mapMD); ok {
		t.Error("unwrapWrappedArray accepted a map<...> values field")
	}
}

// buildMapValuesMessage constructs
//
//	message Wrapper { map<string, int32> values = 1; }
//
// protoreflect models this as a repeated synthetic nested message
// named values whose own shape has key / value fields.
func buildMapValuesMessage(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	fileName := "test_wrapper_map.proto"
	pkg := "test.wrapper"
	syntax := "proto2"
	// Synthetic entry message for the map.
	entryName := "ValuesEntry"
	entryMapEntry := true
	keyName, valName := "key", "value"
	keyNum, valNum := int32(1), int32(2)
	strType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	i32Type := descriptorpb.FieldDescriptorProto_TYPE_INT32
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE

	entry := &descriptorpb.DescriptorProto{
		Name: &entryName,
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: &keyName, Number: &keyNum, Type: &strType, Label: &optional},
			{Name: &valName, Number: &valNum, Type: &i32Type, Label: &optional},
		},
		Options: &descriptorpb.MessageOptions{MapEntry: &entryMapEntry},
	}
	wrapperName := "Wrapper"
	valuesName := "values"
	valuesNum := int32(1)
	valuesTypeName := ".test.wrapper.Wrapper.ValuesEntry"
	wrapper := &descriptorpb.DescriptorProto{
		Name:       &wrapperName,
		NestedType: []*descriptorpb.DescriptorProto{entry},
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     &valuesName,
				Number:   &valuesNum,
				Type:     &msgType,
				TypeName: &valuesTypeName,
				Label:    &repeated,
			},
		},
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:        &fileName,
		Package:     &pkg,
		Syntax:      &syntax,
		MessageType: []*descriptorpb.DescriptorProto{wrapper},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().Get(0)
}

// stringField builds a FieldDescriptorProto for use with buildMessageDesc.
func stringField(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
	n, t, l := name, typ, label
	number := num
	return &descriptorpb.FieldDescriptorProto{
		Name:   &n,
		Number: &number,
		Type:   &t,
		Label:  &l,
	}
}

// buildMessageDesc assembles a standalone protoreflect.MessageDescriptor
// from its field list. Used only by tests in this file.
func buildMessageDesc(t *testing.T, name string, fields []*descriptorpb.FieldDescriptorProto) protoreflect.MessageDescriptor {
	t.Helper()
	syntax := "proto2"
	fileName := "test_wrapper_" + name + ".proto"
	pkg := "test.wrapper"
	file := &descriptorpb.FileDescriptorProto{
		Name:    &fileName,
		Package: &pkg,
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: &name, Field: fields},
		},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return fd.Messages().Get(0)
}

func TestMessageTypeFromDescriptor_RecursiveMessageTerminates(t *testing.T) {
	t.Parallel()

	// Build `message Tree { repeated Tree children = 1; }` — without the
	// visited-set cycle check, messageTypeFromDescriptor would recurse
	// into children's element type forever and blow the goroutine stack.
	fileName := "test_recursive.proto"
	pkg := "test.recursive"
	syntax := "proto2"
	treeName := "Tree"
	childrenName := "children"
	childrenNum := int32(1)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	treeTypeName := ".test.recursive.Tree"

	tree := &descriptorpb.DescriptorProto{
		Name: &treeName,
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     &childrenName,
				Number:   &childrenNum,
				Type:     &msgType,
				TypeName: &treeTypeName,
				Label:    &repeated,
			},
		},
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:        &fileName,
		Package:     &pkg,
		Syntax:      &syntax,
		MessageType: []*descriptorpb.DescriptorProto{tree},
	}
	fd, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}

	treeMD := fd.Messages().Get(0)
	st, err := messageTypeFromDescriptor(treeMD, true)
	if err != nil {
		t.Fatalf("messageTypeFromDescriptor(Tree): %v", err)
	}
	if st.NumFields() != 1 {
		t.Fatalf("Tree struct field count = %d, want 1", st.NumFields())
	}
	// The children field is repeated → ArrayType(inner); the inner
	// should be the Unresolved placeholder because entering Tree a
	// second time hits the cycle guard.
	childDT := st.Fields()[0].Type()
	arr, ok := childDT.(*api.ArrayType)
	if !ok {
		t.Fatalf("children type %T, want *ArrayType", childDT)
	}
	if _, ok := arr.ElementType().(*api.UnresolvedType); !ok {
		t.Errorf("children element type %T, want *UnresolvedType (cycle placeholder)", arr.ElementType())
	}
}

func TestMessageTypeFromDescriptor_UUIDFallbackStructShape(t *testing.T) {
	t.Parallel()

	// If anyone ever routes a UUID descriptor through
	// messageTypeFromDescriptor directly (bypassing the UUID
	// short-circuit), the result is a 2-field struct — this pins the
	// expected shape so callers know what the non-short-circuit path
	// produces.
	uuidMD := (&gen.UUID{}).ProtoReflect().Descriptor()
	st, err := messageTypeFromDescriptor(uuidMD, true)
	if err != nil {
		t.Fatalf("messageTypeFromDescriptor(UUID): %v", err)
	}
	if st.NumFields() != 2 {
		t.Errorf("UUID struct field count = %d, want 2", st.NumFields())
	}
}

// FuzzMessageTypeFromDescriptor feeds arbitrary FileDescriptorProto bytes
// through protodesc.NewFile + messageTypeFromDescriptor. The walk recurses
// through nested-message field types; without the visited-set added in
// swingshift-35, a fuzzer-crafted self-referential shape would stack-overflow.
// This fuzz pins that guard along with the proto-unmarshal + descriptor
// resolution paths.
func FuzzMessageTypeFromDescriptor(f *testing.F) {
	// Seed 1: a minimal valid proto2 file with one empty message.
	syntax := "proto2"
	msgName := "M"
	seed1 := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("seed1.proto"),
		Package: proto.String("test"),
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: &msgName},
		},
	}
	if b, err := proto.Marshal(seed1); err == nil {
		f.Add(b)
	}
	// Seed 2: self-referential message (the exact class of bug the
	// visited-set guards against).
	childrenName := "children"
	childrenNum := int32(1)
	repeated := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	treeTypeName := ".test.M"
	seed2 := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("seed2.proto"),
		Package: proto.String("test"),
		Syntax:  &syntax,
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: &msgName,
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     &childrenName,
						Number:   &childrenNum,
						Type:     &msgType,
						TypeName: &treeTypeName,
						Label:    &repeated,
					},
				},
			},
		},
	}
	if b, err := proto.Marshal(seed2); err == nil {
		f.Add(b)
	}
	// Pathological bytes.
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, blob []byte) {
		fdp := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(blob, fdp); err != nil {
			return
		}
		fd, err := protodesc.NewFile(fdp, nil)
		if err != nil {
			return
		}
		for i := 0; i < fd.Messages().Len(); i++ {
			md := fd.Messages().Get(i)
			// Must not panic / stack-overflow. A non-nil error is fine;
			// a (nil, nil) pair is the forbidden state.
			st, err := messageTypeFromDescriptor(md, true)
			if err != nil {
				continue
			}
			if st == nil {
				t.Fatalf("messageTypeFromDescriptor returned (nil, nil) for %s", md.FullName())
			}
		}
	})
}
