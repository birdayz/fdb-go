# RFC-043: Generic N-way join execution (lift the 3-way scope limit)

**Status:** Draft

**Epic:** RFC-038 (multi-way join ordering). This is **PR-D** — it removes the
`isThreeWay` gate RFC-042 installed and makes re-enumerated joins of *any* arity
return correct rows.

## Problem

RFC-042 made 3-way joins FROM-order-independent and cost-optimal, but explicitly
scoped the re-enumeration to `n == 3` (`rule_partition_select.go`, the
`isThreeWay := n == 3` gate). For `n > 3` the rule falls back to Java's original
classification, which leaves those FROM-orders **unplannable** — a 4-way chain
join fails to plan loudly (`could not plan query`), exactly as on pre-RFC master.

That is honest (a loud failure, never silent wrong rows — pinned by
`TestFDB_MultiwayJoinOrder_Limit`) but it is a real capability gap: ≥4-way joins
do not work in this port at all. Java supports arbitrary arity. This RFC closes
the gap.

## Investigation — why ≥4-way breaks at execution (not planning, not cost)

I traced the failure by temporarily widening the gate to `n == 4` and
instrumenting. The 4-way chain `SELECT a.val FROM a,b,c,d WHERE a.id=b.a_ref AND
b.id=c.b_ref AND c.id=d.c_ref` **plans and executes** — it does not fail to
plan, and the joins all fire (one row out). The plan:

```
Project([A.VAL],
  NestedLoopJoin(INNER, [c.id=d.c_ref],
    NestedLoopJoin(INNER, [b.id=c.b_ref],
      FlatMap(outer=Scan(B), inner=Scan(A, [a.id=b.a_ref])),   <-- A consumed here
      Scan(C)),
    Scan(D)))
```

Probing each deep-table column individually pins the mechanism exactly:

| projection | result |
|---|---|
| `a.val` | **NULL** |
| `a.id`  | **NULL** |
| `b.id`, `b.a_ref`, `c.id`, `d.id` | correct |

**Every column of the deepest table A is lost; `b/c/d` survive.** The join
predicate `a.id = b.a_ref` is evaluated *inside* the deepest `FlatMap`, where A
is bound — so the join fires correctly — but the `FlatMap` flows only its
**outer** (B) upward. A is consumed for its predicate and then **dropped from the
merged row** that travels to the top. The top-level `Project([A.VAL])` reads a
column that no longer exists → NULL.

### Root cause

The 3-way path works because an intermediate join only ever needs to flow **one**
table's columns up. RFC-042's Case-2 flows a single `QuantifiedObjectValue` —
`NamedForEachQuantifier(lowerAlias, lower)` with result `QOV(lowerAlias)` — and
the executor's merged-row machinery carries that one table's qualified columns
(`ALIAS.COL`) to the parent. For `n ≥ 4`, an intermediate join must carry **more
than one** table's columns: the join keys the upper levels need **plus**
projections from tables buried below it. A single `QOV` cannot express that, so
the consumed-but-still-needed table is dropped.

### There is no generation-only shortcut

One might hope to *avoid* multi-column intermediate flow by choosing a
decomposition where every projected/joined table is terminal (a direct upper
quantifier). That fails in general: consider `SELECT b.val FROM a,b,c,d WHERE
<chain a—b—c—d>`. For `b` to be a terminal upper quantifier, the lower must be
`{a,c,d}` — but `a` only connects to `b`, so `{a,c,d}` is **disconnected** (a
cross product) and is correctly skipped. Every valid decomposition therefore
buries `b` inside a join, so its columns **must** be flowed up through that join.
Multi-column intermediate flow is mandatory, not avoidable.

### What already exists (the scaffold)

`rule_partition_select.go` already contains **Case 3** (lines ~382–441): when the
upper correlates to ≥2 distinct lower aliases it builds a
`RecordConstructorValue{_0: QOV(la0), _1: QOV(la1), …}` as the lower's result and
a `TranslationMap` rewriting upper predicates/result to
`FieldValue(QOV(lowerQ), _i).col`. This is the Java-faithful generation. It is
currently **dead-coded** for the active scope by the `multiAliasCase3` skip + the
`isThreeWay` gate. So the *generation* side is ~80% written; the gap is that the
**executor cannot resolve the multi-column flow** these constructors produce.

## Fix

Make an intermediate join flow **all live columns** of its lower — every column
any ancestor (upper join predicate or final projection) references — and make the
parent resolve them. Two implementation routes; this RFC commits to **(b)**.

### Route (b) — merged-qualified-key flow *(recommended)*

