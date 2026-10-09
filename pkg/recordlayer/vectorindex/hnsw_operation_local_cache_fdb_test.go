package vectorindex

import (
	"context"

	"fdb.dev/pkg/recordlayer"

	. "github.com/onsi/ginkgo/v2"

	"fdb.dev/gen"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// HNSW node caches are operation-local (RFC-257 WS-D section 1, Java's
// per-operation caches): a node a SNAPSHOT search fetched carries no read
// conflict, so it may not serve a later insert in the same transaction. The
// insert re-reads it serializably, and a concurrent rewrite of that node makes
// the transaction conflict. With the maintainer-lifetime cache this used to
// commit, its insert having read the graph through the search's snapshot.
var _ = Describe("HNSW operation-local node cache", func() {
	It("does not let a snapshot search's nodes serve a later insert", func() {
		ctx := context.Background()
		index := recordlayer.NewVectorIndex("hnsw_oplocal", recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("quantity")), 2)
		builder := baseBuilder()
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		ks := specSubspace()
		save := func(store *recordlayer.FDBRecordStore, id int64, x, y int32) {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(x), Quantity: proto.Int32(y)})
			Expect(err).NotTo(HaveOccurred())
		}
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ks).Create()
			Expect(err).NotTo(HaveOccurred())
			for i := int64(1); i <= 4; i++ {
				save(store, i, int32(i), int32(i))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A: a snapshot search over the whole graph, then an insert.
		txA, err := sharedDB.CreateTransaction()
		Expect(err).NotTo(HaveOccurred())
		defer txA.Cancel()
		storeA, err := recordlayer.NewStoreBuilder().SetContext(recordlayer.NewFDBRecordContext(txA, nil)).SetMetaDataProvider(md).SetSubspace(ks).Open()
		Expect(err).NotTo(HaveOccurred())
		results, err := storeA.SearchVectorIndex(index, []float64{0, 0}, 4, 16)
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(4))

		// B rewrites the graph the search read, and commits first.
		_, err = sharedDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(ks).Open()
			Expect(err).NotTo(HaveOccurred())
			save(store, 5, 2, 3)
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		save(storeA, 6, 3, 2)
		Expect(txA.Commit().Get()).To(HaveOccurred(),
			"A's insert read nodes B rewrote; it must conflict, not commit over the snapshot search's cache")
	})
})
