# RFC-032: QOV-based FieldValue Migration — Delete stripAlias*

Status: Draft

## Problem

10 `stripAlias*` calls across the NLJ rule (8) and PushFilterBelowJoinRule (2) do string-based alias prefix stripping on FieldValues. When a predicate like `A.NAME = 'foo'` is pushed to a child scan, `stripAlias` converts the flat `FieldValue{Field: "A.NAME"}` to `FieldValue{Field: "NAME"}` so it matches the scan's unqualified row keys.

This works for simple 2-table joins but breaks on:
- **3+ table joins**: wrong-prefix strip when the same column name appears in multiple tables
- **Self-joins**: both sides share the same table name, stripping is ambiguous
- **Correlated subqueries with ambiguous column names**: no strip or wrong strip

Java uses `Value.rebase(AliasMap)` — structural `CorrelationIdentifier` retargeting, zero string manipulation.

## Investigation

Go already has the infrastructure:

1. **QOV-based FieldValues**: `FieldValue(QOV(correlationId), "column")` exists and is used in correlated scan construction (`tryFlatMapPlan`), EXISTS builders (`qualifyBareFieldValue`), and index matching.

2. **The SQL resolver already produces QOV-based FieldValues** for multi-source scopes (`ResolveIdentifier` at `expr.go:227-233`). When `needsQualification` is true, it returns `NewFieldValue(NewQuantifiedObjectValue(corrID), field, typ)`.

3. **`executePredicatesFilter` supports alias binding**: `NewRecordQueryPredicatesFilterPlanWithAlias` binds the current row under a correlation alias at execution time, so QOV-based predicates resolve via `evaluateCorrelated`.

4. **`yieldGeneralFlatMap` already uses `WithAlias`** without stripping — this is the correct pattern, already proven in production.

5. **TODO 7.1 (alias unification) is done**: quantifier aliases match table aliases at creation. `PartitionBinarySelectRule` uses `NamedForEachQuantifier(quantifier.GetAlias(), ...)` to preserve this.

## Fix

### 1. Fix `executePredicatesFilter` binding condition

Current code only binds the row under `innerAlias` when params/subqueries/bindings
exist. A QOV predicate `qov(alias).col` over a **single-table scan** row (bare keys)
then can't resolve — the scan row has no `ALIAS.COL` qualified key. So we must bind
the alias for scan-level filters.

But the row must **not** be bound under a single alias when the filter's input is a
**merged join row** (NLJ / FlatMap output): there `qov(b).col` would bare-resolve to
whichever quantifier last wrote the bare key. On a null-filled LEFT JOIN row (b absent)
that wrongly picks up the outer row's bare value instead of NULL.

Change: `bindAlias := innerAlias != "" && !producesMergedRows(inner)`. Scan/index inners
bind the alias (bare lookup); NLJ/FlatMap inners skip the binding and resolve through the
qualified-key `Datum` path. Plus a nil-`evalCtx` guard.

### 2. Replace `stripAlias` + bare filter with `WithAlias` filter (NLJ rule, 8 sites)

Every `stripAliasFromPredicates(preds, prefix) + NewRecordQueryPredicatesFilterPlan(plan, stripped)` becomes `NewRecordQueryPredicatesFilterPlanWithAlias(plan, preds, correlation)`.

### 3. Replace `stripAlias` in PushFilterBelowJoinRule (2 sites)

Stop stripping. Keep QOV-based predicates as-is. Use `NamedForEachQuantifier` for the filter's inner quantifier so its alias matches the predicate's QOV correlation.

### 4. Thread correlation through `tryPushPredicatesIntoScan`

Add correlation parameter so fallback `NewRecordQueryPredicatesFilterPlan` becomes `WithAlias`.

### 5. Delete dead code

Remove `stripAliasFromPredicates`, `stripAliasPredicate`, `stripAliasValue`, `stripAliasFromPredicates` (NLJ wrapper).

## Performance

No regression — replaces string manipulation (allocations, prefix scans) with a direct pointer comparison on CorrelationIdentifier in the eval context. Execution-time binding is an O(1) map insertion already present in the `WithAlias` path.

## Test plan

- All 46 test targets pass (no regression)
- Existing join tests (inner, left outer, cross, EXISTS, NOT EXISTS, 3-way chained, self-joins) exercise the changed code paths
- Determinism checks on join-heavy tests (10 runs each)
- Specific focus: `TestFDB_JoinChained`, `TestFDB_SelfJoin`, `TestFDB_CorrelatedSubquery` shapes
