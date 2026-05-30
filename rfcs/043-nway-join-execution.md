# RFC-043: Generic N-way join execution (lift the 3-way scope limit)

**Status:** Implemented (v3). Graefe + Torvalds ACK'd v2; implementation revealed
two honest scope corrections (cost-selection and enumeration-efficiency are
follow-ups — see "Delivered vs deferred" and Status progression). Re-review of v3
pending.

## Delivered vs deferred (read this first)

**Delivered — N-way EXECUTION correctness.** ≥4-way joins, which were unplannable
before this change, now return CORRECT rows: root, **middle-table** (no terminal
decomposition), and **star** projections, under both FROM-orders, with the
largest table reached via its FK index (never full-scanned). The mechanism is
general — verified correct up to 6-way. Pinned by `TestFDB_MultiwayJoinOrder_Nway`
+ `TestJoinMergeAllValue_*` unit tests. The 3-way probe (`..._Probe`) stays green.

**Deferred — two follow-ups, found during implementation, documented honestly:**
1. **Cost-optimal, order-invariant SELECTION for ≥4-way.** RFC-042 makes 3-way
   plans byte-identical + optimal across FROM-orders. For ≥4-way the larger bushy
   search space + merged-row costing does NOT yet converge both orders onto the
   single optimal left-deep index-probe chain (a 4-way may drive from a larger
   table). This is a **cost-model** matter (epic PR-D / RFC-041), orthogonal to the
   execution correctness delivered here. The test asserts correct rows + the
   index-probe property, NOT plan byte-identity.
2. **Enumeration efficiency.** Full bushy re-enumeration is exponential and the
   shared join sub-products do not yet intern tightly enough: a 4-way fits the
   default task budget (~57k tasks), but a 5-way needs ~200k and exceeds it, so it
   loud-fails by default (correct when given a larger budget). Making sub-products
   intern so the budget scales polynomially is the efficiency follow-up (ties into
   RFC-039 broad memo merging). `..._Nway` pins the ≥5-way contract as "correct OR
   loud, never wrong rows".

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

### The mechanism: chained qualified-key merges

The merge `Evaluate` (`value_join_merge*.go`) **preserves already-qualified keys**
(any key containing `.`) and qualifies only bare keys. So when a merge's *outer*
is itself a merged row carrying `A.*, B.*`, those pass through untouched and only
the inner table is freshly qualified. A **chain** of merges therefore accumulates
the full set `A.*, B.*, C.*, …` up the join spine — exactly the `mergeRows`
qualified-key propagation that already carries `b/c/d` today. Pinned directly by
`TestJoinMergeAllValue_ChainsQualifiedKeys`.

The whole bug was that the re-enumerated lower flowed `GetFlowedObjectValue()` (a
single `QOV`), which is *not* a merged row — so the chain broke at the bottom and
the consumed inner table was dropped. The fix flows a merged row from any lower
with ≥2 live tables.

