# RFC: Cascades Plan Extraction — Architectural Principles

**Status:** In Progress  
**Date:** 2026-05-21  
**Reference:** Graefe, "The Cascades Framework for Query Optimization" (IEEE Data Engineering Bulletin, 1995)

## Ground Truth

The Cascades framework has a single authoritative principle for plan selection:

> **Physical operator nodes reference equivalence classes (groups), not specific plans. Plan assembly traverses the group DAG at extraction time, picking the cheapest member at each group.**

This means:

1. **Implementation rules produce physical expressions whose children are References (groups).** They do NOT embed specific inner plans. The inner plan is unknown at rule-fire time — it gets chosen at extraction time.

2. **Cost estimation during planning uses operator-local heuristics (HintCost) plus child group costs.** The child cost comes from the group (Reference), not from a specific plan. This is why cost comparison between alternative physical plans within a group is valid — they all use the same child group costs.

3. **Plan extraction walks the Reference DAG top-down.** At each Reference, it picks the cheapest physical member (by EstimateCost). For each child quantifier, it recurses into the child Reference and picks the cheapest there too. The final plan tree is assembled by `WithChildren`, which rebuilds each physical wrapper with freshly-extracted children.

## Go Implementation

### Architecture Split

Java's `RecordQueryPlan` implements `RelationalExpressionWithChildren` — physical plans ARE expressions. Our Go port has a split:

- `RelationalExpression` — the planner's expression (logical or physical wrapper)
- `RecordQueryPlan` — the executor's plan (concrete operator tree)
- Physical wrappers bridge the two: they implement `RelationalExpression` and carry an embedded `RecordQueryPlan`

The embedded plan is populated at rule-fire time with whatever inner plan `findPhysicalPlan` returns. **This plan may be stale by extraction time** — other rules may have produced better alternatives in the child References. This is safe because:

- During planning, the embedded plan is only used for cost estimation (HintCost), which accesses operator-local state (predicates, sort keys, etc.) and child group costs — not the specific inner plan structure.
- At extraction time, `WithChildren` rebuilds the plan from freshly-extracted children.

### The WithChildren Contract

Every non-leaf physical wrapper MUST implement `WithChildren` as follows:

```go
func (w *physicalXxxWrapper) WithChildren(qs []expressions.Quantifier) (...) {
    newPlan := w.plan  // fallback to stale plan
    if childPlan := extractChildPlan(qs[0]); childPlan != nil {
        // Rebuild with fresh child — operator-local state from w.plan,
        // child plan from the extracted quantifier.
        newPlan = plans.NewRecordQueryXxxPlan(w.plan.GetLocalState(), childPlan)
    }
    return &physicalXxxWrapper{plan: newPlan, innerQuant: qs[0]}, nil
}
```

Key points:
- `extractChildPlan(q)` gets the plan from a quantifier's singleton Reference (populated by recursive extraction)
- Operator-local state (predicates, sort keys, aliases, etc.) comes from `w.plan` getters — it doesn't change
- Only the child plan changes — that's what extraction picked from the child group

### Cost Estimation

`firstMemberCostMemoised` (in `properties/cost.go`) prefers `FinalMembers` over `Members` when estimating child Reference costs. During the PLANNING phase, child References have physical wrappers (from bottom-up implementation). Using their HintCost produces consistent estimates across the physical plan tree.

`findPhysicalExpr` uses `EstimateCost` to compare physical alternatives within a group. No type-based preferences (FlatMap vs NLJ) — the cost model decides.

### Invariants

1. **All members of a Reference produce the same result set.** If they don't (e.g., a bare scan vs a projected scan in the same group), that's a bug in the rewrite rules, not in the cost model.

2. **WithChildren must rebuild the embedded plan from fresh children.** Copying `w.plan` unchanged produces structurally inconsistent plan trees where the embedded plan references a different sub-tree than the quantifier DAG.

3. **Cost-based selection is safe across all query shapes** — CTEs, recursive CTEs, multi-table joins, derived tables — because plan assembly happens at extraction time, not at rule-fire time. The embedded plan during planning is a cost-estimation hint, not the execution plan.

## Diagnosed Blocker: Plan/Reference DAG Divergence

**Discovered 2026-05-21.** WithChildren plan rebuild was implemented for ALL 22 wrappers and caused 4 test regressions (left join returning cross-join results, recursive CTE infinite loop, GROUP BY returning ungrouped results, ambiguous column star failure). Root cause investigation follows.

