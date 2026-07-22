# RFC-186 — REWRITING cost derivation from the chosen tree + join-cost leaf soundness

**Status:** ACKED — Graefe + Torvalds both delta-confirmed the folded draft (§6);
implementation-ready, gated only on PR #509 (panic sweep) merging first.
**Tracks:** LATEST PRIOS 2026-07-21 item 2 (triple-review convergence: Graefe B+ finding 6,
codex findings 10 and 11, Torvalds finding on magic caps adjacent).
**Baseline:** master at `d70c4d96a` (PR #509, panic sweep, merged). All citations resolve
against that.

## 1. The defect class

Cost-model inputs during REWRITING are not a function of the candidate expression tree — they
are a function of **memo population history**. Two equivalent candidates score differently
because one's child group accumulated more exploratory rewrites. OptimizeGroup then prunes on
that score, and REWRITING pruning is destructive (AdvancePlannerStage promotes exactly one
winner as the PLANNING seed): an unsoundly-scored loser is gone, including potentially the
only implementable shape.

Four concrete sub-defects, each with the evidence line:

**A. Property derivation traverses memo members instead of the one final expression.**
- `properties/expression_count_property.go:15-32` (`EvaluateExpressionCount`): recurses via
  `ref.Members()` — which is **exploratory-ONLY** (`reference.go:258-262`) — and SUMS across
  every member. So it is worse than double-counting: the CHOSEN child (a final member) can be
  entirely invisible to the count while dead exploratory alternatives are billed, all
  memo-history-dependently.
- `planning_cost_model.go:147-173` (`predicateCountByLevel`): recurses via `AllMembers()`
  (exploratory + final), summing across alternatives; the code comment even records the wrong
  turn ("AllMembers (not firstPhysicalChild) — this runs in the REWRITING phase").
- `planning_cost_model.go:654-697` (`countResidualPredicates` — Go's port of Java's
  `NormalizedResidualPredicateProperty` conjunct counting) and `:955-987` (deep hash):
  descend only PHYSICAL children, so during REWRITING (all-logical memo) logical descendants
  are invisible — conjunct counts and tie-break hashes see only the root.

Java is unambiguous: `ExpressionCountProperty.forReference`
(`ExpressionCountProperty.java:114-123`) does `Verify.verify(finalExpressions.size() == 1)`
and visits `getOnlyElement(finalExpressions)`. Every property tier in
`RewritingCostModel.compare` (outerJoinCount → selectCount → tableFunctionCount →
normalizedConjuncts → predicateCountByLevel → semanticHash) derives through exactly ONE final
expression per child reference. The property is a function of the chosen tree, full stop.

**B. The join-cost leaf prices a partial PK prefix as a point probe.**
`planning_cost_model.go:1151-1155` passes `fullBindUnique=true` for every
`RecordQueryScanPlan`; `scanLikeCost` (`:1243-1246`) declares 1 row when
`numBound == len(comps)` — but `comps` is the PRESENT comparisons, not the PK arity. A
correlated join binding only `tenant_id` of PK `(tenant_id, order_id)` carries one comparison,
passes the check, and a million-row prefix scan is priced as a repeatable 1-row inner probe —
catastrophic join orders. The CORRECT check already exists in the same file:
`scanPlanProvableMaxCard` (`:1527-1550`) requires full-PK coverage via
`ctx.GetPrimaryKeyColumns`, and the call site at `:1155` has `ctx` in scope (the index arm
uses it one line below).

**C. `getWinnerForOrdering` silently substitutes the global cheapest.**
`winner_lookup.go:46-50`: when no member satisfies the requested ordering it falls back to
`ref.Winner()` / cheapest-valid — the caller cannot distinguish "satisfies" from "fallback".
Callers passing a REAL ordering: `rule_implement_filter.go:82`, `rule_implement_limit.go:52`,
`rule_implement_projection.go:86` (the PreserveOrdering callers are by-design fine). Each must
either re-derive ordering from the returned member or receive an explicit satisfaction signal.

**D. The join-cost recursive switch is first-child-transparent for whole operator classes.**
`planning_cost_model.go:1122-1224` omits limits, aggregations, union variants,
multi-intersections, IN operators, and recursive plans — an unknown operator exposes its
first child's cardinality and pays zero merge cost (silent first-child default at
`:1214-1223`), flipping join-order selection under e.g. a union. `HintCost` formulas exist
per plan type in `plan/plans/cost.go` (limit/aggregation/unions at `:257-345`,
MultiIntersection at `:355`, IN and recursive at their own sites) — which is exactly why D
dispatches to `HintCost` rather than enumerating formulas (see §2 D).

