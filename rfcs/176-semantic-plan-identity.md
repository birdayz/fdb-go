# RFC-176: Semantic plan identity — tighten the vector-config equality arms, retire ExplainValue-string keying

**Status:** DRAFT — query-engine change; needs Graefe ACK on this RFC and on each
implementation PR, plus Torvalds/codex/@claude per house gauntlet.
**Origin:** PR #446 round 3. Migrating `RecordQueryProjectionPlan` identity to Java's semantic
model (option b of the review loop) was implemented, regressed two tests, was root-caused to a
PRE-EXISTING coarseness in the values-level equality arms, and was reverted with the blocker
documented at the revert site (`pkg/recordlayer/query/plan/plans/projection.go:62-81`,
"NOTE(Java alignment)"). This RFC is that follow-up: fix the root cause, then complete the
migration #446 had to abort.
**Cross-refs:** RFC-175 (registry; this executes the #446-flagged follow-up), RFC-173 (§6
sequencing — memo identity is planner-wide), RFC-037 (memo merges), RFC-045/SPFresh (the
vector extension whose config fields are at issue).
**Effort:** P1 ≈ 1 shift (local to `values/`), P2 ≈ 1-2 shifts (10 call sites + validation
gates), P3 trivial. Staged PRs, each independently green and gauntleted.

---

## 1. Problem

Go keys physical-plan identity on **explain strings**. Ten plan types implement
`EqualsWithoutChildren`/`HashCodeWithoutChildren` by comparing `values.ExplainValue`
renderings of their result/projection Values (enumerated by grep over
`pkg/recordlayer/query/plan/plans/`):

> comparator.go, default_on_empty.go, first_or_default.go, map.go, merge_sort_union.go,
> multi_intersection.go, projection.go, sort.go, streaming_aggregation.go, values.go

Java never does this. Java keys plan identity **semantically**:
`RecordQueryMapPlan.equalsWithoutChildren` is class-equality + `semanticEqualsForResults`
(RecordQueryMapPlan.java:157-166); `planHash` folds `getResultValue()` as a `PlanHashable`
(:195-203), where `FieldValue`'s hash includes the `FieldPath` whose `ResolvedAccessor`
identity is the ordinal.

String-keyed identity has a structural defect, demonstrated twice in one PR: it is sound only
if the rendering is **injective over every semantic discriminator any Value will ever carry**.
PR #446 needed two consecutive rounds on exactly this: round 3 (ordinal reads rendered
identically → memo unified projections reading different slots) and round 4 (`#`-discriminator
collided with legal quoted identifiers → escaping). Every future Value attribute must remember
to join the rendering, forever — identity-by-rendering is a standing whack-a-mole. Java's
model has no such coupling: rendering is for humans, identity is structural.

## 2. Why the migration was reverted in #446 — the real blocker

`values.EqualsWithoutChildren` (`map_field_values.go:251`) — the Go port of Java's
`Value.equalsWithoutChildren()` — has **type-assertion-only arms** for the windowed/vector
family (`map_field_values.go:465-488`): `DistanceRowNumberValue`, its four metric
specialisations, `RowNumberValue`, `RowNumberHighOrderValue`, `RankValue` all compare as
`_, ok := b.(*T); return ok`.

Grounded against the structs, the arms split into two groups:

- **Already Java-consistent (no change needed):** `RankValue{WindowedValue}` and the four
  metric specialisations (`CosineDistanceRowNumberValue{WindowedValue}` etc.) carry **no
  non-child attributes** — partitioning/argument Values live on the embedded `WindowedValue`
  (`value_windowed.go:29-32`) and are compared as children. A class check is exactly Java's
  `WindowedValue.equalsWithoutChildren` = super(class) + `getName()`
  (WindowedValue.java:166-169); in Go the concrete type IS the name.
