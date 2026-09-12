# RFC-251: SUM and AVG adopt their first non-NULL value

Status: implemented and verified; RFC and implementation reviews ACKed.

## Pre-implementation evidence and review

Uncached Bazel ran the new executor unit test and real-FDB SQL test before the
production edit. Both failed with `0000000000000000` where the expected bits
were `8000000000000000`. The FDB fixture-bit control passed, and failures
occurred for SUM/AVG on both DOUBLE and FLOAT under all four scan budgets.
A separate uncached yamsql execution ran the new scenario and failed both
queries: actual `[[0,2]]` versus expected `[[-0,1],[0,1]]`.

The initial combined filter executed no yamsql tests because a slash inside
the alternation was split as a subtest filter; its green is not evidence.
The corrected command used
`--test_arg=-test.run=^TestYamsqlConformance$/^aggregate_signed_zero$`, and its
output included the parent and scenario RUN lines and the two mismatches.

Before the production edit, both virtual reviewers (Claude Sonnet) ACKed:

- Graefe: session `c5cb3dea-e8dd-4be8-9a4f-35b09782ab43`.
- Torvalds: session `6e466ee5-d405-4e56-aca7-88184bc11e72`.

They independently checked the Java NumericAccumulator path, Go per-aggregate
count semantics, continuation state preservation, and regression dimensions.

## Finding

On `0a55930f6`, `aggregateCursor.accumulateRow` initializes the floating running
sum at positive zero and adds every operand to it. IEEE addition of `+0.0` and
`-0.0` produces `+0.0`; a group containing only negative zeros therefore loses
its sign in both SUM and AVG. This is observable beyond formatting: grouping
those results must distinguish them from positive-zero aggregate results.

The new `TestAggregateSumAvgSignedZero` checks result bits for SUM and AVG,
including empty/all-NULL inputs, one/multiple negative zeros, intervening NULLs,
positive zeros, both mixed-zero orders and finite cancellation. It round-trips
partial state at every split in each input. `TestFDB_AggregateSumAvgSignedZero`
checks SQL over real FDB for DOUBLE and FLOAT, grouped and scalar execution,
with scan budgets 1, 2, 3 and an effectively unlimited budget. It first checks
that the fixture really stored negative-zero bits and asserts a StreamingAgg
plan. The yamsql scenario groups SUM/AVG outputs, expecting two singleton
zero groups rather than one merged group; ordinary numeric row equality alone
cannot distinguish signed zero.

## Java reference and decision

Read in full: core `cursors/aggregate/DoubleState.java` and
`query/plan/cascades/values/NumericAggregationValue.java` (4.12.11.0).

These are different algorithms. The older `DoubleState` does seed SUM with
positive zero. The Cascades SQL reference is **NumericAccumulator**, whose
state starts as NULL. `PhysicalOperator.evalPartialToPartial` returns the
other operand directly when either partial is NULL. SUM_D adopts the first
double; AVG_D adopts its `(double, count=1)` pair. SUM_F/AVG_F have the same
initial-state behavior. Later partials are added normally.

Use the existing per-aggregate non-NULL count (`gs.counts[i]`, incremented
before the SUM/AVG switch) to distinguish absent state from a real sum of
zero: on count 1 assign the floating running sum to the operand; otherwise add.
Do not use `gs.count`, which also counts NULL operands, or compare the sum to
zero, which would overwrite legitimate cancellation/mixed-zero states.

Retain integer accumulation and overflow checks, AVG's exact integer sum,
result types, planner rules, grouping identity, and the continuation layout.
The count and floating sum already survive continuation serialization, so the
same rule works after a page containing only NULLs and after a non-NULL partial.
No storage/index bytes or continuation schema change is needed.

## Performance and verification

This adds one predictable count comparison per SUM/AVG operand and no new
allocation or retained state. Measure the existing 1M SQL stress workload
before and after, twice per source, sequentially on the same filesystem and
Bazel output base. Record source identities, RUN populations, row/plan
signatures, loads and timings; do not claim a speedup from this correctness fix.