## 2. Proposed changes

**A (core).** Introduce one shared traversal helper in `properties`:
`ForEachChildFinal(expr, fn)` — for each quantifier's reference, recurse into its SINGLE final
expression; if `len(FinalMembers()) != 1` at costing time, fail loud (plan-time Cascades-infra
assert, §4 class, mirroring Java's `Verify.verify`). **The ==1 invariant is REWRITING-ONLY**
(Graefe change 1): PLANNING deliberately retains multiple finals per group — the
winner-per-requested-ordering retention in `OptimizeGroupTask` (unified_tasks.go:553-575) —
so the helper takes the phase (or the REWRITING-only call sites gate it) and a test proves
the assert does NOT fire on a PLANNING multi-final group. Convert the four sites in A to it:
- `EvaluateExpressionCount` — recurse via the child's one final expression (drop the
  sum-over-members).
- `predicateCountByLevel` — same.
- `countResidualPredicates` — during REWRITING recurse via child finals (keep the physical
  descent for PLANNING where it is correct).
- the deep tie-break hash — same split.

Implementation order (instrument-first, as PERMANENT harness infra — not a one-off script):
the invariant checker lives next to the existing `final_member_invariant.go`
(`VerifyOneFinalPlanPerReference` / `Planner.SetVerifyOneFinal`) as a REWRITING-phase
counterpart checking `len(FinalMembers()) == 1` at the `OptimizeGroupTask` compare site,
enabled permanently in the test harness. Expected 0 violations by bottom-up task order
(OptimizeInputs schedules child OptimizeGroup before the parent compares; REWRITING
PruneToSet keeps exactly `{bestFinal}`). Any violation is a task-ordering bug to fix FIRST —
the invariant is Java's, not an aspiration.

**B.** Thread the PK-length gate into the scan arm at `:1153` and pass it as
`fullBindUnique`. No change to `scanLikeCost` itself. **Precision (Graefe change 2):** this is
NOT the same policy as `scanPlanProvableMaxCard` — that helper with nil ctx falls through and
ALLOWS the 1-row bound (:1545); part B is deliberately STRICTER for join ordering
(ctx-absent → false, never a point probe on unprovable uniqueness). Extract ONE shared
PK-coverage predicate (`pkFullyEqualityBound(pl, ctx) (bool, provable bool)`) used by both
sites with their differing nil-ctx policies explicit at each call site — two subtly different
inline gates in one file is how this bug was born.

**C (re-specified after plannability review).** The fallback yield is LOAD-BEARING and stays:
under a sort, the filter group's only requested ordering is the sort's
(`rule_implement_sort.go:146-154` — no preserve), so declining would empty the group and
`ImplementInMemorySortRule` bails at its `findPhysicalPlan(innerRef) == nil` guard → no plan
at all. The three real-ordering callers do NOT stamp ordering: the wrappers are
`orderingDelegator`s, satisfaction is re-derived through `OrderingSourceRef`
(`winner_lookup.go:265-279`), and `pinOrderedSpine` already declines unsatisfied spines — the
semantics "yield the fallback, never CLAIM the ordering" already hold downstream. The change
is therefore contract-tightening, not behavior: `getWinnerForOrdering` returns
`(member, satisfied bool)` so no future caller can mistake fallback for satisfaction; the
three call sites document the delegation; and a pin proves a fallback member is never treated
as ordering-satisfying (the enforcer fires instead). PreserveOrdering callers: `satisfied`
trivially true.

**D.** Close the omitted-operator hole by DISPATCH, not duplication (Graefe change 3): the
recursive walk's non-leaf case calls the plan's own `HintCost(child, stats)` (the single
source of truth in `plan/plans/cost.go`) instead of hand-copying formulas into new switch
arms. Explicit arms remain ONLY for the scan/index leaves (the RFC-069 selectivity override)
and the join operators the walk exists to recurse through. This kills the CLASS — a new plan
type is priced automatically by its own HintCost — and the loud default (log-once +
first-child) covers anything without one.

## 2b. Noted, out of scope (recorded per review)

