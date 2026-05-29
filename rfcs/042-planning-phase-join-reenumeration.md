# RFC-042: FROM-order-independent multi-way join ordering

**Status:** v4 — root cause re-established from real instrumentation. The v1–v3
"task-engine ordering" framing was **wrong** (see Correction). The acceptance
probe `TestFDB_MultiwayJoinOrder_Probe` is RED; this RFC documents the actual,
multi-layer gap, lands the one layer that is a clean Java-divergence bug, and
scopes the remaining layers.

## What "done" means

`SELECT t1.id FROM <t1,t2,t3 in any order> WHERE t3.t2_id=t2.id AND t2.t1_id=t1.id`
must produce the **same** cost-optimal physical plan regardless of FROM-clause
order (chain: t1=1 row ← t2=20 ← t3=200; optimal drives from t1). Two opposite
FROM-orders ⇒ byte-identical EXPLAIN, driving from the 1-row table.

## Correction — what v1–v3 got wrong

v1–v3 claimed the blocker was a Cascades **task-engine ordering** bug (a
re-enumerated join associativity dropping because its sub-product reference
wasn't optimized before the parent's `ImplementNestedLoopJoinRule` ran, so
`findBestPhysicalExpr` returned nil). **Instrumentation disproved this:** there
are **zero** nil-child NLJ bails. Both associativities are built as physical
members. The earlier "step 1 made the probe pass" was an artifact — the probe
file was not yet registered in BUILD.bazel, so `--test.run` reported "no tests
to run" (a false green). Once gazelle registered it, the probe is RED.

## Actual root cause — three layers

Instrumented on the 3-table chain under both FROM-orders:

### Layer 1 (root-caused; fix is NOT a clean removal) — REWRITING did not produce a FROM-order-independent flat seed

The SQL→cascades translator builds a multi-table inner-join FROM as a **nested
binary** tree of 2-quantifier `SelectExpression`s (`Select(Select(a,b),c)`).
`SelectMergeRule` flattens this to the canonical flat 3-quantifier select — the
seed `PartitionSelectRule` needs to re-enumerate associativities from.

But Go carried a **Go-only** rule, `PushProjectionBelowJoinRule` (no Java
equivalent — Java's `PlanningRuleSet` has only `RemoveProjectionRule` +
`MergeProjectionAndFetchRule`, and prunes columns during PLANNING via
requested-value push-down). It matched a `LogicalProjectionExpression` over a
2-quantifier inner join and classified the projected columns **by string alias
prefix** (`strings.ToUpper(aliases[0])`). It fired **only when a projected
column mapped to a top-level join quantifier**:

* big-first (`SELECT t1.id FROM t3,t2,t1`): `t1` is a direct top-level quantifier
  of the outer join → rule fires → wraps the join's children in
  `LogicalProjectionExpression`s → those intervening projections **block
  `SelectMergeRule`** → no flat seed → PLANNING cannot re-enumerate → plan locked
  to the FROM-order shape `T1⋈(T2⋈T3)`.
* small-first (`SELECT t1.id FROM t1,t2,t3`): `t1` is buried in the sub-join;
  its alias matches neither top-level side → rule **bails** (the `default: return`
  arm) → flat seed survives → re-enumeration runs → optimal `(T1⋈T2)⋈T3`.

**Attempted fix — removal regresses recursive CTE.** The obvious fix (drop the
Go-only `PushProjectionBelowJoinRule` from `DefaultExpressionRules`) makes
big-first's PLANNING seed flat (verified) and `cascades_test` + all non-FDB
plan-harness/core-query targets green — **but the full `just test` gate fails**:
`TestFDB_RecursiveCTECrossJoin` returns 4 rows instead of 5 (wrong results),
plus `TestFDB_RecursiveCTERename` and `TestFDB_CascadesRecursiveCTE`. So this
Go-only rule is **load-bearing for recursive-CTE cross-join correctness** — it
papers over a real gap in the recursive-CTE / cross-join column handling that
Java handles without such a rule. Removing it outright is a correctness
regression and was reverted.

The clean fix must therefore be one of (Java-faithful, in order of preference):
1. **Fix the underlying recursive-CTE/cross-join column gap** so the rule is no
   longer needed, then remove it (Java has no such rule, so the gap is the real
   divergence). Largest, but eliminates the Go-only rule entirely.
2. **Let the flat seed form despite the pushed projections** — teach
   `SelectMergeRule` (or a projection pull-up in REWRITING) to flatten
   `Select → LogicalProjection → Select` so PartitionSelect gets a flat seed even
   when the rule fired. Preserves the recursive-CTE benefit.
3. **Narrow the rule** so it does not fire on the join shapes whose flattening
   PLANNING depends on — fragile; rejected unless 1/2 prove intractable.

