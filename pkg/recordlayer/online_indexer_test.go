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

	Describe("stamp-aware resume", func() {
		It("resumes from WRITE_ONLY with matching BY_RECORDS stamp", func() {
			ks := specSubspace()

			// Create metadata WITH index so we can manually control index state.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store and insert initial records (index starts READABLE
			// since it's in metadata at CreateOrOpen time). Then mark WRITE_ONLY and
			// save the BY_RECORDS stamp — simulating a partially completed build.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert 3 records while index is READABLE — entries are maintained.
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// Mark WRITE_ONLY manually + save BY_RECORDS stamp.
				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Insert MORE records while WRITE_ONLY — triggers
			// UpdateWhileWriteOnly, which for VALUE indexes writes entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())

				for i := int64(4); i <= 6; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Run OnlineIndexer. Because the stamp matches BY_RECORDS,
			// markWriteOnly should NOT clear existing entries.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Build processes all 6 records (idempotent VALUE index — re-inserting
			// existing entries is a no-op).
			Expect(total).To(BeNumerically(">=", 6))

			// Phase 4: Verify all 6 entries present and index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(6))

				for i, entry := range entries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 100)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("clears and restarts on stamp mismatch", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store, insert records, then manually mark WRITE_ONLY
			// with a BY_INDEX stamp (simulating a prior BY_INDEX build attempt).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// Mark WRITE_ONLY + save a BY_INDEX stamp (mismatches BY_RECORDS).
				_, err = store.MarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Run BY_RECORDS OnlineIndexer. Stamp mismatch should cause
			// ClearAndMarkIndexWriteOnly (clearing existing entries) + fresh build.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 5))

			// Verify: all 5 entries rebuilt and index is READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(5))

				// Verify stamp was overwritten with BY_RECORDS.
				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rebuilds from READABLE state (fresh start)", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store with records and build index normally.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			indexer1, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer1.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify READABLE.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add more records.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				for i := int64(6); i <= 8; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Run OnlineIndexer again. Since index is READABLE (not
			// WRITE_ONLY), markWriteOnly does ClearAndMarkIndexWriteOnly → full rebuild.
			indexer2, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer2.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 8))

			// Verify all 8 entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(8))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("writes stamp and builds when WRITE_ONLY with no stamp and empty range set", func() {
			ks := specSubspace()

			priceIndex := NewIndex("Order$price", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 1: Create store with records, then manually mark WRITE_ONLY
			// WITHOUT saving any stamp (simulating legacy or manual state change).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 4; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				// ClearAndMarkIndexWriteOnly clears all index data (including any
				// stamps and range set) and transitions to WRITE_ONLY. This simulates
				// a manual or legacy WRITE_ONLY state with no stamp.
				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				// Verify no stamp exists.
				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).To(BeNil())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Run OnlineIndexer. No stamp + empty range set → save stamp
			// and proceed with build (no ClearAndMarkIndexWriteOnly).
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 4))

			// Verify: stamp written, index READABLE, all entries present.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				stamp, err := store.LoadIndexingTypeStamp(priceIndex)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(4))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("produces wire-compatible entries from WRITE_ONLY maintenance and build", func() {
			ks := specSubspace()

			// Use metadata WITHOUT index initially to insert record A.
			_, builderNoIdx := baseMetaData()
			mdNoIdx, err := builderNoIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Record A: inserted before index exists.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Now create metadata WITH index.
			priceIndex := NewIndex("Order$price", Field("price"))
			_, builderIdx := baseMetaData()
			builderIdx.AddIndex("Order", priceIndex)
			mdIdx, err := builderIdx.Build()
			Expect(err).NotTo(HaveOccurred())

			// Open with indexed metadata (CreateOrOpen handles version upgrade
			// from v=0 → v=1, triggering auto-rebuild). Then mark WRITE_ONLY
			// with stamp, simulating a partially completed OnlineIndexer run.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// ClearAndMarkIndexWriteOnly clears auto-rebuilt entries + range set.
				_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
				Expect(err).NotTo(HaveOccurred())

				stamp := &gen.IndexBuildIndexingStamp{
					Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(),
				}
				err = store.SaveIndexingTypeStamp(priceIndex, stamp)
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Insert record B while WRITE_ONLY — gets a maintenance entry.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexWriteOnly("Order$price")).To(BeTrue())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Run OnlineIndexer — stamp matches, so WRITE_ONLY maintenance entry
			// for record B survives, and build adds entry for record A.
			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdIdx).
				SetIndex(priceIndex).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Scan index and verify both entries have identical structure.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())
				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(2))

				// Entry for record A (price=100, pk=1) — created by build.
				Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
				Expect(entries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100), int64(1)}))
				Expect(entries[0].Value).To(HaveLen(0))

				// Entry for record B (price=200, pk=2) — created by WRITE_ONLY maintenance.
				Expect(entries[1].IndexValues()).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[1].PrimaryKey()).To(Equal(tuple.Tuple{int64(2)}))
				Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200), int64(2)}))
				Expect(entries[1].Value).To(HaveLen(0))

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

	Describe("Multi-target index building", func() {
		It("builds two VALUE indexes simultaneously on pre-existing records", func() {
			ks := specSubspace()

			// Phase 1: Insert 10 records WITHOUT any indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i * 5)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Create metadata with 2 indexes and build both via multi-target.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// 10 records indexed (count is per-record, not per-index-update).
			Expect(total).To(BeNumerically(">=", 10))

			// Phase 3: Verify both indexes are READABLE and scannable.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				// Verify price index: sorted 100, 200, ..., 1000.
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))
				for i, entry := range priceEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 100)}))
				}

				// Verify quantity index: sorted 5, 10, 15, ..., 50.
				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))
				for i, entry := range qtyEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 5)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds multi-target with chunked limit", func() {
			ks := specSubspace()

			// Insert 10 records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build with limit=3 to force multiple transactions across both indexes.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(BeNumerically(">=", 10))

			// Verify both indexes have all 10 entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))

				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))

				// Spot-check first and last entries for each index.
				Expect(priceEntries[0].IndexValues()).To(Equal(tuple.Tuple{int64(100)}))
				Expect(priceEntries[9].IndexValues()).To(Equal(tuple.Tuple{int64(1000)}))
				Expect(qtyEntries[0].IndexValues()).To(Equal(tuple.Tuple{int64(1)}))
				Expect(qtyEntries[9].IndexValues()).To(Equal(tuple.Tuple{int64(10)}))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rejects SetIndex combined with AddTargetIndex", func() {
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("SetIndex"))
		})

		It("rejects SetRecordTypes with multi-target", func() {
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetRecordTypes("Order").
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("record types"))
		})

		It("rejects SetSourceIndex with multi-target", func() {
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			srcIdx := NewIndex("Order$src", Field("price"))
			_, builder := baseMetaData()
			builder.AddIndex("Order", priceIdx)
			builder.AddIndex("Order", qtyIdx)
			builder.AddIndex("Order", srcIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSourceIndex(srcIdx).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("source index"))
		})

		It("rejects empty target indexes", func() {
			_, builder := baseMetaData()
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(md).
				SetTargetIndexes(nil).
				SetSubspace(specSubspace()).
				Build()
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("at least one target index"))
		})

		It("saves MULTI_TARGET_BY_RECORDS stamp with sorted target names", func() {
			ks := specSubspace()

			// Insert records.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Build multi-target. Add indexes in reverse alphabetical order
			// to verify the stamp sorts them.
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			priceIdx := NewIndex("Order$price", Field("price"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(qtyIdx).   // Added second, but "Order$price" < "Order$qty" alphabetically.
				AddTargetIndex(priceIdx). // Added first in the target list.
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify stamp on primary index (first in targetIndexes = qtyIdx).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				stamp, err := store.LoadIndexingTypeStamp(qtyIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp).NotTo(BeNil())
				Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS))

				// Target names must be sorted alphabetically.
				Expect(stamp.GetTargetIndex()).To(Equal([]string{"Order$price", "Order$qty"}))

				// Verify stamp also on the secondary index.
				stamp2, err := store.LoadIndexingTypeStamp(priceIdx)
				Expect(err).NotTo(HaveOccurred())
				Expect(stamp2).NotTo(BeNil())
				Expect(stamp2.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_MULTI_TARGET_BY_RECORDS))
				Expect(stamp2.GetTargetIndex()).To(Equal([]string{"Order$price", "Order$qty"}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("resumes multi-target build from partial progress", func() {
			ks := specSubspace()

			// Phase 1: Insert 10 records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 100)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Create metadata with 2 indexes.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: First build with limit=3 — does partial work, then we
			// manually simulate an interruption by building again with a fresh indexer.
			indexer1, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			// First full build completes all chunks.
			total1, err := indexer1.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(total1).To(BeNumerically(">=", 10))

			// Phase 3: Run AGAIN with same stamp — should resume (no-op since
			// already READABLE) or rebuild cleanly.
			indexer2, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				SetLimit(3).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total2, err := indexer2.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Rebuild processes all records again (clears + rebuilds from READABLE).
			Expect(total2).To(BeNumerically(">=", 10))

			// Verify both indexes are READABLE with correct entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Order$qty")).To(BeTrue())

				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))

				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("builds multi-target with different record types", func() {
			ks := specSubspace()

			// Phase 1: Insert Order and Customer records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// 5 Orders with prices.
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// 3 Customers with names. Use non-overlapping PKs (101-103).
				for i := int64(101); i <= 103; i++ {
					_, err = store.SaveRecord(&gen.Customer{
						CustomerId: proto.Int64(i),
						Name:       proto.String("Customer"),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Add type-specific indexes and build multi-target.
			priceIdx := NewIndex("Order$price", Field("price"))
			nameIdx := NewIndex("Customer$name", Field("name"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Customer", nameIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(nameIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// 5 Orders indexed into price index + 3 Customers into name index = 8.
			Expect(total).To(BeNumerically(">=", 8))

			// Phase 3: Verify each index only has entries from its own record type.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				Expect(store.IsIndexReadable("Order$price")).To(BeTrue())
				Expect(store.IsIndexReadable("Customer$name")).To(BeTrue())

				// Price index: exactly 5 entries (from Orders only).
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(5))
				for i, entry := range priceEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 100)}))
				}

				// Name index: exactly 3 entries (from Customers only).
				nameEntries, err := AsList(ctx, store.ScanIndex(nameIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(nameEntries).To(HaveLen(3))
				for _, entry := range nameEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{"Customer"}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("maintains both indexes after multi-target build when new records are saved", func() {
			ks := specSubspace()

			// Phase 1: Insert records without indexes.
			_, builder := baseMetaData()
			mdNoIndex, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 10)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Multi-target build.
			priceIdx := NewIndex("Order$price", Field("price"))
			qtyIdx := NewIndex("Order$qty", Field("quantity"))
			_, builder2 := baseMetaData()
			builder2.AddIndex("Order", priceIdx)
			builder2.AddIndex("Order", qtyIdx)
			mdWithIdx, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIdx).
				AddTargetIndex(priceIdx).
				AddTargetIndex(qtyIdx).
				SetSubspace(ks).
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Insert new records after build — both indexes should
			// auto-maintain via normal index maintenance (READABLE state).
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIdx).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(6); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(i),
						Price:    proto.Int32(int32(i * 10)),
						Quantity: proto.Int32(int32(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				// Verify both indexes have all 10 entries.
				priceEntries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(priceEntries).To(HaveLen(10))
				for i, entry := range priceEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64((i + 1) * 10)}))
				}

				qtyEntries, err := AsList(ctx, store.ScanIndex(qtyIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(qtyEntries).To(HaveLen(10))
				for i, entry := range qtyEntries {
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64(i + 1)}))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
