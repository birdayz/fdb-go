package recordlayer

// Adversarial edge case tests for FDB Record Layer indexes.
// Targets the trickiest corners of index maintenance.

import (
	"context"
	"math"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Index Edge Cases", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	Describe("VALUE index with zero-value fields", func() {
		It("indexes a record where all fields are zero/default", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("order_price", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save with all defaults (order_id=0, price=0).
				// Proto2: unset optional fields → not serialized.
				// But order_id is the PK, it must be set.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(0), Price: proto.Int32(0)})
				Expect(err).NotTo(HaveOccurred())

				// Scan the index — should find the entry.
				entries, err := collectIndexEntries(ctx, store, "order_price", TupleRangeAllOf(tuple.Tuple{int64(0)}))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				// Save with PK = MaxInt64 — extreme PK value.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(math.MaxInt64), Price: proto.Int32(42)})
				Expect(err).NotTo(HaveOccurred())

				rec, err := store.LoadRecord(tuple.Tuple{int64(math.MaxInt64)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).NotTo(BeNil())
				Expect(rec.Record.(*gen.Order).GetPrice()).To(Equal(int32(42)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("COUNT index edge cases", func() {
		It("handles save-delete-save cycle correctly", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetRecordCountKey(&EmptyKeyExpression{})
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
				Expect(err).NotTo(HaveOccurred())
				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(1)))

				// Delete.
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())
				count, err = store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(0)))

				// Re-save same PK.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(20)})
				Expect(err).NotTo(HaveOccurred())
				count, err = store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(1)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("SUM index with negative values", func() {
		It("correctly sums negative field values", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			sumIdx := NewSumIndex("order_sum_price", Ungrouped(Field("price")))
			builder.AddIndex("Order", sumIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save records with negative prices.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(-100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)})
				Expect(err).NotTo(HaveOccurred())

				// SUM should be -100 + 50 = -50.
				sumFunc := &IndexAggregateFunction{
					Name:    FunctionNameSum,
					Operand: &EmptyKeyExpression{},
					Index:   "order_sum_price",
				}
				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, sumFunc, TupleRangeAll, IsolationLevelSerializable)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result[0]).To(Equal(int64(-50)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("VALUE index update-in-place", func() {
		It("correctly updates index entry when indexed field changes", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("order_price", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save with price=10.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
				Expect(err).NotTo(HaveOccurred())

				// Update to price=20 (same PK).
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(20)})
				Expect(err).NotTo(HaveOccurred())

				// Old entry (price=10) should be gone.
				oldEntries, err := collectIndexEntries(ctx, store, "order_price", TupleRangeAllOf(tuple.Tuple{int64(10)}))
				Expect(err).NotTo(HaveOccurred())
				Expect(oldEntries).To(BeEmpty())

				// New entry (price=20) should exist.
				newEntries, err := collectIndexEntries(ctx, store, "order_price", TupleRangeAllOf(tuple.Tuple{int64(20)}))
				Expect(err).NotTo(HaveOccurred())
				Expect(newEntries).To(HaveLen(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("handles update where indexed field doesn't change (no-op)", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("order_price2", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save and re-save with same price — index should still have exactly 1 entry.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
				Expect(err).NotTo(HaveOccurred())

				entries, err := collectIndexEntries(ctx, store, "order_price2", TupleRangeAllOf(tuple.Tuple{int64(42)}))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("COUNT index with rapid inserts and deletes", func() {
		It("count stays consistent after many save-delete cycles", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetRecordCountKey(&EmptyKeyExpression{})
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Rapid insert/delete cycle: insert 20, delete odd PKs, verify count=10.
				for i := int64(0); i < 20; i++ {
					_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
					Expect(err).NotTo(HaveOccurred())
				}

				for i := int64(1); i < 20; i += 2 {
					_, err := store.DeleteRecord(tuple.Tuple{i})
					Expect(err).NotTo(HaveOccurred())
				}

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(10)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("VALUE index with MaxInt32 and MinInt32 prices", func() {
		It("indexes extreme int32 values correctly", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", NewIndex("order_price_extreme", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save with extreme int32 values.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(math.MaxInt32)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(math.MinInt32)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(0)})
				Expect(err).NotTo(HaveOccurred())

				// Scan all — should get all 3 in tuple order (MinInt32 < 0 < MaxInt32).
				allEntries, err := collectIndexEntries(ctx, store, "order_price_extreme", TupleRangeAll)
				Expect(err).NotTo(HaveOccurred())
				Expect(allEntries).To(HaveLen(3))

				// Verify order: MinInt32 first, then 0, then MaxInt32.
				Expect(allEntries[0].Key[0]).To(Equal(int64(math.MinInt32)))
				Expect(allEntries[1].Key[0]).To(Equal(int64(0)))
				Expect(allEntries[2].Key[0]).To(Equal(int64(math.MaxInt32)))

				// Point lookup for MaxInt32.
				maxEntries, err := collectIndexEntries(ctx, store, "order_price_extreme", TupleRangeAllOf(tuple.Tuple{int64(math.MaxInt32)}))
				Expect(err).NotTo(HaveOccurred())
				Expect(maxEntries).To(HaveLen(1))
				Expect(maxEntries[0].PrimaryKey()).To(Equal(tuple.Tuple{int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("DeleteAllRecords with indexes", func() {
		It("clears all index entries and count", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.SetRecordCountKey(&EmptyKeyExpression{})
			builder.AddIndex("Order", NewIndex("order_price_del", Field("price")))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save 10 records.
				for i := int64(0); i < 10; i++ {
					_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
					Expect(err).NotTo(HaveOccurred())
				}

				count, err := store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(10)))

				// DeleteAllRecords.
				err = store.DeleteAllRecords()
				Expect(err).NotTo(HaveOccurred())

				// Count should be 0.
				count, err = store.GetRecordCount()
				Expect(err).NotTo(HaveOccurred())
				Expect(count).To(Equal(int64(0)))

				// Index should be empty.
				entries, err := collectIndexEntries(ctx, store, "order_price_del", TupleRangeAll)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				// Records should be gone.
				rec, err := store.LoadRecord(tuple.Tuple{int64(5)})
				Expect(err).NotTo(HaveOccurred())
				Expect(rec).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// RANK index edge cases
	// =========================================================================
	Describe("RANK index edge cases", func() {
		It("handles many records with identical scores", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			builder.AddIndex("Order", rankIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save 10 orders all with price=100.
				for i := int64(1); i <= 10; i++ {
					_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
					Expect(err).NotTo(HaveOccurred())
				}

				// RankForScore(100) should return 0 (first and only score).
				maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
				Expect(mErr).NotTo(HaveOccurred())
				maintainer := maintainerIface.(*rankIndexMaintainer)

				rank, err := maintainer.RankForScore(tuple.Tuple{int64(100)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(0)))

				// Rank scan [0,10) should return all 10 entries.
				rankRange := TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(10)})
				entries, err := AsList(ctx, store.ScanIndexByType(rankIdx, IndexScanByRank, rankRange, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(10))

				// Without CountDuplicates, the RankedSet has 1 unique score.
				// ScoreForRank(0) returns 100, ScoreForRank(1) returns nil (no 2nd unique score).
				score, err := maintainer.ScoreForRank(tuple.Tuple{int64(0)})
				Expect(err).NotTo(HaveOccurred())
				Expect(score).To(Equal(tuple.Tuple{int64(100)}))

				score, err = maintainer.ScoreForRank(tuple.Tuple{int64(1)})
				Expect(err).NotTo(HaveOccurred())
				Expect(score).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("rank scan with empty store returns empty", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			builder.AddIndex("Order", rankIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Empty store: scan [0,100) should return nothing.
				rankRange := TupleRangeBetween(tuple.Tuple{int64(0)}, tuple.Tuple{int64(100)})
				entries, err := AsList(ctx, store.ScanIndexByType(rankIdx, IndexScanByRank, rankRange, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("score for rank beyond dataset returns nil", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			builder.AddIndex("Order", rankIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save 3 records with distinct prices.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)})
				Expect(err).NotTo(HaveOccurred())

				maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
				Expect(mErr).NotTo(HaveOccurred())
				maintainer := maintainerIface.(*rankIndexMaintainer)

				// Rank 10 is beyond the 3 records; should return nil.
				score, err := maintainer.ScoreForRank(tuple.Tuple{int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(score).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("delete reduces rank", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			rankIdx := NewRankIndex("rank_by_price", GroupBy(Field("price")))
			builder.AddIndex("Order", rankIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save 3 records: prices 10, 20, 30.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30)})
				Expect(err).NotTo(HaveOccurred())

				// Before delete: rank of 30 should be 2.
				maintainerIface, mErr := store.getIndexMaintainer(rankIdx)
				Expect(mErr).NotTo(HaveOccurred())
				maintainer := maintainerIface.(*rankIndexMaintainer)

				rank, err := maintainer.RankForScore(tuple.Tuple{int64(30)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(2)))

				// Delete order_id=2 (price=20).
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				// After delete: rank of 30 should now be 1 (was 2).
				maintainerIface, mErr = store.getIndexMaintainer(rankIdx)
				Expect(mErr).NotTo(HaveOccurred())
				maintainer = maintainerIface.(*rankIndexMaintainer)

				rank, err = maintainer.RankForScore(tuple.Tuple{int64(30)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(1)))

				// Rank of 10 should still be 0.
				rank, err = maintainer.RankForScore(tuple.Tuple{int64(10)}, false)
				Expect(err).NotTo(HaveOccurred())
				Expect(*rank).To(Equal(int64(0)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// PERMUTED_MIN/MAX index edge cases
	// =========================================================================
	Describe("PERMUTED_MIN/MAX index edge cases", func() {
		It("permuted min tracks minimum value per group", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			// Group by price (group key), order_id is the tracked value, permutedSize=1.
			idx := NewPermutedMinIndex("order_pmin", GroupBy(Field("order_id"), Field("price")), 1)
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Price=100 group: order_ids 5, 3, 8.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(8), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// Price=200 group: order_id 10.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())

				// BY_GROUP: min per group.
				// Price=100 min order_id=3, Price=200 min order_id=10.
				// Permuted key: [order_id, price] sorted by order_id.
				byGroup, err := AsList(ctx, store.ScanIndexByType(
					idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byGroup).To(HaveLen(2))
				Expect(byGroup[0].Key[0]).To(Equal(int64(3))) // min for price=100
				Expect(byGroup[0].Key[1]).To(Equal(int64(100)))
				Expect(byGroup[1].Key[0]).To(Equal(int64(10))) // min for price=200
				Expect(byGroup[1].Key[1]).To(Equal(int64(200)))

				// Now save a new record in price=100 group with lower order_id=1.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// BY_GROUP: price=100 min should now be order_id=1.
				byGroup, err = AsList(ctx, store.ScanIndexByType(
					idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byGroup).To(HaveLen(2))
				Expect(byGroup[0].Key[0]).To(Equal(int64(1))) // new min for price=100
				Expect(byGroup[0].Key[1]).To(Equal(int64(100)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("permuted min after delete of min record", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewPermutedMinIndex("order_pmin_del", GroupBy(Field("order_id"), Field("price")), 1)
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Price=100 group: order_ids 10, 20, 30.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(20), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(30), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// Min is order_id=10.
				byGroup, err := AsList(ctx, store.ScanIndexByType(
					idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byGroup).To(HaveLen(1))
				Expect(byGroup[0].Key[0]).To(Equal(int64(10)))

				// Delete the min (order_id=10).
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(10)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				// New min should be order_id=20.
				byGroup, err = AsList(ctx, store.ScanIndexByType(
					idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byGroup).To(HaveLen(1))
				Expect(byGroup[0].Key[0]).To(Equal(int64(20)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("permuted max with ties", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			idx := NewPermutedMaxIndex("order_pmax_ties", GroupBy(Field("order_id"), Field("price")), 1)
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Price=50 group: 3 records all with same order_id range.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50)})
				Expect(err).NotTo(HaveOccurred())

				// BY_GROUP: max order_id for price=50 should be 3.
				byGroup, err := AsList(ctx, store.ScanIndexByType(
					idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byGroup).To(HaveLen(1))
				Expect(byGroup[0].Key[0]).To(Equal(int64(3)))
				Expect(byGroup[0].Key[1]).To(Equal(int64(50)))

				// BY_VALUE: all 3 entries.
				byValue, err := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(byValue).To(HaveLen(3))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// TEXT index edge cases
	// =========================================================================
	Describe("TEXT index edge cases", func() {
		It("empty string field produces no tokens", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			textIdx := NewTextIndex("customer_name_text", Field("name"))
			builder.AddIndex("Customer", textIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save a customer with empty name.
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(1),
					Name:       proto.String(""),
				})
				Expect(err).NotTo(HaveOccurred())

				// Scan all tokens - should be empty.
				entries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("single character tokens", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			textIdx := NewTextIndex("customer_name_text", Field("name"))
			builder.AddIndex("Customer", textIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(1),
					Name:       proto.String("a b c"),
				})
				Expect(err).NotTo(HaveOccurred())

				// Each single-character token should be found.
				for _, token := range []string{"a", "b", "c"} {
					entries, err := AsList(ctx, store.ScanIndexByType(
						textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{token}), nil, ForwardScan()))
					Expect(err).NotTo(HaveOccurred())
					Expect(entries).To(HaveLen(1), "token %q should have 1 entry", token)
				}

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("update changes token set", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			textIdx := NewTextIndex("customer_name_text", Field("name"))
			builder.AddIndex("Customer", textIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save with "hello world".
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(1),
					Name:       proto.String("hello world"),
				})
				Expect(err).NotTo(HaveOccurred())

				// Verify "hello" exists.
				helloEntries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(helloEntries).To(HaveLen(1))

				// Update to "goodbye world" (same PK).
				_, err = store.SaveRecord(&gen.Customer{
					CustomerId: proto.Int64(1),
					Name:       proto.String("goodbye world"),
				})
				Expect(err).NotTo(HaveOccurred())

				// "hello" should now be gone.
				helloEntries, err = AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"hello"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(helloEntries).To(BeEmpty())

				// "goodbye" should exist.
				goodbyeEntries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"goodbye"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(goodbyeEntries).To(HaveLen(1))

				// "world" should still exist (unchanged token).
				worldEntries, err := AsList(ctx, store.ScanIndexByType(
					textIdx, IndexScanByTextToken, TupleRangeAllOf(tuple.Tuple{"world"}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(worldEntries).To(HaveLen(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// =========================================================================
	// BITMAP_VALUE index edge cases
	// =========================================================================
	Describe("BITMAP_VALUE index edge cases", func() {
		// hasBit checks if a specific bit is set in a bitmap byte slice.
		hasBit := func(bitmapBytes []byte, offset int64) bool {
			byteIdx := offset / 8
			if int(byteIdx) >= len(bitmapBytes) {
				return false
			}
			return (bitmapBytes[byteIdx] & (1 << (offset % 8))) != 0
		}

		It("bitmap set and check for single position", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			// Grouped by price, position = order_id.
			bitmapIdx := NewBitmapValueIndex("order_bitmap_edge", GroupBy(Field("order_id"), Field("price")))
			builder.AddIndex("Order", bitmapIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save at position 0 (order_id=0) in group price=5.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(0), Price: proto.Int32(5)})
				Expect(err).NotTo(HaveOccurred())

				// Scan by group=5 to get bitmap.
				entries, err := AsList(ctx, store.ScanIndexByType(
					bitmapIdx, IndexScanByGroup, TupleRangeAllOf(tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				bitmapBytes, ok := entries[0].Value[0].([]byte)
				Expect(ok).To(BeTrue())
				Expect(hasBit(bitmapBytes, 0)).To(BeTrue(), "bit 0 should be set")
				Expect(hasBit(bitmapBytes, 1)).To(BeFalse(), "bit 1 should not be set")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("bitmap survives delete", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			bitmapIdx := NewBitmapValueIndex("order_bitmap_del", GroupBy(Field("order_id"), Field("price")))
			builder.AddIndex("Order", bitmapIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save positions 0 and 1 in group price=5.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(0), Price: proto.Int32(5)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(5)})
				Expect(err).NotTo(HaveOccurred())

				// Both bits should be set.
				entries, err := AsList(ctx, store.ScanIndexByType(
					bitmapIdx, IndexScanByGroup, TupleRangeAllOf(tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				bitmapBytes, ok := entries[0].Value[0].([]byte)
				Expect(ok).To(BeTrue())
				Expect(hasBit(bitmapBytes, 0)).To(BeTrue())
				Expect(hasBit(bitmapBytes, 1)).To(BeTrue())

				// Delete position 0.
				deleted, err := store.DeleteRecord(tuple.Tuple{int64(0)})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				// Position 1 should still be set, position 0 cleared.
				entries, err = AsList(ctx, store.ScanIndexByType(
					bitmapIdx, IndexScanByGroup, TupleRangeAllOf(tuple.Tuple{int64(5)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				bitmapBytes, ok = entries[0].Value[0].([]byte)
				Expect(ok).To(BeTrue())
				Expect(hasBit(bitmapBytes, 0)).To(BeFalse(), "bit 0 should be cleared after delete")
				Expect(hasBit(bitmapBytes, 1)).To(BeTrue(), "bit 1 should still be set")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("large position value", func() {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			bitmapIdx := NewBitmapValueIndex("order_bitmap_large", GroupBy(Field("order_id"), Field("price")))
			builder.AddIndex("Order", bitmapIdx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ss := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
				if err != nil {
					return nil, err
				}

				// Save at position 5000 in group price=7.
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5000), Price: proto.Int32(7)})
				Expect(err).NotTo(HaveOccurred())

				// Scan should return an entry; the position 5000 bit should be set.
				entries, err := AsList(ctx, store.ScanIndexByType(
					bitmapIdx, IndexScanByGroup, TupleRangeAllOf(tuple.Tuple{int64(7)}), nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).ToNot(BeEmpty())

				// Find the entry containing position 5000.
				// Default entry size is 10000, so position 5000 fits in aligned offset 0.
				// The bit at position 5000 within that entry should be set.
				found := false
				for _, e := range entries {
					bitmapBytes, ok := e.Value[0].([]byte)
					if !ok {
						continue
					}
					// Position within entry = 5000 - (alignedOffset * entrySize).
					// With default entrySize=10000 and aligned to 0, offset = 5000.
					if hasBit(bitmapBytes, 5000) {
						found = true
						break
					}
				}
				Expect(found).To(BeTrue(), "bit at position 5000 should be set")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})

// collectIndexEntries scans an index and returns all entries.
func collectIndexEntries(ctx context.Context, store *FDBRecordStore, indexName string, scanRange TupleRange) ([]*IndexEntry, error) {
	cursor := store.ScanIndex(store.metaData.GetIndex(indexName), scanRange, nil, ForwardScan())
	var entries []*IndexEntry
	for entry, err := range Seq2(cursor, ctx) {
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