### The Problem

Go's implementation rules wire specific child plans directly into the embedded `RecordQueryPlan`, but the wrapper's quantifiers point at generic Reference groups. The plan tree and the Reference DAG have **different structures**:

```
Reference DAG (what quantifiers see):
  FlatMap wrapper → inner quant → Reference{ Scan(ORDERS) }

Plan tree (what the executor runs):
  FlatMapPlan → inner plan = IndexPlan(ORDERS, customer_id=$outer)
```

The implementation rule creates a correlated `IndexPlan` (for the join predicate `customer_id = $outer.id`) and embeds it in the FlatMap plan. But the inner quantifier ranges over the base scan Reference (from `call.MemoizeExpression(innerExpr)`) which contains only the uncorrelated `Scan(ORDERS)`.

When WithChildren rebuilds the plan, `extractChildPlan` finds the base scan in the singleton Reference (created by extraction), and the FlatMap is rebuilt with `Scan(ORDERS)` instead of `IndexPlan(ORDERS, customer_id=$outer)`. The correlation is lost → cross join.

### Why This Happens

In Java's Cascades, the plan IS the expression. `RecordQueryFlatMapPlan` extends `RelationalExpressionWithChildren`. Its children ARE References. The correlated index scan is a member of the inner Reference's group. There's no divergence.

In Go's architecture, physical wrappers and plans are separate objects. The implementation rules construct the plan inline:
```go
indexPlan := cand.ToScanPlan(prefix, reverse)
flatMapPlan := plans.NewRecordQueryFlatMapPlan(outerPlan, indexPlan, ...)
leftQ := expressions.ForEachQuantifier(call.MemoizeExpression(leftExpr))
rightQ := expressions.ForEachQuantifier(call.MemoizeExpression(rightExpr))
call.Yield(newPhysicalFlatMapWrapper(flatMapPlan, leftQ, rightQ))
```

Here `rightExpr` is the base scan expression, memoized into the scan group. But `indexPlan` is the correlated plan created inline. The two diverge.

### Affected Operators

All operators where the implementation rule wires a child plan that diverges from the child Reference:

- **FlatMap** — inner plan is a correlated index scan; inner Reference is the base scan group
- **Nested Loop Join** — similar to FlatMap for correlated inner scans
- **Streaming Aggregation** — inner plan is an ordered index scan; inner Reference may be the base scan group
- **InJoin** — inner plan is an index scan; inner Reference may be the base scan group
- **Recursive DFS/Level Union** — child plan carries temp-table correlation
- Potentially any wrapper where the rule calls `findPhysicalPlan(innerRef)` to get the plan but `MemoizeExpression(innerExpr)` to get the quantifier

### Current State (2026-05-21)

**22 of 27 non-leaf wrappers** have proper WithChildren rebuild.

**REBUILD (proper Cascades extraction):**
FlatMap, PredicatesFilter, Filter, Distinct, TypeFilter, Insert, Delete, Update, Map, Limit, InMemorySort, FirstOrDefault, DefaultOnEmpty, FetchFromPartialRecord, TempTableInsert, Union, Intersection, MergeSortUnion, MultiIntersection, UnorderedUnion, InUnion, Projection

**CONDITIONAL REBUILD (leaf-replaceable child only):**
InJoin (rebuilds when child is scan/filter/sort/etc., passthrough for correlated children)

**PASSTHROUGH (correlated, need rule fixes):**
NLJ, StreamingAgg, RecursiveDfsJoin, RecursiveLevelUnion

**Bugs fixed:**

1. `RecordQueryScanPlan.EqualsWithoutChildren`/`HashCodeWithoutChildren` now include `scanComparisons`. Previously, correlated scans (with scan comparisons) and uncorrelated scans (without) had the same hash/equals, causing `memoizeLeaf` to merge them into the same Reference — violating the Cascades invariant that all members of a group produce the same result set.

2. `PartitionBinarySelectRule` now preserves joinType when yielding the repartitioned SelectExpression. Previously it used `BuildSelectWithResultValue` which defaults to `JoinInner`, contaminating LEFT OUTER join groups with INNER join alternatives — a Cascades invariant violation (all members of a group must produce the same result set).

3. `NormalizePredicatesRule` now preserves joinType when yielding normalized predicates. Same invariant violation as #2.

