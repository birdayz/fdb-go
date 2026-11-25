//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/metadatapb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// extensionOptionsRecords is a records file carrying the extension options
// Java's in-code setRecords reads: the file's (schema), the record type's
// (record) record_type_key and since_version, and each field's (field)
// primary_key, index and deprecated indexed. mutate edits it per case.
func extensionOptionsRecords(mutate func(*descriptorpb.FileDescriptorProto)) *descriptorpb.FileDescriptorProto {
	fieldOpts := func(o *metadatapb.FieldOptions) *descriptorpb.FieldOptions {
		fo := &descriptorpb.FieldOptions{}
		proto.SetExtension(fo, metadatapb.E_Field, o)
		return fo
	}
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	field := func(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type, opts *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: optional, Type: typ.Enum(), Options: opts}
	}
	recOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(recOpts, metadatapb.E_Record, &metadatapb.RecordTypeOptions{RecordTypeKey: &metadatapb.Value{LongValue: proto.Int64(7)}})
	unionOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(unionOpts, metadatapb.E_Record, &metadatapb.RecordTypeOptions{Usage: metadatapb.RecordTypeOptions_UNION.Enum()})
	fileOpts := &descriptorpb.FileOptions{}
	proto.SetExtension(fileOpts, metadatapb.E_Schema, &metadatapb.SchemaOptions{SplitLongRecords: proto.Bool(true), StoreRecordVersions: proto.Bool(true)})
	tags := field("tags", 5, descriptorpb.FieldDescriptorProto_TYPE_STRING, fieldOpts(&metadatapb.FieldOptions{Index: &metadatapb.FieldOptions_IndexOption{}}))
	tags.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("extension_options.proto"),
		Package:    proto.String("extopts"),
		Syntax:     proto.String("proto2"),
		Dependency: []string{"record_metadata_options.proto"},
		Options:    fileOpts,
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Rec"),
				Field: []*descriptorpb.FieldDescriptorProto{
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, fieldOpts(&metadatapb.FieldOptions{PrimaryKey: proto.Bool(true)})),
					field("price", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32, fieldOpts(&metadatapb.FieldOptions{Index: &metadatapb.FieldOptions_IndexOption{
						Unique:  proto.Bool(true),
						Options: []*metadatapb.Index_Option{{Key: proto.String("allowedForQuery"), Value: proto.String("true")}},
					}})),
					field("score", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64, fieldOpts(&metadatapb.FieldOptions{Indexed: metadatapb.Index_RANK.Enum()})),
					field("name", 4, descriptorpb.FieldDescriptorProto_TYPE_STRING, nil),
					tags,
				},
				Options: recOpts,
			},
			{
				Name:    proto.String("RecordTypeUnion"),
				Field:   []*descriptorpb.FieldDescriptorProto{{Name: proto.String("_Rec"), Number: proto.Int32(1), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".extopts.Rec")}},
				Options: unionOpts,
			},
		},
	}
	if mutate != nil {
		mutate(fdp)
	}
	return fdp
}

func setFieldOptions(fdp *descriptorpb.FileDescriptorProto, field string, o *metadatapb.FieldOptions) {
	for _, f := range fdp.MessageType[0].Field {
		if f.GetName() == field {
			f.Options = &descriptorpb.FieldOptions{}
			proto.SetExtension(f.Options, metadatapb.E_Field, o)
		}
	}
}

