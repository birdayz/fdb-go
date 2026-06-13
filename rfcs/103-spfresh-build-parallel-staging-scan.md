# RFC-103 — SPFresh bulk-build: parallel staging scan

**Status:** proposed (design review FIRST — risky concurrency change)
**Scope:** `pkg/recordlayer/spfresh_index_maintainer.go` (`BuildSPFreshIndex` —
the staging record-scan pass) + a range-bounded variant of
`spfreshScanRecordBatches`. Build path only; no wire format, no query path,
no foreground write-path change.
**Gates:** Torvalds (code/concurrency), Graefe (systems/determinism), codex
(external). spfresh-reviewer confirms recall is unaffected (identical staged
set). All four on BOTH the RFC and the impl; re-request after every commit.

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
- **No cross-record state.** Each record is independently routed
  (`nearestCell`, a pure function of the post-coarse, frozen `coarseVec`) and
  staged (fp16 STAGING + SIDECAR keyed by `(cell, recordPK)` / `(recordPK)`).
- **Order-independent output.** waveB closure-assigns the FULL staged set, read
  back in FDB key order (sorted by `(cell, PK)`), never write order; the staged
  set — and therefore the build's centroids/assignments — is identical
  regardless of staging order or shard count ⇒ **determinism preserved** (the
  hard part of RFC-102's contract does not arise here: the staged SET is the
  invariant, not an iteration order).
- **Disjoint shards don't conflict.** Split the record PK range into S
  half-open sub-ranges; run S concurrent staging transactions, each over its
  disjoint sub-range. Per-record staging keys ⇒ even two shards staging into the
  same cell write disjoint keys (no write conflict). The delete-fence (Torvalds
  094.2 #2 — each staging tx REAL-reads its record range) is preserved
  **per-shard**: each shard fences its own disjoint sub-range, and the union of
  the disjoint conflict ranges equals the single full-scan fence.

### Range splitting (the tiling contract)

The scan switches from the unbounded `store.ScanRecords` to the range-bounded
`store.ScanRecordsInRange(low, high, lowEP, highEP, continuation, props)`
(`store.go:1245`). Shards tile the record keyspace as **half-open primary-key
ranges**:

    shard 0:    [TreeStart, b₁)
    shard i:    [bᵢ,        bᵢ₊₁)
    shard S-1:  [b_{S-1},   TreeEnd)

with `lowEP = RangeInclusive`, `highEP = RangeExclusive` on interior bounds and
`TreeStart` / `TreeEnd` at the two ends. Three load-bearing invariants on the
boundaries:

1. **Boundaries are primary-key tuples, never raw bytes.** A cut at a PK `b`
   puts record `b` and all its split chunks *wholly* in the upper shard:
   `recordsSubspace.Pack(b)` is the common prefix of every key of record `b`, so
   `[.., Pack(b))` excludes the whole record and `[Pack(b), ..)` includes all of
   it. Feeding a raw FDB key — a `LocalityGetBoundaryKeys` result or an
   interpolated byte string — straight into `ScanRecordsInRange` is **forbidden**:
   a raw boundary can land *inside* a split record (`PK + suffix`), tearing one
   record across two shards (codex r2 P1). The boundary source therefore yields
   PKs; any candidate that does not cleanly unpack to a PK is **dropped** (a
   dropped boundary only coarsens the split — fewer shards, still a correct
   gapless tiling).

2. **The tiling is gapless with ±∞ ends.** Shard 0 starts at `TreeStart`, shard
   S-1 ends at `TreeEnd`, so the union of the disjoint sub-ranges is the *entire*
   record range. No gap ⇒ no delete escapes every shard's conflict range; no
   overlap ⇒ no two shards fence the same key. A record inserted *past* the
   highest boundary during the build still falls inside shard S-1's
   `[.., TreeEnd)` fence — the full-scan fence is not shrunk (Torvalds r1 #2).

3. **Boundaries are record-aligned and roughly count-balanced.** Source:
   - **Multi-shard cluster:** FDB shard splits via `LocalityGetBoundaryKeys`
     over the record subspace (the `indexing_mutual.go:99` pattern), each
     unpacked to a PK; non-PK boundaries dropped per (1).
   - **Single-node (testcontainers — the path the tests exercise):**
     `LocalityGetBoundaryKeys` returns empty, so derive the S-1 interior PK
     boundaries **by record count** by piggy-backing on the already-serial
     **sample scan** (§"sample scan stays serial"), which visits every record in
     PK order *before* staging. Capturing S-1 count-quantile PKs there costs no
     extra I/O and yields valid, record-aligned, balanced boundaries for *any*
     PK type. This **supersedes** the original "byte interpolation" sketch,
     which could land mid-record and skew badly on tuple-encoded sequential keys
     (Graefe/codex). Imbalance is a perf footgun, never a correctness one
     (determinism is split-quality-invariant), but count-quantiles make the
     shards even by construction.
   Worst case (no boundaries derived) ⇒ S=1 ⇒ today's serial scan (trivial
   revert; the safe floor).

### Per-shard scan: ranged continuation

Each shard is still batched into continuation-bounded transactions (the 5 s /
10 MB tx limits are per-shard-unchanged — same `config.stagingScanBatch()` byte
bound). Within a shard the **high bound is held constant across every batch**;
only the low end advances: the first batch uses `lowEP = RangeInclusive` at the
shard's `low` (or `TreeStart` for shard 0); resumed batches use
`lowEP = EndpointTypeContinuation` while keeping the SAME `high` / `highEP`. This
is the one place `ScanRecordsInRange` differs from `ScanRecords` (which forces
`highEP = TreeEnd` on a continuation, `store.go:1167`) — get it wrong and a
resumed batch escapes its shard's range or fails to tile (codex r2 P1 #2). The
scan stays **SERIALIZABLE** per shard: each shard's read-conflict range over its
disjoint sub-range is the per-shard delete fence (an isolation property — the
union of the disjoint conflict ranges equals the single full-scan fence; Graefe).

### Concurrency & error handling

S = `spfreshBuildCellWorkers` (or a dedicated knob); mirror the existing
`forEachCellParallel` shape (`spfresh_build.go:297`). The shard goroutines share
only immutable builder state: `b.storage` / `b.token` / `b.config`, and the
post-coarse, frozen `b.coarseVec` / `b.cellIDs` that `nearestCell` reads. The
wave-A-only `b.idMu` allocator is **untouched** on the staging path, so there is
no shared mutable state and `-race` must be clean. First error cancels the
shared `ctx` so the other shards tear down their in-flight `spfreshRun` instead
of finishing a 5 s tx; the fan-out **waits for all goroutines** before returning.
A failed shard re-runs the WHOLE staging pass on the build's retry — staging
Sets are idempotent (`(cell, recordPK)` / `(recordPK)` keys, same fp16 value), so
a shard that committed before the failure is harmlessly re-Set (codex r2 P2). On
a clean run the maintainer's range-set + flip bookkeeping is unchanged.

## Determinism & recall

- **Determinism.** The staged SET is shard-count-invariant (every record staged
  exactly once into its routed cell, idempotently). coarsePass (the centroids
  everything routes on) is unchanged and runs BEFORE staging. waveA loads each
  cell's staging via a key-ordered range read (`spfreshLoadStagingCell`, sorted
  by PK — not write order) and seeds k-means++ off `seed+cellID`; waveB reads the
  full staged set deterministically. So for a **quiescent record set** (what the
  bulk build targets and the determinism test exercises) the build output is
  **byte-identical** regardless of S — including fine IDs, since fine-ID
  allocation happens in wave A over the PK-sorted staged set, after staging.
  (A test pins S=1 vs S=8 byte-identical topology.) Under concurrent foreground
  writers the guarantee is the weaker but sufficient one the serial scan already
  gives — same staged set ⇒ same recall; S does not weaken it (codex r2 P3).
