package recordlayer

import (
	"context"
	"errors"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// afterBuildCommitTransactor runs a competing transaction only after the first
// build transaction commits, so queued producer behavior is observed durably.
type afterBuildCommitTransactor struct {
	fdb.Transactor
	after func() error
}

func (t *afterBuildCommitTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	value, err := t.Transactor.Transact(fn)
	if err == nil && t.after != nil {
		after := t.after
		t.after = nil
		err = after()
	}
	return value, err
}

var _ = Describe("Format 15 queued lifecycle", func() {
	ctx := context.Background()
	metadata := func() (*RecordMetaData, *Index, *Index) {
		builder := baseBuilder()
		vector := NewVectorIndex("vector", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		ordinary := NewIndex("ordinary", Field("price"))
		builder.AddIndex("Order", vector)
		builder.AddIndex("Order", ordinary)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md, vector, ordinary
	}
	order := func(id int64, price int32) *gen.Order {
		return &gen.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(1), Price: proto.Int32(price)}
	}
	It("keeps default creation at 14 while explicitly creating upgrading and reopening 15", func() {
		md, _, _ := metadata()
		root := specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			Expect(store.GetFormatVersion()).To(Equal(int32(14)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Open()
			if err != nil {
				return nil, err
			}
			Expect(store.GetFormatVersion()).To(Equal(int32(15)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
			if err != nil {
				return nil, err
			}
			Expect(store.GetFormatVersion()).To(Equal(int32(15)))
			_, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root.Sub("explicit")).SetFormatVersion(15).Create()
			if err != nil {
				return nil, err
			}
			_, err = NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root.Sub("future")).SetFormatVersion(16).Create()
			var unsupported *UnsupportedFormatVersionError
			Expect(errors.As(err, &unsupported)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("builds mixed queued and ordinary targets through normal opening with live producers", func() {
		md, vector, ordinary := metadata()
		root := specSubspace()
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Create()
			if err != nil {
				return nil, err
			}
			for _, record := range []*gen.Order{order(1, 10), order(2, 20)} {
				if _, err := store.SaveRecord(record); err != nil {
					return nil, err
				}
			}
			return nil, disableIndexes(store, vector, ordinary)
		})
		Expect(err).NotTo(HaveOccurred())
		writerRan := false
		interleaver := &afterBuildCommitTransactor{Transactor: sharedDB.transactor, after: func() error {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
				if err != nil {
					return nil, err
				}
				Expect(store.GetIndexState(vector.Name)).To(Equal(IndexStateWriteOnlyWithQueue))
				Expect(store.GetIndexState(ordinary.Name)).To(Equal(IndexStateWriteOnly))
				if _, err := store.SaveRecord(order(1, 100)); err != nil {
					return nil, err
				}
				if _, err := store.DeleteRecord(tuple.Tuple{int64(2)}); err != nil {
					return nil, err
				}
				if _, err := store.SaveRecord(order(3, 30)); err != nil {
					return nil, err
				}
				Expect(rc.HasVersionMutations()).To(BeTrue())
				writerRan = true
				return nil, nil
			})
			return err
		}}
		db := NewFDBDatabaseWithTransactor(interleaver, sharedDB.db)
		oi, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).SetSubspace(root).SetTargetIndexes([]*Index{vector, ordinary}).SetFormatVersion(15).SetLimit(1).SetPolicy(&IndexingPolicy{PendingWriteQueueIndexes: map[string]bool{vector.Name: true, ordinary.Name: true}}).Build()
		Expect(err).NotTo(HaveOccurred())
		count, err := oi.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int64(2)))
		Expect(writerRan).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			if err != nil {
				return nil, err
			}
			for _, index := range []*Index{vector, ordinary} {
				Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
				begin, end := heartbeatSubspace(root, index).FDBRangeKeys()
				rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(BeEmpty())
			}
			empty, err := store.isIndexPendingQueueEmpty(vector)
			Expect(empty).To(BeTrue())
			Expect(err).NotTo(HaveOccurred())
			results, err := store.SearchVectorIndexWithPrefix(vector, tuple.Tuple{int64(1)}, []float64{100}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))
			Expect(results[0].PrimaryKey).To(Equal(tuple.Tuple{int64(1)}))
			Expect(results[0].Distance).To(BeZero())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	for _, terminal := range []string{"unreadable success", "cancelled", "blocked"} {
		It("cleans and resumes a queued build after "+terminal, func() {
			md, vector, _ := metadata()
			root := specSubspace()
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Create()
				if err != nil {
					return nil, err
				}
				if _, err = store.SaveRecord(order(1, 10)); err != nil {
					return nil, err
				}
				return nil, disableIndexes(store, vector)
			})
			Expect(err).NotTo(HaveOccurred())
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			interleaver := &afterBuildCommitTransactor{Transactor: sharedDB.transactor, after: func() error {
				if terminal == "cancelled" {
					cancel()
					return nil
				}
				if terminal == "blocked" {
					_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Open()
						if err != nil {
							return nil, err
						}
						stamp, err := store.LoadIndexingTypeStamp(vector)
						if err != nil {
							return nil, err
						}
						stamp.Block = proto.Bool(true)
						return nil, store.SaveIndexingTypeStamp(vector, stamp)
					})
					return err
				}
				return nil
			}}
			oi, err := NewOnlineIndexerBuilder().SetDatabase(NewFDBDatabaseWithTransactor(interleaver, sharedDB.db)).SetMetaData(md).SetSubspace(root).SetIndex(vector).SetMarkReadable(false).SetPolicy(&IndexingPolicy{PendingWriteQueueIndexes: map[string]bool{vector.Name: true}}).Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = oi.BuildIndex(work)
			switch terminal {
			case "unreadable success":
				Expect(err).NotTo(HaveOccurred())
			case "cancelled":
				Expect(errors.Is(err, context.Canceled)).To(BeTrue())
			case "blocked":
				var partly *PartlyBuiltError
				Expect(errors.As(err, &partly)).To(BeTrue())
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				if err != nil {
					return nil, err
				}
				Expect(store.GetIndexState(vector.Name)).To(Equal(IndexStateWriteOnlyWithQueue))
				begin, end := heartbeatSubspace(root, vector).FDBRangeKeys()
				rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				Expect(rows).To(BeEmpty())
				_, err = store.SaveRecord(order(2, 20))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			resumed, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).SetIndex(vector).SetPolicy(&IndexingPolicy{AllowUnblock: true}).Build()
			Expect(err).NotTo(HaveOccurred())
			_, err = resumed.BuildIndex(ctx)
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := resumed.openStore(rc)
				if err != nil {
					return nil, err
				}
				Expect(store.GetIndexState(vector.Name)).To(Equal(IndexStateReadable))
				results, err := store.SearchVectorIndexWithPrefix(vector, tuple.Tuple{int64(1)}, []float64{0}, 10, 100)
				Expect(results).To(HaveLen(2))
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})
