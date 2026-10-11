package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Java's RecordMetaDataBuilder.setLocalFileDescriptor: meta-data loaded from
// its stored proto takes the compiled descriptors, as a Java program reading
// an FDBMetaDataStore does to save its compiled records.
var _ = Describe("WithLocalFileDescriptor", func() {
	storedDemo := func() *gen.MetaData {
		b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", NewIndex("order_price", Field("price")))
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		return p
	}
	openStore := func(rtx *FDBRecordContext, md *RecordMetaData) *FDBRecordStore {
		store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
		Expect(err).NotTo(HaveOccurred())
		return store
	}

	It("uses the compiled descriptors, keeping the stored keys and indexes", func() {
		p := storedDemo()
		plain, err := RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
		Expect(err).NotTo(HaveOccurred())
		local, err := RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData), WithLocalFileDescriptor(demopb.File_record_layer_demo_proto))
		Expect(err).NotTo(HaveOccurred())

		order := (&demopb.Order{}).ProtoReflect().Descriptor()
		Expect(plain.GetRecordType("Order").Descriptor).NotTo(BeIdenticalTo(order))
		rt := local.GetRecordType("Order")
		Expect(rt.Descriptor).To(BeIdenticalTo(order))
		Expect(rt.UnionFieldDescriptor.Number()).To(Equal(protoreflect.FieldNumber(1)))
		Expect(local.GetUnionDescriptor()).To(BeIdenticalTo((&demopb.UnionDescriptor{}).ProtoReflect().Descriptor()))
		Expect(rt.PrimaryKey).To(Equal(plain.GetRecordType("Order").PrimaryKey))
		Expect(local.GetIndex("order_price")).NotTo(BeNil())

		_, err = local.ToProto()
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue(), "ToProto: %v", err)
		Expect(mdErr.Message).To(Equal("cannot serialize meta-data with a local records descriptor to proto"))

		// Records written through either meta-data read through the other.
		_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			_, err := openStore(rtx, local).SaveRecord(&demopb.Order{OrderId: proto.Int64(7), Price: proto.Int32(70)})
			Expect(err).NotTo(HaveOccurred())
			loaded, err := openStore(rtx, local).LoadRecord(tuple.Tuple{int64(7)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Record).To(BeAssignableToTypeOf(&demopb.Order{}), "the compiled type is decoded")
			viaPlain, err := openStore(rtx, plain).LoadRecord(tuple.Tuple{int64(7)})
			Expect(err).NotTo(HaveOccurred())
			Expect(viaPlain.Record.ProtoReflect().Get(plain.GetRecordType("Order").Descriptor.Fields().ByName("price")).Int()).To(Equal(int64(70)))
			hits, err := AsList(context.Background(), openStore(rtx, local).ScanIndexRecords("order_price", TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(hits).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("synthesizes a union for a local file without one", func() {
		file := protodesc.ToFileDescriptorProto(demopb.File_record_layer_demo_proto)
		var kept []*descriptorpb.DescriptorProto
		for _, m := range file.MessageType {
			if m.GetName() != "UnionDescriptor" {
				kept = append(kept, m)
			}
		}
		file.MessageType = kept
		noUnion, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
		Expect(err).NotTo(HaveOccurred())

		md, err := RecordMetaDataFromProto(storedDemo(), WithLocalFileDescriptor(noUnion))
		Expect(err).NotTo(HaveOccurred())
		union := md.GetUnionDescriptor()
		Expect(string(union.Name())).To(Equal("RecordTypeUnion"))
		Expect(union.ParentFile().Path()).To(Equal("_synthetic_record_layer_demo.proto"))
		rt := md.GetRecordType("Order")
		Expect(rt.Descriptor).To(BeIdenticalTo(noUnion.Messages().ByName("Order")))
		Expect(rt.UnionFieldDescriptor.Number()).To(Equal(protoreflect.FieldNumber(1)))
		Expect(rt.UnionFieldDescriptor.ContainingMessage()).To(BeIdenticalTo(union))

		_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			order := dynamicpb.NewMessage(rt.Descriptor)
			order.Set(rt.Descriptor.Fields().ByName("order_id"), protoreflect.ValueOfInt64(8))
			_, err := openStore(rtx, md).SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())
			plain, err := RecordMetaDataFromProto(storedDemo())
			Expect(err).NotTo(HaveOccurred())
			loaded, err := openStore(rtx, plain).LoadRecord(tuple.Tuple{int64(8)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.RecordType.Name).To(Equal("Order"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a local file the stored records cannot evolve into", func() {
		// The stored records have a record type, Extra, the compiled file lacks.
		file := protodesc.ToFileDescriptorProto(demopb.File_record_layer_demo_proto)
		file.MessageType = append(file.MessageType, &descriptorpb.DescriptorProto{
			Name: proto.String("Extra"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("id"), Number: proto.Int32(1),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			}},
		})
		for _, m := range file.MessageType {
			if m.GetName() == "UnionDescriptor" {
				m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
					Name: proto.String("_Extra"), Number: proto.Int32(9),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
					TypeName: proto.String(".com.apple.foundationdb.record.Extra"),
				})
			}
		}
		withExtra, err := protodesc.NewFile(file, protoregistry.GlobalFiles)
		Expect(err).NotTo(HaveOccurred())
		b := NewRecordMetaDataBuilder().SetRecords(withExtra)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.GetRecordType("Extra").SetPrimaryKey(Field("id"))
		stored, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := stored.ToProto()
		Expect(err).NotTo(HaveOccurred())
		_, err = RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
		Expect(err).NotTo(HaveOccurred(), "the stored meta-data itself loads")
		_, err = RecordMetaDataFromProto(p, WithLocalFileDescriptor(demopb.File_record_layer_demo_proto))
		var evoErr *MetaDataEvolutionError
		Expect(errors.As(err, &evoErr)).To(BeTrue(), "got %v (%T)", err, err)
		Expect(evoErr.Message).To(Equal("record type removed from union: Extra"))
	})

	// A small records file of its own, the stored and local sides of one schema.
	optional := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	scalar := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Label: optional, Type: descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()}
	}
	ref := func(name string, num int32, msg string) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: proto.String(name), Number: proto.Int32(num), Label: optional,
			Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), TypeName: proto.String(".localkey." + msg),
		}
	}
	message := func(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields}
	}
	file := func(msgs ...*descriptorpb.DescriptorProto) protoreflect.FileDescriptor {
		fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name: proto.String("localkey.proto"), Package: proto.String("localkey"), Syntax: proto.String("proto2"), MessageType: msgs,
		}, nil)
		Expect(err).NotTo(HaveOccurred())
		return fd
	}
	// stored is fd's meta-data with every type keyed by id, as a store holds it.
	stored := func(fd protoreflect.FileDescriptor, edit func(*RecordMetaDataBuilder)) *gen.MetaData {
		b := NewRecordMetaDataBuilder().SetRecords(fd)
		for i := 0; i < fd.Messages().Len(); i++ {
			if name := fd.Messages().Get(i).Name(); name != "RecordTypeUnion" {
				b.GetRecordType(string(name)).SetPrimaryKey(Field("id"))
			}
		}
		if edit != nil {
			edit(b)
		}
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		return p
	}
	order := func(name string) *descriptorpb.DescriptorProto {
		return message(name, scalar("id", 1), scalar("price", 2))
	}
	other := message("Other", scalar("id", 1))

	It("takes the record type key from the local union's smallest field", func() {
		// RecordType.getRecordTypeKey (RecordType.java:171-182) reads the meta-data's
		// union, which is the local one (RecordMetaDataBuilder.java:281-287).
		storedFile := file(order("Order"), other, message("RecordTypeUnion", ref("_Order", 5, "Order"), ref("_Other", 6, "Other")))
		localFile := file(order("Order"), other, message("RecordTypeUnion", ref("_Order_low", 4, "Order"), ref("_Order", 5, "Order"), ref("_Other", 6, "Other")))
		p := stored(storedFile, func(b *RecordMetaDataBuilder) { b.GetRecordType("Other").SetRecordTypeKey("other") })

		plain, err := RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData))
		Expect(err).NotTo(HaveOccurred())
		Expect(plain.GetRecordType("Order").GetRecordTypeKey()).To(Equal(int64(5)))
		md, err := RecordMetaDataFromProto(p, WithLocalFileDescriptor(localFile))
		Expect(err).NotTo(HaveOccurred())
		rt := md.GetRecordType("Order")
		Expect(rt.GetRecordTypeKey()).To(Equal(int64(4)))
		// The union field written is still the stored one (RecordMetaDataBuilder.java:804-809).
		Expect(rt.UnionFieldDescriptor.Number()).To(Equal(protoreflect.FieldNumber(5)))
		Expect(md.GetRecordType("Other").GetRecordTypeKey()).To(Equal("other"), "an explicit key is kept")
	})

	It("renames a record type through the local file, keeping its key, indexes and primary key", func() {
		storedFile := file(order("Order"), message("RecordTypeUnion", ref("_Order", 1, "Order")))
		localFile := file(order("Purchase"), message("RecordTypeUnion", ref("_Purchase", 1, "Purchase")))
		p := stored(storedFile, func(b *RecordMetaDataBuilder) { b.AddIndex("Order", NewIndex("price", Field("price"))) })

		md, err := RecordMetaDataFromProto(p, WithLocalFileDescriptor(localFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetRecordType("Order")).To(BeNil())
		rt := md.GetRecordType("Purchase")
		Expect(rt).NotTo(BeNil())
		Expect(rt.Descriptor).To(BeIdenticalTo(localFile.Messages().ByName("Purchase")))
		Expect(rt.GetRecordTypeKey()).To(Equal(int64(1)))
		Expect(rt.PrimaryKey).To(Equal(Field("id")))
		Expect(rt.GetIndexes()).To(HaveLen(1))
		Expect(rt.GetIndexes()[0].Name).To(Equal("price"))
		Expect(md.RecordTypesForIndex(md.GetIndex("price"))).To(ConsistOf(rt))
	})

	It("honours WithEvolutionValidator", func() {
		storedFile := file(order("Order"), message("RecordTypeUnion", ref("_Order", 1, "Order")))
		renamedField := message("Order", scalar("id", 1), scalar("cost", 2))
		localFile := file(renamedField, message("RecordTypeUnion", ref("_Order", 1, "Order")))
		p := stored(storedFile, nil)

		_, err := RecordMetaDataFromProto(proto.Clone(p).(*gen.MetaData), WithLocalFileDescriptor(localFile))
		var evoErr *MetaDataEvolutionError
		Expect(errors.As(err, &evoErr)).To(BeTrue(), "the default validator refuses a field rename: %v (%T)", err, err)
		lenient := NewMetaDataEvolutionValidator().SetAllowFieldRenames(true).Build()
		md, err := RecordMetaDataFromProto(p, WithLocalFileDescriptor(localFile), WithEvolutionValidator(lenient))
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetRecordType("Order").Descriptor.Fields().ByName("cost")).NotTo(BeNil())
	})

	It("skips a local record type the stored meta-data lacks", func() {
		// RecordMetaDataBuilder.java:795-797: no new types from a local union.
		storedFile := file(order("Order"), message("RecordTypeUnion", ref("_Order", 1, "Order")))
		localFile := file(order("Order"), other, message("RecordTypeUnion", ref("_Order", 1, "Order"), ref("_Other", 2, "Other")))
		md, err := RecordMetaDataFromProto(stored(storedFile, nil), WithLocalFileDescriptor(localFile))
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetRecordType("Other")).To(BeNil())
		Expect(md.RecordTypes()).To(HaveLen(1))
		Expect(md.GetRecordType("Order").Descriptor).To(BeIdenticalTo(localFile.Messages().ByName("Order")))
	})

	It("refuses to synthesize a union for a oneof union or a removed type", func() {
		oneofUnion := message("RecordTypeUnion", ref("_Order", 1, "Order"), ref("_Other", 2, "Other"))
		for _, f := range oneofUnion.Field {
			f.OneofIndex = proto.Int32(0)
		}
		oneofUnion.OneofDecl = []*descriptorpb.OneofDescriptorProto{{Name: proto.String("record")}}
		_, err := RecordMetaDataFromProto(stored(file(order("Order"), other, oneofUnion), nil), WithLocalFileDescriptor(file(order("Order"), other)))
		var mdErr *MetaDataError
		Expect(errors.As(err, &mdErr)).To(BeTrue(), "got %v (%T)", err, err)
		Expect(mdErr.Message).To(Equal("Adding record type to oneof is not allowed"))

		plainUnion := message("RecordTypeUnion", ref("_Order", 1, "Order"), ref("_Other", 2, "Other"))
		_, err = RecordMetaDataFromProto(stored(file(order("Order"), other, plainUnion), nil), WithLocalFileDescriptor(file(order("Order"))))
		Expect(errors.As(err, &mdErr)).To(BeTrue(), "got %v (%T)", err, err)
		Expect(mdErr.Message).To(Equal("Record type Other removed"))
	})
})
