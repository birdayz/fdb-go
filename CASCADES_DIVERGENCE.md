# Cascades Divergences from Java

Comprehensive audit of all known divergences between Go's Cascades implementation and Java's `fdb-record-layer-core` (tag 4.11.1.0). Tracked for 1:1 alignment work.

---

## RESOLVED DIVERGENCES

### ~~D-1: Sort elimination phase placement~~ — DONE (dayshift-82)

Removed Go-only `SortOverOrderedElimRule` (EXPLORE phase). Sort elimination now happens exclusively in `ImplementSortRule` (PLANNING phase), matching Java's `RemoveSortRule` 1:1. Dead code file `rule_sort_over_ordered_elim.go` deleted.

### ~~D-3: DistinctOnUniqueElimRule — exploration vs physical planning~~ — MOSTLY DONE (swingshift-83)

Removed Go-only `DistinctOnUniqueElimRule` (EXPLORE phase) and `ImplementDistinctRule` (BatchA EXPLORE phase). Distinct elimination + implementation now happens exclusively in `ImplementDistinctFinalRule` (PLANNING phase), matching Java's `ImplementDistinctRule` (ImplementationCascadesRule). PlanContext threaded through to PLANNING-phase rules via `FireImplementationRuleWithContext`. Dead code files deleted.

