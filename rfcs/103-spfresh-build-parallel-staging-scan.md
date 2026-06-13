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

1. **Boundaries are primary-key tuples, never raw bytes — and the indexed PK
   keyspace must be prefix-safe.** A cut at a PK `b` puts record `b` and all its
   split chunks *wholly* in the upper shard: `recordsSubspace.Pack(b)` is the
   common prefix of every key of record `b`, so `[.., Pack(b))` excludes the
   whole record and `[Pack(b), ..)` includes all of it. Feeding a raw FDB key — a
   `LocalityGetBoundaryKeys` result or an interpolated byte string — straight
   into `ScanRecordsInRange` is **forbidden**: a raw boundary can land *inside* a
   split record (`PK + suffix`), tearing one record across two shards. Any
   candidate that does not cleanly unpack to a PK is **dropped** (a dropped
   boundary only coarsens the split — fewer shards, still a correct gapless
   tiling).

   The subtler hazard (codex r2/r3 P1): a cut at `Pack(b)` tears a **split**
   record R only if it falls strictly between two of R's chunk keys. R's keys are
   `Pack(R.pk, s)` where the suffix `s` is an **integer** (`recordVersionSuffix=-1`,
   `unsplitRecord=0`, `startSplitRecord=1,2,…`; constants.go). `Pack(b)` lands
   between two of them iff `b = R.pk ++ [s]` for an integer `s` in that range —
   i.e. **`R.pk` is `b` with its last element removed, and that last element is an
   integer**. (This is deliberately *not* argued as "Pack(R.pk) is a byte-prefix
   of Pack(b)": string/bytes element encodings are **not** byte-prefix-free —
   `Pack(("a",))` is a byte-prefix of `Pack(("a\x00",))` — but such a false
   prefix appends `0xff…`, never an integer-suffix encoding, so it sorts *past*
   all of R's chunks and tears nothing. Only an integer-element extension can
   land between chunks.) Two facts rule the tear out:
   - Boundaries are full PKs of the **indexed record type**, all of one **fixed
     arity** A. `b` with its last element removed has arity A−1, so it is never a
     PK of the indexed type ⇒ no indexed boundary tears an indexed record.
   - For *co-resident* types, parallel staging (S>1) is **gated** on
     `RecordMetaData.PrimaryKeyHasRecordTypePrefix()` (metadata.go:1125 — the
     **ALL-types** predicate; the per-expression `primaryKeyHasRecordTypePrefix`
     is insufficient, since one indexed type could be type-key-prefixed while a
     co-resident type has a bare PK) **or** a single record type. Then every
     type's keyspace begins with its own distinct `RecordTypeKey`, so
     `b`-minus-last-element (prefixed by the indexed type key, arity A−1) cannot
     equal any other type's PK, and `Pack(b)` cannot fall inside a foreign
     record's chunks. The excluded config — the `collision_test.go` prefix-overlap
     pathology (mixed types, some without a type-key prefix) — falls back to
     **S=1** (today's serial scan, always correct).

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
     `LocalityGetBoundaryKeys` returns empty, so derive the interior PK
     boundaries **by record count** by piggy-backing on the already-serial
     **sample scan** (§"sample scan stays serial"), which visits every record in
     PK order *before* staging. The sample scan is one pass and learns `totalN`
     only as it goes, so the boundaries are **approximate** quantiles from a
     bounded-memory decimation: keep an evenly-spaced reservoir of ≤ M candidate
     PKs (systematic decimation — when the buffer fills, drop every other entry
     and double the stride; O(M) memory, no second pass), then pick the S-1
     evenly-spaced candidates as boundaries. This costs no extra I/O and yields
     valid, record-aligned boundaries for *any* PK type — **superseding** the
     original "byte interpolation" sketch, which could land mid-record and skew
     badly on tuple-encoded sequential keys (Graefe/codex). Approximate quantiles
     (or fewer than S-1 distinct candidates ⇒ fewer shards) only affect balance,
     never correctness — determinism and the staged set are split-quality- and
     shard-count-invariant.
   Worst case (no boundaries derived, or the prefix-safety gate fails) ⇒ S=1 ⇒
   today's serial scan (trivial revert; the safe floor).

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

- **Determinism (what RFC-103 controls: the staged set).** The staged SET is
  **shard-count-invariant** — every record staged exactly once into its routed
  cell (`nearestCell` is a pure function of the frozen, pre-staging `coarseVec`),
  idempotently, to a per-record key. So the STAGING keyspace
  (`(cell, recordPK) → fp16` + sidecar) is **byte-identical for S=1 vs S=8**.
  This is the precise invariant the parallelization must preserve, and the
  **headline test pins it directly**: run coarsePass + the (parallel) staging
  scan, dump the staging keyspace, assert byte-identical across S — plus an
  end-to-end recall-equivalence check on the finished index.
- **Fine IDs are NOT a determinism axis (pre-existing, independent of S).** Build
  output past staging is *not* byte-reproducible even on master: fine-ID
  allocation (`claimFineIDs`, `spfresh_build.go:501`) doles consecutive IDs from
  a shared-mutex pool in wave-A **worker-completion order** (`forEachCellParallel`
  dispatches via an atomic counter; retries re-claim fresh IDs), so which cell
  gets which fine-ID block varies run-to-run regardless of shard count. RFC-103
  neither improves nor worsens this. The determinism test therefore asserts the
  **staged set** (above) and **recall**, NOT byte-identical fines/postings/counters
  (codex r2 P2). Reproducible fine-ID numbering is a separate, out-of-scope
  follow-up (deterministic allocation in cellID order).
- The staged-set invariant holds for a **quiescent record set** (what the bulk
  build targets and the test exercises). Under concurrent foreground writers the
  guarantee is the same one the serial scan already gives — same staged set ⇒
  same recall; S does not weaken it.
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
disjoint conflict ranges = the full record range; (1b) no record is torn at a
boundary — a tear needs `b`-minus-last-element to be another record's PK with an
integer suffix; boundaries are fixed-arity indexed PKs (so `b`-minus-last is the
wrong arity) and S>1 is gated to single-type or ALL-types-RecordTypeKey-prefixed
stores via `RecordMetaData.PrimaryKeyHasRecordTypePrefix()` (else S=1), so no
`Pack(b)` lands inside another record's split chunks; (2) staging-key disjointness
across shards into a shared cell — `stagingKey(cell, pk)` / `sidecarKey(pk)`
include the (trimmed) record PK, so two shards into one cell write disjoint keys;
(3) FDB 5 s / 10 MB tx limits per shard — unchanged (same per-shard byte bound);
(4) ranged-continuation correctness — the high bound is held across every batch
so a resumed batch cannot escape its shard; (5) error propagation /
partial-failure idempotency — first-error ctx-cancel + wait-all; whole-pass
retry over idempotent Sets. Pure build-path code; trivial revert. Worst case
(S=1) degenerates to today's serial scan.

## Test plan

- **Determinism (headline):** S=1 vs S=8 → **byte-identical staging keyspace**
  (`(cell, recordPK) → fp16` + sidecar, dumped after coarsePass + staging) on a
  quiescent record set, plus **recall equivalence** on the finished index. NOT
  fine IDs / postings (pre-existing wave-A allocation nondeterminism — see
  Determinism).
- **Prefix-safety gate:** a single-record-type / all-types-`RecordTypeKey`-prefixed
  store shards (S>1); a `collision_test.go`-style prefix-overlap store (a bare-PK
  type co-resident with a type-key-prefixed one) falls back to S=1. Pin both.
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
