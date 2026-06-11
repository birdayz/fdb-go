package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// Merge lifecycle (RFC-094 §6): a posting drained below Lmin folds its
// members into their nearest ACTIVE siblings. ONE transaction — the drain is
// ≤ Lmin entries by trigger definition, far inside the split's validated
// budget:
//
//  1. claim the merge task; zombie rules as for SEAL (FORWARD/DEAD/absent
//     centroid ⇒ delete task, no-op);
//  2. post-split cooldown: a child younger than T_cool is skipped (task
//     deleted; the next sub-Lmin delete probe re-files it) — the
//     split↔merge oscillation guard;
//  3. REAL-read the posting (the §6 fence: a concurrent insert appending to
//     this posting — it is ACTIVE until this very tx commits — aborts the
//     merge at the resolver, or the merge's FORWARD flip aborts the insert's
//     state read; whichever loses retries and sees truth);
//  4. per member: REAL membership read (the per-pk serialization point);
//     move the copy to the nearest ACTIVE target not already in the copy-set
//     (already-replicated-there members just drop the merged copy, keeping
//     at least one);
//  5. clear the merged posting behind an HDR FORWARD naming the two nearest
//     targets (stale readers re-probe there); flip the centroid FORWARD with
//     the same targets (a stale-cache INSERT follows them through the
//     existing write-fence forward-follow); counters; changelog; task clear.
//
// commit_unknown retry: FORWARD centroid ⇒ no-op (the committed tx cleared
// the task too).
func spfreshMergeFine(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, cellID, fineID int64) error {
	quantizer := newSPFreshQuantizer(config)
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		cent, err := spfreshReadCentroidForWrite(tx, s, cellID, fineID)
		if err != nil {
			if errors.Is(err, errSPFreshNotFound) {
				tx.Clear(s.taskKey(spfreshTaskMerge, fineID))
				return nil // moved by a coarse split: zombie
			}
			return err
		}
		if cent.state == spfreshStateForward || cent.state == spfreshStateDead {
			tx.Clear(s.taskKey(spfreshTaskMerge, fineID))
			return nil // already merged/split (commit_unknown retry lands here too)
		}
		if cent.state != spfreshStateActive {
			// SEALED: a split owns this centroid; the merge trigger is stale.
			tx.Clear(s.taskKey(spfreshTaskMerge, fineID))
			return nil
		}
		if _, cerr := spfreshTaskClaim(tx, s, spfreshTaskMerge, fineID, owner, spfreshLeaseDeadline(), spfreshNowMs()); cerr != nil {
			if errors.Is(cerr, errSPFreshNotFound) || errors.Is(cerr, errSPFreshLeaseHeld) {
				return nil // task gone, or another executor is mid-lifecycle
			}
			return cerr
		}

		// Post-split cooldown (epoch = creation ms for split children; bulk-
		// built centroids carry 0 and merge freely).
		if cent.epoch > 0 && spfreshNowMs()-cent.epoch < int64(config.CooldownSec)*1000 {
			tx.Clear(s.taskKey(spfreshTaskMerge, fineID))
			return nil
		}

		// Targets: the nearest ACTIVE siblings IN THE SAME CELL, found by
		// loading the cell directly — the posting HDR can carry only one
		// cellID, so cross-cell targets would be invisible to stale queries
		// resolving the FORWARD (codex 094.3 r1 P2); and filtering a GLOBAL
		// top-K route down to the cell could come up empty even when the cell
		// has perfectly good siblings, making an under-Lmin posting a
		// permanent non-candidate (codex 094.3 r2). The REAL cell read also
		// fences a racing coarse split of this cell.
		vec, err := cent.vector()
		if err != nil {
			return err
		}
		cellRows, _, _, lerr := spfreshLoadCellForWrite(tx, s, cellID)
		if lerr != nil {
			return lerr
		}
		var targets []spfreshMergeTarget
		for _, r := range cellRows {
			if r.fineID == fineID || r.row.state != spfreshStateActive {
				continue
			}
			tvec, verr := r.row.vector()
			if verr != nil {
				return verr
			}
			targets = append(targets, spfreshMergeTarget{fineID: r.fineID, vec: tvec})
		}
		// Nearest-first by distance to the merged centroid, capped at Kn.
		for i := 1; i < len(targets); i++ {
			for j := i; j > 0 && spfreshSquaredDistance(vec, targets[j].vec) < spfreshSquaredDistance(vec, targets[j-1].vec); j-- {
				targets[j], targets[j-1] = targets[j-1], targets[j]
			}
		}
		if len(targets) > config.Kn {
			targets = targets[:config.Kn]
		}
		if len(targets) == 0 {
			// No ACTIVE sibling in this cell to drain into (last centroid
			// standing, or the neighborhood lives in other cells): not a
			// merge candidate. Drop the task; the next probe may retry after
			// the topology changes.
			tx.Clear(s.taskKey(spfreshTaskMerge, fineID))
			return nil
		}

		// Drain: REAL posting read (the fence), per-pk membership-serialized
		// moves.
		entries, err := spfreshLoadPostingForSplit(tx, s, fineID)
		if err != nil {
			return err
		}
		counterDeltas := map[int64]int64{}
		for _, e := range entries {
			mem, merr := spfreshReadMembership(tx, s, e.pk)
			if merr != nil {
				if errors.Is(merr, errSPFreshNotFound) {
					return fmt.Errorf("spfresh merge: posting %d member %v has no membership row", fineID, e.pk)
				}
				return merr
			}
			newSet := make([]int64, 0, len(mem))
			for _, id := range mem {
				if id != fineID {
					newSet = append(newSet, id)
				}
			}
			// The member's vector, for nearest-target choice and residual
			// encode. Sidecar is authoritative; without it we cannot re-encode.
			data, gerr := tx.Get(s.sidecarKey(e.pk)).Get()
			if gerr != nil {
				return gerr
			}
			if data == nil {
				return fmt.Errorf("spfresh merge: posting %d member %v has no sidecar vector", fineID, e.pk)
			}
			v, verr := vectorcodec.Deserialize(data)
			if verr != nil {
				return verr
			}
			if len(newSet) == 0 || !spfreshAnyIn(newSet, targets) {
				// Move the copy to the nearest target. The entry condition
				// guarantees NO target is in the remaining copy-set and
				// targets is non-empty, so a best always exists — replication
				// can never hit zero here.
				best, bestD := targets[0].fineID, spfreshSquaredDistance(v, targets[0].vec)
				bestVec := targets[0].vec
				for _, t := range targets[1:] {
					if d := spfreshSquaredDistance(v, t.vec); d < bestD {
						best, bestD, bestVec = t.fineID, d, t.vec
					}
				}
				residual := make([]float64, len(v))
				for d := range v {
					residual[d] = v[d] - bestVec[d]
				}
				tx.Set(s.postingKey(best, e.pk), quantizer.Encode(residual))
				counterDeltas[best]++
				newSet = append(newSet, best)
			}
			tx.Set(s.membershipKey(e.pk), encodeMembership(newSet))
		}
		for id, delta := range counterDeltas {
			spfreshCounterAdd(tx, s, spfreshCounterFine, id, delta)
		}

		// Retire the posting behind a FORWARD to the two nearest targets.
		pr, err := s.postingRange(fineID)
		if err != nil {
			return err
		}
		tx.ClearRange(pr)
		tgtA := targets[0].fineID
		tgtB := tgtA
		if len(targets) > 1 {
			tgtB = targets[1].fineID
		}
		tx.Set(s.postingHDRKey(fineID), encodePostingHDR(cellID, tgtA, tgtB))
		spfreshSaveCentroid(tx, s, cellID, fineID, encodeCentroidRowRaw(spfreshStateForward, spfreshNowMs(), tgtA, tgtB, cent.vecBytes))
		tx.Clear(s.counterKey(spfreshCounterFine, fineID))
		// The cell lost a fine centroid.
		spfreshCounterAdd(tx, s, spfreshCounterCell, cellID, -1)
		tx.Clear(s.taskKey(spfreshTaskMerge, fineID))
		return spfreshAppendDeltas(tx, s, []spfreshDelta{
			{op: spfreshOpForwardFine, ids: []int64{fineID, tgtA, tgtB}},
		})
	})
}

// spfreshMergeTarget is one drain destination: an ACTIVE sibling centroid.
type spfreshMergeTarget struct {
	fineID int64
	vec    []float64
}

// spfreshAnyIn reports whether any of ids is a target.
func spfreshAnyIn(ids []int64, targets []spfreshMergeTarget) bool {
	for _, id := range ids {
		for _, t := range targets {
			if t.fineID == id {
				return true
			}
		}
	}
	return false
}
