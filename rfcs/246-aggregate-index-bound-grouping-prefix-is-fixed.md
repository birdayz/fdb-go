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
key — and make it ONE derivation. `splitKeyOrder` (`plans/ordering.go`) now
serves `RecordQueryIndexPlan.HintOrdering`, `RecordQueryIndexPlan.
HintRichOrdering` and both aggregate forms, replacing the two hand-rolled
copies the index plan carried (the shape the file's own comments name as how
its plain and rich forms once drifted apart). It reads a scan's comparisons
and reports:

* `pinned` — the leading coordinates PINNED to one physical key
  (`equalityPrefixLenOnColumns`, asked with the physical coordinate types so a
  FLOAT bound by an untyped operand does not count);
* `fixedLen` — the leading prefix bound by any equality (`ownOrderPrefixLen`).
  A coordinate in `[pinned, fixedLen)` — a signed-zero constant such as
  `d = 0.0`, or a possibly-zero untyped operand on a float column — keeps its
  own order in the scan's direction but nothing after it is globally ordered,
  so the tail is dropped wholesale (`tailDropped`);
* `tail` — the sorted coordinates after the prefix, truncated at the first
  FLOAT/DOUBLE (the NaN-tie hazard), and `untruncated` for the storage-key
  completeness stamp the index's rich form makes.

`RecordQueryAggregateIndexPlan.HintOrdering` claims `tail` at its ORDINALS in
the flowed row `[groupCols…, FUNC(col)]` (the tail's first key is slot
`fixedLen`, not slot 0). A scan that pins every grouping column flows at most
one group and reports a known, key-less ordering; under a dropped tail it
reports unknown.

