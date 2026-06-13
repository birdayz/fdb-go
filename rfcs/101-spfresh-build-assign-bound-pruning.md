# RFC-101 — SPFresh bulk-build assign: triangle-inequality bound pruning

**Status:** proposed
**Scope:** `pkg/recordlayer/spfresh_build.go` (`spfreshBuildRouter`, `assign`,
`buildRouter`) + a new bound-pruned fused top-k in `spfresh_kmeans.go`. Build
path only — no wire format, no query path, no foreground write.
**Gates:** Torvalds (code), Graefe (systems), codex (external), spfresh-reviewer
(this is EXACT — identical assignment — so recall is unchanged by construction;
the spfresh-reviewer angle is "is it really exact").

## Problem (profile-grounded)

After RFC-099 made wave-B two-level, `assign` is still the dominant build cost
(≈137 s of a ~190 s 1M build). A CPU profile of `BenchmarkSPFreshBuildAssign`
(~245 coarse cells × ~25 fines ≈ 6,100 fines, w_b=32) is unambiguous:

```
flat  flat%        function
3.19s 64.6%  spfreshSquaredDistance      <- the cost IS distance FLOPs
0.56s 11.3%  spfreshNearestK (top-k bookkeeping)
0.15s  3.0%  runtime.memmove
0.10s  2.0%  spfreshNearestK insert
0.05s  1.0%  sort.Search
...          mapaccess 1.6%, closure 2.4%   <- gather/bookkeeping all minor
```

`assign` ≈ 97 µs/op, **85 % in squared-L2**, ≈10 % gather/top-k bookkeeping.
RFC-100 established that the *per-distance* cost cannot be cut in pure Go
(float32-scalar 1.00×, SIMD deferred). The only lever is **fewer distances.**

Per vector `assign` computes ≈ 245 coarse + ≈ w_b·cellFines (≈ 800) fine
distances. The fine scan is ≈77 % of the distances and is the prunable part.

## Key observation

`assign` already calls `spfreshNearestK(vec, coarseIDs, coarseVecs, w)` which
returns the w_b nearest cells **with their squared distance**
(`spfreshCandidate.d2`). So `d(v, c)` is in hand for every gathered cell **for
free**. The build clusters/routes uniformly in **squared L2**
(`spfresh_build.go:390`, `:568,:582`; cosine vectors are pre-normalized at
`:697`), and L2 is a metric, so the **triangle inequality holds in the build's
own distance space** for every configured `VectorMetric`:

```
d(v, f) >= | d(v, c) - d(c, f) |     for any fine f in cell c   (L2, exact)
```

If `(d(v,c) - maxRadius(c))² > kthBest_d2` then **no fine in cell c** can enter
the running top-k → skip the whole cell. Per-fine, if `(d(v,c) - d(c,f))² >
kthBest_d2` → skip that fine. This is **exact**: it removes only distances
provably outside the top-k, so the returned top-k (and thus the closure
copy-set and the on-disk assignment) is **bit-identical** to today's flat
gather. No recall change — by construction, not by A/B.

## Design

`buildRouter` precomputes, once, per cell:
- `cellFineDist[cellID] []float64` — `d(c_centroid, fine_j)` (L2 = √d²) for each
  fine, in the same order as `cellFineVecs[cellID]`.
- `cellRadius[cellID] float64` — `max_j cellFineDist[cellID][j]`.

Cost: one pass over the ~6,100 fines (≈6,100 squared-distances + √), at build
start, amortized over 1M assigns — negligible.

`assign` replaces the materialized gather + `spfreshNearestK` with a **fused
bound-pruned top-k** `spfreshTopKAcrossCells`:

