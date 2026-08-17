package values

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestFieldTypeForProtoFieldProducesExactExecutableShapes(t *testing.T) {
	t.Parallel()

	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("rfc232_proto_field_exact.proto"),
		Package: proto.String("rfc232.exact"),
		Syntax:  proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("State"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("UNKNOWN"), Number: proto.Int32(0)},
				{Name: proto.String("READY"), Number: proto.Int32(1)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Child"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("value"), Number: proto.Int32(1),
					Label: descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum(),
					Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
				}},
			},
			{
				Name: proto.String("Node"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("next"), Number: proto.Int32(1),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String(".rfc232.exact.Node"),
				}},
			},
			{
				Name: proto.String("Row"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_REQUIRED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()},
					{Name: proto.String("child"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".rfc232.exact.Child")},
					{Name: proto.String("scores"), Number: proto.Int32(3), Label: descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum()},
					{Name: proto.String("state"), Number: proto.Int32(4), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".rfc232.exact.State")},
					{Name: proto.String("unsigned"), Number: proto.Int32(5), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_UINT64.Enum()},
					{Name: proto.String("node"), Number: proto.Int32(6), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".rfc232.exact.Node")},
				},
			},
		},
	}
	file, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	row := file.Messages().ByName("Row")
	fields := row.Fields()

	tests := []struct {
		name     protoreflect.Name
		code     TypeCode
		nullable bool
		check    func(*testing.T, Type)
	}{
		{name: "id", code: TypeCodeLong, nullable: false},
		{name: "child", code: TypeCodeRecord, nullable: true, check: func(t *testing.T, typ Type) {
			record, ok := typ.(*RecordType)
			if !ok || len(record.Fields) != 1 || record.Fields[0].FieldType.Code() != TypeCodeLong || record.Fields[0].FieldType.IsNullable() {
				t.Fatalf("child type = %v, want nullable RECORD<required LONG>", typ)
			}
		}},
		{name: "scores", code: TypeCodeArray, nullable: false, check: func(t *testing.T, typ Type) {
			array, ok := typ.(*ArrayType)
			if !ok || array.ElementType == nil || array.ElementType.Code() != TypeCodeDouble || array.ElementType.IsNullable() {
				t.Fatalf("scores type = %v, want non-null ARRAY<non-null DOUBLE>", typ)
			}
		}},
		{name: "state", code: TypeCodeEnum, nullable: true},
		{name: "unsigned", code: TypeCodeLong, nullable: true},
		{name: "node", code: TypeCodeRecord, nullable: true, check: func(t *testing.T, typ Type) {
			record, ok := typ.(*RecordType)
			if !ok || len(record.Fields) != 1 {
				t.Fatalf("recursive node type = %v, want one-field record", typ)
			}
			if _, ok := record.Fields[0].FieldType.(anyRecordType); !ok {
				t.Fatalf("recursive back-edge type = %T, want exact AnyRecord", record.Fields[0].FieldType)
			}
		}},
	}

	for _, test := range tests {
		test := test
		t.Run(string(test.name), func(t *testing.T) {
			t.Parallel()
			typ := FieldTypeForProtoField(fields.ByName(test.name))
			if typ.Code() != test.code || typ.IsNullable() != test.nullable {
				t.Fatalf("type = %v (nullable=%v), want code=%v nullable=%v", typ, typ.IsNullable(), test.code, test.nullable)
			}
			if _, err := SnapshotExactType(typ); err != nil {
				t.Fatalf("snapshot exact type: %v", err)
			}
			if test.check != nil {
				test.check(t, typ)
			}
		})
	}

	message := dynamicpb.NewMessage(row)
	scores := message.Mutable(fields.ByName("scores")).List()
	scores.Append(protoreflect.ValueOfFloat64(2.5))
	message.Set(fields.ByName("state"), protoreflect.ValueOfEnum(1))
	message.Set(fields.ByName("unsigned"), protoreflect.ValueOfUint64(42))
	if got := ProtoFieldToRowValue(fields.ByName("scores"), message.Get(fields.ByName("scores"))); len(got.([]any)) != 1 {
		t.Fatalf("scores carrier = %#v, want one-element []any", got)
	}
	if got := ProtoFieldToRowValue(fields.ByName("state"), message.Get(fields.ByName("state"))); got != int64(1) {
		t.Fatalf("state carrier = %#v, want int64 enum ordinal", got)
	}
	if got := ProtoFieldToRowValue(fields.ByName("unsigned"), message.Get(fields.ByName("unsigned"))); got != int64(42) {
		t.Fatalf("unsigned carrier = %#v, want int64", got)
	}
}

func TestFieldTypeForProtoFieldReturnsIndependentTypeGraphs(t *testing.T) {
	t.Parallel()
	message := nestedFixtureMessage(t)
	recordField := message.Descriptor().Fields().ByName("rec")
	first := FieldTypeForProtoField(recordField).(*RecordType)
	first.Fields[0].Name = "MUTATED"
	second := FieldTypeForProtoField(recordField).(*RecordType)
	if second.Fields[0].Name == "MUTATED" {
		t.Fatal("caller mutation leaked into a later proto-field type derivation")
	}
	if _, err := SnapshotExactType(second); err != nil {
		t.Fatalf("independent derived graph is not exact: %v", err)
	}
}