`deepHashCode`'s `firstPhysicalChild` descent (planning_cost_model.go:980) is
insertion-order-dependent in PLANNING as well — the file's own comment flags it. Not part of
this RFC's REWRITING scope; recorded here so it is not lost (candidate for the item-2
follow-up list).

## 3. What this RFC does NOT change

- REWRITING pruning aggressiveness (exactly-one-winner promotion) — Java's design; the fix
  makes the pruning DECISION sound, not less destructive.
- Exploratory-member retention (needed for match memoization) — Java keeps them too; they just
  become invisible to cost derivation, as in Java.
- The RewritingCostModel tier ORDER for the tiers Go has. NOTE the honest scope: Go
  deliberately omits Java's first tier, `outerJoinCount` (`planning_cost_model.go:75-97`
  documents the rationale) — this RFC does not add it; that omission is a pre-existing,
  documented divergence outside this RFC's scope.
- `scanLikeCost`'s metadata-independence contract (RFC-069) — the PK gate is computed by the
  caller that already holds ctx.

## 4. Failure scenarios closed (test plan)

1. **Determinism pin (A):** plan the same query twice with different exploration epochs /
   rule-application order (harness: seed extra no-op rewrites into one run); assert identical
   REWRITING property values and identical chosen winner. Today this can flip.
2. **Alternative-count independence pin (A):** construct a memo where a child group holds 1
   vs N equivalent exploratory alternatives of the same subtree; assert selectCount /
   predicateCountByLevel identical in both.
3. **Join-order regression (B):** composite PK `(tenant, id)`, correlated join binding only
   `tenant`; assert the inner is NOT priced cardinality-1 and the join order does not select
   the prefix scan as inner point probe (EXPLAIN-shape pin + cost assertion).
4. **Ordering-fallback pin (C, re-specified):** a group whose only member violates the
   requested ordering under a sort; assert (a) the rule still YIELDS (the group is not
   emptied — plannability preserved), (b) the fallback member is never CLAIMED as satisfying
   (pinOrderedSpine declines the spine), and (c) the in-memory-sort enforcer fires and the
   final plan is correctly ordered.
5. **Operator-dispatch pins (D):** per newly-priced operator class, a focused cost assertion
   that the walk's result equals the plan's own `HintCost` (union of N children costs the
   sum + merge, limit caps, etc.) — pinning the dispatch, not hand-copied formulas.
6. **Stress before/after:** 1M stress run per CLAUDE.md workflow; plan-shape diff over the
   cross-engine corpus (explain-differ) to catch unintended winner flips. Any flip must be
   explainable as "previously unsoundly scored".
7. **Invariant-scope pins (Graefe change 1):** (a) the ==1 assert FIRES on a REWRITING
   costing over an un-optimized child (constructed memo); (b) the assert does NOT fire on a
   PLANNING group holding multiple finals via winner-per-ordering retention.
8. **No-empty-group pin (part C):** declining every real-ordering member cannot empty the
   group — the preserve/enforcer path must remain and produce the sort-enforced plan.

## 5. Risks

- **Winner flips.** Sound scoring WILL change some REWRITING winners. The triage artifact is
  NAMED and committed, not hand-waved: an explain-diff report (checked into the PR) listing
  EVERY corpus flip with before/after plan shape + cost trace, each line classified
  (expected-better | neutral | regression), every regression pinned with a yamsql scenario
  before merge, and the 1M stress before/after row added to TODO.md's baseline table.
- **The ==1 invariant may be violated today** (un-optimized child at costing time). The
  instrument-first step converts that from a landmine into a finding; violations are
  task-ordering bugs and block the enforcement commit until fixed.
- **D's dispatch changes join costing broadly.** Mitigation: land D as its own commit with
  the stress comparison isolated to it.

## 6. Review log

- Graefe: ACK-WITH-CHANGES — (1) scope the ==1 invariant to REWRITING + PLANNING no-fire
  test; (2) extract one shared PK-coverage predicate, state B's stricter nil-ctx policy
  explicitly; (3) part D dispatches to HintCost instead of copying formulas. All folded
  above. Also endorsed: assert-not-decline for invariant violations (Q2), caller-side gate
  layering (Q3), keeping the concrete walk (Q5 — group-winner derivation would reintroduce
  the shared-winner defect).
