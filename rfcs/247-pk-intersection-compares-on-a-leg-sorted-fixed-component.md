# RFC-247: a primary-key intersection compares on every component its legs do not all fix alike

## Finding and reference

RFC-245 made `createPrimaryKeyIntersection` (`cascades/intersector_primary_key.go`)
prove its comparison key leg by leg: the key must contain every primary-key
component a leg does NOT itself fix, or "equal keys across the legs" does not
mean "the same record" and the merge returns wrong rows. Two things are wrong
with where that left the intersector, one on each side of the proof.

**It forfeits a sound plan.** In RFC-245's own reproducer — `PRIMARY KEY
(pk1, pk2)`, indexes `(b, pk1)` and `(pk2)`, `WHERE b = 1 AND pk2 = 3` — the
proof declines because the only key the merged ordering ever OFFERS is
`(pk1)`. Both legs are physically ordered by `(pk1, pk2)`: the `(b, pk1)` leg
by its key continuation, the `(pk2)` leg trivially, every row of it carrying
`pk2 = 3`. A merge on `(pk1, pk2)` aligns the two streams exactly and reads
only the rows both predicates admit; the surviving plan is one covering scan
with the other predicate as a residual. Why the key is never offered: the
legs' orderings are merged with Java's intersection combinator
(`combineBindingsForIntersection`, `properties/rich_ordering.go:1321`, a port
of `Ordering.java:1065`), and SORTED ∧ FIXED → FIXED. That is the right
answer for the intersection's OUTPUT ordering — every surviving row carries
the constant — but it erases that one leg SORTS the value, and
`EnumerateSatisfyingIntersectionComparisonKeyValues` (`:493`, a port of
`SetOperationsOrdering.enumerateSatisfyingComparisonKeyValues`,
`Ordering.java:1337`) then filters `pk2` out as a singular fixed value.

**It still admits an unsound one.** The proof reads "fixed in every leg" as
"omittable", and never asks whether the legs fix the component to the SAME
value. Measured at `64a737edd` with the RFC-245 unit harness: legs
`(A, VERSION, ID)` fixing `VERSION = 1` and `(B, VERSION, ID)` fixing
`VERSION = 2` build an intersection comparing on `(ID)` alone. Records
`(id = 1, version = 1)` from A and `(id = 1, version = 2)` from B align on
that key and the cursor emits one, for a conjunction whose true result is
empty. Surfaced by the RFC lap on this RFC; RFC-245's arm for "every leg fixes
the component" used one literal for both legs and could not see it.

Both are the same missing clause. The premise the merge cursor rests on is
"equal comparison keys across the legs ⇒ the same record", and for a
primary-key component OMITTED from the key that holds only when every leg
fixes it AND every leg fixes it to the same comparison. A component one leg
sorts is not constant there; a component the legs fix to different constants
is constant in each but differs between them. Either way it must be compared.

## Decision

Keep the merged ordering and its algebra exactly as they are — the OUTPUT
ordering an intersection advertises, `GetEqualityBoundValues` (which feeds
`ImplementSortRule` and the redundancy pruning at
`intersector_primary_key.go:357`), `normalizeOrderingDependencies`, and every
consumer of `MergeOrderingsForIntersection` are untouched. The change is
confined to how the intersector computes what MUST be compared, offers it,
and proves the offer:

1. **`mustCompare`, computed by the intersector from the legs' own
   orderings** (the same `adjustedIntersectionOrdering` results it already
   walks at `:624-652` and must retain for step 4). For each primary-key
   component: if some leg does not bind it FIXED, or the legs' fixed
   comparisons are not all `comparisonsEqual` (`partial_match_identity.go:203`,
   the semantic equality the partial-match identity already uses; `nil`
   equals `nil`, which is how the implicit record-type component stays
   omittable), the component must be in the comparison key. Restricted to
   `pkValues` deliberately: the primary key is what identifies a record, and
   a non-key column one leg sorts and another fixes adds nothing to the proof
   while adding a factor to the enumeration.

2. **Offer it.** `EnumerateSatisfyingIntersectionComparisonKeyValues` takes
   the set as a second argument — there is ONE function, whose empty-set case
   is today's behaviour, not a second enumeration beside it. Its element
   filter becomes: singular-non-fixed in the merged ordering (Java's filter)
   OR a member of `mustCompare`. The requested-ordering reduction is
   unchanged: a requested part the merged ordering binds FIXED is compatible
   with any direction and is dropped from the required prefix, as Java does.
   `SatisfyingPermutations` runs over the merged poset as before. A widened
   value carries no edges there (`normalizeOrderingDependencies:1256` strips
   a fixed value's own edges and bypasses it as a dependency), so for the
   reproducer this offers BOTH `(pk1, pk2)` and `(pk2, pk1)`. The
   enumeration is deliberately not made cleverer: the merged set cannot
   carry per-leg facts once a value is fixed, and a second dependency
   structure invented here would be one more thing to keep in step with the
   legs. Bound: the widened elements are at most the primary-key components,
   so a partition enumerates at most `|pk|!` candidates beyond today's, each
   rejected in `O(legs × |key|)`; the primary key's width is a schema
   constant, and the requested prefix, when there is one, fixes the leading
   positions.

3. **Prove the key identifies a record in every leg** — RFC-245's
   `comparisonKeyIdentifiesRecordInEveryLeg`, now stated as its real
   invariant: the key contains every `mustCompare` component. The per-leg
   free-component form it replaces was this minus the same-comparison clause.

4. **Prove the DIRECTED key against every leg's own ordering.** After
   `DirectionalOrderingParts` (`rich_ordering.go:650`: a merged-FIXED part
   takes the requested direction or `FIXED`) and `AdjustFixedBindings`
   (`set_plan_helpers.go:27`, `RecordQuerySetPlan.adjustFixedBindings`: a
   FIXED part takes the comparison's direction — the executor side already
   exists), the parts are turned into a `RequestedOrdering` and EVERY leg's
   adjusted ordering must `Satisfies` it (`rich_ordering.go:287`). This is
   the gate that makes step 2 sound rather than optimistic, and it is the
   same predicate sort elision rests on. It works through `MapAll`'s cascade
   (`combinatorics/partially_ordered_set.go:252`, Java's
   `PartiallyOrderedSet.mapAll`): filtering a leg's poset to the requested
   values removes an element once ALL of its dependencies are gone, so a
   leg that orders `pk2` only within `pk1` refuses `[pk2, pk1]` (the
   requested order contradicts the surviving edge), and a leg that orders
   `pk2` only within some value the key omits loses `pk2` and refuses the
   key outright. For the reproducer the `(b, pk1)` leg satisfies
   `[pk1 ASC, pk2 ASC]` and refuses `[pk2 ASC, pk1 ASC]`; the `(pk2)` leg
   satisfies either, `pk2` being FIXED there. Exactly one key survives. A leg
   scanned the other way, or a request that directs `pk2` against the way a
   leg sorts it, fails the same gate. An asserted bridge, never a silent
   fallback.

   **Precondition, stated because the gate stands on it:** a leg ordering
   built by `NewRichOrdering` chains ONLY its non-fixed keys
   (`rich_ordering.go:71-84`) — a fixed key has no edges and nothing depends
   on it. That is why filtering the `(b, pk1)` leg to `{pk1, pk2}` does not
   cascade through the fixed `b` to an empty set. Were fixed keys ever
   chained, the gate would refuse this RFC's own reproducer and the widening
   would no-op with every existing test green; it is pinned as such
   (`TestNewRichOrdering_FixedKeysCarryNoEdges`, whose failure message names
   this gate).

Everything after that is the existing path: `NaturalComparisonKeyValues`,
`plainFieldComparisonParts`, baking against each scan's row (the `(pk2)`
scan's row carries `pk1` as its PK suffix, the `(b, pk1)` scan's row carries
`pk2`), canonicalisation, `compensateIntersection`.

**The intersection plan's advertised ordering.**
`RecordQueryIntersectionPlan.HintOrdering` (`plans/ordering.go:1063`) claims
the comparison key with each part's direction and NULLS placement. For a
widened part that claim is vacuous by constancy: the value is FIXED in the
merged ordering because at least one leg fixes it, so every row the
intersection emits carries that one value (the same fact `commonOrdering`
states, and stronger than Java's `applyComparisonKey`, `Ordering.java:1399`,
would produce), and any direction or NULLS placement is trivially satisfied —
including a leg that fixes it by `IS NULL`
(`properties/physical_equality_shape.go:332`), whose one value is NULL. Pinned
by `TestIntersector_WidenedPartOrderingClaimIsVacuousByConstancy`.

**Why not change the combinator instead.** Keeping `[SORTED, FIXED]` as two
bindings would make the merged value read as SORTED to every consumer
(`SortOrderOf:219` returns the one sorted direction), so an ORDER BY on it
would no longer be discounted as equality-bound, and every intersection
ordering in the planner would change shape. The output ordering is not wrong
today; only the comparison-key offer and the omission proof are. Confining the
change to the intersector keeps Java's algebra intact and the blast radius at
one rule; `intersector_primary_key.go:673` is the enumeration's only
production caller.

**Java, and what is not being ported.** Java does not build the sound plan;
it builds the unsound one (`isCompatibleComparisonKey` subtracts the UNION of
equality-bound values — RFC-245, TODO.md section 9). Two Java hooks look like
they might express this and do not. `promoteToDirectional()`
(`Ordering.java:1358, 1386`) is `false` for `Intersection` (`:1539`) and is
gated on `bindings.size() > 1`; `combineBindingsForIntersection` collapses
SORTED ∧ FIXED to the single fixed binding, so the fact never reaches a
binding-count test, and flipping the hook would promote a different
population (multi-fixed values) without reaching this one. `applyComparisonKey`
(`:1399`) derives the set operation's OUTPUT ordering from a chosen key; it
does not choose one. Neither is ported. Nothing here touches the wire; Go
expresses a merge Java cannot express soundly, over the same index entries
Java reads.

Out of scope, stated so it is not read as covered: the union path (its
combinator is conjunctive and its comparison key requires the whole primary
key with no equality subtraction — RFC-245's review established that
asymmetry) and the multi-aggregate intersection (`rule_aggregate_data_access.go`
builds its own comparison key over grouping values and never reaches this
enumeration).

## Verification

* Unit (`cascades/intersector_leg_bound_pk_test.go`):
  * `TestIntersector_DeclinesPrimaryKeyComponentFixedInOneLegOnly` becomes
    `…ComparesOnTheComponentOneLegFixes`: legs `(A, ID, VERSION)` and
    `(VERSION, ID)` yield EXACTLY ONE intersection, comparing on
    `(ID, VERSION)` in that order. "Exactly one, in that order" is the pin
    of the directed gate: with the gate removed (mutated in place) the
    enumeration also yields `(VERSION, ID)`, a second intersection is built
    on a key the `(A, ID, VERSION)` leg does not deliver, and the arm reddens.
  * `…AcceptsPrimaryKeyComponentFixedInEveryLeg` still compares on `(ID)`
    alone — both legs fix VERSION to the same literal.
  * New `…ComparesOnTheComponentLegsFixToDifferentConstants`: the RFC lap's
    reproducer above yields exactly one intersection on `(ID, VERSION)`;
    red at `64a737edd` (built on `(ID)`).
  * `…ThreeWayKeepsSoundPairDropsLegFixingPkComponent` becomes
    `…ThreeWayComparesOnBothComponentsInEveryPartition`: its "only the A ∧ B
    pair", "2 children" and "scanC is inside no intersection" assertions
    flip to: every intersection built compares on `(ID, VERSION)`, the
    partitions built are exactly those the redundancy pruning admits
    (measured and stated in the test), and `scanC` appears in at least one.
  * New arms: a leg whose key is `(VERSION, X, ID)` with only VERSION bound —
    ID ordered only within X, which the other leg cannot compare — yields no
    intersection (the `(VERSION)` offer fails step 3, `(VERSION, ID)` fails
    step 4 in that leg, and X is not offered); a reverse-scanned
    `(VERSION, ID)` leg against a forward `(A, ID, VERSION)` leg yields none
    (ID's directions disagree, so it never enters the merged ordering and
    step 3 declines the `(VERSION)` offer).
  * `TestIntersector_WidenedPartOrderingClaimIsVacuousByConstancy`: the
    built plan's `HintOrdering` claims `[ID ASC, VERSION ASC]`, and its
    `commonOrdering` binds VERSION FIXED; with the fixing leg bound by an
    `IS NULL` range the same holds.
* Unit (`properties/rich_ordering_test.go`):
  * the enumeration with an empty `mustCompare` equals the enumeration at
    `64a737edd` element for element over the existing cases; with a merged
    FIXED value in `mustCompare` it offers that value in both positions.
  * `TestNewRichOrdering_FixedKeysCarryNoEdges`: a fixed key between two
    sorted keys neither has nor receives an edge, and `Satisfies` over the
    two sorted keys alone holds; the failure message names the step-4 gate.
  * `TestRichOrdering_SatisfiesDropsAValueOnlyWhenEveryDependencyIsGone`: a
    leg poset built with explicit multi-parent edges (`NewRichOrderingWithDeps`)
    keeps a value whose one remaining parent is requested and drops it when
    neither is — the cascade's actual rule, degree to zero, not "any parent
    removed".
* Plan shape + rows (`sqldriver/pk_intersection_leg_bound_component_fdb_test.go`,
  the RFC-245 pin): the reproducer and its `ORDER BY pk1 DESC` twin are
  asserted PER QUERY to plan an intersection over `TI_B_PK1` and `TI_PK2`
  comparing on `(PK1, PK2)` (reverse for the DESC twin) — a positive
  assertion, not the conditional property arm, which stays as is; row
  expectations unchanged. If the cost model does not choose the intersection
  the arm fails and the reason gets fixed in this change, not filed.
* Cross-engine: the RFC-245 probe and corpus entry
  (`pk_intersection_leg_bound_component_count`, `DivergenceJavaWrongRowsGoCorrect`)
  keep asserting Java's wrong answer and Go's right one; Go's PLAN changes,
  its rows do not.
* `TestFDB_MetamorphicCompositePrimaryKey` and its DML twin (the net that found
  RFC-245's defect) stay green with their floors.
* EXPLAIN corpus diff `64a737edd` vs the implementation; 1M stress comparison
  as for RFC-245/246; planner fuzz; every mutation claim with its
  mutation-present grep.

## Review

RFC lap: Graefe ACK with conditions, Torvalds ACK with conditions, both folded
above before implementation — the different-constants hole (measured, and
fixed here rather than filed), the intersector-computed `mustCompare` set
replacing a legs parameter on the ordering property, the `HintOrdering`
vacuity statement and pin, the Java disposal of `promoteToDirectional` /
`applyComparisonKey`, the widening restricted to primary-key components with
the enumeration bound stated, the fixed-key edge exclusion stated as the
gate's precondition and pinned, the `MapAll` sentence corrected to "all
dependencies removed" with a multi-parent pin, one enumeration function
rather than a survivor, the per-query positive plan assertion, and the
three-way flips spelled out. Implementation lap, codex and @claude recorded
below.
