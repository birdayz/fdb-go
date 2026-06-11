package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
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
			mtimer.GetCount(CountSPFreshZombieCleans)
		Expect(perKind).To(Equal(int64(total)),
			"per-kind counters must decompose exactly the sweep's action count")
	})
})
