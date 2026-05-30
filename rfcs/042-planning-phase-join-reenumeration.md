# RFC-042: FROM-order-independent multi-way join ordering

**Status:** v12 — **IMPLEMENTED for 3-way joins. The acceptance probe
`TestFDB_MultiwayJoinOrder_Probe` is GREEN.** A 3-way chain join planned under
two opposite FROM-orders yields BYTE-IDENTICAL, cost-optimal physical plans that
drive from the 1-row t1 and index-probe the 200-row t3 via `t3_by_t2`, returning
the correct 200 rows for both orders. Full repo green; deterministic 8×.

**Scope: 3-way only. ≥4-way joins are explicitly NOT supported** and fail to plan
LOUDLY (`could not plan query`) — exactly as on master (4-way has never planned
in this port). The re-enumeration routes a spanning join predicate to the upper
and flows the correlated lower quantifier up as a single QOV; that correlation
threads cleanly through exactly ONE level of upper re-partitioning (3-way), but
for ≥4-way it must survive TWO+ nested re-partitions and the executor's flattened
NLJ/FlatMap merge loses the lower alias → the projection resolves to NULL.
`PartitionSelectRule` therefore gates the new classification on `n == 3` and
keeps Java's original split for `n > 3`. A loud plan-failure is acceptable; a
silent wrong row is not. Completing the executor's **nested
`RecordConstructorValue` + `TranslationMap` resolution** is the N-way follow-up
(Graefe-endorsed contract). The boundary is pinned by
`TestFDB_MultiwayJoinOrder_Limit`, which asserts the 3-way star is correct AND
the 4-way either returns the correct `alpha` row or fails loudly — never a silent
NULL row.

It took FIVE landed fixes, none of which is the broad TODO 7.1:

1. **L1 — removed the Go-only `PushProjectionBelowJoinRule`** (recursive-CTE gap
   closed at translation time). Unblocks the flat seed. (`1059aed8`)
2. **NLJ hash-join qualification (`d420567b`)** — `fieldName` now qualifies a
   QOV-child `FieldValue` so the ≥100-row hash-join path extracts outer/inner
   columns correctly (was backwards → 0 rows at scale). Pinned by
   `equijoin_columns_test.go`.
3. **Spanning-predicate classification (`bc0cb131`)** — for a 3-way join, route a
   join predicate that references both partition halves to the UPPER
   (correlating), so PartitionSelect generates the re-enumerated index-probing
   `(t1⋈t2)⋈t3` associativity for EVERY FROM-order (the L3.0 cost-vector proof
   showed it was generated 0× for big-first before this).
