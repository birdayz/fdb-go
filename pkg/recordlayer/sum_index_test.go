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

	// The sibling of the test above, on the dimension it does not reach. That
	// one drives the sum to zero by REMOVING the only record; this one never
	// removes anything -- it saves a record whose value is the negation of the
	// group's running total, so the zero is reached on the ADDING path.
	//
	// Java issues the COMPARE_AND_CLEAR after every mutation, with no remove
	// guard (AtomicMutationIndexMaintainer.updateIndexKeys), so both spellings
	// clear. Gating the clear on `remove` passes the remove-path test above and
	// still leaves a Go store holding an entry a Java store would not -- the
	// same metadata and the same record sequence producing different index
	// content, which is a wire divergence, not a behavioural nicety.
	It("clears entry when an added record cancels the running sum to zero", func() {
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

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(42)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(-42)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0),
				"both records are still present, but their sum is zero and clearWhenZero is set, "+
					"so Java would have cleared the entry on the second save")

			// And the entry comes back the moment the sum leaves zero again,
			// so the clear is not a tombstone.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(7)})
			Expect(err).NotTo(HaveOccurred())
			entries, err = AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(7)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// GROUPED, which is a different code path and not a cosmetic variation of
	// the ungrouped tests above. A grouped SUM insert is served by an inline
	// fast path in the maintainer that writes its ADD directly instead of going
	// through sumMutation.applyMutation; the ungrouped spelling has no such
	// path. So the clear can be correct for every ungrouped test in this file
	// and still be missing for every grouped insert, which is exactly the state
	// this file was in -- the option looked covered because the covered
	// spelling was the one without the fast path.
	It("clears a grouped entry when the group's values sum to zero on insert", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price_by_quantity",
			GroupBy(Field("price"), Field("quantity")))
		sumIdx.SetClearWhenZero(true)
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		save := func(id int64, quantity, price int32) {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(id),
					Quantity: proto.Int32(quantity),
					Price:    proto.Int32(price),
				})
			})
			Expect(err).NotTo(HaveOccurred())
		}
		groups := func() map[int64]int64 {
			out := map[int64]int64{}
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
				Expect(err).NotTo(HaveOccurred())
				for _, e := range entries {
					out[e.Key[0].(int64)] = e.Value[0].(int64)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}

		// group 1 cancels to zero across two transactions; group 2 does not.
		save(1, 1, 42)
		Expect(groups()).To(Equal(map[int64]int64{1: 42}))
		save(2, 1, -42)
		save(3, 2, 7)
		Expect(groups()).To(Equal(map[int64]int64{2: 7}),
			"group 1's entry summed to zero on an INSERT and clearWhenZero is set, "+
				"so it must be cleared exactly as the ungrouped spelling is")

		// A single record whose value is itself zero is the case
		// IndexOptions.CLEAR_WHEN_ZERO names outright: the group must not appear.
		save(4, 3, 0)
		Expect(groups()).To(Equal(map[int64]int64{2: 7}))
	})

	// The guard, not the clear. Everything above asserts what happens when
	// clearWhenZero IS set; this asserts that without it the zero entry SURVIVES,
	// which is the default every SQL-created SUM index runs under.
	//
	// It is the SUM sibling of count_index_test.go's "without ClearWhenZero
	// leaves zero-value entries", and it was missing. Dropping the guard from all
	// three atomic arms at once is caught by that count test, so the option
	// looked covered; dropping it from the SUM arm ALONE was green across the
	// whole package. An unguarded SUM clear would make every SQL-created SUM
	// index silently drop groups that sum to zero -- live groups, wrong rows,
	// and a divergence from a Java store maintaining the same index from the
	// same metadata.
	It("without ClearWhenZero leaves a zero-valued sum entry in place", func() {
		ks := specSubspace()

		sumIdx := NewSumIndex("sum_price_unguarded", GroupBy(Field("price"), Field("quantity")))
		// Deliberately NOT SetClearWhenZero: this is the SQL default.
		builder := baseMetaData()
		builder.AddIndex("Order", sumIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		save := func(id int64, quantity, price int32) {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				return store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(id),
					Quantity: proto.Int32(quantity),
					Price:    proto.Int32(price),
				})
			})
			Expect(err).NotTo(HaveOccurred())
		}

		// Reach zero on the ADDING path, which is the path the inline grouped
		// fast path serves and the one an unguarded clear would fire on.
		save(1, 1, 42)
		save(2, 1, -42)
		// And on the adding path with a value that is itself zero.
		save(3, 2, 0)

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			entries, err := AsList(ctx, store.ScanIndex(sumIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			got := map[int64]int64{}
			for _, e := range entries {
				got[e.Key[0].(int64)] = e.Value[0].(int64)
			}
			Expect(got).To(Equal(map[int64]int64{1: 0, 2: 0}),
				"clearWhenZero is NOT set, so both zero-summing groups must keep their "+
					"entries at 0. A clear firing here would drop live groups and diverge "+
					"from a Java store reading the same metadata.")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
