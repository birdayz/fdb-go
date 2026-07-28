# RFC-195 — The cost model must not contradict what the property layer proves

Status: **proposed**, revision 3 — awaiting joint-review ACK before implementation.
Closes: TODO CQ-29 (six violating estimates), CQ-30 (cost's private cardinality duplicate).
Revisions 1 and 2 were each NAK'd twice. Rev 1 clamped two of the three cost
walks and not the one winner selection and the invariant test actually
measure — its own acceptance test refuted it. Rev 2 moved the proof to the
right layer but rode bounds on the first-member cost recursion
(order-dependent, and TIGHTER than the property layer's own deliberate
weakening) and clamped only the physical side (inverting the extraction
preference on the RFC's own table shapes). This revision keeps rev 2's
proof-relocation and corrects both: bounds compose order-independently over
all members, and the clamp is symmetric across the logical/physical pair,
with both properties pinned by tests the earlier designs would have failed.

## The defect, measured

`cardinality_cost_bound_test.go` holds every plan shape's cost estimate against
the interval `computeCardinalities` **proves** for that shape. Six shapes are
currently excluded as known violations, and each one still reproduces:

| shape | cost estimate | proven bound | error |
|---|---|---|---|
| `streamingAggregation/ungrouped` | **700 000** | max = 1 | 700 000× too high |
| `recursiveLevelUnion/recursiveLegCollapsesTowardZero` | 0 | min = 1 | below a guaranteed row |
| `defaultOnEmpty/overZeroCostChild` | 0 | min = 1 | below a guaranteed row |
| `distinct/overExactlyOneChild` | 0.7 | min = 1 | below a guaranteed row |
| `unorderedPrimaryKeyDistinct/overExactlyOneChild` | 0.7 | min = 1 | below a guaranteed row |
| `typeFilter/overExactlyOneChild` | 0.5 | min = 1 | below a guaranteed row |

These are not six bugs. They are one principle violated six times.

## Why it matters beyond tidiness

The 700 000× case is not cosmetic. An ungrouped aggregate is priced as if it
emits 700 000 rows when it provably emits one, so **every join ordering above it
is computed against a number that is wrong by five orders of magnitude**. The
`below min` cases are subtler and arguably worse: a plan priced at 0 or 0.7 rows
wins comparisons it should lose, and a zero propagates multiplicatively through
`FlatMapCost` and `NestedLoopJoinCost` — costing an entire join subtree at zero.

## Root cause, restated after review

Revision 1 said "cardinality has two homes" and proposed a clamp that consulted
both — three homes. The review corrected the diagnosis on two axes:

**There are THREE cost walks, and revision 1 clamped the wrong two.**

1. `properties.EstimateCost` → `localCost` (`properties/cost.go:308,511`) —
   the walk winner selection ranks with (`planning_cost_model.go:404-405`) and
   the walk the invariant test itself measures
   (`cardinality_cost_bound_test.go:115`).
2. `concretePlanCost` (`planning_cost_model.go:1541`) — the join-ordering walk
   over concrete `GetChildren()`.
3. `partitionCost` (`expression_partition.go:444`) — partition ranking, which
   feeds RULE SELECTION, not just plan choice.

Clamping 2 and 3 while the test and the winner rank via 1 means revision 1's
own acceptance criterion ("delete the six exclusions, the invariant holds")
fails on day one.

**The misplaced thing is the proof, not the clamp.** The layering is
`expressions ← properties ← plans ← cascades`. The derivation
`computeCardinalities` (`plan_properties.go:570`) lives at the TOP, in
`cascades`, yet it switches exclusively on `plans.*` types. `properties` cannot
see it, which is why revision 1 contorted the clamp upward into the two walks
that happen to live high enough — and missed the one that matters. The
derivation's home is the `plans` layer, beside the `HintCost` formulas it
constrains.

There is also a second two-homes problem the review surfaced: the cost
FORMULAS live twice (localCost's switch in `properties/cost.go:511` for
expression types; `cost_formulas.go` shared by `concretePlanCost` and the
physical wrappers' `HintCost`). This RFC does not unify the formula homes —
that is real but separable — it unifies the CARDINALITY home and makes every
formula home consume it.

## Decision

**1. The derivation moves down to where its subjects live — BOTH kinds of
subject.** Cost derivation already has two homes by layering necessity, and
bounds derivation mirrors that structure exactly rather than pretending one
mechanism can serve both:

- *Physical plans* implement an interface DEFINED IN `properties` — the exact
  `CostHinter` precedent (`properties/cost.go:690-705`): properties declares,
  plans implement, methods beside the cost formulas.

  ```go
  // properties
  type CardinalityProver interface {
      ProvenCardinalities(child []Cardinalities) Cardinalities
  }
  ```

- *Logical expressions* CANNOT implement that interface — `expressions` sits
  below `properties`, and the method signature names `Cardinalities`, so
  implementing it would invert the layering. Their derivation is typed arms
  in a `properties`-local `provenCardinalities(e, child)` switch, beside
  `localCost`'s existing typed arms — the same duality cost already has
  (switch for logical, interface for physical), applied to bounds.

This is two arms, not two homes, and the difference is TESTED: a
logical/physical PARITY test pins that every logical arm and its physical
twin derive the identical interval from identical child intervals. Without
that pin the split re-creates the drift this RFC exists to end; with it, the
pair is one derivation expressed twice by layering necessity.
`cascades.computeCardinalities` becomes a thin adapter over the plan-side
methods (keeping its signature for the property-map consumers).

The parity requirement is not hygiene — it is CORRECTNESS. `localCost`'s
contract (`cost.go:669-676`) is that a physical wrapper hints a cost
equivalent to or lower than its logical operator, so cost-driven extraction
prefers the physical plan (`rule_implement_filter_test.go:82-107` pins it).
A clamp applied only to the physical side floors `distinct/overExactlyOne`
to 1 while `LogicalDistinctExpression` keeps 0.7 — the preference INVERTS on
exactly the shapes in this RFC's own table. Symmetric clamping (same bounds,
both sides) preserves the ≤ relation by construction; the parity test is
what makes "symmetric" a fact rather than an intention.

**2. Every cost walk clamps at its combine step; bounds compose
ORDER-INDEPENDENTLY, not along the cost recursion.**

```
effective = clamp(formulaEstimate, provenMin, provenMax)   // per bound, only when known
```

One shared helper, applied at three combine sites: `localCost`'s dispatch,
`combineConcreteCost`, and `partitionCost`.

How child bounds are obtained differs from how child COSTS are obtained, and
the difference is load-bearing. Revision 2 said bounds ride "the same
recursion, one extra value" — that elegance was a defect. The cost recursion
resolves a child `Reference` to its FIRST member
(`firstMemberCostMemoised`, `properties/cost.go:349`); for cost that
arbitrariness is accepted and long-standing. For BOUNDS it is TIGHTER than
the property layer's own answer: `cardinalitiesForRef` deliberately weakens
across ALL members ("a group with both bounded and unbounded final members
correctly weakens to the least-constraining bound. Never second-guess it."),
and taking `members[0]`'s bounds instead means a group whose first member is
a unique probe (max=1) and whose second is a scan clamps the parent to a
1-vs-700000 cliff decided by MEMBER INSERTION ORDER — arrival-order
dependence in `GetBest`, the CQ-23/CQ-24 class, re-entering through the
bounds channel. So:

- In the EXPRESSION walk, a child `Reference`'s interval is
  `WeakenCardinalities` over `Members()` + `FinalMembers()`, recursively,
  memoised per Reference — order-independent by construction. Cost keeps
  first-member; bounds do not.
- In the CONCRETE walk there are no references — bounds derive from the
  concrete child, which also preserves that walk's deliberate decoupling
  from shared-group winners: no group-weakened interval
  (`plan_properties.go:991`) enters it.

What in-walk derivation (in this corrected form) still buys, versus reading
primed property maps:

- *Priming*: the walks never read `Reference` property maps
  (`cardinalitiesForRef` returning Unknown on an unprimed map was the no-op
  trap — five of the six floors derive from the child, and at
  `concretePlanCost` children are never primed). Deriving from the tree
  being costed means the bounds exist whenever the cost does.
- *Determinism*: one expression, one cost — before priming, after priming,
  and under any member-insertion order. Pinned by BOTH a priming-invariance
  test and a member-order-permutation test (below); the permutation test is
  the one the first-member design would have failed while priming-invariance
  stayed green.

**Plumbing, named rather than waved at:** `localCost` gains the child
intervals alongside child costs; `estimateCostMemoised`
(`properties/cost.go:308-330`) threads `[]Cardinalities` next to `[]Cost`;
the per-`Reference` memo caches the pair (cost from the first member, bounds
weakened over all members); `firstMemberCostMemoised` keeps its name and its
cost-only job, with the bounds memo beside it, not inside it.

At `partitionCost` the walk sees no children (`HintCost(nil, …)`): structural
bounds that need no child (ungrouped aggregate max=1) still clamp; child-floor
bounds are unknown there and the clamp is a deterministic no-op. Since
partition ranking feeds rule selection, its plan-shape diffs are reviewed with
the same care as goldens (Consequences).

**3. Zero is preserved, deliberately.** The clamp floors only when the proof
says min ≥ 1. A proven-zero or possibly-zero child keeps its zero — the
`FlatMapCost` LIMIT-0 contract survives untouched. (CockroachDB goes as far as
panicking on RowCount ≤ 0 with Max > 0; our equivalent guard is the standing
invariant test, not a runtime panic.) A dedicated shape pins zero-preservation
so the clamp can never be "fixed" into flooring guaranteed-empty legs.

## Prior art, honestly attributed

CockroachDB does exactly this: `statistics_builder.go:3151`
(`finalizeFromCardinality`) clamps every RowCount into the proven
`[Cardinality.Min, Max]`, with `props/cardinality.go` documenting the bounds as
"hard bounds that are never incorrect" versus estimates. It applies the helper
from ~29 per-operator sites and polices the invariant with assertions; our
standing `cardinality_cost_bound_test.go` is the stronger analogue of that
police.

**Java has no such defect — and no such rung.** `PlanningCostModel.java:100-330`
is a pure lexicographic tier comparator; there is NO numeric cost estimate
anywhere in it, hence nothing to clamp. Its tier 1 already compares proven
maxima and lets an unknown bound lose. The statistics-driven scalar rung in Go
(`planning_cost_model.go:397-405`) is a documented Go-only read-side extension
(sanctioned; wire untouched). This RFC is therefore not closing a Java
divergence — it is making a Go extension stop contradicting the Java-ported
proof layer it sits next to. Revision 1 failed to say this.

## Rejected alternatives

**Add a floor to each violating formula** (`max(1, in*0.7)` and friends). Fixes
six instances of a class while leaving the class open, and puts "this operator
guarantees a row" in a second place that must be kept in sync with the
property. The next operator gets it wrong again — exactly how these six
accumulated.

**Delete the estimate; use only the proven interval** (the literal CQ-30
reading). The property yields an interval, frequently `[0, unknown]`; ranking
needs a point. Two unbounded scans would become incomparable. Cost
legitimately estimates — constrained by proof. One home for the DERIVATION,
two readers with different jobs; that is not the two-homes pathology, and this
revision stops pretending otherwise.

**Java's shape: make the proof a higher tier instead of clamping the
heuristic.** Already true — Go ports Java's tier list including the
proven-maxima tier, and the scalar rung sits BELOW those tiers. The defect is
that the extension rung lies within its own slot, and tiers cannot fix a rung
that only fires on ties above it. The clamp makes the rung internally
consistent; the tiers above it are untouched.

**Clamp by reading the primed property maps** (revision 1's implicit design).
No-op for five of six shapes at the concrete walk, and arrival-order
nondeterminism at `GetBest`. Rejected for the reasons in Decision §2.

## Consequences to expect

Plan selection **will** change — that is the point, and it must be measured,
not assumed:

- Every currently-excluded shape becomes correctly priced; the ungrouped
  aggregate case moves by five orders of magnitude, which will reorder joins
  above it.
- `cardinality_cost_bound_test.go`'s six exclusions must be *deleted*, not
  updated. They are self-cleaning: the harness fails loudly if a documented
  violation stops reproducing, so leaving them would turn this change red.
- The plan-shape golden and the plandiff corpus will move — including diffs
  caused by `partitionCost` clamping changing RULE selection, which are
  reviewed shape-by-shape, not blindly re-blessed. A cost fix that changes a
  plan to a worse one is a regression this RFC would otherwise hide.
- The 1M stress comparison must run before and after (CLAUDE.md's
  planner-change workflow), since join reordering is exactly what it measures.

## Verification plan

1. Delete all six `addExcluded` registrations; the standing invariant must
   then hold for every shape with no exceptions — and it now CAN, because the
   clamp lives in the walk the test measures.
2. Mutation-check the clamp in both directions: removing the floor must redden
   the five `below min` shapes; removing the cap must redden the ungrouped
   aggregate.
3. **Priming invariance**: for a corpus of plans, `properties.EstimateCost`
   before `computeRefPlanProperties` equals after — pinned as a test, since
   this is the determinism hazard the review named and
   `cost_model_total_preorder_test.go` cannot see it (its corpus is unprimed).
4. **Member-order invariance**: permuting a `Reference`'s member insertion
   order leaves every cost unchanged. This is the test the first-member
   bounds design would have FAILED while priming-invariance stayed green —
   the two invariance tests cover different doors into the same
   nondeterminism.
5. **Zero preservation**: a proven-zero leg under FlatMap keeps cost 0 through
   the clamp; pinned with the exact shape.
6. **Method-level agreement, not walk-level**: for every operator, the
   plan-side `ProvenCardinalities` method and `cascades.computeCardinalities`'
   corresponding arm produce the same interval FROM THE SAME CHILD INTERVALS.
   Asserting agreement on whole-walk outputs would be self-refuting: the
   property map weakens child bounds across all members while the concrete
   walk uses the concrete child, so their outputs legitimately differ — the
   invariant is that no one re-forks the per-operator derivation, not that
   every walk sees the same tree.
7. **Logical/physical parity**: every logical arm in `provenCardinalities`
   and its physical twin's method produce the identical interval from
   identical child intervals — the pin that keeps the clamp symmetric and the
   extraction preference (`physical ≤ logical`) intact; the existing
   preference test (`rule_implement_filter_test.go`) must stay green.
8. `TestCardinalityPropertyBoundsCostEstimate_RandomCombos` stays green — the
   guard against the clamp introducing a new inconsistency in composed plans.
9. Full plan-shape golden + plandiff corpus diff, every changed record
   reviewed and the reasoning recorded.
10. 1M stress before/after, results recorded in TODO.md's baseline table.

## What this does not do

It does not make the cost model's estimates *accurate* — a scan's selectivity
guess remains a guess. It makes them **consistent with what is already
proved**, which is a strictly weaker and strictly achievable claim. It also
does not unify the two FORMULA homes (`localCost`'s switch vs
`cost_formulas.go`) — that is real debt, now stated rather than hidden, and it
is separable because both homes will consume the single cardinality home this
RFC creates. Genuine cardinality estimation from statistics is a separate
workstream.