- Torvalds: ACK-WITH-CHANGES — (1) `Members()` is exploratory-only (defect worse than
  drafted; now stated), Go symbol is `countResidualPredicates`, D's formula range corrected;
  (2) §3's tier-order parity claim scoped to the tiers Go has (Java's `outerJoinCount` tier
  deliberately omitted, documented); (3) the invariant instrument is permanent harness infra
  beside `final_member_invariant.go`, not a one-off script; (4) part C AS ORIGINALLY DRAFTED
  (decline-on-unsatisfied) NAK'd as a plannability regression — the fallback yield is
  load-bearing under a sort; C re-specified to contract-tightening (yield, never claim; the
  existing `orderingDelegator`/`pinOrderedSpine` machinery already carries the semantics);
  test 4 rewritten accordingly; (5) the winner-flip triage artifact named (committed
  explain-diff + per-flip classification + yamsql pins + 1M stress table row).
  NOTE: Graefe's Q4 endorsement of decline-on-unsatisfied was superseded by Torvalds's
  plannability evidence; Graefe verified that evidence and ruled the re-specified C correct.
- Delta re-confirmations: Graefe ACK ("proceed to implementation") + Torvalds ACK (3
  editorial nits, folded). RFC is implementation-ready pending #509 merge.

## 7. Implementation addendum — §2A as landed (Graefe-ruled)

The instrument-first probe REFUTED §2A's original mechanism before any conversion landed.
The one-final invariant checker found multi-final child groups at REWRITING compare time
across the corpus; the task-level trace (TestPlanHarness_LeftJoin) showed child groups'
REWRITING OptimizeGroup never runs — only the root's phase-init compare executes — and the
stage boundary deliberately crosses un-pruned groups (the universal prune was previously
tried and reverted: PLANNING cannot re-derive the RFC-153 buried-leg / cross-join-EXISTS
shapes). Java's Verify(==1) is the CONJUNCTION of physical prune + PLANNING re-derivation
parity; Go has neither safely available. §2A's ==1-by-pruning premise was therefore false
on this architecture.

Graefe ruling (full text in the review log): ACK the re-specified mechanism — DESIGNATED-
FINAL DERIVATION, "a virtual prune": every REWRITING cost property derives through one
deterministically-chosen final per child reference, chosen with the SAME comparator
OptimizeGroup uses, with four binding conditions, all implemented:
1. Generation-keyed memoization (expressions.FinalsGeneration, bumped at every final-set
   mutation site; conservative global over-approximation of the subtree generation vector).
2. Cycle guard (visiting set; hash-ranked conservative answer on a back-edge, uncached).
3. OptimizeGroup's own compare derives through designations (the planner-owned scope backs
   costModelForPhase(REWRITING)) and coherence is asserted wherever a REWRITING winner is
   stamped: winner == designation (SetVerifyRewritingCoherence, permanently on in the
   embedded plan harness).
4. Stamping unchanged; children stay unstamped through REWRITING.

Consequences landed with the mechanism:
- Tier 3 (normalized residual conjuncts) is LIVE on logical trees for the first time — the
  physical-only descent counted nothing on the all-logical REWRITING memo.
- Tier 4's AllMembers walker (the defect site) is DELETED; the designated walker replaces it.
- Tier 5's tie-break hashes the designated tree (the old firstPhysicalChild descent hashed
  only the root during REWRITING, making ties arrival-order-resolved).
- A re-push-OptimizeGroup-on-final-growth hook was tried and REVERTED (documented at the
  yield site): re-pruning stamped groups late loses alternatives PLANNING needs — the same
  failure mode as the reverted universal prune.
- Flip triage round 1: exactly one behavioral flip across the embedded suite —
  DistinctOverUnionAll: the old inflated tier-4 accidentally picked the two-legged union
  form; the designated comparator deterministically picks the single-branch collapse
  (identical UNION ALL branches under the enclosing DISTINCT), whose pk-scan is provably
  distinct → dedup validly elided → filter-pushed scan. Classified EXPECTED-BETTER (correct
  rows, fewer operators); the bug-hunt pin updated to reject the actual bug class (multi-leg
  union without dedup) while accepting the collapse.
- DIVERGENCES.md records "virtual vs physical prune" with the end-state (designation
  degenerates to Verify(==1) when PLANNING re-derivation parity lands).

Parts B, C, D proceed unchanged per §2.