Rule count stays 46 until one of these lands. This layer is root-caused but
**not yet fixed**.

### Layer 2 (OPEN) — re-enumerated associativities carry a residual-predicate penalty

With the flat seed, the top join reference now holds every associativity as a
physical member. But the cost model does not pick the cheapest. `PlanningCostModel`
(Java and Go identical) ranks by, in order: (#2) max data-access cardinality,
(#3) **normalized residual-predicate count**, … and only much later (#14) the
recursive join-order cost (RFC-041's `compareJoinOrdering`/`BestMemberCostWith`).

For the chain, the **FROM-order-native** associativity embeds *both* join
predicates as NLJ join conditions (resid=0), because the native sub-join exposes
the intermediate join column directly. The **re-enumerated** associativity is
built by `PartitionSelectRule`'s Case-3 path (lower select flows a
`RecordConstructorValue`; the upper predicate is translated to
`FieldValue(QOV(lowerQ), "_i")`). `ImplementNestedLoopJoinRule` does not
recognize that constructor-field correlation as an embeddable equi-join
condition, so it renders it as a **correlated `PredicatesFilter` on the inner**
(resid=2). Criterion #3 therefore prefers the resid=0 FROM-order-native shape
**before** join-order cost is ever consulted — so the planner always picks the
FROM-order associativity. (small-first looks correct only because its native
shape happens to be the optimal one.)

This is **not obviously a Java divergence**: Java's `PlanningCostModel`
(criterion #3, `NormalizedResidualPredicateProperty`) and `PartitionSelectRule`
(identical Case-3 `RecordConstructorValue` + `TranslationMap`) match Go.
Determining whether Java embeds the re-enumerated predicate cleanly (e.g. via
extra value-simplification of `FieldValue`-over-`RecordConstructor` that Go is
missing, collapsing the constructor reference back to the bare column so the
predicate stays embeddable) requires checking Java's value-simplification /
`RemoveProjectionRule` interaction on this shape.

### Layer 3 (OPEN) — no index-nested-loop join (SARGed join probe)

Even with secondary indexes on the join columns (`t2(t1_id)`, `t3(t2_id)`), the
planner produces full `Scan(T2)`/`Scan(T3)` NLJs, **not** correlated index
probes (`IndexScan(t3_by_t2, [=t2.id])`). With an index SARG the join predicate
would be consumed by the inner's index access (resid=0), tying the residual
criterion across associativities and letting join-order cost decide — the
realistic way this Cascades architecture does cost-based join ordering. Go's
data-access matching does produce single-table index scans
(`TestPlanHarness_Index*`), but no existing test pins a join whose inner is an
**index probe of a correlated join predicate**; `TestPlanHarness_JoinOnIndex`
only asserts `FlatMap`, never an inner index scan. Whether Go's
`abstract_data_access_rule`/match-candidate machinery matches a correlated
comparison predicate against an index candidate to yield a correlated index scan
is the open question for this layer.

## Direction

* **Layer 1: root-caused, NOT landed** — naive removal of the Go-only
  `PushProjectionBelowJoinRule` regresses recursive-CTE correctness. Pursue
  fix-option 1 (close the recursive-CTE/cross-join gap, then remove) or 2
  (flatten through the pushed projection). Java-parity: Java has no such rule, so
  the recursive-CTE path is a genuine Go gap to close.
* **Layers 2 & 3: open, and require Java-parity verification first** (CLAUDE.md:
  "verify Java actually supports it before treating a TODO as parity work").
  Both must be checked against Java's real behavior on this exact shape:
  - L2: does Java's value simplification keep the re-enumerated join predicate
    embeddable (resid=0)? If yes → port the missing simplification. If Java also
    renders a residual filter, the byte-identical assertion exceeds Java parity.
  - L3: does Java's data-access matching SARG a correlated join predicate against
    a secondary index (index-nested-loop join)? If yes → the gap is in Go's
    match-candidate machinery for correlated predicates. If no → the indexed
    probe path is a Go extension, allowed only with deep tests.

## Test plan

`TestFDB_MultiwayJoinOrder_Probe` (indexed join columns): (a) order-invariance —
both FROM-orders byte-identical EXPLAIN; (b) cost-optimal — drives from the 1-row
table, T3 reached last via its index, never full-scanned. Currently RED on L2/L3.
Plus the no-regression gate (46→45 rule count; plandiff conformance; stress-1M;
determinism 10×).

## Status progression

Draft v1–v3 (wrong root cause, retracted) → v4 (correct multi-layer root cause,
all three layers root-caused; none landed — L1 removal regresses recursive CTE)
→ Implemented when L1/L2/L3 are resolved (Java parity verified first) and the
probe is green under the full gate.
