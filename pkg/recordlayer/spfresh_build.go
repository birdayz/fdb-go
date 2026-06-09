package recordlayer

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// SPFresh bulk build (RFC-094 §8): two-level clustering with a real state
// machine — sample → coarse k-means → staging assignment → wave A (per-cell
// fine k-means, CENTROIDS_DONE) → wave B (closure assignment over the
// completed global table, postings + membership + exact-by-construction
// counters, staging cleared in the closing tx, FINALIZED) → generation flip.
//
// Each per-cell step is one bounded transaction claimed through the
// deterministic cellfin task rows, so a crashed build resumes via lease
// expiry and idempotent re-runs — the same recovery machinery splits use.
//
// The vector source is abstracted as a slice here; the maintainer slice wires
// record scanning (and 094.2 wires the foreground/staging interleaving).

// spfreshBuildInput is one vector to index.
type spfreshBuildInput struct {
	pk  tuple.Tuple
	vec []float64
}

// spfreshBuilder drives a bulk build of one generation.
type spfreshBuilder struct {
	db      *FDBDatabase
	storage *spfreshStorage
	config  SPFreshConfig
	owner   string // lease owner identity for cellfin claims

	// batch sizes, overridable in tests
	stagingBatch int

	cellIDs   []int64     // coarse cell ids (parallel to coarseVecs)
	coarseVec [][]float64 // coarse centroid vectors
}

func newSPFreshBuilder(db *FDBDatabase, storage *spfreshStorage, config SPFreshConfig, owner string) *spfreshBuilder {
	return &spfreshBuilder{db: db, storage: storage, config: config, owner: owner, stagingBatch: 200}
}

// build runs the full §8 pipeline over the inputs and flips the generation
// readable. seed makes the clustering deterministic for tests.
func (b *spfreshBuilder) build(ctx context.Context, inputs []spfreshBuildInput, seed int64) error {
	if len(inputs) == 0 {
		return fmt.Errorf("spfresh build: no inputs")
	}

	// 1+2: sample (all inputs at test scale; reservoir at production scale is
	// the maintainer's record-scan concern) and coarse k-means.
	// K₀ = N·r / (avgFill · cellTarget); avgFill ≈ ⅔·Lmax (RFC-094 §8).
	avgFill := (2 * b.config.Lmax) / 3
	k0 := (len(inputs)*b.config.Replication + avgFill*b.config.CellTarget - 1) / (avgFill * b.config.CellTarget)
	if k0 < 1 {
		k0 = 1
	}
	sample := make([][]float64, len(inputs))
	for i := range inputs {
		sample[i] = inputs[i].vec
	}
	coarse, _ := spfreshKMeans(sample, k0, seed, 25)

	// Allocate cell IDs and write COARSE rows.
	err := spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		start, cerr := spfreshClaimIDBlock(tx, b.storage)
		if cerr != nil {
			return cerr
		}
		b.cellIDs = make([]int64, len(coarse))
		b.coarseVec = coarse
		deltas := make([]spfreshDelta, 0, len(coarse))
		for i, vec := range coarse {
			id := start + int64(i)
			b.cellIDs[i] = id
			spfreshSaveCoarse(tx, b.storage, id, encodeCentroidRow(spfreshStateActive, 0, 0, 0, vec))
			deltas = append(deltas, spfreshDelta{op: spfreshOpAddCell, ids: []int64{id}})
		}
		// One cellfin task row per cell — the build state machine.
		for _, id := range b.cellIDs {
			if _, terr := spfreshTaskSetIfAbsent(tx, b.storage, spfreshTaskCellfin, id); terr != nil {
				return terr
			}
		}
		return spfreshAppendDeltas(tx, b.storage, deltas)
	})
	if err != nil {
		return fmt.Errorf("spfresh build: coarse pass: %w", err)
	}

	// 3: staging assignment pass (batched txs; resumable at batch granularity
	// because staging writes are idempotent Sets).
	for lo := 0; lo < len(inputs); lo += b.stagingBatch {
		hi := min(lo+b.stagingBatch, len(inputs))
		batch := inputs[lo:hi]
		err := spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
			tx := rtx.Transaction()
			for _, in := range batch {
				cell := b.nearestCell(in.vec)
				fp16 := vectorcodec.SerializeHalf(in.vec)
				spfreshSaveStaging(tx, b.storage, cell, in.pk, fp16)
				if b.config.Sidecar {
					spfreshSaveSidecar(tx, b.storage, in.pk, fp16)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("spfresh build: staging batch at %d: %w", lo, err)
		}
	}

	// 4: wave A — per-cell fine k-means on the FULL staged membership
	// (RFC-094 §8: the sampling floor doesn't move down a level), sub-Lmin
	// fold, CENTROIDS_DONE. All cells complete before wave B (closure needs
	// the global fine table).
	fineIDs := make(map[int64][]int64)      // cellID -> fine ids
	fineVecs := make(map[int64][][]float64) // cellID -> fine vectors
	for _, cellID := range b.cellIDs {
		if err := b.waveA(ctx, cellID, seed, fineIDs, fineVecs); err != nil {
			return fmt.Errorf("spfresh build: wave A cell %d: %w", cellID, err)
		}
	}

	// 5: wave B — closure assignment across the completed table.
	router := b.buildRouter(fineIDs, fineVecs)
	for _, cellID := range b.cellIDs {
		if err := b.waveB(ctx, cellID, router); err != nil {
			return fmt.Errorf("spfresh build: wave B cell %d: %w", cellID, err)
		}
	}

	// 6: flip readable.
	err = spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		spfreshSetGeneration(tx, b.storage, b.storage.generation)
		return spfreshAppendDeltas(tx, b.storage, []spfreshDelta{
			{op: spfreshOpGeneration, ids: []int64{b.storage.generation}},
		})
	})
	if err != nil {
		return fmt.Errorf("spfresh build: flip: %w", err)
	}
	return nil
}

