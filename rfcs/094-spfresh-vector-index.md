# RFC-094 — SPFresh: an FDB-native vector index (centroid + posting lists, incremental rebalancing)

**Status:** Draft — awaiting review (Torvalds + codex; record-layer scope, no Cascades surface)

**Scope:** A second, Go-only vector index type built *for* FoundationDB's performance
model, targeting 1M–10M vectors with linear writer scalability. Architecture: SPANN's
centroid + posting-list layout for reads, SPFresh's LIRE protocol for in-place
incremental updates, RaBitQ (already in-tree, `pkg/rabitq`) for compressed distance.
The existing HNSW index (`IndexTypeVector`) is unchanged and remains the
Java-wire-compatible option.

**TODO.md anchor:** "Exploration: a second, FDB-native vector index
(SPFresh/DiskANN/SPANN/beam)" — this RFC is the design gate that entry calls for.
Batched beam search (the wire-neutral HNSW query improvement listed there) is
complementary and explicitly out of scope here.

---

## 1. Problem

The HNSW index is now 100% Java-compliant (PR #278) — and that is exactly why it
cannot reach the 1M–10M / many-concurrent-writers regime on FDB. Measured on this
codebase (SIFT-128D unless noted; testcontainer FDB, ~0.3–0.5ms per round trip):

| Metric | HNSW on FDB (measured) | Why |
|---|---|---|
| Build / insert | ~35–70 vec/sec, *degrading* with graph size (349 → 48 vec/sec from 100 → 1k vectors) | every insert greedy-descends the graph: O(layers × hops) **dependent** point reads; reverse-edge writes mutate shared hub nodes |
| Search p50 | 25–73 ms | the traversal is sequential: each hop needs the previous hop's neighbor list — round-trip *depth*, not bandwidth |
| Concurrent writers | serialized | per-prefix write lock (Java parity); without it, FDB-1020 conflict storms on shared adjacency keys |
| Gap vs disk-backed ANN systems | ~16× QPS vs Qdrant after all wire-neutral wins | architectural, not implementational |

The wire-neutral optimizations already landed (raw spans, distance-from-bytes,
4-lane distance, per-tx cache) bought 2.4×. The remaining gap is structural:
**HNSW's unit of work is a pointer chase; FDB's unit of efficiency is a range read.**

### What a high-latency networked KV store is actually good at

The design constraints, from first principles:

| FDB property | Design consequence |
|---|---|
| ~0.3–2 ms per round trip, regardless of payload | minimize round-trip **depth**; dependent reads are the enemy |
| range reads stream MB/s; futures pipeline | fetch *wide*, not *deep*: many parallel range reads in one burst |
| optimistic concurrency; conflicts abort whole txs | foreground writes must touch disjoint keys; shared-key read-modify-write is poison |
| atomic ops (ADD) and versionstamps are conflict-free | counters and append-keys come for free |
| 5 s tx limit, 10 MB tx size, 100 KB value | any unit of maintenance work must be small and resumable |
| no server-side compute | all distance math runs client-side, so the routing state it needs must be small enough to cache client-side |

SPANN/SPFresh is precisely the architecture these constraints select for: a small
routing structure (centroids) that lives client-side, big dumb contiguous blocks
(posting lists) that fetch in one round trip, writes that append to one partition
without touching any shared structure, and all rebalancing pushed to background
transactions that bear the retry cost instead of foreground writes.

## 2. Architecture overview

```
                       CLIENT (stateless, per-process cache)
  ┌─────────────────────────────────────────────────────────────┐
  │  centroid table: []{centroidID, RaBitQ code}  (~10–60 MB)   │
  │  refreshed incrementally via versionstamped changelog        │
  │  SIMD brute-force scan selects k_c nearest centroids         │
  └───────────────┬─────────────────────────────────────────────┘
                  │ k_c parallel GetRange (ONE round-trip burst)
                  ▼
  FDB   POSTINGS/(centroidID, pk) → RaBitQ code [+ fp16]   ← insert appends here
        CENTROIDS/(centroidID)    → full vector + state
        CHANGELOG/(versionstamp)  → centroid delta (add/del/forward)
        MEMBERSHIP/(pk)           → [centroidID...]         ← delete reads here
        COUNTERS/(centroidID)     → posting size (atomic ADD)
        TASKS/(versionstamp)      → split/merge work items
        META/                     → config, build state, GC horizon
```

- **Search** = scan cached centroids (CPU, zero I/O) → fetch k_c postings in
  parallel (1 RT) → RaBitQ distances (CPU) → re-rank top-C by reading source
  records (1 RT, parallel point reads). **Constant round-trip depth ≈ 3.**
- **Insert** = assign to r nearest centroids (CPU, cached) → write r posting keys +
  1 membership key + r atomic counter ADDs → commit. **No shared structure
  touched. Zero foreground conflicts.** No per-prefix lock.
- **Rebalance** (SPFresh LIRE) = background: split oversized postings via 2-means,
  reassign boundary vectors from neighboring postings, merge undersized ones.
  Splits conflict with concurrent appends **by design** — and the *split* retries,
  never the insert.

## 3. Key layout

All under the index's subspace `S` (standard vector-index subspace allocation; a new
index type ⇒ new index ⇒ its own subspace, so no collision with Java HNSW is possible
by construction). Grouping prefixes (for `PARTITION BY`-style indexes) compose in
front exactly as the HNSW index does today.

