# RFC-100 — SPFresh bulk-build distance kernel: float32 / SIMD / code-domain

**Status:** REJECTED (measured) — float32-scalar is a no-op in pure Go; SIMD
and int8 code-domain are deferred with rationale below. No code change. This RFC
is the decision record so a future shift does not re-chase a dud lever.
**Scope (investigated):** `pkg/recordlayer/spfresh_kmeans.go`
(`spfreshSquaredDistance`, the single largest flat CPU cost in build+routing
profiles), the build hot loops in `spfresh_build.go`, `pkg/rabitq/`.
**Gates:** Torvalds (code/measurement rigor), Graefe (systems), codex (external),
spfresh-reviewer (no recall change — there is no code change).

## Hypothesis (from the 10×-bulk-import lever list)

The build is CPU-bound on `spfreshSquaredDistance` (squared-L2, 128-dim). The
in-memory vectors are `[][]float64`. Storing/operating in `float32` would halve
the bytes touched and (the hope) ~2× the distance throughput — the standard
FAISS move. "Code-domain" int8 distance was the stretch goal (4× pack).

## Measurement (the load-bearing part)

Standalone micro-bench, exact copy of the production 4-lane f64 kernel vs an
8-lane f32 kernel (f32 accumulators) and a 4-lane f32-with-f64-accumulator
variant. SIFT-like uint8-valued data, 128-dim. AMD Ryzen 9 3900X (Zen 2, AVX2).

| Regime | Working set | f64 4-lane | f32 8-lane (f32 acc) | f32 (f64 acc) |
|---|---|---|---|---|
| **compute-bound** (candidate set L3-resident, N=8192 ⇒ 8MB f64) | fits L3 | 48.5 ns/dist | 48.3 ns/dist (**1.00×**) | 52.4 ns/dist (0.93×) |
| **bandwidth-bound** (N=65536 ⇒ 64MB f64 > 16MB L3) | exceeds L3 | 79.1 ns/dist | 62.9 ns/dist (**1.26×**) | 64.4 ns/dist (1.23×) |

Max relative error of the f32 kernel vs f64 on uint8 SIFT data: **0.000e+00**
(squared-L2 sums of uint8 differences are ≤ 128·255² ≈ 8.3M, exactly
representable in float32's 24-bit mantissa). So precision is NOT the blocker.

## Why float32-scalar is a no-op

**Go does not auto-vectorize.** A scalar `float32` multiply issues the same one
FP instruction as a scalar `float64` multiply — the 8-lane f32 kernel does 8
scalar ops where the 4-lane f64 does 4, but over half as many loop iterations:
**identical scalar FP-op count**, identical throughput. float32 wins *only* on
**memory bandwidth**, and only when the working set exceeds L3 (the 1.26× row).

The build's dominant cost — the two-level wave-B **assign** path (RFC-099) —
gathers `w_b` cells' fines (~`w_b × cellTarget` ≈ 32×48 ≈ 1.5 MB at 1M
defaults), which is **L3-resident ⇒ compute-bound ⇒ float32 buys nothing
(1.00×)**. The coarse-k-means pass streams all 1M points (≈1 GB) but is ALSO
compute-bound at production defaults, not bandwidth-bound: the reused centroid
set is only k₀≈246 vectors ≈ 0.2 MB (fits L1/L2), and each point is read once
and reused across all 246 centroid comparisons (246×128 flops/point), so the
point-stream bandwidth is amortized away. So float32's only measured win (1.13–
1.26× in a synthetic >L3 all-distinct micro-bench) does **not** apply to the real
coarse pass either — the true whole-build f32 win is **≤3 %, leaning lower**, in
exchange for migrating the entire build path from `[][]float64` to `[][]float32`
and re-pinning every determinism test (the float-reduction order changes). Poor
ROI; rejected. (The k-means *compute* cost is real and dominant — 34.6 % of build
CPU in profile — but the lever there is fewer distances via Hamerly bound pruning
in pure deterministic Go, RFC-102, not a float32 constant factor.)

## Why SIMD is deferred (not "never")

Realizing float32's benefit on the compute-bound assign path needs *actual*
SIMD (AVX2: 8×f32/instruction; AVX-512: 16×) — a 2–4× kernel win, the FAISS
approach. But:
1. **No assembly precedent.** The repo is deliberately pure-Go (`find -name '*.s'`
   ⇒ none; only `klauspost/cpuid` as an indirect dep). A hand-rolled `.s` kernel
   + pure-Go fallback + runtime CPU dispatch + Bazel/nogo wiring is a large,
   maintenance-heavy departure from the codebase's character (design principle
   5: "simple code").
2. **Determinism/reproducibility.** Build output (which fine a vector lands in)
   is pinned bit-exact per `(vectors, k, seed)` regardless of GOMAXPROCS
   (`spfresh_kmeans.go:16-21`). A per-ISA SIMD kernel makes centroids
   *machine-dependent* (AVX-512 box ≠ scalar box) unless the fallback and the
   asm are bit-identical — which AVX reductions are not, without extra ordering
   work. Not worth it for a build-time path.
3. **Algorithmic levers dominate.** Cutting the distance *count* (RFC-101
   Hamerly/Elkan bound pruning in the Lloyd assignment step; two-level routing
   already done in RFC-099) attacks the same cost in pure, deterministic,
   portable Go with no kernel rewrite — strictly better ROI than a 2× constant
   on every distance when bounds let us skip most distances entirely.

If a future requirement makes the kernel the proven bottleneck *after* the
algorithmic levers are exhausted, revisit SIMD as a self-contained,
fallback-guarded package with a determinism strategy (fixed reduction tree).

## Why int8 code-domain is deferred

Same root cause: in pure-Go *scalar* code, int8 arithmetic is promoted to int
and is **not** faster than float64 — the pack only pays off under SIMD (VNNI).
Separately, RaBitQ (`pkg/rabitq`) is *residual* quantization (codes relative to
an assigned centroid), so its `EstimateDistance` does not directly give a
query-vs-fine-centroid distance for the assignment decision without the centroid
context. A code-domain pre-filter is a real idea but is (a) SIMD-gated for the
speed and (b) recall-affecting (a cheap pre-filter can drop the true-nearest
fine), so it would need its own recall A/B — out of scope for "make the existing
float kernel faster," which is what this RFC set out to test.

## Decision

Do **not** migrate the build to float32 and do **not** add a SIMD kernel now.
The measured win is ≤3 % whole-build for a large, determinism-perturbing,
character-breaking change. Redirect the perf budget to the distance-*count*
levers that win in pure deterministic Go: **RFC-101** (Lloyd bound pruning) and
**RFC-102** (phase pipeline / parallel committers). The 4-lane f64 kernel stays;
it is already ~0.38 ns/dim, near scalar-FP throughput.
