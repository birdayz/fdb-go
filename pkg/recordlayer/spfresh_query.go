package recordlayer

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// SPFresh query path (RFC-094 §4): route on the cached two-level table →
// fetch k_c postings in one parallel burst (snapshot reads, fetch-capped) →
// RaBitQ residual distances → exact re-rank of the top C from the fp16
// sidecar (parallel point reads). Three network round trips happy path
// (GRV + postings + re-rank); +2 RT per forwarded posting (≤ depth 2, then
// the caller refreshes); all reads snapshot — a query never conflicts with
// anything.

// spfreshSearcher executes searches against one generation's storage using a
// shared routing cache.
type spfreshSearcher struct {
	storage *spfreshStorage
	config  SPFreshConfig
	cache   *spfreshRoutingCache
	quant   *spfreshQuantizer

	// runtime knobs (RFC-094 §4 defaults; never stored)
	w        int     // coarse cells probed
	kc       int     // fine postings fetched (CAP under ε-pruning)
	c        int     // re-rank candidates
	epsilon  float64 // SPANN Eq.(3) pruning ratio ε₂; ≤ 0 disables pruning
	noRerank bool    // rank by RaBitQ estimates alone (skip the sidecar wave)

	// timer is the context's StoreTimer (nil-receiver-safe; see
	// spfresh_metrics.go for the event set).
	timer *StoreTimer

	// capped collects postings whose fetch returned exactly the 4×Lmax+1 cap
	// — past the split-dispatch envelope, tail invisible to this query. The
	// maintainer re-files their split tasks after the search (the read-path
	// envelope repair): the cap equals the dispatch envelope precisely so a
	// cap-hit is PROOF a split trigger was lost, never a healthy state.
	capped []spfreshRouted
}

func newSPFreshSearcher(storage *spfreshStorage, config SPFreshConfig, cache *spfreshRoutingCache) *spfreshSearcher {
	return &spfreshSearcher{
		storage: storage,
		config:  config,
		cache:   cache,
		quant:   newSPFreshQuantizer(config),
		// 094.5 freeze, from the SIFT-1M foreground-fill sweep: at kc=64,
		// w=32 buys +0.8pp recall@10 over w=16 (0.952 vs 0.944) at EQUAL
		// p50/QPS — the L1 hop is in-memory CPU, so probing more cells is
		// free until kc gates the posting reads (the 100k sweep could not
		// see this: w covered 26% of cells there vs 3% at 1M). Per-query
		// overrides ride the scan contract's High tuple (k, kc, w, c[, ε])
		// — e.g. 16/24/64 gives 0.826 at ~11 ms p50 for latency-first
		// callers (w=16, not 8, for the same reason).
		w:  spfreshDefaultProbeW,
		kc: 64,
		c:  200,
		// SPANN §4.2's published recall@10 setting: SPTAG applies
		// MaxDistRatio = 1+ε = 8 directly to SQUARED distances (see
		// spfreshPruneRouted). With pruning on, kc is a CAP, not a constant
		// cost: the paper's Fig. 2 shows 80% of SIFT-1M queries need ~6
		// posting lists while 99% need 114 — Eq. (3) gives the easy
		// majority the short probe and the hard tail the cap.
		epsilon: 7.0,
	}
}

// spfreshPruneRouted applies SPANN Eq. (3) query-aware dynamic pruning to a
// d2-ascending routed list: probe list ij ⟺ Dist(q,c_ij) ≤ (1+ε)·Dist(q,c_i1).
// Eq. (3)'s Dist is SPTAG's distance — SQUARED L2 — and the reference
// implementation applies MaxDistRatio = 1+ε directly to those squared values:
// the published ε₂=7.0 operating point is an 8× bound in d²-space (true
// distance ≈2.83×). Squaring the ratio here (an earlier reading of Eq. (3)
// as true-distance) made ε=7.0 a 64× bound that never bound anything — the
// paper review caught it via the inert 1M A/B. The pruned tail is returned
// for the starvation widening — the caller refetches it when the probed set
// can't fill the re-rank budget.
func spfreshPruneRouted(routed []spfreshRouted, epsilon float64) (probe, pruned []spfreshRouted) {
	if epsilon <= 0 || len(routed) <= 1 {
		return routed, nil
	}
	threshold := (1 + epsilon) * routed[0].d2
	cut := len(routed)
	for i := 1; i < len(routed); i++ {
		if routed[i].d2 > threshold {
			cut = i
			break
		}
	}
	return routed[:cut], routed[cut:]
}

