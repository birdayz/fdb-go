package recordlayer

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/vectorcodec"
)

// RFC-156 Phase C — demand-driven, budget-bounded ordered-stream SPFresh cursor.
//
// Phase A exposed a resumable frontier but its one-shot wrapper scans the FULL
// conditional horizon up front (probe + the ε-pruned tail iff starved + depth-1
// forwards). Phase B's planner shape (Limit → Filter → ordered VectorIndexScan)
// then culls a FIXED re-ranked horizon — so a rare residual whose matches lie
// beyond that horizon silently under-returns (< k, no signal).
//
// Phase C makes the ordered scan a true STREAMING cursor: as the consumer
// (FilterCursor → Limit) drains the current finalized prefix and pulls for more,
// the searcher WIDENS in a BATCH — admitting the next group of ε-pruned cells in
// d2 order, then (once that tail drains) re-routing across ALL cells with a
// larger w — re-ranks the newly-scored candidates, and yields more. It keeps
// widening until the Limit above has its k survivors (it stops pulling and
// closes the cursor), the BUDGET CAP is hit, or the index is exhausted.
//
// Widening is BATCHED (spfreshStreamBudget.widenBatch cells per pull, ONE
// parallel range-read burst each), never a one-cell-serial widen step (rejected
// in Phase A review).
//
// RELAXED-MONOTONICITY EMISSION BARRIER (RFC-156 invariant 2).
// Cells are admitted in centroid-d2 order, but a survivor in a
// later (farther-centroid) batch can be CLOSER than one already scored from a
// nearer cell. Emitting greedily would latch the wrong k-set under Filter→Limit
// — recall drift vs the one-shot, which scans its whole horizon BEFORE
// finalize()ing. So a candidate is finalized/emitted only once it is provably
// ahead of the widen frontier: B = the NEXT un-admitted cell's distance MINUS
// maxResidual (the largest member-to-centroid residual seen — the near-side
// correction for members closer to the query than their own centroid; see
// nextCellBarrier for the soundness argument, which rests on SPANN closure
// replication + LIRE-uniform radii, not an unconditional triangle bound).
// Candidates with distance < B emit; >= B are HELD and re-evaluated after the
// next widen (B grows). At the stop B=+inf so every held candidate flushes in
// order. Emission is thereby distance-monotone up to the frontier ≡ the
// one-shot's scanned-horizon finalize → no later widen surfaces a
// closer-than-emitted survivor (pinned by the oracle-parity + near-side-ablation
// tests). B is in the candidate metric space via spfreshMetricDistance on the
// centroid (reranked/estimate are metric-space — the RaBitQ estimate "equals
// dist(q,v)" per spfreshQuantizer.Distance — while d2 is SQUARED L2; mixing them
// would be a scale bug). METRIC GUARD: the residual subtraction is only valid for
// EUCLIDEAN; for every other metric the barrier is disabled and the cursor holds
// all candidates to the terminal then flushes the full sorted set
// (materialize-then-emit, exact for any metric — see streamInit / BLOCKER 4).
//
// Honest truncation: when the budget cap (max cells probed / max candidates) is
// hit before the consumer is satisfied, the cursor returns
// NoNextReason.ScanLimitReached + a positional continuation (NOT SourceExhausted)
// and fires CountSPFreshFilteredTruncated. "Only N rows match" instead surfaces
// SourceExhausted with exactly those N — two distinct outcomes, never a silent
// < k.
//
// Single-transaction snapshot: all widening happens within ONE transaction's
// generation-pinned snapshot. The continuation is POSITIONAL over the
// materialized result (mirrors Java's ListCursor + FromListWithContinuation) —
// the traversal frontier is never serialized; a cross-transaction resume re-runs
// the search and skips positionally (Java's semantics; RFC-156 §C).
//
// Memory is O(budget): the dedup map `best`, the exact-distance cache `reranked`,
// the emitted-span set, the re-routed centroid list, and the finalized prefix are
// each bounded by the candidate/cell budget — never by the corpus size (RFC-156
// invariant 5).

// spfreshStreamBudget bounds an ordered-stream search so it stays well within
// the FDB 5 s transaction limit (RFC-156 §6/§8). maxCells caps posting (range)
// reads; maxCandidates caps the retained dedup set (and thus the total re-rank
// point reads, each candidate re-ranked once). widenBatch is the cells admitted
// per demand-driven step.
type spfreshStreamBudget struct {
	maxCells      int
	maxCandidates int
	widenBatch    int
}

