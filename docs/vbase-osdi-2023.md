# VBASE: Unifying Online Vector Similarity Search and Relational Queries via Relaxed Monotonicity

**Authors:** Qianxi Zhang, Shuotao Xu, Qi Chen, Guoxin Sui, Jiadong Xie, Zhizhen Cai, Yaoqi Chen, Yinxuan He, Yuqing Yang, Fan Yang, Mao Yang, Lidong Zhou (Microsoft Research Asia and collaborators).

**Source:** 17th USENIX Symposium on Operating Systems Design and Implementation (OSDI '23), July 2023, Boston, MA. <https://www.usenix.org/conference/osdi23/presentation/zhang-qianxi>

**Note:** This document is an **own-words reference summary** of the paper, written for this project's vector-index and Cascades query-engine work (the analog of `docs/graefe-cascades-1995.md` for the optimizer). It restates the paper's *ideas, equations, and results* — which are facts/methods, not protected expression — in our own words; it does **not** reproduce the authors' prose. VBASE has no Creative-Commons preprint (it is not on arXiv; the USENIX copy is open-access for reading/download only), so a verbatim copy is intentionally avoided. For the full text consult the original publication. Cite the paper, not this summary.

> **Why this is the spec, not just background.** VBASE is the principled answer to "how does a query planner compose approximate vector search with arbitrary relational predicates, joins, and aggregates *correctly and efficiently*." It is directly on our lineage — the authors overlap with the SPANN (`docs`/skill summary) and SPFresh teams, and VBASE explicitly integrates SPANN as one of its indices. RFC-156 ports its model to our Cascades engine over the SPFresh index. See **§8 Relevance to this project**.

---

## Abstract (summary)

High-dimensional vector indices power approximate similarity search for online services, and applications increasingly need to combine that search with relational operators (filters, joins, aggregation). The obstacle: vector indices are **not monotonic**, the property conventional indices rely on. Existing vector-enabled databases work around this by using the index's `TopK` interface to materialize a temporary, monotonic, sorted list of `K` nearest neighbors, then running relational operators over that list. This is fragile because the right `K` cannot be predicted: too small starves downstream filters of results (low recall); too large wastes traversal.

VBASE identifies **relaxed monotonicity** — a property that *most well-designed vector indices already satisfy* — and uses it to build a single query engine over a `Next`-style iterator interface (not `TopK`) that drives both vector and scalar indices. It **provably** produces the same results as the optimal-`K` TopK approach while determining the needed traversal depth on the fly. Reported results: parity with state-of-the-art on plain TopK; **up to three orders of magnitude** lower average/tail latency on complex queries at equal-or-better recall; and a vector **join** finished in ~16 s at 99.9%+ recall, ~7,000× faster than a brute-force scan.

---

## 1. The problem: monotonicity and the TopK-tentative-index hack

Conventional indices (B-tree) are **monotonic**: a query can walk the index in one direction (ascending/descending) and stop as soon as the ordering guarantees nothing better remains. This is what lets a database avoid a full scan.

High-dimensional vector indices cannot afford strict monotonicity (curse of dimensionality), so they are built as graphs or clustered/partitioned structures that approach a target only *approximately*. Traversal distance to the query vector does not decrease monotonically — it oscillates. Because of this, vector indices expose only an **approximate `TopK`**: traverse "far enough" that a closer-than-current-K neighbor is unlikely, then stop.

To bolt relational operators onto this, existing systems (PASE, AnalyticDB-V, Milvus, etc.) first call `TopK(K')` to collect `K'` candidates, sort them (a *tentative monotonic index*), then apply filters/joins/limits on that sorted list. The fatal weakness is choosing `K'`. For "find the `X` cheapest products most similar to this image," you cannot know in advance how many of the `K'` nearest will survive the price filter — it may be far fewer than `X`. The result is either a conservatively huge `K'` (wasted work) or trial-and-error over many `K'` values (repeated, duplicated traversal).

**Contributions** (paper's own list, paraphrased):
1. Formally defines **relaxed monotonicity**, characterizing why well-designed vector indices work.
2. Builds a **unified database engine** on relaxed monotonicity that runs complex queries over both vector and scalar indices.
3. **Proves** the unified engine yields results equivalent to the optimal-`K` TopK approach, with a strictly more efficient plan.
4. Implements VBASE on PostgreSQL in ~2,000 LOC and evaluates 8 complex SQL queries over a 1M-row hybrid (vector + scalar) recipe dataset.

---

## 2. Background: the query classes and the database/vector divide

The paper categorizes emerging online vector queries (Table 1 in the paper) into four shapes:

- **S1 — Single-vector TopK:** `K` closest vectors to a query vector. Plain embedding retrieval.
- **S2 — Single-vector TopK + scalar filter:** TopK among rows satisfying a scalar predicate (price, popularity, category). *This is the hybrid-search case our discussion centered on.*
- **S3 — Multi-column TopK:** intersect/combine TopK over several vector columns (e.g. image + text embedding with weights).
- **S4 — Vector similarity (range) filter:** return all rows within a distance threshold of a target (similarity join, near-duplicate detection).

No prior system supported all four efficiently. The root cause is the semantic divide: relational engines assume index monotonicity; vector indices only offer approximate `TopK`. Relaxed monotonicity is the bridge.

---

## 3. Relaxed Monotonicity (§3.1) — the core idea

### Two-phase traversal pattern

Empirically, traversal of both graph (HNSW) and partition (IVFFlat) indices shows two phases:

- **Phase 1 — approach:** despite large oscillations, the traversal moves *toward* the neighborhood of the query vector `q`.
- **Phase 2 — depart:** once it has found `q`'s neighborhood, the traversal *stabilizes and steadily moves away*.

A TopK search can safely terminate early once it has entered Phase 2 with enough results, because further traversal is unlikely to surface anything closer. Relaxed monotonicity is the formalization of "have we entered Phase 2?"

### Definitions

Let traversal have reached step `s`, visiting vectors `v_1 … v_s`.

**Neighborhood radius (Eq. 1):**
```
R_q = Max( TopE( { Distance(q, v_j) : j ∈ [1, s−1] } ) )
```
`TopE` is the set of the `E` nearest neighbors of `q` seen so far; `R_q` is the distance to the farthest of those `E`. It defines `q`'s "neighbor sphere." A `K`-NN query needs `E ≥ K`. During Phase 1, `R_q` shrinks; in Phase 2 it stabilizes.

**Current traversal position (Eq. 2):**
```
M_q^s = Median( { Distance(q, v_i) : i ∈ [s−w+1, s] } )
```
the *median* distance to `q` over the most recent `w` traversal steps (the "traversal window"). Median (not mean) is used to discard outliers in the window.

**Definition 1 — Relaxed Monotonicity (Eq. 3):**
```
∃ s  such that  ∀ t ≥ s,  M_q^t ≥ R_q
```
i.e. there is a step `s` past which every step moves into a region whose distance to `q` is outside `q`'s neighbor sphere. An index "follows relaxed monotonicity" if such an `s` exists. In the engine this is checked incrementally and reduces to **`M_q^s > R_q`** (steps beyond `s` are assumed to keep holding).

### The two parameters, `E` and `w`

`E` (neighbor-sphere size / priority-queue size) and `w` (traversal-window size) are the only knobs. They capture each index's internal behavior and trade accuracy for latency (larger ⇒ more accurate, slower). Instantiations:

| Index | `E` | `w` | Notes |
|---|---|---|---|
| **Graph (HNSW)** | `ef` (best-first candidate-queue size) | `1` | The sorted candidate queue *is* the neighbor sphere; each new vector is compared against `R_q` (the queue's farthest), so the window is a single step. |
| **Partition (IVFFlat, SPANN)** | `K` (the query's K) | total vectors in the `m` probed clusters | Phase 1 = picking the `m` closest centroids; Phase 2 = scanning those `m` clusters; Eq. 3 holds once all `m` clusters are traversed. |
| **Scalar (B-tree)** | `1` | `1` | Strict monotonicity is the special case where Eq. 3 is always true. |

### The four components every ANNS index already has

Per ANN-Benchmarks, mainstream vector indices implement: (1) **index traversal**; (2) a **termination check**; (3) a **monotonicity check** (has it reached Phase 2?); (4) a **priority queue** of the `K` best so far. The monotonicity check is typically a *necessary condition* of termination. VBASE's claim is that component (3), however each index implements it, satisfies Definition 1 — so the index merely needs to expose its traversal and tune `ef`/`m` (⇄ `E`/`w`).

---

## 4. Unified Query Execution Engine (§3.2)

VBASE uses the **Volcano / Iterator model** (`Open` / `Next` / `Close`). Each operator pulls a stream of tuples from its child; execution stops when a termination condition is met. Conventional scalar indices already fit this. Vector indices — which historically expose only `TopK` — are **re-architected to expose their internal traversal** through `Next`: each `Next` returns the next-closest unvisited vector in traversal order.

### Generalized termination check

VBASE augments the *ordinary* operator termination condition with the **relaxed-monotonicity check** (`M_q^s > R_q`). A query stops **only when both** hold:

- **OrderBy-with-limit (TopK):** ordinary condition = "`K` results collected." VBASE additionally requires Phase 2 (relaxed monotonicity true). So: collect `K`, *and* confirm traversal is veering away, then stop.
- **Distance range filter:** ordinary condition = "a traversed vector exceeds range `R`." VBASE additionally requires Phase 2 — guarding against stopping during Phase-1 oscillations.

For a scalar/B-tree index the relaxed-monotonicity check is always true, collapsing to the classic iterator termination.

### Why this is faster — and what it newly enables

The crucial structural change: instead of **filter-after-TopK**, VBASE does **filter-during-traversal**. The index scan streams candidates in distance order; the filter runs inline; the engine keeps pulling until it has `K` *surviving* results and Phase 2 is reached. No `K'` to guess. This is the main source of VBASE's wins on S2/S3/S4, and it composes naturally with:

- **Multi-column** queries (a native NRA algorithm over multiple vector iterators — §6.4),
- **Joins**: a vector join (e.g. document auto-tagging — for each unlabeled doc find the nearest label embedding) becomes a nested-loop / index join under a distance range filter, rather than a brute-force cross product.

---

## 5. Result Equivalence (§3.3) — the correctness proof

This is the load-bearing guarantee: VBASE's streaming, filter-during-traversal plan returns **the same answer** as the idealized optimal-`K` TopK plan.

### TopK + filter

A TopK-based system collects `K'` candidates, sorts, filters, limits to `K`:
```
r1 = Limit_K( Filter( Limit_{K'}( Sort(R1) ) ) )                         (Eq. 4)
```
where `R1` is the set of vectors actually traversed. With `filter_selectivity` = (output rows)/(input rows) of the filter, this becomes
```
r1 = Limit_{K''}( Filter( Sort(R1) ) ),  K'' = min(K, K' × filter_selectivity)   (Eq. 5)
```
If the system could pick the **optimal** `K̃ = K / filter_selectivity` (so `K' × filter_selectivity = K`), Eq. 5 reduces to
```
r1 = Limit_K( Filter( Sort(R1) ) )                                       (Eq. 6)
```
VBASE instead traverses `R2` and filters inline:
```
r2 = Limit_K( Sort( Filter(R2) ) )                                       (Eq. 7)
```
Because both use the **same index, same traversal, same relaxed-monotonicity termination**, they visit the **same set of vectors**: `R1 = R2`. `Filter` and `Sort` commute, so **`r1 = r2`. ∎**

The takeaway is the dilemma VBASE escapes: with a static `K'`,
- `K' × filter_selectivity < K` ⇒ too few results ⇒ **low recall**;
- `K' × filter_selectivity > K` ⇒ correct but **wasteful** traversal;
- predicting the exact `K̃` requires knowing `filter_selectivity` per query in advance.

VBASE reaches `K̃ × filter_selectivity = K` **on the fly** via the termination check — no prediction.

### Range filter

Analogously, `r1 = Filter(Limit_{K'}(Sort(R1)))` (Eq. 8) → `Limit_{K''}(Filter(Sort(R1)))`, `K'' = K' × filter_selectivity` (Eq. 9) → under optimal `K̃`, `r1 = Limit_T(Filter(Sort(R1))) = Filter(Sort(R1))` (Eq. 10), while VBASE computes `r2 = Filter(R2)` (Eq. 11). Same traversal ⇒ `R1 = R2`; filters are order-insensitive ⇒ **`r1 = r2`. ∎**

---

## 6. Implementation (§4)

VBASE is built on **PostgreSQL**, ~2,000 LOC total, ~<200 LOC per integrated index.

### 6.1 Relaxed-monotonicity check (§4.1)

Two queues track traversal state, shared by all indices:
- **`smallestQueue`** — a priority queue of size `E`, the visited nearest neighbors of `q` (yields `R_q`).
- **`recentQueue`** — size `w`, the most recent traversal window (yields `M_q^s`).

On each visited vector both queues update; the check evaluates Eq. 3 from them. `E`, `w` are data- and index-dependent and are the accuracy/latency knobs. The check lives in the **index-scan operator** so individual indices don't reimplement it.

### 6.2 Vector index integration (§4.2)

Each index exposes `Open` / `Next` / `Close`:
- **HNSW (graph):** remove the TopK priority queue; keep only traversal state — a visited bitmap, the current vector, and the candidate set. `Open` searches the upper layers to find an entry point; each `Next` returns the closest unvisited node, records it, and expands its neighbors into the candidate set; `Close` frees the state.
- **IVFFlat / SPANN (partition):** `Open` sorts all posting lists by centroid distance (near→far); each `Next` reads vectors from the nearest lists one by one; state is the sorted lists + current read position.
- **Index-scan operator:** a new PostgreSQL "vector index" access type forwards `Next` to the underlying index and returns each vector's tuple address so the engine can fetch the base row. The relaxed-monotonicity check is applied here.
- **OrderBy-with-limit / Range filter / Join** are then ordinary operators stacked on the scan, terminating per the generalized condition.

### 6.3 Query planning & cost model (§4.3)

This is the part directly relevant to a cost-based FLAT-vs-ANN decision:

- **Vector computation cost:** scalar op cost `t` (e.g. 0.0025); a distance computation costs `tv(dim) = t · c · dim`, where `c` reflects SIMD and `dim` the dimensionality.
- **Selectivity estimation:** sampling-based. Uniformly sample vectors at a small rate (paper uses **0.001**), store in metadata, apply the query's filter on the sample. Reported **q-error < 1.1** in most cases at **<1 ms** overhead.
- **Index-scan cost:** per step `Cstep = tv(dim) + tIO`. For **partition** indices (IVFFlat/SPANN):
  ```
  Cp = Nc × Cstep + max( ⌈Sel(q)·N / Np⌉, m ) × Np × Cstep
  ```
  (`N` table size, `Nc` #centroids, `Np` avg vectors/partition, `m` partitions needed for the monotonicity check). For **graph** indices (HNSW):
  ```
  Cg = Nstart × Cstep + max( Sel(q)·N, NE ) × Riter × Cstep
  ```
  (`Nstart` upper-layer steps in `Open`, `NE` steps to satisfy the monotonicity check, `Riter` avg distance calls per step). The index-dependent terms are estimated by sampling inside the database's `Analyze`.

### 6.4 Multi-column scan optimization (§4.4)

TopK-based systems do multi-column queries by repeatedly doubling `K` and re-running NRA — redundant traversal. VBASE runs **NRA natively over the live iterators**. Pure round-robin wastes effort on low-quality indices, so VBASE uses local (last-round results) + global (running average score `avg_i` per index) signals to allocate traversal: a high-quality index `i` is visited `W_i = ⌈ n2 × (1/avg_i) / Σ_j (1/avg_j) ⌉` times per round, while every index is still visited at least once (exploration vs. exploitation).

---

## 7. Evaluation highlights (§5)

**Benchmark:** Recipe1M extended to a vector+scalar hybrid — a Recipe table (~330,922 rows) with two 1,024-d embeddings (image, description) plus a scalar `popularity`, and a Tag table (10,000 tag vectors). Eight SQL queries:

- **Q1** single-vector TopK; **Q2** TopK + numeric filter; **Q3** TopK + string filter; **Q4** multi-column TopK; **Q5/Q6** multi-column TopK + numeric/string filter; **Q7** vector range filter; **Q8** similarity **join** (Recipe ⋈ Tag on inner-product ≤ D).

`K = 50`. Baselines: Milvus, Elasticsearch (vector search), PASE, PostgreSQL (databases), all on HNSW (`M=16`, `ef_construction=200`, `ef_search=64`). PostgreSQL = brute-force ground truth.

**Headline results (latency in ms; recall vs. ground truth):**

| Query | Best baseline | VBASE | Note |
|---|---|---|---|
| Q1 TopK | PASE ~4.8 avg @0.9949 | ~4.9 avg @0.9949 | parity (same algorithm); VBASE ~2.8% slower than PASE because the iterator fetches base tuples during traversal. |
| Q2 TopK+numeric | PASE 29.3 / Milvus 33.7 / ES 97.9 (ES recall **0.501**, undershot K) | **11.7** @0.9989 | baselines must guess `K'`. |
| Q3 TopK+string | PASE 13.2 / ES 79.9 | **7.9** @0.9983 | |
| Q4–Q6 multi-column | only Milvus; Milvus 5,610 / 12,638 / 6,543 | **19.8 / 35.8 / 21.6** @0.96+ | **200–300×**, higher recall; Milvus often worse than seq-scan. |
| Q7 range filter | only VBASE (PASE simulated) | 10.8 avg / 168.9 p99 @0.9992 | p99 ≫ avg when a query matches up to 10k rows. |
| Q8 join | PostgreSQL 129,051,273 ms (brute force) | **16,335 ms** @0.9992 | **~7,900×**. Other systems can't run it. |

**Selectivity sensitivity (Q2, PASE vs VBASE):** the optimal `K̃` swings with `filter_selectivity` (e.g. avg `K̃ ≈ 1,772` at selectivity 0.03 vs `≈ 291` at 0.3, with large variance even within one selectivity). A static `K'` cannot stay accurate across this range; `K' = 10,000` gets near-exact recall but collapses latency. VBASE tracks `K̃` per query automatically.

**Billion-scale (§5.4):** VBASE integrates **SPANN** (disk-based, NVMe) for billion-scale datasets, demonstrating the same engine works over an external-memory partition index.

**Summary:** TopK parity; **100–1000×** on complex queries at equal-or-better recall; the win comes from filter-during-traversal + on-the-fly termination instead of `K'` guessing.

---

## 8. Relevance to this project (editorial — our framing, not the paper)

How VBASE maps onto our stack, and why RFC-156 adopts it:

- **Relaxed monotonicity already holds for SPFresh.** SPFresh is a SPANN-derived partition index. Per §3.1's partition instantiation (`E = K`, `w` = vectors in the `m` probed clusters), our search loop — route to nearest coarse cells, then scan fine posting lists in centroid-distance order — *is* a relaxed-monotonic traversal. We don't need a new index; we need to expose its traversal.

- **`Next`, not `TopK`, is the integration point.** Our SPFresh search today is one-shot top-`k`. The VBASE change is to expose it as a resumable iterator (`Open`/`Next`/`Close`): pull posting-list candidates in distance order, maintain `smallestQueue(E)` + `recentQueue(w)`, and let the Cascades operators above (filter, limit, join) drive termination via the generalized check (`M_q^s > R_q` **and** the operator's own condition). This is precisely the resumable-cursor design that fixes our latent **post-filter under-return / wrong-answer** bug: a residual `WHERE` no longer truncates a fixed top-`k` — the scan keeps yielding until `K` *surviving* rows are found and Phase 2 is reached.

- **The equivalence proof (§3.3) is our correctness argument.** It shows filter-during-traversal returns the same rows as the optimal-`K` plan — i.e. the answer is the true `K` nearest *that satisfy the predicate*, not "the predicate-survivors among the global top-`k`."

- **The cost model (§4.3) is the basis for FLAT-vs-ANN selection.** Sampling-based `Sel(q)` plus the `Cp`/`Cg` scan-cost formulas give Cascades a principled way to choose, by estimated selectivity, between (a) an ANN iterator scan and (b) a value-index → exact-distance-sort (FLAT) plan for highly selective predicates.

- **Scope/divergence note for the RFC:** VBASE is implemented on PostgreSQL's Volcano executor; we implement the same model on our `RecordCursor`/continuation executor and the Cascades planner. The *idea* (relaxed-monotonic `Next` + generalized termination + selectivity-costed plan choice) ports directly; the *mechanism* (cursor continuations, match-candidate rules, physical wrappers) is ours. Java's record-layer has no equivalent, so this is an allowed read-side extension (wire format is untouched — vector storage is unchanged).

---

*Citation:* Qianxi Zhang et al., "VBASE: Unifying Online Vector Similarity Search and Relational Queries via Relaxed Monotonicity," OSDI '23. ISBN 978-1-939133-34-2.
