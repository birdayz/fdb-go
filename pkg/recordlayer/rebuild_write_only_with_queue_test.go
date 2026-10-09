package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// A metadata upgrade whose rebuild policy answers WRITE_ONLY_WITH_QUEUE
// (FDBRecordStore.rebuildOrMarkIndex: clearAndMarkIndexWriteOnlyWithQueue).
// At format 15 the new index is cleared and queued; at format 14 Go refuses
// the open, where Java writes state 4 regardless (DIVERGENCES "Queued index
// states require format 15 and a queue-capable index").
var _ = Describe("Rebuild policy WRITE_ONLY_WITH_QUEUE", func() {
	ctx := context.Background()
	versions := func() (*RecordMetaData, *RecordMetaData, *Index) {
		b1 := baseBuilder()
		b1.AddIndex("Order", NewIndex("ordinary", Field("price")))
		v1, err := b1.Build()
		Expect(err).NotTo(HaveOccurred())
		b2 := baseBuilder()
		b2.AddIndex("Order", NewIndex("ordinary", Field("price")))
		vector := newValueWithQueueIndex("vector", Concat(Field("quantity"), Field("price")))
		vector.AddedVersion, vector.LastModifiedVersion = v1.Version()+1, v1.Version()+1
		b2.AddIndex("Order", vector)
		v2, err := b2.Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(v2.Version()).To(BeNumerically(">", v1.Version()))
		return v1, v2, vector
	}
	seed := func(v1 *RecordMetaData, format int32) subspace.Subspace {
		root := specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(v1).SetSubspace(root).SetFormatVersion(format).Create()
			if err != nil {
				return nil, err
			}
			for id := int64(1); id <= 3; id++ {
				if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(1), Price: proto.Int32(int32(id))}); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return root
	}
	queuePolicy := func(*Index, int64, bool) IndexState { return IndexStateWriteOnlyWithQueue }

	It("clears and queues the new index at format 15", func() {
		v1, v2, vector := versions()
		root := seed(v1, 15)
		// Residue in the new index's subspace, which the rebuild must clear.
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			rc.Transaction().Set(fdb.Key(root.Sub(IndexKey, vector.SubspaceTupleKey(), "residue").Bytes()), []byte{1})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(v2).SetSubspace(root).
				SetIndexRebuildPolicy(queuePolicy).Open()
			if err != nil {
				return nil, err
			}
			Expect(store.GetIndexState("vector")).To(Equal(IndexStateWriteOnlyWithQueue))
			Expect(store.GetIndexState("ordinary")).To(Equal(IndexStateReadable))
			r, err := fdb.PrefixRange(root.Sub(IndexKey, vector.SubspaceTupleKey()).Bytes())
			if err != nil {
				return nil, err
			}
			kvs, err := rc.Transaction().GetRange(r, fdb.RangeOptions{}).GetSliceWithError()
			Expect(kvs).To(BeEmpty(), "the rebuild cleared the index subspace")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("refuses the open at format 14", func() {
		v1, v2, _ := versions()
		root := seed(v1, 14)
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			_, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(v2).SetSubspace(root).
				SetIndexRebuildPolicy(queuePolicy).Open()
			return nil, err
		})
		var unsupported *UnsupportedFeatureForFormatVersionError
		Expect(errors.As(err, &unsupported)).To(BeTrue(), "error: %v", err)
		Expect(unsupported.RequiredVersion).To(Equal(int32(15)))
		// Nothing was written: the store still opens at the old metadata.
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(v1).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			Expect(store.GetFormatVersion()).To(Equal(int32(14)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