const (
	// Calibrated to stay within the 5 s tx for typical selectivities: ≤512 posting
	// range reads (issued in ≤8 bursts of 64) and ≤4000 candidate re-rank point
	// reads, all snapshot, all parallel within a burst. Validated by the
	// §8 budget-exhaustion stress scenario. Tune here, never a design constant.
	spfreshDefaultStreamCellBudget      = 512
	spfreshDefaultStreamCandidateBudget = 4000
	spfreshStreamWidenBatch             = 64
)

func defaultSPFreshStreamBudget() spfreshStreamBudget {
	return spfreshStreamBudget{
		maxCells:      spfreshDefaultStreamCellBudget,
		maxCandidates: spfreshDefaultStreamCandidateBudget,
		widenBatch:    spfreshStreamWidenBatch,
	}
}

func (b spfreshStreamBudget) sanitized() spfreshStreamBudget {
	if b.maxCells <= 0 {
		b.maxCells = spfreshDefaultStreamCellBudget
	}
	if b.maxCandidates <= 0 {
		b.maxCandidates = spfreshDefaultStreamCandidateBudget
	}
	if b.widenBatch <= 0 {
		b.widenBatch = spfreshStreamWidenBatch
	}
	return b
}

// streamInit primes the frontier for streaming use: copies the budget caps,
// seeds cellsProbed from the cells the probe set already scored, and allocates
// the streaming-only maps. searchInit must have run first (it scored the probe
// set into best and recorded those cells in scoredCells).
func (f *spfreshFrontier) streamInit(budget spfreshStreamBudget) {
	b := budget.sanitized()
	f.maxCells = b.maxCells
	f.maxCandidates = b.maxCandidates
	f.widenBatch = b.widenBatch
	f.reranked = make(map[string]float64)
	f.deleted = make(map[string]struct{})
	f.emittedSpans = make(map[string]struct{})
	// BLOCKER 4 — metric guard. The emission barrier subtracts a residual margin
	// (a REVERSE triangle inequality), which is only valid when the re-rank metric
	// is a true metric whose admission order matches it. That holds for EUCLIDEAN
	// (sqrt-L2) only: squared-L2 (EuclideanSquare) fails the triangle inequality,
	// and cosine (1−cos) / unnormalized inner-product (−dot) are not metrics at all
	// (and IP's d2-admission order can even disagree with metric order). For every
	// non-Euclidean metric we DISABLE the streaming barrier and fall back to
	// materialize-then-emit: hold ALL candidates through widening and flush the
	// full sorted scanned set at the terminal — EXACT for any metric (it is the
	// one-shot's behaviour, just without early streaming). The early-streaming
	// barrier is the Euclidean fast path; correctness never depends on it.
	//
	// SIDECAR guard. The barrier margin maxResidual (the max cell residual
	// ‖vec−centroid‖) is accumulated ONLY in the sidecar re-rank loop
	// (streamRebuildFinalized). With Sidecar=false (or noRerank) it stays 0, so
	// B = nextCentroidDist − maxResidual degrades to the PURE-CENTROID barrier —
	// which admits in centroid (d2) order, NOT exact-distance order, so early
	// emission can be out of order (recall blip). Gate the barrier on Sidecar too
	// and fall back to the same materialize-then-emit path the non-Euclidean
	// metrics use (B=−inf through widening, +inf at the terminal): exact, just no
	// early streaming. (Mirrors the accumulation guard at streamRebuildFinalized.)
	f.barrierEnabled = f.s.config.Metric == VectorMetricEuclidean &&
		f.s.config.Sidecar && !f.s.noRerank
	f.cellsProbed = len(f.scoredCells)
	if len(f.best) > f.peakBest {
		f.peakBest = len(f.best)
	}
	// The probe set may already exceed the candidate budget on a dense query —
	// honour the cap from the first pull rather than after the first widen.
	if f.cellsProbed >= f.maxCells || len(f.best) >= f.maxCandidates {
		f.budgetHit = true
	}
}

