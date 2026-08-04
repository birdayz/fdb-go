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

// IndexAggregateFunction.Index names an index the caller would LIKE to use, not one
// the store is obliged to use. Java's IndexFunctionHelper
// (IndexFunctionHelper.java:110-115) returns from the named-index branch only on the
// readable path:
//
//	if (function.getIndex() != null) {
//	    final Index index = store.getRecordMetaData().getIndex(function.getIndex());
//	    if (store.getRecordStoreState().isReadable(index)) {
//	        return Optional.of(store.getIndexMaintainer(index));
//	    }
//	}
//	return indexesForRecordTypes(store, recordTypeNames) ...
//
// so a non-readable name falls through to the general search rather than failing.
// Both directions of that fall-through are load-bearing and neither implies the
// other: it must find a different readable index when one exists, and it must still
// refuse to answer when none does. A fall-through that only satisfies the first is
// indistinguishable from "answer from anything".
var _ = Describe("AggregateFunctionNamedIndex", func() {
	ctx := context.Background()

	orderKeyedMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	saveOrders := func(store *FDBRecordStore, n int64) {
		for i := int64(1); i <= n; i++ {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(i * 10))})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	// (a) The named index is WRITE_ONLY and a DIFFERENT readable index answers the
	// same function. Java falls through and uses it; failing on the name instead
	// turns a rebuild of one redundant index into an outage for every caller that
	// ever named it.
	It("falls through from a non-readable named index to a readable one that answers", func() {
		ks := specSubspace()

		builder := orderKeyedMetaData()
		builder.AddIndex("Order", NewCountIndex("count_named", Ungrouped(EmptyKey())))
		builder.AddIndex("Order", NewCountIndex("count_other", Ungrouped(EmptyKey())))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 6)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			// Clearing is what a real rebuild does, so the named index holds a
			// count of 0 while WRITE_ONLY. If the fall-through ever preferred it
			// anyway the answer below would be 0, not 6.
			_, err = store.ClearAndMarkIndexWriteOnly("count_named")
			Expect(err).NotTo(HaveOccurred())
			Expect(store.IsIndexReadable("count_named")).To(BeFalse())
			Expect(store.IsIndexReadable("count_other")).To(BeTrue())

			fn := NewCountAggregateFunction(GroupAll(EmptyKey()))
			fn.Index = "count_named"

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAll, IsolationLevelSnapshot)
			Expect(err).NotTo(HaveOccurred(),
				"a WRITE_ONLY named index must fall through to the general search, not fail")
			Expect(result).To(Equal(tuple.Tuple{int64(6)}),
				"the answer must come from the readable index, so it is the real count")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// (b) The named index is WRITE_ONLY and nothing else can answer. The fall-through
	// must end in "no appropriate index" — the same answer an unnamed call would
	// get — and never in a number. A cleared WRITE_ONLY COUNT index reads as 0, so
	// the failure mode this pins is a silent, plausible zero.
	It("still refuses to answer when the fall-through finds no readable index", func() {
		ks := specSubspace()

		builder := orderKeyedMetaData()
		builder.AddIndex("Order", NewCountIndex("count_only", Ungrouped(EmptyKey())))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 6)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.ClearAndMarkIndexWriteOnly("count_only")
			Expect(err).NotTo(HaveOccurred())

			fn := NewCountAggregateFunction(GroupAll(EmptyKey()))
			fn.Index = "count_only"

			result, err := store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAll, IsolationLevelSnapshot)
			Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue(),
				"the fall-through must end in the ordinary no-appropriate-index answer: %v", err)
			Expect(result).To(BeNil(),
				"never a number — the cleared WRITE_ONLY index would report 0 for a store holding 6 records")

			// The unnamed call is the reference: naming a non-readable index must
			// land in exactly the same place as naming nothing at all.
			unnamed := NewCountAggregateFunction(GroupAll(EmptyKey()))
			_, err = store.EvaluateAggregateFunction(ctx, []string{"Order"}, unnamed,
				TupleRangeAll, IsolationLevelSnapshot)
			Expect(errors.As(err, new(*AggregateFunctionNotSupportedError))).To(BeTrue())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A name that does not exist at all is still an error, not a fall-through: Java
	// dereferences it with RecordMetaData.getIndex, which throws on an unknown name
	// (RecordMetaData.java:300-306) before readability is ever consulted. Without
	// this the two arms above would also pass on an implementation that ignored
	// fn.Index entirely.
	It("fails on a named index that does not exist, rather than falling through", func() {
		ks := specSubspace()

		builder := orderKeyedMetaData()
		builder.AddIndex("Order", NewCountIndex("count_only", Ungrouped(EmptyKey())))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			saveOrders(store, 6)

			fn := NewCountAggregateFunction(GroupAll(EmptyKey()))
			fn.Index = "no_such_index"

			_, err = store.EvaluateAggregateFunction(ctx, []string{"Order"}, fn,
				TupleRangeAll, IsolationLevelSnapshot)
			Expect(errors.As(err, new(*IndexNotFoundError))).To(BeTrue(),
				"an unknown index name is a metadata error, not a reason to answer from another index: %v", err)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
