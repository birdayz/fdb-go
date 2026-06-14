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

// RFC-104 online assignment refinement: re-evaluate vectors' closure copy-set
// against the CURRENT (converged) topology and move the changed ones, recovering
// the closure replication that fast ingest never fired (a vector closure-
// replicated against the coarse insertion-time topology lands at 1.0× and is
// never re-evaluated as the topology refines). Generalizes the NPA per-pk
// closure-move (spfresh_npa.go) from split neighborhoods to the global
// population. Measured to recover fast-fill recall to the bulk baseline.

// spfreshRefineScanBatch bounds the membership-key snapshot scan per tx;
// spfreshRefineMoveBatch bounds pks re-evaluated+moved per move tx (each move is
// a sidecar read + a route + per-new-copy REAL centroid reads + posting writes,
// so keep it well under the 5 s / 10 MB limits — the NPA move budget shape).
const (
	spfreshRefineScanBatch = 5000
	spfreshRefineMoveBatch = 100
)

// spfreshRefineKc is the candidate-pool width for the closure re-eval: it MUST
// match the bulk build's MAX pool (4·spfreshClosurePool, the cap the build's
// router widens to). A narrower pool re-evaluates a converged index with too few
// candidates and drops replicas the wide build placed — regressing recall and
// breaking the no-op-on-converged property (codex r1 P2). =64 for the default
// Replication=2, 96/128 for r=3/4.
func spfreshRefineKc(config SPFreshConfig) int { return 4 * spfreshClosurePool(config.Replication) }

