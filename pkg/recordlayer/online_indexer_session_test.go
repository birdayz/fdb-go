package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fdb.dev/pkg/dst"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// batchMutationTransactor commits a real competing write after a successful
// batch body but before its commit. The backend, not this wrapper, determines
// whether to abort and retry the batch.
type batchMutationTransactor struct {
	fdb.Transactor
	mutate   func() error
	attempts int
}

func (t *batchMutationTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return t.Transactor.Transact(func(tx fdb.WritableTransaction) (any, error) {
		t.attempts++
		value, err := fn(tx)
		if err != nil {
			return nil, err
		}
		if t.mutate != nil {
			mutate := t.mutate
			t.mutate = nil
			if err := mutate(); err != nil {
				return nil, err
			}
		}
		return value, nil
	})
}

var _ = Describe("OnlineIndexer batch session fencing", func() {
	ctx := context.Background()
	for _, byIndex := range []bool{false, true} {
		It(fmt.Sprintf("conflicts a snapshot batch with a concurrent record deletion byIndex=%t", byIndex), func() {
			root := specSubspace()
			builder := baseBuilder()
			target := NewIndex("target", Field("price"))
			source := NewIndex("source", Field("quantity"))
			builder.AddIndex("Order", target)
			builder.AddIndex("Order", source)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{target}, limit: 10}
			if byIndex {
				oi.sourceIndex = source
			}
			var recordRange fdb.ExactRange
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(2)})
				if err != nil {
					return nil, err
				}
				recordRange = store.getRangeForRecord(tuple.Tuple{int64(1)})
				_, err = store.ClearAndMarkIndexWriteOnly(target.Name)
				if err != nil {
					return nil, err
				}
				return nil, store.SaveIndexingTypeStamp(target, oi.buildIndexingStamp())
			})
			Expect(err).NotTo(HaveOccurred())
			interleaver := &batchMutationTransactor{Transactor: sharedDB.transactor, mutate: func() error {
				_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) { rc.Transaction().ClearRange(recordRange); return nil, nil })
				return err
			}}
			oi.db = NewFDBDatabaseWithTransactor(interleaver, sharedDB.db)
			build := oi.buildRange
			if byIndex {
				build = oi.buildRangeByIndex
			}
			_, _, err = build(ctx)
			// The source entry deliberately remains: BY_INDEX must report the
			// orphan on retry, never commit stale target maintenance.
			if byIndex {
				var orphan *RecordCoreStorageError
				Expect(errors.As(err, &orphan)).To(BeTrue())
				Expect(orphan.Message).To(Equal("record not found from index entry"))
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(interleaver.attempts).To(BeNumerically(">=", 2))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				if err != nil {
					return nil, err
				}
				begin, end := store.indexSubspace(target).FDBRangeKeys()
				rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				Expect(rows).To(BeEmpty(), "aborted snapshot batch must not resurrect the deleted record in the target")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
	for _, mutual := range []bool{false, true} {
		It(fmt.Sprintf("aborts staged index writes after a follower range claim is lost mutual=%t", mutual), func() {
			root := specSubspace()
			builder := baseBuilder()
			first := NewIndex("first", Field("price"))
			second := NewIndex("second", Field("quantity"))
			builder.AddIndex("Order", first)
			builder.AddIndex("Order", second)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{first, second}, limit: 10, mutual: mutual, leaseLengthMs: 30000}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(2)})
				if err != nil {
					return nil, err
				}
				for _, index := range oi.targetIndexes {
					if _, err := store.ClearAndMarkIndexWriteOnly(index.Name); err != nil {
						return nil, err
					}
					if err := store.SaveIndexingTypeStamp(index, oi.buildIndexingStamp()); err != nil {
						return nil, err
					}
				}
				_, err = NewIndexingRangeSet(root, second).InsertRange(rc.Transaction(), nil, nil, true)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			if mutual {
				m, setupErr := newMutualIndexBuilder(oi)
				Expect(setupErr).NotTo(HaveOccurred())
				_, _, err = m.buildMutual(ctx)
			} else {
				_, _, err = oi.buildRange(ctx)
			}
			var lost *IndexRangeClaimLostError
			Expect(errors.As(err, &lost)).To(BeTrue())
			Expect(lost.IndexName).To(Equal(second.Name))
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := oi.openStore(rc)
				if err != nil {
					return nil, err
				}
				for _, index := range oi.targetIndexes {
					begin, end := store.indexSubspace(index).FDBRangeKeys()
					rows, err := rc.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
					Expect(err).NotTo(HaveOccurred())
					Expect(rows).To(BeEmpty(), "failed claim must roll back all staged index writes")
				}
				missing, err := NewIndexingRangeSet(root, first).FirstMissingRange(rc.Transaction())
				Expect(missing).NotTo(BeNil())
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
	It("publishes mutual fragment progress only after a successful commit", func() {
		root := specSubspace()
		builder := baseBuilder()
		index := NewIndex("target", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{index}, limit: 10, mutual: true, leaseLengthMs: 30000}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			_, err = store.ClearAndMarkIndexWriteOnly(index.Name)
			if err != nil {
				return nil, err
			}
			if err := store.SaveIndexingTypeStamp(index, oi.buildIndexingStamp()); err != nil {
				return nil, err
			}
			_, err = NewIndexingRangeSet(root, index).InsertRange(rc.Transaction(), nil, nil, true)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		m, err := newMutualIndexBuilder(oi)
		Expect(err).NotTo(HaveOccurred())
		failure := &RecordCoreError{Message: "reject exhausted fragment commit"}
		interleaver := &batchMutationTransactor{Transactor: sharedDB.transactor, mutate: func() error { return failure }}
		oi.db = NewFDBDatabaseWithTransactor(interleaver, sharedDB.db)
		_, _, err = m.buildMutual(ctx)
		Expect(err).To(BeIdenticalTo(failure))
		Expect(m.iterType).To(Equal(fragmentFull))
		oi.db = sharedDB
		_, more, err := m.buildMutual(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(more).To(BeFalse())
		Expect(m.iterType).To(Equal(fragmentRecover))
		oi.cleanupPendingQueueHeartbeat(m.heartbeat)
	})

	for _, change := range []string{"disabled", "blocked", "method", "other-session"} {
		It("rejects an empty batch after "+change, func() {
			root := specSubspace()
			builder := baseBuilder()
			index := NewIndex("target", Field("price"))
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			oi := &OnlineIndexer{db: sharedDB, metaData: md, subspace: root, targetIndexes: []*Index{index}, limit: 10, sessionHeartbeat: NewIndexingHeartbeat("builder", 30000, false, sharedDB.Env())}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
				if err != nil {
					return nil, err
				}
				_, err = store.ClearAndMarkIndexWriteOnly(index.Name)
				if err != nil {
					return nil, err
				}
				stamp := oi.buildIndexingStamp()
				switch change {
				case "disabled":
					_, err = store.MarkIndexDisabled(index.Name)
				case "blocked":
					stamp.Block = proto.Bool(true)
				case "method":
					stamp.Method = gen.IndexBuildIndexingStamp_BY_INDEX.Enum()
				case "other-session":
					err = NewIndexingHeartbeat("competitor", 30000, false, sharedDB.Env()).CheckAndUpdate(rc.Transaction(), root, index)
				}
				if err != nil {
					return nil, err
				}
				return nil, store.SaveIndexingTypeStamp(index, stamp)
			})
			Expect(err).NotTo(HaveOccurred())
			_, _, err = oi.buildRange(ctx)
			Expect(err).To(HaveOccurred())
			if change == "blocked" || change == "method" {
				var partly *PartlyBuiltError
				Expect(errors.As(err, &partly)).To(BeTrue())
			}
			_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				missing, err := NewIndexingRangeSet(root, index).FirstMissingRange(rc.Transaction())
				Expect(missing).NotTo(BeNil(), "rejected batch cannot mark the empty store built")
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}
})

