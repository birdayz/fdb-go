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

// These specs pin the three answers Java's getSnapshotRecordCount /
// IndexFunctionHelper.indexesForRecordTypes give and Go used to get wrong. Every
// one of them is a WRONG-ANSWER shape — a count that came back plausible and small
// instead of erroring — so each assertion states the number, not just the absence of
// an error.
var _ = Describe("RecordCountRollup", func() {
	ctx := context.Background()

	// recordTypeKeyedMetaData gives all three demo types a record-type-prefixed
	// primary key, which is what a store counting by record type needs.
	recordTypeKeyedMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Concat(RecordTypeKey(), Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(Concat(RecordTypeKey(), Field("customer_id")))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Concat(RecordTypeKey(), Field("id")))
		return builder
	}

	saveOrders := func(store *FDBRecordStore, n int64) {
		for i := int64(1); i <= n; i++ {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
			Expect(err).NotTo(HaveOccurred())
		}
	}
	saveCustomers := func(store *FDBRecordStore, n int64) {
		for i := int64(1); i <= n; i++ {
			_, err := store.SaveRecord(&gen.Customer{CustomerId: proto.Int64(i), Name: proto.String("c")})
			Expect(err).NotTo(HaveOccurred())
		}
	}
	saveTypedRecords := func(store *FDBRecordStore, n int64) {
		for i := int64(1); i <= n; i++ {
			_, err := store.SaveRecord(&gen.TypedRecord{Id: proto.Int64(i), ValInt32: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	// ── Item 1: a grouped record-count key must roll up ──────────────────────
	//
	// Java: getSnapshotRecordCount(EMPTY, EMPTY, filter) takes the
	// key.isPrefixKey(recordCountKey) branch (FDBRecordStore.java:2306-2311) —
	// EmptyKeyExpression is a prefix of everything (BaseKeyExpression.java:73-75) —
	// and sums the whole count subspace. Reading the ungrouped slot instead answers
	// 0 for a fully counted store, because a grouped store never writes that slot.
	It("GetRecordCount rolls up every group of a grouped record-count key", func() {
		ks := specSubspace()

		builder := recordTypeKeyedMetaData()
		builder.SetRecordCountKey(RecordTypeKey())
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 7)
			saveCustomers(store, 4)

			total, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(11)),
				"a grouped record-count key must be summed across groups, not read out of the ungrouped slot")

			same, err := store.GetSnapshotRecordCount(tuple.Tuple{})
			Expect(err).NotTo(HaveOccurred())
			Expect(same).To(Equal(int64(11)))

			// The per-group reads are the other direction: an empty count key must
			// not start summing when the value names one group.
			orders, err := store.GetSnapshotRecordCountForRecordType("Order")
			Expect(err).NotTo(HaveOccurred())
			Expect(orders).To(Equal(int64(7)))
			customers, err := store.GetSnapshotRecordCountForRecordType("Customer")
			Expect(err).NotTo(HaveOccurred())
			Expect(customers).To(Equal(int64(4)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("GetRecordCount still reads the single slot for an ungrouped record-count key", func() {
		ks := specSubspace()

		builder := recordTypeKeyedMetaData()
		builder.SetRecordCountKey(EmptyKey())
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 7)
			saveCustomers(store, 4)

			total, err := store.GetRecordCount()
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(11)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// ── Item 2: indexesForRecordTypes, all three branches ────────────────────

	Describe("indexesForRecordTypes", func() {
		// Empty list → UNIVERSAL indexes only
		// (IndexFunctionHelper.java:180-181). A store-wide question cannot be
		// answered by an index scoped to one record type: that index holds entries
		// for that type alone, so it reports that type's count as the store's.
		It("does not answer a store-wide aggregate from a single-type index", func() {
			ks := specSubspace()

			builder := recordTypeKeyedMetaData()
			builder.AddIndex("Order", NewCountIndex("count_order", Ungrouped(EmptyKey())))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				saveOrders(store, 7)
				saveCustomers(store, 4)

				// The Order-only index would answer 7 — a plausible, wrong total.
				_, err = store.EvaluateAggregateFunction(ctx, nil,
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue(),
					"an empty record-type list must consider universal indexes only, so an Order-scoped COUNT index cannot answer a store-wide count: %v", err)

				// Scoped to Order, the same index is exactly the right answer.
				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"},
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(7)}))

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("answers a store-wide aggregate from a universal index", func() {
			ks := specSubspace()

			builder := recordTypeKeyedMetaData()
			builder.AddIndex("Order", NewCountIndex("count_order", Ungrouped(EmptyKey())))
			builder.AddUniversalIndex(NewCountIndex("count_universal", Ungrouped(EmptyKey())))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				saveOrders(store, 7)
				saveCustomers(store, 4)

				result, err := store.EvaluateAggregateFunction(ctx, nil,
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(11)}),
					"the universal index counts every type; picking the Order-scoped one would answer 7")

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// Exactly one record type → RecordType.getIndexes()
		// (IndexFunctionHelper.java:182-183), the type's OWN single-type indexes. A
		// multi-type index also covers other types, so it cannot count this one
		// alone.
		It("does not answer a single-type aggregate from a multi-type index", func() {
			ks := specSubspace()

			builder := recordTypeKeyedMetaData()
			builder.AddMultiTypeIndex([]string{"Order", "Customer"},
				NewCountIndex("count_order_customer", Ungrouped(EmptyKey())))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				saveOrders(store, 7)
				saveCustomers(store, 4)

				// The multi-type index would answer 11 for a question about Order.
				_, err = store.EvaluateAggregateFunction(ctx, []string{"Order"},
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue(),
					"a single record type must consider its own single-type indexes only: %v", err)

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		// More than one record type → the multi-type indexes covering EXACTLY that
		// set (IndexFunctionHelper.java:184-187). A superset index over-counts; a
		// subset index under-counts; neither may be selected.
		It("answers a multi-type aggregate only from an index over exactly that set", func() {
			ks := specSubspace()

			builder := recordTypeKeyedMetaData()
			// The superset index is registered FIRST on purpose: both have the same
			// column size, so selection breaks the tie on candidate order. Without
			// the exact-set filter the superset is what gets picked, and the wrong
			// answer is a bigger number rather than an error.
			builder.AddMultiTypeIndex([]string{"Order", "Customer", "TypedRecord"},
				NewCountIndex("count_three", Ungrouped(EmptyKey())))
			builder.AddMultiTypeIndex([]string{"Order", "Customer"},
				NewCountIndex("count_two", Ungrouped(EmptyKey())))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				saveOrders(store, 7)
				saveCustomers(store, 4)
				saveTypedRecords(store, 3)

				// count_two covers exactly {Order, Customer} → 11.
				// count_three covers a superset → 14, and must not be chosen.
				result, err := store.EvaluateAggregateFunction(ctx, []string{"Order", "Customer"},
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(tuple.Tuple{int64(11)}),
					"only the index covering exactly {Order, Customer} may answer; the superset index would answer 14")

				// No index covers exactly {Order, TypedRecord}.
				_, err = store.EvaluateAggregateFunction(ctx, []string{"Order", "TypedRecord"},
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue(),
					"a multi-type index over a different set must not answer: %v", err)

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reports an unknown record type rather than finding no candidates", func() {
			ks := specSubspace()

			builder := recordTypeKeyedMetaData()
			builder.AddIndex("Order", NewCountIndex("count_order", Ungrouped(EmptyKey())))
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())

			_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())

				_, err = store.EvaluateAggregateFunction(ctx, []string{"NoSuchType"},
					NewCountAggregateFunction(GroupAll(EmptyKey())),
					TupleRangeAll, IsolationLevelSnapshot)
				Expect(errors.As(err, new(*MetaDataError))).To(BeTrue(),
					"an unknown record type is Java's getIndexableRecordType throw, not an empty candidate list: %v", err)

				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// ── Item 3: no record-count key falls through to the index path ──────────
	//
	// Java: FDBRecordStore.java:2320-2322 — when there is no record-count key, or
	// its state is not READABLE, getSnapshotRecordCount does not fail; it evaluates
	// count(key) over the universal indexes. Erroring instead blinds
	// IndexBuildState.recordsInTotal, which is the only progress denominator an
	// online index build has.
	It("counts from a universal COUNT index when there is no record-count key", func() {
		ks := specSubspace()

		priceIndex := NewIndex("Order$price", Field("price"))
		builder := recordTypeKeyedMetaData()
		builder.AddUniversalIndex(NewCountIndex("count_universal", Ungrouped(EmptyKey())))
		builder.AddIndex("Order", priceIndex)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetRecordCountKey()).To(BeNil())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 9)

			total, err := store.GetSnapshotRecordCount(tuple.Tuple{})
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(9)),
				"with no record-count key the count must fall through to the universal COUNT index, not error")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// The fall-through is what populates the build-progress denominator.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.ClearAndMarkIndexWriteOnly("Order$price")
			Expect(err).NotTo(HaveOccurred())

			state, err := LoadIndexBuildState(store, priceIndex)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.State).To(Equal(IndexStateWriteOnly))
			Expect(state.RecordsInTotal).NotTo(BeNil(),
				"RecordsInTotal must be populated from the universal COUNT index; it is nil only when no COUNT index can answer")
			Expect(*state.RecordsInTotal).To(Equal(int64(9)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// ── Item 4: a value of the wrong width is an error, not a count ──────────
	//
	// Java: getSnapshotRecordCount throws recordCoreException("key and value are
	// not the same size") when key.getColumnSize() != value.size()
	// (FDBRecordStore.java:2295-2297) — before either read, and only once the
	// counters are READABLE.
	//
	// A two-column count key writes its counters at (RECORD_COUNT_KEY, typeKey,
	// orderId). Asking for the count of just (typeKey) packs a slot nothing ever
	// writes, so without the guard the store decodes a missing value as 0 and
	// answers a confident, wrong number for a store that holds records.
	It("rejects a count value whose width disagrees with the record-count key", func() {
		ks := specSubspace()

		builder := recordTypeKeyedMetaData()
		builder.SetRecordCountKey(Concat(RecordTypeKey(), Field("order_id")))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(md.GetRecordCountKey().ColumnSize()).To(Equal(2))

		orderTypeKey := md.GetRecordType("Order").GetRecordTypeKey()

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 5)

			_, err = store.GetSnapshotRecordCount(tuple.Tuple{orderTypeKey})
			var mismatch *RecordCountKeySizeMismatchError
			Expect(errors.As(err, &mismatch)).To(BeTrue(),
				"a 1-column value against a 2-column record-count key must fail, not read an unwritten slot: %v", err)
			Expect(mismatch.KeyColumnSize).To(Equal(2))
			Expect(mismatch.ValueSize).To(Equal(1))

			// The other direction: the empty value is EmptyKeyExpression.EMPTY,
			// column size 0 against column size 0, so it must still roll up.
			total, err := store.GetSnapshotRecordCount(tuple.Tuple{})
			Expect(err).NotTo(HaveOccurred())
			Expect(total).To(Equal(int64(5)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
