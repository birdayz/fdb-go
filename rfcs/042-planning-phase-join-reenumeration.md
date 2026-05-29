# RFC-042: PLANNING-phase join re-enumeration (the task-engine half of multi-way join ordering)

**Status:** Draft v2 — direction ACK'd by Graefe + Torvalds; root-cause corrected per
Torvalds NAK (v1 misattributed the firing path). Re-confirming v2 before implementing.

## Correction (v2) — what v1 got wrong

v1 claimed `PartitionSelectRule` fires via "System A" (`ExploreReferenceTask` →
`TransformReferenceTask`) in production. **False, confirmed by grep + both reviewers:**
`ExploreReferenceTask` is pushed only by `Explore()` (planner.go:364) and System A's own
`SaturationCheckTask` loop (:566, :641) — **never by `Plan()`**, which pushes
`InitiatePlannerPhaseTask` (System B). Every production caller uses `Plan()`
(cascades_generator.go:316, plan_harness.go:84). So **System A is test-only/vestigial dead
code**, and in production `PartitionSelectRule` fires via **System B in the REWRITING phase**
(`p.rules` = `DefaultExpressionRules`, which lists it at default_rules.go:120; REWRITING's
`rulesForPhase` returns `p.rules`). Also: Go has **no** `Verify(finalMembers.size()==1)`
invariant — `AdvancePlannerStage` (reference.go) promotes ALL finalMembers; that check is
Java-only. The corrected root cause and fix (below) are mechanism **(c)**: the promoted
PLANNING seed is a partitioned binary tree, not a flat select, so PLANNING's already-wired
`PartitionSelectRule` has no ≥3-quantifier select to re-partition.

**Epic:** RFC-038 PR-C/PR-D, continuing RFC-041. RFC-041 fixed the cost *model* (it now
ranks join orders correctly by recursive, stats-aware total cost). This RFC fixes the
planner *task engine* so the optimal join order is actually a candidate the cost model
gets to choose — the last blocker for "multi-way join ordering proven".

## Problem (precisely instrumented in RFC-041 §Implementation findings)

The acceptance probe (`TestFDB_MultiwayJoinOrder_Probe`, 3-table chain t1=1/t2=20/t3=200)
plans the SAME join under two FROM-orders and they do NOT converge — big-first gets the
costlier `T1⋈(T2⋈T3)` (1077), small-first the optimal `(T1⋈T2)⋈T3` (769). The cost model
*would* pick 769, but for big-first that associativity is never offered as a candidate.

Root cause (corrected v2): join-order enumeration runs **only in REWRITING**, and the
PLANNING seed is already partitioned, so PLANNING cannot re-associate.

* In REWRITING (System B, `p.rules` = `DefaultExpressionRules` incl. `PartitionSelectRule`),
  `PartitionSelectRule` enumerates all bipartitions as *exploratory* members of the join
  Reference.
* `AdvancePlannerStage` (reference.go) promotes the Reference's `finalMembers` to PLANNING
  and drops the exploratory alternatives. The promoted seed is an **already-partitioned
  nested-binary select** (2-quantifier levels), not the flat N-quantifier select.
* In PLANNING, `PlanningExplorationRules` (default_rules.go:139, wired via
  `WithPlanningExpressionRules`, planner.go:254) DOES list `PartitionSelectRule`, but it
  fires **0 times** — because there is no flat ≥3-quantifier select among the PLANNING
  members to bind; only `PartitionBinarySelectRule` fires per 2-quantifier level, swapping
  operands *within* a level but never **re-associating** the tree. The FROM-order-chosen
  associativity is locked at the phase boundary.

This also explains why RFC-041's tried-and-reverted phase-move (remove `PartitionSelectRule`
from REWRITING) made things *worse*: it deleted the REWRITING firing site, and PLANNING
still couldn't fire it (no flat seed) — so enumeration stopped entirely.

**Vestigial System A (cleanup, not the bug):** `ExploreReferenceTask` →
`TransformReferenceTask` (planner.go:511-595) is a second, non-phase-keyed rule driver
reachable only via `Explore()` (test-only). It is dead in production (`Plan()` never pushes
it). Both reviewers flagged it as a code smell to delete; doing so removes the "two systems"
confusion but is orthogonal to the join-ordering fix.

## Java reference

