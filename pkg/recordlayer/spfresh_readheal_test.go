package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// Read-path envelope repair (spfresh-reviewer freeze re-review, P2): the
// query path's 4×Lmax+1 posting fetch cap equals the split-dispatch envelope,
// so a capped read is PROOF a split trigger was lost — the posting's tail is
// live-but-unfindable (the exact shape of the master churn flake). The search
// must count the cap-hit and re-file the split task so the envelope heals
// from reads instead of trusting every lifecycle forever.

// balloonWithoutTrigger injects entries into pk 1's posting like
// balloonSweeperTenant but WITHOUT filing the split task — the lost-trigger
// damage state. Returns the target pk whose entry sorts past the fetch cap
// and its vector.
func balloonWithoutTrigger(sub subspace.Subspace, entries int) (tuple.Tuple, []float64, int64, int64) {
	ctx := context.Background()
	storage := newSPFreshStorage(sub, 1)
	config := DefaultSPFreshConfig(2)
	config.Lmax = 16
	quantizer := newSPFreshQuantizer(config)
	var fine, cell int64
	var targetPK tuple.Tuple
	var targetVec []float64
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		tx := rtx.Transaction()
		mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
		Expect(merr).NotTo(HaveOccurred())
		fine = mem[0]
		var ferr error
		cell, ferr = spfreshFindCentroidCell(tx, storage, fine)
		Expect(ferr).NotTo(HaveOccurred())
		row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, fine)
		Expect(rerr).NotTo(HaveOccurred())
		cvec, verr := row.vector()
		Expect(verr).NotTo(HaveOccurred())
		for i := 0; i < entries; i++ {
			pk := tuple.Tuple{int64(50000 + i)}
			v := []float64{float64(i%40) * 0.3, float64(i%37) * 0.3}
			tx.Set(storage.postingKey(fine, pk), quantizer.Encode([]float64{v[0] - cvec[0], v[1] - cvec[1]}))
			tx.Set(storage.membershipKey(pk), encodeMembership([]int64{fine}))
			tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			spfreshCounterAdd(tx, storage, spfreshCounterFine, fine, 1)
			if i == entries-1 {
				targetPK = pk
				targetVec = v
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return targetPK, targetVec, fine, cell
}

func readHealSearch(tenant SPFreshTenant, timer *StoreTimer, query []float64, k int) []int64 {
	ctx := context.Background()
	type sbd interface {
		ScanByDistance(TupleRange, []byte, ScanProperties) RecordCursor[*IndexEntry]
	}
	var ids []int64
	_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
		ids = ids[:0]
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
			Low:  tuple.Tuple{SerializeVector(query)},
			High: tuple.Tuple{int64(k)},
		}, nil, ScanProperties{})
		for {
			res, cerr := cursor.OnNext(ctx)
			if cerr != nil {
				return nil, cerr
			}
			if !res.HasNext() {
				break
			}
			ids = append(ids, res.GetValue().Key[0].(int64))
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return ids
}

