# RFC: A Distributed Graph Database on FoundationDB (Go)

**Status:** DRAFT — design-space exploration. **This RFC deliberately takes no positions.** It enumerates the design areas, the options discovered for each, and the open questions. Decisions come later, per-area, gated by the de-risking experiments in §14.

**Working name:** TBD
**Substrate:** FoundationDB (strictly-serializable, horizontally-scalable ordered KV) via the pure-Go client (`pkg/fdbgo`) in the fdb-record-layer-go repo.

---

## 1. Vision & framing

Build a distributed property-graph database whose defensible position is **transactional correctness + horizontal scale**, not raw per-hop latency. FDB gives us serializable ACID across arbitrary subgraphs and near-linear read/traversal-throughput scaling — properties no incumbent graph DB combines. We inherit FDB's one structural weakness (per-hop network latency floor) and design *around* it rather than pretending to beat in-memory engines on single-query latency.

### 1.1 What we are NOT trying to be (working assumptions, challengeable)
- Not a single-box, in-memory, lowest-latency engine (that's Neo4j/Memgraph's turf; the network floor forbids winning there).
- Not a raw deep-analytics speed king (that's TigerGraph's compiled MPP turf).
- Not (initially) Java/record-layer wire-compatible — this is a standalone product, so we are free to design optimal graph encodings. **OPEN:** do we ever want Java-record-layer interop? Default assumption: no.

### 1.2 The design contract we keep returning to
**"Always-correct, eventually-optimal."** The base graph is always ACID-correct and authoritative. Every accelerator — derived indexes, in-memory caches, the learning optimizer — is *non-authoritative* and can only *propose*; correctness never depends on it. This contract is what makes non-determinism (including an LLM in the loop) provably unable to harm correctness. It recurs in §5, §6, §7, §8 and should be treated as a hard invariant, not a slogan.

---

## 2. Cross-cutting design principles (candidate invariants)

1. **Adjacency = ordered prefix range scan.** The one hot primitive; the whole storage design serves it.
2. **Work-per-boundary-crossing, not local-vs-remote, is the latency metric.** Amortize every remote RTT over a large chunk of local in-memory work (§6). Ping-pong (1 hop/RTT) is death; batched handoff is fine.
3. **Accelerators propose; measured reality disposes.** Fitness-gate every optimization; base always valid (§7).
4. **The big hot structures are pointer-free.** Flat primitive-slice/columnar representation for GC-invisibility *and* zero-copy (§3, §10).
5. **Throughput scales only if load spreads.** Hot-vertex bucketing and counter-sharding are load-bearing for the scaling claim, not nice-to-haves (§9).

---

## 3. DESIGN AREA — Storage & data model

