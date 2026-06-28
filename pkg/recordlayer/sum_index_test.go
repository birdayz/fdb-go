package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("SumIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("sums values ungrouped (total sum)", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert orders with prices 100, 200, 300
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Single entry with empty key, value = 100+200+300 = 600
			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(HaveLen(0))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(600)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("sums values grouped by a field", func() {
		ks := specSubspace()

		// SUM order_id grouped by price — sum of order IDs for each price bucket.
		sumIdx := NewSumIndex("sum_id_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// price=100: order_ids 1,2; price=200: order_id 3
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// price=100: sum(order_id) = 1+2 = 3
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))

			// price=200: sum(order_id) = 3
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("decrements sum on delete", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete order with price=200
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Sum should be 100+300 = 400
			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(400)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("updates sum when record value changes", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Update order 1: price 100 → 500
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)})
			Expect(err).NotTo(HaveOccurred())

			// Sum should be 500+200 = 700
			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(700)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("updates sum when record moves between groups", func() {
		ks := specSubspace()

		// SUM order_id grouped by price
		sumIdx := NewSumIndex("sum_id_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// price=100: order_id=1, price=200: order_id=2
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Move order 1 from price=100 to price=200
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// price=100 should be 0 (may or may not have an entry depending on FDB cleanup)
			// price=200 should be 1+2 = 3
			if len(entries) == 2 {
				Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(0)}))
				Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(3)}))
			} else {
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
				Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("skips common entries on update (no-op optimization)", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Update with same price — no change to sum
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(100)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scans specific grouping key with TupleRangeAllOf", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_id_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Query only price=100
			entries, err := AsList(ctx, store.ScanIndex(sumIdx,
				TupleRangeAllOf(tuple.Tuple{int64(100)}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)})) // 1+2
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reverse scans sum index", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_id_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ReverseScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Reverse: price=200 first, then price=100
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateWhileWriteOnly skips sum for records outside built range", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.ClearAndMarkIndexWriteOnly(sumIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			// Partially built: PK range [0x00, pack(5))
			irs := NewIndexingRangeSet(ks, sumIdx)
			pk5 := tuple.Tuple{int64(5)}.Pack()
			_, err = irs.InsertRange(rtx.Transaction(), []byte{0x00}, pk5, false)
			Expect(err).NotTo(HaveOccurred())

			// In range — should update sum
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Outside range — should NOT update sum
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(999)})
			Expect(err).NotTo(HaveOccurred())

			// Complete the range set so checkIndexBuilt passes, then mark readable.
			_, err = irs.InsertRange(rtx.Transaction(), pk5, nil, false)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexReadable(sumIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			// Only 100+200=300, the 999 was outside the range
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(300)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles negative sums correctly", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(-50)})
			Expect(err).NotTo(HaveOccurred())

			// Sum should be 100 + (-50) = 50
			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(50)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("rebuilds SUM index correctly", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 4; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 50))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Rebuild index
			err = store.RebuildIndex(sumIdx)
			Expect(err).NotTo(HaveOccurred())

			// Sum should be 50+100+150+200 = 500
			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(500)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears entry when sum reaches zero with ClearWhenZero option", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price", Ungrouped(Field("price")))
		sumIdx.SetClearWhenZero(true)
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert single order and delete it — sum goes to zero
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Entry should be cleared (not left at sum=0)
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
