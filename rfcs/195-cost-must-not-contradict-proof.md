# RFC-195 — The cost model must not contradict what the property layer proves

Status: **proposed**, revision 2 — awaiting joint-review ACK before implementation.
Closes: TODO CQ-29 (six violating estimates), CQ-30 (cost's private cardinality duplicate).
Revision 1 was NAK'd twice on the same fatal finding: it clamped two of the
three cost walks and not the one that winner selection and the invariant test
actually measure, so its own acceptance test refuted it. This revision moves
the PROOF instead of patching walks, which is what the layering was trying to
say all along.

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

**1. The derivation moves down to where its subjects live.** Each plan type
gets its proven-interval derivation as a method beside its cost formula,
exposed through an interface DEFINED IN `properties` — the exact `CostHinter`
precedent (`properties/cost.go:690-705`): properties declares, plans
implement, no layering inversion.

```go
// properties
type CardinalityProver interface {
    ProvenCardinalities(child []Cardinalities) Cardinalities
}
```

`cascades.computeCardinalities` becomes a thin adapter over the same methods
(it keeps its signature for the property-map consumers). After this move the
derivation has ONE home; the property map and every cost walk are readers.

**2. Every cost walk derives bounds IN-WALK and clamps at its combine step.**

```
effective = clamp(formulaEstimate, provenMin, provenMax)   // per bound, only when known
```

One shared helper, applied at three combine sites: `localCost`'s dispatch,
`combineConcreteCost`, and `partitionCost`. Each walk composes child INTERVALS
bottom-up exactly as it already composes child COSTS — the same recursion, one
extra value flowing through it.

In-walk derivation is not an implementation detail; it is what makes the clamp
sound, and it answers the two hardest review findings at once:

- *Priming*: the walks never read `Reference` property maps for bounds
  (`cardinalitiesForRef` returning Unknown on an unprimed map was the no-op
  trap — five of the six floors derive from the child, and at
  `concretePlanCost` children are never primed). Deriving from the same tree
  being costed means the bounds exist whenever the cost does.
- *Determinism*: a clamp that reads primed maps is a no-op before
  `computeRefPlanProperties` runs and live after, so `GetBest`'s pairwise fold
  would become arrival-order dependent — the CQ-23/CQ-24 failure class
  reintroduced through the cost model. In-walk derivation gives one expression
  one cost, before and after priming, by construction — and a
  priming-invariance test pins it (below).
- *Decoupling* (`concretePlanCost` walks concrete children precisely to avoid
  shared-group winners): bounds derived from the concrete tree preserve that —
  no group-weakened interval (`plan_properties.go:991`) ever enters this walk.

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
4. **Zero preservation**: a proven-zero leg under FlatMap keeps cost 0 through
   the clamp; pinned with the exact shape.
5. **One derivation**: `cascades.computeCardinalities` and the in-walk
   derivation agree on every corpus shape (they call the same methods; the
   test proves nobody re-forks them).
6. `TestCardinalityPropertyBoundsCostEstimate_RandomCombos` stays green — the
   guard against the clamp introducing a new inconsistency in composed plans.
7. Full plan-shape golden + plandiff corpus diff, every changed record
   reviewed and the reasoning recorded.
8. 1M stress before/after, results recorded in TODO.md's baseline table.

## What this does not do

It does not make the cost model's estimates *accurate* — a scan's selectivity
guess remains a guess. It makes them **consistent with what is already
proved**, which is a strictly weaker and strictly achievable claim. It also
does not unify the two FORMULA homes (`localCost`'s switch vs
`cost_formulas.go`) — that is real debt, now stated rather than hidden, and it
is separable because both homes will consume the single cardinality home this
RFC creates. Genuine cardinality estimation from statistics is a separate
workstream.