Instead of flowing a single `QOV` (Case-2) or a `RecordConstructorValue` whose
ordinals the executor can't resolve (Case-3), flow a **`JoinMergeResultValue`**
(the merged-row value introduced in RFC-042, commit `8cdd6d9f`) that produces a
row with **qualified `ALIAS.COL` keys for every live lower table**. The upper
levels then reference `A.COL` / `B.COL` directly and resolve through the
**existing** merged-key path (`fieldName` → `splitQualified` → `matchesAlias` →
`lookupJoinKey` in `streaming_cursors.go`). **No `TranslationMap`, no ordinal
field access, no new value-evaluation code** — this is precisely *why* `b/c/d`
already resolve today; it extends the same mechanism to the consumed-then-dropped
table.

Required pieces:
1. **Column-liveness pass** (Cascades rule): for each partition, compute the set
   of lower columns referenced by (i) upper predicates and (ii) the result value,
   transitively through the upper's own re-partitions. The lower must flow a
   merged value projecting exactly those `(alias, col)` pairs — not just the
   single correlated alias.
2. **Lower result = merged value over the live set** instead of `QOV(lowerAlias)`
   (Case-2) / ordinal `RecordConstructorValue` (Case-3). When the live set is a
   single full table, this degenerates to today's Case-2 (no behavior change for
   3-way).
3. **Drop the `isThreeWay` gate and the `multiAliasCase3` skip**, keeping only the
   `disconnectedLower` skip (cross products remain genuinely degenerate at any
   arity).

### Route (a) — nested `RecordConstructorValue` *(fallback if (b) can't express a shape)*

Keep Case-3's constructor flow and teach the executor to resolve
`FieldValue(QOV(lowerQ), _i).col` through the nested `FlatMap`/NLJ merge — i.e.
evaluate a field access into a `RecordConstructorValue` an intermediate join
flowed. More faithful to Java but touches the value-evaluation + merge core
(`flat_map_cursor.go::computeResult`, `streaming_cursors.go` merge). Held in
reserve; only needed if a bushy shape exists that route (b)'s flat qualified-key
merge cannot represent.

### Alias-namespace risk (TODO 7.1)

Each re-enumeration level mints fresh quantifier aliases (`q$N`). The #1
silent-bug source in this engine is `q$N` vs table-alias mismatch
(query-engine skill; TODO 7.1). Route (b) mitigates by keying the merged row on
**table-qualified** names end-to-end (the same `ALIAS.COL` convention that works
for `b/c/d` today), so projections resolve by table alias regardless of the
intermediate `q$N`. If a shape is found where table-qualified keys are
insufficient, that shape forces TODO 7.1 (alias unification) to land first — this
is the principal scoping risk and is called out as a gate, not assumed away.

## Performance

- **Search space:** the rule already enumerates all `2^N − 2` bipartitions and
  recurses; the cost model (RFC-041) ranks them and Cascades memoizes shared
  sub-products. Dropping `isThreeWay` does **not** change the enumeration — only
  which partitions yield a *plannable* (vs skipped) member. N is small in
  practice (join arity); the `disconnectedLower` skip still prunes cross products.
- **No extra columns on the wire.** This is read-path execution only — the merged
  intermediate value lives in-cursor; nothing about record/index/continuation
  encoding changes. Wire compat is untouched.
- **Stress-1M before/after** must stay within thresholds (the flow change can only
  add already-needed columns to an in-memory merged row; it does not change the
  chosen plan for 2/3-way, which dominate the corpus). Recorded in TODO.md.

## Test plan

The existing `TestFDB_MultiwayJoinOrder_Limit` flips from "4-way must fail loudly"
to "4-way must return correct rows" — it already asserts *correct-or-loud*, so it
becomes a positive correctness test for free once 4-way plans.

New `TestFDB_NwayJoinOrder` matrix — the dimensions that were unprobed and let the
3-way limit stand:
- **Shapes:** chain (`a—b—c—d`), star (`hub→{w,x,y,z}`), and a bushy/clique mix.
- **Projection position:** root, **middle** (the case with no terminal
  decomposition — the load-bearing one), and leaf table.
- **Arity:** 4-way and 5-way (prove it's general, not a 4-way special-case).
- **FROM-order independence:** every shape under ≥2 opposite FROM-orders →
  byte-identical EXPLAIN.
- **Correctness:** exact row values, not just counts (the 3-way bug shipped green
  precisely because a len-only check missed a `[""]` NULL row).
- **Cost-optimality:** EXPLAIN-pinned — drive from the smallest table, index-probe
  the largest last, never full-scan an indexed FK.
- Determinism 10× on each planner assertion (alias non-determinism is always a
  bug here).
- Unit test for the column-liveness pass (analogous to `TestLowerAliasesConnected`
  for the connectivity check).

## Status progression

Draft → reviewed (Graefe + Torvalds ACK) → implemented (route (b); `isThreeWay`
gate removed; `TestFDB_NwayJoinOrder` green; `Limit` test flipped to positive) →
PR + @claude LGTM. If route (b) hits an inexpressible shape, escalate to route
(a) and/or gate on TODO 7.1, documented here.