func (b *spfreshBuilder) nearestCell(vec []float64) int64 {
	best, bestD := b.cellIDs[0], spfreshSquaredDistance(vec, b.coarseVec[0])
	for i := 1; i < len(b.cellIDs); i++ {
		if d := spfreshSquaredDistance(vec, b.coarseVec[i]); d < bestD {
			best, bestD = b.cellIDs[i], d
		}
	}
	return best
}

// waveA claims the cell's task, k-means the staged vectors into fine
// centroids (folding sub-Lmin clusters into their nearest sibling), writes
// the CENTROIDS rows + addFine deltas, and advances the task to
// CENTROIDS_DONE. Idempotent: a re-run (lease takeover after a crash)
// rewrites the same rows for an unfinalized cell.
func (b *spfreshBuilder) waveA(ctx context.Context, cellID int64, seed int64, outIDs map[int64][]int64, outVecs map[int64][][]float64) error {
	return spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		row, err := spfreshTaskClaim(tx, b.storage, spfreshTaskCellfin, cellID, b.owner, spfreshLeaseDeadline(), spfreshNowMs())
		if err != nil {
			return err
		}
		if row.state == spfreshCellfinFinalized {
			// Crash-recovered re-run of a finished cell: reload its centroids
			// for the router instead of re-clustering.
			rows, _, _, lerr := spfreshLoadCell(tx, b.storage, cellID)
			if lerr != nil {
				return lerr
			}
			for _, r := range rows {
				vec, verr := r.row.vector()
				if verr != nil {
					return verr
				}
				outIDs[cellID] = append(outIDs[cellID], r.fineID)
				outVecs[cellID] = append(outVecs[cellID], vec)
			}
			return nil
		}

		pks, vecBytes, err := spfreshLoadStagingCell(tx, b.storage, cellID)
		if err != nil {
			return err
		}
		if len(pks) == 0 {
			// Empty cell (skewed coarse k-means): nothing to cluster; mark done.
			row.state = spfreshCellfinCentroidsDone
			tx.Set(b.storage.taskKey(spfreshTaskCellfin, cellID), encodeTaskRow(row))
			return nil
		}
		vecs := make([][]float64, len(pks))
		for i, vb := range vecBytes {
			v, derr := vectorcodec.Deserialize(vb)
			if derr != nil {
				return derr
			}
			vecs[i] = v
		}

		// pop·r/avgFill fine centroids, ≥1 (RFC-094 §8 formula with r).
		avgFill := (2 * b.config.Lmax) / 3
		k := (len(vecs)*b.config.Replication + avgFill - 1) / avgFill
		if k < 1 {
			k = 1
		}
		cents, assign := spfreshKMeans(vecs, k, seed+cellID, 25)

		// Sub-Lmin fold: clusters below the merge threshold fold into their
		// nearest sibling (or build completion dumps a pile of merge tasks on
		// the fresh rebalancer — LanceDB r3 #2). Skipped when k == 1.
		counts := make([]int, len(cents))
		for _, a := range assign {
			counts[a]++
		}
		keep := make([]int, 0, len(cents))
		for c := range cents {
			if counts[c] >= b.config.Lmin() || len(cents) == 1 {
				keep = append(keep, c)
			}
		}
		if len(keep) == 0 {
			keep = []int{0}
		}

		start, err := spfreshClaimIDBlock(tx, b.storage)
		if err != nil {
			return err
		}
		deltas := make([]spfreshDelta, 0, len(keep))
		for i, c := range keep {
			fineID := start + int64(i)
			spfreshSaveCentroid(tx, b.storage, cellID, fineID, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cents[c]))
			outIDs[cellID] = append(outIDs[cellID], fineID)
			outVecs[cellID] = append(outVecs[cellID], cents[c])
			deltas = append(deltas, spfreshDelta{op: spfreshOpAddFine, ids: []int64{cellID, fineID}})
		}
		row.state = spfreshCellfinCentroidsDone
		tx.Set(b.storage.taskKey(spfreshTaskCellfin, cellID), encodeTaskRow(row))
		return spfreshAppendDeltas(tx, b.storage, deltas)
	})
}

