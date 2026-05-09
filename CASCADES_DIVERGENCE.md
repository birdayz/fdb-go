# Cascades Divergences from Java

Comprehensive audit of all known divergences between Go's Cascades implementation and Java's `fdb-record-layer-core` (tag 4.11.1.0). Tracked for 1:1 alignment work.

---

## ARCHITECTURAL DIVERGENCES (structural differences in how the planner works)

### D-1: Sort elimination phase placement

**Java:** Sort elimination happens ONLY in the PLANNING phase. `RemoveSortRule` extends `ImplementationCascadesRule<LogicalSortExpression>`. It runs bottom-up during PLANNING, inspects inner partition ordering properties, and yields plans with `strictlySorted` markers.

**Go:** Sort elimination happens in TWO places:
1. `SortOverOrderedElimRule` (ExpressionRule) — fires during EXPLORE phase. Structurally eliminates LogicalSortExpression when inner member's `EstimateOrdering` satisfies sort keys.
2. `ImplementSortRule` (ImplementationRule) — fires during PLANNING phase. Mirrors Java's RemoveSortRule with partition-based ordering check and strictlySorted.

**Consequence:** In Go, simple sort eliminations happen during exploration (before physical plans exist), so `ImplementSortRule`'s strictlySorted path never fires for simple cases. In Java, strictlySorted is always set when applicable because RemoveSortRule is the sole sort handler.

**Fix:** ~~Remove `SortOverOrderedElimRule` from `BatchAExpressionRules()`.~~ **DONE (dayshift-82).** Sort elimination now happens exclusively in `ImplementSortRule` (PLANNING phase), matching Java 1:1. All sort tests converted from `Explore()` to `Plan()` with `WithImplementationRules`.

**Effort:** 0.5 shift (completed).

**Files:**
- `pkg/recordlayer/query/plan/cascades/rule_sort_over_ordered_elim.go` (Go-only exploration rule)
- `pkg/recordlayer/query/plan/cascades/rule_implement_sort.go` (Java-aligned PLANNING rule)
- `pkg/recordlayer/query/plan/cascades/default_rules.go:134` (registration)

---

### D-2: PushOrdering rules — structural rewrite vs constraint propagation

**Java:** `PushRequestedOrdering*` rules extend `CascadesRule` and implement `PreOrderRule`. They run during the PLANNING phase in pre-order (top-down). They push ordering CONSTRAINTS to child References via the constraint map — no structural tree changes.

**Go:** `PushOrderingThrough*` rules are ExpressionRules that fire during EXPLORE phase. They perform STRUCTURAL REWRITES — physically moving Sort nodes below Filter/GroupBy/Distinct/Union/etc, creating new expression tree variants in the memo.

**Consequence:** Go's memo contains structural variants (Sort-below-Filter as alternative to Sort-above-Filter). Java's memo doesn't — Sort stays in place, constraints propagate. Both produce correct results but Go's memo is larger and the optimization path is different.

**Fix:** Convert all 10 `PushOrderingThrough*` rules from ExpressionRule (structural rewrite) to ImplementationRule (constraint push). They should push `RequestedOrdering` constraints to child References during PLANNING's top-down pass, not create structural variants during exploration.

**Effort:** ~2-3 shifts. Major architectural change touching 10 rules + all dependent tests.

**Files:**
- `rule_push_ordering_through_filter.go`
- `rule_push_ordering_through_groupby.go`
- `rule_push_ordering_through_distinct.go`
- `rule_push_ordering_through_unique.go`
- `rule_push_ordering_through_union.go`
- `rule_push_ordering_through_delete.go`
- `rule_push_ordering_through_insert.go`
- `rule_push_ordering_through_update.go`
- `rule_push_ordering_through_temp_table_insert.go`
- `rule_push_ordering_through_projection.go` (if exists)

---

### D-3: DistinctOnUniqueElimRule — exploration vs physical planning

**Java:** DISTINCT elimination happens during PLANNING via `ImplementDistinctRule` + `DistinctRecordsProperty`. The physical rule checks whether the inner plan already produces distinct records; if so, the Distinct operator is elided at physical plan construction time.

