# RFC-156: VBASE-style hybrid vector search in Cascades

**Status:** Draft (pre-Graefe)
**Phase:** vector / query-engine (slots after RFC-046 multi-partition, RFC-094 SPFresh)
**Scope:** Cascades planner + query executor + SPFresh searcher (`pkg/recordlayer/spfresh_query.go`). **Zero wire-format change** — on-disk SPFresh layout, the `VectorIndexScanContinuation` proto, and the `DistanceRank` plan encoding are all unchanged; this is a **read-side** extension only. Resumable search state lives in process memory for a query's lifecycle, never serialized into a continuation (mirrors Java).
**Spec:** [`docs/vbase-osdi-2023.md`](../docs/vbase-osdi-2023.md) (VBASE, OSDI '23). VBASE is the algorithmic reference for this RFC the way Graefe is for the optimizer.
**Reviewers (gate, all required):** Graefe (Cascades alignment) · Torvalds (code quality) · codex · @claude · **spfresh-reviewer** (SPANN/SPFresh fidelity of the searcher refactor).

---

## 1. Problem — a latent wrong-answer bug, not just a missing feature

Take a `documents` table with a normal index on `(user_id, city)` and an **un-partitioned** SPFresh vector index on `embedding`:

```sql
SELECT id FROM documents
WHERE user_id = 42 AND city = 'berlin'
QUALIFY ROW_NUMBER() OVER (ORDER BY euclidean_distance(embedding, :q)) <= 10;
```

Intent: *the 10 nearest documents to `:q` among user 42's Berlin documents.* What we plan today:

```
RecordQueryVectorIndexPlan(SPFresh, k=10, ordered by distance)   ← top-10 nearest over ALL docs
  → indexFetchCursor                                              ← fetch base records
    → Filter(user_id=42 AND city='berlin')                       ← residual, applied AFTER
```

The `k=10` is consumed **into the scan** (`ToScanPlan` extracts `k` from the `DistanceRankComparison`, `vector_index_match_candidate.go:243`), *below* the `WHERE`. So the index returns the 10 globally-nearest rows and the residual filters them — yielding the user-42/Berlin **subset of the global top-10**, which is almost always **empty**. Worked example: 1,000 docs, user 42 has 5 Berlin docs at global distance-ranks {200, 201, 500, 700, 999}; correct answer = those 5; actual answer = ∅.

This is **semantically wrong, not merely low-recall**: pushing `LIMIT 10` beneath a `WHERE` is illegal even for an exact index. The only case that is correct today is when the filter columns are the **partition key** (consumed into the scan prefix → search *within* the partition); SPFresh rejects `PARTITION BY` (`metadata/builder.go:265`), so even that escape hatch is HNSW-only. No test exercises a non-partition residual on a vector query — it is an unprobed dimension (classic green-CI latent bug, cf. CLAUDE.md).

**Java has the same limitation** (`VectorIndexMaintainer.kNearestNeighborSearch` is a single fixed-`k` HNSW call materialized into a `ListCursor`; residuals filter that fixed set — same under-return). So fixing this is an **allowed read-side extension that goes beyond Java** (per CLAUDE.md "query reach may exceed Java" — wire compat is untouched), not a conformance break.

### Root cause — three gating sites

1. `vector_index_match_candidate.go:208` `ComputeBoundParameterPrefixMap` — consumes the contiguous **equality** partition-prefix run and **always retains the distance-rank binding**; a non-partition residual is left above the scan while the rank-limit went below it.
2. `value_distance_row_number.go:75` `DistanceRowNumberValue.IsIndexOnly() == true` — distance ordering can **only** come from a vector index; there is no scalar-distance + sort path. This is true because, like Java, **we have no physical sort operator** (RemoveSortRule eliminates sorts; ordering comes from indexes only). So there is no exact FLAT fallback to choose.
3. `plan_executability.go:49` `validateNoIndexOnlyResidual` — correctly *rejects* a plan that carries an index-only `DistanceRank` as a post-scan residual (good — it prevents one class of garbage), but it does nothing about a *non-index-only* residual (`user_id`/`city`) sitting beneath a rank-limit pushed into the scan.

---

## 2. The model — VBASE relaxed monotonicity (see `docs/vbase-osdi-2023.md`)

The generic fix is **not** a vector-specific patch; it is to model approximate vector search the way VBASE does and let Cascades' ordinary ordering/limit/filter machinery compose it.

- **Relaxed monotonicity already holds for SPFresh.** SPFresh is a SPANN-derived partition index. Per VBASE §3.1's partition instantiation, `E = k` and `w` = number of vectors in the `m` probed clusters; Phase 1 = choosing the `m` nearest cells, Phase 2 = scanning them. Our search loop (route to coarse cells → scan fine posting lists in centroid-distance order → RaBitQ → exact re-rank) **is** a relaxed-monotonic traversal (`spfresh_query.go:120`).
- **Vector index = a distance-*ordering* provider with a resumable `Next` iterator**, not a "top-k black box." This is the key reframing. The index streams candidates in (approximate) distance-to-`:q` order; the consumer pulls until satisfied; the index signals Phase 2 (relaxed-monotonicity termination) when further pulls are unlikely to beat the current best.
- **Generalized termination:** a query stops when the operator's own condition **and** the relaxed-monotonicity check (`M_q^s > R_q`, VBASE Eq. 3) both hold. For `Limit(k)` over a distance-ordered stream: stop once `k` *surviving* rows are out **and** Phase 2 is reached.
- **Correctness comes free from VBASE's equivalence proof** (§3.3, Eqs. 4–7): filter-during-traversal returns the *same* rows as the optimal-`k̃` plan — i.e. the true `k` nearest **that satisfy the predicate**, not the predicate-survivors of the global top-`k`.
- **The §4.3 cost model** (sampling-based selectivity + `Cp`/`Cg` scan-cost formulas) is the basis for choosing FLAT vs ANN.

With this reframing the offending query lowers to the obviously-correct shape:

```
Limit(10)                                   ← rank ≤ 10 over the FILTERED, distance-ordered stream
  → Filter(user_id=42 AND city='berlin')    ← residual; order-preserving
    → VectorIndexScan(ordered by distance(embedding,:q), resumable)
```

A `Filter` preserves ordering, so `Limit(10)` over the distance-ordered filtered stream is exactly "the 10 nearest matching rows." No `IsIndexOnly` residual survives — the distance is an **ordering property the scan provides**, and the limit is a plain row limit. This is the Cascades-idiomatic, property-driven form Graefe's framework expects.

---

## 3. Investigation — what exists today (file:line)

### Java reference (tag 4.12.11.0)
- `VectorIndexMaintainer.java:220-238` — one `hnsw.kNearestNeighborsSearch(adjustedLimit, efSearch, …)` call, materialized into a `ListCursor`; continuation is **positional within the materialized top-k**, *not* HNSW traversal state.
- `VectorIndexScanMatchCandidate.java:68-72,427` — recognizes only `WHERE partition_key = v QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY distance) <= k`; exactly one distance-rank comparison; no FLAT, no residual composition, no re-probe.
- `CardinalitiesProperty` / `PlanningCostModel` — heuristic; cardinality is `atMostOne` (PK bound) else `unknown`; **no vector-specific selectivity**.
- **Conclusion:** resumable re-probe, sound predicate composition, FLAT, and selectivity-costed vector choice are all **absent in Java** ⇒ allowed Go read-side extensions. Wire structs (`VectorIndexScanComparisons`, `VectorIndexScanBounds`, `DistanceRankValueComparison`, continuations) stay 1:1.

### Go today
- **Cursor model** — `pkg/recordlayer/cursor.go:203` `RecordCursor[T].OnNext`; `RecordCursorResult` (`:110`); `RecordCursorContinuation{ToBytes,IsEnd}` (`:43`); `Bytes`/`End`/`Start` continuations (`:53,:76,:92`); `NoNextReason` incl. `ScanLimitReached`/`TimeLimitReached` (`:11`); `FromListWithContinuation` re-enters at a 4-byte BE position (`:298`). This is the iterator abstraction the resumable scan implements.
- **Vector scan execution** — physical plan `RecordQueryVectorIndexPlan` (`plans/vector_index_scan.go:28`); executor `executeVectorIndexScan` (`executor/executor.go:316-387`) → `store.ScanIndexByType(idx, IndexScanByDistance, …)` → wrapped in `indexFetchCursor`. **One-shot**: all top-k materialized via `FromListWithContinuation` (`spfresh_index_maintainer.go:516`).
- **SPFresh search** — `spfresh_query.go:25` `spfreshSearcher{w,kc,c,epsilon,…,capped}`; `search()` (`:120-359`): route → SPANN ε-prune → one parallel posting burst (cap `4·Lmax+1`) → starvation widening → forwarded-child resolve → top-`C` by estimate → exact sidecar re-rank → top-`k`. **No frontier retained**; entire traversal is a black box that materializes top-k. `searchCurrentGeneration` (`spfresh_index_maintainer.go:521-598`) sets `kc/w/c/epsilon` from plan knobs.
- **Cost model** — `planning_cost_model.go:36` `PlanningCostModelLess` (cardinality of data accesses → residuals → access count → tie-breakers → scalar cost). **No statistics**: `physical_vector_index_scan_wrapper.go:91` `vectorScanCardinality` returns literal `k` (default 10); `HintCost` (`:84`) = `card · multiplier`. A full scan (card≈N) always loses to a vector scan (card≈k) regardless of selectivity — so today the planner *cannot* prefer FLAT.
- **No physical sort operator** — ordering is index-provided (RemoveSortRule architecture, as in Java). This is the architectural reason FLAT doesn't exist and `DistanceRowNumberValue.IsIndexOnly()=true`.
- **Gates** — required-for-binding (`abstract_data_access_rule.go:86-111`, returns `{distanceAlias}`); `validateNoIndexOnlyResidual` (`plan_executability.go:49`); `IsIndexOnly` walk (`predicate_multi_map.go:912,947`). Distance-rank lowering: `logical_qualify.go:185` `applyDistanceRankTransform` → `predicates/distance_rank_transform.go:29` `TransformRowNumberDistanceRankMaybe`.

---

## 4. Design

Phased so each phase is independently green and Phase B alone closes the wrong-answer bug.

### Phase A — Resumable distance-ordered SPFresh cursor (engine primitive)

Refactor `spfreshSearcher.search()` into an `Open`/`Next`/`Close` iterator that yields candidates in approximate distance order:
- `searchInit(tx, query) → *spfreshFrontier` — routing + initial cell selection + ε-prune; retains state: routing snapshot + generation, probed/pruned cell sets, the candidate heap (RaBitQ estimates), per-cell read cursors, the relaxed-monotonicity queues `smallestQueue(E=k)` + `recentQueue(w)` (VBASE §4.1).
- `searchNext(frontier, batch) → ([]spfreshSearchResult, phase2 bool)` — emit the next distance-ordered batch; **widen** (admit the next nearest pruned cell / extend `m`) when the heap drains; update the monotonicity queues; report Phase 2 when `M_q^s > R_q`.
- `Close` frees the frontier.

Faithfulness (spfresh-reviewer gate): widening must extend probed cells in **centroid-distance order** and re-apply SPANN ε-pruning over the widened set; RaBitQ estimate ordering and exact-sidecar re-rank are unchanged; this is VBASE's partition-index instantiation (`E=k`, `w`=vectors in `m` probed clusters), *not* a new pruning scheme. The existing one-shot `search()` becomes a thin wrapper (`searchNext` until `k` and Phase 2), so **all current queries are byte-for-byte unchanged**.

A new `spfreshSearchCursor` implements `RecordCursor[IndexEntry]` over the frontier. **Resumable state is in-memory for the query lifecycle only**; cross-transaction continuations stay positional over the *materialized* result (Phase C), like Java's `ListCursor` and `FromListWithContinuation` — no traversal frontier is serialized, so no wire/continuation change and no fragile mid-traversal resume.

**Three algorithms the spfresh-reviewer required be specified (iteration 1, §10):**

- **ε-pruning halting + incremental equivalence.** Widening pops the next-nearest cell from the ε-pruned tail (sorted at `searchInit` by SPANN Eq. 3) and admits it only if it passes the *current* ε-threshold. Re-pruning is **monotonic**: a cell pruned at a tighter `R_q` stays pruned as `R_q` can only shrink or hold during traversal, so incrementally admitting cells in centroid-distance order is **equivalent to one-shot ε-pruning of the routed set up to the widened horizon** (proof obligation pinned by a test: incremental-widen result == batch-prune result on real data). The widening loop halts when **any** of: enough survivors for the outstanding `Limit` demand are exact-re-ranked **and the top-`C` heap is finalized** (no in-flight candidate can re-rank below the current `k`-th survivor — spfresh-reviewer minor), Phase 2 fires (`M_q^s > R_q`), or the budget cap is reached.
- **RaBitQ re-rank timing — emit only exact-re-ranked rows.** Candidates are gathered in RaBitQ-estimate order but are **never emitted to the operator above until they have been exact-sidecar-re-ranked**; emission lags traversal by the re-rank window so the operator always sees rows in *true* distance order. Re-rank fires on the finalized prefix (a prefix is final once Phase 2 holds for it or the heap below the current top-`C` cannot contain a closer vector), never per-raw-batch — avoiding both premature truncation and pre-re-rank mis-ordering.
- **HDR / forwarded-child mid-traversal.** If a cell transitions SEALED→FORWARD (RFC-094 §6) while its read cursor is live, the frontier state machine, on seeing the HDR marker, **re-routes to the child cellIDs** (point-read children, enqueue their postings into the pruned-cell set in centroid-distance order, +2 RT — identical to the one-shot path), and discards the parent's partial read without orphaning it (the parent contributes no entries post-FORWARD). Generation-pinning (single read-version, Phase C) means a generation flip cannot occur mid-query.

### Phase B — Planner composition (the correctness fix)

> **Revised after Graefe/Torvalds NAK (iteration 1, §10).** Distance order is **parameter-dependent** (it depends on the bound query vector `:q`), so it is **not** a generic, requestable Cascades `Ordering` property — Cascades cannot thread a query parameter through one, and modeling it as such would either fabricate a parameter-independent property or require a generic sort operator (RemoveSortRule forbids both). Instead the ordering is **intrinsic to the `RecordQueryVectorIndexPlan` node** itself (exactly as Java carries it in `DistanceRankValueComparison`): the vector scan *is* the operator that emits rows in `distance(field,:q)` order, and only vector-aware rules reason about that fact. No generic ordering property is introduced, so **no cross-rule property audit is needed** and RemoveSortRule is untouched.

Two clean, decoupled pieces (Graefe's match-then-implement separation):

1. **Match-candidate emits ONE canonical form** (`vector_index_match_candidate.go:208,243`). It matches the vector index for the distance ranking and emits a `RecordQueryVectorIndexPlan` that is a **distance-ordered, resumable stream** (Phase A) — it consumes only the partition-equality prefix into the scan range, and it does **not** consume `k` as a scan-internal pre-filter limit and does **not** introspect residual `WHERE` predicates. (Match-candidates produce a shape; composing residuals/limits is a rule's job — Graefe finding #2.) The residual `Filter` and the `Limit(k)` are left *above* the scan by normal generation:
   ```
   Limit(k) → Filter(residual) → VectorIndexScan(distance-ordered, resumable)
   ```
   A `Filter` is order-preserving, so `Limit(k)` over the filtered distance-ordered stream is exactly "the k nearest matching rows." This is the correct, default plan.

2. **A separate optimization rule `SinkLimitIntoVectorScanRule`** recognizes `Limit(k)` *directly* above a `VectorIndexScan` with **no intervening row-dropping/order-disturbing operator** (i.e. no residual `Filter`) and folds `k` into the scan's `k`-parameter — restoring today's efficient one-shot top-`k` for the no-residual and partition-only cases (backward compatible, byte-for-byte). When a residual `Filter` intervenes, the rule does **not** fire, and the scan must stream until `Filter`+`Limit` collect `k` survivors (resumable, Phase A).

**On `IsIndexOnly` (Graefe finding #1, coherence).** In the canonical form the QUALIFY rank is lowered to *(a)* the vector scan's intrinsic distance ordering plus *(b)* a plain `Limit(k)` — the `DistanceRowNumberValue` index-only marker is **not produced as a residual predicate at all** in the composed-with-residual shape, so `validateNoIndexOnlyResidual` (`plan_executability.go:49`) has nothing to reject and `IsIndexOnly()` staying `true` is consistent (the marker only ever lives inside the scan binding on the sink path). The framing is now coherent: distance is a *plan-node-intrinsic ordering*, never a free-floating residual value.

Result: the offending query plans to `Limit → Filter → ordered VectorIndexScan` and returns the correct `k` nearest matching rows. **Pin with a red→green FDB test on the exact query above.** Depends on Phase A (the scan must yield more than `k` so the filter has rows to cull).

### Phase C — Single-transaction materialization, honest truncation, filter fusion

> **Revised after Torvalds NAK (iteration 1, §10).** A budget cap reported only via a metric, combined with positional cross-transaction continuations, is a silent under-return on pagination: re-running a budget-capped search from a positional offset in a *new* transaction sees a different snapshot under concurrent writes and can return "k−n forever." Fixed below by materializing within one snapshot and surfacing truncation through the existing limit channel.

- **Single-transaction snapshot, then materialize.** The filtered/resumable vector scan resolves its full result *within one transaction's snapshot* — generation-pinned at a single read-version — collecting `k` survivors (or stopping at the budget cap), then materializes them. This mirrors Java's `ListCursor` (materialize-then-paginate) and is what makes pagination stable. We do **not** resume the *search* itself across transactions (FDB snapshots are valid only ~5 s; an exact cross-transaction snapshot resume is impossible anyway).
- **Pagination is positional over the materialized result** (existing `FromListWithContinuation`, `cursor.go:298`), not a re-run of the search — so concurrent writes between pages cannot reorder or drop rows (Torvalds findings #2, #3 eliminated).
- **Honest truncation via the existing limit channel.** When the budget cap is hit before `k` survivors, the cursor returns the partial result with **`NoNextReason.ScanLimitReached`** + a continuation (`cursor.go:11`) — exactly as row/byte/time limits already do. This distinguishes "only N rows match the predicate" (`SourceExhausted`, terminal) from "budget hit at N, more may exist" (`ScanLimitReached`, resumable). The SQL layer treats it like any other scan-limit. **Never a silent `< k`** — the difference between the two reasons is the signal. Add `CountSPFreshFilteredTruncated` as telemetry *in addition to* the reason, not instead of it.
- **Filter-during-traversal fusion** (VBASE optimization). The `spfreshSearchCursor` evaluates the residual on each candidate before it counts toward `k`, avoiding fetch+discard churn. Inline evaluation needs the filter columns mid-scan — load them from the SPFresh sidecar where cheap, else a base-record fetch inside the cursor. The fusion is a *physical* optimization of the logical `Limit → Filter → Scan` shape; it does not change semantics or the truncation contract above.
- **Budget-cap calibration (Torvalds handoff TODO).** The cap value (max cells probed / max candidates) is an implementation tuning knob, not a design constant. It MUST be calibrated empirically so that search + materialize completes well within the FDB 5 s transaction limit for typical selectivities — otherwise the design trades a clean `ScanLimitReached` truncation for a hard `TimeLimitReached`/timeout. Validate with the §8 budget-exhaustion stress scenario before Phase C ships.

### Phase D — FLAT path + cost-based selection (OPTIONAL follow-on, not a correctness prerequisite)

> **Revised after Torvalds/Graefe NAK (iteration 1, §10).** Phases B+C are correct (or honestly `ScanLimitReached`) for **all** selectivities on their own — they do not depend on Phase D. Phase D is a *performance* optimization: it makes very-selective filters fast and exact, and lets the cost model route them away from a budget-capped ANN scan. It is explicitly gated and may ship later or never without compromising correctness.

For a selective predicate, exact beats ANN. The plan is `ValueIndexScan(user_id,city) → computeDistance(scalar) → TopN(k)`:
- `distance(field,:q)` is already an ordinary scalar `Value` (`euclidean_distance` is a scalar function over the stored vector; only the `DistanceRowNumberValue` *wrapper* is index-only) — so computing it over fetched records needs no new value machinery.
- The `TopN(k)` is a **bounded `k`-heap by a scalar value** — like the distance ordering in Phase B, this is **parameter-dependent ordering and therefore must be a concrete physical operator, not a generic Cascades ordering property or a general sort.** It is distinct from a full sort (bounded heap, cost-gated to small filtered sets), but it *does* introduce ordering an index didn't provide, which RemoveSortRule otherwise excludes.
- **Gate (was open question #2, now a hard prerequisite):** Phase D ships only on top of a bounded in-memory post-processor. **First verify `docs/rfc-001-in-memory-post-processing.md` provides one**; if it does, target it; if not, that infrastructure (a bounded TopN enforcer with an explicit Graefe ACK of its own) is a prerequisite RFC, not part of this one.

Cost-based choice: estimate the residual's **selectivity**; route selective → FLAT, broad/absent → resumable ANN. Phase the selectivity source — **(D1)** heuristic (equality on a unique/high-NDV column ⇒ selective); **(D2)** sampling-based per VBASE §4.3 (sample rate 0.001, q-error < 1.1, <1 ms) built on `docs/rfc-table-statistics.md` / RFC-031, not a parallel stats stack. Wire the estimate into `planning_cost_model.go` so a selective FLAT plan can out-cost a vector scan (today `vectorScanCardinality` makes that impossible — §3).

### Phase E — Generalization: joins & multi-column (follow-on)

Because the vector scan is now a composable ordered iterator, a vector K-NN can be the inner of a join (VBASE Q8: similarity join as index-nested-loop under a distance range, ~7,900× over brute force) and multi-column scoring can use VBASE §4.4's native NRA instead of repeated top-k. Scoped as a separate follow-on RFC once A–D land; listed here so the design isn't painted into a vector-only corner.

---

## 5. Correctness

- **Equivalence:** VBASE §3.3 proves filter-during-traversal returns the same rows as the optimal-`k̃` plan; that is our argument that `Limit → Filter → orderedScan` yields the true `k` nearest matching rows.
- **No silent truncation:** budget-cap hits are logged + metered (Phase C).
- **Determinism:** planner output for a fixed query must be stable across runs (query-engine skill); page-by-page continuation (returned-row-limit 1) must equal the unpaged result.

---

## 6. Performance

- Queries **without** a residual (plain TopK) take the existing one-shot path unchanged — Phase A keeps `search()` as a wrapper; zero regression.
- ANN-with-residual: bounded by the budget cap and the 5 s tx (single snapshot); memory is O(heap + batch).
- FLAT: chosen only when the filtered subset is small ⇒ cheap exact scan.
- Stress-1M has no residual-vector query today; add one, and run the 1M stress before/after as a regression guard (CLAUDE.md stress workflow).

---

## 7. Open questions

*Resolved in iteration 1 (see §10):* ~~ordering-property vs plan-node~~ → ordering is intrinsic to the vector-scan node, never a generic property (Phase B). ~~bounded-TopN vs RemoveSortRule~~ → FLAT's `TopN` is a concrete physical operator gated on RFC-001, not a generic sort (Phase D).

Remaining, genuinely open:
1. **Selectivity infra scope.** Ship Phase D1 (heuristic) only for now, or build D2 (sampling-based, VBASE §4.3) on RFC-031 / rfc-table-statistics in the same effort? (Affects whether the cost model can route to FLAT precisely vs heuristically.)
2. **Does RFC-001 in-memory post-processing actually provide a bounded `TopN` enforcer?** Must be verified before Phase D is scheduled; if not, that's a prerequisite RFC with its own Graefe ACK.

---

## 8. Test plan

- **The bug, pinned:** FDB E2E (`vector_search_e2e_fdb_test.go`, testcontainers, `t.Parallel()`, unique subspace): the `user_id/city` query → assert the result is the **k nearest matching rows** (insert decoys nearer in distance but failing the predicate; assert they're excluded and the correct matches returned in distance order). Red on master, green after Phase B.
- **Canonical plan shape (Graefe landing condition):** EXPLAIN-assert the plan is `Limit(k) → Filter(user_id,city) → VectorIndexScan(distance-ordered)` — `Limit` and `Filter` are **above** the scan, `k` is **not** consumed into the scan, and **no `IsIndexOnly` residual** exists. Pin a code comment at the match-candidate: it never emits `DistanceRowNumberValue` as a residual; the distance marker lives only in `RecordQueryVectorIndexPlan`'s binding.
- **Pre/post-filter semantics:** vary selectivity; assert never the "global-top-k ∩ predicate" wrong answer.
- **Plan selection:** EXPLAIN-assert selective predicate → FLAT (`TopN` over value-index), broad/no predicate → resumable `VectorIndexScan` (Phase D).
- **Resumable + pagination determinism:** page-by-page (returned-row-limit 1) over the *materialized* result == unpaged result, 10× for planner stability; assert the continuation is positional (no search re-run).
- **Concurrent-write stability (Torvalds #3):** insert/delete matching rows *between* pages of a paginated filtered KNN; assert the page sequence is consistent with the single-snapshot materialization (no row appears twice / vanishes mid-pagination) — proving the single-transaction-materialize design holds.
- **Honest truncation (Torvalds #2):** a pathologically selective filter that exhausts the budget cap before `k` survivors → assert the cursor returns **`ScanLimitReached`** (not `SourceExhausted`), the `CountSPFreshFilteredTruncated` metric fires, and "only N match" (`SourceExhausted`) is a *distinct, asserted* outcome — never a silent `< k`.
- **Plan selection by selectivity (Phase D):** EXPLAIN-assert selective predicate → FLAT (`TopN` over value-index), broad/no predicate → resumable `VectorIndexScan`.
- **SPFresh fidelity (spfresh-reviewer):** (a) incremental-widen result == one-shot batch-ε-prune result on real data (ε-pruning equivalence); (b) emitted rows are in exact (re-ranked) distance order and re-rank fires only on finalized prefixes (no pre-re-rank emission); (c) a split forcing SEALED→FORWARD mid-traversal → assert no orphaned postings and the same result as a quiescent run (HDR state machine); recall on the churn/soak suite (`bench/spfresh_churn_soak_test.go`) unchanged.
- **Budget/5s-bound stress (Torvalds handoff TODO):** a worst-case selective filter over a large index → assert search + materialize stays within the FDB 5 s tx (returns `ScanLimitReached`, never `TimeLimitReached`/timeout); use it to calibrate the budget-cap value.
- **Fuzz:** the resumable frontier state machine (init/next/widen/terminate/forward) gets a fuzz target.

---

## 9. Divergence note

After this lands, Go's read-side **exceeds** Java for vector queries (sound predicate composition, FLAT, selectivity-costed choice) — an allowed extension; **wire format is untouched** (on-disk SPFresh layout, continuation proto, `DistanceRank` encoding all unchanged; resumable state never serialized). Record a `DIVERGENCES.md` entry: "Vector search composes with arbitrary residual predicates via VBASE-style ordered iterator + cost-based FLAT; Java is one-shot top-k-then-filter."

---

## 10. Review responses — iteration 1

Reviewers: Graefe **NAK**, Torvalds **NAK**, spfresh-reviewer **ACK with findings**. All findings addressed; re-review requested (an ACK covers only the HEAD it reviewed).

| # | Finding | Resolution |
|---|---|---|
| Graefe 1 | "Ordering property" framing incoherent with `IsIndexOnly=true` | Phase B: distance is a **plan-node-intrinsic ordering**, not a generic property; the index-only marker is **not produced as a residual** in the composed form, so the framing is coherent. |
| Graefe 2 | Match-candidate must not introspect residuals | Phase B: match-candidate emits **one canonical ordered-stream form**; a separate **`SinkLimitIntoVectorScanRule`** folds `k` into the scan only when no residual intervenes. |
| Graefe 3 | Missing ordering-property preservation audit | Moot — no generic requestable ordering property is introduced; only vector-aware rules reason about the scan's intrinsic order, so no cross-rule audit is needed. |
| Graefe 4 / Torvalds 4 | Bounded TopN (Phase D) violates RemoveSortRule | Phase D: `TopN` is a **concrete physical operator** (parameter-dependent ordering can't be a property), **gated on RFC-001**; Phase D is demoted to an **optional follow-on**, not a correctness prerequisite. |
| Torvalds 1 | Parameter-dependent ordering can't be a Cascades `Ordering` property | Same as Graefe 1/2 — encoded in `RecordQueryVectorIndexPlan`, exactly Torvalds's resolution (b). |
| Torvalds 2 | Budget cap = silent under-return on pagination | Phase C: truncation surfaces via **`NoNextReason.ScanLimitReached`** (distinct from `SourceExhausted`), not a metric-only signal. |
| Torvalds 3 | Concurrent-write ordering instability across pages | Phase C: **single-transaction snapshot materialization**; pagination is positional over the fixed result, never a cross-tx search re-run. |
| Torvalds 5 | No truncation/concurrency/pagination tests | §8: added honest-truncation, concurrent-write-stability, and pagination-determinism tests. |
| spfresh 2 | ε-pruning halting/equivalence unspecified | Phase A: monotonic incremental re-prune ≡ batch prune; explicit halting condition; pinned by an equivalence test. |
| spfresh 3 | RaBitQ re-rank timing unspecified | Phase A: emit only **exact-re-ranked** rows; re-rank on finalized prefixes, never per raw batch. |
| spfresh 5 | HDR/forward mid-traversal unspecified | Phase A: SEALED→FORWARD re-routes to children in centroid order, no orphaned reads; generation-pinning prevents mid-query flips. |
| Torvalds (Phase A) / spfresh 1,4 | — | **ACK'd** unchanged: resumable cursor and `E=k`,`w`=cluster-vectors relaxed-monotonicity mapping. |