func TestOnlineIndexerOngoingStampValidation(t *testing.T) {
	t.Parallel()
	index := NewIndex("target", Field("price"))
	oi := &OnlineIndexer{targetIndexes: []*Index{index}}
	env := dst.NewSim(17)
	store := &FDBRecordStore{context: NewFDBRecordContext(nil, env)}
	for _, tc := range []struct {
		name      string
		stamp     *gen.IndexBuildIndexingStamp
		wantError bool
	}{
		{"legacy missing BY_RECORDS", nil, false},
		{"method changed", &gen.IndexBuildIndexingStamp{Method: gen.IndexBuildIndexingStamp_BY_INDEX.Enum()}, true},
		{"ancillary fields do not decide ongoing validation", &gen.IndexBuildIndexingStamp{Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(), TargetIndex: []string{"different"}}, false},
		{"permanent block", &gen.IndexBuildIndexingStamp{Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(), Block: proto.Bool(true)}, true},
		{"future block", &gen.IndexBuildIndexingStamp{Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(), Block: proto.Bool(true), BlockExpireEpochMilliSeconds: proto.Uint64(uint64(env.Now().UnixMilli() + 1))}, true},
		{"expired block", &gen.IndexBuildIndexingStamp{Method: gen.IndexBuildIndexingStamp_BY_RECORDS.Enum(), Block: proto.Bool(true), BlockExpireEpochMilliSeconds: proto.Uint64(uint64(env.Now().UnixMilli()))}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := oi.validateBuildStamp(store, index, tc.stamp)
			if (err != nil) != tc.wantError {
				t.Fatalf("validateBuildStamp error=%v; wantError=%v", err, tc.wantError)
			}
		})
	}
}

