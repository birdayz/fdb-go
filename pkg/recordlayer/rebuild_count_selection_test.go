package recordlayer

// The record count an IndexRebuildPolicy sees decides whether an evolution-added
// index is built INLINE (and ends READABLE) or left for a background build. Java
// picks that count from an ordered chain, and each step of the chain changes the
// ANSWER, not just the cost:
//
//   - an eligible readable COUNT index gives a real number, and a real number
//     under MAX_RECORDS_FOR_REBUILD is an inline build;
//   - when every index being built is on one record-type-prefixed type, the
//     emptiness probe covers only that type's range, so an index over a type that
//     holds no records is built inline even on a store full of other types;
//   - only when neither applies does the one-record probe run, and it can only say
//     "empty" or "unbounded".
//
// Skip a step and the answer degrades silently to DISABLED — correct results, but
// Java's inline path gone and a background build owed on every such store.

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("RebuildRecordCountSelection", func() {
	ctx := context.Background()

	// flatMetaData: primary keys with NO record-type prefix, so
	// singleRecordTypeWithPrefixKey never applies and the COUNT-index consultation
	// is the only thing that can produce a count.
	flatMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// prefixedMetaData: every primary key starts with the record type key, so each
	// type's records occupy a contiguous sub-range and type-scoping applies.
	prefixedMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		return builder
	}

	// (a) A readable COUNT index is consulted before the one-record probe.
	//
	// 5 records, no record-count key, and a universal COUNT index that has been
	// readable since the store was created. Java's getRecordCountForRebuildIndexes
	// reaches that index through getSnapshotRecordCount's
	// evaluateAggregateFunction fallback (FDBRecordStore.java:2320-2322) and gets 5,
	// which is <= MAX_RECORDS_FOR_REBUILD, so the evolution-added index is rebuilt
	// inline and marked READABLE.
	//
	// Jump straight to the probe instead and the store reads as "non-empty" —
	// Long.MAX_VALUE — and the index goes DISABLED. That is the mutation this pins.
	It("consults a readable COUNT index before falling back to the one-record probe", func() {
		ks := specSubspace()

		countIdx := NewCountIndex("globalCount", GroupAll(EmptyKey()))
		builder1 := flatMetaData()
		builder1.AddUniversalIndex(countIdx)
		md1, err := builder1.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for i := int64(1); i <= 5; i++ {
				if _, sErr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}); sErr != nil {
					return nil, sErr
				}
			}
			// The count index must really be readable and really hold the count,
			// otherwise the assertion below could pass for the wrong reason.
			Expect(store.IsIndexReadable("globalCount")).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		priceIndex := NewIndex("Order$price", Field("price"))
		builder2 := flatMetaData()
		builder2.AddUniversalIndex(NewCountIndex("globalCount", GroupAll(EmptyKey())))
		builder2.AddIndex("Order", priceIndex)
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable),
				"a store of 5 records with a readable COUNT index reporting 5 is far below "+
					"MAX_RECORDS_FOR_REBUILD (200), so Java rebuilds the evolution-added index "+
					"inline and marks it READABLE. DISABLED here means the eligible readable "+
					"COUNT index was never consulted and the one-record probe — which cannot "+
					"tell 5 records from 10^9 — reported Long.MAX_VALUE instead "+
					"(FDBRecordStore.java:4850-4861).")
			// Inline build means BUILT, not merely marked: the index must answer.
			entries, lErr := AsList(ctx, store.ScanIndex(priceIndex, TupleRangeAll, nil, ForwardScan()))
			if lErr != nil {
				return nil, lErr
			}
			Expect(entries).To(HaveLen(5))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// (a-negative) The consultation must NOT use an index that is itself being built.
	//
	// Same shape, except the COUNT index is added by the SAME evolution as the value
	// index. It holds no entries and has no state on disk, so consulting it would
	// answer 0 for a store of 201 records and rebuild everything inline. Java
	// excludes the indexes being built with an IndexQueryabilityFilter
	// (FDBRecordStore.java:4839-4841).
	It("does not count with an index that is itself being built", func() {
		ks := specSubspace()

		md1, err := flatMetaData().Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for i := int64(1); i <= 201; i++ {
				if _, sErr = store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i))}); sErr != nil {
					return nil, sErr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		builder2 := flatMetaData()
		builder2.AddUniversalIndex(NewCountIndex("globalCount", GroupAll(EmptyKey())))
		builder2.AddIndex("Order", NewIndex("Order$price", Field("price")))
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateDisabled),
				"the only COUNT index in the meta-data is itself evolution-added and holds no "+
					"entries yet. Reading 0 from it would rebuild a 201-record store inline "+
					"inside the store-open transaction.")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// (b) The probe is scoped to the one record type all the new indexes are on.
	//
	// 201 Customers, zero Orders, and an evolution that adds an index on Order.
	// singleRecordTypeWithPrefixKey resolves to Order (FDBRecordStore.java:4909-4929),
	// so the probe runs over Order's record-type-key range only
	// (FDBRecordStore.java:4872), finds it empty, reports 0 and the index is built
	// over a zero-length range and marked READABLE.
	//
	// Probe the whole store instead and a Customer record answers for Orders:
	// Long.MAX_VALUE, DISABLED, and a background build owed for an index that could
	// never have had an entry.
	It("scopes the probe to the single record type the new indexes are on", func() {
		ks := specSubspace()

		md1, err := prefixedMetaData().Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for i := int64(1); i <= 201; i++ {
				if _, sErr = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i)}); sErr != nil {
					return nil, sErr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		builder2 := prefixedMetaData()
		builder2.AddIndex("Order", NewIndex("Order$price", Field("price")))
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable),
				"every index being built is on Order, whose primary key is record-type "+
					"prefixed, and the store holds no Orders at all. Java probes only that "+
					"type's key range (FDBRecordStore.java:4872) and gets 0. DISABLED here "+
					"means the probe still ran over the whole store and let a Customer record "+
					"decide the fate of an index on Order.")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// (b-control) Type-scoping must NARROW the probe, never defeat it.
	//
	// Same store, but the new index is on Customer — the type that HOLDS the 201
	// records. The scoped probe must find them and the index must stay DISABLED.
	//
	// This control cannot red under "drop the scoping": the unscoped probe covers a
	// superset of the scoped range, so removing the scoping can only ever make the
	// probe MORE likely to say "non-empty", never less, and DISABLED is already the
	// expectation. What it does catch is a scoping that points at the wrong range —
	// scope to Order (empty) here instead of Customer and this test reports READABLE.
	It("keeps an index DISABLED when the scoped type is the populated one", func() {
		ks := specSubspace()

		md1, err := prefixedMetaData().Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			for i := int64(1); i <= 201; i++ {
				if _, sErr = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i)}); sErr != nil {
					return nil, sErr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		builder2 := prefixedMetaData()
		builder2.AddIndex("Customer", NewIndex("Customer$name", Field("name")))
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			Expect(store.GetIndexState("Customer$name")).To(Equal(IndexStateDisabled),
				"the 201 records ARE Customers and the new index is on Customer, so the "+
					"scoped probe must find them. READABLE here means the scoping is looking "+
					"at the wrong key range — it narrowed the probe past the very records the "+
					"index has to cover.")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// (b-rebuild) The inline rebuild scan is scoped the same way.
	//
	// Java's inline rebuild starts from IndexingCommon.computeRecordsRange() and
	// "can skip indexing records that are outside this range"
	// (IndexingMultiTargetByRecords.java:187-193). The observable consequence is
	// that records outside the range are not READ at all, so a rebuild of an index
	// on Order cannot be broken by anything sitting in Customer's range.
	//
	// The unreadable record is the probe: with the scan scoped, the rebuild never
	// touches it and the index ends READABLE; unscoped, the rebuild deserializes it
	// and the store open fails.
	It("scopes the inline rebuild scan to the indexed record types", func() {
		ks := specSubspace()

		md1, err := prefixedMetaData().Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md1).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			if _, sErr = store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(1)}); sErr != nil {
				return nil, sErr
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Corrupt the Customer record's stored bytes in place. Anything that reads it
		// as a record now fails; anything that stays inside Order's range does not
		// see it.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			recSub := ks.Sub(RecordKey)
			begin, end := recSub.FDBRangeKeys()
			kvs, gErr := rtx.Transaction().
				GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).
				GetSliceWithError()
			if gErr != nil {
				return nil, gErr
			}
			Expect(kvs).NotTo(BeEmpty(), "the Customer record must exist to be corrupted")
			for _, kv := range kvs {
				rtx.Transaction().Set(kv.Key, []byte{0xff, 0xff, 0xff, 0xff})
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		builder2 := prefixedMetaData()
		builder2.AddIndex("Order", NewIndex("Order$price", Field("price")))
		md2, err := builder2.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, sErr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md2).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			Expect(store.GetIndexState("Order$price")).To(Equal(IndexStateReadable))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred(),
			"the inline rebuild of an index on Order must scan only Order's record-type-key "+
				"range. Reaching an unreadable record in Customer's range means the rebuild is "+
				"still scanning the whole store, which Java's inline rebuild does not do "+
				"(IndexingMultiTargetByRecords.java:187-193).")
	})
})