// spfreshRefinePKInTx re-evaluates one pk's closure copy-set against the routing
// cache and moves it if the set changed, in the caller's transaction. Returns
// true iff it moved (wrote). The move is fenced two ways: (1) a REAL read of the
// pk's MEMBERSHIP (the per-pk serialization point — a concurrent update/delete
// aborts one side at the resolver, the loser's retry sees truth); (2) a REAL
// read of each kept NEW copy's centroid state, rejecting non-ACTIVE — cache.route
// returns ACTIVE+SEALED, and the move must not deposit a posting into a fine that
// seals/splits concurrently (the NPA fence). Idempotent: an already-optimal pk
// re-evaluates to the same set → spfreshSameIDSet no-op.
func spfreshRefinePKInTx(tx fdb.Transaction, s *spfreshStorage, config SPFreshConfig, quantizer *spfreshQuantizer, cache *spfreshRoutingCache, kc int, pk tuple.Tuple) (bool, error) {
	current, merr := spfreshReadMembership(tx, s, pk)
	if merr != nil {
		if errors.Is(merr, errSPFreshNotFound) {
			return false, nil // deleted since the scan
		}
		return false, merr
	}
	// The vector lives in the fp16 sidecar (the exact re-rank store).
	sc, gerr := tx.Snapshot().Get(s.sidecarKey(pk)).Get()
	if gerr != nil {
		return false, gerr
	}
	if sc == nil {
		return false, nil // no sidecar: nothing to re-evaluate against
	}
	vec, derr := vectorcodec.Deserialize(sc)
	if derr != nil {
		return false, derr
	}
	// Candidate pool = the vector ROUTED against the current topology (the kc
	// nearest ACTIVE/SEALED fines over its w nearest cells) — discovers fines the
	// coarse insertion-time topology rejected.
	routed, rerr := cache.route(tx, s, vec, config.BuildAssignCells, kc)
	if rerr != nil {
		return false, rerr
	}
	if len(routed) == 0 {
		return false, nil
	}
	pool := make([]spfreshCandidate, 0, len(routed))
	vecByID := make(map[int64][]float64, len(routed))
	cellByID := make(map[int64]int64, len(routed))
	for _, r := range routed {
		pool = append(pool, spfreshCandidate{id: r.fineID, d2: r.d2, vec: r.vec})
		vecByID[r.fineID] = r.vec
		cellByID[r.fineID] = r.cellID
	}
	spfreshSortCandidates(pool)
	kept := spfreshClosure(pool, config.Replication, config.Alpha)
	closed := make([]int64, 0, len(kept))
	for _, k := range kept {
		closed = append(closed, k.id)
	}
	// Topology lifecycle fence: REAL-read each kept NEW copy's centroid and drop
	// non-ACTIVE/missing (NPA's fence). Existing copies (already in `current`) get
	// no new write, so they skip re-verification.
	curSet := make(map[int64]bool, len(current))
	for _, id := range current {
		curSet[id] = true
	}
	newSet := make([]int64, 0, len(closed))
	for _, id := range closed {
		if curSet[id] {
			newSet = append(newSet, id)
			continue
		}
		row, ferr := spfreshReadCentroidForWrite(tx, s, cellByID[id], id)
		if ferr != nil {
			if errors.Is(ferr, errSPFreshNotFound) {
				continue // fine gone (split/GC): drop this new copy
			}
			return false, ferr
		}
		if row.state != spfreshStateActive {
			continue // sealing/splitting: don't write into it
		}
		newSet = append(newSet, id)
	}
	// Never unindex on refinement: if the fence rejected EVERY new candidate (the
	// pk's whole closure is concurrently sealing) and no existing copy survived,
	// leave the current copies in place — they keep the vector findable, and the
	// rebalancer + a later refine pass recover it once the fines settle. Without
	// this, the clear loop below would wipe `current` and orphan the vector
	// (@claude review; NPA can't hit this — it filters non-ACTIVE out of the pool
	// pre-closure, so an all-sealed neighborhood short-circuits on len(pool)==0).
	if len(newSet) == 0 {
		return false, nil
	}
	if spfreshSameIDSet(current, newSet) {
		return false, nil // already optimal: idempotent no-op
	}
	keep := make(map[int64]bool, len(newSet))
	for _, id := range newSet {
		keep[id] = true
	}
	for _, id := range current {
		if !keep[id] {
			tx.Clear(s.postingKey(id, pk))
			spfreshCounterAdd(tx, s, spfreshCounterFine, id, -1)
		}
	}
	for _, id := range newSet {
		if !curSet[id] {
			residual := make([]float64, len(vec))
			for d := range vec {
				residual[d] = vec[d] - vecByID[id][d]
			}
			tx.Set(s.postingKey(id, pk), quantizer.Encode(residual))
			spfreshCounterAdd(tx, s, spfreshCounterFine, id, 1)
		}
	}
	tx.Set(s.membershipKey(pk), encodeMembership(newSet))
	return true, nil
}

// spfreshLoadRefineCache loads the routing cache for the current generation,
// re-validated each call: a generation flip (bulk rebuild) must not route the
// remaining pks against dead topology (codex/Torvalds P2).
func spfreshLoadRefineCache(ctx context.Context, db *FDBDatabase, s *spfreshStorage) (*spfreshRoutingCache, error) {
	cache := newSPFreshRoutingCache(0)
	if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		return cache.fullReload(rtx.Transaction(), s, s.generation)
	}); rerr != nil {
		return nil, fmt.Errorf("spfresh refine: load routing cache: %w", rerr)
	}
	return cache, nil
}

