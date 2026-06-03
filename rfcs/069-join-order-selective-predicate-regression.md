# RFC-069: Cost-model phantom costing breaks join ordering (2-way regression + multiway)

**Status:** Draft
**Severity:** CRITICAL — a perf regression (2-way) plus a multi-way join correctness + perf
regression once the criterion-#2 fix lands.
**Found by:** stress-1M comparison vs the 2026-05-27 baseline (`join_10_outer`), then the
RFC-041/042 multiway acceptance tests during the fix.

## Problem

### Symptom 1 — 2-way selective-predicate join (the original regression)

```sql
SELECT o.id, c.name FROM orders o, customers c WHERE o.customer_id = c.id AND o.id < 10 ORDER BY o.id
```

regressed from ~4 ms (May-27 baseline, 1 M orders / 100 K customers) to effectively infinite
(hung). It returns the correct rows — purely a plan/perf regression. The planner drives off the
full `Scan(CUSTOMERS)` and re-scans/​probes ORDERS per customer (≈10¹¹ reads) instead of driving
off the selective `o.id < 10` PK range (10 rows) and PK-looking-up customers (~20 reads).

Reproducer (EXPLAIN-only, ~4 s): `pkg/relational/sqldriver` `TestFDB_JoinSelPred_Repro`
(both the `id<10` range and `id=5` point forms).

### Symptom 2 — multi-way join ordering (exposed by the criterion-#2 fix)

The RFC-041/042 acceptance tests (`TestFDB_MultiwayJoinOrder_Probe`, `…IndexProbe`,
`…Nway`) regress: a 3-way chain `t1(1) ← t2(20) ← t3(200)` on indexed FKs plans a
full `Scan(T3)` instead of an `IndexScan(t3_by_t2)` probe, and the 4-way form selects a
**semantically broken** re-enumeration that returns **0 rows** (see "Two latent bugs" below).

## Root cause

Two interacting cost-model defects, both proven by instrumentation.

### A. Phantom costing: criteria descend logical Memo References, not the concrete plan