```
cells := spfreshNearestK(vec, coarseIDs, coarseVecs, w)   // ascending d², unchanged
top := newBoundedTopK(pool)                                // (d2,id,vec), size pool
for _, c := range cells {
    dvc := sqrt(c.d2)
    if top.full() && sq(dvc - cellRadius[c.id]) > top.worst() { continue } // skip cell
    fd := cellFineDist[c.id]
    for j, fineVec := range cellFineVecs[c.id] {
        if top.full() {
            lb := dvc - fd[j]
            if lb > 0 && lb*lb > top.worst() { continue }   // skip fine
        }
        top.offer(cellFineIDs[c.id][j], spfreshSquaredDistance(vec, fineVec), fineVec)
    }
}
cands := top.sortedAscending()   // identical to spfreshNearestK(gathered, pool)
```

The closure (`spfreshClosure`) and the bounded-widening pool loop are
**unchanged** — they consume `cands` exactly as today. The fused form also drops
the per-assign gather allocation (the `gIDs`/`gVecs` slices, the `30 KB`/op,
most of the `9 allocs`).

`top.offer` keeps the same `(d2 asc, id asc)` order as `spfreshNearestK` so ties
break identically ⇒ bit-identical output. Determinism per `(vectors,k,seed)` is
preserved (no float-order change in the kept distances; skipped distances are
never summed into anything).

`spfreshNearestK` stays as the primitive for the coarse scan and for all other
callers (write/merge/npa) — RFC-101 touches only the build's fine scan.

## Recall / correctness argument

EXACT. The bound `(d(v,c) - d(c,f))² ≤ d(v,f)²` is the L2 triangle inequality;
a fine is skipped only when its lower bound already exceeds the pool-th best
*actual* squared distance, so it could not have been in the pool. The pool fed
to the closure is therefore identical set-and-order to the flat scan's, so:
- replication count, RNG diversity, α-ratio, bounded widening: all unchanged;
- the chosen fine IDs written to postings: unchanged;
- recall: unchanged (the binding-regime A/B from RFC-099 still applies verbatim;
  we additionally assert **bit-identical** assignment in the test).

