package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"sort"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// newWindowedVectorIndex builds a VECTOR index carrying a row-number window
// predicate — the shape SlidingWindowIndexMaintainerFactory.isSlidingWindowIndex
// selects. The vector is (coord_x, coord_y), so the ordering field (price) and
// the partition field (quantity) stay independent of the vector itself; a test
// that reused one field for two roles could not tell window movement apart from
// vector movement.
func newWindowedVectorIndex(name string, size int32, dir gen.RowNumberWindowPredicate_Direction, partitionFields ...string) *Index {
	idx := NewVectorIndex(name, Concat(Field("coord_x"), Field("coord_y")), 2)
	rn := &gen.RowNumberWindowPredicate{
		OrderingField: []string{"price"},
		Size:          proto.Int32(size),
		Direction:     dir.Enum(),
	}
	for _, f := range partitionFields {
		rn.PartitionFields = append(rn.PartitionFields, &gen.FieldPath{Field: []string{f}})
	}
	Expect(idx.SetPredicateProto(&gen.Predicate{RowNumberWindowPredicate: rn})).To(Succeed())
	return idx
}

// newPartitionedWindowedVectorIndex is the deleteRecordsWhere-capable shape:
// the HNSW graph is prefix-partitioned by the same column the window partitions
// by, via KeyWithValue(Concat(prefixField, vector...), 1). Java needs this
// because deleteWhere must be acceptable to BOTH the window (against the
// partition key) and the wrapped vector maintainer (against its own
// KeyWithValue expression).
func newPartitionedWindowedVectorIndex(name string, size int32, dir gen.RowNumberWindowPredicate_Direction, partitionField string) *Index {
	idx := NewVectorIndex(name,
		KeyWithValue(Concat(Field(partitionField), Field("coord_x"), Field("coord_y")), 1), 2)
	rn := &gen.RowNumberWindowPredicate{
		OrderingField:   []string{"price"},
		Size:            proto.Int32(size),
		Direction:       dir.Enum(),
		PartitionFields: []*gen.FieldPath{{Field: []string{partitionField}}},
	}
	Expect(idx.SetPredicateProto(&gen.Predicate{RowNumberWindowPredicate: rn})).To(Succeed())
	return idx
}

// slidingWindowSubspaceFor rebuilds the keyspace-10 subspace the maintainer
// writes under, from the OUTSIDE — from the store subspace and the index's
// subspace tuple key — so the assertions below are about stored bytes rather
// than about the maintainer agreeing with itself.
func slidingWindowSubspaceFor(storeSubspace subspace.Subspace, idx *Index) subspace.Subspace {
	return storeSubspace.Sub(IndexSlidingWindowSpaceKey, idx.SubspaceTupleKey())
}

// readSlidingWindowEntries returns every entry key stored for a partition, in
// key order, together with the packed primary key each maps to.
func readSlidingWindowEntries(tx fdb.ReadTransaction, sw subspace.Subspace, partition tuple.Tuple) ([]tuple.Tuple, [][]byte) {
	entries := swPartitionSub(sw, partition).Sub(slidingWindowEntriesSubspaceKey)
	begin, end := entries.FDBRangeKeys()
	kvs, err := tx.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
	Expect(err).NotTo(HaveOccurred())
	keys := make([]tuple.Tuple, 0, len(kvs))
	vals := make([][]byte, 0, len(kvs))
	for _, kv := range kvs {
		k, uerr := entries.Unpack(kv.Key)
		Expect(uerr).NotTo(HaveOccurred())
		keys = append(keys, k)
		vals = append(vals, kv.Value)
	}
	return keys, vals
}

func swPartitionSub(sw subspace.Subspace, partition tuple.Tuple) subspace.Subspace {
	if len(partition) == 0 {
		return sw
	}
	args := make([]tuple.TupleElement, len(partition))
	for i, v := range partition {
		args[i] = v
	}
	return sw.Sub(args...)
}

// readSlidingWindowMeta returns the stored count and boundary pointer for a
// partition. A nil boundary means the meta key is absent.
func readSlidingWindowMeta(tx fdb.ReadTransaction, sw subspace.Subspace, partition tuple.Tuple) (int64, tuple.Tuple) {
	meta := swPartitionSub(sw, partition).Sub(slidingWindowMetaSubspaceKey)

	countBytes, err := tx.Get(meta.Pack(tuple.Tuple{slidingWindowCountKey})).Get()
	Expect(err).NotTo(HaveOccurred())
	count, err := decodeSlidingWindowLong(countBytes)
	Expect(err).NotTo(HaveOccurred())

	boundaryBytes, err := tx.Get(meta.Pack(tuple.Tuple{slidingWindowBoundaryKey})).Get()
	Expect(err).NotTo(HaveOccurred())
	if boundaryBytes == nil {
		return count, nil
	}
	boundary, err := tuple.Unpack(boundaryBytes)
	Expect(err).NotTo(HaveOccurred())
	return count, boundary
}

// searchPKs returns the primary keys the wrapped HNSW graph currently holds,
// sorted, by asking for far more neighbours than can exist. This is the
// observable that matters: "in the window" means "present in the delegate".
func searchPKs(store *FDBRecordStore, idx *Index, prefix tuple.Tuple) []int64 {
	results, err := store.SearchVectorIndexWithPrefix(idx, prefix, []float64{0, 0}, 1000, 400)
	Expect(err).NotTo(HaveOccurred())
	pks := make([]int64, 0, len(results))
	for _, r := range results {
		Expect(r.PrimaryKey).To(HaveLen(1))
		v, ok := asInt64(r.PrimaryKey[0])
		Expect(ok).To(BeTrue())
		pks = append(pks, v)
	}
	sort.Slice(pks, func(i, j int) bool { return pks[i] < pks[j] })
	return pks
}

