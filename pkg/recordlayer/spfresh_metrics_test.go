package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// SPFresh StoreTimer instrumentation: the operator-facing counters must move
// when the paths they meter run — search decomposition through a
// timer-carrying context, insert events through SaveRecord, and per-kind
// maintenance actions through the sweeper's Timer option.
var _ = Describe("SPFresh StoreTimer instrumentation", func() {
	ctx := context.Background()

	It("records search, insert, and maintenance events", func() {
		tenant, sub := newSweeperTenant("spf_metrics", 12, true)

		timer := NewStoreTimer()

		// INSERTS through the production write path with the timer on the
		// context: insert timings, fence reads, and replica counts move.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.SetTimer(timer)
			store, serr := tenant.StoreBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			for i := 100; i < 110; i++ {
				if _, serr := store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(int64(i)),
					Price:   proto.Int32(int32(i % 5)), Quantity: proto.Int32(int32(i % 3)),
				}); serr != nil {
					return nil, serr
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(timer.GetCount(EventSPFreshInsert)).To(Equal(int64(10)))
		Expect(timer.GetCount(CountSPFreshInsertFenceReads)).To(BeNumerically(">=", 10),
			"every insert REAL-reads at least its nearest candidate")
		Expect(timer.GetCount(CountSPFreshInsertReplicas)).To(BeNumerically(">=", 10))
		Expect(timer.GetTimeNanos(EventSPFreshInsert)).To(BeNumerically(">", 0))

		// SEARCH through the maintainer with the timer on the context: the
		// search event and the probe/scan/rerank decomposition move.
		type sbd interface {
			ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.SetTimer(timer)
			store, serr := tenant.StoreBuilder(rtx)
			if serr != nil {
				return nil, serr
			}
			idx := store.GetMetaData().GetIndex(tenant.IndexName)
			maintainer, merr := store.GetIndexMaintainer(idx)
			if merr != nil {
				return nil, merr
			}
			cursor := maintainer.(sbd).ScanByDistance(TupleRange{
				Low:  tuple.Tuple{SerializeVector([]float64{1, 1})},
				High: tuple.Tuple{int64(3)},
			}, nil, ScanProperties{})
			for {
				res, cerr := cursor.OnNext(ctx)
				if cerr != nil {
					return nil, cerr
				}
				if !res.HasNext() {
					break
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(timer.GetCount(EventSPFreshSearch)).To(Equal(int64(1)))
		Expect(timer.GetCount(CountSPFreshPostingsProbed)).To(BeNumerically(">", 0))
		Expect(timer.GetCount(CountSPFreshEntriesScanned)).To(BeNumerically(">", 0))
		Expect(timer.GetCount(CountSPFreshRerankReads)).To(BeNumerically(">", 0))

		// MAINTENANCE through the sweeper's Timer option: balloon a posting
		// so a real split cascade runs, then assert per-kind action counters
		// agree with the sweep result.
		mtimer := NewStoreTimer()
		balloonSweeperTenant(sub, 80)
		var total int
		for pass := 0; pass < 50; pass++ {
			res, serr := SweepSPFreshIndexes(ctx, sharedDB, []SPFreshTenant{tenant},
				SPFreshSweepOptions{Timer: mtimer})
			Expect(serr).NotTo(HaveOccurred())
			total += res.Actions
			if res.Worked == 0 {
				break
			}
		}
		Expect(total).To(BeNumerically(">", 0))
		perKind := mtimer.GetCount(CountSPFreshSplits) + mtimer.GetCount(CountSPFreshMerges) +
			mtimer.GetCount(CountSPFreshCSplits) + mtimer.GetCount(CountSPFreshNPAs) +
			mtimer.GetCount(CountSPFreshZombieCleans) + mtimer.GetCount(CountSPFreshCSplitDefers)
		Expect(perKind).To(Equal(int64(total)),
			"per-kind counters must decompose exactly the sweep's action count")
	})

	// Per-kind action counters must count ACTIONS — a
	// merge-task cleanup clear is a zombie clean, not a merge; a csplit
	// pause-window defer-bump is a deferral, not a coarse split.
	It("attributes cleanup clears and defer-bumps to their own counters", func() {
		config := DefaultSPFreshConfig(2)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-attr").Sub("kinds"), 1)
		timer := NewStoreTimer()

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			// A merge task whose centroid exists nowhere: the handler clears
			// it — a cleanup write, not a merge.
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskMerge, 77)
			Expect(terr).NotTo(HaveOccurred())
			// A csplit task on a cell holding a SEALED row: the handler bumps
			// the defer count — a deferral, not a coarse split.
			spfreshSaveCentroid(tx, storage, 1, 10, encodeCentroidRowRaw(spfreshStateSealed, 0, 0, 0, vectorcodec.Serialize([]float64{1, 0})))
			spfreshSaveCentroid(tx, storage, 1, 11, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{2, 0}))
			_, terr = spfreshTaskSetIfAbsent(tx, storage, spfreshTaskCSplit, 1)
			Expect(terr).NotTo(HaveOccurred())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "attr-test", 7, 0, timer)
		Expect(rerr).NotTo(HaveOccurred())
		Expect(worked).To(Equal(2), "both writes consume budget")
		Expect(timer.GetCount(CountSPFreshMerges)).To(BeZero(), "a cleanup clear is not a merge")
		Expect(timer.GetCount(CountSPFreshCSplits)).To(BeZero(), "a defer-bump is not a coarse split")
		Expect(timer.GetCount(CountSPFreshZombieCleans)).To(Equal(int64(1)))
		Expect(timer.GetCount(CountSPFreshCSplitDefers)).To(Equal(int64(1)))
	})
})
