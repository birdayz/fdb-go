package recordlayer

import (
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// SPFresh foreground write path (RFC-094 §5), running inside the caller's
// record-save transaction. The conflict story, restated at the site that
// depends on it:
//
//   - the generation read is REAL — a build flip committing mid-insert aborts
//     this write at the resolver (it must re-route into the new generation);
//     inserts never conflict each other on it (read-read).
//   - fine-centroid STATE reads are REAL — a split SEALing the centroid aborts
//     a straggler insert; an insert that commits first is in the frozen
//     posting the split reads (RFC-094 §6, sound both directions).
//   - the MEMBERSHIP read is REAL — the same-pk serialization point between
//     concurrent writers of one record and between writers and split moves.
//   - posting/sidecar/membership writes are BLIND, counters are atomic ADDs —
//     inserts of distinct pks into the same posting never conflict each other.
//   - the split/merge probes are SAMPLED snapshot counter reads; the trigger
//     write is a REAL-read Set-if-absent on the deterministic task key
//     (the conflict range is the point — a live claim must not be clobbered).

const (
	// spfreshInsertProbeCells is the routing width (cells) for inserts — wide
	// enough to surface r·closure candidates plus non-ACTIVE fallbacks without
	// paying the query path's full w.
	spfreshInsertProbeCells = 8
	// spfreshInsertCandidates is the fine-candidate pool: the closure picks at
	// most Replication from the front; the tail is the next-nearest fallback
	// supply when fenced candidates turn out SEALED/FORWARD/absent.
	spfreshInsertCandidates = 16
	// spfreshProbeSampleEvery samples 1-in-N writes for the counter probe
	// (RFC-094 §5: probes are sampled so the trigger key never becomes a
	// per-write hot read).
	spfreshProbeSampleEvery = 8
)

// spfreshWriteContext carries the per-write resolved state.
type spfreshWriteContext struct {
	storage *spfreshStorage
	cache   *spfreshRoutingCache
}

// spfreshResolveForWrite resolves the readable generation with the REAL-read
// write fence and ensures the routing cache is loaded. No readable generation
// means the index was never built: the 094.x contract is build-then-write.
func (m *spfreshIndexMaintainer) spfreshResolveForWrite() (*spfreshWriteContext, error) {
	metaStorage := newSPFreshStorage(m.indexSubspace, 0)
	gen, err := spfreshReadGenerationForWrite(m.tx, metaStorage)
	if errors.Is(err, errSPFreshNotFound) {
		// Cold start (RFC-094 §6b): a READABLE index with no generation is an
		// EMPTY index — the SQL flow is CREATE INDEX then INSERT, no bulk
		// build. Bootstrap generation 1 with one cell in this same
		// transaction; the REAL generation read above fences racing first
		// inserts (both see absent, both write, the loser aborts at the
		// resolver and its retry sees the bootstrap). Fine centroids arrive
		// with the inserts themselves; fine and coarse splits grow the shape
		// from there — growth never requires a retrain.
		gen, err = m.spfreshBootstrap(metaStorage)
	}
	if err != nil {
		return nil, err
	}
	storage := newSPFreshStorage(m.indexSubspace, gen)
	cache := spfreshCacheFor(m.indexSubspace, gen)
	if !cache.ready(gen) {
		if err := cache.fullReload(m.tx, storage, gen); err != nil {
			return nil, fmt.Errorf("spfresh index %q: routing reload: %w", m.index.Name, err)
		}
	}
	return &spfreshWriteContext{storage: storage, cache: cache}, nil
}

// spfreshInsert indexes one (pk, vector): route on cache → closure copy-set →
// REAL state fence per chosen centroid (non-ACTIVE/absent drops to the
// next-nearest) → membership/posting/sidecar writes + counter ADDs → sampled
// split probe. An existing membership row (update) is cleared from keys
// derived from this same-tx read.
func (m *spfreshIndexMaintainer) spfreshInsert(wc *spfreshWriteContext, pk tuple.Tuple, vec []float64) error {
	routed, err := wc.cache.routeForWrite(m.tx, wc.storage, vec, spfreshInsertProbeCells, spfreshInsertCandidates)
	if err != nil {
		if !errors.Is(err, errSPFreshEmptyRouting) {
			return fmt.Errorf("spfresh index %q: route insert: %w", m.index.Name, err)
		}
		// Freshly bootstrapped index: the cache has no coarse cells yet.
		if rerr := wc.cache.fullReload(m.tx, wc.storage, wc.storage.generation); rerr != nil {
			return rerr
		}
		routed = nil
	}
	if len(routed) == 0 {
		// No fine centroids exist yet: this insert mints the first (§6b).
		first, ferr := m.spfreshFirstCentroid(wc.storage, vec)
		if ferr != nil && !errors.Is(ferr, errSPFreshStaleRoute) {
			return ferr
		}
		if ferr == nil {
			routed = first
		} else if routed, err = wc.cache.routeForWrite(m.tx, wc.storage, vec, spfreshInsertProbeCells, spfreshInsertCandidates); err != nil {
			return fmt.Errorf("spfresh index %q: route insert (post-mint): %w", m.index.Name, err)
		}
	}
	ierr := m.spfreshInsertRouted(wc.storage, routed, pk, vec)
	if !errors.Is(ierr, errSPFreshStaleRoute) {
		return ierr
	}
	// Every routed candidate failed the authoritative fence: the cache is
	// stale beyond the in-place fallbacks (e.g. a COARSE split relocated the
	// centroids — forward-follow looks in the parent's old cell and finds
	// nothing). §5's "re-route conservatively": reload the cache in this
	// transaction and retry once.
	if rerr := wc.cache.fullReload(m.tx, wc.storage, wc.storage.generation); rerr != nil {
		return fmt.Errorf("spfresh index %q: stale-route reload: %w", m.index.Name, rerr)
	}
	routed, err = wc.cache.routeForWrite(m.tx, wc.storage, vec, spfreshInsertProbeCells, spfreshInsertCandidates)
	if err != nil {
		return fmt.Errorf("spfresh index %q: route insert (reloaded): %w", m.index.Name, err)
	}
	if ierr := m.spfreshInsertRouted(wc.storage, routed, pk, vec); ierr != nil {
		return fmt.Errorf("spfresh index %q: insert after cache reload: %w", m.index.Name, ierr)
	}
	return nil
}

// errSPFreshStaleRoute: every routed candidate failed the authoritative state
// fence — the routing cache is stale beyond in-place recovery and must be
// reloaded.
var errSPFreshStaleRoute = errors.New("spfresh: routed candidates all stale (cache reload required)")

// spfreshBootstrap establishes generation 1 with a single empty cell — the
// §6b cold-start shape. Runs inside the caller's transaction; the caller has
// already REAL-read the generation as absent (the racing-bootstrap fence).
func (m *spfreshIndexMaintainer) spfreshBootstrap(metaStorage *spfreshStorage) (int64, error) {
	storage := newSPFreshStorage(m.indexSubspace, 1)
	cellID, err := spfreshClaimIDBlock(m.tx, storage)
	if err != nil {
		return 0, err
	}
	// The cell's routing vector is the zero vector — with one cell, routing
	// is degenerate anyway, and the first coarse split recomputes fresh
	// 2-means centers by construction.
	spfreshSaveCoarse(m.tx, storage, cellID, encodeCentroidRow(spfreshStateActive, 0, 0, 0, make([]float64, m.config.NumDimensions)))
	spfreshCounterSet(m.tx, storage, spfreshCounterCell, cellID, 0)
	spfreshSetGeneration(m.tx, metaStorage, 1)
	if err := spfreshAppendDeltas(m.tx, storage, []spfreshDelta{
		{op: spfreshOpAddCell, ids: []int64{cellID}},
		{op: spfreshOpGeneration, ids: []int64{1}},
	}); err != nil {
		return 0, err
	}
	return 1, nil
}

// spfreshFirstCentroid creates the index's first fine centroid AT the
// inserted vector (§6b: "one cell, one fine centroid (first vector)") and
// returns it as the routed candidate set. Same-transaction with the insert;
// racing first inserts conflict on the cell's centroid range read.
func (m *spfreshIndexMaintainer) spfreshFirstCentroid(storage *spfreshStorage, vec []float64) ([]spfreshRouted, error) {
	ids, _, err := spfreshLoadAllCoarseForWrite(m.tx, storage)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("spfresh index %q: no coarse cells (corrupt bootstrap)", m.index.Name)
	}
	cellID := ids[0]
	// REAL-read the cell: two racing first inserts both see it empty and both
	// mint a centroid — the range conflict aborts one, whose retry routes to
	// the winner's centroid normally.
	rows, _, _, lerr := spfreshLoadCellForWrite(m.tx, storage, cellID)
	if lerr != nil {
		return nil, lerr
	}
	if len(rows) > 0 {
		return nil, errSPFreshStaleRoute // someone else just minted: re-route
	}
	fineID, err := spfreshClaimIDBlock(m.tx, storage)
	if err != nil {
		return nil, err
	}
	rt, rerr := vectorcodecRoundtrip(vec)
	if rerr != nil {
		return nil, rerr
	}
	spfreshSaveCentroid(m.tx, storage, cellID, fineID, encodeCentroidRow(spfreshStateActive, 0, 0, 0, rt))
	spfreshCounterSet(m.tx, storage, spfreshCounterFine, fineID, 0)
	spfreshCounterAdd(m.tx, storage, spfreshCounterCell, cellID, 1)
	if err := spfreshAppendDeltas(m.tx, storage, []spfreshDelta{
		{op: spfreshOpAddFine, ids: []int64{cellID, fineID}},
	}); err != nil {
		return nil, err
	}
	return []spfreshRouted{{cellID: cellID, fineID: fineID, state: spfreshStateActive, vec: rt, d2: 0}}, nil
}

