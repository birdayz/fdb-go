package recordlayer

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/vectorcodec"
)

// GC (RFC-094 §6): FORWARD/DEAD rows past the horizon are purged. The FORWARD
// flip stamps its epoch field with the flip time, so age is row-local. The
// posting of a retired centroid is expected to hold ONLY its HDR; any
// residual entry is DRAINED VIA MEMBERSHIP RE-CHECK, never blind-cleared —
// an entry whose membership still names the retired posting is live data
// (re-homed to the nearest ACTIVE sibling, exactly like a merge drain); an
// entry its membership disclaims is an orphan and only then cleared. That
// rule is the §6 invariant the churn tests assert.
//
// The changelog is trimmed to the same horizon: entries older than
// (read version − horizon) clear, and the trim boundary is recorded in
// META/horizon — a cache whose cursor predates it MUST full-reload (its
// incremental history is gone), which refresh() enforces.

// spfreshVersionsPerMs is FDB's commit-version advance rate (~1e6/sec).
const spfreshVersionsPerMs = 1000

// spfreshGCSweep purges retired topology older than horizonMs and trims the
// changelog. Returns the number of purged rows. Safe to run concurrently with
// everything else: purges REAL-read the row and re-verify state + age.
func spfreshGCSweep(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, horizonMs int64) (int, error) {
	// Collect candidates (snapshot scan; each purge re-verifies in its own tx).
	type fineRef struct{ cellID, fineID int64 }
	var fines []fineRef
	var cells []int64
	now := spfreshNowMs()
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		fines, cells = fines[:0], cells[:0]
		tx := rtx.Transaction()
		ids, rows, lerr := spfreshLoadAllCoarse(tx, s)
		if lerr != nil {
			return lerr
		}
		for i, cellID := range ids {
			if rows[i].state == spfreshStateForward && rows[i].epoch > 0 && now-rows[i].epoch >= horizonMs {
				cells = append(cells, cellID)
			}
			cellRows, _, _, cerr := spfreshLoadCell(tx, s, cellID)
			if cerr != nil {
				return cerr
			}
			for _, r := range cellRows {
				if (r.row.state == spfreshStateForward || r.row.state == spfreshStateDead) &&
					r.row.epoch > 0 && now-r.row.epoch >= horizonMs {
					fines = append(fines, fineRef{cellID: cellID, fineID: r.fineID})
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	purged := 0
	quantizer := newSPFreshQuantizer(config)
	for _, ref := range fines {
		err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			tx := rtx.Transaction()
			cent, cerr := spfreshReadCentroidForWrite(tx, s, ref.cellID, ref.fineID)
			if cerr != nil {
				if errors.Is(cerr, errSPFreshNotFound) {
					return nil // already purged
				}
				return cerr
			}
			if cent.state != spfreshStateForward && cent.state != spfreshStateDead {
				return nil // re-verify failed: not retired after all
			}
			if cent.epoch == 0 || spfreshNowMs()-cent.epoch < horizonMs {
				return nil
			}

			// Residual drain: REAL posting read; re-home entries the
			// membership still claims, clear only disclaimed orphans.
			entries, perr := spfreshLoadPostingForSplit(tx, s, ref.fineID)
			if perr != nil {
				return perr
			}
			for _, e := range entries {
				mem, merr := spfreshReadMembership(tx, s, e.pk)
				if merr != nil {
					if errors.Is(merr, errSPFreshNotFound) {
						continue // record deleted: the range clear below removes it
					}
					return merr
				}
				claims := false
				for _, id := range mem {
					if id == ref.fineID {
						claims = true
					}
				}
				if !claims {
					continue // orphan: cleared with the posting range below
				}
				// Live residual: re-home to the nearest ACTIVE sibling not
				// already in the copy-set (the merge-drain rule).
				if rerr := spfreshRehome(tx, s, config, quantizer, e.pk, ref.fineID, mem); rerr != nil {
					return rerr
				}
			}

			pr, rerr := s.postingRange(ref.fineID)
			if rerr != nil {
				return rerr
			}
			tx.ClearRange(pr)
			tx.Clear(s.centroidKey(ref.cellID, ref.fineID))
			tx.Clear(s.counterKey(spfreshCounterFine, ref.fineID))
			return spfreshAppendDeltas(tx, s, []spfreshDelta{
				{op: spfreshOpDeadFine, ids: []int64{ref.fineID}},
			})
		})
		if err != nil {
			return purged, err
		}
		purged++
	}

	for _, cellID := range cells {
		err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			tx := rtx.Transaction()
			coarse, cerr := spfreshReadCoarseForWrite(tx, s, cellID)
			if cerr != nil {
				if errors.Is(cerr, errSPFreshNotFound) {
					return nil
				}
				return cerr
			}
			if coarse.state != spfreshStateForward || coarse.epoch == 0 || spfreshNowMs()-coarse.epoch < horizonMs {
				return nil
			}
			// The cell must be empty (its fine rows moved at split, its HDR is
			// the only resident); fine rows still present mean fine GC hasn't
			// caught up — skip this round.
			rows, _, _, lerr := spfreshLoadCell(tx, s, cellID)
			if lerr != nil {
				return lerr
			}
			if len(rows) > 0 {
				return nil
			}
			tx.Clear(s.centroidHDRKey(cellID))
			tx.Clear(s.coarseKey(cellID))
			return spfreshAppendDeltas(tx, s, []spfreshDelta{
				{op: spfreshOpDeadCell, ids: []int64{cellID}},
			})
		})
		if err != nil {
			return purged, err
		}
		purged++
	}

	// Changelog trim: clear entries older than (read version − horizon) and
	// record the boundary in META/horizon for refresh()'s staleness check.
	err = spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		rv, rverr := tx.GetReadVersion().Get()
		if rverr != nil {
			return rverr
		}
		horizonVersion := rv - horizonMs*spfreshVersionsPerMs
		if horizonVersion <= 0 {
			return nil
		}
		var boundary [10]byte // versionstamp shape: 8B version + 2B batch
		binary.BigEndian.PutUint64(boundary[:8], uint64(horizonVersion))
		end := fdb.Key(append(s.changelog.Bytes(), boundary[:]...))
		begin := fdb.Key(s.changelog.Bytes())
		tx.ClearRange(fdb.KeyRange{Begin: begin, End: end})
		tx.Set(s.metaKey(spfreshMetaHorizon), end)
		return nil
	})
	if err != nil {
		return purged, err
	}
	return purged, nil
}

