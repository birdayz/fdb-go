package recordlayer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
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
	pk  tuple.Tuple // index-trimmed primary key — the staging-row key component
	vec []float64
	// fullPK is the UN-trimmed record primary key. Staging keys use the trimmed
	// pk (above), but parallel-staging shard boundaries (RFC-103) must bound the
	// records subspace, which is keyed by the full record PK — the two differ
	// whenever the index key overlaps PK components (TrimPrimaryKey drops them).
	fullPK tuple.Tuple
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

	// Fine-ID pool for the wave-A fan-out. Claiming an allocator block
	// inside each per-cell transaction put a REAL RMW on the single META
	// allocator key into every k-means-length transaction: with 8 workers
	// every overlapping pair conflicted, commits serialized at ~1 per tx
	// lifetime, and each abort redid the per-cell clustering. The pool
	// claims blocks in their own tiny standalone transactions instead;
	// doling sub-ranges is mutex-local.
	idMu   sync.Mutex
	idNext int64 // next undoled ID; pool is [idNext, idEnd)
	idEnd  int64
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
	if err := b.coarsePass(ctx, sample, len(inputs), seed); err != nil {
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

// coarsePass is §8 steps 1+2: coarse k-means over the SAMPLE (the maintainer
// reservoir-samples its record scan past spfreshCoarseSampleCap) and the
// COARSE/cellfin row writes. totalN is the FULL dataset size: K₀ must cover
// every record, not just the sample — deriving it from a capped sample would
// shrink the topology by the sampling ratio. Committing the coarse table
// BEFORE the assignment scan is what closes the lost-record window: from
// this point on a foreground write can always route itself.
// K₀ = N·r / (avgFill · cellTarget); avgFill ≈ ⅔·Lmax (RFC-094 §8).
func (b *spfreshBuilder) coarsePass(ctx context.Context, sample [][]float64, totalN int, seed int64) error {
	if len(sample) == 0 {
		return fmt.Errorf("spfresh build: no inputs")
	}
	if totalN < len(sample) {
		totalN = len(sample)
	}
	avgFill := (2 * b.config.Lmax) / 3
	k0 := (totalN*b.config.Replication + avgFill*b.config.CellTarget - 1) / (avgFill * b.config.CellTarget)
	if k0 < 1 {
		k0 = 1
	}
	if k0 > spfreshMaxDeltasPerTx {
		// coarsePass writes one changelog delta per cell in ONE transaction
		// (idempotent under retry); the changelog's 2-byte user-version caps a
		// tx at spfreshMaxDeltasPerTx deltas. At defaults that is ~267M records
		// — BELOW the K0>sample cliff (~1.0B) — so guard it here with a clear
		// message instead of failing deep in spfreshAppendDeltas with a generic
		// "too many deltas". Lifting it needs the coarse-table commit to chunk
		// the changelog across transactions (not yet implemented) (codex).
		return fmt.Errorf("spfresh build: K0 %d exceeds the single-tx changelog limit (%d cells); the coarse-table build does not yet chunk the changelog across transactions",
			k0, spfreshMaxDeltasPerTx)
	}
	if k0 > len(sample) {
		// The k > n clamp in spfreshKMeans would silently shrink the very
		// topology K₀-from-totalN exists to protect. This is the hard cliff
		// — K0 = totalN·r/(avgFill·cellTarget) outruns the 250k sample at
		// ~1.0B records at defaults — not the sample-QUALITY envelope
		// (points-per-centroid thins long before that). Fail loudly —
		// raising spfreshCoarseSampleCap is the fix, not building a coarse
		// table sized by the sampling ratio.
		return fmt.Errorf("spfresh build: K0 %d exceeds the %d-point training sample (totalN %d outgrew spfreshCoarseSampleCap %d)",
			k0, len(sample), totalN, spfreshCoarseSampleCap)
	}
	coarse, _ := spfreshKMeansBuild(sample, k0, seed, 25)
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
// spfreshBuildCellWorkers bounds the per-cell wave fan-out. The waves are
// FDB-transaction + small-k-means work per INDEPENDENT cell (each cell owns
// its cellfin task row, centroid rows, postings), so cells parallelize
// safely; the bound keeps the builder from monopolizing the cluster — at 1M
// the sequential walk over ~3k cells dominated the build wall-clock long
// after the coarse k-means was parallelized.
const spfreshBuildCellWorkers = 8

// claimFineIDs returns n consecutive fine IDs from the builder's pool,
// refilling it with a standalone one-key transaction when it runs dry.
// Attempt-fresh semantics are preserved: a retried wave-A transaction doles
// fresh IDs and the skipped ones are never reused (the ID space outlasts the
// waste — 2^63 across 2^16-sized blocks).
func (b *spfreshBuilder) claimFineIDs(ctx context.Context, n int) (int64, error) {
	b.idMu.Lock()
	defer b.idMu.Unlock()
	if int64(n) > spfreshIDBlockSize {
		return 0, fmt.Errorf("spfresh build: %d fine IDs exceed one allocator block (%d)", n, spfreshIDBlockSize)
	}
	if b.idNext+int64(n) > b.idEnd {
		var start int64
		if err := spfreshRun(ctx, b.db, func(rtx *FDBRecordContext) error {
			var cerr error
			start, cerr = spfreshClaimIDBlock(rtx.Transaction(), b.storage)
			return cerr
		}); err != nil {
			return 0, err
		}
		b.idNext, b.idEnd = start, start+spfreshIDBlockSize
	}
	start := b.idNext
	b.idNext += int64(n)
	return start, nil
}

// forEachCellParallel runs fn over the builder's cells with bounded
// concurrency, stopping at the first error (in-flight cells finish; their
// re-run is idempotent via the cellfin state machine anyway).
func (b *spfreshBuilder) forEachCellParallel(fn func(cellID int64) error) error {
	var next, errOnce atomic.Int64
	var firstErr error
	var wg sync.WaitGroup
	workers := spfreshBuildCellWorkers
	if workers > len(b.cellIDs) {
		workers = len(b.cellIDs)
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(b.cellIDs) || errOnce.Load() != 0 {
					return
				}
				if err := fn(b.cellIDs[i]); err != nil {
					if errOnce.CompareAndSwap(0, 1) {
						firstErr = err
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

func (b *spfreshBuilder) finalize(ctx context.Context, seed int64) error {
	fineIDs := make(map[int64][]int64)      // cellID -> fine ids
	fineVecs := make(map[int64][][]float64) // cellID -> fine vectors
	var mapsMu sync.Mutex                   // guards the shared maps across cell workers
	if err := b.forEachCellParallel(func(cellID int64) error {
		if err := b.waveA(ctx, cellID, seed, &mapsMu, fineIDs, fineVecs); err != nil {
			return fmt.Errorf("spfresh build: wave A cell %d: %w", cellID, err)
		}
		return nil
	}); err != nil {
		return err
	}

	router := b.buildRouter(fineIDs, fineVecs)
	if err := b.forEachCellParallel(func(cellID int64) error {
		if err := b.waveB(ctx, cellID, router); err != nil {
			return fmt.Errorf("spfresh build: wave B cell %d: %w", cellID, err)
		}
		return nil
	}); err != nil {
		return err
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
		// The build's per-cell bookkeeping (Cellfin rows) is dead the moment
		// the generation publishes — clear it in the SAME transaction, so
		// the task subspace holds only live maintenance triggers. The
		// sweeper's pending-work probe and every rebalancer scan depend on
		// "tasks non-empty ⇔ work to do"; leaking build garbage here made a
		// freshly built index look permanently busy.
		cellfinRange, rerr := fdb.PrefixRange(b.storage.tasks.Pack(tuple.Tuple{spfreshTaskCellfin}))
		if rerr != nil {
			return rerr
		}
		tx.ClearRange(cellfinRange)
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
func (b *spfreshBuilder) waveA(ctx context.Context, cellID int64, seed int64, mapsMu *sync.Mutex, outIDs map[int64][]int64, outVecs map[int64][][]float64) error {
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
			if errors.Is(err, errSPFreshNotFound) {
				// The flip cleared the Cellfin rows: the build already
				// published — a late retry of this wave is a no-op.
				return nil
			}
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
		cents, assign := spfreshKMeansBuild(vecs, k, seed+cellID, 25)

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

		start, err := b.claimFineIDs(ctx, len(keep))
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
	mapsMu.Lock()
	outIDs[cellID] = append(outIDs[cellID], stagedIDs...)
	outVecs[cellID] = append(outVecs[cellID], stagedVecs...)
	mapsMu.Unlock()
	return nil
}

// spfreshBuildRouter routes a vector to fine centroids two-level (RFC-099):
// the w_b nearest coarse cells, then the closure over their fines — the same
// candidate set a query would probe, not a flat scan of the global fine table.
type spfreshBuildRouter struct {
	coarseIDs    []int64
	coarseVecs   [][]float64
	cellFineIDs  map[int64][]int64
	cellFineVecs map[int64][][]float64
	// RFC-101 prune metadata, derived once from the maps above:
	// cellFineDist[cellID][j] is the L2 distance from the cell's coarse centroid
	// to its j-th fine (same order as cellFineVecs); cellRadius[cellID] is the
	// max over those. The triangle inequality d(v,f) >= |d(v,c) - d(c,f)| then
	// lets assign skip whole cells / individual fines that cannot enter the
	// pool — EXACT, so the returned top-k (and the assignment) is unchanged.
	cellFineDist map[int64][]float64
	cellRadius   map[int64]float64
	w            int
}

func (b *spfreshBuilder) buildRouter(fineIDs map[int64][]int64, fineVecs map[int64][][]float64) *spfreshBuildRouter {
	r := &spfreshBuildRouter{
		coarseIDs:    b.cellIDs,
		coarseVecs:   b.coarseVec,
		cellFineIDs:  fineIDs,
		cellFineVecs: fineVecs,
		w:            b.config.BuildAssignCells,
	}
	r.precomputePrune()
	return r
}

// precomputePrune derives the RFC-101 cell-centroid→fine L2 distances and per-cell
// radii from the router's coarse + fine maps. One pass over all fines (~6,100 at
// 1M), amortized over every per-vector assign. coarseVecs[i] is the centroid for
// coarseIDs[i] (the cell a query and the build both route on — same primitive).
func (r *spfreshBuildRouter) precomputePrune() {
	r.cellFineDist = make(map[int64][]float64, len(r.coarseIDs))
	r.cellRadius = make(map[int64]float64, len(r.coarseIDs))
	for i, cid := range r.coarseIDs {
		cvec := r.coarseVecs[i]
		fvs := r.cellFineVecs[cid]
		fd := make([]float64, len(fvs))
		maxR := 0.0
		for j, fv := range fvs {
			d := math.Sqrt(spfreshSquaredDistance(cvec, fv))
			fd[j] = d
			if d > maxR {
				maxR = d
			}
		}
		r.cellFineDist[cid] = fd
		r.cellRadius[cid] = maxR
	}
}

// assign returns the closure copy-set (fineIDs) and the fine vectors for
// residual encoding. It routes to the w_b nearest cells (two-level) and runs
// the SAME bounded-widening closure as before over their fines.
//
// The candidate pool is wider than the replica target so the closure's RNG
// rule has same-direction candidates to skip, and it widens past a fixed pool
// that would truncate just ahead of a diverse in-ratio candidate (codex
// 094.4 r2) — but the widening is BOUNDED at two doublings (4× the base pool).
// Unbounded "widen until the ratio break" was quadratic at 1M density: hundreds
// of fines sit inside α²·d²(c1) and the RNG rejects them all as same-direction,
// so the pool doubled to the entire fine table PER VECTOR. Past the cap the
// same argument as the insert path's cap applies: under-replication only costs
// recall, never records.
//
// On the two-level restriction: the default w_b == w_q (the query probe width),
// so the build considers exactly the cells a query for this vector probes. A
// diverse replica that would land OUTSIDE those cells is not lost recall — a
// query for this vector never probes there either (and a query for some OTHER
// vector near that cell finds its own near replicas, not this one). The flat
// scan's habit of placing such far replicas was wasted work, not recall.
// Measured: recall is identical at w_b ∈ {w_q, larger, flat} even when w_b
// gathers ~20% of cells (RFC-099). NPA is split-local (NPA-bounded, SPFresh
// §3.3) and does NOT re-closure globally, so query-reachability — not NPA — is
// the justification.
func (r *spfreshBuildRouter) assign(vec []float64, rep int, alpha float64) (ids []int64, fvecs [][]float64) {
	// Two-level: the w_b nearest coarse cells (ascending d²). With fewer than
	// w_b cells (small index) this is every cell — identical to a flat scan, so
	// small indexes are unaffected.
	cells := spfreshNearestK(vec, r.coarseIDs, r.coarseVecs, r.w)

	base := spfreshClosurePool(rep)
	for pool := base; ; pool *= 2 {
		cands := r.gatherTopK(vec, cells, pool)
		kept := spfreshClosure(cands, rep, alpha)
		if len(kept) >= rep || len(cands) < pool || pool >= 4*base ||
			(len(cands) > 0 && cands[len(cands)-1].d2 > alpha*alpha*cands[0].d2) {
			for _, c := range kept {
				ids = append(ids, c.id)
				fvecs = append(fvecs, c.vec)
			}
			return ids, fvecs
		}
	}
}

// spfreshPruneLowerBound returns a GUARANTEED lower bound on the true distance
// d(v,f), given d(v,c)=dvc and d(c,f)=dcf — both rounded sqrts of dims-term
// summed squared distances. The triangle inequality gives d(v,f) >= dvc - dcf in
// exact arithmetic, but a fixed slack cannot make that safe: when dvc ≈ dcf
// (a fine almost on the query but far from its cell centroid) the subtraction
// CANCELS catastrophically and the relative error of dvc-dcf is unbounded as
// d(v,f) → 0 (codex). So we subtract an ABSOLUTE error term that scales with the
// operand magnitude (dvc+dcf), not with the difference: it bounds the two sqrt
// roundings, the dims-term squared-distance sum roundoff, and the subtraction.
// The honest per-distance relative error is ~(n/2 + 6.5)·u (Higham: tree-summed
// squared distance over n=dims lanes + the sqrt), and 2^-51 = 4u, so the
// subtracted (dims+2)·4u·(dvc+dcf) dominates that absolute perturbation of
// dvc-dcf with margin (verified: 20M adversarial trials, 0 violations, tightest
// margin ~11u below the true d²). When cancellation dominates, the returned
// bound goes <= 0 and the caller must
// not prune (it scores the fine exactly). This keeps gatherTopK EXACT for all
// float64 inputs. Real SIFT data (uint8 0..255) never cancels; this guards the
// arbitrary-magnitude float64 vectors the index accepts.
func spfreshPruneLowerBound(dvc, dcf float64, dims int) float64 {
	errAbs := (dvc + dcf) * float64(dims+2) * 0x1p-51
	return dvc - dcf - errAbs
}

// spfreshMinPrunableWorst gates the prune to the NORMAL float64 range. The
// magnitude-scaled error term in spfreshPruneLowerBound is relative, so it
// underflows in the subnormal regime (squared distances < 0x1p-1022, i.e.
// coordinates ~1e-160) and the squaring lb*lb can round up by a subnormal ulp at
// an exact tie — a wrong skip (codex P3). When the pool-th best is subnormal we
// simply don't prune; the fine is scored exactly, so gatherTopK stays
// byte-identical. If worst is normal, any lb*lb that could exceed it is itself
// normal, so the proven normal-range analysis holds. No real vector data is
// subnormal (SIFT is uint8; embeddings are O(1)); this is pure exactness rigor.
const spfreshMinPrunableWorst = 0x1p-1022

// gatherTopK returns the `pool` nearest fines across the given cells, pruned by
// the L2 triangle inequality. EXACT: byte-identical to spfreshNearestK over a
// flat gather of the same cells' fines — it offers candidates in the same order
// (cell-ascending, then cellFineVecs order) and only skips fines whose
// roundoff-conservative lower bound (spfreshPruneLowerBound) already exceeds the
// pool-th best actual distance, so they could not have been in the pool. The
// prune activates only once the pool is full (and the boundary is a normal
// float, spfreshMinPrunableWorst), so it never prevents the pool from filling
// when enough fines exist (⇒ the `len < pool` widening termination in assign is
// unchanged). cells must be ascending by d².
func (r *spfreshBuildRouter) gatherTopK(vec []float64, cells []spfreshCandidate, pool int) []spfreshCandidate {
	dims := len(vec)
	top := newSpfreshBoundedTopK(pool)
	for _, cell := range cells {
		fvs := r.cellFineVecs[cell.id]
		fids := r.cellFineIDs[cell.id]
		fd := r.cellFineDist[cell.id]
		dvc := math.Sqrt(cell.d2)
		if top.full() && top.worst() >= spfreshMinPrunableWorst {
			// No fine in this cell can be closer than d(v,c) - radius(c); a
			// non-positive bound is vacuous (cancellation) and is not pruned.
			if lb := spfreshPruneLowerBound(dvc, r.cellRadius[cell.id], dims); lb > 0 && lb*lb > top.worst() {
				continue
			}
		}
		for j, fv := range fvs {
			if top.full() && top.worst() >= spfreshMinPrunableWorst {
				if lb := spfreshPruneLowerBound(dvc, fd[j], dims); lb > 0 && lb*lb > top.worst() {
					continue
				}
			}
			top.offer(fids[j], spfreshSquaredDistance(vec, fv), fv)
		}
	}
	return top.out
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
			if errors.Is(err, errSPFreshNotFound) {
				// The flip cleared the Cellfin rows: the build already
				// published — a late retry of this wave is a no-op.
				return nil
			}
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
	// estimator rejects zero-norm cosine queries by design. The cosine
	// estimate formula is 0.5·euclideanSquare, and at a zero query that
	// degenerates to 0.5·‖residual_c‖² — so score these codes with the
	// EUCLIDEAN estimator (no zero-query guard, identical encoded fields)
	// and keep the 0.5 cosine scale: the estimates stay monotone within the
	// posting AND comparable across postings (a constant best-case estimate
	// here created an Lmax-sized tie that could evict the true match from
	// the top-C cut before the exact re-rank — codex 094.4 r2+r3).
	if s.config.Metric == VectorMetricCosine {
		var norm float64
		for _, v := range residualQuery {
			norm += v * v
		}
		if !(norm > 0) {
			sc := rabitq.NewQuantizer(rabitq.MetricEuclidean, s.config.NumExBits).NewScorer(residualQuery)
			return func(code []byte) (float64, error) {
				est, err := sc.Score(code, dims)
				return 0.5 * est, err
			}
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

// spfreshLeaseDurationMs is the task-lease length (RFC-094 §6 — long enough
// to cover a multi-tx lifecycle; an executor that dies mid-lifecycle has its
// lease reclaimed after it expires). Var, not const, so concurrency tests can
// force mid-lifecycle takeover between two executors with a short lease.
var spfreshLeaseDurationMs int64 = 60_000

func spfreshLeaseDeadline() int64 { return spfreshNowMs() + spfreshLeaseDurationMs }
