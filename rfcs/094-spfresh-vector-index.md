# RFC-094 — SPFresh: an FDB-native vector index (centroid + posting lists, incremental rebalancing)

**Status:** Rev 2 — rev 1 was NAK'd by all four reviewers (FDB C++ author, Torvalds,
LanceDB founder, codex; verdicts on PR #279). Rev 2 redesigns the centroid lifecycle
(SEAL→SPLIT→FORWARD), fixes the ID allocator, restates the conflict claims honestly,
corrects the round-trip and clustering math, and adds backpressure, observability,
filtered search, and cold start. Per-finding dispositions are on the PR.

**Scope:** A second, Go-only vector index type built *for* FoundationDB's performance
model, targeting 1M–10M vectors with linear writer scalability. Architecture: SPANN's
centroid + posting-list layout for reads, SPFresh's LIRE protocol for in-place
incremental updates, RaBitQ (in-tree, `pkg/rabitq`) for compressed distance. The
existing HNSW index (`IndexTypeVector`) is unchanged and remains the
Java-wire-compatible option.

**TODO.md anchor:** "Exploration: a second, FDB-native vector index
(SPFresh/DiskANN/SPANN/beam)". Batched beam search (the wire-neutral HNSW query
improvement listed there) is complementary and out of scope here.

---

## 1. Problem

