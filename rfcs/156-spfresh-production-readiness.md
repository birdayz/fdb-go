# RFC-156 — SPFresh production readiness: the ship-gate + a competitive optimization roadmap

**Status:** proposed (design review FIRST). Production-readiness, not perf.
**Scope:** two layers.
  - **Layer 1 (ship-gate):** model-based fault injection for the SPFresh lifecycle
    (`pkg/recordlayer/chaos/` — a new `verify_vector_spfresh.go` + `StoreModel`
    extension; no change to `verify_vector.go`'s HNSW arm), a shipped reference
    maintenance worker (`cmd/spfresh-maintainer` or equivalent), and an ops-doc
    refresh. No wire-format, query-path, or build change.
  - **Layer 2 (optimization roadmap):** distributed maintenance fleet, a
    read-scale-out proof on a multi-node cluster, and 10M→100M scale validation.
    Mostly harness/validation + one ergonomics change (changelog chunking for the
    build cap); flagged inline where any wire/format touch is implied.
**Gates:** Torvalds (code/concurrency), Graefe (systems/architecture — the
distributed maintenance design and the fault-injection model are systems work),
codex (external), spfresh-reviewer (recall/paper fidelity — chaos must preserve
the LIRE membership invariants, and the scale-validation recall floors are in
scope). All gates on BOTH the RFC and each implementation slice; re-request after
every commit (an ACK only covers the HEAD it reviewed).
**Supersedes:** the "Open work" section of `094-spfresh-status.md` Tier-1 #1/#2 and
Tier-2 #3/#4 — this RFC is the execution plan for them. The status doc remains the
living tracker; this RFC is the design.

---

## 1. Problem — what "production ready" actually means here

`094-spfresh-status.md` is honest about the state: SPFresh is **feature-complete
and correct in isolation** (full SPANN+LIRE lifecycle, RaBitQ residuals, fp16
sidecar re-rank, usable from SQL single-partition), with deep functional +
FDB-integration + 20-tenant-soak coverage. It is **not** unqualified
production-ready, and there is exactly **one** gate plus a set of operability and
scale items behind it.

**The one real gate: zero model-based fault coverage.** The chaos harness
(`pkg/recordlayer/chaos/`) has no SPFresh files; `verify_vector.go:26` models only
HNSW (`IndexTypeVector`), never `IndexTypeVectorSPFresh`. So the entire maintenance
lifecycle — splits, merges, coarse-splits, NPA, GC, **and** RFC-104 refinement —
is unverified under injected faults (`commit_unknown`, conflict retries) and under
concurrency (the refiner-vs-rebalancer race is exercised only in isolation: 9
Ginkgo specs, no goroutines, no concurrent rebalancer). This matters more than
usual because **a vector index corrupts *quietly*** — wrong results, not a crash —
which is the failure mode hardest to notice. We have tested that it *works*; we
have not tested that it *survives faults*.

Everything else open is operability/scale, not a functional defect:
- **No shipped maintenance runner.** `SweepSPFreshIndexes` / `RefineSPFreshIndexes`
  / `RebalanceSPFreshIndex` are library entry points; no `cmd/` binary or ticker
  drives them on a cadence (status Tier-2 #4).
- **`SPFRESH_OPERATIONS.md` is stale wrt RFC-104** — no refinement section
  (status Tier-1 #2). §6 also still claims "bulk build at ≥1M is slower than
  foreground fill (wave-B flat-scan)", which RFC-099/101 falsified (bulk build is
  now the fast path, ≈10.7k vec/s).
- **Validated only to 1M.** 10M soak pending; the ~267M-vector single-store build
  cap (`spfreshMaxDeltasPerTx = 65536`, `spfresh_storage.go:510`) is untested near
  its limit.

This RFC closes the gate and turns the operability/scale items into a roadmap —
but it also has to answer the question the work exists to answer: **why this index
at all, when LanceDB serves billions of vectors on cheap object storage?** §2 makes
that case from measured architecture, not marketing, and §4's roadmap targets
exactly the axes where the honest answer today is "we haven't proven it yet."

---

## 2. Competitive landscape — how LanceDB scales, and our case (researched 2026-06)

We must present an honest trade-off. LanceDB is the right comparison: it is the
embedded/lakehouse vector store we already benchmarked head-to-head
(`VECTOR_BENCHMARK_RESULTS.md` → "LanceDB head-to-head"), it uses the **same RaBitQ
quantization family** we do, and it has a real distributed mode.

### 2.1 How Lance scales

Two distinct things — don't conflate them.

**Lance OSS (the format) bakes storage/compute separation into the file format.**
Data + all index artifacts (IVF-PQ, HNSW, inverted, bitmap) live on object storage
(S3/GCS/Azure) or disk; compute is stateless. The write model is **MVCC via
manifest swap**: every commit creates a new immutable table version using atomic
`put-if-not-exists` / `rename-if-not-exists` object-store primitives. Optimistic
concurrency — conflicts are detected, then *rebased* (two deletes merge their
deletion vectors; non-overlapping updates coexist) or retried; Append-vs-Overwrite
and similar are incompatible. Practical throughput: **~1–4 transactions/sec per
table** on object stores. On S3 they historically needed an external manifest store
(DynamoDB) for concurrent writers; S3 native conditional writes (2024) now allow it
directly. The ANN index is a **batch build** — freshly written data lands in an
*unindexed fragment that is brute-force scanned* until a reindex/compaction makes it
ANN-visible. **Freshness is manual.**

**LanceDB Enterprise (the distributed product)** is full storage/compute
disaggregation:
- **Control plane** — config, service discovery, identity, policy, cluster
  lifecycle. Serves no data, runs no queries.
- **Data plane**, three independently-scaled node types:
  - *Query nodes* — validate/resolve/plan requests.
  - *Plan executors* — the read layer; **cache-backed reads against object
    storage** with consistent-hashing cache locality (low miss rate).
  - *Indexers* — build/merge indexes and compact, **off the request path**, driven
    by a background coordinator reacting to commit "follow-up signals."
- **Object storage** holds durable data, manifests, index artifacts.
- Distributed query execution scaling to **10B vectors** (marketed); disk-based,
  "100× cheaper than memory-based." Scale query fleets 2→200 and back to zero.

**The headline:** Lance's scaling story is **read scale-out + offline batch
indexing on cheap object storage**. Write concurrency is fundamentally throttled by
the single manifest swap per table — they get ingest throughput by *batching*
(~10k rows/commit × ~4 commits/sec ≈ the 40k vec/s we measured), **not** by
concurrent fine-grained transactions.

### 2.2 The trade-off

| Axis | LanceDB | SPFresh-on-FDB |
|---|---|---|
| Write model | Batch append + manifest swap, ~1–4 commits/sec/table; freshness manual (delta brute-forced until compaction) | Per-record ACID insert *committed with the record write*; many concurrent writers; fresh-on-commit; online LIRE rebalancing |
| Concurrent fine-grained writes | Manifest-bottlenecked — one writer wins, others retry | **Architecturally superior** — FDB resolves conflicts at key-range granularity, no table-wide lock |
| Bulk ingest | **~75× faster** (batch + offline index) | Slower — transactional per-record online ingest |
| Read scale-out | Stateless compute over object storage, 2→200 nodes, scale-to-zero, consistent-hashing cache locality | Stateless snapshot reads, scale with client count — **but FDB is the substrate, not scale-to-zero object storage** |
| Cost at rest | Object storage (~100× cheaper, cold-tierable) | FDB SSD — pricier, **no native object-storage cold tiering** |
| Indexer/query isolation | Decoupled by design (distinct node types) | Sweeper-fleet concept does this, but library-only, not shipped |
| Validated scale | 10B (marketed) | **1M validated**; 10M soak pending; ~267M single-store build cap |
| Transactional integrity | No cross-record txn; dual-write risk vs source records | **ACID with the record write — zero dual-write divergence** |

### 2.3 Our case

Lance wins decisively on **bulk ingest** and **cost-at-rest** (cheap object storage
+ scale-to-zero compute). We win decisively on **transactional freshness**,
**concurrent multi-writer fine-grained writes**, and **no dual-write divergence** —
because the index *is part of the same ACID transaction as the record write*.
Different workloads, and we should say so plainly:

> When the workload is "build a static index once, serve read-mostly," use an
> embedded library. When records mutate transactionally across many writers and
> tenants on shared infrastructure — where the index must be ACID-consistent with
> the source records and search-fresh within a commit, with no reindex step and no
> dual-write divergence — the embedded library is not in the running.

The roadmap in §4 is built to *defend and close* this case: prove the read
scale-out and concurrent-write advantages we claim (today only at single-node
testcontainer scale), and close the two axes where Lance is genuinely ahead and we
have no answer yet — operational maturity (a real maintenance fleet) and scale
(1M → 100M). Cost-at-rest tiering is the one axis we explicitly **do not** chase
here (§5).

---

## 3. Layer 1 — the ship-gate (what unblocks "production ready")

### 3.1 Model-based fault injection for the SPFresh lifecycle (the gate)

This is the highest-value open item and the single lever to an unconditional "yes."

**Reuse the existing chaos infrastructure** (`pkg/recordlayer/chaos/`), exactly as
the HNSW/record-store arms do:
- `StoreModel` — an in-memory shadow. Extend it (or add a parallel
  `SPFreshModel`) to shadow the **logical** index state that must hold regardless of
  physical topology: per-pk membership (the set of fine centroids a pk is assigned
  to), the posting lists, and centroid lifecycle state (ACTIVE / SEALED / FORWARD /
  DEAD). The model does **not** mirror the topology k-means decisions (those are
  legitimately nondeterministic under concurrency); it mirrors the **invariants**.
- `ChaosTransactor` — inject faults at tx boundaries via the production-side
  `NewFDBDatabaseWithTransactor` hook. Targeted (`InjectOnce(FaultCommitUnknown)`)
  for regression pins; random (`WithSeed(N), WithFaults(FaultsRetryHeavy)`) for
  discovery. Seeded for reproducibility.
- Wire `verify_vector.go`'s switch to recognize `IndexTypeVectorSPFresh` and
  dispatch to the new verifier — the one line (`verify_vector.go:26`) that today
  silently models HNSW only.

**Invariants `Verify()` must check after each operation (and after each injected
fault + retry):**
1. **Membership ⊆ postings** — every fine a pk claims membership in has a posting
   entry for that pk. (Already the spine of `SPFreshDebugIntegrity`; now checked
   continuously under faults, not just sampled at the end.)
2. **All membership targets ACTIVE** — no membership pointing at a SEALED/FORWARD/
   DEAD centroid that GC should have reaped or that a FORWARD should have redirected.
3. **No orphaned posting entries** — no posting entry whose target centroid is
   DEAD/missing (post-GC).
4. **Idempotence under retry** — a `commit_unknown` replay does not double-apply:
   no duplicate posting entries, no double-counted split, no lost-and-relived task.
   This is where the fence-heavy design (REAL-read serialization + conflict ranges)
   gets its first adversarial test.
5. **Recall floor (the quiet-corruption detector)** — after a fault-injected
   operation sequence, a sampled brute-force recall@10 check stays above a
   threshold. This is the one invariant that catches *silent* drift the structural
   checks miss; it is the reason this gate exists.
6. **Generation monotonicity** — no cross-generation leakage; a mid-write
   generation flip aborts and retries into the new generation (status §"Generation
   bumped unexpectedly").

**The race the isolation tests never hit:** run **refinement concurrently with the
rebalancer**, both under injected faults. RFC-104's 9 specs run the refiner alone;
the lifecycle fence claims they are safe to run concurrently — this proves or breaks
that claim. Per the PRIME DIRECTIVE, any failure here is a real bug to root-cause
and fix, with a deterministic regression pinned (seeded `InjectOnce`), never a skip.

**Done when:** a seeded chaos suite drives concurrent writers + rebalancer +
refiner + sweeper through the full fault menu (commit_unknown, conflict, retry
storms) with all six invariants green across a soak, and every bug it surfaces is
fixed + pinned. spfresh-reviewer signs off that the invariants are faithful to LIRE
(membership/replication semantics) and the recall floor is set correctly.

### 3.2 Reference maintenance worker (basic — the "how do I run this" gap)

Ship a minimal `cmd/spfresh-maintainer` (status Tier-2 #4, ~50 LOC core): discover
tenants → loop `SweepSPFreshIndexes` + `RefineSPFreshIndexes` on independent
cadences (refine slower than sweep) → wire the existing metrics
(`CountSPFreshRefineMoves`/`Converged`, the `spfresh_*` events) → honor
`SPFreshSweepOptions` budgets. The fleet/distributed version is §4.1; this slice is
the single-process reference loop so a deployment has a working starting point and
the ops doc has something concrete to document.

### 3.3 Ops-doc refresh

`SPFRESH_OPERATIONS.md`: add the RFC-104 refinement section (cadence, the two
metrics, how `Converged` lets a caller back off quiescent tenants, the
refiner/rebalancer fence), and fix the stale §6 known-limit (bulk build is the fast
path post-099/101, not slower than foreground fill). Cross-link the chaos
guarantees from §3.1 once they land.

---

## 4. Layer 2 — optimization roadmap (the three prioritized axes)

Prioritized per the scoping decision: **distributed maintenance worker**, **read
scale-out proof**, **scale validation 10M→100M**. Each is framed as a falsifiable
claim with a measurement that proves or kills it — not a feature to build on faith.

### 4.1 Distributed maintenance fleet (Lance's indexer/query split, our version)

Lance isolates indexer compute from query compute by design. We have the primitives
(`SweepSPFreshIndexes` is concurrent-safe by construction — unique lease owners,
task-level exclusion) but no fleet story. Formalize §3.2's reference worker into a
**shardable fleet**: N workers each own a shard of the tenant list (consistent hash
or static range), independent sweep/refine cadences, per-tenant failure isolation
(the joined-error pass continues), and the metrics wired for backlog/lag alerting.

**Claim to prove:** maintenance throughput scales ~linearly with worker count and
never competes with the serving path (a separate fleet, not in-process on writers).
**Measurement:** drive a multi-tenant write load, scale the maintainer fleet 1→K,
show task-queue drain rate scales with K and `spfresh_lease_skips` stays ~0 with
sharded ownership (climbs only on deliberate overlap). This is also the deployment
shape the 20-tenant soak already exercises informally — make it a first-class,
measured configuration.

### 4.2 Read scale-out proof (multi-node cluster, multi-client QPS)

Every query number we have is from a **single-node testcontainer** with FDB server
CPU inside the same box. The claim — "reads scale out with stateless clients against
distributed storage" — is *architecturally* true (snapshot reads don't conflict) but
**unproven at scale**. Lance proves theirs (2→200 query nodes). We must prove ours.

**Claim to prove:** aggregate query QPS scales ~linearly with client process count
until FDB storage-server CPU saturates, and per-query p50 stays flat as read clients
are added (snapshot reads create no write conflicts). **Measurement:** a multi-node
FDB cluster (≥3 storage processes on separate CPU), a built 1M+ SPFresh topology,
and N client processes each running the serving-shape query loop (`SIFT_QPS`
harness). Plot QPS vs N and p50 vs N. **Kill criterion:** if p50 climbs or QPS
plateaus well below storage saturation, we have a hidden serialization point (a hot
routing-cache, a metadata-version read, a GRV bottleneck) — root-cause it; that is a
real finding, not a tuning footnote.

### 4.3 Scale validation 10M → 100M

**Status (done):** the SIFT churn soak now asserts BOTH recall-stability and the
chaos-gate **structural** invariants at scale (via `SPFreshCheckIntegrity`), and
both hold on real SIFT data (`VECTOR_BENCHMARK_RESULTS.md` → "structural-integrity
scale validation"):
- **SIFT-100k, 6 churn waves:** recall@10 flat 0.994→0.992; members==live (one row
  per record); maxPostingLen 234 ≤ Lmax; oversizedHard=0, badTargets=0,
  membership⊄postings=0 — every chaos-gate invariant holds at 100× the chaos-test
  scale under churn.
- **SIFT-1M, 4 churn waves (the ceiling):** recall@10 dead flat at 0.962 across all
  waves (zero decay) — the SPFresh §5.2 recall-stability property at the ceiling.

So **1M is now a *validated* ceiling** (recall-stable under churn), and the
structural invariants are proven to 100k. **Remaining:** (a) the structural
assertion runs in ONE tx so it auto-skips >200k — asserting structure at 1M+ needs
the batched-scan integrity variant (the recall-monitor §3.1/§4 follow-up applies
here too); (b) >1M (10M/100M) needs a larger dataset than SIFT-1M and the build-cap
work below.

**Plan for >1M:** extend the harness past SIFT-1M (a larger/synthetic-clustered
base set), and add the batched integrity scan so structure is asserted at the
ceiling, not just recall. **Recall floors are spfresh-reviewer-owned** — fixed
probes cover a shrinking list fraction as N grows (kc=64 covers 64/11,336 fines at
1M; far less at 100M), so the kc/w defaults likely need a per-scale freeze, exactly
as 094.5 re-tuned for the 1M+ regime. That re-tune is in scope.

**Dependency — the build cap.** A single-store bulk build hard-errors above ~267M
vectors: `coarsePass` writes one changelog delta per coarse cell in ONE transaction,
capped at `spfreshMaxDeltasPerTx = 65536` cells by the 2-byte versionstamp
user-version (`spfresh_storage.go:510`, gated `spfresh_build.go:120-130`). At 100M in
a single store this is close enough to matter; lifting it needs the coarse-table
commit to **chunk the changelog across transactions** (status Tier-2 #3, not yet
implemented). Multi-tenant fleets sit far under the cap, so this only blocks the
single-giant-store 100M case — but the 100M validation must either run multi-tenant
(under the cap) or land the chunking first. **This is the one Layer-2 item with a
potential wire/format-adjacent touch** (the changelog commit boundary); it gets the
full gate treatment if we build it.

---

## 5. Non-goals

- **Cost / object-storage cold tiering.** The clearest architectural gap vs Lance
  (cold posting lists / sidecar to cheaper storage) — and explicitly **out of scope**
  here. FDB has no native object-storage tiering; bolting one on is a large,
  separate effort with its own wire/durability questions. Noted as future work, not
  chased in this RFC.
- **Cross-engine Java parity for this index.** SPFresh is Go-built/Go-read by
  design (Java has no SPFresh index type) — the stated, accepted cost of an
  FDB-native layout. Unchanged.
- **10B-vector parity.** We target a credible production ceiling (100M validated),
  not Lance's marketed 10B.
- **Cross-cluster / multi-region.** Single FDB cluster; out of scope (status §6).
- **Bulk-ingest throughput parity with Lance.** Architecturally impossible to match
  a batch-append-no-transaction store with per-record ACID inserts, and not the
  workload we serve. We compete on freshness, not bulk load.

---

## 6. Open questions (for review)

1. **Recall-floor threshold (§3.1 invariant 5).** What absolute recall@10 floor
   should the chaos suite enforce after a fault sequence, at what N, and over what
   query sweep? spfresh-reviewer to set — too loose and it misses quiet corruption;
   too tight and it flags legitimate topology variance. Proposal: floor at the
   measured fast-budget recall minus a variance margin, at the soak's N.
2. **Model granularity (§3.1).** Shadow only the logical invariants (membership/
   posting/lifecycle), or also the topology? Proposal: invariants only — topology
   k-means is legitimately nondeterministic under concurrency; modeling it would
   force false positives. Graefe call.
3. **Build-cap chunking vs multi-tenant 100M (§4.3).** Land the changelog chunking
   (real wire-adjacent work) to validate a single 100M store, or validate 100M only
   in the multi-tenant shape (under the cap) and defer chunking? Proposal: validate
   multi-tenant first (cheaper, the real deployment model), file chunking as a
   follow-up if a single-giant-store customer appears.
4. **Read-scale-out test environment (§4.2).** Real multi-node FDB cluster in CI is
   expensive; a multi-process single-box cluster understates network RTT (the very
   thing that makes the stateless-read story pay off). Proposal: multi-process box
   for the regression gate, one documented real-cluster run for the headline number.
5. **Is the reference worker a `cmd/` binary or a library + example?** A binary is
   more "batteries included" but invites it becoming the de-facto required runtime.
   Proposal: library-first (`SPFreshMaintainer` loop) + a thin `cmd/` wrapper, so a
   deployment can embed it or run it standalone.

---

## 7. Review & sequencing plan

Ship-gate first (it is the actual blocker), then the roadmap axes in parallel:

1. **§3.1 chaos arm** — the gate. Design ACK (all four) → implement the model +
   verifier → run seeded soak → fix+pin every bug it finds → impl ACK.
2. **§3.2/§3.3 reference worker + ops doc** — small, unblocks deployment; can land
   alongside the chaos work.
3. **§4.1 fleet**, **§4.2 read scale-out**, **§4.3 scale validation** — independent;
   each is a measurement that either confirms a claim in §2.3 or surfaces a real
   bug to fix. 4.3's build-cap dependency (§4.3 / OQ 3) is the only one with a
   potential wire touch and gets the full gate.

Each slice re-requests all four gates after every commit. No slice merges with a
NAK from any gate, and the chaos arm (§3.1) does not merge with any invariant red.