**Remaining gap:** Go's elimination check uses logical-level PK column coverage (walks LogicalProjectionExpression to check projected fields against PK). Java's check uses physical-level `DistinctRecordsProperty` on the plan partition. Go's approach misses cases where a unique-index scan makes DISTINCT redundant without explicit PK column projection. Fixing this requires aligning Go's `computeDistinctRecords` for `RecordQueryProjectionPlan` to distinguish pass-through projections from reshaping ones (matching Java's `RecordQueryMapPlan` logic).

### ~~D-9: PatternForLikeValue DOTALL mismatch~~ — RETRACTED (dayshift-82)

No divergence exists. Java's `Pattern.compile(rhs)` does NOT use DOTALL. Go's default regexp behavior already matches.

### ~~D-10: InOpValue numeric coercion~~ — DONE (dayshift-82)

`equalsAny` now promotes mixed int/float pairs to float64 for comparison, matching Java's `Comparisons.evalComparison(EQUALS)`.

### ~~D-12: GetEqualityBoundValues semantics~~ — DONE (dayshift-82)

`GetEqualityBoundValues` now uses "any binding fixed" semantics (break on first fixed), matching Java's `Multimaps.filterValues(isFixed)`.

---

## OPEN ARCHITECTURAL DIVERGENCES

### D-2: PushOrdering rules — structural rewrite vs constraint propagation

**Java:** `PushRequestedOrdering*` rules extend `CascadesRule` and implement `PreOrderRule`. They run during the PLANNING phase in pre-order (top-down). They push ordering CONSTRAINTS to child References via the constraint map — no structural tree changes.

**Go:** `PushOrderingThrough*` rules are ExpressionRules that fire during EXPLORE phase. They perform STRUCTURAL REWRITES — physically moving Sort nodes below Filter/GroupBy/Distinct/Union/etc, creating new expression tree variants in the memo.

**Consequence:** Go's memo contains structural variants (Sort-below-Filter as alternative to Sort-above-Filter). Java's memo doesn't — Sort stays in place, constraints propagate. Both produce correct results but Go's memo is larger and the optimization path is different.

**Fix:** Convert all 10 `PushOrderingThrough*` rules from ExpressionRule (structural rewrite) to ImplementationRule (constraint push).

**Effort:** ~2-3 shifts. Major architectural change touching 10 rules + all dependent tests.

---

### D-4: Cost model — Go-native vs Java Postgres-inspired

**Java:** Complex cost model (Postgres-inspired, bit-precise). Memoized on equivalence classes. Multi-dimensional (CPU, I/O, memory).

**Go:** Simplified Go-native cost model (RFC-024). Cardinality + CPU heuristic. Computed on demand, not memoized.

**Fix:** Intentional design decision (RFC-024). Not a bug — deliberate simplification.

**Effort:** N/A (by design).

---

### D-5: InComparisonToExplodeRule architecture

**Java:** `InComparisonToExplodeRule` creates a `SelectExpression` with a `ForEach` quantifier over `ExplodeExpression`, then `AbstractDataAccessRule` resolves predicates against index candidates within the SelectExpression.

**Go:** Simplified architecture. Multi-element IN uses Union approach where each filter leg independently matches indexes.

**Fix:** Port `AbstractDataAccessRule` for `SelectExpression` predicates. Requires `SelectExpression` + `ExplorationCascadesRule` + `TranslationMap` infrastructure.

**Effort:** ~2-3 shifts (gated on M-1).

---

### D-6: DeMorgan / BooleanNormalizer placement

**Java:** `BooleanNormalizer` applies De Morgan distribution + nested-NOT push-down as a separate pre-CNF pass.

**Go:** Optional via `NormalizationRules()` that callers compose with the simplification driver. This is architecturally correct — Java also separates De Morgan from the default simplification pipeline. Go's `DefaultSimplifyRules()` explicitly documents why De Morgan is excluded (node-increasing, violates strict reduction).

**Status:** Not a divergence. Both Java and Go separate normalization from default simplification.

---

### D-7: AggregateDataAccessRule — single-aggregate only

**Java:** Handles multi-aggregate matching via intersection of aggregate indexes.

**Go:** Simplified to single-aggregate matching only.

**Fix:** Port multi-aggregate matching. Requires intersection infrastructure.

**Effort:** ~1 shift.

---

### D-8: CardinalityProperty coupling to Cost

**Java:** `CardinalitiesProperty` is a separate class with min/max bounds.

**Go:** Cardinality is a field on `Cost`, shared computation.

**Fix:** Split into separate property when needed.

**Effort:** ~0.5 shift.

---

### D-11: ConstantObjectValue type promotion

**Java:** `ConstantObjectValue.eval` consults `EvaluationContext.dereferenceConstant` + `PromoteValue.isPromotionNeeded` for type promotion.

**Go:** Looks up via `ConstantDeref` interface but does NOT handle type promotion — value returned as-is.

**Status:** Incomplete. Becomes real when execution routes through ConstantObjectValue with type-mismatched constants.

**Fix:** Wire type promotion through SQL-type coercion.

**Effort:** ~0.5 shift.

---

## GO EXTENSIONS (features Go has that Java doesn't)

### E-1: In-Memory Sort (RecordQueryInMemorySortPlan)

Physical operator that materializes inner result and sorts in memory. Fallback when no index satisfies ORDER BY. Java's RemoveSortRule eliminates sorts or fails. Go-only correctness improvement.

---

## MISSING JAVA INFRASTRUCTURE (not yet ported)

### M-1: TranslationMap — PARTIALLY PORTED (swingshift-83)

**Ported:** `AliasMap` (bidirectional CorrelationIdentifier mapping, immutable, builder pattern) in `alias_map.go`. `ForwardMap()` bridge to existing `values.RebaseValue()`. 8 unit tests.

**Remaining:** Wire `AliasMap` into `PushRequestedOrderingThroughSortRule` alias translation (currently not needed — Go's ordering parts are pure FieldValues). Extend `values.RebaseValue` to handle remaining Value types (currently handles QuantifiedObjectValue, ArithmeticValue, CastValue, ScalarFunctionValue, RecordConstructorValue, FieldValue, ConstantValue, NullValue, BooleanValue, ParameterValue; missing: PromoteValue). Build `DecorrelateValuesRule` using the AliasMap infrastructure.

Gates: DecorrelateValuesRule (scalar subqueries), AbstractDataAccessRule, MatchPartition infrastructure.

### M-2: MatchPartition / PartialMatch / Compensation

Required for advanced index matching (partial index utilization, compensation predicates). Gates: covering index via Cascades.

### M-3: PushReferencedFields rules (5 rules)

Column pruning optimization. Reduces I/O by tracking which columns each operator needs.

### M-4: PlanPartition property-based matching

Java's ImplementationRules match against PlanPartition property sets (ordering, distinct, stored-record). Go approximates with simpler partition iteration.

---

## PRIORITY ORDER FOR REMAINING 1:1 ALIGNMENT

1. **Scalar subqueries** — biggest user-visible gap. Needs DecorrelateValuesRule + SelectExpression. AliasMap (M-1) now ported as foundation. ~2-3 shifts.
2. **D-7** (multi-aggregate) — 1 shift
3. **D-8** (CardinalityProperty split) — 0.5 shift
4. **D-11** (ConstantObjectValue promotion) — 0.5 shift (not triggered yet)
5. **D-2** (PushOrdering constraint vs structural) — 2-3 shifts
6. **D-5** (InComparison architecture) — 2-3 shifts (M-1 foundation now available)