// spfreshSearchResult is one search hit.
type spfreshSearchResult struct {
	PrimaryKey tuple.Tuple
	Distance   float64
}

// spfreshApproxHit is an approximate candidate before re-rank. span is the
// posting key's flat-packed pk suffix (see postingPKSpan) — the dedup key,
// the sidecar-key suffix, and the deterministic tie-break; it is decoded to
// a tuple only for the final k winners. Hot-loop entries (~kc·Lavg per
// query) never box a tuple.
type spfreshApproxHit struct {
	span string
	est  float64
}

// spfreshFrontier is the resumable, in-memory state of one SPFresh search
// (RFC-156 Phase A). It reframes the index as a distance-*ordering* provider
// with an Open/Next iterator rather than a one-shot top-k black box, mapping
// onto VBASE's partition-index instantiation (docs/vbase-osdi-2023.md §3.1):
// E = k, w = the vectors in the probed clusters; Phase 1 = choosing the m
// nearest cells, Phase 2 = scanning them.
//
// State retained (and ONLY this — memory is O(best + cTop), never O(corpus),
// RFC-156 invariant 5): the ε-pruned tail (d2-ascending) plus the position
// widening has reached in it; the min-estimate dedup map keyed by pk span; the
// forwarded-child queue; the relaxed-monotonicity queues (recentQueue M_q^s,
// smallestQueue R_q); and the finalized (exact re-ranked, distance-ordered)
// prefix streamed out so far. Lives for one query's lifecycle only — never
// serialized into a continuation (mirrors Java's ListCursor; wire-format
// untouched, RFC-156 §1).
type spfreshFrontier struct {
	s     *spfreshSearcher
	tx    fdb.ReadTransaction
	query []float64

	// pruned is the ε-pruned tail (d2-ascending) the probe set left behind;
	// tailPos is how far widening has admitted it. The probe set was scored at
	// init and is not retained as a list — its candidates live in best.
	pruned  []spfreshRouted
	tailPos int

	// best is the residual-estimate dedup map keyed by the raw pk span (the
	// flat-packed pk suffix, see postingPKSpan): min-estimate across closure
	// replicas (RFC-094 §4/§7). Lookups with the string(span) conversion never
	// allocate; assignments copy the span to an owned string. Nothing in the
	// hot loop decodes a tuple. Bounded by distinct candidates seen.
	best     map[string]float64
	residual []float64 // scratch, reused per posting

	// forwards are stale-cache HDR redirects (SEALED→FORWARD) discovered while
	// scanning; their child centroids are point-read inline (resolveForward,
	// +1 RT) and their postings fetched in one burst at finalize time.
	forwards         []spfreshRouted
	forwardsResolved bool

	// Relaxed-monotonicity queues (VBASE §4.1, Eq. 3). recentQueue holds the
	// last w traversed estimates → M_q^s is its median; smallestQueue keeps the
	// E=k smallest estimates → R_q is its max (the current k-th best). Phase 2
	// ⟺ M_q^s > R_q: the recently-traversed cells no longer beat the running
	// k-th best. These are PHASE-A TELEMETRY ONLY (CountSPFreshPhase2Reached) —
	// neither the one-shot wrapper nor the Phase C streaming cursor early-stops on
	// phase2: the wrapper always scans its full conditional horizon, and the
	// streaming cursor terminates on the emission barrier + budget/exhaustion (see
	// spfresh_stream.go). recentQueue is sized at init (w = probe width);
	// smallestQueue at the first searchNext, then seeded from the probe estimates.
	recent   *spfreshRecentQueue
	smallest *spfreshSmallestQueue
	phase2   bool

	// Finalization. cTop = max(c, demand) is the top-C budget; finalized is the
	// exact re-ranked, distance-ordered winners; emitPos streams them across
	// searchNext calls. done is set once the conditional horizon the one-shot
	// path scans (probe + widen-if-starved + forwards) is exhausted and
	// finalized is built — only then is a result guaranteed in true distance
	// order (RFC-156 invariant 2: emit only exact-re-ranked finalized prefixes).
	cTop      int
	finalized []spfreshSearchResult
	emitPos   int
	done      bool

	empty bool // route returned nothing — nothing to emit, exhausted immediately

	// ---- RFC-156 Phase C streaming-cursor state (unused by the one-shot wrapper) ----
	// All of this stays O(budget): bounded by the candidate/cell budget caps, never
	// O(corpus). scoredCells dedups posting reads across the probe set, the ε-pruned
	// tail, and the all-cells re-route so widening never re-scores a cell.
	scoredCells map[int64]struct{} // fineIDs whose posting list has been scored

	// reranked caches each candidate's EXACT (sidecar) distance so re-rank fires
	// once per span across the whole stream (total reads ≤ candidate budget);
	// deleted holds spans whose sidecar vanished between bursts; emittedSpans holds
	// spans already yielded (positional dedup + never-drop-a-later-closer-row).
	reranked     map[string]float64
	deleted      map[string]struct{}
	emittedSpans map[string]struct{}

	// spanCent maps each candidate span to the (shared, read-only) centroid vector
	// of the min-estimate replica that scored it — so the emission barrier can
	// compute that member's residual ‖v−c‖ at re-rank time. maxResidual is the
	// largest residual seen; the Euclidean barrier B = nextCentroidDist −
	// maxResidual corrects for NEAR-SIDE members (closer to the query than their
	// own centroid) that a pure-centroid barrier emits out of order. This is NOT an
	// unconditional triangle bound (maxResidual is a running max over already-scored
	// members); its soundness rests on SPANN's (1+ε) closure replication — a
	// boundary member is replicated into its near centroid and scored early, so
	// maxResidual has converged before far cells admit — plus LIRE's roughly-uniform
	// cell radii. See nextCellBarrier for the full argument. Populated
	// unconditionally (searchInit) so the probe set is covered; the wrapper ignores it.
	spanCent    map[string][]float64
	maxResidual float64

	// The all-cells re-route admitted in d2 order once the ε-pruned tail drains.
	widenRouted        []spfreshRouted
	widenPos           int
	widenRerouted      bool
	widenRouteComplete bool // the re-route returned the COMPLETE centroid set (not budget-capped)

	cellsProbed   int
	maxCells      int // budget: max posting cells probed
	maxCandidates int // budget: max distinct candidates retained
	widenBatch    int // cells admitted per widen step
	budgetHit     bool
	streamExhaust bool

	// streamFinalized is the current un-emitted, exact-distance-sorted prefix; it
	// is rebuilt from (best \ emitted \ deleted) after each widen and drained
	// positionally. peakBest tracks the high-water dedup-map size (the memory
	// assertion: it must stay ≤ candidate budget + one batch, never grow with the
	// corpus).
	streamFinalized []spfreshStreamHit
	streamPos       int
	streamStarted   bool // the probe's finalized prefix has been built+drained at least once
	streamFlushed   bool // the final B=+inf flush has run (all held candidates released in order)
	peakBest        int

	// barrierEnabled gates the early-streaming emission barrier to metrics where
	// the residual-margin (reverse-triangle) bound is valid — EUCLIDEAN only (see
	// streamInit). For every other metric the cursor holds ALL candidates until the
	// terminal and flushes the full sorted scanned set (materialize-then-emit),
	// which is EXACT for any metric. disableNearSide forces the pure-centroid
	// barrier (maxResidual treated as 0) — test-only, for the ablation proving the
	// −maxResidual near-side correction is load-bearing.
	barrierEnabled  bool
	disableNearSide bool
}

