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

	order := func() *demopb.Order { return &demopb.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)} }
	arms := []struct {
		name string
		save func(*FDBRecordStore) error
	}{
		{"SaveRecord", func(s *FDBRecordStore) error { _, err := s.SaveRecord(order()); return err }},
		{"SaveRecordsPipelined", func(s *FDBRecordStore) error {
			_, err := s.SaveRecordsPipelined([]proto.Message{order()}, RecordExistenceCheckNone)
			return err
		}},
		{"SaveRecordBatch", func(s *FDBRecordStore) error {
			_, err := s.SaveRecordBatch([]proto.Message{order()})
			return err
		}},
		// The descriptor is refused ahead of the batch's UTF-8 pre-check.
		{"SaveRecordBatch of a record with invalid UTF-8", func(s *FDBRecordStore) error {
			_, err := s.SaveRecordBatch([]proto.Message{&demopb.Customer{CustomerId: proto.Int64(1), Name: proto.String("\xff")}})
			return err
		}},
		{"DryRunSaveRecord", func(s *FDBRecordStore) error {
			_, err := s.DryRunSaveRecord(order(), RecordExistenceCheckNone)
			return err
		}},
	}
	for _, c := range []struct {
		name   string
		mutate func(*gen.MetaData)
	}{
		{"a same-named compiled message of another shape", reshaped},
		{"a compiled message shaped alike, over meta-data loaded without WithLocalFileDescriptor", nil},
	} {
		for _, arm := range arms {
			It("refuses "+c.name+" on "+arm.name, func() {
				md := loaded(c.mutate)
				_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
					store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
					Expect(err).NotTo(HaveOccurred())
					err = arm.save(store)
					var mdErr *MetaDataError
					Expect(errors.As(err, &mdErr)).To(BeTrue(), "got %v (%T), want a MetaDataError", err, err)
					Expect(mdErr.Message).To(Equal("descriptor did not match record type"))
					exists, err := store.RecordExists(tuple.Tuple{int64(1)}, IsolationLevelSerializable)
					Expect(err).NotTo(HaveOccurred())
					Expect(exists).To(BeFalse())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}

	It("saves a record of the loaded meta-data's own descriptor", func() {
		md := loaded(nil)
		_, err := sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			desc := md.GetRecordType("Order").Descriptor
			own := dynamicpb.NewMessage(desc)
			own.Set(desc.Fields().ByName("order_id"), protoreflect.ValueOfInt64(5))
			_, err = store.SaveRecord(own)
			Expect(err).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
