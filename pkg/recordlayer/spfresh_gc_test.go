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
		worked, err := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "recovery-owner", 7)
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
