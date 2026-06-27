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

// search returns the k nearest neighbors of query. The routing cache must be
// loaded (the maintainer refreshes it off the query path).
func (s *spfreshSearcher) search(tx fdb.ReadTransaction, query []float64, k int) ([]spfreshSearchResult, error) {
	if k <= 0 {
		return nil, nil
	}
	defer s.timer.RecordSince(EventSPFreshSearch, time.Now())
	routed, err := s.cache.route(tx, s.storage, query, s.w, s.kc)
	if err != nil {
		return nil, err
	}
	if len(routed) == 0 {
		return nil, nil
	}

	// SPANN Eq. (3) query-aware dynamic pruning: probe only the routed lists
	// whose centroid distance is within (1+ε) of the nearest one — easy
	// queries pay a handful of range reads, hard ones the full kc cap. The
	// pruned tail is refetched below if the probed set starves the re-rank
	// budget (RFC-094 §4 adaptive widening).
	probe, pruned := spfreshPruneRouted(routed, s.epsilon)
	s.timer.IncrementBy(CountSPFreshPostingsProbed, int64(len(probe)))
	s.timer.IncrementBy(CountSPFreshPostingsPruned, int64(len(pruned)))

	// One parallel burst: all posting range reads issued before any resolves.
	// The fetch cap (4×Lmax+1 rows) bounds an unmaintained posting's cost to
	// THIS query (metered, never unbounded — RFC-094 §4).
	limit := 4*s.config.Lmax + 1
	type postingFetch struct {
		routed spfreshRouted
		future fdb.RangeResult
	}

	// Residual distance estimation per posting; min-estimate dedup across
	// closure replicas (RFC-094 §4/§7). best is keyed by the raw pk span —
	// lookups with the string(span) conversion never allocate; assignments
	// (first insert AND min-improvement updates, bounded by the replica
	// count per pk) copy the span to an owned string. Nothing in the hot
	// loop decodes a tuple.
	best := make(map[string]float64)
	residual := make([]float64, len(query))
	var forwards []spfreshRouted // stale-cache HDR redirects, resolved after the burst
	fetchAndScore := func(rts []spfreshRouted) error {
		fetches := make([]postingFetch, 0, len(rts))
		for _, rt := range rts {
			r, rerr := s.storage.postingRange(rt.fineID)
			if rerr != nil {
				return rerr
			}
			fetches = append(fetches, postingFetch{
				routed: rt,
				future: tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll}),
			})
		}
		for _, f := range fetches {
			kvs, kerr := f.future.GetSliceWithError()
			if kerr != nil {
				return fmt.Errorf("spfresh search: posting %d: %w", f.routed.fineID, kerr)
			}
			s.timer.IncrementBy(CountSPFreshEntriesScanned, int64(len(kvs)))
			if len(kvs) == limit {
				s.timer.Increment(CountSPFreshCappedPostingReads)
				s.capped = append(s.capped, f.routed)
			}
			for d := range query {
				residual[d] = query[d] - f.routed.vec[d]
			}
			// One scorer per posting: the residual query's self-dot and the
			// code buffer are computed once and reused across the posting's
			// codes (the per-code allocation path dominated estimate CPU —
			// 094.4).
			score := s.quant.scorer(residual, s.config.NumDimensions)
			prefixLen := len(s.storage.postings.Pack(tuple.Tuple{f.routed.fineID}))
			for _, kv := range kvs {
				span, isEntry, perr := s.storage.postingPKSpan(kv.Key, prefixLen)
				if perr != nil {
					return perr
				}
				if !isEntry {
					// Forwarded posting (split landed inside our staleness
					// window): queue the children, resolved below (+2 RT
					// bounded).
					cellID, childA, childB, herr := decodePostingHDR(kv.Value)
					if herr != nil {
						return herr
					}
					fwd, ferr := s.resolveForward(tx, cellID, childA, childB)
					if ferr != nil {
						return ferr
					}
					forwards = append(forwards, fwd...)
					continue
				}
				est, derr := score(kv.Value)
				if derr != nil {
					return derr
				}
				if cur, ok := best[string(span)]; !ok || est < cur {
					best[string(span)] = est
				}
			}
		}
		return nil
	}
	if err := fetchAndScore(probe); err != nil {
		return nil, err
	}
	// ε-pruning starvation widening (RFC-094 §4): if the pruned probe set
	// can't fill the re-rank budget, fetch the pruned tail too — one extra
	// burst, only on starved queries, never beyond the kc cap. The budget is
	// the SAME cTop the re-rank keeps below (max(s.c, k)): gating on s.c alone
	// skipped the pruned tail once the probe set hit the rerank budget, so a
	// k > s.c scan could return fewer than k rows even with enough records in
	// the pruned postings (codex). cTop is reused at the top-C cut below.
	cTop := s.c
	if cTop < k {
		cTop = k
	}
	if len(pruned) > 0 && len(best) < cTop {
		s.timer.Increment(CountSPFreshStarvationWiden)
		if err := fetchAndScore(pruned); err != nil {
			return nil, err
		}
	}

	// Resolve forwarded children: one more parallel burst (depth 1; deeper
	// chains are the caller's refresh signal — we treat children's own HDRs
	// as absent entries here and rely on the next refresh, bounded per spec).
	s.timer.IncrementBy(CountSPFreshForwardFollows, int64(len(forwards)))
	if len(forwards) > 0 {
		type fwdFetch struct {
			routed spfreshRouted
			future fdb.RangeResult
		}
		ff := make([]fwdFetch, 0, len(forwards))
		for _, rt := range forwards {
			r, rerr := s.storage.postingRange(rt.fineID)
			if rerr != nil {
				return nil, rerr
			}
			ff = append(ff, fwdFetch{routed: rt, future: tx.Snapshot().GetRange(r, fdb.RangeOptions{Limit: limit, Mode: fdb.StreamingModeWantAll})})
		}
		for _, f := range ff {
			kvs, kerr := f.future.GetSliceWithError()
			if kerr != nil {
				return nil, fmt.Errorf("spfresh search: forwarded posting %d: %w", f.routed.fineID, kerr)
			}
			if len(kvs) == limit {
				s.timer.Increment(CountSPFreshCappedPostingReads)
				s.capped = append(s.capped, f.routed)
			}
			for d := range query {
				residual[d] = query[d] - f.routed.vec[d]
			}
			score := s.quant.scorer(residual, s.config.NumDimensions)
			prefixLen := len(s.storage.postings.Pack(tuple.Tuple{f.routed.fineID}))
			for _, kv := range kvs {
				span, isEntry, perr := s.storage.postingPKSpan(kv.Key, prefixLen)
				if perr != nil || !isEntry {
					continue // chain depth 2: next refresh handles it
				}
				est, derr := score(kv.Value)
				if derr != nil {
					return nil, derr
				}
				if cur, ok := best[string(span)]; !ok || est < cur {
					best[string(span)] = est
				}
			}
		}
	}

	if len(best) == 0 {
		return nil, nil
	}

	// Top-C by estimate; deterministic span tie-break.
	hits := make([]spfreshApproxHit, 0, len(best))
	for span, est := range best {
		hits = append(hits, spfreshApproxHit{span: span, est: est})
	}
	slices.SortFunc(hits, func(a, b spfreshApproxHit) int {
		if c := cmp.Compare(a.est, b.est); c != 0 {
			return c
		}
		return strings.Compare(a.span, b.span)
	})
	// cTop = max(s.c, k), computed above for the starvation budget.
	if cTop < len(hits) {
		hits = hits[:cTop]
	}

	// Exact re-rank from the fp16 sidecar (parallel point reads; the sidecar
	// key is built straight from the span — no decode). With the sidecar
	// disabled the estimates stand (the maintainer's record-read fallback
	// arrives with the maintainer slice).
	if s.config.Sidecar && !s.noRerank {
		s.timer.IncrementBy(CountSPFreshRerankReads, int64(len(hits)))
		futures := make([]fdb.FutureByteSlice, len(hits))
		for i, h := range hits {
			futures[i] = tx.Snapshot().Get(s.storage.sidecarKeyFromSpan(h.span))
		}
		kept := hits[:0]
		for i, h := range hits {
			data, gerr := futures[i].Get()
			if gerr != nil {
				return nil, fmt.Errorf("spfresh search: sidecar read: %w", gerr)
			}
			if data == nil {
				continue // deleted between bursts: skip
			}
			vec, derr := vectorcodec.Deserialize(data)
			if derr != nil {
				return nil, derr
			}
			h.est = spfreshMetricDistance(s.config.Metric, query, vec)
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
	if k < len(hits) {
		hits = hits[:k]
	}

	// Decode pk tuples for the k winners only (~k decodes per query vs one
	// per scanned posting entry before the span rewrite).
	results := make([]spfreshSearchResult, 0, len(hits))
	for _, h := range hits {
		pk, derr := tuple.Unpack([]byte(h.span))
		if derr != nil {
			return nil, fmt.Errorf("spfresh search: decode winner pk: %w", derr)
		}
		results = append(results, spfreshSearchResult{PrimaryKey: pk, Distance: h.est})
	}
	return results, nil
}

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
