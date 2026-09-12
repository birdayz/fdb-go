# RFC-251: SUM and AVG seed from the first non-NULL operand

Status: implemented and locally verified; no push or merge requested.

## Finding

At `0a55930f65828b12e1ab1de1c3c56d2f7c11d489`, the executor's
`aggregateCursor.accumulateRow` adds every SUM/AVG operand to a zero-initialized
`float64` accumulator. IEEE addition of `+0` and `-0` produces `+0`, so SUM and
AVG of only negative zeros return the wrong sign. NULLs do not fix this: the
first non-NULL operand still gets added to the implicit positive zero.

`TestAggregateSumInitialState` ran under uncached Bazel and failed on the
negative-zero cases for both functions, including leading/interleaved NULLs
and restored partial states. It compares float bits, not `==`, because numeric
equality would certify the defect. Ordinary positive zero, mixed signed zeros,
cancellation, all-NULL input and empty input are controls.

The SQL regression `TestFDB_AggregateSignedZero` inserts actual DOUBLE values
into real FDB, checks their stored sign, and checks scalar and grouped SUM/AVG
at five scanned-row budgets. Its covering `(g, id, d)` index orders each group
by its integer id so leading NULLs and mixed signs have explicit input order;
EXPLAIN must show a streaming aggregate with no intervening sort. All five
budget subtests ran and failed under uncached Bazel: both scalar and grouped
SUM/AVG returned bits `0000000000000000` instead of `8000000000000000` for
the negative-zero groups. This is a read-side result defect; no record, index,
or continuation schema changes.

The yamsql reproducer also ran before the edit: grouping the SUM or AVG results
returned one `[0, 2]` row instead of two groups `[-0, 1]` and `[0, 1]`. Thus the
sign defect changes downstream row counts, not only floating-point rendering.

## Reference and decision

Read in full: Java 4.12.11.0 `NumericAggregationValue.java` and
`cursors/aggregate/StreamGrouping.java`.
`NumericAccumulator.state` starts as NULL. `PhysicalOperator.evalPartialToPartial`
returns its other argument when either partial is NULL. SUM_D's initial partial
is the operand itself; AVG_D's initial partial is `(operand, 1)`. Addition begins
only at the second non-NULL operand. The running sum is therefore `-0.0` for
an all-negative-zero group, and AVG_D preserves that sign when dividing by its
positive count.

Use the existing per-aggregate non-NULL count in Go: after incrementing
`gs.counts[i]`, assign the operand to `gs.sums[i]` when the count is one;
otherwise add it. Keep the exact integer accumulator and its overflow checks
unchanged. The partial-state codec already carries this count and the double
sum, including signed zero. Restoring an all-NULL prefix must still seed from
the next non-NULL operand; restoring a non-empty prefix must add, not reset.

Rejected alternatives:

- Special-case negative zero at finalization: cannot distinguish an all-negative
  group from mixed signs or finite cancellation, and fails through continuations.
- Seed with negative zero: encodes a floating-point trick instead of Java's
  absent-partial semantics and is not a general first-value accumulator.
- Track another initialized flag: duplicates the non-NULL count already used
  for SUM/AVG NULL results and already serialized in continuations.
- Use total group row count: leading NULLs make it the wrong population.

The only execution overhead is a first-value branch per SUM/AVG operand; there
are no allocations, additional scans, planner alternatives, or property changes.
This is not a performance improvement claim.

## Verification and gates

- Preserve and run the failing unit test under Bazel. It round-trips after every
  possible input prefix, including empty and completed prefixes.
- Preserve and run the real-FDB SQL regression. Assert exact results rather than
  only equality between paged and unpaged executions. The additional
  `aggregate_sum_signed_zero.yaml` scenario groups the aggregate results: losing
  the negative sign merges two groups into one. Its row counts distinguish the
  defect even though the YAML numeric comparator equates the two zero signs.
- Graefe and Torvalds RFC ACK before the production edit; joint implementation
  review and a single Codex review after the regression tests pass.
- Run affected targets uncached, the aggregate-continuation fuzzer, and `just test`.
  Hash the changed source files before/after final verification.
- Run the 1M stress target twice per state, sequentially at the same merge-base
  and in one worktree on the same filesystem. The main `/home` disk is 98% full;
  use `/var/tmp` for both measured states and Bazel outputs (57% used when checked).
  Record both source identities and the actual test/row/plan populations.

## Design review

Both virtual reviewers ACKed before the production edit (Claude Sonnet):

- Graefe: `d89fcc3e-c91e-43f2-b690-41b003ed8482`, ACK. Checked Java's absent-partial
  identity, the existing count discriminator, and both continuation states.
- Torvalds: `24a5211c-ad31-4c18-97eb-e24745953e0d`, ACK. Checked the bug site,
  serialized non-NULL count, bitwise tests, and ordered real-FDB input.

## Implementation review and focused verification

Reviewed production blob: `db1459188bfd7865caf7ce0649713a51255ff2c0`
(before: `930ee118157ea3b6510adff633058ee0f210a123`). The two Go tests and
yamsql scenario were frozen along with the production source for these reviews:

- Graefe: ACK, session `52a01294-7f7d-4c32-ada4-892f697ea378`.
- Torvalds: ACK, session `3d3af25a-644d-4be1-9ce7-7eff9f271734`.
- Codex: no blocking findings, session `01a0977c-b650-7700-a255-9e97b75edcdb`.

Gazelle's two listings of the executor test file are in distinct attributes,
`srcs` and `embedsrcs`, not duplicate entries in `srcs`; no manual BUILD edit
was made. `just gazelle` and `bazelisk mod tidy` completed successfully.

The executor, SQL driver, and yamsql Bazel targets all passed **in full and
uncached**. Their output includes all 20 new unit subtests, all five FDB budget
subtests, and the new scenario's `2/2 passed` report. The existing
`FuzzAggregateContinuation` consumed its five seeds and passed 2,474,305
executions in 15 seconds with four workers. This fuzzes the continuation codec;
it is not a substitute for the new accumulator or SQL assertions. All four
changed source/test files were MD5-checked unchanged after these executions.

The sequential 1M comparison passed twice per state with identical 24-RUN,
22-timed-row and 11-EXPLAIN populations. Exact source identities, starting load,
all per-query timings, and limitations are recorded in `TODO.md`, section 11,
“Stress test 1M baseline — RFC-251 SUM/AVG initial state”.

The first repository-wide run caught the new corpus entries missing from the
plan-shape golden (`TestPlanShapeGolden`). Regeneration changed only the corpus
header and added the two new queries' plan/shape entries; no existing query's
plan changed. The new snapshot is included, not an expectation relaxation for
an existing query. The corrected explaindiff target then ran uncached and
passed, including `TestPlanShapeGolden`.

`just test` subsequently passed all 92 Bazel targets (one executed in that
invocation, 91 cached), in 121.238 seconds. This is explicitly not an uncached
full-suite claim: the earlier full run executed 17 targets and reported the
snapshot failure above; the affected executor/SQL/yamsql suites and corrected
explaindiff suite also have their own uncached passing runs. MD5 checks confirmed
the production source, both new Go tests, and the yamsql scenario were unchanged
through all final runs. The golden/corpus ledger updates are included.
