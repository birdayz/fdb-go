# RFC-249: a conjunction binds an index the way Java's SelectExpression binds it — flat conjuncts, one range per column

## Finding and reference

Three shapes, all wrong plans on correct rows, all one root cause. Schema:
`T(id PK, a, b, c, s, v)`, `idx_a(a)`, `idx_ab(a, b)`, `idx_c(c)`.

```
SELECT id FROM t WHERE a >= 2 AND a < 5
  Project(PredicatesFilter(IndexScan(IDX_A, [<>] COVERING), [1 preds]))
  scan comparisons: [0] >= 2            residual: a < 5        -- scans to the END of the index

SELECT id FROM t WHERE a = 1 AND b BETWEEN 2 AND 5
  scan comparisons: [0] = 1, [1] >= 2   residual: b <= 5       -- every BETWEEN is half-bounded

SELECT id FROM t WHERE a IN (1, 2) AND b = 3
  Project(PredicatesFilter(Scan(T), [1 preds]))                 -- full table scan
SELECT id FROM t WHERE id IN (1, 2) AND b > 5
  Project(PredicatesFilter(Scan(T), [1 preds]))                 -- full table scan for two PK probes
```

`a IN (1, 2)` alone plans `InJoin(IndexScan(IDX_A, [=]))`; `a = 1 AND b IN (5, 4)`
plans `InJoin(PredicatesFilter(IndexScan(IDX_AB, [=, *]), [b = $q]))` — the IN
binding as a residual FILTER, not the second scan column. So the IN-join is
reachable, but only when the IN happens to be alone in a filter, and the range
fold never happens at all.

Root cause, read from the Java side. Java's SQL layer produces
`SelectExpression`s, and that constructor normalizes the predicate list
(`SelectExpression.java:112-121` → `partitionPredicates`, `:682-725`):

1. `flattenPredicate(AndPredicate.class, …)` (`:770-793`) — every top-level
   AND is lifted into the list, so the list IS the conjunction.
2. `simplifyConjunction` (`:735-758`) — all comparisons on ONE value are
   folded through a `RangeConstraints.Builder` (`RangeConstraints.java:787-798`,
   admitting exactly the scan-prefix comparison types) into ONE sargable
   `PredicateWithValueAndRanges`.

At match time that sargable maps to the candidate's `Placeholder` with
`asMergedComparisonRange()` (`PredicateWithValueAndRanges.java:410-490`,
`RangeConstraints.java:161`), which is `ComparisonRange.mergeAll`
(`ComparisonRange.java:340-420`): a TOTAL merge whose `MergeResult` carries the
range PLUS a residual list. Equality wins; inequalities accumulate (deduplicated);
a second different equality, an inequality beside an equality, and every
`ScanComparisons.ComparisonType.NONE` comparison (NOT_EQUALS, IN, LIKE, TEXT_*)
come back as residuals, re-applied by compensation. `InComparisonToExplodeRule`
(`InComparisonToExplodeRule.java:124-129`) matches `selectExpression(some(IN
predicate), …)` over that flattened list.

Go diverges at every step of that chain:

* The translator wraps the whole WHERE as one element,
  `LogicalFilterExpression([AndPredicate])` (`cascades_translator.go:3458`), and
  neither `NewLogicalFilterExpression` nor `newSelectExpression` flattens. The
  match paths flatten for themselves (`flattenConjuncts` in
  `rule_match_intermediate.go:1460`, `partial_match.go:328`,
  `rule_partition_select.go:307`, `selectSubsumptionFlattenConjunctsMaybe`), so
  index matching works; `InComparisonToExplodeRule` (`rule_in_to_explode.go:84-96`)
  reads the top-level list only and never sees an IN under an AND. Measured:
  with a probe print in the rule, `a IN (1, 2) AND b = 3` shows the rule seeing
  exactly one predicate, the `AndPredicate`, and declining; `a = 1 AND b IN (5, 4)`
  reaches an IN-join only because index compensation over `IDX_AB [=, *]`
  happened to leave `b IN (5, 4)` ALONE in a residual filter the rule then
  exploded — which is also why that binding is a filter and not a scan column.
* `matchSingleSourceAgainstSelect` (`rule_match_intermediate.go:991-1049`) binds
  ONE query comparison per placeholder and `break`s; every other comparison on
  that column is a residual. The select-subsumption route
  (`select_subsumption_predicates.go:163`) has Java's per-candidate
  cross-product, which in Java chooses among DIFFERENT predicates implying one
  candidate; with Go's one-comparison-per-predicate model it chooses `a >= 2`
  OR `a < 5` for the placeholder, never both.
* `ComparisonRange.Merge` (`predicates/comparison_range.go:140`) is not total:
  `MergeResult{Ok, Residual}` with `Ok=false` for equality/inequality mixes, and
  no caller reads `Residual` (the standing `TODO.md` entry
  "ComparisonRange.MergeResult drops Java's residual LIST").
* The aggregate path already folds per column (RFC-248,
  `rule_aggregate_data_access.go:571-585`) but all-or-nothing: a column whose
  comparisons do not all merge binds nothing.

The executor is not the gap: `bindRangeTail` (`executor/scan_range_binding.go:938`)
intersects every inequality in a range, so a two-sided range is representable
today and simply never produced.

## Decision

Four parts, one mechanism each, all Java's.