var _ = Describe("SlidingWindowIndex", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// =====================================================================
	// THE WIRE CONTRACT. Everything else proves self-consistency; this
	// proves the bytes are where Java puts them.
	// =====================================================================
	It("writes its bookkeeping at exactly Java's keys under prefix 10", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_layout", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range []struct{ id, price int64 }{{1, 10}, {2, 20}} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(o.id), Price: proto.Int32(int32(o.price)),
					CoordX: proto.Int64(o.id), CoordY: proto.Int64(o.id),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)

			// Nothing may live directly under prefix 10 outside this index's
			// own subspace, and nothing outside prefix 10 may be the window's.
			Expect(sw.Bytes()).To(HavePrefix(string(ks.Sub(IndexSlidingWindowSpaceKey).Bytes())))

			// ENTRIES region, key 0: [windowValue..., primaryKey...] -> packed PK.
			keys, vals := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(2))
			Expect(keys[0]).To(Equal(tuple.Tuple{int64(10), int64(1)}))
			Expect(keys[1]).To(Equal(tuple.Tuple{int64(20), int64(2)}))
			Expect(vals[0]).To(Equal(tuple.Tuple{int64(1)}.Pack()))
			Expect(vals[1]).To(Equal(tuple.Tuple{int64(2)}.Pack()))

			// The exact byte key, composed independently of the maintainer.
			wantEntryKey := ks.Sub(IndexSlidingWindowSpaceKey, idx.SubspaceTupleKey()).
				Sub(slidingWindowEntriesSubspaceKey).
				Pack(tuple.Tuple{int64(10), int64(1)})
			got, err := tx.Get(wantEntryKey).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{int64(1)}.Pack()))

			// META region, key 1: COUNT at 3, BOUNDARY at 4.
			meta := ks.Sub(IndexSlidingWindowSpaceKey, idx.SubspaceTupleKey()).
				Sub(slidingWindowMetaSubspaceKey)
			countBytes, err := tx.Get(meta.Pack(tuple.Tuple{3})).Get()
			Expect(err).NotTo(HaveOccurred())
			// Java stores the count as a PACKED TUPLE, not a raw int64.
			Expect(countBytes).To(Equal(tuple.Tuple{int64(2)}.Pack()))

			boundaryBytes, err := tx.Get(meta.Pack(tuple.Tuple{4})).Get()
			Expect(err).NotTo(HaveOccurred())
			// ASC/MIN keeps the smallest; the boundary is the WORST in-window
			// entry, i.e. the largest price present.
			Expect(boundaryBytes).To(Equal(tuple.Tuple{int64(20), int64(2)}.Pack()))

			// 2 and 5 are deliberately unused in the meta region.
			for _, hole := range []int{2, 5} {
				v, gerr := tx.Get(meta.Pack(tuple.Tuple{hole})).Get()
				Expect(gerr).NotTo(HaveOccurred())
				Expect(v).To(BeNil(), "meta key %d must stay unused (Java leaves it empty)", hole)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// MIN (ASC) — keeps the smallest ordering values.
	// =====================================================================
	It("ASC/MIN keeps the N smallest and evicts the largest", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_min", 3, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert prices 50, 40, 30 — fills the window.
			for i, price := range []int32{50, 40, 30} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1, 2, 3}))

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(3)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(50), int64(1)}))

			// A BETTER entry (price 20) evicts the worst (price 50, PK 1).
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(4), Price: proto.Int32(20),
				CoordX: proto.Int64(9), CoordY: proto.Int64(9),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2, 3, 4}))

			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(3)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(40), int64(2)}))

			// EVICTION MOVES THE POINTER, NOT THE DATA: the evicted entry is
			// still in the entry list, now on the overflow side.
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(4))
			Expect(keys[3]).To(Equal(tuple.Tuple{int64(50), int64(1)}))

			// A WORSE entry (price 100) never reaches the delegate, but is
			// still tracked in the entry list.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(5), Price: proto.Int32(100),
				CoordX: proto.Int64(11), CoordY: proto.Int64(11),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2, 3, 4}))
			keys, _ = readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(5))
			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(3)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(40), int64(2)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// MAX (DESC) — the OTHER comparator. ASC passing proves nothing here:
	// every scan direction, every inequality and the boundary's meaning are
	// all mirrored.
	// =====================================================================
	It("DESC/MAX keeps the N largest and evicts the smallest", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_max", 3, gen.RowNumberWindowPredicate_DESC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{30, 40, 50} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1, 2, 3}))

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(3)))
			// MAX's worst in-window entry is the SMALLEST price.
			Expect(boundary).To(Equal(tuple.Tuple{int64(30), int64(1)}))

			// A larger price evicts the smallest.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(4), Price: proto.Int32(60),
				CoordX: proto.Int64(9), CoordY: proto.Int64(9),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2, 3, 4}))
			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(3)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(40), int64(2)}))

			// A smaller price is tracked but never indexed.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(5), Price: proto.Int32(1),
				CoordX: proto.Int64(11), CoordY: proto.Int64(11),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2, 3, 4}))
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// RE-ELECTION: deleting an in-window record promotes the best overflow
	// entry, in BOTH directions.
	// =====================================================================
	for _, dirCase := range []struct {
		name      string
		dir       gen.RowNumberWindowPredicate_Direction
		prices    []int32 // for PKs 1..4
		inWindow  []int64
		afterDel  []int64
		delPK     int64
		wantBound tuple.Tuple
	}{
		{
			name:      "ASC/MIN",
			dir:       gen.RowNumberWindowPredicate_ASC,
			prices:    []int32{10, 20, 30, 40},
			inWindow:  []int64{1, 2},
			afterDel:  []int64{2, 3},
			delPK:     1,
			wantBound: tuple.Tuple{int64(30), int64(3)},
		},
		{
			name:      "DESC/MAX",
			dir:       gen.RowNumberWindowPredicate_DESC,
			prices:    []int32{40, 30, 20, 10},
			inWindow:  []int64{1, 2},
			afterDel:  []int64{2, 3},
			delPK:     1,
			wantBound: tuple.Tuple{int64(20), int64(3)},
		},
	} {
		dirCase := dirCase
		It("re-elects the best overflow entry after an in-window delete ("+dirCase.name+")", func() {
			ks := specSubspace()
			idx := newWindowedVectorIndex("sw_reelect", 2, dirCase.dir)
			builder := baseMetaData()
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				for i, price := range dirCase.prices {
					_, err = store.SaveRecord(&gen.Order{
						OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
						CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
					})
					Expect(err).NotTo(HaveOccurred())
				}
				Expect(searchPKs(store, idx, nil)).To(Equal(dirCase.inWindow))

				// Delete the BEST in-window record. Its slot must be refilled
				// from overflow, not left short.
				deleted, err := store.DeleteRecord(tuple.Tuple{dirCase.delPK})
				Expect(err).NotTo(HaveOccurred())
				Expect(deleted).To(BeTrue())

				Expect(searchPKs(store, idx, nil)).To(Equal(dirCase.afterDel))

				tx := rtx.Transaction()
				sw := slidingWindowSubspaceFor(store.subspace, idx)
				count, boundary := readSlidingWindowMeta(tx, sw, nil)
				Expect(count).To(Equal(int64(2)))
				Expect(boundary).To(Equal(dirCase.wantBound))

				// The deleted record's entry is gone from the entry list.
				keys, _ := readSlidingWindowEntries(tx, sw, nil)
				Expect(keys).To(HaveLen(3))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("deleting an overflow record leaves the window and its pointer untouched", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_del_overflow", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{10, 20, 30} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1, 2}))

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			_, boundaryBefore := readSlidingWindowMeta(tx, sw, nil)

			// PK 3 (price 30) is overflow.
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1, 2}))
			count, boundaryAfter := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(2)), "an overflow delete must not move the count")
			Expect(boundaryAfter).To(Equal(boundaryBefore))
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("empties the partition and clears the boundary when the last entry goes", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_empty", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(10),
				CoordX: proto.Int64(1), CoordY: proto.Int64(1),
			})
			Expect(err).NotTo(HaveOccurred())

			deleted, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(0)))
			Expect(boundary).To(BeNil(), "the boundary key must be CLEARED, not left dangling")
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(BeEmpty())
			Expect(searchPKs(store, idx, nil)).To(BeEmpty())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// PARTITIONED. Tuple.from() (the unpartitioned case) is a real code path
	// covered above; this is the other one.
	// =====================================================================
	It("keeps an independent window per partition", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_part", 2, gen.RowNumberWindowPredicate_ASC, "quantity")
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Partition 7: prices 10, 20, 30 (PKs 1,2,3) — 30 overflows.
			// Partition 8: prices 15, 25    (PKs 4,5)   — both in window.
			for _, o := range []struct {
				id, qty, price int64
			}{
				{1, 7, 10}, {2, 7, 20}, {3, 7, 30}, {4, 8, 15}, {5, 8, 25},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
					CoordX:   proto.Int64(o.id), CoordY: proto.Int64(o.id),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)

			c7, b7 := readSlidingWindowMeta(tx, sw, tuple.Tuple{int64(7)})
			Expect(c7).To(Equal(int64(2)))
			Expect(b7).To(Equal(tuple.Tuple{int64(20), int64(2)}))
			k7, _ := readSlidingWindowEntries(tx, sw, tuple.Tuple{int64(7)})
			Expect(k7).To(HaveLen(3))

			c8, b8 := readSlidingWindowMeta(tx, sw, tuple.Tuple{int64(8)})
			Expect(c8).To(Equal(int64(2)))
			Expect(b8).To(Equal(tuple.Tuple{int64(25), int64(5)}))
			k8, _ := readSlidingWindowEntries(tx, sw, tuple.Tuple{int64(8)})
			Expect(k8).To(HaveLen(2))

			// The two partitions do not share a window: partition 8 keeps both
			// its records even though partition 7 already had to evict one.
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1, 2, 4, 5}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// deleteWhere — supported partitioned, REFUSED unpartitioned.
	// =====================================================================
	It("deleteRecordsWhere clears a partition group and delegates", func() {
		ks := specSubspace()
		// Java requires BOTH halves of the decorator to accept the prefix:
		// SlidingWindowIndexMaintainer.canDeleteWhere checks it against the
		// PARTITION key and then delegates to VectorIndexMaintainer.canDeleteWhere,
		// which checks it against the index's own KeyWithValue expression. So the
		// realistic — and only workable — shape is a vector index whose HNSW
		// prefix column IS the partition column, with that column leading the
		// primary key.
		idx := newPartitionedWindowedVectorIndex("sw_dw", 2, gen.RowNumberWindowPredicate_ASC, "quantity")
		builder := baseMetaData()
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{
				{1, 7, 10}, {2, 7, 20}, {3, 8, 15},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
					CoordX:   proto.Int64(o.id), CoordY: proto.Int64(o.id),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			Expect(store.DeleteRecordsWhere(tuple.Tuple{int64(7)})).To(Succeed())

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)

			// Partition 7's whole group is gone from keyspace 10.
			p7 := swPartitionSub(sw, tuple.Tuple{int64(7)})
			begin, end := p7.FDBRangeKeys()
			kvs, gerr := tx.GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(kvs).To(BeEmpty())

			// Partition 8 survives untouched.
			c8, b8 := readSlidingWindowMeta(tx, sw, tuple.Tuple{int64(8)})
			Expect(c8).To(Equal(int64(1)))
			Expect(b8).To(Equal(tuple.Tuple{int64(15), int64(8), int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deleteRecordsWhere is refused for an unpartitioned window", func() {
		ks := specSubspace()
		// Same index SHAPE as the partitioned spec above — so the store-level
		// alignment gate and the wrapped vector maintainer both accept the
		// prefix — but with NO partition fields on the window. The refusal must
		// then come from the window itself, which is the arm under test; an
		// index shape that failed the earlier gate would have proved nothing
		// about it.
		idx := NewVectorIndex("sw_dw_unpart",
			KeyWithValue(Concat(Field("quantity"), Field("coord_x"), Field("coord_y")), 1), 2)
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())
		builder := baseMetaData()
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Quantity: proto.Int32(7), Price: proto.Int32(10),
				CoordX: proto.Int64(1), CoordY: proto.Int64(1),
			})
			Expect(err).NotTo(HaveOccurred())

			derr := store.DeleteRecordsWhere(tuple.Tuple{int64(7)})
			Expect(derr).To(HaveOccurred())
			var swErr *SlidingWindowDeleteWhereError
			Expect(errors.As(derr, &swErr)).To(BeTrue(),
				"an unpartitioned window has one entry list for the whole index; "+
					"silently clearing all of it would be data loss")

			// THE REFUSAL MUST COST NOTHING. Returning nil here commits the
			// transaction — which is what a caller that logs the error and
			// carries on would do — so if the refusal came after
			// DeleteRecordsWhere had queued its record/version/count clears,
			// the record would be gone while the index still described it.
			// Java asks its capability questions in the deleter's constructor,
			// before a single range is touched, and so must this.
			rec, rerr := store.LoadRecord(tuple.Tuple{int64(7), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(),
				"a refused deleteRecordsWhere must not have already deleted the records")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// ...and the commit really happened, so this is read from FDB rather
		// than from the aborted transaction's own writes.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(serr).NotTo(HaveOccurred())
			rec, rerr := store.LoadRecord(tuple.Tuple{int64(7), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())

			sw := slidingWindowSubspaceFor(store.subspace, idx)
			keys, _ := readSlidingWindowEntries(rtx.Transaction(), sw, nil)
			Expect(keys).To(HaveLen(1), "the window must be untouched too")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deleteRecordsWhere is refused when the prefix names non-partition columns", func() {
		ks := specSubspace()
		// The trap this pins: an ARITY-only guard. The primary key and the HNSW
		// prefix lead with `quantity`, the window partitions by `price`, and
		// both are one column wide — so `DeleteRecordsWhere((7))` passes every
		// width check while meaning two different things to the two structures.
		//
		// Clearing keyspace-10 partition (7) would wipe the window for price=7
		// while the records actually being deleted are the quantity=7 ones,
		// spread across every price partition — leaving entries, counts and
		// boundaries that mis-promote on the next delete. Java asks the
		// structural question instead: matchesSatisfyingQuery(partitionKey).
		idx := NewVectorIndex("sw_dw_mismatch",
			KeyWithValue(Concat(Field("quantity"), Field("coord_x"), Field("coord_y")), 1), 2)
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField:   []string{"order_id"},
				Size:            proto.Int32(2),
				Direction:       gen.RowNumberWindowPredicate_ASC.Enum(),
				PartitionFields: []*gen.FieldPath{{Field: []string{"price"}}},
			},
		})).To(Succeed())
		builder := baseMetaData()
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{
				{1, 7, 100}, {2, 7, 200}, {3, 8, 100},
			} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
					CoordX:   proto.Int64(o.id), CoordY: proto.Int64(o.id),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			derr := store.DeleteRecordsWhere(tuple.Tuple{int64(7)})
			Expect(derr).To(HaveOccurred())
			var swErr *SlidingWindowDeleteWhereError
			Expect(errors.As(derr, &swErr)).To(BeTrue(),
				"an arity-only guard accepts this and clears the wrong partition; got %v", derr)

			// Nothing was deleted, and every window partition is intact.
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			for _, price := range []int64{100, 200} {
				keys, _ := readSlidingWindowEntries(rtx.Transaction(), sw, tuple.Tuple{price})
				Expect(keys).NotTo(BeEmpty(), "price partition %d must be untouched", price)
			}
			rec, rerr := store.LoadRecord(tuple.Tuple{int64(7), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// The preflight's own regression: it must strip the record-type key before
	// comparing, or it rejects every type-prefixed schema on the first column.
	//
	// A window partitions by FIELD PATHS, and a record-type key is not a field,
	// so a type-prefixed primary key ALWAYS carries a leading column the
	// partition key cannot have. Java strips it (indexEvaluated = evaluated[1:])
	// before asking the index anything, and so does the code that computes the
	// prefix the maintainer is handed — checking the unstripped prefix here made
	// the preflight disagree with the very prefix it was gating.
	//
	// Both arms of the strip are driven, because they refuse differently: the
	// whole-type prefix leaves NOTHING to compare, and the narrower one leaves a
	// partition column that must match.
	for _, dw := range []struct {
		name      string
		prefix    func(typeKey any) tuple.Tuple
		wantSW    int // entries left in the surviving partition
		surviving int64
	}{
		{
			name:      "whole record type",
			prefix:    func(k any) tuple.Tuple { return tuple.Tuple{k} },
			wantSW:    0,
			surviving: 0,
		},
		{
			name:      "one partition of the type",
			prefix:    func(k any) tuple.Tuple { return tuple.Tuple{k, int64(7)} },
			wantSW:    1,
			surviving: 8,
		},
	} {
		dw := dw
		It("deleteRecordsWhere works under a record-type-key prefix ("+dw.name+")", func() {
			ks := specSubspace()
			idx := newPartitionedWindowedVectorIndex("sw_dw_typekey", 2, gen.RowNumberWindowPredicate_ASC, "quantity")
			builder := baseMetaData()
			builder.GetRecordType("Order").SetPrimaryKey(
				Concat(RecordTypeKey(), Field("quantity"), Field("order_id")))
			builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
			builder.AddIndex("Order", idx)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, serr := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(serr).NotTo(HaveOccurred())

				for _, o := range []struct{ id, qty, price int64 }{
					{1, 7, 10}, {2, 7, 20}, {3, 8, 15},
				} {
					_, e := store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(o.id),
						Quantity: proto.Int32(int32(o.qty)),
						Price:    proto.Int32(int32(o.price)),
						CoordX:   proto.Int64(o.id), CoordY: proto.Int64(o.id),
					})
					Expect(e).NotTo(HaveOccurred())
				}

				typeKey := md.GetRecordType("Order").GetRecordTypeKey()
				Expect(store.DeleteRecordsWhere(dw.prefix(typeKey))).To(Succeed())

				sw := slidingWindowSubspaceFor(store.subspace, idx)
				k7, _ := readSlidingWindowEntries(rtx.Transaction(), sw, tuple.Tuple{int64(7)})
				Expect(k7).To(BeEmpty(), "quantity 7's partition must be cleared either way")
				k8, _ := readSlidingWindowEntries(rtx.Transaction(), sw, tuple.Tuple{int64(8)})
				Expect(k8).To(HaveLen(dw.wantSW))

				if dw.surviving != 0 {
					rec, rerr := store.LoadRecord(tuple.Tuple{typeKey, int64(8), int64(3)})
					Expect(rerr).NotTo(HaveOccurred())
					Expect(rec).NotTo(BeNil(), "a narrower prefix must leave the other partition alone")
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("deleteRecordsWhere clears an UNPARTITIONED window when the whole type goes", func() {
		ks := specSubspace()
		// The one prefix an unpartitioned window CAN serve. Java reaches its
		// whole-type arm by returning true without asking the maintainer
		// anything (FDBRecordStore.java:2050-2051), so the partition check never
		// runs — and it must not run here either, or Go refuses a delete Java
		// performs.
		//
		// The shape that reaches it is a type-prefixed primary key with an index
		// root that omits the type column, which is the ordinary way to write
		// one; the derived index prefix is then empty by construction.
		idx := NewVectorIndex("sw_dw_whole_type",
			KeyWithValue(Field("vector_data"), 0), 3)
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())
		builder := baseMetaData()
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for i, price := range []int32{10, 20, 30} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:    proto.Int64(int64(i + 1)),
					Price:      proto.Int32(price),
					VectorData: SerializeVector([]float64{float64(i), 0, 0}),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(3))

			typeKey := md.GetRecordType("Order").GetRecordTypeKey()
			Expect(store.DeleteRecordsWhere(tuple.Tuple{typeKey})).To(Succeed())

			keys, _ = readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(BeEmpty())
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(0)))
			Expect(boundary).To(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("deleteRecordsWhere is refused when the index root repeats the record-type key", func() {
		ks := specSubspace()
		// The shape that shows the preflight must judge the prefix the
		// MAINTAINER receives, not the caller's raw one.
		//
		// computeSingleTypeIndexDeletePrefix strips the record-type column only
		// when the index root does NOT repeat it. Here the root does, so the
		// action prefix keeps it — (typeKey, quantity) — while the window's
		// subspace is keyed by (quantity). A preflight that stripped the column
		// on its own would compare (quantity) against the partition, approve,
		// and then hand the maintainer a two-element tuple that addresses a
		// subspace nothing lives under: the records and the HNSW graph go, the
		// window's entries and counts stay, and the call returns SUCCESS.
		//
		// A window partitions by field paths and can never have a record-type
		// column, so refusing is the honest answer rather than stripping.
		//
		// The partition is TWO columns on purpose. With a one-column partition
		// the width check refuses first and the per-column comparison is never
		// reached — the spec would pass whatever that comparison did. At two
		// columns the widths agree and the only thing that can refuse is the
		// column-by-column check, which is the arm under test.
		idx := NewVectorIndex("sw_dw_rtk_root",
			KeyWithValue(Concat(RecordTypeKey(), Field("quantity"), Field("coord_x"), Field("coord_y")), 2), 2)
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"order_id"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
				PartitionFields: []*gen.FieldPath{
					{Field: []string{"quantity"}},
					{Field: []string{"price"}},
				},
			},
		})).To(Succeed())
		builder := baseMetaData()
		builder.GetRecordType("Order").SetPrimaryKey(
			Concat(RecordTypeKey(), Field("quantity"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{{1, 7, 10}, {2, 8, 20}} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
					CoordX:   proto.Int64(o.id), CoordY: proto.Int64(o.id),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			typeKey := md.GetRecordType("Order").GetRecordTypeKey()
			derr := store.DeleteRecordsWhere(tuple.Tuple{typeKey, int64(7)})
			Expect(derr).To(HaveOccurred())
			var swErr *SlidingWindowDeleteWhereError
			Expect(errors.As(derr, &swErr)).To(BeTrue(),
				"approving this clears the wrong subspace and reports success; got %v", derr)

			// Nothing went: not the records, not the window.
			rec, rerr := store.LoadRecord(tuple.Tuple{typeKey, int64(7), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			keys, _ := readSlidingWindowEntries(rtx.Transaction(), sw, tuple.Tuple{int64(7), int64(10)})
			Expect(keys).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("re-elects from overflow when the deleted boundary was the only window entry", func() {
		ks := specSubspace()
		// A DELIBERATE DIVERGENCE from Java, pinned here because it is one.
		//
		// The inward rescan after deleting the boundary only looks at the WINDOW
		// side. At size 1 the boundary is the extreme-most entry in the whole
		// partition, so the rescan finds nothing while overflow sits just past
		// it — and Java reads that as "partition emptied", clears the boundary,
		// and returns null, which makes re-election exit immediately. The window
		// ends up empty with its overflow stranded: never promoted, never
		// searchable, and the next delete of a stranded entry hits Java's own
		// "boundary is missing but entry exists, possible corruption" throw.
		//
		// Go promotes instead. Size 1 is a legal and likely configuration, so
		// this is not an exotic corner.
		idx := newWindowedVectorIndex("sw_size_one", 1, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for i, price := range []int32{10, 20, 30} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(e).NotTo(HaveOccurred())
			}
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1}))

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(10), int64(1)}))

			// Delete the sole window member. Order 2 (price 20) must take its
			// place — the window has room and an entry is waiting for it.
			deleted, derr := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(derr).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)),
				"the window still holds one record; a count of 0 is the upstream defect")
			Expect(boundary).To(Equal(tuple.Tuple{int64(20), int64(2)}))
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2}),
				"order 2 must be searchable, not stranded in overflow")

			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(2))

			// And the promotion is repeatable rather than a one-off: deleting
			// the new boundary promotes the next one.
			deleted, derr = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(derr).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(30), int64(3)}))
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{3}))

			// Emptying it for real still reports empty — the genuine
			// partition-emptied case must not be swallowed by the promotion.
			deleted, derr = store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(derr).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(0)))
			Expect(boundary).To(BeNil())
			keys, _ = readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("re-elects from overflow at size one under DESC too", func() {
		ks := specSubspace()
		// MAX is the mirror image: the inward rescan runs the other way and the
		// overflow lies below the boundary rather than above it. ASC passing
		// proves nothing about it.
		idx := newWindowedVectorIndex("sw_size_one_desc", 1, gen.RowNumberWindowPredicate_DESC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for i, price := range []int32{30, 20, 10} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(e).NotTo(HaveOccurred())
			}
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1}))

			deleted, derr := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(derr).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(20), int64(2)}))
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears keyspace 10 when every record is deleted", func() {
		ks := specSubspace()
		// DeleteAllRecords enumerates the subspaces it clears, so a new prefix
		// has to be added by hand — Java's second range clear runs to the end of
		// the store and picks one up for free. Left behind, the window's count
		// and boundary describe a graph that no longer exists: the next save
		// reads a full window, takes the eviction branch, and evicts against a
		// boundary naming a record that is gone.
		idx := newWindowedVectorIndex("sw_delete_all", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for i, price := range []int32{10, 20, 30} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(3))

			Expect(store.DeleteAllRecords()).To(Succeed())

			keys, _ = readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(BeEmpty(), "stale entries survive a delete-all")
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(0)))
			Expect(boundary).To(BeNil())

			// The store is usable afterwards: a fresh save fills the window
			// again rather than evicting against a boundary that named a
			// record delete-all removed.
			_, e := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(9), Price: proto.Int32(5),
				CoordX: proto.Int64(9), CoordY: proto.Int64(9),
			})
			Expect(e).NotTo(HaveOccurred())
			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(5), int64(9)}))
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{9}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("serves a generic BY_DISTANCE scan through the decorator", func() {
		ks := specSubspace()
		// ScanIndexByType is the path the SQL executor takes, and it reaches the
		// access method by asking whether the maintainer satisfies
		// byDistanceScanner. A decorator satisfies no capability interface of
		// its delegate, so before the unwrap this reported a windowed vector
		// index as one that "does not support BY_DISTANCE scan" — a capability
		// REMOVED by wrapping, reported as one the index never had.
		//
		// The two dedicated entry points (SearchVectorIndex / ScanVectorIndex)
		// do not cover this: they were patched with a vector-specific unwrap,
		// and this is the interface-based site they do not go through.
		idx := newWindowedVectorIndex("sw_by_distance", 10, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			_, e := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(10),
				CoordX: proto.Int64(3), CoordY: proto.Int64(4),
			})
			Expect(e).NotTo(HaveOccurred())

			entries, cerr := AsList(ctx, store.ScanIndexByType(idx, IndexScanByDistance,
				VectorDistanceScanRange([]float64{0, 0}, 5, 100), nil, ForwardScan()))
			Expect(cerr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// The window is a WRITE-side decorator: reads still work.
	// =====================================================================
	It("vector search still resolves through the decorator", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_search", 10, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(10),
				CoordX: proto.Int64(3), CoordY: proto.Int64(4),
			})
			Expect(err).NotTo(HaveOccurred())

			// Before the unwrap seam existed, this failed with
			// "index ... is not a VECTOR index" — the decorator hid the
			// concrete maintainer from the store-level entry point.
			results, serr := store.SearchVectorIndex(idx, []float64{0, 0}, 5, 100)
			Expect(serr).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].Distance).To(BeNumerically("~", 5.0, 1e-9))

			// The cursor entry point resolves through the decorator too.
			entries, cerr := AsList(ctx, store.ScanVectorIndex(idx, []float64{0, 0}, 5, 100, nil, ForwardScan()))
			Expect(cerr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// WRITE_ONLY (online index build). A sliding window is NOT idempotent —
	// it keeps a COUNT — so re-applying an insert the indexer already
	// processed would inflate the count and then evict a record that should
	// have stayed. This is the axis no other spec here probes: every one of
	// them goes through Update(), where each record arrives exactly once.
	// =====================================================================
	It("does not double-count a record the indexer already processed", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_writeonly", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.MarkIndexWriteOnly(idx.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(store.GetIndexState(idx.Name)).To(Equal(IndexStateWriteOnly))

			saved, err := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(10),
				CoordX: proto.Int64(0), CoordY: proto.Int64(0),
			})
			Expect(err).NotTo(HaveOccurred())

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			count, _ := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)))

			// Re-apply the SAME record as a fresh insert. That is the situation
			// Java's updateWhileWriteOnly exists for and states in its own
			// comment: "if newRecord WAS previously indexed, the delete removes
			// it from the window and decrements the counter, so the subsequent
			// insert does not double-count". It is driven at the maintainer
			// because SaveRecord cannot express it — the store always supplies
			// the existing record as `old`, which is the very hand-off the
			// indexer's own earlier pass does not make.
			maintainer, err := store.getIndexMaintainer(idx)
			Expect(err).NotTo(HaveOccurred())
			swm, ok := maintainer.(*slidingWindowIndexMaintainer)
			Expect(ok).To(BeTrue(), "a windowed vector index must be decorated")

			Expect(swm.UpdateWhileWriteOnly(nil, saved)).To(Succeed())

			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(1)),
				"one record is in the window; re-processing it must not make the count say two")
			Expect(boundary).To(Equal(tuple.Tuple{int64(10), int64(1)}))

			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(1))

			// An inflated count SHOWS here and nowhere else: at count==2 with a
			// window of 2 the next insert takes the window-full branch, compares
			// against the boundary, and — being worse — never enters the graph,
			// so the window silently holds one record instead of two.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Price: proto.Int32(20),
				CoordX: proto.Int64(1), CoordY: proto.Int64(1),
			})
			Expect(err).NotTo(HaveOccurred())

			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(2)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(20), int64(2)}))

			// Both records really are in the delegate. Asked through the
			// maintainer rather than through the store, because a WRITE_ONLY
			// index is not scannable — the store-level search refuses before it
			// can answer the question this spec is asking.
			vm, ok := unwrapVectorMaintainer(maintainer)
			Expect(ok).To(BeTrue())
			results, serr := vm.SearchKNN(nil, []float64{0, 0}, 10, 100)
			Expect(serr).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears its keyspace-10 bookkeeping when the index data is cleared", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_clear", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{10, 20, 30} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(3))

			// Java's clearIndexData clears indexSlidingWindowSubspace(index)
			// alongside the index and secondary subspaces. Without it a rebuild
			// starts against an emptied graph but a full window: the first
			// insert takes the window-full branch, compares against a boundary
			// naming a record the graph no longer holds, and never inserts.
			_, err = store.ClearAndMarkIndexWriteOnly(idx.Name)
			Expect(err).NotTo(HaveOccurred())

			keys, _ = readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(BeEmpty(), "stale entries would survive the rebuild")
			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(0)))
			Expect(boundary).To(BeNil())

			// And the rebuild really does refill the window.
			for i, price := range []int32{10, 20, 30} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			count, boundary = readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(2)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(20), int64(2)}))

			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			vm, ok := unwrapVectorMaintainer(maintainer)
			Expect(ok).To(BeTrue())
			results, serr := vm.SearchKNN(nil, []float64{0, 0}, 10, 100)
			Expect(serr).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("moves a record between partitions, emptying the old one and filling the new", func() {
		ks := specSubspace()
		// An update can change the PARTITION value, not just the ordering one —
		// a dimension every other spec here holds fixed. The record has to leave
		// its old partition COMPLETELY (entry, count, boundary, graph) and
		// arrive in the new one, and the two halves are done by different code:
		// handleDelete evaluates the partition from the OLD record and
		// handleInsert from the NEW one, so a maintainer that evaluated either
		// once would strand the record in one partition while claiming it in the
		// other.
		idx := newPartitionedWindowedVectorIndex("sw_move", 2, gen.RowNumberWindowPredicate_ASC, "quantity")
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			// Partition 1 holds two records in-window plus one in overflow, so
			// the move also has to trigger a re-election on the way out.
			for _, o := range []struct{ id, price int64 }{{1, 10}, {2, 20}, {3, 30}} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(o.id), Quantity: proto.Int32(1),
					Price:  proto.Int32(int32(o.price)),
					CoordX: proto.Int64(o.id), CoordY: proto.Int64(o.id),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			maintainer, merr := store.getIndexMaintainer(idx)
			Expect(merr).NotTo(HaveOccurred())
			vm, ok := unwrapVectorMaintainer(maintainer)
			Expect(ok).To(BeTrue())
			graphPKs := func(partition int64) []int64 {
				res, e := vm.SearchKNN(tuple.Tuple{partition}, []float64{0, 0}, 50, 200)
				Expect(e).NotTo(HaveOccurred())
				out := make([]int64, 0, len(res))
				for _, r := range res {
					out = append(out, r.PrimaryKey[0].(int64))
				}
				sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
				return out
			}
			Expect(graphPKs(1)).To(Equal([]int64{1, 2}))

			// Move order 1 to partition 2, keeping its price.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Quantity: proto.Int32(2),
				Price:  proto.Int32(10),
				CoordX: proto.Int64(1), CoordY: proto.Int64(1),
			})
			Expect(err).NotTo(HaveOccurred())

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)

			// OLD partition: the entry is gone and order 3 was promoted into the
			// slot the departure freed.
			k1, _ := readSlidingWindowEntries(tx, sw, tuple.Tuple{int64(1)})
			Expect(k1).To(Equal([]tuple.Tuple{
				{int64(20), int64(2)},
				{int64(30), int64(3)},
			}))
			c1, b1 := readSlidingWindowMeta(tx, sw, tuple.Tuple{int64(1)})
			Expect(c1).To(Equal(int64(2)))
			Expect(b1).To(Equal(tuple.Tuple{int64(30), int64(3)}))
			Expect(graphPKs(1)).To(Equal([]int64{2, 3}),
				"the moved record must leave the old partition's graph, and the "+
					"overflow entry must take its place")

			// NEW partition: a window of its own, holding only the arrival.
			k2, _ := readSlidingWindowEntries(tx, sw, tuple.Tuple{int64(2)})
			Expect(k2).To(Equal([]tuple.Tuple{{int64(10), int64(1)}}))
			c2, b2 := readSlidingWindowMeta(tx, sw, tuple.Tuple{int64(2)})
			Expect(c2).To(Equal(int64(1)))
			Expect(b2).To(Equal(tuple.Tuple{int64(10), int64(1)}))
			Expect(graphPKs(2)).To(Equal([]int64{1}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// =====================================================================
	// UPDATE of an in-window record's ordering value.
	// =====================================================================
	It("re-positions a record whose window value changes", func() {
		ks := specSubspace()
		idx := newWindowedVectorIndex("sw_update", 2, gen.RowNumberWindowPredicate_ASC)
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			for i, price := range []int32{10, 20, 30} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i + 1)), Price: proto.Int32(price),
					CoordX: proto.Int64(int64(i)), CoordY: proto.Int64(int64(i)),
				})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{1, 2}))

			// Move PK 1 from the best position to the worst. It leaves the
			// window; PK 3 (price 30) is re-elected in its place.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(99),
				CoordX: proto.Int64(0), CoordY: proto.Int64(0),
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(searchPKs(store, idx, nil)).To(Equal([]int64{2, 3}))

			tx := rtx.Transaction()
			sw := slidingWindowSubspaceFor(store.subspace, idx)
			keys, _ := readSlidingWindowEntries(tx, sw, nil)
			Expect(keys).To(HaveLen(3))
			// The OLD entry key must be gone — a stale one would keep the
			// record in two window positions at once.
			Expect(keys[0]).To(Equal(tuple.Tuple{int64(20), int64(2)}))
			Expect(keys[1]).To(Equal(tuple.Tuple{int64(30), int64(3)}))
			Expect(keys[2]).To(Equal(tuple.Tuple{int64(99), int64(1)}))

			count, boundary := readSlidingWindowMeta(tx, sw, nil)
			Expect(count).To(Equal(int64(2)))
			Expect(boundary).To(Equal(tuple.Tuple{int64(30), int64(3)}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("SlidingWindowIndex validation", func() {
	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// Java's validator does not END at the window checks: its last line is
	// `delegateIndexValidator.validate(metaDataValidator)`
	// (SlidingWindowIndexMaintainerFactory.java:238), so a windowed VECTOR is
	// still validated AS a vector index. Without that call a malformed option
	// reaches parseHNSWConfig, which is deliberately permissive and substitutes
	// a default — so the index builds and writes a graph whose connectivity is
	// not the one declared, indistinguishably from a correct index.
	It("runs the wrapped vector index's option validation", func() {
		idx := newWindowedVectorIndex("sw_bad_m", 2, gen.RowNumberWindowPredicate_ASC)
		idx.Options[IndexOptionHNSWM] = "not-a-number"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).To(HaveOccurred(),
			"a windowed vector index must be validated as a vector index too")
		Expect(err.Error()).To(ContainSubstring("incorrect index options"))
		Expect(err.Error()).To(ContainSubstring("hnswM"))
	})

	It("runs the wrapped vector index's metric validation", func() {
		// The metric arm is separate from the numeric ones: parseHNSWConfig's
		// default branch maps ANY unrecognised name to Euclidean, so a typo
		// silently redefines what "nearest" means for every query the index
		// serves. Java uses Metric.valueOf, which throws.
		idx := newWindowedVectorIndex("sw_bad_metric", 2, gen.RowNumberWindowPredicate_ASC)
		idx.Options[IndexOptionVectorMetric] = "COSIGN_METRIC"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("incorrect index options"))
	})

	It("accepts a windowed vector index whose options are all well-formed", func() {
		// The other direction, without which the two specs above are satisfied
		// by a validator that refuses everything.
		idx := newWindowedVectorIndex("sw_good_opts", 2, gen.RowNumberWindowPredicate_ASC)
		idx.Options[IndexOptionHNSWM] = "16"
		idx.Options[IndexOptionVectorMetric] = "COSINE_METRIC"
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a unique sliding window index", func() {
		idx := newWindowedVectorIndex("sw_unique", 2, gen.RowNumberWindowPredicate_ASC)
		idx.SetUnique()
		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sliding window index does not support unique indexes"))
	})

	It("refuses a sliding window index spanning multiple record types", func() {
		// A UNIVERSAL index covers every record type, and Java's validator
		// admits exactly one. `price` is the only field all three demo types
		// share, so the index expression itself validates everywhere and the
		// multiple-types arm is what fails — not a field-resolution error.
		idx := NewVectorIndex("sw_universal", Concat(Field("price"), Field("price")), 2)
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())
		builder := baseMetaData()
		builder.AddUniversalIndex(idx)
		_, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("sliding window index delegate has multiple types"))
	})

	It("refuses a row-number window nested under a disjunction", func() {
		// The shape has to be AND(rowWindow, OR(..., rowWindow)) rather than a
		// bare OR, and that is not incidental: the placement check only runs on
		// an index the factory DECORATED, and the factory's search recurses
		// through AND only. A window reachable solely under an OR is therefore
		// never seen — see the spec below, which pins that Java accepts it too.
		idx := NewVectorIndex("sw_or", Concat(Field("coord_x"), Field("coord_y")), 2)
		rn := func() *gen.Predicate {
			return &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			}}
		}
		trueArm := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		}}
		Expect(idx.SetPredicateProto(&gen.Predicate{AndPredicate: &gen.AndPredicate{
			Children: []*gen.Predicate{
				rn(),
				{OrPredicate: &gen.OrPredicate{Children: []*gen.Predicate{trueArm, rn()}}},
			},
		}})).To(Succeed())

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must not appear under a disjunction"))
	})

	It("does not decorate an index whose window is reachable only under an OR", func() {
		// A NEGATIVE RESULT, pinned because it is what makes the placement check
		// above narrower than it looks. Java's
		// SlidingWindowIndexMaintainerFactory.findRowNumberWindowPredicate
		// recurses through AND and NOT through OR, so OR(rowWindow, TRUE) is not
		// a sliding window index at all: the factory hands back the undecorated
		// vector factory, the validator never runs, and the metadata is accepted.
		//
		// Go matches that rather than refusing, because refusing would make a
		// Java-authored store unopenable. If findRowNumberWindowPredicateProto
		// is ever widened to recurse through OR, this spec fails — and that
		// failure is the signal that the placement check has become reachable
		// for this shape and the two must be reconciled.
		idx := NewVectorIndex("sw_or_only", Concat(Field("coord_x"), Field("coord_y")), 2)
		rn := &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"},
			Size:          proto.Int32(2),
			Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
		}}
		trueArm := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		}}
		Expect(idx.SetPredicateProto(&gen.Predicate{OrPredicate: &gen.OrPredicate{
			Children: []*gen.Predicate{rn, trueArm},
		}})).To(Succeed())

		Expect(idx.HasRowNumberWindowPredicate()).To(BeFalse())
		Expect(isSlidingWindowIndex(idx)).To(BeFalse())

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("loads a window the maintainer's narrower lookup cannot reach, and fails at first use", func() {
		// AND(AND(rowWindow)). The FACTORY's recursive search finds it and
		// decorates the index; the MAINTAINER's lookup, which only inspects the
		// root and the immediate children of a root AND, does not. Java has that
		// same asymmetry.
		//
		// WHERE it fails is the point of this spec. Java runs the validator at
		// metadata-build time but resolves the window in the MAINTAINER's
		// constructor, reached only when that index is used — so the metadata
		// BUILDS, and the failure arrives at the first save. Refusing at build
		// would make a Java-authored store unopenable, taking every unrelated
		// record in it down with the one broken index.
		idx := NewVectorIndex("sw_nested_and",
			KeyWithValue(Field("vector_data"), 0), 3)
		rn := &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"},
			Size:          proto.Int32(2),
			Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
		}}
		inner := &gen.Predicate{AndPredicate: &gen.AndPredicate{Children: []*gen.Predicate{rn}}}
		Expect(idx.SetPredicateProto(&gen.Predicate{AndPredicate: &gen.AndPredicate{
			Children: []*gen.Predicate{inner},
		}})).To(Succeed())

		Expect(idx.HasRowNumberWindowPredicate()).To(BeTrue(),
			"the factory's recursive search must still see it — that is what makes "+
				"the maintainer's narrower lookup reachable at all")

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred(), "Java builds this metadata; Go must too")
		Expect(md).NotTo(BeNil())

		// And the failure does arrive, loudly, when the index is used.
		ks := specSubspace()
		_, err = sharedDB.Run(context.Background(), func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())
			_, saveErr := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Price: proto.Int32(10),
				VectorData: SerializeVector([]float64{1, 2, 3}),
			})
			Expect(saveErr).To(HaveOccurred())
			Expect(saveErr.Error()).To(ContainSubstring("requires a RowNumberWindowPredicate"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("accepts a row-number window conjoined with an ordinary filtering arm", func() {
		// AND(rowWindow, value) is a legal shape and the maintainer must honour
		// BOTH: the per-record arm decides who is a candidate at all, the window
		// decides which candidates are kept.
		idx := NewVectorIndex("sw_and_value", Concat(Field("coord_x"), Field("coord_y")), 2)
		rn := &gen.Predicate{RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"},
			Size:          proto.Int32(2),
			Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
		}}
		trueArm := &gen.Predicate{ConstantPredicate: &gen.ConstantPredicate{
			Value: gen.ConstantPredicate_TRUE.Enum(),
		}}
		Expect(idx.SetPredicateProto(&gen.Predicate{AndPredicate: &gen.AndPredicate{
			Children: []*gen.Predicate{rn, trueArm},
		}})).To(Succeed())

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not decorate a non-vector index that carries a window predicate", func() {
		// Java's registry hands back the undecorated factory here, so the index
		// holds every record. Go must not refuse the metadata — refusing would
		// make a Java-authored store unopenable — and must not decorate either.
		idx := NewIndex("value_with_window", Field("price"))
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())

		Expect(isSlidingWindowIndex(idx)).To(BeFalse())

		builder := baseMetaData()
		builder.AddIndex("Order", idx)
		_, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		// It is still classified as a PARTIAL index, so nothing serves it as a
		// full one. That is the property that makes not-refusing safe.
		Expect(idx.HasFilteringPredicate()).To(BeTrue())
	})

	It("does not decorate a SPFresh index that carries a window predicate", func() {
		// Deliberate: keyspace 10's layout is the wire contract for Java's HNSW
		// vector index. SPFresh is a Go-only extension (RFC-094), so pairing the
		// two would write bytes under prefix 10 that no Java engine can read.
		idx := NewIndex("spfresh_with_window", KeyWithValue(Field("vector_data"), 0))
		idx.Type = IndexTypeVectorSPFresh
		Expect(idx.SetPredicateProto(&gen.Predicate{
			RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
				OrderingField: []string{"price"},
				Size:          proto.Int32(2),
				Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
			},
		})).To(Succeed())

		Expect(idx.HasRowNumberWindowPredicate()).To(BeTrue())
		Expect(isSlidingWindowIndex(idx)).To(BeFalse(),
			"SPFresh must never be decorated: prefix 10's layout is Java's, and a "+
				"Go-only pairing would write bytes Java cannot interpret")
	})
})