**An N-ary `JoinMergeAllValue` was needed** (not just the binary
`JoinMergeResultValue`, correcting v2's claim that binary suffices). The rule sets
a sub-select's result value *before* it knows how that sub-select re-partitions,
so it cannot name a binary outer/inner there; the value must carry "merge all my
live quantifiers". `JoinMergeAllValue{Aliases []CorrelationIdentifier}` does this
and Evaluates by the same qualified-key passthrough (binary is the 2-alias case).
It is a leaf value with memo hash/equality + `GetCorrelatedToOfValue` support.

### Concrete changes (as implemented)

1. **Liveness, per partition** (`rule_partition_select.go`). The live lower set =
   the lowers referenced by a spanning upper predicate (folded during predicate
   classification) **plus** those the result value needs:
   - the flat seed's translator-built `JoinMergeResultValue` names two arbitrary
     aliases and hides the real projection (it lives in the `Project` above), so
     the rule cannot see what's needed and keeps **all** lower aliases live — this
     applies only at the top;
   - a re-enumerated `JoinMergeAllValue` lists **exactly** the aliases its parent
     needs (`GetCorrelatedToOfValue` returns them) — keep only those (flowing all
     would multiply distinct merge sub-products and blow up the search);
   - any other result value contributes the lowers it actually references.
   This includes keys live only for an ancestor predicate, not just the projection.
2. **Flow rule** keyed on the live-set size:
   - `≤ 1` live → today's single-`QOV` Case-2, **unchanged** (3-way degenerate
     case, no behavior change; the `..._Probe` test stays green);
   - `≥ 2` live → the lower flows `JoinMergeAllValue{live}`; the upper predicates
     and result resolve those columns through the merged row by table-qualified
     name (no translation). When the parent is itself a `JoinMergeAllValue`, the
     upper re-stamps the merge over its **immediate** quantifiers (the original
     deep aliases are collapsed into the lower's merged map).
3. **Stable merge alias.** The merged lower's quantifier alias is derived from its
   (sorted) live set (`$m_<names>`), not a fresh unique id, so identical merged
   sub-joins reached from different bipartitions intern to one memo Reference. The
   live set is also sorted (`dedupAliases`) for determinism + interning. Without
   this the search space explodes (and plans would be non-deterministic).
4. **Drop** `isThreeWay` and the `multiAliasCase3` skip; **keep** `disconnectedLower`
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

- **Wire compat untouched.** Read-path execution only — the merged intermediate
  value lives in-cursor; nothing about record/index/continuation encoding changes.
- **No regression in tested behaviour.** Full cascades + executor + sqldriver +
  plandiff + yamsql green; the 3-way `..._Probe` and plandiff plan-shapes are
  unchanged (≤1-live lowers keep the single-`QOV` Case-2 path, untouched). 2/3-way
  joins — which dominate the corpus — are unaffected.
- **Enumeration is exponential — the known cost frontier.** Full bushy
  re-enumeration over `2^N − 2` bipartitions, with the shared join sub-products not
  yet interning tightly, grows the task count super-linearly: ~57k tasks for a
  4-way (fits the default 100k budget) but ~200k for a 5-way (exceeds it → loud
  fail by default). The stable merge alias (change 3) is what keeps it from being
  far worse. Making sub-products intern so the budget scales polynomially is the
  **efficiency follow-up** (RFC-039 broad memo merging). This is why the default
  budget bounds the practical arity to 4-way, and ≥5-way is "correct OR loud".

## Test plan (as implemented)

`TestFDB_MultiwayJoinOrder_Nway` (replaces the old `_Limit` test):
- **Shapes:** indexed chain (`t1—t2—t3—t4`) and star (`hub→{w,x,y}`).
- **Projection position:** root (`t1.id`), **middle** (`t2.x` — the no-terminal-
  decomposition case), and the buried star hub (`hub.label`).
- **Both FROM-orders** of the chain return correct rows.
- **Correctness:** exact row values + counts (not len-only — the 3-way bug shipped
  green because a len check missed a `[""]` NULL row).
- **Index-probe property:** the 2000-row `t4` is reached via `t4_by_t3`, never
  full-scanned, under both orders. (NOT byte-identity — see "Delivered vs
  deferred": order-invariant cost SELECTION for ≥4-way is the cost follow-up.)
- **≥5-way contract:** correct OR loud plan-failure, never a wrong row.

`TestJoinMergeAllValue_*` (unit): the qualified-key passthrough chains so a buried
table's column survives a nested merge (the core mechanism); nil-when-unbound;
`GetCorrelatedToOfValue` reports the listed aliases (liveness propagation).

`TestLowerAliasesConnected` (existing, retained): the cross-product `disconnectedLower`
skip that still gates at any arity.

## Status progression

Draft (v1) → v2 (addressed Graefe + Torvalds NAK: 7.1 reframed as satisfied
precondition; route (a) rejected; committed to the merge-chained flow; specified
liveness; dropped "~80%") → reviewed (Graefe + Torvalds ACK) → **v3 (IMPLEMENTED,
with two honest scope corrections found in implementation: (i) an N-ary
`JoinMergeAllValue` WAS needed — v2's "binary suffices" was wrong; (ii)
cost-optimal order-invariant SELECTION for ≥4-way and enumeration efficiency for
≥5-way are deferred follow-ups, not delivered. 4-way returns correct rows for all
shapes/orders at the default budget; mechanism verified correct to 6-way.)** →
re-review (Graefe + Torvalds) → PR + @claude LGTM.
