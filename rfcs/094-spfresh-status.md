# RFC-094 (status): SPFresh — current state & open work

**Status:** Living tracker (authoritative). This is the single index of what's
**shipped**, what's **open**, and what's been **measured and rejected** for the
SPFresh FDB-native vector index. Per-feature *design* detail lives in the numbered
RFCs linked at the bottom; *benchmark numbers* in
`pkg/recordlayer/VECTOR_BENCHMARK_RESULTS.md`. This file supersedes the scattered
SPFresh sections that used to live in `TODO.md`.

**Last code-verified: 2026-06-14.** Every "shipped"/"open" line below was
cross-checked against the code (file:line) and, where relevant, a passing test —
not carried forward from prior notes. Items proven stale during that pass are
flagged inline.

---

## TL;DR

SPFresh is a **feature-complete, FDB-native vector index** — SPANN + LIRE: coarse
cells → fine centroids → posting lists, RaBitQ-quantized residuals, an fp16
sidecar for exact re-rank. It is **Go-built / Go-read only** (Java has no SPFresh
index type; it forfeits cross-engine record sharing for *this index*, the stated
cost of an FDB-native layout). It is **usable from SQL today** (single-partition).
Recall is measured and **ingest-rate-dependent** (faster fills drift; refinement
and higher kc recover it); the per-fill probe ceiling is a budget tradeoff, not a defect.

It has deep **functional + FDB-integration + multi-tenant-soak** coverage, but the
**one hardening gap before unqualified production sign-off is model-based
fault-injection (chaos), which the lifecycle currently lacks entirely** (Tier-1
below). The recall + multi-tenant + build-perf backlogs are closed; the open work
is that chaos gap plus operability (ops-doc refresh, a reference worker) and two
scaling/ergonomics items. None are functional defects, but "production-ready"
should wait on the chaos coverage.

---

## Usable from SQL (verified — 1 SPFresh e2e + 6 vector-SQL surface tests pass)

```sql
CREATE VECTOR INDEX docs_emb USING SPFRESH ON docs(embedding) OPTIONS(...);

SELECT id FROM docs
QUALIFY ROW_NUMBER() OVER (
  ORDER BY euclidean_distance(embedding, [0.9, 0.1, 0.0])
) <= 10;
```

Supported (the query/plan/execute path is pinned by real-FDB e2e tests in
`pkg/relational/sqldriver/vector_*_e2e_fdb_test.go`; DDL rejections + metric mismatch
by embedded/unit tests, noted per item):
- `CREATE VECTOR INDEX … USING HNSW` **or** `USING SPFRESH` (`ddl.go:189`). **SPFresh does
  NOT support `PARTITION BY`** — it errors at DDL time (`metadata/builder.go:216`, pinned by
  the embedded `TestVectorDDL_SPFreshErrors`); partitioned vector indexes are HNSW-only.
- The Java-exact K-NN form `QUALIFY ROW_NUMBER() OVER (… ORDER BY <distance>(vec, q)) <= K` (`logical_qualify.go`). The SPFresh **FDB e2e** (`vector_spfresh_e2e_fdb_test.go`) runs it un-partitioned.
- Distance functions `EUCLIDEAN_DISTANCE`, `EUCLIDEAN_SQUARE_DISTANCE`, `COSINE_DISTANCE`, `DOT_PRODUCT_DISTANCE` (`walk.go:706`).
- Plans to a physical **BY_DISTANCE vector index scan** (EXPLAIN-pinned), never a full scan; the index-only `DistanceRank` predicate is never lowered to a residual filter (`predicate_multi_map.go`, `plan_executability.go`).
- A metric mismatch (e.g. `cosine_distance` against a EUCLIDEAN index) errors cleanly with `UnplannableIndexOnlyResidualError` rather than panicking (pinned by the plan-level `TestVectorPlan_MetricMismatchDoesNotMatchVector`, not an FDB e2e).
- **Multi-partition fan-out over a partial partition prefix (RFC-046) is HNSW-only** (SPFresh has no partitioning). Listed here only to draw the SPFresh/HNSW line.

> **Accuracy note:** earlier tracking implied SQL vector search was unfinished
> "Phase 9 / window-function parity" work. That was stale — Phase 9 (9.1–9.5) is
> complete and tested. SQL K-NN over SPFresh works (single-partition).

---

## Shipped (closed work) — all VERIFIED