4. **Disconnected cross-product skip (`bc0cb131`)** — skip a partition whose
   lower has ≥2 quantifiers no lower predicate connects (its multi-alias
   `RecordConstructorValue` result doesn't resolve at execution); the connected
   associativities cover the same join orders correctly.
5. **3-way scope gate (this commit)** — gate the new classification + skips on
   `n == 3`; keep Java's original split for `n > 3` so ≥4-way joins fail to plan
   loudly (as on master) rather than returning silent NULL rows. Pinned by
   `TestFDB_MultiwayJoinOrder_Limit`. (Addresses Torvalds' v11 NAK: the prior
   `multiAliasCase3` skip left a 4-way Case-2 hole that returned a `[""]` row.)

The history below records the multi-version root-causing (v4–v10), including the
disproven theories (task-engine ordering; cost mis-accounting; TODO 7.1
alias-namespace), since each was instrumented, not guessed.

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

### L3 implementation plan (v6 — revised per Graefe + Torvalds NAK of v5)

Both reviewers NAK'd the v5 plan and **converged**: the attack order was inverted
and the prime suspect (L3.1 "flat result value") mis-roots the bug. v5's L3.1 is
**dropped** as the lead step:
- Graefe: the 0-row reroute did **not** prove the sub-product result is too
  narrow. Java's `PartitionSelectRule` seeds `lowersCorrelatedToByUppers` from
  `resultCorrelatedToLowers` **and every upper-predicate's `correlatedToLowerAliases`**.
  If `t2.t1_id` isn't exposed, the spanning predicate simply isn't in
  `upperPredicates`, so its `t2` correlation never enters the set — that's a
  **predicate-classification** issue, not a result-value-shape defect. Forking the
  sub-product to a flat `JoinMergeResultValue` would desync Java's
  `RecordConstructorValue`+`TranslationMap` contract and re-introduce the exact
  qualified-key collision class L1's recursive-CTE fix just escaped. **Don't.**
- Torvalds: "fix the cost model if it's mis-ranked" is unfalsifiable until proven;
  changing the cost model on a hypothesis is a multi-hour rabbit hole.

**Revised steps, strictly ordered:**

* **L3.0 (PROOF, no code change) — DONE. Result: GENERATION, not cost.**
  Instrumented `planningCostModelCompareWith` to count, per FROM-order, how many
  times a 3-way top plan that index-probes T3 is costed against an alternative:
  ```
  FROM t1,t2,t3 : T3-index-probe costed 111×  (and #2 maxCard 90 < 180 → it WINS)
  FROM t3,t2,t1 : T3-index-probe costed   0×  (never generated, never costed)
  ```
  The cost model is **correct** — when both plans exist (small-first) criterion
  #2 (max data-access cardinality, 90 vs 180) strictly prefers the index-probe.
  Big-first **never generates** the index-probe associativity, so there is
  nothing to cost. **L3.2 (cost accounting) is therefore NOT the bug and is
  dropped** — no cost-model edit (Torvalds hard rule satisfied: the deciding
  factor was proven before any change, and it is "plan absent", not "plan
  mis-costed"). The L3.0 run also surfaced that the **L2 degenerate-partition
  rejection rejects *every* chain bipartition**, so PartitionSelect emits no
  re-enumerated associativity and each FROM-order falls back to its native
  nested FROM shape (small-first's native `(t1⋈t2)⋈t3` happens to index-probe T3;
  big-first's native `(t3⋈t2)⋈t1` does not). Re-enumeration is effectively off
  for chains — the proper fix supersedes the rejection (see L3.1).
* **L3.2 (cross-product NLJ cardinality) — DROPPED.** L3.0 proved the cost model
  is not the bug (the index-probe isn't generated, not mis-costed). No cost-model
  edit. Kept here only to record that the v5 prime suspect was disproven by L3.0.
* **L3.1 (THE fix, per L3.0) — make re-enumeration generate the index-probe
  associativity for every FROM-order, NOT result-value shape.** Two parts:
  - **(a) Classification + supersede the L2 rejection.** A spanning join predicate
    (references both partition halves) must route to the **upper** (folding its
    `correlatedToLowerAliases` into `lowersCorrelatedToByUppers`, as Java's
    "must do in upper" branch does), making the partition a **valid correlated
    join** instead of the degenerate lower-cross-product the L2 guard rejects.
    With spanning predicates routed up, the L2 `degenerate` rejection no longer
    triggers and is removed (it was a correctness band-aid that disabled
    re-enumeration). The `{T1,T2}|{T3}` partition is then generated for both
    orders: lower flows the columns the upper needs via Case-2/3
    (`RecordConstructorValue`+`TranslationMap`, Java contract preserved), upper
    holds `t3.t2_id = lowerQ._i.id` correlating T3 to the lower's t2.
  - **(b) Index match for the constructor-field correlated probe.** The
    re-enumerated upper's join predicate is `t3.t2_id = FieldValue(QOV(lowerQ), _i).id`
    (the lower flows a `RecordConstructorValue`). The index matcher currently
    SARGs `t3.t2_id = T2.id` (a plain qualified field — small-first's native
    JoinMerge shape) but not the constructor-field form, so the re-enumerated
    associativity full-scans T3. Teach the correlated-probe matcher to accept the
    constructor-field correlation so the re-enumerated `(T1⋈T2)⋈T3` index-probes
    T3 — then it is generated, costed, and (per L3.0) strictly wins #2 for both
    orders.
  - **Gates (Torvalds):** every existing join **row-correctness** assertion
    unchanged after (a); a unit test that the re-enumerated `(T1⋈T2)⋈T3` member
    is generated with an `IndexScan(T3_BY_T2)` inner after (b); both FROM-orders
    return the right 200 rows throughout. Any NULL/0-row regression → revert.

* **L3.1(a) — the 0-rows root cause was NOT TODO 7.1; it was a specific NLJ
  hash-join bug, now FIXED.** The classification change (route spanning
  predicates to the upper + fold the lower alias) produces, for **both**
  FROM-orders, the byte-identical index-probing plan
  `Project(T1.ID, NLJ([p2], Scan(T1), FlatMap(Scan(T2), IndexScan(T3_BY_T2,[=]))))`.
  It returned 0 rows **only at large cardinality** (≥100 inner rows) and the
  correct rows at small cardinality — a data-dependence that disproved the
  "alias resolution always fails" (TODO 7.1) theory. Instrumentation traced it
  precisely: the re-enumerated predicate `p2` is `FieldValue{Field:"T1_ID",
  Child: QOV("T2")} = FieldValue{Field:"ID", Child: QOV("T1")}`; the NLJ's
  hash-join column extractor (`extractEquijoinColumns`/`fieldName`) returned only
  the **bare** Field for a QOV-child FieldValue, so `splitQualified` saw an empty
  qualifier, `matchesAlias("", x)` was always true, and it picked outer/inner
  **backwards** — keying the hash index on the wrong column → 0 rows. (The hash
  path only activates at ≥100 rows, hence the data-dependence; small data used
  the linear path and matched correctly.) **Fixed (commit `d420567b`):**
  `fieldName` now returns the qualified `ALIAS.COL` from the QOV child's
  correlation. Pinned by `equijoin_columns_test.go` (fails without the fix:
  `outerCol="T1_ID"` backwards). Full suite green. **This is the real executor
  blocker — TODO 7.1 is NOT required for byte-invariance.**

  Remaining for the probe: the classification change itself (route spanning to
  upper) — with the `fieldName` fix in place it yields byte-identical correct
  rows for the **indexed** t1/t2/t3 chain (the index-probe associativity wins on
  cost), but applied unconditionally it regresses **non-indexed** 3-way shapes
  (the A→B→C chain `TestFDB_CascadesThreeWayJoin`, the Customer/Region/Sales star
  `TestFDB_ThreeTableFrom` → 0 rows). Instrumented why: the change makes
  PartitionSelect also generate the **cross-product** partition (e.g. `{A,C}|{B}`
  for the chain, A and C unrelated). With both spanning predicates folded, that
  lower is a Case-3 `RecordConstructorValue{_0:A, _1:C}`, and the upper predicates
  are translated to `FieldValue(QOV(lowerQ), _i).col`. At execution the top NLJ's
  `mergeRows` flattens the `{A,C}` sub-product row under the lower quantifier
  alias, producing over-qualified keys (`A.B_REF`, `q$N.A_ID`, …) but **not** the
  nested `q$N._0.A_ID` the translated predicate looks for → the predicate
  evaluates to `<nil>` → 0 rows. For the indexed chain this broken cross-product
  plan loses on cost to the index-probe, so it never surfaces; without an index
  it wins and the 0 rows show.

  **So byte-identical N≥3 ordering needs THREE executor/planner fixes, NOT TODO
  7.1:** (1) the `fieldName` hash-join qualification — **DONE** (`d420567b`);
  (2) the spanning-predicate classification change (generates the re-enumerated
  associativities); (3) **Case-3 `RecordConstructorValue` predicate resolution** —
  the executor must resolve a translated `FieldValue(QOV(lowerQ), _i).col` against
  the merged row (navigate the `_i` constructor field), and/or `mergeRows` must
  expose the sub-product columns under the names the translated predicate uses.
  (2)+(3) are coupled — (2) is safe only once (3) makes the Case-3 plans it
  generates execute correctly. (3) is the genuine remaining executor work; it is
  Java's `RecordConstructorValue`+`TranslationMap` contract (Graefe: preserve it),
  so the fix is in the executor's nested-reference resolution, not the plan shape.
* **L3.3 (STOP signal, not a step) — winner-selection ties.** If, after L3.2, the
  index-probe plan only wins via a tie-break, that means it is **not strictly
  cheaper** → L3.2 is incomplete; STOP and re-measure, do not paper over with an
  alias-order-independent tie-break. Byte-identity must emerge from FROM-order-blind
  enumeration + a total, alias-independent cost order. If a genuine structural tie
  remains, break it on **plan structure**, never quantifier-insertion (`q$N`) order.
* **L3.4 — restore the probe + cross-group reach.** Re-add
  `TestFDB_MultiwayJoinOrder_Probe` (byte-identical + drives-from-t1 +
  index-probes-T3 + **row-correctness** for both orders). If sub-products remain
  in disjoint References, finish the RFC-037 cross-group merge reach (PR-C/PR-D,
  tasks #10/#11). Correctly last.

**Hard STOP / revert criteria (Torvalds):**
1. Any test that returned data now returns NULL/0 rows → **revert immediately**,
   do not debug forward.
2. Reaching for a tie-break to achieve byte-identity → L3.2 isn't done; STOP.
3. Any cost-model edit without the L3.0 per-criterion vector proving the deciding
   criterion → not allowed.

**Java-parity stance (CLAUDE.md):** byte-identical-across-FROM-order for N≥3 is
the stated `/goal` and the natural consequence of complete, FROM-order-blind
cost-based enumeration (which Java's Cascades does). The cost-accounting fix
(L3.2) is pure parity (Java costs a cross product as the product). Where a step
would *exceed* Java it is flagged as a read-side extension (deep tests, wire
compat holds). No step changes anything on the wire.

## Test plan

`TestFDB_MultiwayJoinOrder_Probe` (indexed join columns): (a) order-invariance —
both FROM-orders byte-identical EXPLAIN; (b) cost-optimal — drives from the 1-row
table, T3 reached last via its index, never full-scanned; (c) **row-correctness**
— both orders return the right 200 rows (the dimension that the prior fake-green
test missed). All three GREEN.

`TestFDB_MultiwayJoinOrder_Limit` pins the 3-way/4-way boundary honestly: the
3-way star join returns the correct projected value, and the 4-way chain (both
FROM-orders) either returns the correct `alpha` row OR fails to plan loudly —
never a silent NULL row. This is the regression sentinel against the v11 hole
(4-way planned but `a.val` resolved to a `[""]` NULL row). If a future change
makes 4-way plan, this test forces it to also return correct rows.

Plus the no-regression gate (REWRITING-rule count; plandiff conformance;
determinism 8×) and the recursive-CTE row gate.

## Status progression

Draft v1–v3 (wrong root cause, retracted) → v4 (correct multi-layer root cause,
none landed) → v5/v6/v7 → v8 (L3.0 PROVED generation-not-cost; L3.1(a) attempted —
byte-identical index-probe achieved for both orders but 0 rows; blocker isolated
to TODO 7.1 alias-namespace unification; reviewer-ACK'd L3 plan) → v11 (probe
GREEN; four landed fixes) → **v12 (Torvalds NAK addressed — 3-way scope gate;
4-way fails loudly as on master, not silent NULL rows; pinned by
`TestFDB_MultiwayJoinOrder_Limit`). IMPLEMENTED for 3-way; N-way executor
resolution is the documented follow-up.**