**Go:** `DistinctOnUniqueElimRule` is a logical rewrite rule (ExpressionRule, EXPLORE phase) that removes Distinct before physical planning by checking PK/unique-index column coverage.

**Consequence:** Same optimization, different phase. Go removes Distinct earlier (fewer downstream Cascades nodes to process). Java removes it later (during physical plan finalization).

**Fix:** Move to PLANNING phase as an ImplementationRule, matching Java's `ImplementDistinctRule` approach.

**Effort:** ~0.5 shift.

**File:** `rule_distinct_on_unique_elim.go:28-42`

---

### D-4: Cost model — Go-native vs Java Postgres-inspired

**Java:** Complex cost model (Postgres-inspired, bit-precise). Memoized on equivalence classes. Multi-dimensional (CPU, I/O, memory).

**Go:** Simplified Go-native cost model (RFC-024). Cardinality + CPU heuristic. Computed on demand, not memoized. Sub-Reference recursion picks first member (not cheapest).

**Consequence:** Plans may differ from Java because cost comparison may prefer different alternatives. Both produce correct results.

**Fix:** Intentional design decision (RFC-024). Not a bug — deliberate simplification for Go.

**Effort:** N/A (by design). Future: align if cross-engine plan-shape parity becomes a goal.

**File:** `properties/cost.go`

---

### D-5: InComparisonToExplodeRule architecture

**Java:** `InComparisonToExplodeRule` creates a `SelectExpression` with a `ForEach` quantifier over `ExplodeExpression`, then `AbstractDataAccessRule` resolves predicates against index candidates within the SelectExpression.

**Go:** Simplified architecture. Multi-element IN uses Union approach where each filter leg independently matches indexes.

**Consequence:** Same query results, less elegant architecture. Java's shared-context predicate resolution within SelectExpression is more architecturally cohesive.

**Fix:** Port `AbstractDataAccessRule` for `SelectExpression` predicates. Requires `SelectExpression` + `ExplorationCascadesRule` + `TranslationMap` infrastructure.

**Effort:** ~2-3 shifts (gated on TranslationMap).

**File:** `rule_in_to_explode.go:26-32`

---

### D-6: DeMorgan / BooleanNormalizer placement

**Java:** `BooleanNormalizer` applies De Morgan distribution + nested-NOT push-down as a built-in part of the default simplification pipeline.

**Go:** Optional via `NormalizationRules()` that callers compose with the simplification driver.

**Consequence:** Go callers must explicitly opt-in to normalization. Java does it by default.

**Fix:** Wire `NormalizationRules()` into the default simplification path.

**Effort:** ~0.5 day.

**File:** `rule_demorgan.go:60-65`, `simplifier.go:119`

---

### D-7: AggregateDataAccessRule — single-aggregate only

**Java:** Handles multi-aggregate matching via intersection of aggregate indexes.

**Go:** Simplified to single-aggregate matching only.

**Consequence:** Queries with multiple aggregates cannot use aggregate index intersections.

**Fix:** Port multi-aggregate matching. Requires intersection infrastructure.

**Effort:** ~1 shift.

**File:** `rule_aggregate_data_access.go:23-25`

---

### D-8: CardinalityProperty coupling to Cost

**Java:** `CardinalitiesProperty` is a separate class with min/max bounds.

**Go:** Cardinality is a field on `Cost`, shared computation. Will split when Cost gains I/O modeling.

**Consequence:** Less modular. No min/max cardinality bounds.

**Fix:** Split into separate property when needed.

**Effort:** ~0.5 shift.

**File:** `properties/cardinality.go:1-14`

---

## VALUE EVALUATION DIVERGENCES (theoretical, become real when downstream consumers land)

### ~~D-9: PatternForLikeValue DOTALL mismatch~~ — RETRACTED

**Investigation:** Java's `LikeOperatorValue.likeOperation` (line 97) calls `Pattern.compile(rhs)` WITHOUT DOTALL. The prior comment claiming `Pattern.DOTALL` was incorrect. Go's default regexp behavior (`.` does NOT match `\n`) is already aligned with Java. No divergence exists. Comment fixed in dayshift-82.

---

