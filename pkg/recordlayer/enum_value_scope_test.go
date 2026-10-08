package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/protoscope"
)

// sharedEnumValueMetaData is the MetaData Java's relational DDL stores for
// `create type as enum e1('X', 'Y') create type as enum e2('Y', 'Z')
// create table t(id bigint, a e1, b e2, primary key(id))` (the conformance
// shape enums_sharing_value_name, accepted and stored by the target): two
// top-level enums that share the value name Y. protobuf-java scopes an enum
// value under its enum type, so the file is valid there; protoc and
// protobuf-go scope it beside the enum, at file level, where the two Ys
// collide.
func sharedEnumValueMetaData() *gen.MetaData {
	return enumScopeMetaData([]string{"E1", "E2"}, [][]string{{"X", "Y"}, {"Y", "Z"}})
}

// selfNamedEnumValueMetaData is what Java's relational DDL stores for
// `create type as enum status('STATUS', 'DONE') create table t(id bigint,
// a status, primary key(id))` (the conformance shape enum_value_named_like_enum):
// protobuf-java scopes the value as STATUS.STATUS, protobuf-go beside the enum,
// where it collides with the enum's own name.
func selfNamedEnumValueMetaData() *gen.MetaData {
	return enumScopeMetaData([]string{"STATUS"}, [][]string{{"STATUS", "DONE"}})
}

// enumScopeMetaData is a one-table records file whose fields A, B, … hold the
// named top-level enums, in order.
func enumScopeMetaData(names []string, values [][]string) *gen.MetaData {
	opt := func(name string, n int32, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(n),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: typ.Enum(),
		}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	enum := func(name string, values ...string) *descriptorpb.EnumDescriptorProto {
		e := &descriptorpb.EnumDescriptorProto{Name: proto.String(name)}
		for i, v := range values {
			e.Value = append(e.Value, &descriptorpb.EnumValueDescriptorProto{Name: proto.String(v), Number: proto.Int32(int32(i))})
		}
		return e
	}
	fields := []*descriptorpb.FieldDescriptorProto{opt("ID", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, "")}
	var enums []*descriptorpb.EnumDescriptorProto
	for i, name := range names {
		fields = append(fields, opt(string(rune('A'+i)), int32(i+2), descriptorpb.FieldDescriptorProto_TYPE_ENUM, name))
		enums = append(enums, enum(name, values[i]...))
	}
	unionOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(unionOpts, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
	return &gen.MetaData{
		Records: &descriptorpb.FileDescriptorProto{
			Name:       proto.String("P"),
			Dependency: []string{gen.File_tuple_fields_proto.Path(), gen.File_record_metadata_options_proto.Path()},
			MessageType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("T"), Field: fields},
				{Name: proto.String("RecordTypeUnion"), Options: unionOpts, Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("T_0"), Number: proto.Int32(1),
					Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String("T"),
					Options: &descriptorpb.FieldOptions{},
				}}},
			},
			EnumType: enums,
		},
		RecordTypes: []*gen.RecordType{{
			Name:       proto.String("T"),
			PrimaryKey: &gen.KeyExpression{Field: &gen.Field{FieldName: proto.String("ID"), FanType: gen.Field_SCALAR.Enum()}},
		}},
	}
}

// TestEnumsSharingAValueNameLoadAsJavaLoadsThem: meta-data Java stores with two
// enums sharing a value name loads, each enum keeps its own values by name and
// number, and the records file the loaded meta-data writes back is the stored
// one, byte for byte (the scoping Go needs to build the descriptor must not
// reach the wire).
func TestEnumsSharingAValueNameLoadAsJavaLoadsThem(t *testing.T) {
	t.Parallel()
	assertEnumScopeLoadsAsJava(t, sharedEnumValueMetaData(), map[string]string{"A": "E1", "B": "E2"},
		map[string][]string{"A": {"X", "Y"}, "B": {"Y", "Z"}})
}

// TestAnEnumValueNamedLikeItsEnumLoadsAsJavaLoadsIt: the same for an enum one of
// whose values is spelled like the enum, which no depth of scoping separates
// from the enum's own name in protobuf-go.
func TestAnEnumValueNamedLikeItsEnumLoadsAsJavaLoadsIt(t *testing.T) {
	t.Parallel()
	assertEnumScopeLoadsAsJava(t, selfNamedEnumValueMetaData(), map[string]string{"A": "STATUS"},
		map[string][]string{"A": {"STATUS", "DONE"}})
}

func assertEnumScopeLoadsAsJava(t *testing.T, stored *gen.MetaData, javaNames map[string]string, values map[string][]string) {
	t.Helper()
	md, err := RecordMetaDataFromProto(proto.Clone(stored).(*gen.MetaData))
	if err != nil {
		t.Fatalf("loading meta-data Java stores: %v", err)
	}
	rt := md.GetRecordType("T")
	if rt == nil {
		t.Fatal("record type T missing")
	}
	for field, want := range values {
		fd := rt.Descriptor.Fields().ByName(protoreflect.Name(field))
		if fd == nil || fd.Enum() == nil {
			t.Fatalf("field %s is not an enum field", field)
		}
		if got, want := protoscope.JavaFullName(fd.Enum()), javaNames[field]; string(got) != want {
			t.Errorf("field %s's enum reads as %s, want Java's name %s", field, got, want)
		}
		vals := fd.Enum().Values()
		if vals.Len() != len(want) {
			t.Fatalf("field %s's enum has %d values, want %v", field, vals.Len(), want)
		}
		for i, name := range want {
			if got := vals.Get(i); string(got.Name()) != name || int(got.Number()) != i {
				t.Errorf("field %s value %d = %s=%d, want %s=%d", field, i, got.Name(), got.Number(), name, i)
			}
		}
	}
	back, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back.GetRecords(), stored.GetRecords()) {
		t.Fatalf("the records file written back differs from the stored one:\n got %v\nwant %v", back.GetRecords(), stored.GetRecords())
	}
}