// streamWidenBatch admits the next batch of cells in centroid-distance (d2) order
// — first the ε-pruned tail left by searchInit, then (once it drains) the
// all-cells re-route — scores them into best (one parallel burst), resolves any
// forwarded children, and feeds the relaxed-monotonicity queues. It sets
// budgetHit when the cell/candidate cap is reached and streamExhaust when the
// routed horizon is genuinely exhausted (the index has nothing more to admit).
func (f *spfreshFrontier) streamWidenBatch() error {
	s := f.s
	batch := make([]spfreshRouted, 0, f.widenBatch)

	// 1. Drain the ε-pruned tail (cells within the original w-nearest set that
	// SPANN Eq. (3) skipped). Already-scored cells are dedup-skipped.
	for f.tailPos < len(f.pruned) && len(batch) < f.widenBatch {
		rt := f.pruned[f.tailPos]
		f.tailPos++
		if _, done := f.scoredCells[rt.fineID]; done {
			continue
		}
		batch = append(batch, rt)
	}

	// 2. Tail drained — re-route across ALL coarse cells (a larger w) to widen
	// beyond the original routed set, admitting new centroids in d2 order. The
	// re-route is bounded to maxCells nearest centroids, so widenRouted is
	// O(budget), never O(corpus).
	if len(batch) < f.widenBatch && f.tailPos >= len(f.pruned) {
		if !f.widenRerouted {
			if err := f.streamReroute(); err != nil {
				return err
			}
		}
		for f.widenPos < len(f.widenRouted) && len(batch) < f.widenBatch {
			rt := f.widenRouted[f.widenPos]
			f.widenPos++
			if _, done := f.scoredCells[rt.fineID]; done {
				continue
			}
			batch = append(batch, rt)
		}
	}

	if len(batch) == 0 {
		// Nothing left to admit. Claiming exhaustion (SourceExhausted — "the index
		// is complete, this is the whole answer") requires BOTH the re-route
		// returned the COMPLETE centroid set AND no posting was read-capped this
		// scan. A capped posting (fetch hit the 4×Lmax+1 read cap — RFC-094 §4)
		// is an oversized, unmaintained list whose larger-PK tail was INVISIBLE to
		// scoreCells: rows we never examined. Claiming completeness with capped
		// postings outstanding is a silent under-return — a residual matching only
		// an invisible-tail row would return SourceExhausted with the wrong/empty
		// set and no truncation signal. Report budget truncation instead →
		// ScanLimitReached, and refileCapped (at the terminal) re-files the split
		// so a retry after the rebalancer maintains the posting sees the true rows.
		// Mirrors the coarse-route conservatism (streamReroute: len(routed) < maxCells).
		if f.widenRouteComplete && len(f.s.capped) == 0 {
			f.streamExhaust = true
		} else {
			f.budgetHit = true
		}
		return nil
	}

	s.timer.Increment(CountSPFreshStreamWiden)
	if err := f.scoreCells(batch); err != nil {
		return err
	}
	f.cellsProbed += len(batch)

	// Resolve any forwarded children queued by this batch (SEALED→FORWARD seen
	// mid-traversal), then clear the queue so the next batch starts fresh.
	if len(f.forwards) > 0 {
		if err := f.resolveForwardPostings(); err != nil {
			return err
		}
		f.forwards = nil
	}

	if len(f.best) > f.peakBest {
		f.peakBest = len(f.best)
	}
	if f.cellsProbed >= f.maxCells || len(f.best) >= f.maxCandidates {
		f.budgetHit = true
	}
	return nil
}

// streamReroute routes across ALL coarse cells (w = cache.coarseCount()) for up
// to maxCells nearest centroids, drops the cells already scored, and keeps the
// remainder d2-sorted for batched admission. widenRouteComplete records whether
// the route returned the whole centroid set (so a drained widenRouted means the
// index is exhausted) or was capped at the budget (so more cells exist).
//
// Cost note: this single re-route is O(#centroids) (the routing scan over L1/L2),
// the ONE step not bounded by maxCells — but #centroids ≪ #vectors (each cell
// holds ~Lmax entries), so it stays well below O(corpus). It runs at most once
// per query (guarded by widenRerouted), after the ε-pruned tail drains.
func (f *spfreshFrontier) streamReroute() error {
	s := f.s
	f.widenRerouted = true
	allCells := s.cache.coarseCount()
	if allCells <= 0 {
		f.widenRouteComplete = true
		return nil
	}
	routed, err := s.cache.route(f.tx, s.storage, f.query, allCells, f.maxCells)
	if err != nil {
		if errors.Is(err, errSPFreshEmptyRouting) {
			f.widenRouteComplete = true
			return nil
		}
		return err
	}
	// route caps at kc(=maxCells) AFTER d2-sorting; a result shorter than the cap
	// is the complete centroid set. (At the exact maxCells boundary we
	// conservatively treat it as capped — reports ScanLimitReached rather than
	// claiming exhaustion it can't prove.)
	f.widenRouteComplete = len(routed) < f.maxCells
	out := routed[:0]
	for _, rt := range routed {
		if _, done := f.scoredCells[rt.fineID]; done {
			continue
		}
		out = append(out, rt)
	}
	f.widenRouted = out
	f.widenPos = 0
	return nil
}