### D-10: InOpValue numeric coercion

**Java:** `InOpValue` uses `Comparisons.evalComparison(EQUALS, ...)` which performs numeric coercion (`int64 == float64` if values are equal).

**Go:** Uses Go's `==` on `any` — dynamic-type-AND-value equality. No numeric coercion.

**Status:** Theoretical. No planner rule currently rewrites IN-list comparisons to `InOpValue`. Becomes real when such a rule lands.

**Fix:** ~~Route through SQL-equality comparator.~~ **DONE (dayshift-82).** `equalsAny` now promotes mixed int/float pairs to float64 for comparison. 3 new tests.

**Effort:** 0.5 day (completed).

---

### D-11: ConstantObjectValue type promotion

**Java:** `ConstantObjectValue.eval` consults `EvaluationContext.dereferenceConstant` + `PromoteValue.isPromotionNeeded` for type promotion.

**Go:** Looks up via `ConstantDeref` interface but does NOT handle type promotion — value returned as-is.

**Status:** Incomplete. Becomes real when execution routes through ConstantObjectValue with type-mismatched constants.

**Fix:** Wire type promotion through SQL-type coercion.

**Effort:** ~0.5 shift.

**File:** `values/value_constant_object.go:22-27`

---

### D-12: GetEqualityBoundValues semantics

**Java:** `Ordering.getEqualityBoundValues()` uses `Multimaps.filterValues(isFixed)` — keeps values with ANY fixed binding.

**Go:** `RichOrdering.GetEqualityBoundValues()` requires ALL bindings to be fixed.

**Status:** No practical impact today (index scans produce single-binding entries). Becomes real if multi-binding scenarios emerge (e.g., intersection of two indexes on the same column with different bindings).

**Fix:** Change `AreAllBindingsFixed` to `HasAnyFixedBinding` in the filter.

**Effort:** 1 line + test update.

**File:** `rich_ordering.go:123-133`

---

## GO EXTENSIONS (features Go has that Java doesn't)

### E-1: In-Memory Sort (RecordQueryInMemorySortPlan)

**Description:** Physical operator that materializes inner result and sorts in memory. Fallback when no index satisfies ORDER BY.

**Java equivalent:** None. Java's RemoveSortRule eliminates sorts or fails.

**Justification:** SQL-standard ORDER BY should work regardless of index availability. Go-only correctness improvement.

**Files:**
- `plans/in_memory_sort.go`
- `rule_implement_in_memory_sort.go`
- `physical_in_memory_sort_wrapper.go`
- `executor/executor.go:128-130, 2119-2122`

---

## MISSING JAVA INFRASTRUCTURE (not yet ported)

### M-1: TranslationMap

Required for correlation rebasing in subquery decorrelation, predicate push-down through correlated quantifiers. Gates: DecorrelateValuesRule, AbstractDataAccessRule, MatchPartition infrastructure.

### M-2: MatchPartition / PartialMatch / Compensation

Required for advanced index matching (partial index utilization, compensation predicates). Gates: covering index via Cascades.

### M-3: PushReferencedFields rules (5 rules)

Column pruning optimization. Reduces I/O by tracking which columns each operator needs.

### M-4: PlanPartition property-based matching

Java's ImplementationRules match against PlanPartition property sets (ordering, distinct, stored-record). Go approximates with simpler partition iteration.

---

## PRIORITY ORDER FOR 1:1 ALIGNMENT

1. **D-1** (remove SortOverOrderedElimRule) — 0.5 shift, unblocks strictlySorted
2. **D-9** (DOTALL fix) — 1 line
3. **D-12** (GetEqualityBoundValues ANY vs ALL) — 1 line
4. **D-10** (InOpValue coercion) — 0.5 day
5. **D-6** (BooleanNormalizer default) — 0.5 day
6. **D-3** (DistinctOnUniqueElim phase) — 0.5 shift
7. **D-2** (PushOrdering constraint vs structural) — 2-3 shifts
8. **D-7** (multi-aggregate) — 1 shift
9. **D-5** (InComparison architecture) — 2-3 shifts (gated on M-1)
10. **D-11** (ConstantObjectValue promotion) — 0.5 shift
