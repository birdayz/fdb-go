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
		// FORWARD/DEAD rows are COMPLETED lifecycles: they never become
		// ACTIVE again, so deferring on them would block the split forever —
		// but they MUST MOVE with the partition, not drop: GC discovers
		// purgeable fineIDs by scanning cells' centroid rows, so a dropped
		// tombstone's posting HDR (and any live residual §6's drain protects)
		// would leak forever (Torvalds 094.3 #2). They're header-sized rows;
		// they ride to the nearest new cell and stay out of the k-means and
		// the cell counters (which count ACTIVE centroids).
		allRows, _, _, lerr := spfreshLoadCellForWrite(tx, s, cellID)
		if lerr != nil {
			return lerr
		}
		rows := allRows[:0]
		var tombstones []spfreshCellRow
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
			default: // FORWARD/DEAD
				tombstones = append(tombstones, r)
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
		// A one-sided partition (k-means collapse on near-duplicate vectors)
		// would publish an EMPTY ACTIVE cell — which ensureCell treats as an
		// error, failing every query that probes it (codex 094.3 r1 P2).
		// Nothing meaningful to split either.
		partition := make([]int, 2)
		for _, c := range assign {
			partition[c]++
		}
		if partition[0] == 0 || partition[1] == 0 {
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
			spfreshAudit("cmove", cellID, r.fineID, r.row.state)
			spfreshSaveCentroid(tx, s, cells[c], r.fineID, encodeCentroidRowRaw(r.row.state, r.row.epoch, r.row.childA, r.row.childB, r.row.vecBytes))
			counts[c]++
			// Repair the PAUSING window: while this csplit deferred, the
			// starvation guard suppressed every fine-split PROBE for the
			// cell — postings that crossed Lmax in that window have NO task,
			// and post-quiescence nothing re-probes them (the 300k/1M fills
			// ended with an empty queue and 4k-entry postings; recall
			// collapsed). Re-file triggers for any oversized posting as part
			// of completing the split that caused the pause.
			cnt, cerr := spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, r.fineID)
			if cerr != nil {
				return cerr
			}
			if cnt > int64(config.Lmax) {
				if _, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskSplit, r.fineID); terr != nil {
					return terr
				}
			}
		}
		// Tombstones ride to their nearest new cell (GC discovery), without
		// counting toward the ACTIVE-centroid cell counters.
		spfreshAudit("csplit-counts", cellID, int64(len(rows)), byte(len(tombstones)))
		for _, r := range tombstones {
			v, verr := r.row.vector()
			c := 0
			if verr == nil && spfreshSquaredDistance(v, cents[1]) < spfreshSquaredDistance(v, cents[0]) {
				c = 1
			}
			spfreshSaveCentroid(tx, s, cells[c], r.fineID, encodeCentroidRowRaw(r.row.state, r.row.epoch, r.row.childA, r.row.childB, r.row.vecBytes))
		}
		deltas := make([]spfreshDelta, 0, 3)
		for i, id := range cells {
			// The routing vector is the FRESH 2-means center (recomputed at
			// every cell split by construction — §6b).
			spfreshSaveCoarse(tx, s, id, encodeCentroidRow(spfreshStateActive, 0, 0, 0, cents[i]))
			spfreshCounterSet(tx, s, spfreshCounterCell, id, counts[i])
			deltas = append(deltas, spfreshDelta{op: spfreshOpAddCell, ids: []int64{id}})
			// Re-trigger: a child born already over cellMax must split again.
			// The only OTHER csplit trigger site is the fine-split commit, so
			// without this the topology stops converging the moment writes
			// stop — a cell that grew far past cellMax under sustained load
			// stays a routing hotspot forever (caught by the 100k foreground
			// fill: one cell held 270+ fine centroids and reads degraded to
			// ~107 ms / 0.90 recall).
			if counts[i] > int64(config.CellMax) {
				if _, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskCSplit, id); terr != nil {
					return terr
				}
			}
		}

		// Retire the old cell: centroid range cleared behind the HDR forward
		// marker (stale L2 fetchers follow it), COARSE row FORWARD.
		cr, rerr := s.cellRange(cellID)
		if rerr != nil {
			return rerr
		}
		spfreshAudit("csplit-rangeclear", cellID, -1, 0)
		tx.ClearRange(cr)
		tx.Set(s.centroidHDRKey(cellID), encodeCellHDR(cellA, cellB))
		spfreshSaveCoarse(tx, s, cellID, encodeCentroidRowRaw(spfreshStateForward, spfreshNowMs(), cellA, cellB, coarse.vecBytes))
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