// spfreshStreamHit is one entry of the streaming cursor's un-emitted, sorted
// finalized prefix: the pk span (decoded to a tuple only when actually yielded)
// and its exact (re-ranked) distance.
type spfreshStreamHit struct {
	span string
	dist float64
}

// searchInit routes, applies SPANN Eq. (3) ε-pruning, and fetches & scores the
// PROBE set, leaving the ε-pruned tail for incremental widening (RFC-156 Phase
// A). It performs no widening, forward resolution, or re-rank — those happen in
// searchNext, which knows the caller's demand (k) and so can size cTop and the
// E=k smallest queue.
func (s *spfreshSearcher) searchInit(tx fdb.ReadTransaction, query []float64) (*spfreshFrontier, error) {
	f := &spfreshFrontier{
		s:           s,
		tx:          tx,
		query:       query,
		best:        make(map[string]float64),
		residual:    make([]float64, len(query)),
		recent:      newSPFreshRecentQueue(s.w),
		scoredCells: make(map[int64]struct{}),
		spanCent:    make(map[string][]float64),
	}
	routed, err := s.cache.route(tx, s.storage, query, s.w, s.kc)
	if err != nil {
		return nil, err
	}
	if len(routed) == 0 {
		f.empty = true
		f.done = true
		return f, nil
	}

	// SPANN Eq. (3) query-aware dynamic pruning: probe only the routed lists
	// whose centroid distance is within (1+ε) of the nearest one — easy
	// queries pay a handful of range reads, hard ones the full kc cap. The
	// pruned tail is admitted by widening below if the probed set starves the
	// re-rank budget (RFC-094 §4 adaptive widening).
	probe, pruned := spfreshPruneRouted(routed, s.epsilon)
	s.timer.IncrementBy(CountSPFreshPostingsProbed, int64(len(probe)))
	s.timer.IncrementBy(CountSPFreshPostingsPruned, int64(len(pruned)))
	f.pruned = pruned

	// One parallel burst over the probe set (all range reads issued before any
	// resolves). The fetch cap (4×Lmax+1 rows) bounds an unmaintained posting's
	// cost to THIS query (metered, never unbounded — RFC-094 §4).
	if err := f.scoreCells(probe); err != nil {
		return nil, err
	}
	return f, nil
}