// spfreshBuildRouter routes a vector to fine centroids across ALL cells (the
// wave-B closure table). Flat scan — build-time only.
type spfreshBuildRouter struct {
	ids   []int64
	cells []int64
	vecs  [][]float64
}

func (b *spfreshBuilder) buildRouter(fineIDs map[int64][]int64, fineVecs map[int64][][]float64) *spfreshBuildRouter {
	r := &spfreshBuildRouter{}
	for _, cellID := range b.cellIDs {
		for i, id := range fineIDs[cellID] {
			r.ids = append(r.ids, id)
			r.cells = append(r.cells, cellID)
			r.vecs = append(r.vecs, fineVecs[cellID][i])
		}
	}
	return r
}

// assign returns the closure copy-set (fineIDs) and the fine vectors for
// residual encoding.
func (r *spfreshBuildRouter) assign(vec []float64, rep int, alpha float64) (ids []int64, fvecs [][]float64) {
	cands := spfreshNearestK(vec, r.ids, r.vecs, rep)
	kept := spfreshClosure(cands, rep, alpha)
	for _, c := range kept {
		for i, id := range r.ids {
			if id == c.id {
				ids = append(ids, id)
				fvecs = append(fvecs, r.vecs[i])
				break
			}
		}
	}
	return ids, fvecs
}

// waveB claims the cell, REAL-reads its full staging range (the conflict
// fence for stragglers committing during the window; stragglers that
// committed before are returned as data and processed — RFC-094 §8),
// closure-assigns every staged vector across the global table, writes
// postings + membership + counter ADDs, clears staging, and advances the task
// to FINALIZED. commit_unknown idempotence: ADDs may double-count on retry —
// counters are advisory and reconcile at the first split/merge; posting/
// membership Sets are idempotent.
func (b *spfreshBuilder) waveB(ctx context.Context, cellID int64, router *spfreshBuildRouter) error {
	quantizer := b.newQuantizer()
	return spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		row, err := spfreshTaskClaim(tx, b.storage, spfreshTaskCellfin, cellID, b.owner, spfreshLeaseDeadline(), spfreshNowMs())
		if err != nil {
			return err
		}
		if row.state == spfreshCellfinFinalized {
			return nil // re-run of a finished cell: no-op
		}
		if row.state != spfreshCellfinCentroidsDone {
			return fmt.Errorf("spfresh build: wave B before wave A for cell %d (state %d)", cellID, row.state)
		}

		pks, vecBytes, err := spfreshLoadStagingCell(tx, b.storage, cellID)
		if err != nil {
			return err
		}
		counterDeltas := make(map[int64]int64)
		for i, pk := range pks {
			vec, derr := vectorcodec.Deserialize(vecBytes[i])
			if derr != nil {
				return derr
			}
			ids, fvecs := router.assign(vec, b.config.Replication, b.config.Alpha)
			for j, fineID := range ids {
				residual := make([]float64, len(vec))
				for d := range vec {
					residual[d] = vec[d] - fvecs[j][d]
				}
				tx.Set(b.storage.postingKey(fineID, pk), quantizer.Encode(residual))
				counterDeltas[fineID]++
			}
			tx.Set(b.storage.membershipKey(pk), encodeMembership(ids))
		}
		for fineID, delta := range counterDeltas {
			spfreshCounterAdd(tx, b.storage, spfreshCounterFine, fineID, delta)
		}
		// Cell fine-count counter (the coarse-split trigger input, §6b).
		spfreshCounterAdd(tx, b.storage, spfreshCounterCell, cellID, int64(len(counterDeltas)))

		// Clear staging in this same closing tx; the REAL staging read above
		// covers the whole range.
		r, rerr := b.storage.stagingCellRange(cellID)
		if rerr != nil {
			return rerr
		}
		tx.ClearRange(r)

		row.state = spfreshCellfinFinalized
		tx.Set(b.storage.taskKey(spfreshTaskCellfin, cellID), encodeTaskRow(row))
		return nil
	})
}

func (b *spfreshBuilder) newQuantizer() *spfreshQuantizer {
	return newSPFreshQuantizer(b.config)
}

// spfreshQuantizer wraps the RaBitQ quantizer for posting residual codes.
type spfreshQuantizer struct {
	q VectorQuantizer
}

func newSPFreshQuantizer(config SPFreshConfig) *spfreshQuantizer {
	return &spfreshQuantizer{q: spfreshNewRaBitQ(config)}
}

func (s *spfreshQuantizer) Encode(residual []float64) []byte {
	return s.q.Encode(residual)
}

// Distance estimates the metric distance between a residual query (q − c) and
// a stored residual code (v − c). For Euclidean this equals dist(q, v).
func (s *spfreshQuantizer) Distance(residualQuery []float64, code []byte, dims int) (float64, error) {
	return s.q.Distance(residualQuery, code, dims)
}

func spfreshLeaseDeadline() int64 { return spfreshNowMs() + 60_000 }
