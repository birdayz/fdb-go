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

var _ = Describe("TypedStoreConformance", func() {
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

	It("typed save then base load", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			order1 := &gen.Order{
				OrderId: proto.Int64(1001),
				Price:   proto.Int32(25),
				Flower: &gen.Flower{
					Type:  proto.String("Rose"),
					Color: gen.Color_RED.Enum(),
				},
			}

			typedStored, err := orderStore.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Typed store saved Order ID: %d\n", typedStored.PrimaryKey[0])

			baseLoaded, err := baseStore.LoadRecord(tuple.Tuple{1001})
			Expect(err).NotTo(HaveOccurred())
			Expect(baseLoaded).NotTo(BeNil(), "base store could not load record saved by typed store")

			loadedOrder := baseLoaded.Record.(*gen.Order)
			GinkgoWriter.Printf("Base store loaded Order ID: %d, Price: %d, Type: %s\n",
				*loadedOrder.OrderId, *loadedOrder.Price, *loadedOrder.Flower.Type)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("base save then typed load", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			order2 := &gen.Order{
				OrderId: proto.Int64(2002),
				Price:   proto.Int32(50),
				Flower: &gen.Flower{
					Type:  proto.String("Tulip"),
					Color: gen.Color_YELLOW.Enum(),
				},
			}

			baseStored, err := baseStore.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("Base store saved Order ID: %d\n", baseStored.PrimaryKey[0])

			typedLoaded, err := orderStore.LoadRecord(tuple.Tuple{2002})
			Expect(err).NotTo(HaveOccurred())
			Expect(typedLoaded).NotTo(BeNil(), "typed store could not load record saved by base store")

			GinkgoWriter.Printf("Typed store loaded Order ID: %d, Price: %d, Type: %s\n",
				*typedLoaded.Record.OrderId, *typedLoaded.Record.Price, *typedLoaded.Record.Flower.Type)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("wire format verification", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			testOrder := &gen.Order{
				OrderId: proto.Int64(9999),
				Price:   proto.Int32(99),
				Flower: &gen.Flower{
					Type:  proto.String("TestFlower"),
					Color: gen.Color_BLUE.Enum(),
				},
			}

			testOrder1 := proto.Clone(testOrder).(*gen.Order)
			testOrder1.OrderId = proto.Int64(3001)

			testOrder2 := proto.Clone(testOrder).(*gen.Order)
			testOrder2.OrderId = proto.Int64(3002)

			// Save with base store
			_, err = baseStore.SaveRecord(testOrder1)
			Expect(err).NotTo(HaveOccurred())

			// Save with typed store
			_, err = orderStore.SaveRecord(testOrder2)
			Expect(err).NotTo(HaveOccurred())

			// Read raw data from FDB to compare wire format
			recordsSubspace := baseStore.Subspace().Sub(RecordKey)

			key1 := recordsSubspace.Pack(tuple.Tuple{3001, 0})
			rawData1 := rtx.Transaction().Get(key1).MustGet()

			key2 := recordsSubspace.Pack(tuple.Tuple{3002, 0})
			rawData2 := rtx.Transaction().Get(key2).MustGet()

			Expect(rawData1).NotTo(BeNil(), "failed to retrieve raw data for base store record")
			Expect(rawData2).NotTo(BeNil(), "failed to retrieve raw data for typed store record")

			union1 := &gen.UnionDescriptor{}
			union2 := &gen.UnionDescriptor{}

			err = proto.Unmarshal(rawData1, union1)
			Expect(err).NotTo(HaveOccurred())

			err = proto.Unmarshal(rawData2, union2)
			Expect(err).NotTo(HaveOccurred())

			Expect(union1.XOrder).NotTo(BeNil(), "base store record should contain Order field")
			Expect(union2.XOrder).NotTo(BeNil(), "typed store record should contain Order field")
			Expect(*union1.XOrder.Price).To(Equal(*union2.XOrder.Price))
			Expect(*union1.XOrder.Flower.Type).To(Equal(*union2.XOrder.Flower.Type))

			GinkgoWriter.Printf("Wire format identical: Both records stored as UnionDescriptor with Order field\n")
			GinkgoWriter.Printf("Base store data size: %d bytes\n", len(rawData1))
			GinkgoWriter.Printf("Typed store data size: %d bytes\n", len(rawData2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("cross-load verification", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			// Save with base store
			order1 := &gen.Order{
				OrderId: proto.Int64(3001),
				Price:   proto.Int32(99),
				Flower: &gen.Flower{
					Type:  proto.String("TestFlower"),
					Color: gen.Color_BLUE.Enum(),
				},
			}
			_, err = baseStore.SaveRecord(order1)
			Expect(err).NotTo(HaveOccurred())

			// Save with typed store
			order2 := &gen.Order{
				OrderId: proto.Int64(3002),
				Price:   proto.Int32(99),
				Flower: &gen.Flower{
					Type:  proto.String("TestFlower"),
					Color: gen.Color_BLUE.Enum(),
				},
			}
			_, err = orderStore.SaveRecord(order2)
			Expect(err).NotTo(HaveOccurred())

			// Load base-saved record with typed store
			crossLoaded1, err := orderStore.LoadRecord(tuple.Tuple{3001})
			Expect(err).NotTo(HaveOccurred())
			Expect(crossLoaded1).NotTo(BeNil())

			// Load typed-saved record with base store
			crossLoaded2, err := baseStore.LoadRecord(tuple.Tuple{3002})
			Expect(err).NotTo(HaveOccurred())
			Expect(crossLoaded2).NotTo(BeNil())

			GinkgoWriter.Println("Cross-compatibility verified: Both stores can read each other's records")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertRecord succeeds for new record", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			stored, err := orderStore.InsertRecord(&gen.Order{
				OrderId: proto.Int64(5001),
				Price:   proto.Int32(42),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(stored.PrimaryKey).To(Equal(tuple.Tuple{int64(5001)}))
			Expect(*stored.Record.Price).To(Equal(int32(42)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("InsertRecord fails for duplicate", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			_, err = orderStore.InsertRecord(&gen.Order{
				OrderId: proto.Int64(5002),
				Price:   proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = orderStore.InsertRecord(&gen.Order{
				OrderId: proto.Int64(5002),
				Price:   proto.Int32(20),
			})
			Expect(err).To(HaveOccurred())
			var e *RecordAlreadyExistsError
			Expect(errors.As(err, &e)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateRecord succeeds for existing record", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			_, err = orderStore.SaveRecord(&gen.Order{
				OrderId: proto.Int64(5003),
				Price:   proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			stored, err := orderStore.UpdateRecord(&gen.Order{
				OrderId: proto.Int64(5003),
				Price:   proto.Int32(99),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(*stored.Record.Price).To(Equal(int32(99)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateRecord fails for non-existent record", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			_, err = orderStore.UpdateRecord(&gen.Order{
				OrderId: proto.Int64(5004),
				Price:   proto.Int32(99),
			})
			Expect(err).To(HaveOccurred())
			var e *RecordDoesNotExistError
			Expect(errors.As(err, &e)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteRecord removes record", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			_, err = orderStore.SaveRecord(&gen.Order{
				OrderId: proto.Int64(5005),
				Price:   proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			deleted, err := orderStore.DeleteRecord(tuple.Tuple{int64(5005)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			loaded, err := orderStore.LoadRecord(tuple.Tuple{int64(5005)})
			Expect(err).NotTo(HaveOccurred())
			Expect(loaded).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("RecordExists returns correct result", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			exists, err := orderStore.RecordExists(tuple.Tuple{int64(5006)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeFalse())

			_, err = orderStore.SaveRecord(&gen.Order{
				OrderId: proto.Int64(5006),
				Price:   proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			exists, err = orderStore.RecordExists(tuple.Tuple{int64(5006)}, SerializableIsolation)
			Expect(err).NotTo(HaveOccurred())
			Expect(exists).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("GetRecordCount tracks count", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			mdBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			mdBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			mdBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			mdBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			mdBuilder.SetRecordCountKey(EmptyKey())
			mdWithCount, buildErr := mdBuilder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(mdWithCount).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			count, err := orderStore.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(0)))

			_, err = orderStore.SaveRecord(&gen.Order{
				OrderId: proto.Int64(6001),
				Price:   proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			count, err = orderStore.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(1)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("Context and Subspace return valid handles", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			Expect(orderStore.Context()).NotTo(BeNil())
			Expect(orderStore.Subspace()).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("DeleteAllRecords clears all records", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			mdBuilder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			mdBuilder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			mdBuilder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			mdBuilder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			mdBuilder.SetRecordCountKey(EmptyKey())
			mdWithCount, buildErr := mdBuilder.Build()
			Expect(buildErr).NotTo(HaveOccurred())

			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(mdWithCount).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				_, err = orderStore.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			count, err := orderStore.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(3)))

			err = orderStore.DeleteAllRecords()
			Expect(err).NotTo(HaveOccurred())

			count, err = orderStore.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(count).To(Equal(int64(0)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("typed ScanRecords returns typed cursor", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			baseStore, err := NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(metaData).
				SetSubspace(specSubspace()).
				CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			orderStore, err := GetTypedRecordStore[*gen.Order](baseStore, "Order")
			Expect(err).NotTo(HaveOccurred())

			// Save multiple orders + a customer (should be filtered out by typed scan)
			for i := int64(1); i <= 3; i++ {
				_, err = orderStore.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			_, err = baseStore.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(100),
				Name:       proto.String("Alice"),
			})
			Expect(err).NotTo(HaveOccurred())

			// Typed scan should return only orders, with correct type
			cursor := orderStore.ScanRecords(nil, ForwardScan())
			var orders []*gen.Order
			for rec, scanErr := range Seq2(cursor, ctx) {
				Expect(scanErr).NotTo(HaveOccurred())
				// rec is *FDBStoredRecord[*gen.Order] — compile-time type safety
				Expect(rec.Record).NotTo(BeNil())
				Expect(rec.Record.OrderId).NotTo(BeNil())
				orders = append(orders, rec.Record)
			}
			Expect(orders).To(HaveLen(3), "typed scan should return exactly 3 orders, no customers")
			Expect(*orders[0].OrderId).To(Equal(int64(1)))
			Expect(*orders[1].OrderId).To(Equal(int64(2)))
			Expect(*orders[2].OrderId).To(Equal(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
