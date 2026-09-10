# RFC-248: an aggregate index answers a grouping-column predicate it cannot bind as a residual, not a decline

## Finding and reference

`AggregateDataAccessRule` (`cascades/rule_aggregate_data_access.go`) can use an
aggregate index only when every filter predicate under the GroupBy is an
EQUALITY on the contiguous LEADING run of the index's grouping columns
(`aggInnerFilterFullyConsumable`). Anything else declines the index and the
query full-scans the base table into a streaming aggregation:

```
CREATE INDEX sum_abc AS SELECT SUM(v) FROM T GROUP BY a, b, c
SELECT a, b, c, SUM(v) FROM t WHERE b = 'x'             GROUP BY a, b, c   -- non-leading equality
SELECT a, b, c, SUM(v) FROM t WHERE a = 'x' AND c = 'z' GROUP BY a, b, c   -- gap in the prefix
SELECT a, b, c, SUM(v) FROM t WHERE a > 'm'             GROUP BY a, b, c   -- leading inequality
```

The first two are pinned as must-decline in `embedded/bug_hunt_cascades_test.go`
(`TestBugHunt_AggregateIndexMultiKeyResidual`) and the decline is CORRECT
today: the rule has no residual, so a predicate it cannot bind would be
dropped and the index would answer aggregates over the wrong groups (that
drop is the bug the guard was written against). RFC-246 fixed the neighbouring
gap — a conjunction of leading equalities was read whole and declined — and
surfaced this one.

Java does not decline any of the three. `AggregateDataAccessRule extends
AbstractDataAccessRule`, so the index rides the generic match + compensation
path: `AggregateIndexExpansionVisitor.constructSelectHaving` makes every
grouping value a `Placeholder`, a query predicate on ANY grouping column
matches, `AggregateIndexMatchCandidate.computeBoundParameterPrefixMap` turns
the leading run — equalities, then at most one inequality — into scan
comparisons, and every matched predicate the scan does not apply is
re-applied by `Compensation.applyAllNeededCompensations` as a predicate over
the candidate's top, the select-having row, where the grouping values are
visible. Only a predicate on the aggregation INPUT (a non-grouping column) is
impossible there, because the aggregate has already consumed those rows.

## Decision

Replace the consume-or-decline guard with a three-way PARTITION of the
flattened filter predicates, computed once per candidate by one function that
every scan site consumes:

1. **Scan bounds.** For each grouping column, every comparison on it whose
   other side reads no field (`groupColComparisonIndex`, the equality-only
   `groupColEqualityIndex` widened to the comparison types `ComparisonRange.
   Merge` accepts, keeping the `valueReadsField` gate that keeps `a = b` out
   of the scan) is FOLDED per column through `ComparisonRange.Merge`. Today's
   `buildAggScanPrefix` does not do that: it keeps the FIRST comparison on a
   column and drops the rest (`first equality on a column wins`), which is
   harmless only because the guard rejects a second comparison first; under
   a partition that would bind `a > 5` and silently drop `a < 10`, the exact
   drop the guard exists to prevent. A column whose comparisons do not all
   merge (`Merge` returns `Ok: false`, `a = 1 AND a = 2`) gets NO range and
   all of its predicates fall to bucket 2. The candidate's own
   `ComputeBoundParameterPrefixMap` (`aggregate_index_candidate.go:262`, the
   port of `MatchCandidate.computeBoundParameterPrefixMap`) then truncates to
   the leading run — equalities, then at most one inequality. That function
   has no production caller today and `ToScanPlan` breaks only on an ABSENT
   column, never after an inequality, so the truncated map — not the folded
   one — is what `ToScanPlan` and `candidateBindingRangesEligible` receive at
   ALL THREE scan sites (the plain scan, the COUNT(*) companion merge, the
   multi-aggregate intersection). Predicates on a column in the truncated run
   are bound; a leading inequality thereby becomes a range scan of the group
   key, as the value index's does (RFC-246's `splitKeyOrder` keeps a range
   coordinate SORTED in the tail, never FIXED — `ownOrderPrefixLen` stops at
   the first non-equality — so the ordering derivation already says the
   right thing about it).