// searchNext emits the next exact-re-ranked, distance-ordered candidates, up to
// demand of them. On the first call it advances the search to the conditional
// horizon the one-shot path scans (widen the ε-pruned tail iff the probe set
// starved the re-rank budget, then resolve forwarded children) and builds the
// finalized prefix; subsequent calls stream from it. exhausted reports that the
// finalized prefix is fully drained (the routed horizon is exhausted).
//
// RFC-156 invariant 2 — re-rank only finalized prefixes: nothing is emitted
// until it is exact-sidecar-re-ranked AND the conditional horizon is exhausted
// (no un-probed cell in that horizon can contain a closer vector). No RaBitQ-
// estimate-order emission. For the wrapper (demand = k) this is byte-for-byte
// identical to the pre-refactor one-shot search.
func (s *spfreshSearcher) searchNext(f *spfreshFrontier, demand int) ([]spfreshSearchResult, bool, error) {
	if demand <= 0 {
		return nil, f.done && f.emitPos >= len(f.finalized), nil
	}
	if f.empty {
		return nil, true, nil
	}
	if !f.done {
		if err := f.advance(demand); err != nil {
			return nil, false, err
		}
	}
	end := f.emitPos + demand
	if end > len(f.finalized) {
		end = len(f.finalized)
	}
	batch := f.finalized[f.emitPos:end]
	f.emitPos = end
	return batch, f.emitPos >= len(f.finalized), nil
}

