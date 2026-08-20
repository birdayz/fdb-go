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

	It("refuses BEFORE clearing when a maintainer cannot serve the prefix", func() {
		ks := specSubspace()

		// An ungrouped TEXT index refuses any non-empty prefix — once text is
		// tokenized there is no range that corresponds to one. It used to raise
		// that refusal from inside DeleteWhere, which DeleteRecordsWhere calls
		// AFTER queueing the record, version and count clears: a caller that
		// logged the error and committed lost the record while the index kept
		// describing it.
		//
		// Java asks canDeleteWhere of every maintainer in the deleter's
		// constructor, before a single range is touched.
		textIdx := NewTextIndex("customer$name_text", Field("name"))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("name"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Customer", textIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			_, e := store.SaveRecord(&gen.Customer{
				CustomerId: proto.Int64(1), Name: proto.String("hello world"),
			})
			Expect(e).NotTo(HaveOccurred())

			derr := store.DeleteRecordsWhere(tuple.Tuple{"hello world"})
			Expect(derr).To(HaveOccurred())
			Expect(derr.Error()).To(ContainSubstring("customer$name_text"))

			// THE RECORD IS STILL THERE. Returning nil commits, which is what a
			// caller that swallows the error would do.
			rec, rerr := store.LoadRecord(tuple.Tuple{"hello world"})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(),
				"a refused deleteRecordsWhere must not have already deleted the records")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Read it back from FDB after the commit, not from the same transaction.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(serr).NotTo(HaveOccurred())
			rec, rerr := store.LoadRecord(tuple.Tuple{"hello world"})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			entries, lerr := AsList(ctx, store.ScanIndexByType(
				textIdx, IndexScanByTextToken, TupleRangeAll, nil, ForwardScan()))
			Expect(lerr).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2), "hello + world")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a vector prefix that reaches past the index's key columns", func() {
		ks := specSubspace()

		// The store-level alignment check normalises a KeyWithValue to its FULL
		// inner key, so a prefix reaching into the VECTOR columns aligns
		// positionally and is accepted. getSubspaceForPrefix then addresses a
		// subspace one level deeper than any graph that exists: the clear hits
		// nothing, reports success, and the deleted records' HNSW nodes stay
		// queryable.
		//
		// Java bounds it at the split point — KeyWithValueExpression.getColumnSize()
		// — in canDeleteWhere, and again with a Verify inside deleteWhere.
		vecIdx := NewVectorIndex("vec_split_bound",
			KeyWithValue(Concat(Field("quantity"), Field("price"), Field("vector_data")), 1), 3)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(
			Concat(Field("quantity"), Field("price"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			_, e := store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Quantity: proto.Int32(7), Price: proto.Int32(10),
				VectorData: SerializeVector([]float64{1, 2, 3}),
			})
			Expect(e).NotTo(HaveOccurred())

			// One column is fine — it is the split point.
			Expect(vecIdx.RootExpression.ColumnSize()).To(Equal(1))

			// Two reaches past it, into the vector columns.
			derr := store.DeleteRecordsWhere(tuple.Tuple{int64(7), int64(10)})
			Expect(derr).To(HaveOccurred())
			Expect(derr.Error()).To(ContainSubstring("vec_split_bound"))

			rec, rerr := store.LoadRecord(tuple.Tuple{int64(7), int64(10), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())
			res, serr2 := store.SearchVectorIndexWithPrefix(vecIdx,
				tuple.Tuple{int64(7)}, []float64{0, 0, 0}, 10, 100)
			Expect(serr2).NotTo(HaveOccurred())
			Expect(res).To(HaveLen(1),
				"nothing may have been cleared, and the node must still be findable")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a prefix that reaches past an aggregate index's grouping columns", func() {
		ks := specSubspace()

		// A COUNT is physically keyed by its GROUPING columns alone — the
		// grouped column is what is being aggregated, not part of the key —
		// while the store's alignment check normalises a GroupingKeyExpression
		// to its WHOLE key. So a prefix reaching into the grouped column aligns
		// positionally, the clear addresses a subspace no entry lives in, the
		// records go, and the count keeps counting them.
		countIdx := NewCountIndex("order$count_by_qty", GroupBy(Field("price"), Field("quantity")))
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(
			Concat(Field("quantity"), Field("price"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", countIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{{1, 7, 10}, {2, 7, 20}} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			// One column is the grouping width and is fine.
			// Two reaches into the grouped column.
			derr := store.DeleteRecordsWhere(tuple.Tuple{int64(7), int64(10)})
			Expect(derr).To(HaveOccurred())
			Expect(derr.Error()).To(ContainSubstring("order$count_by_qty"))

			rec, rerr := store.LoadRecord(tuple.Tuple{int64(7), int64(10), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil(), "a refused delete must not have removed the records")

			// The grouping-width prefix still works, and it really clears.
			Expect(store.DeleteRecordsWhere(tuple.Tuple{int64(7)})).To(Succeed())
			entries, lerr := AsList(ctx, store.ScanIndex(countIdx, TupleRangeAll, nil, ForwardScan()))
			Expect(lerr).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty(), "the aggregate must be gone with its records")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses a prefix that enters a permuted index's permuted columns", func() {
		ks := specSubspace()

		// The secondary subspace stores the group key PERMUTED: with grouping
		// (quantity, price) and permutedSize 1 its keys read
		// (quantity, aggregate, price). A prefix reaching into the permuted
		// column matches the PRIMARY entries and nothing in the secondary, so
		// the primary clear succeeds, the secondary clear hits nothing, and a
		// BY_GROUP scan keeps answering with the extremum of deleted records.
		permIdx := NewPermutedMaxIndex("order$permuted_max",
			GroupBy(Field("order_id"), Concat(Field("quantity"), Field("price"))), 1)
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(
			Concat(Field("quantity"), Field("price"), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		builder.AddIndex("Order", permIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, serr := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(serr).NotTo(HaveOccurred())

			for _, o := range []struct{ id, qty, price int64 }{{1, 7, 10}, {2, 7, 20}} {
				_, e := store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Quantity: proto.Int32(int32(o.qty)),
					Price:    proto.Int32(int32(o.price)),
				})
				Expect(e).NotTo(HaveOccurred())
			}

			// groupingCount 2 minus permutedSize 1 leaves one clearable column.
			derr := store.DeleteRecordsWhere(tuple.Tuple{int64(7), int64(10)})
			Expect(derr).To(HaveOccurred())
			Expect(derr.Error()).To(ContainSubstring("order$permuted_max"))

			rec, rerr := store.LoadRecord(tuple.Tuple{int64(7), int64(10), int64(1)})
			Expect(rerr).NotTo(HaveOccurred())
			Expect(rec).NotTo(BeNil())

			// And the one-column prefix clears BOTH subspaces, which is the
			// property the bound exists to protect.
			Expect(store.DeleteRecordsWhere(tuple.Tuple{int64(7)})).To(Succeed())
			byGroup, gerr := AsList(ctx, store.ScanIndexByType(
				permIdx, IndexScanByGroup, TupleRangeAll, nil, ForwardScan()))
			Expect(gerr).NotTo(HaveOccurred())
			Expect(byGroup).To(BeEmpty(), "stale BY_GROUP entries would answer for deleted records")
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
