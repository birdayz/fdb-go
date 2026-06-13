# RFC-102 — SPFresh bulk-build k-means: convergence-fraction early-stop

**Status:** proposed (design pivoted from Hamerly after measurement + design review)
**Scope:** `pkg/recordlayer/spfresh_kmeans.go` (`spfreshKMeansCore` + a build-only
wrapper). Build path only (coarse + wave-A); foreground split/csplit unchanged.
No wire format, no query path.
**Gates:** Torvalds (code), Graefe (systems/determinism), codex (external),
spfresh-reviewer (k-means clustering feeds recall — in scope).

## Problem (profile-grounded)

A full-build CPU profile: `spfreshSquaredDistance` is **47.7 % of build CPU**,
its dominant caller the **k-means Lloyd assignment step (34.6 %, 13.66 s at
100k)** — bigger than wave-B assign (already cut by RFC-099 + RFC-101). Lloyd's
assignment is `O(n·k)` distances **every** iteration, regardless of how many
points actually move.

## Measurement (the decision driver — Torvalds: "this data should exist first")

**Per-iteration reassignment count** (instrumented, real run):
- Coarse-1M shape (n=100k, **k=246**): runs the **full 25 iters without
  converging** — reassignments decay `99900 → 13529 → … → 149` and are still
  >0 at iter 25. Early-stop (exact zero) **never fires**. 14 of 25 iters move
  <1 % of points: a long micro-refinement **tail**.
- Split shape (**k=2**, n=256/4096): converges in **2–3 iters** (`[207,70,0]`,
  `[2086,0]`) — exact-zero stop fires immediately.

**Recall vs iterations** (SIFT-100k, recall@10):

| maxIters | 25 | 10 | 6 | 4 |
|---|---|---|---|---|
| recall@10 | 0.9970 | 0.9950 | 0.9960 | 0.9950 |
| build vec/s | 12,162 | 13,578 | 14,328 | 14,363 |

Recall is **flat within query noise from 4 to 25 iterations.** The long tail
(iters ~5–25) is **pure wasted work — it does not move recall.**

## Design decision: stop the wasted tail, do NOT accelerate it (Hamerly rejected)

The design review first proposed **Hamerly bound pruning** (skip the k-scan for
points that can't change cluster). The measurement + review killed that:
- **Torvalds:** Hamerly's win is partly redundant with the existing early-stop,
  and the *simpler* lever is to stop the tail, not make it cheap. The A/B proves
  the tail is wasted — so **deleting** it beats **accelerating** it.
- Bit-identical Hamerly would need conservative roundoff bounds (the RFC-101
  4-round codex slog) **+** a tie-break-aware predicate (codex P2: the existing
  Lloyd breaks ties to the lowest index; `u≤l` skip violates that after a
  reseed) **+** reseed-as-teleport `δ=∞` handling (Graefe). High complexity to
  *preserve* iterations the A/B shows are worthless.

**Chosen lever:** a **convergence-fraction early-stop** — stop when fewer than
`ε·n` points are reassigned in an iteration (ε = 1 %). At high k this trims the
micro-refinement tail (stops ~iter 12 vs running to 25); at low k (splits) it is
inert because they reach exact zero first. ~10 lines, no bound state, no roundoff
or tie-break subtlety (it is a reassignment-count comparison — the count is an
order-independent sum, so the stop is deterministic and GOMAXPROCS-invariant).

## Why this satisfies the split-path bit-identity mandate (Graefe + spfresh-reviewer)

`spfreshKMeansCore` is **shared** with the foreground split/csplit path
(`spfresh_split.go:255,587`, `spfresh_csplit.go:154`, all **k=2**), which feeds
LIRE update-stability recall and has **no A/B harness**. So the convergence
fraction is a **parameter**, not a global change:
- **Splits/csplit** call `spfreshKMeans` → `convergeFraction = 0` ⇒ **exact-zero
  stop, bit-identical to today** — the foreground clustering is provably
  unchanged.
- **Build coarse + wave-A** call the new `spfreshKMeansBuild` → ε = 1 %, on the
  recall-A/B-validated bulk path only.

This is strictly safer than "rely on the empirical fact that k=2 converges before
the threshold" — the parameter makes the split path exact **by construction**.

## Determinism & recall

- **Determinism.** The reassignment count is an order-independent sum; the
  centroid-update reduction order is unchanged (fixed chunks). The stop decision
  depends only on that count ⇒ deterministic per (vectors,k,seed),
  GOMAXPROCS-invariant. Existing `TestSPFreshKMeansDeterministic /
  …WorkerCountInvariance / …DeterministicParallel` stay green (they compare
  threshold-vs-threshold).
- **Recall.** Build clustering changes slightly (stops the tail) — validated
  recall-neutral by the A/B above (flat 4–25 iters; ε=1 % stops ~iter 12, inside
  the flat region). Split clustering is bit-identical (fraction 0) ⇒ LIRE
  update-stability recall provably unchanged.
- **Empty-cluster reseed / k>n clamp** unchanged (they run on the same
  assignment Lloyd produces; the only change is *when* the loop stops).

## Test plan

- Determinism trio stays green (threshold-vs-threshold + workers=1-vs-parallel).
- A unit test: `convergeFraction=0` reproduces exact-zero-stop behavior
  (split-path bit-identity); a high-k case stops before maxIters with the
  fraction.
- Recall A/B (SIFT-100k/500k) recall@10 unchanged vs exact.
- Build-rate micro-measurement (the A/B's vec/s) records the speedup.

## Measured (threshold ON vs exact, SIFT)

| N | build EXACT | build ε=1% | speedup | recall EXACT | recall ε=1% |
|---|---|---|---|---|---|
| 100k | 12,351 vec/s | 12,776 vec/s | **1.034×** | 0.9970 | 0.9960 |
| 500k | 10,198 vec/s | 10,352 vec/s | **1.015×** | 0.9840 | 0.9830 |

Recall unchanged (Δ ≤ 0.001 = query noise). **Honest caveat: the measured win is
small (1.5–3.4 %, near noise) at these scales** because the coarse k is small
(k0 = 25 at 100k, 123 at 500k) and converges in a few iterations — the long
non-converging tail that this lever trims only appears at the **1M coarse pass
(k0 = 246)**, where the instrumented iteration curve shows 25 non-converging
iters (still 149 reassignments at iter 25). That 1M win is unmeasured here (a 1M
build exceeds the short-bench budget) but follows directly from the curve. So:
the lever's payoff scales with N/k and is marginal at verifiable scales.

## Risks / rollback

Build clustering is build-time only (no wire impact), recall-A/B-gated; split is
bit-identical. Pure code change, trivial revert. Worst case (ε too small) it
never trims ⇒ identical to today. ε is a named constant with the A/B as
justification; a future sweep can tune it (the A/B shows headroom down to ~4
iters, so ε has margin).

## Stacks with / not in scope

Completes the pure-Go bulk-build perf stack: RFC-099 (two-level assign) + RFC-101
(assign bound pruning) cut wave-B; RFC-102 trims the k-means tail. The
per-distance kernel is at the scalar floor (RFC-100; SIMD deferred). A build-wide
parallelism/scheduling pass is a separate future RFC (needs a build-only profile
— the whole-test ~1.4× CPU/wall is diluted by the serial query+ground-truth
phases).
