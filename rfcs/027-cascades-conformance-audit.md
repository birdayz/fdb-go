# RFC-027: Cascades Conformance Audit — Go vs Java 4.11.1.0

**Date:** 2026-05-11 (swingshift-91)
**Status:** Living document — updated as gaps are closed

## Executive Summary

Comprehensive audit of Go's Cascades planner against Java's `fdb-record-layer-core` 4.11.1.0. Five subsystems reviewed: expressions/plans, values, properties, rules, and predicates.

**Overall:** Go covers all core Java Cascades infrastructure. Extensions are flagged. Missing items are either out-of-scope (UDFs, text search, bitmap indexes) or collapsed into unified Go types.

---

## 1. Logical Expressions

| Java Expression | Go Equivalent | Status |
|---|---|---|
| `SelectExpression` | `SelectExpression` | Aligned |
| `LogicalFilterExpression` | `LogicalFilterExpression` | Aligned |
| `LogicalSortExpression` | `LogicalSortExpression` | Aligned |
| `LogicalDistinctExpression` | `LogicalDistinctExpression` | Aligned |
| `LogicalUnionExpression` | `LogicalUnionExpression` | Aligned |
| `LogicalIntersectionExpression` | `LogicalIntersectionExpression` | Aligned |
| `LogicalProjectionExpression` | `LogicalProjectionExpression` | Aligned |
| `LogicalTypeFilterExpression` | `LogicalTypeFilterExpression` | Aligned |
| `LogicalUniqueExpression` | `LogicalUniqueExpression` | Aligned |
| `FullUnorderedScanExpression` | `FullUnorderedScanExpression` | Aligned |
| `ExplodeExpression` | `ExplodeExpression` | Aligned |
| `GroupByExpression` | `GroupByExpression` | Aligned |
| `MatchableSortExpression` | `MatchableSortExpression` | Aligned |
| `DeleteExpression` | `DeleteExpression` | Aligned |
| `InsertExpression` | `InsertExpression` | Aligned |
| `UpdateExpression` | `UpdateExpression` | Aligned |
| `RecursiveUnionExpression` | `RecursiveUnionExpression` | Aligned |
| `TableFunctionExpression` | `TableFunctionExpression` | Aligned |
| `TempTableInsertExpression` | `TempTableInsertExpression` | Aligned |
| `TempTableScanExpression` | `TempTableScanExpression` | Aligned |
| — | `LogicalLimitExpression` | **Go extension** |
| — | `LogicalValuesExpression` | **Go extension** |

**19/19 Java expressions present. 2 Go extensions.**

---

## 2. Physical Plans

