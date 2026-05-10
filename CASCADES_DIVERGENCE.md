# Cascades Divergences from Java

Comprehensive audit of all known divergences between Go's Cascades implementation and Java's `fdb-record-layer-core` (tag 4.11.1.0). Tracked for 1:1 alignment work.

---

## RESOLVED DIVERGENCES

### ~~D-1: Sort elimination phase placement~~ — DONE (dayshift-82)

Removed Go-only `SortOverOrderedElimRule` (EXPLORE phase). Sort elimination now happens exclusively in `ImplementSortRule` (PLANNING phase), matching Java's `RemoveSortRule` 1:1. Dead code file `rule_sort_over_ordered_elim.go` deleted.

### ~~D-3: DistinctOnUniqueElimRule — exploration vs physical planning~~ — DONE

Removed Go-only `DistinctOnUniqueElimRule` (EXPLORE phase) and `ImplementDistinctRule` (BatchA EXPLORE phase). Distinct elimination + implementation now happens exclusively in `ImplementDistinctFinalRule` (PLANNING phase), matching Java's `ImplementDistinctRule` (ImplementationCascadesRule). PlanContext threaded through to PLANNING-phase rules via `FireImplementationRuleWithContext`. Dead code files deleted.

Physical-level `DistinctRecordsProperty` now checked per FinalMember, matching Java 1:1: `RecordQueryProjectionPlan` returns `false` (projection reshapes output, two different records can project to the same tuple), `RecordQueryMapPlan` propagates child distinctness only for identity mappings (result value is a `QuantifiedObjectValue` whose correlation matches the inner quantifier). Logical-level PK column coverage retained as fallback.

### ~~D-9: PatternForLikeValue DOTALL mismatch~~ — RETRACTED (dayshift-82)

No divergence exists. Java's `Pattern.compile(rhs)` does NOT use DOTALL. Go's default regexp behavior already matches.

### ~~D-10: InOpValue numeric coercion~~ — DONE (dayshift-82)

`equalsAny` now promotes mixed int/float pairs to float64 for comparison, matching Java's `Comparisons.evalComparison(EQUALS)`.

### ~~D-12: GetEqualityBoundValues semantics~~ — DONE (dayshift-82)

`GetEqualityBoundValues` now uses "any binding fixed" semantics (break on first fixed), matching Java's `Multimaps.filterValues(isFixed)`.

### ~~D-8: CardinalityProperty coupling to Cost~~ — DONE

Ported Java's `CardinalitiesProperty` 1:1. Go now has `Cardinality` (single bound: known int64 or unknown) and `Cardinalities` (min/max pair) types in `properties/cardinality.go`, matching Java's inner classes. Three merge helpers (`IntersectCardinalities`, `UnionCardinalities`, `WeakenCardinalities`) match Java's visitor methods exactly including unknown-handling semantics. `computeCardinalities` in `plan_properties.go` handles all Go plan types with per-type logic matching the Java visitor. The `PropCardinalities` property key is wired into `computeWrapperProperties` alongside existing properties. The old `EstimateCardinality` (Cost-walk on logical expressions) is retained for backward compatibility; the new `Cardinalities` operates on physical plan wrappers.

### ~~D-11: ConstantObjectValue type promotion~~ — DONE

`ConstantObjectValue.Evaluate` now matches Java's `eval()` 1:1: after dereferencing via `ConstantDeref`, applies `promoteConstant` for numeric widening (INT->LONG, INT->FLOAT, INT->DOUBLE, LONG->FLOAT, LONG->DOUBLE, FLOAT->DOUBLE). Relation-typed results pass through without promotion. Mirrors Java's `PromoteValue.isPromotionNeeded` + `resolvePhysicalOperator` chain.

---

## OPEN ARCHITECTURAL DIVERGENCES

### ~~D-2: PushOrdering rules — constraint propagation~~ — DONE (nightshift-84)

All 10 `PushOrderingThrough*` rules converted from EXPLORE-phase structural rewrites (ExpressionRules that physically moved Sort nodes) to PLANNING-phase constraint propagation (ImplementationRules that push `RequestedOrdering` constraints top-down via `ConstraintMap`). Matching Java's `PushRequestedOrdering*` architecture 1:1.

Transparent rules (Sort, Distinct, Unique, Delete, Filter, Insert, Update, TempTableInsert): pass ordering constraints through unchanged. Complex rules (Projection, GroupBy, Union): translate/synthesize orderings. Expression partition fix ensures ordered and unordered plans get separate partitions for sort elimination.

---

### D-4: Cost model — Go-native vs Java Postgres-inspired

**Java:** Complex cost model (Postgres-inspired, bit-precise). Memoized on equivalence classes. Multi-dimensional (CPU, I/O, memory).

**Go:** Simplified Go-native cost model (RFC-024). Cardinality + CPU heuristic. Computed on demand, not memoized.

**Fix:** Intentional design decision (RFC-024). Not a bug — deliberate simplification.

**Effort:** N/A (by design).

---

### ~~D-5: InComparisonToExplodeRule architecture~~ — DONE (nightshift-84)

InComparisonToExplodeRule now produces SelectExpression + ExplodeExpression matching Java 1:1. Multi-element IN creates a SelectExpression with two ForEach quantifiers (table scan + Explode(inList)) and an equality predicate correlating the column to the exploded value via QuantifiedObjectValue.

Full infrastructure ported: Placeholder, GraphExpansion, MatchableSortExpression, ValueIndexExpansion, predicate-to-Placeholder matching, AbstractDataAccessRule, generateDataAccess planner phase, PredicateMultiMap.

---