// search returns the k nearest neighbors of query. The routing cache must be
// loaded (the maintainer refreshes it off the query path).
//
// RFC-156 Phase A: search is now a thin wrapper over the resumable iterator —
// searchInit then loop searchNext until k finalized results or exhausted. It is
// the safety net: byte-for-byte identical to the pre-refactor one-shot for every
// k (RFC-156 invariant 4), proven by the parity + ε-pruning-equivalence tests.
func (s *spfreshSearcher) search(tx fdb.ReadTransaction, query []float64, k int) ([]spfreshSearchResult, error) {
	if k <= 0 {
		return nil, nil
	}
	defer s.timer.RecordSince(EventSPFreshSearch, time.Now())
	f, err := s.searchInit(tx, query)
	if err != nil {
		return nil, err
	}
	var out []spfreshSearchResult
	for len(out) < k {
		batch, exhausted, nerr := s.searchNext(f, k-len(out))
		if nerr != nil {
			return nil, nerr
		}
		out = append(out, batch...)
		if exhausted {
			break
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	if k < len(out) {
		out = out[:k]
	}
	return out, nil
}

// advance scans the conditional horizon the one-shot path scans — probe (done
// at init) + the ε-pruned tail iff the probe set starved the re-rank budget +
// forwarded children — then builds the exact re-ranked, distance-ordered
// finalized prefix. Called once per frontier (guarded by done).
//
// RFC-156 invariants: the widen DECISION mirrors the one-shot exactly (checked
// once on the post-probe best size; cf. the comment below), and the starved-widen
// fetches the whole pruned tail in ONE parallel burst — byte-for-byte the one-shot
// spfreshPruneRouted + fetch-all (best is a min-est dedup map; a cell pruned at a
// tighter R_q stays pruned). The relaxed-monotonicity queues (refreshPhase2) are
// fed here but are PHASE-A TELEMETRY ONLY — they never truncate this exact
// horizon (the wrapper must stay byte-for-byte identical to the one-shot,
// invariant 4). The Phase C streaming cursor does NOT consult f.phase2 either: it
// emits exact-distance-ordered results gated by the relaxed-monotonicity EMISSION
// BARRIER (B = nextCentroidDist − maxResidual; see spfresh_stream.go), holding
// candidates ≥ B and flushing all at B=+inf on budget/exhaustion — there is no
// phase2-driven early stop anywhere. CountSPFreshPhase2Reached remains a pure
// observability counter.
func (f *spfreshFrontier) advance(demand int) error {
	s := f.s
	// cTop = max(s.c, k): gating widening on s.c alone skipped the pruned tail
	// once the probe set hit the rerank budget, so a k > s.c scan could return
	// fewer than k rows even with enough records in the pruned postings (codex).
	// cTop is reused at the top-C cut in finalize.
	cTop := s.c
	if cTop < demand {
		cTop = demand
	}
	f.cTop = cTop

	// Size the E=k smallest queue now that the demand is known, and seed it from
	// the probe estimates already deduped into best (it keeps only the E
	// smallest, so seeding from best is equivalent to having fed every probe
	// estimate). recentQueue was fed during the probe scan at init.
	f.smallest = newSPFreshSmallestQueue(demand)
	for _, est := range f.best {
		f.smallest.push(est)
	}
	f.refreshPhase2()

	// ε-pruning starvation widening (RFC-094 §4): if the pruned probe set can't
	// fill the re-rank budget, admit the pruned tail too — only on starved
	// queries, never beyond the kc cap. The decision is made ONCE on the
	// post-probe best size (one-shot semantics: it fetched the WHOLE pruned tail
	// when starved, not "until best ≥ cTop"). The wrapper widens the whole tail in
	// ONE parallel burst — byte-for-byte the pre-refactor fetchAndScore(pruned),
	// one RTT not one-per-cell. True per-step, demand-driven widening is the Phase C
	// streaming cursor's job (it pulls the tail incrementally across OnNext calls,
	// gated by the emission barrier — not by phase2); Phase A scans the full horizon
	// up front to guarantee wrapper parity (invariant 4).
	if f.tailPos < len(f.pruned) && len(f.best) < cTop {
		s.timer.Increment(CountSPFreshStarvationWiden)
		if err := f.scoreCells(f.pruned[f.tailPos:]); err != nil {
			return err
		}
		f.tailPos = len(f.pruned)
		f.refreshPhase2()
	}

	// Resolve forwarded children: one more parallel burst (depth 1; deeper
	// chains are the caller's refresh signal — we treat children's own HDRs as
	// absent entries there and rely on the next refresh, bounded per spec).
	if !f.forwardsResolved {
		if err := f.resolveForwardPostings(); err != nil {
			return err
		}
		f.forwardsResolved = true
	}

	if err := f.finalize(); err != nil {
		return err
	}
	f.done = true
	return nil
}

// scoreCells fetches each cell's posting list in one parallel burst (all range
// reads issued before any resolves) and scores its entries into best, queuing
// forwarded children and feeding the relaxed-monotonicity queues. The fetch cap
// (4×Lmax+1 rows) bounds an unmaintained posting's cost to THIS query.
func (f *spfreshFrontier) scoreCells(cells []spfreshRouted) error {
	s := f.s
	limit := 4*s.config.Lmax + 1
	type postingFetch struct {
		routed spfreshRouted
		future fdb.RangeResult
	}
	fetches := make([]postingFetch, 0, len(cells))
	for _, rt := range cells {
		r, rerr := s.storage.postingRange(rt.fineID)
		if rerr != nil {
			return rerr
		}
		if f.scoredCells != nil {
			f.scoredCells[rt.fineID] = struct{}{}
		}
		fetches = append(fetches, postingFetch{
			routed: rt,
			future: f.tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll}),
		})
	}
	for _, ft := range fetches {
		kvs, kerr := ft.future.GetSliceWithError()
		if kerr != nil {
			return fmt.Errorf("spfresh search: posting %d: %w", ft.routed.fineID, kerr)
		}
		s.timer.IncrementBy(CountSPFreshEntriesScanned, int64(len(kvs)))
		if len(kvs) == limit {
			s.timer.Increment(CountSPFreshCappedPostingReads)
			s.capped = append(s.capped, ft.routed)
		}
		for d := range f.query {
			f.residual[d] = f.query[d] - ft.routed.vec[d]
		}
		// One scorer per posting: the residual query's self-dot and the code
		// buffer are computed once and reused across the posting's codes (the
		// per-code allocation path dominated estimate CPU — 094.4).
		score := s.quant.scorer(f.residual, s.config.NumDimensions)
		prefixLen := len(s.storage.postings.Pack(tuple.Tuple{ft.routed.fineID}))
		for _, kv := range kvs {
			span, isEntry, perr := s.storage.postingPKSpan(kv.Key, prefixLen)
			if perr != nil {
				return perr
			}
			if !isEntry {
				// Forwarded posting (split landed inside our staleness window):
				// queue the children, resolved before finalize (+2 RT bounded).
				cellID, childA, childB, herr := decodePostingHDR(kv.Value)
				if herr != nil {
					return herr
				}
				fwd, ferr := s.resolveForward(f.tx, cellID, childA, childB)
				if ferr != nil {
					return ferr
				}
				f.forwards = append(f.forwards, fwd...)
				continue
			}
			est, derr := score(kv.Value)
			if derr != nil {
				return derr
			}
			f.observe(est)
			key := string(span)
			if cur, ok := f.best[key]; !ok || est < cur {
				f.best[key] = est
				if f.spanCent != nil {
					f.spanCent[key] = ft.routed.vec // min-estimate replica's centroid
				}
			}
		}
	}
	return nil
}

