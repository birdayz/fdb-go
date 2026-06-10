package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// Fine-split lifecycle primitives (RFC-094 §6): SEAL → SPLIT → FORWARD as two
// single-transaction steps over the deterministic split task row. In 094.2
// these are invoked manually (tests pin the foreground-vs-split
// interleavings); the autonomous rebalancer that claims triggers and runs
// them on a timer is 094.3, as are the NPA reassignment follow-ups, merges,
// and coarse splits.
//
// Idempotence map (each step's commit_unknown retry):
//   SEAL    → claim is ours, centroid SEALED, task row carries our childIDs
//             ⇒ resume with those IDs.
//   SPLIT   → parent already FORWARD ⇒ no-op success (the task row was
//             cleared in the same committed transaction).

// spfreshSealOutcome reports what SEAL decided.
type spfreshSealOutcome struct {
	proceed bool // false: zombie/no-op (task deleted, or foreign live lease)
	childA  int64
	childB  int64
}

// spfreshSealFine is §6 step 1, one tiny transaction: claim the split task,
// verify the centroid is ACTIVE at this cell (zombie rules below), mint child
// IDs, and persist SEALED + childIDs. Sealing freezes posting APPENDS (the
// insert path's REAL state read sees SEALED and re-routes); updates/deletes
// still clear parent keys and are reconciled by SPLIT's REAL posting read.
//
// Zombie rules (RFC-094 §6): FORWARD/DEAD ⇒ the split already happened or the
// centroid is gone — delete the stale task, no-op. ABSENT at this cell ⇒ the
// row moved in a coarse split — delete the task; the next probe recreates it
// under the new cellID.
func spfreshSealFine(ctx context.Context, db *FDBDatabase, s *spfreshStorage, owner string, cellID, fineID int64) (spfreshSealOutcome, error) {
	var out spfreshSealOutcome
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		out = spfreshSealOutcome{}
		tx := rtx.Transaction()
		row, err := spfreshTaskClaim(tx, s, spfreshTaskSplit, fineID, owner, spfreshLeaseDeadline(), spfreshNowMs())
		if err != nil {
			if errors.Is(err, errSPFreshNotFound) {
				return nil // task gone or foreign live lease: nothing to do here
			}
			return err
		}

		cent, err := spfreshReadCentroidForWrite(tx, s, cellID, fineID)
		if err != nil {
			if errors.Is(err, errSPFreshNotFound) {
				tx.Clear(s.taskKey(spfreshTaskSplit, fineID))
				return nil // moved by a coarse split: next probe re-files it
			}
			return err
		}
		switch cent.state {
		case spfreshStateForward, spfreshStateDead:
			tx.Clear(s.taskKey(spfreshTaskSplit, fineID))
			return nil // zombie task
		case spfreshStateSealed:
			if row.childA == 0 {
				return fmt.Errorf("spfresh split: centroid %d SEALED but task row carries no child IDs", fineID)
			}
			out = spfreshSealOutcome{proceed: true, childA: row.childA, childB: row.childB}
			return nil // resume (commit_unknown retry or lease takeover)
		case spfreshStateActive:
			// fall through to seal
		default:
			return fmt.Errorf("spfresh split: centroid %d in unknown state %d", fineID, cent.state)
		}

		// One allocator claim per split keeps the primitive self-contained;
		// the 094.3 rebalancer amortizes a block across its whole run. The
		// ID space outlasts the waste (2^63 / 2^16 claims).
		start, err := spfreshClaimIDBlock(tx, s)
		if err != nil {
			return err
		}
		// Preserve the raw vector bytes: SEALED still routes reads.
		spfreshSaveCentroid(tx, s, cellID, fineID, encodeCentroidRowRaw(spfreshStateSealed, cent.epoch, 0, 0, cent.vecBytes))
		row.state = spfreshSplitTaskSealed
		row.childA, row.childB = start, start+1
		tx.Set(s.taskKey(spfreshTaskSplit, fineID), encodeTaskRow(row))
		out = spfreshSealOutcome{proceed: true, childA: row.childA, childB: row.childB}
		return nil
	})
	return out, err
}

