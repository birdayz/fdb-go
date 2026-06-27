package recordlayer

import (
	"context"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("BasicScan", func() {
	It("scans all records in correct order", func() {
		ctx := context.Background()

		// Create metadata
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		metaData, buildErr := builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())

		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(ks).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save some test orders
			testOrders := []*gen.Order{
				{
					OrderId: proto.Int64(1001),
					Price:   proto.Int32(25),
					Flower:  &gen.Flower{Type: proto.String("Rose"), Color: gen.Color_RED.Enum()},
				},
				{
					OrderId: proto.Int64(1002),
					Price:   proto.Int32(30),
					Flower:  &gen.Flower{Type: proto.String("Tulip"), Color: gen.Color_YELLOW.Enum()},
				},
				{
					OrderId: proto.Int64(1003),
					Price:   proto.Int32(35),
					Flower:  &gen.Flower{Type: proto.String("Lily"), Color: gen.Color_BLUE.Enum()},
				},
			}

			// Save all test orders
			for _, order := range testOrders {
				_, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan all records
			cursor := store.ScanRecords(nil, ForwardScan())
			defer func() { _ = cursor.Close() }()

			var foundOrders []int64
			scanCtx := context.Background()

			for {
				result, err := cursor.OnNext(scanCtx)
				Expect(err).NotTo(HaveOccurred())

				if !result.HasNext() {
					break
				}

				record := result.GetValue()
				order, ok := record.Record.(*gen.Order)
				Expect(ok).To(BeTrue())

				foundOrders = append(foundOrders, *order.OrderId)
				GinkgoWriter.Printf("Found order: ID=%d, Price=%d, Type=%s\n",
					*order.OrderId, *order.Price, *order.Flower.Type)
			}

			// Verify we found all orders
			Expect(foundOrders).To(HaveLen(len(testOrders)))

			// Verify order IDs (should be in key order: 1001, 1002, 1003)
			expectedIDs := []int64{1001, 1002, 1003}
			for i, expectedID := range expectedIDs {
				Expect(foundOrders[i]).To(Equal(expectedID))
			}

			GinkgoWriter.Printf("Scan test passed: found %d orders in correct order\n", len(foundOrders))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
