# RFC-150 — Correlated-join winner-selection correctness (B1) + retire the Go-only `tryFlatMapPlan` (Phase 2 of RFC-148)

**Status:** Draft. Split into **Phase 2a (B1a — the nil-inner-Fetch winner-selection guard)** and **Phase 2b
(retire `tryFlatMapPlan` + B1b + B2)** after a root-cause investigation corrected the diagnosis (see §0).

## 0. Root cause of the pre-existing 0-row bug (corrected diagnosis)

The headline bug `SELECT t.k FROM o,t,u WHERE t.k=5 AND t.a>1 AND t.fk=o.id AND u.x=t.x` →
`FlatMap(... inner=Fetch(<nil>))` → 0 rows (on **master** and every prior HEAD) is NOT the "correlated
SUBSEL scan stamped standalone" mechanism originally feared. The consumed leg ref is **non-correlated** to
the outer. The real defect: a **nil-inner `Fetch` SHELL** (the RFC-070 extraction template
`NewRecordQueryFetchFromPartialRecordPlan(nil, …)`, `rule_push_filter_through_fetch.go:101-106`; its real
inner lives in the wrapper quantifier, resolved only via `WithChildren`) is selected as a join child by
**`findBestPhysicalExpr` (`physical_wrapper.go`) — the ONE of three winner-selectors that omits the
`isNilInnerFetch` guard** its siblings `getWinnerForOrdering` and `findBestValidPhysicalExpr` both apply (the
wrapper's own contract doc mandates it). `ImplementNestedLoopJoinRule.OnMatch` (`:92-93`) picks the cheap
nil shell and embeds its plan **directly** (`GetRecordQueryPlan`, never `WithChildren`) → `Fetch(<nil>)`.

**Phase 2a fix (B1a) — minimal, Java-faithful:** select join children through the nil-safe
`findBestValidPhysicalExpr`; delete the unguarded `findBestPhysicalExpr` (2 callers, both the NLJ). Java has
no nil-inner-template concept (a Go RFC-022 plan/wrapper-split artifact); the faithful invariant is "a join
consumes its child through the single nil-safe winner path, never a bespoke pick-cheapest-member path."
plandiff byte-identical (the nil shells were only ever wrongly-selected invalid children) + the pre-existing
bug fixed. Pinned by `TestPlanHarness_JoinLegResidualNoNilFetch`.

**Ordering for the rot-fix (RFC-148's deferred predicate-shape retirement):** B1a FIRST, then retire the
`compensationSafeForYield` shape gate (compound/IN residuals materialize via `yieldUnknown`). Confirmed
empirically: retiring the shape gate WITHOUT B1a re-opens the bug for the OR variant; WITH B1a it plans
correctly.

**Phase 2b (the original scope below)** retires `tryFlatMapPlan` + the `!refIsJoinLeg` muzzle (B1b: a
correlated INDEX scan referencing an unbound outer stamped standalone — a SECOND mode not exercised by the
nil-fetch repro) + B2 (LEFT/FULL OUTER residual placement). Still the deep, PR-#201-class half.

**Item:** TODO.md §7.7 (the join-leg half) — full Java alignment of correlated-join access.
**Reviewers:** **Graefe** (data-access / winner-selection / join-leg consumption — REQUIRED, this is the
0-row surface) + Torvalds + codex + @claude.
**Classification:** query-engine **parity + correctness**. No wire impact, but **plan-shape AND row-level**
sensitive (LEFT/FULL OUTER residual placement changes NULL-extension → wrong rows if mishandled). Per-shape
EXPLAIN + **row-count** proofs + 1M stress mandatory.

---

## 1. Problem — Go has two correlated-join-access paths; Java has one

For `SELECT * FROM o, t WHERE t.fk = o.id AND t.amount > 100`, Go can reach a correlated inner index scan
two structurally disjoint ways (full trace in RFC-148 §2):
- **PATH A — `tryFlatMapPlan`** (`rule_implement_nested_loop_join.go:1009-1289`): a **Go-only** rule that
  manually re-derives a correlated index/PK scan from the flat select's `sel.GetPredicates()` over a **bare
  `RecordQueryScanPlan`** inner (type-assert `:1018`), and owns residual placement (above-FlatMap for INNER
  `:1272-1283`; inner-pushed for LEFT OUTER `:1241-1255`). Java has **no equivalent**.
- **PATH B — data-access compensation** on the `PartitionBinarySelectRule` `SUBSEL` ref: the matcher sargs
  the correlation into a `PartialMatch` bound prefix (Java's only mechanism, `AbstractDataAccessRule`).

Maintaining both is a CLAUDE.md "no parallel pipelines" violation: two producers, double maintenance, and a
latent **pinned-plan-flip / 0-row** surface. RFC-076 v5 + RFC-148 (Phase 1) leave the join-leg coupling
(`!refIsJoinLeg`/`refHasCorrelatedMatch`, `planner.go:490,511,573-582`) in place precisely because removing
it without the safeguards below ships a 0-row plan (the PR-#201 class).

**Critical framing (Graefe).** It is NOT true that "PATH B is muzzled on join legs." `!refIsJoinLeg` blocks
only `implementDataAccessCompensation` — the **residual filter** (`:511`). The **bare correlated scan
wrapper** is `InsertFinal`'d unconditionally (`:499`). So a *no-residual* correlated `SUBSEL` already carries
a physical correlated index scan today and PATH B can already fire for it; only the **residual** case is
muzzled. PATH A/B competition is therefore **partly pre-existing** — this RFC must map the
**no-residual-vs-residual** interaction exhaustively, not assume a clean "turn PATH B on".

## 2. Goal

Retire PATH A (`tryFlatMapPlan` + the EXISTS analogs `tryExistsFlatMap`/`buildExistsFlatMap`), remove the
`!refIsJoinLeg`/`refHasCorrelatedMatch` data-access guard, and let the data-access path (PATH B) be the
single producer of correlated-join access, consumed by the join structure — 1:1 with Java. Plans and rows
identical to today for every pinned shape.

## 3. Design — the three load-bearing invariants (Graefe conditions)

**B1 — a STRUCTURAL no-correlated-standalone-leg-winner invariant, ported from Java's task graph, not a Go
guard. SOLVED — root-caused + empirically proven.**

*The Java mechanism (verified in source).* `OptimizeGroup` is constructed in exactly two places:
`CascadesPlanner.java:584` (the uncorrelated **query root**) and `:1269` inside `OptimizeInputs.execute`, over
a parent expression's `quantifier.getRangesOver()`. `OptimizeInputs` is constructed in exactly **one** place,
`:524`, only for already-implemented **plan** expressions. Therefore a correlated reference's group is
optimized/pruned **only** as the inner child of the binding parent (the FlatMap), with the outer alias live —
never as a free-standing root. Exploration *does* visit the inner independently (so the data-access rule
generates the correlated scan), but the correlation operand stays **symbolic** through planning and is bound
per-row at runtime by the FlatMap. No standalone-unbound state exists → no 0-row, and no muzzle is needed.

*The Go divergence (root cause).* `unified_tasks.go:88` (`ExploreGroupTask.Run`) pushes `OptimizeInputsTask`
for **every exploratory (logical) member** of a ref during PLANNING. `OptimizeInputsTask.Run` then pushes
`OptimizeGroupTask` per child ref (`:389`). So a correlated leg's `OptimizeGroupTask` — which prunes the ref
to one winner with **zero** correlation awareness (`PruneWith`/`SetWinner`/`stampOrderingWinners`/
`findBestValidPhysicalExpr`) — fires whenever ANY logical parent member has the leg as a child, independent of
whether the binding physical FlatMap exists. That is the gap `!refIsJoinLeg` band-aids. (Child *exploration*
is separately driven by `ExploreExprTask` step 4, `:138-142`, so it does not depend on `OptimizeInputsTask`.)

*The fix (1:1 port of Java's `:524`).* Gate `OptimizeInputsTask` construction at `unified_tasks.go:88` to
**physical** members only (`isPhysical(expr)`). Then `OptimizeGroupTask(C)` for a correlated leg is reachable
only via a *physical* parent's `OptimizeInputsTask` — i.e. only as the inner child of the implemented FlatMap,
outer binding live — exactly Java's invariant. This is the structural property, not a `refIsJoinLeg` flag.

*Empirical proof (prototype, this branch).* (a) B1 alone: **plandiff byte-identical** + full cascades +
embedded green — the logical-member-driven `OptimizeInputsTask` was redundant for every existing shape.
(b) B1 + muzzle OFF (`refIsJoinLeg` removed): **plandiff byte-identical** + cascades + embedded + the full
**sqldriver FDB row-count** suite + yamsql + every correlated 0-row sentinel
(`CorrelatedResidualNotStandaloneLeg`, `MultiTableJoinCompoundResidualNotMaterialized`,
`JoinLegResidualNoNilFetch`, `ResidualCompensationPreservesOrdering`) **all green**. The muzzle is proven
redundant once B1 holds. Patch saved as evidence.

**B2 — residual-placement reconciliation is a CORRECTNESS axis, not plan-shape.** When PATH B owns residual
placement (PATH A owns it today), **LEFT/FULL OUTER** placement must be proven with **row-level** tests: a
residual on the wrong side of an outer join changes NULL-extension → **wrong rows**, not merely slower.
Inner-join above-FlatMap vs leg-pushed is plan-shape; outer-join side is correctness. Enumerate INNER /
LEFT OUTER / FULL OUTER × (covering / residual) × (PK-prefix / secondary-index) and pin each.

**B3 — do NOT retire `matchBoundPrefixIsCorrelated`** (`abstract_data_access_rule.go:515`) with
`refHasCorrelatedMatch` — it still gates the RFC-069 intersection exclusion (`planner.go:550`). Only the
**data-access join-leg muzzle** use of the correlated signal is removed; the intersection-exclusion use
stays.

**Plus** RFC-148's `pushDataAccessTasks` re-entry/termination guard (inherited) and the `comp.IsImpossible()`
equivalence audit (prerequisite, shared with RFC-148's index-only arm).

## 4. Method — two pieces (decomposition proven empirically)

**Piece 1 — B1 (the task-graph invariant) + remove the `!refIsJoinLeg` muzzle.** PROVEN safe above (plandiff
byte-identical + FDB row-count + every 0-row sentinel green). Lands first, independently of PATH A: it ports
Java's structural invariant and retires the band-aid, while `tryFlatMapPlan` stays. This is the principial
core — replace the special-case `if X` muzzle with the emergent structural property (design-principle #10).

**Piece 2 — retire `tryFlatMapPlan` (PATH A).** Empirically, dropping PATH A wholesale (with Piece 1 in
place) breaks exactly one dimension: **`TestPlanHarness_LeftJoin`** falls back from a correlated `FlatMap` to a
materialized `NestedLoopJoin(LEFT OUTER)`. Root cause: Go has no `RewriteOuterJoinRule` (Java
`rules/RewriteOuterJoinRule.java`) — PATH A hand-rolls LEFT-OUTER residual placement (inner-pushed,
`rule_implement_nested_loop_join.go:1129-1137`), and PATH B's `yieldGeneralFlatMap` is blocked for LEFT OUTER
by `canSwap := joinType != JoinLeftOuter` (`:179`). **So Piece 2's real work is porting `RewriteOuterJoinRule`:
push the ON-predicates BELOW the null-extension boundary into a correlated null-supplying inner select**, so
PATH B produces the correlated LEFT/FULL OUTER FlatMap with correct NULL-extension — only then can PATH A be
deleted. This is the B2 correctness axis below.

Grind shape-by-shape; for each, route correlated access through PATH B, prove EXPLAIN + row-count identical,
then remove the corresponding PATH A branch. Order (least → most NULL-sensitive):
1. INNER join, secondary-index correlation, no residual (PATH B already fires — prove parity + remove PATH A
   branch).
2. INNER join, PK-prefix correlation, no residual.
3. INNER join + residual (un-muzzle the residual filter on join legs; B1 invariant must be in place first).
4. LEFT OUTER (residual placement = correctness, B2 row-level proofs).
5. FULL OUTER (the materialized-NLJ-only path — confirm PATH B's correlated scan is *never* chosen for FULL,
   which `tryFlatMapPlan` guards today at `rule_implement_nested_loop_join.go` FULL-OUTER guard).
6. EXISTS / lateral subquery-in-FROM (`tryExistsFlatMap`).
Only after all six: delete `tryFlatMapPlan` + analogs and the `!refIsJoinLeg` data-access guard.

## 5. Test plan

- **Per-shape EXPLAIN + row-count**, before/after, for every entry in §4's matrix. Row-count is mandatory on
  outer joins (NULL-extension correctness).
- **Full plandiff byte-identical** across the corpus + **1M stress before/after** (per shape and final).
- **B1 regression**: a correlated leg that, without the invariant, would be stamped standalone → assert it
  is consumed by the join and rows are non-zero (the PR-#201 reproduction).
- **Termination**: inherited from RFC-148 (growth-keyed guard) re-validated with PATH B as sole producer.
- Existing sentinels stay green: `correlated_intersection_guard_test`, `TestFDB_CascadesFlatMapCorrelatedJoin`
  (+ LEFT/LIMIT variants), `zz_join_selpred_repro_test` (RFC-069), `plan_shape_conformance_test`,
  EXISTS/`outer_join_parity` suites.

## 6. Gate & risk

**Graefe ACK on RFC + impl**, per shape if needed. This is the highest-0-row-risk change in the item-5
series: it removes the only guard currently preventing a correlated standalone-leg winner. The mitigation is
the B1 structural invariant (not a test), per-shape row-level proofs, and staged removal. **Never delete a
PATH A branch before its PATH B replacement is EXPLAIN+row-count-proven.**

## 7. Scope

**In:** retire `tryFlatMapPlan` + EXISTS analogs; remove the `!refIsJoinLeg`/`refHasCorrelatedMatch`
data-access muzzle; the B1 structural winner-selection invariant; LEFT/FULL OUTER residual-placement
reconciliation; the no-residual-vs-residual interaction map. **Out:** `matchBoundPrefixIsCorrelated`
(retained for intersection exclusion); `Compensation` construction; anything RFC-148 (Phase 1) owns.