// spfreshRehome moves a live residual copy from a retired posting to the
// nearest ACTIVE sibling not already in the pk's copy-set (the merge-drain
// rule, applied at GC time).
func spfreshRehome(tx fdb.Transaction, s *spfreshStorage, config SPFreshConfig, quantizer *spfreshQuantizer, pk tuple.Tuple, deadID int64, mem []int64) error {
	data, gerr := tx.Get(s.sidecarKey(pk)).Get()
	if gerr != nil {
		return gerr
	}
	if data == nil {
		return fmt.Errorf("spfresh gc: residual %v has no sidecar vector", pk)
	}
	v, derr := vectorcodec.Deserialize(data)
	if derr != nil {
		return derr
	}
	cache := newSPFreshRoutingCache(0)
	if rerr := cache.fullReload(tx, s, s.generation); rerr != nil {
		return rerr
	}
	routed, rerr := cache.route(tx, s, v, 4, config.Kn)
	if rerr != nil {
		return rerr
	}
	inSet := map[int64]bool{}
	newSet := make([]int64, 0, len(mem))
	for _, id := range mem {
		if id != deadID {
			inSet[id] = true
			newSet = append(newSet, id)
		}
	}
	for _, r := range routed {
		if r.fineID == deadID || inSet[r.fineID] {
			continue
		}
		row, rerr := spfreshReadCentroidForWrite(tx, s, r.cellID, r.fineID)
		if rerr != nil {
			if errors.Is(rerr, errSPFreshNotFound) {
				continue
			}
			return rerr
		}
		if row.state != spfreshStateActive {
			continue
		}
		cvec, verr := row.vector()
		if verr != nil {
			return verr
		}
		residual := make([]float64, len(v))
		for d := range v {
			residual[d] = v[d] - cvec[d]
		}
		tx.Set(s.postingKey(r.fineID, pk), quantizer.Encode(residual))
		spfreshCounterAdd(tx, s, spfreshCounterFine, r.fineID, 1)
		newSet = append(newSet, r.fineID)
		break
	}
	if len(newSet) == 0 {
		return fmt.Errorf("spfresh gc: residual %v has no re-home target (no ACTIVE sibling)", pk)
	}
	tx.Set(s.membershipKey(pk), encodeMembership(newSet))
	return nil
}
