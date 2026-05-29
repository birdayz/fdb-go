# RFC-042: PLANNING-phase join re-enumeration (the task-engine half of multi-way join ordering)

**Status:** Draft — seeking Graefe + Torvalds sign-off on DIRECTION before implementing.

**Epic:** RFC-038 PR-C/PR-D, continuing RFC-041. RFC-041 fixed the cost *model* (it now
ranks join orders correctly by recursive, stats-aware total cost). This RFC fixes the
planner *task engine* so the optimal join order is actually a candidate the cost model
gets to choose — the last blocker for "multi-way join ordering proven".

## Problem (precisely instrumented in RFC-041 §Implementation findings)

The acceptance probe (`TestFDB_MultiwayJoinOrder_Probe`, 3-table chain t1=1/t2=20/t3=200)
plans the SAME join under two FROM-orders and they do NOT converge — big-first gets the
costlier `T1⋈(T2⋈T3)` (1077), small-first the optimal `(T1⋈T2)⋈T3` (769). The cost model
*would* pick 769, but for big-first that associativity is never offered as a candidate.

Root cause — the Go planner has **two overlapping rule-driving task systems**:

* **System A** — `ExploreReferenceTask` → `TransformReferenceTask` (planner.go:541) fires
  `p.rules` (the main set = `DefaultExpressionRules` + `RewritingRules`). This is the ONLY
  path that ever fires `PartitionSelectRule` (instrumented: **22 fires**).
* **System B** — `ExploreGroupTask` → `ExploreExprTask` → `TransformExprTask`
  (unified_tasks.go) fires `rulesForPhase(phase)`; in PLANNING that's
  `planningExpressionRules`, which **lists** `PartitionSelectRule` (default_rules.go:139)
  yet fires it **0 times** (instrumented).

So join-order enumeration happens only in REWRITING. There, `PartitionSelectRule`
enumerates all bipartitions as *exploratory* members, but `advancePlannerStage`
(reference.go) promotes only the single `finalMembers` winner to PLANNING and drops the
exploratory alternatives. The promoted seed is an already-partitioned nested-binary select
(2-quantifier levels), so in PLANNING there is no flat ≥3-quantifier select for
`PartitionSelectRule` to re-partition — only `PartitionBinarySelectRule` fires per level,
swapping operands *within* a level but never **re-associating** the tree. The
FROM-order-chosen associativity is locked at the phase boundary.

This also explains why RFC-041's tried-and-reverted phase-move (remove `PartitionSelectRule`
from REWRITING per Graefe's first reading of Java's `PlanningRuleSet`) made things *worse*:
it deleted the only path (System A) that actually fires the rule, and System B never picked
it up.

## Java reference

Graefe (RFC-041 consult #2) established: Java has join enumeration **only** in
`PlanningRuleSet` (never `RewritingRuleSet`, which is pure normalization), it fires during
PLANNING on the promoted canonical seed, all associativities become competing members of
one Reference, and `PlanningCostModel` picks the cheapest at `OptimizeGroup`. Java's
single-winner promotion is real (`Verify(finalMembers.size()==1)`), but the promoted seed
is the **normalized flat select**, and PLANNING re-enumerates from it. The Go divergence is
that System B's PLANNING exploration does not effectively re-enumerate from the seed.

## Proposed direction (for reviewer confirmation)

Make PLANNING re-enumerate join associativities from a canonical flat seed, Cascades-faithful:

1. **REWRITING normalizes, doesn't enumerate.** Join-order enumeration
   (`PartitionSelectRule` / `PartitionBinarySelectRule`) should not run in REWRITING; the
   REWRITING output for a join is the **canonical flat N-quantifier `SelectExpression`**
   (via `SelectMergeRule`), promoted as the single PLANNING seed. (Matches Java's
   `RewritingRuleSet` = normalization only.)
2. **PLANNING re-enumerates from the flat seed.** Ensure System B actually fires
   `planningExpressionRules` (incl. `PartitionSelectRule`) on the promoted flat select, so
   every associativity becomes a competing member of the join Reference. The instrumented
   "0 fires in PLANNING" must become ">0 on the 3-quantifier seed".
3. **Cost picks at extraction.** RFC-041's stats-aware `compareJoinOrdering` +
   recursive best-member cost then selects the cheapest order. (Already done; also confirm
   the extraction path routes through it rather than the first-member `CostLessWith`.)

**Open design question for reviewers:** is the right mechanism (a) to *unify* the two task
systems (retire System A's `TransformReferenceTask` REWRITING driver in favour of System
B's phase-aware `ExploreGroupTask` for both phases), or (b) to keep both but make System B's
PLANNING exploration fire `planningExpressionRules` on the re-seeded flat select, or (c) to
fix `advancePlannerStage` so the promoted seed is the flat select AND PLANNING re-explores
it? (a) is the most Cascades-faithful (one memo, one driver, phase-keyed rule sets) but the
largest blast radius; (c) is the most surgical. Seeking Graefe's architectural call and
Torvalds's risk read.

## Risk / blast radius

This touches the task engine that drives **every** query, so the gate is mandatory and
heavy: 46/46 targets incl. plandiff conformance (no plan-shape regressions on non-join
queries), stress-1M before/after, determinism 10×, and the new probe green. Any approach
must preserve the `Verify(finalMembers.size()==1)` promotion invariant.

## Test plan (the proof)

`TestFDB_MultiwayJoinOrder_Probe` green: (a) order-invariance — same join under multiple
FROM-orders → byte-identical EXPLAIN; (b) cost-optimal — drives from the smallest table,
EXPLAIN-pinned, differing from FROM-order; (c) cost-monotonicity — perturb a table's count,
the chosen order flips; (d) determinism 10×; (e) shared sub-products merged (PR-A). Plus
the full no-regression gate above.

## Status progression

Draft → (reviewer ACK on direction) → Implemented when the probe is green and the gate passes.