// nextCellBarrier returns B — the distance below which a candidate is treated as
// ahead of the widen frontier. It is the NEXT un-admitted cell's centroid distance
// MINUS maxResidual (the largest member-to-centroid residual ‖v−c‖ seen):
// dist(q,v) >= dist(q,c') − ‖v−c'‖ >= nextCentroidDist − maxResidual.
//
// SOUNDNESS (the honest version). This is NOT an
// unconditional triangle bound: maxResidual is a RUNNING max over already-scored
// members, so a far-centroid cell admitted late could in principle hold a
// near-query, higher-than-seen-residual member. What actually makes the bound
// sound for SPFresh is SPANN's (1+ε) CLOSURE REPLICATION: such a boundary member
// is also replicated into its NEAR centroid (its closure), so it is scored EARLY
// — it is already in `best`, and maxResidual has effectively converged — before
// the far cell admits (VBASE §3.3's equivalence guarantee). LIRE keeps cell radii
// roughly uniform, so maxResidual from near cells bounds far cells in practice.
// Net: EXACT for Euclidean under complete replication; a practical (not
// unconditional) bound otherwise — and correctness is never RISKED because a wrong
// guess only reorders WITHIN the scanned set, while a skewed far-cell radius is at
// worst a recall blip, never a lost record. (For non-Euclidean metrics the bound
// is invalid entirely and the barrier is disabled — see streamInit / the caller.)
//
// B is in the candidate metric space (spfreshMetricDistance on the centroid; the
// cell's stored d2 is SQUARED L2 — not directly comparable). Returns +inf once no
// cell remains (the one-time re-route is triggered here to learn the next cell).
func (f *spfreshFrontier) nextCellBarrier() (float64, error) {
	s := f.s
	var nextDist float64
	switch {
	case f.tailPos < len(f.pruned):
		nextDist = spfreshMetricDistance(s.config.Metric, f.query, f.pruned[f.tailPos].vec)
	default:
		if !f.widenRerouted {
			if err := f.streamReroute(); err != nil {
				return 0, err
			}
		}
		if f.widenPos < len(f.widenRouted) {
			nextDist = spfreshMetricDistance(s.config.Metric, f.query, f.widenRouted[f.widenPos].vec)
		} else {
			return math.Inf(1), nil // no cell remains
		}
	}
	margin := f.maxResidual
	if f.disableNearSide {
		margin = 0 // ablation: pure-centroid barrier (proves −maxResidual is load-bearing)
	}
	b := nextDist - margin
	if b < 0 {
		b = 0
	}
	return b, nil
}