`PlanningCostModel` (Go) evaluates its criteria — operator counts (`findExpressionsByType`),
max-cardinality (#2), unmatched-index-fields (#12), residual predicates (#3), and the recursive
join cost (#15 `compareJoinOrdering` via `BestMemberCostWith`) — by walking a physical wrapper's
**quantifiers**, descending each child `Reference` to its *cheapest* (`bestPhysicalChild`) or
*first* (`firstPhysicalChild`) member.

For a join whose inner ranges over a **shared multi-member group** (e.g. a correlated
sub-Select that has both an `Intersection` and a bare `IndexScan` member), that descent lands
on a **phantom** member — *not* the plan the extracted join actually executes. The catastrophic
`customers`-outer plan's inner is an `Intersection(IndexScan, Scan(ORDERS))` (3 data accesses),
but the cost walk descended to a bare `IndexScan` (2 data accesses) and produced a *negative*
unmatched-field count, so criterion #12 ranked the bad plan ahead of the good one. The same
phantom under-costs bad multi-way orders.

Java's `PlanningCostModel.compare(a,b)` instead evaluates every property
(`FindExpressionVisitor`, `CardinalitiesProperty`, `UnmatchedFieldsCountProperty`) over the
**concrete candidate plan tree** (`RecordQueryPlan`). The candidate's child references are the
singletons belonging to that concrete tree, so there is no shared-group ambiguity. Go must do
the same: cost the concrete `RecordQueryPlan` the wrapper carries (`GetRecordQueryPlan()`), which
is fully formed at construction (built from already-extracted child plans).

### B. Criterion #2 must use provable bounds, not a flat selectivity estimate (Java-aligned)

For the range form, criterion #2 ("max cardinality of all data accesses, lower wins") on HEAD
fed `HintCost(...).Cardinality` — a `FilterSelectivity/RangeSelectivity × N` **estimate** — into
the comparison. With real stats it estimated the `orders` range at `2000×0.33≈660` rows vs the
`customers` full scan at `~100`, so #2 picked **customers-outer (100 < 660)** and the principled
total-cost criterion never ran. Java's `CardinalitiesProperty` has **no selectivity estimate**:
a scan/index access contributes a *known* max cardinality only when its comparisons fully
equality-bind the key (⇒ 1); a range/partial/full bind is `unknownMaxCardinality()`, and the
whole #2 block is then guarded out. Porting that (provable bounds → abstain for ranges) is
correct **but removes the discriminator multiway relied on**: under abstaining #2, the
index-probe vs full-scan choice falls to the later cost criteria — which were the phantom-broken
#12/#15. So fixing #2 (B) without fixing the phantom (A) regresses multiway.

### Latent bugs the cost change exposes

1. **InMemorySort enforcer sorts the first member, not the winner.**
   `ImplementInMemorySortRule` baked `findPhysicalPlan(innerRef)` (the first-yielded join order)
   and pinned the sort's quantifier to it, so the sort always wrapped an arbitrary order even
   when the cost model picked a cheaper one. (Also `physicalInMemorySortWrapper.WithChildren`
   refused to rebuild over a join inner — `isLeafReplaceable` excludes joins — so extraction
   couldn't substitute the winner.)

2. **Invalid 4-way re-enumeration (predicate mis-placement).** The 4-way Nway form has a
   re-enumerated plan `NLJ(FlatMap(T3,T4-probe), NLJ[2 preds](T1,T2))` whose outer NLJ has **no
   join predicate** (a cross product) and whose inner `NLJ(T1,T2)` carries the spanning
   predicate `t3.t2_id = t2.id` — referencing T3, which is not available in the T1⋈T2 branch →
   resolves to null → **0 rows**. HEAD cost-pruned this; the phantom cost selects it. A correct
   concrete cost rejects it (a predicate-less outer NLJ is an expensive cross product), and the
   PartitionSelectRule routing that emits it is independently suspect.

3. **Correlated primary-key intersection → 0 rows.** For a correlated FlatMap inner whose
   predicates match more than one index candidate — e.g. `orders o WHERE c.id = o.customer_id
   AND o.status <> 'cancelled'` (one *correlated* join predicate + one *local* predicate), or the
   3-way spanning case where the inner `c` matches both `c_aid = a.aid` and `c_bid = b.bid`
   (two predicates correlated to *different* outer quantifiers) — `pushDataAccessTasks` offered a
   primary-key index intersection of those legs to the cost model. The new concrete cost ranks the
   intersection cheap (one leg is a 1-row PK probe / both legs are tiny correlated probes) and
   selects it, but Go's intersection cursor evaluates each leg without the per-leg FlatMap
   correlation bound, so the intersection yields **0 rows**. HEAD's phantom cost happened to
   rank the correct correlated-index-scan-plus-residual-filter plan first, hiding this. Java never
   folds a correlated join predicate into an index intersection — index intersections combine
   *local, independently-evaluable* sargable predicates on one record type; a correlated predicate
   is resolved by the FlatMap/NLJ correlation and any remaining local predicate becomes a residual
   filter (exactly what master plans). Fix is a generation guard (item 10), not a cost band-aid.

4. **Redundant outer InMemorySort at scale (`group_by_status`).** `SELECT status, COUNT(*),
   SUM(amount) FROM orders GROUP BY status ORDER BY status` plans, at 1M rows, as
   `InMemorySort([STATUS], StreamingAgg([STATUS], InMemorySort([STATUS], Scan)))` — a redundant
   outer sort over the 4 aggregated groups, where master and the small-scale plan correctly
   eliminate it (`StreamingAgg` already emits in grouping-key order). Root cause: Java's
   `RemoveSortRule` eliminates the sort *structurally*; Go's `ImplementInMemorySortRule` yields the
   sort *unconditionally* and relies on the cost model to discard it. That works until the sorted
   output is tiny relative to the plan's base cost — the eliminated and sort-wrapped plans tie on
   every ordinal criterion (the extra sort is not directly penalised) *and* on the scale-dominated
   scalar cost (a 4-row sort is negligible against a 1M-row StreamingAgg), so the arbitrary
   structural-hash tiebreak picks the redundant sort. Correct results, ~0 perf cost, but a genuine
   plan-quality regression. Fix (item 11): a deterministic "fewer in-memory sorts" tiebreak before
   the hash.

## Proposed solution

Evaluate the cost-model properties over the **concrete `RecordQueryPlan` tree** for physical
expressions, matching Java. Concretely:

1. **`findExpressionsByType(e, stats, ctx)`** — for a `physicalPlanExpression`, walk
   `GetRecordQueryPlan()` recursively (`walkConcretePlan`) computing operator counts, provable
   max-cardinality (scan: all-equality full PK ⇒ 1; index: unique + all-equality ⇒ 1; else
   unknown), and unmatched-index-fields. For a logical expression (no concrete plan), retain the
   memo-descent walk (no extracted plan exists, so the phantom concept does not apply).

2. **`countResidualPredicates`, depth criteria (typeFilter/fetch/distinct), and the deterministic
   hash** dispatch to the concrete plan for physical expressions, so #3 (which gates *before*
   #4) and the tie-breakers are phantom-consistent with #2/#4.

3. **`compareJoinOrdering`** costs the concrete plan tree instead of `BestMemberCostWith`.
   A join's total cost then reflects its real embedded children — the index-probe order beats
   the full-scan order, and a predicate-less cross-product outer NLJ is correctly expensive.
   **Gate + position (refined during impl):** it must fire for any pair whose concrete plan
   CONTAINS a join (FlatMap/NLJ) — not just pairs that ARE a bare join wrapper — because the
   top-of-query join-order alternatives are `Project(join)` / `InMemorySort(join)` members of one
   Reference; gating on the wrapper TYPE missed them. And it runs EARLY (right after the
   data-access-count criterion #4, before the structural fetch/unmatched/map tie-breakers #7–#14):
   the total concrete cost is the principled join-order discriminator (Go's substitute for Java's
   CardinalitiesProperty, which discriminates early), and a "fewer index scans" fetch heuristic
   (#10) must NOT override a large total-cost difference between join orders — that was the
   multiway regression (an index-probe order lost to a full-scan order on fetch count despite
   being far cheaper). For the 2-way the data-access count (#4) still decides first, so the
   2-way is unaffected by the reposition.
   **No formula duplication (Torvalds):** the per-operator cost formula must live in ONE place,
   keyed on operator kind, called by BOTH the physical wrapper's `HintCost` and the concrete-plan
   walk. Do not hand-transliterate the wrapper formulas into a second function — that creates two
   sources of truth that drift (a `FilterSelectivity` tweak in one silently disagrees with the
   wrapper that built the plan). Concretely: extract a shared `joinCostKind(...)`-style helper (or
   have `concretePlanCost` and the wrappers both delegate to a single `properties`-level function
   per operator) so there is exactly one definition of each operator's cost.

   **Criterion #15 is a deliberate Go EXTENSION, not a Java port (Graefe).** Java has no
   recursive-cost criterion: `CardinalitiesProperty` computes max-cardinality *through* joins
   (`visitFlatMapPlan` does `outer.times(inner)`), so its FlatMap-outer tiebreak can order
   multi-way joins, and when all leaves are unbounded it abstains straight to `planHash`. Go has
   not ported the full `CardinalitiesProperty`, so the FlatMap-outer-cardinality tiebreak alone
   cannot order multi-way joins. #15 (recursive concrete cost) is the Go-native substitute that
   compensates for that gap. It is allowed under "read-side reach may exceed Java" and is pinned
   by the tie-only order-invariance test below; it must NOT be described as Java-faithful.

4. **Criterion #2 provable bounds (B)** stays — it is the Java-aligned behaviour and is what lets
   the range form fall through to the principled criteria. Index/PK metadata is resolved via
   `PlanContext` (threaded into the comparator) for the real planning path; the `nil ctx` /
   `nil stats` conservative path (index treated as non-unique, scan PK length 0) must remain
   correct and is exercised by a dedicated unit test.

5. **`unmatchedFieldsForIndex`** counts the index's KEY columns only (NOT + primary key):
   `columnSize − numComparisons` where `columnSize = len(indexKeyColumns)`,
   `numComparisons = equalitySize + (hasInequality?1:0)`, clamped ≥ 0.
   **Rationale (corrected per Graefe — do not misattribute to Java):** Java's
   `getSargableAliases()` *does* include the trimmed PK suffix, so Java's `columnSize` is index
   key + PK. Key-only is nonetheless correct **in Go** because Go's match candidate
   (`plan_context_builder.go`) never folds the PK into `sargableAliases` — it stores
   `pkColumnNames` separately. So counting key columns matches Go's candidate model. (Adding the
   PK here would over-count and penalize a fully-bound index probe vs a full scan — the multiway
   regression.) The real divergence is that Go's candidate omits the PK suffix from sargables;
   note it, don't paper a PK term over it.

6. **InMemorySort enforcer** ranges the sort's quantifier over the inner **group** (`innerRef`)
   so extraction resolves the group's cost **winner**, and `physicalInMemorySortWrapper.
   WithChildren` always rebuilds over its resolved child (a sort imposes no structural constraint
   on what it sorts).

7. **MANDATORY — correctness of newly-selected join plans (both reviewers).** A predicate that
   resolves to null and yields wrong rows is a *correctness* bug, and a correct optimizer must
   never depend on cost to avoid an invalid plan. The cost rewrite SELECTS join plans the
   committed baseline never executed, exposing two latent bugs that must be fixed at the root:
   - **(7a) PartitionSelectRule spanning-predicate routing.** When a multi-way bipartition
     collapses the lower into a merge quantifier, a spanning predicate routed to the upper was
     added verbatim — its correlation set still named the buried lower alias, which the upper
     does not bind. Re-partition then mis-sinks it into a partition that can't resolve it → 0
     rows (4-way). Fix: rebase the predicate's buried-lower references onto the merge quantifier's
     qualified `ALIAS.COL` access so its correlation set names the merge alias (valid + correctly
     re-classifiable). Java's `PartitionSelectRule` only routes a spanning predicate down when its
     upper aliases don't transitively depend on the lower; Go's flat seed has empty
     quantifier-correlations so that guard never fires.
   - **(7b) Deeply-nested FlatMap result-value projection.** A two-level index-probe
     `FlatMap(FlatMap(T1,T2-probe), T3-probe)` mis-resolved an outer-most column (`t1.id`) when
     read through the two-level merged row → wrong values. Fix the merged-row keying / FieldValue
     resolution so a qualified `ALIAS.COL` written by the inner merge resolves at the outer level.
   Both are pre-existing latent bugs (the baseline cost model avoided them by selecting different
   plans); the concrete cross-product costing in (3) is defense-in-depth, not the fix.

8. **Residual-predicate criterion (#3) must count a materialized NLJ's join predicate.**
   `countResidualPredicates` counted only `PredicatesFilter`/`Filter`, not the predicate carried
   by a `RecordQueryNestedLoopJoinPlan`. But an NLJ evaluates its join predicate PER (outer,inner)
   pair — it is not satisfied by a SARG, exactly like a residual filter. Without counting it, a
   materialized `NLJ(Scan(ORDERS), Scan(CUSTOMERS), [join pred])` looked like it had FEWER
   residuals (0) than the correlated `FlatMap(PredicatesFilter(Scan(ORDERS),[amount>50]),
   Scan(CUSTOMERS,[=]))` (1) — which actually SARG'd the join key into a PK probe — so #3
   spuriously preferred the materialized join over the correlated one. Counting NLJ predicates as
   residual (Java has no join-predicate-bearing NLJ, so this is Go-specific) makes #3 prefer the
   SARG'd correlated join, as it should.

9. **Multi-aggregate `MultiIntersectionOnValues` execution under a WHERE-equality prefix.** The
   cost rewrite selects the designed multi-aggregate plan (per-aggregate index streams intersected
   on the group key); a latent bug made it return 0 rows when a grouping column is equality-bound
   by a WHERE filter. Fixed at the executor / comparison-key construction (root cause), with a
   regression test — another previously-unselected plan whose latent bug the cost change exposed.

10. **Exclude correlated legs from primary-key intersection candidates.** In
    `pushDataAccessTasks`, the cross-candidate intersection block filters its match set to
    restricted scans; add `&& !matchBoundPrefixIsCorrelated(m)`. `matchBoundPrefixIsCorrelated`
    inspects the match's bound parameter prefix (`GetBoundParameterPrefixMap`) and returns true if
    any bound comparison's RHS operand is correlated (`Comparison.GetCorrelatedTo()` non-empty) —
    i.e. the scan is a correlated join access, not a local constant-bound predicate. This removes
    the invalid intersection from the memo *at generation* (Graefe: invalid plan never generated,
    not merely cost-rejected), so the correct correlated-index-scan-plus-residual-filter plan is
    the inner. Local multi-index intersections (the TODO 7.2 purpose — constant-bound predicates on
    one type) are unaffected. The cost model is untouched, so the perf win stands. Pinned by
    `TestFDB_QualityProbe_JoinPredicateEdgeCases/join_with_not_equal` and
    `TestFDB_JoinMerge_OuterColumn_NotDropped` (both 0 rows on the un-guarded branch, correct after).

11. **"Fewer in-memory sorts" tiebreak, BEFORE the scalar-cost fallback.** When two plans tie on
    every ordinal criterion (same data access, residuals, joins, fetches, …) and differ only in how
    many `RecordQueryInMemorySortPlan` nodes they carry, prefer fewer (`opsA.inMemorySortCount` vs
    `opsB`). Given identical data access, a sort is pure overhead, so this is safe. It is placed
    *before* the scalar-cost fallback deliberately: that fallback (`EstimateCostWith`) descends the
    Memo by best-member and costs a wrapper's child group at its CHEAPEST member, not the child
    actually embedded — so an `InMemorySort` over a `StreamingAgg` is costed as cheap as the
    aggregate group's cheapest member (a 4-entry aggregate-index scan), and a redundant ORDER BY
    sort over an already-grouping-ordered aggregate would win at scale (`group_by_status`).
    Discriminating on sort count before that phantom-prone fallback restores the sort-eliminated
    plan; it also reinforces the right answer for the aggregate-index-vs-streaming-agg tie (0 vs 1
    sort). Java eliminates these sorts structurally (RemoveSortRule); this is the Go cost-model
    analogue. (The scalar fallback itself is left memo-based: extending the concrete-plan walk to
    cover aggregate/union operators is the principled long-term fix but out of scope here.)

This is a Java-aligned port of the cost *evaluation* (evaluate-on-concrete-plan); it is not a
criterion reorder except for repositioning the join-cost discriminator (item 3). Criterion #15
(recursive concrete join cost) is the one explicit Go extension; items 7–11 close Go-specific
correctness/quality gaps in plans the new cost model now selects.

## Performance

The concrete-plan walk/cost is O(plan-tree-size) per comparison — same order as the memo descent
it replaces, with no per-call memo allocation for the walk. Plan-selection quality strictly
improves (phantom-free). The `join_10_outer` plan returns to a 10-row PK-range driver
(single-digit ms). Validate via the 1M stress comparison.

## Test plan

- `TestFDB_JoinSelPred_Repro` — assert the **positive** plan shape (drive off ORDERS) for BOTH
  the `id<10` range and `id=5` point forms. Red on HEAD-with-Fix-B-only, green after.
- `TestFDB_MultiwayJoinOrder_Probe`, `…IndexProbe`, `…Nway` — index-probe the large tables,
  order-invariant to FROM-clause, and **correct rows** (the 4-way 0-row case).
- Order-invariance-on-ties test (Torvalds): ≥2 orderings that tie at every ordinal criterion and
  diverge only at `compareJoinOrdering`; chosen plan invariant to FROM-permutation. This also
  pins criterion #15 (the Go-extension recursive cost).
- **Memo-level "invalid plan never generated" test (Torvalds/Graefe #4):** assert the
  cross-branch-predicate enumeration (a spanning predicate routed into a partition that does not
  bind its correlations) is NOT a member of the memo — not merely "not selected". This is the
  correctness guard that does not depend on cost.
- **`nil ctx` / `nil stats` conservative-path unit test:** exercise `indexMetadata` (non-unique
  fallback), `scanPlanProvableMaxCard` (pkLen=0), and the comparator on the no-context path.
- New cost-model unit tests rewritten for concrete-plan evaluation (the synthetic
  stub-plan-child tests no longer model real wrappers).
- `plandiff` cross-engine conformance — diff **plan shapes**, not just row counts (a shared-group
  walk change can re-rank ties anywhere).
- Full `just test`; planner determinism 10× on the affected tests.
- stress-1M: `join_10_outer` back to single-digit ms; full table vs the 2026-05-27 baseline shows
  no regression elsewhere.

## Reviewers

Per the query-engine mandate: **Graefe** (Cascades alignment — must ACK), **Torvalds** (code
quality), then **codex** and **@claude** on the implementation.