// The admission matrix. A session checks heartbeats at two points: at open,
// before metadata reconciliation (checkOpenHeartbeats), and once its action is
// resolved, only if it builds (prepareIndexingState). What each cell expects:
//   - metadata modes: the stored metadata is older, so reconciliation will run
//     and the open-time check refuses every blocker.
//   - state "readable": the session skips. Through BuildIndex the open-time
//     check still refuses legacy and malformed keys, but a live peer does not
//     stop it; prepareIndexingState alone checks nothing.
//   - state "disabled": every target is DISABLED, the session clears them
//     (REBUILD), and a clear admits no other live session, mutual or not.
//   - state "write-only": a continued build. A mutual session admits a live
//     mutual peer and builds; anything else refuses every blocker.
//   - state "mismatch": a DISABLED primary with a WRITE_ONLY follower is
//     refused as a state mismatch before heartbeats are consulted (Java checks
//     the followers before setIndexingTypeOrThrow reaches the heartbeats).
//
// A refused or skipping session must leave every store byte as it found it.
var _ = Describe("OnlineIndexer preparation heartbeat admission", func() {
	for _, mode := range []string{"ordinary", "queued", "multi", "mutual", "metadata", "metadata-mutual", "direct-ordinary", "direct-queued", "direct-multi"} {
		multi := mode == "multi" || mode == "mutual" || mode == "direct-multi"
		metadata := mode == "metadata" || mode == "metadata-mutual"
		mutualMode := mode == "mutual" || mode == "metadata-mutual"
		direct := strings.HasPrefix(mode, "direct-")
		states := []string{"readable", "disabled", "write-only"}
		if multi {
			states = append(states, "mismatch")
		}
		if metadata {
			states = []string{"readable"} // Target indexes do not exist in the old metadata.
		}
		for _, blocker := range []string{"legacy", "malformed", "live"} {
			for _, state := range states {
				It(fmt.Sprintf("preserves heartbeat evidence mode=%s blocker=%s state=%s", mode, blocker, state), func() {
					ctx := context.Background()
					root := specSubspace()
					builder := baseBuilder()
					primary := NewIndex("a_primary", Field("price"))
					follower := NewVectorIndex("z_follower", KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
					primary.AddedVersion, primary.LastModifiedVersion = 2, 2
					follower.AddedVersion, follower.LastModifiedVersion = 2, 2
					builder.AddIndex("Order", primary)
					builder.AddIndex("Order", follower)
					builder.SetVersion(2)
					md, err := builder.Build()
					Expect(err).NotTo(HaveOccurred())
					targets := []*Index{primary}
					if mode == "queued" || mode == "direct-queued" {
						targets = []*Index{follower}
					} else if multi {
						targets = []*Index{primary, follower}
					}
					blocked := targets[len(targets)-1]
					storedMD := md
					if metadata {
						oldBuilder := baseBuilder().SetVersion(1)
						storedMD, err = oldBuilder.Build()
						Expect(err).NotTo(HaveOccurred())
					}
					peer := NewIndexingHeartbeat("live follower", 60_000, true, nil)
					key := peer.heartbeatKey(root, blocked)
					if blocker == "legacy" {
						key = heartbeatSubspace(root, blocked).Pack(tuple.Tuple{"legacy-owner"})
					} else if blocker == "malformed" {
						key = heartbeatSubspace(root, blocked).Pack(tuple.Tuple{int64(7)})
					}
					_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(storedMD).SetSubspace(root).SetFormatVersion(15).Create()
						if err != nil {
							return nil, err
						}
						if !metadata {
							for i, target := range targets {
								switch {
								case state == "disabled" || state == "mismatch" && i == 0:
									_, err = store.MarkIndexDisabled(target.Name)
								case state == "write-only" || state == "mismatch":
									_, err = store.ClearAndMarkIndexWriteOnly(target.Name)
								}
								Expect(err).NotTo(HaveOccurred())
							}
						}
						peer.update(rc.Transaction(), root, blocked)
						value, err := rc.Transaction().Get(peer.heartbeatKey(root, blocked)).Get()
						Expect(err).NotTo(HaveOccurred())
						rc.Transaction().Clear(peer.heartbeatKey(root, blocked))
						rc.Transaction().Set(key, value)
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
					ib := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).SetTargetIndexes(targets).
						SetPolicy(&IndexingPolicy{PendingWriteQueueIndexes: map[string]bool{follower.Name: true}})
					if mutualMode {
						ib.SetMutualIndexing()
					}
					oi, err := ib.Build()
					Expect(err).NotTo(HaveOccurred())
					var before []fdb.KeyValue
					_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						before, err = rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
						return nil, err
					})
					Expect(err).NotTo(HaveOccurred())
					assert := func(err error) (preserved bool) {
						switch {
						case metadata:
						case state == "mismatch" && (direct || blocker == "live"):
							var invalid *IndexingValidationError
							Expect(errors.As(err, &invalid)).To(BeTrue(), "error: %v", err)
							Expect(invalid.Message).To(Equal("A target index state doesn't match the primary index state"))
							return true
						case state == "readable" && (direct || blocker == "live"):
							Expect(err).NotTo(HaveOccurred(), "a skipping session was stopped")
							return true
						case state == "write-only" && mutualMode && blocker == "live":
							Expect(err).NotTo(HaveOccurred(), "a continued mutual build refused a live mutual peer")
							return false
						}
						if blocker == "live" {
							var locked *SynchronizedSessionLockedError
							Expect(errors.As(err, &locked)).To(BeTrue(), "error: %v", err)
						} else {
							var incompatible *IndexingHeartbeatKeyError
							Expect(errors.As(err, &incompatible)).To(BeTrue(), "error: %v", err)
						}
						return true
					}
					preserved := true
					if direct {
						_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
							store, err := oi.openStore(rc)
							Expect(err).NotTo(HaveOccurred())
							_, _, err = oi.prepareIndexingState(store)
							preserved = assert(err)
							// Commit intentionally: refusal must precede mutations, not
							// merely rely on the caller aborting the transaction.
							return nil, nil
						})
						Expect(err).NotTo(HaveOccurred())
					} else {
						_, err = oi.BuildIndex(ctx)
						preserved = assert(err)
					}
					if !preserved {
						return
					}
					_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
						after, err := rc.Transaction().GetRange(root, fdb.RangeOptions{}).GetSliceWithError()
						Expect(err).NotTo(HaveOccurred())
						Expect(after).To(Equal(before), "admission must preserve all store bytes")
						return nil, nil
					})
					Expect(err).NotTo(HaveOccurred())
				})
			}
		}
	}
})