### 3.1 Substrate: raw fdb-go vs. on top of the record layer
- **Option A — raw `pkg/fdbgo`.** Full control of key layout; optimal graph encoding; no relational impedance; we own indexing + traversal. *Pro:* best possible layout, no protobuf, product-appropriate. *Con:* we rebuild secondary indexes, continuations, online index build.
- **Option B — on the record layer.** Free: rich indexes (value/rank/version/spatial/text/vector), online resumable index builder, covering indexes, a Cascades planner that *already* has recursive-CTE traversal, correlated joins, EXISTS, GROUP BY. *Pro:* ~80% of an engine for free, fastest path to working. *Con:* relational lens; protobuf on the hot path (Java wire-compat we don't need); the planner is general-purpose, not graph-optimized.
- **Hybrid (noted):** raw fdb-go for the adjacency core + traversal kernel; *borrow* the record layer's index-maintainer and online-builder code for property indexes.
- **OPEN:** A vs B vs hybrid. Leaning signals: for a *competitive product* the hot path wants raw; for a *fast prototype* B is unbeatable. Sequencing option: prototype on B, harden on A.

### 3.2 Adjacency encoding (if raw)
Candidate layout — the KV analog of index-free adjacency:
```
V | {vid}                                       -> packed vertex props
E | {vid} | {dir} | {label} | {sortkey} | {other} -> denormalized edge props
```
Design levers discovered, each an OPEN knob:
- **Denormalize edge props into the value** → a hop needs no second fetch. (Strongly favored.)
- **`sortkey` in the key** → ordered adjacency for free (top-k neighbors = limited range read), no secondary index.
- **`label` in the prefix** → typed-edge scan is a sub-prefix, no filter.
- **`{dir}` out/in** → in-edges are a mirror written in the same txn (bidirectional consistency is free under ACID).
- **`{bucket}` for supernodes** → spread a hot vertex's edges across the keyspace → FDB auto-splits across shards → parallel fan-out reads + no single hot write-shard. **This is also the PowerGraph vertex-cut unit (§6).**

### 3.3 Value/record format
- **Option — custom packed columnar ("Arrow-like, but ours").** Fixed-width, pointer-free edge batches; zero-copy iteration; SIMD-filterable. Shaped by FDB limits: many small ≤100KB value-blobs, each a batch of ~dozens–hundreds of edges keyed by `(vertex, bucket)` — columnar sharded across small values, not one big chunk.
- **Option — protobuf** (only if Option B / record layer). Allocates, slower; the reason to avoid the record layer on the hot path.
- **Caveat to verify:** "zero-copy" realistically means zero-copy *from value bytes into edge iteration*, not from the socket — confirm whether `fdbgo.GetRange` hands a sub-slice of its receive buffer or a copy.
- **OPEN:** exact columnar layout; per-value batch size; compression.

### 3.4 Property / vertex / edge model
- **OPEN:** typed schema vs schemaless; property value types; secondary indexes on properties (build vs borrow from record layer); how vertex content (for §7 semantic affinity) is stored/embedded.

---

## 4. DESIGN AREA — Query & traversal engine

### 4.1 Traversal execution
- **Option — reuse record-layer recursive operators** (`RecursiveLevelUnion` = semi-naive BFS, `RecursiveDfsJoin` = DFS, `TempTable` frontier). Fast to working; row-at-a-time relational operators; not a parallel graph engine.
- **Option — purpose-built BSP/Pregel traversal kernel** (batched frontier expansion, visited-set as roaring bitmap, continuation-checkpointed across the 5s boundary). Required to be *competitive*; more work.
- **OPEN:** which, and whether the record-layer operators are a stepping stone.

### 4.2 Pattern matching (cyclic subgraph queries)
- **Worst-Case-Optimal Join (Leapfrog Triejoin)** over the sorted keyspace — FDB's sorted adjacency + key selectors are an unusually good fit (seekable sorted iterators are exactly LFTJ's requirement). The record-layer binary-join planner would *not* do this → argues for raw.
- **OPEN:** LFTJ vs binary-join; when each fires.

### 4.3 Planning
- **Cost-based, fed by exact statistics** maintained via atomic ops (§7) — expand the smaller frontier first, optimal WCOJ variable orderings. We *know* cardinalities, not estimate them.
- **OPEN:** how much planner to build vs. heuristic traversal strategies.

### 4.4 Query language (see §11 for the state-of-GQL analysis)
- **Option — openCypher core, track GQL convergence** (what the vendors themselves do; stable, widely understood).
- **Option — SQL/PGQ (`GRAPH_TABLE`)** — bolt graph pattern matching onto a SQL surface; *cheapest if we're on/near the record layer's SQL engine* (reuse planner/executor/continuations). Shares the GPML pattern core with GQL.
- **Option — GQL-native from scratch** (TuGraph ANTLR grammar exists; Spanner Graph proves it viable; early-adopter cost).
- **OPEN:** front-end choice; note the "one pattern engine, two front doors" idea (GPML core exposed via both SQL/PGQ and a Cypher/GQL front-end).

---

## 5. DESIGN AREA — Derived structures / novel indexes

