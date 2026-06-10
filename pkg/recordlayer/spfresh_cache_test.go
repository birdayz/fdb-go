package recordlayer

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

var _ = Describe("SPFresh routing cache", func() {
	ctx := context.Background()

	// seedTopology writes a 2-cell, 4-fine-centroid topology:
	//   cell 1: fine 10 @ (0,0), fine 11 @ (10,0)
	//   cell 2: fine 20 @ (100,0), fine 21 @ (110,0)  [fine 21 SEALED]
	seedTopology := func(s *spfreshStorage) {
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, s, 1)
			spfreshSaveCoarse(tx, s, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{5, 0}))
			spfreshSaveCoarse(tx, s, 2, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{105, 0}))
			spfreshSaveCentroid(tx, s, 1, 10, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, s, 1, 11, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{10, 0}))
			spfreshSaveCentroid(tx, s, 2, 20, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{100, 0}))
			spfreshSaveCentroid(tx, s, 2, 21, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{110, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	It("full reload + route: nearest ACTIVE fine centroids across probed cells", func() {
		s := newSPFreshStorage(specSubspace().Sub("spfresh-cache").Sub("route"), 1)
		seedTopology(s)
		cache := newSPFreshRoutingCache(0)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, s, 1)).To(Succeed())

			// w=2 probes both cells; SEALED fine 21 must be excluded.
			routed, rerr := cache.route(tx, s, []float64{1, 0}, 2, 10)
			Expect(rerr).NotTo(HaveOccurred())
			ids := []int64{}
			for _, r := range routed {
				ids = append(ids, r.fineID)
			}
			Expect(ids).To(Equal([]int64{10, 11, 20}), "ascending distance, SEALED excluded")
			Expect(routed[0].cellID).To(Equal(int64(1)))
			Expect(routed[0].vec).To(Equal([]float64{0, 0}), "routed vector needed for residuals")

			// w=1 probes only the nearest cell.
			routed, rerr = cache.route(tx, s, []float64{1, 0}, 1, 10)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).To(HaveLen(2))

			// kc caps the result.
			routed, rerr = cache.route(tx, s, []float64{1, 0}, 2, 1)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).To(HaveLen(1))
			Expect(routed[0].fineID).To(Equal(int64(10)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("incremental refresh applies forward deltas; generation flip forces reload", func() {
		s := newSPFreshStorage(specSubspace().Sub("spfresh-cache").Sub("refresh"), 1)
		seedTopology(s)
		cache := newSPFreshRoutingCache(0)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, cache.fullReload(rtx.Transaction(), s, 1)
		})
		Expect(err).NotTo(HaveOccurred())

		// A fine split elsewhere: forwardFine(11 -> 30, 31) + addFine entries.
		// The cache must evict cell 1 so the next route reloads it.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// Simulate the split's centroid writes: 11 FORWARD, children ACTIVE.
			spfreshSaveCentroid(tx, s, 1, 11, encodeCentroidRow(spfreshStateForward, 1, 30, 31, []float64{10, 0}))
			spfreshSaveCentroid(tx, s, 1, 30, encodeCentroidRow(spfreshStateActive, 1, 0, 0, []float64{9, 0}))
			spfreshSaveCentroid(tx, s, 1, 31, encodeCentroidRow(spfreshStateActive, 1, 0, 0, []float64{11, 0}))
			return nil, spfreshAppendDeltas(tx, s, []spfreshDelta{
				{op: spfreshOpForwardFine, ids: []int64{11, 30, 31}},
			})
		})
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.refresh(tx, s, 1)).To(Succeed())
			routed, rerr := cache.route(tx, s, []float64{10, 0}, 1, 10)
			Expect(rerr).NotTo(HaveOccurred())
			ids := []int64{}
			for _, r := range routed {
				ids = append(ids, r.fineID)
			}
			Expect(ids).To(ContainElements(int64(30), int64(31)), "children visible after refresh")
			Expect(ids).NotTo(ContainElement(int64(11)), "forwarded parent no longer routable")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A generation delta forces a full reload signal.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, spfreshAppendDeltas(rtx.Transaction(), s, []spfreshDelta{
				{op: spfreshOpGeneration, ids: []int64{2}},
			})
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			Expect(cache.refresh(rtx.Transaction(), s, 1)).To(MatchError(errSPFreshNotFound))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Mismatched current generation also demands a reload (the caller's
		// horizon/generation check).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			Expect(cache.refresh(rtx.Transaction(), s, 99)).To(MatchError(errSPFreshNotFound))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("coarse-forward is followed one hop during routing", func() {
		s := newSPFreshStorage(specSubspace().Sub("spfresh-cache").Sub("cfwd"), 1)
		// Cell 3 forwarded to cells 4 and 5 (a coarse split); the stale L1
		// still routes to 3 — the cell's HDR redirects.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, s, 1)
			spfreshSaveCoarse(tx, s, 3, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			return nil, cache.fullReload(rtx.Transaction(), s, 1)
		})
		Expect(err).NotTo(HaveOccurred())

		// The coarse split commits AFTER the cache loaded L1: cell 3's centroid
		// range now holds only the HDR; children 4/5 hold the rows.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			tx.Set(s.centroidHDRKey(3), encodeCellHDR(4, 5))
			spfreshSaveCoarse(tx, s, 3, encodeCentroidRow(spfreshStateForward, 0, 4, 5, []float64{0, 0}))
			spfreshSaveCoarse(tx, s, 4, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{-1, 0}))
			spfreshSaveCoarse(tx, s, 5, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{1, 0}))
			spfreshSaveCentroid(tx, s, 4, 40, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{-1, 0}))
			spfreshSaveCentroid(tx, s, 5, 50, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{1, 0}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Route with the STALE cache (no refresh): must follow the HDR.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			routed, rerr := cache.route(rtx.Transaction(), s, []float64{0.5, 0}, 1, 10)
			Expect(rerr).NotTo(HaveOccurred())
			ids := []int64{}
			for _, r := range routed {
				ids = append(ids, r.fineID)
			}
			Expect(ids).To(ConsistOf(int64(40), int64(50)), "stale route follows the cell HDR to the children")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("LRU evicts least-recently-used cells at the budget", func() {
		s := newSPFreshStorage(specSubspace().Sub("spfresh-cache").Sub("lru"), 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, s, 1)
			for cell := int64(1); cell <= 3; cell++ {
				spfreshSaveCoarse(tx, s, cell, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{float64(cell * 10), 0}))
				spfreshSaveCentroid(tx, s, cell, cell*100, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{float64(cell * 10), 0}))
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		cache := newSPFreshRoutingCache(2) // budget: 2 cells
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, s, 1)).To(Succeed())
			_, e := cache.ensureCell(tx, s, 1)
			Expect(e).NotTo(HaveOccurred())
			_, e = cache.ensureCell(tx, s, 2)
			Expect(e).NotTo(HaveOccurred())
			_, e = cache.ensureCell(tx, s, 1) // touch 1 (now MRU)
			Expect(e).NotTo(HaveOccurred())
			_, e = cache.ensureCell(tx, s, 3) // evicts 2 (LRU)
			Expect(e).NotTo(HaveOccurred())

			cache.mu.RLock()
			_, has1 := cache.cells[1]
			_, has2 := cache.cells[2]
			_, has3 := cache.cells[3]
			cache.mu.RUnlock()
			Expect(has1).To(BeTrue())
			Expect(has2).To(BeFalse(), "LRU cell evicted at budget")
			Expect(has3).To(BeTrue())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// Routing to pk of evicted cell 2 reloads it transparently (a miss,
		// one range read).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			routed, rerr := cache.route(rtx.Transaction(), s, []float64{20, 0}, 1, 10)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).To(HaveLen(1))
			Expect(routed[0].fineID).To(Equal(int64(200)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("routes deterministically with tuple pks in adjacent subspaces untouched", func() {
		// Guard against subspace bleed: another index's keys next door must
		// not appear in cell loads.
		root := specSubspace().Sub("spfresh-cache").Sub("bleed")
		s := newSPFreshStorage(root, 1)
		other := newSPFreshStorage(root, 2) // generation 2 = different keyspace
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, s, 1)
			spfreshSaveCoarse(tx, s, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, s, 1, 10, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, other, 1, 99, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			tx.Set(other.postingKey(10, tuple.Tuple{int64(1)}), []byte{0x01})
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		cache := newSPFreshRoutingCache(0)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			Expect(cache.fullReload(tx, s, 1)).To(Succeed())
			routed, rerr := cache.route(tx, s, []float64{0, 0}, 1, 10)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(routed).To(HaveLen(1))
			Expect(routed[0].fineID).To(Equal(int64(10)), "generation 2's centroid must not bleed into generation 1")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
