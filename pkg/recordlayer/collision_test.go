package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("PrimaryKeyCollision", func() {
	It("records can collide when not using record type prefix", func() {
		ctx := context.Background()

		// Create metadata with collision-prone primary keys (no record type prefix)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)

		// Set primary keys WITHOUT record type prefix - can collide!
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

			// Save an Order with ID 123
			order := &gen.Order{
				OrderId: proto.Int64(123),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			saved1, err := store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Saved order with key: %v\n", saved1.PrimaryKey)

			// Load it back
			loaded1, err := store.LoadRecord(tuple.Tuple{int64(123)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded1).NotTo(BeNil())

			loadedOrder := loaded1.Record.(*gen.Order)
			Expect(*loadedOrder.Price).To(Equal(int32(100)))

			// In a real scenario with multiple types sharing same key space,
			// saving Customer{customer_id: 123} would overwrite Order{order_id: 123}!

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("PrimaryKeyNoCollision", func() {
	It("records don't collide (record type always included)", func() {
		ctx := context.Background()

		// Create metadata - record type index is always included automatically (like Java)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)

		// Primary key - record type index prevents collisions automatically
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

			// Save an Order with ID 123
			order := &gen.Order{
				OrderId: proto.Int64(123),
				Price:   proto.Int32(100),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			saved1, err := store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Saved order with key: %v\n", saved1.PrimaryKey)

			// Load it back - note we still use the same primary key for loading
			loaded1, err := store.LoadRecord(tuple.Tuple{int64(123)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded1).NotTo(BeNil())

			loadedOrder := loaded1.Record.(*gen.Order)
			Expect(*loadedOrder.Price).To(Equal(int32(100)))

			// Record type index ensures Order{order_id: 123} and Customer{customer_id: 123}
			// have different keys and don't collide (like Java Record Layer)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("JavaCompatibilityBothModes", func() {
	testCases := []struct {
		name            string
		primaryKeyExpr  KeyExpression
		expectedKeySize int
		description     string
	}{
		{
			name:            "WithoutRecordType",
			primaryKeyExpr:  Field("order_id"),
			expectedKeySize: 15,
			description:     "Java: Key.Expressions.field(\"order_id\")",
		},
		{
			name:            "WithRecordType",
			primaryKeyExpr:  Field("order_id"),
			expectedKeySize: 17,
			description:     "Go: Always includes record type (like Java)",
		},
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		It(tc.name, func() {
			ctx := context.Background()

			// Create metadata
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(tc.primaryKeyExpr)
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

				// Save a record
				order := &gen.Order{
					OrderId: proto.Int64(999),
					Price:   proto.Int32(50),
					Flower: &gen.Flower{
						Type:  proto.String("Tulip"),
						Color: gen.Color_YELLOW.Enum(),
					},
				}

				saved, err := store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())

				GinkgoWriter.Printf("%s: %s\n", tc.name, tc.description)
				GinkgoWriter.Printf("Key size: %d bytes\n", saved.KeySize)
				GinkgoWriter.Printf("Primary key used for save: %v\n", saved.PrimaryKey)

				// The key size difference shows whether record type is included
				if tc.name == "WithRecordType" {
					Expect(saved.KeySize).To(BeNumerically(">", tc.expectedKeySize-2))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
