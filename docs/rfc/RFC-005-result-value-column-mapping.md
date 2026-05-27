# RFC-005: Result-Value-Based Column Mapping

## Status: Proposed

## Problem

Go derives column order and field resolution from the physical plan tree. `deriveColumnsFromPlan` walks NLJ/FlatMap plans and constructs column definitions from outer-first, inner-second order. Predicate scope resolution in `yieldGeneralFlatMap` classifies predicates by string-matching quantifier aliases. This breaks when:

1. **Commutative join swap** changes physical outer/inner order (columns reversed)
2. **Multi-table inner joins** hide table aliases behind quantifier aliases (predicate misclassification)
3. **Correlated plans in materialization contexts** (EXISTS NLJ materializes a correlated FlatMap once)

The winner-based plan selection (RFC-004) exposed all three because the cost model can now pick any equivalent plan, not just the first by insertion order. Four targeted workarounds were applied:

- `SQLColumnOrderReversed` on NLJ and FlatMap (column order)
- `collectPlanAliases` + multi-table inner skip in `yieldGeneralFlatMap` (predicate scope)
- `findBestNonCorrelatedPhysicalExpr` in `implementExistsJoin` (materialization safety)
- NLJ always-yielded for correlated joins as fallback

These work but don't compose cleanly. Each is a reaction to the previous one exposing another edge case.

## Root Cause

Java doesn't have these problems because every plan carries a `resultValue` -- a `Value` expression (typically `RecordConstructorValue`) that specifies exactly which fields from which quantifier, in what order. When quantifiers swap, `resultValue.translateCorrelations(translationMap)` rewrites the value to match. Column order, predicate scope, and aggregation key resolution all derive from one source of truth.

Go's `resultValue` field exists on `SelectExpression` and `RecordQueryFlatMapPlan` but isn't wired into:
- `deriveColumnsFromPlan` (column derivation)
- `executePredicatesFilter` (field resolution)
- `executeAggregation` (GROUP BY key resolution)

## Proposed Fix

### Phase 1: Column derivation via resultValue

Replace `deriveColumnsFromJoin` and `deriveColumnsFromFlatMap` tree-walking with `resultValue`-based derivation:

```go
func deriveColumnsFromResultValue(rv values.Value, md *recordlayer.RecordMetaData) []executor.ColumnDef {
    // Walk the Value tree (RecordConstructorValue, FieldValue, etc.)
    // to extract column names and types in the correct order.
    // The Value carries correlation identifiers that map to specific
    // quantifier aliases — no physical outer/inner assumption needed.
}
```

This eliminates `SQLColumnOrderReversed` on both NLJ and FlatMap. The `resultValue` IS the column spec regardless of physical plan direction.

### Phase 2: Predicate resolution via value algebra

Replace `predicateReferencesAlias` string matching with value-based correlation resolution:

```go
func predicateReferencesQuantifier(pred predicates.QueryPredicate, alias values.CorrelationIdentifier) bool {
    // Walk predicate's FieldValues, check if any resolve through
    // the given correlation identifier via the value algebra.
}
```

This eliminates `collectPlanAliases` and the multi-table inner skip. Predicates are classified by their actual correlation dependencies, not by string-matching table names in the plan tree.

### Phase 3: Correlation-aware materialization

Replace `findBestNonCorrelatedPhysicalExpr` with correlation analysis on the `resultValue`:

```go
func planIsCorrelatedTo(plan plans.RecordQueryPlan, alias values.CorrelationIdentifier) bool {
    // Check if the plan's resultValue (or any nested value) references
    // the given correlation identifier.
}
```

The EXISTS NLJ checks this before materializing. Plans correlated to the outer scope are re-executed per outer row (FlatMap semantics) instead of materialized once (NLJ semantics).

## What This Eliminates

| Current workaround | Replaced by |
|---|---|
| `SQLColumnOrderReversed` on NLJ + FlatMap | `resultValue` column derivation |
| `deriveColumnsFromJoin` / `deriveColumnsFromFlatMap` tree-walking | `deriveColumnsFromResultValue` |
| `collectPlanAliases` + `referencesAnyAlias` | Value-based correlation resolution |
| `yieldGeneralFlatMap` multi-table inner skip | Correct predicate classification via value algebra |
| `findBestNonCorrelatedPhysicalExpr` | `planIsCorrelatedTo` analysis |
| NLJ always-yielded for correlated joins | Not needed when FlatMap predicates are correct |

## Scope

~200-300 lines, mostly in:
- `pkg/relational/core/embedded/cascades_generator.go` (column derivation)
- `pkg/recordlayer/query/executor/executor_new_plans.go` (predicate resolution)
- `pkg/recordlayer/query/plan/cascades/rule_implement_nested_loop_join.go` (cleanup workarounds)

## Non-Goals

- Full Java `Value` algebra port (translation maps, simplification). Only the column/field resolution path.
- Changing the `resultValue` construction in the plan visitor. The existing values are correct; they just aren't used for column derivation.

## References

- Java: `RecordQueryFlatMapPlan.java:124` — `resultValue.eval(store, nestedContext)`
- Java: `Value.translateCorrelations()` — rewrites values when quantifiers change
- RFC-004: Winner-based plan selection (the change that exposed these issues)
