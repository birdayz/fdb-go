package values

// A captured exact type must accept every scalar the STORED-row mapper can
// produce, because the mapper is what typed the row in the first place.
//
// protoScalarShapeCompatible was a second copy of that mapping and the copies
// drifted. The mapper carries every unsigned and fixed integer as LONG (the
// runtime materializer emits int64) and carries an alias-bearing enum as LONG
// too, because EnumType refuses number aliases. The copy had a case for
// neither, so both fell to its default and answered "incompatible" — and the
// caller reads that as "this descriptor disagrees with the captured shape",
// which rejects the WHOLE protobuf row before any ordinal is read.
//
// This is wire-compat territory, not tidiness: Java writes uint64 columns, and
// a Go reader that refuses the record cannot share the cluster.

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func scalarShapeDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("scalar_shape_test.proto"),
		Package: proto.String("fdb.test.scalarshape"),
		Syntax:  proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{
				Name: proto.String("PLAIN"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("P_A"), Number: proto.Int32(0)},
					{Name: proto.String("P_B"), Number: proto.Int32(1)},
				},
			},
			{
				// allow_alias: two names share number 1, which EnumType refuses,
				// so the mapper carries this as LONG.
				Name:    proto.String("ALIASED"),
				Options: &descriptorpb.EnumOptions{AllowAlias: proto.Bool(true)},
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("A_ZERO"), Number: proto.Int32(0)},
					{Name: proto.String("A_ONE"), Number: proto.Int32(1)},
					{Name: proto.String("A_ONE_ALIAS"), Number: proto.Int32(1)},
				},
			},
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("SCALARS"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("U32"), Number: proto.Int32(1), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_UINT32.Enum()},
				{Name: proto.String("U64"), Number: proto.Int32(2), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()},
				{Name: proto.String("F32"), Number: proto.Int32(3), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_FIXED32.Enum()},
				{Name: proto.String("F64"), Number: proto.Int32(4), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_FIXED64.Enum()},
				{Name: proto.String("I64"), Number: proto.Int32(5), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
				{Name: proto.String("S"), Number: proto.Int32(6), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: proto.String("PE"), Number: proto.Int32(7), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".fdb.test.scalarshape.PLAIN")},
				{Name: proto.String("AE"), Number: proto.Int32(8), Label: opt, Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".fdb.test.scalarshape.ALIASED")},
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	return fd.Messages().ByName("SCALARS")
}

// Whatever the stored-row mapper says a column IS, the shape check must accept.
// Driven off the mapper rather than a hand-written table, so the two cannot
// drift apart again the way they already did once.
func TestProtoScalarShapeAcceptsEveryTypeTheMapperProduces(t *testing.T) {
	t.Parallel()
	md := scalarShapeDescriptor(t)
	if md.Fields().Len() == 0 {
		t.Fatal("fixture has no fields; the loop below would prove nothing")
	}
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		mapped := FieldTypeForProtoField(fd)
		if IsUnresolved(mapped) {
			t.Fatalf("%s: the mapper itself declined this column, so the fixture is wrong", fd.Name())
		}
		handle, err := SnapshotExactType(mapped)
		if err != nil {
			t.Fatalf("%s: the mapper's own type is not exact: %v", fd.Name(), err)
		}
		exact, ok := handle.(*exactType)
		if !ok {
			t.Fatalf("%s: unexpected handle %T", fd.Name(), handle)
		}
		if !protoFieldShapeCompatible(fd, exact) {
			t.Errorf("%s (%v): the shape check REJECTED the type the stored-row mapper"+
				" produced for it (%v). The caller reads that as a descriptor/shape"+
				" disagreement and refuses the entire row.", fd.Name(), fd.Kind(), mapped)
		}
	}
}

// The two kinds the old hand-written switch had no case for — an unsigned
// integer and an alias-bearing enum — asserted separately rather than as one
// table, so a failure names which axis regressed instead of just "a kind".
func TestProtoScalarShapeUnsignedAndAliasedEnum(t *testing.T) {
	t.Parallel()
	md := scalarShapeDescriptor(t)
	longType := &exactType{code: TypeCodeLong, nullable: true}
	longType.finishCanonical()

	for _, name := range []string{"U32", "U64", "F32", "F64"} {
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("fixture lacks %s", name)
		}
		if !protoScalarShapeCompatible(fd, longType) {
			t.Errorf("%s: an unsigned/fixed column was not accepted as LONG, so a row"+
				" containing it is rejected outright — Java writes these", name)
		}
	}

	aliased := md.Fields().ByName("AE")
	if aliased == nil {
		t.Fatal("fixture lacks AE")
	}
	if !protoScalarShapeCompatible(aliased, longType) {
		t.Error("an allow_alias enum was not accepted as LONG, though the mapper carries" +
			" it as LONG precisely because EnumType refuses number aliases")
	}

	// A PLAIN enum must still be an ENUM, not silently widened to LONG.
	plain := md.Fields().ByName("PE")
	if protoScalarShapeCompatible(plain, longType) {
		t.Error("a non-aliased enum was accepted as LONG; the alias carve-out must not" +
			" swallow ordinary enums")
	}
}

// The deliberate storage aliases survive, and nothing wider sneaks in with them.
func TestProtoScalarShapeKeepsOnlyTheIntendedStorageAliases(t *testing.T) {
	t.Parallel()
	md := scalarShapeDescriptor(t)
	mk := func(code TypeCode) *exactType {
		e := &exactType{code: code, nullable: true}
		e.finishCanonical()
		return e
	}
	i64 := md.Fields().ByName("I64")
	s := md.Fields().ByName("S")

	if !protoScalarShapeCompatible(i64, mk(TypeCodeVersion)) {
		t.Error("VERSION over an int64 column was refused; that alias is intentional")
	}
	for _, code := range []TypeCode{TypeCodeDate, TypeCodeTimestamp, TypeCodeString} {
		if !protoScalarShapeCompatible(s, mk(code)) {
			t.Errorf("%v over a string column was refused; DATE/TIMESTAMP store as STRING", code)
		}
	}
	// Negative controls: the aliases are one-directional and narrow.
	if protoScalarShapeCompatible(s, mk(TypeCodeLong)) {
		t.Error("LONG was accepted over a STRING column")
	}
	if protoScalarShapeCompatible(i64, mk(TypeCodeTimestamp)) {
		t.Error("TIMESTAMP was accepted over an int64 column; it stores as STRING")
	}
	if protoScalarShapeCompatible(i64, mk(TypeCodeInt)) {
		t.Error("INT was accepted over an int64 column; width is not an alias")
	}
}
