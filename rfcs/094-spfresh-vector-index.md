# RFC-094 — SPFresh: an FDB-native vector index (two-level centroid routing + posting lists, incremental rebalancing)

**Status:** Rev 3. Rev 1 was NAK'd 4/4 (FDB C++ author, Torvalds, LanceDB founder,
codex); rev 2 closed the lost-vector hole with the SEAL→SPLIT→FORWARD lifecycle and
was re-NAK'd on narrower grounds (routing-scan cost, inline-split recovery, split
read semantics, stale-cache cutover visibility, build/phase inconsistency, task
dedup). Rev 3 incorporates all round-2 findings; verdicts and dispositions on
PR #279.

**Scope:** A second, Go-only vector index type built *for* FoundationDB's
performance model, targeting 1M–10M vectors with linear writer scalability.
Architecture: SPANN's centroid + posting-list layout with **two-level routing**,
SPFresh's LIRE protocol for in-place incremental updates, RaBitQ (in-tree,
`pkg/rabitq`) residual quantization. The existing HNSW index (`IndexTypeVector`)
is unchanged and remains the Java-wire-compatible option.

**TODO.md anchor:** "Exploration: a second, FDB-native vector index
(SPFresh/DiskANN/SPANN/beam)". Batched beam search (wire-neutral HNSW query
improvement) is complementary and out of scope.

---

## 1. Problem

The HNSW index is now 100% Java-compliant (PR #278) — and that is exactly why it
cannot reach the 1M–10M / many-concurrent-writers regime on FDB. Measured on this
codebase (SIFT-128D unless noted; testcontainer FDB, ~0.3–0.5 ms per round trip):

| Metric | HNSW on FDB (measured) | Why |
|---|---|---|
| Build / insert | ~35–70 vec/sec, *degrading* with graph size (349 → 48 vec/sec from 100 → 1k vectors) | every insert greedy-descends the graph: O(layers × hops) **dependent** point reads; reverse-edge writes mutate shared hub nodes |
| Search p50 | 25–73 ms | sequential traversal — round-trip *depth*, not bandwidth |
| Concurrent writers | serialized | per-prefix write lock (Java parity); without it, FDB-1020 storms on shared adjacency keys |
| Gap vs disk-backed ANN systems | ~16× QPS vs Qdrant after all wire-neutral wins | architectural |

**HNSW's unit of work is a pointer chase; FDB's unit of efficiency is a range read.**

### What a high-latency networked KV store is actually good at

| FDB property | Design consequence |
|---|---|
| ~0.3–2 ms per round trip, payload-insensitive | minimize round-trip **depth**; dependent reads are the enemy |
| range reads stream large payloads; futures pipeline | fetch *wide*: parallel range reads in one burst |
| `REPLY_BYTE_LIMIT` = 80 KB per range reply (ClientKnobs.cpp:66) | size postings so one posting = one reply |
| optimistic concurrency; only the *committer's read ranges* are checked (SkipList.cpp:983) | blind writes never abort; conflict ranges are taken **deliberately**, on the side that must lose |
| atomic ADD / versionstamps are conflict-free | counters and ordered logs come free — but ADD is **not** fetch-and-add |
| 5 s / 10 MB tx limits, 100 KB values | maintenance units must be small; rev 3 requires split/merge to fit ONE tx |
| no server-side compute | distance math is client-side; routing state must be cacheable, and its **CPU cost budgeted like a round trip** |

## 2. Architecture overview

```
                     CLIENT (stateless; per-process two-level cache)
  ┌────────────────────────────────────────────────────────────────────┐
  │ L1: coarse centroids (~2k, fp16, ~3 MB)  — always resident          │
  │ L2: fine centroids per coarse cell (~60/cell, fp16, ~90 KB/cell)    │
  │     LRU by cell; miss = one 90 KB range read                        │
  │ refreshed by background timer (1 s) via CHANGELOG; horizon ⇒ reload │
  └──────────────┬─────────────────────────────────────────────────────┘
                 │ route: scan L1 (3 MB) → probe w coarse cells → scan
                 │ their L2 (~w×60 codes) → k_c nearest fine centroids
                 ▼ k_c parallel GetRange (one round-trip burst)
  FDB  POSTINGS/(fineID, pk) → RaBitQ residual code     ← inserts append
       POSTINGS/(fineID, HDR) → FORWARD(children) after a split
       CENTROIDS/(cellID, fineID) → fp32 vector + state + epoch
       SIDECAR/(pk) → fp16 vector        MEMBERSHIP/(pk) → [fineID...]
       CHANGELOG/(versionstamp) → delta  COUNTERS/(fineID) → size (ADD)
       TASKS/(kind, fineID) → {owner, lease, childIDs}   META/ → config…
```

- **Search** = L1+L2 scan (CPU, **< 0.5 ms** — §4) → k_c parallel posting reads
  (1 RT) → RaBitQ residual distances → re-rank top-C from the fp16 sidecar (1 RT).
  **3 network round trips** happy path (GRV + postings + re-rank); worst-case
  additions itemized in §4.
- **Insert** = route on cache → **real reads of the target fine centroids' state
  rows** (the deliberate conflict range) → blind-write posting/sidecar/membership
  keys + counter ADDs.
- **Rebalance** = SEAL→SPLIT→FORWARD (§6), single-tx splits, posting-header
  forwarding for stale caches, deterministic task keys, lease-recoverable —
  including when executed inline by a foreground writer.

### Honest conflict claims

| Pair | Outcome |
|---|---|
| insert(pk₁) vs insert(pk₂) | **never conflict** (disjoint writes; state reads are read-only on rows nobody is writing) |
| insert vs insert, same pk | serialize on `MEMBERSHIP/pk` — semantically required |
| insert vs seal/split of a target centroid | the later one retries; a straggler insert aborts on its state read and re-routes |
| update/delete vs split moving the same pk | serialize on `MEMBERSHIP/pk`; the split's **real** posting read (§6) makes the resolver abort whichever is later; retry sees truth |
| query vs anything | snapshot reads — never conflicts |

Foreground writes never conflict with each other (except same-pk); the conflict
surface with maintenance is one small state row per assigned centroid, paid only
while that centroid is actually splitting.

## 3. Key layout

All under the index's own subspace `S` (new index type ⇒ no Java collision by
construction). Grouping prefixes compose in front as for the HNSW index.

