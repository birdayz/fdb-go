# RFC-103 — SPFresh bulk-build: parallel staging scan

**Status:** proposed (design review FIRST — risky concurrency change)
**Scope:** `pkg/recordlayer/spfresh_index_maintainer.go` (`BuildSPFreshIndex` —
the staging record-scan pass). Build path only; no wire format, no query path,
no foreground write-path change.
**Gates:** Torvalds (code/concurrency), Graefe (systems/determinism), codex
(external), spfresh-reviewer (build correctness; recall unaffected — same staged
set).

## Problem (measured)

The bulk build is only **2.7× parallel on a 24-thread box** (SIFT-100k:
GOMAXPROCS=24 7.78 s vs GOMAXPROCS=1 20.99 s) — ~30 % serial (Amdahl). A
per-phase wall-clock profile (100k, GOMAXPROCS=24, build 7.75 s) shows why:

| phase | time | % | parallel today |
|---|---|---|---|
| sample scan (reservoir) | 1.24 s | 16 % | **serial** |
| coarse pass (k-means) | 0.58 s | 7 % | parallel |
| **staging scan** | **2.83 s** | **37 %** | **serial** |
| finalize (waveA + waveB) | 3.10 s | 40 % | parallel (per-cell worker pool) |

The two record scans (53 %) are **sequential** `spfreshScanRecordBatches` passes.
The pure-Go FDB client is **synchronous/blocking** (`tx.Get`/range reads block on
the round-trip), so a sequential scan is **latency-bound** — it waits a full
round-trip per batch. This is the same gap the HNSW notes call out (Java
pipelines reads, ~5–10× faster). At 1M the scans dominate even more: they are
O(N) sequential, while finalize parallelizes well (k0=246 cells feed the per-cell
worker pool).

## Lever: parallelize the staging scan

The staging scan (37 %, the larger of the two) is the tractable target:
- **No cross-record state.** Each record is independently routed (two-level
  cache) and staged (fp16 STAGING + SIDECAR keyed by `(cell, recordPK)`).
- **Order-independent output.** waveB closure-assigns the FULL staged set; the
  staged set — and therefore the build's centroids/assignments — is identical
  regardless of staging order or shard count ⇒ **determinism preserved** (the
  hard part of RFC-102's contract does not arise here: the staged SET is the
  invariant, not an iteration order).
- **Disjoint shards don't conflict.** Split the record PK range into S
  sub-ranges; run S concurrent staging transactions, each over its disjoint
  sub-range. Per-record staging keys ⇒ even two shards staging into the same
  cell write disjoint keys (no write conflict). The delete-fence (Torvalds
  094.2 #2 — each staging tx REAL-reads its record range) is preserved
  **per-shard**: each shard fences its own disjoint sub-range.

### Range splitting

Reuse the `indexing_mutual.go` boundary-key pattern (`LocalityGetBoundaryKeys`)
where the cluster is multi-shard. On single-node clusters (testcontainers)
boundary keys are absent, so split the record subspace into S equal PK
sub-ranges by key interpolation (the records are PK-ordered). Either way the
shards are disjoint ranges over the same record set; concurrency hides the
per-batch round-trip latency (the actual bottleneck) even on single-node.

Concurrency S = the existing `spfreshBuildCellWorkers` (or a dedicated knob).
Each shard runs the existing `stageInTx` batch loop over its sub-range — the
batch byte-bound (codex 094.2 r1 P2) is unchanged per shard.

## Determinism & recall

- **Determinism.** The staged SET is shard-count-invariant (every record is
  staged exactly once into its routed cell, idempotently). coarsePass (the
  centroids everything routes on) is unchanged and runs BEFORE staging. waveB
  reads the full staged set deterministically. So build output is identical
  regardless of S ⇒ per-(records,seed) reproducibility holds. (A test pins
  S=1 vs S=8 byte-identical topology.)
- **Recall.** Unchanged — identical staged set ⇒ identical clustering.
- **The sample scan stays SERIAL** (16 %, out of scope): reservoir sampling
  (Algorithm R) is inherently sequential and a shard-count-invariant parallel
  reservoir is non-trivial (per-shard reservoirs + a deterministic merge, or a
  two-pass stratified sample). Deferred to a follow-on; this RFC takes the
  larger, safe 37 %.

## Risks / rollback

Concurrency change → the review must scrutinize: (1) the per-shard delete-fence
still fences correctly (disjoint ranges); (2) staging-key disjointness across
shards into a shared cell (verify the staging key includes recordPK); (3) FDB
5 s / 10 MB tx limits per shard (unchanged — same batch bound per shard); (4)
error propagation / partial-failure idempotency (a failed shard re-runs; staging
Sets are idempotent). Pure build-path code; trivial revert. Worst case (S=1)
degenerates to today's serial scan.

## Test plan

- **Determinism:** S=1 vs S=8 staging → byte-identical topology (cells, fines,
  postings, counters) and recall.
- **Build correctness:** existing build+query e2e + chunked-cascade stay green.
- **Measurement:** SIFT-100k/500k build wall-clock + the GOMAXPROCS A/B — expect
  the staging-scan fraction to drop sharply (latency hidden by S-way concurrency);
  target the build's serial floor down toward the sample scan + coarse + flip.
- **Race:** `-race` on a parallel-staging build test.

## Stacks with / not in scope

Completes the bulk-build perf stack's parallelism axis: RFC-099/101 (assign) +
RFC-102 (k-means) cut CPU; RFC-103 cuts the serial-I/O floor. Out of scope: the
sample-scan reservoir parallelization (follow-on) and SIMD (RFC-100, rejected).
The win is largest at 1M where the scans dominate the serial fraction.
