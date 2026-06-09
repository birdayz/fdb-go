# RFC-094 — SPFresh: an FDB-native vector index (two-level centroid routing + posting lists, incremental rebalancing)

**Status:** Rev 5. Review history: rev 1 NAK'd 4/4 (FDB C++ author, Torvalds,
LanceDB founder, codex); rev 2 closed the lost-vector hole (SEAL lifecycle);
rev 3 added two-level routing/build, single-tx splits, HDR cutover; rev 4 added
coarse splits, the build state machine, and the α/tail fixes — round 4 returned
Torvalds ACK (editorial conditions), LanceDB ACK (non-blocking), FDB author
NAK-narrow (two confirmed races), codex 6 findings. Rev 5 incorporates all of
round 4: the trigger-probe real read + the SEALED-resumes zombie restriction, the
wave-B straggler fence direction fix, the generation-prefixed layout (abort GC +
retrain swap), HDR cell-qualified children + refresh-on-absent, counter kind
tags, raw vector encoding, the coarse-split starvation guard, and the corrected
build formulas/I/O. Verdicts and dispositions on PR #279.

**Scope:** A second, Go-only vector index type built *for* FoundationDB's
performance model. **Design target: consistently good performance across 1M–10M
vectors, regardless of whether the index was bulk-built or grown incrementally
from zero**, with linear writer scalability. Architecture: SPANN centroid +
posting-list layout with two-level routing, SPFresh LIRE incremental rebalancing
(now at both levels), RaBitQ residual quantization (in-tree, `pkg/rabitq`). The
existing HNSW index (`IndexTypeVector`) is unchanged and remains the
Java-wire-compatible option.

**TODO.md anchor:** "Exploration: a second, FDB-native vector index
(SPFresh/DiskANN/SPANN/beam)". Batched beam search (wire-neutral HNSW query
improvement) is complementary and out of scope.

---

## 1. Problem

