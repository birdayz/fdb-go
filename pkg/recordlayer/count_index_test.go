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

var _ = Describe("CountIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("counts records by grouping key", func() {
		ks := specSubspace()

		// COUNT index grouped by price
		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 5 orders: 3 with price=100, 2 with price=200
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan count index — should see 2 entries
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Price 100: count=3
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(3)}))

			// Price 200: count=2
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("decrements count on delete", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 orders with price=100
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete one
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Count should be 2
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("updates count when record changes grouping key", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 2 orders with price=100, 1 with price=200
			for i := int64(1); i <= 2; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			order := &gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Update order 1 to change price from 100 to 200
			updatedOrder := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)}
			_, err = store.SaveRecord(updatedOrder)
			Expect(err).NotTo(HaveOccurred())

			// Price 100: count=1, Price 200: count=2
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("handles ungrouped count (total count)", func() {
		ks := specSubspace()

		// Ungrouped count — empty grouping key, counts all records
		countIdx := NewCountIndex("total_count", Ungrouped(EmptyKey()))
		builder := baseMetaData()
		builder.AddUniversalIndex(countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 4 orders
			for i := int64(1); i <= 4; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Single entry with empty key, count=4
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(HaveLen(0))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(4)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scans specific grouping key with TupleRangeAllOf", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert orders with prices 100, 200, 300
			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			order := &gen.Order{OrderId: proto.Int64(6), Price: proto.Int32(300)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Query only price 200
			entries, err := AsList(ctx, store.ScanIndex(countIdx,
				TupleRangeAllOf(tuple.Tuple{int64(200)}), nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("shared scan-record budget aggregates across legs of a fan-out (RFC-106a)", func() {
		// countKVCursor threads a (possibly shared) *ScanLimiterState through
		// resolveScanLimiterState, so every leg of an IN-join/IN-union fan-out
		// charges the same aggregate budget instead of resetting per leg —
		// same mechanism as the multidimensional prefix skip-scan and the
		// sqldriver IN-join/IN-union tests, and the bitmap index's own
		// equivalent test. Nothing in this file exercised it: every other
		// test here drives exactly one scan against one fresh ScanProperties,
		// so a regression that stopped sharing scanState across legs
		// (reverting countKVCursor to a fresh per-cursor budget) would ship
		// silently.
		//
		// Drive store.ScanIndex N times reusing the SAME ScanProperties (and
		// so the same *ScanLimiterState pointer) — the mechanism
		// executeInJoin/executeInUnion use for real IN-list legs
		// (props.ClearSkipAndLimit(), never a fresh DefaultExecuteProperties()
		// per leg). Two distinct prices scanned per leg are individually far
		// under the cap; only the aggregate across legs can trip it.
		ks := specSubspace()

		countIdx := NewCountIndex("count_fanout_legs", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		const scanLimit = 10
		const numLegs = 10

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, price := range []int32{100, 200} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(price)), Price: proto.Int32(price)})
				Expect(err).NotTo(HaveOccurred())
			}

			// ONE ScanProperties (and so one shared *ScanLimiterState) reused
			// across every leg — the fan-out sharing mechanism under test.
			props := ForwardScan()
			props.ExecuteProperties = props.ExecuteProperties.WithScannedRecordsLimit(scanLimit)

			var tripped int
			for leg := 0; leg < numLegs; leg++ {
				legProps := props
				legProps.ExecuteProperties = legProps.ExecuteProperties.ClearSkipAndLimit()
				entries, legErr := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, legProps))
				if legErr != nil {
					var sle *ScanLimitReachedError
					Expect(errors.As(legErr, &sle)).To(BeTrue(),
						"leg %d: want ScanLimitReachedError, got: %v", leg, legErr)
					tripped++
					continue
				}
				Expect(entries).To(HaveLen(2), "leg %d: an untripped leg must see both grouping keys", leg)
			}

			Expect(tripped).To(BeNumerically(">", 0),
				"the aggregate scan budget across %d individually-small legs (cap=%d) never tripped — "+
					"the shared ScanLimiterState is not being charged across count-index legs", numLegs, scanLimit)
			Expect(tripped).To(BeNumerically("<", numLegs),
				"every leg tripped — the cap is too tight to also prove early legs succeed on their own merits")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateWhileWriteOnly skips count for records outside built range", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Mark index WRITE_ONLY (clears any auto-built data).
			_, err = store.ClearAndMarkIndexWriteOnly(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			// Simulate partial build: mark PK range [0x00, pack(5)) as built.
			// PKs 1-4 are "already built", PK 5+ are not.
			irs := NewIndexingRangeSet(ks, countIdx)
			pk5 := tuple.Tuple{int64(5)}.Pack()
			_, err = irs.InsertRange(rtx.Transaction(), []byte{0x00}, pk5, false)
			Expect(err).NotTo(HaveOccurred())

			// Save records in built range — should update count.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Save records outside built range — should NOT update count.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Complete the range set so checkIndexBuilt passes, then mark readable.
			_, err = irs.InsertRange(rtx.Transaction(), pk5, nil, false)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexReadable(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Only records in built range (PK=2, PK=3) should be counted.
			// PK=7 and PK=10 were outside the range — skipped.
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateWhileWriteOnly updates count for records inside built range", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Mark index WRITE_ONLY.
			_, err = store.ClearAndMarkIndexWriteOnly(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			// Simulate full range built (entire key space).
			irs := NewIndexingRangeSet(ks, countIdx)
			_, err = irs.InsertRange(rtx.Transaction(), []byte{0x00}, []byte{0xff}, false)
			Expect(err).NotTo(HaveOccurred())

			// All saves should update the count — entire range is built.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.MarkIndexReadable(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateWhileWriteOnly handles delete in built range", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Mark index WRITE_ONLY with full range built.
			_, err = store.ClearAndMarkIndexWriteOnly(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())
			irs := NewIndexingRangeSet(ks, countIdx)
			_, err = irs.InsertRange(rtx.Transaction(), []byte{0x00}, []byte{0xff}, false)
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 records.
			for i := int64(1); i <= 3; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete one — should decrement count.
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			_, err = store.MarkIndexReadable(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("UpdateWhileWriteOnly skips delete for records outside built range", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Mark WRITE_ONLY with only range [0x00, pack(5)) built.
			_, err = store.ClearAndMarkIndexWriteOnly(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())
			irs := NewIndexingRangeSet(ks, countIdx)
			pk5 := tuple.Tuple{int64(5)}.Pack()
			_, err = irs.InsertRange(rtx.Transaction(), []byte{0x00}, pk5, false)
			Expect(err).NotTo(HaveOccurred())

			// Insert record in built range, then save outside built range.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Delete the record outside built range — should NOT decrement.
			_, err = store.DeleteRecord(tuple.Tuple{int64(7)})
			Expect(err).NotTo(HaveOccurred())

			// Complete the range set so checkIndexBuilt passes, then mark readable.
			_, err = irs.InsertRange(rtx.Transaction(), pk5, nil, false)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexReadable(countIdx.Name)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			// Only PK=2 was counted (in built range). PK=7 was skipped on insert AND delete.
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(1)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("reverse scans count index", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(1); i <= 3; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(100)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			for i := int64(4); i <= 5; i++ {
				order := &gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(200)}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}

			// Reverse scan
			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ReverseScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Reverse order: 200 first, then 100
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[1].Value).To(Equal(tuple.Tuple{int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears entry when count reaches zero with ClearWhenZero option", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		countIdx.SetClearWhenZero(true)
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 2 orders with price=100, 1 with price=200
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Delete the only price=200 order — should clear the entry entirely
			_, err = store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Only price=100 remains — price=200 entry was cleared (not left at 0)
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(100)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("without ClearWhenZero leaves zero-value entries", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("count_by_price", GroupAll(Field("price")))
		// Default: no ClearWhenZero
		builder := baseMetaData()
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())

			// Without ClearWhenZero, the entry with count=0 remains
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(entries[0].Value).To(Equal(tuple.Tuple{int64(0)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