// vectorcodecRoundtrip pins a vector to its stored fp16 form (one table, one
// set of bytes — the Torvalds 094.2 #3 rule).
func vectorcodecRoundtrip(vec []float64) ([]float64, error) {
	return vectorcodec.Deserialize(vectorcodec.SerializeHalf(vec))
}

// spfreshInsertRouted is the post-routing half of the insert; the WriteOnly
// staging path routes within a single FINALIZED cell instead of on the cache.
func (m *spfreshIndexMaintainer) spfreshInsertRouted(storage *spfreshStorage, routed []spfreshRouted, pk tuple.Tuple, vec []float64) error {
	// Fence: verify candidates ACTIVE with REAL state reads, in nearest-first
	// order, until Replication verified candidates are in hand. The cache said
	// ACTIVE; the authoritative row decides: SEALED/absent ⇒ drop and take the
	// next-nearest (RFC-094 §5 step 2); FORWARD ⇒ a stale cache routed us to a
	// split parent — FOLLOW its children from the authoritative row instead of
	// skipping, or an insert near a freshly split centroid fails with no
	// candidates until the cache reloads (codex 094.2 r1 P1). Worklist kept
	// d2-sorted as children are spliced in; visit budget bounds forward chains.
	// verified is kept d2-ASCENDING by sorted insertion: spfreshClosure
	// assumes nearest-first (verified[0] is its c1), and a followed FORWARD
	// child can be NEARER than an already-verified candidate — appending
	// would hand closure a wrong c1 and mis-assign the insert (codex 094.2
	// r2). The cutoff is sound for the same reason: stop only when the
	// sorted worklist's head can no longer improve the best Replication.
	verified := make([]spfreshCandidate, 0, m.config.Replication+2)
	vecs := make(map[int64][]float64, m.config.Replication)
	cells := make(map[int64]int64, m.config.Replication)
	work := append([]spfreshRouted(nil), routed...)
	seen := make(map[int64]bool, len(work))
	for examined := 0; len(work) > 0 && examined < 4*(len(routed)+2); examined++ {
		cand := work[0]
		work = work[1:]
		if seen[cand.fineID] {
			continue
		}
		seen[cand.fineID] = true
		if len(verified) >= m.config.Replication && cand.d2 >= verified[m.config.Replication-1].d2 {
			break
		}
		row, rerr := spfreshReadCentroidForWrite(m.tx, storage, cand.cellID, cand.fineID)
		if rerr != nil {
			if errors.Is(rerr, errSPFreshNotFound) {
				continue
			}
			return rerr
		}
		switch row.state {
		case spfreshStateActive:
			// verified below
		case spfreshStateForward:
			for _, childID := range []int64{row.childA, row.childB} {
				if childID == 0 || seen[childID] {
					continue
				}
				crow, cerr := spfreshReadCentroidForWrite(m.tx, storage, cand.cellID, childID)
				if cerr != nil {
					if errors.Is(cerr, errSPFreshNotFound) {
						continue
					}
					return cerr
				}
				cvec, cverr := crow.vector()
				if cverr != nil {
					return cverr
				}
				child := spfreshRouted{cellID: cand.cellID, fineID: childID, vec: cvec, d2: spfreshSquaredDistance(vec, cvec)}
				at := len(work)
				for i := range work {
					if child.d2 < work[i].d2 {
						at = i
						break
					}
				}
				work = append(work[:at], append([]spfreshRouted{child}, work[at:]...)...)
			}
			continue
		default:
			continue // SEALED/DEAD: next-nearest
		}
		cvec, verr := row.vector()
		if verr != nil {
			return verr
		}
		nc := spfreshCandidate{id: cand.fineID, d2: cand.d2}
		at := len(verified)
		for i := range verified {
			if nc.d2 < verified[i].d2 {
				at = i
				break
			}
		}
		verified = append(verified[:at], append([]spfreshCandidate{nc}, verified[at:]...)...)
		vecs[cand.fineID] = cvec
		cells[cand.fineID] = cand.cellID
	}
	if len(verified) == 0 {
		return fmt.Errorf("spfresh index %q: no ACTIVE fine centroid among %d routed candidates: %w", m.index.Name, len(routed), errSPFreshStaleRoute)
	}
	kept := spfreshClosure(verified, m.config.Replication, m.config.Alpha)

	// Same-pk serialization point: an existing copy-set means this is an
	// update — clear the old keys derived from THIS read (a split moving the
	// pk concurrently rewrites membership, so the resolver orders us).
	oldIDs, merr := spfreshReadMembership(m.tx, storage, pk)
	if merr != nil && !errors.Is(merr, errSPFreshNotFound) {
		return merr
	}
	for _, fineID := range oldIDs {
		m.tx.Clear(storage.postingKey(fineID, pk))
		spfreshCounterAdd(m.tx, storage, spfreshCounterFine, fineID, -1)
	}

	quantizer := newSPFreshQuantizer(m.config)
	fp16 := vectorcodec.SerializeHalf(vec)
	newIDs := make([]int64, 0, len(kept))
	for _, c := range kept {
		cvec := vecs[c.id]
		residual := make([]float64, len(vec))
		for d := range vec {
			residual[d] = vec[d] - cvec[d]
		}
		m.tx.Set(storage.postingKey(c.id, pk), quantizer.Encode(residual))
		spfreshCounterAdd(m.tx, storage, spfreshCounterFine, c.id, 1)
		newIDs = append(newIDs, c.id)
	}
	m.tx.Set(storage.membershipKey(pk), encodeMembership(newIDs))
	if m.config.Sidecar {
		m.tx.Set(storage.sidecarKey(pk), fp16)
	}

	// Sampled split probe (RFC-094 §5 step 2, trigger only — the consuming
	// rebalancer and the 4×Lmax inline split are 094.3). Deterministic by pk
	// hash so tests can pin it; the unconditional 2×Lmax branch bounds how far
	// an unlucky sampling run can overshoot before a trigger lands.
	for _, fineID := range newIDs {
		count, cerr := spfreshCounterReadSnapshot(m.tx, storage, spfreshCounterFine, fineID)
		if cerr != nil {
			return cerr
		}
		if count <= int64(m.config.Lmax) {
			continue
		}
		if spfreshSampledProbe(pk) || count > int64(2*m.config.Lmax) {
			// Starvation guard (§6b): a pending coarse split past its defer
			// limit pauses fine-split issuance for the cell until it runs.
			paused, perr := spfreshCSplitPaused(m.tx, storage, cells[fineID])
			if perr != nil {
				return perr
			}
			if paused {
				continue
			}
			if _, terr := spfreshTaskSetIfAbsent(m.tx, storage, spfreshTaskSplit, fineID); terr != nil {
				return terr
			}
		}
	}
	return nil
}

