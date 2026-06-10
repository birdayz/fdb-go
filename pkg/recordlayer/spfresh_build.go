package recordlayer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/rabitq"
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
	token   []byte // ownership token (META/build) — see spfreshVerifyBuilderToken

	// batch sizes, overridable in tests
	stagingBatch int

	cellIDs   []int64     // coarse cell ids (parallel to coarseVecs)
	coarseVec [][]float64 // coarse centroid vectors
}

func newSPFreshBuilder(db *FDBDatabase, storage *spfreshStorage, config SPFreshConfig, owner string) *spfreshBuilder {
	// Uniqueness, not secrecy: the token only has to distinguish two builder
	// instances racing the same index.
	token := make([]byte, 16)
	binary.LittleEndian.PutUint64(token, rand.Uint64())
	binary.LittleEndian.PutUint64(token[8:], rand.Uint64())
	return &spfreshBuilder{db: db, storage: storage, config: config, owner: owner, token: token, stagingBatch: 200}
}

// build runs the full §8 pipeline over the inputs and flips the generation
// readable: coarsePass → stageBatch loop → finalize. The maintainer's
// BuildSPFreshIndex drives the same steps with record scans interleaved
// (coarse FIRST, then the assignment scan — the ordering §8's foreground
// interleaving depends on); direct callers (tests) use this composition.
func (b *spfreshBuilder) build(ctx context.Context, inputs []spfreshBuildInput, seed int64) error {
	sample := make([][]float64, len(inputs))
	for i := range inputs {
		sample[i] = inputs[i].vec
	}
	if err := b.coarsePass(ctx, sample, seed); err != nil {
		return err
	}
	for lo := 0; lo < len(inputs); lo += b.stagingBatch {
		hi := min(lo+b.stagingBatch, len(inputs))
		if err := b.stageBatch(ctx, inputs[lo:hi]); err != nil {
			return err
		}
	}
	return b.finalize(ctx, seed)
}

// coarsePass is §8 steps 1+2: coarse k-means over the sample (all inputs at
// test scale; reservoir sampling at production scale is the maintainer's
// record-scan concern) and the COARSE/cellfin row writes. Committing the
// coarse table BEFORE the assignment scan is what closes the lost-record
// window: from this point on a foreground write can always route itself.
// K₀ = N·r / (avgFill · cellTarget); avgFill ≈ ⅔·Lmax (RFC-094 §8).
func (b *spfreshBuilder) coarsePass(ctx context.Context, sample [][]float64, seed int64) error {
	if len(sample) == 0 {
		return fmt.Errorf("spfresh build: no inputs")
	}
	avgFill := (2 * b.config.Lmax) / 3
	k0 := (len(sample)*b.config.Replication + avgFill*b.config.CellTarget - 1) / (avgFill * b.config.CellTarget)
	if k0 < 1 {
		k0 = 1
	}
	coarse, _ := spfreshKMeans(sample, k0, seed, 25)
	// Roundtrip the centroids through fp16 BEFORE anything routes on them:
	// the COARSE rows store fp16, so foreground writers route on the
	// roundtripped vectors — if the builder's own staging routed on the raw
	// k-means output, a boundary vector could land in DIFFERENT cells on the
	// two paths, get double-staged, and leave an orphaned posting entry whose
	// membership row names only the last writer (Torvalds 094.2 #3). One
	// table, one set of bytes, both routers.
	for i, vec := range coarse {
		rt, rerr := vectorcodec.Deserialize(vectorcodec.SerializeHalf(vec))
		if rerr != nil {
			return fmt.Errorf("spfresh build: fp16 roundtrip centroid %d: %w", i, rerr)
		}
		coarse[i] = rt
	}

	// Allocate cell IDs and write COARSE rows. IDEMPOTENT under retry: the
	// build-state row (META, generation-scoped via the task subspace) records
	// the minted cell block in the SAME tx; a commit_unknown retry re-reads it
	// and reuses the block instead of minting a second orphaned cell set
	// (Torvalds 094.1 #1a).
	err := spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		// Claim build ownership (or re-find our own claim on a commit_unknown
		// retry). The maintainer path already took the token atomically with
		// the pre-build clear; for builders driven directly the claim is here.
		if terr := spfreshClaimBuilderToken(tx, b.storage, b.token); terr != nil {
			return terr
		}
		if prior, perr := tx.Get(b.storage.taskKey(spfreshTaskCellfin, 0)).Get(); perr != nil {
			return perr
		} else if prior != nil {
			row, derr := decodeTaskRow(prior)
			if derr != nil {
				return derr
			}
			if row.childB != int64(len(coarse)) {
				// A prior ABANDONED build's residue with a different record
				// set (BuildSPFreshIndex clears the target generation before
				// building, so this is defense-in-depth, not a live path):
				// reusing it would index coarseVec out of range.
				return fmt.Errorf("spfresh build: build-state row records %d cells, this run computed %d — clear the target generation and rebuild", row.childB, len(coarse))
			}
			b.cellIDs = make([]int64, row.childB)
			for i := range b.cellIDs {
				b.cellIDs[i] = row.childA + int64(i)
			}
			b.coarseVec = coarse
			return nil
		}
		start, cerr := spfreshClaimIDBlock(tx, b.storage)
		if cerr != nil {
			return cerr
		}
		tx.Set(b.storage.taskKey(spfreshTaskCellfin, 0), encodeTaskRow(spfreshTaskRow{childA: start, childB: int64(len(coarse))}))
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
	return nil
}

