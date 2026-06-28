package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SaveRecordWithOptions_ErrorPaths", func() {
	var metaData *RecordMetaData

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var buildErr error
		metaData, buildErr = builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("SaveUnknownRecordType", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Flower is not registered as a record type -- it's a nested message, not a union member
			flower := &gen.Flower{
				Type:  proto.String("Rose"),
				Color: gen.Color_RED.Enum(),
			}
			_, err = store.SaveRecord(flower)
			var mdErr *MetaDataError
			Expect(errors.As(err, &mdErr)).To(BeTrue())
			Expect(mdErr.Message).To(ContainSubstring("unknown record type"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertDuplicate", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			}
			_, err = store.InsertRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Insert same key again -- should fail
			order2 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			}
			_, err = store.InsertRecord(order2)
			Expect(err).To(HaveOccurred())
			var existsErr *RecordAlreadyExistsError
			Expect(errors.As(err, &existsErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateNonExistent", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(999),
				Price:   proto.Int32(100),
			}
			_, err = store.UpdateRecord(order)
			Expect(err).To(HaveOccurred())
			var notExistErr *RecordDoesNotExistError
			Expect(errors.As(err, &notExistErr)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SaveOverwriteNoCheck", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order1 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			}
			_, err = store.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			// Overwrite with different price -- should succeed (no existence check)
			order2 := &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(200),
			}
			stored, err := store.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())
			Expect(stored).NotTo(BeNil())

			// Verify the overwrite took effect
			loaded, err := store.LoadRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil())
			Expect(loaded.Record.(*gen.Order).GetPrice()).To(Equal(int32(200)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertThenUpdate", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(specSubspace()).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order1 := &gen.Order{
				OrderId: proto.Int64(5),
				Price:   proto.Int32(50),
			}
			_, err = store.InsertRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{
				OrderId: proto.Int64(5),
				Price:   proto.Int32(500),
			}
			_, err = store.UpdateRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			loaded, err := store.LoadRecord(tuple.Tuple{int64(5)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded.Record.(*gen.Order).GetPrice()).To(Equal(int32(500)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