**1. The predicate list is a flat conjunction, established at construction.**
`NewLogicalFilterExpression` and `newSelectExpression` lift nested
`AndPredicate`s the way `SelectExpression.partitionPredicates` does
(`flattenPredicate(AndPredicate.class)`: recursive at the top level; an AND
under an OR is left as the OR's child). `LogicalFilterExpression` is Go's
SQL-side stand-in for Java's `SelectExpression` — Java's own
`LogicalFilterExpression` is only ever built by compensation from an already-flat
list — so the invariant belongs on both Go constructors. The cost model is
unmoved: residual predicates are counted by CNF size (`normalFormSize` in
`countClassifiedResidualPredicates`), which is the same number for `[And(a, b)]`
and `[a, b]`. The ad-hoc `flattenConjuncts` at the match sites become no-ops on
constructor-built expressions and stay, because their inputs are not all
constructor-built (compensation predicate lists are assembled by hand).

Termination is unchanged by the invariant: the flattening is idempotent
(flatten∘flatten = flatten), `NormalizePredicatesRule`'s no-op guard is
`isInNormalForm` over the conjunction of the list — a flat list of leaves and
ORs IS in CNF, so a constructor-normalized Select does not re-fire it — and
`FilterMergeRule`/`FilterDedupPredicatesRule` yield strictly fewer expressions
or predicates. Memo identity follows the normalized list: a filter built from
`[And(a, b)]` and one built from `[a, b]` are the same member
(`EqualsWithoutChildren`/`HashCodeWithoutChildren` read the stored list), which
is pinned.

`InComparisonToExplodeRule` is then unchanged in shape and sees the IN as a
conjunct; the other conjuncts ride into the inner filter as `otherPreds`, where
`a = $explode AND b = 3` binds `IDX_AB [=, =]` and `ImplementInJoinRule` wraps
it. Nested INs explode one per firing on the memoized inner filter, as today.

**2. `ComparisonRange.Merge` becomes Java's total merge.** `MergeResult` is
`{Range, Residuals []*Comparison}`; `Ok` goes. Semantics, arm for arm from
`ComparisonRange.java:358-400`, driven by a THREE-way classifier
`scanRangeComparisonType(t) → equality | inequality | none` that replaces the
two-way `isScanRangeEqualityType` (Java's `ScanComparisons.getComparisonType`,
with Go's two documented deliberate differences kept: `NOT_DISTINCT_FROM` an
equality, `DISTANCE_RANK_*` inequalities — NOT none, so the malformed-tail
rejection in `bindScanComparisonsToRangeSet` keeps firing):

| state + incoming | result |
|---|---|
| any + none-type (NOT_EQUALS, IN, LIKE, TEXT_*, …) | range unchanged, incoming residual |
| Empty + X | from(X) |
| Equality + inequality | range unchanged, incoming residual |
| Equality + same equality (`comparisonsEqual`, the semantic-equality surface `partial_match_identity.go:203` already uses for ranges) | range unchanged, no residual |
| Equality + other equality | range unchanged, incoming residual |
| Inequality + inequality already present (`comparisonsEqual`) | range unchanged, no residual |
| Inequality + new inequality | appended |
| Inequality + equality | Equality(incoming); every accumulated inequality residual |

Java's `merge(ComparisonRange)` overload (`:403-420`) is ported as `MergeRange`
— the equality/empty/inequality-by-inequality fold with residuals
accumulated — and `mergeComparisonRanges` (`match_info_merge.go:200-262`), the
CROSS-quantifier merge the TODO entry "ComparisonRange.MergeResult drops
Java's residual LIST" is about, calls it and keeps failing closed on a
non-empty residual list; carrying residuals across quantifier boundaries is
that entry's remaining scope and is not done here.

Every non-test reader of `Ok` — eleven, counted by
`grep -rn "\.Ok\b" pkg --include=*.go | grep -v _test` restricted to
`MergeResult` receivers at `b6789c1a0` — reads `Complete()` (which is
`len(Residuals) == 0`, the one method on the result) where it read `Ok`, and the NONE arm (today Empty + NOT_EQUALS is an INEQUALITY range;
under Java it is a residual) moves no plan at any of them, site by site:

* `rule_match_intermediate.go:1304` (`bindOrientedComparison`) and
  `select_subsumption_predicates.go:616` — pre-filtered by
  `isSargableComparisonForMatch`, which admits scan-range types, IS [NOT]
  NULL and DISTANCE_RANK only: never a none-type.
* `rule_aggregate_data_access.go:579` — pre-filtered by RFC-248's admission
  set (`groupColComparisonIndex`), the value-index range set plus the NULL
  comparisons.
* `rule_implement_nested_loop_join.go:4364` — builds an EQUALS.
* `rule_implement_nested_loop_join.go:2069` and `:4037` — REBUILD an existing
  range comparison by comparison; a range only ever holds what a prior
  `Merge` admitted, and DISTANCE_RANK stays an inequality, so no none-type
  is ever rebuilt. Their error/decline arms are unreachable from the type
  change and stay as written.
* `planning_cost_model.go:2660` and `plans/cost.go:778` — merge `eqColumns`
  entries, collected with `Type == ComparisonEquals`.
* `properties/rich_ordering.go:1087` — merges a translated EQUALITY
  comparison (`comparison.GetEqualityComparison()`).
* `match_info_merge.go:257` — fail-closed as above.
* `predicates/range_constraints.go:139` (`AsComparisonRange`) — no non-test
  caller; keeps returning `(nil, false)` on a non-empty residual list, and its
  comment, which asserts "Go's MergeResult carries no residual list", is
  rewritten — as are `match_info_merge.go:218-231` and the TODO entry's "no
  caller in the tree reads Residual at all", with the zero-grep for the
  superseded phrasing reported in the implementation.

**3. Every comparison bound to one placeholder folds into one range.** One
helper, `foldPlaceholderBindings`, used by all three binding sites:

```
foldPlaceholderBindings(bound []placeholderBinding) (merged *ComparisonRange, members []placeholderBinding)
```

Deterministic in predicate order, which is Java's order too (`RangeConstraints`
keeps insertion order and `mergeAll` walks it): the bindings are merged left
to right with the total merge above; a binding whose comparison comes back as
a residual is a non-member, and when an equality displaces accumulated
inequalities those earlier members become non-members. The end state is
Java's: if any binding is an equality, the range is the FIRST equality and
every other binding is a non-member (a duplicate of it is a member — `a = 1
AND a = 1` binds once and re-applies nothing); otherwise the range carries
every distinct inequality. The fold is UNCONDITIONAL — it never consults the
candidate mid-fold; what the candidate can execute is decided once, below,
over the whole range. Non-members are exactly Java's residual list and flow through the
existing residual mapping (re-applied as a filter above the scan); members
each get a sargable mapping on the placeholder's alias with the SAME merged
range, so `tryMergeParameterBindings` sees equal ranges for the alias and the
prefix map carries the fold.