The HNSW index is now 100% Java-compliant (PR #278) — and that is exactly why it
cannot reach the 1M–10M / many-concurrent-writers regime on FDB. Measured on this
codebase (SIFT-128D unless noted; testcontainer FDB, ~0.3–0.5 ms per round trip):

| Metric | HNSW on FDB (measured) | Why |
|---|---|---|
| Build / insert | ~35–70 vec/sec, *degrading* with graph size (349 → 48 vec/sec from 100 → 1k vectors) | every insert greedy-descends the graph: O(layers × hops) **dependent** point reads; reverse-edge writes mutate shared hub nodes |
| Search p50 | 25–73 ms | sequential traversal: each hop needs the previous hop's neighbor list — round-trip *depth*, not bandwidth |
| Concurrent writers | serialized | per-prefix write lock (Java parity); without it, FDB-1020 conflict storms on shared adjacency keys |
| Gap vs disk-backed ANN systems | ~16× QPS vs Qdrant after all wire-neutral wins | architectural, not implementational |

**HNSW's unit of work is a pointer chase; FDB's unit of efficiency is a range read.**

### What a high-latency networked KV store is actually good at

| FDB property | Design consequence |
|---|---|
| ~0.3–2 ms per round trip, regardless of payload | minimize round-trip **depth**; dependent reads are the enemy |
| range reads stream large payloads; futures pipeline | fetch *wide*: many parallel range reads in one burst |
| `REPLY_BYTE_LIMIT` = 80 KB per range-read reply (ClientKnobs.cpp:66) | size postings so one posting = one reply |
| optimistic concurrency; conflicts abort whole txs | foreground writes touch disjoint keys; conflict ranges are taken *deliberately*, where we want serialization |
| atomic ADD and versionstamps are conflict-free | counters and append-keys come for free — but ADD is **not** fetch-and-add (no unique value returned) |
| 5 s tx limit, 10 MB tx size, 100 KB value | maintenance work must be small, chunked, resumable |
| no server-side compute | all distance math runs client-side; the routing state it needs must be cacheable client-side |

SPANN/SPFresh is the architecture these constraints select for: a small routing
structure (centroids) cached client-side, big contiguous blocks (posting lists)
fetched in single round trips, writes that append to one partition, and rebalancing
pushed to background transactions that bear the retry cost.

## 2. Architecture overview

```
                       CLIENT (stateless, per-process cache)
  ┌──────────────────────────────────────────────────────────────┐
  │  centroid table: []{centroidID, fp16 full vector, state}      │
  │  refreshed by a BACKGROUND timer (default 1 s) via changelog  │
  │  SIMD scan selects k_c nearest ACTIVE centroids               │
  └───────────────┬──────────────────────────────────────────────┘
                  │ k_c parallel GetRange (one round-trip burst)
                  ▼
  FDB   POSTINGS/(centroidID, pk) → RaBitQ residual code
        SIDECAR/(pk)              → fp16 full vector (re-rank)
        CENTROIDS/(centroidID)    → full vector + state(+children) + epoch
        CHANGELOG/(versionstamp)  → centroid delta        MEMBERSHIP/(pk) → [cID...]
        COUNTERS/(centroidID)     → posting size (ADD)    TASKS/(vs)      → work items
        META/                     → config, ID allocator, horizon, transform
```

- **Search** = scan cached centroids (CPU, zero I/O — the cache refresh is off the
  query path) → fetch k_c postings in parallel (1 RT) → RaBitQ residual distances
  (CPU) → re-rank top-C from the fp16 sidecar (1 RT, parallel point reads).
  **3 network round trips** (GRV + postings + re-rank), constant in N.
- **Insert** = route on cached centroids → **read the target centroids' state rows
  (real reads — the deliberate conflict range, §5)** → write r posting keys +
  sidecar + membership + counter ADDs → commit.
- **Rebalance** (LIRE) = background SEAL→SPLIT→FORWARD lifecycle (§6): a sealed
  centroid rejects late writers via their state read; the big split transaction then
  runs against a frozen posting. Foreground inserts conflict **only** with a
  seal/split of their own target centroid — never with each other.

### Honest conflict claims (rev 2)

| Pair | Outcome |
|---|---|
| insert(pk₁) vs insert(pk₂), any centroids | **never conflict** (disjoint writes, disjoint state reads of ACTIVE rows that nobody is writing) |
| insert vs insert, same pk | serialize on `MEMBERSHIP/pk` — semantically required |
| insert vs seal/split of a target centroid | the **later** one retries: a straggler insert aborts on its state read and re-routes; a seal racing a committed append retries and re-seals (§6) |
| update/delete vs split that moves the same pk | serialize on `MEMBERSHIP/pk`; the foreground op retries against the new truth |
| query vs anything | snapshot reads — never conflicts, never aborts anyone |

Rev 1 claimed "zero foreground conflicts"; that was false for the
straggler/membership cases above (FDB-author finding #2). What survives — and is the
property that matters — is: **foreground writes never conflict with *each other*
(except same-pk), and the conflict surface with maintenance is a single small state
row per assigned centroid, paid only while that centroid is actually splitting.**

## 3. Key layout

All under the index's subspace `S` (new index type ⇒ own subspace ⇒ no collision
with Java HNSW by construction). Grouping prefixes compose in front exactly as the
HNSW index does today.

| Subspace | Key | Value | Notes |
|---|---|---|---|
| `S/0` CENTROIDS | `(centroidID int64)` | `Tuple{fp32 vector, state, epoch, [childA, childB]}` | state: ACTIVE / SEALED / FORWARD / DEAD |
| `S/1` POSTINGS | `(centroidID, pk Tuple)` | `Tuple{rabitqResidualCode}` | the range `(centroidID, *)` *is* the posting |
| `S/2` MEMBERSHIP | `(pk Tuple)` | `Tuple{centroidID...}` | authoritative copy-set for delete/update/reassign |
| `S/3` COUNTERS | `(centroidID)` | little-endian int64 | atomic ADD; **advisory only** (§6 reconciles) |
| `S/4` CHANGELOG | `(versionstamp+userVersion)` | `Tuple{op, centroidID, payload}` | multiple entries per tx need distinct 2-byte user-versions (FDB-author #6) |
| `S/5` TASKS | `(versionstamp+userVersion)` | `Tuple{kind, centroidID, owner, leaseDeadline, [childIDs]}` | claims carry a lease; expired claims are reclaimable (Torvalds #7) |
| `S/6` META | `(key)` | misc | config echo, **centroid ID allocator (transactional RMW — §6)**, changelog GC horizon, RaBitQ transform, build state |
| `S/7` SIDECAR | `(pk Tuple)` | fp16 full vector | re-rank source; written on insert, cleared on delete |

**Centroid ID allocation:** a META allocator key read-modify-written with a *real*
read (deliberate conflict). Contention scope = concurrent split/merge txs only —
background ops, low rate. Rev 1's "atomic ADD + snapshot read" was not a
fetch-and-add: two rebalancers could observe the same value and mint colliding IDs
(codex P1). IDs are never reused.

**Sizing at 10M × 768D** (RaBitQ 1 ex-bit ≈ 192 B/code + key overhead ≈ 220 B/entry,
replication r=2 ⇒ 20M entries):

- `Lmax = 256` ⇒ posting ≤ ~56 KB ⇒ **one `REPLY_BYTE_LIMIT` reply per posting**
  (rev 1's Lmax=512 ≈ 113 KB silently cost 2 sequential hops per posting —
  FDB-author #4b). Avg fill ~⅔ ⇒ ~170 entries ⇒ **~118k centroids**.
- Client cache = 118k × (1.5 KB fp16 + 16 B meta) ≈ **180 MB** per process at 10M
  (≈ 18 MB at 1M). Full vectors are *required* client-side: residual encoding needs
  `v − c` and scoring needs `q − c`; a quantized centroid code cannot produce either
  (codex P2). fp16 centroids also avoid compounding routing error (LanceDB #7).
  Multi-tenant processes enforce a global cache budget (default 1 GB, LRU across
  (index, prefix) tables); a table over budget falls back to scanning CENTROIDS
  per query (one extra range-read burst — degraded, correct, metered).
- POSTINGS ≈ 4.4 GB; SIDECAR ≈ 10M × 1.5 KB = 15 GB (optional but default-on, §7).

The centroid-density tradeoff is real and is a *tuning axis*, not a constant: recall
wants many small postings probed wide (the IVF rule of thumb is nprobe ≈ √nlist for
high recall); FDB wants postings near the 80 KB reply size. Phase 094.1's benchmark
(on real 768D embeddings, not SIFT — LanceDB #1) freezes the defaults; the layout
supports any (Lmax, nlist) point without migration (splits/merges move between them).

## 4. Query path

```
budget: 3 network round trips; p50 target < 10 ms at 10M (validate in 094.1/5)

bg   changelog refresh runs on a process timer (default 1 s): GetRange(CHANGELOG,
     from: cachedVersion) + read META horizon. If cachedVersion < horizon (client
     slept past GC), FULL reload of the centroid table (Torvalds #3). Queries
     NEVER spend a round trip on cache maintenance (rev 1's per-query RT0 was a
     hidden hot key: every query in the fleet hammering one changelog shard —
     LanceDB #5).

RT0  GRV (its own proxy round trip — rev 1 "piggybacked" it away; FDB-author #4a)
CPU  SIMD scan of cached fp16 centroids → k_c nearest ACTIVE
     (118k × 768D fp16 ≈ sub-ms); SPANN ε-pruning: drop centroids with
     dist > (1+ε)·d_nearest
RT1  k_c parallel GetRange(POSTINGS/(cID,*)) snapshot reads, each one reply;
     per-posting fetch cap RangeOptions.Limit = 2×Lmax (backpressure guard — an
     unmaintained oversized posting degrades THIS query bounded-ly and emits a
     metric, instead of streaming 220 MB — Torvalds #4)
CPU  RaBitQ residual distances vs (q − c) per posting; top-C heap (C ≈ 2–4× k);
     replica dedup keeps the MIN estimate per pk (closure copies carry different
     residuals — LanceDB #3a)
RT2  parallel point reads of SIDECAR/(pk) for the C candidates (1.5 KB each;
     ~600 KB at C=400 — 8× cheaper than reading 12 KB source records; LanceDB #6)
     → exact fp16 re-rank → top-k. (Sidecar disabled ⇒ read source records.)
```

- FORWARD centroid met via a ≤1 s-stale cache: routing already has the children
  from the delta; residually, one extra RT re-fetches child postings. Children are
  fully written before the parent flips FORWARD (§6), so no window returns wrong
  results.
- Tombstone-free reads: deletes physically clear posting keys (§5).
- **Filtered search** (LanceDB #7 — specced now because it constrains the layout):
  the posting scan already streams (pk, code) pairs; a pushed-down predicate
  evaluates as a pk-set/bitmap filter *before* the top-C heap, and k_c widens
  adaptively when selectivity starves the heap (same loop, no second path). Exact
  per-query semantics ride the existing vector-scan planner surface; no Cascades
  changes.
- `EXPLAIN`: new scan type `VectorSPFreshIndexScan`; continuations: the scan
  returns top-k like today's HNSW scan (a result-set continuation, not a traversal
  continuation — same contract).

## 5. Write path

**Insert(pk, v):**
1. Route on the cached table: RNG-rule closure assignment (SPANN §4.2) — keep
   centroid c_i of the r nearest only if `dist(v, c_i) < α · dist(c_1, c_i)`;
   r ∈ [1,4] default 2, α = 1.0.
2. One transaction:
   - **real read** of `CENTROIDS/(c_i)` for each assigned centroid — the deliberate
     conflict range that makes the SEAL lifecycle sound (§6). Not ACTIVE ⇒ drop
     c_i, take the next-nearest from the cache (re-read its state) — the
     authoritative state read corrects any cache staleness.
   - read `MEMBERSHIP/pk` (real): existing row ⇒ update — clear old posting keys
     (and their counter ADD −1) in the same tx.
   - `Set(POSTINGS/(c_i, pk), residualCode(v, c_i))`, `Set(SIDECAR/pk, fp16(v))`,
     `Set(MEMBERSHIP/pk, [c_i...])`, `Add(COUNTERS/c_i, +1)`.
   - split trigger probe, **sampled** (default 1/8 of inserts): snapshot-read the
     counter; if > Lmax, blind-write a TASKS item. Sampling bounds the hot-key
     *read* load on a hot centroid's counter — RYW must do a real storage read to
     apply your own ADD locally (DEPENDENT_WRITE, RYWIterator.cpp:34) (FDB-author
     #5). If the snapshot count exceeds the **hard ceiling 4×Lmax**, the writer
     performs the seal+split inline before returning (insert-side backpressure:
     the index never depends on an external daemon for boundedness — Torvalds #4).

Cost: GRV + one parallel read burst (state rows ∥ membership ∥ sampled counter) +
commit ≈ **3 round trips** (rev 1 claimed the same total with wrong itemization).
Single-writer throughput is therefore tx-latency-bound at ~300–500 inserts/sec;
the path to high aggregate rates is **batching** (many vectors per tx — the record
layer's natural batch write) and **parallel writers**, which this design scales
linearly because writers share no keys. Rev 1's ">1,000 vec/sec, 1 writer" implied
unstated batching (Torvalds #1); the honest target table is §9.

**Delete(pk):** read membership → clear posting keys + sidecar + membership,
`Add(COUNTERS/c_i, −1)`; sampled merge-trigger probe. Precise point deletes — no
tombstones, no read filtering, no SPFresh garbage problem.

**Update** = insert with an existing membership row (one tx, above).

## 6. LIRE maintenance: the SEAL → SPLIT → FORWARD lifecycle

Background **rebalancer**: claims a TASKS item (tx write of owner + lease deadline;
expired leases reclaimable — a dead rebalancer never wedges a task), does bounded
work, repeats. Runs **in-process on writers by default** (a goroutine the maintainer
owns, like HNSW maintenance work happens inline today), optionally as a dedicated
runner; multiple instances coexist (claims serialize transactionally).

**Split(c)** — three steps, each idempotent:

1. **SEAL** (tiny tx): read task claim; read `CENTROIDS/c` — require ACTIVE;
   allocate child IDs from the META allocator (real RMW); write state SEALED +
   child IDs into the task row. Idempotent under `commit_unknown_result`: the
   retry that finds SEALED **with child IDs already recorded in its own task row**
   treats the seal as committed and proceeds to step 2; SEALED with a different
   task's IDs is impossible (the claim serializes ownership).
   *Fencing:* every foreground insert real-reads the state row (§5). Seal-vs-insert
   races serialize: if the seal commits first, the straggler insert aborts on its
   state read and re-routes; if the insert commits first, the seal (whose write
   intersects the insert's read range... no — whose *own* claim re-read and state
   write conflict with nothing the insert wrote) — the seal simply commits too, and
   the insert it raced is already in the posting it sealed, which is fine: **the
   split reads the posting *after* the seal**, so it sees every entry that will ever
   exist there. After SEAL commits, no insert can add to c (state read sees SEALED)
   — the posting is frozen. This closes rev 1's lost-vector hole: a blind write into
   a cleared range can no longer happen, because the writer's state read serializes
   it against the lifecycle (the unanimous 4-reviewer finding; resolver mechanics:
   only the *reader's* ranges are checked, SkipList.cpp:983, so the read must be on
   the foreground side).
   *Livelock:* rev 1's split could retry forever against a hot posting's appends
   (LanceDB #3); sealing first means the big tx below runs contention-free.
2. **SPLIT** (big tx, or chunked): read `CENTROIDS/c` — require SEALED with *these*
   child IDs (idempotency guard); range-read the frozen posting; 2-means
   client-side; write children (ACTIVE, **exact** counters — mandatory counter
   reconciliation, drift never compounds; Torvalds #5), their POSTINGS + rewritten
   MEMBERSHIP rows; clear the parent posting; flip parent → FORWARD(children);
   CHANGELOG entries (distinct user-versions); **clear the TASKS key in this tx**.
   Under `commit_unknown_result` the retry re-reads state: FORWARD (or task gone)
   ⇒ already committed ⇒ no-op — no garbage 2-means, no re-minted IDs, no double
   FORWARD (FDB-author #3). If posting bytes exceed single-tx comfort, chunk:
   children fill first, parent stays SEALED, the final chunk flips FORWARD — readers
   see the old complete posting or the forward, never a partial child set.
3. **NPA reassignment** (follow-up tasks, the recall-preserving step from SPFresh):
   for centroids in the K_n nearest neighbors of old c, re-evaluate each member's
   nearest-centroid set under the new children; rewrite moved pks' postings +
   membership in small batches (~64/tx). Serializes with foreground updates of the
   same pk via the membership key.

**Merge(c)** when sampled counter < Lmin (= Lmax/8): same lifecycle (SEAL, drain
into nearest neighbors, FORWARD-to-nothing). A **post-split merge cooldown** (no
merge task for a child within T_cool, default 10 min) prevents split→reassign→merge
thrash when NPA drains a fresh child (LanceDB #4).

**GC:** FORWARD/DEAD centroids older than the changelog horizon: range-read the
(supposedly empty) posting first; **if any entry exists, drain it to the children
instead of clearing** — a one-line invariant check that converts any future
lifecycle bug from silent data loss into a metric + repair (and a chaos `Verify()`
invariant). Then purge centroid + changelog prefix.

The rebalancer is optional for *correctness* of stored data, but rev 2 removes the
"and therefore nobody needs to run it" implication: the inline hard-ceiling split
(§5) bounds posting growth even with no rebalancer, and the query-side fetch cap
bounds the read cost of any backlog (Torvalds #4).

## 7. RaBitQ integration

- Global transform (rotator seed + rotated centroid) in META — same encoding as the
  HNSW AccessInfo; established exactly at build time (bulk path computes the true
  mean; the from-zero incremental path uses the SAMPLES-style bootstrap already
  in-tree).
- Posting codes quantize the **residual** `v − centroid(c)` — IVF-standard, tightest
  where RaBitQ's error bound lives; one global rotation is fine (data-independent;
  LanceDB confirmed). Scoring computes `q − c` per probed posting — full centroid
  vectors come from the client cache (codex P2 made this explicit).
- Closure replicas of one pk carry *different* residuals ⇒ different estimates;
  dedup keeps the min estimate (§4); the re-rank step then decides exactly.
- Re-rank reads the fp16 SIDECAR (default on), not 12 KB source records; disabling
  the sidecar (option) falls back to source-record reads — leaner storage, fatter
  RT2. No fp16-in-posting variant (rev 1's "disk-bypass" bloated every posting 8×
  for a per-query saving the sidecar gets cheaper — LanceDB #6).

## 8. Build path (bulk)

Via `OnlineIndexer`, index in `WriteOnly` during build:

1. **Sample pass:** reservoir-sample 256k vectors.
2. **Coarse k-means:** K₀ = 2,048 centroids on the sample — **125 samples per
   centroid** (≥ the ~39–128 floor k-means needs; rev 1 trained 59k centroids from
   the same sample = 4.3 each, i.e. garbage Voronoi cells — LanceDB #2). Write
   CENTROIDS + transform.
3. **Assignment pass:** stream all records in OnlineIndexer batches through the
   *normal insert path* (closure assignment + posting/membership/sidecar writes).
   One write path — build is just batched inserts (no parallel pipeline).
4. **Split-driven growth:** initial postings average ~10k entries (≫ Lmax); the
   standard lifecycle (§6) splits them down to equilibrium (~5 generations,
   2k → ~118k centroids). This is the *same* machinery as steady-state — the build
   exercises it at scale, and "growth by splitting" is also the **cold-start**
   story: an empty index begins life as ONE centroid (the first vector) and grows
   purely by splits. No degenerate brute-force mode, no second code path (rev 1's
   open question 3 — Torvalds #6: the emergent version is the design).
5. Flip `Readable`.

**Derived cost, 10M × 768D** (Torvalds #1 — CPU terms dominate, not round trips):
coarse k-means ≈ 256k × 2k × 768 × ~15 iters ≈ 6×10¹² MACs ≈ ~1 min on 8 cores;
assignment ≈ 10M × 2k × 768 ≈ 1.5×10¹³ MACs ≈ ~3 min (BLAS/SIMD batched);
write I/O ≈ 10M × ~2 KB across ~50k batch txs at 8-way parallelism ≈ ~20–40 min;
split cascade rewrites ≈ 5 × 4.4 GB ≈ 22 GB of background churn ≈ ~30–60 min
overlappable with foreground writes. **Total ≈ 1–2 h wall clock, I/O-bound** — the
rev 1 number, now with its dominant terms shown. HNSW extrapolates to ~12 days.

## 9. Performance targets (each row: how derived; validate in 094.1/094.5)

| Operation | HNSW (measured) | SPFresh target | Derivation |
|---|---|---|---|
| Insert, 1 writer, no batching | 35–70 vec/sec | ~300–500 vec/sec | 1 tx ≈ 2–3 ms, latency-bound |
| Insert, batched (200/tx) | n/a | ~10k+ vec/sec/writer | 50 tx/s × 200; tx ≈ 400 KB writes |
| Insert scaling, N writers | ~flat (lock) | ~linear to FDB commit ceiling | zero shared foreground keys |
| Search p50 @ 1M | 25–73 ms | < 8 ms | 3 RTs + ~2 ms CPU + ~1.4 MB fetched |
| Search p50 @ 10M | n/a | < 12 ms | same depth; bigger CPU scan + cache |
| Recall@10, SIFT-1M | ~0.95 (ef=64) | ≥ 0.95 @ tuned (k_c, nlist) | re-rank makes in-posting recall the only loss source |
| Recall@10, 768D real embeddings | n/a | **TBV in 094.1** on DBpedia-OpenAI / Cohere-wiki | rev 1 cited SPANN curves from a 16%-heads regime at our 0.6% ratio — not transferable (LanceDB #1); expect k_c ≈ √nlist territory |
| Bulk build 10M | ~12 days (extrapolated) | 1–2 h | §8 derivation |

## 10. Wire format & Java story

- **New index type** `IndexTypeVectorSPFresh = "vector_spfresh"` — Go-only
  extension, permitted by the project charter (read-side capabilities Java lacks
  entirely; wire compat untouched: the index only writes under its own subspace,
  records stay fully Java-readable). Java apps sharing *metadata* containing this
  index type fail maintainer lookup as for any unknown type — deployments keep
  writers Go-only or keep the index out of shared metadata; documented in godoc.
- All structural options (`spfreshLmax`, `spfreshReplication`, `spfreshAlpha`,
  dims, metric, RaBitQ bits, sidecar on/off) **immutable** via the
  `validateVectorIndexOptions`-style evolution check from day one (the PR #278
  lesson: immutability is what makes config-derived invariants sound). Runtime
  knobs (k_c, ε, C, refresh interval, rebalancer pacing) are not stored.
- Known accepted hotspot: CHANGELOG/TASKS versionstamped keys are ascending-key
  writes to a tail shard (FDB-author #5). Volume is split/merge-rate (background,
  low), not insert-rate — accepted and metered.

## 11. Observability (Torvalds #8)

Metrics (via the maintainer's existing stats hook pattern): posting size p50/p99
(sampled at split/query), counter drift observed at reconciliation, centroid count +
state distribution, cache staleness seconds + full-reload count, task queue depth +
oldest-task age, seal-conflict rate (foreground retries caused by maintenance),
query fetch-cap overage count, re-rank candidate count, **online recall sampler** —
a background job brute-forces a sampled query set against a posting scan and reports
live recall (LanceDB #8); permanent distribution drift shows up here, and the answer
to it is a **retrain**: build a new generation under a fresh META epoch with the
bulk path and atomically swap (documented; phase 094.5).

## 12. Testing plan (repo standard: no mocks, real FDB, t.Parallel)

1. **Unit/fuzz:** posting + sidecar + membership codecs (0 panics / 200k execs);
   RNG-rule assignment; seeded 2-means; changelog application; lease expiry.
2. **FDB integration:** e2e insert/search/delete/update; closure dedup
   (min-estimate); FORWARD traversal with a deliberately stale cache; horizon
   overrun → full reload; filtered search; cold start (1 vector → splits → 100k).
3. **Lifecycle interleavings, each pinned as a deterministic test:**
   straggler-insert-vs-SEAL (insert aborts, re-routes — **the rev-1 lost-vector
   interleaving, pinned exactly as the FDB author wrote it**); append-then-seal
   ordering (split sees the late append); split `commit_unknown` retry = no-op;
   chunked split crash + lease-expiry resume; delete-vs-split same pk; GC
   drain-not-clear invariant.
4. **Concurrency stress:** N writers — assert **conflict metrics**, not just
   throughput: zero insert-vs-insert 1020s; insert-vs-split conflicts bounded by
   split count (the honest claim, tested as stated — Torvalds #4); rebalancer
   concurrent with writers + readers.
5. **Chaos:** extend `StoreModel` with the invariant {membership row ⇔ exact
   posting keys ⇔ sidecar row; counters within declared drift; SEALED/FORWARD
   postings consistent with children}; faults across seal/split/GC.
6. **Recall/perf:** SIFT-1M *and* a 768D real-embedding set (DBpedia-OpenAI or
   Cohere-wiki) vs brute force; A/B vs HNSW via the `VECTOR_BENCHMARK_RESULTS.md`
   harness; 1M stress-table entry; 10M churn soak (SPFresh's headline: sustained
   1–5%/day updates + a hotspot-region burst, flat recall).

## 13. Phases (one PR each; e2e-proven per the no-fake-checkboxes rule)

1. **094.1** Layout + codecs + bulk build (coarse k-means + assignment) + query
   path **including filtered search and the real-embedding recall benchmark**;
   read-only after build (stated honestly in the maintainer).
2. **094.2** Foreground writes (insert/update/delete, state-row fencing, sidecar,
   counters, task enqueue, inline hard-ceiling split) + N-writer conflict-metrics
   stress + cold-start-by-splitting e2e.
3. **094.3** Rebalancer: SEAL/SPLIT/FORWARD + NPA reassign + merge + cooldown +
   GC-drain + lease recovery + chaos invariants + the pinned interleaving suite.
4. **094.4** RaBitQ residual tuning + re-rank/C tuning + sidecar on/off A-B.
5. **094.5** 10M churn soak, online recall sampler, retrain/swap, tuning-defaults
   freeze, `VECTOR_BENCHMARK_RESULTS.md` update.

## 14. Alternatives considered

- **DiskANN/Vamana on FDB:** round-trip depth still O(path length); inserts still
  mutate shared adjacency. Rejected for this substrate.
- **Batched beam search over existing HNSW:** real wire-neutral query win; tracked
  separately in TODO.md; does nothing for write scalability.
- **IVF-Flat without LIRE:** degrades under churn and at region boundaries; the
  rebalancing layer is cheap here (it's just more transactions) and is what makes
  this production-grade.
- **Brute force + RaBitQ:** optimal to ~100k vectors; the cold-start shape of this
  design (few centroids, big postings) *is* that, served by the same code path.

## 15. References

- SPANN (Chen et al., NeurIPS 2021) — centroid/posting architecture, closure
  assignment, ε-pruning, hierarchical balanced clustering.
- SPFresh (Xu et al., SOSP 2023) — LIRE: split / merge / neighbor-posting
  reassignment; recall-under-churn methodology.
- RaBitQ (Gao & Long, SIGMOD 2024) — in-tree at `pkg/rabitq`; residual fit per §7.
- FDB 7.3.75 sources for the mechanics cited throughout: resolver
  (`fdbserver/SkipList.cpp:983`), `REPLY_BYTE_LIMIT` (`fdbclient/ClientKnobs.cpp:66`),
  RYW dependent-write (`fdbclient/RYWIterator.cpp:34`), versionstamp user-versions
  (`fdbclient/ReadYourWrites.actor.cpp:2252`).
- `pkg/recordlayer/VECTOR_BENCHMARK_RESULTS.md` — measured HNSW-on-FDB numbers (§1).
- PR #279 — rev 1 review verdicts (FDB C++ author, Torvalds, LanceDB founder,
  codex) and per-finding dispositions.