Guard: the prune uses √ of squared L2. For `VectorMetricInnerProduct` the build
*still clusters in L2* (it does not re-define `assign`'s distance), so the bound
matches what `assign` computes and remains exact w.r.t. the build's own
behavior. (We are not asserting IP is a metric — we are asserting the prune is
exact w.r.t. the squared-L2 scan the build already performs.) A unit test pins
bit-identical output for Euclidean AND a normalized-cosine topology.

Float64 roundoff (cancellation): `d(v,c)` and `d(c,f)` are SEPARATELY-rounded
sqrts, so `d(v,c)-d(c,f)` cancels catastrophically when they are close (a fine
almost on the query but far from its cell centroid) — the raw `(d(v,c)-d(c,f))²`
can then exceed the true `d(v,f)²` by an amount no fixed slack can bound (the
relative overshoot → ∞ as `d(v,f) → 0`, codex P2). `spfreshPruneLowerBound`
subtracts a magnitude-scaled absolute error term `(d(v,c)+d(c,f))·(dims+2)·2⁻⁵¹`
(bounds the two sqrt roundings + the dims-term sum roundoff + the subtraction),
so the bound NEVER exceeds the actual computed distance; when cancellation
dominates the bound goes ≤ 0 and the fine is scored exactly rather than skipped.
`TestSPFreshPruneLowerBoundIsConservative` asserts `lb² ≤ d²(v,f)` over an
adversarial sweep driving the cancellation regime (the naive bound overshoots in
~180k/400k trials there; the guarded bound in zero) plus codex's exact triple.
Real SIFT data (uint8 0..255) never cancels; this guards the arbitrary-magnitude
float64 vectors the index accepts.

Subnormal regime: the magnitude-scaled error term is relative and underflows when
squared distances are subnormal (`< 0x1p-1022`, coordinates ~1e-160), where the
squaring `lb*lb` can round up by a subnormal ulp at an exact tie (codex P3). The
prune is therefore GATED to a normal-float pool boundary (`spfreshMinPrunableWorst
= 0x1p-1022`): a subnormal `worst` skips the prune and the fine is scored exactly,
so gatherTopK stays byte-identical. `TestSPFreshGatherTopKExactSubnormal` pins it
(codex's exact triple wrong-skips id 1 without the gate; passes with).

## Test plan

- **Exactness (the load-bearing test):** for several random topologies + metrics,
  assert `assign` (pruned) returns the **byte-identical** `(ids, fvecs)` as a
  reference flat scan — fuzz over dims, cell count, fines/cell, w_b, rep, alpha.
- **Micro-bench:** `BenchmarkSPFreshBuildAssign` before/after — the fine scan
  drops (measured 1.16× on `assign` at 128-dim, dimensionality-limited — see
  Measured); allocs/bytes drop sharply (gather removed, 10×).
- **Recall A/B:** 100k–200k real-SIFT build, recall@10 identical (it must be —
  exact); the binding-regime harness re-run as a backstop.
- **Determinism:** existing build+query e2e and chunked-cascade tests stay green;
  the workers=1-vs-parallel determinism tests unaffected (assign is per-vector).

## Measured

- **Exactness (700 random trials, fuzzed over dims/cells/fines/w_b/pool/rep/α):**
  `gatherTopK` byte-identical to a flat `spfreshNearestK` over the same cells
  (`TestSPFreshGatherTopKExactVsFlat`, 400 trials), and full `assign`
  byte-identical to the pre-RFC-101 flat assign (`TestSPFreshAssignExactVsFlat`,
  300 trials). Fail-closed.
- **Assign micro-bench** (`BenchmarkSPFreshBuildAssign`, 245 cells × 25 fines,
  128-dim, w_b=32): **97.2 µs → 84.0 µs/assign = 1.16×**; allocs/op **9 → 7**,
  bytes/op **30,019 → 3,011 (10× fewer)** — the per-vector gather slice is gone.
- **200k real-SIFT build A/B, default CellTarget** (50 cells, ~46 fines/cell,
  w_b=32 ⇒ gathers 64% of cells, so the prune barely binds and high-dim distance
  concentration limits it): build **10,889 → 11,492 vec/s = 1.055×**, recall@10
  **0.9940 → 0.9940 (identical — exact)**.

Honest framing: the distance-pruning win is **dimensionality-limited**. SIFT is
128-dim; the triangle-inequality bound (like Elkan/Hamerly) loses force as the
distance distribution concentrates in high dimensions, so cell/fine skips are
modest (1.16× on assign, ~5% end-to-end at 200k where w_b gathers most cells).
The win grows toward 1M (246 cells, w_b=32 gathers 13% ⇒ more whole cells
skippable) but stays incremental, not order-of-magnitude — the per-distance cost
is already at the pure-Go floor (RFC-100) and the count cannot be cut much in
high-D without changing recall. **The robust, scale-independent win is the
allocation elimination** (10× fewer bytes/assign ⇒ proportionally less GC churn
across 1M+ assigns) plus the EXACTNESS (zero recall risk) and the consistency
with the query path. Ship as a clean exact improvement, not the headline 10×
lever — there is no pure-Go headline 10× (that needs SIMD/GPU, out of scope).

## Risks / rollback

Exact transform → the only risk is an *implementation* bug making it non-exact,
which the bit-identical fuzz test is designed to catch (fail-closed). No
wire/format change; revert is pure code. Worst case the bound never prunes
(degrades to the flat scan + one √ per cell) — still correct, marginally slower;
the bit-identical test still passes.

## Stacks with

RFC-099 (two-level, done) narrowed the candidate cells; RFC-101 prunes within
them (exact). Independent of RFC-102 (k-means bound pruning / pipeline). These
are incremental, stacking wins on top of RFC-099 — not a 10× by themselves. The
per-distance kernel is already at the pure-Go floor (RFC-100); the honest ceiling
for pure-Go bulk build is "well-parallelized + these exact prunes," and a true
order-of-magnitude would require leaving pure Go (SIMD/GPU), a separate decision.
