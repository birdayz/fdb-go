package recordlayer

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// The record-layer entry point to the same MIN/NULL defect the SQL layer has:
// EvaluateAggregateFunction over a PERMUTED_MIN index. The stored extremum is
// NULL whenever the group holds a NULL, because NULL outranks every value in
// tuple order, so a group with real values beside a NULL reads back as NULL
// unless the read resolves it against the ordinary subspace.
//
// The aggregated column here is `quantity`, which is nullable and is not the
// primary key. The index groups by `price` with permutedSize=1, so the ordinary
// entries are (price, quantity, pk...) while the permuted ones are
// (quantity, price) — the layout where the value sorts FIRST, which is the
// harder one for the repair to get right: the group key has to be recovered
// from the permuted suffix before the ordinary subspace can be probed.
var _ = Describe("PermutedMinNullSemantics", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	// minQuantity evaluates MIN(quantity) over the given range.
	evaluate := func(store *FDBRecordStore, name string, r TupleRange) (tuple.Tuple, error) {
		return store.EvaluateAggregateFunction(ctx, []string{"Order"},
			&IndexAggregateFunction{
				Name:    name,
				Operand: GroupBy(Field("quantity"), Field("price")),
			},
			r, IsolationLevelSerializable)
	}

	It("MIN ignores NULLs in a mixed group, and stays NULL for an all-NULL group", func() {
		ks := specSubspace()

		builder := baseMetaData()
		builder.AddIndex("Order", NewPermutedMinIndex("Order$minQtyByPrice",
			GroupBy(Field("quantity"), Field("price")), 1))
		builder.AddIndex("Order", NewPermutedMaxIndex("Order$maxQtyByPrice",
			GroupBy(Field("quantity"), Field("price")), 1))
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// price=100 : quantities {NULL, 5, 9}  -> MIN 5,  MAX 9
			// price=200 : quantities {NULL, NULL}  -> MIN NULL, MAX NULL
			// price=300 : quantities {4}           -> MIN 4,  MAX 4
			save := func(id int64, price int32, qty *int32) {
				rec := &gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(price)}
				rec.Quantity = qty
				_, serr := store.SaveRecord(rec)
				Expect(serr).NotTo(HaveOccurred())
			}
			save(1, 100, nil)
			save(2, 100, proto.Int32(5))
			save(3, 100, proto.Int32(9))
			save(4, 200, nil)
			save(5, 200, nil)
			save(6, 300, proto.Int32(4))

			got, err := evaluate(store, FunctionNameMin, TupleRangeAllOf(tuple.Tuple{int64(100)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{int64(5)}),
				"MIN over a group holding a NULL beside real values must be the smallest REAL value")

			got, err = evaluate(store, FunctionNameMax, TupleRangeAllOf(tuple.Tuple{int64(100)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{int64(9)}),
				"MAX is unaffected by NULLs — they sort lowest and never win")

			got, err = evaluate(store, FunctionNameMin, TupleRangeAllOf(tuple.Tuple{int64(200)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{nil}),
				"a group with no non-NULL value has MIN NULL")

			got, err = evaluate(store, FunctionNameMin, TupleRangeAllOf(tuple.Tuple{int64(300)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{int64(4)}))

			// Across ALL groups: the all-NULL group must contribute nothing
			// rather than dragging the whole aggregate to NULL.
			got, err = evaluate(store, FunctionNameMin, TupleRangeAll)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{int64(4)}),
				"MIN over every group is the smallest real value anywhere, not the NULL a "+
					"NULL-bearing group stores")

			// Removing the smallest real value falls back to the next one, with
			// the NULL still present and still ignored.
			_, err = store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			got, err = evaluate(store, FunctionNameMin, TupleRangeAllOf(tuple.Tuple{int64(100)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{int64(9)}))

			// Removing the last real value leaves only the NULL: now MIN is NULL.
			_, err = store.DeleteRecord(tuple.Tuple{int64(3)})
			Expect(err).NotTo(HaveOccurred())
			got, err = evaluate(store, FunctionNameMin, TupleRangeAllOf(tuple.Tuple{int64(100)}))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(tuple.Tuple{nil}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