### D-6: DeMorgan / BooleanNormalizer placement

**Java:** `BooleanNormalizer` applies De Morgan distribution + nested-NOT push-down as a separate pre-CNF pass.

**Go:** Optional via `NormalizationRules()` that callers compose with the simplification driver. This is architecturally correct — Java also separates De Morgan from the default simplification pipeline. Go's `DefaultSimplifyRules()` explicitly documents why De Morgan is excluded (node-increasing, violates strict reduction).

**Status:** Not a divergence. Both Java and Go separate normalization from default simplification.

---

### ~~D-7: AggregateDataAccessRule — multi-aggregate matching~~ — DONE (nightshift-84)

AggregateDataAccessRule now handles multi-aggregate GROUP BY queries by finding one AggregateIndexMatchCandidate per aggregate (all with identical grouping columns) and building a RecordQueryMultiIntersectionOnValuesPlan. Result value combines grouping columns (from first child) + one aggregate per child via RecordConstructorValue.

---

## GO EXTENSIONS (features Go has that Java doesn't)

### E-1: In-Memory Sort (RecordQueryInMemorySortPlan)

Physical operator that materializes inner result and sorts in memory. Fallback when no index satisfies ORDER BY. Java's RemoveSortRule eliminates sorts or fails. Go-only correctness improvement.

---

## MISSING JAVA INFRASTRUCTURE (not yet ported)

### M-1: TranslationMap — PARTIALLY PORTED (swingshift-83)

**Ported:** `AliasMap` (bidirectional CorrelationIdentifier mapping, immutable, builder pattern) in `alias_map.go`. `ForwardMap()` bridge to existing `values.RebaseValue()`. 8 unit tests.

**Remaining:** Wire `AliasMap` into `PushRequestedOrderingThroughSortRule` alias translation (currently not needed — Go's ordering parts are pure FieldValues). `values.RebaseValue` now handles 13 types (QuantifiedObjectValue, ArithmeticValue, CastValue, PromoteValue, ScalarFunctionValue, RecordConstructorValue, NotValue, AggregateValue, FieldValue, ConstantValue, NullValue, BooleanValue, ParameterValue). Remaining types (AndOrValue, LikeOperatorValue, PickValue, etc.) can be added when needed. Build `DecorrelateValuesRule` using the AliasMap infrastructure.

Gates: DecorrelateValuesRule (scalar subqueries), AbstractDataAccessRule, MatchPartition infrastructure.

### M-2: MatchPartition / PartialMatch / Compensation — MOSTLY PORTED (dayshift-85)

Foundation types complete: TranslationMap, BiMap (structural equality), GroupByMappings, MatchedOrderingPart, MatchInfo (Regular + Adjusted + Builder), Compensation (No/Impossible/ForMatch), PartialMatchImpl, MatchPartition, SingleMatchedAccess, MaxMatchMap (with TranslateQueryValueMaybe/PullUpMaybe/AdjustMaybe), Traversal (candidate tree walker), Value.Replace tree substitution.

Matching rules wired into planner: MatchLeafRule (leaf expressions), MatchIntermediateRule (composing child matches), AdjustMatches (absorbing candidate-side expressions). All three fire during EXPLORE via MatchingRules(). SelectMergeRule normalizes nested Select/Filter combinations.

**dayshift-85 progress:**
- PredicateCompensationMap: real identity-keyed map (was entry-count stub). Methods: Entries, ApplyCompensations, Amend.
- PredicateCompensationFunc: extended with Amend + ApplyCompensationForPredicate. OfPredicateCompensation factory wraps predicates with alias-rebase translation.
- ResultCompensationFunction: extended with Amend + ApplyCompensationForResult. ResultCompensationOfValue factory.
- ForMatchCompensation: Apply (wraps expression with residual filters) + Intersect (combines compensations for index intersections).
- QueryPlanConstraint: ported from placeholder to full type (wraps QueryPredicate, IsTautology/IsConstrained).

**Complete.** Full recursive MaxMatchMap.compute ported (dayshift-85): incrementalValueMatcher with lazy candidate tracking, cross-product children enumeration, memoization, branch-and-bound pruning via maxDepthBound. Only the Simplification variant-expansion step (MaxMatchMapSimplificationRuleSet) is deferred — it generates algebraically equivalent rewrites and requires a separate rule engine.

### M-3: PushReferencedFields rules — DONE (dayshift-85)

All 4 rules ported: Filter, Select, Distinct, Unique. ReferencedFields constraint type + ReferencedFieldsConstraintKey. Propagates top-down during PLANNING constraint pass.

### M-4: PlanPartition property-based matching — DONE (dayshift-85)

Go has full PlanPartition infrastructure: ToPlanPartitions, RollUpPlanPartitions, AllAttributesExcept, per-expression property isolation. Matcher abstraction added: FilterPlanPartitions, SelectMinCostPartition, WhereDistinct/WhereStored/WhereOrdered convenience filters.

---

## PRIORITY ORDER FOR REMAINING 1:1 ALIGNMENT

1. ~~**D-7** (multi-aggregate) — DONE~~
2. **Scalar subqueries** — biggest user-visible gap. DecorrelateValuesRule landed (dayshift-85). Remaining: wire into SQL translator for `(SELECT MAX(v) FROM t)` patterns, correlated subquery infrastructure. ~1-2 shifts.
3. ~~**D-8** (CardinalityProperty split) — DONE~~
4. ~~**D-11** (ConstantObjectValue promotion) — DONE~~
5. ~~**D-2** (PushOrdering constraint vs structural) — DONE~~
6. ~~**D-5** (InComparison architecture) — DONE~~