Whether the folded range can be EXECUTED at that coordinate is decided where
it is decided today, once, for the whole range: the candidate's
`ComputeBoundParameterPrefixMap` (physical key type known, STARTS_WITH alone —
`bindRangeTail` executes a lone STARTS_WITH only — no known-constant NaN,
positional truncation) and `candidateBindingRangesEligible`. A fold group the
candidate declines is demoted whole, every member to residual, by the
reconciliation that already exists after the placeholder loop. No second
gate is added and nothing is factored out of the prefix maps. The one shape
this changes is `s STARTS_WITH 'x' AND s > 'a'`: today the first-bound
STARTS_WITH is a lone scan bound and `s > 'a'` a residual; folded, the range
holds both, the prefix map declines it and both are residuals over an
unbounded scan. Java folds the same way and its `InequalityRangeCombiner`
throws `Unexpected inequality comparison` at execution
(`ScanComparisons.java:598-648`), so a bound-plus-residual plan for that shape
is not a Java plan either; and it is unreachable from SQL — `ComparisonStartsWith`
has ZERO producers under `pkg/relational` (`grep -rn ComparisonStartsWith
pkg/relational --include=*.go`, positive control `ComparisonEquals` in the
same tree), the only producer being the record-layer index-predicate path.
The vector candidate's eligibility check runs over the RAW bindings by design
(a NaN partition equality must decline the candidate, not become a residual
— `vector_index_match_candidate.go:254-278`), so on a vector partition column
the same folded shape declines the CANDIDATE; a unit arm pins that outcome
so a future LIKE→STARTS_WITH conversion re-decides it deliberately.

One invariant bends to carry this, and it is stated rather than discovered:
`PredicateMultiMap.Builder.checkConflicts` (`PredicateMultiMap.java:738-751`,
Go `predicate_multi_map.go:684-696`) rejects a candidate predicate mapped by
more than one query predicate. Java never trips it for a same-column
conjunction because the fold has already made those comparisons ONE query
predicate. Go's members are several query predicates mapping one placeholder,
so `checkConflicts` admits exactly that shape — every mapping of the
placeholder carries the same parameter alias and the same (pointer-identical)
merged range, i.e. the members of one fold — and rejects anything else as
before. The fold group IS Java's sargable, spelled over Go's leaves; two
unrelated query predicates claiming one placeholder is still a conflict.

