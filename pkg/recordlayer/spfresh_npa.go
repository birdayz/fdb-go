package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// NPA reassignment (RFC-094 §6 step 3): after a split, vectors in the
// NEIGHBORING postings may now be closer to a child centroid than to their
// current copies. Re-evaluate the closure copy-set of every member of the
// K_n nearest postings around the children and move the ones whose set
// changed. Per-pk atomic: each move re-reads the pk's MEMBERSHIP with a REAL
// read inside the moving transaction — the same serialization point the
// foreground write path uses, so a concurrent update/delete of the pk aborts
// one side at the resolver and the loser's retry sees truth.
//
// Idempotence: the recompute is deterministic over authoritative state — a
// re-run (commit_unknown retry, lease takeover) finds already-moved pks'
// closure unchanged and no-ops them. The task is cleared last.

// spfreshNPABatch bounds pks moved per transaction (the §6 budget: ~10–30 KB
// of moves over 1–2 txs per split).
const spfreshNPABatch = 64

// spfreshNPARun claims and executes one NPA task. Returns
// errSPFreshNotFound-free semantics: a missing/zombie task is a clean no-op.
// wrote reports whether any write committed (lifecycle work or a stale-task
// clear) — false only for budget-free skips (task gone / foreign live lease).
func spfreshNPARun(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, parentID int64) (wrote bool, err error) {
	// Claim + plan: resolve the children and the neighborhood, and collect
	// move CANDIDATES (snapshot reads — staleness here only affects which pks
	// get considered; the move tx re-verifies everything per pk).
	type candidate struct {
		pk  tuple.Tuple
		vec []float64
	}
	var cands []candidate
	var childA, childB int64
	skipped := false // no write at all: task gone or foreign live lease
	stale := false   // stale task cleared: a write, but no move pass to run
	err = spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		cands = cands[:0]
		skipped, stale = false, false
		tx := rtx.Transaction()
		if _, cerr := spfreshTaskClaim(tx, s, spfreshTaskNPA, parentID, owner, spfreshLeaseDeadline(), spfreshNowMs()); cerr != nil {
			if errors.Is(cerr, errSPFreshNotFound) || errors.Is(cerr, errSPFreshLeaseHeld) {
				skipped = true // task gone, or another executor is mid-lifecycle
				return nil
			}
			return cerr
		}
		// The parent's posting HDR carries (cell, childA, childB); absent
		// means GC already drained the parent — stale task, delete.
		hdr, herr := tx.Get(s.postingHDRKey(parentID)).Get()
		if herr != nil {
			return herr
		}
		if hdr == nil {
			tx.Clear(s.taskKey(spfreshTaskNPA, parentID))
			stale = true
			return nil
		}
		cellID, a, b, derr := decodePostingHDR(hdr)
		if derr != nil {
			return derr
		}
		childA, childB = a, b

		// Neighborhood: the K_n nearest fine centroids around each child.
		cache := newSPFreshRoutingCache(0)
		if rerr := cache.fullReload(tx, s, s.generation); rerr != nil {
			return rerr
		}
		neighbors := map[int64]bool{}
		for _, childID := range []int64{childA, childB} {
			row, rerr := spfreshReadCentroidForWrite(tx, s, cellID, childID)
			if rerr != nil {
				if errors.Is(rerr, errSPFreshNotFound) {
					continue // child re-split/moved already; its NPA follows it
				}
				return rerr
			}
			cvec, verr := row.vector()
			if verr != nil {
				return verr
			}
			routed, rerr := cache.route(tx, s, cvec, 4, config.Kn)
			if rerr != nil {
				return rerr
			}
			for _, r := range routed {
				if r.fineID != childA && r.fineID != childB {
					neighbors[r.fineID] = true
				}
			}
		}

		// Candidates: members of the neighbor postings (snapshot, capped at
		// the one-reply contract like the query path).
		// Sidecar reads issued as one parallel burst (the plan is one tx; a
		// serial Get per member at production Lmax flirts with the 5s limit).
		type pending struct {
			pk  tuple.Tuple
			fut fdb.FutureByteSlice
		}
		var futs []pending
		for fineID := range neighbors {
			entries, _, _, _, perr := spfreshLoadPostingSnapshot(tx, s, fineID, 4*config.Lmax+1)
			if perr != nil {
				return perr
			}
			for _, e := range entries {
				futs = append(futs, pending{pk: e.pk, fut: tx.Snapshot().Get(s.sidecarKey(e.pk))})
			}
		}
		for _, f := range futs {
			data, gerr := f.fut.Get()
			if gerr != nil {
				return gerr
			}
			if data == nil {
				continue // no sidecar: nothing to re-evaluate against
			}
			v, verr := vectorcodec.Deserialize(data)
			if verr != nil {
				return verr
			}
			cands = append(cands, candidate{pk: f.pk, vec: v})
		}
		return nil
	})
	if err != nil || skipped {
		return false, err
	}
	if stale {
		return true, nil
	}

	// Move pass: per-pk atomic closure re-evaluation, batched.
	quantizer := newSPFreshQuantizer(config)
	for lo := 0; lo < len(cands); lo += spfreshNPABatch {
		hi := min(lo+spfreshNPABatch, len(cands))
		batch := cands[lo:hi]
		err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			tx := rtx.Transaction()
			// Centroid rows verified once per tx, REAL (the lifecycle fence):
			// only ACTIVE centroids participate in the recomputed copy-set.
			type centInfo struct {
				vec []float64
				ok  bool
			}
			cents := map[int64]centInfo{}
			lookup := func(fineID int64) (centInfo, error) {
				if ci, hit := cents[fineID]; hit {
					return ci, nil
				}
				cellID, ferr := spfreshFindCentroidCell(tx, s, fineID)
				if ferr != nil {
					if errors.Is(ferr, errSPFreshNotFound) {
						cents[fineID] = centInfo{}
						return centInfo{}, nil
					}
					return centInfo{}, ferr
				}
				row, rerr := spfreshReadCentroidForWrite(tx, s, cellID, fineID)
				if rerr != nil {
					if errors.Is(rerr, errSPFreshNotFound) {
						cents[fineID] = centInfo{}
						return centInfo{}, nil
					}
					return centInfo{}, rerr
				}
				if row.state != spfreshStateActive {
					cents[fineID] = centInfo{}
					return centInfo{}, nil
				}
				vec, verr := row.vector()
				if verr != nil {
					return centInfo{}, verr
				}
				ci := centInfo{vec: vec, ok: true}
				cents[fineID] = ci
				return ci, nil
			}

			for _, cand := range batch {
				// Per-pk serialization point.
				current, merr := spfreshReadMembership(tx, s, cand.pk)
				if merr != nil {
					if errors.Is(merr, errSPFreshNotFound) {
						continue // deleted since planning
					}
					return merr
				}
				// Candidate set: current copies ∪ the split children.
				ids := make([]int64, 0, len(current)+2)
				seen := map[int64]bool{}
				for _, id := range append(append([]int64{}, current...), childA, childB) {
					if !seen[id] {
						seen[id] = true
						ids = append(ids, id)
					}
				}
				var pool []spfreshCandidate
				vecsByID := map[int64][]float64{}
				for _, id := range ids {
					ci, lerr := lookup(id)
					if lerr != nil {
						return lerr
					}
					if !ci.ok {
						continue
					}
					pool = append(pool, spfreshCandidate{id: id, d2: spfreshSquaredDistance(cand.vec, ci.vec), vec: ci.vec})
					vecsByID[id] = ci.vec
				}
				if len(pool) == 0 {
					continue
				}
				spfreshSortCandidates(pool)
				kept := spfreshClosure(pool, config.Replication, config.Alpha)
				newSet := make([]int64, 0, len(kept))
				for _, k := range kept {
					newSet = append(newSet, k.id)
				}
				if spfreshSameIDSet(current, newSet) {
					continue // already optimal: idempotent no-op
				}
				keep := map[int64]bool{}
				for _, id := range newSet {
					keep[id] = true
				}
				for _, id := range current {
					if !keep[id] {
						tx.Clear(s.postingKey(id, cand.pk))
						spfreshCounterAdd(tx, s, spfreshCounterFine, id, -1)
					}
				}
				cur := map[int64]bool{}
				for _, id := range current {
					cur[id] = true
				}
				for _, id := range newSet {
					if !cur[id] {
						residual := make([]float64, len(cand.vec))
						for d := range cand.vec {
							residual[d] = cand.vec[d] - vecsByID[id][d]
						}
						tx.Set(s.postingKey(id, cand.pk), quantizer.Encode(residual))
						spfreshCounterAdd(tx, s, spfreshCounterFine, id, 1)
					}
				}
				tx.Set(s.membershipKey(cand.pk), encodeMembership(newSet))
			}
			return nil
		})
		if err != nil {
			return true, fmt.Errorf("spfresh NPA: move batch at %d: %w", lo, err)
		}
	}

	// Done: clear the task.
	return true, spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		rtx.Transaction().Clear(s.taskKey(spfreshTaskNPA, parentID))
		return nil
	})
}

