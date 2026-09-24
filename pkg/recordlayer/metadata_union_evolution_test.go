package recordlayer

import (
	"context"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func unionEvolutionField(name string, number int32, kind descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
	f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(number), Type: kind.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()}
	if typeName != "" {
		f.TypeName = proto.String(typeName)
	}
	return f
}

type unionSchemaTest interface {
	Helper()
	Fatal(...any)
}

func unionEvolutionSchema(t unionSchemaTest, version int, swapped, incompatible bool, aliases bool) *RecordMetaData {
	t.Helper()
	stringType, numberType := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_INT64
	if swapped && !incompatible {
		stringType, numberType = numberType, stringType
	}
	message := func(name string, kind descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{
			unionEvolutionField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
			unionEvolutionField("payload", 2, kind, ""),
		}}
	}
	first, second := ".union_evolution.Alpha", ".union_evolution.Beta"
	if swapped {
		first, second = second, first
	}
	fields := []*descriptorpb.FieldDescriptorProto{
		unionEvolutionField("_Alpha", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, first),
		unionEvolutionField("_Beta", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, second),
	}
	if aliases {
		fields = append(fields,
			unionEvolutionField("alias_high", 9, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, first),
			unionEvolutionField("alias_middle", 4, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, first))
	}
	options := &descriptorpb.MessageOptions{}
	proto.SetExtension(options, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
	file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Dependency: []string{"record_metadata_options.proto"},
		Name:       proto.String("union_evolution.proto"), Package: proto.String("union_evolution"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{message("Alpha", stringType), message("Beta", numberType), {Name: proto.String("Envelope"), Options: options, Field: fields}},
	}, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	b := NewRecordMetaDataBuilder().SetRecordsWithUnionName(file, "Envelope")
	b.GetRecordType("Alpha").SetPrimaryKey(Field("id"))
	b.GetRecordType("Beta").SetPrimaryKey(Field("id"))
	b.SetVersion(version)
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	return md
}

func TestUnionEvolutionUsesTagIdentity(t *testing.T) {
	t.Parallel()
	old := unionEvolutionSchema(t, 1, false, false, false)
	new := unionEvolutionSchema(t, 2, true, false, false)
	if got := new.GetRecordType("Beta").GetRecordTypeKey(); got != int64(1) {
		t.Fatalf("tag 1 points to Beta, not the _Alpha field name: %v", got)
	}
	renames, err := DefaultMetaDataEvolutionValidator().getTypeRenames(old, new)
	if err != nil {
		t.Fatal(err)
	}
	if renames["Alpha"] != "Beta" || renames["Beta"] != "Alpha" {
		t.Fatalf("wrong identity correspondence: %v", renames)
	}
	if err := ValidateEvolution(old, new); err != nil {
		t.Fatalf("compatible tag-preserving swap: %v", err)
	}
	if err := NewMetaDataEvolutionValidator().SetDisallowTypeRenames(true).Build().Validate(old, new); err == nil {
		t.Fatal("name reuse must not bypass rename prohibition")
	}
	incompatible := unionEvolutionSchema(t, 2, true, true, false)
	if err := ValidateEvolution(old, incompatible); err == nil {
		t.Fatal("same surviving names masked incompatible tag-paired descriptors")
	}
}

func TestUnionAliasDefaultKeyAndSerializationTag(t *testing.T) {
	t.Parallel()
	for _, swapped := range []bool{false, true} {
		md := unionEvolutionSchema(t, 1, swapped, false, true)
		name, tag := "Alpha", protoreflect.FieldNumber(1)
		if swapped {
			name, tag = "Beta", 9
		}
		rt := md.GetRecordType(name)
		if rt.GetRecordTypeKey() != int64(1) {
			t.Fatalf("default key must use smallest alias tag: %v", rt.GetRecordTypeKey())
		}
		if rt.UnionFieldDescriptor.Number() != tag {
			t.Fatalf("serialization tag=%d, want %d", rt.UnionFieldDescriptor.Number(), tag)
		}
		for _, alias := range []protoreflect.FieldNumber{1, 4, 9} {
			if md.fieldNumberToRecordType[alias] != rt {
				t.Fatalf("alias %d must decode as %s", alias, name)
			}
		}
	}
}