The HNSW index is now 100% Java-compliant (PR #278) — and that is exactly why it
cannot reach the 1M–10M / many-concurrent-writers regime on FDB. Measured
(SIFT-128D unless noted; testcontainer FDB, ~0.3–0.5 ms per round trip):

| Metric | HNSW on FDB (measured) | Why |
|---|---|---|
| Build / insert | ~35–70 vec/sec, *degrading* with graph size | O(layers × hops) **dependent** point reads per insert; reverse-edge writes mutate shared hub nodes |
| Search p50 | 25–73 ms | sequential traversal — round-trip *depth* |
| Concurrent writers | serialized | per-prefix write lock (Java parity); without it, FDB-1020 storms |
| Gap vs disk-backed ANN | ~16× QPS vs Qdrant after all wire-neutral wins | architectural |

**HNSW's unit of work is a pointer chase; FDB's unit of efficiency is a range read.**

### What a high-latency networked KV store is actually good at

| FDB property | Design consequence |
|---|---|
| ~0.3–2 ms per round trip | minimize round-trip **depth**; dependent reads are the enemy |
| range reads stream; futures pipeline | fetch *wide*: parallel range bursts |
| `REPLY_BYTE_LIMIT` = 80 KB per range reply (ClientKnobs.cpp:66; caps `limitBytes`, NativeAPI.actor.cpp:4226) | size postings AND routing cells to fit one reply |
| only the *committer's read ranges* are checked (SkipList.cpp:983) | blind writes never abort; conflict ranges are taken **deliberately**, on the side that must lose |
| atomic ADD / versionstamps are conflict-free | counters/logs come free; ADD is **not** fetch-and-add |
| 5 s / 10 MB tx, 100 KB values | every maintenance unit fits ONE tx (enforced) |
| no server-side compute | routing state must be client-cacheable and its **CPU budgeted like a round trip** |

## 2. Architecture overview

```
                     CLIENT (stateless; per-process two-level cache)
  ┌────────────────────────────────────────────────────────────────────┐
  │ L1: coarse cells (~2.5k @10M, fp16, ~3.8 MB) — always resident      │
  │ L2: fine centroids per cell (~48/cell, fp16, ≤77 KB/cell) — LRU;    │
  │     miss = one range read (one reply at target fill; ≤2 near cellMax)│
  │ background refresh timer (1 s) via CHANGELOG; horizon ⇒ full reload │
  └──────────────┬─────────────────────────────────────────────────────┘
                 │ route: scan L1 → probe w=32 cells → ~1.5k fine
                 │ distances → k_c nearest fine centroids
                 ▼ k_c parallel GetRange (one burst)
  FDB  POSTINGS/(fineID, pk) → RaBitQ residual code     ← inserts append
       POSTINGS/(fineID, HDR) → FORWARD(children)        after fine split
       CENTROIDS/(cellID, fineID) → fp16 vector + state + epoch
       CENTROIDS/(cellID, HDR) → FORWARD(cells)          after coarse split
       COARSE/(cellID) → fp16 vector + state             SIDECAR/(pk) → fp16
       MEMBERSHIP/(pk) → [fineID...]                     COUNTERS/(kind,id) → ADD
       CHANGELOG/(vs) → delta      TASKS/(kind, id) → {owner, lease, …}
       STAGING/(cellID, pk) → fp16 vector (build-only)   META/ → config…
```

- **Search** = L1+L2 scan (< 0.5 ms CPU) → k_c parallel posting reads (1 burst)
  → RaBitQ residual distances → re-rank top-C from SIDECAR (1 burst). **3 network
  round trips happy path**; worst-case additions itemized in §4; tail math in §9.
- **Insert** = route on cache → **real reads of target fine centroids' state
  rows** (the deliberate conflict fence) → blind writes + counter ADDs.
- **Rebalance** = SEAL→SPLIT→FORWARD at the fine level (§6) **and a metadata-only
  split at the coarse level** (§6b) — postings are keyed by `fineID` alone, so
  restructuring cells moves *no posting data*. This is what makes "grown from
  zero" converge to the same shape as "bulk built" (§8), keeping routing flat
  across 1M–10M:

| Scale | fine centroids | cells | L1 scan | L2 probed | route CPU |
|---|---|---|---|---|---|
| 1M | ~12k | ~250 | 0.4 MB | ~1.5k codes | < 0.3 ms |
| 10M | ~118k | ~2.5k | 3.8 MB | ~1.5k codes | < 0.5 ms |

### Honest conflict claims

| Pair | Outcome |
|---|---|
| insert(pk₁) vs insert(pk₂) | **never conflict** (disjoint writes; state reads are read-only on rows nobody is writing) |
| insert vs insert, same pk | serialize on `MEMBERSHIP/pk` — semantically required |
| insert vs seal/split of a target fine centroid | the later one retries; a straggler insert aborts on its state read and re-routes |
| insert vs coarse split of a routed cell | no fence needed: fineIDs are stable, postings unaffected; a state read against a moved row sees *absent* ⇒ re-route (conservative, correct) |
| update/delete vs split moving the same pk | serialize on `MEMBERSHIP/pk` + the split's **real** posting read; later one retries against truth |
| query vs anything | snapshot reads — never conflicts |

### 2.1 FDB-native leverage (the global commit version, exploited)

FDB's sequencer hands every transaction a globally ordered version — the read
version at GRV, capturable as versionstamps on commit. The design exploits it
four ways; the first two are load-bearing above, the rest are optimizations
slotted for 094.4 (optional — none affect the correctness arguments):

1. **One consistent snapshot per query** (load-bearing): all k_c posting reads +
   the re-rank burst execute at a single read version — a query can never see a
   torn split (half parent, half children). The HDR/FORWARD reasoning depends on
   this; loosely-coupled ANN stores cannot offer it.
2. **The changelog is an exact change feed** (load-bearing): "every topology
   delta since version V", no clock skew, no ETags — the entire cache protocol.
3. **`\xff/metadataVersion` piggyback** (094.4): this special key's value is
   returned to every client *with* the GRV — zero extra reads, zero conflict
   ranges, zero round trips. Bump it on **generation swaps and coarse splits**
   (rare, topology-level events): every query then learns "routing table
   changed" for free at RT0, shrinking worst-case staleness for exactly the
   events that most disturb routing. NOT for fine splits (~78/s at peak — the
   key is cluster-global and shared with every other layer; HDR markers already
   handle fine-split staleness correctly). The Java record layer uses the same
   key for metadata caching — same restraint applies.
4. **Watches** (094.4): a watch on the changelog-head key converts the 1 s
   refresh timer into server push — staleness drops toward ~ms (directly
   shrinking the ~6 % HDR-forward query tax at peak ingest, §4) and idle
   processes stop polling.

And one **product guarantee** that falls out of the global version rather than
costing anything: **strict read-after-write freshness**. An ACKed insert is
visible to every subsequent search, cluster-wide — postings are read live at the
query's read version; only *routing* is cached, and the state-read fence plus
HDR markers correct it. Disk-ANN systems built on refresh intervals
(Elasticsearch, SPANN-on-SSD pipelines) cannot make this promise; here it is
the default. (Speculative, explicitly deferred: per-posting modification
markers via `SET_VERSIONSTAMPED_VALUE` would enable client-side hot-posting
caching with exact invalidation — a 094.4 experiment only if query skew data
says it pays.)

## 3. Key layout

All under the index's own subspace `S` (new index type ⇒ no Java collision).
Grouping prefixes compose in front as for the HNSW index.

**Generation prefix:** every subspace below except META lives under
`S/(generation)`, an int chosen at build/retrain start and recorded in META;
the `Readable` flip atomically updates META's current generation. This is what
makes both stories clean: an **abandoned build** (never reached Readable) is
GC'd by range-clearing its entire generation — not just staging; rev 4 left
partial COARSE/CENTROIDS/POSTINGS/MEMBERSHIP/SIDECAR/COUNTERS rows visible to
the next build (codex r4 #5) — and a **retrain** (§11) builds generation g+1
and swaps; the superseded generation is range-cleared only after a **grace
period ≥ maxStaleness** (the 10-min horizon). Queries against a superseded g
degrade to partial-at-worst under MVCC and re-resolve at their next refresh.
Writers are fenced *at the flip*, not at the clear (during the grace window g's
rows are still ACTIVE, so state reads alone would pass and the mutation would be
invisible in g+1 and eventually GC'd — codex r5 #1): **every write tx real-reads
META's current-generation key and uses it BOTH ways** (codex r6 #1): as a
**value check** — the read value must equal the tx's cached generation, else
refresh and retry (covers a tx that *starts* after the flip: the flip already
committed, so no conflict will fire, but the value mismatch is visible) — and
as a **conflict fence** — the flip's write aborts every *in-flight* writer of g
at the resolver. One point read in the existing parallel burst, written only by
flip txs. Together the two cases are exhaustive: a write lands in g only if the
flip neither committed before its read nor during its window — i.e. never.
Belt-and-braces, a writer whose target rows are all absent forces a synchronous
refresh rather than blind-writing (FDB-author r5).

**Vector encoding:** vector fields in CENTROIDS/COARSE/STAGING/SIDECAR values
are stored as **raw fixed-width suffixes**, not tuple-escaped bytes elements —
tuple encoding escapes 0x00 (→ 0x00FF) and fp16 vectors are zero-heavy, which
would silently eat the one-reply byte budgets (FDB-author r4 #5).

| Subspace | Key | Value | Notes |
|---|---|---|---|
| `S/0` CENTROIDS | `(cellID, fineID)` | `Tuple{fp16 vector, state, epoch, [childA, childB]}` | **fp16 on disk** (fp32 rows made a cell ~184 KB = 3 replies; fp16 keeps encode/score consistent with the cache — codex r3 #4); state: ACTIVE / SEALED / FORWARD / DEAD |
| | `(cellID, HDR)` | `FORWARD{cellIDs}` | coarse-split marker for stale L2 fetchers |
| `S/0'` COARSE | `(cellID)` | `Tuple{fp16 vector, state}` | state: ACTIVE / FORWARD(cells) |
| `S/1` POSTINGS | `(fineID, pk)` | `Tuple{rabitqResidualCode}` | the range is the posting; **independent of cellID** — coarse restructure moves no data |
| | `(fineID, HDR)` | `FORWARD{cellID, childIDs}` | fine-split marker. **HDR = tuple null (0x00)**: sorts before every legal pk because nulls are rejected in primary-key components — the spec leans on that invariant explicitly (FDB-author r3 #1). HDR occupies one row of the fetch cap: `Limit = 2×Lmax + 1`. The payload carries the children's **cellID** — a stale client may have cached this fine centroid under a cell it has since left via a coarse split, so child keys must never be derived from the routed cell (codex r4 #3); if the cell-qualified point reads still return absent (deeper staleness), force a synchronous cache refresh and re-route |
| `S/2` MEMBERSHIP | `(pk)` | `Tuple{fineID...}` | authoritative copy-set |
| `S/3` COUNTERS | `(FINE, fineID)` / `(CELL, cellID)` | int64 LE | **kind-tagged** — fineIDs and cellIDs come from the same block allocator but the two counter families must never alias (codex r4 #6). Atomic ADD; **advisory** — reconciled exactly at split/merge; build writes via ADD (commutes across cross-cell writers; commit_unknown drift self-corrects at first reconciliation) |
| `S/4` CHANGELOG | `(versionstamp+uv)` | `Tuple{op, ids…}` | distinct 2-byte user-versions per tx. **Horizon advancement:** the rebalancer periodically sets META horizon = now − maxStaleness (default 10 min) and prunes older entries; GC of FORWARD markers keys off it |
| `S/5` TASKS | `(kind, id)` | `Tuple{owner, leaseDeadline, payload}` | deterministic keys; kinds: split, merge, csplit, cellfin (build). **Trigger probes take a REAL read of the row and `Set` only when absent** — rev 4's snapshot-read probe was a confirmed race: a claim committing between the probe's GRV and its commit was invisible and the blind Set clobbered owner/lease/childIDs; worst case it landed between SEAL and SPLIT, lost the childIDs, and the old zombie rule then deleted the task — a permanently SEALED centroid (FDB-author r4 #3, codex r4 #2). The real read costs one point conflict range on the rare over-threshold path only. **Zombie rule (restricted):** a claimer that finds its target FORWARD / DEAD / absent deletes the task row and no-ops; a **SEALED target is an in-flight split to RESUME** (childIDs are in the task row) — never deleted. Pinned tests for both |
| `S/6` META | `(key)` | misc | config echo, ID block allocator (2¹⁶/claim), horizon, RaBitQ transform, build state |
| `S/7` SIDECAR | `(pk)` | fp16 vector | re-rank source |
| `S/8` STAGING | `(cellID, pk)` | fp16 vector | **build-only**: cleared per cell at finalization; abandoned builds are handled by generation GC (the whole superseded generation is range-cleared — see the generation-prefix note above) |

**Centroid IDs:** block-allocated (2¹⁶ per claimer, one real RMW per block). IDs
never reused. Fine centroids keep their `fineID` for life; their *cell* can change
only via a coarse split (§6b).

**Sizing at 10M × 768D** (RaBitQ 1 ex-bit ≈ 192 B code, ~220 B/entry, closure
r≈2 ⇒ ~20M entries): `Lmax = 256` (posting ≤ ~56 KB = one reply), avg fill ~170 ⇒
~118k fine centroids; `cellTarget = 48` fine/cell (≤ 77 KB L2 read = one reply),
`cellMax = 96` ⇒ ~2.5k cells. L1 ≈ 3.8 MB pinned; full L2 residency ≈ 190 MB,
LRU by cell with one-reply misses. POSTINGS ≈ 4.4 GB; SIDECAR ≈ 15 GB.

## 4. Query path

```
budget: 3 network RTs happy path; targets §9 (p50 AND p99, derived)

bg   1 s timer: CHANGELOG delta + META horizon; cachedVersion < horizon ⇒ full
     reload. Queries never pay cache-maintenance RTs.

RT0  GRV.
CPU  L1 scan (2.5k × fp16) → w = 32 cells (rev 3's w=16 probed 0.78% of cells
     with no coarse-boundary mitigation; w=32 doubles L2 work to ~1.5k distances,
     still < 0.5 ms total, and halves coarse-boundary loss — LanceDB r3; sweep
     w ∈ {16,32,64} in 094.1, coarse-level closure only if 64 still leaks).
     L2 scan → k_c = 96 nearest ACTIVE fine centroids (adaptive → 192 under
     ε-pruning starvation). L2 cell miss: one range read, one reply (≤ 77 KB).
RT1  k_c parallel GetRange(POSTINGS/(fineID,*)), snapshot, Limit = 2×Lmax+1
     (fetch cap: an oversized posting degrades THIS query boundedly + metric).
     HDR FORWARD row (stale cache; split landed inside our refresh window):
     point-read the children's CENTROIDS rows **using the cellID carried in the
     HDR payload — never the cell the client routed through** (the parent may
     itself have moved cells via a coarse split since the client cached it;
     deriving from the routed cell silently dropped the posting — codex r4 #3) —
     then fetch their postings: +2 RT, bounded. Absent even at the HDR's cell, or
     chain depth > 1 (child split again within the window): forced synchronous
     cache refresh, then re-route — bounded at 2 hops by spec (Torvalds r3).
     At peak ingest (10k inserts/s ⇒ ~78 splits/s across ~118k centroids),
     P(query touches ≥1 forwarded posting) ≈ k_c·splits/s·window/nlist ≈ 6 %;
     0.6 % at 1k inserts/s; linear in the refresh interval (LanceDB r3 #3).
CPU  RaBitQ residual distances vs (q − c_fine); top-C heap (C = 400); replica
     dedup keeps the MIN estimate per pk.
RT2  parallel SIDECAR reads for top-C (≈ 600 KB) → exact fp16 re-rank → top-k.
```

Worst-case additions, itemized: +1 RT per L2 cell miss; +2 RT per forwarded
posting (≤ depth 2, then refresh); inline split (§5) is a metered maintenance
event, not hidden latency.

**Filtered search:** predicate evaluates as a pk filter on streamed (pk, code)
pairs before the top-C heap; k_c widens adaptively under selectivity starvation.
**Small-set crossover:** filter candidate set ≤ ~2k pks ⇒ skip routing, fetch
SIDECAR rows directly, exact scan (≤ 3 MB, 1 RT). Same executor.

`EXPLAIN`: `VectorSPFreshIndexScan`; top-k result-set continuation (same contract
as the HNSW scan).

## 5. Write path

**Insert(pk, v):**
1. Route on cache. Closure (SPANN): keep fine centroid c_i of the r nearest iff
   `dist(v, c_i) ≤ α · dist(v, c_1)`, **α = 1.2 default** — rev 3's α = 1.0
   admitted only c_1, silently making r = 1 and invalidating the sizing and
   recall math built on r ≈ 2 (LanceDB r3 #5 + codex r3 #1, found independently).
   r = 2 cap, config-immutable.
2. One transaction:
   - **real reads** of `CENTROIDS/(cell, c_i)` — ACTIVE required; SEALED/FORWARD
     **or absent** (the row moved in a coarse split — fineID still valid but we
     re-route conservatively) ⇒ drop c_i, take next-nearest.
   - read `MEMBERSHIP/pk` (real); existing ⇒ update: clear old keys **derived
     from this same-tx read**, counter −1s.
   - blind-write `POSTINGS/(c_i, pk)`, `SIDECAR/pk`, `MEMBERSHIP/pk`,
     `Add(COUNTERS/(FINE, c_i), +1)`.
   - sampled probe (1/8): snapshot-read counter; > Lmax ⇒ **REAL-read**
     `TASKS/(split, c_i)` and `Set` only if absent (the conflict range is the
     point of the read — §3 TASKS row). Hard ceiling 4×Lmax ⇒
     inline split: **after this insert tx commits**, the writer claims the same
     task row and runs the identical §6 lifecycle synchronously in its own
     transactions (claiming inside the insert tx would start the lease before
     durability and bloat the insert's conflict surface — FDB-author r3 #5);
     lease-recoverable if the writer dies mid-lifecycle.

Happy path ≈ 3 RTs ⇒ ~300–500 inserts/s unbatched per writer; ~10k+/s batched
(200/tx); N writers scale linearly.

**Delete(pk):** membership read → clear posting keys (from that read) + sidecar +
membership; counter −1s; sampled merge probe (Set-if-absent). No tombstones.

## 6. Fine-level LIRE: SEAL → SPLIT → FORWARD

Rebalancer: in-process on writers by default, optional dedicated runner; claims
serialize transactionally; leases expire and are reclaimable.

**Split(c)** — two single-tx steps (chunking is forbidden; `Lmax × maxEntryBytes`
is validated against tx limits; the 4×Lmax ceiling worst case is ~225 KB read /
~450 KB written):

1. **SEAL** (tiny): read claim; read `CENTROIDS/(cell, c)` = ACTIVE (FORWARD /
   DEAD ⇒ zombie rule: delete task, no-op; SEALED with own childIDs ⇒ resume at
   step 2; **absent at this cell** ⇒ the row moved in a coarse split — delete the
   task and let the next probe recreate it under the new cellID); child IDs from
   the claimer's block; write
   SEALED + childIDs into the task row. `commit_unknown` retry: SEALED + own
   task row carrying these IDs ⇒ proceed. Fencing sound both directions
   (FDB-author r2: straggler insert's real state read aborts at the resolver;
   an insert that commits first is in the frozen posting the split reads).
2. **SPLIT** (one tx): guard-read SEALED+children; **REAL-read the posting**
   (sealing froze appends; a concurrent update/delete still clears parent keys —
   the real read range makes the resolver abort this split so its retry sees
   truth; a snapshot read would resurrect a moved/deleted entry); 2-means;
   write children (ACTIVE, **exact** counters) **in the parent's cell**; child
   POSTINGS; rewrite moved pks' MEMBERSHIP in-tx; clear parent posting; write
   `POSTINGS/(c, HDR)`; flip centroid FORWARD; changelog; clear task row.
   `commit_unknown` retry: FORWARD ⇒ no-op.
3. **NPA reassignment** (follow-up tasks): K_n = 8 nearest fine centroids;
   ~10–30 KB of moves over 1–2 txs per split; per-pk atomic; serializes with
   foreground via membership keys.

**Merge(c)** at counter < Lmin (= Lmax/8): same lifecycle; drain ≤ Lmin entries =
one tx; HDR FORWARD(targets). Post-split cooldown T_cool = 10 min.

**GC:** FORWARD/DEAD past horizon: range-read posting — expect HDR only; any
residual entry is **drained via membership re-check, never blind-cleared**
(invariant + metric + chaos `Verify()`); purge centroid row, HDR, changelog.

## 6b. Coarse-level growth: metadata-only cell splits

The piece that makes incremental growth reach 10M with flat routing (rev 3's cold
start kept ONE coarse cell forever — at scale its L2 *was* rev 2's flat scan;
Torvalds r3 #2, LanceDB r3 #4, codex r3 #3, found independently).

Because POSTINGS and MEMBERSHIP are keyed by `fineID` alone, restructuring cells
moves **no posting data** — only ~cellMax small CENTROIDS rows (~150 KB).

**CoarseSplit(cell)**, trigger: fine-count counter (`COUNTERS/(CELL, cellID)`, ADD'd by
fine splits/merges) > cellMax = 96, probed by the fine-split tx (Set-if-absent
`TASKS/(csplit, cellID)`). One tx:
- read claim; read `COARSE/(cell)` = ACTIVE (zombie rule applies);
- **read all the cell's CENTROIDS rows (real read); require every fine centroid
  ACTIVE — defer (requeue with backoff) if any is SEALED/FORWARD.** This
  exclusion rule is what keeps the two lifecycles composable: a fine split holds
  its centroid SEALED across its SEAL→SPLIT window, so a coarse split can never
  relocate a row out from under a fine lifecycle's guard re-read; conversely the
  coarse split's real reads of every row mean a racing fine SEAL aborts one of
  the two at the resolver — whichever loses retries. No cross-lifecycle window.
- 2-means over the fine centroid *vectors*; allocate two cellIDs; write two
  COARSE rows (ACTIVE) — **their routing vectors are the fresh 2-means centers,
  i.e. the coarse vector is recomputed at every cell split by construction**
  (made explicit per LanceDB r4); fine-count counters; rewrite the fine
  CENTROIDS rows under their new cells (fineID, state, epoch preserved); write
  `CENTROIDS/(oldCell, HDR) = FORWARD(cells)` for stale L2 fetchers; flip
  `COARSE/(oldCell)` → FORWARD; changelog; clear task.
- Inserts need no fence (§2 table): fineIDs are stable; a state read against a
  moved row sees absent and re-routes.
- **Starvation guard** (LanceDB r4): the defer-on-SEALED rule could let a
  hotspot cell starve its own coarse split — continuous fine splits keep some
  centroid SEALED under exactly the load that needs the cell split. After
  `csplitDeferLimit = 8` consecutive deferrals, fine-split task issuance for
  that cell is paused (probes skip the Set) until the coarse split completes;
  metered.

Cold start therefore: one cell, one fine centroid (first vector); fine splits
grow centroids; coarse splits grow cells; the shape converges to the bulk-built
one. Retrain (§11) remains a *drift* tool only — growth never requires it.

## 7. RaBitQ integration

Global transform in META (same encoding as HNSW AccessInfo; exact mean at build;
SAMPLES bootstrap from zero). Posting codes quantize `v − c_fine`; scoring forms
`q − c_fine` from the cached fp16 fine vector. Encode and score use the **same
fp16 centroid representation** end-to-end (disk = cache = fp16), so residuals are
consistent by construction. Closure replicas carry different residuals ⇒ dedup
keeps the min estimate; re-rank decides exactly. Re-rank reads SIDECAR; disabling
it (option) falls back to source records.

## 8. Build path (bulk): two-level clustering with a real state machine

Rev 3's build had no recovery spec ("~3–6 txs per cell" with nobody tracking
cells — Torvalds r3 #1) and staged RaBitQ codes that cannot train k-means or
re-encode residuals (codex r3 #2). Rev 4:

1. **Sample pass:** reservoir-sample 256k vectors.
2. **Coarse k-means:** K₀ = **N·r / (avgFill · cellTarget)** ≈ 20M/(170·48) ≈
   2.45k @10M on the sample (≥ 100 samples/centroid ✓) — `avgFill` counts
   posting *entries* (replicated), `N` counts source vectors, so r must appear
   or the build mints half the cells and immediately violates Lmax (Torvalds
   r4 #2, codex r4 #1). Write COARSE rows + transform.
3. **Coarse assignment pass** (OnlineIndexer batches, resumable by record range):
   stream records; write `STAGING/(cellID, pk) = fp16(v)` — **full vectors**,
   because pass 4 must train k-means and re-encode residuals (lossy codes can't;
   staging is 15 GB, not rev 3's understated 4.4 GB) — plus `SIDECAR/pk`.
4. **Per-cell finalization — wave A (centroids):** one deterministic
   `TASKS/(cellfin, cellID)` row per cell, lease-claimed like any task. Per cell
   (pop ≈ N/K₀ ≈ 4.1k staged vectors): range-read staging (~4.1k × 1.6 KB ≈
   6.5 MB → multiple replies, fine — build path, off-query); k-means to
   **pop·r/avgFill ≈ 48** fine centroids (= cellTarget by construction;
   ~85 full members each);
   **fold sub-Lmin clusters into nearest siblings before writing** (or build
   completion dumps thousands of merge tasks — LanceDB r3 #2); write fine
   CENTROIDS rows only. Idempotent re-run: rewriting centroids for an
   unfinalized cell is harmless; the cellfin row advances to **CENTROIDS_DONE**
   — deliberately distinct from wave-B completion (codex r5 #2).
5. **Wave B (postings):** per cell, after wave A completes globally (closure
   assignment needs *all* fine centroids — cross-cell replicas would otherwise
   target centroids that don't exist yet; this two-wave order plus ADD-not-Set
   counters resolves the cross-cell counter race — codex r3 #6): assign each
   staged vector (closure across neighbor cells' centroids via the now-complete
   table); write final POSTINGS + MEMBERSHIP; `Add` counters; clear that cell's
   staging range in the same tx as its last batch, and write **FINALIZED** into
   its cellfin row — **the closing tx's real read covers the cell's ENTIRE
   staging range, and the tx processes every row that read returns which earlier
   batches did not cover** (the rows are already in hand from the read) before
   clearing; if the uncovered remainder exceeds the tx budget, commit *without*
   clearing and run another batch. Two distinct straggler cases, both closed: a
   straggler committing **during** the closing tx's window conflicts its read
   range and aborts/re-runs the finalizer (FDB-author r5); one committing
   **before** its read version is simply *visible data* — no conflict fires —
   and must be assigned, not cleared (codex r6 #2).
   **The fence direction lives on the straggler, not the finalizer** (rev 4 had
   it backwards — only the committer's reads are checked, so a finalizer-side
   read cannot stop a later blind staging write from landing in the cleared
   range: an orphaned vector, the rev-1 hole reborn in the build path;
   FDB-author r4 #4): a foreground insert staging into a not-yet-finalized cell
   takes a **REAL read of that cell's cellfin row** and routes by its state —
   **only FINALIZED routes to the live path**; absent or CENTROIDS_DONE keeps
   staging (between the waves the cell's centroids exist but its postings do
   not; rev 5's single "DONE" conflated the two and lost the straggler's vector
   — codex r5 #2). A FINALIZED transition mid-flight aborts the straggler at
   the resolver, whose retry routes live. The finalizer's own staging range
   read still serializes it against stragglers that committed first.
6. Flip `Readable` (atomically updating META's current generation). Abandoned
   builds: the entire superseded generation is range-cleared (§3) — staging,
   partial centroids, postings, everything.

**Build/foreground interleaving (declared — FDB-author r3 #3):** in 094.1 the
build runs with the index in `WriteOnly` but the *application contract* is
build-then-write (no foreground vector writes until Readable; enforced by the
maintainer rejecting writes pre-Readable for this index type in 094.1). From
094.2 on, foreground inserts during a build write to STAGING for unfinalized
cells (coarse-routed; no fine centroids needed) and to the live path for
finalized ones; wave B's real staging read serializes stragglers exactly like a
fine split serializes appends.

**Derived cost, 10M × 768D:** coarse k-means ≈ 1 min; assignment pass ≈ 3 min
CPU + ~50k batch txs writing 15 GB staging + 15 GB sidecar ≈ 40–60 min at 8-way;
wave A reads the 15 GB staging once (2.45k cells × ~6.5 MB) + writes ~190 MB of
centroids ≈ 15–25 min; wave B reads staging again (15 GB) + writes 4.4 GB
postings ≈ 20–30 min. **Total ≈ 1.5–2.5 h wall, I/O-bound; ~65 GB total I/O**
(30 GB written, 30 GB re-read, postings/centroids — each term above, summed
honestly per Torvalds r4 #3). **Transient disk high-water ≈ 35 GB** (staging +
sidecar coexist until wave B clears staging — LanceDB r4). HNSW extrapolates to
~12 days.

## 9. Performance targets (derived; validate in 094.1/094.5)

| Operation | HNSW (measured) | SPFresh target | Derivation |
|---|---|---|---|
| Insert, 1 writer, unbatched | 35–70 vec/s | ~300–500 vec/s | 1 tx ≈ 2–3 ms |
| Insert, batched 200/tx | n/a | ~10k+ vec/s/writer | 50 tx/s × 200 |
| Insert scaling, N writers | ~flat (lock) | ~linear | no shared foreground keys |
| Routing CPU / query | n/a | < 0.5 ms | 3.8 MB L1 + ~1.5k L2 distances |
| Search p50 @ 1M / 10M | 25–73 ms / n/a | < 8 ms / 9–12 ms | GRV ~0.5 + RT1 burst ~2–3 + RT2 burst ~1.5–2 + CPU ~1–2 |
| **Search p99 @ 10M** | n/a | **≤ 40 ms under load (TBV)** | RT1 completes at the max of k_c=96 parallel reads: burst p50 ≈ per-read p99.3, burst p99 ≈ per-read p99.99 — tail amplification is the cost of fan-out and is stated, not hidden (Torvalds r3 #3). Mitigations: one-reply postings, shard-batched requests in the pure-Go client, k_c reads spread across storage teams; the client's per-storage-server multiplexing must sustain a ~3.5 MB burst (094.1 measures it) |
| Recall@10, SIFT-1M | ~0.95 (ef=64) | ≥ 0.95 tuned | re-rank ⇒ only in-posting loss |
| Recall@10, 768D real embeddings | n/a | 0.92–0.95 **TBV in 094.1** (DBpedia-OpenAI / Cohere-wiki; w sweep {16,32,64}) | closure r≈2 (α=1.2) + re-rank |
| Bulk build 10M | ~12 days (extrapolated) | 1.5–2.5 h | §8 derivation |

## 10. Wire format & Java story

- New index type `IndexTypeVectorSPFresh = "vector_spfresh"` — Go-only extension
  per the project charter; records stay fully Java-readable; index writes only
  under its own subspace. Java apps sharing metadata fail maintainer lookup as
  for any unknown type; documented.
- Structural options immutable via the evolution validator from day one
  (`spfreshLmax`, `spfreshLminRatio`, `spfreshCellTarget`, `spfreshCellMax`,
  `spfreshReplication`, `spfreshAlpha`, `spfreshKn`, `spfreshCooldownSec`, dims,
  metric, RaBitQ bits, sidecar). Runtime knobs (w, k_c, ε, C, refresh interval,
  pacing) are not stored.
- Accepted, documented hotspots: CHANGELOG versionstamped tail-shard writes
  (split/merge-rate only); TASKS deterministic-keyed (no queue flood).

## 11. Observability

Maintainer-stats metrics: posting size p50/p99; counter drift at reconciliation;
fine/coarse counts + state distribution; fine-per-cell p99 (coarse-split health);
L2 miss rate; cache staleness + full reloads; task age + lease takeovers;
seal-conflict rate; HDR-forward encounters (expected ≈ 6 % of queries at peak
ingest, §4); fetch-cap overage; inline-split count (≈ 0 with a live rebalancer);
coarse-split deferrals + starvation-guard activations; **coarse drift gauge**
‖COARSE vector − mean(fine vectors)‖ p99 (cells that stop splitting are exactly
the ones nobody recenters — this is the tripwire before retrain; LanceDB r4);
build wave progress + staging bytes; **online recall sampler**. Permanent
distribution drift ⇒ **retrain** (new generation under a fresh META epoch via §8,
atomic swap; 094.5) — needed for drift only, never for growth (§6b).

## 12. Testing plan (no mocks, real FDB, t.Parallel)

1. **Unit/fuzz:** all codecs incl. HDR rows and task rows (0 panics / 200k
   execs); RNG closure (α=1.2 admits r=2 — regression on the α=1.0 bug);
   seeded k-means; ID blocks; HDR-sorts-before-every-pk property test against
   the tuple encoder.
2. **Integration e2e:** insert/search/delete/update; min-estimate dedup; stale
   FORWARD via posting HDR (+2 RT path returns moved vectors); chain depth 2 ⇒
   forced refresh; horizon overrun ⇒ reload; filtered + crossover; **cold start
   1 → 1M by fine+coarse splits, asserting routing shape converges (cells > 1,
   fine-per-cell ≤ cellMax)**.
3. **Lifecycle interleavings (deterministic, pinned):** straggler-insert-vs-SEAL;
   insert-commits-before-seal; update/delete racing SPLIT (real posting read ⇒
   split retries; no stale child copy); split/seal `commit_unknown` no-ops;
   inline split with writer killed between SEAL and SPLIT ⇒ lease recovery;
   **coarse-split-vs-fine-SEAL both orders (the §6b exclusion rule)**;
   insert-state-read-vs-coarse-move (absent ⇒ re-route); zombie task rows
   (stale merge task after split ⇒ deleted; **SEALED target ⇒ resumed, never
   deleted**); **trigger probe vs concurrent claim — the probe's real read
   makes the resolver order them; a clobber is unrepresentable** (the rev-4
   race, pinned); **straggler staging write vs wave-B DONE — the straggler's
   real cellfin read aborts it post-finalization; no orphaned staging entry**
   (the rev-4 fence-direction bug, pinned); stale-cell HDR forward (parent
   moved cells, then split — children found via the HDR's cellID, codex r4
   #3); GC drain-not-clear.
4. **Concurrency stress:** N writers — conflict *metrics*: zero insert-vs-insert
   1020s; insert-vs-split bounded by split count; rebalancer + coarse splits
   concurrent with writers/readers.
5. **Chaos:** `StoreModel` invariant {membership ⇔ posting keys ⇔ sidecar;
   counters within tolerance; SEALED/FORWARD/HDR consistent with children;
   every fine centroid in exactly one cell}; faults across seal/split/csplit/
   GC/build waves.
6. **Recall/perf:** SIFT-1M + DBpedia-OpenAI/Cohere-wiki vs brute force (w sweep);
   p99 fan-out measurement (the §9 burst math); A/B vs HNSW; 1M stress entry;
   10M churn soak (1–5 %/day + hotspot burst, flat recall).

## 13. Phases (one PR each; every phase benchmarks what it builds)

1. **094.1** Layout + codecs + full §8 bulk build (state machine, both waves) +
   query path (two-level routing, filtered, crossover) + real-embedding recall
   benchmark + p99 burst measurement. Build-then-write contract; read-only after
   build, enforced and stated.
2. **094.2** Foreground writes (fencing, sidecar, counters, real-read triggers,
   build/foreground staging interleaving) + N-writer conflict-metrics stress.
   Carries the §6 SEAL/SPLIT transaction *primitives* (invoked manually from
   tests — no rebalancer, no triggers firing) so the update/delete-vs-split and
   probe-vs-claim interleavings are pinned here; the autonomous lifecycle
   (claims, leases, NPA, merges, coarse splits) is 094.3 (Torvalds r4 #4).
3. **094.3** Rebalancer: fine SEAL/SPLIT/FORWARD + HDR + NPA + merge + cooldown +
   **coarse splits (§6b)** + GC-drain + lease recovery + inline split + zombie
   rules + chaos + the full pinned interleaving suite + cold-start-to-1M e2e.
4. **094.4** RaBitQ residual tuning; w/k_c/C tuning; sidecar A/B.
5. **094.5** 10M churn soak, online recall sampler, retrain/swap, defaults
   freeze, `VECTOR_BENCHMARK_RESULTS.md` update.

## 14. Alternatives considered

- **DiskANN/Vamana on FDB:** round-trip depth O(path); shared adjacency writes.
  Rejected.
- **Batched beam search over HNSW:** real wire-neutral query win; tracked
  separately; no write-scalability help.
- **IVF-Flat without LIRE:** degrades under churn/boundaries.
- **Flat (single-level) routing:** dies at ~25–30k centroids (~2.5M vectors) —
  measured against rev 2's own numbers; two-level is mandatory at target scale,
  and §6b is what keeps it true for incrementally-grown indexes.
- **Brute force + RaBitQ:** optimal ≤ ~100k; this design's cold-start shape *is*
  that, on the same code path; the filtered small-set crossover is its
  query-time twin.

## 15. References

- SPANN (Chen et al., NeurIPS 2021); SPFresh (Xu et al., SOSP 2023); RaBitQ
  (Gao & Long, SIGMOD 2024) — `pkg/rabitq`.
- FDB 7.3.75: resolver `fdbserver/SkipList.cpp:983`; `REPLY_BYTE_LIMIT`
  `fdbclient/ClientKnobs.cpp:66` (+ `NativeAPI.actor.cpp:4226`); RYW
  dependent-write `fdbclient/RYWIterator.cpp:34`; versionstamp user-versions
  `fdbclient/ReadYourWrites.actor.cpp:2252`.
- `pkg/recordlayer/VECTOR_BENCHMARK_RESULTS.md` (measured HNSW numbers, §1).
- PR #279 — rev 1–3 verdicts (FDB C++ author, Torvalds, LanceDB founder, codex)
  and per-finding dispositions.