// spfreshDelete removes one pk: membership-driven (no tombstones, RFC-094 §5)
// — clear the posting copies named by the same-tx membership read, the
// sidecar, and the membership row; counter −1s; sampled merge probe. A pk
// with no membership row was never indexed: no-op.
func (m *spfreshIndexMaintainer) spfreshDelete(storage *spfreshStorage, pk tuple.Tuple) error {
	ids, err := spfreshReadMembership(m.tx, storage, pk)
	if err != nil {
		if errors.Is(err, errSPFreshNotFound) {
			return nil
		}
		return err
	}
	for _, fineID := range ids {
		m.tx.Clear(storage.postingKey(fineID, pk))
		spfreshCounterAdd(m.tx, storage, spfreshCounterFine, fineID, -1)
	}
	m.tx.Clear(storage.membershipKey(pk))
	m.tx.Clear(storage.sidecarKey(pk))

	if spfreshSampledProbe(pk) {
		for _, fineID := range ids {
			count, cerr := spfreshCounterReadSnapshot(m.tx, storage, spfreshCounterFine, fineID)
			if cerr != nil {
				return cerr
			}
			if count < int64(m.config.Lmin()) {
				if _, terr := spfreshTaskSetIfAbsent(m.tx, storage, spfreshTaskMerge, fineID); terr != nil {
					return terr
				}
			}
		}
	}
	return nil
}

// spfreshSampledProbe selects ~1-in-spfreshProbeSampleEvery pks for the
// counter probe. Hash-based, not random: a transaction retry must make the
// same decision, and tests pin probe behavior with chosen pks.
func spfreshSampledProbe(pk tuple.Tuple) bool {
	h := fnv.New64a()
	_, _ = h.Write(pk.Pack())
	return h.Sum64()%spfreshProbeSampleEvery == 0
}