// resolveForwardPostings fetches the postings of the forwarded children queued
// during scanning, in one parallel burst, scoring them into best (RFC-156
// invariant 3 — HDR / forwarded-child handling, identical to the one-shot path:
// the parent contributes no entries post-FORWARD, depth-2 chains defer to the
// next cache refresh).
func (f *spfreshFrontier) resolveForwardPostings() error {
	s := f.s
	s.timer.IncrementBy(CountSPFreshForwardFollows, int64(len(f.forwards)))
	if len(f.forwards) == 0 {
		return nil
	}
	limit := 4*s.config.Lmax + 1
	type fwdFetch struct {
		routed spfreshRouted
		future fdb.RangeResult
	}
	ff := make([]fwdFetch, 0, len(f.forwards))
	for _, rt := range f.forwards {
		r, rerr := s.storage.postingRange(rt.fineID)
		if rerr != nil {
			return rerr
		}
		if f.scoredCells != nil {
			f.scoredCells[rt.fineID] = struct{}{}
		}
		ff = append(ff, fwdFetch{routed: rt, future: f.tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})})
	}
	for _, ft := range ff {
		kvs, kerr := ft.future.GetSliceWithError()
		if kerr != nil {
			return fmt.Errorf("spfresh search: forwarded posting %d: %w", ft.routed.fineID, kerr)
		}
		if len(kvs) == limit {
			s.timer.Increment(CountSPFreshCappedPostingReads)
			s.capped = append(s.capped, ft.routed)
		}
		for d := range f.query {
			f.residual[d] = f.query[d] - ft.routed.vec[d]
		}
		score := s.quant.scorer(f.residual, s.config.NumDimensions)
		prefixLen := len(s.storage.postings.Pack(tuple.Tuple{ft.routed.fineID}))
		for _, kv := range kvs {
			span, isEntry, perr := s.storage.postingPKSpan(kv.Key, prefixLen)
			if perr != nil || !isEntry {
				continue // chain depth 2: next refresh handles it
			}
			est, derr := score(kv.Value)
			if derr != nil {
				return derr
			}
			f.observe(est)
			key := string(span)
			if cur, ok := f.best[key]; !ok || est < cur {
				f.best[key] = est
				if f.spanCent != nil {
					f.spanCent[key] = ft.routed.vec // min-estimate replica's centroid
				}
			}
		}
	}
	return nil
}

