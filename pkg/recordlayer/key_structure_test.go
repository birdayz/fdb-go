package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("KeyStructure", func() {
	It("JavaCompatibleKeyStructure", func() {
		ctx := context.Background()

		primaryKeyExpr := Field("order_id")
		orderId := int64(100)
		expectedKey := tuple.Tuple{int64(100), int64(0)}

		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(primaryKeyExpr)
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{
				OrderId: proto.Int64(orderId),
				Price:   proto.Int32(50),
			}

			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Read the raw key from FDB
			recordsSubspace := store.Subspace().Sub(RecordKey)
			expectedFullKey := recordsSubspace.Pack(expectedKey)

			// Try to read with expected key
			value := rtx.Transaction().Get(expectedFullKey).MustGet()
			if value == nil {
				// Debug: scan all keys to see what's there
				GinkgoWriter.Println("Scanning all keys in records subspace:")
				iter := rtx.Transaction().GetRange(recordsSubspace, fdb.RangeOptions{
					Limit: 10,
				}).Iterator()

				for iter.Advance() {
					kv := iter.MustGet()
					unpacked, err := recordsSubspace.Unpack(kv.Key)
					if err == nil {
						GinkgoWriter.Printf("  Found key: %v\n", unpacked)
					}
				}
				Fail("No value found at expected key")
			} else {
				GinkgoWriter.Printf("Found record at expected key structure: %v\n", expectedKey)
				GinkgoWriter.Printf("  Key size: %d bytes\n", len(expectedFullKey))
				GinkgoWriter.Printf("  Value size: %d bytes\n", len(value))
			}

			// Verify LoadRecord works with the right primary key
			loadKey := tuple.Tuple{orderId}
			loaded, err := store.LoadRecord(loadKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).NotTo(BeNil(), "Failed to load record with primary key %v", loadKey)
			GinkgoWriter.Printf("LoadRecord successful with primary key: %v\n", loadKey)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
