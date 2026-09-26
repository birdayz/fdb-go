package recordlayer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// recordingFilter is an IndexMaintenanceFilter answering values for every
// record and, under IndexValuesSome, admitting an entry unless its key's first
// column is drop. It records every entry it is asked about.
type recordingFilter struct {
	values IndexValues
	drop   any
	mu     sync.Mutex
	asked  []IndexEntry
}

func (f *recordingFilter) MaintainIndex(*Index, proto.Message) IndexValues { return f.values }

func (f *recordingFilter) MaintainIndexValue(_ *Index, _ proto.Message, entry *IndexEntry) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, IndexEntry{Key: entry.Key, Value: entry.Value})
	return len(entry.Key) == 0 || entry.Key[0] != f.drop
}

// Java's IndexMaintenanceFilter (IndexMaintenanceFilter.java) and its readers:
// IndexMaintenanceUtils.getFilterTypeForRecord and
// StandardIndexMaintainer.filteredIndexEntries in every maintainer, the
// sliding window's refusal of SOME, and the online indexer's store. The
// NO_NULLS filter against the JVM, maintainer by maintainer, is conformance
// "RFC-257 NullStandin" (its NO_NULLS cases).
var _ = Describe("IndexMaintenanceFilter", func() {
	ctx := context.Background()

	build := func(add func(*RecordMetaDataBuilder)) *RecordMetaData {
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		add(b)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	// run opens the spec's store with filter in a transaction.
	run := func(md *RecordMetaData, filter IndexMaintenanceFilter, body func(*FDBRecordStore, *FDBRecordContext) error) error {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).
				SetIndexMaintenanceFilter(filter).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return nil, body(store, rtx)
		})
		return err
	}
	indexKVs := func(md *RecordMetaData, name string) []fdb.KeyValue {
		var kvs []fdb.KeyValue
		Expect(run(md, nil, func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			var err error
			kvs, err = rtx.Transaction().GetRange(store.IndexSubspace(md.GetIndex(name)), fdb.RangeOptions{}).GetSliceWithError()
			return err
		})).To(Succeed())
		return kvs
	}
	unpackKeys := func(md *RecordMetaData, name string) []tuple.Tuple {
		var keys []tuple.Tuple
		Expect(run(md, nil, func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			sub := store.IndexSubspace(md.GetIndex(name))
			kvs, err := rtx.Transaction().GetRange(sub, fdb.RangeOptions{}).GetSliceWithError()
			for _, kv := range kvs {
				t, uerr := sub.Unpack(kv.Key)
				Expect(uerr).NotTo(HaveOccurred())
				keys = append(keys, t)
			}
			return err
		})).To(Succeed())
		return keys
	}
	order := func(id int64, price, quantity int32) *gen.Order {
		return &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(price), Quantity: proto.Int32(quantity)}
	}

	It("defaults to NORMAL, which a store and its builders report", func() {
		md := build(func(*RecordMetaDataBuilder) {})
		Expect(NewStoreBuilder().GetIndexMaintenanceFilter()).To(Equal(IndexMaintenanceFilterNormal))
		Expect(run(md, nil, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			Expect(store.GetIndexMaintenanceFilter()).To(Equal(IndexMaintenanceFilterNormal))
			return nil
		})).To(Succeed())
		Expect(run(md, IndexMaintenanceFilterNoNulls, func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			Expect(store.GetIndexMaintenanceFilter()).To(Equal(IndexMaintenanceFilterNoNulls))
			Expect(store.AsBuilder().GetIndexMaintenanceFilter()).To(Equal(IndexMaintenanceFilterNoNulls))
			Expect(store.CopyBuilder(rtx).GetIndexMaintenanceFilter()).To(Equal(IndexMaintenanceFilterNoNulls))
			return nil
		})).To(Succeed())
	})

	It("asks a SOME filter about each entry's evaluated key and value, without the primary key", func() {
		md := build(func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("covering", KeyWithValue(Concat(Field("price"), Field("quantity")), 1)))
		})
		filter := &recordingFilter{values: IndexValuesSome, drop: int64(7)}
		Expect(run(md, filter, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			for _, o := range []*gen.Order{order(1, 7, 1), order(2, 8, 2)} {
				if _, err := store.SaveRecord(o); err != nil {
					return err
				}
			}
			return nil
		})).To(Succeed())
		Expect(filter.asked).To(ConsistOf(
			IndexEntry{Key: tuple.Tuple{int64(7)}, Value: tuple.Tuple{int64(1)}},
			IndexEntry{Key: tuple.Tuple{int64(8)}, Value: tuple.Tuple{int64(2)}},
		))
		Expect(unpackKeys(md, "covering")).To(Equal([]tuple.Tuple{{int64(8), int64(2)}}))
	})

	It("maintains nothing for NONE, on insert, update and delete, and every entry for ALL", func() {
		md := build(func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewIndex("price", Field("price")))
			b.AddIndex("Order", NewCountIndex("count_by_qty", GroupAll(Field("quantity"))))
		})
		none := &recordingFilter{values: IndexValuesNone}
		Expect(run(md, none, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			if _, err := store.SaveRecord(order(1, 5, 1)); err != nil {
				return err
			}
			if _, err := store.SaveRecord(order(1, 6, 1)); err != nil {
				return err
			}
			_, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			return err
		})).To(Succeed())
		Expect(indexKVs(md, "price")).To(BeEmpty())
		Expect(indexKVs(md, "count_by_qty")).To(BeEmpty())
		Expect(none.asked).To(BeEmpty(), "NONE asks about no entry")

		all := &recordingFilter{values: IndexValuesAll}
		Expect(run(md, all, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			_, err := store.SaveRecord(order(2, 9, 3))
			return err
		})).To(Succeed())
		Expect(unpackKeys(md, "price")).To(Equal([]tuple.Tuple{{int64(9), int64(2)}}))
		Expect(unpackKeys(md, "count_by_qty")).To(Equal([]tuple.Tuple{{int64(3)}}))
		Expect(all.asked).To(BeEmpty(), "ALL asks about no entry")
	})

	It("filters an atomic index's whole key, the fast paths bypassed", func() {
		md := build(func(b *RecordMetaDataBuilder) {
			b.AddIndex("Order", NewCountIndex("count_by_qty", GroupAll(Field("quantity"))))
			b.AddIndex("Order", NewSumIndex("sum_by_qty", GroupBy(Field("price"), Field("quantity"))))
			b.AddIndex("Order", NewMaxEverLongIndex("max_by_qty", GroupBy(Field("price"), Field("quantity"))))
		})
		filter := &recordingFilter{values: IndexValuesSome, drop: int64(7)}
		Expect(run(md, filter, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			for _, o := range []*gen.Order{order(1, 10, 7), order(2, 20, 8), order(3, 30, 8)} {
				if _, err := store.SaveRecord(o); err != nil {
					return err
				}
			}
			// An update that moves a record out of the dropped group.
			_, err := store.SaveRecord(order(1, 40, 9))
			return err
		})).To(Succeed())
		counts := map[string]int64{}
		Expect(run(md, nil, func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			for _, name := range []string{"count_by_qty", "sum_by_qty", "max_by_qty"} {
				sub := store.IndexSubspace(md.GetIndex(name))
				kvs, err := rtx.Transaction().GetRange(sub, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return err
				}
				for _, kv := range kvs {
					t, err := sub.Unpack(kv.Key)
					Expect(err).NotTo(HaveOccurred())
					v := int64(binary.LittleEndian.Uint64(kv.Value))
					counts[fmt.Sprintf("%s/%d", name, t[0].(int64))] = v
				}
			}
			return nil
		})).To(Succeed())
		// count: group 8 has two, group 9 one (record 1's move), group 7 none.
		Expect(counts).To(HaveKeyWithValue("count_by_qty/8", int64(2)))
		Expect(counts).To(HaveKeyWithValue("count_by_qty/9", int64(1)))
		Expect(counts).NotTo(HaveKey("count_by_qty/7"))
		// sum and max: group 8 is 20+30 and max 30; group 7 never written.
		Expect(counts).To(HaveKeyWithValue("sum_by_qty/8", int64(50)))
		Expect(counts).NotTo(HaveKey("sum_by_qty/7"))
		Expect(counts).To(HaveKeyWithValue("max_by_qty/8", int64(30)))
		Expect(counts).NotTo(HaveKey("max_by_qty/7"))
	})

	It("refuses SOME in the sliding window, and maintains no window entry for NONE", func() {
		idx := newWindowedVectorIndex("sw_filter", 2, gen.RowNumberWindowPredicate_ASC)
		md := build(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", idx) })
		o := &gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), CoordX: proto.Int64(1), CoordY: proto.Int64(1)}
		err := run(md, &recordingFilter{values: IndexValuesSome}, func(store *FDBRecordStore, _ *FDBRecordContext) error {
			_, err := store.SaveRecord(o)
			return err
		})
		var rc *RecordCoreError
		Expect(errors.As(err, &rc)).To(BeTrue(), "%v", err)
		Expect(rc.Message).To(Equal("filtering type SOME is not supported"))

		Expect(run(md, &recordingFilter{values: IndexValuesNone}, func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			if _, err := store.SaveRecord(o); err != nil {
				return err
			}
			keys, _ := readSlidingWindowEntries(rtx.Transaction(), slidingWindowSubspaceFor(store.subspace, idx), nil)
			Expect(keys).To(BeEmpty())
			return nil
		})).To(Succeed())
	})

	It("is the online indexer's stores' filter", func() {
		ks := specSubspace()
		noIndex := build(func(*RecordMetaDataBuilder) {})
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(noIndex).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for _, o := range []*gen.Order{order(1, 7, 1), order(2, 8, 1), order(3, 7, 1)} {
				if _, err := store.SaveRecord(o); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		priceIndex := NewIndex("Order$price", Field("price"))
		withIndex := build(func(b *RecordMetaDataBuilder) { b.AddIndex("Order", priceIndex) })
		indexer, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(withIndex).SetIndex(priceIndex).
			SetSubspace(ks).SetIndexMaintenanceFilter(&recordingFilter{values: IndexValuesSome, drop: int64(7)}).Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(withIndex).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			entries, err := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1))
			Expect(entries[0].IndexValues()).To(Equal(tuple.Tuple{int64(8)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Java's StandardIndexMaintainer.updateWhileWriteOnly for a non-idempotent
// index (:255-328): during a BY_INDEX build the range set holds the source
// index's entry keys, so a write applies where the record's source key is
// built; Go checked the primary key against that range set.
var _ = Describe("A non-idempotent index under a BY_INDEX build", func() {
	ctx := context.Background()

	It("applies a write where the record's source index key is built", func() {
		source := NewIndex("Order$price", Field("price"))
		count := NewCountIndex("count_by_qty", GroupAll(Field("quantity")))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", source)
		b.AddIndex("Order", count)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		ks := specSubspace()
		step := func(body func(*FDBRecordStore, *FDBRecordContext) error) {
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, body(store, rtx)
			})
			Expect(err).NotTo(HaveOccurred())
		}
		countOf := func(qty int64) int64 {
			var n int64
			step(func(store *FDBRecordStore, rtx *FDBRecordContext) error {
				v, err := rtx.Transaction().Get(fdb.Key(store.IndexSubspace(count).Pack(tuple.Tuple{qty}))).Get()
				if len(v) == 8 {
					n = int64(binary.LittleEndian.Uint64(v))
				}
				return err
			})
			return n
		}
		// The count index is under a BY_INDEX build from the price index that
		// has covered the source keys below (50).
		step(func(store *FDBRecordStore, rtx *FDBRecordContext) error {
			if _, err := store.ClearAndMarkIndexWriteOnly(count.Name); err != nil {
				return err
			}
			if err := store.SaveIndexingTypeStamp(count, &gen.IndexBuildIndexingStamp{
				Method:                         gen.IndexBuildIndexingStamp_BY_INDEX.Enum(),
				SourceIndexSubspaceKey:         tuple.Tuple{source.SubspaceTupleKey()}.Pack(),
				SourceIndexLastModifiedVersion: proto.Int32(int32(source.LastModifiedVersion)),
			}); err != nil {
				return err
			}
			return insertIndexBuildRange(NewIndexingRangeSet(store.subspace, count), rtx.Transaction(), count, nil, tuple.Tuple{int64(50)}.Pack())
		})
		// Source key (10, 1000) is built, though primary key (1000) sorts after
		// (50); source key (90, 1) is not, though primary key (1) sorts before.
		step(func(store *FDBRecordStore, _ *FDBRecordContext) error {
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1000), Price: proto.Int32(10), Quantity: proto.Int32(3)}); err != nil {
				return err
			}
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(90), Quantity: proto.Int32(3)})
			return err
		})
		Expect(countOf(3)).To(Equal(int64(1)))
		// An update whose source key leaves the built range: the removal
		// applies, the insertion does not.
		step(func(store *FDBRecordStore, _ *FDBRecordContext) error {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(1000), Price: proto.Int32(95), Quantity: proto.Int32(4)})
			return err
		})
		Expect(countOf(3)).To(Equal(int64(0)))
		Expect(countOf(4)).To(Equal(int64(0)))
		// An update whose source key stays in the built range applies whole.
		step(func(store *FDBRecordStore, _ *FDBRecordContext) error {
			if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(20), Quantity: proto.Int32(5)}); err != nil {
				return err
			}
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(7), Price: proto.Int32(20), Quantity: proto.Int32(6)})
			return err
		})
		Expect(countOf(5)).To(Equal(int64(0)))
		Expect(countOf(6)).To(Equal(int64(1)))
	})

	It("refuses a stamp of a method it cannot place a write under", func() {
		count := NewCountIndex("count_by_qty", GroupAll(Field("quantity")))
		b := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		b.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		b.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		b.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		b.AddIndex("Order", count)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			if _, err := store.MarkIndexWriteOnly(count.Name); err != nil {
				return nil, err
			}
			if err := store.SaveIndexingTypeStamp(count, &gen.IndexBuildIndexingStamp{Method: gen.IndexBuildIndexingStamp_SCRUB_REPAIR.Enum()}); err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Quantity: proto.Int32(3)})
			return nil, err
		})
		var rc *RecordCoreError
		Expect(errors.As(err, &rc)).To(BeTrue(), "%v", err)
		Expect(rc.Message).To(Equal("unable to update write-only index with current type stamp"))
	})
})