- **Defective (the blocker):** three types carry Go-only vector/HNSW config that never joined
  identity —
  | Type | Ignored non-child attributes |
  |---|---|
  | `DistanceRowNumberValue` (value_distance_row_number.go:35) | `Metric DistanceOperator`, `EfSearch *int`, `IsReturningVectors *bool` |
  | `RowNumberValue` (value_row_number.go:38) | `EfSearch *int`, `IsReturningVectors *bool` |
  | `RowNumberHighOrderValue` (value_row_number_high_order.go:30) | `EfSearch *int`, `IsReturningVectors *bool` |

  These fields are a Go read-side extension (Java 4.12 has no vector search), so there is no
  Java arm to copy — but Java's **principle** is unambiguous: every non-child attribute joins
  `equalsWithoutChildren` (`PromoteValue` compares its target type, `ParameterValue` its
  ordinal+name, Go's own `CastValue`/`ScalarFunctionValue` arms already follow it). The vector
  config fields were bolted onto the structs without joining identity.

The coarseness is **live**, not theoretical. #446's aborted migration made plan identity
exactly as coarse as these arms, and two tests caught real winner changes:
`TestVectorPlan_TighterOuterLimitDoesNotFold` (SinkLimit's fold/decline vector-scan
alternatives memo-merged → wrong winner extracted: `rank<=10` scan where ordered-scan +
`Limit(3)` was required) and `TestFDB_RFC130_RecursiveCTE_NoDoubleCharge` (plan shift pushed
temp-table residency past the budget). Today those alternatives stay distinct **only because
their explain strings happen to differ** — identity soundness by rendering accident.

The hash side is coarse in lockstep: `writeSemanticHash` (semantic_hash.go) has no arms for
this family, so all of them fall into the generic `"v:"+v.Name()` bucket (:146). Equality and
hash are therefore *consistently* coarse — equal⟹same-hash holds today and breaks only if one
side is tightened without the other. P1 tightens both in one commit.

## 3. Plan (staged PRs, each gauntleted)

- **P1 — tighten the three defective arms + hash, lockstep.** In
  `values.EqualsWithoutChildren`: `DistanceRowNumberValue` compares `Metric` + `EfSearch` +
  `IsReturningVectors`; `RowNumberValue` / `RowNumberHighOrderValue` compare `EfSearch` +
  `IsReturningVectors`. Pointer fields compare by nil-ness + pointee (nil ≠ &v; Java's
  optional-config semantics). Same discriminators join `writeSemanticHash` (explicit arms —
  not `SelfSemanticHash`, these types live in the values package). The Java-consistent arms
  (RankValue, metric variants) stay type-only, with a one-line comment saying why. Pins:
  per-type equality/hash unit tests (differ-by-one-field ≠, equal ⟹ same hash), and the two
  #446-regressed tests stay green (they must — P1 makes identity FINER, never coarser).
- **P2 — migrate the ten plan-identity sites to the semantic helpers.**
  `EqualsWithoutChildren` → `values.SemanticEqualsUnderAliasMap` (semantic_equals.go:17) over
  the plan's result/projection Values (Java's `semanticEqualsForResults`);
  `HashCodeWithoutChildren` → `values.SemanticHashCode`. All ten sites in one PR — a partial
  migration leaves two identity regimes in one memo. Re-run the #446 identity pins unchanged:
  `TestProjectionPlan_Identity_ResolvedOrdinal` and `_OrdinalVsLiteralHashField` must hold
  under the semantic model (ordinal is already in `FieldValue`'s equality arm,
  map_field_values.go:261-268). Validation gates, all mandatory: full suite; the 1M stress
  before/after comparison (CLAUDE.md planner-change workflow); the plandiff cross-engine
  corpus; `FuzzPlanner_*` + `FuzzCostSanity` runs; RFC-077 task-count baseline (memo-identity
  changes move member counts — a large swing is a red flag, not noise).
- **P3 — demote the `#`-escape to explain-format-only.** After P2 the rendering no longer
  carries identity. Keep the escape and its injectivity tests (`TestFieldValue_
  ExplainOrdinalEscape_RFC173`, plans-level collision pin) as explain-format pins — debugging
  output that collapses distinct reads is still a bug — but rewrite the projection.go:62-81
  NOTE to describe the semantic model and delete the "blocked by" paragraph. Trivial, can ride
  P2's PR as its final commit.

## 4. Non-goals

- **Winner-based child costing / restoring the cost-monotonicity pin** (RFC-175 §2 A2
  registered follow-up, `properties/cost.go`) — separate item; touches the cost model, not
  identity. Not bundled here.
- **C4 index-only gating end-state** (RFC-175 Track C) — own RFC, unchanged.
- Any change to `ProjectionColumnName` / `OutputColumnName` / Datum keys — the naming
  contract is untouched by all three phases (P1/P2 are identity-layer only).
- RFC-173 slices. See sequencing.

## 5. Sequencing vs the RFC-173 freeze

This is the #446-flagged follow-up and sits in the same keep-the-lights-on lane the owner
authorized for RFC-175 execution ("work on these follow ups", 2026-07-02). Interaction with
173 is real but bounded: 173's slices churn `FieldValue` and the executor row model — P1
touches neither (three arms + hash in `values/`, additive, finer-only). P2 changes memo
behavior planner-wide; its risk is bounded by the validation gates above, and doing it BEFORE
173's Slice 2/3 is strictly better — the wedge slices inherit a Java-shaped identity model
instead of building more on string keying (every new Value 173 adds would otherwise owe the
rendering another discriminator). If a 173 slice lands mid-P2, rebase and re-run the gates;
the corpus + stress diffs are the conflict detector.

## 6. Acceptance criteria

- **P1:** the three arms compare all listed fields (unit-pinned per field);
  `grep -A2 "case \*DistanceRowNumberValue" map_field_values.go` shows field comparisons, not
  bare type assertions; `writeSemanticHash` has explicit arms for the three types; equal ⟹
  same-hash property test over the family; `TestVectorPlan_TighterOuterLimitDoesNotFold` and
  `TestFDB_RFC130_RecursiveCTE_NoDoubleCharge` green.
- **P2:** zero `ExplainValue` calls inside any `EqualsWithoutChildren`/`HashCodeWithoutChildren`
  in `pkg/recordlayer/query/plan/plans/` (greppable); all ten sites use the semantic helpers;
  #446 identity pins green unchanged; stress-1M delta within noise (row counts identical,
  durations comparable per the TODO.md baseline table); plandiff corpus 0 new mismatches;
  RFC-077 task-count baseline delta explained or zero.
- **P3:** projection.go NOTE rewritten (no "blocked by" paragraph); `#`-escape tests retained
  and passing; no identity code path reads a rendering.

## 7. Review log

- (pending) Graefe — RFC + per-PR implementation ACKs.
- (pending) Torvalds, codex, @claude — RFC PR and each implementation PR.
