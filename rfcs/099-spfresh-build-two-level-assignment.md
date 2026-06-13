# RFC-099 — SPFresh bulk-build: two-level wave-B assignment routing

**Status:** proposed
**Scope:** `pkg/recordlayer/spfresh_build.go` (`spfreshBuildRouter`, `assign`, `buildRouter`, `waveB`). Build path only — no wire-format, no query-path, no foreground-write change.
**Gates:** Torvalds (code), Graefe (systems/architecture), codex (external), spfresh-reviewer (recall fidelity — this changes assignment, so recall is in scope).

## Problem

The bulk build (`BuildSPFreshIndex`) is CPU-bound and dominated by wave-B
**assignment**: for every imported vector, `spfreshBuildRouter.assign` calls
`spfreshNearestK(vec, r.ids, r.vecs, pool)` where `r.ids` is the **global** fine
table — every fine centroid across every cell. At 1M vectors with ~6,100 fines
that is ≈ 1M × 6,100 × 128 ≈ 4.8×10¹¹ distance FLOPs, measured at **501 µs per
vector** (BenchmarkSPFreshBuildAssign, ~6,100 fines) ⇒ ~8 min of pure
assignment at 1M, the bulk of the build (the TODO-#6 "memory-bandwidth-bound,
~15–20 min" item). Wave-B is already parallel across cells; the cost is the
work *per vector*, not the loop.

## Key observation

The **query** path already routes two-level (RFC-094 §4): scan L1 coarse cells,
probe the *w* nearest cells, score only their fine centroids. The **build**
assignment does a flat global scan instead. This is not just slow — it is
*inconsistent*: the flat scan can assign a vector to a globally-near fine that
lives in a cell **no query probes**, so the entry is correct on disk but
unreachable by a kNN query. Two-level build routing makes assignment use the
**same** candidate set a query would, which is faster *and* recall-consistent.

## Design

`spfreshBuildRouter` gains the coarse layer:

```
type spfreshBuildRouter struct {
    coarseIDs  []int64       // all cells
    coarseVecs [][]float64   // cell routing centroids
    cellFineIDs  map[int64][]int64      // cell -> fine ids
    cellFineVecs map[int64][][]float64  // cell -> fine vecs
}
```

`assign(vec, rep, alpha)`:
1. `cells := spfreshNearestK(vec, coarseIDs, coarseVecs, w)` — the *w* nearest cells.
2. Gather the fines of those *w* cells into a candidate slice (`~w × cellTarget`).
3. `spfreshNearestK(vec, gathered, pool)` → `spfreshClosure(...)` — **identical**
   closure/RNG/bounded-widening logic as today, applied to the gathered subset.

`w` is the build assignment width `w_b`, a new per-index config option
`BuildAssignCells` / `spfreshBuildAssignCells`, **derived as exactly the query
probe width `w_q`** (`spfreshDefaultProbeW`, default 32) so the build assigns
over the SAME nearest-cell neighborhood a query navigates, and so a query-width
sweep moves `w_b` with it (no silent drift). The flat-scan path is removed;
`buildRouter` populates the coarse + per-cell maps it already has (`waveA`
produced per-cell fines; the coarse centroids come from `coarsePass`).

`w_b = w_q` (not larger): a replica the build would place in v's cell ranked
`w_q+1..w_b` is never reached by a query FOR v (which probes only `w_q` cells),
so a larger `w_b` is wasted build work with no recall gain (codex). A smaller
`w_b` is permitted (narrows candidate cells, never places an unreachable
replica) but the default mirrors the query.

## Recall argument (the load-bearing part)

Assignment correctness requires: a vector's chosen fine centroids are in cells a
query for that vector will probe. The query probes its *w_q* nearest cells
(default 32). If the build's *w_b* ≥ *w_q*, every fine the build could assign is
in a cell the query probes → **no recall loss vs flat scan, and a strict gain on
the flat scan's cross-cell mis-assignments**. We set `w_b` with margin over the
default `w_q`. The closure replication (RNG diversity, α-ratio, bounded
widening) is unchanged — it just operates on the two-level candidate set.

Edge cases:
- A vector whose true nearest fine straddles a cell boundary: covered as long as
  both cells are within `w_b` (margin handles it).
- Fewer than `w_b` cells exist (small index): gather all → identical to flat scan.

## Test plan

- **Micro:** BenchmarkSPFreshBuildAssign — expect ≈ (allFines / (w_b × cellTarget))×
  fewer distances; ~5–30× at 1M-scale topology.
- **Recall A/B (fast):** 100k real-SIFT bulk build, recall@10 before/after must
  not regress (baseline 0.997). w_b swept (16/32/48) to pick the default.
- **Convergence:** existing build+query e2e and the chunked-cascade tests stay green.

## Risks / rollback

Recall-affecting. If the A/B shows any regression at the chosen `w_b`, raise
`w_b` (toward the flat-scan limit) or revert. No wire/format change, so revert
is a pure code rollback; existing indexes are unaffected (build-time only).

## Measured

- **Assignment micro-bench** (BenchmarkSPFreshBuildAssign, ~6,100-fine topology,
  w_b=48): **501 µs → 136 µs per vector = 3.7×**, allocs 6→9 (pre-sized gather),
  bytes/op 1.6 KB→46 KB (the gathered candidate slice; bounded by w_b×cellTarget).
- **500k real-SIFT bulk build A/B** (flat w_b=100000 vs two-level w_b=48):
  build **6,776 → 9,266 vec/s = 1.37×**, recall@10 **0.9755 → 0.9755 (identical,
  zero regression)**. At 500k only ~60 cells form so w_b=48 prunes modestly; the
  full-build win grows with scale (at 1M, 246 cells by the formula below ⇒ the
  default w_b=32 scans ≈ 32/246 ≈ 13% of cells ⇒ ~7.7× fewer assignment
  distances, the dominant build cost).

- **Binding-regime A/B** (200k real-SIFT, CellTarget=4 ⇒ **589 coarse cells** —
  by `coarsePass`'s K₀ = ⌈N·r / (avgFill·CellTarget)⌉ with avgFill = (2·Lmax)/3
  in integer arithmetic (=170 at Lmax=256): ⌈400000/680⌉=589, confirmed by the
  `TOPOLOGY: cells=589` log — so w_b prunes hard, reproducing and exceeding the
  1M pruning ratio without a 1M build): recall@10 **0.9870 identical** at
  w_b ∈ {flat=100000, 48, 32}, even though w_b=32 gathers only **32/589 ≈ 5.4%**
  of cells (w_b=48 ≈ 8.1%). Build rate 4,261 → 5,116 vec/s flat→w_b=32. This is
  the load-bearing validation Graefe and the paper review asked for: recall
  holds where w_b BINDS HARD, confirming the closure's α-bounded replicas span
  only a few cells, so recall is insensitive to w_b above a small floor.
  Reproduce: `SPFRESH_BENCH=1 SIFT_N=200000 SIFT_CELL_TARGET=4 SIFT_BUILD_W={100000,48,32}`.

Two-level routing matches the query path, so assignment quality is unchanged
while the work per vector drops. The headline lever toward the 10× bulk-import
goal; stacks with float32/code-domain distance (RFC-100), cheaper k-means
(RFC-101), and pipeline/fan-out (RFC-102).
