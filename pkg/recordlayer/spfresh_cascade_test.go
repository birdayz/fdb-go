package recordlayer

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// The chunked-split CASCADE: a massively ballooned posting must converge all
// the way down to ≤4×Lmax through repeated re-triggered chunked drains under
// the plain rebalancer loop (the 300k/1M fills quiesced with 4k-entry
// postings and recall collapsed — the cascade was broken somewhere).
var _ = Describe("SPFresh chunked cascade convergence", func() {
	ctx := context.Background()

	It("a 1500-entry posting converges to <=4xLmax postings via rebalancing", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16 // envelope 64
		config.CellTarget = 4
		config.CellMax = 8
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-cascade").Sub("conv"), 1)

		var inputs []spfreshBuildInput
		id := int64(1)
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{float64(i%5) * 0.2, float64(i%7) * 0.2}})
			id++
		}
		for i := 0; i < 8; i++ {
			inputs = append(inputs, spfreshBuildInput{pk: tuple.Tuple{id}, vec: []float64{50 + float64(i%2)*0.1, float64(i%3) * 0.1}})
			id++
		}
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-1")
		Expect(builder.build(ctx, inputs, 42)).To(Succeed())

		// Balloon one posting to 1500 spread-out members + file its trigger.
		quantizerB := newSPFreshQuantizer(config)
		var firstFine int64
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			mem, merr := spfreshReadMembership(tx, storage, tuple.Tuple{int64(1)})
			Expect(merr).NotTo(HaveOccurred())
			firstFine = mem[0]
			cellOf, ferr := spfreshFindCentroidCell(tx, storage, firstFine)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cellOf, firstFine)
			Expect(rerr).NotTo(HaveOccurred())
			cvec, verr := row.vector()
			Expect(verr).NotTo(HaveOccurred())
			for i := 0; i < 1500; i++ {
				pk := tuple.Tuple{int64(10000 + i)}
				v := []float64{float64(i%40) * 0.3, float64(i%37) * 0.3}
				residual := []float64{v[0] - cvec[0], v[1] - cvec[1]}
				tx.Set(storage.postingKey(firstFine, pk), quantizerB.Encode(residual))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{firstFine}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
				spfreshCounterAdd(tx, storage, spfreshCounterFine, firstFine, 1)
			}
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, firstFine)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())

		// Drain to quiescence with the plain rebalancer loop.
		for round := 0; round < 200; round++ {
			worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "cascade", int64(round))
			Expect(rerr).NotTo(HaveOccurred())
			if worked == 0 {
				break
			}
		}

		// EVERY ACTIVE posting must be within the envelope.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cells, _, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			worst := 0
			for _, cid := range cells {
				rows, _, _, cerr := spfreshLoadCell(tx, storage, cid)
				Expect(cerr).NotTo(HaveOccurred())
				for _, r := range rows {
					if r.row.state != spfreshStateActive {
						continue
					}
					entries, _, _, _, perr := spfreshLoadPostingSnapshot(tx, storage, r.fineID, 100000)
					Expect(perr).NotTo(HaveOccurred())
					if len(entries) > worst {
						worst = len(entries)
					}
				}
			}
			Expect(worst).To(BeNumerically("<=", 4*config.Lmax),
				fmt.Sprintf("queue quiesced but worst ACTIVE posting holds %d entries (envelope %d) — the cascade stalled", worst, 4*config.Lmax))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// The two fill-killers from the 300k/1M benchmark debugging, pinned.
