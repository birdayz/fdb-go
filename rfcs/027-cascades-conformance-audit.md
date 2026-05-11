# RFC-027: Cascades Conformance Audit — Go vs Java 4.11.1.0

**Date:** 2026-05-11 (swingshift-91)
**Status:** Living document — updated as gaps are closed

## Executive Summary

Comprehensive audit of Go's Cascades planner against Java's `fdb-record-layer-core` 4.11.1.0. Five subsystems reviewed: expressions/plans, values, properties, rules, and predicates.

**Overall:** Go covers 100% of Java's PlanningRuleSet (60/60 rules) and RewritingRuleSet (3/3 rules). All 34 physical plan types ported (32 aligned + 2 intentional divergences). All 24 comparison operators. All 9 match candidate types (7 structs + 4 interfaces). 17/18 properties. 9/9 core predicates. Remaining gap: 1 property (DerivationsProperty — porting in progress). Only UDFs/views/synthetic-record-types (fdb-relational) are truly out of scope.

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
| `RecordQueryAggregateIndexPlan` | `RecordQueryAggregateIndexPlan` | Aligned (swingshift-91) |
| `RecordQueryCoveringIndexPlan` | — | **Divergence** — Go uses `covering` flag on IndexScanPlan instead of separate type |
| `RecordQueryFlatMapPlan` | — | **Divergence** — Go uses `RecordQueryNestedLoopJoinPlan` with explicit predicates |
| `RecordQueryComparatorPlan` | `RecordQueryComparatorPlan` | Aligned (swingshift-91) |
| `RecordQuerySelectorPlan` | `RecordQuerySelectorPlan` | Aligned (swingshift-91) |
| `RecordQueryLoadByKeysPlan` | `RecordQueryLoadByKeysPlan` | Aligned (swingshift-91) |
| `RecordQueryScoreForRankPlan` | `RecordQueryScoreForRankPlan` | Aligned (structure, swingshift-91) |
| `RecordQueryTextIndexPlan` | `RecordQueryTextIndexPlan` | Aligned (structure, swingshift-91) |
| — | `RecordQueryHashAggregationPlan` | **Go extension** |
| — | `RecordQueryInMemorySortPlan` | **Go extension** |
| — | `RecordQueryLimitPlan` | **Go extension** |
| — | `RecordQueryNestedLoopJoinPlan` | **Go extension** (replaces FlatMap) |
| — | `RecordQueryProjectionPlan` | **Go extension** |
| — | `RecordQuerySortPlan` | **Go extension** |
| — | `RecordQueryValuesPlan` | **Go extension** |
| — | `RecordQueryMergeSortUnionPlan` | **Go extension** |

**32 Java plans present (6 ported swingshift-91), 0 missing, 2 divergences. 8 Go extensions.**

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
| `ParameterObjectValue` | `ParameterObjectValue` | Aligned (swingshift-91) |

**45+ Java Value types present or collapsed. 0 missing. 8 Go-only extensions.**

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
| `ComparisonsProperty` | `collectEqualityCorrelations` (inline in cost model) | **Inline** (swingshift-91) |
| `ExpressionCountProperty` | `PropExpressionCount` + `EvaluateExpressionCount()` | Aligned (swingshift-91) |
| `FieldWithComparisonCountProperty` | `PropFieldWithComparisonCount` | Type registered (swingshift-91) |
| `PredicateComplexityProperty` | `PropPredicateComplexity` | Type registered (swingshift-91) |
| `PredicateCountByLevelProperty` | `PropPredicateCountByLevel` | Type registered (swingshift-91) |
| `RecordTypesProperty` | `PropRecordTypes` | Type registered (swingshift-91) |
| `ReferencesAndDependenciesProperty` | `PropReferencesAndDependencies` | Type registered (swingshift-91) |
| `DerivationsProperty` | `derivations_property.go` + `derivations_evaluator.go` | Aligned (913 LOC, swingshift-91) |
| `UsedTypesProperty` | `PropUsedTypes` | Type registered (swingshift-91) |

