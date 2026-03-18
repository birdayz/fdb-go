package recordlayer

import (
	"context"
	"math/big"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("HilbertValue", func() {
	It("computes known values for simple 2D coordinates", func() {
		// (0, 0) should give a deterministic Hilbert value
		hv00 := hilbertValue([]int64{0, 0})
		Expect(hv00).NotTo(BeNil())
		Expect(hv00.Sign()).To(BeNumerically(">=", 0))

		// Different coordinates must produce different Hilbert values
		hv10 := hilbertValue([]int64{1, 0})
		hv01 := hilbertValue([]int64{0, 1})
		hv11 := hilbertValue([]int64{1, 1})

		Expect(hv00.Cmp(hv10)).NotTo(Equal(0), "(0,0) and (1,0) should differ")
		Expect(hv00.Cmp(hv01)).NotTo(Equal(0), "(0,0) and (0,1) should differ")
		Expect(hv00.Cmp(hv11)).NotTo(Equal(0), "(0,0) and (1,1) should differ")
		Expect(hv10.Cmp(hv01)).NotTo(Equal(0), "(1,0) and (0,1) should differ")

		// Same coordinates must give the same value (deterministic)
		hv00Again := hilbertValue([]int64{0, 0})
		Expect(hv00.Cmp(hv00Again)).To(Equal(0))
	})

	It("handles negative coordinates", func() {
		hvNeg := hilbertValue([]int64{-1, -1})
		hvPos := hilbertValue([]int64{1, 1})
		Expect(hvNeg).NotTo(BeNil())
		Expect(hvPos).NotTo(BeNil())
		Expect(hvNeg.Cmp(hvPos)).NotTo(Equal(0))
	})

	It("empty dimensions returns zero", func() {
		hv := hilbertValue([]int64{})
		Expect(hv.Cmp(big.NewInt(0))).To(Equal(0))
	})

	It("preserves locality — nearby points have closer Hilbert values than distant points", func() {
		// Core property of Hilbert curves: spatial locality preservation.
		hvOrigin := hilbertValue([]int64{100, 100})
		hvNear := hilbertValue([]int64{101, 100})
		hvFar := hilbertValue([]int64{100000, 100000})

		distNear := new(big.Int).Abs(new(big.Int).Sub(hvOrigin, hvNear))
		distFar := new(big.Int).Abs(new(big.Int).Sub(hvOrigin, hvFar))

		Expect(distNear.Cmp(distFar)).To(Equal(-1),
			"nearby point should have closer Hilbert value than distant point")
	})
})

var _ = Describe("RTree", func() {
	ctx := context.Background()

	It("insert and scan all", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_insert_scan")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			// Insert 3 points.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)}, // key suffix (PK)
				tuple.Tuple{},         // value
			)).To(Succeed())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(30), int64(40)}},
				tuple.Tuple{int64(2)},
				tuple.Tuple{},
			)).To(Succeed())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(50), int64(60)}},
				tuple.Tuple{int64(3)},
				tuple.Tuple{},
			)).To(Succeed())

			// Scan all — should return all 3 items.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(3))

			// Collect all key suffixes.
			pks := make(map[int64]bool)
			for _, item := range items {
				pk, ok := item.KeySuffix[0].(int64)
				Expect(ok).To(BeTrue())
				pks[pk] = true
			}
			Expect(pks).To(HaveKey(int64(1)))
			Expect(pks).To(HaveKey(int64(2)))
			Expect(pks).To(HaveKey(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete removes point", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_delete")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			// Insert 2 points.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{},
			)).To(Succeed())

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(30), int64(40)}},
				tuple.Tuple{int64(2)},
				tuple.Tuple{},
			)).To(Succeed())

			// Delete the first point.
			Expect(rt.Delete(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
			)).To(Succeed())

			// Scan — only second point should remain.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].KeySuffix).To(Equal(tuple.Tuple{int64(2)}))
			Expect(items[0].Point.Coordinates).To(Equal(tuple.Tuple{int64(30), int64(40)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete nonexistent point is a no-op", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_delete_noop")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{},
			)).To(Succeed())

			// Delete a point that was never inserted.
			Expect(rt.Delete(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(99), int64(99)}},
				tuple.Tuple{int64(999)},
			)).To(Succeed())

			// Original point still there.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update replaces value for same key", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_update")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			// Insert a point with value.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{int64(100)}, // original value
			)).To(Succeed())

			// Update same point + suffix with a new value.
			Expect(rt.InsertOrUpdate(rtx.Transaction(),
				Point{Coordinates: tuple.Tuple{int64(10), int64(20)}},
				tuple.Tuple{int64(1)},
				tuple.Tuple{int64(200)}, // updated value
			)).To(Succeed())

			// Scan — should have exactly 1 item with the new value.
			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(1))
			Expect(items[0].Value).To(Equal(tuple.Tuple{int64(200)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan empty tree returns nil", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_empty")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scan with MBR predicate filters results", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_mbr_predicate")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			// Insert points at (10, 20), (50, 60), (100, 200).
			for _, pt := range []struct {
				x, y, pk int64
			}{
				{10, 20, 1},
				{50, 60, 2},
				{100, 200, 3},
			} {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{pt.x, pt.y}},
					tuple.Tuple{pt.pk},
					tuple.Tuple{},
				)).To(Succeed())
			}

			// MBR predicate: only points within [0,70] x [0,70].
			queryMBR := MBR{Low: []int64{0, 0}, High: []int64{70, 70}}
			items, err := rt.Scan(rtx.Transaction(), nil, nil, func(m MBR) bool {
				return queryMBR.Overlaps(m)
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(2))

			pks := make(map[int64]bool)
			for _, item := range items {
				pk, _ := item.KeySuffix[0].(int64)
				pks[pk] = true
			}
			Expect(pks).To(HaveKey(int64(1)))
			Expect(pks).To(HaveKey(int64(2)))
			Expect(pks).NotTo(HaveKey(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clear removes all data", func() {
		ks := specSubspace()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			sub := ks.Sub("rtree_clear")
			config := DefaultRTreeConfig(2)
			storage := newRTreeStorage(sub, config)
			rt := NewRTree(storage, config)

			for i := int64(0); i < 5; i++ {
				Expect(rt.InsertOrUpdate(rtx.Transaction(),
					Point{Coordinates: tuple.Tuple{i * 10, i * 20}},
					tuple.Tuple{i},
					tuple.Tuple{},
				)).To(Succeed())
			}

			items, err := rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(HaveLen(5))

			rt.Clear(rtx.Transaction())

			items, err = rt.Scan(rtx.Transaction(), nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(items).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("MultidimensionalIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("basic lifecycle — save records and scan index", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3 orders with different (price, quantity).
			for _, o := range []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 100, 10},
				{2, 200, 20},
				{3, 300, 30},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Scan index — should return 3 entries.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			// Verify that all coordinate pairs are present.
			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				Expect(len(e.Key)).To(BeNumerically(">=", 2))
				x, ok := e.Key[0].(int64)
				Expect(ok).To(BeTrue())
				y, ok := e.Key[1].(int64)
				Expect(ok).To(BeTrue())
				found[coordPair{x, y}] = true
			}
			Expect(found).To(HaveKey(coordPair{100, 10}))
			Expect(found).To(HaveKey(coordPair{200, 20}))
			Expect(found).To(HaveKey(coordPair{300, 30}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete record clears index entry", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save one order.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Verify it is in the index.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			// Delete the record.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Index should be empty.
			entries, err = AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update record updates index entry", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save original order.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(100),
				Quantity: proto.Int32(10),
			})
			Expect(err).NotTo(HaveOccurred())

			// Update same order with new price and quantity.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(1),
				Price:    proto.Int32(500),
				Quantity: proto.Int32(50),
			})
			Expect(err).NotTo(HaveOccurred())

			// Should have exactly 1 entry with the new coordinates.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].Key[0]).To(Equal(int64(500)))
			Expect(entries[0].Key[1]).To(Equal(int64(50)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multiple records — save 5 and verify all present", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		type order struct {
			id       int64
			price    int32
			quantity int32
		}
		orders := []order{
			{1, 10, 100},
			{2, 20, 200},
			{3, 30, 300},
			{4, 40, 400},
			{5, 50, 500},
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range orders {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, _ := e.Key[0].(int64)
				y, _ := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			for _, o := range orders {
				Expect(found).To(HaveKey(coordPair{int64(o.price), int64(o.quantity)}))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("mixed save and delete — interleaved operations", func() {
		ks := specSubspace()

		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)
		mdIdx := NewMultidimensionalIndex("md_price_qty", dimExpr)
		builder := baseMetaData()
		builder.AddIndex("Order", mdIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Save 3.
			for _, id := range []int64{1, 2, 3} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(id),
					Price:    proto.Int32(int32(id * 100)),
					Quantity: proto.Int32(int32(id * 10)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Delete #2.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Save #4.
			_, err = store.SaveRecord(&gen.Order{
				OrderId:  proto.Int64(4),
				Price:    proto.Int32(400),
				Quantity: proto.Int32(40),
			})
			Expect(err).NotTo(HaveOccurred())

			// Should have 3 entries: #1, #3, #4.
			entries, err := AsList(ctx, store.ScanIndex(mdIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))

			type coordPair struct{ x, y int64 }
			found := make(map[coordPair]bool)
			for _, e := range entries {
				x, _ := e.Key[0].(int64)
				y, _ := e.Key[1].(int64)
				found[coordPair{x, y}] = true
			}
			Expect(found).To(HaveKey(coordPair{100, 10}))
			Expect(found).NotTo(HaveKey(coordPair{200, 20})) // deleted
			Expect(found).To(HaveKey(coordPair{300, 30}))
			Expect(found).To(HaveKey(coordPair{400, 40}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("DimensionsKeyExpression", func() {
	It("proto round-trip preserves expression", func() {
		wholeKey := Concat(Field("price"), Field("quantity"))
		dimExpr := Dimensions(wholeKey, 0, 2)

		// Serialize to proto.
		protoExpr := dimExpr.ToKeyExpression()
		Expect(protoExpr).NotTo(BeNil())
		Expect(protoExpr.Dimensions).NotTo(BeNil())
		Expect(protoExpr.Dimensions.GetPrefixSize()).To(Equal(int32(0)))
		Expect(protoExpr.Dimensions.GetDimensionsSize()).To(Equal(int32(2)))

		// Deserialize from proto.
		restored, err := KeyExpressionFromProto(protoExpr)
		Expect(err).NotTo(HaveOccurred())

		restoredDim, ok := restored.(*DimensionsKeyExpression)
		Expect(ok).To(BeTrue(), "restored expression should be *DimensionsKeyExpression")
		Expect(restoredDim.PrefixSize).To(Equal(0))
		Expect(restoredDim.DimensionsSize).To(Equal(2))
		Expect(restoredDim.ColumnSize()).To(Equal(dimExpr.ColumnSize()))
		Expect(restoredDim.FieldNames()).To(Equal(dimExpr.FieldNames()))
	})

	It("proto round-trip with prefix", func() {
		// 3-column key: 1 prefix + 2 dimensions
		wholeKey := Concat(Field("tags"), Field("price"), Field("quantity"))
		dimExpr := Dimensions(wholeKey, 1, 2)

		protoExpr := dimExpr.ToKeyExpression()
		restored, err := KeyExpressionFromProto(protoExpr)
		Expect(err).NotTo(HaveOccurred())

		restoredDim := restored.(*DimensionsKeyExpression)
		Expect(restoredDim.PrefixSize).To(Equal(1))
		Expect(restoredDim.DimensionsSize).To(Equal(2))
		Expect(restoredDim.SuffixSize()).To(Equal(0))
		Expect(restoredDim.ColumnSize()).To(Equal(3))
	})

	It("SplitIndexEntry correctly partitions tuple", func() {
		dimExpr := Dimensions(Concat(Field("price"), Field("quantity")), 0, 2)

		entry := tuple.Tuple{int64(100), int64(200)}
		prefix, dims, suffix := dimExpr.SplitIndexEntry(entry)

		Expect(prefix).To(BeNil())
		Expect(dims).To(Equal(tuple.Tuple{int64(100), int64(200)}))
		Expect(suffix).To(BeNil())
	})

	It("SplitIndexEntry with prefix and suffix", func() {
		// 4-column: 1 prefix, 2 dimensions, 1 suffix
		wholeKey := Concat(Field("tags"), Field("price"), Field("quantity"), Field("order_id"))
		dimExpr := Dimensions(wholeKey, 1, 2)

		entry := tuple.Tuple{"group1", int64(100), int64(200), int64(42)}
		prefix, dims, suffix := dimExpr.SplitIndexEntry(entry)

		Expect(prefix).To(Equal(tuple.Tuple{"group1"}))
		Expect(dims).To(Equal(tuple.Tuple{int64(100), int64(200)}))
		Expect(suffix).To(Equal(tuple.Tuple{int64(42)}))
	})
})