Sites: (a) `matchSingleSourceAgainstSelect` — per placeholder, collect every
unmatched query comparison that `bindOrientedComparison` binds, fold, mark
the members matched; the reconciliation against `ComputeBoundParameterPrefixMap`
after the loop is unchanged and still owns positional truncation and the
whole-group demotion. (b) `enumerateSelectSubsumptionPredicateAlternatives` —
a group's alternatives become lists of mappings (`selectedMappings` is a list
per group): a placeholder group's sargable mappings fold into ONE alternative
(the members, rebuilt with the merged range); non-placeholder groups
enumerate one mapping per alternative as today. (c)
`partitionAggregatePredicates` — the per-column fold uses the helper: members
bind, non-members are residuals over the aggregate row (RFC-248's bucket 2),
replacing the all-or-nothing column.

**4. Three defects the flat conjunction surfaced, fixed in the same change.**

* *A dangling ON-clause existential.* `translateJoinWithExists` and
  `buildExistentialJoinSelect` append the join's `OnPredicate` — which carries
  the ON clause's `EXISTS(q$N)` marker — but attached existential quantifiers
  for the WHERE's subqueries only, so `JOIN c ON … AND EXISTS(d) WHERE
  EXISTS(e)` produced a Select whose `EXISTS(q$2)` named a quantifier no
  Select owned. On master the buried-existential backstop refused the query
  by accident (the ON predicate arrived as one AND, which reads as "buried");
  with the conjunction lifted the marker reached the planner, which found
  nothing to peel and DROPPED it — every row the ON-EXISTS should have
  excluded came back (`exists_in_on_probe_test.go` pinned the refusal, on data
  that could not tell a dropped EXISTS from an applied one). Both sites now
  attach the ON's existential quantifiers as `translateJoin` does, the query
  plans with one probe per EXISTS, and `CheckBuriedExistentialPredicate` has
  a second condition — a directly-handled existential must name a quantifier
  its expression owns — returning `DanglingExistentialPredicateError`
  otherwise, so the shape can never again reach the planner silently.
  Mutation-checked: with the attachment removed at both sites the guard
  refuses the query (`0AF00 … q$2`); with it present the plan carries two
  `FirstOrDefault` probes.

  The attachment covers the binary shape only; the same query over THREE
  legs was refused on master and on the branch alike (`0AF00 join did not
  ordinalize`), by the cluster gate's poison for an N-way join carrying its
  own existential, and a projected EXISTS over such a cluster reached the
  gathered branch, whose `gatherInnerClusterOnPredicates` returns NOTHING for
  a root carrying ON-EXISTS — the root's ON predicates dropped with it. The
  root cause is one spelling reaching two mechanisms: Java has no ON-EXISTS
  at all, because `QueryVisitor.visitSimpleTable` folds every inner-join ON
  conjunct into the WHERE of the single SelectExpression it builds, while Go
  carried an inner join's ON-EXISTS on the join (`OnExistsSubqueries`) and
  special-cased it in the translator — a gate arm for the bare 2-way, a
  projection-level lift for a FROM with no WHERE, a boundary in every cluster
  walk, and nothing for the shapes above. The fold now happens where Java
  does it, in the BUILDER: `embedded/on_exists_fold.go`'s
  `foldInnerOnExistsIntoWhere` is the last step of every query-block build
  (`visitSimpleTableBody`, `…_postBuild`), moving the ON's markers into the
  block's WHERE conjunction (FROM order first) and its subqueries onto the
  WHERE filter, synthesizing the filter directly above the join when the
  block has no WHERE. The join cluster is COPIED (the tree is shared with the
  generator's guards); the block's own filter and the shell above the join
  are the builder's and are updated in place. The walk is every INNER join
  reachable through INNER joins — an inner lateral unnest included: a comma
  unnest is a cross join with the outer row's array, so its outer's
  ON-EXISTS is the block's WHERE-EXISTS exactly as for any inner join, and
  Java's fold has no unnest boundary either. An OUTER join is a boundary
  where Java also stops (`collapseLeftSideOperators` wraps the pending side
  in its own select): each inner cluster under it is filtered IN PLACE,
  directly above itself — its rows filtered before the outer join preserves
  or null-extends them, which is exactly what the inner join's ON did — for
  the preserved and the null-supplying side alike (the WHERE would be wrong
  for a RIGHT or FULL join's null-supplying cluster: the null-extended rows
  must survive). The decision is made per join BEFORE recursing, so no
  child's lift is ever collected under an unlifted parent. It refuses, with
  the WHERE's own wording, the two shapes that cannot lift whole — an EXISTS
  under an OR (its marker would stay under the OR while its quantifier
  moved: the dangling shape again) and a subquery with no marker in
  conjunct position. A plan leaving the builder never carries an
  `OnExistsSubqueries`; the translator asserts that in `translateJoin`
  (`0AF00 … reached the translator unfolded`) and every translator consumer
  of the spelling is deleted: the gate arm and `scanFamilyLegCteAware`, the
  projection-level lift and its cardinality-known ON rejection, the
  ON-attach halves of both existential flattens, `translateJoin`'s ON-exists
  arm, and the `OnExistsSubqueries` boundaries in the cluster, chained-unnest,
  unnest-gather and clustered-scalar walks.

  The in-place filter is a leg shape the ordinal seed had never typed: a
  `Filter(inner cluster)` as an OUTER box's leg. `ordinalEligible` already
  admitted it (a filter is transparent to eligibility), but the seed's
  layout sites — `ordinalLegType`'s buried-leg bounds, `legBinding`'s
  `$BOX` mint, `legBakeWindow`, the buried bake windows of
  `gatedJoinLegTypes` / `ordinalJoinSeedFields`, `ordinalLegColumns` —
  classified a leg as a box only when the node IS a join, so the seed
  declared the leg one opaque run under its rightmost leaf while the leg's
  own select flowed the cluster's buried legs: two members of one reference
  disagreeing on their result type (`MemberResultTypeDisagreementError`,
  legs `[C 0 3]` vs `[A 0 1][C 1 2]`). `gatedLegBox` (the join beneath a
  leg's filters) is now the one classification at all seven sites, and the
  leg types exactly as the bare cluster did on master. Four former answers
  are kept (LEFT, LEFT+WHERE, RIGHT, FULL over an inner cluster with
  ON-EXISTS, rows identical to master), and five former rejections became
  answers, as in Java: two EXISTS in one ON, a cardinality-known EXISTS in
  ON (substituted like the WHERE's), every three-leg ON+WHERE shape, the
  cluster below a LEFT join followed by a further JOIN, and the cluster left
  of a lateral unnest. EXISTS under OR in ON is now refused by the builder
  with the WHERE's 0A000 instead of the backstop's 0AF00. Mutation-checked
  twice: with the builder fold disabled every ON-EXISTS arm in
  `TestFDB_ExistsInOn*` reddens through the translator's assertion — a
  refusal, never wrong rows; with `gatedLegBox` reverted to join-only,
  exactly the five outer-join arms of
  `TestFDB_ExistsInOnBelowOuterJoinAndBesideUnnest` and the seed unit pin
  redden while the unnest arms stay green.

* *Criterion #6 read the root only.* `inPlanPenaltyRank` penalises an
  IN-plan whose bindings never became search arguments, and it read
  `isInPlan(root)` as Java's `PlanningCostModel.compareInOperator` does. Java
  can, because its exploded Select is yielded into the SAME reference as the
  un-exploded one, so an InJoin meets its filter-only sibling at that
  reference's root. Go's per-ordering winner partitions let both survive the
  reference (the InJoin advertises the IN-binding order), so they meet again
  one level up as `Project(InJoin(Filter(Scan)))` against
  `Project(Filter(Scan))`, where the root-only read saw two Projects, and the
  "more IN-joins wins" rung below (Java's `numSourcesInJoin`) picked the
  IN-join: `v IN (1, 2)` over an UNINDEXED column already scanned the table
  once per IN element on master, and the flat conjunction would have extended
  that to `v IN (…) AND …`. The rung now reads the tree — every IN-plan node,
  each judged against the search arguments beneath it — which is the property
  Java's Javadoc states ("avoid plans generated out of an IN-transformation
  that wasn't able to translate the rewritten equality into a SARG") rather
  than the position Java's memo happens to let it read from. Eight corpus
  scenarios of that shape move from N scans to one; a SARGed IN-join wrapped
  the same way keeps its standing (`TestCostModel_WrappedUnsargedInPlanLosesToThePlainFilter`).
* *`extractInnerFilterPredicates` read every filter member of the GroupBy's
  inner reference*, including the compensation filters index matching leaves
  beside the base-row filter (`Filter([a > 0], IndexScan(IDX_A, [= 1]))` beside
  `Filter([a = 1, a > 0], Scan)`), so a conjunct was collected once per member.
  Harmless under RFC-248's all-or-nothing column, visible under the fold:
  `a = 1 AND a > 0 GROUP BY a` carried FOUR residuals above the aggregate scan
  and lost on cost to a full scan. Predicates are now collected once per
  distinct (`StructurallyEqual`) predicate per input alias; the union over
  equivalent members is one conjunction.

**5. Observed, not changed.** `derived_table_patterns_java.yaml#5` swaps its
join order under the flat list. Both orders cost the same under
`NestedLoopJoinCost` (symmetric in outer and inner: the materialized NLJ
reads each side once), so the winner is the memo's arrival order, which
follows predicate order — the baseline flips the same way when the SQL's
conjuncts are swapped. A tie broken by predicate order is a pre-existing
property of that formula, recorded in TODO.md, not something this RFC
touches.

**6. What this does NOT change.** `PredicateWithValueAndRanges` stays a
candidate-side/structural type; the query side keeps one comparison per
predicate. Java folds at Select construction into a sargable; Go folds at
placeholder-binding time with the same algebra (`ComparisonRange.merge`) and
produces the same scan comparisons and the same residual set. The difference
is where the fold lives, not what it computes.

## Rejected alternatives

* **Port `simplifyConjunction` and make `PredicateWithValueAndRanges` a live
  query predicate.** The literal port. It means every consumer of a query
  predicate learns a second comparison-bearing leaf: `bindOrientedComparison`,
  the subsumption binder, NLJ join-key detection (`flattenAndPredicates` and
  the equi-join walk read `ComparisonPredicate`), DNF/CNF normalization, the
  DeMorgan rule, `PushFilterThroughFetch`, compensation, `Eval` for the
  residual case (today a stub returning UNKNOWN), Explain, hashing. The fold
  at binding time reaches the identical scan comparisons with the existing
  leaf; the sargable type buys nothing the plans can see. Loses on blast
  radius with no plan gained.
* **Flatten only inside `InComparisonToExplodeRule`.** Fixes the IN-join and
  leaves the fourth ad-hoc flattener where Java has an invariant. A rule that
  cannot trust its expression's shape is the pattern that produced this bug.
* **Fold in the translator (`exactFilter`).** Puts an index-matching concern
  in the SQL layer and misses every filter the planner builds (compensation,
  explode inner filters, merged filters). Java's fold is in the expression
  constructor for the same reason.
* **Keep first-comparison-wins and have the executor tighten from the residual.**
  The executor cannot: a residual is a row filter, not a key bound; the scan
  has already read the tail.
* **Make the fold pick the tighter bound instead of carrying both.** Requires
  evaluating comparands at plan time; a parameter has no value then. Java
  carries both and lets `toTupleRange`/`bindRangeTail` intersect at execution.

## Verification

* Unit, `predicates/comparison_range_test.go`: the total merge, every row of
  the table above with the residual LIST asserted (which comparisons, not how
  many); none-type to residual; dedup on both equality and inequality through
  `comparisonsEqual`; `MergeRange` over Empty/Equality/Inequality operands.
  `match_info_merge_ranges_test.go` keeps its fail-closed pins over the new
  shape (`TestMergeComparisonRanges_EqualityInequalityRejectsUnlikeJava` stays,
  its message updated to say the residual list now exists and what remains is
  carrying it across quantifiers). `AsComparisonRange` keeps its `(nil,false)`
  pin on `x = 5 AND x > 7`.
* Unit, `cascades/fold_placeholder_bindings_test.go`: equality wins over
  inequalities in either order (both orders run, same range, same member
  set); first equality wins over a second, by list order; duplicate equality
  is a member; two inequalities merge and BOTH are in the range (the arm
  counts comparisons and names them); a duplicate inequality is a member and
  not appended; a none-type is a non-member; STARTS_WITH beside an inequality
  folds to a two-comparison range (the fold is unconditional) and the
  value-index prefix map then declines the alias; the same shape on a vector
  partition column makes `bindingRangesEligible` false. `checkConflicts`
  admits a fold group and still rejects two unrelated mappings of one
  placeholder, mutation-checked by removing the fold-group arm.
* Unit, `expressions`: `NewLogicalFilterExpression` and `newSelectExpression`
  flatten `[And(a, And(b, c))]` to three predicates, leave `Or(And(a, b), c)`
  alone, are idempotent, and `[And(a, b)]` / `[a, b]` build equal-and-same-hash
  members. `NormalizePredicatesRule` does not yield on a constructor-built
  flat conjunction (the fixpoint claim).
* Unit, `query/exists_guard_dangling_test.go`: a dangling `EXISTS(q)` and a
  dangling `NOT EXISTS(q)` are refused with the alias named; the same Select
  with the quantifier attached passes; a buried existential still reports
  `BuriedExistentialPredicateError`. Plan, `bug_hunt_cascades_test.go`
  (`…ExistsInOnBesideWhereExistsKeepsBothExistentials`): the plan carries two
  existential probes. Rows, `sqldriver/exists_in_on_probe_test.go`
  (`TestFDB_ExistsInOnPlusWhereExists`): on data where dropping either EXISTS
  changes the answer, the ON+WHERE form and the WHERE+WHERE form both return
  exactly `(2, 51)`; over three legs, the EXISTS in the root join's ON, in
  the nested join's ON, beside a plain WHERE, and with no WHERE at all each
  return exactly the rows the WHERE+WHERE control does; two EXISTS in one
  ON answer `(1, 50)`; a cardinality-known EXISTS in ON answers exactly as
  the WHERE spelling (`exists_over_aggregate_fdb_test.go`
  `join_on_known_false_substituted`); EXISTS under OR in ON and in WHERE
  fail with the same 0A000 and the same message. Unit,
  `embedded/on_exists_fold_test.go` (32 arms): the lift takes the root's, a
  nested join's and two EXISTS of one ON, in FROM order, keeps the other ON
  conjuncts, leaves an untyped nil when the ON was only the EXISTS, copies
  every node on the path and shares untouched legs, never lifts an OUTER
  join's own ON-EXISTS, filters an inner cluster under an OUTER join in
  place (nothing lifts PAST the outer join; the outer join and the root are
  copied with their ONs intact), lifts THROUGH an inner lateral unnest,
  refuses an EXISTS under OR and a subquery without a conjunct marker (and
  the refusal propagates through a clean parent), and is the identity with
  nothing to lift; the fold merges into the WHERE filter ahead of its own
  conjuncts and subqueries, synthesizes the filter under a shell and at the
  root, folds an OUTER root's legs in place with and without a WHERE (the
  WHERE takes nothing), leaves a text-only WHERE alone, touches nothing
  without a lift, and propagates the refusal; and, driven through the SQL
  builder (`TestOnExistsFold_NoJoinLeavesTheBuilderUnfolded`, 12 spellings
  incl. ORDER BY/LIMIT with no WHERE, below a LEFT join with and without a
  WHERE and at three legs, and left of a lateral unnest), no join leaves the
  builder carrying `OnExistsSubqueries` and the filter that took the lift
  carries every existential with its marker in conjunct position. Rows,
  `sqldriver/exists_in_on_probe_test.go`
  (`TestFDB_ExistsInOnBelowOuterJoinAndBesideUnnest`, 9 arms): LEFT (with
  the WHERE spelling as control), LEFT+WHERE, LEFT then JOIN, RIGHT and FULL
  (the null-supplying cluster: the row the WHERE spelling would drop is
  pinned present), and the cluster left of a lateral unnest with and without
  a WHERE (control agrees). Unit, `query/gated_leg_box_test.go`: a filter
  over a box is the box at every seed-layout site — same fields, same two
  buried legs, same `C$BOX` binding, same bake window, same ordinal columns,
  and the same buried bake windows under an OUTER box — and a scan, a
  filtered scan and a projection are not. Plan, `plan_harness_test.go`
  `join_on_known_false_substituted`: the ON and WHERE spellings of a
  cardinality-known EXISTS plan to the same tree.