`HintRichOrdering` is added alongside, the binding-carrying form Java's
candidate produces: the prefix as `FixedBinding(comparison)` for `i < pinned`
and `SortedBinding` for the rest of it, the tail as `SortedBinding`. FIXED is
bound from the COLUMN-AWARE `pinned`, never from the operand-only
`EqualityPinsSinglePhysicalKey` — that predicate answers "pins" for a FLOAT
coordinate bound by an untyped non-constant operand (an IN binding, a
parameter), which may be zero at runtime and then widens across both
signed-zero blocks; binding it FIXED ("no order, any requested direction is
satisfied") would let `ORDER BY d DESC` elide its sort against a forward scan
that emits -0.0 before +0.0. The value index's rich form carried exactly that
drift (`pinned` computed column-aware, the binding decided operand-only); it is
corrected in the same change and pinned for both plan types. Today no plan
reaches it — the in-union declines a float IN-list on the plain ordering
first — so this is a latent inconsistency closed, not a live wrong answer.

The rich form is load-bearing, not decorative. Sort elision runs on the RICH
ordering (`memberSatisfiesOrdering` → `computeWrapperRichOrdering(pe).
Satisfies(requested)`, and `ImplementSortRule` reads
`GetEqualityBoundValues()` to discount request keys the scan fixes). With the
plain form's prefix dropped and no rich form, `computeWrapperRichOrdering`'s
derived fallback would carry NO binding for `b`, and `ORDER BY b, a` or
`ORDER BY b DESC, a` under `b = 1` could not be satisfied. Measured: deleting
`HintRichOrdering` reddens exactly the two embedded arms that request `b`
(`count_prefix_fixed_full_order`, `count_prefix_fixed_desc_then_asc`) and no
other. An in-union over `WHERE b IN (…) GROUP BY b, a` would also read the
FIXED binding, but that shape does not reach the aggregate index today (the
aggregate data-access rule matches equality-bound groups only), so it is
stated as the design's second consumer and not claimed as covered.

Distinctness follows Java: `OrderingProperty.visitAggregateIndexPlan` hands
`computeOrderingFromScanComparisons` the inner scan's `isStrictlySorted()`,
so the rich form claims `DistinctOverAllKeysIf(indexPlan.IsStrictlySorted())`
— the same flag the value index reads — where the first cut hard-coded
`NotDistinct`.

What changes beyond the property itself: every aggregate index plan's
`PropRichOrdering` flips from the derived all-sorted fallback to these
bindings, and that property is read by the sort rule (above), the in-join,
distinct-union and nested-loop-join rules, and partition roll-ups. None of
them is edited; each now sees a bound grouping column as FIXED rather than
SORTED, which is strictly more information and what Java reports. No wire
format, executor, cost formula or rule body changes; the EXPLAIN baseline is
unchanged because the corpus holds no shape that reads the difference.

Permuted MIN/MAX indexes interpose the aggregate value before the permuted
grouping suffix, so `groupCols[fixedLen:]` would misread their key; they never
become an aggregate plan (`tryAggregateIndexCandidate` declines
`permutedSize > 0`, and the candidate caps bindings at
`physicalGroupingPrefixCount`), and `groupingOrderSplit` states that
precondition at the site.

## Verification

* Unit (`plans/aggregate_index_ordering_test.go`): unbound scan keeps `[B, A]`
  (descending under a reverse scan); `b = 1` yields `[A]` at ordinal 1, rich
  form `B` fixed / `A` sorted with exactly one equality-bound value; both
  bound yields a known key-less ordering, rich form all fixed; `b > 1` keeps
  `[B, A]`; `d = 1.0` yields `[A]`; `d = 0.0` yields unknown, rich form `[D]`
  sorted with the tail dropped; a DOUBLE tail under `b = 1` yields unknown; a
  reverse scan under `b = 1` yields `[A]` descending with `B` fixed; a DOUBLE
  bound by an UNKNOWN-typed parameter yields unknown with the rich form `[D]`
  SORTED while the same operand on a LONG is FIXED. Under the pre-fix split
  (mutated in place: `fixedLen = 0`, tail = all columns) the three prefix
  arms fail and the two unchanged arms pass; under the operand-only FIXED
  classification (mutated in both rich forms) the two untyped-operand arms —
  the aggregate one and its index twin in `index_scan_ordering_test.go` —
  fail.
* Plan shape (`embedded/aggregate_index_equality_prefix_ordering_test.go`):
  seven fixed-prefix shapes (COUNT, MAX, a primary-key grouping column, full
  `ORDER BY b, a`, `ORDER BY b DESC, a`, `LIMIT`, a pinned DOUBLE prefix) plan
  with no `RecordQueryInMemorySortPlan` in the typed tree; three controls
  (unbound, signed-zero DOUBLE prefix, DOUBLE tail) keep theirs; every arm
  asserts the aggregate index is reached. Deleting `HintRichOrdering` reddens
  exactly the two arms that request `b`.
* Rows (`sqldriver/aggregate_index_equality_prefix_ordering_fdb_test.go`):
  the indexed/unindexed twin over ten reads whose order now comes from the
  index — NULL groups, LIMIT/OFFSET, HAVING, a pinned DOUBLE prefix —
  compared as SEQUENCES against the sorting oracle through six DML stages that
  add, empty, revive and merge groups. Every read is asserted, via the typed
  plan, to be served by the aggregate index AND to carry no in-memory sort;
  without the second assertion the test passes with the fix reverted (a sorted
  plan answers the same sequence), so that assertion is what makes it a pin
  of the index's order rather than of the sort's.
* EXPLAIN corpus (`cmd/explain-differ`), `d6b5a0d84` vs this change: 2955
  entries, 2955 identical — the corpus holds no aggregate-index shape with a
  bound prefix and an ORDER BY on the next grouping column.
* `just test` green on the commit (pre-commit hook), stress and fuzz recorded
  below.

## Review

Graefe (Cascades alignment) and Torvalds (code quality) reviewed this RFC and
the implementation together; codex reviewed the diff. All three found the same
defect in the first cut — the rich form decided FIXED with the operand-only
predicate while the prefix length was column-aware, so an untyped operand on a
DOUBLE grouping column bound FIXED — and it is folded above, in both rich
forms, with the pins named in Verification. Also folded: the shared
`splitKeyOrder` (three copies were two too many), the permutation
precondition stated at `groupingOrderSplit`, distinctness taken from the inner
scan's `isStrictlySorted()` as Java does, the `ORDER BY b DESC, a` and
reverse-scan arms, the no-sort assertion in the rows twin, the corrected
header claim in `plans/ordering.go` (the aggregate rich form is LIVE, and the
enumeration of producers that ask the ordering-claim predicate names it), and
the "no rule changes" sentence replaced by the list of consumers whose input
changes. @claude on the PR.