var _ = Describe("SlidingWindow comparators", func() {
	// A unit pin that drives EVERY arm of the comparator with explicit state,
	// rather than relying on whichever arms the FDB corpus above happens to
	// reach. MIN and MAX are mirror images, so an arm exercised only in one
	// direction is an untested arm in the other.
	It("orders entry keys in both directions", func() {
		lo := tuple.Tuple{int64(10), int64(1)}
		hi := tuple.Tuple{int64(20), int64(1)}
		same := tuple.Tuple{int64(10), int64(1)}

		Expect(slidingWindowMin.isBetter(lo, hi)).To(BeTrue())
		Expect(slidingWindowMin.isBetter(hi, lo)).To(BeFalse())
		Expect(slidingWindowMin.isBetter(lo, same)).To(BeFalse(), "isBetter is STRICT")

		Expect(slidingWindowMax.isBetter(hi, lo)).To(BeTrue())
		Expect(slidingWindowMax.isBetter(lo, hi)).To(BeFalse())
		Expect(slidingWindowMax.isBetter(hi, hi)).To(BeFalse())

		// isInWindow is inclusive of the boundary itself, in both directions.
		Expect(slidingWindowMin.isInWindow(lo, hi)).To(BeTrue())
		Expect(slidingWindowMin.isInWindow(hi, hi)).To(BeTrue())
		Expect(slidingWindowMin.isInWindow(hi, lo)).To(BeFalse())

		Expect(slidingWindowMax.isInWindow(hi, lo)).To(BeTrue())
		Expect(slidingWindowMax.isInWindow(lo, lo)).To(BeTrue())
		Expect(slidingWindowMax.isInWindow(lo, hi)).To(BeFalse())

		// isWorseOrEqual is the negation of isBetter — the equality case is the
		// one that decides whether a tying entry becomes the new boundary while
		// the window is still filling.
		Expect(slidingWindowMin.isWorseOrEqual(hi, lo)).To(BeTrue())
		Expect(slidingWindowMin.isWorseOrEqual(lo, same)).To(BeTrue())
		Expect(slidingWindowMin.isWorseOrEqual(lo, hi)).To(BeFalse())

		Expect(slidingWindowMax.isWorseOrEqual(lo, hi)).To(BeTrue())
		Expect(slidingWindowMax.isWorseOrEqual(hi, hi)).To(BeTrue())
		Expect(slidingWindowMax.isWorseOrEqual(hi, lo)).To(BeFalse())
	})

	It("orders negative and mixed-width integers by tuple encoding, not by byte length", func() {
		// The comparator compares PACKED bytes, which is only sound because the
		// FDB tuple encoding is order-preserving. Negative integers are the case
		// where a naive byte comparison of the VALUES would disagree.
		neg := tuple.Tuple{int64(-100), int64(1)}
		zero := tuple.Tuple{int64(0), int64(1)}
		big := tuple.Tuple{int64(1 << 40), int64(1)}

		Expect(slidingWindowMin.isBetter(neg, zero)).To(BeTrue())
		Expect(slidingWindowMin.isBetter(zero, big)).To(BeTrue())
		Expect(slidingWindowMin.isBetter(neg, big)).To(BeTrue())
		Expect(slidingWindowMax.isBetter(big, neg)).To(BeTrue())

		// ...and the packed order matches the physical key order FDB scans in.
		Expect(bytes.Compare(neg.Pack(), zero.Pack())).To(BeNumerically("<", 0))
		Expect(bytes.Compare(zero.Pack(), big.Pack())).To(BeNumerically("<", 0))
	})

	It("stores the count the way Java reads it", func() {
		// Java: Tuple.from(value).pack() / Tuple.fromBytes(bytes).getLong(0).
		// A raw little-endian int64 here would be silently unreadable by Java.
		for _, v := range []int64{0, 1, 42, 1 << 40, -1} {
			encoded := encodeSlidingWindowLong(v)
			Expect(encoded).To(Equal(tuple.Tuple{v}.Pack()))
			decoded, err := decodeSlidingWindowLong(encoded)
			Expect(err).NotTo(HaveOccurred())
			Expect(decoded).To(Equal(v))
		}
		// An absent key decodes as zero, matching Java's `counterBytes == null`.
		zero, err := decodeSlidingWindowLong(nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(zero).To(Equal(int64(0)))
	})

	It("keyAfter is the immediate successor", func() {
		Expect(slidingWindowKeyAfter([]byte{0x01, 0x02})).To(Equal([]byte{0x01, 0x02, 0x00}))
		Expect(slidingWindowKeyAfter(nil)).To(Equal([]byte{0x00}))
		// It must not mutate its input — the caller still holds the boundary key.
		orig := []byte{0xff}
		_ = slidingWindowKeyAfter(orig)
		Expect(orig).To(Equal([]byte{0xff}))
	})
})
