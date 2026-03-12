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
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
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

	Describe("BuildIndex on RANK index", func() {
		It("builds a RANK index on pre-existing records", func() {
			ks := specSubspace()

			// Phase 1: Insert records WITHOUT any index.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				prices := []int32{500, 100, 300, 200, 400}
				for i, price := range prices {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price)})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build RANK index online.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Phase 3: Verify BY_VALUE scan (sorted by price).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("rank_by_price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(rankIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				expectedPrices := []int64{100, 200, 300, 400, 500}
				for i, entry := range entries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}

				// Verify BY_RANK scan — rank 0 should be price 100, rank 4 should be price 500.
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(5))
				for i, entry := range rankEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds RANK index with small limit (chunked)", func() {
			ks := specSubspace()

			// Insert 15 records with various prices.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 15; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=3 to force multiple transactions.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 15))

			// Verify all 15 entries present and ranked correctly.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(rankIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(15))

				for i, entry := range entries {
					expectedPrice := int64((i + 1) * 10)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
				}

				// BY_RANK: rank 0→price 10, rank 14→price 150.
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(15)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(15))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("RANK index maintained after build (new records ranked correctly)", func() {
			ks := specSubspace()

			// Insert 3 records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(500)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build RANK index.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Insert new records — they should be ranked correctly.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// Insert prices that interleave: 200 (rank 1), 400 (rank 3)
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(400)})
				Expect(err).NotTo(HaveOccurred())

				// BY_RANK should now return: 100, 200, 300, 400, 500
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(5))

				expectedPrices := []int64{100, 200, 300, 400, 500}
				for i, entry := range rankEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds RANK index with duplicate scores", func() {
			ks := specSubspace()

			// Insert records with duplicate prices.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// 3 records at price=100, 2 at price=200, 1 at price=300
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(6), Price: proto.Int32(300)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build RANK index.
			rankIndex := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", rankIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(rankIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 6))

			// Verify: B-tree has 6 entries (one per record), ranked set has 3 scores.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				// BY_VALUE: 6 entries (3 at 100, 2 at 200, 1 at 300).
				entries, err := AsList(ctx, store.ScanIndex(rankIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(6))

				// BY_RANK: ranks [0,3) maps to scores [100,300+) → all 6 B-tree entries.
				rankEntries, err := AsList(ctx, store.ScanIndexByType(rankIndex, IndexScanByRank,
					TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(3)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(rankEntries).To(HaveLen(6))

				// Verify scores in order: 100, 100, 100, 200, 200, 300.
				expectedPrices := []int64{100, 100, 100, 200, 200, 300}
				for i, entry := range rankEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrices[i]}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("BY_INDEX strategy", func() {
		It("builds using BY_INDEX from existing readable source index", func() {
			ks := specSubspace()

			// Phase 1: Insert records WITH source index already present and READABLE.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			// DON'T add target index yet.
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 10; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add target index to metadata, build BY_INDEX.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex) // source must still be in metadata
			builder2.AddIndex("Order", qtyIndex)   // target
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 10))

			// Phase 3: Verify target index entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(qtyIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Verify sorted by quantity: 1, 2, ..., 10.
				for i, entry := range entries {
					expectedQty := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedQty}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds BY_INDEX with chunked transactions via SetLimit", func() {
			ks := specSubspace()

			// Phase 1: Insert 20 records with source index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 20; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build target index BY_INDEX with limit=3.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			builder2.AddIndex("Order", qtyIndex)
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 20))

			// Verify all 20 entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(qtyIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(20))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("saves BY_INDEX indexing type stamp with source metadata", func() {
			ks := specSubspace()

			// Insert records with source index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build target BY_INDEX.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			builder2.AddIndex("Order", qtyIndex)
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Load the stamp and verify fields.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(qtyIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_INDEX))

				// SourceIndexSubspaceKey should be the tuple-packed subspace key of the source index.
				expectedSubspaceKey := tuple.Tuple{priceIndex.SubspaceTupleKey()}.Pack()
				Expect(stamp.GetSourceIndexSubspaceKey()).To(Equal(expectedSubspaceKey))

				// SourceIndexLastModifiedVersion should match the source index's version.
				Expect(stamp.GetSourceIndexLastModifiedVersion()).To(Equal(int32(priceIndex.LastModifiedVersion)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects source index that is not VALUE type", func() {
			ks := specSubspace()

			countIndex := NewCountIndex("Order$count", GroupBy(Field("price")))
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", countIndex)
			builder.AddIndex("Order", qtyIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(qtyIndex).
				SetSourceIndex(countIndex).
				SetSubspace(ks).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must be a VALUE index"))
		})

		It("rejects source index whose root expression creates duplicates", func() {
			ks := specSubspace()

			fanOutIndex := NewIndex("Order$tags", FanOut("tags"))
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", fanOutIndex)
			builder.AddIndex("Order", qtyIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(qtyIndex).
				SetSourceIndex(fanOutIndex).
				SetSubspace(ks).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("creates duplicates"))
		})

		It("rejects source and target on different record types", func() {
			ks := specSubspace()

			// Source on Customer, target on Order.
			nameIndex := NewIndex("Customer$name", Field("name"))
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Customer", nameIndex)
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSourceIndex(nameIndex).
				SetSubspace(ks).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not cover source index type"))
		})

		It("maintains target index after BY_INDEX build when new records are inserted", func() {
			ks := specSubspace()

			// Phase 1: Insert records with source index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			mdSrc, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdSrc).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build target BY_INDEX.
			qtyIndex := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIndex)
			builder2.AddIndex("Order", qtyIndex)
			mdBoth, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdBoth).
				SetIndex(qtyIndex).
				SetSourceIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Insert new records and verify target index is maintained.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdBoth).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(6); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100)), Quantity: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}

				// All 10 entries should be in the target index.
				entries, err := AsList(ctx, store.ScanIndex(qtyIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Verify sorted by quantity: 1..10.
				for i, entry := range entries {
					expectedQty := int64(i + 1)
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedQty}))
				}
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