- **Recall.** Unchanged — identical staged set ⇒ identical clustering.
- **The sample scan stays SERIAL** (16 %, out of scope): reservoir sampling
  (Algorithm R) is inherently sequential (each swap depends on the running count)
  and a shard-count-invariant parallel reservoir is non-trivial (per-shard
  reservoirs + a deterministic merge, or a two-pass stratified sample). Deferred
  to a follow-on; this RFC takes the larger, safe 37 %. The sample scan does pull
  double duty here: it is also where single-node count-quantile shard boundaries
  are captured (read-only, no extra I/O, still serial).

## Risks / rollback

The review must scrutinize: (1) the per-shard delete-fence still fences — the
boundaries are PK-aligned, half-open, gapless, ±∞ at the ends, so the union of
disjoint conflict ranges = the full record range; (2) staging-key disjointness
across shards into a shared cell — `stagingKey(cell, pk)` / `sidecarKey(pk)`
include the (trimmed) record PK, so two shards into one cell write disjoint keys;
(3) FDB 5 s / 10 MB tx limits per shard — unchanged (same per-shard byte bound);
(4) ranged-continuation correctness — the high bound is held across every batch
so a resumed batch cannot escape its shard; (5) error propagation /
partial-failure idempotency — first-error ctx-cancel + wait-all; whole-pass
retry over idempotent Sets. Pure build-path code; trivial revert. Worst case
(S=1) degenerates to today's serial scan.

## Test plan

- **Determinism (headline):** S=1 vs S=8 staging → byte-identical topology
  (cells, fines, postings, counters) and recall, on a quiescent record set.
- **Tiling correctness:** assert the shard boundaries tile the record range
  gaplessly and disjointly (every record read by exactly one shard; union =
  whole range); a split-record dataset (vectors large enough to split) proves no
  record is torn across a shard boundary.
- **Ranged continuation:** a shard with a small per-batch limit spanning several
  batches reads exactly its `[low, high)` and no more (the high bound holds
  across resumed batches).
- **Build correctness:** existing build+query e2e + chunked-cascade stay green.
- **Race:** `-race` on a parallel-staging build test (MANDATORY — concurrency
  change). First-error path: inject a shard failure, assert the others cancel and
  the build retry succeeds idempotently.
- **Measurement:** SIFT-100k/500k build wall-clock + the GOMAXPROCS A/B — expect
  the staging-scan fraction to drop sharply (latency hidden by S-way concurrency);
  target the build's serial floor down toward the sample scan + coarse + flip.

## Stacks with / not in scope

Completes the bulk-build perf stack's parallelism axis: RFC-099/101 (assign) +
RFC-102 (k-means) cut CPU; RFC-103 cuts the serial-I/O floor. Out of scope: the
sample-scan reservoir parallelization (follow-on) and SIMD (RFC-100, rejected).
The win is largest at 1M where the scans dominate the serial fraction.