Graefe (RFC-041 consult #2) established: Java has join enumeration **only** in
`PlanningRuleSet` (never `RewritingRuleSet`, which is pure normalization), it fires during
PLANNING on the promoted canonical seed, all associativities become competing members of
one Reference, and `PlanningCostModel` picks the cheapest at `OptimizeGroup`. Java's
single-winner promotion is real (`Verify(finalMembers.size()==1)`), but the promoted seed
is the **normalized flat select**, and PLANNING re-enumerates from it. The Go divergence is
that System B's PLANNING exploration does not effectively re-enumerate from the seed.

## Direction (mechanism c — reviewer-confirmed)

Make PLANNING re-enumerate join associativities from a **canonical flat seed**,
Cascades-faithful (Java: `RewritingRuleSet` = normalization only; `PlanningRuleSet` holds
`PartitionSelectRule`; enumeration fires in PLANNING on the promoted seed, all
associativities become competing members of one Reference, `OptimizeGroup` costs them —
Graefe '95 §2.3 memoized AND/OR-graph search).

1. **REWRITING normalizes to a flat select; it must NOT promote a partitioned associativity.**
   The REWRITING-promoted seed for a join must be the **canonical flat N-quantifier
   `SelectExpression`** (via `SelectMergeRule`). Concretely: remove `PartitionSelectRule`/
   `PartitionBinarySelectRule` from the REWRITING set (`DefaultExpressionRules`) so REWRITING
   does not generate partitioned alternatives, AND verify `RewritingCostModelLess`/
   `FinalizeExpressionsRule` deterministically promote the flat merge (Graefe: "the
   load-bearing assumption"). Add an assertion test: the promoted PLANNING seed for the
   3-way probe has quantifier count == 3 (flat).
2. **PLANNING re-enumerates from the flat seed.** `PlanningExplorationRules` already lists
   `PartitionSelectRule` and is already wired (planner.go:254) — once the seed is flat, it
   fires (instrumented "0 fires in PLANNING" must become ">0 on the 3-quantifier seed") and
   every associativity becomes a competing member. (No task-engine change needed — this is
   why both reviewers landed on (c) over the (a) "unify" rewrite: System B is already wired
   correctly; only the seed shape is wrong.)
3. **Cost picks the cheapest.** RFC-041's stats-aware `compareJoinOrdering` + recursive
   best-member cost selects the optimal order at `OptimizeGroup`. (Already done.) **Also
   confirm** the final extraction (`extractBestPlanFromSelectorVisited`) routes join-order
   selection through the stats-aware comparator, not the first-member `CostLessWith` — if it
   doesn't, fix that too (this is a concrete verification step, not assumed-done).
4. **Cleanup (separable):** delete the vestigial test-only System A (`Explore()` +
   `ExploreReferenceTask`/`TransformReferenceTask`/`SaturationCheckTask`) or move it into
   test helpers, removing the "two rule drivers" smell. Land as its own commit so the
   join-ordering fix's diff stays focused.

**Mechanism decision:** (c), surgical. Graefe ACK'd (a)-"unify" as ideal-but-larger; Torvalds
showed System A is already dead so "unify" reduces to the (4) cleanup, leaving (c) as the
actual fix. Both converge on (c) + cleanup.

## Risk / blast radius

This touches the task engine that drives **every** query, so the gate is mandatory and
heavy: 46/46 targets incl. plandiff conformance (no plan-shape regressions on non-join
queries), stress-1M before/after, determinism 10×, and the new probe green. Note Go has no
`Verify(finalMembers.size()==1)` invariant (Java-only; `AdvancePlannerStage` promotes all
finalMembers) — so the fix must instead ensure REWRITING deterministically leaves the
canonical flat select as the promoted seed (the load-bearing assumption in §Direction-1),
pinned by the seed-quantifier-count assertion test.

## Test plan (the proof)

`TestFDB_MultiwayJoinOrder_Probe` green: (a) order-invariance — same join under multiple
FROM-orders → byte-identical EXPLAIN; (b) cost-optimal — drives from the smallest table,
EXPLAIN-pinned, differing from FROM-order; (c) cost-monotonicity — perturb a table's count,
the chosen order flips; (d) determinism 10×; (e) shared sub-products merged (PR-A). Plus
the full no-regression gate above.

## Status progression

Draft → (reviewer ACK on direction) → Implemented when the probe is green and the gate passes.
