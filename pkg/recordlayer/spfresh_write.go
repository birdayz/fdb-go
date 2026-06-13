package recordlayer

import (
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"

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
	bootstrapped := false
	if errors.Is(err, errSPFreshNotFound) {
		bootstrapped = true
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
	writeCache := m.txWriteCache()
	if writeCache == nil || !writeCache.ready(gen) {
		// The write path routes on a TX-LOCAL cache only (kept in the record
		// context's session, which lives one transaction): loading the
		// process-global cache through a WRITING transaction publishes
		// uncommitted RYW state — minted centroids, bootstrap cells — and an
		// abort leaves every other writer routing on phantoms (caught by the
		// concurrent foreground-fill benchmark). Seed L1 from the global
		// cache when it's warm; otherwise load from this tx. Same-tx
		// searches route on this cache too (RYW; Torvalds final-gauntlet
		// S1), which is why it lives on the context, not the maintainer:
		// another store instance in this transaction must find it.
		global := spfreshCacheFor(m.indexSubspace, gen)
		if !bootstrapped && global.ready(gen) {
			writeCache = global.cloneForWrite()
		} else {
			writeCache = newSPFreshRoutingCache(0)
			if err := writeCache.fullReloadTxLocal(m.tx, storage, gen); err != nil {
				return nil, fmt.Errorf("spfresh index %q: routing reload: %w", m.index.Name, err)
			}
		}
		m.setTxWriteCache(writeCache)
	}
	return &spfreshWriteContext{storage: storage, cache: writeCache}, nil
}

// spfreshInsert indexes one (pk, vector): route on cache → closure copy-set →
// REAL state fence per chosen centroid (non-ACTIVE/absent drops to the
// next-nearest) → membership/posting/sidecar writes + counter ADDs → sampled
// split probe. An existing membership row (update) is cleared from keys
// derived from this same-tx read.
func (m *spfreshIndexMaintainer) spfreshInsert(wc *spfreshWriteContext, pk tuple.Tuple, vec []float64) error {
	// Bounded attempts, each running the FULL path: route -> mint-the-first-
	// centroid when none exist (§6b) -> authoritative fence. Between
	// attempts the cache reloads: a stale view can both present phantom
	// candidates (rejected by the fence) AND hide that the index is still
	// centroidless — the mint fallback must therefore be reachable on every
	// attempt, not only the first (caught by the concurrent foreground-fill
	// benchmark: phantom candidates -> reload to an empty cell -> the old
	// single-shot retry hard-errored instead of minting).
	defer m.timer.RecordSince(EventSPFreshInsert, time.Now())
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			m.timer.Increment(CountSPFreshStaleRouteRetry)
			if rerr := wc.cache.fullReloadTxLocal(m.tx, wc.storage, wc.storage.generation); rerr != nil {
				return fmt.Errorf("spfresh index %q: stale-route reload: %w", m.index.Name, rerr)
			}
		}
		routed, err := wc.cache.routeForWrite(m.tx, wc.storage, vec, spfreshInsertProbeCells, spfreshInsertCandidates)
		if err != nil && !errors.Is(err, errSPFreshEmptyRouting) {
			return fmt.Errorf("spfresh index %q: route insert: %w", m.index.Name, err)
		}
		if len(routed) == 0 {
			// No fine centroids visible: mint the first (§6b). A concurrent
			// mint surfaces as errSPFreshStaleRoute — reload and re-route.
			first, ferr := m.spfreshFirstCentroid(wc.storage, vec)
			if ferr != nil {
				if errors.Is(ferr, errSPFreshStaleRoute) {
					lastErr = ferr
					continue
				}
				return ferr
			}
			routed = first
		}
		ierr := m.spfreshInsertRouted(wc.storage, routed, pk, vec)
		if !errors.Is(ierr, errSPFreshStaleRoute) {
			return ierr
		}
		lastErr = ierr
	}
	return fmt.Errorf("spfresh index %q: insert did not converge after cache reloads: %w", m.index.Name, lastErr)
}

// errSPFreshStaleRoute: every routed candidate failed the authoritative state
// fence — the routing cache is stale beyond in-place recovery and must be
// reloaded.
var errSPFreshStaleRoute = errors.New("spfresh: routed candidates all stale (cache reload required)")