Verification requirements (completed):

- Observe the new unit, SQL and yamsql regressions fail under uncached Bazel.
- Obtain Graefe and Torvalds RFC ACK before editing production code.
- Run the affected targets uncached and `just test` on frozen source bytes.
- Confirm the new test files appear in their Bazel target source lists.
- Run implementation review once at completion, plus Codex review.
- Retain the reproducers, including the SQL fixture-bit control and the
  positive-zero/cancellation/NULL controls. No test skips beyond no Docker.

## Implementation reviews and affected-target verification

Production blob: `21290e07d205e24d37ab1842801a9ad7aa702414` (before:
`930ee118157ea3b6510adff633058ee0f210a123`). Both implementation reviewers
ACKed the production code and all three regression files:

- Graefe: session `cb60e16c-d391-4c6a-8d98-cd21e2a55d2f`.
- Torvalds: session `ae1086a6-e382-4fa0-b642-9607a2982038`.
- Independent Codex review: no findings; verified first-partial adoption and
  continuation state preservation.

Uncached full affected targets passed: executor (1,839 RUN events), sqldriver
(6,561), yamsql (422). Within those populations the new unit regression ran
its parent and 54 leaf cases; 22 of those leaves failed before the fix and all
54 passed after. The real-FDB regression reported seven groups and eight
scalar queries at each of four scan budgets; the new yamsql scenario reported
2/2 queries passed. MD5 checks confirmed the production Go file, both Go
regressions and the YAML scenario were unchanged across that execution.

The first full-suite run exposed a missing generated-artifact update:
`TestPlanShapeGolden` rejected the two new corpus queries. Regenerating with
`go run ./cmd/explain-differ dump` changed the population header and added
exactly their two entries. Comparing the 2,955 pre-existing golden entries
against `0a55930f6` found their contents unchanged. No existing query plan or
expected result was re-blessed to hide a failure.

## Completed 1M stress comparison

Measured on 2026-09-12. Before is `0a55930f65828b12e1ab1de1c3c56d2f7c11d489`,
the merge-base at measurement time. After is that same base with only
`pkg/recordlayer/query/executor/streaming_cursors.go` replaced by blob
`21290e07d205e24d37ab1842801a9ad7aa702414`. Both source states ran in the
same detached worktree under `/var/tmp/query-hunt-signed-zero/compare` and
reused the same Bazel output base. `/dev/nvme1n1p3` was 57% used with 376 GiB
available; neither measured tree used the nearly full `/home` filesystem.
Each state's production blob was checked before and after each sample.

All four sequential uncached executions emitted 24 RUN events (parent plus
23 query arms), identical row counts for the 22 timed queries below, the
same 1,000,000-row COUNT assertion and identical text for all 11 emitted
EXPLAINs. These are the harness's emitted populations, not a claim that it
compares all million row values or emits a plan for every arm.

| Source | Test time 1 | Test time 2 | Start load averages (1/5/15 min), samples 1; 2 |
|---|---:|---:|---|
| Before | 192.22 s | 192.33 s | 5.87/3.44/2.90; 4.09/3.97/3.25 |
| After | 191.34 s | 191.26 s | 2.38/3.51/3.25; 4.14/3.92/3.46 |

Command (twice per source, before samples finished before after samples):

```sh
bazelisk --output_user_root=/var/tmp/query-hunt-bazel \
  --output_base=/var/tmp/query-hunt-bazel/df3c3a010e8fb322a73a329098a931e6 \
  test //pkg/relational/sqldriver/stress:stress_test \
  --nocache_test_results --test_output=streamed \
  --test_arg=-test.run=^TestFDB_Stress_1M$
```