var _ = Describe("SPFresh lease exclusion + mint guard (300k fill bugs)", func() {
	ctx := context.Background()

	It("a live foreign lease excludes other claimers", func() {
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-lease").Sub("excl"), 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, terr := spfreshTaskSetIfAbsent(rtx.Transaction(), storage, spfreshTaskSplit, 42)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())

		// Executor A claims with a live lease.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, cerr := spfreshTaskClaim(rtx.Transaction(), storage, spfreshTaskSplit, 42, "exec-A", spfreshLeaseDeadline(), spfreshNowMs())
			return nil, cerr
		})
		Expect(err).NotTo(HaveOccurred())

		// Executor B is excluded while A's lease lives. Two executors SHARING
		// an owner string reclaimed each other's leases freely (the claim
		// keeps same-owner reclaim for in-executor retries) — zero mutual
		// exclusion — which let concurrent rebalancers interleave multi-tx
		// lifecycles and orphan ~3/4 of all entries in the 300k fill.
		// RebalanceSPFreshIndex now mints a unique owner per invocation.
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, cerr := spfreshTaskClaim(rtx.Transaction(), storage, spfreshTaskSplit, 42, "exec-B", spfreshLeaseDeadline(), spfreshNowMs())
			Expect(cerr).To(MatchError(errSPFreshNotFound), "a live foreign lease must exclude other claimers")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// An EXPIRED foreign lease is reclaimable (crash recovery).
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			key := storage.taskKey(spfreshTaskSplit, 42)
			data, gerr := tx.Get(key).Get()
			Expect(gerr).NotTo(HaveOccurred())
			row, derr := decodeTaskRow(data)
			Expect(derr).NotTo(HaveOccurred())
			row.leaseDeadlineMs = spfreshNowMs() - 1
			tx.Set(key, encodeTaskRow(row))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			_, cerr := spfreshTaskClaim(rtx.Transaction(), storage, spfreshTaskSplit, 42, "exec-B", spfreshLeaseDeadline(), spfreshNowMs())
			return nil, cerr
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("every rebalancer invocation mints a UNIQUE lease owner", func() {
		// Reverting to a fixed per-index owner re-opens the bypass: the
		// same-owner reclaim in spfreshTaskClaim would let two concurrent
		// RebalanceSPFreshIndex calls steal each other's live leases.
		a := spfreshRebalanceOwner("idx")
		b := spfreshRebalanceOwner("idx")
		Expect(a).NotTo(Equal(b))

		storage := newSPFreshStorage(specSubspace().Sub("spfresh-lease").Sub("uniq"), 1)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			if _, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskSplit, 7); terr != nil {
				return nil, terr
			}
			_, cerr := spfreshTaskClaim(tx, storage, spfreshTaskSplit, 7, a, spfreshLeaseDeadline(), spfreshNowMs())
			Expect(cerr).NotTo(HaveOccurred())
			// The second invocation cannot touch the first's live lease.
			_, cerr = spfreshTaskClaim(tx, storage, spfreshTaskSplit, 7, b, spfreshLeaseDeadline(), spfreshNowMs())
			Expect(cerr).To(MatchError(errSPFreshNotFound))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("the first-centroid mint refuses unless the index is GENUINELY empty", func() {
		config := DefaultSPFreshConfig(2)
		sub := specSubspace().Sub("spfresh-lease").Sub("mint")
		storage := newSPFreshStorage(sub, 1)
		idx := &Index{Name: "spf_mint_guard"}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			// The post-coarse-split shape that used to invite phantom mints:
			// ids[0] is a FORWARDED EMPTY cell; the live topology is in a
			// later cell. The original mint blindly used ids[0] on any
			// transient zero-candidate route and created centroids no query
			// could route to (the 300k fill orphaned thousands of entries
			// this way; recall collapsed to 0.17).
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRowRaw(spfreshStateForward, 1, 2, 3, nil))
			spfreshSaveCoarse(tx, storage, 2, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, storage, 2, 10, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{1, 1}))

			m := &spfreshIndexMaintainer{
				standardIndexMaintainer: standardIndexMaintainer{index: idx, indexSubspace: sub, tx: tx},
				config:                  config,
			}
			_, ferr := m.spfreshFirstCentroid(storage, []float64{5, 5})
			Expect(ferr).To(MatchError(errSPFreshStaleRoute), "a non-empty index must never mint a first centroid")
			// And nothing was written into the forwarded cell.
			rows, _, _, lerr := spfreshLoadCell(tx, storage, 1)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(rows).To(BeEmpty())
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
