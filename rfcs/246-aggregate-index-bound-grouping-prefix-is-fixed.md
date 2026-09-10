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

* `fixedLen` — the leading prefix bound by any equality (`ownOrderPrefixLen`);
* `pins[i]`, for `i < fixedLen` — whether coordinate `i` is PINNED to one
  physical key (`EqualityPinsSinglePhysicalKeyOnColumn`, asked with the
  physical coordinate type so a FLOAT bound by an untyped operand does not
  pin). A prefix coordinate that does not pin — a signed-zero constant such as
  `d = 0.0`, or a possibly-zero untyped operand on a float column — keeps its
  own order in the scan's direction but nothing after it is globally ordered,
  so the tail is dropped wholesale (`tailDropped`). This is a per-coordinate
  fact, not a prefix length: under `d = 0.0 AND b = 1` the widened `d` does
  not pin and `b` still does (every admitted row carries `b = 1`, one physical
  key within each of `d`'s blocks), so `b` binds FIXED and `ORDER BY b DESC`
  is free over a forward scan. The second cut read pins as the length of the
  leading pinned run and demoted that `b` to SORTED — a regression against
  the merge-base's operand-only classification, which was per-coordinate —
  pinned at all three levels (property, plan shape, and the aggregate twin).
  The cascades-side twin of this question,
  `ValueIndexScanMatchCandidate.ComputeMatchedOrderingParts`, had the same
  shape: it broke at the first coordinate that carries no order through
  itself and so emitted NO part for a pinned `b` after a widened `d`, while
  the plan side now says FIXED. It continues past such a coordinate for
  coordinates that PIN (asked through the same `IndexColumnCouldBeFloat` /
  `EqualityPinsSinglePhysicalKeyOnColumn` pair, fed the candidate's physical
  key types), stops at the first that does not, and still refuses the PK
  suffix — so the two derivations classify every coordinate alike, which is
  what its comment claimed and had stopped being true;
* `tail` — the sorted coordinates after the prefix, truncated at the first
  FLOAT/DOUBLE (the NaN-tie hazard), and `untruncated` for the storage-key
  completeness stamp the index's rich form makes.

`RecordQueryAggregateIndexPlan.HintOrdering` claims `tail` at its ORDINALS in
the flowed row `[groupCols…, FUNC(col)]` (the tail's first key is slot
`fixedLen`, not slot 0). A scan that pins every grouping column flows at most
one group and reports a known, key-less ordering; under a dropped tail it
reports unknown.

`HintRichOrdering` is added alongside, the binding-carrying form Java's
candidate produces: each prefix coordinate as `FixedBinding(comparison)` when
`pins[i]` and `SortedBinding` otherwise, the tail as `SortedBinding`. FIXED is
bound from the COLUMN-AWARE `pins`, never from the operand-only
`EqualityPinsSinglePhysicalKey` — that predicate answers "pins" for a FLOAT
coordinate bound by an untyped non-constant operand (an IN binding, a
parameter), which may be zero at runtime and then widens across both
signed-zero blocks; binding it FIXED ("no order, any requested direction is
satisfied") would let `ORDER BY d DESC` elide its sort against a forward scan
that emits -0.0 before +0.0. The value index's rich form carried exactly that
drift (the prefix length computed column-aware, the binding decided
operand-only); it is corrected in the same change and pinned for both plan
types. Today no plan
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
format, executor or cost formula changes; the EXPLAIN baseline is unchanged
because the corpus holds no shape that reads the difference.

One rule body does change, found on the way to the per-coordinate pin's SQL
face. `AggregateDataAccessRule` read a filter's predicate list whole, and
`WHERE a = 'x' AND b = 'y'` arrives as ONE `AndPredicate`, so every
multi-equality on an aggregate index's grouping prefix — including the shape
above and the plain `WHERE d = 1.0 AND b = 1 GROUP BY d, b` — declined the
index and full-scanned into a streaming aggregation. The decline was pinned as
expected behaviour (`bug_hunt_cascades_test.go`, `and_wrapped_multi_equality`)
with a "perf follow-up that needs conjunct flattening" note tracked nowhere
else. Java's `SelectExpression` holds its conjuncts flat, so both the
consumption guard (`aggInnerFilterFullyConsumable`) and the bound builder
(`extractInnerFilterPredicates`) now read `flattenConjuncts` of the filter's
predicates — the same list, so the two cannot disagree on what a filter holds.
The gap and non-leading-key declines are unchanged (the guard sees the gap
after flattening). The corpus holds no such shape either.

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
  SORTED while the same operand on a LONG is FIXED; `d = 0.0 AND a = 1`
  yields `[D]` SORTED and `[A]` FIXED (its index twin,
  `TestRecordQueryIndexPlan_HintRichOrdering_PinnedCoordinateAfterWidenedOneStaysFixed`,
  is green at `d6b5a0d84` and red at `82e7d1c19`); the rich form's
  distinctness is false over the default inner scan and true over a
  `WithStrictlySorted` one. Under the pre-fix split (mutated in place:
  `fixedLen = 0`, tail = all columns) the three prefix arms fail and the two
  unchanged arms pass; under the operand-only FIXED classification (mutated
  in both rich forms) the two untyped-operand arms — the aggregate one and its
  index twin in `index_scan_ordering_test.go` — fail; under pins read as a
  prefix length (mutated in `splitKeyOrder`) exactly the two
  after-a-widened-coordinate arms and the embedded pin below fail, and the
  untyped-operand and distinctness arms stay green.
* Unit, candidate side (`cascades/signed_zero_pk_suffix_ordering_test.go`):
  over INDEX(V DOUBLE, B LONG), `v = 0.0 AND b = 1` emits `[V, B]` with B's
  equality range and no PK suffix; `v = 0.0` with B unbound emits `[V]`; two
  widened equalities over INDEX(V, V2) emit `[V]`. With the loop restored to
  break at the first non-carrying coordinate (mutated in place), exactly the
  `[V, B]` arm fails and the other four `TestMatchedOrderingParts_*` arms
  pass. `equalityPrefixLenOnColumns`, whose column-aware arm had no caller
  left, is folded into the operand-only `equalityPrefixLen` the PK scan uses.
* Plan shape (`embedded/aggregate_index_equality_prefix_ordering_test.go`):
  seven fixed-prefix shapes (COUNT, MAX, a primary-key grouping column, full
  `ORDER BY b, a`, `ORDER BY b DESC, a`, `LIMIT`, a pinned DOUBLE prefix) plan
  with no `RecordQueryInMemorySortPlan` in the typed tree; three controls
  (unbound, signed-zero DOUBLE prefix, DOUBLE tail) keep theirs; every arm
  asserts the aggregate index is reached. Deleting `HintRichOrdering` reddens
  exactly the two arms that request `b`.
* Plan shape (`embedded/equality_prefix_pins_per_coordinate_test.go`): over
  `INDEX(d, b)` and `GROUP BY d, b`, `WHERE d = 0.0 AND b = 1` with
  `ORDER BY b DESC` / `ORDER BY b` / `ORDER BY d` / `ORDER BY d DESC` (the
  last by a reverse scan) plans with no in-memory sort for both plan types,
  and the control `ORDER BY id` keeps its sort; the value-index arms assert a
  value-index scan is reached and the aggregate arms assert the aggregate
  index plan itself (an aggregate plan wraps its own index scan, so counting
  both would let an aggregate arm pass on a streaming aggregation). Under pins-as-prefix-length exactly the four arms that
  request `b` fail (index and aggregate, ASC and DESC) and the three others
  pass; with the conjunct flattening removed the two aggregate arms no longer
  reach the index. `bug_hunt_cascades_test.go`'s `and_wrapped_multi_equality`
  flips from must-decline to must-bind, alongside a new every-key-bound arm;
  `gap_in_prefix` and `non_leading_key` still decline.
* Rows (`sqldriver/aggregate_index_equality_prefix_ordering_fdb_test.go`):
  the indexed/unindexed twin over twelve reads whose order now comes from
  the index — NULL groups, LIMIT/OFFSET, HAVING, a pinned DOUBLE prefix, and
  two every-column-bound point reads whose `AND` the rule now flattens —
  compared as SEQUENCES against the sorting oracle through six DML stages that
  add, empty, revive and merge groups. Every read is asserted, via the typed
  plan, to be served by the aggregate index AND to carry no in-memory sort;
  without the second assertion the test passes with the fix reverted (a sorted
  plan answers the same sequence), so that assertion is what makes it a pin
  of the index's order rather than of the sort's.
* EXPLAIN corpus (`cmd/explain-differ`), `d6b5a0d84` vs `984ccbca3`: 2955
  entries, 2955 identical, 0 shape flips — the corpus holds no aggregate-index
  shape with a bound prefix and an ORDER BY on the next grouping column, and
  no multi-equality on an aggregate index's grouping prefix.
* `just test` green on every commit (pre-commit hook, 92 targets). At
  `984ccbca3`, one `--nocache_test_results` run of
  `//pkg/recordlayer/query/plan/plans:plans_test`,
  `//pkg/recordlayer/query/plan/cascades:cascades_test` and
  `//pkg/relational/core/embedded:embedded_test` filtered to
  `TestAggregateIndexPlan_Hint|TestRecordQueryIndexPlan_HintRichOrdering_|
  TestAggregateIndexEqualityPrefixElidesSort|TestPinnedCoordinateAfterWidenedOneElidesSort|
  TestBugHunt_AggregateIndexMultiKeyResidual|TestMatchedOrderingParts_` printed
  43 `=== RUN` lines (19 top-level tests, 24 sub-arms), 19 `--- PASS`, 0
  `--- FAIL`; `//pkg/relational/sqldriver:sqldriver_test` filtered to
  `TestFDB_AggregateIndexEqualityPrefixOrdering|TestFDB_SignedZero` printed
  2 `--- PASS`. Every mutation claim above was taken with the mutated text
  `grep -c`'d present (1 or 2 lines) in the same invocation and absent (0)
  after restoring.
* Planner fuzz at `984ccbca3`, 30s each: `FuzzPlanner_Determinism` 5,826,390
  executions, PASS; `FuzzPlanner_PlanFullPipeline` 2,047,991 executions, PASS
  (at `82e7d1c19`: 5,786,862 and 2,061,381, PASS).

### 1M stress comparison

Baseline `d6b5a0d84` (the merge-base on 2026-09-09; `origin/master` was at
the same commit) in a worktree on the same filesystem (`/home`, 99% used, 13G
free) versus `984ccbca3`, `TestFDB_Stress_1M` uncached, strictly sequential,
in TWO orderings of two runs per side — base, branch, base, branch (load at
each start 8.7 / 3.0 / 2.1 / 2.2) and then branch, base, branch, base (1.4 /
2.9 / 3.0 / 2.8) — with `ordering.go`, `rule_aggregate_data_access.go` and
`match_candidate_index.go` md5-checked in both trees after each sequence.
All eight runs have 24 `=== RUN` lines and 24 passes; all 22 labelled
readings agree on row counts across the eight runs.

The first ordering read 1.7–2.6x on its first five readings (the three PK
lookups, `idx_customer eq`, `idx_amount range`) in BOTH branch runs. The
second ordering read the same 1.7–2.6x on the same five readings in BOTH base
runs: the disturbance sits at positions 2 and 4 of each sequence whichever
tree runs there, and the identically-planned `PK needle` later in every run
is 1.00. The planner-only cost of those shapes, measured with no FDB behind
it (`embedded/plan_stress_shapes_bench_test.go`, 3×200 iterations per tree):
PK lookup 1.50 vs 1.50 ms, idx_customer eq 2.22 vs 2.22 ms, idx_amount range
2.35 vs 2.36 ms, GROUP BY status 1.71 vs 1.72 ms, SUM by status 1.60 vs
1.63 ms (+2%, the per-call `pins` slice and closure), IN-list 10.3 vs
10.3 ms. Ratio below = min over the four runs per side:

| query | rows | base | branch | ratio |
|---|---|---|---|---|
| PK lookup id=0 / N/2 / N-1 | 1 | 8.5 / 8.5 / 5.1 ms | 8.6 / 8.4 / 6.3 ms | 1.02 / 0.98 / 1.22 |
| idx_customer eq | 8 | 6.5 ms | 6.4 ms | 0.99 |
| idx_amount range >9000 | 100017 | 191.0 ms | 190.8 ms | 1.00 |
| idx_status count pending | 1 | 333.3 ms | 336.9 ms | 1.01 |
| full scan filter amount>5000 | 1 | 679.3 ms | 596.7 ms | 0.88 |
| GROUP BY status | 4 | 5.9 ms | 5.7 ms | 0.96 |
| GROUP BY status COUNT only | 4 | 5.4 ms | 5.7 ms | 1.06 |
| SUM by status (aggregate index) | 4 | 5.7 ms | 6.0 ms | 1.05 |
| GROUP BY customer HAVING | 47271 | 575.9 ms | 579.7 ms | 1.01 |
| JOIN 10 orders x customers | 10 | 19.7 ms | 19.5 ms | 0.99 |
| ORDER BY PK (full) | 1000000 | 3854 ms | 3779 ms | 0.98 |
| ORDER BY PK + index filter | 8 | 8.9 ms | 9.0 ms | 1.01 |
| scan all rows ordered / wide | 1000000 | 3672 / 3925 ms | 3627 / 3870 ms | 0.99 / 0.99 |
| IN-list 5 values | 46 | 18.9 ms | 18.9 ms | 1.00 |
| PK needle id=999999 | 1 | 5.8 ms | 5.8 ms | 1.00 |
| PK+filter needle id=500000 | 1 | 7.3 ms | 7.4 ms | 1.01 |
| full scan sparse filter | 97 | 3316 ms | 3262 ms | 0.98 |
| UPDATE by index / DELETE single row | 8 / 1 | 8.7 / 6.2 ms | 8.8 / 6.3 ms | 1.00 / 1.02 |

`PK lookup id=N-1` at 1.22 is one 5.1 ms base reading against 6.3 ms in the
other three base runs and 6.3 ms on the branch — the same query at
`82e7d1c19` read 6.2 vs 6.3. The workload's aggregate-index reads bind no
grouping prefix and carry a single predicate, so `splitKeyOrder` reports what
the old derivation did and the conjunct flattening sees one conjunct; their
plans are unchanged (EXPLAIN lines identical across all eight logs). This is
a no-change confirmation.

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
changes.

The delta re-confirmation on the fold found three more things, all folded:
Graefe and Torvalds both caught the header still calling the PK scan's rich
form staging when the data-access leaf reaches it (and Torvalds the count
having lost its "in this file" scope) — the header now states the dispatch
route (`computeWrapperRichOrdering` reaches the rich form of any memo
expression that implements it, and every plan with one does) instead of a
liveness list; Torvalds that the `IsStrictlySorted()` true arm had no pin —
added; and codex that the fold's prefix-length reading of the pinned
coordinates demoted a pinned coordinate after a widened one to SORTED, a
regression against the merge-base — corrected to per-coordinate `pins` and
pinned at property, plan-shape and rows level, with the aggregate rule's
conjunct flattening found and fixed on the way to that pin's SQL face.

The second delta re-confirmation (per-coordinate pins + conjunct flattening):
codex ACK. Graefe ACK with one condition, folded — the cascades-side
`ComputeMatchedOrderingParts` broke at the first non-carrying coordinate and
emitted nothing for a still-pinned equality after it, contradicting its own
"the two derivations cannot classify a column differently" comment; it now
continues for pinned coordinates and the comment states what is shared.
Torvalds ACK with four conditions, folded — the dead column-aware arm of
`equalityPrefixLenOnColumns` deleted, the aggregate arms of the embedded pin
assert the aggregate plan rather than any index scan, and the EXPLAIN corpus,
Bazel-by-name run, mutation-present greps and stress comparison re-run at the
final head and recorded above with their SHAs. @claude on the PR.
