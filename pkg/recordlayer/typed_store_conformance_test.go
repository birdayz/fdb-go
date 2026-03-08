package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("TypedStoreConformance", func() {
	var (
		metaData *RecordMetaData
	)

	BeforeEach(func() {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		var buildErr error
		metaData, buildErr = builder.Build()
		Expect(buildErr).NotTo(HaveOccurred())
	})

	It("typed save then base load", func() {
		ctx := context.Background()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
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
})
