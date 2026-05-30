# RFC-043: Generic N-way join execution (lift the 3-way scope limit)

**Status:** Draft v2 (Graefe + Torvalds NAK addressed — see Status progression)

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

### What exists, and what is genuinely missing

`rule_partition_select.go` contains a dead-coded **Case 3** (lines ~382–447):
when the upper correlates to ≥2 distinct lower aliases it builds a
`RecordConstructorValue{_0: QOV(la0), _1: QOV(la1), …}` + a `TranslationMap`
rewriting upper refs to `FieldValue(QOV(lowerQ), _i).col`. It is the Java-faithful
*generation*, gated off by the `multiAliasCase3` skip + `isThreeWay`.

But the generation existing is **not** the bulk of the work, and this RFC does
**not** reuse Case 3 — see Fix below, it is rejected. The genuine gap is on the
**result-value / execution** side: today's Case-2 lower flows a single
`QuantifiedObjectValue` (`GetFlowedObjectValue()`), i.e. one table's row. There is
**no** mechanism that makes an intermediate join flow *more than one* table's
columns, and Case 3's `RecordConstructorValue` ordinal flow is **0%** resolvable
by the executor (its own comment says "UNREACHABLE for n == 3"). That missing
multi-table flow — not the generation — is what this RFC builds.

## Fix

Make every re-enumerated intermediate join flow a **merged row** (qualified
`ALIAS.COL` keys for all live tables) instead of a single `QOV`, so the live
columns of every buried table accumulate up the join spine and the final
`Project` reads any table's column from the top merged row.

### Why a *binary* merge value suffices (the key correctness argument)

`JoinMergeResultValue` (`values/value_join_merge.go`) is **binary** —
`{OuterAlias, InnerAlias}`, `Evaluate` merges exactly two bindings. A single one
cannot carry ≥2 buried tables. **But it does not have to**, because its `Evaluate`
**preserves already-qualified keys** (`merged[k]=val` for any key containing `.`)
and qualifies only bare keys. So when its *outer* is itself a merged row carrying
`A.*, B.*`, those pass through untouched and only the inner table is freshly
qualified. A **chain** of binary merges therefore accumulates the full set
`A.*, B.*, C.*, …` up a left-deep spine. The merge is realized **per binary join
level** — never as one wide N-ary value — so **no new value type is needed**, and
this is exactly the `mergeRows` qualified-key propagation that already carries
`b/c/d` to the top today.

The whole bug is that the re-enumerated lower flows `GetFlowedObjectValue()` (a
single `QOV`, `quantifier.go`), which is *not* a merged row — so the chain breaks
at the bottom and the consumed inner table is dropped. The fix makes the
≥2-live-table lower flow `JoinMergeResultValue(outer, inner)` instead.

### Concrete changes

1. **Column-liveness set** (in `rule_partition_select.go`, per partition). Define
   `live(lower) = correlationClosure(upperPredicates ∪ resultValue) ∩ lowerAliases`
   — the lower aliases referenced by an upper join predicate **or** the result
   value, taken through the correlation order (`GetCorrelatedToOfValue` /
   `GetCorrelatedToOfPredicate` already give the per-node sets;
   `fullCorrelationOrder` already gives the transitive closure). This is Graefe's
   formulation: the output schema a group must flow is its consumers'
   reference set restricted to it. **Crucially it includes join keys upper levels
   need, not only the final projection** — a deep key live solely for an ancestor
   predicate must survive.
2. **Flow rule** keyed on `|live(lower)|`:
   - `|live| ≤ 1` → today's single-`QOV` Case-2, **unchanged** (the 3-way path is
     this degenerate case — no behavior change, pinned by an EXPLAIN-identity +
     determinism test).
   - `|live| ≥ 2` → the lower flows a merged row. Realized by
     `ImplementNestedLoopJoinRule` synthesizing `JoinMergeResultValue(outerAlias,
     innerAlias)` when it implements a re-enumerated join sub-select (binary, at
     the point outer/inner are known); the per-level chaining above carries all
     live columns. This is a **result-value change in the implement rule** — *not*
     "no value-eval changes"; it is small and reuses the existing binary value,
     but it is real and must be tested as such.
