# RFC-004: Winner-Based Plan Selection (Eliminate findPhysicalPlan)

**Status:** Draft  
**Author:** unified-planning-phase  
**Java reference:** fdb-record-layer 4.11.1.0, `Reference.java`, `RecordQueryPlanner.java`

## Problem

15 implementation rules select child physical plans via `findPhysicalPlan(innerRef)`, which returns the **first** physical member of a Reference by insertion order. This violates the Cascades contract: plan quality depends on which rule fires first, not on the cost model.

### Concrete regressions caused by this pattern

| Query pattern | What happened | Root cause |
|---|---|---|
| `WHERE a=val ORDER BY b` | Full primary scan (2.93s) instead of IndexScan(a=val)+InMemorySort(b) (3.5ms) | `ImplementInMemorySortRule` wrapped the first physical plan (full scan), missing the selective IndexScan |
| `GROUP BY a HAVING ... ORDER BY a` | Full scan + InMemorySort (10s) instead of AggregateIndex (104ms) | `ImplementFilterRule` wrapped the first physical plan at GroupBy ref (InMemorySort variant), missing the IndexScan variant |

Both were fixed with targeted patches (wrapping all members in ImplementFilterRule, wrapping restricted Fetch plans in InMemorySortRule). But the underlying pattern exists in 15 rules:

```
rule_implement_filter.go          rule_implement_projection.go
rule_implement_limit.go           rule_implement_streaming_agg.go
rule_implement_typefilter.go      rule_implement_in_memory_sort.go
rule_implement_insert.go          rule_implement_delete.go
rule_implement_update.go          rule_implement_union.go
rule_implement_intersection.go    rule_implement_temp_table_insert.go
rule_implement_nested_loop_join.go (3 call sites)
rule_implement_recursive_dfs_join.go
rule_implement_recursive_level_union.go
```

### Why targeted patches don't scale

The ImplementFilterRule fix (wrap all inner members) works for unary operators. For joins, the equivalent is N×M: for each outer physical plan × each inner physical plan, yield an NLJ. With 5 outer and 5 inner alternatives, that's 25 NLJ variants — most of which are wasteful. The Cascades answer is to consider only the **winner** from each child, not the full member list.

### Band-aid rules that masked this

Four Go-only rules (`PushFilterThroughSortRule`, `PullFilterAboveSortRule`, `PushFilterThroughProjectionRule`, `PullFilterAboveProjectionRule`) restructured the logical tree so that the "first" physical plan at each Reference happened to be the right one. They were removed because they caused correctness bugs (IN-predicate ordering, projection column resolution). The regressions surfaced because the first-plan-only pattern was exposed without the tree restructuring.

## Java Architecture

### Reference winners

Java's `Reference` (equivalence group) maintains a **winner map** keyed by physical properties:

```java
// Reference.java
class Reference {
    Map<PlanProperty<?>, Map<?, RecordQueryPlan>> winners;

    // Get the best physical plan satisfying the given properties.
    Optional<RecordQueryPlan> getWinner(RequestedOrdering ordering);

    // Set the winner for the given properties.
    void setWinner(RequestedOrdering ordering, RecordQueryPlan plan);
}
```

After all implementation rules fire on a Reference during the bottom-up pass, Java's planner runs a **winner selection pass** that evaluates each physical member's cost under each required ordering and stores the cheapest as the winner.

### How implementation rules use winners

Java's implementation rules don't call `findPhysicalPlan`. They ask the child Reference for its winner under the required physical properties:

```java
// Conceptual (simplified from Java source)
class ImplementFilterRule {
    void onMatch(call) {
        // The filter is ordering-transparent — pass parent's
        // requested ordering through to child.
        for (RequestedOrdering ordering : call.getRequestedOrderings()) {
            RecordQueryPlan childWinner = innerRef.getWinner(ordering);
            if (childWinner != null) {
                call.yield(new FilterPlan(predicates, childWinner));
            }
        }
    }
}
```

For NLJ, the rule asks each child for its winner independently — no N×M explosion:

```java
class ImplementNestedLoopJoinRule {
    void onMatch(call) {
        RecordQueryPlan outerWinner = outerRef.getWinner(requiredOrdering);
        RecordQueryPlan innerWinner = innerRef.getWinner(PRESERVE);
        if (outerWinner != null && innerWinner != null) {
            call.yield(new NLJPlan(outerWinner, innerWinner));
        }
    }
}
```

### Plan extraction

Plan extraction walks top-down. At the root, the planner picks the winner for the query's top-level required ordering. Each winner's children are themselves winners at their References, forming a coherent plan tree where every node is the cost-optimal choice for its required properties.

## Proposed Go Implementation

### Phase 1: Winner infrastructure on Reference

Add to `expressions.Reference`:

```go
type Reference struct {
    // ... existing fields ...

    // winners maps RequestedOrdering → best physical expression.
    // Populated by stampWinners after all implementation rules fire.
    winners map[string]expressions.RelationalExpression
}

func (r *Reference) GetWinner(ordering *RequestedOrdering) expressions.RelationalExpression
func (r *Reference) SetWinner(ordering *RequestedOrdering, expr expressions.RelationalExpression)
```

The map key is a canonical string representation of the RequestedOrdering (e.g., `"[A ASC, B DESC]"` or `"PRESERVE"`).

### Phase 2: Winner selection pass

After all implementation rules fire on a Reference during `implementBottomUp`, run `stampWinners(ref, costModel)`:

