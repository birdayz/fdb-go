# RFC-167 — Cascades plan determinism: eliminate the nil-inner-shell tie-resolution leak

**Status:** Draft — needs Graefe + Torvalds + @claude + codex ACK (query-engine change).
**Tracks:** RFC-164 NONDETERMINISM line-item (now promoted to its own design).
**Relates to:** RFC-070 (deferred child-linkage / "shells"), RFC-024 (plan-hash cache key), RFC-069 (cost model).

## 1. Problem

The same query can produce 2–3 distinct **plans** across runs (rows are always correct — this is plan churn / continuation-cache churn, medium severity). Canonical repro:

```sql
-- idx_a(a), idx_b(b), idx_c(c)
SELECT id FROM t WHERE a = 5 AND b = 7 AND c = 9
```

picks `IndexScan(IDX_A | IDX_B | IDX_C)` nondeterministically. Two map-iteration sources were already fixed (see §6, Phase 0 — landed): `partialMatchMap` now iterates in insertion order, and match-candidates are index-name-sorted. That fixes a 2-index tie but **not** the multi-equality tie above.

## 2. Root cause (verified against code + Java spec)

Java has **no nil-inner shells**. Its push-through rules memoize a *concrete* inner into a `Reference` at yield time (`PushFilterThroughFetchRule.java:197-205`: `Quantifier.physical(call.memoizePlan(innerPlan))`), prune every `Reference` to **exactly one** final member bottom-up (`Reference.java:210` `Verify.verify(finalMembers.size()==1)`, `CascadesPlanner` OptimizeGroup), and extract via `Iterables.getOnlyElement`. So Java's cost comparator and its **`planHash` tie-break** (`PlanningCostModel.java:320-329`, `StableSelectorCostModel.java:43-56`) always run over a fully-linked, single-member-child plan and form a **true total order**.

Go diverges via **RFC-070 "shells"**: push-through rules yield physical wrappers whose embedded plan has `GetInner()==nil` — **5 nil-inner unary types** (`physicalFetchFromPartialRecordWrapper`, `physicalPredicatesFilterWrapper`, `physicalDistinctWrapper`, `physicalInJoinWrapper`, `physicalMapWrapper`) **plus a 6th set-op case** that retains *stale eager* children rather than nil. The real child lives only on the wrapper's `innerQuant`, relinked at extraction by `WithChildren`. Shells defeat the total order at **three points**:

1. **Selection guard is Fetch-only.** `isNilInnerFetch` (`physical_fetch_from_partial_record_wrapper.go:169`) matches only the Fetch type, so nil-inner Map/Filter/Distinct shells can be stamped as the `NoProperties` winner by the live selection site `OptimizeGroupTask.Run` (`unified_tasks.go:404`) and by `winner_lookup`/`stampOrderingWinners`.
2. **The `planHash` tie-break can't see the buried index.** For a *physical* shell, criterion #17 runs through `costExprHash`→`concretePlanHash(w.plan)` (`planning_cost_model.go:1801-1810` / `:1771-1780`), **not** `deepHashCode` (which is only the *logical* fallback). `concretePlanHash` hashes `w.plan.HashCodeWithoutChildren()` and walks `w.plan.GetChildren()` — which for a nil-inner Fetch/Filter is **empty** — so the buried index is invisible and idx_a/idx_b/idx_c **collapse to the identical value** → `0` (tie). The leaf `RecordQueryIndexPlan.HashCodeWithoutChildren` *does* include the index name (`index_scan.go:171-174`), but the shell's plan hides it. **(Graefe correction: the locus is `concretePlanHash`, not `deepHashCode`, and not the wrapper's `HashCodeWithoutChildren` — see §6 Phase 1.)**
3. **Extraction relink is first-member.** `findPhysicalPlan` (`physical_wrapper.go:313-322`) and the in-memo relinks pick the **first** physical member with no comparator at all.

Net: on a multi-equality tie the winning index is chosen by Go's per-process slice/member iteration order, not by cost.

### 2a. Orthogonal correctness bug exposed by the obvious fix

`WithPrimaryKeyIntersector` (`intersector_primary_key.go:20-23`) **discards its `requestedOrderings` argument** and pairs any two different-candidate matches keyed on the raw primary key, with **no common-ordering gate**. Java's `WithPrimaryKeyDataAccessRule.createIntersectionAndCompensation` (lines 112-200) gates emission on a merged INTERSECTION ordering and rejects leg-pairs lacking a common pk ordering (`noViableIntersection`). So Go **over-generates** `Intersection(idx_a[a=5], idx_b[b>10])`; the `b>10` range leg emits `(b,pk)`-order, violating the pk-sorted-merge precondition (`merge_cursor.go:170-195`) — a **plausible wrong-rows risk** (not yet runtime-confirmed; see Open Questions), and a definite plan-shape regression vs the single index. For the legitimate **all-equality** case every leg is pk-ordered, the intersection is valid, and criterion #3 (residual count) correctly prefers it — matching Java.

**Why this matters for the determinism fix:** the moment shells stop winning artificially, criterion #3 (residual-predicate count, `planning_cost_model.go:224-228`, which runs *before* the #17 hash) makes the **0-residual intersection** the winner — including the invalid `a=5 AND b>10` one. So **the determinism fix is unsafe without the intersection ordering-gate**, and the naive "exclude shells at selection" attempt (already tried and reverted) regressed exactly this.

## 3. Java is the spec for the fix

The faithful property to restore is **not** "insertion order" — it is:

> **Every `Reference` is pruned to exactly one CONCRETE final member; selection ties are then broken by a structural `planHash` (an integer), inside the cost comparator.**

Go already has the port of the tie-break (`costExprHash`/`concretePlanHash`, criterion #17) and half the prune machinery (`OptimizeGroupTask` does `PruneWith(bestFinal)` + `SetWinner`). The defect is purely that **shells hide the concrete index from the hash** and the **guard/relink are not shell-complete**. The fix restores the Java discipline; it must **not** introduce a Go-only mechanism (e.g. tie-breaking on `Explain()` text, or evaluating cost *inside* the hash).

## 4. Architectural decision

**Restore Java's "prune-to-one-concrete-member + structural `planHash` tie-break" discipline for shells.** Concretely:

- Make the shell's structural hash **inner-aware** via a **template-aware `exprConcreteHash`** (mirroring the existing `exprConcreteCost`/`exprConcreteCounts` resolvers that already walk `innerQuant` to the pruned member), called from `costExprHash` in place of the inner-blind `concretePlanHash(w.plan)` — so criterion #17 distinguishes idx_a/idx_b/idx_c **structurally — no cost-in-hash, no recursion into the comparator.** (Critique-driven: the "cost-aware `deepHashCode`" was rejected as circular/Java-unfaithful. **Graefe correction:** the fix is NOT the wrapper's `HashCodeWithoutChildren` — #17 never calls it for a physical shell.)
- Generalize the Fetch-only guard to **all** nil-inner shell types at **every** selection site. The two `OptimizeGroup` implementations are NOT redundant-dead: `planner.go OptimizeGroup`/`OptimizeReferenceTask` is dead for `Plan()` (production) but **live under `Explore()`**, which `FuzzPlanner_Determinism`/`FuzzPlanner_Confluence` + several tests drive — so it must be made shell-aware (or its optimize tail migrated to the unified path), **not blindly deleted** (Graefe correction).
- Fix the foundational scalar-tie primitive (`bestPhysicalChild`→`Reference.GetBest`, `planning_cost_model.go:479-487`) with a structural tiebreak; relink and #17 reuse it.
- **Land the intersection ordering-gate (§2a) FIRST / atomically with the guard-generalization.** Port Java's common-ordering gate so the invalid `a=5 AND b>10` intersection is gone from *generation* before any shell-exclusion changes selection.

> **IMPLEMENTATION FINDING (Phase-1a landed; refines the atomicity above).** The
> inner-aware shell hash ALONE makes the headline multi-equality tie deterministic
> — as *pure tie-resolution*: it surfaces the buried index into criterion #17 so the
> comparator is a true total order, but it does NOT change which member wins (the
> cheapest, a single-index shell, still wins, now deterministically). It does **not**
> exclude shells, so it does **not** expose the intersection — meaning Phase 4's gate
> is **not** required to land with the hash fix. The gate is required only with the
> *guard-generalization* (Phase 1b, which makes shells stop winning → exposes the
> intersection → re-ranking). So the safe decomposition is: **Phase 1a = the hash fix
> (determinism, no plan change, no stress needed); Phase 1b+4 = guard-generalization +
> ordering-gate together (the re-ranking, mandatory 1M stress).** A crude
> "all-columns-equality-bound" gate is INSUFFICIENT for Phase 4 — it breaks
> vector/partition-inequality intersections (`TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix`);
> Phase 4 must use the full ordering machinery (`MergeOrderingsForIntersection`, which
> exists at `rich_ordering.go:680` but is currently unused).

**Explicitly DEFER** the full Java end-state ("eliminate shells, adopt eager `memoizePlan`") to a separate gated RFC (Phase 5). It is the north star but a large, deliberate-per-RFC-070 refactor (8 rule sites, ~15 relink wrappers) with no wire impact — low urgency once Phases 1–4 make behavior deterministic. **The Phase 1–3 helpers are Phase-5-deletable debt and must be marked as such** (code + DIVERGENCES.md) with a tracked expiry, not an indefinite "north star" (per CLAUDE.md "the simplified version rots").

## 5. Cross-engine divergence — decision required

Java tie-breaks with `planHash(PlanHashable.CURRENT_FOR_CONTINUATION)` — the **same hash family embedded in continuation tokens (wire format)**. Go's `concretePlanHash` bottoms out in `plan_hash.go`'s FNV hash, documented as a Go-only cache key (RFC-024). These are **different algorithms**: even after Go is internally deterministic, Go and Java will pick **different tie-winner indexes** for the identical query. Rows are identical (no records-wire impact), but (a) **EXPLAIN silently diverges from Java on the shared query surface**, and (b) the tie-winner feeds `planHash` into continuations.

**This RFC must pick one, explicitly:**
- **(i) Converge** — tie-break using Java-compatible `planHash(CURRENT_FOR_CONTINUATION)` semantics so Go and Java agree on the tie-winner. Required if cross-engine continuation/EXPLAIN parity on ties is wanted.
- **(ii) Intra-Go stability only** — document in the RFC + DIVERGENCES.md that tie-winner index choice is **not** guaranteed to match Java, and confirm no continuation is expected to be re-planned across engines.

**Recommendation:** (ii) for Phases 1–4 (it's the minimal, ships determinism now), with (i) tracked as a follow-up *iff* cross-engine continuation re-planning is ever a requirement. Document loudly; silent FNV divergence is **not** acceptable under the conformance principle.

## 6. Phased plan (revised per critiques)

**Phase 0 — DONE (landed separately).** `partialMatchMap` insertion-order iteration + index-name-sorted match-candidates. Pinned by `TestPlanDeterminism_EqualCostIndexTie`.

**Phase 1 — Inner-aware shell hash + general guard + Explore-path migration.** *This is the phase that actually fixes the headline query* (the completeness critic showed the guard alone is a no-op for all-shell groups).
- **Inner-aware hash at the right locus (Graefe-corrected).** Add a template-aware `exprConcreteHash` mirroring `exprConcreteCost` (`planning_cost_model.go:1237`) / `exprConcreteCounts` (`:1323`) — for a nil-inner shell, resolve through `e.GetQuantifiers()`→`innerQuant`→the single pruned member and fold that member's `concretePlanHash` (which already includes index name, reverse, covering, strictlySorted). Call it from `costExprHash` (`:1801`) in place of the inner-blind `concretePlanHash(w.plan)` for shells. **Do NOT edit the wrapper's `HashCodeWithoutChildren` — #17 never calls it for a physical shell, so that would be a no-op on the live path.** Pure structural; no comparator re-entry.
- Add `isNilInnerShell` (all 5 unary nil-inner types via the `{GetInner()}` stub check) and replace `isNilInnerFetch` at every selection site (`unified_tasks.go:404`, `winner_lookup.go:58/89`, `planner.go:1036`), **including the currently-unguarded ordered branch** of `OptimizeGroup`.
- **Make the legacy `planner.go` `OptimizeGroup`/`OptimizeReferenceTask` path shell-aware too — do NOT delete it (Graefe correction).** It is dead for `Plan()` but LIVE under `Explore()`, which `FuzzPlanner_Determinism`/`FuzzPlanner_Confluence` + `physical_properties_test`/`planner_test` drive; deleting it breaks the determinism fuzzer §7 relies on. Either migrate `Explore()`'s optimize tail to the unified `OptimizeGroupTask` path or apply the same generalized guard + real-over-shell swap there. Sequence this explicitly within Phase 1.
- Fix the `bestPhysicalChild`→`GetBest` scalar tie (`planning_cost_model.go:479-487`) with the structural tiebreak as the foundational primitive.
- **Gate:** the 3-index query is deterministic across 50 in-process **and** N subprocess runs *before* Phase 1 is "done".

**Phase 2 — Deterministic extraction relink (only what remains needed).** Route `WithChildren` relinks through a cost-winner/structural resolver instead of bare `findPhysicalPlan`; inject the tiebreak through the `BestMemberSelector`/`planner.BestMember` boundary (package `cascades`), since `extract.go` lives in package `properties` and **cannot** import `cascades` (import cycle — the synthesized "reuse the planning total order in `extract.go`" is infeasible as written). Much of this becomes moot once child refs are singletons (Phase 1/3); add only the demonstrably-needed pieces.

**Phase 3 — Prune every physical child ref to one concrete member.** Verify no physical ref reaches extraction with a multi-member exploratory `members` slice that `findPhysicalPlan` would scan first (`AllMembers = members ++ finalMembers`). This makes the first-member scan moot in production, mirroring Java's `getOnlyElement`. The live extraction path is `ExtractBestPlanFromSelector`→`rebuildExpressionFromSelectorVisited` (singleton refs), so the real tie is the per-Reference `Winner` stamp + the `extract.go` `GetBest` fallback — both must carry the structural tiebreak.

**Phase 4 — Ordering-gate the primary-key intersector (CORRECTNESS — land with Phase 1b).** Port Java's common-ordering gate into `WithPrimaryKeyIntersector`: stop discarding `requestedOrderings`; compute the merged INTERSECTION ordering (`Ordering.merge(...,INTERSECTION)` + `enumerateSatisfyingComparisonKeyValues` + `isCompatibleComparisonKey`, `WithPrimaryKeyDataAccessRule.java:112-200`) and emit only when all legs share a common pk ordering. Drops `Intersection(idx_a[=], idx_b[>])`; keeps the all-equality intersection.

> **IMPLEMENTATION BLOCKER (found while attempting Phase 4 — see OQ#6).** Two leg-level gate
> formulations were tried and reverted, both breaking `TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix`:
> (a) a crude "every index column equality-bound" check; (b) the *proper* per-leg check
> `computeWrapperRichOrdering(leg).Satisfies(pkRequested)`. Both correctly drop the value-range
> leg (`idx_b[b>10]` → `(b,pk)` order) but ALSO drop the **vector leg** of a partition-inequality
> vector intersection (RFC-046): the vector scan's `HintRichOrdering` reports *distance-rank*
> order, not pk order, so any pk-order leg gate excludes it → the query becomes unplannable. Yet
> on master that intersection plans (and presumably executes correctly). So **a vector leg validly
> participates in a pk-keyed intersection despite its static ordering ≠ pk** — the execution
> (vector cursor / RFC-046 partition merge) must reconcile it. The correct gate cannot be a blanket
> per-leg pk-order check; it needs Java's `enumerateSatisfyingComparisonKeyValues` semantics, which
> account for how each candidate type participates, AND resolution of OQ#6 (does the vector leg's
> records actually arrive pk-ordered at the merge, or does the intersection plan re-sort?). **Phase 4
> is blocked on OQ#6; do not ship a leg-level pk-order gate.**

**Phase 5 — North star (separate gated RFC, tracked with an expiry).** Eliminate shells: migrate the 8 push-through sites to eager `memoizePlan` and delete the relink machinery. Pure-internal (no wire impact). Documented in DIVERGENCES.md.

## 7. Regression nets

- **PRIMARY (immune to per-process seeding):** a **structural** relink/winner unit test — build a `*Reference`, `InsertFinal` members in scrambled order (idx_c, idx_a, idx_b), assert the relinked inner / stamped winner equals the comparator winner regardless of insertion order. Pins "extraction picks the cost winner, not `members[0]`" without depending on flushing out map randomness.
- **Determinism property tests (full `Plan()`, real `PlanContext`):** table-driven N∈{2,3,4,5} equality-bound single-column indexes, **plus reverse-scan ties, IN-list ties, and set-op ties**. Assert `Explain()` stability across 50 in-process runs **and N subprocesses** (`exec go test -run … -count=1`) — the subprocess harness is **mandatory** (the Phase-0 fixes converted map ranges to slices whose order may be pointer/seed-stable within a process but flip across processes; in-process loops cannot catch that).
- **Full-pipeline net:** `FuzzPlanner_Determinism` runs only `Explore()` with a nil `PlanContext` and asserts member-*count* — blind to which index wins. Add a determinism fuzz driving full `Plan()` with a real `NewPlanContextFromIndexDefs` + multi-column `ComparisonEquals`, asserting `Explain` stability.
- **Set-op net:** UNION/INTERSECTION over tied single-column indexes (set-op shells + `ChildrenAsSet` dedup are a second, independent nondeterminism source).
- **Correctness net (Phase 4):** an **FDB integration test** proving the pk-keyed sorted-merge intersection is not emitted for a range leg (or returns correct rows if reachable elsewhere) — the merge-cursor precondition makes this a wrong-rows risk, not just shape.
- **Test-oracle constraint:** the comparator tiebreak MUST remain a structural **integer** hash (`planHash` semantics). `Explain()`-string comparison is permitted **only** in test oracles, never in selection logic.

## 8. 1M stress — MANDATORY (critique-corrected)

The synthesized design claimed "1M stress NOT required for Phases 1–3 — tie resolution only." **The regression critic refuted this:** once shells stop winning artificially, criterion #3 (residual count) re-ranks the all-equality query from a single-index scan to a **3-way Intersection + sorted-merge** — a broad index-selection + execution-shape change (3 scans + merge vs 1 scan + residual filter) with real I/O/latency impact, across **every** mixed/multi-predicate query. Per CLAUDE.md's planner-change rule, run the **master-vs-branch 1M stress comparison** (row counts + durations, worktree baseline) for the combined Phase-1+4 (and 2/3) landing. The targeted FDB row-correctness test is necessary but not sufficient.

## 9. Open questions (resolve before/within implementation)

1. **Is the intersection the post-fix winner of the headline query?** Empirically dump the competing `SelectExpression` ref members of `a=5 AND b=7 AND c=9` after Phase 1 and confirm whether criterion #3 elects the 3-way Intersection (it almost certainly does) — this decides how much of the #17 single-index disambiguation work is even on-path.
2. **Is the pk-merge wrong-rows risk real or already prevented?** Confirm at runtime whether `MaximumCoverageMatches` or an inserted sort already stops the non-pk-ordered range leg from reaching `merge_cursor`'s sorted-merge precondition. (Currently PLAUSIBLE, not confirmed.)
3. **Does Go have `Ordering.merge(INTERSECTION)` / `enumerateSatisfyingComparisonKeyValues`?** If not, building it 1:1 from Java is a Phase-4 prerequisite.
4. **Set-op staleness:** verify set-op `WithChildren` rebuilds children from the fresh quantifiers rather than retaining the stale eager `w.plan` (`physical_wrapper.go:1351/1450`, `physical_unordered_union_wrapper.go:62`); the stale-children retention is a latent correctness smell independent of determinism.
5. **Cross-engine:** confirm no continuation is expected to be re-planned across Go/Java engines (decides §5 (i) vs (ii)).
6. **Vector-leg participation in a pk-keyed intersection (BLOCKS Phase 4).** `TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix` (RFC-046) plans an intersection whose **vector** leg reports *distance-rank* order from `HintRichOrdering`, not pk order — yet the pk-keyed `RecordQueryIntersectionPlan` is emitted and (apparently) correct. Both a crude and a proper per-leg pk-order gate were tried and reverted because they drop this vector leg → unplannable. Resolve: does the vector cursor actually yield pk-ordered records at the merge (so the gate should consult execution-order, not `HintRichOrdering`), or does the intersection plan re-sort, or is the vector intersection a different (non-sorted-merge) shape that should be exempt from the pk-order gate entirely? Until this is answered, **no leg-level pk-order gate is correct** — the fix must follow Java's `enumerateSatisfyingComparisonKeyValues`, which encodes per-candidate-type participation. (OQ#2 — whether the value-range pk-merge is even reachable / wrong-rows — should be confirmed first; if `MaximumCoverageMatches` already prevents it, Phase 4 may reduce to an optimality gate, not a correctness one.)

## 10. Status of the already-landed work

Phase 0 (the two map-iteration fixes) is implemented, validated (FuzzPlanner_Determinism 831k execs, full sqldriver + no-FDB green), and pinned by `TestPlanDeterminism_EqualCostIndexTie`. It is the foundation; the remaining phases close the multi-equality / shell / intersection layer.

---

*Investigation: 7 facet readers + synthesis + 3 adversarial critics (regression / completeness / Java-fidelity), ~1.0M tokens, all findings code-cited. This RFC reflects the critique-revised design, not the first synthesis.*
