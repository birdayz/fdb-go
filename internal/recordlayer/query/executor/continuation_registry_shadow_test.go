package executor

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/recordlayer"
)

// shadowFlowerMetaData is a schema declaring com.apple.foundationdb.record.Flower,
// the full name of a message this binary has compiled, with another shape.
func shadowFlowerMetaData(tb testing.TB) (*recordlayer.RecordMetaData, protoreflect.MessageDescriptor) {
	tb.Helper()
	field := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(num),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   typ.Enum(),
		}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("shadow_flower.proto"),
		Syntax:  proto.String("proto2"),
		Package: proto.String("com.apple.foundationdb.record"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Flower"), Field: []*descriptorpb.FieldDescriptorProto{
				field("petals", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
				field("name", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""),
			}},
			{Name: proto.String("Garden"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
				field("flower", 2, msg, ".com.apple.foundationdb.record.Flower"),
			}},
			{Name: proto.String("RecordTypeUnion"), Field: []*descriptorpb.FieldDescriptorProto{
				field("_Garden", 1, msg, ".com.apple.foundationdb.record.Garden"),
			}},
		},
	}, nil)
	if err != nil {
		tb.Fatalf("build shadow file: %v", err)
	}
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	b.GetRecordType("Garden").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		tb.Fatalf("build shadow metadata: %v", err)
	}
	flower := fd.Messages().ByName("Flower")
	if registered := (&demopb.Flower{}).ProtoReflect().Descriptor(); registered.FullName() != flower.FullName() {
		tb.Fatalf("shadow Flower is %s, want the compiled name %s", flower.FullName(), registered.FullName())
	}
	return md, flower
}

// A buffered STRUCT value flagged as a compiled message decodes with the store
// metadata's descriptor: a compiled message that only shares its full name is
// another type, and decoding as it misreads every field.
func TestContProtoMessage_GeneratedFlagDecodesWithMetadataDescriptor(t *testing.T) {
	t.Parallel()
	md, flowerDesc := shadowFlowerMetaData(t)
	flower := dynamicpb.NewMessage(flowerDesc)
	flower.Set(flowerDesc.Fields().ByName("petals"), protoreflect.ValueOfInt64(7))
	flower.Set(flowerDesc.Fields().ByName("name"), protoreflect.ValueOfString("tulip"))
	tok := mustContValue(t, flower)
	if tok[0] != contValProtoMsg || tok[1] != 1 {
		t.Fatalf("token starts [%d %d], want [%d 1]", tok[0], tok[1], contValProtoMsg)
	}
	tok[1] = 0 // the generated-message representation

	t.Run("with the metadata resolver", func(t *testing.T) {
		t.Parallel()
		v, rest, err := readContValue(tok)
		if err != nil || len(rest) != 0 {
			t.Fatalf("readContValue: %v, %d trailing bytes", err, len(rest))
		}
		got, err := resolvePendingProtoValues(v, metadataMessageResolver(md))
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		dyn, ok := got.(*dynamicpb.Message)
		if !ok {
			t.Fatalf("resolved %T, want *dynamicpb.Message over the metadata's Flower", got)
		}
		if dyn.Descriptor() != flowerDesc {
			t.Fatalf("resolved with descriptor %v, want the metadata's", dyn.Descriptor())
		}
		if !proto.Equal(dyn, flower) {
			t.Fatalf("resolved %v, want %v", dyn, flower)
		}
	})
	t.Run("without a resolver", func(t *testing.T) {
		t.Parallel()
		v, _, err := readContValue(tok)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := resolvePendingProtoValues(v, nil); err == nil {
			t.Fatalf("resolved %T (%v) with no metadata to check the compiled type against", got, got)
		}
	})
}

// A plan's computed file that declares a message the metadata compiles does not
// shadow it: the metadata's descriptor is resolved, so its compiled type restores.
func TestMetadataMessageResolver_MetadataBeforeComputedFiles(t *testing.T) {
	t.Parallel()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	b.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	b.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	computed, err := protodesc.NewFile(protodesc.ToFileDescriptorProto(demopb.File_record_layer_demo_proto), protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	resolve := metadataMessageResolver(md, computed)
	flower := (&demopb.Flower{}).ProtoReflect().Descriptor()
	if got, err := resolve(string(flower.FullName())); err != nil || got != flower {
		t.Fatalf("resolved %v (%v), want the metadata's compiled Flower", got, err)
	}
	v, _, err := readContValue(mustContValue(t, &demopb.Flower{Type: proto.String("rose")}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolvePendingProtoValues(v, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*demopb.Flower); !ok {
		t.Fatalf("restored %T, want *demopb.Flower", got)
	}
}