4. `DecorrelateValuesRule` now preserves joinType.

5. `ExtractBestPlan`'s `rebuildWithFreshChildren` now preserves joinType when rebuilding SelectExpressions.

6. `ImplementProjectionRule` now uses `expressions.InitialOf(innerExpr)` instead of `call.MemoizeExpression(innerExpr)` to create a dedicated singleton Reference for the inner expression. Previously, `MemoizeExpression` returned the ORIGINAL inner Reference (the join group), which contained many alternative physical plans with different predicate structures. At extraction time, cost-based selection could pick a different alternative than the one chosen at rule-fire time — producing structurally inconsistent plan trees (e.g., predicates migrating between NLJ levels, INNER chosen over LEFT OUTER).

7. `InUnion.WithChildren` now preserves `maxSize` using `NewRecordQueryInUnionPlanWithMaxSize`.

### Fix Path for Remaining 5 Wrappers

The remaining 5 wrappers (NLJ, InJoin, StreamingAgg, RecursiveDfsJoin, RecursiveLevelUnion) are all correlated operators whose inner plans diverge from the Reference DAG. The NLJ rule creates correlated index scans inline; the inner quantifier's Reference contains only the uncorrelated base scan. Enabling rebuild causes extraction to extract the uncorrelated scan, breaking correlation.

The proper Cascades fix requires rule-level changes:

1. **Implementation rules must populate inner References with correlated plans.** Either use `expressions.InitialOf(innerExpr)` for dedicated singleton References (as done for Projection), or ensure the inner Reference contains the correlated expression.

2. **NLJ attempted rebuild result:** 6 test failures — all EXISTS/NOT EXISTS related. The NLJ rule has ~10 code paths creating inner quantifiers via `call.MemoizeExpression`. Fixing each to use dedicated References is feasible but requires careful per-path analysis.

Remaining rules to fix: `ImplementNestedLoopJoinRule`, `StreamingAggFromIndexRule`, `ImplementRecursiveDfsJoinRule`, `ImplementRecursiveLevelUnionRule`.

### Extraction Blocker Analysis (2026-05-22)

The extraction architecture has a fundamental gap: `extractBestPlanFromSelectorVisited` trusts the EXPLORE-phase `bestMember` stamp when it's physical (line 141: `if !isPhysicalPlan(best)`), and **skips FinalMembers entirely**. Since BatchA rules (ImplementFilterRule, ImplementIndexScanRule, etc.) fire during EXPLORE and produce physical wrappers, the EXPLORE-phase stamp is always physical for filter/scan References. The PLANNING-phase FinalMembers (which contain index scans, InJoin, streaming agg from data access + implementation rules) are never considered.

**Impact:** This blocks ALL planner-driven index pushdown: IN-list InJoin(IndexScan), multi-predicate PK scan, covering index scans. The planner correctly produces optimal plans in FinalMembers, but extraction can't select them.

**Attempted fixes and why they fail:**

1. **Prefer FinalMembers over selector:** Picking `bestFrom(finals, CostLessWith(stats))` causes correctness regressions in CTE, JOIN, UNION queries. FinalMembers include physical plans with stale child references (from `FinalizeExpressionsRule` + passthrough `WithChildren`). Cost-based selection picks these stale plans.

2. **Prefer leaf-only FinalMembers:** Bare leaf scans (IndexScan, Scan) without wrapping operators are picked at the wrong level — DISTINCT/SORT/FILTER above them are lost.

3. **Suppress physical bestMember for References with PartialMatches:** Correctly defers to FinalMembers, but FinalMembers contain the same problematic stale-child plans from `FinalizeExpressionsRule`.

4. **De-prioritize physical in OptimizeReferenceTask:** Changes UNION/CTE plan selection causing correctness regressions.

**Root cause:** `FinalizeExpressionsRule` wraps ALL exploratory members (including physical wrappers) into FinalMembers via `WithQuantifiers` passthrough. This populates FinalMembers with stale copies that look valid to the cost model but produce wrong results when extraction rebuilds their children.

**The Java-aligned fix:** Move BatchA physical implementation rules from EXPLORE phase to PLANNING phase. This ensures EXPLORE only produces logical alternatives, the selector's bestMember is logical, and extraction naturally falls through to PLANNING-phase FinalMembers. This is a structural refactor requiring BatchA rules to be re-typed as ImplementationRules (yielding into FinalMembers instead of exploratory Members).