* Unit, `cascades/in_plan_order_independence_test.go`: the wrapped
  unSARGed IN-plan ranks 1 and loses to the plain filter through the full
  chain with the "more IN-joins" rung pointing the other way; the wrapped
  SARGed IN-plan ranks 0 and keeps beating the plain filter; the 32-plan
  order-independence sweep (496 pairs, 29760 triples) still holds.
* Plan shape, `embedded/conjunction_binding_test.go` over the typed tree
  (`GetScanComparisons()`, residual count): `a >= 2 AND a < 5` → one range,
  two comparisons, no residual; `a > 2 AND a > 3` → two comparisons, no
  residual (the executor intersects); `a = 1 AND b BETWEEN 2 AND 5` → `[=, <>]`
  with two comparisons on `b`; `a > 2 AND a < 5 AND b = 3` → `[<>]` on `a`
  with two comparisons and `b = 3` residual (the fold does not extend the
  run past an inequality); `a = 1 AND a > 0` → `[=]` + 1 residual;
  `a = 1 AND a = 2` → `[=]` + 1 residual; `a = 1 AND a = 1` → `[=]` + 0
  residuals; `a IN (1, 2) AND v = 3` → InJoin over `IDX_A [=]` with residual
  `v = 3`; `a IN (1, 2) AND b = 3` → InJoin over `IDX_AB [=, =]`;
  `id IN (1, 2) AND b > 5` → InJoin over a PK scan; `a IN (1, 2) AND b IN (5, 4)`
  → nested IN-joins over `IDX_AB [=, =]`; `a IN (1, 2) AND a > 1` → equality
  wins, `a > 1` residual; `a = 1 AND b IN (5, 4)` now binds `b` as the second
  scan column; `v IN (1, 2)` over an unindexed column, alone, beside a
  conjunct and below an aggregate, is ONE filtered scan and no IN-join. Each
  arm asserts the scan comparisons by type and comparand, not the rendered
  `[<>]`.
