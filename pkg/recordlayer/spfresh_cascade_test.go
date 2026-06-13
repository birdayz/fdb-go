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
			worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "cascade", int64(round), 0, nil)
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
			Expect(cerr).To(MatchError(errSPFreshLeaseHeld), "a live foreign lease must exclude other claimers")
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

		// Cross-PROCESS uniqueness (codex P1): every process counts the
		// sequence from zero, so the owner must embed a per-process random
		// nonce or two live workers on different machines collide on
		// "rebalance-idx-1". Pin: the owner contains this process's nonce,
		// and the nonce generator is random, not constant.
		Expect(a).To(ContainSubstring(spfreshProcessNonce))
		Expect(spfreshProcessNonce).NotTo(BeEmpty())
		Expect(newSPFreshProcessNonce()).NotTo(Equal(newSPFreshProcessNonce()))

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
			Expect(cerr).To(MatchError(errSPFreshLeaseHeld))
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

			// The all-candidates-stale error must stay CHEAP (codex P2): it
			// is a normal retryable outcome inside the caller's save
			// transaction, and embedding the topology dump made every
			// transient stale route scan the whole index. "hist=" is the
			// dump's posting-histogram signature.
			ierr := m.spfreshInsertRouted(storage,
				[]spfreshRouted{{cellID: 2, fineID: 99, state: spfreshStateActive, vec: []float64{1, 1}, d2: 0}},
				tuple.Tuple{int64(777)}, []float64{1, 1})
			Expect(ierr).To(MatchError(errSPFreshStaleRoute))
			Expect(ierr.Error()).NotTo(ContainSubstring("hist="),
				"stale-route errors must not embed the O(index) topology dump")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Torvalds delta-review findings on dc894af8, pinned: the csplit pause-window
