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

### M-1: TranslationMap — COMPLETE (swingshift-86)

**Ported:** `AliasMap` (bidirectional CorrelationIdentifier mapping, immutable, builder pattern) in `alias_map.go`. `ForwardMap()` bridge to `values.RebaseValue()`. 8 unit tests.

`values.RebaseValue` is now generic (swingshift-86): leaf values with correlation aliases (QuantifiedObjectValue, QuantifiedRecordValue, ExistsValue, ScalarSubqueryValue, ObjectValue, UnmatchedAggregateValue, ConstantObjectValue) remap directly; all non-leaf values recurse children and reconstruct via `WithChildren()`. Covers all ~37 Value types automatically.

`values.ValuesStructurallyEqual` rewritten (swingshift-86) to use `EqualsWithoutChildren` + recursive `Children()` comparison — matches Java's `semanticEquals` pattern exactly. `EqualsWithoutChildren` has exhaustive dispatch for all 37 types (no reflect fallback). `GetCorrelatedToOfValue` handles all 7 correlation-bearing leaf types.

### M-2: MatchPartition / PartialMatch / Compensation — COMPLETE (swingshift-86)

Foundation types complete: TranslationMap, BiMap (structural equality), GroupByMappings, MatchedOrderingPart, MatchInfo (Regular + Adjusted + Builder), Compensation (No/Impossible/ForMatch), PartialMatchImpl, MatchPartition, SingleMatchedAccess, MaxMatchMap (with TranslateQueryValueMaybe/PullUpMaybe/AdjustMaybe), Traversal (candidate tree walker), Value.Replace tree substitution.

Matching rules wired into planner: MatchLeafRule (leaf expressions), MatchIntermediateRule (composing child matches), AdjustMatches (absorbing candidate-side expressions). All three fire during EXPLORE via MatchingRules(). SelectMergeRule normalizes nested Select/Filter combinations.

**Compensation infrastructure — fully ported (swingshift-86):**
- Compensation.Intersect: full Java algorithm — recursive child intersection, GroupByMappings merge (union matched, filter unmatched), predicate map intersection (keep common, amend), quantifier set operations (union matched, intersect unmatched), impossibility validation. 9 tests.
- Compensation.Union: full Java algorithm — merge predicate maps, multi-ForEach impossibility check, recursive child union. 7 tests.
- UnmatchedAggregateValue: new non-evaluable marker Value for unmatched aggregates during index matching.
- Real Amend implementations: PredicateCompensationFunc.Amend and ResultCompensationFunction.Amend now call replaceUnmatchedAggregateValues (ports Java's replaceNewlyMatchedValues). IsImpossible computed at construction via predicateContainsUncompensatableValues / valueContainsUnmatchedAggregates.
- ApplyCompensationForPredicate/Result: uses translateValueCorrelations (full TranslationMap function application), not just alias rebasing.
- PredicateCompensationMap.Get: identity-keyed lookup for intersection algorithm.
- replacePredicateValues: ports Java's QueryPredicate.replaceValuesMaybe.

**PullUp type ported (swingshift-86):** PullUp chain for translating values across expression boundaries during matching. PullUpValueMaybe walks the chain bottom-up via MaxMatchMap. ComputeResultCompensation uses PullUp to determine whether result-shape compensation is needed.

**Remaining:** MaxMatchMap Simplification variant-expansion (MaxMatchMapSimplificationRuleSet) — generates algebraically equivalent rewrites and requires a separate rule engine. Deferred. PullUp.Visitor pattern (MatchPullUp, PullUpVisitor) — requires expression visitor infrastructure.

### M-3: PushReferencedFields rules — DONE (dayshift-85)

All 4 rules ported: Filter, Select, Distinct, Unique. ReferencedFields constraint type + ReferencedFieldsConstraintKey. Propagates top-down during PLANNING constraint pass.

### M-4: PlanPartition property-based matching — DONE (dayshift-85)

Go has full PlanPartition infrastructure: ToPlanPartitions, RollUpPlanPartitions, AllAttributesExcept, per-expression property isolation. Matcher abstraction added: FilterPlanPartitions, SelectMinCostPartition, WhereDistinct/WhereStored/WhereOrdered convenience filters.

---

## PRIORITY ORDER FOR REMAINING 1:1 ALIGNMENT

1. ~~**D-7** (multi-aggregate) — DONE~~
2. **Scalar subqueries** — Go-only extension (Java's grammar has no `subqueryExpressionAtom`). DecorrelateValuesRule landed (dayshift-85) for other values-box patterns. Scalar subquery translation is a Go extension, clearly separated.
3. ~~**D-8** (CardinalityProperty split) — DONE~~
4. ~~**D-11** (ConstantObjectValue promotion) — DONE~~
5. ~~**D-2** (PushOrdering constraint vs structural) — DONE~~
6. ~~**D-5** (InComparison architecture) — DONE~~
7. **6 unported ImplementationCascadesRules** — MergeFetchIntoCoveringIndexRule, PushDistinctBelowFilterRule, PushDistinctThroughFetchRule, PushFilterThroughFetchRule, PushMapThroughFetchRule, PushSetOperationThroughFetchRule. All require RecordQueryFetchFromPartialRecordPlan (covering index fetch plan type) which Go doesn't have. ~9-15 shifts total.
8. **MaxMatchMap Simplification** — variant-expansion step generating algebraically equivalent value rewrites. Requires separate simplification rule engine (needs Type.Record field metadata). Deferred.
9. **SelectExpression.compensate** — full predicate compensation computation. Needs expression-level `compensate` method port (~100 LOC). PullUp + ComputeResultCompensation already ported (swingshift-86).
10. **MaxMatchMap ValueEquivalence parameter** — cross-alias matching in PullUpValueMaybe. ComputeMaxMatchMap currently uses structural matching only; Java passes ValueEquivalence for cross-scope comparisons.
