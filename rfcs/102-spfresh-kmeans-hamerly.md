# RFC-102 — SPFresh bulk-build k-means: Hamerly bound pruning

**Status:** proposed (design review FIRST, then implement)
**Scope:** `pkg/recordlayer/spfresh_kmeans.go` (`spfreshKMeansWorkers` — the Lloyd
iteration). Build path only; no wire format, no query path, no foreground write.
**Gates:** Torvalds (code), Graefe (systems/determinism), codex (external),
spfresh-reviewer (k-means clustering quality feeds recall — in scope).

## Problem (profile-grounded)

A full 100k SIFT build CPU profile: `spfreshSquaredDistance` is **47.7% of all
build CPU (18.8s/39.5s)**. Its dominant caller is the **k-means Lloyd assignment
step** (`spfreshKMeansWorkers.func4`): **34.6% (13.66s)** — larger than wave-B
assign (22.5%, already cut by RFC-099 two-level + RFC-101 bound pruning). RaBitQ
encode is 7.2%; FDB syscalls+futex only ~10% (the build is CPU-bound, so
I/O-overlap/pipelining is NOT the lever — confirmed, not assumed).

Lloyd's assignment step is `O(n·k)` per iteration: every point computes its
distance to **every** centroid to find the nearest. At 1M with the default
topology that is 1M × 245 coarse centroids × iterations — the single biggest
distance sink in the build, and it grows with k (245 at 1M vs 25 at 100k).

## Key observation

Across Lloyd iterations, **most points stop changing cluster** after the first
few. Hamerly's algorithm (Hamerly, "Making k-means even faster", SDM'10) keeps,
per point i:
- `u(i)` — an upper bound on the distance to its **assigned** centroid;
- `l(i)` — a lower bound on the distance to its **second-nearest** centroid.

After centroids move by `δ(c)` in an iteration, update `u(i) += δ(a(i))` and
`l(i) -= max_{c≠a(i)} δ(c)`. If `u(i) <= l(i)`, no other centroid can be closer
than the assigned one → **skip the entire k-centroid scan for point i**. Only
when `u(i) > l(i)` do we tighten `u(i)` (recompute the one assigned distance) and,
if still `u(i) > l(i)`, do the full k-scan. In steady state the vast majority of
points are skipped, turning `O(n·k)` into ≈`O(n + (changed points)·k)` per
iteration.

This is the k-means analogue of RFC-101's assign pruning, but the win is from
**temporal stability across iterations**, NOT from single-comparison distance
concentration — so it is **not dimensionality-limited** the way RFC-101 was
(1.16× at 128-D). Hamerly routinely gives 2–5× on Lloyd, and the win **grows
with k** (more centroids to skip), so it is larger at 1M (k=245) than at 100k
(k=25). Memory is `O(n)` (two floats + an int per point) — feasible at 1M, unlike
Elkan's `O(n·k)`.

## Design

Augment `spfreshKMeansWorkers`'s Lloyd loop (the existing parallel-chunked
assignment at spfresh_kmeans.go:169-254):
- After k-means++ seeding, the first iteration is a full scan that also records
  `u(i)=d(i,a(i))`, `l(i)=d(i, 2nd-nearest)`.
- Each later iteration: compute per-centroid drift `δ(c)=‖newCentroid_c −
  oldCentroid_c‖` (k sqrts, negligible) and `δmax1/δmax2` (the two largest
  drifts) for the `l(i)` update. Per point, apply the bound updates; skip the
  scan when `u(i) <= l(i)`; else tighten and conditionally rescan.
- Bounds are **per-point, index-disjoint** → the existing chunked parallelism
  and its fixed-chunk reduction order are preserved unchanged.

## Determinism & recall (the load-bearing constraints)

1. **Determinism.** The pinned guarantee is *reproducible per (vectors,k,seed),
   GOMAXPROCS-invariant* — i.e. run-to-run and worker-count identical. Hamerly's
   bound updates are per-point and the centroid-update reduction order (the only
   float-order-sensitive part) is **unchanged** (same fixed chunks). So Hamerly
   stays deterministic and worker-invariant. **Open design question for the
   review:** do we require Hamerly to be *bit-identical to the current
   brute-force Lloyd* (⇒ conservative roundoff-aware bounds, the RFC-101
   treatment — `u` a guaranteed over-estimate, `l` a guaranteed under-estimate,
   else a roundoff-induced wrong-skip changes a reassignment and the centroids
   diverge), OR is "deterministic + recall-neutral, possibly a different valid
   local optimum" acceptable (⇒ standard bounds, far simpler)? The build
   clustering is build-time only (no wire impact) and the determinism tests
   compare Hamerly-vs-Hamerly (not against a frozen golden), so the looser
   contract passes them — but a reviewer may want bit-identical for auditability.
   **Recommendation: bit-identical via conservative bounds** (reuse RFC-101's
   `spfreshPruneLowerBound` discipline), so the change is provably a pure speedup
   and existing separation/quality tests are untouched.
2. **Recall.** If bit-identical (recommended), recall is unchanged by
   construction. If the looser contract is chosen, a 100k–200k real-SIFT recall
   A/B must show no regression. Either way the empty-cluster re-seed and the
   `k>n` clamp paths are preserved.

## Test plan

- **Exactness (if bit-identical):** assert Hamerly assignment == brute-force
  Lloyd assignment over random `(vectors,k,seed)`, fuzzed — byte-identical
  centroids. Reuse the conservative-bound roundoff regression shape from RFC-101.
- **Determinism:** existing `TestSPFreshKMeansDeterministic`,
  `…WorkerCountInvariance`, `…DeterministicParallel` stay green.
- **Micro-bench:** `BenchmarkSPFreshKMeans` before/after — expect a clear win
  that grows with k (sweep k); report distances-skipped.
- **Recall:** 100k–200k SIFT build recall@10 unchanged.
- **Convergence:** same iteration count / final objective as brute-force Lloyd.

## Risks / rollback

If bit-identical: same numerical-roundoff care as RFC-101 (conservative bounds);
the exactness fuzz is the safety net. Pure code change, build-time only, trivial
revert. Worst case Hamerly never skips (bookkeeping overhead only) — still
correct, marginally slower; caught by the bench.

## Stacks with / not in scope

Stacks with RFC-099 (two-level assign) + RFC-101 (assign pruning): together they
cut the two biggest distance sinks (assign + k-means) in pure deterministic Go.
**Out of scope (separate future RFC):** the per-distance kernel is at the pure-Go
scalar floor (RFC-100 — SIMD deferred); and a build-wide parallelism/scheduling
pass (the whole-test profile showed only ~1.4× CPU/wall, but that average is
diluted by the serial query+ground-truth phases — a build-only profile is needed
before claiming the build under-parallelizes; deferred to its own RFC).
