# RFC-163 — Cascades bug-hunt correctness fixes (batch)

Status: Draft (awaiting Graefe + Torvalds ACK)
Branch: `hunt/cascades-bug-hunt`
Scope: query-engine (Graefe-gated)

## Motivation

A multi-agent + differential bug hunt across the Cascades planner/optimizer/executor
(14 subsystems, adversarially verified, cross-checked against the Java 4.12.11.0 spec)
surfaced 9 confirmed defects. This RFC covers the 6 with small, conservative,
clearly-correct fixes. Each is pinned by a red→green regression test; the full
`sqldriver` suite is green. The 3 remaining (cost selectivity, NULLS ordering,
plan non-determinism) are riskier and tracked in TODO.md for their own cycles.

All 6 fixes are **conservative**: they make the planner decline an unsafe
optimization and fall back to an already-correct path, or tighten a rewrite's
guard. None introduces a new plan shape; none can produce a wrong row that the
buggy code didn't already.

## Fixes

### 1. AGG-RESIDUAL (critical) — `rule_aggregate_data_access.go`
`AggregateDataAccessRule.OnMatch` yielded an aggregate-index scan whenever the
grouping keys + aggregate matched a candidate, but `buildAggScanPrefix` only turns
grouping-key **equalities** into scan bounds. Any other input filter (a non-group
column, or an inequality) was silently dropped — the precomputed aggregate is over
ALL rows, so the result is wrong (`WHERE f=1 GROUP BY g` returned the unfiltered
sum). Java runs the full data-access compensation (`reduce(impossible, intersect)`
→ `isImpossible()` → `applyAllNeededCompensations`) and declines the match when a
residual can't be compensated. Fix: `aggInnerFilterFullyConsumable` — if any input
filter predicate is not a grouping-key equality, decline the match (both the single-
and multi-aggregate paths) → StreamingAgg-over-filtered-scan fallback.

### 2. IN-LIMIT-NIL (critical) — `physical_limit_wrapper.go`
`physicalLimitWrapper.WithChildren` relinked its extracted inner only when
`isLeafReplaceable(inner)` (which excludes `Projection`, `InJoin`, …). For a
top-level `LIMIT` over a `Projection` over an IN-join data access, it kept the
eager nil-inner plan snapshot built at rule time → `Limit(Project(Fetch(<nil>)))`
/ `Limit(Project(InJoin(<nil>)))` → 0 rows (non-covering) or an execution error
(covering). This is the same bug class the fetch wrapper fixed under RFC-070. Fix:
always relink the extracted inner (`WithInner` preserves the static/runtime cap).

### 3. HAVING-PUSHDOWN (high) — `rule_push_filter_through_groupby.go`
`predicateReferencesOnlyKeys` inspected only the comparison LHS, so `HAVING g >
SUM(v)` (grouping-key LHS, aggregate RHS) was classified pushable and pushed below
the GroupBy onto the raw scan, where `SUM(v)` does not exist. The plan flipped
purely on operand order. Java pushes nothing through a GroupBy. Fix:
`comparandReferencesOnlyKeys` also requires the RHS comparand to be a grouping-key
field or a constant; the sound `g op <constant>` pushdown is preserved.

### 4. COUNT-COL-COVERING (high) — `rule_implement_streaming_agg.go`
`isCountOnlyAggregation` returned true for any `Function==AggCount`, so scalar
`COUNT(col)` force-marked the supporting index scan COVERING with zero columns →
`col` evaluates NULL for every row → `COUNT(col)=0`. Fix: gate on a true COUNT(*)
(operand nil or `ConstantValue{nil}`), mirroring the executor's `isCountStar`.
COUNT(col) now reads via Fetch (honors SQL NULL semantics); COUNT(*) still covers.

### 5. DISTINCT-UNIONALL (high) — `plan_properties.go`
`computeDistinctRecords` listed `RecordQueryUnionPlan` — Go's NO-DEDUP UNION ALL
variant — among the distinct-producing plans. `ImplementDistinctFinalRule` then
treated its partition as already distinct and elided an enclosing `SELECT DISTINCT`
→ duplicate rows. Fix: report it non-distinct (next to `UnorderedUnionPlan`). The
test that pinned the old assumption (`…_UnionPlanIsTrue`) is corrected to
`…_UnionPlanIsFalse`.

### 6. CAST-ROUND (low, Java parity) — `values/values.go`
`CAST(double AS INT/BIGINT)` used the pre-Java-7 `floor(x+0.5)` algorithm, which
rounds `0.49999999999999994`→1 and `-0.5`→-1, diverging from `java.lang.Math.round`
(Java 7+, JDK-6430675). Fix: a faithful bit-manipulation port of `Math.round`; the
already-integral branch returns `a` unchanged so the caller's overflow range-check
still fires (`CAST(1e20 AS BIGINT)` still errors).

## Risk / validation
- Conservative by construction (decline-to-correct-fallback / tighten-guard / exact-parity).
- Pins: 5 plan-only (`PlanQueryForTest`) + 3 FDB row-level + 1 values unit; full
  `sqldriver` suite green; full `just test` green.
- No wire-format, key-encoding, continuation, or index-entry change.
