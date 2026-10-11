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
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Java resolves a saved record's type by its descriptor (FDBRecordStore.java:552,
// RecordMetaData.java:219-223): a message only named like a record type is
// refused, for the untyped store as for the typed one.
var _ = Describe("SaveDescriptorIdentity", func() {
	loaded := func(mutate func(*gen.MetaData)) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		built, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		p, err := built.ToProto()
		Expect(err).NotTo(HaveOccurred())
		if mutate != nil {
			mutate(p)
		}
		md, err := RecordMetaDataFromProto(p)
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// The stored Order has a field the compiled one lacks.
	reshaped := func(p *gen.MetaData) {
		for _, m := range p.Records.MessageType {
			if m.GetName() == "Order" {
				m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{
					Name: proto.String("note"), Number: proto.Int32(60),
					Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				})
			}
		}
	}

	for _, c := range []struct {
		name   string
		mutate func(*gen.MetaData)
	}{
		{"a same-named compiled message of another shape", reshaped},
		{"a compiled message shaped alike, over meta-data loaded without WithLocalFileDescriptor", nil},
	} {
		It("refuses "+c.name+" on an untyped save", func() {
			md := loaded(c.mutate)
			_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
				var mdErr *MetaDataError
				Expect(errors.As(err, &mdErr)).To(BeTrue(), "got %v (%T), want a MetaDataError", err, err)
				Expect(mdErr.Message).To(Equal("descriptor did not match record type"))
				_, err = store.SaveRecordsPipelined([]proto.Message{&demopb.Order{OrderId: proto.Int64(2)}}, RecordExistenceCheckNone)
				Expect(errors.As(err, &mdErr)).To(BeTrue(), "pipelined: got %v (%T)", err, err)
				_, err = store.SaveRecordBatch([]proto.Message{&demopb.Order{OrderId: proto.Int64(3)}})
				Expect(errors.As(err, &mdErr)).To(BeTrue(), "batch: got %v (%T)", err, err)
				_, err = store.DryRunSaveRecord(&demopb.Order{OrderId: proto.Int64(4)}, RecordExistenceCheckNone)
				Expect(errors.As(err, &mdErr)).To(BeTrue(), "dry run: got %v (%T)", err, err)
				Expect(mdErr.Message).To(Equal("descriptor did not match record type"))
				for _, pk := range []int64{1, 2, 3} {
					exists, err := store.RecordExists(tuple.Tuple{pk}, IsolationLevelSerializable)
					Expect(err).NotTo(HaveOccurred())
					Expect(exists).To(BeFalse())
				}

				// A record of the meta-data's own descriptor saves.
				desc := md.GetRecordType("Order").Descriptor
				own := dynamicpb.NewMessage(desc)
				own.Set(desc.Fields().ByName("order_id"), protoreflect.ValueOfInt64(5))
				_, err = store.SaveRecord(own)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
