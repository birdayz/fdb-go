package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

var _ = Describe("SPFresh GC + lease recovery", func() {
	ctx := context.Background()

	testConfig := func() SPFreshConfig {
		c := DefaultSPFreshConfig(2)
		c.Lmax = 32
		c.CellTarget = 4
		c.CellMax = 8
		return c
	}

	buildTwoClusters := func(sub string) (*spfreshStorage, []spfreshBuildInput) {
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-gc").Sub(sub), 1)
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i%2) * 0.1, float64(i%3) * 0.1}})
			id++
		}
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{50 + float64(i%2)*0.1, float64(i%3) * 0.1}})
			id++
		}
		builder := newSPFreshBuilder(sharedDB, storage, testConfig(), "builder-1")
		Expect(builder.build(ctx, inputs, 42)).To(Succeed())
		return storage, inputs
	}

	splitPosting := func(storage *spfreshStorage, memberPK tuple.Tuple) (cellID, fineID int64, out spfreshSealOutcome) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, memberPK)
			Expect(merr).NotTo(HaveOccurred())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			Expect(ferr).NotTo(HaveOccurred())
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		var serr error
		out, serr = spfreshSealFine(ctx, sharedDB, storage, "gc-test", cellID, fineID)
		Expect(serr).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, testConfig(), "gc-test", cellID, fineID, 7)).To(Succeed())
		return cellID, fineID, out
	}

	It("purges a forwarded posting past the horizon; claimed residuals are re-homed, never blind-cleared", func() {
		storage, inputs := buildTwoClusters("purge")
		config := testConfig()
		cellID, fineID, _ := splitPosting(storage, tuple.Tuple{int64(1)})

		// A LIVE residual in the retired posting: membership claims it (the
		// §6 drain rule's protected case — must be re-homed, not cleared).
		residual := tuple.Tuple{int64(999)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			quantizer := newSPFreshQuantizer(config)
			tx.Set(storage.postingKey(fineID, residual), quantizer.Encode([]float64{0.1, 0.1}))
			tx.Set(storage.membershipKey(residual), encodeMembership([]int64{fineID}))
			tx.Set(storage.sidecarKey(residual), vectorcodec.SerializeHalf([]float64{0.2, 0.1}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		purged, err := spfreshGCSweep(ctx, sharedDB, storage, config, 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(purged).To(BeNumerically(">", 0))

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// The parent posting, HDR, centroid row and counter are gone.
			entries, perr := spfreshLoadPostingForSplit(tx, storage, fineID)
			Expect(perr).NotTo(HaveOccurred())
			Expect(entries).To(BeEmpty())
			hdr, herr := tx.Get(storage.postingHDRKey(fineID)).Get()
			Expect(herr).NotTo(HaveOccurred())
			Expect(hdr).To(BeNil(), "posting HDR purged")
			_, cerr := spfreshReadCentroidForWrite(tx, storage, cellID, fineID)
			Expect(cerr).To(MatchError(errSPFreshNotFound), "FORWARD centroid row purged")
			// The claimed residual was RE-HOMED: membership names an ACTIVE
			// sibling and the posting entry exists there.
			mem, merr := spfreshReadMembership(tx, storage, residual)
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).NotTo(BeEmpty(), "live residual blind-cleared by GC")
			Expect(mem).NotTo(ContainElement(fineID))
			for _, id := range mem {
				data, gerr := tx.Get(storage.postingKey(id, residual)).Get()
				Expect(gerr).NotTo(HaveOccurred())
				Expect(data).NotTo(BeNil())
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Queries survive the purge: the trim forces stale cursors to reload.
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			searcher := newSPFreshSearcher(storage, config, cache)
			got, serr := searcher.search(tx, inputs[0].vec, len(inputs))
			Expect(serr).NotTo(HaveOccurred())
			Expect(len(got)).To(BeNumerically(">=", len(inputs)), "records lost across GC")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a young FORWARD row survives a horizoned sweep", func() {
		storage, _ := buildTwoClusters("young")
		config := testConfig()
		cellID, fineID, _ := splitPosting(storage, tuple.Tuple{int64(1)})

		// Horizon = 1h: the freshly forwarded row must NOT be purged.
		_, err := spfreshGCSweep(ctx, sharedDB, storage, config, 3_600_000)
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			cent, cerr := spfreshReadCentroidForWrite(rtx.Transaction(), storage, cellID, fineID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateForward), "young FORWARD row purged before its horizon")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("an expired foreign lease is reclaimed and the split completes (crash recovery)", func() {
		storage, _ := buildTwoClusters("lease")
		config := testConfig()

		var cellID, fineID int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			Expect(ferr).NotTo(HaveOccurred())
			// A dead owner claimed the task and SEALED the centroid, then
			// crashed: lease deadline in the past, children minted.
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := spfreshSealFine(ctx, sharedDB, storage, "dead-owner", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			data, gerr := tx.Get(storage.taskKey(spfreshTaskSplit, fineID)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			row, derr := decodeTaskRow(data)
			Expect(derr).NotTo(HaveOccurred())
			row.leaseDeadlineMs = spfreshNowMs() - 60_000 // crashed: lease expired
			tx.Set(storage.taskKey(spfreshTaskSplit, fineID), encodeTaskRow(row))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A different owner's rebalancer reclaims the expired lease and
		// completes the lifecycle (SEAL resumes with the SAME children).
		worked, err := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "recovery-owner", 7, 0, nil)
		Expect(err).NotTo(HaveOccurred())
		Expect(worked).To(BeNumerically(">", 0))
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			cent, cerr := spfreshReadCentroidForWrite(rtx.Transaction(), storage, cellID, fineID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateForward), "expired lease must be reclaimed and the split completed")
			Expect(cent.childA).To(Equal(out.childA), "recovery must resume with the dead owner's minted children")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Torvalds 094.3 NAK regressions.
var _ = Describe("SPFresh GC horizon + tombstone discovery (Torvalds 094.3)", func() {
	ctx := context.Background()

	testConfig := func() SPFreshConfig {
		c := DefaultSPFreshConfig(2)
		c.Lmax = 32
		c.CellTarget = 4
		c.CellMax = 8
		return c
	}

	It("#1: the changelog trim actually clears entries and stale cursors are forced to reload", func() {
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-gc").Sub("trim"), 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			return nil, spfreshAppendDeltas(tx, storage, []spfreshDelta{{op: spfreshOpAddCell, ids: []int64{1}}})
		})
		Expect(err).NotTo(HaveOccurred())

		// A cache anchors its cursor at the current tail...
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, cache.fullReload(rtx.Transaction(), storage, 1)
		})
		Expect(err).NotTo(HaveOccurred())
		// ...then more history lands behind its back...
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, spfreshAppendDeltas(rtx.Transaction(), storage, []spfreshDelta{{op: spfreshOpAddCell, ids: []int64{2}}})
		})
		Expect(err).NotTo(HaveOccurred())
		// ...and the trim erases it (horizon 0 = everything below now).
		_, err = spfreshGCSweep(ctx, sharedDB, storage, testConfig(), 0)
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// The trim ACTUALLY cleared (the pre-fix boundary lacked the
			// tuple versionstamp type byte, sorted below every real key, and
			// cleared nothing — silently unbounded changelog growth).
			deltas, _, derr := spfreshReadDeltasSince(tx, storage, nil, 100)
			Expect(derr).NotTo(HaveOccurred())
			Expect(deltas).To(BeEmpty(), "trim cleared nothing: boundary key encoding is wrong")
			// A stale cursor MUST escalate to a full reload.
			rerr := cache.refresh(tx, storage, 1)
			Expect(rerr).To(MatchError(errSPFreshNotFound), "stale cursor predates the trim and must force a reload")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// ...and the reload must CONVERGE: after an empty-log trim, the
		// reloaded cursor floors at the horizon — a bare-prefix anchor would
		// leave cursor < horizon and force ANOTHER reload every interval
		// until a new delta lands (codex 094.3 r2).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, storage, 1)).To(Succeed())
			Expect(cache.refresh(tx, storage, 1)).To(Succeed(), "post-trim reload must not loop on the horizon")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("a merge finds in-cell targets even when the global neighborhood lives elsewhere (codex 094.3 r2)", func() {
		config := testConfig()
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-gc").Sub("mergecell"), 1)
		member := tuple.Tuple{int64(7)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			// Cell 1: the under-Lmin centroid at (0,0) and ONE far in-cell
			// sibling at (30,0). Cell 2: Kn near-identical centroids right
			// next to (0,0) — the GLOBAL top-K is entirely cell 2, so the
			// pre-fix route-then-filter found no in-cell target and skipped.
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{15, 0}))
			spfreshSaveCoarse(tx, storage, 2, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{2, 0}))
			spfreshSaveCentroid(tx, storage, 1, 10, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, storage, 1, 11, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{30, 0}))
			for i := int64(0); i < int64(config.Kn); i++ {
				spfreshSaveCentroid(tx, storage, 2, 20+i, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{1 + 0.1*float64(i), 0}))
			}
			// One member in the under-Lmin posting.
			quantizer := newSPFreshQuantizer(config)
			tx.Set(storage.postingKey(10, member), quantizer.Encode([]float64{0.1, 0}))
			tx.Set(storage.membershipKey(member), encodeMembership([]int64{10}))
			tx.Set(storage.sidecarKey(member), vectorcodec.SerializeHalf([]float64{0.1, 0}))
			spfreshCounterSet(tx, storage, spfreshCounterFine, 10, 1)
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskMerge, 10)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(errOnly(spfreshMergeFine(ctx, sharedDB, storage, config, "merge-cell", 1, 10))).To(Succeed())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cent, cerr := spfreshReadCentroidForWrite(tx, storage, 1, 10)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(cent.state).To(Equal(spfreshStateForward), "merge must drain via the in-cell sibling, not skip because the global top-K lives in another cell")
			Expect(cent.childA).To(Equal(int64(11)))
			mem, merr := spfreshReadMembership(tx, storage, member)
			Expect(merr).NotTo(HaveOccurred())
			Expect(mem).To(Equal([]int64{11}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("#2: a tombstone rides the coarse split so GC can still find and purge it", func() {
		config := testConfig()
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-gc").Sub("ride"), 1)
		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i%2) * 0.1, float64(i%3) * 0.1}})
			id++
		}
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{50 + float64(i%2)*0.1, float64(i%3) * 0.1}})
			id++
		}
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, inputs, 42)).To(Succeed())

		// Split a posting: its parent becomes a FORWARD tombstone in cell C.
		var cellID, fineID int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			fineID = mem[0]
			var ferr error
			cellID, ferr = spfreshFindCentroidCell(tx, storage, fineID)
			Expect(ferr).NotTo(HaveOccurred())
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, fineID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		out, err := spfreshSealFine(ctx, sharedDB, storage, "gc-test", cellID, fineID)
		Expect(err).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeTrue())
		Expect(spfreshSplitFine(ctx, sharedDB, storage, config, "gc-test", cellID, fineID, 7)).To(Succeed())

		// Coarse-split the tombstone's cell.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskCSplit, cellID)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(errOnly(spfreshCoarseSplit(ctx, sharedDB, storage, config, "gc-test", cellID, 7))).To(Succeed())

		// The tombstone moved with the partition (pre-fix it was dropped:
		// GC's scan could never find it; its posting HDR leaked forever).
		var newCell int64
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			coarse, cerr := spfreshReadCoarseForWrite(tx, storage, cellID)
			Expect(cerr).NotTo(HaveOccurred())
			Expect(coarse.state).To(Equal(spfreshStateForward))
			for _, child := range []int64{coarse.childA, coarse.childB} {
				if _, rerr := spfreshReadCentroidForWrite(tx, storage, child, fineID); rerr == nil {
					newCell = child
				}
			}
			Expect(newCell).NotTo(BeZero(), "FORWARD tombstone dropped by the coarse split — GC discovery broken")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// And GC purges it through its new location.
		_, err = spfreshGCSweep(ctx, sharedDB, storage, config, 0)
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			_, cerr := spfreshReadCentroidForWrite(tx, storage, newCell, fineID)
			Expect(cerr).To(MatchError(errSPFreshNotFound), "ridden tombstone purged")
			hdr, herr := tx.Get(storage.postingHDRKey(fineID)).Get()
			Expect(herr).NotTo(HaveOccurred())
			Expect(hdr).To(BeNil(), "posting HDR purged through the moved tombstone")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