// spfreshRefineAll is the one-shot full-pass prototype (no budget/cursor) used by
// the RFC-104 recovery validation. Production callers use RefineSPFreshIndex.
// Returns the number of pks moved.
func spfreshRefineAll(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig) (int, error) {
	mr, err := fdb.PrefixRange(s.membership.Bytes())
	if err != nil {
		return 0, fmt.Errorf("spfresh refine: membership range: %w", err)
	}
	var pks []tuple.Tuple
	begin := mr.Begin
	for {
		var nextBegin fdb.Key
		// Truncate back to the pre-batch length at the top of the closure: a
		// snapshot-scan retry (e.g. past_version) must not append this batch's pks
		// twice (the spfreshRefineRound pattern — @claude review).
		baseLen := len(pks)
		if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			nextBegin = nil
			pks = pks[:baseLen]
			kvs, kerr := rtx.Transaction().Snapshot().GetRange(
				fdb.KeyRange{Begin: begin, End: mr.End},
				fdb.RangeOptions{Limit: spfreshRefineScanBatch, Mode: fdb.StreamingModeWantAll},
			).GetSliceWithError()
			if kerr != nil {
				return kerr
			}
			for _, kv := range kvs {
				pk, uerr := s.membership.Unpack(kv.Key)
				if uerr != nil {
					return fmt.Errorf("spfresh refine: unpack membership key: %w", uerr)
				}
				pks = append(pks, pk)
			}
			if len(kvs) == spfreshRefineScanBatch {
				last := kvs[len(kvs)-1].Key
				nextBegin = append(append(fdb.Key{}, last...), 0x00)
			}
			return nil
		}); rerr != nil {
			return 0, rerr
		}
		if nextBegin == nil {
			break
		}
		begin = nextBegin
	}

	cache, err := spfreshLoadRefineCache(ctx, db, s)
	if err != nil {
		return 0, err
	}
	quantizer := newSPFreshQuantizer(config)
	kc := spfreshRefineKc(config)
	moved := 0
	for lo := 0; lo < len(pks); lo += spfreshRefineMoveBatch {
		hi := min(lo+spfreshRefineMoveBatch, len(pks))
		batch := pks[lo:hi]
		batchMoved := 0
		if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			batchMoved = 0
			tx := rtx.Transaction()
			for _, pk := range batch {
				m, perr := spfreshRefinePKInTx(tx, s, config, quantizer, cache, kc, pk)
				if perr != nil {
					return perr
				}
				if m {
					batchMoved++
				}
			}
			return nil
		}); rerr != nil {
			return moved, fmt.Errorf("spfresh refine: move batch at %d: %w", lo, rerr)
		}
		moved += batchMoved
	}
	return moved, nil
}

// --- Budgeted online op (RFC-104 production) -------------------------------

// spfreshRefineCursor is the persisted round-robin position: the generation it
// belongs to (a rebuild/retrain flips it → the cursor resets), the moves
// accumulated SINCE the last wrap (so convergence is judged over a full cursor
// CYCLE, not one budgeted pass — a tenant with n > budget completes a cycle over
// several passes; an early pass can move rows while the wrapping tail pass moves
// none, codex), and the raw membership-relative key bytes to resume after (nil =
// start of the keyspace). Format: [generation:8 LE][movedSinceWrap:8 LE][after...].
type spfreshRefineCursor struct {
	generation     int64
	movedSinceWrap int64
	after          []byte // relative membership key of the last refined pk, or nil
}

func (s *spfreshStorage) refineCursorKey() fdb.Key { return s.metaKey(spfreshMetaRefineCursor) }

// spfreshReadRefineCursor reads the persisted cursor, resetting to the start of
// the current generation (movedSinceWrap=0) if absent, too short, or stamped with
// a stale generation.
func spfreshReadRefineCursor(tx fdb.ReadTransaction, s *spfreshStorage) (spfreshRefineCursor, error) {
	data, err := tx.Get(s.refineCursorKey()).Get()
	if err != nil {
		return spfreshRefineCursor{}, err
	}
	cur := spfreshRefineCursor{generation: s.generation}
	if data == nil || len(data) < 16 {
		return cur, nil
	}
	if int64(binary.LittleEndian.Uint64(data[:8])) != s.generation {
		return cur, nil // stale generation → restart this generation
	}
	cur.movedSinceWrap = int64(binary.LittleEndian.Uint64(data[8:16]))
	if len(data) > 16 {
		cur.after = append([]byte(nil), data[16:]...)
	}
	return cur, nil
}