* Rows, `sqldriver/conjunction_binding_fdb_test.go`: every plan arm above run
  against real FDB with an in-Go oracle over generated rows including NULLs;
  the two-sided range arms additionally assert the plan carries both bounds so
  a correct-rows-by-residual regression cannot pass. `bug_hunt_cascades_test.go`
  gets the IN-under-AND and two-sided-range arms as bug-hunt pins.
* Aggregate: `TestAggregatePredicatePartition` gains `a = 1 AND a > 0` →
  `[=]` bound, `a > 0` residual (was: nothing bound); the RFC-248 contradictory
  arm's expectation moves to "first equality bound, second residual", with
  the rows pin re-run.
* Gates: full `just test`; EXPLAIN corpus diff base `b6789c1a0` vs head:
  2955 entries, 127 differing, 58 shape flips, 0 plan regressions — every
  flip bucketed by its old/new plan text: 35 lose a residual filter (a
  two-sided range or a duplicate equality folded into the scan, with the
  Fetch that re-checked it), 9 IN-under-AND scenarios gain an IN-join or
  IN-union (`in_list_pushdown.yaml#23,25,34,35,39`, `in_expression_types#5`,
  `in_list_comprehensive#4`, `in_list_index_plan#5`, `in_plan_winner_stability#7`),
  4 IN-joins bind the IN as a scan column instead of a filter
  (`in_list_pushdown.yaml#24,27,28,31`), 9 unindexed-IN scenarios move from N
  scans to one (part 4), and 1 join-order swap (part 5); the other 69 entries
  are `[N preds]` counts on AND-carrying filters rendering the flat list, one
  of them a dedup (`index_range_predicates_java#6`, two identical ORs become
  one residual); the 9 yamsql `plan_contains`
  pins on the old shapes are moved to the new ones; `plan_shape.golden`
  re-dumped; factory corpus re-blessed (231 of 8150 scenarios across 36
  family files, plan shape only, ledger
  `retirements/2026-09-10-rfc249-conjunction-binds-one-range-per-column.json`);
  simfdb `orders.golden` (one PLAN line, zero rows); planner fuzz
  (`FuzzPlanner_Determinism`, `FuzzPlanner_PlanFullPipeline`, 30 s each);
  determinism ×10 on the new FDB tests; 1M stress base vs head, ≥2 per side,
  sequential.

