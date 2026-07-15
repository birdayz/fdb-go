package values

import (
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