// streamRebuildFinalized exact-re-ranks every newly-scored candidate (once per
// span, cached in reranked; also accumulating maxResidual for the barrier), then
// rebuilds the un-emitted, distance-sorted prefix from (best \ emitted \ deleted)
// — holding back any candidate NOT yet provably ahead of the widen frontier
// (distance >= B). Only candidates strictly nearer than B are finalized; held
// ones stay for the next widen (B grows as farther cells admit). At a terminal
// (exhaustion / budget) B=+inf, so every held candidate flushes in order and
// streamFlushed latches. Emission is therefore distance-monotone up to the
// frontier ≡ the one-shot's whole-horizon finalize (RFC-156 invariant 2; no
// recall drift). Deterministic span tie-break → pagination stability.
//
// For a non-Euclidean metric (barrierEnabled=false) B is −inf throughout widening
// (hold EVERY candidate) and +inf only at the terminal — i.e. materialize-then-
// emit the full sorted scanned set, exact for any metric (BLOCKER 4).
func (f *spfreshFrontier) streamRebuildFinalized() error {
	s := f.s

	// Re-rank the newly-scored candidates FIRST (so maxResidual reflects this
	// batch before the barrier is computed from it).
	if s.config.Sidecar && !s.noRerank {
		var need []string
		for span := range f.best {
			if _, emitted := f.emittedSpans[span]; emitted {
				continue
			}
			if _, del := f.deleted[span]; del {
				continue
			}
			if _, ok := f.reranked[span]; ok {
				continue
			}
			need = append(need, span)
		}
		if len(need) > 0 {
			s.timer.IncrementBy(CountSPFreshRerankReads, int64(len(need)))
			futures := make([]fdb.FutureByteSlice, len(need))
			for i, span := range need {
				futures[i] = f.tx.Snapshot().Get(s.storage.sidecarKeyFromSpan(span))
			}
			for i, span := range need {
				data, gerr := futures[i].Get()
				if gerr != nil {
					return fmt.Errorf("spfresh stream: sidecar read: %w", gerr)
				}
				if data == nil {
					f.deleted[span] = struct{}{} // deleted between bursts
					continue
				}
				vec, derr := vectorcodec.Deserialize(data)
				if derr != nil {
					return derr
				}
				f.reranked[span] = spfreshMetricDistance(s.config.Metric, f.query, vec)
				// Track the largest member-to-centroid residual for the near-side
				// barrier correction (triangle inequality; see nextCellBarrier).
				if cv, ok := f.spanCent[span]; ok {
					if r := spfreshMetricDistance(s.config.Metric, vec, cv); r > f.maxResidual {
						f.maxResidual = r
					}
				}
			}
		}
	}

	// B = +inf at a terminal (release everything, in order). During widening:
	// the near-side-corrected frontier for Euclidean, or −inf (hold EVERYTHING
	// until the terminal — materialize-then-emit) for any metric where the
	// reverse-triangle bound is invalid (BLOCKER 4).
	barrier := math.Inf(1)
	if f.streamExhaust || f.budgetHit {
		f.streamFlushed = true
	} else if !f.barrierEnabled {
		barrier = math.Inf(-1) // non-Euclidean: hold all candidates until the terminal
	} else {
		b, berr := f.nextCellBarrier()
		if berr != nil {
			return berr
		}
		barrier = b
	}

	hits := f.streamFinalized[:0]
	for span, est := range f.best {
		if _, emitted := f.emittedSpans[span]; emitted {
			continue
		}
		if _, del := f.deleted[span]; del {
			continue
		}
		dist := est // RaBitQ estimate fallback when the sidecar is disabled
		if d, ok := f.reranked[span]; ok {
			dist = d
		}
		if dist >= barrier {
			continue // HELD: not yet ahead of the widen frontier (a nearer vector
			// may still lie in an un-admitted cell). Re-evaluated next widen.
		}
		hits = append(hits, spfreshStreamHit{span: span, dist: dist})
	}
	slices.SortFunc(hits, func(a, b spfreshStreamHit) int {
		if c := cmp.Compare(a.dist, b.dist); c != 0 {
			return c
		}
		return strings.Compare(a.span, b.span)
	})
	f.streamFinalized = hits
	f.streamPos = 0
	return nil
}

// spfreshStreamCursor is the RecordCursor[*IndexEntry] over a streaming frontier
// (RFC-156 Phase C). It lives for one transaction; pagination is positional.
type spfreshStreamCursor struct {
	m          *spfreshIndexMaintainer
	f          *spfreshFrontier
	storage    *spfreshStorage
	skip       int  // positional resume: rows to discard before emitting
	pos        int  // total rows produced so far (incl. skipped) — the continuation
	capRefiled bool // read-path split re-file done (once, at terminal/Close)
	closed     bool
}

