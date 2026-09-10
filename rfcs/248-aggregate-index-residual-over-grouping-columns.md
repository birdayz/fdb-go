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
   `OrPredicate`, `NotPredicate` — whose FieldValue leaves, ENUMERATED (not
   probed with the boolean `valueReadsField`), each name a grouping column of
   the candidate by `aggColumnMatches`; a predicate with no FieldValue leaf
   at all is not "vacuously all grouping", it is declined. The predicate is
   rewritten with `predicates.ReplaceValues` onto the row of the plan the
   rule yields — grouping column `i` is ordinal `i` of the candidate's
   `groupCols`, which is the LEAF row's layout `[groupCols…, FUNC(col)]`
   and the companion-merge and intersection rows' layout alike (all three
   place the grouping columns at ordinals `0..n-1`) — and then the bridge is
   ASSERTED: the rewritten predicate's correlated-to set must be exactly the
   yielded plan's alias, or the predicate declines. That is what catches a
   `QuantifiedObjectValue`/`ObjectValue` leaf, an outer correlation, and a
   predicate kind `ReplaceValues` returns unchanged (its default arm is a
   silent pass-through, indistinguishable from a no-op rewrite). A residual
   over grouping columns filters WHOLE groups and is sound: the group's
   aggregate is complete regardless of which groups survive.

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

* Unit (`cascades/rule_aggregate_data_access_test.go` or a new file beside
  it): the partition over hand-built predicates — leading equality bound;
  leading inequality bound and ends the run; equality after an inequality is
  residual; non-leading equality residual; gap residual; contradictory
  equalities on one column residual (no bound); `IS NULL` / `IN` / OR of
  grouping predicates residual; column-to-column on two grouping columns
  residual; non-grouping leaf declines; a leaf on the aggregated column
  declines. Each residual is asserted to reference the aggregate row's
  ordinal for its column and nothing else.
* Plan shape (`embedded/bug_hunt_cascades_test.go`, the two must-decline arms
  flip to must-bind-with-residual, plus new arms): over the typed tree,
  `non_leading_key` plans `PredicatesFilter(AggregateIndex(SUM_ABC…))` with
  an unbounded scan; `gap_in_prefix` plans the same over `[a = 'x']`;
  `leading_inequality` plans `AggregateIndex` over a range with NO filter;
  `range_then_equality` (`a > 'm' AND b = 'y'`) plans the range with a
  residual on `b`; `non_grouping_residual` (`… AND v > 0`) still declines to
  `StreamingAgg`; the COUNT(*) companion merge carries the residual above the
  merge. Each arm asserts the residual's predicate count and that the scan's
  comparison arity is what the run allows.
* Rows (`sqldriver/…_fdb_test.go`, indexed/unindexed twin): the three shapes
  plus `IS NULL`, `IN`, a column-to-column residual, an ORDER BY over a
  residual-filtered scan, through DML that empties and revives groups, for
  COUNT(*) (plain) and SUM (companion-merged); every read asserted via the
  typed plan to be served by the aggregate index.
* Unit for the partition's hazards, each an arm: `a > 5 AND a < 10` folds to
  one range (the mutation restoring first-wins reddens it); `a = 1 AND a = 2`
  binds nothing and both are residual; the truncated map, not the folded
  one, reaches `ToScanPlan` at all three sites (an equality after an
  inequality never appears in the scan's comparisons); a predicate kind
  outside the allow-list declines; a predicate with no FieldValue leaf
  declines; a correlated leaf declines through the bridge.
* Unit (`plans/index_scan_ordering_test.go`):
  `RecordQueryPredicatesFilterPlan.HintRichOrdering` carries its source's
  FIXED bindings. Embedded arms `residual_keeps_fixed_binding_desc` and
  `residual_keeps_sorted_tail` (`ORDER BY a DESC, b` / `ORDER BY b` over a
  residual-filtered `[a = 'x']` scan) plan with no sort — through the
  delegator walk, and measured to stay green with the rich form removed.
* The `leading_inequality` arms state their column type: a STRING or BIGINT
  range keeps the companion merge's ordering gate; a one-sided FLOAT range is
  terminal there and the SUM shape falls to the plain-scan arm.
* Every pin measured red under the mutation that reverts bucket 2 to a
  decline, with the mutation-present grep; EXPLAIN corpus diff with its
  population stated and every flip read; fuzz; 1M stress as for RFC-246/247.

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
rather than asserted. Implementation lap, codex and @claude recorded below.
