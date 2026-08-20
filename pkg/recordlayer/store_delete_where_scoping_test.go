package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// DeleteRecordsWhere used to clear a SINGLE-TYPE index in its entirety whenever
// it cleared anything at all, regardless of how narrow the primary-key prefix
// was. The prefix scoped the RECORD clear correctly and then the index clear
// ignored it, so surviving records lost their index entries — silently, with
// the write succeeding, and only visible as rows missing from queries that the
// index served.
//
// Java refuses this shape outright: canDeleteWhereForIndexOnStoredTypes
// (FDBRecordStore.java:2041-2056) only clears a whole index on the
// `indexMatcher == null` arm — the delete-where component was exactly a
// RecordTypeKeyComparison — and otherwise demands that the query match a prefix
// of the index's own key expression, throwing
// "deleteRecordsWhere not supported by index X" when it does not.
//
// The specs below drive the three arms separately, because they fail
// differently: one must clear everything, one must clear a scoped range, and
// one must refuse.
var _ = Describe("DeleteRecordsWhere index scoping", func() {
	ctx := context.Background()

	It("refuses a prefix that does not align with a single-type index's key expression", func() {
		ks := specSubspace()

		// PK = (quantity, order_id); index on price alone. A prefix on quantity
		// says nothing about where the price entries live.
		idx := NewIndex("order$price", Field("price"))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{
				{1, 7, 100}, {2, 7, 200}, {3, 8, 300},
			} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			derr := store.DeleteRecordsWhere(tuple.Tuple{int64(7)})
			Expect(derr).To(HaveOccurred(),
				"clearing the whole `price` index here would delete order 3's entry "+
					"while order 3 itself survives")
			Expect(derr.Error()).To(ContainSubstring("order$price"))

			// The refusal must be total: nothing may have been cleared.
			entries, lerr := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(lerr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(3))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("scopes the clear when the prefix DOES align with the index's leading columns", func() {
		ks := specSubspace()

		// Index leads with the same column the prefix names, so the clear can
		// be scoped to exactly the deleted records.
		idx := NewIndex("order$qtyPrice", Concat(Field("quantity"), Field("price")))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(Field("quantity"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", idx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{
				{1, 7, 100}, {2, 7, 200}, {3, 8, 300},
			} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			Expect(store.DeleteRecordsWhere(tuple.Tuple{int64(7)})).To(Succeed())

			entries, lerr := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(lerr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(1), "quantity 8's entry must survive")
			Expect(entries[0].Key[0]).To(Equal(int64(8)))
			Expect(entries[0].Key[1]).To(Equal(int64(300)))

			rec, rerr := store.LoadRecord(tuple.Tuple{int64(8), int64(3)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("clears the whole index when the prefix selects the entire record type", func() {
		ks := specSubspace()

		// PK leads with the record-type key and the prefix is exactly that key:
		// every record of the type goes, so every entry of its index goes too.
		// This is Java's `indexMatcher == null` arm, and it is the ONLY case in
		// which a whole-index clear is correct.
		idx := NewIndex("order$priceOnly", Field("price"))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
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

			for _, o := range []struct{ id, price int64 }{{1, 100}, {2, 200}} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(o.id), Price: proto.Int32(int32(o.price)),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			orderType := md.GetRecordType("Order")
			Expect(orderType).NotTo(BeNil())
			Expect(store.DeleteRecordsWhere(tuple.Tuple{orderType.GetRecordTypeKey()})).To(Succeed())

			entries, lerr := AsList(ctx, store.ScanIndex(idx, TupleRangeAll, nil, ForwardScan()))
			Expect(lerr).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
