package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// followupClockBackend advances only the record layer's simulated clock on each
// committed-queue-entry clear. All transaction and commit results come from FDB.
// Five seconds per entry forces the drainer's four-second quota to split work
// across transactions without waiting thirty wall-clock seconds for a lease.
type followupClockBackend struct {
	fdb.BackendDatabase
	clock       *dst.SimClock
	queuePrefix []byte
	clears      int
	afterCommit func()
}

func (b *followupClockBackend) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	tx, err := b.BackendDatabase.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	return &followupClockTransaction{WritableTransaction: tx, backend: b}, nil
}

func (b *followupClockBackend) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return b.TransactCtx(context.Background(), fn)
}

func (b *followupClockBackend) TransactCtx(ctx context.Context, fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	value, err := runTransactCtx(b.BackendDatabase, ctx, func(tx fdb.WritableTransaction) (any, error) {
		return fn(&followupClockTransaction{WritableTransaction: tx, backend: b})
	})
	if err == nil && b.afterCommit != nil {
		b.afterCommit()
	}
	return value, err
}

type followupClockTransaction struct {
	fdb.WritableTransaction
	backend *followupClockBackend
}

func (tx *followupClockTransaction) ClearRange(r fdb.ExactRange) {
	tx.WritableTransaction.ClearRange(r)
	begin, _ := r.FDBRangeKeys()
	if bytes.HasPrefix(begin.FDBKey(), tx.backend.queuePrefix) {
		tx.backend.clears++
		tx.backend.clock.Advance(5 * time.Second)
	}
}

func (tx *followupClockTransaction) Commit() fdb.FutureNil {
	return &followupClockCommit{FutureNil: tx.WritableTransaction.Commit(), backend: tx.backend}
}

type followupClockCommit struct {
	fdb.FutureNil
	backend *followupClockBackend
}

func (f *followupClockCommit) Get() error {
	err := f.FutureNil.Get()
	if err == nil && f.backend.afterCommit != nil {
		f.backend.afterCommit()
	}
	return err
}

