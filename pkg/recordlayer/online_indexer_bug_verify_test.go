package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("OnlineIndexerBugVerify", func() {
	ctx := context.Background()

	Describe("Bug1: boundary double-count for non-idempotent COUNT index", func() {
		// Java uses limit+1 and a "one-ahead" pattern: it reads limit+1 items,
		// processes only limit, and uses the (limit+1)th item's PK as the
		// exclusive continuation point. The boundary record is never re-processed.
		//
		// Test: 9 records, COUNT index, limit=3 → 3 chunks.
		// Expected (correct): count = 9.
		// If bug exists: count > 9 (boundary records double-counted).

		It("should not double-count boundary records", func() {
			ks := specSubspace()

			// Phase 1: Insert 9 records WITHOUT the COUNT index.
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			mdNoIndex, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 9; i++ {
					order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(100))}
					_, err = store.SaveRecord(order)
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build COUNT index online with limit=3 (forces 3 chunks).
			countIdx := NewCountIndex("bug1_count", Ungrouped(EmptyKey()))
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder2.AddIndex("Order", countIdx)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(countIdx).
				SetSubspace(ks).
				SetLimit(3). // 9 records / 3 per chunk = 3 chunks, 2 boundaries.
				Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Phase 3: Read the COUNT index value. Should be exactly 9.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				fn := &IndexAggregateFunction{
					Name:    FunctionNameCount,
					Operand: Ungrouped(EmptyKey()),
					Index:   "bug1_count",
				}
				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn, TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())

				count := result[0].(int64)
				Expect(count).To(Equal(int64(9)),
					"COUNT index double-counted boundary records across chunks")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Bug2: type filtering exhausts cursor limit, skips remaining records", func() {
		// When all records in a chunk are filtered out by shouldIndexRecord
		// (wrong type), the scan position must still advance. Otherwise the
		// full remaining range gets marked as done despite unscanned records.
		//
		// Test: 10 Customers (PKs 1-10), then 5 Orders (PKs 11-15).
		// Build an Order-only VALUE index with limit=5.
		// Expected (correct): all 5 Orders are indexed.
		// If bug exists: 0 entries (Orders skipped).

		It("should not skip records when type filter exhausts cursor limit", func() {
			ks := specSubspace()

			// Phase 1: Insert 10 Customers then 5 Orders.
			builder1 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder1.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder1.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder1.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			mdNoIndex, err := builder1.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i := int64(1); i <= 10; i++ {
					_, err = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("Cust")})
					Expect(err).NotTo(HaveOccurred())
				}
				for i := int64(11); i <= 15; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			// Phase 2: Build Order-only VALUE index with limit=5.
			priceIndex := NewIndex("bug2_price", Field("price"))
			builder2 := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder2.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder2.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder2.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder2.AddIndex("Order", priceIndex)
			mdWithIndex, err := builder2.Build()
			Expect(err).NotTo(HaveOccurred())

			indexer, err := NewOnlineIndexerBuilder().
				SetDatabase(sharedDB).
				SetMetaData(mdWithIndex).
				SetIndex(priceIndex).
				SetSubspace(ks).
				SetLimit(5).
				Build()
			Expect(err).NotTo(HaveOccurred())

			total, err := indexer.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			// Bug 30 FIX: counts ALL scanned records (10 Customers + 5 Orders = 15),
			// matching Java's IndexingBase.handleCursorResult().
			Expect(total).To(Equal(int64(15)))

			// Phase 3: Verify the index has all 5 Order entries.
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
				Expect(err).NotTo(HaveOccurred())

				entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())

				Expect(entries).To(HaveLen(5),
					"OnlineIndexer skipped Order records because type filtering exhausted cursor limit")

				for i, entry := range entries {
					expectedPK := int64(11 + i)
					expectedPrice := expectedPK * 100
					Expect(entry.IndexValues()).To(Equal(tuple.Tuple{expectedPrice}))
					Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{expectedPK}))
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