// spfreshBootstrap establishes generation 1 with a single empty cell — the
// §6b cold-start shape. Runs inside the caller's transaction; the caller has
// already REAL-read the generation as absent (the racing-bootstrap fence).
func (m *spfreshIndexMaintainer) spfreshBootstrap(metaStorage *spfreshStorage) (int64, error) {
	// A builder token with NO generation means a bulk build is in flight (or
	// died pre-flip): bootstrapping would create a live generation 1 that the
	// build keeps writing into — and the build's flip would then self-ACK the
	// BOOTSTRAP's generation as its own committed flip (Torvalds 094.4 #1).
	// REAL read: a build taking the token concurrently aborts this insert at
	// the resolver. A crashed build's residue is cleared by rerunning
	// BuildSPFreshIndex (its entry takeover re-takes the token).
	if tok, terr := m.tx.Get(metaStorage.metaKey(spfreshMetaBuild)).Get(); terr != nil {
		return 0, terr
	} else if tok != nil {
		return 0, fmt.Errorf("spfresh index %q: a bulk build is in flight (or died before flipping) — retry after it completes, or rerun BuildSPFreshIndex", m.index.Name)
	}
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
	ids, coarseRows, err := spfreshLoadAllCoarseForWrite(m.tx, storage)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("spfresh index %q: no coarse cells (corrupt bootstrap)", m.index.Name)
	}
	// The mint exists ONLY for the §6b cold start: one ACTIVE cell, zero fine
	// rows anywhere. A transient zero-candidate route at STEADY STATE must
	// NOT reach it — the original version blindly minted into ids[0], which
	// after the first coarse split is a FORWARDED EMPTY cell: the minted
	// centroid lives where no query routes, and every entry inserted against
	// it is invisible (the 300k fill orphaned thousands of entries this way;
	// recall collapsed to 0.17 — caught by the centroid audit trail showing
	// orphans saved into the forwarded cell 1).
	cellID := int64(0)
	for i, id := range ids {
		rows, _, _, lerr := spfreshLoadCellForWrite(m.tx, storage, id)
		if lerr != nil {
			return nil, lerr
		}
		if len(rows) > 0 {
			// The index is NOT empty: never mint. The cache that routed us
			// here is stale — evict so the retry sees the real topology.
			spfreshCacheFor(m.indexSubspace, storage.generation).evictCell(id)
			return nil, errSPFreshStaleRoute
		}
		if cellID == 0 && coarseRows[i].state == spfreshStateActive {
			cellID = id
		}
	}
	if cellID == 0 {
		return nil, errSPFreshStaleRoute // no ACTIVE cell: topology mid-flux, retry
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
	// Evict the process-local cached EMPTY bootstrap cell: queries that ran
	// before this insert cached it with zero candidates, and the amortized
	// refresh can be throttled past this window — they would miss the new
	// record until the next changelog refresh (codex 094.4 P2). Same-process
	// only by design; other processes converge via the addFine delta above.
	spfreshCacheFor(m.indexSubspace, storage.generation).evictCell(cellID)
	// The TX-LOCAL cache cached the same empty cell while routing this very
	// insert — a same-transaction search routes on it (RYW), so it must
	// reload the cell and see the minted centroid.
	if wc := m.txWriteCache(); wc != nil {
		wc.evictCell(cellID)
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
	sawInFlight := false
	work := append([]spfreshRouted(nil), routed...)
	seen := make(map[int64]bool, len(work))
	// Speculative verification burst: the loop REAL-reads candidate rows as
	// its lifecycle fence, and reading them one-by-one serialized up to
	// spfreshClosurePool() round trips inside the user's save transaction —
	// the RNG diversity scan made this the fill bottleneck (916→615 vec/s
	// at 100k when the pool widened). Issue the likely candidates as ONE
	// snapshot burst; a read conflict key is added only for rows the loop
	// actually examines (snapshot read + explicit conflict key ≡ the
	// serializable read: same read version, same data, same conflict
	// surface — speculative rows the cutoffs never reach contribute none).
	// Keyed by (cellID, fineID) — the exact key the future was issued for —
	// so the consumed data and the conflict key can never diverge: a
	// candidate surfacing at a DIFFERENT cell than the burst read simply
	// misses the map and takes the direct serializable read (Torvalds 094.4
	// F13: a conflict fence must be locally correct, not correct via
	// lifecycle-wide arguments about where a fineID can appear).
	specFuts := make(map[spfreshSpecKey]fdb.FutureByteSlice, spfreshClosurePool(m.config.Replication))
	for i := 0; i < len(work) && i < spfreshClosurePool(m.config.Replication); i++ {
		specFuts[spfreshSpecKey{work[i].cellID, work[i].fineID}] = m.tx.Snapshot().Get(storage.centroidKey(work[i].cellID, work[i].fineID))
	}
	// Drain whatever the cutoffs never consumed before this attempt returns
	// (consumed entries are deleted from the map): a pending future must not
	// outlive the attempt — Transact may reset and retry this transaction,
	// and an old-attempt read resolving into the retry's state is exactly
	// the cross-attempt contamination the RYW machinery cannot guard
	// (codex 094.4 r3). The burst already paid the round trip, so draining
	// resolved futures costs nothing on the happy path.
	defer func() {
		for _, fut := range specFuts {
			_, _ = fut.Get()
		}
	}()
	for examined := 0; len(work) > 0 && examined < 4*(len(routed)+2); examined++ {
		cand := work[0]
		work = work[1:]
		if seen[cand.fineID] {
			continue
		}
		seen[cand.fineID] = true
		// Closure-driven cutoffs. The RNG diversity rule may have to look
		// PAST the first Replication candidates for a different-direction
		// replica, so "Replication verified" is not enough to stop (a pool
		// of exactly r turns every RNG skip into silent under-replication —
		// codex 094.4). The queue is ascending at every pop, so:
		//  1. done: r diverse replicas kept and the head can no longer
		//     enter the kept set;
		//  2. ratio: the head already fails the closure's ratio bound
		//     against the nearest verified candidate — everything after
		//     fails it too (no REAL read needed to know);
		//  3. cap: stop spending REAL centroid reads hunting diversity.
		if kept := spfreshClosure(verified, m.config.Replication, m.config.Alpha); len(kept) >= m.config.Replication && cand.d2 >= kept[len(kept)-1].d2 {
			break
		}
		if len(verified) > 0 && cand.d2 > m.config.Alpha*m.config.Alpha*verified[0].d2 {
			break
		}
		if len(verified) >= spfreshClosurePool(m.config.Replication) {
			break
		}
		m.timer.Increment(CountSPFreshInsertFenceReads)
		row, rerr := m.spfreshConsumeCentroidRead(storage, specFuts, cand.cellID, cand.fineID)
		if rerr != nil {
			if errors.Is(rerr, errSPFreshNotFound) {
				continue
			}
			return rerr
		}
		switch row.state {
		case spfreshStateActive:
			// verified below
		case spfreshStateSealed:
			sawInFlight = true
			continue // a split owns it; next-nearest (or retry below)
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
			continue // DEAD: next-nearest
		}
		cvec, verr := row.vector()
		if verr != nil {
			return verr
		}
		nc := spfreshCandidate{id: cand.fineID, d2: cand.d2, vec: cvec}
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
		if sawInFlight {
			// Every reachable centroid is mid-lifecycle (SEALED) — the §6
			// cold-start corner where ONE hot posting is being split and no
			// ACTIVE sibling exists yet. The split commits within its two-tx
			// window; surface the same retryable conflict a resolver abort
			// would (RFC-094 §6 "whichever loses retries"), so the enclosing
			// transaction re-runs with a fresh read version and sees the
			// children ACTIVE.
			return fdb.Error{Code: 1020} // not_committed
		}
		// Keep this error CHEAP: it is a normal retryable outcome during
		// split churn, raised inside the caller's save transaction. The full
		// topology dump (spfreshDebugTopology) scans every posting in the
		// index — embedding it here turned a transient retry into O(index)
		// range reads. Diagnostics go through the exported
		// SPFreshDebugTopology instead.
		return fmt.Errorf("spfresh index %q: no ACTIVE fine centroid among %d routed candidates: %w", m.index.Name, len(routed), errSPFreshStaleRoute)
	}
	kept := spfreshClosure(verified, m.config.Replication, m.config.Alpha)
	m.timer.IncrementBy(CountSPFreshInsertReplicas, int64(len(kept)))

	// (Speculative futures the cutoffs never examined are simply dropped:
	// snapshot reads, no conflict ranges, already paid for by the burst.)

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

// spfreshDebugTopology summarizes the coarse table + fine states
// (diagnostics). lmax buckets the posting-size histogram against the index's
// REAL configured envelope.
func spfreshDebugTopology(tx fdb.Transaction, s *spfreshStorage, lmax int) string {
	ids, rows, err := spfreshLoadAllCoarse(tx, s)
	if err != nil {
		return fmt.Sprintf("coarse err=%v", err)
	}
	// Pending tasks by kind + posting-size histogram + entry totals: the
	// convergence diagnostics (is the queue truly drained? are postings
	// within Lmax? where do the entries live?).
	taskCounts := map[int64]int{}
	if r, rerr := fdb.PrefixRange(s.tasks.Bytes()); rerr == nil {
		if kvs, gerr := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError(); gerr == nil {
			for _, kv := range kvs {
				if t, uerr := s.tasks.Unpack(kv.Key); uerr == nil && len(t) == 2 {
					if k, ok := t[0].(int64); ok {
						taskCounts[k]++
					}
				}
			}
		}
	}
	postingSizes := map[string]int{}
	totalEntries, totalActive := 0, 0
	for _, id := range ids {
		cellRows, _, _, cerr := spfreshLoadCell(tx, s, id)
		if cerr != nil {
			continue
		}
		for _, r := range cellRows {
			if r.row.state != spfreshStateActive {
				continue
			}
			totalActive++
			entries, _, _, _, perr := spfreshLoadPostingSnapshot(tx, s, r.fineID, 100000)
			if perr != nil {
				continue
			}
			totalEntries += len(entries)
			switch {
			case len(entries) <= lmax:
				postingSizes["<=Lmax"]++
			case len(entries) <= 4*lmax:
				postingSizes["<=4Lmax"]++
			default:
				postingSizes[">4Lmax"]++
			}
		}
	}
	out := fmt.Sprintf("gen=%d cells=%d activeFines=%d entries=%d tasks=%v hist=%v [", s.generation, len(ids), totalActive, totalEntries, taskCounts, postingSizes)
	for i, id := range ids {
		if i > 14 {
			out += "..."
			break
		}
		cellRows, _, _, cerr := spfreshLoadCell(tx, s, id)
		if cerr != nil {
			out += fmt.Sprintf("c%d(err) ", id)
			continue
		}
		states := map[byte]int{}
		for _, r := range cellRows {
			states[r.row.state]++
		}
		out += fmt.Sprintf("c%d(st%d,fines=%v) ", id, rows[i].state, states)
	}
	return out + "]"
}

// SPFreshDebugTopology dumps an index's coarse/fine topology summary
// (benchmark/operational diagnostics).
func SPFreshDebugTopology(rtx *FDBRecordContext, store *FDBRecordStore, indexName string) string {
	idx := store.GetMetaData().GetIndex(indexName)
	if idx == nil {
		return "index not found"
	}
	storage := newSPFreshStorage(store.indexSubspace(idx), 0)
	gen, err := spfreshReadGenerationSnapshot(rtx.Transaction(), storage)
	if err != nil {
		return fmt.Sprintf("gen err=%v", err)
	}
	return spfreshDebugTopology(rtx.Transaction(), newSPFreshStorage(store.indexSubspace(idx), gen), parseSPFreshConfig(idx).Lmax)
}

// SPFreshDebugIntegrity samples up to `sample` pks evenly from the index's
// OWN membership rows (no assumption about pk shape) and reports, for each,
// whether every membership target holds the posting entry and what state the
// target centroid is in. Diagnostics only: it streams the membership keyspace
// (O(index) reads) — never call it on a production write path.
func SPFreshDebugIntegrity(rtx *FDBRecordContext, store *FDBRecordStore, indexName string, sample int) string {
	idx := store.GetMetaData().GetIndex(indexName)
	if idx == nil {
		return "index not found"
	}
	metaStorage := newSPFreshStorage(store.indexSubspace(idx), 0)
	gen, err := spfreshReadGenerationSnapshot(rtx.Transaction(), metaStorage)
	if err != nil {
		return fmt.Sprintf("gen err=%v", err)
	}
	s := newSPFreshStorage(store.indexSubspace(idx), gen)
	tx := rtx.Transaction()
	// Classify the STATE of each membership target: an entry is only
	// query-visible if its centroid is ACTIVE (or SEALED) in some cell.
	ids, _, lerr := spfreshLoadAllCoarse(tx, s)
	if lerr != nil {
		return fmt.Sprintf("coarse err=%v", lerr)
	}
	fineState := map[int64]byte{}
	for _, cellID := range ids {
		rows, _, _, cerr := spfreshLoadCell(tx, s, cellID)
		if cerr != nil {
			continue
		}
		for _, r := range rows {
			fineState[r.fineID] = r.row.state
		}
	}
	// Collect the membership pks (keys only matter), then sample evenly.
	r, rerr := fdb.PrefixRange(s.membership.Bytes())
	if rerr != nil {
		return fmt.Sprintf("membership range err=%v", rerr)
	}
	kvs, gerr := tx.Snapshot().GetRange(r, fdb.RangeOptions{Mode: fdb.StreamingModeWantAll}).GetSliceWithError()
	if gerr != nil {
		return fmt.Sprintf("membership scan err=%v", gerr)
	}
	var pks []tuple.Tuple
	for _, kv := range kvs {
		if pk, uerr := s.membership.Unpack(kv.Key); uerr == nil {
			pks = append(pks, pk)
		}
	}
	step := len(pks) / sample
	if step < 1 {
		step = 1
	}
	missingEntry, ok := 0, 0
	sampled := 0
	targetStates := map[string]int{}
	var absentFines []int64
	for i := 0; i < len(pks); i += step {
		pk := pks[i]
		mem, merr := spfreshReadMembership(tx, s, pk)
		if merr != nil {
			continue // raced a concurrent delete between scan and read
		}
		sampled++
		all := true
		for _, fineID := range mem {
			data, perr := tx.Snapshot().Get(s.postingKey(fineID, pk)).Get()
			if perr != nil || data == nil {
				all = false
			}
			if st, known := fineState[fineID]; known {
				targetStates[fmt.Sprintf("st%d", st)]++
			} else {
				targetStates["ABSENT"]++
				if len(absentFines) < 3 {
					absentFines = append(absentFines, fineID)
				}
			}
		}
		if all {
			ok++
		} else {
			missingEntry++
		}
	}
	// Trace up to 3 ABSENT targets: posting size, HDR presence/payload, and
	// pending-task state — enough to identify which lifecycle lost them.
	examples := ""
	for _, fineID := range absentFines {
		entries, _, _, _, _ := spfreshLoadPostingSnapshot(tx, s, fineID, 100000)
		hdr, _ := tx.Snapshot().Get(s.postingHDRKey(fineID)).Get()
		hdrInfo := "none"
		if hdr != nil {
			hc, ha, hb, herr := decodePostingHDR(hdr)
			hdrInfo = fmt.Sprintf("(cell=%d a=%d b=%d err=%v)", hc, ha, hb, herr)
		}
		task, _ := tx.Snapshot().Get(s.taskKey(spfreshTaskSplit, fineID)).Get()
		cnt, _ := spfreshCounterReadSnapshot(tx, s, spfreshCounterFine, fineID)
		examples += fmt.Sprintf(" absent[f%d: posting=%d hdr=%s splitTask=%v counter=%d trail=%v]", fineID, len(entries), hdrInfo, task != nil, cnt, SPFreshAuditTrail(fineID))
	}
	return fmt.Sprintf("members=%d sampled=%d ok=%d membershipWithoutEntry=%d targetStates=%v%s",
		len(pks), sampled, ok, missingEntry, targetStates, examples)
}

// spfreshConsumeCentroidRead resolves a candidate centroid row from the
// speculative snapshot burst when one was issued, adding the read conflict
// key the lifecycle fence requires (snapshot + explicit conflict key is
// exactly the serializable read spfreshReadCentroidForWrite performs — same
// read version, same data, same conflict surface). Candidates without a
// burst future (forward children discovered mid-walk) fall back to the
// direct serializable read.
// spfreshSpecKey identifies a speculative centroid read by the exact
// (cellID, fineID) key it was issued for.
type spfreshSpecKey struct{ cellID, fineID int64 }

func (m *spfreshIndexMaintainer) spfreshConsumeCentroidRead(storage *spfreshStorage, futs map[spfreshSpecKey]fdb.FutureByteSlice, cellID, fineID int64) (spfreshCentroidRow, error) {
	fut, ok := futs[spfreshSpecKey{cellID, fineID}]
	if !ok {
		return spfreshReadCentroidForWrite(m.tx, storage, cellID, fineID)
	}
	delete(futs, spfreshSpecKey{cellID, fineID})
	data, err := fut.Get()
	if err != nil {
		return spfreshCentroidRow{}, fmt.Errorf("spfresh: read centroid (%d,%d): %w", cellID, fineID, err)
	}
	if cerr := m.tx.AddReadConflictKey(storage.centroidKey(cellID, fineID)); cerr != nil {
		return spfreshCentroidRow{}, cerr
	}
	if data == nil {
		return spfreshCentroidRow{}, errSPFreshNotFound
	}
	return decodeCentroidRow(data)
}
