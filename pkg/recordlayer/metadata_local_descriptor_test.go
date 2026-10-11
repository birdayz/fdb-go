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
})
