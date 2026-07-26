# RFC-192: Make the join-ordering criterion a total preorder (CQ-22, CQ-23)

**Status:** Draft — revision 1. Requests a Graefe ruling on WHICH of three options to take, before any
implementation. No code changes proposed until that ruling is in.
**Area:** Cascades query engine — `PlanningCostModel.compareJoinOrdering`, `compareRecursiveCTE`,
`properties/cost_formulas.go` (`FlatMapCost`, `NestedLoopJoinCost`), `Reference.GetBest`
**Reviewers:** Graefe (the ruling: which option, and whether the Go-only materialized NLJ earns its keep),
Torvalds (code quality), codex, @claude

## The defect

`compareJoinOrdering` chooses its comparison metric from `joinShapesDiffer(planA, planB)` — a property of the
**pair**, not of either plan:

- shapes differ (materialized NLJ vs correlated FlatMap) → compare raw `Cost.CPU`
- shapes same (FlatMap-vs-FlatMap, NLJ-vs-NLJ) → fall through to `Cost.Less` (Total, then Cardinality)

A metric selected per-pair cannot be transitive in general, and here it is not. Two independent cycles are
reproduced against the real cost formulas:

    costA (FlatMap) CPU=36.5715 Total=36.9765
    costB (NLJ)     CPU=24.9885 Total=105.1785
    costC (FlatMap) CPU=15.795  Total=56.295
    compare(A,C) = -1   (same shape  -> Total)
    compare(A,B) =  1   (differ      -> CPU)
    compare(B,C) =  1   (differ      -> CPU)
    => A<C, B<A, C<B simultaneously

and a smaller one found by targeted search:

    NLJ(T0,T2) < FM(T0,T4) < FM(T1,T0), but FM(T1,T0) < NLJ(T0,T2)
    with leaf cardinalities T0=1, T1=2, T2=3, T4=8

This matters because `Reference.GetBest` (`expressions/reference.go:300`) elects the winning plan with a single
pairwise fold — `best := all[0]; for m in all[1:] { if less(m, best) { best = m } }`. Under a cycle the winner is
whatever `AllMembers()` happened to yield first. Measured: the three rotations of `{A,B,C}` elect `C`, `A`, and
`B` respectively. `GetBest`'s own doc comment states the precondition being violated: *"the comparator must be a
total order on Cost."*

No live SQL query is currently known to flip. Member insertion order is not SQL-controllable — it is an artifact
of rule firing order. **That is the hazard, not the reassurance:** adding or reordering a rule silently changes
the elected plan for unchanged SQL, with nothing in the query to indicate why.

`compareRecursiveCTE` (CQ-23) has the sibling defect on ties: `compare(DFS, Level) = -1` strictly, but an
unclassified candidate ties with both, so `DFS ~ Unclassified ~ Level` while `DFS < Level`. Indifference must be
transitive in a total preorder. It is called on every pair, so "unclassified" is the common case, and
`recursiveCTEKind` classifies only through single-child pass-throughs — a DFS or Level plan under a multi-child
node misclassifies as unclassified.

## Root cause

The pair-dependent switch is a **workaround for a deeper inconsistency**, and the existing code comment says so:
the two cost formulas disagree about the output cardinality of the same logical join.

    properties/cost_formulas.go:56   FlatMapCost.Cardinality        = outerCard * FilterSelectivity * mult
    properties/cost_formulas.go:86   NestedLoopJoinCost.Cardinality = outerCard * innerCard * FilterSelectivity * mult

Output cardinality is a property of the **logical group**: every physical implementation of one join must agree
on how many rows it produces. Because these do not, the cardinality term is an unfair discriminator across
shapes, so the code switches to CPU for cross-shape pairs — and pair-dependence is exactly what breaks
transitivity. The workaround and the defect are the same line.

## The obvious fix is measurably worse than the bug — do not repeat it

Unifying the formulas (give `FlatMapCost` the cross-product form) and deleting the `joinShapesDiffer` branch
**does** kill both cycles. It was implemented and measured. It is unsound, and the reason is structural:

**The two shapes are not costed from the same inputs.** `RewriteOuterJoinRule` pushes the join predicate INTO the
FlatMap's inner subtree as a concrete `PredicatesFilter`, so that predicate's selectivity is already baked into
`inner.Cardinality` before `FlatMapCost` sees it. `NestedLoopJoinCost` receives raw outer/inner and applies
`FilterSelectivity` itself, once, on the cross-product. Unified, FlatMap applies selectivity **twice** and is
systematically undercosted — precisely in the non-probe case.

Measured consequences of that attempt:

| Signal | Result |
|---|---|
| Test targets failing | 5 (`cascades`, `explaindiff`, `yamsql`, `embedded`, `sqldriver`) |
| Top-level tests failing | 10, incl. `TestRFC152_PreservedOnlyOnPredicate_MaterializedNLJ`, `TestRFC153_PreservedOnly_StillMaterializes` |
| Correctness | `TestFDB_ExistsInnerShadow` fails — "no frontier row resolved", an executor-level failure, not a plan-shape difference |
| Plan quality | `TestFDB_MultiwayJoinOrder_Nway` stops index-probing a 2000-row table |
| Corpus | 36/2633 plans change shape (32 NLJ→FlatMap, 3 lose an `InMemorySort`) |
| Real statistics | `flatmap_secondary_index#3`: two ordered index scans with no sort → full table scan + materialized sort |

