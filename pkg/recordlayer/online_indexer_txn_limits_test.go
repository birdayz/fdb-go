package recordlayer

import (
	"context"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Advance only during index maintenance, not on clock reads: resetting the
// transaction's start time for every record must leave its elapsed time at zero.
type transactionLimitClockFilter struct {
	normalMaintenanceFilter
	clock *dst.SimClock
}

func (f transactionLimitClockFilter) MaintainIndex(*Index, proto.Message) IndexValues {
	f.clock.Advance(11 * time.Millisecond)
	return IndexValuesAll
}

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

		m := newMutualIndexBuilder(oi)
		n, hasMore, err := m.buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(1)), "a 1-byte write limit commits after the first record")
		Expect(hasMore).To(BeTrue())

		// With the default limit the rest of the claim is one chunk.
		oi.maxWriteLimitBytes = 900_000
		n, hasMore, err = m.buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(4)))
		Expect(hasMore).To(BeTrue())
	})

	It("commits a mutual chunk early once the transaction time limit is passed", func() {
		root := specSubspace()
		clock := dst.NewSimClock(dst.Epoch)
		env := &dst.Env{Clock: clock, Random: dst.NewSeededRandomness(73)}
		db := NewFDBDatabase(sharedDB.db).SetEnv(env)
		builder := baseBuilder()
		index := NewIndex("by_price", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
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
		oi, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).SetIndex(index).
			SetSubspace(root).SetMutualIndexing().SetLimit(100).SetMaxWriteLimitBytes(900_000).
			SetTransactionTimeLimitMilliseconds(10).
			SetIndexMaintenanceFilter(transactionLimitClockFilter{clock: clock}).Build()
		Expect(err).NotTo(HaveOccurred())
		oi.sessionHeartbeat = NewIndexingHeartbeat("mutual timed worker", 30_000, true, env)
		Expect(oi.markWriteOnly(ctx)).Error().To(Succeed())
		defer oi.cleanupPendingQueueHeartbeat(oi.sessionHeartbeat)

		m := newMutualIndexBuilder(oi)
		n, hasMore, err := m.buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(1)), "indexing one record advances 11ms past the 10ms budget")
		Expect(hasMore).To(BeTrue())
		Expect(clock.Now().Sub(dst.Epoch)).To(Equal(11 * time.Millisecond))

		// Read in a new transaction to prove the early chunk and its boundary committed.
		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			ranges := NewIndexingRangeSet(root, index)
			contains, err := ranges.ContainsKey(rc.Transaction(), tuple.Tuple{int64(1)}.Pack())
			if err != nil {
				return nil, err
			}
			Expect(contains).To(BeTrue())
			missing, err := ranges.FirstMissingRange(rc.Transaction())
			if err != nil {
				return nil, err
			}
			Expect(missing).To(Equal(&RangeSetRange{Begin: tuple.Tuple{int64(2)}.Pack(), End: rangeSetFinalKey}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Relax only the time budget; unchanged count/write limits admit the entire remainder.
		oi.transactionTimeLimitMs = 1_000
		n, hasMore, err = m.buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(int64(4)))
		Expect(hasMore).To(BeTrue())
		n, hasMore, err = m.buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeZero())
		Expect(hasMore).To(BeFalse())
		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			complete, err := NewIndexingRangeSet(root, index).IsComplete(rc.Transaction())
			if err != nil {
				return nil, err
			}
			Expect(complete).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(oi.markReadable(ctx)).To(Succeed())

		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			if err != nil {
				return nil, err
			}
			Expect(store.IsIndexReadable(index.Name)).To(BeTrue())
			entries, err := AsList(ctx, store.ScanIndex(index, TupleRangeAll, nil, ForwardScan()))
			if err != nil {
				return nil, err
			}
			Expect(entries).To(HaveLen(5))
			for i, entry := range entries {
				Expect(entry.IndexValues()).To(Equal(tuple.Tuple{int64(10 * (i + 1))}))
				Expect(entry.PrimaryKey()).To(Equal(tuple.Tuple{int64(i + 1)}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
