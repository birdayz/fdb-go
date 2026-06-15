package recordlayer

import (
	"context"
	"errors"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("indexMaintainer internals", func() {
	Describe("removeCommonEntries", func() {
		// Helper: build an Index with no PK dedup for simpler entry construction.
		simpleIndex := func() *Index {
			return NewIndex("test_idx", Field("price"))
		}

		mkEntry := func(key tuple.Tuple, pk tuple.Tuple) indexEntry {
			return indexEntry{key: key, primaryKey: pk}
		}

		mkEntryWithValue := func(key tuple.Tuple, pk tuple.Tuple, val tuple.Tuple) indexEntry {
			return indexEntry{key: key, primaryKey: pk, value: val}
		}

		It("returns empty slices when both old and new are empty", func() {
			idx := simpleIndex()
			old, new, err := removeCommonEntries(idx, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(BeNil())
			Expect(new).To(BeNil())
		})

		It("returns empty slices when both old and new are zero-length", func() {
			idx := simpleIndex()
			old, new, err := removeCommonEntries(idx, []indexEntry{}, []indexEntry{})
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(BeNil())
			Expect(new).To(BeNil())
		})

		It("returns all old entries when new is empty", func() {
			idx := simpleIndex()
			entries := []indexEntry{mkEntry(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)})}
			old, new, err := removeCommonEntries(idx, entries, []indexEntry{})
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(HaveLen(1))
			Expect(new).To(BeNil())
		})

		It("returns all new entries when old is empty", func() {
			idx := simpleIndex()
			entries := []indexEntry{mkEntry(tuple.Tuple{int64(200)}, tuple.Tuple{int64(2)})}
			old, new, err := removeCommonEntries(idx, []indexEntry{}, entries)
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(BeNil())
			Expect(new).To(HaveLen(1))
		})

		It("removes all entries when old and new are identical", func() {
			idx := simpleIndex()
			e1 := mkEntry(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)})
			e2 := mkEntry(tuple.Tuple{int64(200)}, tuple.Tuple{int64(1)})
			old, new, err := removeCommonEntries(idx, []indexEntry{e1, e2}, []indexEntry{e1, e2})
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(BeNil())
			Expect(new).To(BeNil())
		})

		It("keeps only the differing entries on partial overlap", func() {
			idx := simpleIndex()
			common := mkEntry(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)})
			oldOnly := mkEntry(tuple.Tuple{int64(200)}, tuple.Tuple{int64(1)})
			newOnly := mkEntry(tuple.Tuple{int64(300)}, tuple.Tuple{int64(1)})

			old, new, err := removeCommonEntries(idx,
				[]indexEntry{common, oldOnly},
				[]indexEntry{common, newOnly},
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(HaveLen(1))
			Expect(new).To(HaveLen(1))
			// old should contain the 200 entry, new the 300 entry
			Expect(old[0].key).To(Equal(tuple.Tuple{int64(200)}))
			Expect(new[0].key).To(Equal(tuple.Tuple{int64(300)}))
		})

		It("treats entries with different values as non-common (KeyWithValue)", func() {
			idx := simpleIndex()
			// Same key+PK but different value portions — must not be removed.
			oldE := mkEntryWithValue(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)}, tuple.Tuple{"Rose"})
			newE := mkEntryWithValue(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)}, tuple.Tuple{"Tulip"})

			old, new, err := removeCommonEntries(idx, []indexEntry{oldE}, []indexEntry{newE})
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(HaveLen(1))
			Expect(new).To(HaveLen(1))
		})

		It("treats entries with identical values as common (KeyWithValue)", func() {
			idx := simpleIndex()
			oldE := mkEntryWithValue(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)}, tuple.Tuple{"Rose"})
			newE := mkEntryWithValue(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)}, tuple.Tuple{"Rose"})

			old, new, err := removeCommonEntries(idx, []indexEntry{oldE}, []indexEntry{newE})
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(BeNil())
			Expect(new).To(BeNil())
		})

		It("treats nil value vs non-nil value as different", func() {
			idx := simpleIndex()
			oldE := mkEntry(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)}) // value = nil
			newE := mkEntryWithValue(tuple.Tuple{int64(100)}, tuple.Tuple{int64(1)}, tuple.Tuple{"Rose"})

			old, new, err := removeCommonEntries(idx, []indexEntry{oldE}, []indexEntry{newE})
			Expect(err).NotTo(HaveOccurred())
			Expect(old).To(HaveLen(1))
			Expect(new).To(HaveLen(1))
		})
	})

	Describe("indexKeyContainsNull", func() {
		It("returns false for empty tuple", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{})).To(BeFalse())
		})

		It("returns false when no element is nil", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{int64(1), "hello", int64(2)})).To(BeFalse())
		})

		It("returns true when all elements are nil", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{nil, nil})).To(BeTrue())
		})

		It("returns true when first element is nil", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{nil, int64(1)})).To(BeTrue())
		})

		It("returns true when last element is nil", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{int64(1), nil})).To(BeTrue())
		})

		It("returns true when middle element is nil", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{int64(1), nil, int64(3)})).To(BeTrue())
		})

		It("returns false for single non-nil element", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{"value"})).To(BeFalse())
		})

		It("returns true for single nil element", func() {
			Expect(indexKeyContainsNull(tuple.Tuple{nil})).To(BeTrue())
		})
	})

	Describe("tuplesEqual", func() {
		It("returns true for both empty", func() {
			Expect(tuplesEqual(tuple.Tuple{}, tuple.Tuple{})).To(BeTrue())
		})

		It("returns true for identical single-element tuples", func() {
			Expect(tuplesEqual(tuple.Tuple{int64(42)}, tuple.Tuple{int64(42)})).To(BeTrue())
		})

		It("returns true for identical multi-element tuples", func() {
			a := tuple.Tuple{int64(1), "hello", int64(3)}
			b := tuple.Tuple{int64(1), "hello", int64(3)}
			Expect(tuplesEqual(a, b)).To(BeTrue())
		})

		It("returns false for different lengths", func() {
			Expect(tuplesEqual(tuple.Tuple{int64(1)}, tuple.Tuple{int64(1), int64(2)})).To(BeFalse())
		})

		It("returns false for same-length different values", func() {
			Expect(tuplesEqual(tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)})).To(BeFalse())
		})

		It("returns false for same-length different types", func() {
			Expect(tuplesEqual(tuple.Tuple{"1"}, tuple.Tuple{int64(1)})).To(BeFalse())
		})

		It("returns true for both nil tuples", func() {
			Expect(tuplesEqual(nil, nil)).To(BeTrue())
		})

		It("returns true comparing nil vs empty tuple", func() {
			// tuple.Tuple(nil).Pack() and tuple.Tuple{}.Pack() are both empty — verify behavior.
			// The FDB tuple layer packs both as empty bytes.
			a := tuple.Tuple(nil)
			b := tuple.Tuple{}
			Expect(tuplesEqual(a, b)).To(BeTrue())
		})
	})

	Describe("checkKeyValueSizes", func() {
		idx := NewIndex("test_idx", Field("price"))
		pk := tuple.Tuple{int64(42)}

		It("returns nil for small key and value", func() {
			key := make([]byte, 100)
			value := make([]byte, 50)
			Expect(checkKeyValueSizes(idx, pk, key, value)).NotTo(HaveOccurred())
		})

		It("returns nil at exactly the key limit", func() {
			key := make([]byte, keySizeLimit)
			value := make([]byte, 0)
			Expect(checkKeyValueSizes(idx, pk, key, value)).NotTo(HaveOccurred())
		})

		It("returns nil at exactly the value limit", func() {
			key := make([]byte, 0)
			value := make([]byte, valueSizeLimit)
			Expect(checkKeyValueSizes(idx, pk, key, value)).NotTo(HaveOccurred())
		})

		It("returns IndexKeySizeError when key exceeds limit", func() {
			key := make([]byte, keySizeLimit+1)
			value := make([]byte, 0)
			err := checkKeyValueSizes(idx, pk, key, value)
			Expect(err).To(HaveOccurred())

			var keySizeErr *IndexKeySizeError
			Expect(errors.As(err, &keySizeErr)).To(BeTrue())
			Expect(keySizeErr.IndexName).To(Equal("test_idx"))
			Expect(keySizeErr.PrimaryKey).To(Equal(pk))
			Expect(keySizeErr.KeySize).To(Equal(keySizeLimit + 1))
			Expect(keySizeErr.Limit).To(Equal(keySizeLimit))
		})

		It("returns IndexValueSizeError when value exceeds limit", func() {
			key := make([]byte, 0)
			value := make([]byte, valueSizeLimit+1)
			err := checkKeyValueSizes(idx, pk, key, value)
			Expect(err).To(HaveOccurred())

			var valueSizeErr *IndexValueSizeError
			Expect(errors.As(err, &valueSizeErr)).To(BeTrue())
			Expect(valueSizeErr.IndexName).To(Equal("test_idx"))
			Expect(valueSizeErr.PrimaryKey).To(Equal(pk))
			Expect(valueSizeErr.ValueSize).To(Equal(valueSizeLimit + 1))
			Expect(valueSizeErr.Limit).To(Equal(valueSizeLimit))
		})

		It("checks key before value (key error takes precedence)", func() {
			key := make([]byte, keySizeLimit+1)
			value := make([]byte, valueSizeLimit+1)
			err := checkKeyValueSizes(idx, pk, key, value)
			Expect(err).To(HaveOccurred())

			var keySizeErr *IndexKeySizeError
			Expect(errors.As(err, &keySizeErr)).To(BeTrue(), "expected key error to take precedence over value error")
		})

		It("returns nil for zero-length key and value", func() {
			Expect(checkKeyValueSizes(idx, pk, nil, nil)).NotTo(HaveOccurred())
		})
	})

	Describe("indexEntryKey", func() {
		It("concatenates index values and PK without dedup", func() {
			idx := NewIndex("test_idx", Field("price"))
			// No primaryKeyComponentPositions set → full PK appended
			key, err := indexEntryKey(idx, tuple.Tuple{int64(100)}, tuple.Tuple{int64(42)})
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal(tuple.Tuple{int64(100), int64(42)}))
		})

		It("concatenates multi-value index with multi-component PK", func() {
			idx := NewIndex("test_idx", Concat(Field("price"), Field("name")))
			key, err := indexEntryKey(idx, tuple.Tuple{int64(100), "Alice"}, tuple.Tuple{int64(1), int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal(tuple.Tuple{int64(100), "Alice", int64(1), int64(2)}))
		})

		It("deduplicates PK components that appear in index key", func() {
			idx := NewIndex("test_idx", Concat(Field("price"), Field("order_id")))
			idx.primaryKeyComponentPositions = []int{1} // order_id is at index position 1 → trimmed
			pk := tuple.Tuple{int64(42)}
			key, err := indexEntryKey(idx, tuple.Tuple{int64(100), int64(42)}, pk)
			Expect(err).NotTo(HaveOccurred())
			// PK fully deduplicated, nothing appended
			Expect(key).To(Equal(tuple.Tuple{int64(100), int64(42)}))
		})

		It("partial dedup keeps non-overlapping PK components", func() {
			idx := NewIndex("test_idx", Field("name"))
			// PK = (RecordType, name). RecordType not in index (-1), name at 0 → trimmed.
			idx.primaryKeyComponentPositions = []int{-1, 0}
			key, err := indexEntryKey(idx, tuple.Tuple{"Alice"}, tuple.Tuple{int64(1), "Alice"})
			Expect(err).NotTo(HaveOccurred())
			// Only RecordType (int64(1)) is appended; "Alice" is deduplicated
			Expect(key).To(Equal(tuple.Tuple{"Alice", int64(1)}))
		})

		It("works with empty index values", func() {
			idx := NewIndex("test_idx", Field("price"))
			key, err := indexEntryKey(idx, tuple.Tuple{}, tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(key).To(Equal(tuple.Tuple{int64(1)}))
		})
	})

	Describe("StandardIndexMaintainer.Update", func() {
		ctx := context.Background()

		buildMeta := func(indexes ...*Index) *RecordMetaData {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			for _, idx := range indexes {
				builder.AddIndex("Order", idx)
			}
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}

		scanRawEntries := func(tx fdb.WritableTransaction, indexSub subspace.Subspace) []fdb.KeyValue {
			begin, end := indexSub.FDBRangeKeys()
			kvs, err := tx.GetRange(
				fdb.KeyRange{Begin: begin, End: end},
				fdb.RangeOptions{},
			).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			return kvs
		}

		It("insert (nil old, non-nil new) creates entry", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)},
				}
				Expect(maintainer.Update(nil, rec)).To(Succeed())

				kvs := scanRawEntries(rtx.Transaction(), indexSub)
				Expect(kvs).To(HaveLen(1))

				entryTuple, err := indexSub.Unpack(kvs[0].Key)
				Expect(err).NotTo(HaveOccurred())
				Expect(entryTuple).To(Equal(tuple.Tuple{int64(500), int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("delete (non-nil old, nil new) clears entry", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(500)},
				}
				// Insert first
				Expect(maintainer.Update(nil, rec)).To(Succeed())
				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(HaveLen(1))

				// Delete
				Expect(maintainer.Update(rec, nil)).To(Succeed())
				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("update with changed value replaces entry", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				oldRec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				}
				newRec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(200)},
				}
				// Insert old
				Expect(maintainer.Update(nil, oldRec)).To(Succeed())

				// Update old -> new
				Expect(maintainer.Update(oldRec, newRec)).To(Succeed())

				kvs := scanRawEntries(rtx.Transaction(), indexSub)
				Expect(kvs).To(HaveLen(1))
				entryTuple, err := indexSub.Unpack(kvs[0].Key)
				Expect(err).NotTo(HaveOccurred())
				Expect(entryTuple[0]).To(Equal(int64(200)))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("update with unchanged value skips mutations (common entry optimization)", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				}
				// Insert
				Expect(maintainer.Update(nil, rec)).To(Succeed())

				// Update with identical record (should be no-op for index)
				samePriceRec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				}
				Expect(maintainer.Update(rec, samePriceRec)).To(Succeed())

				kvs := scanRawEntries(rtx.Transaction(), indexSub)
				Expect(kvs).To(HaveLen(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("update with both nil is a no-op", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				Expect(maintainer.Update(nil, nil)).To(Succeed())
				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("StandardIndexMaintainer.DeleteWhere", func() {
		ctx := context.Background()

		buildMeta := func(indexes ...*Index) *RecordMetaData {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			for _, idx := range indexes {
				builder.AddIndex("Order", idx)
			}
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}

		scanRawEntries := func(tx fdb.WritableTransaction, indexSub subspace.Subspace) []fdb.KeyValue {
			begin, end := indexSub.FDBRangeKeys()
			kvs, err := tx.GetRange(
				fdb.KeyRange{Begin: begin, End: end},
				fdb.RangeOptions{},
			).GetSliceWithError()
			Expect(err).NotTo(HaveOccurred())
			return kvs
		}

		It("empty prefix clears all index entries", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert several records to populate index
				for i := int64(1); i <= 5; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(HaveLen(5))

				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)
				Expect(maintainer.DeleteWhere(tuple.Tuple{})).To(Succeed())
				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(BeEmpty())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("non-empty prefix clears only matching entries", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Insert records with different prices
				for i := int64(1); i <= 3; i++ {
					_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 100))})
					Expect(err).NotTo(HaveOccurred())
				}

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(HaveLen(3))

				// Delete only entries with price=200
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)
				Expect(maintainer.DeleteWhere(tuple.Tuple{int64(200)})).To(Succeed())

				// Should have 2 entries left (price=100 and price=300)
				kvs := scanRawEntries(rtx.Transaction(), indexSub)
				Expect(kvs).To(HaveLen(2))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("no-op when no entries match prefix", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)
				Expect(maintainer.DeleteWhere(tuple.Tuple{int64(999)})).To(Succeed())

				Expect(scanRawEntries(rtx.Transaction(), indexSub)).To(HaveLen(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("evaluateIndex", func() {
		ctx := context.Background()

		It("returns nil entries when predicate filters out the record", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			priceIndex.SetPredicate(func(msg proto.Message) bool {
				return msg.(*gen.Order).GetPrice() >= 500
			})

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)},
				}
				entries, err := maintainer.evaluateIndex(rec)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns entries when predicate matches", func() {
			priceIndex := NewIndex("Order$price", Field("price"))
			priceIndex.SetPredicate(func(msg proto.Message) bool {
				return msg.(*gen.Order).GetPrice() >= 500
			})

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(600)},
				}
				entries, err := maintainer.evaluateIndex(rec)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				Expect(entries[0].key).To(Equal(tuple.Tuple{int64(600)}))
				Expect(entries[0].primaryKey).To(Equal(tuple.Tuple{int64(1)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns entries without predicate (all records indexed)", func() {
			priceIndex := NewIndex("Order$price", Field("price"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", priceIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, priceIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(priceIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record:     &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(50)},
				}
				entries, err := maintainer.evaluateIndex(rec)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("KeyWithValueExpression splits key and value portions", func() {
			coveringIndex := NewIndex("covering", KeyWithValue(Concat(Field("price"), Nest("flower", Field("type"))), 1))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", coveringIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, coveringIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(coveringIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(42)},
					Record: &gen.Order{
						OrderId: proto.Int64(42),
						Price:   proto.Int32(999),
						Flower:  &gen.Flower{Type: proto.String("Rose")},
					},
				}
				entries, err := maintainer.evaluateIndex(rec)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(1))
				// Key portion: [price=999]
				Expect(entries[0].key).To(Equal(tuple.Tuple{int64(999)}))
				// Value portion: [flower.type="Rose"]
				Expect(entries[0].value).To(Equal(tuple.Tuple{"Rose"}))
				Expect(entries[0].primaryKey).To(Equal(tuple.Tuple{int64(42)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("FanOut produces multiple entries", func() {
			tagIndex := NewIndex("Order$tags", FanOut("tags"))

			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			builder.AddIndex("Order", tagIndex)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			ks := specSubspace()
			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				indexSub := store.subspace.Sub(IndexKey, tagIndex.SubspaceTupleKey())
				maintainer := newStandardIndexMaintainer(tagIndex, indexSub, rtx.Transaction(), store)

				rec := &FDBStoredRecord[proto.Message]{
					PrimaryKey: tuple.Tuple{int64(1)},
					Record: &gen.Order{
						OrderId: proto.Int64(1),
						Price:   proto.Int32(100),
						Tags:    []string{"premium", "gift", "vip"},
					},
				}
				entries, err := maintainer.evaluateIndex(rec)
				Expect(err).NotTo(HaveOccurred())
				Expect(entries).To(HaveLen(3))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("uniqueness checking edge cases", func() {
		ctx := context.Background()

		buildMeta := func(indexes ...*Index) *RecordMetaData {
			builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
			for _, idx := range indexes {
				builder.AddIndex("Order", idx)
			}
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			return md
		}

		It("null key bypasses uniqueness check allowing duplicates", func() {
			// Unique index on a nested field. When the nested field is unset,
			// the key is (nil), so uniqueness is not enforced.
			flowerIndex := NewIndex("Order$flower", Nest("flower", Field("type"))).SetUnique()
			md := buildMeta(flowerIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				// Two records without flower set — both should succeed
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(200)})
				Expect(err).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("same-PK re-save does not trigger uniqueness violation", func() {
			priceIndex := NewIndex("Order$price", Field("price")).SetUnique()
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				// Re-save same record with same price via store (reloads existing record)
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("different-PK with same value returns uniqueness violation", func() {
			priceIndex := NewIndex("Order$price", Field("price")).SetUnique()
			md := buildMeta(priceIndex)

			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
				Expect(err).NotTo(HaveOccurred())

				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(100)})
				Expect(err).To(HaveOccurred())

				var violation *RecordIndexUniquenessViolationError
				Expect(errors.As(err, &violation)).To(BeTrue())
				Expect(violation.IndexName).To(Equal("Order$price"))
				Expect(violation.PrimaryKey).To(Equal(tuple.Tuple{int64(2)}))
				Expect(violation.ExistingKey).To(Equal(tuple.Tuple{int64(1)}))
				Expect(violation.IndexKey).To(Equal(tuple.Tuple{int64(100)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("uniqueness violation error message is descriptive", func() {
			e := &RecordIndexUniquenessViolationError{
				IndexName:   "myIndex",
				IndexKey:    tuple.Tuple{int64(42)},
				PrimaryKey:  tuple.Tuple{int64(2)},
				ExistingKey: tuple.Tuple{int64(1)},
			}
			msg := e.Error()
			Expect(msg).To(ContainSubstring("myIndex"))
			Expect(msg).To(ContainSubstring("42"))
		})
	})

	Describe("error type structure", func() {
		It("IndexKeySizeError has correct fields and message", func() {
			e := &IndexKeySizeError{
				IndexName:  "bigIndex",
				PrimaryKey: tuple.Tuple{int64(99)},
				KeySize:    15000,
				Limit:      10000,
			}
			msg := e.Error()
			Expect(msg).To(ContainSubstring("bigIndex"))
			Expect(msg).To(ContainSubstring("15000"))
			Expect(msg).To(ContainSubstring("10000"))
		})

		It("IndexValueSizeError has correct fields and message", func() {
			e := &IndexValueSizeError{
				IndexName:  "bigIndex",
				PrimaryKey: tuple.Tuple{int64(99)},
				ValueSize:  200000,
				Limit:      100000,
			}
			msg := e.Error()
			Expect(msg).To(ContainSubstring("bigIndex"))
			Expect(msg).To(ContainSubstring("200000"))
			Expect(msg).To(ContainSubstring("100000"))
		})
	})

	Describe("deleteWhereRange", func() {
		ctx := context.Background()

		It("clears exact prefix key via PrefixRange (not FDBRangeKeys)", func() {
			// This tests the critical difference: PrefixRange includes the exact
			// prefix key, while FDBRangeKeys excludes it. This matters for
			// ungrouped aggregate indexes that write to the exact prefix.
			ks := specSubspace()
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				tx := rtx.Transaction()
				sub := ks.Sub("idx")

				// Write a key at exact prefix (like ungrouped COUNT writes to indexSubspace.Pack(tuple.Tuple{}))
				exactKey := sub.Pack(tuple.Tuple{})
				tx.Set(fdb.Key(exactKey), []byte("value"))
				// Write a key with a sub-element
				subKey := sub.Pack(tuple.Tuple{int64(1)})
				tx.Set(fdb.Key(subKey), []byte("other"))

				// deleteWhereRange with empty prefix should clear BOTH
				Expect(deleteWhereRange(tx, sub, tuple.Tuple{})).To(Succeed())

				// Verify exact prefix key is gone
				val, err := tx.Get(fdb.Key(exactKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val).To(BeNil())

				// Verify sub-element key is also gone
				val2, err := tx.Get(fdb.Key(subKey)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(val2).To(BeNil())

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