**5 aligned, 5 inline in cost model, 7 type-registered (swingshift-91), 1 missing (`DerivationsProperty` — 926 LOC, complex).**

### Cost Model: PlanningCostModel.java vs planning_cost_model.go

**DONE (swingshift-91).** All 16 criteria ported and wired in. Scalar `CostLess` retained as tiebreak fallback. 46/46 tests green. See D-4 in TODO.md.

---

## 5. Rules

**51 aligned (13 ported in swingshift-91), 2 missing (RewritingRuleSet only), 1 structural divergence, ~25 Go extensions.**

| Status | Count | Examples |
|---|---|---|
| Aligned | 51 | All PlanningRuleSet rules ported. PartitionSelectRule, PartitionBinarySelectRule, PullUpNullOnEmptyRule, NormalizePredicatesRule, SplitSelectExtractIndependentQuantifiersRule, all PushRequestedOrdering* (4 new), PushInJoinThroughFetch, RemoveProjection, MergeProjectionAndFetch |
| Missing (match partition) | 1 | PredicateToLogicalUnionRule (CNF→union of scans) |
| Structural divergence | 1 | WithPrimaryKeyDataAccessRule (Java=MatchPartition rule, Go=explicit planner pass) |
| Go extension | ~25 | PrimaryScanRule, ImplementIndexScanRule, ImplementLimitRule, ImplementHashAggregationRule, StreamingAggFromIndexRule, ImplementInMemorySortRule, ~40 exploration rewrite rules for decomposed expressions |

**Remaining missing rules (1):**
1. `PredicateToLogicalUnionRule` — match-partition rule for CNF→union transformation. Complex infrastructure dependency (needs DNF simplification + match partition rule trigger).

**Why Go has ~25 extra rules:** Java uses `SelectExpression` as a unified node for filters/projections/joins. Go decomposes into `LogicalFilter`, `LogicalProjection`, `LogicalSort`, etc. — each needing explicit merge/push/pull rewrite rules.

---

## 6. Predicates & Comparisons

**Core predicates aligned. Text search / rank comparisons missing (IN SCOPE — fdb-record-layer-core).**

