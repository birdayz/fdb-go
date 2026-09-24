//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
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
	fieldOpts := func(o *gen.FieldOptions) *descriptorpb.FieldOptions {
		fo := &descriptorpb.FieldOptions{}
		proto.SetExtension(fo, gen.E_Field, o)
		return fo
	}
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	field := func(name string, number int32, typ descriptorpb.FieldDescriptorProto_Type, opts *descriptorpb.FieldOptions) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Label: optional, Type: typ.Enum(), Options: opts}
	}
	recOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(recOpts, gen.E_Record, &gen.RecordTypeOptions{RecordTypeKey: &gen.Value{LongValue: proto.Int64(7)}})
	unionOpts := &descriptorpb.MessageOptions{}
	proto.SetExtension(unionOpts, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
	fileOpts := &descriptorpb.FileOptions{}
	proto.SetExtension(fileOpts, gen.E_Schema, &gen.SchemaOptions{SplitLongRecords: proto.Bool(true), StoreRecordVersions: proto.Bool(true)})
	tags := field("tags", 5, descriptorpb.FieldDescriptorProto_TYPE_STRING, fieldOpts(&gen.FieldOptions{Index: &gen.FieldOptions_IndexOption{}}))
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
					field("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, fieldOpts(&gen.FieldOptions{PrimaryKey: proto.Bool(true)})),
					field("price", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32, fieldOpts(&gen.FieldOptions{Index: &gen.FieldOptions_IndexOption{
						Unique:  proto.Bool(true),
						Options: []*gen.Index_Option{{Key: proto.String("allowedForQuery"), Value: proto.String("true")}},
					}})),
					field("score", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64, fieldOpts(&gen.FieldOptions{Indexed: gen.Index_RANK.Enum()})),
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

func setFieldOptions(fdp *descriptorpb.FileDescriptorProto, field string, o *gen.FieldOptions) {
	for _, f := range fdp.MessageType[0].Field {
		if f.GetName() == field {
			f.Options = &descriptorpb.FieldOptions{}
			proto.SetExtension(f.Options, gen.E_Field, o)
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
			setFieldOptions(f, "name", &gen.FieldOptions{PrimaryKey: proto.Bool(true)})
		}, "com.apple.foundationdb.record.metadata.MetaDataException", "Only one primary key per record type is allowed have: Field { 'id' None}; adding on name"},
		{"a primary key on a repeated field", func(f *descriptorpb.FileDescriptorProto) {
			setFieldOptions(f, "id", &gen.FieldOptions{})
			setFieldOptions(f, "tags", &gen.FieldOptions{PrimaryKey: proto.Bool(true)})
		}, "com.apple.foundationdb.record.metadata.MetaDataException", "Primary key cannot be set on a repeated field"},
		{"an index option repeated", func(f *descriptorpb.FileDescriptorProto) {
			setFieldOptions(f, "name", &gen.FieldOptions{Index: &gen.FieldOptions_IndexOption{Options: []*gen.Index_Option{
				{Key: proto.String("k"), Value: proto.String("1")}, {Key: proto.String("k"), Value: proto.String("2")},
			}}})
		}, "java.lang.IllegalArgumentException", "Multiple entries with same key: k=2 and k=1"},
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
				default:
					var mdErr *recordlayer.MetaDataError
					Expect(errors.As(goErr, &mdErr)).To(BeTrue(), "Go: %T %v", goErr, goErr)
				}
				Expect(goErr.Error()).To(Equal(c.text))
				return
			}
			Expect(java.Valid).To(BeTrue(), "Java: %s %s", java.Class, java.Error)
			Expect(goErr).NotTo(HaveOccurred())
			javaMD := &gen.MetaData{}
			Expect(proto.Unmarshal(intsToBytes(java.MetaData), javaMD)).To(Succeed())
			goMD, err := md.ToProto()
			Expect(err).NotTo(HaveOccurred())
			for _, p := range []*gen.MetaData{javaMD, goMD} {
				p.Records = nil
				p.Dependencies = nil
			}
			Expect(proto.Equal(goMD, javaMD)).To(BeTrue(), "Go:\n%s\nJava:\n%s", prototext.Format(goMD), prototext.Format(javaMD))
		})
	}
})
