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
	cleaned bool // a cleanup WRITE committed (zombie clear / lease release) —
	// counts against action budgets, unlike a pure foreign-lease skip
	childA int64
	childB int64
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
			if errors.Is(err, errSPFreshNotFound) || errors.Is(err, errSPFreshLeaseHeld) {
				return nil // task gone, or another executor is mid-lifecycle
			}
			return err
		}

		cent, err := spfreshReadCentroidForWrite(tx, s, cellID, fineID)
		if err != nil {
			if errors.Is(err, errSPFreshNotFound) {
				// Absent at OUR cellID — usually moved by a coarse split. If
				// the seal already committed (row.childA != 0), this task row
				// is the ONLY copy of the child IDs (the SEALED centroid row
				// stores children as 0,0): clearing it would strand the
				// posting SEALED forever, unresumable. Relocate first; if the
				// fine exists elsewhere, keep the task — the next scan
				// resumes the split at the right cell. Pre-seal tasks
				// (childA == 0) are safe to clear: the next probe re-files.
				if row.childA != 0 {
					if _, ferr := spfreshFindCentroidCell(tx, s, fineID); ferr == nil {
						// Moved mid-split: keep the task for a fresh scan to
						// resume at the right cell — and RELEASE the lease
						// the claim above just wrote, or every other
						// invocation (unique owners) skips this task as
						// live-foreign until expiry, stalling the split for
						// a full lease interval (codex P2).
						row.owner = ""
						row.leaseDeadlineMs = 0
						tx.Set(s.taskKey(spfreshTaskSplit, fineID), encodeTaskRow(row))
						out.cleaned = true
						return nil
					} else if !errors.Is(ferr, errSPFreshNotFound) {
						return ferr
					}
				}
				tx.Clear(s.taskKey(spfreshTaskSplit, fineID))
				out.cleaned = true
				return nil // truly gone (or pre-seal): next probe re-files it
			}
			return err
		}
		switch cent.state {
		case spfreshStateForward, spfreshStateDead:
			tx.Clear(s.taskKey(spfreshTaskSplit, fineID))
			out.cleaned = true
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
		spfreshAudit("seal", cellID, fineID, spfreshStateSealed)
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
	oversized := false
	var oversizedChildren [2]int64
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		oversized = false
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
		// Oversized posting: under sustained load a posting can balloon far
		// past the 4×Lmax inline-split ceiling before its split runs (the
		// RFC's no-chunking rule assumed writers run that ceiling inline,
		// which needs after-commit hooks the record context doesn't have).
		// Rewriting every entry + membership in ONE transaction then blows
		// FDB's 10 MB limit — writes AND the per-entry read-conflict ranges
		// (sidecar + membership point reads) count, which is why a byte
		// estimate of the writes alone under-predicted (transaction_too_large
		// at the 1M foreground fill, twice). Dispatch on the DESIGN ENVELOPE
		// instead: ≤ 4×Lmax is what the config validator proves fits; bigger
		// postings drain CHUNKED — sealing froze appends, and each chunk
		// moves pks atomically with their membership rewrites, so
		// updates/deletes serialize per pk exactly like the single-tx
		// argument.
		if len(entries) > 4*config.Lmax {
			oversized = true
			oversizedChildren = [2]int64{childA, childB}
			return nil // single-tx path abandoned; drain outside
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

		// Children that ALREADY EXIST pin the geometry: a chunked drain that
		// lost its lease after moving some chunks leaves ACTIVE children whose
		// entries are RaBitQ residuals against the committed centers — if the
		// takeover resumes here (the parent shrank back under the envelope),
		// recomputing 2-means and overwriting those centroid rows would
		// corrupt every drained entry's code and the counterSet below would
		// erase their counts. Same rule as the chunked planner's resume guard:
		// load the committed centers, assign the remainder to the nearest, and
		// counterAdd instead of counterSet.
		childrenExist := false
		var cents [][]float64
		var assign []int
		if existA, eaErr := spfreshReadCentroidForWrite(tx, s, cellID, childA); eaErr == nil {
			existB, ebErr := spfreshReadCentroidForWrite(tx, s, cellID, childB)
			if ebErr != nil {
				if !errors.Is(ebErr, errSPFreshNotFound) {
					return ebErr
				}
				return fmt.Errorf("spfresh split: child %d exists but %d does not — torn chunked plan", childA, childB)
			}
			va, vaErr := existA.vector()
			if vaErr != nil {
				return vaErr
			}
			vb, vbErr := existB.vector()
			if vbErr != nil {
				return vbErr
			}
			childrenExist = true
			cents = [][]float64{va, vb}
			assign = make([]int, len(vecs))
			for i, v := range vecs {
				if spfreshSquaredDistance(v, vb) < spfreshSquaredDistance(v, va) {
					assign[i] = 1
				}
			}
		} else if !errors.Is(eaErr, errSPFreshNotFound) {
			return eaErr
		}
		if !childrenExist {
			// Fresh split: 2-means over the members; degenerate sizes assign
			// everything to child A and leave child B at the parent's
			// position, empty.
			if len(vecs) >= 2 {
				cents, assign = spfreshKMeans(vecs, 2, seed, 25)
				if len(cents) < 2 {
					cents = append(cents, parentVec)
				}
			} else {
				cents = [][]float64{parentVec, parentVec}
				assign = make([]int, len(vecs)) // all → child A
			}
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
			if childrenExist {
				// Resume of a partially-drained chunked split: the committed
				// centroid rows stand (geometry pinned by their entries' codes)
				// and the drained entries' counts must survive.
				spfreshCounterAdd(tx, s, spfreshCounterFine, childID, counts[i])
			} else {
				// epoch = creation time (ms): the merge lifecycle's post-split
				// cooldown reads it (T_cool, RFC-094 §6 — split↔merge
				// oscillation guard).
				spfreshSaveCentroid(tx, s, cellID, childID, encodeCentroidRow(spfreshStateActive, spfreshNowMs(), 0, 0, cents[i]))
				spfreshCounterSet(tx, s, spfreshCounterFine, childID, counts[i])
			}
			// Re-trigger: under sustained write load a posting can balloon
			// far past Lmax before its split runs, so each child may be born
			// over Lmax itself — and split triggers otherwise file ONLY from
			// insert probes, so once writes stop the topology would freeze
			// with oversized postings (entries beyond the 4×Lmax read cap
			// become invisible to queries — caught by the 100k foreground
			// fill at recall 0.87, 187 fines where ~1100 were needed). On a
			// chunked resume the trigger must gate on the child's TOTAL
			// (drained + remainder, via the counter read — RYW covers the add
			// above), not the remainder alone: a child at 0.9·Lmax drained +
			// 0.5·Lmax remainder is over the envelope with no other trigger
			// site left once writes stop (Torvalds re-review S-NEW; the
			// chunked finalize already did this right).
			trigger := counts[i]
			if childrenExist {
				count, cterr := spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, childID)
				if cterr != nil {
					return cterr
				}
				trigger = count
			}
			if trigger > int64(config.Lmax) {
				if _, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskSplit, childID); terr != nil {
					return terr
				}
			}
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
		spfreshSaveCentroid(tx, s, cellID, fineID, encodeCentroidRowRaw(spfreshStateForward, spfreshNowMs(), childA, childB, cent.vecBytes))
		tx.Clear(s.counterKey(spfreshCounterFine, fineID))
		// The cell gained a fine centroid net (+2 children, −1 parent) — but a
		// resumed chunked split's planner already counted it when it created
		// the children.
		if !childrenExist {
			spfreshCounterAdd(tx, s, spfreshCounterCell, cellID, 1)
		}
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
	if err != nil || !oversized {
		return err
	}
	return spfreshSplitFineChunked(ctx, db, s, config, owner, cellID, fineID, oversizedChildren[0], oversizedChildren[1], seed)
}