### 1M stress comparison

Base `b6789c1a0` (the merge-base on 2026-09-10) in
`/home/birdy/projects/fdb-baseline-pki`; head is this branch's working tree
at the fold commit. Six sequential runs (3 base, then 3 head, never
concurrent), 23/23 `--- PASS` on every run, load average 1.7–3.8 at each
run's end. Per-run logs `/tmp/stress249-{base,head}{1,2,3}.log`.

| query | base min of 3 (`b6789c1a0`) | head min of 3 | ratio | samples base / head |
|---|---|---|---|---|
| pk_lookup_first | 0.04s | 0.06s | 1.50x | 0.06/0.04/0.06 / 0.06/0.06/0.06 |
| pk_lookup_middle | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.03 / 0.01/0.01/0.02 |
| pk_lookup_last | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.01 / 0.01/0.01/0.01 |
| index_customer_eq | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.03 / 0.01/0.01/0.02 |
| index_amount_range | 0.19s | 0.20s | 1.05x | 0.21/0.19/0.27 / 0.25/0.20/0.27 |
| index_status_count | 0.33s | 0.34s | 1.03x | 0.41/0.42/0.33 / 0.34/0.40/0.39 |
| full_scan_count | 3.02s | 2.97s | 0.98x | 3.02/3.03/3.06 / 2.99/2.97/2.97 |
| full_scan_filter | 0.59s | 0.70s | 1.19x | 0.59/0.76/0.71 / 0.72/0.75/0.70 |
| group_by_status | 0.02s | 0.01s | 0.50x | 0.04/0.02/0.03 / 0.02/0.03/0.01 |
| group_by_status_count_only | 0.02s | 0.01s | 0.50x | 0.02/0.02/0.02 / 0.03/0.02/0.01 |
| sum_by_status | 0.02s | 0.01s | 0.50x | 0.02/0.02/0.03 / 0.02/0.03/0.01 |
| group_by_customer_having | 0.59s | 0.58s | 0.98x | 0.76/0.59/0.64 / 0.62/0.62/0.58 |
| join_10_outer | 0.04s | 0.04s | 1.00x | 0.04/0.04/0.04 / 0.04/0.04/0.04 |
| order_by_pk_full | 3.80s | 3.81s | 1.00x | 3.80/3.83/3.81 / 3.86/3.81/3.82 |
| order_by_pk_index_filter | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.01 / 0.01/0.01/0.01 |
| scan_all_narrow | 3.62s | 3.63s | 1.00x | 3.64/3.65/3.62 / 3.64/3.66/3.63 |
| scan_all_wide | 3.87s | 3.86s | 1.00x | 3.88/3.90/3.87 / 3.89/3.89/3.86 |
| in_list | 0.02s | 0.02s | 1.00x | 0.02/0.02/0.02 / 0.02/0.02/0.02 |
| needle_in_haystack_pk | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.01 / 0.01/0.01/0.01 |
| needle_in_haystack_filter | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.01 / 0.01/0.01/0.01 |
| full_scan_sparse_filter | 3.28s | 3.29s | 1.00x | 3.29/3.30/3.28 / 3.32/3.30/3.29 |
| update_by_index | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.01 / 0.01/0.01/0.01 |
| delete_single_row | 0.01s | 0.01s | 1.00x | 0.01/0.01/0.01 / 0.01/0.01/0.01 |

Every timed query's plan is identical on both sides (the stress test EXPLAINs
its arms; `full_scan_filter` gained an EXPLAIN line in this change so its
plan — `StreamingAgg(IndexScan(IDX_AMOUNT, [<>] COVERING))` on both sides —
is on the record). The rows off 1.00x are all sub-100 ms point reads or
within the sample spread: `pk_lookup_first` is the first read after the
load, position-dependent 2x noise (0.04/0.06 on base vs 0.06 ×3 on head);
`full_scan_filter`'s 1.19x is base's single 0.59 s outlier against 0.70–0.76 s
on the other five samples — with n=2 per side it read 1.22x and motivated the
third pair; the three `group_by_status*` rows are 10 ms readings of aggregate
index probes. No planner or executor path this RFC touches is exercised by
the two 3-second full scans, and they moved by ≤ 0.02 s.

## Review

- **Graefe, RFC lap: ACK with 6 conditions**, all folded (the eleven `.Ok`
  readers enumerated with the per-site NONE-arm mapping, `MergeRange` ported,
  three-way `scanRangeComparisonType`, the mid-fold `carriable` gate and
  `BoundRangeCarriable` deleted in favour of the candidate's own prefix-map
  truncation, the fixpoint paragraph, the `a > 2 AND a < 5 AND b = 3` arm).
  Delta re-confirmation: ACK.
- **Torvalds, RFC lap: NAK** (three holes: the unenumerated `.Ok` readers,
  the vector candidate's raw-bindings path under a folded STARTS_WITH range,
  the `BoundRangeCarriable` factoring drifting from the fold). All three
  folded as above; the STARTS_WITH shape declines the candidate and is
  pinned. Delta re-confirmation: ACK.
