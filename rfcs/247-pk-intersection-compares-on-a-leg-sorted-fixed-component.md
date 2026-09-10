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
   omittable), the component must be in the comparison key. Reading a leg's
   binding fails CLOSED (`legFixedComparison`): a leg carrying more than one
   binding for the component, or a FIXED payload that is not a
   `*predicates.Comparison`, or a typed nil, reports "not fixed" and the
   component is compared. Only a nil payload is the implicit component; a
   payload of another type must not collapse into it, or two legs fixing a
   component to different constants would compare "nil == nil" and omit it —
   the bug this RFC closes, re-armed by a provider change (the `plans`
   package already stores a `*ComparisonRange` in its own FIXED bindings). Restricted to
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
   takes the requested direction or `FIXED`), a part the merged ordering
   binds FIXED is reset to FIXED (`widenedPartsTakeTheMergeDirection`): such a
   part is in the key only because the intersector widened it, and its
   requested direction is vacuous — every emitted row carries the one value,
   so the output satisfies any direction on it as it stands — while keeping
   the stamp makes `ORDER BY pk2 DESC, pk1` produce the mixed key
   `[pk1 ASC, pk2 DESC]` that no leg delivers and forfeits the merge. Then
   `AdjustFixedBindings` (`set_plan_helpers.go:27`,
   `RecordQuerySetPlan.adjustFixedBindings`: a FIXED part takes the
   comparison's direction — the executor side already exists) directs it
   with the merge, and the parts are turned into a `RequestedOrdering` and EVERY leg's
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
    `…ThreeWayBuildsTheWidestPartitionOnBothComponents`: its "only the A ∧ B
    pair", "2 children" and "scanC is inside no intersection" assertions
    flip to, measured: exactly ONE intersection, over all three legs
    (`scanA, scanB, scanC`), comparing on `(ID, VERSION)` — the redundancy
    pruning keeps the widest sound partition.
  * `TestPrimaryKeyComponentsToCompare_OmitsOnlyWhatEveryLegFixesAlike`
    drives the omission proof over hand-built leg orderings, one arm per
    branch of `legFixedComparison`: same comparison everywhere and nil
    (implicit) everywhere omit; different comparisons, a sorted leg, an
    unreadable (string) payload in one or every leg, two bindings in a leg,
    a typed nil comparison, and a leg whose ordering does not mention the
    component at all (the loop's fall-through) all compare.
  * `TestIntersector_WidenedPartIgnoresItsRequestedDirection`:
    `[VERSION DESC, ID ASC]` and `[ID ASC, VERSION DESC]` over forward legs
    build the forward merge on `(ID, VERSION)`; `[ID DESC, VERSION ASC]`
    over reverse legs builds the reverse one; every part carries the merge
    direction.
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
  the RFC-245 pin): the reproducer, its `ORDER BY pk1 DESC` twin and
  `ORDER BY pk2 DESC, pk1` are asserted PER QUERY to plan an intersection
  over exactly `TI_B_PK1, TI_PK2` in that leg order (the planner sorts
  candidates by name) comparing on `(PK1, PK2)`, forward or reverse as
  stated. `ORDER BY pk1 DESC, pk2` is a rows-only case: it is not merged,
  and not because of this RFC — every match picks its scan direction against
  the request on its own (`SatisfiesAnyRequestedOrderings`, the structure
  Java has), the `(b, pk1)` leg satisfies `[pk1 DESC, pk2 ASC]` in neither
  direction and arrives forward while `(pk2)` arrives reversed, and the
  reversed `(pk2)` scan with a residual is the right plan. The unit arm that
  merges `[ID DESC, VERSION ASC]` over two REVERSE legs states what the
  intersector does when both legs are reversed, which that rule does not
  produce for this request; the three-way `a = 1 AND b = 1 AND pk2 = 3`
  over `TI_A, TI_B_PK1, TI_PK2` and `a = 1 AND pk2 = 3` over `TI_A, TI_PK2`
  likewise. Measured at the implementation: the cost model chooses every
  one of them (`Fetch(Intersection(IndexScan(TI_B_PK1, [=, *] COVERING),
  IndexScan(TI_PK2, [=] COVERING)))`, `REVERSE` on both legs and the merge
  for the DESC shapes). Row expectations unchanged.
* Cross-engine: the RFC-245 probe and corpus entry
  (`pk_intersection_leg_bound_component_count`, `DivergenceJavaWrongRowsGoCorrect`)
  keep asserting Java's wrong answer and Go's right one; Go's PLAN changes,
  its rows do not.
* `TestFDB_MetamorphicCompositePrimaryKey` and its DML twin (the net that found
  RFC-245's defect) stay green with their floors.
* Mutations, each taken with the mutated text `grep -c`'d present (1) in
  the same invocation and absent (0) after restoring, over the 29
  `TestIntersector_*` / `TestPrimaryKeyComponentsToCompare_*` top-level
  tests: the directed gate disabled (`if false && !everyLegDeliversComparisonKey`)
  reddens `ComparesOnTheComponentOneLegFixes`, both arms of
  `WidenedPartOrderingClaimIsVacuousByConstancy`, and
  `ThreeWayBuildsTheWidestPartitionOnBothComponents` — the three "exactly
  one" pins — and nothing else (the two decline arms and the
  different-constants arm stay green: their declines and their
  both-components assertion do not depend on the gate); the same-comparison
  clause disabled reddens exactly `ComparesOnTheComponentLegsFixToDifferentConstants`;
  the requested-direction reset disabled reddens exactly the three arms of
  `WidenedPartIgnoresItsRequestedDirection`.
* EXPLAIN corpus (`cmd/explain-differ`, the yamsql conformance corpus),
  `64a737edd` vs the implementation: 2955 entries, 2955 identical, 0 shape
  flips. Population: that corpus holds no composite-primary-key intersection
  whose legs fix a component unevenly — the `T_PKI` shapes live in the
  cross-engine `SeedRunCorpus` (`plandiff/corpus.go`), not in the EXPLAIN
  baseline — so this green says the directed gate declined none of the
  corpus's EXISTING primary-key intersections (every pk-intersection
  candidate now passes through it), not that it exercised the widening.
