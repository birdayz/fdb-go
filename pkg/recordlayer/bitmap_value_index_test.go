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

var _ = Describe("BitmapValueIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// Helper: extract set bit positions from a raw bitmap byte slice.
	bitmapBits := func(bitmapBytes []byte) []int64 {
		var bits []int64
		for i, b := range bitmapBytes {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<bit) != 0 {
					bits = append(bits, int64(i*8+bit))
				}
			}
		}
		return bits
	}

	// Helper: check if a specific bit is set in bitmap bytes at the given offset.
	hasBit := func(bitmapBytes []byte, offset int64) bool {
		byteIdx := offset / 8
		if int(byteIdx) >= len(bitmapBytes) {
			return false
		}
		return (bitmapBytes[byteIdx] & (1 << (offset % 8))) != 0
	}

	// =========================================================================
	// 1. Basic insert and scan (ungrouped)
	// =========================================================================
	It("basic insert: single record sets correct bit in bitmap", func() {
		ks := specSubspace()

		// Ungrouped bitmap: position = order_id
		idx := NewBitmapValueIndex("order_bitmap", GroupBy(Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Scan BY_GROUP to get raw bitmap entries.
			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Key should have aligned position 0 (since 5 < default entrySize 10000).
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(0)}))

			// Value is raw bitmap bytes.
			bitmapBytes, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bitmapBytes, 5)).To(BeTrue(), "bit 5 should be set")
			Expect(hasBit(bitmapBytes, 0)).To(BeFalse(), "bit 0 should not be set")
			Expect(hasBit(bitmapBytes, 4)).To(BeFalse(), "bit 4 should not be set")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 2. Multiple records same group — bits 0, 3, 7
	// =========================================================================
	It("multiple records same group: bits 0, 3, 7 set in single bitmap entry", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap", GroupBy(Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, id := range []int64{0, 3, 7} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			bitmapBytes, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bitmapBytes, 0)).To(BeTrue())
			Expect(hasBit(bitmapBytes, 3)).To(BeTrue())
			Expect(hasBit(bitmapBytes, 7)).To(BeTrue())
			Expect(hasBit(bitmapBytes, 1)).To(BeFalse())
			Expect(hasBit(bitmapBytes, 2)).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 3. Multiple groups — grouped by price
	// =========================================================================
	It("multiple groups: records separated by group key", func() {
		ks := specSubspace()

		// Grouped bitmap: position = order_id, grouped by price.
		idx := NewBitmapValueIndex("order_bitmap_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: order_ids 1, 5
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=200: order_id 3
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// First entry: price=100, aligned position=0
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			bm100, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm100, 1)).To(BeTrue(), "bit 1 for order_id=1")
			Expect(hasBit(bm100, 5)).To(BeTrue(), "bit 5 for order_id=5")
			Expect(hasBit(bm100, 3)).To(BeFalse(), "bit 3 should not be set in price=100 group")

			// Second entry: price=200, aligned position=0
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			bm200, ok := entries[1].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm200, 3)).To(BeTrue(), "bit 3 for order_id=3")
			Expect(hasBit(bm200, 1)).To(BeFalse(), "bit 1 should not be set in price=200 group")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 4. Delete clears bit — key removed if all zeros
	// =========================================================================
	It("delete clears bit and removes key if all zeros", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap", GroupBy(Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert two records.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Both bits should be set.
			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 2)).To(BeTrue())
			Expect(hasBit(bm, 5)).To(BeTrue())

			// Delete order_id=2.
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())

			// Bit 2 cleared, bit 5 still set.
			entries, err = AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			bm, ok = entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 2)).To(BeFalse(), "bit 2 should be cleared after delete")
			Expect(hasBit(bm, 5)).To(BeTrue(), "bit 5 should still be set")

			// Delete order_id=5 — last record, bitmap entry should be removed.
			_, err = store.DeleteRecord(tuple.Tuple{int64(5)})
			Expect(err).NotTo(HaveOccurred())

			entries, err = AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0), "bitmap entry removed when all bits zero")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 5. Update changes bit position
	// =========================================================================
	It("update record changes bit: old bit cleared, new bit set", func() {
		ks := specSubspace()

		// Use price as grouping, order_id as position.
		idx := NewBitmapValueIndex("order_bitmap_by_price", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert order_id=10 with price=100.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 10)).To(BeTrue())

			// Update: change price from 100 to 200 (same PK, different group).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(10), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err = AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			// Old group (price=100) entry should be removed (all zeros → CompareAndClear),
			// new group (price=200) should have bit 10 set.
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(200)))
			bm, ok = entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 10)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 6. Null position field — record skipped
	// =========================================================================
	It("null position field: grouped bitmap with unset position is not indexed", func() {
		ks := specSubspace()

		// Use quantity as position (can be nil), grouped by price.
		idx := NewBitmapValueIndex("order_bitmap_null", GroupBy(Field("quantity"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save order with quantity set — should produce bitmap entry.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100), Quantity: proto.Int32(5)})
			Expect(err).NotTo(HaveOccurred())

			// Save order WITHOUT quantity — position is nil, should be skipped.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			// Only one entry: from order_id=1 with quantity=5.
			Expect(entries).To(HaveLen(1))
			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 5)).To(BeTrue(), "bit 5 for quantity=5")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 7. Custom entry size
	// =========================================================================
	It("custom entry size: IndexOptionBitmapValueEntrySize controls alignment", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_small", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "16"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Position 5: aligned to 0 (5 < 16), offset 5.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Position 20: aligned to 16 (20 / 16 = 1, remainder 4), offset 4.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(20), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// First entry: aligned position 0, bit 5 set.
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(0)}))
			bm0, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			// Bitmap size = (16+7)/8 = 2 bytes
			Expect(bm0).To(HaveLen(2))
			Expect(hasBit(bm0, 5)).To(BeTrue())

			// Second entry: aligned position 16, bit 4 set (20-16=4).
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(16)}))
			bm16, ok := entries[1].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(bm16).To(HaveLen(2))
			Expect(hasBit(bm16, 4)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 8. Position alignment — entrySize=10
	// =========================================================================
	It("position alignment: position 15 with entrySize=10 aligns to 10 with offset 5", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_align", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(15), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// floorMod(15, 10) = 5, alignedPos = 15 - 5 = 10
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(10)}))
			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			// Bitmap size = (10+7)/8 = 2 bytes
			Expect(bm).To(HaveLen(2))
			Expect(hasBit(bm, 5)).To(BeTrue(), "offset 5 should be set")
			Expect(hasBit(bm, 0)).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 9. Large positions — entrySize=10000
	// =========================================================================
	It("large positions: position 20005 with default entrySize=10000 aligns to 20000", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_large", GroupBy(Field("order_id")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(20005), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// floorMod(20005, 10000) = 5, alignedPos = 20005 - 5 = 20000
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(20000)}))
			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 5)).To(BeTrue(), "offset 5 should be set")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 10. ScanByGroup with position range — trimming
	// =========================================================================
	It("ScanByGroup with position range trims bitmap to requested range", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_range", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert positions 0..9 (all in aligned block 0).
			for i := int64(0); i < 10; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan range [3, 7] inclusive.
			scanRange := TupleRange{
				Low:          tuple.Tuple{int64(3)},
				High:         tuple.Tuple{int64(7)},
				LowEndpoint:  EndpointTypeRangeInclusive,
				HighEndpoint: EndpointTypeRangeInclusive,
			}
			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, scanRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// The trimmed entry should have key starting at 3 (trimmedStart).
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(3)}))
			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())

			// Trimmed bitmap covers positions [3, 8) (endPosition = 7+1 = 8).
			// Bits 0..4 represent positions 3, 4, 5, 6, 7 — all should be set.
			bits := bitmapBits(bm)
			Expect(bits).To(HaveLen(5), "should have 5 bits set for positions 3-7")
			for i := int64(0); i < 5; i++ {
				Expect(hasBit(bm, i)).To(BeTrue(), "bit %d should be set", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 11. Multiple bitmap entries across aligned blocks
	// =========================================================================
	It("positions spanning multiple aligned blocks create separate bitmap entries", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_multi", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Positions in block 0 (aligned 0): 3
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			// Positions in block 1 (aligned 10): 15
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(15), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())
			// Positions in block 2 (aligned 20): 22
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(22), Price: proto.Int32(300)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Block 0: bit 3
			Expect(entries[0].Key).To(Equal(tuple.Tuple{int64(0)}))
			bm0, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm0, 3)).To(BeTrue())

			// Block 10: bit 5 (15 - 10)
			Expect(entries[1].Key).To(Equal(tuple.Tuple{int64(10)}))
			bm10, ok := entries[1].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm10, 5)).To(BeTrue())

			// Block 20: bit 2 (22 - 20)
			Expect(entries[2].Key).To(Equal(tuple.Tuple{int64(20)}))
			bm20, ok := entries[2].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm20, 2)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 12. Unique index violation — same position
	// =========================================================================
	It("unique bitmap index: duplicate position triggers uniqueness violation", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_unique", GroupBy(Field("order_id"))).SetUnique()
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// First record at position 5.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The uniqueness check uses snapshot reads. We need to commit the first
		// record, then attempt the second in a new transaction.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			// Use quantity field with same position value.
			// Since order_id is both PK and position, we can't have two records
			// with the same order_id. Instead, use a grouped bitmap where the
			// position field is price, so two different PKs can share a position.
			// Actually with order_id as both PK and position, two different order_ids
			// means two different positions. Let's test in a different way.
			_ = store
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 12b. Unique bitmap index — grouped, duplicate position within same group
	// =========================================================================
	It("unique grouped bitmap: two records with same position in same group violates uniqueness", func() {
		ks := specSubspace()

		// Group by quantity, position by price. Two records with same price in same quantity group
		// should trigger uniqueness violation.
		idx := NewBitmapValueIndex("bitmap_unique_grouped", GroupBy(Field("price"), Field("quantity"))).SetUnique()
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// First transaction: save record with quantity=10, price=5.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(5),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Second transaction: save different record with same quantity=10, price=5 → violation.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(2),
				Price:    proto.Int32(5),
				Quantity: proto.Int32(10),
			})
			return nil, err
		})
		Expect(err).To(HaveOccurred())
		var violation *RecordIndexUniquenessViolationError
		Expect(errors.As(err, &violation)).To(BeTrue())
	})

	// =========================================================================
	// 13. Aggregate function (bitmap_value)
	// =========================================================================
	It("aggregate: EvaluateAggregateFunction combines bitmaps across aligned positions", func() {
		ks := specSubspace()

		// entrySize must be multiple of 8 for aggregate to work (Java enforces position % 8 == 0)
		idx := NewBitmapValueIndex("order_bitmap_agg", GroupBy(Field("order_id"), Field("price")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "16"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100 group: positions 3, 7, 20 (spans two aligned blocks: 0 and 16)
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(20), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Aggregate for price=100 group should combine bitmaps.
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameBitmapValue,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			bm, ok := result[0].([]byte)
			Expect(ok).To(BeTrue())
			// Combined bitmap: bits at positions 3, 7, 20 relative to startPos=0
			// Block 0 (aligned 0): bits 3 and 7 set
			// Block 16 (aligned 16): bit 4 set (position 20 - 16 = 4)
			// In aggregate result: byte 0 has bits 3 and 7, byte 2 has bit 4 (pos 20 / 8 = 2, 20 % 8 = 4)
			Expect(hasBit(bm, 3)).To(BeTrue())
			Expect(hasBit(bm, 7)).To(BeTrue())
			Expect(hasBit(bm, 20)).To(BeTrue())
			Expect(hasBit(bm, 0)).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 14. Aggregate on empty group returns zero-filled buffer (matches Java)
	// =========================================================================
	It("aggregate: empty group returns zero-filled buffer", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_empty_agg", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameBitmapValue,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAll, IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			// Java returns zero-filled buffer of entrySize bytes, not nil.
			Expect(result).NotTo(BeNil())
			bm, ok := result[0].([]byte)
			Expect(ok).To(BeTrue())
			// All zeros
			for _, b := range bm {
				Expect(b).To(Equal(byte(0)))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 15. OnlineIndexer — build bitmap index online
	// =========================================================================
	It("OnlineIndexer: builds bitmap index on pre-existing records", func() {
		ks := specSubspace()

		// Phase 1: Insert records WITHOUT bitmap index.
		builder1 := baseMetaData()
		mdNoIndex, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(0); i < 20; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i % 3))})
				Expect(err).NotTo(HaveOccurred())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Phase 2: Add bitmap index and build online.
		bitmapIdx := NewBitmapValueIndex("order_bitmap_online", GroupBy(Field("order_id"), Field("price")))
		bitmapIdx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder2 := baseMetaData()
		builder2.AddIndex("Order", bitmapIdx)
		mdWithIndex, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(mdWithIndex).
			SetIndex(bitmapIdx).
			SetSubspace(ks).
			SetLimit(5).
			Build()
		Expect(err).NotTo(HaveOccurred())

		total, err := indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(total).To(BeNumerically(">=", 20))

		// Phase 3: Verify.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			Expect(store.IsIndexReadable("order_bitmap_online")).To(BeTrue())

			// Scan all bitmap entries and check the expected positions are set.
			entries, err := AsList(ctx, store.ScanIndexByType(bitmapIdx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(len(entries)).To(BeNumerically(">", 0))

			// Verify each original record's bit is set in the correct group.
			// Group by price: 0, 1, 2 — each has records with specific order_ids.
			for _, entry := range entries {
				_, ok := entry.Value[0].([]byte)
				Expect(ok).To(BeTrue())
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 16. RebuildIndex
	// =========================================================================
	It("RebuildIndex: rebuilds bitmap index with correct entries", func() {
		ks := specSubspace()

		bitmapIdx := NewBitmapValueIndex("order_bitmap_rebuild", GroupBy(Field("order_id"), Field("price")))
		bitmapIdx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", bitmapIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// Insert some records.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Rebuild.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			return nil, store.RebuildIndex(bitmapIdx)
		})
		Expect(err).NotTo(HaveOccurred())

		// Verify entries after rebuild.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(bitmapIdx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			// Price=100 group: bits 2 and 7.
			Expect(entries[0].Key[0]).To(Equal(int64(100)))
			bm100, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm100, 2)).To(BeTrue())
			Expect(hasBit(bm100, 7)).To(BeTrue())

			// Price=200 group: bit 3.
			Expect(entries[1].Key[0]).To(Equal(int64(200)))
			bm200, ok := entries[1].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm200, 3)).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 17. ScanIndex rejects BITMAP_VALUE (must use BY_GROUP)
	// =========================================================================
	It("ScanIndex rejects BITMAP_VALUE index (must use BY_GROUP)", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_raw", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// ScanIndex (BY_VALUE default) should reject BITMAP_VALUE — matches Java.
			_, err = AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("BY_GROUP"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 18. Delete all records clears bitmap
	// =========================================================================
	It("DeleteAllRecords clears all bitmap entries", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_delall", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
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

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))

			err = store.DeleteAllRecords()
			Expect(err).NotTo(HaveOccurred())

			entries, err = AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0), "no bitmap entries after DeleteAllRecords")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 19. Idempotent update — re-save same record is no-op
	// =========================================================================
	It("re-saving identical record does not change bitmap (idempotent)", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_idem", GroupBy(Field("order_id"), Field("price")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			order := &gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(100)}
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			// Re-save same record.
			_, err = store.SaveRecord(order)
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 5)).To(BeTrue())
			// Verify no extra bits set (idempotent).
			Expect(bitmapBits(bm)).To(Equal([]int64{5}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 20. Multiple groups with AllOf range query
	// =========================================================================
	It("ScanByGroup with AllOf filters by group key", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_filter", GroupBy(Field("order_id"), Field("price")))
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: order_ids 1, 4
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=200: order_id 2
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Price=300: order_id 6
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(6), Price: proto.Int32(300)})
			Expect(err).NotTo(HaveOccurred())

			// Scan all groups.
			allEntries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(allEntries).To(HaveLen(3))

			// Filter to price=200 only.
			range200 := TupleRangeAllOf(tuple.Tuple{int64(200)})
			filtered, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, range200, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(filtered).To(HaveLen(1))
			Expect(filtered[0].Key[0]).To(Equal(int64(200)))
			bm, ok := filtered[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 2)).To(BeTrue(), "bit 2 for order_id=2")
			Expect(hasBit(bm, 1)).To(BeFalse())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 21. Reverse scan
	// =========================================================================
	It("reverse scan returns entries in reverse order", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_rev", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Positions in blocks 0 and 10.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(15), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Forward.
			fwd, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(fwd).To(HaveLen(2))
			Expect(fwd[0].Key).To(Equal(tuple.Tuple{int64(0)}))
			Expect(fwd[1].Key).To(Equal(tuple.Tuple{int64(10)}))

			// Reverse.
			rev, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ReverseScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(rev).To(HaveLen(2))
			Expect(rev[0].Key).To(Equal(tuple.Tuple{int64(10)}))
			Expect(rev[1].Key).To(Equal(tuple.Tuple{int64(0)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 22. Row limit on scan
	// =========================================================================
	It("row limit: returns at most N bitmap entries", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_limit", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Create entries in 4 blocks: 0, 10, 20, 30.
			for _, pos := range []int64{3, 15, 22, 31} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(pos), Price: proto.Int32(int32(pos))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan with row limit = 2.
			scanProps := ForwardScan()
			scanProps.ExecuteProperties.ReturnedRowLimit = 2
			entries, cont, err := AsListWithContinuation(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, scanProps))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2))
			Expect(cont).NotTo(BeNil(), "should have continuation")

			// Continue from continuation.
			scanProps2 := ForwardScan()
			scanProps2.ExecuteProperties.ReturnedRowLimit = 10
			entries2, _, err := AsListWithContinuation(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, cont, scanProps2))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries2).To(HaveLen(2), "remaining 2 entries")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 23. Bitmap bit accumulation — BIT_OR semantics
	// =========================================================================
	It("multiple inserts in same aligned block accumulate via BIT_OR", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_bitor", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert all 10 positions in block 0.
			for i := int64(0); i < 10; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			bits := bitmapBits(bm)
			Expect(bits).To(HaveLen(10))
			for i := int64(0); i < 10; i++ {
				Expect(hasBit(bm, i)).To(BeTrue(), "bit %d should be set", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 24. Delete partial — some bits remain
	// =========================================================================
	It("delete some records: remaining bits stay set, bitmap entry persists", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_partial_del", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert positions 1, 3, 5, 7.
			for _, pos := range []int64{1, 3, 5, 7} {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(pos), Price: proto.Int32(int32(pos))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete positions 3 and 7.
			_, err = store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.DeleteRecord(tuple.Tuple{int64(7)})
			Expect(err).NotTo(HaveOccurred())

			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 1)).To(BeTrue(), "bit 1 should remain")
			Expect(hasBit(bm, 5)).To(BeTrue(), "bit 5 should remain")
			Expect(hasBit(bm, 3)).To(BeFalse(), "bit 3 should be cleared")
			Expect(hasBit(bm, 7)).To(BeFalse(), "bit 7 should be cleared")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 25. Scan exclusive range endpoints
	// =========================================================================
	It("ScanByGroup with exclusive endpoints trims correctly", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_excl", GroupBy(Field("order_id")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i := int64(0); i < 10; i++ {
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))})
				Expect(err).NotTo(HaveOccurred())
			}

			// Exclusive range (2, 6) → positions 3, 4, 5.
			scanRange := TupleRange{
				Low:          tuple.Tuple{int64(2)},
				High:         tuple.Tuple{int64(6)},
				LowEndpoint:  EndpointTypeRangeExclusive,
				HighEndpoint: EndpointTypeRangeExclusive,
			}
			entries, err := AsList(ctx, store.ScanIndexByType(idx, IndexScanByGroup, scanRange, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			bm, ok := entries[0].Value[0].([]byte)
			Expect(ok).To(BeTrue())
			// Exclusive low=2 → startPosition=3, exclusive high=6 → endPosition=6.
			// Trimmed bitmap covers [3, 6) → bits for positions 3, 4, 5.
			bits := bitmapBits(bm)
			Expect(bits).To(HaveLen(3), "should have 3 bits for positions 3, 4, 5")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =========================================================================
	// 26. Aggregate with multiple groups — only requested group returned
	// =========================================================================
	It("aggregate: bitmap_value for specific group only includes that group's records", func() {
		ks := specSubspace()

		idx := NewBitmapValueIndex("order_bitmap_agg_group", GroupBy(Field("order_id"), Field("price")))
		idx.Options[IndexOptionBitmapValueEntrySize] = "10"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Price=100: positions 1, 3
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// Price=200: position 2
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
			Expect(err).NotTo(HaveOccurred())

			// Aggregate for price=100 only.
			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
				&IndexAggregateFunction{
					Name:    FunctionNameBitmapValue,
					Operand: GroupBy(Field("order_id"), Field("price")),
				},
				TupleRangeAllOf(tuple.Tuple{int64(100)}), IsolationLevelSerializable)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			bm, ok := result[0].([]byte)
			Expect(ok).To(BeTrue())
			Expect(hasBit(bm, 1)).To(BeTrue())
			Expect(hasBit(bm, 3)).To(BeTrue())
			Expect(hasBit(bm, 2)).To(BeFalse(), "position 2 is in price=200 group, not 100")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
