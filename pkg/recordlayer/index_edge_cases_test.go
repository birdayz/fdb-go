package recordlayer

// Adversarial edge case tests for FDB Record Layer indexes.
// Targets the trickiest corners of index maintenance.

import (
	"context"
	"math"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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