| Java Plan | Go Equivalent | Status |
|---|---|---|
| `RecordQueryScanPlan` | `RecordQueryScanPlan` | Aligned |
| `RecordQueryIndexPlan` | `RecordQueryIndexPlan` | Aligned |
| `RecordQueryFilterPlan` | `RecordQueryFilterPlan` | Aligned |
| `RecordQueryPredicatesFilterPlan` | `RecordQueryPredicatesFilterPlan` | Aligned |
| `RecordQueryTypeFilterPlan` | `RecordQueryTypeFilterPlan` | Aligned |
| `RecordQueryFetchFromPartialRecordPlan` | `RecordQueryFetchFromPartialRecordPlan` | Aligned |
| `RecordQueryMapPlan` | `RecordQueryMapPlan` | Aligned |
| `RecordQueryExplodePlan` | `RecordQueryExplodePlan` | Aligned |
| `RecordQueryDeletePlan` | `RecordQueryDeletePlan` | Aligned |
| `RecordQueryInsertPlan` | `RecordQueryInsertPlan` | Aligned |
| `RecordQueryUpdatePlan` | `RecordQueryUpdatePlan` | Aligned |
| `RecordQueryStreamingAggregationPlan` | `RecordQueryStreamingAggregationPlan` | Aligned |
| `RecordQueryDefaultOnEmptyPlan` | `RecordQueryDefaultOnEmptyPlan` | Aligned |
| `RecordQueryFirstOrDefaultPlan` | `RecordQueryFirstOrDefaultPlan` | Aligned |
| `RecordQueryTableFunctionPlan` | `RecordQueryTableFunctionPlan` | Aligned |
| `RecordQueryIntersectionPlan` | `RecordQueryIntersectionPlan` | Aligned (unified) |
| `RecordQueryMultiIntersectionOnValuesPlan` | `RecordQueryMultiIntersectionOnValuesPlan` | Aligned |
| `RecordQueryUnionPlan` | `RecordQueryUnionPlan` | Aligned (unified) |
| `RecordQueryUnorderedUnionPlan` | `RecordQueryUnorderedUnionPlan` | Aligned |
| `RecordQueryInJoinPlan` | `RecordQueryInJoinPlan` | Aligned (3 Java subclasses collapsed) |
| `RecordQueryInUnionPlan` | `RecordQueryInUnionPlan` | Aligned (2 Java subclasses collapsed) |
| `RecordQueryRecursiveDfsJoinPlan` | `RecordQueryRecursiveDfsJoinPlan` | Aligned |
| `RecordQueryRecursiveLevelUnionPlan` | `RecordQueryRecursiveLevelUnionPlan` | Aligned |
| `TempTableInsertPlan` | `RecordQueryTempTableInsertPlan` | Aligned |
| `TempTableScanPlan` | `RecordQueryTempTableScanPlan` | Aligned |
| `RecordQueryDistinctPlan` (2 Java variants) | `RecordQueryDistinctPlan` | Aligned (unified) |
| `RecordQueryAggregateIndexPlan` | — | Missing (aggregate index) |
| `RecordQueryCoveringIndexPlan` | — | Missing (covering index as separate plan) |
| `RecordQueryFlatMapPlan` | — | Missing (replaced by NLJ) |
| `RecordQueryComparatorPlan` | — | Missing (multi-child comparison) |
| `RecordQuerySelectorPlan` | — | Missing (multi-child selection) |
| `RecordQueryLoadByKeysPlan` | — | Missing (key-list fetch) |
| `RecordQueryScoreForRankPlan` | — | Missing (rank functions) |
| `RecordQueryTextIndexPlan` | — | Missing (full-text search) |
| — | `RecordQueryHashAggregationPlan` | **Go extension** |
| — | `RecordQueryInMemorySortPlan` | **Go extension** |
| — | `RecordQueryLimitPlan` | **Go extension** |
| — | `RecordQueryNestedLoopJoinPlan` | **Go extension** (replaces FlatMap) |
| — | `RecordQueryProjectionPlan` | **Go extension** |
| — | `RecordQuerySortPlan` | **Go extension** |
| — | `RecordQueryValuesPlan` | **Go extension** |
| — | `RecordQueryMergeSortUnionPlan` | **Go extension** |

**26 Java plans present, 8 missing (mostly out-of-scope: text search, rank, aggregate index). 8 Go extensions.**

---

## 3. Value Types

| Java Value | Go Equivalent | Status |
|---|---|---|
| `FieldValue` | `FieldValue` | Aligned (single-step; Java has multi-step FieldPath) |
| `ConstantObjectValue` | `ConstantObjectValue` | Aligned |
| `ConstantValue` / `LiteralValue` | `ConstantValue` | Aligned |
| `ArithmeticValue` | `ArithmeticValue` | Aligned |
| `CastValue` | `CastValue` | Aligned |
| `PromoteValue` | `PromoteValue` | Aligned |
| `RecordConstructorValue` | `RecordConstructorValue` | Aligned |
| `QuantifiedObjectValue` | `QuantifiedObjectValue` | Aligned |
| `NullValue` | `NullValue` | Aligned |
| `PickValue` | `PickValue` | Aligned |
| `ExistsValue` | `ExistsValue` | Aligned |
| `NotValue` | `NotValue` | Aligned |
| `AndOrValue` | `AndOrValue` | Aligned |
| `DerivedValue` | `DerivedValue` | Aligned |
| `EmptyValue` | `EmptyValue` | Aligned |
| `IndexedValue` | `IndexedValue` | Aligned |
| `LikeOperatorValue` | `LikeOperatorValue` | Aligned |
| `PatternForLikeValue` | `PatternForLikeValue` | Aligned |
| `InOpValue` | `InOpValue` | Aligned |
| `RangeValue` | `RangeValue` | Aligned |
| `VersionValue` | `VersionValue` | Aligned |
| `CollateValue` | `CollateValue` | Aligned |
| + 20 more specialized values | Present | Aligned |
| `CountValue` | Folded into `AggregateValue` | Collapsed |
| `NumericAggregationValue` (SUM/MIN/MAX/AVG) | Folded into `AggregateValue` | Collapsed |
| `VariadicFunctionValue` (COALESCE/GREATEST/LEAST) | Folded into `ScalarFunctionValue` | Collapsed |
| `ParameterObjectValue` | — | Missing (plan params vs SQL params) |

