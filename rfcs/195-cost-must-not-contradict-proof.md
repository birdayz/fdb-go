# RFC-195 — The cost model must not contradict what the property layer proves

Status: **proposed**, awaiting Graefe + Torvalds ACK before implementation.
Closes: TODO CQ-29 (six violating estimates), CQ-30 (cost's private cardinality duplicate).

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

## Root cause: cardinality has two homes

Cardinality is computed twice, by two layers that never meet:

- `computeCardinalities(w, plan) properties.Cardinalities` (`plan_properties.go:570`)
  derives **structural** bounds — an ungrouped aggregate emits exactly one row,
  `DefaultOnEmpty` emits at least one, `UNION ALL` emits at least its seed. These
  are proofs, not estimates.
- `HintCost(child []Cost, …) Cost` derives a **point estimate** from selectivity
  constants: `DistinctCost` returns `in * DistinctSelectivity`, `TypeFilterCost`
  returns `in * TypeFilterSelectivity`, `recursiveCost` returns
  `seedCard * recCard`.

The cost formulas take only a child `Cost` — a bare number. They have no access
to the proven bounds, so they *structurally cannot* respect them. Five of the six
violations are a selectivity multiplier with no floor; the sixth is a multiplier
with no cap. Every one is the cost layer asserting something the property layer
has already disproved.

This is the same shape as every defect found in the preceding branch: a fact with
two homes, where one home can prove things the other cannot see.

## Why it matters beyond tidiness

The 700 000× case is not cosmetic. An ungrouped aggregate is priced as if it
emits 700 000 rows when it provably emits one, so **every join ordering above it
is computed against a number that is wrong by five orders of magnitude**. The
`below min` cases are subtler and arguably worse: a plan priced at 0 or 0.7 rows
wins comparisons it should lose, and a zero propagates multiplicatively through
`FlatMapCost` and `NestedLoopJoinCost` — costing an entire join subtree at zero.

## Decision

**The cost model's point estimate is clamped to the interval the property layer
proves.** Where the property proves nothing, the formula's estimate stands
unchanged.

```
effective = clamp(formulaEstimate, provenMin, provenMax)
```

with each bound applied only when it is known (`Cardinality.IsUnknown()` is
false). This keeps the selectivity heuristics where they are informative — they
remain the only source of information for unbounded scans — and defers to proof
wherever proof exists. A heuristic that contradicts a proof is never the better
number.

### Where it goes

Two choke points, both already in the `cascades` package where
`computeCardinalities` is in scope:

- `planning_cost_model.go:1674`, the central `HintCost` dispatch in the
  join-ordering cost walk.
- `expression_partition.go:452`, the partition-level cost used for ranking.

A single `clampToProvenCardinality(plan, cost) Cost` applied at both. This prices
every current plan type and every future one automatically, which is the same
argument that justified the `HintCost` dispatch itself.

### Rejected alternatives

**Add a floor to each violating formula** (`max(1, in*0.7)` and friends). This is
what the six exclusions individually suggest, and it is wrong: it fixes six
instances of a class while leaving the class open, and it puts the knowledge
"this operator guarantees a row" in a second place that must be kept in sync with
the property. The next operator added gets it wrong again — exactly how these six
accumulated.

**Make cost read cardinality from the property entirely, deleting the formulas'
cardinality output** (the literal reading of CQ-30). Attractive, but wrong as
stated: the property yields an *interval*, frequently `[0, unknown]`, while
cost ranking needs a *point*. Deleting the estimate would leave the planner with
no basis to rank two unbounded scans. Cost legitimately estimates; it just must
not estimate outside the proof.

**Clamp inside each plan's `HintCost`.** Would require the `plans` package to
depend on `cascades`' property computation, inverting the layering.

## Consequences to expect

Plan selection **will** change — that is the point, and it must be measured, not
assumed:

- Every currently-excluded shape becomes correctly priced; the ungrouped
  aggregate case moves by five orders of magnitude, which will reorder joins
  above it.
- `cardinality_cost_bound_test.go`'s six exclusions must be *deleted*, not
  updated. They are self-cleaning: the harness fails loudly if a documented
  violation stops reproducing, so leaving them would turn this change red.
- The plan-shape golden and the plandiff corpus will move. Every changed record
  needs review, not blind re-blessing — a cost fix that changes a plan to a
  *worse* one is a regression this RFC would otherwise hide.
- The 1M stress comparison must run before and after (CLAUDE.md's planner-change
  workflow), since join reordering is exactly what it measures.

## Verification plan

1. Delete all six `addExcluded` registrations; the standing invariant must then
   hold for every shape with no exceptions.
2. Mutation-check the clamp in both directions: removing the floor must make the
   five `below min` shapes red; removing the cap must make the ungrouped
   aggregate red.
3. `TestCardinalityPropertyBoundsCostEstimate_RandomCombos` — the existing
   random-composition sweep — must stay green, which is the guard against the
   clamp introducing a *new* inconsistency in composed plans.
4. Full plan-shape golden + plandiff corpus diff, every changed record reviewed
   and the reasoning recorded.
5. 1M stress before/after, results recorded in TODO.md's baseline table.

## What this does not do

It does not make the cost model's estimates *accurate* — a scan's selectivity
guess remains a guess. It makes them **consistent with what is already proved**,
which is a strictly weaker and strictly achievable claim. Genuine cardinality
estimation from statistics is a separate workstream.