// spfreshSplitFineChunked drains an OVERSIZED sealed posting into its two
// children across multiple bounded transactions. Correctness story:
//
//   - the parent is SEALED: appends are frozen (the write fence re-routes
//     inserts), so the entry set only shrinks via updates/deletes — which
//     serialize per pk through the membership reads each chunk takes;
//   - children are created ACTIVE up front (one tiny tx) so queries and the
//     write path can route to them while the drain runs; the parent stays
//     SEALED and readable, and a pk lives in exactly ONE of parent/child at
//     any commit point (the move rewrites posting + membership atomically);
//   - each chunk moves ≤ 4×Lmax entries (~bounded writes), assigning by
//     distance to the child centroids (computed by 2-means over a ≤4×Lmax
//     SAMPLE of the posting — sampling two centers is statistically fine and
//     keeps the planning read bounded);
//   - the final tx REAL-reads the (now empty) parent range — a straggler
//     update/delete that landed mid-drain conflicts it exactly like the
//     single-tx path — then writes the HDR forward marker, flips FORWARD,
//     clears the task, changelog, NPA follow-up.
//
// Idempotence: the task row carries the children (SEAL minted them); a crash
// resumes via lease takeover and the drain continues where the posting
// stands; a commit_unknown retry of the final tx no-ops on FORWARD.
//
// spfreshChunkedSplitPlan is step 1: child centroids from a bounded sample,
// written ACTIVE (idempotent: re-running overwrites with freshly sampled
// centers only while the children are still empty — entries committed to
// them pin the geometry, so re-runs skip the rewrite once rows exist), the
// parent's HDR forward marker, and the cellMax probe — all one transaction,
// so a reader routed to the SEALED parent finds the redirect the moment any
// entry can have moved.
func spfreshSplitFineChunked(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, cellID, fineID, childA, childB int64, seed int64) error {
	quantizer := newSPFreshQuantizer(config)
	chunk := 4 * config.Lmax

	centA, centB, err := spfreshChunkedSplitPlan(ctx, db, s, config, owner, cellID, fineID, childA, childB, seed)
	if err != nil {
		return fmt.Errorf("spfresh chunked split: plan: %w", err)
	}

	// 2: drain in bounded chunks (pk-atomic moves; membership is the truth).
	for {
		moved := 0
		err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			moved = 0
			tx := rtx.Transaction()
			entries, _, _, _, perr := spfreshLoadPostingSnapshot(tx, s, fineID, chunk)
			if perr != nil {
				return perr
			}
			for _, e := range entries {
				mem, merr := spfreshReadMembership(tx, s, e.pk)
				if merr != nil {
					if errors.Is(merr, errSPFreshNotFound) {
						tx.Clear(s.postingKey(fineID, e.pk)) // orphan of a racing delete
						moved++
						continue
					}
					return merr
				}
				data, gerr := tx.Get(s.sidecarKey(e.pk)).Get()
				if gerr != nil {
					return gerr
				}
				if data == nil {
					return fmt.Errorf("spfresh chunked split: member %v has no sidecar", e.pk)
				}
				v, derr := vectorcodec.Deserialize(data)
				if derr != nil {
					return derr
				}
				childID, cvec := childA, centA
				if spfreshSquaredDistance(v, centB) < spfreshSquaredDistance(v, centA) {
					childID, cvec = childB, centB
				}
				residual := make([]float64, len(v))
				for d := range v {
					residual[d] = v[d] - cvec[d]
				}
				tx.Set(s.postingKey(childID, e.pk), quantizer.Encode(residual))
				tx.Clear(s.postingKey(fineID, e.pk))
				for j, id := range mem {
					if id == fineID {
						mem[j] = childID
					}
				}
				tx.Set(s.membershipKey(e.pk), encodeMembership(mem))
				spfreshCounterAdd(tx, s, spfreshCounterFine, childID, 1)
				moved++
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("spfresh chunked split: drain: %w", err)
		}
		if moved == 0 {
			break
		}
	}

	// 3: finalize — REAL-read the empty parent (the straggler fence), HDR,
	// FORWARD, counters, task, changelog, NPA.
	return spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		cent, cerr := spfreshReadCentroidForWrite(tx, s, cellID, fineID)
		if cerr != nil {
			return cerr
		}
		if cent.state == spfreshStateForward {
			return nil // commit_unknown retry: done
		}
		entries, perr := spfreshLoadPostingForSplit(tx, s, fineID)
		if perr != nil {
			return perr
		}
		if len(entries) > 0 {
			return fmt.Errorf("spfresh chunked split: %d stragglers after drain (retry)", len(entries))
		}
		tx.Set(s.postingHDRKey(fineID), encodePostingHDR(cellID, childA, childB))
		spfreshSaveCentroid(tx, s, cellID, fineID, encodeCentroidRowRaw(spfreshStateForward, spfreshNowMs(), childA, childB, cent.vecBytes))
		tx.Clear(s.counterKey(spfreshCounterFine, fineID))
		tx.Clear(s.taskKey(spfreshTaskSplit, fineID))
		tx.Set(s.taskKey(spfreshTaskNPA, fineID), encodeTaskRow(spfreshTaskRow{childA: childA, childB: childB}))
		// Children may themselves be over Lmax: re-trigger (the same gap the
		// single-tx path closes).
		for _, childID := range []int64{childA, childB} {
			count, cterr := spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, childID)
			if cterr != nil {
				return cterr
			}
			if count > int64(config.Lmax) {
				if _, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskSplit, childID); terr != nil {
					return terr
				}
			}
		}
		return spfreshAppendDeltas(tx, s, []spfreshDelta{
			{op: spfreshOpForwardFine, ids: []int64{fineID, childA, childB}},
		})
	})
}

