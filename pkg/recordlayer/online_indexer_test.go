package recordlayer

import (
	"context"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("OnlineIndexer", func() {
	ctx := context.Background()

	// Helper: create metadata with an Order type (PK on order_id) and NO indexes initially.
	baseMetaData := func() (*RecordMetaData, *RecordMetaDataBuilder) {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		return nil, builder
	}

	Describe("BuildIndex on existing data", func() {
		It("builds a VALUE index on pre-existing records", func() {
			ks := specSubspace()

			// Phase 1: Insert records WITHOUT any index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Create metadata WITH index and build it online.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(5). // Process 5 records per transaction to test chunking.
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Idempotent indexes may re-scan boundary records across chunks.
			Expect(total).To(BeNumerically(">=", 10))

			// Phase 3: Verify index is READABLE and can be scanned.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Verify order: prices should be sorted 100, 200, ..., 1000.
				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 100)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds a composite index with PK dedup", func() {
			ks := specSubspace()

			// Insert records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build composite index (price, order_id) with PK dedup on order_id.
			compositeIndex := NewIndex("Order$price_id", Concat(Field("price"), Field("order_id")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", compositeIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(compositeIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(5)))

			// Verify: entries should be deduplicated (2 elements, not 3).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(compositeIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 100)
					expectedPK := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice, expectedPK}))
					Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{expectedPK}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles empty store", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Create the store first.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				_, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(0)))

			// Verify readable.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("index is maintained after build (new records)", func() {
			ks := specSubspace()

			// Insert 5 records without index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Insert more records after build — index should auto-maintain.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(6); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds index with small limit (many transactions)", func() {
			ks := specSubspace()

			// Insert 20 records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 20; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=3 — forces many transactions.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 20))

			// Verify all entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds unique index", func() {
			ks := specSubspace()

			// Insert records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build unique index.
			uniqueIndex := NewIndex("Order$price_unique", Field("price")).SetUnique()
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", uniqueIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(uniqueIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(5)))
		})

		It("filters to correct record type", func() {
			ks := specSubspace()

			// Insert both Orders and Customers.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
				}
				// Use non-overlapping PKs (101-103) to avoid colliding with Order PKs (1-5).
				for i := int64(101); i <= 103; i++ {
					_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Test")})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build Order-only index — should only index 5 Orders, not 3 Customers.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify exactly 5 entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Builder validation", func() {
		It("rejects missing database", func() {
			_, err := NewOnlineIndexerBuilder().
				SetMetaData(&RecordMetaData{}).
				SetIndex(NewIndex("test", Field("x"))).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
		})

		It("rejects missing index", func() {
			_, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(&RecordMetaData{}).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
		})
	})
})
