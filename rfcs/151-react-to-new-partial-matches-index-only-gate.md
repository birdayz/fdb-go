# RFC-151 — React to new partial matches + adopt Java's `!isIndexOnly()` ImplementFilterRule gate

**Status:** Draft.
**Item:** TODO.md §7.7 follow-up — the match-level index-only consumption stand-in (the second of RFC-148's two named deferrals; the first, the winner-selection invariant, became RFC-150 B1a).
**Reviewers:** **Graefe** (data-access / matching / task scheduling — REQUIRED, this is the matching+scheduling surface) + Torvalds + codex + @claude.
**Classification:** query-engine **parity**. No wire impact. Plan-shape sensitive (vector recall): per-shape EXPLAIN + plandiff byte-identical + 1M stress.

## 0. Problem — Go reacts to new expressions, Java reacts to new partial matches

Go's `pushDataAccessTasks` (the data-access consumption that turns a `PartialMatch` into an index scan) runs **inline at the start of `ExploreExprTask.Run`** (`unified_tasks.go:114`), i.e. BEFORE the matching rules (`MatchIntermediateRule`/`MatchLeafRule`, fired as later `TransformExprTask`s) have seeded this round's partial matches on the ref. So a match seeded during a ref's exploration is consumed only by a **later, incidental re-exploration** of that ref — and today the only thing that re-triggers it is an unrelated rule yielding a new physical EXPRESSION member (e.g. `ImplementFilterRule` realizing a `RecordQueryPredicatesFilterPlan`).

Java does not rely on this accident. `CascadesPlanner.executeRuleCall` (`CascadesPlanner.java:1058-1062`) iterates `ruleCall.getNewPartialMatches()` and schedules a follow-up `AdjustMatch`/data-access task **per new partial match**; partial-match yields (`CascadesRuleCall.yieldPartialMatch`, `:254-268`) are first-class planner artifacts. Go's planner reacts to new expression members but **never to new partial matches** — that is the divergence.

### Why it surfaces now: the index-only vector filter

For `SELECT doc_id FROM docs WHERE zone='z1' QUALIFY ROW_NUMBER() OVER (… ORDER BY euclidean_distance(embedding,q)) <= 3`, the logical tree is `Project([doc_id], LogicalFilter((zone='z1' AND <DistanceRank> <= 3), scan))`. The vector candidate's partial match binds BOTH `zone` and the index-only `DistanceRank` (verified — `flattenConjuncts` already handles the single `AndPredicate`). But the match is consumed only after `ImplementFilterRule` yields a physical `PredicatesFilter` that re-triggers exploration of the filter ref.

That accidental coupling blocks the Java alignment we want: Java's `ImplementFilterRule` binds `all(anyCompensatablePredicate())` where the extractor is `!isIndexOnly()` (`ImplementFilterRule.java:62`, `QueryPredicateMatchers.java:66-68`) — it does NOT fire for an index-only predicate. Adopting that gate in Go removes the incidental re-trigger, so the fully-bound vector match is never consumed → the query fails to plan. Go has had to keep a Go-only late net (`validateNoIndexOnlyResidual`) + a downstream proxy guard (`compensationSafeForYield`'s index-only branch) instead of the structural Java gate.

## 1. Design

**B1 — React to new partial matches (the load-bearing fix).** In `TransformExprTask.Run`'s `fireExprRule`, after `t.Rule.OnMatch(call)`, if the rule GREW `t.Ref`'s partial-match set during PLANNING, re-run `p.pushDataAccessTasks(t.Ref, t.Expr)` — the Go equivalent of Java reacting to `getNewPartialMatches()`. Self-bounded by the existing match-growth re-entry guard inside `pushDataAccessTasks` (`planner.go`, RFC-148 §3c). This is non-disruptive on its own (plandiff byte-identical; it only adds an EARLIER consumption of matches that an incidental re-exploration would have consumed anyway).

**B2 — Adopt Java's `!isIndexOnly()` ImplementFilterRule gate.** `ImplementFilterRule.OnMatch` returns early if any predicate carries an index-only value (`predicateContainsUncompensatableValues`), 1:1 with Java. A `RecordQueryPredicatesFilterPlan` cannot evaluate an index-only predicate at runtime; the gate keeps such a filter unrealized. With B1 in place, the legitimate vector scan is still produced (the match is consumed directly).

**B3 — RETAIN `validateNoIndexOnlyResidual` as the catch-all backstop; add a logical-side clean error.** B2 gates only ONE of Go's physical-filter builders (`ImplementFilterRule`). An index-only `DistanceRank` in the ORIGINAL query — notably inside a JOIN, where it is a `SelectExpression` predicate — reaches a physical residual via `ImplementSimpleSelectRule` / the NLJ residual builder, which B2 does not see (Graefe + Torvalds both reproduced this leak when the net was prematurely retired). So the physical-walk net STAYS, running unconditionally on the realized plan — the one place that covers every physical-filter path. `findIndexOnlyLogicalResidual` is ADDED for the complementary case: when B2 leaves the best plan *non*-physical, the physical walk sees nothing, so the logical walk surfaces the same clean `UnplannableIndexOnlyResidualError`. Fully retiring the net requires gating ALL builders (`ImplementSimpleSelectRule`, NLJ, …) or retiring them (the `ImplementIndexScanRule` retirement) — out of scope here.

**B4 — Delete `compensationSafeForYield`'s per-predicate index-only branch.** Redundant: a yielded index-only `LogicalFilter` only reaches `ImplementFilterRule` (now gated by B2), and the catch-all net backstops any physical leak. The **inner-scan guard** (vector/aggregate inner) STAYS — it protects a *normal* residual over a top-K/grouping (the `TrailingEqualityResidual` shape), which neither B2 nor the net covers. (Graefe confirmed: the leak is the original-query join Select, not a compensation, so B4 is not implicated.)

## 2. Test plan

- **Vector recall**: all `TestVectorPlan_*` (incl. `QualifyPlansToVectorScan` → clean `Project(VectorIndexScan(BY_DISTANCE, rank<=3))`; `MetricMismatchDoesNotMatchVector` (single-table, LogicalFilter→gated rule→logical check); **`MetricMismatchInJoinDoesNotLeak`** (join, Select→ungated builder→physical net) → both clean `UnplannableIndexOnlyResidualError`); FDB `TestFDB_VectorSearch_MultiPartition*` (incl. `TrailingEqualityResidual` stays unplannable via the inner-scan guard).
- **plandiff byte-identical** across the corpus (B1 alone, and B1+B2+B3+B4) — confirms no non-vector plan shape moves.
- **1M stress** before/after.
- New unit test: `findIndexOnlyLogicalResidual` (nested-under-quantifier + clean-tree).

## 3. Gate & risk

**Graefe ACK on RFC + impl.** Risk is vector recall (a matching/scheduling change on the index-only path). Mitigation: B1 is non-disruptive in isolation (proven byte-identical); B2's only behavioral change is the metric-mismatch error TYPE (reconciled by B3); the inner-scan top-K guard is untouched. This is strictly LESS Go-only machinery than before (one late net + one proxy branch deleted; one Java-faithful scheduling reaction added).

## 4. Scope

**In:** the partial-match re-trigger; the `!isIndexOnly()` gate; the logical-side error; deletion of `validateNoIndexOnlyResidual` + `compensationSafeForYield`'s index-only branch. **Out:** aggregate `UnmatchedAggregateValue` consumption beyond what the gate already covers; anything RFC-148/150 owns; the `tryFlatMapPlan` retirement (RFC-150 Phase 2b).