func spfreshWriteRefineCursor(tx fdb.Transaction, s *spfreshStorage, cur spfreshRefineCursor) {
	buf := make([]byte, 16+len(cur.after))
	binary.LittleEndian.PutUint64(buf[:8], uint64(cur.generation))
	binary.LittleEndian.PutUint64(buf[8:16], uint64(cur.movedSinceWrap))
	copy(buf[16:], cur.after)
	tx.Set(s.refineCursorKey(), buf)
}

// spfreshRefineRound runs ONE budgeted refinement pass: it resumes the persisted
// cursor, re-evaluates up to `budget` pks against the current topology, moves the
// changed ones, and advances+persists the cursor (wrapping to the start when it
// reaches the end). Returns (moves, cycleConverged): cycleConverged is true only
// when this pass WRAPS and the whole cursor cycle since the last wrap moved
// nothing — the honest convergence signal even when n > budget spreads a cycle
// across several passes (codex). It does NOT drain to quiescence — the caller
// loops on its own cadence. Cursor advance is idempotent under commit_unknown and
// tolerates benign double-coverage across executors (every move is idempotent).
func spfreshRefineRound(ctx context.Context, db *FDBDatabase, s *spfreshStorage, config SPFreshConfig, budget int) (int, bool, error) {
	cache, err := spfreshLoadRefineCache(ctx, db, s)
	if err != nil {
		return 0, false, err
	}
	quantizer := newSPFreshQuantizer(config)
	kc := spfreshRefineKc(config)
	prefix := s.membership.Bytes()

	var cur spfreshRefineCursor
	if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		c, cerr := spfreshReadRefineCursor(rtx.Transaction(), s)
		cur = c
		return cerr
	}); rerr != nil {
		return 0, false, rerr
	}

	mr, err := fdb.PrefixRange(prefix)
	if err != nil {
		return 0, false, fmt.Errorf("spfresh refine: membership range: %w", err)
	}
	begin := mr.Begin
	if cur.after != nil {
		// Resume strictly after the last refined key (relative bytes → absolute).
		begin = append(append(append(fdb.Key{}, prefix...), cur.after...), 0x00)
	}

	moved := 0
	// The budget bounds WORK (pks re-evaluated), not moves: a converged or
	// low-move index does few moves but must still return promptly and advance
	// the cursor incrementally — otherwise the first call on a large quiescent
	// index would walk the entire membership keyspace before returning (codex
	// rfc104-impl P1). `moved` is only the reported per-pass move count.
	processed := 0
	// sinceWrap accumulates moves over the WHOLE cursor cycle (across earlier
	// passes since the last wrap, carried in the cursor): convergence is judged on
	// it, not the per-pass `moved`, so a budgeted tenant whose tail pass happens to
	// move nothing isn't falsely declared converged (codex fleet P2).
	sinceWrap := cur.movedSinceWrap
	cycleConverged := false
	for processed < budget {
		want := min(spfreshRefineMoveBatch, budget-processed)
		var pks []tuple.Tuple
		var lastRel []byte
		short := false
		// Re-evaluated per ATTEMPT: spfreshRun auto-retries the body on conflict,
		// so a counter mutated inside the closure would tally aborted attempts.
		// Fold into the outer `moved` only after the commit succeeds (codex
		// rfc104-impl P1 — the spfreshRefineAll prototype already does this).
		batchMoved := 0
		if rerr := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
			pks = pks[:0]
			lastRel = nil
			short = false
			batchMoved = 0
			tx := rtx.Transaction()
			kvs, kerr := tx.Snapshot().GetRange(
				fdb.KeyRange{Begin: begin, End: mr.End},
				fdb.RangeOptions{Limit: want, Mode: fdb.StreamingModeWantAll},
			).GetSliceWithError()
			if kerr != nil {
				return kerr
			}
			for _, kv := range kvs {
				pk, uerr := s.membership.Unpack(kv.Key)
				if uerr != nil {
					return fmt.Errorf("spfresh refine: unpack membership key: %w", uerr)
				}
				pks = append(pks, pk)
			}
			if len(kvs) > 0 {
				lastRel = append([]byte(nil), []byte(kvs[len(kvs)-1].Key)[len(prefix):]...)
			}
			short = len(kvs) < want
			// Refine this batch in the SAME tx, then advance the cursor atomically.
			for _, pk := range pks {
				m, perr := spfreshRefinePKInTx(tx, s, config, quantizer, cache, kc, pk)
				if perr != nil {
					return perr
				}
				if m {
					batchMoved++
				}
			}
			next := spfreshRefineCursor{generation: s.generation}
			if short {
				next.after = nil        // reached the end: wrap to start
				next.movedSinceWrap = 0 // new cycle begins: reset the accumulator
			} else {
				next.after = lastRel
				next.movedSinceWrap = sinceWrap + int64(batchMoved) // carry the cycle total
			}
			spfreshWriteRefineCursor(tx, s, next)
			return nil
		}); rerr != nil {
			return moved, cycleConverged, fmt.Errorf("spfresh refine round: %w", rerr)
		}
		// Commit succeeded: fold in the committed attempt's moves and pks once.
		moved += batchMoved
		processed += len(pks)
		sinceWrap += int64(batchMoved)
		if short {
			// Cycle complete: it moved `sinceWrap` rows in total (this pass plus the
			// earlier passes since the last wrap). Converged iff that total is zero.
			cycleConverged = sinceWrap == 0
			break // wrapped to start; the next round resumes at the head of a fresh cycle
		}
		begin = append(append(append(fdb.Key{}, prefix...), lastRel...), 0x00)
	}
	return moved, cycleConverged, nil
}