// spfreshFindCentroidCell locates the cell of a fine centroid by scanning the
// coarse table and probing each cell's row (centroids are keyed (cell, fine);
// the fineID alone does not name the cell). Bounded by the coarse count; only
// lifecycle follow-ups need it — the hot paths always know the cell.
func spfreshFindCentroidCell(tx fdb.Transaction, s *spfreshStorage, fineID int64) (int64, error) {
	ids, _, err := spfreshLoadAllCoarse(tx, s)
	if err != nil {
		return 0, err
	}
	for _, cellID := range ids {
		if _, rerr := spfreshReadCentroidForWrite(tx, s, cellID, fineID); rerr == nil {
			return cellID, nil
		} else if !errors.Is(rerr, errSPFreshNotFound) {
			return 0, rerr
		}
	}
	return 0, errSPFreshNotFound
}

// spfreshSortCandidates orders by d2 ascending with id tie-breaks.
func spfreshSortCandidates(cands []spfreshCandidate) {
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && (cands[j].d2 < cands[j-1].d2 || (cands[j].d2 == cands[j-1].d2 && cands[j].id < cands[j-1].id)); j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
}

// spfreshSameIDSet reports set equality ignoring order.
func spfreshSameIDSet(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[int64]bool{}
	for _, id := range a {
		m[id] = true
	}
	for _, id := range b {
		if !m[id] {
			return false
		}
	}
	return true
}