// spfreshSplitFine is §6 step 2, ONE transaction (chunking is forbidden — the
// config validator bounds Lmax×maxEntryBytes against the tx limits): REAL-read
// the frozen posting (the conflict fence against concurrent update/delete
// clears), 2-means the members' sidecar vectors, write both children ACTIVE in
// the parent's cell with exact counters, rewrite moved memberships in-tx,
// clear the parent posting behind an HDR forward marker, flip the parent
// centroid FORWARD, changelog, clear the task.
//
// Degenerate postings (drained below 2 members between trigger and split) keep
// the uniform shape: both children are written, the empty one carries counter
// 0 and is reclaimed by the merge lifecycle (sub-Lmin) in 094.3.
func spfreshSplitFine(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, cellID, fineID int64, seed int64) error {
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		cent, err := spfreshReadCentroidForWrite(tx, s, cellID, fineID)
		if err != nil {
			return err
		}
		if cent.state == spfreshStateForward {
			return nil // commit_unknown retry of a committed split: no-op
		}
		if cent.state != spfreshStateSealed {
			return fmt.Errorf("spfresh split: centroid %d not SEALED (state %d) — SEAL first", fineID, cent.state)
		}
		row, err := spfreshTaskClaim(tx, s, spfreshTaskSplit, fineID, owner, spfreshLeaseDeadline(), spfreshNowMs())
		if err != nil {
			return fmt.Errorf("spfresh split: claim task for SEALED centroid %d: %w", fineID, err)
		}
		if row.state != spfreshSplitTaskSealed || row.childA == 0 {
			return fmt.Errorf("spfresh split: task row for centroid %d not SEALED with children", fineID)
		}
		childA, childB := row.childA, row.childB
		parentVec, err := cent.vector()
		if err != nil {
			return err
		}

		// The frozen membership, by REAL read (the load-bearing fence).
		entries, err := spfreshLoadPostingForSplit(tx, s, fineID)
		if err != nil {
			return err
		}
		vecs := make([][]float64, len(entries))
		futs := make([]fdb.FutureByteSlice, len(entries))
		for i, e := range entries {
			futs[i] = tx.Get(s.sidecarKey(e.pk))
		}
		for i, e := range entries {
			data, gerr := futs[i].Get()
			if gerr != nil {
				return gerr
			}
			if data == nil {
				return fmt.Errorf("spfresh split: posting %d member %v has no sidecar vector (sidecar is required for splits)", fineID, e.pk)
			}
			v, derr := vectorcodec.Deserialize(data)
			if derr != nil {
				return derr
			}
			vecs[i] = v
		}

		// 2-means over the members; degenerate sizes assign everything to
		// child A and leave child B at the parent's position, empty.
		var cents [][]float64
		var assign []int
		if len(vecs) >= 2 {
			cents, assign = spfreshKMeans(vecs, 2, seed, 25)
			if len(cents) < 2 {
				cents = append(cents, parentVec)
			}
		} else {
			cents = [][]float64{parentVec, parentVec}
			assign = make([]int, len(vecs)) // all → child A
		}

		quantizer := newSPFreshQuantizer(config)
		children := []int64{childA, childB}
		counts := []int64{0, 0}
		for i, e := range entries {
			c := assign[i]
			childID := children[c]
			residual := make([]float64, len(vecs[i]))
			for d := range vecs[i] {
				residual[d] = vecs[i][d] - cents[c][d]
			}
			tx.Set(s.postingKey(childID, e.pk), quantizer.Encode(residual))
			counts[c]++
			// Membership rewrite in-tx (REAL read: serializes with foreground
			// writers of the same pk through the resolver).
			mem, merr := spfreshReadMembership(tx, s, e.pk)
			if merr != nil {
				if errors.Is(merr, errSPFreshNotFound) {
					// Deleted between our posting read and here is impossible in
					// one tx (snapshot isolation); absent means the posting and
					// membership disagree — surface it.
					return fmt.Errorf("spfresh split: posting %d member %v has no membership row", fineID, e.pk)
				}
				return merr
			}
			for j, id := range mem {
				if id == fineID {
					mem[j] = childID
				}
			}
			tx.Set(s.membershipKey(e.pk), encodeMembership(mem))
		}

		for i, childID := range children {
			// epoch = creation time (ms): the merge lifecycle's post-split
			// cooldown reads it (T_cool, RFC-094 §6 — split↔merge oscillation
			// guard).
			spfreshSaveCentroid(tx, s, cellID, childID, encodeCentroidRow(spfreshStateActive, spfreshNowMs(), 0, 0, cents[i]))
			spfreshCounterSet(tx, s, spfreshCounterFine, childID, counts[i])
		}

		// Parent: posting cleared behind the HDR forward marker (HDR sorts
		// before every legal pk — late readers following a stale route find
		// the children), centroid FORWARD, advisory counter dropped.
		pr, err := s.postingRange(fineID)
		if err != nil {
			return err
		}
		tx.ClearRange(pr)
		tx.Set(s.postingHDRKey(fineID), encodePostingHDR(cellID, childA, childB))
		spfreshSaveCentroid(tx, s, cellID, fineID, encodeCentroidRowRaw(spfreshStateForward, cent.epoch, childA, childB, cent.vecBytes))
		tx.Clear(s.counterKey(spfreshCounterFine, fineID))
		// The cell gained a fine centroid net (+2 children, −1 parent).
		spfreshCounterAdd(tx, s, spfreshCounterCell, cellID, 1)
		// §6b trigger: the fine-split tx probes the cell's fine count (RYW
		// covers our own ADD) and files the coarse split past cellMax.
		cellCount, ccerr := spfreshCounterReadSnapshot(tx, s, spfreshCounterCell, cellID)
		if ccerr != nil {
			return ccerr
		}
		if cellCount > int64(config.CellMax) {
			if _, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskCSplit, cellID); terr != nil {
				return terr
			}
		}

		tx.Clear(s.taskKey(spfreshTaskSplit, fineID))
		// §6 step 3 follow-up: enqueue the NPA reassignment for the
		// neighborhood. Carries the children; the parent's posting HDR
		// (written above, same tx) carries the cell.
		tx.Set(s.taskKey(spfreshTaskNPA, fineID), encodeTaskRow(spfreshTaskRow{childA: childA, childB: childB}))
		return spfreshAppendDeltas(tx, s, []spfreshDelta{
			{op: spfreshOpAddFine, ids: []int64{cellID, childA}},
			{op: spfreshOpAddFine, ids: []int64{cellID, childB}},
			{op: spfreshOpForwardFine, ids: []int64{fineID, childA, childB}},
		})
	})
}
