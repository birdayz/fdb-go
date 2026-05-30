# RFC-042: FROM-order-independent multi-way join ordering

**Status:** v5 — L1 fully landed (Go-only rule removed entirely, recursive-CTE
gap closed at translation time) and a **correctness** bug in re-enumerated
multi-way joins fixed (degenerate partitions returned wrong/NULL rows, not merely
suboptimal plans). The acceptance probe `TestFDB_MultiwayJoinOrder_Probe`
(byte-identical N≥3 plans) is still RED on **cost-optimality** (L2/L3); both
FROM-orders are now *correct* but not *identical*. This version records what
landed, why the v4 "Layer 2 = residual-penalty" framing under-described the bug
(it was wrong rows, not just cost), and the concrete L2/L3 implementation plan.

### v5 changelog (this session)
- **L1 — DONE (superseding v4's "PLANNING-only, not landed").** `PushProjectionBelowJoinRule`
  is **deleted** (not just moved to PLANNING). Its only load-bearing use —
  recursive-CTE temp-table column alignment — is now handled at translation time.
  Graefe+Torvalds ACK. (commit `1059aed8`)
- **Correctness fix — `PartitionSelectRule` rejects the degenerate partition.**
  Re-enumerated indexed 3-way joins returned FROM-order-dependent **wrong rows**
  (one order 200 rows all-NULL, the other 0 rows; correct is 200 rows). Root: a
  spanning join predicate routed into the lower partition where its upper alias is
  unbound → degenerate Case-1 `{_0}` cross-product. Now rejected. Graefe+Torvalds
  ACK. (commit `f99af166`)
- **Fake-green killed.** `multiway_join_index_probe_test.go` asserted plan shape
  only and never executed the query — it stayed green while returning wrong rows.
  Retrofitted with row-correctness assertions for both FROM-orders.

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

### Layer 1 (FIXED) — REWRITING did not produce a FROM-order-independent flat seed

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

**Why a naive removal regressed recursive CTE — and how it's now closed.**
Dropping the rule flattens the seed but `TestFDB_RecursiveCTECrossJoin` returned
4 rows instead of 5 (missing the deepest descendant 250). Instrumenting the
temp-table inserts showed why: the recursive body
`SELECT b.id, b.parent FROM descendants a, t b WHERE b.parent = a.id` is a join
whose output is the **merged** `JoinMergeResultValue` row carrying QUALIFIED keys
(`B.ID`, `B.PARENT`, `A.ID`). Inserted verbatim, the stale `B.ID` collides with
the **next** recursion level's own `b` join side, clobbering the live row and
stalling the recursion one level early. The push-down was a workaround: it
narrowed the join's children so the merged row only had the schema columns.

**Fix (landed, commit `1059aed8`):** delete the rule and close the gap at
translation time. The recursive leg's normalization projection (already built in
`cascades_translator.go` to map the body's output names to the seed schema) now
reads each column via `FieldValue{Field: <bare col>, Child: QOV(<qualifier>)}`:
`evaluateCorrelated` reads the qualified datum key (`B.ID`) while
`projectionColumnName` returns the **bare** field — so the temp-table row carries
only the clean seed-schema columns and the qualified key that caused the
collision is never emitted. Recursion now reaches 250: `[10 40 50 70 250]`;
UNION-DISTINCT cycle detection passes. Verified: 46 test targets green,
recursive-CTE deterministic 5×, `−404` net lines (rule + its unit test deleted).
This is the genuine L1 closure (v4 left the rule alive in PLANNING).

### Index-nested-loop join — LANDED for 2-table joins (RFC-042 L3, committed)

Implemented and committed: a join's inner now uses a secondary index. Two fixes:
- `MatchIntermediateRule` generalized `matchFilterAgainstSelect` →
  `matchSingleSourceAgainstSelect` and wired a pass-through single-source
  `SelectExpression` (the absorbed inner of a join) to it, so the correlated
  join predicate SARGs the index (porting the missing slice of Java's
  `SelectExpression.subsumedBy`).
- Moved index-candidate matching (`MatchLeafRule`/`MatchIntermediateRule`) to
  PLANNING-only (`PlanningExplorationRules`), matching Java's PlanningRuleSet —
  the absorbed inner is a PLANNING artifact, so REWRITING-only matching never
  saw it; this also fixed a duplicate index-scan-in-Intersection artifact.

Verified: a 2-table join on an indexed column plans
`FlatMap(outer=Scan(outer), inner=Intersection(Fetch(IndexScan(idx,[=])), …))`.
Single-table index selection unchanged; full `just test` green.

**Remaining: 3-way re-enumerated joins.** The inner T3 select index-matches
(verified: 3× against `T3_BY_T2`), but the top join reference has NO index-probe
member — every `(T1⋈T2)⋈T3` associativity uses a full `Scan(T3)`. The index-probe
form needs the `(T1⋈T2)` outer to flow `t2` so the correlated T3 inner can probe
`t3.t2_id = t2.id` via the index. PartitionSelect's Case-3 flows the sub-join
result as a `RecordConstructorValue{_0: t1, _1: t2}` (it must carry both the
projected `t1.id` and the join key `t2.id`), and the absorbed-T3-FlatMap
associativity over that constructor is not generated/selected as a top member.
Next: ensure the re-enumerated `(lower)⋈T3` builds the absorbed-inner FlatMap
whose inner SARGs `T3_BY_T2` via the constructor's `_1` (t2) field — i.e. the
index matcher must accept a `FieldValue(RecordConstructor-QOV, _1)` correlated
probe value, and PartitionBinarySelect must absorb the top join's T3 predicate
even when its correlated alias is deep inside the outer sub-join.

### (historical) Layers 2 & 3 collapse into ONE capability: index-nested-loop join

Instrumentation (post-L1) shows L2 and L3 are the same gap viewed two ways. The
absorbed correlated-inner form that L2 flagged as "residual-penalized" is exactly
the form an **index probe** consumes — `PartitionBinarySelectRule` deliberately
pushes the join predicate into a correlated inner sub-Select precisely so the
inner can be index-SARGed. So the real fix is to make that correlated inner an
**index probe** (then the SARGed predicate is not a residual, the cost ties
collapse to join-order cost, and the optimal drive-from-smallest order wins for
both FROM-orders). Three concrete missing pieces, all in the match layer:

1. **The correlated join predicate is not pushed into the inner *reference*.**
   `ImplementNestedLoopJoinRule.yieldGeneralFlatMap` applies the predicate as a
   post-scan `PredicatesFilter` in the FlatMap wrapper, leaving the inner
   reference a bare `Scan(T3)` — so there is no correlated `Select([t3.t2_id =
   t2.id], Scan(T3))` reference for the index matcher to bind. Verified: even
   `TestPlanHarness_JoinOnIndex` plans `FlatMap(outer=Scan(CUSTOMERS),
   inner=PredicatesFilter(Scan(ORDERS),[1 preds]))` — never an index probe.
2. **No Select-vs-Select match path.** `MatchIntermediateRule` only matches a
   query `LogicalFilterExpression` against a candidate `SelectExpression`
   (`matchFilterAgainstSelect`, rule_match_intermediate.go:205). The Go port
   narrowed Java's general `SelectExpression.subsumedBy` to the Filter-vs-Select
   case (single-table `WHERE` stays a LogicalFilter, so it matches; a join inner
   is a `SelectExpression`, so it never matches). Instrumentation confirmed: for
   the indexed join only `queryExpr=SelectExpression(nq=2)` (the join select)
   reaches the matcher — the inner nq=1 Select is never attempted against the
   index candidate. **Port the Select-vs-Select subsumption path** (analogous to
   matchFilterAgainstSelect, for a 1-quantifier query SelectExpression).
3. **Correlated index-scan generation + cost.** The matched correlated comparison
   (value = a `QuantifiedObjectValue` from the outer) must produce a correlated
   `IndexScan(idx, [=outer.col])` data-access member (the per-outer-row probe),
   and the cost model must let it win over the full-scan FlatMap.

The placeholder-binding machinery itself (`matchFilterAgainstSelect` →
`SetSargable`, rule_match_intermediate.go:385-398) already binds a comparison
into a `ComparisonRange` structurally and does NOT reject correlated comparison
values — so piece 3's matching half is largely present once pieces 1+2 feed it.

This is a real feature (index-nested-loop join), absent in the Go port, spanning
predicate push-down + match infrastructure + data-access generation. It is the
realistic way this Cascades architecture does cost-based multi-way join ordering.

### Layer 2 (CORRECTNESS — FIXED) — degenerate partition returned wrong rows

The v4 framing below ("residual-predicate penalty") under-described the bug: the
Case-3 path did not merely produce a *costlier* plan, it produced a **wrong**
one. Pointer-level instrumentation (`computeResult` / `JoinMergeResultValue.Evaluate`):
for the chain, `PartitionSelectRule` routes the **spanning** join predicate
(e.g. `t2.t1_id = t1.id`, one alias in each partition half) into the **lower**
partition. Java keys this on `uppersDependingOnLowersAliases` (from the
**quantifier** correlation order); Go's flat-seed join quantifiers are
independent scans with no quantifier-level correlations, so that set is always
empty and the spanning predicate always falls to the "can do in lower" branch.
The lower then can't evaluate the predicate (its upper alias is unbound) and
becomes a degenerate **Case-1 cross-product** whose result is a `{_0}` literal
placeholder — discarding the real columns — and whose pushed-down filter
evaluates against unbound aliases. Result: `SELECT t1.id` returned 200 rows all
NULL under one FROM-order and 0 rows under the other (correct is 200 rows, all
`t1.id=1`). 2-table and non-indexed *star* 3-way joins were always correct;
only the indexed-chain re-enumeration was broken.

**Fix (landed, commit `f99af166`):** `PartitionSelectRule` rejects the degenerate
partition — after predicate classification, if any predicate routed to the lower
references an upper alias, `continue` (skip this bipartition). The valid
associativities (where the spanning predicate stays at the join level) then win
**identically for every FROM-order**. Graefe: "prunes only invalid plans — no
valid join order lost, full powerset still enumerated." Both FROM-orders now
return correct rows; the probe's remaining redness is purely cost-optimality
(L3), not correctness.

### Layer 2 (historical framing) — re-enumerated associativities carry a residual-predicate penalty

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

* **L1 (flat seed): DONE** — Go-only rule removed, recursive-CTE gap closed.
* **L2 (correctness of re-enumeration): DONE** — degenerate partition rejected.
* **L3 (cost-optimal byte-invariance): the remaining work** — see plan below.

### Current observed gap (both FROM-orders correct, not identical)

```
t1,t2,t3 → Project(T1.ID, PredicatesFilter(FlatMap(NLJ(Scan T1, Scan T2),
                                                   inner=IndexScan(T3_BY_T2,[=])), [p2]))   # index-probes T3 ✓
t3,t2,t1 → Project(T1.ID, NLJ([p2], Scan T1, NLJ(Scan T2, Scan T3)))                        # full-scans T3 ✗
```

Both return 200 correct rows. small-first reaches the optimal left-deep
`(T1⋈T2)⋈T3` shape (T3 index-probed); big-first wins a right-deep
`T1⋈(T2⋈T3)` whose inner `(T2⋈T3)` is a cross-product NLJ that full-scans the
200-row T3.

### L3 implementation plan (this is what v5 commits to)

Instrumentation this session established three facts that scope the work:
1. The index-probe for T3 **is generated** (the secondary-index path in
   `ImplementNestedLoopJoinRule` is reached with `innerTable=T3, npreds=1`).
2. The cost-visible inner of the index-probe FlatMap already ranges over the
   index-scan wrapper (criterion #2 `maxDataAccessCardinality` reflects the
   probe) — so #2 *should* favor the index-probe (max card 20 vs 200).
3. Yet big-first's winner is the cross-product associativity — so either the
   left-deep index-probe associativity is not winning under big-first's
   quantifier order, or the `(T2⋈T3)` index-probe sub-product can't flow the
   columns the parent predicate needs (a reroute experiment made it return 0
   rows), so it's never a *valid* cheaper member.

Steps, in order, each gated by the full suite + the row-correctness probe:

* **L3.1 — flat sub-product result value.** Make a re-enumerated join
  sub-product flow a flat `JoinMergeResultValue` (qualified columns of both
  sides) instead of the Case-2 `QOV(lowerAlias)` / Case-3 `{_0}` constructor that
  drops the columns the *enclosing* join predicate needs. This is the root of
  fact (3): the `(T2⋈T3)` index-probe must still expose `t2.t1_id` for the
  parent `p2`. With a flat result, the index-probe sub-product is both correct
  *and* a valid cheaper member the cost model can pick. (Touches
  `PartitionSelectRule` Case-2/3 result-value construction + the physical
  FlatMap/`tryFlatMapPlan` result value. Highest regression risk — the 2-table,
  star-join, and EXISTS/correlated paths rely on the present result-value flow,
  so validate against the full suite and revert on any regression.)
* **L3.2 — verify cost ranking across associativities.** With L3.1, confirm the
  left-deep index-probe plan is generated as a root member for *both* FROM-orders
  and that `PlanningCostModel` ranks it below the cross-product NLJ associativity.
  If a criterion before #2 (or the recursive join-order cost #14) mis-ranks the
  cross-product NLJ (whose true cardinality is the product 20×200, not
  max=200), fix the cardinality/cost accounting for the cross-product NLJ.
* **L3.3 — FROM-order-deterministic winner selection.** Once both orders generate
  the same optimal member, ensure tie-breaks in winner selection are independent
  of quantifier insertion order (quantifier aliases `q$N` are minted in
  FROM-order). Without this, two equal-cost members can be selected differently
  per order, breaking byte-identity even when the optimal plan exists for both.
* **L3.4 — restore the probe + cross-group reach (original `/goal`).** Re-add
  `TestFDB_MultiwayJoinOrder_Probe` (byte-identical + drives-from-t1 +
  index-probes-T3, **and a row-correctness assertion**). If L3.1–L3.3 leave the
  `(T1⋈T2)` sub-products in disjoint References (pointer-level proven earlier),
  finish the RFC-037 cross-group merge reach (PR-C/PR-D, tasks #10/#11) so shared
  sub-products intern into one Reference for full N-way optimality.

**Java-parity stance (CLAUDE.md):** byte-identical-across-FROM-order for N≥3 is
the stated `/goal`; it is the natural consequence of complete, FROM-order-blind
cost-based enumeration (which Java's Cascades does). Where a step would *exceed*
Java (e.g. an index-NLJ shape Java's planner doesn't emit on this exact query),
it is an allowed read-side extension provided wire compat holds and it carries
deep tests — flagged at the step. No step changes anything on the wire.

## Test plan

`TestFDB_MultiwayJoinOrder_Probe` (indexed join columns): (a) order-invariance —
both FROM-orders byte-identical EXPLAIN; (b) cost-optimal — drives from the 1-row
table, T3 reached last via its index, never full-scanned; (c) **row-correctness**
— both orders return the right 200 rows (the dimension that the prior fake-green
test missed). Currently RED on (a)/(b) (L3), GREEN on (c) after the Layer-2 fix.
Plus the no-regression gate (43 REWRITING-rule count; plandiff conformance;
stress-1M before/after; determinism 10×) and the recursive-CTE row gate.

## Status progression

Draft v1–v3 (wrong root cause, retracted) → v4 (correct multi-layer root cause,
none landed) → **v5 (L1 removed + L2 correctness fixed, both landed and
reviewer-ACK'd; L3 cost-optimal byte-invariance scoped with a concrete 4-step
plan)** → Implemented when the probe is green under the full gate.
