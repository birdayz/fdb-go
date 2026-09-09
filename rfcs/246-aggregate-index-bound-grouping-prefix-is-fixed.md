# RFC-246: an aggregate index scan's bound grouping prefix is fixed, not sorted

## Finding and reference

At `d6b5a0d84`, `RecordQueryAggregateIndexPlan.HintOrdering` (`plans/ordering.go`)
claims every grouping column of the index as a sorted key, whatever the scan
binds. The underlying `RecordQueryIndexPlan` binds a leading prefix of the
grouping key by equality — that is how `WHERE b = 1` reaches the index — and
the value-index twin of this method, `RecordQueryIndexPlan.HintOrdering`, drops
that prefix from its sorted keys (`TestRecordQueryIndexPlan_HintOrdering_
EqualityPrefixDropped`). The aggregate plan never did, so its ordering
`[b, a]` cannot satisfy a request for `[a]` and the planner materializes the
groups to sort them:

```
SELECT b, a, COUNT(*) FROM t WHERE b = 1 GROUP BY b, a ORDER BY a
  before: Project(InMemorySort([A ASC], AggregateIndex(COUNT, T_CNT_B_A, [B A], T, live_groups_only)))
  after : Project(AggregateIndex(COUNT, T_CNT_B_A, [B A], T, live_groups_only))
```

The scan already delivers that order: an aggregate index stores one entry per
group under the tuple `(b, a)`, the range `b = 1` is a contiguous run of it,
and within the run the entries ascend by `a`. Java's
`AggregateIndexMatchCandidate.computeOrderingFromScanComparisons` says exactly
this — `Binding.fixed` for `i < scanComparisons.getEqualitySize()`, and the
ordering sequence starting at that index — so this is a parity gap in a
property derivation, surfaced by the composite-primary-key twin sweep (RFC-245)
when its aggregate probes were read for plan shape rather than rows only.

Right rows the wrong way: the sort materializes every group of the bound range
before the first row is returned, and defeats a LIMIT that the scan could have
honoured incrementally. The rowdiff harness classifies a sort over an input
the scan already orders as a finding (`checkPlanOrdering`); its generator does
not produce this shape, and neither does the yamsql corpus (the EXPLAIN
baseline is unchanged by the fix).

## Decision

Port the split the value index already applies to its key onto the grouping
key. `RecordQueryAggregateIndexPlan.HintOrdering` reads the inner scan's
comparisons and:

* pins the equality-bound grouping prefix — `equalityPrefixLenOnColumns`, asked
  with the physical coordinate types so a FLOAT bound by an untyped operand
  stops the prefix;
* treats a signed-zero equality (`d = 0.0` on a DOUBLE grouping column, which
  admits both physical zeros) as `ownOrderPrefixLen` does for the index scan:
  it keeps its own order in the scan's direction but nothing after it is
  globally ordered, so the sorted tail is dropped wholesale;
* truncates the sorted tail at the first FLOAT/DOUBLE grouping column, the
  NaN-tie hazard this producer already asked `claimableNameLimit` about;
* keeps each key's ORDINAL in the flowed row `[groupCols…, FUNC(col)]` — the
  tail's first key is slot `fixedLen`, not slot 0.

A scan that pins every grouping column flows at most one group and reports a
known, key-less ordering (ordered by anything), except under a signed-zero
equality, where it reports unknown.

`HintRichOrdering` is added alongside, the binding-carrying form Java's
candidate produces: the prefix as `FixedBinding(comparison)` (or `SortedBinding`
for the signed-zero coordinate, which is two distinct sort values), the tail as
`SortedBinding`. Without it the plain form's dropped prefix would leave the
bound column with no binding at all in the rich ordering
`computeWrapperRichOrdering` derives, and a set-operation consumer (an
in-union over `WHERE b IN (…) GROUP BY b, a`) could not promote it. The
distinctness claim stays `NotDistinct`, what the derived form reported before.

Nothing else changes: no wire format, no executor, no cost formula, no rule.
The aggregate data-access rule and the sort-elision machinery consume the
corrected property as they always did.

## Verification

* Unit (`plans/aggregate_index_ordering_test.go`): unbound scan keeps `[B, A]`
  (descending under a reverse scan); `b = 1` yields `[A]` at ordinal 1, rich
  form `B` fixed / `A` sorted with exactly one equality-bound value; both
  bound yields a known key-less ordering, rich form all fixed; `b > 1` keeps
  `[B, A]`; `d = 1.0` yields `[A]`; `d = 0.0` yields unknown, rich form `[D]`
  sorted with the tail dropped; a DOUBLE tail under `b = 1` yields unknown.
  Under the pre-fix split (mutated in place: `fixedLen = 0`, tail = all
  columns) the three prefix arms fail and the two unchanged arms pass.
* Plan shape (`embedded/aggregate_index_equality_prefix_ordering_test.go`):
  six fixed-prefix shapes (COUNT, MAX, a primary-key grouping column, full
  `ORDER BY b, a`, `LIMIT`, a pinned DOUBLE prefix) plan with no
  `RecordQueryInMemorySortPlan` in the typed tree; three controls (unbound,
  signed-zero DOUBLE prefix, DOUBLE tail) keep theirs; every arm asserts the
  aggregate index is reached.
* Rows (`sqldriver/aggregate_index_equality_prefix_ordering_fdb_test.go`):
  the indexed/unindexed twin over ten reads whose order now comes from the
  index — NULL groups, LIMIT/OFFSET, HAVING, a pinned DOUBLE prefix —
  compared as SEQUENCES against the sorting oracle through six DML stages that
  add, empty, revive and merge groups; every read is asserted to be served by
  the aggregate index via the typed plan.
* EXPLAIN corpus (`cmd/explain-differ`), `d6b5a0d84` vs this change: 2955
  entries, 2955 identical — the corpus holds no aggregate-index shape with a
  bound prefix and an ORDER BY on the next grouping column.
* `just test` green on the commit (pre-commit hook), stress and fuzz recorded
  below.

## Review

Graefe (Cascades alignment) and Torvalds (code quality) on this RFC and the
implementation together; codex and @claude on the PR.