* Bazel-by-name at `558b44205`, `--nocache_test_results`: the 29
  `TestIntersector_*` / `TestPrimaryKeyComponentsToCompare_*` top-level tests
  in `//pkg/recordlayer/query/plan/cascades:cascades_test` all `--- PASS`;
  the three new `properties_test` pins pass; `TestFDB_PkIntersectionLegBoundComponent`,
  `TestFDB_MetamorphicCompositePrimaryKey` and its DML twin pass in
  `//pkg/relational/sqldriver:sqldriver_test`; `just test` 92/92 on both
  commits (pre-commit hook).
* Planner fuzz at `558b44205`, 30s each: `FuzzPlanner_Determinism`
  5,821,100 executions, PASS; `FuzzPlanner_PlanFullPipeline` 2,035,271
  executions, PASS.

### 1M stress comparison

Baseline `64a737edd` (the merge-base on 2026-09-10; `origin/master` was at
the same commit) versus `558b44205`, `TestFDB_Stress_1M` uncached, strictly
sequential, on the same filesystem (`/home`, 99% used, 11G free after
reclaiming the Go build cache), the changed files md5-checked after every
sequence. Every run has 24 `=== RUN` lines and 24 passes; all 22 labelled
readings agree on row counts across every run.

The first two sequences (base, branch, base, branch and then branch, base,
branch, base — baseline in a secondary worktree, the branch in the MAIN
worktree) read the branch 1.7–2.7x slower on the first five point reads in
all four of its runs, and the baseline slow in one of its four; the later,
identically planned `PK needle` read 1.01–1.03 throughout, plans were
identical on both sides (EXPLAIN lines in every log), and the committed
planner-only benchmark (`embedded/plan_stress_shapes_bench_test.go`, 3×200
per tree) agreed to within 1% on every shape (PK lookup 1.51 vs 1.55 ms,
idx_customer eq 2.23 vs 2.24, idx_amount range 2.35 vs 2.37, GROUP BY
1.72 vs 1.73, SUM 1.63 vs 1.64, IN-list 10.36 vs 10.38). That is not the
position artefact RFC-246 measured, so the code and the tree were separated
directly: the main worktree checked out at the BASELINE commit read
9.5 / 8.5 ms (fast), and then, with the CODE swapped across the trees —
branch commit in the secondary worktree, baseline in the main one,
interleaved — the branch read 8.3 / 9.6 ms and the baseline 11.3 / 8.5 ms.
What the runs show is that the branch code, moved to the other tree, is not
slow, while the baseline read 9.5 / 8.5 / 11.3 / 8.5 ms across its four
main-worktree runs — within-side variance, not a localised configuration;
why the four "main worktree at the branch commit" runs were 1.7–2.7x is not
established, only that the code is not what moved. Below is that swapped,
interleaved pair (load at each start 3.2 / 3.4 / 3.5 / 2.9), ratio =
min(branch) / min(base):