// RefineSPFreshIndex runs ONE budgeted refinement pass over the index (RFC-104):
// it advances the persistent round-robin cursor by up to `budget` vectors,
// re-routing each against the current topology and moving the stale ones to
// recover the closure replication fast ingest never fired. It does NOT drain to
// quiescence — the deployment runs it on its own cadence (a refinement loop
// beside the rebalancer loop). Returns (moves, cycleConverged): cycleConverged is
// true when this call wrapped the cursor AND the whole cycle since the last wrap
// moved nothing — the signal a caller uses to back off a converged tenant. It is
// NOT merely "this pass reached the end": a budgeted pass whose tail moves zero
// does not imply the cycle's earlier passes did (codex).
func RefineSPFreshIndex(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string, budget int) (int, bool, error) {
	s, config, err := spfreshResolveRefineTarget(ctx, db, storeBuilder, indexName)
	if err != nil {
		return 0, false, err
	}
	if s == nil {
		return 0, true, nil // not bootstrapped: nothing to refine (vacuously converged)
	}
	if budget <= 0 {
		budget = spfreshRefineMoveBatch
	}
	return spfreshRefineRound(ctx, db, s, config, budget)
}

// RefineSPFreshIndexAll is the exported one-shot validation entry (refine every
// vector once). Production callers use the budgeted RefineSPFreshIndex.
func RefineSPFreshIndexAll(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) (int, error) {
	s, config, err := spfreshResolveRefineTarget(ctx, db, storeBuilder, indexName)
	if err != nil {
		return 0, err
	}
	if s == nil {
		return 0, nil
	}
	return spfreshRefineAll(ctx, db, s, config)
}

// --- Fleet driver (the refinement loop beside the rebalancer loop) ----------

// spfreshRefineFleetBudget is the default per-tenant per-pass vector budget for
// RefineSPFreshIndexes: ten move batches. Small enough that one pass over a large
// fleet stays bounded, large enough that a tenant's cursor makes real progress
// each pass. Tune via SPFreshRefineOptions.BudgetPerTenant.
const spfreshRefineFleetBudget = 1000

// SPFreshRefineOptions tunes one fleet refinement pass.
type SPFreshRefineOptions struct {
	// BudgetPerTenant bounds the vectors re-evaluated per tenant per pass.
	// 0 means the default (spfreshRefineFleetBudget).
	BudgetPerTenant int

	// Timer, when non-nil, accumulates refinement instrumentation across the pass
	// (CountSPFreshRefineMoves, CountSPFreshRefineConverged) — the observability
	// the rebalance sweeper gets via its own Timer. Nil disables recording.
	Timer *StoreTimer
}