var _ = Describe("OnlineIndexer follow-up liveness", func() {
	fixture := func(ctx context.Context, replacement bool) (*OnlineIndexer, *FDBDatabase, *followupClockBackend) {
		root := specSubspace()
		clock := dst.NewSimClock(dst.Epoch)
		env := &dst.Env{Clock: clock, Random: dst.NewSeededRandomness(37)}
		db := NewFDBDatabase(sharedDB.db).SetEnv(env)
		builder := baseBuilder()
		targets := []*Index{
			NewVectorIndex("a_queue", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1),
			NewVectorIndex("b_queue", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1),
			NewIndex("c_ordinary", Field("price")),
		}
		if replacement {
			targets[0].Options[IndexOptionReplacedByPrefix+"0"] = targets[1].Name
			targets[2] = NewVectorIndex("c_queue", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		}
		for _, index := range targets {
			builder.AddIndex("Order", index)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetFormatVersion(15).Create()
			if err != nil {
				return nil, err
			}
			return nil, disableIndexes(store, targets...)
		})
		Expect(err).NotTo(HaveOccurred())
		oi, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).SetSubspace(root).SetTargetIndexes(targets).
			SetPolicy(&IndexingPolicy{PendingWriteQueueIndexes: map[string]bool{"a_queue": true, "b_queue": true, "c_queue": replacement}}).Build()
		Expect(err).NotTo(HaveOccurred())
		oi.sessionHeartbeat = NewIndexingHeartbeat("follow-up owner", 30_000, false, env)
		Expect(oi.markWriteOnly(ctx)).Error().To(Succeed())
		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			Expect(err).NotTo(HaveOccurred())
			for _, index := range targets {
				_, err := NewIndexingRangeSet(root, index).InsertRange(rc.Transaction(), nil, nil, true)
				Expect(err).NotTo(HaveOccurred())
			}
			for id := int64(1); id <= 8; id++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Quantity: proto.Int32(7), Price: proto.Int32(int32(id))})
				Expect(err).NotTo(HaveOccurred())
			}
			for _, index := range oi.queuedIndexes {
				size, err := store.indexingPendingWriteQueue(index, 0).GetQueueSizeNoConflict(rc)
				Expect(err).NotTo(HaveOccurred())
				Expect(size).NotTo(BeNil())
				Expect(*size).To(Equal(int64(8)))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		backend := &followupClockBackend{BackendDatabase: sharedDB.db, clock: clock, queuePrefix: root.Sub(IndexBuildSpaceKey, targets[0].SubspaceTupleKey(), int64(8)).Bytes()}
		oi.db = NewFDBDatabaseWithBackend(backend).SetEnv(env)
		heartbeat := oi.sessionHeartbeat
		DeferCleanup(func() { backend.afterCommit = nil; oi.cleanupPendingQueueHeartbeat(heartbeat) })
		return oi, db, backend
	}

	for _, change := range []string{"disable", "rebuild", "replacement"} {
		It("permanently retires a published target before "+change+" during closeout", func() {
			ctx := context.Background()
			oi, db, backend := fixture(ctx, change == "replacement")
			changed := false
			var competitor *OnlineIndexer
			if change != "replacement" {
				backend.afterCommit = func() {
					if changed {
						return
					}
					_, err := db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store, err := oi.openStore(rc)
						Expect(err).NotTo(HaveOccurred())
						if store.GetIndexState(oi.targetIndexes[0].Name) != IndexStateReadable {
							return nil, nil
						}
						changed = true
						if change == "disable" {
							_, err = store.MarkIndexDisabled(oi.targetIndexes[0].Name)
						}
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
					if changed && change == "rebuild" {
						// A new build over a READABLE index is a rebuild only when the
						// policy asks for one (Java ifReadable REBUILD); the default leaves it.
						competitor, err = NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(oi.metaData).SetSubspace(oi.subspace).SetIndex(oi.targetIndexes[0]).
							SetPolicy(&IndexingPolicy{IfReadable: DesiredActionRebuild}).Build()
						Expect(err).NotTo(HaveOccurred())
						competitor.sessionHeartbeat = NewIndexingHeartbeat("new independent build", 30_000, false, db.Env())
						Expect(competitor.markWriteOnly(ctx)).Error().To(Succeed())
					}
				}
			}
			Expect(oi.markReadable(ctx)).To(Succeed())
			if change != "replacement" {
				Expect(changed).To(BeTrue())
			}
			backend.afterCommit = nil
			_, err := db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				Expect(err).NotTo(HaveOccurred())
				expected := IndexStateDisabled
				if change == "rebuild" {
					expected = IndexStateWriteOnly
				}
				Expect(store.GetIndexState(oi.targetIndexes[0].Name)).To(Equal(expected))
				for _, index := range oi.targetIndexes[1:] {
					Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
				}
				rows, err := rc.Transaction().GetRange(heartbeatSubspace(oi.subspace, oi.targetIndexes[0]), fdb.RangeOptions{}).GetSliceWithError()
				Expect(err).NotTo(HaveOccurred())
				if change == "rebuild" {
					Expect(rows).To(HaveLen(1), "old owner must not recreate its heartbeat")
					stamp, err := store.LoadIndexingTypeStamp(oi.targetIndexes[0])
					Expect(err).NotTo(HaveOccurred())
					Expect(stamp.GetMethod()).To(Equal(gen.IndexBuildIndexingStamp_BY_RECORDS))
				} else {
					Expect(rows).To(BeEmpty())
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			if competitor != nil {
				competitor.cleanupPendingQueueHeartbeat(competitor.sessionHeartbeat)
			}
		})
	}

	for _, committed := range []bool{false, true} {
		It(fmt.Sprintf("publishes peer retirement only after observation commits=%t", committed), func() {
			ctx := context.Background()
			oi, db, _ := fixture(ctx, false)
			first := oi.targetIndexes[0]
			Expect(oi.drainPendingIndexWritesForIndex(ctx, first, oi.sessionHeartbeat)).To(Succeed())
			_, err := db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexReadable(first.Name)
				Expect(err).NotTo(HaveOccurred())
				return nil, store.eraseAllIndexingDataButTheLockAndRangeSet(first)
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				Expect(err).NotTo(HaveOccurred())
				Expect(oi.refreshFollowupHeartbeats(store, oi.sessionHeartbeat)).To(Succeed())
				Expect(oi.retiredBuildTargets[first.Name]).To(BeFalse(), "observation is not yet committed")
				if !committed {
					return nil, &RecordCoreError{Message: "abort retirement observation"}
				}
				return nil, nil
			})
			if committed {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(MatchError("abort retirement observation"))
			}
			Expect(oi.retiredBuildTargets[first.Name]).To(Equal(committed))
			_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				Expect(err).NotTo(HaveOccurred())
				_, err = store.MarkIndexDisabled(first.Name)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			for _, followup := range []string{"drain", "merge"} {
				if followup == "drain" {
					err = oi.drainPendingIndexWritesForIndex(ctx, oi.targetIndexes[1], oi.sessionHeartbeat)
				} else {
					err = oi.mergeIndex(ctx, oi.targetIndexes[1])
				}
				if committed {
					Expect(err).NotTo(HaveOccurred())
				} else {
					// Java's follow-up skips a target that is no longer
					// write-only and its next state check is a storage error,
					// never a validation error the catcher would fall back on.
					var changed *RecordCoreStorageError
					Expect(errors.As(err, &changed)).To(BeTrue(), "%v", err)
					Expect(changed.IndexName).To(Equal(first.Name))
				}
			}
			if committed {
				Expect(oi.markReadable(ctx)).To(Succeed())
			}
		})
	}

	It("starts a new ownership set when the same indexer builds again", func() {
		ctx := context.Background()
		oi, db, backend := fixture(ctx, false)
		Expect(oi.markReadable(ctx)).To(Succeed())
		Expect(oi.retiredBuildTargets).To(HaveLen(3))
		// A new build must not inherit completion from the old session. The targets
		// are READABLE now, so the new session rebuilds them only because the policy
		// asks for it (Java ifReadable REBUILD); the default would leave them.
		backend.queuePrefix = []byte{0xff}
		oi.policy.IfReadable = DesiredActionRebuild
		count, err := oi.BuildIndex(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(count).To(Equal(int64(8)))
		_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			Expect(err).NotTo(HaveOccurred())
			for _, index := range oi.targetIndexes {
				Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, followup := range []string{"drain", "merge"} {
		It("keeps queued and ordinary followers owned through long "+followup+" and retires published targets", func() {
			ctx := context.Background()
			oi, db, backend := fixture(ctx, false)
			checks := 0
			backend.afterCommit = func() {
				checks++
				for _, follower := range oi.targetIndexes[1:] {
					competitor, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(oi.metaData).SetSubspace(oi.subspace).SetIndex(follower).
						SetPolicy(&IndexingPolicy{AllowedTakeovers: map[TakeoverType]bool{TakeoverMultiTargetToSingle: true}}).Build()
					Expect(err).NotTo(HaveOccurred())
					competitor.sessionHeartbeat = NewIndexingHeartbeat("permitted single-target takeover", 30_000, false, db.Env())
					var locked *SynchronizedSessionLockedError
					_, err = competitor.markWriteOnly(ctx)
					Expect(errors.As(err, &locked)).To(BeTrue(), "follower=%s elapsed=%v error=%v", follower.Name, backend.clock.Now().Sub(dst.Epoch), err)
				}
			}
			if followup == "drain" {
				Expect(oi.drainPendingIndexWrites(ctx, oi.sessionHeartbeat)).To(Succeed())
				Expect(backend.clears).To(Equal(8))
			} else {
				for range 8 {
					backend.clock.Advance(5 * time.Second)
					Expect(oi.mergeIndex(ctx, oi.targetIndexes[0])).To(Succeed())
				}
				Expect(backend.clears).To(BeZero(), "HNSW merges only exercise callback wiring, not deferred backend work")
			}
			Expect(checks).To(BeNumerically(">=", 8))
			Expect(backend.clock.Now().Sub(dst.Epoch)).To(BeNumerically(">", 30*time.Second))
			backend.afterCommit = nil
			Expect(oi.markReadable(ctx)).To(Succeed())
			_, err := db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				Expect(err).NotTo(HaveOccurred())
				for _, index := range oi.targetIndexes {
					Expect(store.GetIndexState(index.Name)).To(Equal(IndexStateReadable))
					rows, err := rc.Transaction().GetRange(heartbeatSubspace(oi.subspace, index), fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(BeEmpty(), "later follow-up must not recreate a published target's heartbeat")
				}
				for _, index := range oi.queuedIndexes {
					empty, err := store.indexingPendingWriteQueue(index, 0).IsQueueEmpty(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(empty).To(BeTrue())
					rows, err := store.SearchVectorIndexWithPrefix(index, tuple.Tuple{int64(7)}, []float64{1}, 20, 100)
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(HaveLen(8))
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		})

		for _, change := range []string{"block", "disable"} {
			It(fmt.Sprintf("fences a follower %s during %s before committing queue removal", change, followup), func() {
				ctx := context.Background()
				oi, db, _ := fixture(ctx, false)
				follower := oi.targetIndexes[1]
				_, err := db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := oi.openStore(rc)
					Expect(err).NotTo(HaveOccurred())
					if change == "disable" {
						_, err = store.MarkIndexDisabled(follower.Name)
						return nil, err
					}
					stamp, err := store.LoadIndexingTypeStamp(follower)
					Expect(err).NotTo(HaveOccurred())
					stamp.Block = proto.Bool(true)
					return nil, store.SaveIndexingTypeStamp(follower, stamp)
				})
				Expect(err).NotTo(HaveOccurred())
				if followup == "drain" {
					err = oi.drainPendingIndexWritesForIndex(ctx, oi.targetIndexes[0], oi.sessionHeartbeat)
				} else {
					err = oi.mergeIndex(ctx, oi.targetIndexes[0])
				}
				if change == "block" {
					var blocked *PartlyBuiltError
					Expect(errors.As(err, &blocked)).To(BeTrue(), "%v", err)
				} else {
					var changed *RecordCoreStorageError
					Expect(errors.As(err, &changed)).To(BeTrue(), "%v", err)
					Expect(changed.IndexName).To(Equal(follower.Name))
				}
				_, err = db.Run(ctx, func(rc *FDBRecordContext) (any, error) {
					store, err := oi.openStore(rc)
					Expect(err).NotTo(HaveOccurred())
					queue := store.indexingPendingWriteQueue(oi.targetIndexes[0], 0)
					size, err := queue.GetQueueSizeNoConflict(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(size).NotTo(BeNil())
					Expect(*size).To(Equal(int64(8)), "follower validation failure must roll back the current target's work")
					empty, err := queue.IsQueueEmpty(rc)
					Expect(err).NotTo(HaveOccurred())
					Expect(empty).To(BeFalse())
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
			})
		}
	}
})
