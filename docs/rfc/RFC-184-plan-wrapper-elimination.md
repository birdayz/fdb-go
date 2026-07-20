# RFC-184: Physical-Plan Wrapper Elimination (RFC-183 P5 terminal)

## Status: Draft (needs Graefe + Torvalds ACK before implementation)

This RFC carries out the terminal step RFC-183 §11 deferred: making a plan's
child edge storable exactly once, so the divergent "stored twice" state the
ordinal-binding bug family lived in becomes *unrepresentable* rather than
*reconciled*.

## Problem

During planning, a physical operator is represented **twice**:

1. A **cascades wrapper** (`physical_*_wrapper.go`, 22 of them) that holds the
   live child edge as `innerQuant expressions.Quantifier` — a quantifier over
   the child's **memo Reference** (group), which the optimizer keeps current as
   it explores.
2. The **plan** it wraps (`plans.RecordQuery*Plan`), which itself holds an
   `innerQ expressions.Quantifier` — but pointing at a **snapshot** of the
   child (often a stale or nil inner during planning).

Nothing forces `wrapper.innerQuant` and `wrappedPlan.innerQ` to agree. They are
reconciled only at extraction, in each wrapper's `WithChildren`:

```go
// physical_limit_wrapper.go
if innerPlan := findPhysicalPlan(qs[0].GetRangesOver()); innerPlan != nil {
    newPlan := w.plan.WithInner(innerPlan)   // relink the plan's stale inner
    return &physicalLimitWrapper{plan: newPlan, innerQuant: qs[0]}, nil
}
```

When that reconciliation is imperfect the stale snapshot survives into the
extracted plan. That is not hypothetical — it is a recurring, expensive bug
class. The limit wrapper's own comment records `Limit(Project(Fetch(<nil>)))`
extracting to 0 rows because the eagerly-snapshotted nil-inner plan was left in
place (RFC-070); the same shape recurred for InJoin. The ordinal-binding bugs
this workstream chased (multi-leg, 3-way) are the same family: two
representations of one edge, only one of them correct.

## Current state (why this is now tractable)

RFC-183 P0–P4 already did the hard preparatory work:

- Plans store children as `expressions.Quantifier` (not raw `RecordQueryPlan`
  pointers) — verified: **zero** plan structs hold a raw child pointer.
- Plans implement `expressions.RelationalExpression` in full (structural surface
  directly; `GetResultValue` / `CanCorrelate` / `ChildrenAsSet` /
  `GetCorrelatedToWithoutChildren` via the embedded `PlanExprBase` defaults).
- `physicalPlanExpression` is nothing but `RelationalExpression` +
  `GetRecordQueryPlan()`, and a plan already satisfies both.

So a plan is **already** a valid cascades expression. The wrapper is a vestigial
*second* representation whose only distinct jobs are:

1. Hold the **live** memo child quantifier (`innerQuant`) — the plan holds a
   snapshot instead.
2. **Override** a few `PlanExprBase` defaults with planning-specific bodies:
   `GetResultValue` (fresh QOV), `GetCorrelatedToWithoutChildren` (walks
   predicates/comparands — see `dataAccessExprCorrelations`), and the
   cost/ordering hints `HintCost` / `HintOrdering` / `OrderingSourceRef`, which
   today the wrapper *delegates straight back to the plan* (`w.plan.HintCost`).

## Proposed direction

Make the **plan** the sole cascades expression; delete the wrapper layer.

- The plan's `innerQ` becomes the live memo quantifier the optimizer maintains —
  there is no second edge to snapshot, so `WithInner`/`WithChildren` reconcile
  nothing and the nil-inner class cannot arise.
- The wrapper's overriding physical methods move onto the plan (or a shared
  mixin) so the plan carries its own cost/ordering/correlation node-information —
  which RFC-183 P5 already began (`HintCost` lives on the plan; the wrapper only
  forwards).
- Implementation rules construct the plan directly with its child quantifier
  instead of `newPhysical*Wrapper(plan, innerQuant)`.

Once the wrapper is gone, "child edge stored twice" is unrepresentable: the type
system offers exactly one place — `plan.innerQ` — to put it.

## Why RFC-183 §11 deferred it (the real obstacles)

1. **Live-vs-snapshot child.** The plan's `innerQ` must hold a live memo
   `Reference` throughout exploration, not a materialized child. This must be
   verified safe for every plan constructor and every `With*` copy path; a
   constructor that eagerly reads through `innerQ` would reintroduce the
   snapshot.
2. **`GetResultValue` identity.** The wrapper mints a fresh
   `QuantifiedObjectValue` per call; the plan's `PlanExprBase` default differs.
   Whichever the memo keyed on must be preserved to avoid silent regrouping.
3. **Per-wrapper physical overrides.** Each of the 22 has its own
   `GetCorrelatedToWithoutChildren` / ordering-source semantics that must be
   moved faithfully (the data-access wrappers especially — correlated PK-scan
   probes report correlations through comparands, not children).
4. **Rule-site churn.** Every implementation rule and every `findPhysicalPlan` /
   `IsPhysicalX` call site assumes the wrapper type. These become plan-type
   assertions.

## Phased migration

One wrapper type per phase (transparent unary wrappers first — limit, fetch,
projection, in-memory-sort — where the plan already owns cost/ordering):

1. Move the wrapper's overriding methods onto the plan (or a shared physical
   mixin); prove they equal the wrapper's for that type.
2. Repoint the implementation rule and `IsPhysicalX`/`findPhysicalPlan` at the
   plan; delete the wrapper.
3. Validate the phase (below) before starting the next.

Set operations and joins (multi-child, correlated) come last.

## Validation (per phase)

- **EXPLAIN / yamsql suite** is the primary gate: plan shapes must be identical,
  or any change justified as an equal-cost tie reshuffle — never rubber-stamped.
- **1M stress** for row-count and latency parity (a memo-regrouping regression
  shows up as a plan-shape or row delta).
- The RFC-182 differential harness (row soundness **and** the new cost/ordering
  axis) runs as a generative net across the migration.
- Determinism check (10×) on affected EXPLAIN tests — a wrapper-vs-plan identity
  slip surfaces as a nondeterministic winner.

## Non-goals

- No wire-format change (plans extract to the identical executable form).
- No new query capability; this is purely the internal representation.

## Open questions for review

1. Should the physical override surface live on each plan, or on a shared
   embedded `physicalPlanBase` (parallel to today's `PlanExprBase`)?
2. Is there any memo path that requires the wrapper's fresh-QOV `GetResultValue`
   identity, or can the plan's own result value serve throughout?
3. Order of phases — is "transparent unary first" right, or should a
   data-access leaf (which has no child edge to duplicate) be the pilot?