// SPFreshRefineResult summarizes one fleet refinement pass.
type SPFreshRefineResult struct {
	Moves     int // vectors moved (re-routed) across all tenants this pass
	Refined   int // tenants that completed a refine pass without error
	Converged int // tenants whose cursor wrapped a full cycle moving nothing
}

// RefineSPFreshIndexes runs ONE budgeted refinement pass over each tenant — the
// "refinement loop beside the rebalancer loop" (RFC-104). Unlike
// SweepSPFreshIndexes (task-driven: tenants with no pending lifecycle task rows
// are skipped), refinement is CURSOR-driven and unconditional — every tenant's
// persistent round-robin cursor advances by up to BudgetPerTenant vectors,
// re-routing each against the current (converged) topology and moving the stale
// ones, recovering the closure replication fast ingest never fired.
//
// Run it on a SLOWER cadence than the rebalance sweep: refinement is
// recall-recovery, not correctness, and a fully converged tenant re-scans its
// cursor for zero moves each pass. The result's Converged count (cursor wrapped
// a full cycle with zero moves) lets a caller back off quiescent tenants. As in
// SweepSPFreshIndexes, per-tenant failures are isolated (joined, not fatal — one
// corrupt tenant must not halt fleet refinement) and ctx cancellation ends the
// pass between tenants.
func RefineSPFreshIndexes(ctx context.Context, db *FDBDatabase, tenants []SPFreshTenant, opts SPFreshRefineOptions) (SPFreshRefineResult, error) {
	budget := opts.BudgetPerTenant
	if budget <= 0 {
		budget = spfreshRefineFleetBudget
	}
	var result SPFreshRefineResult
	var errs []error
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return result, errors.Join(append(errs, ctx.Err())...)
		}
		moved, cycleConverged, err := RefineSPFreshIndex(ctx, db, tenant.StoreBuilder, tenant.IndexName, budget)
		result.Moves += moved
		if opts.Timer != nil && moved > 0 {
			opts.Timer.IncrementBy(CountSPFreshRefineMoves, int64(moved))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("refine %q: %w", tenant.IndexName, err))
			continue
		}
		result.Refined++
		if cycleConverged {
			result.Converged++
			if opts.Timer != nil {
				opts.Timer.Increment(CountSPFreshRefineConverged)
			}
		}
	}
	return result, errors.Join(errs...)
}

// spfreshResolveRefineTarget resolves the index's storage (at the readable
// generation) and config. Returns a nil storage when the index is not yet
// bootstrapped (no generation) — nothing to refine.
func spfreshResolveRefineTarget(ctx context.Context, db *FDBDatabase, storeBuilder func(*FDBRecordContext) (*FDBRecordStore, error), indexName string) (*spfreshStorage, SPFreshConfig, error) {
	var s *spfreshStorage
	var config SPFreshConfig
	err := spfreshRun(ctx, db, func(rtx *FDBRecordContext) error {
		store, serr := storeBuilder(rtx)
		if serr != nil {
			return serr
		}
		index := store.GetMetaData().GetIndex(indexName)
		if index == nil {
			return fmt.Errorf("spfresh refine: index %q not found", indexName)
		}
		if index.Type != IndexTypeVectorSPFresh {
			return fmt.Errorf("spfresh refine: index %q has type %q", indexName, index.Type)
		}
		config = parseSPFreshConfig(index)
		gen, gerr := spfreshReadGenerationSnapshot(rtx.Transaction(), newSPFreshStorage(store.indexSubspace(index), 0))
		if gerr != nil {
			if errors.Is(gerr, errSPFreshNotFound) {
				return nil // not bootstrapped
			}
			return gerr
		}
		s = newSPFreshStorage(store.indexSubspace(index), gen)
		return nil
	})
	return s, config, err
}