// finalize builds the exact re-ranked, distance-ordered prefix over the scored
// horizon: top-C by RaBitQ estimate (deterministic span tie-break), then exact
// re-rank from the fp16 sidecar, then re-sort by true distance. This is the
// re-rank-only-finalized-prefix step (RFC-156 invariant 2) — it runs once the
// conditional horizon is exhausted, so the emitted order is true distance order,
// never RaBitQ-estimate order.
func (f *spfreshFrontier) finalize() error {
	s := f.s
	if len(f.best) == 0 {
		f.finalized = nil
		return nil
	}
	// Top-C by estimate; deterministic span tie-break.
	hits := make([]spfreshApproxHit, 0, len(f.best))
	for span, est := range f.best {
		hits = append(hits, spfreshApproxHit{span: span, est: est})
	}
	slices.SortFunc(hits, func(a, b spfreshApproxHit) int {
		if c := cmp.Compare(a.est, b.est); c != 0 {
			return c
		}
		return strings.Compare(a.span, b.span)
	})
	if f.cTop < len(hits) {
		hits = hits[:f.cTop]
	}

	// Exact re-rank from the fp16 sidecar (parallel point reads; the sidecar
	// key is built straight from the span — no decode). With the sidecar
	// disabled the estimates stand (the maintainer's record-read fallback
	// arrives with the maintainer slice).
	if s.config.Sidecar && !s.noRerank {
		s.timer.IncrementBy(CountSPFreshRerankReads, int64(len(hits)))
		futures := make([]fdb.FutureByteSlice, len(hits))
		for i, h := range hits {
			futures[i] = f.tx.Snapshot().Get(s.storage.sidecarKeyFromSpan(h.span))
		}
		kept := hits[:0]
		for i, h := range hits {
			data, gerr := futures[i].Get()
			if gerr != nil {
				return fmt.Errorf("spfresh search: sidecar read: %w", gerr)
			}
			if data == nil {
				continue // deleted between bursts: skip
			}
			vec, derr := vectorcodec.Deserialize(data)
			if derr != nil {
				return derr
			}
			h.est = spfreshMetricDistance(s.config.Metric, f.query, vec)
			kept = append(kept, h)
		}
		hits = kept
		slices.SortFunc(hits, func(a, b spfreshApproxHit) int {
			if c := cmp.Compare(a.est, b.est); c != 0 {
				return c
			}
			return strings.Compare(a.span, b.span)
		})
	}

	// Decode pk tuples for the finalized winners (the span is the flat-packed
	// pk; ~cTop decodes per query vs one per scanned posting entry).
	results := make([]spfreshSearchResult, 0, len(hits))
	for _, h := range hits {
		pk, derr := tuple.Unpack([]byte(h.span))
		if derr != nil {
			return fmt.Errorf("spfresh search: decode winner pk: %w", derr)
		}
		results = append(results, spfreshSearchResult{PrimaryKey: pk, Distance: h.est})
	}
	f.finalized = results
	return nil
}

// observe feeds one residual-estimate observation into the relaxed-monotonicity
// queues (VBASE §4.1): recentQueue (the last w traversed estimates) and, once
// sized at the first searchNext, smallestQueue (the E=k smallest).
func (f *spfreshFrontier) observe(est float64) {
	f.recent.push(est)
	if f.smallest != nil {
		f.smallest.push(est)
	}
}

