package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("DeleteRecord", func() {
	It("deletes a record and verifies it is gone", func() {
		ctx := context.Background()

		// Create metadata
		fileDesc := gen.File_record_layer_demo_proto
		metaDataBuilder := NewRecordMetaDataBuilder().SetRecords(fileDesc)
		metaDataBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		metaDataBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		metaDataBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		recordMetaData, buildErr := metaDataBuilder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			// Create store
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(recordMetaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// First, create a test record
			order := &gen.Order{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(25),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			// Save the record
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Verify the record exists
			primaryKey := tuple.Tuple{int64(1001)}
			loadedRecord, err := store.LoadRecord(primaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(loadedRecord).NotTo(BeNil())

			// Delete the record
			deleted, err := store.DeleteRecord(primaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Verify the record no longer exists
			loadedRecord, err = store.LoadRecord(primaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(loadedRecord).To(BeNil())

			// Try to delete the same record again (should return false)
			deleted, err = store.DeleteRecord(primaryKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeFalse())

			return "delete test completed", nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
