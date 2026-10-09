package vectorindex

import (
	"context"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("DeleteRecordsWhere vector index scoping", func() {
	ctx := context.Background()

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
		// — in CanDeleteWhere, and again with a Verify inside deleteWhere.
		vecIdx := recordlayer.NewVectorIndex("vec_split_bound",
			recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price"), recordlayer.Field("vector_data")), 1), 3)
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(
			recordlayer.Concat(recordlayer.Field("quantity"), recordlayer.Field("price"), recordlayer.Field("order_id")))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, serr := recordlayer.NewStoreBuilder().
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
})
