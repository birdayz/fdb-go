package recordlayer

import (
	"context"

	"fdb.dev/gen/demopb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Online indexer transaction limits", func() {
	ctx := context.Background()
	// Java's IndexingBase.hadTransactionReachedLimits: once a range's
	// transaction passes the write limit (or the transaction time limit), the
	// range commits before its next record, which becomes the range's end.
	It("commits a range early once the write limit is passed", func() {
		root := specSubspace()
		builder := baseBuilder()
		index := NewIndex("by_price", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(10 * i))}); err != nil {
					return nil, err
				}
			}
			_, err = store.ClearAndMarkIndexWriteOnly(index.Name)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())

		build := func(writeLimit int64) *OnlineIndexer {
			oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetIndex(index).
				SetSubspace(root).SetLimit(100).SetMaxWriteLimitBytes(writeLimit).Build()
			Expect(err).NotTo(HaveOccurred())
			return oi
		}
		oi := build(1)
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			if err != nil {
				return nil, err
			}
			return nil, store.SaveIndexingTypeStamp(index, oi.buildIndexingStamp())
		})
		Expect(err).NotTo(HaveOccurred())

		n, hasMore, err := oi.buildRange(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(1)), "a 1-byte write limit commits after the first record")
		Expect(hasMore).To(BeTrue())

		// With the default limit the rest is one range.
		n, hasMore, err = build(900_000).buildRange(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(4)))
		Expect(hasMore).To(BeFalse())
	})

	It("commits a mutual chunk early once the write limit is passed", func() {
		root := specSubspace()
		builder := baseBuilder()
		index := NewIndex("by_price", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 5; i++ {
				if _, err := store.SaveRecord(&demopb.Order{OrderId: proto.Int64(i), Price: proto.Int32(int32(10 * i))}); err != nil {
					return nil, err
				}
			}
			return nil, disableIndexes(store, index)
		})
		Expect(err).NotTo(HaveOccurred())
		oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetIndex(index).
			SetSubspace(root).SetMutualIndexing().SetLimit(100).SetMaxWriteLimitBytes(1).Build()
		Expect(err).NotTo(HaveOccurred())
		oi.sessionHeartbeat = NewIndexingHeartbeat("mutual worker", 30_000, true, nil)
		Expect(oi.markWriteOnly(ctx)).Error().To(Succeed())
		defer oi.cleanupPendingQueueHeartbeat(oi.sessionHeartbeat)

		n, hasMore, err := newMutualIndexBuilder(oi).buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(1)), "a 1-byte write limit commits after the first record")
		Expect(hasMore).To(BeTrue())
	})
})