- **Graefe, implementation lap: ACK with 2 conditions.** (1) The fold
  discriminated residual from displacement by receiver identity
  (`res.Range == merged`), which only the nil arm pinned; a defensive copy
  on the equality-vs-equality residual arm would have made the fold take the
  displacement branch, mark the second equality matched and never re-apply
  it — wrong rows, green suite. Folded: the fold reads the residual list
  (`len==1 && Residuals[0]==incoming` is "incoming is the residual", anything
  else is the displacement), and `TestComparisonRange_MergeIsTotal` now pins
  per arm both receiver identity (every residual and dedup arm hands the
  receiver back; the appends and the displacement do not) and that the
  residual list is one of exactly those two shapes. (2) The
  `inPlanPenaltyRankOfPlan` scope sentence named the fetch as a
  field-holder; it returns its inner as a child and is walked. Folded: the
  sentence names `RecordQueryCoveringIndexPlan` and
  `RecordQueryAggregateIndexPlan`, the two nil-children field-holders, both
  wrapping an index leaf. Delta re-confirmation: ACK (below).
- **Torvalds, implementation lap: NAK** (four bookkeeping defects). (1)
  `partitionAggregatePredicates`' docstring still said `a = 1 AND a = 2`
  "does not merge … both residuals", contradicted 37 lines later by the
  fold's own comment — rewritten. (2) The fetch sentence, as Graefe (2). (3)
  `flattenConjuncts` was dead at all five sites (every site reads
  `GetPredicates()` and both implementors lift in their constructors) and
  its "stays for a hand-built list" justification was false — the function
  and all five calls deleted; the same AND arm in
  `selectSubsumptionFlattenConjunctsMaybe` deleted and the function renamed
  `selectSubsumptionPredicatesWellFormedMaybe`, which is what it does (the
  typed-nil fail-closed preflight); the four call-site comments that said
  "flatten" now say the constructor lifts. (4) The `oneEach()` fallback in
  `selectSubsumptionGroupAlternatives` was an untested silent degradation on
  an unreachable shape — pinned by
  `TestSelectSubsumptionGroupAlternatives_FoldsOnlyWellFormedPlaceholderMappings`
  (two one-comparison mappings fold to one alternative over a shared
  two-comparison range; a mapping over a two-comparison range is not folded
  and comes back untouched one per alternative; non-placeholder groups give
  one per alternative). Delta re-confirmation: ACK ("the fold now tells
  'incoming comparison is the residual' from 'range displaced' by reading the
  residual list … `flattenConjuncts` is gone, a quoted grep over `pkg` finds 0
  hits for either old name against 6 for the renamed function"); two nits
  folded — the displacement arm now has a two-inequality case
  (`equality_into_two_inequalities_residualises_both_in_order`) and the
  non-placeholder group asserts each alternative's mapping by identity.
- **Graefe, implementation delta: ACK** — "Dedups and appends go through the
  `Complete()` arm, and a displacement can't put the incoming equality in its
  own residuals … I checked the one-line `return nil` form of `GetChildren`
  under `plans/` (11 non-test types): those two are the only ones that wrap
  another plan."
- **codex (`codex exec review --base master`): one P2** — the ON-EXISTS
  attachment of Decision part 4 covered the binary join only; a three-leg
  inner cluster with an ON-EXISTS beside a WHERE-EXISTS was still refused
  (codex read the refusal as `CheckBuriedExistentialPredicate`; measured, it
  is the cluster gate's N-way poison, and the projected-EXISTS gathered branch
  additionally dropped the root's ON predicates). Both master and the branch
  behaved identically on all six three-leg shapes probed, so not a regression
  — but a real reach gap in the very mechanism the RFC was fixing. Folded as
  the ON-EXISTS fold above, with the unit and FDB pins listed. The probe also
  surfaced a SEPARATE pre-existing gap — a projected EXISTS beside a
  WHERE-EXISTS fails opaquely (`Cascades planner could not plan query`) even
  on a single table, unrelated to joins or ON — booked in `TODO.md` with the
  reproducer; it is its own capability (the existential fold with two
  quantifiers) and needs its own RFC.
- **Graefe and Torvalds, second implementation delta (the translator-level
  ON-EXISTS fold): both NAK**, converging: the fold belonged in the builder
  (Java's `visitSimpleTable`), where it retires the inner ON-EXISTS spelling
  and every `OnExistsSubqueries` special case, instead of at two translator
  entry points that still missed a FROM with no WHERE under ORDER BY / LIMIT
  (Torvalds) and left the translateJoin ON-exists arm and the attach halves
  as dead dual mechanisms (Graefe); the lift's `return op` after recursing
  could collect a child's lift under an unlifted parent (both); an EXISTS
  under OR in ON lifted the subquery while its marker stayed under the OR
  (both); the cardinality-known ON rejection at the projection root still
  fired before the lift (Graefe); no row pin for a binary ON-EXISTS beside a
  plain WHERE (both). All folded as the builder fold above: boundary decided
  before recursing, EXISTS-under-OR and marker-less subqueries refused, the
  known-truth rejection deleted (the WHERE consumer substitutes), the binary
  ON+plain-WHERE spelling pinned in the builder-exit test and by
  `binary_on_exists_plus_where` rows. Delta re-confirmation: see below.
- **Graefe, Torvalds and codex, third delta (the builder fold): all three
  NAK on one finding** — the fold stopped at an OUTER join and at a lateral
  unnest, so an inner cluster with an ON-EXISTS below a LEFT/RIGHT/FULL join
  (which master answered, through the deleted translateJoin arm) or left of
  a lateral unnest reached the translator's assertion; Java folds at the
  outer-join operand too (`collapseLeftSideOperators`) and has no unnest
  boundary; no builder-exit arm spelled an outer join. Torvalds' minor: the
  RFC's "copies every node" was false for the block's filter and shell.
  Folded as the in-place outer-join fold, the transparent unnest, the seed's
  `gatedLegBox` classification, and the pins above; measured on master
  first, so the four kept answers are pinned to master's rows and the two
  new answers are named as gains. Delta re-confirmation: see below.