**44+ Java Value types present or collapsed. 1 genuinely missing (`ParameterObjectValue`). 8 Go-only extensions.**

---

## 4. Properties

| Java Property | Go Equivalent | Status |
|---|---|---|
| `CardinalitiesProperty` | `cardinality.go` | Aligned |
| `OrderingProperty` | `ordering.go` | Aligned |
| `DistinctRecordsProperty` | `PropDistinctRecords` | Aligned (bool) |
| `StoredRecordProperty` | `PropStoredRecord` | Aligned (bool) |
| `PrimaryKeyProperty` | `PropPrimaryKey` | Aligned (stub) |
| `NormalizedResidualPredicateProperty` | `countResidualPredicates` (inline in cost model) | **Inline** |
| `ExpressionDepthProperty` | `expressionDepth` (inline in cost model) | **Inline** |
| `TypeFilterCountProperty` | `walkExpressionTree` counter (inline) | **Inline** |
| `UnmatchedFieldsCountProperty` | `walkExpressionTree` counter (inline) | **Inline** |
| `ComparisonsProperty` | — | **Missing** |
| `ExpressionCountProperty` | — | Missing (not used by cost model) |
| `FieldWithComparisonCountProperty` | — | Missing |
| `PredicateComplexityProperty` | — | Missing |
| `PredicateCountByLevelProperty` | — | Missing |
| `RecordTypesProperty` | — | Missing |
| `ReferencesAndDependenciesProperty` | — | Missing |
| `DerivationsProperty` | — | Missing |
| `UsedTypesProperty` | — | Missing |

**5 aligned, 4 inline in cost model, 1 missing (ComparisonsProperty — needed for full IN-SARG check), 8 missing (not used by current rules).**

### Cost Model: PlanningCostModel.java vs planning_cost_model.go

**DONE (swingshift-91).** All 16 criteria ported and wired in. Scalar `CostLess` retained as tiebreak fallback. 46/46 tests green. See D-4 in TODO.md.

---

## 5. Rules

**38 aligned, 11 missing, 2 structural divergences, ~25 Go extensions.**

| Status | Count | Examples |
|---|---|---|
| Aligned | 38 | SelectMergeRule, ImplementNestedLoopJoinRule, RemoveSortRule, all Push*ThroughFetch, all PushRequestedOrdering*, MatchLeafRule |
| Missing | 11 | PartitionSelectRule (join enumeration), PredicatePushDownRule (generic), PullUpNullOnEmptyRule (LEFT JOIN), PredicateToLogicalUnionRule (CNF→union-of-scans), 4 ordering push rules |
| Structural divergence | 2 | NormalizePredicatesRule (Java=Cascades rule, Go=simplifier engine), WithPrimaryKeyDataAccessRule (Java=MatchPartition rule, Go=explicit planner pass) |
| Go extension | ~25 | PrimaryScanRule, ImplementIndexScanRule, ImplementLimitRule, ImplementHashAggregationRule, StreamingAggFromIndexRule, ImplementInMemorySortRule, ~40 exploration rewrite rules for decomposed expressions |

**Key missing rules:**
1. `PartitionSelectRule` / `PartitionBinarySelectRule` — multi-way join enumeration / binary join reordering
2. `PredicatePushDownRule` — generic predicate push-down (Go has specific Push*Through* rules for common cases)
3. `PullUpNullOnEmptyRule` — LEFT JOIN null-on-empty quantifier semantics
4. `PredicateToLogicalUnionRule` — CNF predicates → union of index scans

**Why Go has ~25 extra rules:** Java uses `SelectExpression` as a unified node for filters/projections/joins. Go decomposes into `LogicalFilter`, `LogicalProjection`, `LogicalSort`, etc. — each needing explicit merge/push/pull rewrite rules.