// cleanupCommitBarrier holds readiness of a real dispatched FDB commit. Its
// cancellation can deliberately be ineffective, as with a detached pure-Go
// commit. It does not invent commit results or require a blocked Get goroutine.
type cleanupCommitBarrier struct {
	fdb.BackendDatabase
	fdb.CtxTransactor
	entered, released chan struct{}
	ignoreCancel      bool
	gets              atomic.Int32
	releaseOnce       sync.Once
	timeout           atomic.Int64
	txCancelled       atomic.Bool
	futureCancelled   atomic.Bool
	beforeCommit      func(fdb.WritableTransaction)
	timeouts          []int64
	// holdDispatch models a dispatched commit that reaches the cluster LATE, after
	// the cleanup has returned: Commit only records the transaction, cancellation
	// and the timeout are not applied to it (a dispatched pure-Go commit is
	// detached and does not enforce the timeout), and landLate commits it.
	holdDispatch bool
	held         fdb.WritableTransaction
}

// landLate commits the transaction a holdDispatch barrier held, as the cluster
// would when the detached commit finally arrives.
func (b *cleanupCommitBarrier) landLate() error { return b.held.Commit().Get() }

func (b *cleanupCommitBarrier) release() { b.releaseOnce.Do(func() { close(b.released) }) }