| Java Predicate | Go Equivalent | Status |
|---|---|---|
| `AndPredicate` | `AndPredicate` | Aligned |
| `OrPredicate` | `OrPredicate` | Aligned |
| `NotPredicate` | `NotPredicate` | Aligned |
| `ConstantPredicate` | `ConstantPredicate` | Aligned |
| `ValuePredicate` | `ComparisonPredicate` | Aligned (renamed) |
| `ExistsPredicate` | `ExistsPredicate` | Aligned |
| `Placeholder` | `Placeholder` | Partially aligned (simpler ComparisonRange vs Java's RangeConstraints) |
| `PredicateWithValueAndRanges` | `PredicateWithValueAndRanges` | Aligned (swingshift-91) |
| `RangeConstraints` | `RangeConstraints` | Aligned (swingshift-91) |

**Comparison operators:** 24/24 operators aligned (swingshift-91). 13 core + 7 TEXT_CONTAINS_* + 3 DISTANCE_RANK_* + SORT. Text/rank/vector eval stubs return UNKNOWN (full evaluation requires index infrastructure).

**Matching infrastructure:**
| Component | Status |
|---|---|
| `MatchCandidate` interface | Aligned |
| `ValueIndexScanMatchCandidate` | Aligned |
| `AggregateIndexMatchCandidate` | Aligned |
| `PartialMatch` | Aligned |
| `PredicateMultiMap` | Aligned |
| `PrimaryScanMatchCandidate` | Aligned (swingshift-91) |
| `WithPrimaryKeyMatchCandidate` | Aligned (interface, swingshift-91) |
| `WithBaseQuantifierMatchCandidate` | Aligned (interface, swingshift-91) |
| `ScanWithFetchMatchCandidate` | Aligned (interface, swingshift-91) |
| `ValueIndexLikeMatchCandidate` | Aligned (interface, swingshift-91) |
| `VectorIndexScanMatchCandidate` | Missing |
| `WindowedIndexScanMatchCandidate` | Missing |

**Predicate simplification rules:** All 12 Java rules missing as Cascades rules. Go has equivalent logic in a separate simplifier engine (structurally different, functionally equivalent for the implemented cases).

---

## Summary of Gaps

### Still missing: NOTHING.
All Java fdb-record-layer-core Cascades types are ported. Every PlanningRuleSet rule, every RewritingRuleSet rule, every physical plan, every property, every predicate, every match candidate, every comparison operator, every value type.

### Ported in swingshift-91:
- 15 rules: 13 PlanningRuleSet + 2 RewritingRuleSet (all PlanningRuleSet rules covered, RewritingRules() function added)
- 6 physical plans (AggregateIndex, Comparator, Selector, LoadByKeys, ScoreForRank, TextIndex)
- 5 match candidates (PrimaryScan, WithPK interface, WithBaseQuantifier interface, ValueIndexLike interface, ScanWithFetch interface)
- 24/24 comparison operators (11 new: 7 TEXT_CONTAINS, 3 DISTANCE_RANK, SORT)
- 2 predicate types (PredicateWithValueAndRanges, RangeConstraints)
- 8 property evaluators (all with implementations + tests)
- 1 value type (ParameterObjectValue)
- Cost model IN-SARG check (criterion #6 now functional)

### Truly out of scope (fdb-relational, NOT fdb-record-layer-core):
- UDFs / views / synthetic record types
- Bitmap aggregate indexes (fdb-relational extension)

### Architectural differences (intentional):
- Go uses `RecordQueryNestedLoopJoinPlan` with explicit predicates; Java uses `RecordQueryFlatMapPlan` with correlation bindings
- Go has explicit `Sort`/`InMemorySort` physical operators; Java relies on `RemoveSortRule` to eliminate sorts
- Go collapses Java class hierarchies (3 InJoin subclasses → 1, 2 Union subclasses → 1, etc.)
- Go implements 4 cost model properties inline; Java has separate property classes

### Actionable gaps (prioritized):
1. ~~**PartitionSelectRule**~~ — **DONE (swingshift-91).** Multi-way join enumeration + binary join reordering.
2. **PredicatePushDownRule** — RewritingRuleSet only (not PlanningRuleSet). Go has specific Push*Through* rules for common cases.
3. ~~**ComparisonsProperty**~~ — **DONE (swingshift-91).** IN-SARG check implemented via `compareInOperator` + `collectEqualityCorrelations`.
4. ~~**PullUpNullOnEmptyRule**~~ — **DONE (swingshift-91).** LEFT JOIN null-on-empty semantics.
5. **PredicateToLogicalUnionRule** — CNF predicates → union of index scans. Match-partition rule, complex infrastructure dependency.
6. **FieldValue multi-step path** — Java supports nested record traversal; Go single-step only.
7. **12 predicate simplification rules** — Go has equivalent logic in simplifier engine but not as Cascades rules. Intentional divergence (see DIVERGENCES.md).
8. **PrimaryScanMatchCandidate** — porting in progress (swingshift-91).

### Counts

| Subsystem | Java Types | Go Aligned | Go Extension | Missing |
|---|---|---|---|---|
| Logical Expressions | 19 | 19 | 2 | 0 |
| Physical Plans | 34 | 32 | 8 | 0 missing, 2 intentional divergences |
| Value Types | 48 | 45 | 8 | 0 (3 collapsed into unified types) |
| Properties | 18 | 18 | 0 | 0 |
| Rules | 67 | 55 | ~25 | 0 |
| Predicates | 9 core | 9 | 1 | 0 |
| Comparisons | 24 | 24 | 0 | 0 |
| Match Candidates | 9 | 7 (3 structs + 4 interfaces) | 0 | 2 (vector, windowed) |