```go
func stampWinners(ref *Reference, costModel CostModel) {
    // For each RequestedOrdering in the constraint map for this ref:
    orderings := getRequestedOrderingsForRef(ref)
    for _, ordering := range orderings {
        var bestExpr expressions.RelationalExpression
        var bestCost Cost

        for _, m := range ref.AllMembers() {
            ph, ok := m.(physicalPlanExpression)
            if !ok {
                continue
            }
            if !satisfiesOrdering(ph, ordering) {
                continue
            }
            cost := costModel.Cost(ph)
            if bestExpr == nil || cost < bestCost {
                bestExpr = m
                bestCost = cost
            }
        }
        if bestExpr != nil {
            ref.SetWinner(ordering, bestExpr)
        }
    }
}
```

Note: `stampWinners` already exists in a rudimentary form (called from `generateDataAccessRecursive`). This extends it to cover all physical members, not just data access results.

### Phase 3: Migrate implementation rules

Replace `findPhysicalPlan(innerRef)` with `getWinner(innerRef, requiredOrdering)` in each rule. The migration is mechanical:

**Before (current):**
```go
func (r *ImplementFilterRule) OnMatch(call *ExpressionRuleCall) {
    // ...
    for _, m := range innerRef.AllMembers() {  // wrap ALL — our patch
        ph, ok := m.(physicalPlanExpression)
        // ...
    }
}
```

**After (winner-based):**
```go
func (r *ImplementFilterRule) OnMatch(call *ExpressionRuleCall) {
    // ...
    for _, ordering := range call.GetRequestedOrderings() {
        winner := innerRef.GetWinner(ordering)
        if winner == nil {
            continue
        }
        ph := winner.(physicalPlanExpression)
        filterPlan := plans.NewRecordQueryFilterPlan(preds, ph.GetRecordQueryPlan())
        innerQ := expressions.ForEachQuantifier(call.MemoizeExpression(winner))
        call.Yield(NewPhysicalFilterWrapper(filterPlan, innerQ))
    }
}
```

Rules to migrate (in priority order):

1. **Critical path (Sort→Filter→Scan):** ImplementFilterRule ✓ (patched), ImplementProjectionRule, ImplementInMemorySortRule ✓ (patched)
2. **Aggregation path:** ImplementStreamingAggRule
3. **Join path:** ImplementNestedLoopJoinRule (3 sites — biggest win, eliminates N×M)
4. **Set operations:** ImplementUnionRule, ImplementIntersectionRule
5. **DML:** ImplementInsertRule, ImplementDeleteRule, ImplementUpdateRule
6. **Other:** ImplementLimitRule, ImplementTypeFilterRule, ImplementTempTableInsertRule, recursive join/union

### Phase 4: Remove patches

Once all rules use winners:
- Revert ImplementFilterRule's "wrap all members" loop back to single-winner
- Revert ImplementInMemorySortRule's restricted-Fetch wrapping back to single-winner
- The `isRestrictedFetch` and `hasRestrictedScan` helpers become unnecessary

### Phase 5: Cost model

The winner system requires a cost model that can compare plans. The current cost model (`planning_cost_model.go`) is minimal. It needs:

- **Selectivity estimation:** IndexScan(a=val) scans ~N/NDV rows; full scan scans N. Without this, the cost model can't distinguish them.
- **Sort cost:** InMemorySort(K rows) costs O(K log K). On a selective index scan (K << N), this is cheap. On a full scan (K = N), it's expensive.
- **Coverage cost:** Covering index scan avoids Fetch. Non-covering adds random I/O per row.

The statistics infrastructure (`properties.StatisticsProvider`) already exists and feeds table/index cardinality. The cost model just needs to use it.

## Migration Strategy

The migration is incremental. Each phase produces a working system:

1. **Phase 1-2:** Add winner infrastructure + stamp winners. No rule changes. `findPhysicalPlan` still works (returns first member). Winners are stamped but unused. **Testable: verify winners match expectations with unit tests.**

2. **Phase 3:** Migrate rules one at a time, starting with ImplementFilterRule. Each migration is a single commit that replaces `findPhysicalPlan` with `getWinner`. Run `just test` after each. **Testable: existing tests pin plan shapes.**

3. **Phase 4:** Remove patches after all critical-path rules migrated. **Testable: stress test must remain green.**

4. **Phase 5:** Cost model improvements. Orthogonal to the winner migration — can happen in parallel. **Testable: stress test timings improve.**

## Non-Goals

- **Full Java parity in one shot.** Java's winner system interacts with deep Cascades infrastructure (property derivation, memoization, physical properties). The Go port takes the 80/20 path: winner per RequestedOrdering is sufficient for the current rule set.
- **Changing the explore phase.** Matching, AdjustMatches, constraint propagation, and data access generation are unaffected. Only the bottom-up implementation pass changes.
- **Re-adding the 4 removed rules.** The winner system makes them unnecessary — alternatives are compared by cost, not by tree shape.

## Risks

- **Planning time regression.** stampWinners iterates all physical members × all requested orderings. For complex queries with many indexes, this could slow planning. Mitigated by: (a) the number of requested orderings per Reference is small (typically 1-3), (b) physical members are already bounded by the rule set.
- **Cost model accuracy.** If the cost model is wrong, the winner is wrong, and the plan regresses. Mitigated by: (a) start with simple heuristics (selectivity = 1/NDV, sort = N log N), (b) stress test pinning.
- **Semantic correctness.** A winner must satisfy the required ordering. If `satisfiesOrdering` has bugs, the wrong plan wins. Mitigated by: (a) `SatisfiesRequestedOrdering` is already implemented and tested, (b) the stress test catches wrong results.