func (b *cleanupCommitBarrier) CreateWritableTransaction() (fdb.WritableTransaction, error) {
	tx, err := b.BackendDatabase.CreateWritableTransaction()
	if err != nil {
		return nil, err
	}
	return &cleanupBarrierTransaction{WritableTransaction: tx, barrier: b}, nil
}

type cleanupBarrierTransaction struct {
	fdb.WritableTransaction
	barrier *cleanupCommitBarrier
}

func (t *cleanupBarrierTransaction) Options() fdb.TransactionOptions {
	return &cleanupBarrierOptions{TransactionOptions: t.WritableTransaction.Options(), barrier: t.barrier}
}

func (t *cleanupBarrierTransaction) Commit() fdb.FutureNil {
	if t.barrier.holdDispatch {
		t.barrier.held = t.WritableTransaction
		close(t.barrier.entered)
		return &cleanupHeldFuture{}
	}
	if t.barrier.beforeCommit != nil {
		t.barrier.beforeCommit(t.WritableTransaction)
		return t.WritableTransaction.Commit()
	}
	future := &cleanupBarrierFuture{FutureNil: t.WritableTransaction.Commit(), barrier: t.barrier}
	close(t.barrier.entered)
	return future
}

func (t *cleanupBarrierTransaction) Cancel() {
	t.barrier.txCancelled.Store(true)
	if t.barrier.holdDispatch && t.barrier.held != nil {
		return
	}
	t.WritableTransaction.Cancel()
}

// cleanupHeldFuture is the future of a held commit: never ready while the
// cleanup waits, and inert to cancellation, as a detached pure-Go commit is.
type cleanupHeldFuture struct{}

func (cleanupHeldFuture) Get() error {
	Fail("a held commit is never collected")
	return nil
}
func (f cleanupHeldFuture) MustGet()         { _ = f.Get() }
func (f cleanupHeldFuture) BlockUntilReady() { _ = f.Get() }
func (cleanupHeldFuture) IsReady() bool      { return false }
func (cleanupHeldFuture) Cancel()            {}

type cleanupBarrierOptions struct {
	fdb.TransactionOptions
	barrier *cleanupCommitBarrier
}

func (o *cleanupBarrierOptions) SetTimeout(ms int64) error {
	o.barrier.timeout.Store(ms)
	o.barrier.timeouts = append(o.barrier.timeouts, ms)
	if o.barrier.holdDispatch {
		return nil
	}
	return o.TransactionOptions.SetTimeout(ms)
}

type cleanupBarrierFuture struct {
	fdb.FutureNil
	barrier *cleanupCommitBarrier
}

func (f *cleanupBarrierFuture) Get() error {
	f.barrier.gets.Add(1)
	<-f.barrier.released
	return f.FutureNil.Get()
}

func (f *cleanupBarrierFuture) IsReady() bool {
	select {
	case <-f.barrier.released:
		return f.FutureNil.IsReady()
	default:
		return false
	}
}

func (f *cleanupBarrierFuture) Cancel() {
	f.barrier.futureCancelled.Store(true)
	f.FutureNil.Cancel()
	if !f.barrier.ignoreCancel {
		f.barrier.release()
	}
}