| Subspace | Key | Value | Notes |
|---|---|---|---|
| `S/0` CENTROIDS | `(centroidID int64)` | `Tuple{fullVector bytes, state byte, epoch int64}` | state: ACTIVE / FORWARD / DEAD. FORWARD value carries the 2 child centroidIDs |
| `S/1` POSTINGS | `(centroidID, pk Tuple)` | `Tuple{rabitqCode bytes [, fp16 bytes]}` | the contiguous range `(centroidID, *)` *is* the posting list |
| `S/2` MEMBERSHIP | `(pk Tuple)` | `Tuple{centroidID...}` | authoritative copy-set for delete/update/reassign |
| `S/3` COUNTERS | `(centroidID)` | little-endian int64 | atomic ADD on insert/delete/split |
| `S/4` CHANGELOG | `(versionstamp)` | `Tuple{op byte, centroidID, [payload]}` | ADD / DEAD / FORWARD deltas for cache refresh |
| `S/5` TASKS | `(versionstamp)` | `Tuple{kind byte, centroidID}` | split/merge queue, claimed by the rebalancer |
| `S/6` META | `(key)` | misc | config echo, build state, changelog GC horizon, RaBitQ transform (rotator seed + centroid, same encoding as HNSW's AccessInfo) |

`centroidID` is allocated from a META counter via atomic ADD-and-snapshot-read (or
versionstamp-derived); IDs are never reused.

Sizing at 10M × 768D, RaBitQ 1 ex-bit (~192 B/code), `Lmax=512` (avg fill ~⅔ ≈ 340),
replication r=2: ~20M posting entries / 340 ≈ **59k centroids**. Client cache =
59k × ~200 B ≈ **12 MB**. POSTINGS total ≈ 20M × ~220 B ≈ **4.4 GB**. All values
far under 100 KB; a full posting (512 × 220 B ≈ 113 KB) spans many KVs, so the
*value* limit is irrelevant and the *range read* stays one round trip.

## 4. Query path

```
budget: 3 round trips, p50 target < 10 ms at 10M (vs HNSW's 25–73 ms at 1k–1M)

RT0  GetRange(CHANGELOG, from: cachedVersion)     ── usually empty; doubles as
     (snapshot read, piggybacked with GRV)            cache validation + delta feed
CPU  apply deltas; SIMD scan centroid table → k_c nearest ACTIVE centroids
     (RaBitQ asymmetric distance; 59k codes ≈ sub-ms)
RT1  k_c parallel GetRange(POSTINGS/(cID,*)) snapshot reads
     SPANN query-aware pruning: drop centroids with dist > (1+ε)·d_nearest
CPU  RaBitQ distance for every fetched code; maintain top-C heap (C ≈ 2–4× k)
RT2  parallel loads of the C candidate source records (full vectors) → exact
     re-rank → top-k
```

- All reads are **snapshot** reads: queries take no conflict ranges and never abort
  writers or rebalancers.
- A FORWARD centroid encountered via a stale cache: the posting range read returns
  the residual entries plus the forward marker is already in the cached delta; in the
  worst case one extra RT re-fetches the two child postings. Bounded staleness, no
  wrong results (children are written before the parent is marked FORWARD — §6).
- Tombstone-free: deletes physically clear posting keys (§5), so queries do no
  filtering. A pk seen twice (closure replication) dedups in the top-C heap — same
  spirit as Java's HNSW dedup-on-read.
- `EXPLAIN` surfaces as a new scan type (`VectorSPFreshIndexScan`) through the same
  vector-scan planner surface the HNSW index uses; no Cascades rule changes (the
  planner already dispatches on index type for vector predicates).

## 5. Write path

**Insert(pk, v):**
1. Refresh centroid cache (RT0 as above, amortized; inserts tolerate stale routing —
   a slightly-wrong posting choice is a recall detail, repaired by reassignment).
2. RNG-rule closure assignment (SPANN §4.2): take the r nearest centroids, keep
   centroid c_i only if `dist(v, c_i) < α · dist(c_1, c_i)` — replicate to boundary
   regions only. r ∈ [1,4], default 2, α = 1.0 (tunable).
3. One transaction:
   - `Set(POSTINGS/(c_i, pk), code(v))` for each kept c_i (blind writes)
   - `Set(MEMBERSHIP/pk, [c_i...])` — read-modify-write **on this pk only** (an
     existing row means update: clear old posting keys first, in the same tx)
   - `Add(COUNTERS/c_i, +1)` (conflict-free atomic)
   - snapshot-read each counter; if > Lmax, blind-write `TASKS/(vs) = split(c_i)`
     (snapshot read ⇒ no conflict range on the hot counter)

Conflict analysis: two inserts of *different* pks share **no written key** and take
**no read conflict** except their own membership rows ⇒ they never conflict, no
matter how many writers, processes, or machines. Two writes of the *same* pk
serialize on the membership key — which is the semantically required behavior. The
HNSW per-prefix mutex and its multi-instance FDB-1020 storms are gone *by
construction*, not mitigated.

**Delete(pk):** read `MEMBERSHIP/pk` → clear each `POSTINGS/(c_i, pk)`, clear
membership, `Add(COUNTERS/c_i, -1)`, enqueue merge task if a snapshot counter read
< Lmin. Precise point deletes — no tombstones, no query-time filtering, no SPFresh
garbage-accumulation problem (FDB gives us exact keys; an SSD-page design doesn't).

**Update** = insert with an existing membership row (handled above, one tx).

A delete racing a split that moves the same pk serializes correctly through the
membership key: the split rewrites `MEMBERSHIP/pk` (§6), the delete reads it — FDB's
conflict detection orders them, and whichever loses retries against the new truth.

## 6. LIRE maintenance (SPFresh §3, adapted to FDB transactions)

Run by a background **rebalancer** (an `OnlineIndexer`-style job: claim a TASKS key,
do bounded work, commit, repeat — resumable, idempotent via task keys + centroid
epochs). Multiple rebalancer instances coexist: task claims are tx-protected.

**Split(c)** when counter > Lmax:
1. Tx A (bounded: ≤ Lmax entries ≈ 113 KB read + 2× written):
   - range-read `POSTINGS/(c,*)` — this read's conflict range is what makes a
     concurrent foreground append *win*: the append commits, the split conflicts
     and retries with the new entry included. Foreground latency is never taxed.
   - 2-means on the decoded codes (client CPU) → centroids c₁, c₂ (new IDs)
   - write both child CENTROIDS rows (ACTIVE) + their POSTINGS entries + updated
     MEMBERSHIP rows + COUNTERS; mark `c` FORWARD(c₁,c₂); clear old posting range;
     CHANGELOG: ADD c₁, ADD c₂, FORWARD c.
   If Lmax is configured large enough that one tx would exceed limits, the split
   chunks: children are fully written first, parent flips to FORWARD in the final
   chunk — readers see either the old complete posting or the forward, never a
   partial child set.
2. **NPA reassignment** (SPFresh's accuracy-preserving step), as follow-up tasks:
   for each centroid c_n in the K_n nearest neighbors of the *old* c (from the
   cache): for each (pk, code) in `POSTINGS/(c_n,*)`, recompute the nearest-centroid
   set under {…, c₁, c₂}; if it changed, rewrite that pk's posting keys + membership
   (one small tx per batch of ~64 vectors). This is the piece that keeps recall flat
   under churn — SPANN-without-reassignment degrades at region boundaries.
3. GC: FORWARD centroids older than the changelog horizon (all caches refreshed
   past them, horizon = max client staleness budget, default 10 min) flip to DEAD
   and are purged with their changelog prefix.

**Merge(c)** when counter < Lmin (default Lmax/8): move c's residents into their
next-nearest centroids (same machinery as reassignment), FORWARD c to nothing
(DEAD after horizon).

The rebalancer is *optional for correctness* — without it postings grow and recall
at boundaries drifts, but reads and writes stay correct. This is the key operational
property: maintenance debt degrades performance, never data.

## 7. RaBitQ integration

- Global transform (rotator seed + rotated centroid) in META — same encoding and
  bootstrap approach as the HNSW index's AccessInfo (PR #278 made that
  Java-faithful; here it is Go-only, so the simpler **build-time** establishment is
  used: the bulk build computes the exact mean, no sampling protocol needed; the
  incremental-only path falls back to the SAMPLES-style bootstrap).
- Posting codes quantize the **residual** `v − centroid(c)` (IVF-style): residuals
  are small and centered, which is where RaBitQ's error bound is tightest — better
  recall per bit than quantizing absolute positions (and strictly better than the
  HNSW index can do, since HNSW has no per-region anchor).
- Centroid-selection codes in the client cache quantize absolute positions (one
  shared transform).
- Re-rank from source records keeps exact distances out of the index entirely; an
  optional `fp16` per-entry column trades 2× posting size for skipping RT2 when the
  approximation margin is decisive (SPANN's "disk-bypass" trick).

## 8. Build path (bulk)

Via `OnlineIndexer` (exists in-tree), index in `WriteOnly` during build:
1. **Sample pass:** reservoir-sample ~256k vectors (range scan of records).
2. **Hierarchical balanced k-means** (SPANN §4.1) on the sample, client-side, to
   the target centroid count; write CENTROIDS + the RaBitQ transform.
3. **Assignment pass:** stream all records in OnlineIndexer batches; each batch tx
   does closure assignment + posting/membership/counter writes (same code as the
   foreground insert — one write path, no parallel pipeline).
4. Flip to `Readable`. Concurrent foreground writes during the build follow the
   normal WriteOnly contract (they index themselves; the indexer skips built ranges).

10M vectors at batch=200/tx ≈ 50k txs; with 8 parallel OnlineIndexer ranges and
~3 RT/tx this is **~1–2 hours**, vs HNSW's measured 9.5 vec/sec ⇒ ~12 *days*. The
build is also restartable at batch granularity, which the HNSW build is not.

## 9. Expected performance (targets, to be validated in phase 5)

| Operation | HNSW (measured) | SPFresh target | Mechanism |
|---|---|---|---|
| Insert throughput, 1 writer | 35–70 vec/sec | > 1,000 vec/sec | 1 tx, ~3 RT, no traversal |
| Insert scaling, N writers | ~flat (lock) | ~linear to FDB commit ceiling | zero shared keys |
| Search p50 @ 1M | 25–73 ms | < 8 ms | 3 RT, parallel ranges |
| Search p50 @ 10M | n/a (build infeasible) | < 12 ms | k_c, Lmax tuned; same depth |
| Recall@10 | ~0.95 (ef=64) | ≥ 0.90 @ k_c=48; ≥ 0.95 @ k_c=96 | SPANN paper curves + re-rank |
| Bulk build 10M | ~12 days (extrapolated) | 1–2 h | range-scan + batch assign |

(SPANN reaches 90%+ recall@10 touching ~10% of postings; SPFresh holds recall flat
under 1%/day churn with LIRE. Our re-rank step relaxes the in-posting recall
requirement further.)

## 10. Wire format & Java story

- **New index type** `IndexTypeVectorSPFresh = "vector_spfresh"` — a Go-only
  extension, explicitly permitted by the project charter ("the read-side query
  surface MAY go beyond Java... net-new capabilities Java lacks entirely are
  welcome, provided wire compat is never sacrificed and the extension has deep test
  coverage"). Java apps sharing the cluster: records remain fully Java-readable
  (the index only adds entries under its own subspace); a Java app given metadata
  containing this index type fails index-maintainer lookup exactly as for any
  unknown type — deployments that share *metadata* must keep writers Go-only or
  keep the index out of shared metadata. Documented in the index's godoc.
- All structural options (`spfreshLmax`, `spfreshReplication`, `spfreshAlpha`,
  dims, metric, RaBitQ bits) are **immutable** via `validateVectorIndexOptions`-
  style evolution checks from day one (the lesson of RFC-round-4 on PR #278:
  immutability is what makes config-derived invariants sound). Runtime knobs
  (`k_c`, ε, re-rank C, rebalancer pacing) are query/maintenance-time, not stored.
- No changes to record format, continuations (the scan returns top-k like the HNSW
  scan does today), or any Java-shared subspace.

## 11. Testing plan (repo standard: no mocks, real FDB, t.Parallel)

1. **Unit:** posting codec round-trip; RNG-rule assignment; 2-means determinism
   (seeded); changelog delta application; counter/task arithmetic. Fuzz: posting
   value parser, membership parser (0 panics / 200k execs).
2. **FDB integration:** insert/search/delete/update e2e; closure dedup; FORWARD
   traversal with a deliberately stale cache; split mid-stream (insert-during-split
   conflict direction pinned: the *split* retries); merge; delete-vs-split race;
   counter drift reconciliation; recovery of a half-claimed task.
3. **Concurrency:** N-writer stress proving zero foreground 1020s (the headline
   property — assert conflict metrics, not just throughput); rebalancer running
   concurrently with writers + readers.
4. **Chaos:** extend `StoreModel` with the posting/membership invariant
   (membership row ⇔ exact posting keys; counters within drift bound) and
   `Verify()` after faults; split/merge under `commit_unknown`.
5. **Recall/perf:** SIFT-1M recall@10 vs brute force; A/B vs HNSW (the
   `VECTOR_BENCHMARK_RESULTS.md` harness); 1M stress-table entry; 10M soak with
   churn (SPFresh's headline scenario: sustained updates, flat recall).

## 12. Phases (one PR each, e2e-proven per the no-fake-checkboxes rule)

1. **094.1** Layout + static read path: subspaces, codecs, bulk build via
   OnlineIndexer, query path, recall benchmark. (No incremental writes yet —
   index is read-only after build; honest about it in the maintainer.)
2. **094.2** Foreground writes: insert/update/delete + membership + counters +
   task enqueue; N-writer zero-conflict stress.
3. **094.3** Rebalancer: split + NPA reassign + merge + FORWARD/GC + chaos
   invariants.
4. **094.4** RaBitQ residual quantization + fp16 disk-bypass + re-rank tuning.
5. **094.5** 10M soak, churn benchmarks, `VECTOR_BENCHMARK_RESULTS.md` update,
   tuning-defaults freeze.

## 13. Alternatives considered

- **DiskANN/Vamana on FDB:** still a graph traversal — fewer, fatter hops, but the
  round-trip *depth* remains O(path length) and inserts still mutate shared
  adjacency (RobustPrune touches many nodes' lists). Rejected for this substrate;
  its ideas (high degree, PQ-in-memory) are subsumed by posting lists + RaBitQ.
- **Batched beam search over existing HNSW:** real, wire-neutral query win
  (collapses N hops into log-depth batched rounds) — worth doing *on the HNSW
  index*, tracked separately in TODO.md; does nothing for write scalability.
- **IVF-Flat without SPFresh:** SPANN minus rebalancing/closure — degrades under
  churn and at region boundaries; the LIRE layer is cheap on FDB (it's just more
  transactions) and is what makes the index production-grade rather than a demo.
- **Brute force + RaBitQ:** optimal to ~100k vectors (one big parallel range
  read); not viable at 1M+. The SPFresh design *degenerates to* this with one
  centroid, which is effectively what phase 094.1 tests at small N.

## 14. Open questions

1. **Centroid cache in multi-tenant processes:** one cache per (index, store
   prefix); LRU across tenants with a global memory budget — default 256 MB?
2. **Counter accuracy:** atomic counters drift under `commit_unknown` retries
   (the increment is not idempotent). Drift only mis-times split/merge triggers —
   harmless to correctness; the split tx recomputes the true size from the range
   read. Reconcile counters during splits, or also via a periodic task?
3. **Grouped (prefixed) indexes:** per-group centroid tables could be tiny (small
   groups ⇒ brute-force postings, no centroids at all below a threshold N₀ —
   "auto-degenerate" mode). Worth specifying in 094.1 or defer?
4. **`fp16` column default:** on (2× storage, often skips RT2) or off (lean)?
   Decide with phase-4 data.

## 15. References

- SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search
  (Chen et al., NeurIPS 2021) — centroid/posting architecture, closure assignment,
  query-aware pruning, hierarchical balanced clustering.
- SPFresh: Incremental In-Place Update for Billion-Scale Vector Search
  (Xu et al., SOSP 2023) — LIRE protocol: split / merge / neighbor-posting
  reassignment, recall-under-churn methodology.
- RaBitQ (Gao & Long, SIGMOD 2024) — in-tree at `pkg/rabitq`; residual
  quantization fit per §7.
- `pkg/recordlayer/VECTOR_BENCHMARK_RESULTS.md` — the measured HNSW-on-FDB numbers
  motivating §1.