| query | rows | base | branch | ratio |
|---|---|---|---|---|
| PK lookup id=0 / N/2 / N-1 | 1 | 8.5 / 8.4 / 7.0 ms | 8.3 / 8.3 / 6.2 ms | 0.98 / 1.00 / 0.88 |
| idx_customer eq | 8 | 6.3 ms | 6.4 ms | 1.01 |
| idx_amount range >9000 | 100017 | 189.7 ms | 197.5 ms | 1.04 |
| idx_status count pending | 1 | 336.9 ms | 379.8 ms | 1.13 |
| full scan filter amount>5000 | 1 | 550.8 ms | 549.9 ms | 1.00 |
| GROUP BY status | 4 | 6.0 ms | 6.1 ms | 1.01 |
| GROUP BY status COUNT only | 4 | 5.3 ms | 5.5 ms | 1.03 |
| SUM by status (aggregate index) | 4 | 5.8 ms | 5.7 ms | 0.99 |
| GROUP BY customer HAVING | 47271 | 727.4 ms | 627.2 ms | 0.86 |
| JOIN 10 orders x customers | 10 | 29.9 ms | 28.4 ms | 0.95 |
| ORDER BY PK (full) | 1000000 | 3833 ms | 3890 ms | 1.01 |
| ORDER BY PK + index filter | 8 | 8.7 ms | 8.8 ms | 1.01 |
| scan all rows ordered / wide | 1000000 | 3631 / 3896 ms | 3672 / 3927 ms | 1.01 / 1.01 |
| IN-list 5 values | 46 | 18.9 ms | 19.3 ms | 1.02 |
| PK needle id=999999 | 1 | 5.8 ms | 5.8 ms | 1.01 |
| PK+filter needle id=500000 | 1 | 7.3 ms | 7.2 ms | 0.98 |
| full scan sparse filter | 97 | 3341 ms | 3328 ms | 1.00 |
| UPDATE by index / DELETE single row | 8 / 1 | 9.0 / 6.0 ms | 8.9 / 6.5 ms | 0.99 / 1.08 |

`idx_status count pending` at 1.13 and `GROUP BY customer HAVING` at 0.86 are
the two readings whose base runs differ from each other by more than the
sides differ (336.9 vs 376.3 ms; 727.4 vs 793.2 ms). The workload has a
single-component primary key, so no plan in it reaches the widened
enumeration or the directed gate (the gate runs only inside
`createPrimaryKeyIntersection`, which needs two matched accesses; every
stress query has one); this is a no-change confirmation.


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
three-way flips spelled out.

Implementation lap on `81cd73533`: Graefe ACK with conditions, Torvalds ACK
with conditions, codex one P2 — all folded. Graefe and Torvalds both found
the same defect, `legFixedComparison` failing OPEN (a discarded type-assertion
`ok` and `bindings[0]` under several bindings would read an unreadable
payload as the nil implicit component and omit a component two legs fix to
different constants — the bug this RFC closes, re-armed); it fails closed
now with an arm per branch. codex found that a widened part took its
REQUESTED direction, so `ORDER BY pk2 DESC, pk1` forfeited the merge on a
mixed key; the part now takes the merge's direction, pinned at unit and
SQL level. Also folded: the three-way test renamed to what it asserts
(one intersection, all three legs); the mutation claims replaced by the
measured reddened names with their populations; the EXPLAIN corpus stated
with its population; the FDB pin's stale "declines most of the TI shapes"
prose and its leg-order determinism cited; RFC-245's old symbol names
annotated. The stress and fuzz figures are recorded above.

Delta re-confirmation on the fold (`558b44205`): Graefe ACK; Torvalds ACK
with two conditions, folded — the absent-from-a-leg branch of
`legFixedComparison` was claimed in a comment and not driven (row added), and
the stress attribution sentence overstated what the swap showed (rewritten
to the readings). @claude on the PR.