3. **Drop** `isThreeWay` and the `multiAliasCase3` skip; **keep** `disconnectedLower`
   (a cross-product lower is genuinely degenerate at any arity).

### Rejected: route (a) — nested `RecordConstructorValue` + `TranslationMap`

The dead-coded Case-3 path (ordinal `RecordConstructorValue` flowed up, upper refs
translated to `FieldValue(QOV(lowerQ), _i).col`) is **rejected, not held in
reserve.** It is Java's representation, but in Go it would require new
value-evaluation code to resolve ordinal field access into a constructor through
the nested merge — strictly more work than the qualified-merged-key flow above,
which already exists and chains. With aliases unified (below), the qualified-key
representation is semantically equivalent to Java's ordinal tuple, so there is no
reason to build the harder one. The dead Case-3 code is **deleted** as part of
this PR.

### Alias namespaces — a satisfied precondition (TODO 7.1, already DONE)

This flow is correct **because** quantifier aliases already equal table aliases:
TODO 7.1 ("Unify alias namespaces", TODO.md §7.1) is **merged** — its root-cause
fix in `mergeRows` (bare inner keys no longer overwrite qualified keys from nested
joins; the `!exists` guard) is *precisely* the qualified-key preservation this
RFC's chaining depends on. So 7.1 is a **precondition that holds**, not a future
gate or an escalation trigger: re-enumerated `q$N` quantifiers resolve their
columns under the unified table-qualified names, end to end. (Earlier drafts
framed 7.1 as a risk held in reserve — that was stale; it is done.)

## Performance

- **Search space:** the rule already enumerates all `2^N − 2` bipartitions and
  recurses; the cost model (RFC-041) ranks them and Cascades memoizes shared
  sub-products. Dropping `isThreeWay` does **not** change the enumeration — only
  which partitions yield a *plannable* (vs skipped) member. N is small in
  practice (join arity); the `disconnectedLower` skip still prunes cross products.
- **No extra columns on the wire.** This is read-path execution only — the merged
  intermediate value lives in-cursor; nothing about record/index/continuation
  encoding changes. Wire compat is untouched.
- **Merged rows are not wider than the original plan.** The non-re-enumerated plan
  already flows a `JoinMergeResultValue` per join (built by the translator,
  `cascades_translator.go`); this change makes the *re-enumerated* sub-joins flow
  the same. The live-set restriction means an intermediate row carries only the
  columns some ancestor needs — never more than the full merge the original plan
  flowed.
- **Stress-1M before/after** must stay within thresholds (the flow change adds only
  already-needed columns to an in-memory merged row; it does not change the chosen
  plan for 2/3-way, which dominate the corpus). Recorded in TODO.md.

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
- Unit test for the column-liveness set (analogous to `TestLowerAliasesConnected`).
  **Must include the case Graefe flagged:** a deep table whose column is live
  *only* for an ancestor join predicate (not the final projection) — e.g. a chain
  where an intermediate key would be dropped if liveness only tracked the
  projection. That column must be in `live(lower)`.

## Status progression

Draft (v1) → **v2 (addressed Graefe + Torvalds NAK: 7.1 reframed as a satisfied
precondition not a future gate; route (a) `RecordConstructorValue` rejected, not
held in reserve; committed to binary-`JoinMergeResultValue`-chained flow with the
explicit "why binary suffices" argument; specified the `live(lower)` set; dropped
the "~80% written" claim)** → reviewed (Graefe + Torvalds ACK) → implemented
(`isThreeWay` gate + `multiAliasCase3` skip + dead Case-3 removed;
`TestFDB_NwayJoinOrder` green; `_Limit` test flips to positive) → PR + @claude
LGTM.