func TestUnionEvolutionMemoizesDescriptorPairs(t *testing.T) {
	t.Parallel()
	build := func(version int, changed, invalid bool) *RecordMetaData {
		children := []string{"Child", "Child"}
		if changed {
			children = []string{"Left", "Right"}
		}
		root := &descriptorpb.DescriptorProto{Name: proto.String("Root"), Field: []*descriptorpb.FieldDescriptorProto{
			unionEvolutionField("id", 1, descriptorpb.FieldDescriptorProto_TYPE_INT64, ""),
			unionEvolutionField("left", 2, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".pair."+children[0]),
			unionEvolutionField("right", 3, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".pair."+children[1]),
			unionEvolutionField("self", 4, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".pair.Root"),
		}}
		messages := []*descriptorpb.DescriptorProto{root}
		for i, name := range children {
			if i == 1 && name == children[0] {
				continue
			}
			kind := descriptorpb.FieldDescriptorProto_TYPE_INT64
			if invalid && i == 1 {
				kind = descriptorpb.FieldDescriptorProto_TYPE_STRING
			}
			messages = append(messages, &descriptorpb.DescriptorProto{Name: proto.String(name), Field: []*descriptorpb.FieldDescriptorProto{unionEvolutionField("value", 1, kind, "")}})
		}
		// The union is marked as Java finds one ((record).usage = UNION): a
		// message merely named in SetRecordsWithUnionName is refused.
		unionOptions := &descriptorpb.MessageOptions{}
		proto.SetExtension(unionOptions, gen.E_Record, &gen.RecordTypeOptions{Usage: gen.RecordTypeOptions_UNION.Enum()})
		messages = append(messages, &descriptorpb.DescriptorProto{Name: proto.String("Envelope"), Options: unionOptions, Field: []*descriptorpb.FieldDescriptorProto{unionEvolutionField("root", 1, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE, ".pair.Root")}})
		fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Dependency: []string{"record_metadata_options.proto"},
			Name:       proto.String("pair.proto"), Package: proto.String("pair"), Syntax: proto.String("proto2"), MessageType: messages,
		}, protoregistry.GlobalFiles)
		if err != nil {
			t.Fatal(err)
		}
		b := NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "Envelope")
		b.GetRecordType("Root").SetPrimaryKey(Field("id"))
		b.SetVersion(version)
		md, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		return md
	}
	old := build(1, false, false)
	if err := ValidateEvolution(old, build(2, true, false)); err != nil {
		t.Fatalf("recursive compatible pairs: %v", err)
	}
	if err := ValidateEvolution(old, build(2, true, true)); err == nil {
		t.Fatal("second pairing of Child must be validated, not suppressed by the first")
	}
}