func (c *spfreshStreamCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}
	f := c.f
	if f.empty {
		return c.terminalExhausted()
	}
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		if f.streamPos < len(f.streamFinalized) {
			hit := f.streamFinalized[f.streamPos]
			f.streamPos++
			f.emittedSpans[hit.span] = struct{}{}
			c.pos++
			if c.skip > 0 {
				c.skip--
				continue
			}
			pk, derr := tuple.Unpack([]byte(hit.span))
			if derr != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("spfresh stream: decode pk: %w", derr)
			}
			entry := &IndexEntry{Index: c.m.index, Key: pk, Value: tuple.Tuple{nil}, primaryKey: pk}
			return NewResultWithValue(entry, NewBytesContinuation(listCursorContinuation(c.pos))), nil
		}
		// Finalized prefix drained.
		// First pull: build the prefix from the PROBE set (held candidates wait
		// behind the barrier). streamInit may pre-set budgetHit (a dense probe
		// already exceeds the candidate budget) — the rebuild then flushes with
		// B=+inf so the probe candidates are still emitted, never lost to a
		// premature truncation.
		if !f.streamStarted {
			f.streamStarted = true
			if err := f.streamRebuildFinalized(); err != nil {
				return RecordCursorResult[*IndexEntry]{}, err
			}
			continue
		}
		// Once the final B=+inf flush has run and drained, all held candidates have
		// been released in order — only now report the terminal reason.
		if f.streamFlushed {
			if f.streamExhaust {
				return c.terminalExhausted()
			}
			return c.terminalTruncated() // budget cap
		}
		// At a terminal (exhaustion / budget) but not yet flushed: rebuild once
		// with B=+inf to release every held candidate in order.
		if f.streamExhaust || f.budgetHit {
			if err := f.streamRebuildFinalized(); err != nil {
				return RecordCursorResult[*IndexEntry]{}, err
			}
			continue
		}
		// Otherwise widen one batch and rebuild against the new frontier (which may
		// itself set a terminal flag, handled on the next iteration).
		if err := f.streamWidenBatch(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		if err := f.streamRebuildFinalized(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
		// Loop: emit the freshly-finalized rows, or hit a terminal next iteration.
	}
}

// terminalExhausted: the routed horizon is exhausted (the predicate matched at
// most the rows already emitted). SourceExhausted + EndContinuation.
func (c *spfreshStreamCursor) terminalExhausted() (RecordCursorResult[*IndexEntry], error) {
	if err := c.refileCapped(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
}

// terminalTruncated: the budget cap was hit before the consumer was satisfied.
// ScanLimitReached + a positional continuation (resumable, more may exist) and
// the CountSPFreshFilteredTruncated signal — never a silent < k.
func (c *spfreshStreamCursor) terminalTruncated() (RecordCursorResult[*IndexEntry], error) {
	if err := c.refileCapped(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.f.s.timer.Increment(CountSPFreshFilteredTruncated)
	return NewResultNoNext[*IndexEntry](
		ScanLimitReached, NewBytesContinuation(listCursorContinuation(c.pos)),
	), nil
}

// refileCapped runs the read-path split re-file for any posting whose fetch hit
// the 4×Lmax+1 cap during this stream (the same envelope repair the one-shot
// searchCurrentGeneration performs after search). Once per cursor, at terminal.
func (c *spfreshStreamCursor) refileCapped() error {
	if c.capRefiled {
		return nil
	}
	c.capRefiled = true
	if c.f == nil || len(c.f.s.capped) == 0 {
		return nil
	}
	if ferr := c.m.spfreshFileSplitsForCapped(c.storage, c.f.s.capped); ferr != nil {
		return fmt.Errorf("spfresh index %q: read-path split re-file: %w", c.m.index.Name, ferr)
	}
	return nil
}

// Close runs the read-path capped-split re-file too (idempotent via capRefiled):
// an early Close on a satisfied Limit(k) returns before the cursor reaches a
// terminal, so without this the envelope repair for any capped posting scanned so
// far would be skipped. The maintainer tx is still live at Close.
func (c *spfreshStreamCursor) Close() error {
	c.closed = true
	if c.f == nil {
		return nil
	}
	return c.refileCapped()
}

func (c *spfreshStreamCursor) IsClosed() bool { return c.closed }

var _ RecordCursor[*IndexEntry] = (*spfreshStreamCursor)(nil)

// parseStreamPosition reads a 4-byte big-endian positional continuation (the
// same format as FromListWithContinuation). A nil/empty continuation starts at
// the beginning; a short one is an error (Java fail-fast parity).
//
// Cross-tx resume re-runs the search and skips `pos` rows positionally, so deep
// pagination is O(pos) per page → O(pos²) over a full paginated drain. This is
// Java ListCursor parity (materialize-then-skip) and accepted (RFC-156 §C): the
// traversal frontier is deliberately never serialized, keeping the wire format
// and continuation untouched.
func parseStreamPosition(continuation []byte) (int, error) {
	if len(continuation) == 0 {
		return 0, nil
	}
	if len(continuation) < 4 {
		return 0, fmt.Errorf("spfresh stream: invalid continuation: expected at least 4 bytes, got %d", len(continuation))
	}
	return int(continuation[0])<<24 | int(continuation[1])<<16 | int(continuation[2])<<8 | int(continuation[3]), nil
}