// stageBatch is one §8 step-3 assignment transaction: route each input to its
// nearest coarse cell, write STAGING (full fp16 vectors — pass 4 must train
// k-means and re-encode residuals; lossy codes can't) + SIDECAR. Idempotent
// Sets, resumable at batch granularity. Direct-build path only (static
// inputs, no concurrent deletes); the maintainer's assignment scan stages
// via stageInTx INSIDE the scan transaction instead.
func (b *spfreshBuilder) stageBatch(ctx context.Context, batch []spfreshBuildInput) error {
	err := spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		return b.stageInTx(rtx, batch)
	})
	if err != nil {
		return fmt.Errorf("spfresh build: staging batch: %w", err)
	}
	return nil
}

// stageInTx writes one batch's staging rows in the CALLER's transaction. The
// maintainer's assignment scan calls this inside the scan tx so the staging
// writes commit atomically with the scan's REAL read of the record range —
// a delete committing after the scan read aborts the whole tx at the
// resolver and the retry's scan no longer returns the record. Staging the
// batch in a separate tx re-stages pks deleted in between: a permanent ghost,
// since no future delete clears a pk whose record is gone (Torvalds 094.2 #2).
func (b *spfreshBuilder) stageInTx(rtx *FDBRecordContext, batch []spfreshBuildInput) error {
	tx := rtx.Transaction()
	if terr := spfreshVerifyBuilderToken(tx, b.storage, b.token); terr != nil {
		return terr
	}
	for _, in := range batch {
		cell := b.nearestCell(in.vec)
		fp16 := vectorcodec.SerializeHalf(in.vec)
		spfreshSaveStaging(tx, b.storage, cell, in.pk, fp16)
		if b.config.Sidecar {
			spfreshSaveSidecar(tx, b.storage, in.pk, fp16)
		}
	}
	return nil
}

// finalize is §8 steps 4–6: wave A (per-cell fine k-means on the FULL staged
// membership — the sampling floor doesn't move down a level — with sub-Lmin
// fold, CENTROIDS_DONE), wave B (closure assignment across the completed
// global table; postings + membership + ADD counters; staging cleared in the
// closing tx, FINALIZED), and the generation flip.
func (b *spfreshBuilder) finalize(ctx context.Context, seed int64) error {
	fineIDs := make(map[int64][]int64)      // cellID -> fine ids
	fineVecs := make(map[int64][][]float64) // cellID -> fine vectors
	for _, cellID := range b.cellIDs {
		if err := b.waveA(ctx, cellID, seed, fineIDs, fineVecs); err != nil {
			return fmt.Errorf("spfresh build: wave A cell %d: %w", cellID, err)
		}
	}

	router := b.buildRouter(fineIDs, fineVecs)
	for _, cellID := range b.cellIDs {
		if err := b.waveB(ctx, cellID, router); err != nil {
			return fmt.Errorf("spfresh build: wave B cell %d: %w", cellID, err)
		}
	}

	if err := b.flip(ctx); err != nil {
		return fmt.Errorf("spfresh build: flip: %w", err)
	}
	return nil
}