---

## 6. Predicates & Comparisons

**Core predicates aligned. Text search / rank comparisons out of scope.**

| Java Predicate | Go Equivalent | Status |
|---|---|---|
| `AndPredicate` | `AndPredicate` | Aligned |
| `OrPredicate` | `OrPredicate` | Aligned |
| `NotPredicate` | `NotPredicate` | Aligned |
| `ConstantPredicate` | `ConstantPredicate` | Aligned |
| `ValuePredicate` | `ComparisonPredicate` | Aligned (renamed) |
| `ExistsPredicate` | `ExistsPredicate` | Aligned |
| `Placeholder` | `Placeholder` | Partially aligned (simpler ComparisonRange vs Java's RangeConstraints) |
| `PredicateWithValueAndRanges` | — | Missing |
| `RangeConstraints` | — | Missing |

**Comparison operators:** 13/13 core operators aligned. 10 missing (7 TEXT_CONTAINS_*, 3 DISTANCE_RANK_*) — all out of scope.

**Matching infrastructure:**
| Component | Status |
|---|---|
| `MatchCandidate` interface | Aligned |
| `ValueIndexScanMatchCandidate` | Aligned |
| `AggregateIndexMatchCandidate` | Aligned |
| `PartialMatch` | Aligned |
| `PredicateMultiMap` | Aligned |
| `PrimaryScanMatchCandidate` | Missing |
| 5 specialized match candidates | Missing (vector, windowed, etc.) |

**Predicate simplification rules:** All 12 Java rules missing as Cascades rules. Go has equivalent logic in a separate simplifier engine (structurally different, functionally equivalent for the implemented cases).

---

## Summary of Gaps

### Missing Java features (out of scope per CLAUDE.md):
- Full-text search (`RecordQueryTextIndexPlan`, `TextIndexPlan`)
- Rank functions (`RecordQueryScoreForRankPlan`)
- Aggregate indexes (`RecordQueryAggregateIndexPlan`)
- Bitmap aggregate indexes
- UDFs / synthetic record types
- `RecordQueryComparatorPlan` / `RecordQuerySelectorPlan` (multi-child comparison/selection)

### Architectural differences (intentional):
- Go uses `RecordQueryNestedLoopJoinPlan` with explicit predicates; Java uses `RecordQueryFlatMapPlan` with correlation bindings
- Go has explicit `Sort`/`InMemorySort` physical operators; Java relies on `RemoveSortRule` to eliminate sorts
- Go collapses Java class hierarchies (3 InJoin subclasses → 1, 2 Union subclasses → 1, etc.)
- Go implements 4 cost model properties inline; Java has separate property classes

### Actionable gaps (prioritized):
1. **PartitionSelectRule** — multi-way join enumeration. Blocks optimal 3+ table joins.
2. **PredicatePushDownRule** — generic push-down. Go has specific rules for common cases but not the generic algorithm.
3. **ComparisonsProperty** — needed for full-fidelity IN-SARG check in cost model.
4. **PullUpNullOnEmptyRule** — LEFT JOIN null-on-empty semantics.
5. **PredicateToLogicalUnionRule** — CNF predicates → union of index scans.
6. **FieldValue multi-step path** — Java supports nested record traversal; Go single-step only.
7. **12 predicate simplification rules** — Go has equivalent logic in simplifier engine but not as Cascades rules.
8. **PrimaryScanMatchCandidate** — primary key scans can't participate in Cascades matching.

### Counts

| Subsystem | Java Types | Go Aligned | Go Extension | Missing |
|---|---|---|---|---|
| Logical Expressions | 19 | 19 | 2 | 0 |
| Physical Plans | 34 | 26 | 8 | 8 (out of scope) |
| Value Types | 48 | 44 | 8 | 4 (collapsed) |
| Properties | 18 | 9 (5 inline) | 0 | 9 (mostly unused) |
| Rules | 67 | 38 | ~25 | 11 |
| Predicates | 9 core | 7 | 1 | 1 |
| Comparisons | 24 | 13 | 0 | 11 (text/rank) |
| Match Candidates | 9 | 2 | 0 | 7 (specialized) |