// repair must not file split tasks for SEALED rows, and the seal zombie-clear
// must not destroy a sealed task (the only copy of the child IDs).
var _ = Describe("SPFresh sealed-row lifecycle edges", func() {
	ctx := context.Background()

	It("csplit move re-files split tasks for moved oversized ACTIVE rows (pause-window repair)", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		config.CellTarget = 4
		config.CellMax = 8
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-csplit").Sub("repair"), 1)
		quantizer := newSPFreshQuantizer(config)

		// The post-pause shape: an ACTIVE posting ballooned past Lmax while
		// fine-split probes were suppressed, so it has NO split task; the
		// csplit that caused the pause now executes. (SEALED rows never reach
		// the move — the claim defers on them — so the repair only ever sees
		// ACTIVE rows.)
		const fatFine, slimFine = int64(10), int64(11)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{25, 25}))
			cvec := []float64{0, 0}
			spfreshSaveCentroid(tx, storage, 1, fatFine, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cvec))
			for i := 0; i < config.Lmax+4; i++ {
				pk := tuple.Tuple{int64(20000 + i)}
				v := []float64{float64(i%9) * 0.4, float64(i%7) * 0.4}
				tx.Set(storage.postingKey(fatFine, pk), quantizer.Encode([]float64{v[0] - cvec[0], v[1] - cvec[1]}))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{fatFine}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, fatFine, int64(config.Lmax+4))
			// A second ACTIVE fine so the 2-means partition is two-sided.
			spfreshSaveCentroid(tx, storage, 1, slimFine, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{50, 50}))
			spfreshCounterSet(tx, storage, spfreshCounterFine, slimFine, 1)
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, int64(config.CellMax+1))
			_, terr := spfreshTaskSetIfAbsent(tx, storage, spfreshTaskCSplit, 1)
			return nil, terr
		})
		Expect(err).NotTo(HaveOccurred())

		// Drain. The csplit moves both fines; the repair files fatFine's
		// split task in the SAME move transaction; later rounds execute it.
		// Reverting the repair leaves fatFine ACTIVE over Lmax forever with
		// an empty queue — the 300k/1M recall collapse.
		for round := 0; round < 100; round++ {
			worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "pauserepair", int64(round), 0, nil)
			Expect(rerr).NotTo(HaveOccurred())
			if worked == 0 {
				break
			}
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// The coarse split ran: cell 1 is no longer ACTIVE.
			_, coarseRows, lerr := spfreshLoadAllCoarse(tx, storage)
			Expect(lerr).NotTo(HaveOccurred())
			Expect(len(coarseRows)).To(BeNumerically(">", 1), "coarse split must have executed")
			// The repaired split ran to completion: the ballooned parent is
			// FORWARD (not ACTIVE), its entries live in children ≤ Lmax.
			cell, ferr := spfreshFindCentroidCell(tx, storage, fatFine)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, fatFine)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(row.state).NotTo(Equal(spfreshStateActive),
				"queue quiesced with the moved posting still ACTIVE over Lmax — the pause-window repair did not fire")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// Torvalds final-gauntlet B2: the pause-window repair must run on the
	// DEGENERATE task-clearing exits too — a csplit that finds its cell
	// drained below 2 ACTIVE rows (merges won the race) or its 2-means
	// degenerate clears the task, un-pausing the cell, and without the
	// repair an oversized posting whose trigger the pause suppressed stays
	// live-but-truncated forever (the master churn flake's orphan shape).
	It("csplit cleared on a merge-drained cell still re-files the oversized survivor's split", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		config.CellTarget = 4
		config.CellMax = 8
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-csplit").Sub("drained"), 1)
		quantizer := newSPFreshQuantizer(config)

		// ONE ACTIVE fine left in the cell (merges drained the rest during
		// the pause), ballooned past Lmax, NO split task; the csplit task is
		// PAUSING. The handler takes the len(rows)<2 exit.
		const fatFine = int64(10)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{25, 25}))
			cvec := []float64{0, 0}
			spfreshSaveCentroid(tx, storage, 1, fatFine, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cvec))
			for i := 0; i < config.Lmax+4; i++ {
				pk := tuple.Tuple{int64(30000 + i)}
				v := []float64{float64(i%9) * 0.4, float64(i%7) * 0.4}
				tx.Set(storage.postingKey(fatFine, pk), quantizer.Encode([]float64{v[0] - cvec[0], v[1] - cvec[1]}))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{fatFine}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, fatFine, int64(config.Lmax+4))
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, 1)
			tx.Set(storage.taskKey(spfreshTaskCSplit, 1), encodeTaskRow(spfreshTaskRow{state: spfreshCSplitPausing}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		for round := 0; round < 100; round++ {
			worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "drainedrepair", int64(round), 0, nil)
			Expect(rerr).NotTo(HaveOccurred())
			if worked == 0 {
				break
			}
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			// The csplit task is gone (cleared), and the repaired split ran:
			// fatFine is no longer an ACTIVE posting over Lmax.
			data, gerr := tx.Get(storage.taskKey(spfreshTaskCSplit, 1)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(data).To(BeNil(), "degenerate csplit must clear its task")
			cell, ferr := spfreshFindCentroidCell(tx, storage, fatFine)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, fatFine)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(row.state).NotTo(Equal(spfreshStateActive),
				"queue quiesced with the drained cell's survivor still ACTIVE over Lmax — the degenerate-exit repair did not fire")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("csplit cleared on degenerate 2-means still re-files oversized splits", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		config.CellTarget = 4
		config.CellMax = 8
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-csplit").Sub("degen2means"), 1)
		quantizer := newSPFreshQuantizer(config)

		// TWO ACTIVE fines with IDENTICAL centroid vectors (2-means cannot
		// produce two centers), one ballooned past Lmax with NO split task.
		const fatFine, twinFine = int64(10), int64(11)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{25, 25}))
			cvec := []float64{3, 3}
			spfreshSaveCentroid(tx, storage, 1, fatFine, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cvec))
			spfreshSaveCentroid(tx, storage, 1, twinFine, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cvec))
			for i := 0; i < config.Lmax+4; i++ {
				pk := tuple.Tuple{int64(40000 + i)}
				v := []float64{float64(i%9) * 0.4, float64(i%7) * 0.4}
				tx.Set(storage.postingKey(fatFine, pk), quantizer.Encode([]float64{v[0] - cvec[0], v[1] - cvec[1]}))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{fatFine}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, fatFine, int64(config.Lmax+4))
			spfreshCounterSet(tx, storage, spfreshCounterFine, twinFine, 1)
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, 2)
			tx.Set(storage.taskKey(spfreshTaskCSplit, 1), encodeTaskRow(spfreshTaskRow{state: spfreshCSplitPausing}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		for round := 0; round < 100; round++ {
			worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, "degenrepair", int64(round), 0, nil)
			Expect(rerr).NotTo(HaveOccurred())
			if worked == 0 {
				break
			}
		}

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cell, ferr := spfreshFindCentroidCell(tx, storage, fatFine)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, fatFine)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(row.state).NotTo(Equal(spfreshStateActive),
				"queue quiesced with the identical-twin cell's posting still ACTIVE over Lmax — the degenerate-2-means repair did not fire")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// A chunked drain that lost its lease can resume through the SINGLE-TX
	// split path once the parent has shrunk back under the 4×Lmax envelope.
	// The committed children hold entries whose RaBitQ codes are residuals
	// against the committed centroid vectors — the resume must pin that
	// geometry (no 2-means recompute, no centroid overwrite) and ADD to the
	// children's counters rather than resetting them, or every drained entry
	// decodes against the wrong center and the counters lie (codex P1
	// follow-on; found while moving the HDR write into the chunked planner).
	It("single-tx resume of a partially-drained chunked split pins the children's geometry", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-split").Sub("chunkresume"), 1)
		quantizer := newSPFreshQuantizer(config)

		const parent, childA, childB = int64(10), int64(11), int64(12)
		centA := []float64{1, 1}
		centB := []float64{9, 9}
		// 5 entries already drained into each child (residuals vs the
		// committed centers), 10 remaining in the SEALED parent clustered
		// near (2,2) and (8,8) — a fresh 2-means over the remainder would
		// produce visibly different centers, so an overwrite cannot pass.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{5, 5}))
			spfreshSaveCentroid(tx, storage, 1, parent, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, storage, 1, childA, encodeCentroidRow(spfreshStateActive, 0, 0, 0, centA))
			spfreshSaveCentroid(tx, storage, 1, childB, encodeCentroidRow(spfreshStateActive, 0, 0, 0, centB))
			tx.Set(storage.postingHDRKey(parent), encodePostingHDR(1, childA, childB))
			for i := 0; i < 5; i++ {
				pkA, pkB := tuple.Tuple{int64(60000 + i)}, tuple.Tuple{int64(60100 + i)}
				vA := []float64{1, float64(i) * 0.25}
				vB := []float64{9, 9 - float64(i)*0.25}
				tx.Set(storage.postingKey(childA, pkA), quantizer.Encode([]float64{vA[0] - centA[0], vA[1] - centA[1]}))
				tx.Set(storage.membershipKey(pkA), encodeMembership([]int64{childA}))
				tx.Set(storage.sidecarKey(pkA), vectorcodec.SerializeHalf(vA))
				tx.Set(storage.postingKey(childB, pkB), quantizer.Encode([]float64{vB[0] - centB[0], vB[1] - centB[1]}))
				tx.Set(storage.membershipKey(pkB), encodeMembership([]int64{childB}))
				tx.Set(storage.sidecarKey(pkB), vectorcodec.SerializeHalf(vB))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, childA, 5)
			spfreshCounterSet(tx, storage, spfreshCounterFine, childB, 5)
			for i := 0; i < 10; i++ {
				pk := tuple.Tuple{int64(61000 + i)}
				v := []float64{2, float64(i) * 0.5}
				if i >= 5 {
					v = []float64{8, 8 - float64(i-5)*0.5}
				}
				tx.Set(storage.postingKey(parent, pk), quantizer.Encode(v)) // residual vs (0,0)
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{parent}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, parent, 10)
			// The chunked planner already counted the cell's net +1.
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, 3)
			tx.Set(storage.taskKey(spfreshTaskSplit, parent),
				encodeTaskRow(spfreshTaskRow{state: spfreshSplitTaskSealed, childA: childA, childB: childB}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(spfreshSplitFine(ctx, sharedDB, storage, config, "chunkresume-owner", 1, parent, 7)).To(Succeed())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			rowA, raErr := spfreshReadCentroidForWrite(tx, storage, 1, childA)
			Expect(raErr).NotTo(HaveOccurred())
			rowB, rbErr := spfreshReadCentroidForWrite(tx, storage, 1, childB)
			Expect(rbErr).NotTo(HaveOccurred())
			vA, _ := rowA.vector()
			vB, _ := rowB.vector()
			Expect(vA).To(Equal(centA), "resume overwrote child A's committed centroid — drained entries now decode against the wrong center")
			Expect(vB).To(Equal(centB), "resume overwrote child B's committed centroid")
			cntA, _ := spfreshCounterReadSnapshot(tx, storage, spfreshCounterFine, childA)
			cntB, _ := spfreshCounterReadSnapshot(tx, storage, spfreshCounterFine, childB)
			Expect(cntA+cntB).To(Equal(int64(20)), "resume must ADD the remainder to the drained counts, not reset them")
			parentRow, prErr := spfreshReadCentroidForWrite(tx, storage, 1, parent)
			Expect(prErr).NotTo(HaveOccurred())
			Expect(parentRow.state).To(Equal(spfreshStateForward))
			cellCnt, _ := spfreshCounterReadSnapshot(tx, storage, spfreshCounterCell, 1)
			Expect(cellCnt).To(Equal(int64(3)), "the planner already counted the net +1 — the resume must not double-count")
			// Every entry's membership names the posting that holds it.
			for _, base := range []int64{60000, 60100, 61000} {
				count := 5
				if base == 61000 {
					count = 10
				}
				for i := 0; i < count; i++ {
					pk := tuple.Tuple{base + int64(i)}
					mem, mErr := spfreshReadMembership(tx, storage, pk)
					Expect(mErr).NotTo(HaveOccurred())
					for _, fid := range mem {
						data, gErr := tx.Get(storage.postingKey(fid, pk)).Get()
						Expect(gErr).NotTo(HaveOccurred())
						Expect(data).NotTo(BeNil(), "pk %v membership names posting %d with no entry", pk, fid)
					}
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// Torvalds re-review S-NEW: on a chunked resume the child re-trigger
	// must gate on the child's TOTAL (drained + remainder via the counter
	// read), not the remainder alone — a child at 0.9·Lmax drained +
	// 0.5·Lmax remainder is over the envelope with no other trigger site
	// once writes stop.
	It("single-tx resume re-triggers a child whose TOTAL crosses Lmax", func() {
		config := DefaultSPFreshConfig(2)
		config.Lmax = 16
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-split").Sub("resumetrigger"), 1)
		quantizer := newSPFreshQuantizer(config)

		const parent, childA, childB = int64(10), int64(11), int64(12)
		centA := []float64{1, 1}
		centB := []float64{50, 50}
		// Child A: 15 drained entries (0.9·Lmax). Parent: 8 remaining near
		// child A — remainder ≤ Lmax, total 23 > Lmax. Child B stays far and
		// empty.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{5, 5}))
			spfreshSaveCentroid(tx, storage, 1, parent, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCentroid(tx, storage, 1, childA, encodeCentroidRow(spfreshStateActive, 0, 0, 0, centA))
			spfreshSaveCentroid(tx, storage, 1, childB, encodeCentroidRow(spfreshStateActive, 0, 0, 0, centB))
			tx.Set(storage.postingHDRKey(parent), encodePostingHDR(1, childA, childB))
			for i := 0; i < 15; i++ {
				pk := tuple.Tuple{int64(62000 + i)}
				v := []float64{1, float64(i) * 0.2}
				tx.Set(storage.postingKey(childA, pk), quantizer.Encode([]float64{v[0] - centA[0], v[1] - centA[1]}))
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{childA}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, childA, 15)
			spfreshCounterSet(tx, storage, spfreshCounterFine, childB, 0)
			for i := 0; i < 8; i++ {
				pk := tuple.Tuple{int64(63000 + i)}
				v := []float64{2, float64(i) * 0.2}
				tx.Set(storage.postingKey(parent, pk), quantizer.Encode(v)) // residual vs (0,0)
				tx.Set(storage.membershipKey(pk), encodeMembership([]int64{parent}))
				tx.Set(storage.sidecarKey(pk), vectorcodec.SerializeHalf(v))
			}
			spfreshCounterSet(tx, storage, spfreshCounterFine, parent, 8)
			spfreshCounterSet(tx, storage, spfreshCounterCell, 1, 3)
			tx.Set(storage.taskKey(spfreshTaskSplit, parent),
				encodeTaskRow(spfreshTaskRow{state: spfreshSplitTaskSealed, childA: childA, childB: childB}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		Expect(spfreshSplitFine(ctx, sharedDB, storage, config, "resumetrigger-owner", 1, parent, 7)).To(Succeed())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			taskA, gerr := tx.Get(storage.taskKey(spfreshTaskSplit, childA)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(taskA).NotTo(BeNil(),
				"child A totals 23 > Lmax (15 drained + 8 remainder) — the resume must file its split even though the remainder alone is under Lmax")
			taskB, gerr := tx.Get(storage.taskKey(spfreshTaskSplit, childB)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(taskB).To(BeNil(), "child B is empty — no trigger")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// And the filed task drains the child back inside the envelope.
		for round := 0; round < 100; round++ {
			worked, rerr := spfreshRebalanceOnce(ctx, sharedDB, storage, config, fmt.Sprintf("resumetrigger-%d", round), int64(round), 0, nil)
			Expect(rerr).NotTo(HaveOccurred())
			if worked == 0 {
				break
			}
		}
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			cell, ferr := spfreshFindCentroidCell(tx, storage, childA)
			Expect(ferr).NotTo(HaveOccurred())
			row, rerr := spfreshReadCentroidForWrite(tx, storage, cell, childA)
			Expect(rerr).NotTo(HaveOccurred())
			Expect(row.state).NotTo(Equal(spfreshStateActive),
				"queue quiesced with child A still ACTIVE over Lmax — the resume trigger did not fire or did not drain")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	// codex delta P2: a pass that skips a poisoned task but commits work
	// behind it must ACCOUNT that work and refresh the process-local cache
	// before surfacing the error — returning (0, err) reported committed
	// splits as nothing and left routing on the pre-pass topology.
	It("a poisoned task neither hides committed work nor skips the cache refresh", func() {
		tenant, sub := newSweeperTenant("spf_poison", 8, true)
		balloonSweeperTenant(sub, 80) // real work: a split cascade
		storage := newSPFreshStorage(sub, 1)

		// Poison: an undecodable task row at the head of the deterministic
		// scan order (kind split, id 1 — below any allocated fineID). Guard
		// the assumption: if ID allocation ever hands a real fine the id 1,
		// the poison would silently collide with its task key.
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			_, ferr := spfreshFindCentroidCell(tx, storage, 1)
			Expect(ferr).To(MatchError(errSPFreshNotFound),
				"fine id 1 exists — the poison id collides with a real task key; pick an unallocated id")
			tx.Set(storage.taskKey(spfreshTaskSplit, 1), []byte("garbage"))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		total, rerr := RebalanceSPFreshIndex(ctx, sharedDB, tenant.StoreBuilder, tenant.IndexName)
		Expect(rerr).To(HaveOccurred(), "the poisoned task must surface in the joined error")
		Expect(total).To(BeNumerically(">", 0),
			"the balloon's split committed behind the poison — reporting 0 hides real work (codex delta P2)")
		// The eager refresh ran on the error path: the global cache routes on
		// the post-split topology.
		Expect(spfreshCacheFor(sub, 1).ready(1)).To(BeTrue(),
			"the process-local cache must be refreshed when a pass committed work, error or not")
	})

	It("seal zombie-clear preserves a sealed task whose row moved cells", func() {
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-seal").Sub("relocate"), 1)
		const fine = int64(10)
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			spfreshSaveCoarse(tx, storage, 2, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{50, 50}))
			// Sealed mid-split, then moved by a coarse split to cell 2. The
			// task row carries the ONLY copy of the children.
			spfreshSaveCentroid(tx, storage, 2, fine, encodeCentroidRow(spfreshStateSealed, 0, 0, 0, []float64{50, 50}))
			tx.Set(storage.taskKey(spfreshTaskSplit, fine), encodeTaskRow(spfreshTaskRow{state: spfreshSplitTaskSealed, childA: 100, childB: 101}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A stale executor claims with the OLD cell. Pre-fix this cleared
		// the task — stranding the posting SEALED forever (children lost).
		out, serr := spfreshSealFine(ctx, sharedDB, storage, "stale-exec", 1, fine)
		Expect(serr).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeFalse())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			task, gerr := tx.Get(storage.taskKey(spfreshTaskSplit, fine)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(task).NotTo(BeNil(), "sealed task must survive a stale-cell claim: it holds the only child IDs")
			row, derr := decodeTaskRow(task)
			Expect(derr).NotTo(HaveOccurred())
			Expect(row.childA).To(Equal(int64(100)))
			// And the keep must RELEASE the stale executor's lease: a
			// different owner claims it immediately (codex P2 — a kept task
			// leased to a no-progress invocation stalls the split until
			// lease expiry, since unique owners never self-reclaim).
			claimed, cerr := spfreshTaskClaim(tx, storage, spfreshTaskSplit, fine, "other-exec", spfreshLeaseDeadline(), spfreshNowMs())
			Expect(cerr).NotTo(HaveOccurred(), "kept sealed task must be immediately claimable, not lease-stalled")
			Expect(claimed.childA).To(Equal(int64(100)), "children must survive the keep")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())

		// A sealed task whose fine is gone EVERYWHERE is still cleared.
		const goneFine = int64(99)
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			rtx.Transaction().Set(storage.taskKey(spfreshTaskSplit, goneFine), encodeTaskRow(spfreshTaskRow{state: spfreshSplitTaskSealed, childA: 200, childB: 201}))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		out, serr = spfreshSealFine(ctx, sharedDB, storage, "stale-exec", 1, goneFine)
		Expect(serr).NotTo(HaveOccurred())
		Expect(out.proceed).To(BeFalse())
		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			task, gerr := rtx.Transaction().Get(storage.taskKey(spfreshTaskSplit, goneFine)).Get()
			Expect(gerr).NotTo(HaveOccurred())
			Expect(task).To(BeNil(), "a task for a fine absent from every cell is a zombie: cleared")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

// Reservoir sampling (build §8 step 1): the coarse k-means trains on a
// SAMPLE, but K₀ must cover the FULL dataset — deriving it from the sample
// would shrink the topology by the sampling ratio and overfill every cell.
var _ = Describe("SPFresh build sampling", func() {
	ctx := context.Background()

	It("K₀ derives from the full count, not the sample size", func() {
		config := DefaultSPFreshConfig(2)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-sample").Sub("k0"), 1)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-k0")

		// 60 sample points standing in for a 100k-record dataset: K₀ from
		// totalN is 25 cells; K₀ derived from the sample would be 1. The
		// sample can host the full-count topology, so the build must produce
		// exactly the totalN-derived count.
		sample := make([][]float64, 60)
		for i := range sample {
			sample[i] = []float64{float64(i * 3), float64(i % 7)}
		}
		const totalN = 100_000
		Expect(builder.coarsePass(ctx, sample, totalN, 7)).To(Succeed())

		avgFill := (2 * config.Lmax) / 3
		wantK0 := (totalN*config.Replication + avgFill*config.CellTarget - 1) / (avgFill * config.CellTarget)
		Expect(len(builder.cellIDs)).To(Equal(wantK0),
			"k0 must be computed from totalN, not the sample size")
	})

	It("a sample too small for the full-count topology fails loudly", func() {
		config := DefaultSPFreshConfig(2)
		storage := newSPFreshStorage(specSubspace().Sub("spfresh-sample").Sub("k0small"), 1)
		builder := newSPFreshBuilder(sharedDB, storage, config, "builder-k0-small")

		// 4 points cannot host the 25 cells a 100k dataset needs: letting
		// the k>n clamp shrink the topology to 4 cells would silently undo
		// exactly what K₀-from-totalN protects (every cell ~25× overfull).
		// The build must refuse and name the sample-cap remedy.
		sample := [][]float64{{0, 0}, {1, 1}, {50, 50}, {51, 51}}
		err := builder.coarsePass(ctx, sample, 100_000, 7)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exceeds the 4-point training sample"))
	})
})

// Codex 094.4 review: the insert path stopped verifying candidates once
// Replication of them were ACTIVE, so the closure's RNG rule could only ever
// SHRINK the copy-set — a same-direction duplicate next to the nearest
// centroid silently under-replicated the record instead of spending the copy
// on a diverse replica the queue already held.
var _ = Describe("SPFresh insert closure RNG diversity", func() {
	ctx := context.Background()

	It("keeps scanning past a same-direction duplicate for a diverse replica", func() {
		config := DefaultSPFreshConfig(2)
		config.Alpha = 1.5 // ratio bound must admit the diverse replica (d2 1.96 <= 2.25)
		sub := specSubspace().Sub("spfresh-rng").Sub("insert")
		storage := newSPFreshStorage(sub, 1)
		idx := &Index{Name: "spf_rng_insert"}

		// SPANN Figure 5 at the origin: blue nearest, yellow just past blue
		// in the SAME direction, grey farther but OPPOSITE.
		const blue, yellow, grey = int64(10), int64(11), int64(12)
		vecs := map[int64][]float64{
			blue:   {1, 0},
			yellow: {1.3, 0},
			grey:   {-1.4, 0},
		}
		pk := tuple.Tuple{int64(777)}
		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			spfreshSetGeneration(tx, storage, 1)
			spfreshSaveCoarse(tx, storage, 1, encodeCentroidRow(spfreshStateActive, 0, 0, 0, []float64{0, 0}))
			var routed []spfreshRouted
			for _, id := range []int64{blue, yellow, grey} {
				spfreshSaveCentroid(tx, storage, 1, id, encodeCentroidRow(spfreshStateActive, 0, 0, 0, vecs[id]))
				routed = append(routed, spfreshRouted{
					cellID: 1, fineID: id, state: spfreshStateActive,
					vec: vecs[id], d2: spfreshSquaredDistance([]float64{0, 0}, vecs[id]),
				})
			}
			m := &spfreshIndexMaintainer{
				standardIndexMaintainer: standardIndexMaintainer{index: idx, indexSubspace: sub, tx: tx},
				config:                  config,
			}
			Expect(m.spfreshInsertRouted(storage, routed, pk, []float64{0, 0})).To(Succeed())

			ids, merr := spfreshReadMembership(tx, storage, pk)
			Expect(merr).NotTo(HaveOccurred())
			Expect(ids).To(ConsistOf(blue, grey),
				"the copy-set must skip the same-direction duplicate AND reach the diverse replica past index r")
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})