// flip publishes the built generation — CAS: only from the generation this
// build was based on (codex r3: a concurrent build that flipped first must not
// be overwritten; the REAL reads' conflict ranges serialize racing flips).
// Idempotent under commit_unknown_result: cur == target with OUR token still
// in place is this build's own committed flip being retried — success, not a
// concurrent builder (codex r4). The narrow leftover corner — a retry that
// lands after a NEWER build already took the token — reports a takeover error
// even though our flip committed; that build's own BuildSPFreshIndex run
// redoes the completion bookkeeping, so nothing is lost.
func (b *spfreshBuilder) flip(ctx context.Context) error {
	return spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		if terr := spfreshVerifyBuilderToken(tx, b.storage, b.token); terr != nil {
			return terr
		}
		cur, cerr := spfreshReadGenerationForWrite(tx, newSPFreshStorage(b.storage.index, 0))
		if cerr != nil && !errors.Is(cerr, errSPFreshNotFound) {
			return cerr
		}
		if cerr == nil && cur == b.storage.generation {
			return nil // our own committed flip, retried after commit_unknown_result
		}
		if cerr == nil && cur > b.storage.generation {
			// Defense-in-depth only: a builder that flipped past us must own
			// the token, so the verify above fails first in the same snapshot.
			return fmt.Errorf("spfresh build: concurrent build flipped generation %d first; this build (gen %d) is abandoned", cur, b.storage.generation)
		}
		spfreshSetGeneration(tx, b.storage, b.storage.generation)
		return spfreshAppendDeltas(tx, b.storage, []spfreshDelta{
			{op: spfreshOpGeneration, ids: []int64{b.storage.generation}},
		})
	})
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
	// Stage router outputs per ATTEMPT; commit them to the shared maps only
	// after the transaction succeeds — appending inside the retriable closure
	// leaked phantom/duplicate fineIDs into the wave-B router on retries
	// (Torvalds 094.1 #1c, codex P1).
	var stagedIDs []int64
	var stagedVecs [][]float64
	err := spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
		stagedIDs, stagedVecs = stagedIDs[:0], stagedVecs[:0]
		tx := rtx.Transaction()
		if terr := spfreshVerifyBuilderToken(tx, b.storage, b.token); terr != nil {
			return terr
		}
		row, err := spfreshTaskClaim(tx, b.storage, spfreshTaskCellfin, cellID, b.owner, spfreshLeaseDeadline(), spfreshNowMs())
		if err != nil {
			return err
		}
		if row.state == spfreshCellfinCentroidsDone || row.state == spfreshCellfinFinalized {
			// Already clustered (commit_unknown retry or crash recovery):
			// reload the COMMITTED centroids — re-clustering would mint
			// attempt-fresh IDs and duplicate rows (Torvalds 094.1 #1b).
			rows, _, _, lerr := spfreshLoadCell(tx, b.storage, cellID)
			if lerr != nil {
				return lerr
			}
			for _, r := range rows {
				vec, verr := r.row.vector()
				if verr != nil {
					return verr
				}
				stagedIDs = append(stagedIDs, r.fineID)
				stagedVecs = append(stagedVecs, vec)
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
			stagedIDs = append(stagedIDs, fineID)
			stagedVecs = append(stagedVecs, cents[c])
			deltas = append(deltas, spfreshDelta{op: spfreshOpAddFine, ids: []int64{cellID, fineID}})
		}
		// The CELL counter is the cell's FINE-CENTROID count (RFC-094 §3, the
		// 094.3 coarse-split trigger input) — owned here, where the count is
		// exact by construction (Torvalds 094.1 #2).
		spfreshCounterSet(tx, b.storage, spfreshCounterCell, cellID, int64(len(keep)))
		row.state = spfreshCellfinCentroidsDone
		tx.Set(b.storage.taskKey(spfreshTaskCellfin, cellID), encodeTaskRow(row))
		return spfreshAppendDeltas(tx, b.storage, deltas)
	})
	if err != nil {
		return err
	}
	outIDs[cellID] = append(outIDs[cellID], stagedIDs...)
	outVecs[cellID] = append(outVecs[cellID], stagedVecs...)
	return nil
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
		if terr := spfreshVerifyBuilderToken(tx, b.storage, b.token); terr != nil {
			return terr
		}
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
	q      VectorQuantizer
	config SPFreshConfig
}

func newSPFreshQuantizer(config SPFreshConfig) *spfreshQuantizer {
	return &spfreshQuantizer{q: spfreshNewRaBitQ(config), config: config}
}

func (s *spfreshQuantizer) Encode(residual []float64) []byte {
	return s.q.Encode(residual)
}

// scorer returns a per-query estimate function: allocation-free across codes
// when the quantizer is RaBitQ (the posting-scan hot path — 094.4), falling
// back to the general Distance for any other VectorQuantizer.
func (s *spfreshQuantizer) scorer(residualQuery []float64, dims int) func(code []byte) (float64, error) {
	// Cosine + zero RESIDUAL is a legitimate SPFresh input (the query equals
	// a centroid — e.g. querying the first inserted vector), but the RaBitQ
	// estimator rejects zero-norm cosine queries by design. Estimates only
	// rank candidates for the top-C cut; the sidecar re-rank is exact — so
	// rank that posting's entries with a constant best-case estimate and let
	// the re-rank sort truth (codex 094.4 r2).
	if s.config.Metric == VectorMetricCosine {
		var norm float64
		for _, v := range residualQuery {
			norm += v * v
		}
		if !(norm > 0) {
			return func([]byte) (float64, error) { return 0, nil }
		}
	}
	if rq, ok := s.q.(*rabitq.Quantizer); ok {
		sc := rq.NewScorer(residualQuery)
		return func(code []byte) (float64, error) { return sc.Score(code, dims) }
	}
	return func(code []byte) (float64, error) { return s.q.Distance(residualQuery, code, dims) }
}

// Distance estimates the metric distance between a residual query (q − c) and
// a stored residual code (v − c). For Euclidean this equals dist(q, v).
func (s *spfreshQuantizer) Distance(residualQuery []float64, code []byte, dims int) (float64, error) {
	return s.q.Distance(residualQuery, code, dims)
}

func spfreshLeaseDeadline() int64 { return spfreshNowMs() + 60_000 }
