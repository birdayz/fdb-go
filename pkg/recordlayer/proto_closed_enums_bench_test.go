package recordlayer

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// benchRecord is a relational-shaped record, a proto2 message of twelve
// scalar columns, and with enum an ENUM column (a DDL enum is a closed proto2
// enum), so its type reaches a closed enum.
func benchRecord(b *testing.B, enum bool) protoreflect.MessageDescriptor {
	b.Helper()
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	var fields []*descriptorpb.FieldDescriptorProto
	for i := int32(1); i <= 12; i++ {
		typ := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
		if i%3 == 0 {
			typ = descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
		}
		fields = append(fields, &descriptorpb.FieldDescriptorProto{Name: proto.String("c" + string(rune('a'+i))), Number: proto.Int32(i), Label: optional, Type: typ})
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("bench.proto"), Package: proto.String("bench"), Syntax: proto.String("proto2"),
		EnumType: []*descriptorpb.EnumDescriptorProto{{Name: proto.String("E"), Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: proto.String("A"), Number: proto.Int32(0)}, {Name: proto.String("B"), Number: proto.Int32(1)},
		}}},
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("R"), Field: fields}},
	}
	if enum {
		fdp.MessageType[0].Field = append(fields, &descriptorpb.FieldDescriptorProto{
			Name: proto.String("e"), Number: proto.Int32(13), Label: optional,
			Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".bench.E"),
		})
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		b.Fatal(err)
	}
	return fd.Messages().ByName("R")
}

func benchBytes(b *testing.B, md protoreflect.MessageDescriptor, enumValue uint64) []byte {
	b.Helper()
	var out []byte
	for i := protowire.Number(1); i <= 12; i++ {
		if i%3 == 0 {
			out = protowire.AppendBytes(protowire.AppendTag(out, i, protowire.BytesType), []byte("value-of-a-string-column"))
		} else {
			out = protowire.AppendVarint(protowire.AppendTag(out, i, protowire.VarintType), uint64(i)*1000003)
		}
	}
	if md.Fields().ByNumber(13) != nil {
		out = protowire.AppendVarint(protowire.AppendTag(out, 13, protowire.VarintType), enumValue)
	}
	return out
}

// BenchmarkJavaRecordDecode prices the record decode against protobuf-go's
// own: a type that reaches no closed enum takes no scan; one that does pays a
// scan of the bytes for an undeclared number before the same decode, and the
// occurrence-by-occurrence decode only when the bytes hold one.
func BenchmarkJavaRecordDecode(b *testing.B) {
	for _, c := range []struct {
		name      string
		enum      bool
		enumValue uint64
		rule      bool
	}{
		{"protobuf-go, no enum", false, 0, false},
		{"rule, no enum", false, 0, true},
		{"protobuf-go, a declared enum", true, 1, false},
		{"rule, a declared enum", true, 1, true},
		{"rule, an undeclared enum", true, 7, true},
	} {
		b.Run(c.name, func(b *testing.B) {
			md := benchRecord(b, c.enum)
			raw := benchBytes(b, md, c.enumValue)
			x := newClosedEnumReach(false, md)
			if !x.reaches(md) {
				x = nil
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				m := dynamicpb.NewMessage(md)
				var err error
				if c.rule {
					err = javaRecordRule.unmarshal(raw, m, x)
				} else {
					err = proto.Unmarshal(raw, m)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