2. **Residuals.** Every remaining predicate is admitted by an ALLOW-LIST of
   predicate kinds — `ComparisonPredicate`, `ValuePredicate`, `AndPredicate`,
   `OrPredicate`, `NotPredicate` — whose FieldValue leaves ROOTED AT THE
   AGGREGATION INPUT (the filter's inner quantifier, carried with each
   conjunct as `aggregateFilterPredicate.input`), ENUMERATED (not probed
   with the boolean `valueReadsField`), each name a grouping column of the
   candidate by `aggColumnMatches`; a predicate with no such leaf is not
   "vacuously all grouping", it is declined. A field rooted at any OTHER
   quantifier is an outer parameter of a correlated query — `t.b = c.b` in a
   scalar subquery, where the outer `c.b` shares the grouping column's
   NAME — and is carried unchanged, never rewritten: matching by accessor
   path alone would turn that predicate into `group.b = group.b` and pass
   every group. The predicate is rewritten with `predicates.ReplaceValues`
   onto the row of the plan the rule yields — grouping column `i` is
   ordinal `i` of the candidate's `groupCols`, which is the LEAF row's
   layout `[groupCols…, FUNC(col)]` and the companion-merge and intersection
   rows' layout alike (all three place the grouping columns at ordinals
   `0..n-1`) — and then the bridge is ASSERTED (`residualBridgeHolds`): the
   rewritten predicate reads the yielded plan's alias, no longer reads the
   input, and reads EXACTLY the other correlations it read before. That is
   what catches a `QuantifiedObjectValue`/`ObjectValue` leaf, an erased or
   invented correlation, and a predicate kind `ReplaceValues` returns
   unchanged (its default arm is a silent pass-through, indistinguishable
   from a no-op rewrite). A residual over grouping columns filters WHOLE
   groups and is sound: the group's aggregate is complete regardless of
   which groups survive.

   The residual sits BELOW `projectAggregateResultToGroupBy`, as ONE
   `RecordQueryPredicatesFilterPlan` over the aggregate scan / companion
   merge / intersection, not above the projection: that is where Java's
   filtering compensation sits relative to its result compensation
   (`Compensation.java`), the projection elision reads only the child's field
   names and is undisturbed, filter-above-projection was measured at 1.88x
   on the 1M stress suite (the comment at `rule_aggregate_data_access.go:192`),
   and the leaf row's ordinals are the candidate's `groupCols` exactly —
   which the GroupBy row's are NOT when a grouping key is record-typed
   (`MatchesGroupBy` matches over `expandGroupingKeysToPrimitives`, so the
   GroupBy row can carry fewer, wider columns than the candidate). Above the
   companion merge is required, not stylistic: below it the owner leg's
   absent filler is all-NULL.

3. **Decline.** A predicate with a leaf that is not a grouping column reads
   the aggregation input (`v > 0` on the summed column, `region = status`
   with `status` non-grouping); no filter above the scan can reconstruct
   that, so the candidate declines. This is not a bolted-on check: in Java
   it falls out of `GroupByExpression.compensate` returning
   `impossibleCompensation` for an unmatched predicate below the aggregate.

The consume-or-decline guard (`aggInnerFilterFullyConsumable`) is deleted,
not kept beside the partition: two readers of the same predicate list is the
shape that let the residual-drop bug ship. A HAVING on a grouping key is
already pushed below the GroupBy (`TestBugHunt_HavingAggregateNotPushedBelowGroupBy`
pins the other direction) and enters this same partition; nobody adds a
second reader for it.