var _ = Describe("Union identity persistence", func() {
	It("reads an old tag after a name swap and writes the preferred new alias", func() {
		old := unionEvolutionSchema(GinkgoT(), 1, false, false, true)
		new := unionEvolutionSchema(GinkgoT(), 2, true, false, true)
		ss := specSubspace()
		ctx := context.Background()
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(old).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			desc := old.GetRecordType("Alpha").Descriptor
			msg := dynamicpb.NewMessage(desc)
			msg.Set(desc.Fields().ByName("id"), protoreflect.ValueOfInt64(10))
			msg.Set(desc.Fields().ByName("payload"), protoreflect.ValueOfString("preserved"))
			_, err = store.SaveRecord(msg)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(new).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			loaded, err := store.LoadRecord(tuple.Tuple{int64(10)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			reflected := loaded.Record.ProtoReflect()
			Expect(reflected.Descriptor().Name()).To(Equal(protoreflect.Name("Beta")))
			Expect(reflected.Get(reflected.Descriptor().Fields().ByName("payload")).String()).To(Equal("preserved"))
			rt := new.GetRecordType("Beta")
			inner, err := proto.Marshal(loaded.Record)
			Expect(err).NotTo(HaveOccurred())
			for _, tag := range []protowire.Number{1, 4, 9} {
				wire := protowire.AppendBytes(protowire.AppendTag(nil, tag, protowire.BytesType), inner)
				decoded, err := store.deserializeRecord(wire, rt)
				Expect(err).NotTo(HaveOccurred())
				Expect(proto.Equal(decoded, loaded.Record)).To(BeTrue())
			}
			wire, err := serializeUnion(loaded.Record, rt)
			Expect(err).NotTo(HaveOccurred())
			tag, _, n := protowire.ConsumeTag(wire)
			Expect(n).To(BeNumerically(">", 0))
			Expect(tag).To(Equal(protowire.Number(9)))
			_, err = store.SaveRecord(loaded.Record)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(new).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			loaded, err := store.LoadRecord(tuple.Tuple{int64(10)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Record.ProtoReflect().Descriptor().Name()).To(Equal(protoreflect.Name("Beta")))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("uses the current descriptor for same-name generated schema revisions", func() {
		old, err := baseBuilder().Build()
		Expect(err).NotTo(HaveOccurred())
		schema := protodesc.ToFileDescriptorProto(gen.File_record_layer_demo_proto)
		for _, message := range schema.MessageType {
			if message.GetName() == "Order" {
				message.Field = append(message.Field, unionEvolutionField("new_payload", 50, descriptorpb.FieldDescriptorProto_TYPE_STRING, ""))
			}
		}
		fd, err := protodesc.NewFile(schema, protoregistry.GlobalFiles)
		Expect(err).NotTo(HaveOccurred())
		b := NewRecordMetaDataBuilder().SetRecords(fd)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.SetVersion(old.Version() + 1)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(ValidateEvolution(old, md)).To(Succeed())
		rt := md.GetRecordType("Order")
		Expect(rt.newMessage().ProtoReflect().Descriptor()).To(BeIdenticalTo(rt.Descriptor))
		ss := specSubspace()
		ctx := context.Background()
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			message := dynamicpb.NewMessage(rt.Descriptor)
			message.Set(rt.Descriptor.Fields().ByName("order_id"), protoreflect.ValueOfInt64(20))
			message.Set(rt.Descriptor.Fields().ByName("new_payload"), protoreflect.ValueOfString("new schema"))
			_, err = store.SaveRecord(message)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
			Expect(err).NotTo(HaveOccurred())
			loaded, err := store.LoadRecord(tuple.Tuple{int64(20)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			message := loaded.Record.ProtoReflect()
			Expect(message.Descriptor()).To(BeIdenticalTo(rt.Descriptor))
			Expect(message.Get(rt.Descriptor.Fields().ByName("new_payload")).String()).To(Equal("new schema"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

func TestUnionEvolutionRejectsSplitMergeAndRemoval(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"split", "merge", "removal"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			old := unionEvolutionSchema(t, 1, false, false, true)
			schema := protodesc.ToFileDescriptorProto(old.FileDescriptor())
			union := schema.MessageType[2]
			switch mode {
			case "split":
				union.Field[3].TypeName = proto.String(".union_evolution.Beta")
			case "merge":
				union.Field[1].TypeName = proto.String(".union_evolution.Alpha")
			case "removal":
				union.Field = append(union.Field[:1], union.Field[2:]...)
			}
			fd, err := protodesc.NewFile(schema, protoregistry.GlobalFiles)
			if err != nil {
				t.Fatal(err)
			}
			b := NewRecordMetaDataBuilder().SetRecordsWithUnionName(fd, "Envelope")
			for _, name := range []string{"Alpha", "Beta"} {
				if b.recordTypes[name] != nil {
					b.GetRecordType(name).SetPrimaryKey(Field("id"))
				}
			}
			b.SetVersion(2)
			new, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateEvolution(old, new); err == nil {
				t.Fatalf("accepted union %s", mode)
			}
		})
	}
}

var _ = Describe("Union metadata history", func() {
	It("keeps persisted current and history intact when a tag-paired evolution is invalid", func() {
		old := unionEvolutionSchema(GinkgoT(), 1, false, false, true)
		bad := unionEvolutionSchema(GinkgoT(), 2, true, true, true)
		good := unionEvolutionSchema(GinkgoT(), 2, true, false, true)
		oldProto, err := old.ToProto()
		Expect(err).NotTo(HaveOccurred())
		badProto, err := bad.ToProto()
		Expect(err).NotTo(HaveOccurred())
		goodProto, err := good.ToProto()
		Expect(err).NotTo(HaveOccurred())
		ss := specSubspace()
		ctx := context.Background()
		metadataStore := NewFDBMetaDataStore(ss)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, metadataStore.SaveRecordMetaData(rtx.Transaction(), oldProto)
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, metadataStore.SaveRecordMetaData(rtx.Transaction(), badProto)
		})
		Expect(err).To(MatchError(ContainSubstring("type changed")))
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			current, err := metadataStore.LoadRecordMetaDataProto(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(current, oldProto)).To(BeTrue())
			history, err := metadataStore.LoadRecordMetaDataProtoAtVersion(rtx.Transaction(), 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(history).To(BeNil())
			return nil, metadataStore.SaveRecordMetaData(rtx.Transaction(), goodProto)
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			reopened := NewFDBMetaDataStore(ss)
			current, err := reopened.LoadRecordMetaDataProto(rtx.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(current, goodProto)).To(BeTrue())
			history, err := reopened.LoadRecordMetaDataProtoAtVersion(rtx.Transaction(), 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(proto.Equal(history, oldProto)).To(BeTrue())
			md, err := RecordMetaDataFromProto(current)
			Expect(err).NotTo(HaveOccurred())
			Expect(md.GetRecordType("Beta").GetRecordTypeKey()).To(Equal(int64(1)))
			Expect(md.GetUnionFieldForRecordType(md.GetRecordType("Beta")).Number()).To(Equal(protoreflect.FieldNumber(9)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
