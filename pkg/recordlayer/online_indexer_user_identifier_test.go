package recordlayer

import (
	"context"

	"fdb.dev/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// SetRecordTypes takes its names VERBATIM off an exported builder, and two
// readers of that one list disagreed about what a name means.
// indexedRecordTypes resolves each through GetRecordType -- which accepts the
// SQL identifier `MY$TABLE` for a type stored as `MY__1TABLE` --
// while shouldIndexRecordForIndex compared it raw against the stored spelling on
// each record.
//
// The failure is worse than an empty scan. The indexer reports the type as in
// scope, indexes NONE of its records, and then marks the index READABLE: a
// built, empty index that queries answer from and get wrong rows -- or no rows
// -- from. Nothing errors.
//
// The Customer rows are load-bearing. With only one type in the store, "the
// index has 3 entries" is also satisfied by an indexer that indexes
// EVERYTHING, so a regression in the other direction would pass.
var _ = Describe("OnlineIndexer SetRecordTypes with a SQL identifier", func() {
	ctx := context.Background()

	renamedMetaData := func(withIndex *Index) *RecordMetaData {
		GinkgoHelper()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		if withIndex != nil {
			builder.AddIndex("Order", withIndex)
		}
		built, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		md, err := renameRecordTypes(built, map[string]string{"Order": "MY__1TABLE"})
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetRecordType("MY__1TABLE")).NotTo(BeNil(),
			"the rename did not take; the fixture cannot express the defect")
		return md
	}

	It("indexes the records of the type it was configured with, under either spelling", func() {
		ks := specSubspace()
		priceIdx := NewIndex("price_idx", Field("price"))

		// Records first, with NO index, so BuildIndex has real work to do.
		mdNoIndex := renamedMetaData(nil)
		orderType := mdNoIndex.GetRecordType("MY__1TABLE")
		customerType := mdNoIndex.GetRecordType("Customer")
		Expect(customerType).NotTo(BeNil(),
			"Customer went missing from the fixture, so an index-everything regression\n"+
				"would satisfy the count below instead of failing it")

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 3; i++ {
				order := dynamicpb.NewMessage(orderType.Descriptor)
				order.Set(orderType.Descriptor.Fields().ByName("order_id"), protoreflect.ValueOfInt64(i))
				order.Set(orderType.Descriptor.Fields().ByName("price"), protoreflect.ValueOfInt32(int32(i*100)))
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
			}
			// PRIMARY KEYS 101,102 AND NOT 1,2. Neither type has a RecordTypeKey
			// prefix here, so Order(1) and Customer(1) are the SAME primary key and
			// the customers OVERWRITE the orders -- the store ends up holding three
			// records, not five, and the count below fails for a reason that has
			// nothing to do with the predicate under test.
			for i := int64(101); i <= 102; i++ {
				cust := dynamicpb.NewMessage(customerType.Descriptor)
				cust.Set(customerType.Descriptor.Fields().ByName("customer_id"), protoreflect.ValueOfInt64(i))
				if _, err := store.SaveRecord(cust); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		mdWithIndex := renamedMetaData(priceIdx)
		indexer, err := NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(mdWithIndex).
			SetIndex(priceIdx).
			// THE SQL SPELLING. indexedRecordTypes resolves it, so the indexer
			// believes this type is in scope; the per-record predicate has to
			// agree, or the index is built empty and marked readable.
			SetRecordTypes("MY$TABLE").
			SetSubspace(ks).
			Build()
		Expect(err).NotTo(HaveOccurred())

		// The store really holds all five: three MY__1TABLE rows and two Customer
		// rows under non-colliding keys. Asserted rather than assumed, because a
		// silently-overwritten fixture reproduces the very count this spec is
		// looking for and would read as the bug.
		stored, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			s, e := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(mdNoIndex).SetSubspace(ks).Open()
			if e != nil {
				return nil, e
			}
			return AsList(ctx, s.ScanRecords(nil, ForwardScan()))
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(stored).To(HaveLen(5),
			"the fixture did not store five records. Neither type has a RecordTypeKey\n"+
				"prefix, so overlapping primary keys silently OVERWRITE across types.")

		scanned, err := indexer.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(scanned).To(BeNumerically(">=", 5),
			"the indexer did not even visit every record, so the entry count below\n"+
				"would be about the SCAN rather than about the per-record predicate")

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(mdWithIndex).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}
			Expect(store.IsIndexReadable("price_idx")).To(BeTrue(),
				"the index did not come out readable, so the assertion below would be\n"+
					"about a half-built index rather than about which records were indexed")
			entries, err := AsList(ctx, store.ScanIndex(priceIdx, TupleRangeAll, nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			Expect(entries).To(HaveLen(3),
				"The index holds %d entries, not the 3 MY__1TABLE records. Zero means the\n"+
					"per-record predicate compared the configured name RAW against the stored\n"+
					"spelling while indexedRecordTypes resolved it -- a built, READABLE, EMPTY\n"+
					"index that queries answer from. Five means it indexed the Customer rows\n"+
					"too.", len(entries))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// AN UNRESOLVABLE PRESET RECORD TYPE IS REJECTED AT BUILD, because the
// alternative is the worst outcome this file has: a built, READABLE, EMPTY
// index.
//
// Both readers of oi.recordTypes resolve a name, and for a MISSPELLED one that
// is now consistent. Neither ERRORED on a name that resolves to nothing, and
// the failure chain from there is entirely silent: indexedRecordTypes drops it,
// the empty set makes the range computation decline, the build falls back to a
// full scan, the per-record predicate matches nothing, and the index is marked
// readable anyway. Queries then answer from an index holding no entries.
//
// Java cannot express this -- IndexingCommon.fillTargetIndexers takes a
// Collection<RecordType>, not names -- so it is a Go-only builder API that
// failed OPEN on the write path.
var _ = Describe("OnlineIndexer SetRecordTypes with an unresolvable name", func() {
	It("refuses to build rather than producing an empty readable index", func() {
		priceIdx := NewIndex("price_idx_unres", Field("price"))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", priceIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(md).
			SetIndex(priceIdx).
			SetRecordTypes("NoSuchType").
			SetSubspace(specSubspace()).
			Build()
		Expect(err).To(HaveOccurred(),
			"A preset record type naming nothing was ACCEPTED. Every step after this\n"+
				"is silent: no records match, the scan finds nothing to index, and the\n"+
				"index is marked readable regardless -- a built, empty index that queries\n"+
				"answer from. If this is being relaxed, say what now stops that chain.")
		Expect(err.Error()).To(ContainSubstring("record type"),
			"the build now fails for a different reason, so this arm no longer pins the\n"+
				"preset-record-type check it was written for")

		// The control: a resolvable name still builds. Without it, a regression
		// that refused EVERY preset record type would pass the arm above.
		_, err = NewOnlineIndexerBuilder().
			SetDatabase(sharedDB).
			SetMetaData(md).
			SetIndex(priceIdx).
			SetRecordTypes("Order").
			SetSubspace(specSubspace()).
			Build()
		Expect(err).NotTo(HaveOccurred(),
			"a resolvable preset record type stopped building; the check above is now\n"+
				"rejecting good configurations too")
	})
})