| Area | RFC | State | Proof |
|---|---|---|---|
| Core index (cells/fines/postings, LIRE, RaBitQ, fp16 sidecar, split/merge/coarse-split/NPA/GC lifecycle, generation-prefixed layout) | **094** (rev 5) | Shipped | `spfresh_*.go`; full Ginkgo suite |
| Two-level build assignment (route to w_b nearest cells, not a flat fine scan) | **099** | Shipped | `spfresh_build.go:615` `assign` (route `spfreshNearestK:619` → `gatherTopK:623`) |
| Exact triangle-inequality assign prune | **101** | Shipped | `spfreshPruneLowerBound` `spfresh_build.go:654`, used `:691/:697` |
| Parallel sharded staging scan | **103** | Merged (PR #289) | `spfreshStageRecordsSharded` `spfresh_index_maintainer.go:860` |
| Online assignment refinement (ingest recall-drift recovery) + fleet driver + metrics | **104** | Merged (PR #290) | `spfresh_refine.go`; 9 `-race` Ginkgo specs (7 `It` + a 2-entry `DescribeTable`) |
| SQL surface: `CREATE VECTOR INDEX USING SPFRESH`, QUALIFY K-NN (un-partitioned) | **045** (Phase 9.1–9.4) | Shipped | `vector_spfresh_e2e_fdb_test.go` |
| SQL multi-partition fan-out — **HNSW-only** (SPFresh rejects `PARTITION BY`) | **046** (Phase 9.5) | Shipped (HNSW) | `vector_multipartition_e2e_fdb_test.go` |
| Multi-tenant scale-out: sweeper, cross-tenant routing-cache eviction (15min TTL + 4096 cap), per-tenant fairness budgets, many-tenant soak | 094 follow-up | Shipped | `spfresh_sweeper.go`, `spfresh_index_maintainer.go:117`, `bench/spfresh_multitenant_test.go` |
| Recall at scale: ε-pruning (SPANN §3.3), 1M w/ε/QPS sweeps, frozen defaults | 094.5 | Shipped | `VECTOR_BENCHMARK_RESULTS.md` |

### Current performance (SIFT-1M) — two ingest paths

SPFresh has **two ingest paths** with very different throughput *and* recall — the
doc must not be read as "SPFresh ingests at 530 vec/s":

**1. Bulk build (`BuildSPFreshIndex`, build-then-read) — the fast, high-recall path.**
Mark the index disabled, write the records, build the whole topology in one shot,
mark readable. No concurrent-maintenance lag, so it lands the **converged-topology
ideal: ~0.988 recall default** (1.0000 @ 100k) — and ingest is an **order of
magnitude above foreground fill**: ≈1,524 vec/s single-thread k-means baseline,
**7× with the perf stack** (~minutes for 1M). Use this whenever you can ingest
offline / batch.

**2. Foreground / online fill — live `SaveRecord` with the rebalancer looping beside
the writers.** Throughput **205–530 vec/s** at 1M (the perf stack lifted it 205→530),
and **recall is ingest-rate-dependent**: the faster you write, the more the
rebalancer lags and vectors get closure-assigned against a staler topology. This is
the SPFresh online trade and the reason RFC-104 refinement exists. Query side at the
full-perf-stack **530 vec/s** fill (186 cells / 5,835 fines / ρ≈1.01; `c` is *re-rank
candidates* per-query, **not** Lmax):

| Config (w_q / kc / c / ε) | recall@10 | p50 | QPS@16 |
|---|---|---|---|
| **default** 32 / 64 / 200 / 7 | 0.925 | 17.9 ms | 141 |
| **fast** 16 / 24 / 64 / 7 | 0.791 | 6.8 ms | 392 |
| kc=128 cap / 7 | 0.973 | 32.5 ms | 90 |
| kc=192 cap / 7 | 0.987 | 47.2 ms | 64 |

A **slower 110 vec/s** online fill reads ~3.5pp higher at default probes (0.961 /
0.993 / 0.998). Three ways to close the online-fill gap toward the bulk ideal: ingest
at the rate your recall target tolerates; raise kc post-fill (0.987 @ 47ms holds even
on the fast-filled topology); or run RFC-104 refinement. The kc-tail (0.973 → 0.987)
is a probe-budget tradeoff, capped by the FDB range-reply budget. (The earlier 094.5
freeze 0.952/0.826 is superseded — no longer produced by the shipped write path.)

RFC-104 refinement recovers online-fill *drift* back toward the bulk ideal; measured
at **300k**, not re-pinned at 1M:

| 300k fast fill | pre-refine | one-shot refine | bulk (ideal) |
|---|---|---|---|
| default (32/64/200) | 0.9735 | **0.9885** | 0.9880 |
| fast (16/24/64) | 0.8675 | **0.9225** | 0.9205 |

The one-shot `refine-all` fully recovers both budgets; the **budgeted production op**
fully recovers the *default* budget but lands the *fast* budget ~1.75pp short of
bulk-fast (incremental cursor co-evolving with concurrent splits — RFC-104).

---

## Production readiness (assessment — engineering judgment, not a verified claim)

> The facts below (the chaos gap, the build cap, the missing runner) are
> code-verified; the *recommendation* built on them is judgment. Calibrate to your
> own blast radius and risk tolerance.

**One-line verdict:** feature-complete and correct in isolation, but the
maintenance lifecycle has **never been run under model-based fault injection**
(commit_unknown / conflict retries, refiner-vs-rebalancer races). We've tested that
it *works*; we have not tested that it *survives faults*. A vector index corrupts
*quietly* (wrong results, not a crash) — the failure mode hardest to notice — so
this gap is the gate between conditional and unqualified production use.

What *is* hardened: the design is fence-heavy (REAL-read serialization points +
conflict ranges), deletes are membership-driven (no delete tombstones — though the
split/coarse lifecycle does use transient FORWARD/DEAD rows, GC-reaped), operations
are idempotent-under-retry, the FDB-client and
record-layer primitives underneath *are* chaos-tested, and a 20-tenant soak ran
concurrent writers + sweepers without corruption (no faults injected).

**Green-light now** — risk bounded/recoverable:
- Per-tenant indexes (blast radius = one tenant; rebuildable in isolation).
- Re-rankable / advisory results (recs, candidate generation) where you re-rank top-K exactly anyway.
- Bulk-build-then-serve, read-mostly (low lifecycle churn ⇒ minimal exposure to the untested concurrent-fault surface).
- …provided you have **ground-truth recall monitoring** (to detect drift/corruption) and a **rebuild path**.

**Hold** until the chaos arm lands (Tier-1 #1):
- High-churn, write-heavy workloads (lifecycle running constantly = maximum exposure).
- SPFresh as the sole source of truth where silent wrong answers are unacceptable (compliance, dedup).

**Before turning it on (deployment work, not index blockers):**
1. **Wire a maintenance runner** — call `SweepSPFreshIndexes` + `RefineSPFreshIndexes` per tenant on cadences yourself; no reference worker ships (~50 LOC; metrics already exist). *(Tier-2 below.)*
2. **Recall + lag monitoring** — the `CountSPFreshRefine*` and maintenance metrics are emitted; wire them.
3. **Rebuild / recovery runbook** — `SPFRESH_OPERATIONS.md` is stale on refinement *(Tier-1 #2)*.
4. Mind the **~267M-vector single-store build cap** *(Tier-2 below)* — fine for multi-tenant fleets.

**The single lever to unconditional "yes": the SPFresh chaos arm (Tier-1 #1).**

---

## k-means build early-stop (RFC-102) — shipped; the Hamerly pivot-from was rejected

- **RFC-102's actual design — a convergence-fraction early-stop — IS implemented.** The
  RFC is titled "bulk-build k-means: convergence-fraction early-stop" and *pivoted from
  Hamerly* after measurement. The shipped build k-means (`spfreshKMeansBuild`→`spfreshKMeansCore`,
  `spfresh_kmeans.go:106/116`) early-stops when an iteration moves ≤
  `spfreshKMeansBuildConvergeFraction`·n points (= 0.01, `:101`) — that *is* RFC-102.
  Only the **original Hamerly bounds approach was rejected / not implemented** (the RFC is
  still stamped "proposed" but its pivoted design shipped). k-means is the other CPU half of
  the build after two-level routing cut the flat scan; a faster variant is marginal now that
  the 1M build is minutes, not hours.

---

## Open work

### Tier 1 — harden & document what shipped

1. **SPFresh has NO chaos / model-based fault coverage** (broader than refinement
   alone). The chaos harness (`pkg/recordlayer/chaos/`) has **zero** SPFresh files;
   `verify_vector.go:26` models only HNSW (`IndexTypeVector`), never
   `IndexTypeVectorSPFresh`. So the whole lifecycle — splits, merges, coarse
   splits, NPA, GC, **and refinement** — is unverified under injected faults
   (commit_unknown, conflicts) and under concurrency. In particular RFC-104
   refinement is exercised *only in isolation* (9 Ginkgo specs, no goroutines, no
   concurrent rebalancer); the **refiner-vs-rebalancer race** and a
   **fault-injected refine pass** are untested. The per-pk conflict-retry and
   sealing-fence paths *are* unit-pinned, but a model-based run is the gold
   standard this subsystem is missing. **Highest-value open item.**

2. **`SPFRESH_OPERATIONS.md` is stale wrt RFC-104.** It documents the
   rebalancer/sweeper runbook (topologies, budgets, metrics, troubleshooting) but
   has **zero** mention of the refinement loop. Add a refinement section: cadence
   (slower than the rebalance sweep), the two new metrics
   (`CountSPFreshRefineMoves` / `CountSPFreshRefineConverged`), how `Converged`
   lets a caller back off quiescent tenants, and the refiner/rebalancer interaction
   (the lifecycle fence makes them safe to run concurrently).

### Tier 2 — scaling & ergonomics

3. **Changelog chunking for huge single-store builds.** `coarsePass` writes one
   changelog delta per coarse cell in ONE transaction; the 2-byte versionstamp
   user-version caps it at `spfreshMaxDeltasPerTx = 65536` cells (`spfresh_storage.go:510`),
   so a bulk build hard-errors above ~267M vectors **in a single store**
   (`spfresh_build.go:120-130`). Real ceiling, but it only bites one giant tenant —
   multi-tenant fleets (the deployment model) sit far under it. Lifting it needs the
   coarse-table commit to chunk the changelog across transactions (not implemented).

4. **No reference maintenance worker.** `SweepSPFreshIndexes`, `RefineSPFreshIndexes`,
   and `RebalanceSPFreshIndex` are library entry points only — no `cmd/` binary or
   ticker loop drives them on a cadence; a deployment must wire that itself. A
   reference worker (discover tenants → sweep + refine on independent cadences,
   with the metrics wired) would close the "how do I actually run this" gap.

### Nice-to-have — SQL surface follow-ups (none block "done")

- **yamsql vector port** — *real open*: no `.yamsql` scenario covers vector/QUALIFY; Java's `window-function-documentation-queries.yamsql` is unported.
- **`ef_search` FDB behavioral test** — *partial*: the knob is parsed + threaded + unit-tested, but no FDB test proves a non-default `ef_search` changes search behavior.
- **OR-of-two-KNN** — *partial*: `applyDistanceRankTransform` recurses into `OrPredicate`, but no test plans/executes an OR of two K-NN searches.
- **Window-in-`WHERE` rejection** — *real open*: a bare `ROW_NUMBER() … <= K` in `WHERE` (outside `QUALIFY`) isn't pinned to error the Java way (`42F21`).

> **Accuracy note:** "cosine-on-euclidean clean error" was listed as an open
> follow-up but is already DONE (`TestVectorPlan_MetricMismatchDoesNotMatchVector`).

---

## Measured-negative — do NOT re-chase

These levers were investigated with real measurements and rejected. Re-opening one
needs a *new* measurement that contradicts the recorded result.

- **float32 / code-domain distance kernel (RFC-100)** — REJECTED (measured):
  float32-scalar is a no-op in pure Go and SIMD isn't available; the kernel stays
  `spfreshSquaredDistance(a, b []float64)`. After RFC-099/101 the 1M build is
  minutes, so the bandwidth win is a fraction of a one-time build, against a recall
  risk across 20+ call sites incl. the exact re-rank.
- **Lmax granularity (smaller lists)** — NEGATIVE: recall is **probe-bound, not
  granularity-bound**; Lmax=128 lowers recall at every fixed probe budget. Lmax=256 stays.
- **α-led closure replication (r > 2)** — NEGATIVE: at Lmax=256 density the SPANN
  RNG rule rejects every non-home centroid as same-direction; r=4 costs ~20% fill
  throughput for ±1pp recall. r=2 stays.

---

## Reference RFCs

| RFC | Subject | Status |
|---|---|---|
| **094** | SPFresh core (FDB-native vector index) | Rev 5, shipped |
| **099** | Two-level build assignment | Shipped |
| **100** | Build distance-domain (float32/SIMD) | **REJECTED (measured)** |
| **101** | Assign-bound (triangle-inequality) pruning | Shipped |
| **102** | k-means convergence-fraction early-stop | Shipped (Hamerly pivot-from rejected) |
| **103** | Parallel staging scan | Merged (#289) |
| **104** | Online assignment refinement | Merged (#290) |
| **045** | Vector relational (SQL) parity | Shipped |
| **046** | Multi-partition vector scan | Shipped |

Genesis / alternatives: `005-vector-index.md`, `006-ivf-vector-index.md`. SPFresh
*is* the realization of the "second, FDB-native vector index" exploration (the
Go-only, distinct-on-disk-layout ANN index that escapes HNSW's sequential-RTT
latency profile); that `TODO.md` exploration item is effectively closed by RFC-094
and is stale.