var _ = Describe("OnlineIndexer bounded heartbeat cleanup", func() {
	newBarrier := func() *cleanupCommitBarrier {
		return &cleanupCommitBarrier{BackendDatabase: sharedDB.db, CtxTransactor: sharedDB.db, entered: make(chan struct{}), released: make(chan struct{})}
	}
	for _, deadline := range []bool{false, true} {
		for _, ignoreCancel := range []bool{false, true} {
			It(fmt.Sprintf("ends a blocked dispatched commit deadline=%t ignoreCancel=%t", deadline, ignoreCancel), func() {
				barrier := newBarrier()
				barrier.ignoreCancel = ignoreCancel
				defer barrier.release()
				index := NewIndex("target", Field("price"))
				oi := &OnlineIndexer{db: NewFDBDatabaseWithBackend(barrier), subspace: specSubspace(), targetIndexes: []*Index{index}}
				heartbeat := NewIndexingHeartbeat("cleanup", 30_000, false, nil)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- oi.cleanupHeartbeatWithin(ctx, heartbeat) }()
				Eventually(barrier.entered, 5*time.Second).Should(BeClosed(), "must reach the commit wait")
				Expect(barrier.timeout.Load()).To(BeNumerically(">", 0))
				Expect(barrier.timeout.Load()).To(BeNumerically("<=", 2000))
				if !deadline {
					cancel()
				}
				var err error
				Eventually(done, 5*time.Second).Should(Receive(&err))
				if deadline {
					Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "%v", err)
				} else {
					Expect(errors.Is(err, context.Canceled)).To(BeTrue(), "%v", err)
				}
				Expect(barrier.futureCancelled.Load()).To(BeTrue())
				Expect(barrier.txCancelled.Load()).To(BeTrue())
				Expect(barrier.gets.Load()).To(BeZero(), "must not launch a Get that can outlive cleanup")
			})
		}
	}
	// The identity is one per OnlineIndexer, so the next attempt of the same
	// indexer writes the key a timed-out cleanup is still clearing. A clear that
	// lands after that write must not erase it: the cleanup reads each key before
	// clearing it, so the late commit conflicts instead.
	It("a cleanup clear that lands after the next attempt wrote the heartbeat leaves it in place", func() {
		barrier := newBarrier()
		barrier.holdDispatch = true
		root := specSubspace()
		index := NewIndex("target", Field("price"))
		heartbeat := NewIndexingHeartbeat("self", 30_000, false, nil)
		write := func() {
			_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
				heartbeat.update(rc.Transaction(), root, index)
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		write()
		oi := &OnlineIndexer{db: NewFDBDatabaseWithBackend(barrier), subspace: root, targetIndexes: []*Index{index}}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		err := oi.cleanupHeartbeatWithin(ctx, heartbeat)
		Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue(), "%v", err)
		Expect(barrier.held).NotTo(BeNil(), "the cleanup must have dispatched its clear")
		// The next attempt of the same indexer renews the same key.
		write()
		lateErr := barrier.landLate()
		var conflict fdb.Error
		Expect(errors.As(lateErr, &conflict)).To(BeTrue(), "the late clear committed: %v", lateErr)
		// A conflict (1020), or, when the held commit lands more than the MVCC
		// window after its read version, transaction_too_old (1007): both leave
		// the key alone, which is the property.
		Expect(conflict.Code).To(BeElementOf(1020, 1007))
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			value, err := rc.Transaction().Get(heartbeat.heartbeatKey(root, index)).Get()
			Expect(err).NotTo(HaveOccurred())
			Expect(value).NotTo(BeEmpty(), "the live heartbeat was erased")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("retries a real commit conflict within the remaining budget and clears only owned keys", func() {
		barrier := newBarrier()
		root := specSubspace()
		targets := []*Index{NewIndex("a", Field("price")), NewIndex("b", Field("quantity"))}
		self := NewIndexingHeartbeat("self", 30_000, false, nil)
		peer := NewIndexingHeartbeat("peer", 30_000, false, nil)
		_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			for _, index := range targets {
				self.update(rc.Transaction(), root, index)
				peer.update(rc.Transaction(), root, index)
				rc.Transaction().Set(heartbeatSubspace(root, index).Pack(tuple.Tuple{"legacy"}), []byte("preserve"))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		attempts := 0
		conflictKey := root.Pack(tuple.Tuple{"conflict"})
		barrier.beforeCommit = func(tx fdb.WritableTransaction) {
			attempts++
			if attempts != 1 {
				return
			}
			_, err := tx.Get(conflictKey).Get()
			Expect(err).NotTo(HaveOccurred())
			_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
				rc.Transaction().Set(conflictKey, []byte("competing commit"))
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		oi := &OnlineIndexer{db: NewFDBDatabaseWithBackend(barrier), subspace: root, targetIndexes: targets}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		Expect(oi.cleanupHeartbeatWithin(ctx, self)).To(Succeed())
		Expect(attempts).To(Equal(2), "the conflicting transaction must not commit its clears")
		Expect(barrier.timeouts).To(HaveLen(2))
		Expect(barrier.timeouts[0]).To(BeNumerically("<=", 5000))
		Expect(barrier.timeouts[1]).To(BeNumerically(">", 0))
		Expect(barrier.timeouts[1]).To(BeNumerically("<=", barrier.timeouts[0]))
		Expect(barrier.txCancelled.Load()).To(BeTrue())
		_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			for _, index := range targets {
				value, err := rc.Transaction().Get(self.heartbeatKey(root, index)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(value).To(BeEmpty())
				value, err = rc.Transaction().Get(peer.heartbeatKey(root, index)).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(value).NotTo(BeEmpty())
				value, err = rc.Transaction().Get(heartbeatSubspace(root, index).Pack(tuple.Tuple{"legacy"})).Get()
				Expect(err).NotTo(HaveOccurred())
				Expect(value).To(Equal([]byte("preserve")))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("uses an independent cleanup budget and preserves the cancelled build result", func() {
		barrier := newBarrier()
		defer barrier.release()
		builder := baseBuilder()
		index := NewIndex("target", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		oi, err := NewOnlineIndexerBuilder().SetDatabase(NewFDBDatabaseWithBackend(barrier)).SetMetaData(md).SetSubspace(specSubspace()).SetIndex(index).Build()
		Expect(err).NotTo(HaveOccurred())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		done := make(chan error, 1)
		go func() { _, err := oi.BuildIndex(ctx); done <- err }()
		Eventually(barrier.entered, 5*time.Second).Should(BeClosed())
		Expect(barrier.timeout.Load()).To(BeNumerically(">", 20_000))
		Expect(barrier.timeout.Load()).To(BeNumerically("<=", 30_000))
		barrier.release()
		Eventually(done, 5*time.Second).Should(Receive(&err))
		Expect(errors.Is(err, context.Canceled)).To(BeTrue(), "%v", err)
		Expect(barrier.txCancelled.Load()).To(BeTrue())
		Expect(barrier.gets.Load()).To(Equal(int32(1)), "collect only the ready commit")
	})
})

// mutualCommitBarrierTransactor makes two real transaction bodies overlap before
// either can commit. Retries are counted, not hidden behind eventual success.
type mutualCommitBarrierTransactor struct {
	fdb.Transactor
	ctx      context.Context
	ready    chan<- struct{}
	release  <-chan struct{}
	attempts atomic.Int32
}

func (b *mutualCommitBarrierTransactor) Transact(fn func(fdb.WritableTransaction) (any, error)) (any, error) {
	return b.Transactor.Transact(func(tx fdb.WritableTransaction) (any, error) {
		attempt := b.attempts.Add(1)
		value, err := fn(tx)
		if err != nil || attempt != 1 {
			return value, err
		}
		b.ready <- struct{}{}
		select {
		case <-b.release:
			return value, nil
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		}
	})
}

var _ = Describe("Mutual heartbeat renewal concurrency", func() {
	It("commits overlapping disjoint fragment batches without heartbeat-induced retries", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		root := specSubspace()
		builder := baseBuilder()
		index := NewIndex("target", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			for _, id := range []int64{1, 2, 51, 52} {
				if _, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), Price: proto.Int32(int32(id))}); err != nil {
					return nil, err
				}
			}
			return nil, disableIndexes(store, index)
		})
		Expect(err).NotTo(HaveOccurred())
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		var workers sync.WaitGroup
		defer func() { unblock(); cancel(); workers.Wait() }()
		var builders []*mutualIndexBuilder
		var barriers []*mutualCommitBarrierTransactor
		for fragment := 0; fragment < 2; fragment++ {
			oi, err := NewOnlineIndexerBuilder().SetDatabase(sharedDB).SetMetaData(md).SetSubspace(root).SetIndex(index).
				SetMutualIndexing().SetMutualIndexingBoundaries([][]byte{tuple.Tuple{int64(50)}.Pack()}).SetLimit(10).Build()
			Expect(err).NotTo(HaveOccurred())
			oi.sessionHeartbeat = NewIndexingHeartbeat("mutual worker", 30_000, true, nil)
			Expect(oi.markWriteOnly(ctx)).Error().To(Succeed())
			Expect(oi.admittedHeartbeat).To(BeIdenticalTo(oi.sessionHeartbeat))
			defer oi.cleanupPendingQueueHeartbeat(oi.sessionHeartbeat)
			m, err := newMutualIndexBuilder(oi)
			Expect(err).NotTo(HaveOccurred())
			m.fragmentCur, m.fragmentFirst = fragment, fragment
			barrier := &mutualCommitBarrierTransactor{Transactor: sharedDB.db, ctx: ctx, ready: ready, release: release}
			oi.db = NewFDBDatabaseWithTransactor(barrier, sharedDB.db)
			builders = append(builders, m)
			barriers = append(barriers, barrier)
		}
		type outcome struct {
			count int64
			err   error
		}
		done := make(chan outcome, 2)
		for _, m := range builders {
			workers.Add(1)
			go func() {
				defer workers.Done()
				count, _, err := m.buildMutual(ctx)
				done <- outcome{count, err}
			}()
		}
		Eventually(ready, 5*time.Second).Should(Receive())
		Eventually(ready, 5*time.Second).Should(Receive())
		unblock()
		for range 2 {
			var result outcome
			Eventually(done, 5*time.Second).Should(Receive(&result))
			Expect(result.err).NotTo(HaveOccurred())
			Expect(result.count).To(Equal(int64(2)))
		}
		for _, barrier := range barriers {
			Expect(barrier.attempts.Load()).To(Equal(int32(1)), "disjoint fragments must both commit on their first attempt")
		}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			missing, err := NewIndexingRangeSet(root, index).FirstMissingRange(rc.Transaction())
			Expect(err).NotTo(HaveOccurred())
			Expect(missing).To(BeNil())
			store, err := builders[0].indexer.openStore(rc)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.MarkIndexReadable(index.Name)
			Expect(err).NotTo(HaveOccurred())
			cursor := store.ScanIndex(index, TupleRangeAll, nil, ForwardScan())
			defer cursor.Close()
			rows, err := AsList(ctx, cursor)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(4))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not publish admission when a competing incompatible heartbeat aborts preparation", func() {
		ctx := context.Background()
		root := specSubspace()
		builder := baseBuilder()
		index := NewIndex("target", Field("price"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).Create()
			if err != nil {
				return nil, err
			}
			// Preparation must write for the injected concurrent write to conflict it.
			return nil, disableIndexes(store, index)
		})
		Expect(err).NotTo(HaveOccurred())
		transactor := &batchMutationTransactor{Transactor: sharedDB.db, mutate: func() error {
			_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
				rc.Transaction().Set(heartbeatSubspace(root, index).Pack(tuple.Tuple{"legacy"}), []byte("fence first"))
				return nil, nil
			})
			return err
		}}
		oi, err := NewOnlineIndexerBuilder().SetDatabase(NewFDBDatabaseWithTransactor(transactor, sharedDB.db)).SetMetaData(md).SetSubspace(root).SetIndex(index).SetMutualIndexing().Build()
		Expect(err).NotTo(HaveOccurred())
		oi.sessionHeartbeat = NewIndexingHeartbeat("unadmitted", 30_000, true, nil)
		var incompatible *IndexingHeartbeatKeyError
		_, err = oi.markWriteOnly(ctx)
		Expect(errors.As(err, &incompatible)).To(BeTrue())
		Expect(transactor.attempts).To(Equal(2), "first attempt conflicts; retry sees incompatible ownership")
		Expect(oi.admittedHeartbeat).To(BeNil())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store, err := oi.openStore(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(errors.As(oi.renewSessionHeartbeat(store, index, oi.sessionHeartbeat), &incompatible)).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