### 5.1 Hub labeling / 2-hop cover (transactionally maintained)
- Pruned-Landmark-Labeling-style labels `HL|{vid}|{hub} -> distance`; exact shortest-path/reachability answered by merging `L(s)`,`L(t)` — no traversal.
- The novelty lever: **maintained *consistently under updates* via ACID** — dirty bit flipped atomically with the base edge write; dirty pairs served from base traversal, clean pairs from labels → never a stale answer.
- **Caveats:** label size blows up on dense scale-free (social) graphs; deletion is the hard incremental case (insertion easy). Best on road-like / moderate graphs.
- **OPEN:** full 2-hop vs bounded-hop/partial-landmark for large social graphs; deletion strategy (incremental repair vs tombstone + periodic bulk relabel).

### 5.2 Materialized k-hop neighborhoods
- Precompute a supernode's 2-hop set; "friends-of-friends" becomes one range read. Trades write-amplification for read speed → only viable *because* maintenance is deferred to the background maintainer (§6/§7 resolve the write-amp objection).
- **OPEN:** which vertices get materialized (all? hot only? by degree?).

### 5.3 The complementarity insight (worth preserving)
| Graph class | Diameter / fan-out | BSP + lease-cache (§6) | Hub labeling (§5.1) |
|---|---|---|---|
| Social / scale-free | low diam, high fan-out | ✅ ideal | ❌ labels bloat |
| Road / mesh | high diam, low fan-out | ❌ thin frontiers | ✅ small labels |
The two techniques cover each other's weak spots → a planner that *routes between them* per query/region is a strong story. **OPEN:** the routing heuristic.

---

## 6. DESIGN AREA — Background maintainer (the SPFresh pattern)

The repo's SPFresh vector-index rebalancer (`pkg/recordlayer/spfresh_*.go`) is a proven, crash-safe, fleet-scale implementation of exactly the maintenance loop we need. **Reuse the shape wholesale.** Machinery (all DONE in SPFresh, portable):
- FDB-persisted work queue, deterministic `(kind, target_id)` keys, set-if-absent enqueue (read-conflict-fenced, deduped).
- Lease-based claiming (`owner`, `deadline`, globally-unique owner via process nonce); crash recovery = lease expiry.
- Idempotent state-machine lifecycle per op; child identities persisted at freeze point for resume.
- Reader safety: snapshot reads + **forward markers** (nil-tuple sorts first, published atomically with the new location) + min-keyed dedup.
- Versionstamped changelog for cache convergence + GC horizon.
- Two-tier cadence: fast sweep (cheap incremental) + slow refine (expensive global).
- Per-tenant budgets + idle-skip probe + poison isolation.