| Subspace | Key | Value | Notes |
|---|---|---|---|
| `S/0` CENTROIDS | `(cellID, fineID)` | `Tuple{fp32 vector, state, epoch, [childA, childB]}` | a cell's fine centroids are ONE range read (~90 KB); state: ACTIVE / SEALED / FORWARD / DEAD |
| `S/0'` COARSE | `(cellID)` | `Tuple{fp32 vector}` | ~2k rows; fixed between retrain epochs |
| `S/1` POSTINGS | `(fineID, pk)` | `Tuple{rabitqResidualCode}` | range `(fineID, *)` is the posting |
| | `(fineID, HDR)` | `FORWARD{childIDs}` | **posting header**: written by split/merge cutover; HDR sorts before any pk (reserved first element); absent on ACTIVE postings |
| `S/2` MEMBERSHIP | `(pk)` | `Tuple{fineID...}` | authoritative copy-set |
| `S/3` COUNTERS | `(fineID)` | int64 LE | atomic ADD; advisory (reconciled at split/merge) |
| `S/4` CHANGELOG | `(versionstamp+uv)` | `Tuple{op, ids…}` | ordered deltas; distinct 2-byte user-versions per tx |
| `S/5` TASKS | `(kind, fineID)` | `Tuple{owner, leaseDeadline, [childIDs]}` | **deterministic key** — duplicate triggers are idempotent `Set`s, no tail-shard queue flood (codex r2 #4); lease-expired claims reclaimable |
| `S/6` META | `(key)` | misc | config echo, **ID block allocator**, changelog GC horizon, RaBitQ transform, build/retrain epoch |
| `S/7` SIDECAR | `(pk)` | fp16 vector | re-rank source |

**Centroid IDs:** allocated in **blocks of 2¹⁶** per claimer from a META key (real
RMW once per 65k IDs) — removes the per-split single-key serialization (FDB-author
r2 #4: ~150–300 RMW/s ceiling vs every split contending). IDs never reused. Fine
centroids stay in their coarse cell for life; coarse cells change only at retrain.

**Sizing at 10M × 768D** (RaBitQ 1 ex-bit ≈ 192 B code, ~220 B/entry, closure r=2 ⇒
20M entries): `Lmax = 256` ⇒ posting ≤ ~56 KB ⇒ one reply. Avg fill ~170 ⇒
**~118k fine centroids across ~2k cells (~60/cell)**. Client cache: L1 = 3 MB
resident; L2 full residency = ~180 MB, but LRU by cell with graceful misses
(90 KB range read per missed cell) — the rev-2 "fallback = scan all of CENTROIDS"
was dead on arrival (180 MB *per query*; Torvalds r2 #3). Multi-tenant budget
(default 1 GB) evicts L2 cells only; L1 is pinned (3 MB/tenant). POSTINGS ≈ 4.4 GB;
SIDECAR ≈ 15 GB (sidecar write amp per insert: one 1.5 KB row + r×192 B codes).

## 4. Query path

```
budget: 3 network RTs happy path; p50 target 9–12 ms @10M (validate 094.1/094.5)

bg   1 s timer: GetRange(CHANGELOG from cachedVersion) + META horizon read;
     cachedVersion < horizon ⇒ full reload. Queries never pay cache-maintenance RTs.

RT0  GRV.
CPU  L1 scan: 2k × 768D fp16 ≈ 3 MB ≈ ~100 µs → w nearest cells (w=16 default).
     L2 scan: w × ~60 fine codes ≈ 960 distances ≈ < 100 µs → k_c nearest ACTIVE
     fine centroids (k_c=96 default, adaptive to 192 under ε-pruning starvation).
     (Rev 2's flat 118k-centroid scan was ~181 MB of memory traffic ≈ 2–9 ms —
     the "sub-ms" claim was false; two-level routing restores it. LanceDB r2 #1.)
     L2 cell miss: one 90 KB range read, amortized by LRU.
RT1  k_c parallel GetRange(POSTINGS/(fineID,*)), snapshot, Limit = 2×Lmax rows
     (fetch cap: an unmaintained oversized posting degrades THIS query boundedly
     and emits a metric).
     A fetched posting whose HDR says FORWARD (stale cache: split committed after
     our last refresh): point-read the children's CENTROIDS rows + fetch their
     postings — bounded +2 RT, only during the ≤1 s staleness window after a split
     of a probed centroid. Without the header, a stale client would read an EMPTY
     cleared range and silently lose that whole posting's vectors until refresh
     (codex r2 #1).
CPU  RaBitQ residual distances vs (q − c_fine); top-C heap (C=400 default);
     replica dedup keeps the MIN estimate per pk.
RT2  parallel SIDECAR point reads for top-C (1.5 KB × 400 ≈ 600 KB) → exact fp16
     re-rank → top-k. Sidecar disabled ⇒ read source records (12 KB each).
```

Worst-case additions (itemized; Torvalds r2 #2): +1 RT per L2 cell miss,
+2 RT per forwarded posting encountered, and the insert path's inline split
(§5) is an unbounded-tail maintenance event — metered, not hidden.

**Filtered search** (specced in 094.1; constrains layout): predicate evaluates as a
pk-set/bitmap filter on the streamed (pk, code) pairs *before* the top-C heap; k_c
widens adaptively when selectivity starves the heap (same loop). **Small-set
crossover** (LanceDB r2 #4): when the filter's candidate set is ≤ ~2k pks, skip
routing entirely — fetch their SIDECAR rows directly and scan exactly (≤ 3 MB, 1 RT).
Same executor, two entry conditions, no second index path.

`EXPLAIN`: scan type `VectorSPFreshIndexScan`; top-k result-set continuation, same
contract as the HNSW scan today.

## 5. Write path

**Insert(pk, v):**
1. Route on the cache (closure RNG rule: keep c_i of the r nearest fine centroids
   iff `dist(v,c_i) < α·dist(v,c_1)`; r=2, α=1.0 default).
2. One transaction:
   - **real reads** of `CENTROIDS/(cell, c_i)` — must be ACTIVE; SEALED/FORWARD ⇒
     drop c_i, re-read next-nearest (each re-route ≤1 extra read, same RT burst on
     retry). This read is the lifecycle fence (§6): the resolver aborts a straggler
     whose target was sealed after its GRV.
   - read `MEMBERSHIP/pk` (real). Existing row ⇒ update: **the keys to clear are
     derived from this same-tx membership read, never from routing-time cache**
     (FDB-author r2 #2a) — clear old posting keys + counter −1s.
   - blind-write `POSTINGS/(c_i, pk)`, `SIDECAR/pk`, `MEMBERSHIP/pk`,
     `Add(COUNTERS/c_i, +1)`.
   - **sampled trigger probe** (1/8): snapshot-read one counter; > Lmax ⇒
     `Set(TASKS/(split, c_i), unclaimed)` — deterministic key, idempotent under
     backlog (codex r2 #4). Sampling overshoot is geometric (p99 ≈ +37 entries —
     LanceDB r2); the **hard ceiling 4×Lmax** ⇒ the writer runs the split inline:
     **enqueue + claim the same TASKS row, then execute the identical §6 lifecycle
     synchronously** — same code path, lease-recoverable if the writer dies
     mid-lifecycle (rev 2's inline path bypassed TASKS and could wedge a centroid
     SEALED forever; Torvalds r2 #1, FDB-author r2 #1).

Happy path: GRV + one parallel read burst (state rows ∥ membership ∥ sampled
counter) + commit ≈ **3 RTs** ⇒ ~300–500 inserts/s single writer unbatched;
batching (record-layer batch writes, ~200/tx) ⇒ ~10k+/s per writer; N writers
scale linearly (no shared foreground keys).

**Delete(pk):** membership read → clear posting keys (from that read) + sidecar +
membership; counter −1s; sampled merge probe (deterministic task key). Precise
point deletes; no tombstones.

## 6. LIRE maintenance: SEAL → SPLIT → FORWARD

Rebalancer: in-process on writers by default (maintainer-owned goroutine), optional
dedicated runner; instances coexist (claims serialize). Claim = tx write of
{owner, leaseDeadline} into the deterministic TASKS row; expired leases reclaimable.

**Split(c)** — two transactions, both idempotent, total work bounded by 4×Lmax
entries (~225 KB read / ~450 KB written) — **must fit one tx each; there is no
chunked variant** (rev 2's chunking created a half-moved-membership window — codex
r2 #2; at these sizes chunking solves nothing, so it is forbidden by spec, enforced
by config validation: `Lmax × maxEntryBytes` bounded ≪ tx limits):

1. **SEAL** (tiny tx): read claim (own lease); read `CENTROIDS/(cell,c)` = ACTIVE;
   take child IDs from the claimer's ID block; write SEALED + childIDs into the
   task row. `commit_unknown` retry: SEALED + own task row carrying these IDs ⇒
   proceed. Fencing is sound both directions (FDB-author r2 walked it: a straggler
   insert's real state read aborts at the resolver against the seal's write; an
   insert that commits first is simply included in the frozen posting, which the
   split reads *after* the seal).
2. **SPLIT** (one tx): read `CENTROIDS/(cell,c)` = SEALED with these childIDs
   (idempotency guard); **range-read the posting as a REAL read** — explicitly NOT
   a snapshot read: sealing froze *appends*, but a concurrent update/delete still
   clears parent keys; the real read range is what makes the resolver abort this
   split so its retry sees the post-update truth. A snapshot read here would
   re-copy a deleted/moved entry into a child — rev 1's lost-update reborn
   (FDB-author r2 #2). Then: 2-means client-side; write children (ACTIVE, **exact**
   counters — mandatory reconciliation); write child POSTINGS; rewrite each moved
   pk's MEMBERSHIP **in this same tx**; clear the parent posting range; write
   `POSTINGS/(c, HDR) = FORWARD(children)` (the stale-cache visibility marker —
   §4); flip `CENTROIDS/(cell,c)` → FORWARD; CHANGELOG deltas (distinct
   user-versions); clear the TASKS row. `commit_unknown` retry reads state =
   FORWARD ⇒ no-op.
3. **NPA reassignment** (follow-up tasks): for the **K_n = 8** nearest fine
   centroids of old c (config-immutable), re-evaluate members' nearest-centroid
   sets under the children; move changed pks (posting keys + membership atomically
   per pk, batched ~64/tx). Expected write amplification per split at r=2:
   8 × 170 = 1,360 re-evaluated, ~2–5 % move ⇒ ~30–140 entry+membership writes
   ≈ 10–30 KB over 1–2 txs (LanceDB r2 #2). Serializes with foreground same-pk ops
   via membership keys.

**Merge(c)** at sampled counter < Lmin (= Lmax/8, config-immutable): same lifecycle;
the drain is ≤ Lmin entries ⇒ trivially one tx; parent gets the HDR FORWARD
(targets) marker. **Post-split merge cooldown T_cool = 10 min, config-immutable**
(LanceDB r2 #5) prevents split→NPA-drain→merge thrash.

**GC:** FORWARD/DEAD centroids past the changelog horizon: range-read the posting —
expect HDR only; **any residual entry is drained to children via membership
re-check, never blind-cleared** (invariant + metric + chaos `Verify()` item);
then purge centroid row, HDR, changelog prefix.

The rebalancer remains optional for stored-data correctness; boundedness without it
comes from the inline 4×Lmax ceiling and the query fetch cap.

## 7. RaBitQ integration

Global transform (rotator seed + rotated centroid) in META — same encoding as HNSW's
AccessInfo; established exactly at build (true mean); from-zero incremental uses the
in-tree SAMPLES bootstrap. Posting codes quantize the residual `v − c_fine`; scoring
forms `q − c_fine` from the cached fp16 fine vector (full vectors are *required*
client-side — a quantized centroid cannot produce residuals; codex r1 P2). One
global rotation, data-independent. Closure replicas carry different residuals ⇒
dedup keeps the min estimate; re-rank decides exactly. Re-rank reads the fp16
SIDECAR; disabling it (option) falls back to source records.

## 8. Build path (bulk): two-level clustering, no LIRE dependency

Rev 2 built 2k coarse postings and *split its way* to ~118k centroids: ~116k seal
txs through the allocator, ~22 GB of churn, and a phase plan where 094.1 couldn't
build the index it benchmarks (Torvalds r2 #4/#5, codex r2 #3, LanceDB r2 #3).
Rev 3 computes the final shape up front:

1. **Sample pass:** reservoir-sample 256k vectors.
2. **Coarse k-means:** K₀ = 2,048 on the sample (125 samples/centroid ✓). Write
   COARSE rows + the RaBitQ transform.
3. **Coarse assignment pass (OnlineIndexer batches):** stream records; write each
   vector's RaBitQ code into its coarse cell's *staging* posting (build-private
   prefix), counting cells.
4. **Per-cell fine clustering:** for each cell (parallel, independent): range-read
   the staging posting (~5k entries ≈ 1.1 MB), k-means to `pop/avgFill` fine
   centroids (~60/cell, **~86 full members per fine centroid** — the training-set
   floor is met with the cell's full population, not a resample; rev 1's 4.3/centroid
   failure mode does not move down a level), write fine CENTROIDS + final POSTINGS
   (closure-assigned within the cell + RNG rule across neighbor cells' centroids) +
   MEMBERSHIP + exact COUNTERS; clear staging. ~3–6 txs per cell × 2k cells.
5. Flip `Readable`.

**Derived cost, 10M × 768D:** coarse k-means ≈ 6×10¹² MACs ≈ ~1 min (8 cores);
coarse assignment ≈ 1.5×10¹³ MACs ≈ ~3 min + ~50k batch txs ≈ ~20–40 min at 8-way;
per-cell clustering ≈ 10M × 60 × 768 ≈ 4.6×10¹¹ MACs ≈ seconds-per-cell, I/O
≈ read 4.4 GB staging + write 4.4 GB final + 15 GB sidecar (written in pass 3)
≈ ~30 min at 8-way. **Total ≈ 1–1.5 h wall, I/O-bound; zero splits.** Each vector's
index data is written twice (staging, final) ≈ 8.8 GB — vs rev 2's 22 GB cascade.
HNSW extrapolates to ~12 days.

**Cold start** (no bulk build): the index begins as one coarse cell containing one
fine centroid (the first vector) and grows purely by §6 splits — the same machinery,
exercised at small N where its cost is trivial. The bulk path is the optimization
for the case where N is known up front; both produce the same on-disk shape.

## 9. Performance targets (derivation per row; validate in 094.1/094.5)

| Operation | HNSW (measured) | SPFresh target | Derivation |
|---|---|---|---|
| Insert, 1 writer, unbatched | 35–70 vec/s | ~300–500 vec/s | 1 tx ≈ 2–3 ms |
| Insert, batched 200/tx | n/a | ~10k+ vec/s/writer | 50 tx/s × 200 |
| Insert scaling, N writers | ~flat (lock) | ~linear | no shared foreground keys |
| Routing CPU / query | n/a | < 0.5 ms | 3 MB L1 + ~960 L2 distances (§4) |
| Search p50 @ 1M | 25–73 ms | < 8 ms | 3 RT + ~1.5 MB RT1 + 600 KB RT2 |
| Search p50 @ 10M | n/a | 9–12 ms | k_c=96→192 adaptive; C=400 |
| Recall@10, SIFT-1M | ~0.95 (ef=64) | ≥ 0.95 tuned | re-rank ⇒ only in-posting loss |
| Recall@10, 768D real embeddings | n/a | 0.92–0.95 **TBV in 094.1** (DBpedia-OpenAI / Cohere-wiki) | closure r=2 + re-rank beat the bare √nlist heuristic; SPANN curves are from a different heads regime — measured, not asserted |
| Bulk build 10M | ~12 days (extrapolated) | 1–1.5 h | §8 derivation |

## 10. Wire format & Java story

- New index type `IndexTypeVectorSPFresh = "vector_spfresh"` — Go-only extension
  per the project charter (read-side capability Java lacks; records stay fully
  Java-readable; index writes only under its own subspace). Java apps sharing
  *metadata* with this index type fail maintainer lookup as for any unknown type;
  documented in godoc.
- All structural options immutable via the evolution validator from day one
  (`spfreshLmax`, `spfreshLminRatio`, `spfreshReplication`, `spfreshAlpha`,
  `spfreshKn`, `spfreshCooldownSec`, dims, metric, RaBitQ bits, sidecar) — the
  PR #278 lesson: immutability makes config-derived invariants sound. Runtime
  knobs (w, k_c, ε, C, refresh interval, pacing) are not stored.
- Accepted hotspots, documented: CHANGELOG versionstamped tail-shard writes
  (split/merge-rate only); TASKS is deterministic-keyed (no queue flood).

## 11. Observability

Maintainer-stats metrics: posting size p50/p99; counter drift at reconciliation;
centroid count + state distribution; L2 cell-miss rate; cache staleness + full
reloads; task age + lease takeovers; seal-conflict rate (foreground retries from
maintenance); HDR-forward encounters per query; fetch-cap overage; re-rank
candidate count; inline-split occurrences (should be ~0 with a live rebalancer);
**online recall sampler** (background brute-force over a sampled query set).
Permanent distribution drift ⇒ **retrain**: build a new generation under a fresh
META epoch with §8 and atomically swap (094.5).

## 12. Testing plan (no mocks, real FDB, t.Parallel)

1. **Unit/fuzz:** codecs (posting incl. HDR, sidecar, membership, changelog,
   task rows) — 0 panics / 200k execs; RNG-rule; seeded k-means; ID block
   allocator.
2. **Integration e2e:** insert/search/delete/update; closure dedup (min-estimate);
   stale-cache FORWARD via posting HDR (assert the +2 RT path returns the moved
   vectors — the codex r2 #1 blackout, pinned); horizon overrun ⇒ full reload;
   filtered search incl. small-set crossover; cold start 1 → 100k by splits.
3. **Lifecycle interleavings, each a deterministic pinned test:**
   straggler-insert-vs-SEAL (aborts, re-routes — the rev-1 lost-vector
   interleaving); insert-commits-before-seal (split sees it); **update/delete
   racing SPLIT** (real posting read ⇒ split retries; assert no stale child copy —
   FDB-author r2 #2); split `commit_unknown` no-op retry; seal `commit_unknown`
   resume; **inline split: writer killed between SEAL and SPLIT ⇒ lease expiry ⇒
   rebalancer completes it** (the rev-2 wedge, pinned); GC drain-not-clear;
   duplicate task Sets collapse on one key.
4. **Concurrency stress:** N writers — assert conflict *metrics*: zero
   insert-vs-insert 1020s; insert-vs-split conflicts bounded by split count;
   rebalancer concurrent with writers + readers.
5. **Chaos:** `StoreModel` invariant {membership ⇔ posting keys ⇔ sidecar;
   counters within declared drift; SEALED/FORWARD/HDR consistent with children};
   faults across seal/split/GC/inline-split.
6. **Recall/perf:** SIFT-1M and DBpedia-OpenAI/Cohere-wiki vs brute force; A/B vs
   HNSW; 1M stress-table entry; 10M churn soak (1–5 %/day + hotspot burst, flat
   recall; SPFresh's headline scenario).

## 13. Phases (one PR each; e2e-proven; internally consistent — every phase
benchmarks the configuration it actually builds)

1. **094.1** Layout + codecs + **full two-level bulk build (§8)** + query path
   (two-level routing, filtered search, small-set crossover) + real-embedding
   recall benchmark. Read-only after build, stated honestly.
2. **094.2** Foreground writes (state fencing, sidecar, counters, deterministic
   task enqueue) + N-writer conflict-metrics stress + update/delete-vs-split
   pinned interleavings (split machinery stubbed to manual trigger).
3. **094.3** Rebalancer: SEAL/SPLIT/FORWARD + HDR cutover + NPA + merge +
   cooldown + GC-drain + lease recovery + inline-split + chaos + the full pinned
   interleaving suite + cold-start-by-splitting e2e.
4. **094.4** RaBitQ residual tuning, re-rank/C and w/k_c tuning, sidecar A/B.
5. **094.5** 10M churn soak, online recall sampler, retrain/swap, defaults freeze,
   `VECTOR_BENCHMARK_RESULTS.md` update.

## 14. Alternatives considered

- **DiskANN/Vamana on FDB:** round-trip depth still O(path); shared adjacency
  writes. Rejected for this substrate.
- **Batched beam search over HNSW:** real wire-neutral query win; tracked
  separately; no write-scalability help.
- **IVF-Flat without LIRE:** degrades under churn/boundaries; rebalancing is cheap
  here and is what makes this production-grade.
- **Flat (single-level) routing:** rev 2's design; dies at ~25–30k centroids
  (~2.5M vectors) when the table scan exceeds ~1 ms and the cache exceeds tenant
  budgets. Two-level is mandatory at target scale.
- **Brute force + RaBitQ:** optimal ≤ ~100k vectors; this design's cold-start
  shape (one cell, big posting) *is* that, on the same code path; the filtered
  small-set crossover (§4) is its query-time twin.

## 15. References

- SPANN (Chen et al., NeurIPS 2021) — centroid/posting architecture, closure
  assignment, ε-pruning, hierarchical balanced clustering.
- SPFresh (Xu et al., SOSP 2023) — LIRE: split/merge/NPA; recall-under-churn.
- RaBitQ (Gao & Long, SIGMOD 2024) — `pkg/rabitq`; residual fit (§7).
- FDB 7.3.75: resolver `fdbserver/SkipList.cpp:983`; `REPLY_BYTE_LIMIT`
  `fdbclient/ClientKnobs.cpp:66`; RYW dependent-write `fdbclient/RYWIterator.cpp:34`;
  versionstamp user-versions `fdbclient/ReadYourWrites.actor.cpp:2252`.
- `pkg/recordlayer/VECTOR_BENCHMARK_RESULTS.md` — measured HNSW numbers (§1).
- PR #279 — rev 1/rev 2 verdicts (FDB C++ author, Torvalds, LanceDB founder,
  codex) and dispositions.