// refreshPhase2 latches Phase 2 (VBASE Eq. 3: M_q^s > R_q) once both queues are
// full — the median of recently-traversed estimates exceeds the current k-th
// best. TELEMETRY ONLY (CountSPFreshPhase2Reached): nothing consults f.phase2 as
// a stop signal. The one-shot wrapper always scans its full conditional horizon;
// the Phase C streaming cursor terminates on the relaxed-monotonicity EMISSION
// BARRIER plus the budget/exhaustion caps (spfresh_stream.go), NOT on phase2.
func (f *spfreshFrontier) refreshPhase2() {
	if f.phase2 || f.smallest == nil || !f.smallest.full() || !f.recent.full() {
		return
	}
	if f.recent.median() > f.smallest.rq() {
		f.phase2 = true
		f.s.timer.Increment(CountSPFreshPhase2Reached)
	}
}

// spfreshRecentQueue is a ring buffer of the last w traversed residual estimates
// (VBASE §4.1): M_q^s = its median once full.
type spfreshRecentQueue struct {
	buf  []float64
	w    int
	n    int
	head int
}

func newSPFreshRecentQueue(w int) *spfreshRecentQueue {
	if w < 1 {
		w = 1
	}
	return &spfreshRecentQueue{buf: make([]float64, w), w: w}
}

func (q *spfreshRecentQueue) push(v float64) {
	q.buf[q.head] = v
	q.head = (q.head + 1) % q.w
	if q.n < q.w {
		q.n++
	}
}

func (q *spfreshRecentQueue) full() bool { return q.n == q.w }

func (q *spfreshRecentQueue) median() float64 {
	tmp := append([]float64(nil), q.buf[:q.n]...)
	slices.Sort(tmp)
	return tmp[q.n/2]
}

// spfreshSmallestQueue keeps the E=k smallest residual estimates seen (VBASE
// §4.1): R_q = its max (the E-th smallest, i.e. the running k-th best) once full.
type spfreshSmallestQueue struct {
	e    int
	vals []float64 // ascending, len ≤ e
}

func newSPFreshSmallestQueue(e int) *spfreshSmallestQueue {
	if e < 1 {
		e = 1
	}
	return &spfreshSmallestQueue{e: e, vals: make([]float64, 0, e)}
}

func (q *spfreshSmallestQueue) push(v float64) {
	if len(q.vals) == q.e && v >= q.vals[len(q.vals)-1] {
		return // not among the E smallest
	}
	i, _ := slices.BinarySearch(q.vals, v)
	q.vals = append(q.vals, 0)
	copy(q.vals[i+1:], q.vals[i:])
	q.vals[i] = v
	if len(q.vals) > q.e {
		q.vals = q.vals[:q.e]
	}
}

func (q *spfreshSmallestQueue) full() bool { return len(q.vals) == q.e }

func (q *spfreshSmallestQueue) rq() float64 { return q.vals[len(q.vals)-1] }

// resolveForward point-reads the children's centroid rows using the cellID
// CARRIED IN THE HDR — never the cell the client routed through (the parent
// may itself have moved cells; RFC-094 §3/§4, codex r4 #3).
func (s *spfreshSearcher) resolveForward(tx fdb.ReadTransaction, cellID, childA, childB int64) ([]spfreshRouted, error) {
	var out []spfreshRouted
	for _, fineID := range []int64{childA, childB} {
		if fineID == 0 {
			continue
		}
		data, err := tx.Snapshot().Get(s.storage.centroidKey(cellID, fineID)).Get()
		if err != nil {
			return nil, fmt.Errorf("spfresh search: forward child (%d,%d): %w", cellID, fineID, err)
		}
		if data == nil {
			continue // deeper staleness: the next cache refresh re-routes
		}
		row, derr := decodeCentroidRow(data)
		if derr != nil {
			return nil, derr
		}
		vec, verr := row.vector()
		if verr != nil {
			return nil, verr
		}
		out = append(out, spfreshRouted{cellID: cellID, fineID: fineID, vec: vec})
	}
	return out, nil
}

// spfreshMetricDistance computes the exact metric distance for re-ranking.
func spfreshMetricDistance(metric VectorMetric, a, b []float64) float64 {
	return vectorDistance(a, b, metric)
}
