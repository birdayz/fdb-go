package recordlayer

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

type partitionAnnotationProbe struct {
	fdb.WritableTransaction
	prefix    []byte
	count     atomic.Int32
	annotated chan struct{}
}

func (tx *partitionAnnotationProbe) Set(key fdb.KeyConvertible, value []byte) {
	tx.WritableTransaction.Set(key, value)
	if bytes.HasPrefix(key.FDBKey(), tx.prefix) && tx.count.Add(1) == 2 {
		close(tx.annotated)
	}
}

func partitionWait(done <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

var _ = Describe("Partition raw annotation boundaries", func() {
	ctx := context.Background()
	It("fences public annotation APIs and preserves exact violation bytes with committable refusals", func() {
		b := partitionTestBuilder()
		idx := NewIndex("annotation_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		typeKey := md.GetRecordType("Order").GetRecordTypeKey()
		pk1, pk2 := tuple.Tuple{typeKey, int64(-2), int64(-11), int64(1)}, tuple.Tuple{typeKey, int64(-2), int64(-11), int64(2)}
		neighbor := tuple.Tuple{typeKey, int64(1), int64(10), int64(3)}
		value := tuple.Tuple{int64(-2), int64(-11), int64(7)}
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.ClearAndMarkIndexWriteOnly(idx.Name)
			Expect(err).NotTo(HaveOccurred())
			for _, record := range []proto.Message{partitionOrder(1, -11, 7), partitionOrder(2, -11, 7), partitionOrder(3, 10, 8)} {
				_, err := store.SaveRecord(record)
				Expect(err).NotTo(HaveOccurred())
			}
			violations, err := store.ScanUniquenessViolationsForValue(idx, value)
			Expect(err).NotTo(HaveOccurred())
			Expect(violations).To(HaveLen(2), "real WRITE_ONLY duplicates are the annotation control")
			for _, pk := range []tuple.Tuple{pk1, pk2} {
				Expect(store.ResolveUniquenessViolation(idx, value, pk)).To(Succeed())
			}
			violation := root.Sub(IndexUniquenessViolationsKey, idx.SubspaceTupleKey()).Sub(value...)
			Expect(store.AddUniquenessViolation(idx, value, pk1)).To(Succeed())
			Expect(rc.Transaction().Get(violation.Pack(pk1)).MustGet()).To(Equal([]byte{}))
			Expect(store.AddUniquenessViolationWithExisting(idx, value, pk2, pk1)).To(Succeed())
			Expect(rc.Transaction().Get(violation.Pack(pk2)).MustGet()).To(Equal(pk1.Pack()))
			Expect(store.ResolveUniquenessViolation(idx, value, pk1)).To(Succeed())
			Expect(rc.Transaction().Get(violation.Pack(pk1)).MustGet()).To(BeNil())
			before, effects := partitionWholeStore(rc, store), partitionVersionEffectsOf(rc)
			for _, rejected := range []error{
				store.AddUniquenessViolation(idx, tuple.Tuple{int64(1)}, pk1),
				store.AddUniquenessViolationWithExisting(idx, value, pk1, neighbor),
				store.ResolveUniquenessViolation(idx, tuple.Tuple{int64(1)}, pk1),
			} {
				var state *PartitionStateError
				Expect(errors.As(rejected, &state)).To(BeTrue())
				Expect(state.IndexName).To(Equal(idx.Name))
			}
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			Expect(partitionVersionEffectsOf(rc)).To(Equal(effects))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			partitionProtocolStore(rc, md, root)
			rc.Transaction().Clear(partitionSubspace(root, md.GetRecordType("Order")).Pack(tuple.Tuple{"fence", int64(-2)}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			before := partitionWholeStore(rc, store)
			for _, rejected := range []error{store.AddUniquenessViolation(idx, value, pk1), store.AddUniquenessViolationWithExisting(idx, value, pk2, pk1), store.ResolveUniquenessViolation(idx, value, pk2)} {
				var state *PartitionStateError
				Expect(errors.As(rejected, &state)).To(BeTrue())
			}
			Expect(partitionWholeStore(rc, store)).To(Equal(before))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("does not reacquire partition preparation under WRITE_ONLY maintenance with a queued state writer", func() {
		b := partitionTestBuilder()
		idx := NewIndex("annotation_lock_unique", Concat(PartitionExpr(Field("coord_x"), 10), Field("coord_x"), Field("price"))).SetUnique()
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			_, err := store.ClearAndMarkIndexWriteOnly(idx.Name)
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(partitionOrder(1, 0, 7))
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(partitionOrder(99, 990, 99))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		rc, err := sharedDB.OpenRecordContext(ctx)
		Expect(err).NotTo(HaveOccurred())
		defer rc.Cancel()
		gate := newPartitionMaintenanceGate(2)
		defer gate.unblock()
		writer, err := NewStoreBuilder().SetContext(rc).SetMetaDataProvider(md).SetSubspace(root).SetIndexMaintenanceFilter(gate).Open()
		Expect(err).NotTo(HaveOccurred())
		bulk, transition := partitionProtocolStore(rc, md, root), partitionProtocolStore(rc, md, root)
		probe := &partitionAnnotationProbe{WritableTransaction: rc.tx, prefix: root.Sub(IndexUniquenessViolationsKey, idx.SubspaceTupleKey()).Bytes(), annotated: make(chan struct{})}
		rc.tx = probe
		writerDone := make(chan struct{})
		var writerErr error
		go func() { defer close(writerDone); _, writerErr = writer.SaveRecord(partitionOrder(2, 0, 7)) }()
		partitionAwait(gate.entered)
		// Hold the actual bulk-exclusion preparation boundary, but let the test
		// release it on failure instead of leaving an intentional mutex cycle hung.
		bulkOp, err := bulk.beginPartitionMutation()
		Expect(err).NotTo(HaveOccurred())
		var releaseOnce sync.Once
		releaseBulk := func() { releaseOnce.Do(func() { bulkOp.finish() }) }
		defer releaseBulk()
		Expect(bulkOp.prepareTableExclusion(md.GetRecordType("Order"))).To(Succeed())
		transitionDone := make(chan struct{})
		var transitionErr error
		var changed bool
		go func() { defer close(transitionDone); changed, transitionErr = transition.MarkIndexDisabled(idx.Name) }()
		defer func() {
			releaseBulk()
			gate.unblock()
			partitionWait(writerDone, 3*time.Second)
			partitionWait(transitionDone, 3*time.Second)
		}()
		Eventually(func() bool {
			if writer.indexStateView.maintenance.TryRLock() {
				writer.indexStateView.maintenance.RUnlock()
				return false
			}
			return true
		}, 3*time.Second).Should(BeTrue(), "state writer must really queue behind maintenance")
		gate.unblock()
		annotated := partitionWait(probe.annotated, 3*time.Second)
		if !annotated {
			releaseBulk()
		}
		transitionCompleted := partitionWait(transitionDone, 3*time.Second)
		releaseBulk()
		writerCompleted := partitionWait(writerDone, 3*time.Second)
		Expect(annotated).To(BeTrue(), "both raw annotation sets must progress while bulk preparation is held")
		Expect(transitionCompleted).To(BeTrue())
		Expect(writerCompleted).To(BeTrue())
		Expect(writerErr).NotTo(HaveOccurred())
		Expect(transitionErr).NotTo(HaveOccurred())
		Expect(changed).To(BeTrue())
		Expect(probe.count.Load()).To(Equal(int32(2)))
		Expect(bulk.ClearPartitionData("Order", 99)).To(Succeed())
		Expect(partitionProtocolRows(rc, root)).To(HaveLen(6))
		Expect(rc.Commit()).To(Succeed())
	})

	It("keeps plain batch writes free of partition sessions and guards", func() {
		b := baseBuilder()
		idx := NewIndex("plain_batch_control", Field("price"))
		b.AddIndex("Order", idx)
		md, err := b.Build()
		Expect(err).NotTo(HaveOccurred())
		root := specSubspace()
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			store := partitionProtocolStore(rc, md, root)
			var records []proto.Message
			for i := 0; i < 32; i++ {
				records = append(records, &demopb.Order{OrderId: proto.Int64(int64(i)), Price: proto.Int32(int32(i))})
			}
			rows, err := store.SaveRecordBatch(records)
			Expect(err).NotTo(HaveOccurred())
			Expect(rows).To(HaveLen(32))
			entries, err := AsList(ctx, store.ScanIndexRecords(idx.Name, TupleRangeAll, nil, ForwardScan()))
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(32))
			Expect(partitionGuardCount(rc)).To(Equal(0))
			rc.sessionMu.Lock()
			partitionSessions := 0
			for key := range rc.session {
				if _, ok := key.(partitionSessionID); ok {
					partitionSessions++
				}
			}
			rc.sessionMu.Unlock()
			Expect(partitionSessions).To(Equal(0))
			Expect(partitionProtocolRows(rc, root)).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