// A Go program over a records file with extension options builds the
// meta-data a Java program over the same file builds (RecordMetaDataBuilder.
// setRecords with processExtensionOptions, :161-174, :890-957): the built
// meta-data's proto, its records file aside, is equal; or both refuse, with
// the same class and text.
var _ = Describe("SetRecords reads the records file's extension options, as Java's in-code build does", func() {
	for _, c := range []struct {
		name        string
		mutate      func(*descriptorpb.FileDescriptorProto)
		class, text string
	}{
		{"every option", nil, "", ""},
		{"a second primary key", func(f *descriptorpb.FileDescriptorProto) {
			setFieldOptions(f, "name", &metadatapb.FieldOptions{PrimaryKey: proto.Bool(true)})
		}, "com.apple.foundationdb.record.metadata.MetaDataException", "Only one primary key per record type is allowed have: Field { 'id' None}; adding on name"},
		{"a primary key on a repeated field", func(f *descriptorpb.FileDescriptorProto) {
			setFieldOptions(f, "id", &metadatapb.FieldOptions{})
			setFieldOptions(f, "tags", &metadatapb.FieldOptions{PrimaryKey: proto.Bool(true)})
		}, "com.apple.foundationdb.record.metadata.MetaDataException", "Primary key cannot be set on a repeated field"},
		{"an index option repeated", func(f *descriptorpb.FileDescriptorProto) {
			setFieldOptions(f, "name", &metadatapb.FieldOptions{Index: &metadatapb.FieldOptions_IndexOption{Options: []*metadatapb.Index_Option{
				{Key: proto.String("k"), Value: proto.String("1")}, {Key: proto.String("k"), Value: proto.String("2")},
			}}})
		}, "java.lang.IllegalArgumentException", "Multiple entries with same key: k=2 and k=1"},
		{"a since_version", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[0].Options = &descriptorpb.MessageOptions{}
			proto.SetExtension(f.MessageType[0].Options, metadatapb.E_Record, &metadatapb.RecordTypeOptions{
				SinceVersion: proto.Int32(3), RecordTypeKey: &metadatapb.Value{LongValue: proto.Int64(7)},
			})
		}, "", ""},
		{"a record_type_key with two values", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType[0].Options = &descriptorpb.MessageOptions{}
			proto.SetExtension(f.MessageType[0].Options, metadatapb.E_Record, &metadatapb.RecordTypeOptions{
				RecordTypeKey: &metadatapb.Value{LongValue: proto.Int64(7), IntValue: proto.Int32(7)},
			})
		}, "com.apple.foundationdb.record.RecordCoreException", "More than one value encoded in value"},
		{"two record types of one name", func(f *descriptorpb.FileDescriptorProto) {
			f.MessageType = append(f.MessageType, &descriptorpb.DescriptorProto{
				Name: proto.String("Outer"),
				NestedType: []*descriptorpb.DescriptorProto{{
					Name:  proto.String("Rec"),
					Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("id"), Number: proto.Int32(1), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}},
				}},
			})
			f.MessageType[1].Field = append(f.MessageType[1].Field, &descriptorpb.FieldDescriptorProto{
				Name: proto.String("_Outer_Rec"), Number: proto.Int32(2), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".extopts.Outer.Rec"),
			})
		}, "com.apple.foundationdb.record.metadata.MetaDataException", "There is already a record type named Rec"},
	} {
		It("builds as Java does: "+c.name, func() {
			fdp := extensionOptionsRecords(c.mutate)
			fdpBytes, err := proto.Marshal(fdp)
			Expect(err).NotTo(HaveOccurred())
			var java struct {
				Valid    bool   `json:"valid"`
				Error    string `json:"error"`
				Class    string `json:"class"`
				MetaData []int  `json:"metaData"`
			}
			Expect(NewJavaInvoker().InvokeAs(context.Background(), "buildInCodeMetaData", map[string]any{
				"fileDescriptorProto": bytesToInts(fdpBytes),
			}, &java)).To(Succeed())

			fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
			Expect(err).NotTo(HaveOccurred())
			md, goErr := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd).Build()
			fmt.Fprintf(GinkgoWriter, "EXTENSION_OPTIONS %q java=%t %s %q go=%T %v\n", c.name, java.Valid, java.Class, java.Error, goErr, goErr)

			if c.class != "" {
				Expect(java.Valid).To(BeFalse())
				Expect(java.Class).To(Equal(c.class))
				Expect(java.Error).To(Equal(c.text))
				Expect(goErr).To(HaveOccurred())
				switch c.class {
				case "java.lang.IllegalArgumentException":
					var dup *recordlayer.DuplicateIndexOptionError
					Expect(errors.As(goErr, &dup)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				case "com.apple.foundationdb.record.RecordCoreException":
					var rce *recordlayer.RecordCoreError
					Expect(errors.As(goErr, &rce)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				default:
					var mdErr *recordlayer.MetaDataError
					Expect(errors.As(goErr, &mdErr)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				}
				Expect(goErr.Error()).To(Equal(c.text))
				return
			}
			Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
			Expect(goErr).NotTo(HaveOccurred())
			javaMD := &metadatapb.MetaData{}
			Expect(proto.Unmarshal(intsToBytes(java.MetaData), javaMD)).To(Succeed())
			goMD, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			for _, p := range []*metadatapb.MetaData{javaMD, goMD} {
				p.Records = nil
				p.Dependencies = nil
			}
			Expect(proto.Equal(goMD, javaMD)).To(BeTrue(), "Go:\n%s\nJava:\n%s", prototext.Format(goMD), prototext.Format(javaMD))
		})
	}
})
