package recordlayer

import (
	"context"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
)

// Coarse-level growth (RFC-094 §6b): metadata-only cell splits. POSTINGS and
// MEMBERSHIP are keyed by fineID alone, so restructuring cells moves NO
// posting data — only the cell's small CENTROIDS rows. ONE transaction:
//
//   - claim; COARSE row must be ACTIVE (zombie rules as everywhere);
//   - REAL-read every fine CENTROIDS row in the cell and require ALL ACTIVE —
//     DEFER otherwise (§6b composability rule: a fine split holds its
//     centroid SEALED across its window, so a coarse split can never relocate
//     a row out from under a fine lifecycle's guard re-read; the REAL reads
//     mean a racing fine SEAL aborts one side at the resolver);
//   - 2-means over the fine centroid VECTORS; two fresh cells with the
//     2-means centers as their routing vectors (recomputed at every cell
//     split by construction); fine rows rewritten under their new cells
//     (fineID/state/epoch preserved — fine counters are keyed by fineID and
//     don't move); exact CELL counters; old cell's centroid range cleared
//     behind CENTROIDS/(old, HDR) = FORWARD(cells) for stale L2 fetchers;
//     COARSE/(old) flips FORWARD; changelog; task cleared.
//
// Starvation guard (§6b): a hotspot cell's continuous fine splits could keep
// some centroid SEALED under exactly the load that needs the cell split.
// Deferrals are counted in the task row (childA); past
// spfreshCSplitDeferLimit the row enters PAUSING state, which the fine-split
// PROBES honor by skipping issuance for this cell until the coarse split
// completes and clears the task.
//
// Inserts need no fence (§2 table): fineIDs are stable; a state read against
// a moved row sees absent-at-cell and re-routes.

// Coarse-split task states (the task row's state byte).
const (
	spfreshCSplitPending byte = 0
	spfreshCSplitPausing byte = 1 // defer limit hit: fine-split issuance paused
)

func spfreshCoarseSplit(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, cellID int64, seed int64) error {
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		coarse, err := spfreshReadCoarseForWrite(tx, s, cellID)
		if err != nil {
			if errors.Is(err, errSPFreshNotFound) {
				tx.Clear(s.taskKey(spfreshTaskCSplit, cellID))
				return nil // cell gone (already split): zombie
			}
			return err
		}
		if coarse.state != spfreshStateActive {
			tx.Clear(s.taskKey(spfreshTaskCSplit, cellID))
			return nil // FORWARD/DEAD: commit_unknown retry lands here too
		}
		row, cerr := spfreshTaskClaim(tx, s, spfreshTaskCSplit, cellID, owner, spfreshLeaseDeadline(), spfreshNowMs())
		if cerr != nil {
			if errors.Is(cerr, errSPFreshNotFound) {
				return nil // task gone or foreign live lease
			}
			return cerr
		}

		// All fine rows, REAL (the composability fence with fine lifecycles).
		// SEALED defers — a fine split owns that centroid mid-window and the
		// coarse split must not relocate it from under the guard re-read.
		// FORWARD/DEAD rows are COMPLETED lifecycles: tombstones whose
		// forwarding lives in the posting HDRs — they are dropped, not moved
		// (a stale-cache reader of the old (cell, fine) location sees absent
		// and re-routes; deferring on them would block the coarse split
		// forever, since they never become ACTIVE again).
		allRows, _, _, lerr := spfreshLoadCell(tx, s, cellID)
		if lerr != nil {
			return lerr
		}
		rows := allRows[:0]
		for _, r := range allRows {
			switch r.row.state {
			case spfreshStateSealed:
				// Defer: count it; past the limit, pause fine-split issuance
				// for this cell (the starvation guard).
				row.childA++
				if row.childA >= spfreshCSplitDeferLimit {
					row.state = spfreshCSplitPausing
				}
				tx.Set(s.taskKey(spfreshTaskCSplit, cellID), encodeTaskRow(row))
				return nil
			case spfreshStateActive:
				rows = append(rows, r)
			}
		}
		if len(rows) < 2 {
			tx.Clear(s.taskKey(spfreshTaskCSplit, cellID))
			return nil // nothing to split (merges drained it since the trigger)
		}

		// 2-means over the fine centroid vectors.
		vecs := make([][]float64, len(rows))
		for i, r := range rows {
			v, verr := r.row.vector()
			if verr != nil {
				return verr
			}
			vecs[i] = v
		}
		cents, assign := spfreshKMeans(vecs, 2, seed, 25)
		if len(cents) < 2 {
			// Degenerate (identical vectors): nothing meaningful to split.
			tx.Clear(s.taskKey(spfreshTaskCSplit, cellID))
			return nil
		}

		start, aerr := spfreshClaimIDBlock(tx, s)
		if aerr != nil {
			return aerr
		}
		cellA, cellB := start, start+1
		cells := []int64{cellA, cellB}
		counts := []int64{0, 0}
		for i, r := range rows {
			c := assign[i]
			// Rewrite under the new cell: fineID, state, epoch, children and
			// the raw vector bytes preserved verbatim.
			spfreshSaveCentroid(tx, s, cells[c], r.fineID, encodeCentroidRowRaw(r.row.state, r.row.epoch, r.row.childA, r.row.childB, r.row.vecBytes))
			counts[c]++
		}
		deltas := make([]spfreshDelta, 0, 3)
		for i, id := range cells {
			// The routing vector is the FRESH 2-means center (recomputed at
			// every cell split by construction — §6b).
			spfreshSaveCoarse(tx, s, id, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cents[i]))
			spfreshCounterSet(tx, s, spfreshCounterCell, id, counts[i])
			deltas = append(deltas, spfreshDelta{op: spfreshOpAddCell, ids: []int64{id}})
		}

		// Retire the old cell: centroid range cleared behind the HDR forward
		// marker (stale L2 fetchers follow it), COARSE row FORWARD.
		cr, rerr := s.cellRange(cellID)
		if rerr != nil {
			return rerr
		}
		tx.ClearRange(cr)
		tx.Set(s.centroidHDRKey(cellID), encodeCellHDR(cellA, cellB))
		spfreshSaveCoarse(tx, s, cellID, encodeCentroidRowRaw(spfreshStateForward, 0, cellA, cellB, coarse.vecBytes))
		tx.Clear(s.counterKey(spfreshCounterCell, cellID))
		tx.Clear(s.taskKey(spfreshTaskCSplit, cellID))
		deltas = append(deltas, spfreshDelta{op: spfreshOpForwardCell, ids: []int64{cellID, cellA, cellB}})
		return spfreshAppendDeltas(tx, s, deltas)
	})
}

// spfreshCSplitPaused reports whether fine-split issuance for the cell is
// paused by the starvation guard — the write path's split probe checks it
// before filing a new fine-split trigger (one extra read, on the SAMPLED
// probe path only).
func spfreshCSplitPaused(tx fdb.ReadTransaction, s *spfreshStorage, cellID int64) (bool, error) {
	data, err := tx.Get(s.taskKey(spfreshTaskCSplit, cellID)).Get()
	if err != nil {
		return false, fmt.Errorf("spfresh: read csplit task: %w", err)
	}
	if data == nil {
		return false, nil
	}
	row, derr := decodeTaskRow(data)
	if derr != nil {
		return false, derr
	}
	return row.state == spfreshCSplitPausing, nil
}