| Timed query | Rows | Before 1 | Before 2 | After 1 | After 2 |
|---|---:|---:|---:|---:|---:|
| PK id=0 | 1 | 10.645 ms | 10.348 ms | 12.228 ms | 9.120 ms |
| PK id=N/2 | 1 | 9.639 ms | 8.324 ms | 9.805 ms | 8.637 ms |
| PK id=N-1 | 1 | 6.318 ms | 8.602 ms | 8.861 ms | 7.864 ms |
| Customer equality | 8 | 7.859 ms | 7.710 ms | 7.801 ms | 6.422 ms |
| Amount range >9000 | 100017 | 215.848 ms | 298.745 ms | 275.622 ms | 210.119 ms |
| Status count pending | 1 | 557.334 ms | 452.825 ms | 421.110 ms | 479.596 ms |
| Filter amount>5000 | 1 | 851.106 ms | 886.729 ms | 610.556 ms | 601.464 ms |
| GROUP BY status | 4 | 5.918 ms | 5.751 ms | 6.624 ms | 6.423 ms |
| GROUP BY status COUNT | 4 | 5.468 ms | 5.091 ms | 6.466 ms | 5.479 ms |
| SUM by status (index) | 4 | 5.679 ms | 7.247 ms | 6.631 ms | 5.918 ms |
| GROUP BY customer HAVING | 47271 | 617.092 ms | 669.914 ms | 879.193 ms | 848.305 ms |
| Join ten orders | 10 | 22.998 ms | 23.709 ms | 20.890 ms | 45.068 ms |
| ORDER BY PK full | 1000000 | 4.318 s | 4.326 s | 4.212 s | 4.169 s |
| ORDER BY PK + index filter | 8 | 10.388 ms | 10.782 ms | 9.725 ms | 9.867 ms |
| Narrow ordered scan | 1000000 | 3.999 s | 4.060 s | 4.055 s | 3.977 s |
| Wide scan | 1000000 | 4.354 s | 4.397 s | 4.326 s | 4.314 s |
| IN-list five values | 46 | 25.924 ms | 21.142 ms | 25.530 ms | 21.886 ms |
| PK needle | 1 | 6.227 ms | 6.096 ms | 6.269 ms | 6.000 ms |
| PK + filter needle | 1 | 8.004 ms | 8.641 ms | 8.667 ms | 8.159 ms |
| Sparse full-scan filter | 97 | 3.635 s | 3.671 s | 3.682 s | 3.677 s |
| UPDATE by index | 8 | 10.244 ms | 9.828 ms | 9.348 ms | 9.624 ms |
| DELETE single row | 1 | 7.496 ms | 7.987 ms | 6.721 ms | 8.149 ms |

No speedup or bounded-regression claim: individual timings moved in both
directions, including the customer-HAVING query. That query's emitted plan
uses maintained COUNT/SUM aggregate indexes, not the changed streaming-SUM
accumulator. The workload is a broad regression control, not a measurement of
signed-zero aggregation performance. PK lookups exceeded the aspirational
5 ms target on both source states. Correctness of the changed numeric path is
established by the dedicated unit/FDB/yamsql regressions above.

## Full-suite result

`just test` passed all **92/92 Bazel test targets** after the golden update
(2 executed, 90 cached; 116.697 s). The preceding forced-uncached full run
actually executed all 92 targets on the final production/test bytes: 91
passed, and the stale EXPLAIN golden was its sole failure. The final run
re-executed explaindiff and docscheck; the other targets reused those verified
results. No failed test was skipped or expectation weakened.

The new files are in Bazel `srcs` (and the executor's `embedsrcs`), generated
feature/SQL coverage ledgers and the EXPLAIN golden are updated, and
`git diff --check` passed. MD5 checks before and after the runs confirmed the
production Go file, both Go regression files and the YAML scenario did not
change. A temporary PATH wrapper put Bazel output under
`/var/tmp/query-hunt-bazel`; it added `--nocache_test_results` only for the
forced-uncached full invocation. The `just test` recipe itself was unchanged.

Completion is also recorded in TODO.md's RFC-251 block.