**The filter's rich ordering.** RFC-246 made the aggregate plan's
`HintRichOrdering` live, and a `RecordQueryPredicatesFilterPlan` above the
scan has no rich form of its own: `computeWrapperRichOrdering` synthesises
sorted-only bindings from the plain `HintOrdering`, so the filter MEMBER's
`PropRichOrdering` — the property the in-union and in-join partition roll-ups
read — drops the scan's FIXED bindings. Sort elision is NOT affected:
`memberSatisfiesOrdering` walks a filter as an `orderingDelegator` to its
source members' rich form, and the embedded `ORDER BY a DESC, b` arm over a
residual filter stays sort-free with or without a rich form on the filter
(measured under mutation). Java's `OrderingProperty.visitPredicatesFilterPlan`
passes the child's ordering through, bindings and all, so the filter gets
`HintRichOrdering` delegating to its ordering source (`richOrderingOf(
p.OrderingSourceRef())`, the fetch's shape). Measured: no plan in the
2955-entry EXPLAIN corpus changes with or without it — no corpus shape reads
a filter member's rich bindings — so this closes a latent divergence in the
property, pinned at the plans level, and is stated as such, not as a plan
change.

What does NOT change: `dropsVacatedGroups`, the companion decision
(`NeedsGroupExistenceCompanion`), `bindingRangesEligible` (a NaN or unknown-
type bound still declines — it is asked over the truncated map, so a float
inequality that fell to bucket 2 cannot decline the candidate), `MatchesGroupBy`,
the aggregate row layouts, the executor, the wire.

Cost: a residual over the aggregate index reads one entry per group and
filters; the alternative it replaces reads every base row and aggregates.
Nothing new is priced. Whether the model then CHOOSES the index for the
shapes above is MEASURED at implementation (the plan-shape arms are the
measurement); if it prefers the base scan for a shape the index answers
strictly cheaper, that is a cost-model finding fixed in this change with its
own arm, not filed.

Java alignment is by outcome, not mechanism: Go's rule is structural (it
walks the GroupBy's inner filter) where Java's is the generic matcher, so
this RFC ports Java's RESULT — bound leading run with one inequality,
residual on the select-having row, decline on the input — and states that.

## Verification

* Unit (`cascades/rule_aggregate_data_access_residual_test.go`,
  `TestAggregatePredicatePartition`, 17 arms): leading equality bound;
  leading inequality bound and ends the run; `a > 5 AND a < 10` folds to one
  range carrying BOTH comparisons (the arm counts them, so
  first-comparison-wins cannot pass as a fold); contradictory equalities
  bind nothing and are both residual; non-leading equality, gap, IS NULL on a
  non-leading column, OR, NOT and column-to-column over two grouping columns
  residual; leading IS NULL bound as an equality on the null key; a leaf on
  a non-grouping column, a column-to-column with a non-grouping column, a
  predicate with no field leaf, a kind outside the allow-list, a whole-row
  leaf and a vector distance-rank bound all decline. Plus
  `…_ScanReceivesOnlyTheTruncatedRun` (an equality after a range never
  reaches the scan's comparisons) and
  `…_ApplyResidualsRewritesOntoTheAggregateRow` (the residual reads ordinal
  1 of the aggregate row and that row's alias only; a residual the rewrite
  cannot place is refused by the bridge), and
  `…_OuterCorrelationIsCarriedNotRewritten` (`o.region = c.region` over a
  second quantifier of the same row type: the input read moves, the outer
  read stays, the residual is correlated to exactly the aggregate row and
  the outer quantifier); two more table arms: the same shape is a residual,
  and an outer field alone is not a grouping read and declines.
* Plan shape (`embedded/aggregate_index_residual_test.go`, 21 arms over the
  typed tree): each arm asserts whether the aggregate index is reached, the
  scan's comparison arity, the residual filter's predicate count (−1 = no
  filter) and whether an in-memory sort remains. Residual arms: non-leading
  equality (COUNT and the companion-merged SUM), gap, range-then-equality,
  contradictory equalities (2 residuals), IS NULL, IN, column-to-column, OR,
  a one-sided DOUBLE range demoted from the run, leading IS NULL then a gap,
  and the multi-aggregate intersection with a residual (both a full scan and
  a bound one) — the third scan site. Bound-only arms: leading inequality
  (COUNT and SUM), two bounds folding to one range, leading IS NULL. ORDER BY
  arms `residual_keeps_fixed_binding_desc` / `residual_keeps_sorted_tail`
  stay sort-free through the residual. `correlated_outer_field_same_name` —
  the scalar subquery `(SELECT COUNT(*) FROM t WHERE t.b = c.b AND t.a = 'x'
  AND t.c = 'z' GROUP BY …)` — reaches the aggregate index with one bound
  and two residuals. Declines: a non-grouping leaf, a column-to-column with
  the input. Every "reached" arm with `wantScan: 0` is
  the cost measurement: `Filter(unbounded AggregateIndex)` was CHOSEN over
  `Scan + StreamingAgg`. `TestBugHunt_AggregateIndexMultiKeyResidual`'s
  `non_leading_key` and `gap_in_prefix` flip from must-decline to must-bind
  and `TestBugHunt_AggregateIndexResidualNotDropped`'s leading-inequality arm
  becomes a range-bound control; its input-predicate arms still decline.
  `TestAggregateIndexResidual_RecordTypedGroupingKeyIsUnreachable` pins the
  negative result behind the below-the-projection placement: an aggregate
  index over nested struct fields is refused at DDL validation, so a
  record-typed grouping key never reaches the rule with a candidate; the
  arm's failure message names what re-arms.
* Unit (`plans/index_scan_ordering_test.go`):
  `RecordQueryPredicatesFilterPlan.HintRichOrdering` carries its source's
  FIXED bindings. The embedded ORDER BY arms stay green with the rich form
  removed (measured): sort elision delegates through the filter.
* Rows (`sqldriver/aggregate_index_residual_fdb_test.go`,
  `TestFDB_AggregateIndexResidual`): 22 reads over the indexed/unindexed
  twin — every residual shape above, the leading range (STRING), the leading
  IS NULL, a double range, ORDER BY through the residual, HAVING, the
  two-aggregate intersection, and two correlated scalar subqueries whose
  outer field shares the grouping column's name (COUNT and the
  companion-merged SUM) — through 6 DML stages that move groups into
  and out of the residual's selection, empty and revive groups, zero a SUM
  under the range and empty the double-range arm; each read asserted via
  the typed plan to be served by the aggregate index and, by a
  `wantResidual` field (not SQL text), to carry or not carry the residual.
* Superseded prose swept: `grep -rn 'groupColEqualityIndex\|buildAggScanPrefix\|
  aggInnerFilterFullyConsumable' pkg --include='*.go'` → 0 lines (positive
  control `partitionAggregatePredicates` → 10); the three files that carried
  the old reading (`bug_hunt_cascades_test.go`'s header,
  `agg_index_residual_drop_probe_test.go`'s header and case comments,
  `aggregate_column_identity_test.go`) now describe the partition.
* Mutations, each with the mutated text `grep -c`'d present (1) and absent
  (0) after restoring, over `bazelisk test //pkg/recordlayer/query/plan/
  cascades:cascades_test //pkg/relational/core/embedded:embedded_test
  --nocache_test_results --test_arg=--test.run=TestAggregatePredicatePartition|
  TestAggregateIndexResidual|TestBugHunt_AggregateIndex` (56 `=== RUN`
  lines):
  * bucket 2 reverted to a decline (`if len(residuals) > 0 { return nil,
    false }`): 25 leaf arms redden — the 8 residual-bearing partition arms,
    15 embedded residual arms, both bug-hunt arms — plus the two standalone
    partition tests; the 24 bound-only and decline arms stay green.
  * first-comparison-wins restored in the fold (`comparisons[:1]`): the
    fold arm and the contradictory-equalities arm redden, and
    `TestFDB_AggregateIndexResidual` reddens on rows (`a >= 'x' AND a < 'z'`
    binds `>=` alone and returns extra groups).
  * the filter's `HintRichOrdering` removed: the plans pin fails to build;
    no embedded arm reddens (measured, and stated above as the reason the
    method is a property fix, not a plan change).
  * `rootedAt` mutated to "always true" (a rewrite by accessor path alone,
    the shape codex found): over 48 `=== RUN` lines,
    `…_OuterCorrelationIsCarriedNotRewritten`, the outer-column-alone arm,
    the embedded `correlated_outer_field_same_name` arm and
    `TestFDB_AggregateIndexResidual` redden — and they redden as a DECLINE
    (`StreamingAgg`), not as wrong rows, because the exact-set bridge
    refuses the erased correlation independently of `rootedAt`. The bridge
    as first written (`correlated == {root}`) would have accepted it.
* EXPLAIN corpus (`cmd/explain-differ`, the yamsql corpus), `7e3d59a8b` vs
  the implementation: 2955 entries, 2955 identical, 0 shape flips; also
  2955/2955 with and without the filter's rich form. Population: the corpus
  holds no aggregate-index shape with a predicate outside the bound run, so
  this green says the partition declined none of the corpus's EXISTING
  aggregate-index plans (every one now passes through it), not that it
  exercised the residual.
* Planner fuzz and the 1M stress comparison at the implementation head,
  recorded below.

## Review

RFC lap: Graefe ACK with conditions, Torvalds ACK with conditions, folded
above before implementation — the per-column `Merge` fold replacing
first-comparison-wins (both reviewers, the load-bearing one), the truncated
prefix map wired at all three scan sites with the eligibility gate over it,
bucket 2 as an allow-list with enumerated leaves and the asserted
correlated-to bridge (no vacuous universal, no silent pass-through), the
residual placed below the projection with the record-typed-grouping-key
reason, ordinals from the candidate's `groupCols`, the filter's
`HintRichOrdering` delegation, the HAVING and `impossibleCompensation`
statements, the range arm's column type, and the cost outcome measured
rather than asserted.

Implementation lap on `c0f684739`: Graefe ACK with conditions, Torvalds ACK
with conditions, folded — the fold's admission set narrowed to what a
`ComparisonRange` can hold (the value-index range set plus the NULL
comparisons; a vector distance-rank bound now declines, with an arm); the
peel loop's `last` guarded; the positional re-keying at `rekeyScanPrefix`
justified at the site by the `groupingSignature` invariant (order-sensitive)
and the intersection's in-order name check; the multi-aggregate intersection
with a residual pinned at plan and rows level (the third scan site); a
leading IS NULL pinned as a bound (new reach the fold admitted); the
record-typed grouping key pinned as unreachable with its re-arming message;
the FDB twin's SQL-substring exemptions replaced by a `wantResidual` field;
the superseded prose in three files rewritten and the zero-grep reported;
the mutation populations re-derived with their command. Also corrected on
the way: the RFC's own claim that the filter's rich form was needed for
sort elision — it is not (the delegator walk already reaches the source);
the method stays as Java's property, pinned, and stated as a no-plan-change.

codex on `c0f684739`: one P1 — the residual rewrite matched a field to a
grouping column by accessor path alone, so in a correlated shape
(`t.b = c.b`, the outer field sharing the name) BOTH reads were re-hung on
the aggregate row, the correlation vanished, and the bridge of that commit
(`correlated == {root}`) passed the result. Folded: each filter conjunct
carries the alias of the quantifier it reads (the aggregation input); only a
leaf rooted there is a grouping read, an outer-rooted leaf is carried
unchanged; the bridge asserts the exact correlation set (root present, input
gone, every other correlation preserved, none invented). Pinned at unit,
plan and rows level (the scalar-subquery shape reaches the rule), and the
by-name rewrite measured to decline rather than answer. @claude on the PR.