var _ = Describe("SPFresh read-path envelope repair", func() {
	ctx := context.Background()

	It("re-files the lost split task on a capped read and the drain restores findability", func() {
		tenant, sub := newSweeperTenant("spf_readheal", 8, true)
		// Damage: 84 entries past the 4×Lmax=64 envelope, NO split trigger.
		targetPK, targetVec, fine, _ := balloonWithoutTrigger(sub, 84)
		storage := newSPFreshStorage(sub, 1)

		// Search 1: the target's entry sorts past the 65-row cap — the damaged
		// state makes it live-but-unfindable, the cap-hit is counted, and the
		// split task is re-filed from the read path.
		timer := NewStoreTimer()
		ids := readHealSearch(tenant, timer, targetVec, 3)
		Expect(ids).NotTo(ContainElement(targetPK[0].(int64)),
			"the damaged posting's tail must be invisible to the capped read — if this finds the target, the damage setup no longer balloons past the cap")
		Expect(timer.GetCount(CountSPFreshCappedPostingReads)).To(BeNumerically(">=", 1))
		Expect(timer.GetCount(CountSPFreshReadPathSplitFiles)).To(Equal(int64(1)))
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskSplit, fine)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).NotTo(BeNil(), "the capped read must re-file the split task")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A second search takes ZERO additional filing action (the snapshot
		// gate sees the pending task — queries stay conflict-free while the
		// heal is in flight).
		timer2 := NewStoreTimer()
		_ = readHealSearch(tenant, timer2, targetVec, 3)
		Expect(timer2.GetCount(CountSPFreshCappedPostingReads)).To(BeNumerically(">=", 1))
		Expect(timer2.GetCount(CountSPFreshReadPathSplitFiles)).To(Equal(int64(0)))

		// Drain: the re-filed task splits the posting (chunked — it is past
		// the envelope), after which the target is findable at its own vector.
		_, err = RebalanceSPFreshIndex(ctx, sharedDB, tenant.StoreBuilder, tenant.IndexName)
		Expect(err).NotTo(HaveOccurred())
		timer3 := NewStoreTimer()
		ids = readHealSearch(tenant, timer3, targetVec, 3)
		Expect(ids).To(ContainElement(targetPK[0].(int64)),
			"after the read-path-filed split drains, the record must be findable at its own vector")
		Expect(timer3.GetCount(CountSPFreshCappedPostingReads)).To(Equal(int64(0)),
			"post-drain postings are within the envelope — no capped reads")
	})

	// codex final-gauntlet P1: the chunked drain spans many transactions, so
	// the parent's HDR forward marker must be published WITH the children
	// (step 1) — not at finalize — or a reader routed by a stale cache to the
	// SEALED parent has no redirect and every already-moved entry is
	// invisible until a cache refresh.
	It("chunked planner publishes the parent HDR before any entry can move", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-split").Sub("planhdr"), 1)
		quantizer := newSPFreshQuantizer(config)

		const parent, childA, childB = int64(10), int64(11), int64(12)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{5, 5}))
			spfreshSaveCentroid(tx, storage, 1, parent, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{0, 0}))
			for i := 0; i < 4*config.Lmax+20; i++ {
				pk := tuple.Tuple{int64(70000 + i)}
				v := []float64{float64(i%9) * 0.4, float64(i%7) * 0.4}
				tx.Set(storage.postingKey(parent, pk), quantizer.Encode(v))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{parent}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, parent, int64(4*config.Lmax+20))
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, 1)
			tx.Set(storage.taskKey(spfreshTaskSplit, parent),
				encodeTaskRow(spfreshTaskRow{state: spfreshSplitTaskSealed, childA: childA, childB: childB}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		_, _, err = spfreshChunkedSplitPlan(ctx, sharedDB, storage, config, "planhdr-owner", 1, parent, childA, childB, 7)
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			hdr, gerr := tx.Get(storage.postingHDRKey(parent)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(hdr).NotTo(BeNil(), "the planner must publish the parent HDR with the children — before any entry moves")
			cell, hA, hB, derr := decodePostingHDR(hdr)
			Expect(derr).NotTo(HaveOccurred())
			Expect([]int64{cell, hA, hB}).To(Equal([]int64{1, childA, childB}))
			parentRow, prErr := spfreshReadCentroidForWrite(tx, storage, 1, parent)
			Expect(prErr).NotTo(HaveOccurred())
			Expect(parentRow.state).To(Equal(spfreshStateSealed), "the parent stays SEALED and readable through the drain")
			for _, id := range []int64{childA, childB} {
				row, rErr := spfreshReadCentroidForWrite(tx, storage, 1, id)
				Expect(rErr).NotTo(HaveOccurred())
				Expect(row.state).To(Equal(spfreshStateActive))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// The searcher half of the same contract: a stale routing cache (loaded
	// before the children existed, refresh suppressed) probes only the SEALED
	// parent — moved entries must still surface via the HDR follow, and the
	// parent's residual entries must score in the same burst.
	It("mid-drain reads with a stale routing cache see moved and residual entries", func() {
		tenant, sub := newSweeperTenant("spf_middrain", 8, true)
		ctxBg := context.Background()

		var gen int64
		_, err := sharedDB.Run(ctxBg, func(rtx *FDBRecordContext) (any, error) {
			g, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(sub, 0))
			gen = g
			return nil, gerr
		})
		Expect(err).NotTo(HaveOccurred())
		storage := newSPFreshStorage(sub, gen)
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		quantizer := newSPFreshQuantizer(config)

		// Load the routing cache with the PRE-DRAIN topology. pk1 and pk7
		// both sit at vector (0,0).
		timer0 := NewStoreTimer()
		ids := readHealSearch(tenant, timer0, []float64{0, 0}, 4)
		Expect(ids).To(ContainElement(int64(1)))

		// Fabricate the mid-drain state: children ACTIVE, pk1's entry moved
		// to child D, HDR on the parent, parent SEALED. pk7 stays put.
		const childD, childE = int64(910), int64(911)
		centD := []float64{0.5, 0.5}
		var parentFine int64
		_, err = sharedDB.Run(ctxBg, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			parentFine = mem[0]
			cell, ferr := spfreshFindCentroidCell(tx, storage, parentFine)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, parentFine)
			Expect(rerr).NotTo(HaveOccurred())
			spfreshSaveCentroid(tx, storage, cell, childD, encodeCentroidRow(spfreshStateActive, 0, 0, 0, centD))
			spfreshSaveCentroid(tx, storage, cell, childE, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{5, 5}))
			spfreshCounterSet(tx, storage, spfreshCounterFine, childD, 1)
			spfreshCounterSet(tx, storage, spfreshCounterFine, childE, 0)
			// Move pk1: posting entry re-encoded against child D's center,
			// membership rewritten, parent entry cleared.
			v := []float64{0, 0}
			tx.Set(storage.postingKey(childD, tuple.Tuple{int64(1)}), quantizer.Encode([]float64{v[0] - centD[0], v[1] - centD[1]}))
			tx.Clear(storage.postingKey(parentFine, tuple.Tuple{int64(1)}))
			newMem := append([]int64(nil), mem...)
			for j, id := range newMem {
				if id == parentFine {
					newMem[j] = childD
				}
			}
			tx.Set(storage.membershipKey(tuple.Tuple{int64(1)}), encodeMembership(newMem))
			spfreshCounterAdd(tx, storage, spfreshCounterFine, parentFine, -1)
			tx.Set(storage.postingHDRKey(parentFine), encodePostingHDR(cell, childD, childE))
			spfreshSaveCentroid(tx, storage, cell, parentFine, encodeCentroidRowRaw(spfreshStateSealed, row.epoch, 0, 0, row.vecBytes))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Suppress the amortized refresh so the cache stays on the pre-drain
		// view (deterministically inside the interval), then search: pk1 is
		// reachable only through the parent's HDR; pk7 only through the
		// parent's residual entries.
		spfreshCacheFor(sub, gen).lastRefreshMs.Store(spfreshNowMs())
		timer := NewStoreTimer()
		ids = readHealSearch(tenant, timer, []float64{0, 0}, 4)
		Expect(ids).To(ContainElement(int64(1)), "moved entry must surface via the parent HDR follow on a stale cache")
		Expect(ids).To(ContainElement(int64(7)), "the SEALED parent's residual entries must score in the same burst")
		Expect(timer.GetCount(CountSPFreshForwardFollows)).To(BeNumerically(">=", 1),
			"the HDR path must actually have been exercised — if this is 0 the cache refreshed and the test pinned nothing")
	})

	It("honors the csplit pause: a capped read in a pausing cell files nothing", func() {
		tenant, sub := newSweeperTenant("spf_readheal_pause", 8, true)
		_, targetVec, fine, cell := balloonWithoutTrigger(sub, 84)
		storage := newSPFreshStorage(sub, 1)

		// A pausing coarse split owns the cell: fine-split issuance is paused
		// (the csplit move re-files for oversized rows it relocates — the
		// pause-window repair regression pins that half).
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(storage.taskKey(spfreshTaskCSplit, cell),
				encodeTaskRow(spfreshTaskRow{state: spfreshCSplitPausing}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		timer := NewStoreTimer()
		_ = readHealSearch(tenant, timer, targetVec, 3)
		Expect(timer.GetCount(CountSPFreshCappedPostingReads)).To(BeNumerically(">=", 1))
		Expect(timer.GetCount(CountSPFreshReadPathSplitFiles)).To(Equal(int64(0)),
			"the read path must not re-introduce the fine-split/csplit starvation the pause prevents")
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			data, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskSplit, fine)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).To(BeNil(), "no split task may be filed while the cell's csplit is pausing")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