Graph-specific operations (the "LIRE" of the graph — what's NEW to design):
- **Adjacency bucket-split** ≡ SPFresh split (vertex edge-count over threshold → bucket across shards).
- **Hub-label repair** ≡ NPA reassign (bounded affected-set recompute; deletion is the hard case).
- **Community re-partition / lease re-key** ≡ coarse-split (the slow refine).
- **Edge-tombstone / dead-label GC** ≡ GC sweep.
- **Degree/stat reconciliation** (advisory atomic ADD on hot path, exact at rebalance).
- **OPEN:** the affected-set computation for label repair (esp. deletion); split/merge thresholds; oscillation cooldowns.

---

## 7. DESIGN AREA — In-memory compute-ownership (lease) tier

Optional acceleration tier: machines **lease** graph partitions, hold them as in-memory CSR (flat arrays), serve hot traversals at RAM speed (recovering index-free adjacency). FDB stays the durable source of truth; the lease is a transactional key. Prior art to mine: Facebook **TAO**, **Orleans**/Akka Cluster Sharding (virtual actors), CockroachDB **leaseholder** reads.

Design areas:
- **Lease model:** read-lease (shared, replicable to many machines for hot-read hubs) vs write-lease (exclusive, write-through). **OPEN.**
- **Fencing:** fencing version stamped into every response so a stale owner is rejectable; FDB holds the token transactionally. **Non-negotiable for correctness.**
- **Routing:** partition→owner directory (in FDB, cached); consistent-hash vs explicit assignment. **OPEN.**
- **In-memory rep:** CSR as `offsets []uint64`, `neighbors []uint32`, `edgeProps []byte` — pointer-free (GC-invisible, zero-copy). **Hard rule (§10).**
- **Cross-partition handoff:** BSP superstep — expand entire local closure, accumulate cross-partition frontier grouped by destination owner, cross *once per owner* (batched). Remote crossings ≈ partition-space path length, not vertex-space. Migrate compute to data, not data to compute.
- **Supernode handling:** (a) hot-to-expand → vertex-cut (§3.2 buckets on different owners, parallel expand); (b) hot-to-route-through → read-replicas of the hot read-only partition.
- **Coherence with external writers:** owner tails the versionstamped changelog to catch writes from other paths (incl. any external/Java writer on a shared cluster).
- **Memory pressure:** partition too big for RAM → partial cache → graceful fallback to FDB reads.
- **Consistency:** two read tiers — fast lease-local (bounded staleness) vs linearizable (via FDB GRV). **OPEN:** the consistency contract we expose.
- **Sequencing note:** this tier is the leap from "graph layer" (months) to "distributed graph DB" (years). Candidate: ship stateless-over-FDB first, add the lease tier once hot partitions and query-locality are measured.

---

## 8. DESIGN AREA — Self-learning layout optimizer

A closed-loop (MAPE-K) optimizer that packs super-clusters onto nodes and tunes placement from runtime stats. **Hard line: the LLM is NOT in the hot path.** Per-vertex placement is a deterministic streaming partitioner (Fennel/HDRF/METIS) that runs in ms; the learning loop is slow and meta.

- **Stats collection (Monitor):** atomic-op counters for degree, per-partition cut counts, **boundary-crossing counts**, **co-access frequency**, hotness, memory pressure. **Gotcha:** a global counter is itself a hot key → shard counters + sample (SPFresh's 1-in-8 probe). Aggregate periodically into a compact *summary* (a few thousand numbers) — the LLM/optimizer sees the summary, never the graph.
- **Learning core (Analyze/Plan):** bandit / Bayesian-opt / RL for numeric knob-tuning — **cheaper and more principled than an LLM here.** GNN partitioners (GAP) are an option for structure.
- **LLM roles (where it earns its keep, and only here):**
  1. **Semantic co-location** — content-embedding affinity predicts co-traversal that structural cuts miss; feed as a numeric signal into the partitioner. *(The freshest sub-idea; strongest LLM justification.)*
  2. **Meta-policy tuning** — adjust objective weights / replication / repartition triggers over heterogeneous context.
  3. **Operator copilot** — explain "why did partition 47 go hot," propose remediation.
- **Fitness gate (the safety keystone):** every proposal scored by the real cost function, applied only if it beats the incumbent by a margin. Non-determinism → harmless (garbage proposals rejected; incumbent kept). This is §1.2 applied to placement.
- **Execute:** the §6 maintainer applies lease moves / repartitions transactionally.
- **Disciplines:** decisions auditable (log input+decision+rationale+outcome); hysteresis/cooldown vs thrash; A/B everything.
- **OPEN:** learning algorithm; summary schema; embedding model & how content maps to affinity; margin thresholds.

---

## 9. DESIGN AREA — Scaling, capacity, load

- **Scaling by dimension (design to these):** storage capacity ✅ linear; read/traversal throughput ✅ near-linear; **write throughput ⚠️ plateaus** (FDB transaction system — proxies/resolvers/logs); **query latency ❌ flat** (per-hop RTT fixed; accepted per §1).
- **Capacity math:** ~200 B/edge base → ~2.5–5B base edges per storage process → 10s–100s of billions of edges in one cluster; ~1T base edges near FDB's ~384-process tested envelope. Hub labels knock ~5–10× off on social graphs.
- **Single-cluster ceiling ≈ hundreds of TB–~1PB / few-hundred processes.** Beyond → multi-cluster federation, **which breaks cross-cluster ACID** (the one boundary we don't normalize — distinct from the §6/§7 in-memory partition handoff).
- **Load-spread requirements (load-bearing, not optional):** hot-vertex bucketing (§3.2); counter sharding (§8); GRV/transaction-start batching + cached/stale read versions for tolerant reads; admission control / priority classes so fan-out "whale" queries don't starve aggregate throughput.
- **The metric we publish:** aggregate QPS at fixed p99 under concurrency, and the node-count-vs-QPS scaling curve — NOT single-query latency.
- **OPEN:** federation strategy if we exceed one cluster; the consistency story across clusters.

---

## 10. DESIGN AREA — Implementation language & runtime

- **Go + the pure-Go FDB client** (`pkg/fdbgo`). The specific edge: graph BSP issues huge concurrent read fan-out; pure-Go + goroutines serves it natively (netpoller), where the cgo `libfdb_c` binding would suffer per-call overhead + thread-pool starvation. Not just "3.5× faster on a microbench" — architecturally the right concurrency shape.
- **Perf placement:** ~2–5× slower than C++ engines (TigerGraph/Memgraph/Kùzu) on the hot kernel — the conceded axis; comparable-to-better vs the JVM incumbents (Neo4j/JanusGraph), with a memory-footprint win (→ bigger lease partitions → fewer crossings).
- **GC discipline (hard rule):** large hot structures are primitive slices / off-heap arenas, **never `[]*Node`.** Pointer-graph Go dies at scale; flat-array Go is GC-invisible. Same discipline as zero-copy (§3.3).
- **Escape hatch:** the traversal kernel is small and contained — drop it to Rust/C via cgo *only if* profiling proves the gap costs deals. Pay the systems-language tax only where earned; never on the control plane (where Go's velocity is the point).
- **Risk (book it):** we own the pure-Go client as a dependency (not Apple's blessed client) — mitigated by differential-vs-`libfdb_c` testing and already-fixed server-crash bugs (CRASH_BUG.md), but ours to maintain.
- **OPEN:** arena/off-heap library choice; whether/when a native kernel is needed.

---

## 11. Competitive landscape & prior art

- **Neo4j** — keeps the traversable topology on one machine; Fabric = *manual* sharding; Infinigraph shards only *properties*. Wins on cached single-box latency + Cypher/ecosystem maturity. **Our wedge: transparent ACID sharding of the topology.**
- **TigerGraph** — compiled MPP, 10–100× Neo4j on deep queries at scale (LDBC SF-30K: 73B vertices / 534B edges). The real opponent at scale; wins on raw speed. **Our differentiator vs it: ACID/consistency + FDB ops maturity + mixed workload, not speed.**
- **JanusGraph-on-FDB** — the direct architectural precedent; proves the shape works; its per-hop latency + operational weight are exactly what we must out-engineer.
- **Dgraph / Nebula** — distributed-native, looser consistency.
- **Query language (GQL, 2024→):** ISO/IEC 39075:2024; genuinely good language (GPML pattern core), young ecosystem; Neo4j converging Cypher→GQL, Google Spanner Graph ships GQL-native; nobody fully conformant. openCypher = the pragmatic stable core + GQL convergence target; SQL/PGQ = the cheap door if we have a SQL engine.
### 11.1 Novelty verdict (adversarial prior-art sweep, 24 primary sources, 3-vote verified)

**Hypothesis CONFIRMED: every individual pillar is prior art; the *combination* is unclaimed by any system the search surfaced.** The novelty is the intersection, not "it learns."

Component-by-component:
| Pillar | Verdict | Closest prior art / gap |
|---|---|---|
| Graph-on-FDB direct-KV | **DONE (encoding UNCLAIMED)** | JanusGraph FDB adapter — but dead/minimal (v0.1.0 2018, 29 commits, FDB 6.2.x, last touched 2021), generic KV, no custom zero-copy adjacency. *(Anti-novelty claim that it relaxes serializability across multiple FDB txns was REFUTED 0-3 — so it doesn't even undercut our ACID axis.)* |
| Custom zero-copy adjacency / supernode bucketing | **UNCLAIMED** | Galaxybase/LiveGraph/Kùzu are native or single-machine, not FDB; none learned. |
| Self-driving physical layout from stats | **DONE for RELATIONAL, not graph** | NoisePage/Peloton, DBA Bandits, OtterTune — relational only; none on a strictly-serializable distributed graph. |
| **Measured fitness gate (apply-only-if-beats-incumbent)** | **essentially UNCLAIMED (strong form)** | NoisePage = validity gate (not perf); DBA Bandits = asymptotic no-regret; Eraser = predictive filter (explicitly "not zero regression"); Bao = steering, reports real regressions. Nobody does a hard *measured* beat-the-incumbent gate with rollback. |
| Workload-aware graph repartitioning | **DONE (frequency/heuristic)** | Hermes, WASP — frequency-driven; Hermes *explicitly excludes vertex content*. |
| Learned/structural graph partitioning | **DONE (structural only)** | GAP (GNN), CUTTANA — pure topology/edge-cut, structural embeddings. |
| **LLM / vertex-CONTENT-embedding-driven placement** | **UNCLAIMED (medium confidence)** | No system found using content/semantic embeddings to drive graph partitioning/co-location. Closest LLM-loop: AgenticDB (2606.20318) — but relational config/index, not graph placement. |
| Incremental hub/2-hop labeling under updates | **PARTIAL — algorithm DONE, transactional/distributed OPEN** | FULPLL / BPCL / PSL maintain PLL under insert/delete — but *not* shown ACID-consistent on a distributed transactional store. That transactional-consistency wrapper is the potentially-novel part. |
| ACID graph materialized-view / derived-index maintenance | **PARTIAL** | FDB Record Layer (transactional secondary indexes) + "Partial Update" (distributed graph MV maintenance) are close prior art on the substrate; our hub-label-as-MV is an application of it. |
| In-memory lease-owned cache tier | **DONE (pattern), under-assessed here** | TAO (over MySQL), CockroachDB leaseholder, Orleans grains — pattern is established; applying it as a *transactional-lease CSR tier over FDB* wasn't fully assessed. |

**The unclaimed intersection (what to actually claim):** a **fitness-gated (measured, not predicted/asymptotic/steered), transactionally-safe, self-optimizing physical layout — incorporating LLM-semantic-affinity signals from vertex content — on a strictly-serializable distributed graph, under an always-correct-base can-only-propose contract.** The two *individually* most-unclaimed axes (strongest defensible novelty): (1) the **hard measured fitness gate** over a learned optimizer, and (2) **content/semantic-embedding-driven graph placement**.

**Strongest refutation found:** NoisePage (CMU self-driving DBMS) — closest to "autonomously change physical design with a safety gate," but it is relational, acts on *offline model predictions* (no exploratory testing), and its gate is *validity/correctness*, not measured-performance no-regression (the paper concedes it can still make performance-degrading choices caught only afterward). Does not occupy the intersection.

**Honest caveats on the verdict:** "no one did the combination" is a negative over a large, fast-moving literature (combination confidence = *medium*). The LLM-placement axis is absence-of-evidence, not proof of absence — a very recent arXiv preprint could exist. Three pillars deserve a deeper targeted sweep before the novelty is staked hard: incremental hub labeling *in a transactional/distributed setting*, ACID graph-MV maintenance, and the lease-tier-over-FDB.

Sources: JanusGraph-FDB adapter (github.com/JanusGraph/janusgraph-foundationdb); NoisePage (db.cs.cmu.edu/papers/2021/p3211-pavlo.pdf); Eraser (PVLDB 17:926); Bao (arXiv 2004.03814); DBA Bandits (arXiv 2010.09208); Hermes (EDBT 2015); WASP (DSE 2021); GAP (arXiv 1903.00614); CUTTANA (arXiv 2312.08356); Galaxybase (PVLDB 17:3893); TAO (engineering.fb.com); FDB Record Layer (SIGMOD 2019).

---

## 12. Non-goals / explicit deferrals (challengeable)
- Java record-layer wire compatibility (unless §1.1 flips).
- Beating in-memory engines on single-query latency.
- Graph analytics algorithms (PageRank, weighted shortest path) as first-class — initially application-side iterative jobs over the store.
- Cross-FDB-cluster distributed transactions.

---

## 13. Consistency & correctness model (cross-cutting, needs its own decision)
- **Always-correct-eventually-optimal (§1.2)** as the top invariant.
- **Read tiers:** linearizable (FDB GRV) vs bounded-staleness lease-local (§7).
- **The 5s MVCC window problem:** a consistent snapshot for a >5s analytical traversal is not free — options: frontier-checkpoint with an advancing view, or build our own MVCC-versioned edges (`…|{version}`) for longer consistent traversals. **OPEN and sharp — shapes everything; prototype early.**

---

## 14. De-risking experiments (prioritized — these decide feasibility, not benchmarks)
1. **5s-boundary traversal:** 6-hop reachability over a 100M-edge graph; does the recursive/BSP plan checkpoint across the transaction boundary via continuations? Measures the §13 constraint.
2. **Hub-label repair under deletion on a churning supernode:** prove (a) distance queries never return a stale answer (dirty-bit fallback holds) and (b) the repair queue *converges* rather than growing unboundedly. Validates §5 + §6 and finds the write ceiling.
3. **Partition locality reality check:** run a partitioner on the real target graph; measure edge-cut AND the fraction of the real query workload that stays within a partition. This single number decides whether the §7 lease tier is a rocket or wasted machinery.
4. **Format/throughput:** packed columnar adjacency vs protobuf baseline — quantify the scan-throughput win and the honest 2-hop latency gap vs Neo4j.
5. **WCOJ:** LFTJ triangle-count over sorted adjacency vs binary-join — confirm FDB key selectors give competitive seekable iterators.
6. **Scaling curve:** node-count-vs-QPS at fixed p99 (the §9 headline number).
7. **LDBC SNB SF-1000→10K:** finish all 46 queries (Neo4j finishes ~half the BI at SF-1000) under maintained serializability.

---

## 15. Master list of OPEN questions
- Storage substrate: raw fdb-go vs record layer vs hybrid (§3.1).
- Columnar value layout + batch size (§3.3).
- Traversal: reuse recursive operators vs purpose-built BSP kernel (§4.1).
- Pattern matching: LFTJ vs binary-join (§4.2).
- Query language front-end: openCypher / SQL-PGQ / GQL-native (§4.4).
- Hub-label strategy for social graphs + deletion handling (§5.1).
- BSP↔hub-labeling routing heuristic (§5.3).
- Label-repair affected-set computation (§6).
- Lease model (read vs write leases), routing, consistency contract (§7).
- Learning algorithm + summary schema + embedding→affinity mapping + margin thresholds (§8).
- Federation & cross-cluster consistency beyond one FDB cluster (§9).
- Arena/off-heap approach; native kernel yes/when (§10).
- The 5s-window consistent-snapshot strategy (§13).

---

*Sequencing suggestion (not a decision): stateless-over-FDB core (§3–4) → derived structures + maintainer (§5–6) → lease tier (§7) → self-learning optimizer (§8), with the §14 experiments gating each transition.*