Hand-computed on RFC-152's `A=1e6, B=1e6` preserved-only case: before, `Total_NLJ ≈ 7.29e10 < Total_FlatMap ≈
9.84e10` and NLJ correctly wins; after, `Total_FlatMap ≈ 2.624e11 < Total_NLJ ≈ 4.374e11` and FlatMap wins while
doing strictly more work (a full re-scan of B per A-row instead of materialising B once).

A supporting hypothesis was also refuted. It was assumed FlatMap inners are usually cardinality-1 probes, where
the formulas already agree and unification would be near-free. Measured on the plan-shape golden: **182/375
(48.5%) probe-shaped, 193/375 (51.5%) not** — 187 `PredicatesFilter`, 6 `DefaultOnEmpty`. The divergent case is
the majority.

## Java

Java does not have this problem and cannot: it has **no materialized nested-loop-join plan at all**.
`ImplementNestedLoopJoinRule.java` produces only `RecordQueryFlatMapPlan`, and `PlanningCostModel.java:277` gates
this criterion on `a instanceof RecordQueryFlatMapPlan && b instanceof RecordQueryFlatMapPlan`. Within that
single shape, one metric applies to every pair and the criterion is a total preorder by construction.

The materialized NLJ is a Go-only extension (RFC-152), added so a preserved-only LEFT JOIN predicate can
materialise the inner once instead of re-scanning it per outer row. The cost-model incoherence exists **only**
because Go compares two shapes Java never compares.

## Options — the ruling requested

**Option A — make the inputs comparable, keep both shapes.** Stop the double-count at its source: either
`NestedLoopJoinCost` does not apply `FilterSelectivity` when the predicate is already reflected in a child's
cost, or `FlatMapCost` detects a predicate the rewrite rule baked into its inner and does not re-apply it. Then a
single metric is fair for every pair and the pair-dependent branch is deleted.
*Cost:* requires the cost model to know WHERE a predicate is accounted, which it currently does not track. Likely
needs a marker on the cost or a structural check of the inner. Blast radius must be re-measured; it is not
obviously smaller than the failed attempt.

**Option B — Java-shape the criterion, decide cross-shape earlier by an intrinsic rung.** Gate this criterion on
both sides being FlatMap (exactly Java's condition), and rank NLJ-vs-FlatMap in a separate, earlier criterion
using an **intrinsic** per-plan rank rather than a pairwise metric — the pattern already used to fix
`compareInPlan` and `comparePrimaryScanVsIndexScan` in this branch.
*Cost:* requires an intrinsic rank that reproduces RFC-152's intent (materialise a non-probe inner; re-scan a
card-1 probe inner) without consulting the other plan. Whether such a rank exists is the open question — if the
decision genuinely needs both plans' costs, this collapses back to Option A.

**Option C — remove the Go-only materialized NLJ.** Restores exact Java shape; the criterion becomes
FlatMap-only and is a total preorder by construction, with no new machinery.
*Cost:* gives up RFC-152/153's win. Recorded here for completeness and because it is the only option that removes
the incoherence rather than managing it. **Not recommended without a ruling that RFC-152's benefit does not
justify a permanently pair-dependent cost comparison.**

The question for Graefe is not only which option, but the prior one: **does the Go-only materialized NLJ earn a
cost model that must compare two shapes with structurally different cost derivations?** Options A and B both pay
ongoing complexity to keep it. Option C says the extension is not worth that price. That is a design judgement,
not a measurement, and it gates the rest.

## Gating conditions on implementation (whichever option is ruled)

1. The inverted reproducer (`scratchpad/join_ordering_cycle_reproducer_INVERTED.go.keep`, asserts transitivity)
   must pass, and both recorded cycles must be gone.
2. `cost_model_total_preorder_test.go`'s exclusion entry for `compareJoinOrdering` must be REMOVED — its
   `assertKnownViolation` will fail once the criterion is fixed, which is by design.
3. Full corpus re-bless reviewed entry by entry, not bulk-accepted. Every changed plan justified as better or
   equal, with the reasoning recorded.
4. Real-statistics check: `flatmap_secondary_index#3` must not regress (it is invisible to the synthetic-stats
   corpus and caught the failed attempt).
5. RFC-152/153 preserved-only tests must pass without modification.
6. 1M stress comparison before/after, recorded in TODO.md.
7. CQ-23 (`compareRecursiveCTE`) fixed in the same pass — same file, same class, and the property suite covers
   both. Its exclusion entry must also be removed.

## What is already done

- Both cycles reproduced against real formulas, with the fold's rotation-dependent winners.
- The unification dead end implemented, measured, and reverted; findings recorded in TODO.md CQ-22 so the next
  attempt starts past it.
- A property suite (`cost_model_total_preorder_test.go`) enforcing reflexivity, antisymmetry, transitivity,
  indifference-transitivity and fold-stability across every drivable criterion, with a self-cleaning exclusion
  for the two known-broken ones. A future cyclic criterion fails a test rather than waiting for a reviewer.
- Confirmed inert, pinned, no action needed: `typeFilterDepth`/`fetchDepth`/`distinctDepth` share CQ-23's
  abstention shape in isolation, but each sits directly behind the count rung that separates the case it would
  mishandle, so the bad tie is unreachable. Both the break and the ordering that neutralises it are pinned, so
  reordering those rungs fails a test.