// spfreshChunkedSplitPlan is the chunked split's step 1 (see
// spfreshSplitFineChunked): child centroids from a ≤4×Lmax sample, written
// ACTIVE, plus the parent's HDR forward marker and the cellMax probe — one
// transaction, so a stale-routed reader finds the redirect before any entry
// can have moved. Idempotent: children that already exist pin the geometry
// (their entries' RaBitQ codes are residuals against the committed centers)
// and the resume path returns them unchanged.
func spfreshChunkedSplitPlan(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, owner string, cellID, fineID, childA, childB int64, seed int64) ([]float64, []float64, error) {
	var centA, centB []float64
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		tx := rtx.Transaction()
		row, cerr := spfreshTaskClaim(tx, s, spfreshTaskSplit, fineID, owner, spfreshLeaseDeadline(), spfreshNowMs())
		if cerr != nil {
			return cerr
		}
		if row.state != spfreshSplitTaskSealed || row.childA != childA {
			return fmt.Errorf("spfresh chunked split: task row for centroid %d lost its children", fineID)
		}
		for _, childID := range []int64{childA, childB} {
			if existing, rerr := spfreshReadCentroidForWrite(tx, s, cellID, childID); rerr == nil {
				cv, verr := existing.vector()
				if verr != nil {
					return verr
				}
				if childID == childA {
					centA = cv
				} else {
					centB = cv
				}
				continue // resume: child already exists
			} else if !errors.Is(rerr, errSPFreshNotFound) {
				return rerr
			}
		}
		if centA != nil && centB != nil {
			return nil
		}
		entries, _, _, _, perr := spfreshLoadPostingSnapshot(tx, s, fineID, 4*config.Lmax)
		if perr != nil {
			return perr
		}
		if len(entries) == 0 {
			return fmt.Errorf("spfresh chunked split: posting %d empty at planning", fineID)
		}
		vecs := make([][]float64, 0, len(entries))
		for _, e := range entries {
			data, gerr := tx.Snapshot().Get(s.sidecarKey(e.pk)).Get()
			if gerr != nil {
				return gerr
			}
			if data == nil {
				continue
			}
			v, derr := vectorcodec.Deserialize(data)
			if derr != nil {
				return derr
			}
			vecs = append(vecs, v)
		}
		if len(vecs) < 2 {
			return fmt.Errorf("spfresh chunked split: posting %d has %d sampleable vectors", fineID, len(vecs))
		}
		cents, _ := spfreshKMeans(vecs, 2, seed, 25)
		if len(cents) < 2 {
			cents = append(cents, cents[0])
		}
		centA, centB = cents[0], cents[1]
		spfreshSaveCentroid(tx, s, cellID, childA, encodeCentroidRow(spfreshStateActive, spfreshNowMs(), 0, 0, centA))
		spfreshSaveCentroid(tx, s, cellID, childB, encodeCentroidRow(spfreshStateActive, spfreshNowMs(), 0, 0, centB))
		spfreshCounterSet(tx, s, spfreshCounterFine, childA, 0)
		spfreshCounterSet(tx, s, spfreshCounterFine, childB, 0)
		spfreshCounterAdd(tx, s, spfreshCounterCell, cellID, 1)
		// The parent's HDR forward marker lands HERE, with the children — not
		// at finalize. The drain spans many transactions, and a reader whose
		// routing cache still holds only the SEALED parent must find the
		// redirect IN the parent's posting or every entry already moved is
		// invisible to it until a cache refresh (the single-tx split publishes
		// children + HDR atomically for exactly this reason; codex
		// final-gauntlet P1). The searcher scores residual parent entries AND
		// follows the HDR in the same burst, so mid-drain reads see the whole
		// set; both posting loaders skip HDR rows.
		tx.Set(s.postingHDRKey(fineID), encodePostingHDR(cellID, childA, childB))
		// §6b trigger, same as the single-tx path: the cell gained a fine
		// centroid net (+2 children, −1 parent-to-be) — file the coarse split
		// past cellMax or maintenance reports drained while the cell stays an
		// oversized routing hotspot (codex final-gauntlet P2).
		cellCount, ccerr := spfreshCounterReadSnapshot(tx, s, spfreshCounterCell, cellID)
		if ccerr != nil {
			return ccerr
		}
		if cellCount > int64(config.CellMax) {
			if _, terr := spfreshTaskSetIfAbsent(tx, s, spfreshTaskCSplit, cellID); terr != nil {
				return terr
			}
		}
		return spfreshAppendDeltas(tx, s, []spfreshDelta{
			{op: spfreshOpAddFine, ids: []int64{cellID, childA}},
			{op: spfreshOpAddFine, ids: []int64{cellID, childB}},
		})
	})
	return centA, centB, err
}
