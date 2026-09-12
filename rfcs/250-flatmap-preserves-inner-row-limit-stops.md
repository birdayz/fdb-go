# RFC-250: FlatMap preserves every non-exhausted inner stop

Status: implemented; original RFC and source reviews ACKed; merge-gate repairs under final verification.

## Finding and scope

At `0ebf8c715`, `flatMapCursor.OnNext` closes an inner cursor and advances the
outer whenever its no-next reason is not out-of-band. This conflates
`ReturnLimitReached` with `SourceExhausted`. A returned-row budget does not prove
that the inner stream is exhausted; advancing loses its remaining rows and its
continuation.

`TestFlatMapInnerRowLimitPreservesContinuation` constructs the cursor's mid-inner
state using a real `recordlayer.LimitRowsCursor` over two list rows. After the
first row, FlatMap reports `SourceExhausted` instead of `ReturnLimitReached` and
loses the second row's resume position. The new test ran and failed under
uncached Bazel (`//pkg/recordlayer/query/executor:executor_test`), not just Go.

This is a confirmed **internal cursor-contract bug**, not a demonstrated SQL
wrong-answer defect. `executeFlatMap` clears request skip/row limits for both
children; a semantic `RecordQueryLimitPlan` normally reports `SourceExhausted`
when its window finishes. `TestLimitedJoinContinuation` pins scalar-subquery
and derived-join LIMIT/OFFSET answers at six scanned-row budgets (unlimited,
1, 2, 3, 4, 7). It also pins the existing typed rejection of correlated EXISTS
with a positive data-dependent OFFSET: that input cannot exercise this cursor
branch. No claim is made that these shapes reach the faulty row-limit arm.

## Java reference and decision

Read in full: `cursors/FlatMapPipelinedCursor.java`, `RowLimitedCursor.java`,
`SkipCursor.java` under `fdb-record-layer/fdb-record-layer-core/src/main/java/`
(tag 4.12.11.0).

`FlatMapPipelinedCursor.PipelineQueueEntry.doesNotHaveReturnableResult` discards
an inner only when `innerResult.getNoNextReason().isSourceExhausted()`.
`nextResult` otherwise propagates the inner stop reason and `toContinuation`
pairs the prior outer position with the inner position. The Go record-layer
`flatMapCursor[T,V]` already uses this same distinction.

Change the executor's inner-stop condition to `!reason.IsSourceExhausted()`.
Keep the existing continuation construction and sticky terminal-result cache.
This is the same algorithm as Java, not a new plan/rule/property or special
case for one particular stop reason. Completed semantic LIMIT windows remain
source-exhausted and still advance normally.

Rejected alternatives:

- Treat a returned-row limit as exhaustion: loses rows and contradicts Java.
- Resume the inner immediately within this cursor instance: ignores the page
  boundary and its budget; continuation resumption belongs to the caller.
- Add a second condition naming just `ReturnLimitReached`: duplicates the
  source-exhaustion distinction already represented by `NoNextReason`.
- Change request propagation or SQL LIMIT semantics: unnecessary and much
  broader than the defective classification.

No stored keys, record/index bytes, or continuation schema change. Existing
FlatMap continuation encoding carries the checkpoint that was being discarded.

## RFC review

Both virtual reviewers read the Java and Go stop/continuation paths before the
production edit (Claude Sonnet, 2026-09-11):

- Graefe: **ACK**, session `ae1a1659-ce01-464d-ab3a-088e9e3e2dba`.
  Confirmed the source-exhaustion gate and the narrowly stated SQL reachability.
- Torvalds: **ACK**, session `a7e3e80c-7cc0-48a2-b1e2-a7d43a682932`.
  Confirmed the same distinction and required the planned real-FDB child-cap
  regression before completion.

The real-FDB regression was then run before the production edit. Under inner
caps of 1 and 2 it returned `[1 1 1]` and `[1 2 1 2 1 2]`, respectively, instead
of `[1 2 3 1 2 3 1 2 3]`. Both subtests ran and failed under uncached Bazel.
The test explicitly overrides the normally cleared child request cap; its
scope remains an internal executor contract, not ordinary SQL reachability.

## Implementation review

The production file's reviewed Git blob is
`230bfc5e1503ebe05f499949e3a7a26f1597dda4` (baseline blob
`8e60ff7287ce7c4f552eb03d00aaf0571a4aee9d`). The source and regression tests were
frozen for the reviews and subsequent verification:

- Graefe: **ACK**, session `cc9d9747-fc73-4b93-8d3c-2d3cd9701d65`.
- Torvalds: **ACK**, session `59833c52-8fe9-40ea-8224-6d2b5378d37a`.
- Codex: no findings; confirmed the stop classification, continuation handling,
  tests, and Bazel wiring in a single uncommitted-change review.

To distinguish the evidence precisely: the **unit regression and the two
budget subtests of the real-FDB regression** failed on the old source and
passed on the new source. The **SimFDB SQL controls passed on both sources**;
the initial EXISTS probe failed because its expected result overlooked the
existing unsupported-query gate, not because it exposed row loss. The final
control asserts that typed rejection. None of these SQL controls is claimed as
a reproducer of this bug.

The executor target's `TestMain` calls `foundationdbtc.Run` with only the API
version option; the underlying `GenericContainerRequest` has `Started: true`
and no reuse option. All reported targeted Bazel executions disabled test
result caching. Fast startup is not evidence of a mocked or skipped FDB test.

## Verification requirements (fulfilled)

1. Retain the originally failing row-limit regression and verify its checkpoint's outer
   position, inner position, primary-key check value, resumed tail, and sticky
   no-next replay.
2. Exercise a genuinely row-limited FDB scan as the active inner and resume the
   FlatMap from the emitted checkpoint to prove all remaining rows are delivered.
3. Preserve natural inner exhaustion and scan/time/byte-limit handling; run the
   existing FlatMap continuation and terminal-replay tests.
4. Run the SQL LIMIT/OFFSET pagination controls above, the affected Bazel
   targets uncached, and `just test`. Record source hashes for final runs.
5. Measure the 1M stress target twice before and twice after, sequentially, with
   both trees on the same filesystem with adequate free space. Report exact
   source revisions, test populations, and limitations rather than calling this
   a SQL performance improvement.
6. Graefe + Torvalds RFC ACK before the production edit; joint implementation
   review and codex review after the targeted regressions pass. Complete the
   full-suite checks before handoff. No merge/push is requested.

### Pre-amendment FlatMap execution results

- Before the later nightly-triggered cardinality amendment, `just test`: **92 of
  92 Bazel test targets executed and passed**, including
  the committed cross-engine factory corpus. Elapsed time was 1163.535 s;
  this was a fresh output base, not a cached green. A temporary `bazelisk`
  PATH wrapper added `--output_user_root=/var/tmp/query-hunt-bazel` to keep
  builds off the nearly full `/home` filesystem; the recipe itself was unchanged.
- A subsequent uncached full run of `//pkg/recordlayer/query/executor:executor_test`
  and `//pkg/simfdb/hunt/sqlpage:sqlpage_test` passed. Its output included the
  unit regression RUN line, both FDB budget subtest RUN lines and **2**
  `FLATMAP-ROW-LIMIT` reports, plus **18** `LIMIT-CONTINUATION` reports (3 SQL
  controls × 6 configured budgets). FDB returned all nine rows with respectively
  six and three actual row-limit stops at budgets one and two.
- `FuzzFlatMapContinuation` in `//pkg/recordlayer:recordlayer_test`: **4,985,804
  executions in 15 seconds**, four workers, all five seed cases consumed, PASS.
  This exercises the existing generic FlatMap continuation fuzzer, not a new
  SQL reachability claim or a replacement for the executor regression.
- The modified production Go file and all three modified/new Go test files
  were MD5-checked unchanged before and after the full suite, the uncached
  affected-target run, and fuzzing. `git diff --check` passed.

### Completed 1M stress comparison (2026-09-11)

Baseline is `0ebf8c7155544d2cd5e7908d10b74f1ca6910964`, the merge-base at
measurement time. The after source is that same revision with only
`pkg/recordlayer/query/executor/flat_map_cursor.go` replaced by reviewed blob
`230bfc5e1503ebe05f499949e3a7a26f1597dda4`; the baseline file blob is
`8e60ff7287ce7c4f552eb03d00aaf0571a4aee9d`. This identifies the measured engine
by its content identity, since the after state was uncommitted when measured.

Both source states ran sequentially in the same detached worktree under
`/var/tmp`, on `/dev/nvme1n1p3` with 388–391 GiB available (56% used). The main
checkout's `/home` filesystem was nearly full, so neither measured source state
used it for the worktree or Bazel output. The same Bazel output base was reused
between states, and the changed production file was MD5-checked after each run.

Command, repeated twice per source state:

```sh
bazelisk --output_user_root=/var/tmp/query-hunt-bazel test \
  //pkg/relational/sqldriver/stress:stress_test --nocache_test_results \
  --test_output=all --test_arg='-test.run=^TestFDB_Stress_1M$'
```

| Source state | Sample | Test duration | Start load average (1/5/15 min) |
|---|---:|---:|---|
| Baseline | 1 | 216.54 s | 1.63 / 2.37 / 2.39 |
| Baseline | 2 | 199.90 s | 6.52 / 6.82 / 4.54 |
| After | 1 | 197.00 s | 3.22 / 5.46 / 4.53 |
| After | 2 | 198.06 s | 3.88 / 4.74 / 4.43 |

All four uncached runs passed. Each executed the identical **24 RUN lines
(parent plus 23 query subtests)**. Their 22 timed result-row reports matched
exactly; the remaining `full_scan_count` arm independently reported
`COUNT(*) = 1000000 (expected 1000000)` in every run. All 11 emitted EXPLAIN
texts also matched across the four runs.

This is not evidence of a speedup: this SQL population does not demonstrate
reachability of the changed arm, load varied, and the first baseline overlapped
short targeted-regression builds/runs. The point-lookup samples were 6.22–18.64 ms
(across the three `pk_lookup_*` queries and four runs), above the aspirational
5 ms target on both states. The complete timed query population is recorded
below so total-duration improvement cannot hide an individual movement.

| Query | Rows | Before 1 | Before 2 | After 1 | After 2 |
|---|---:|---:|---:|---:|---:|
| pk_lookup_first | 1 | 8.718 ms | 11.485 ms | 10.770 ms | 18.401 ms |
| pk_lookup_middle | 1 | 8.343 ms | 11.357 ms | 9.865 ms | 18.635 ms |
| pk_lookup_last | 1 | 6.219 ms | 8.755 ms | 8.570 ms | 14.462 ms |
| index_customer_eq | 8 | 6.357 ms | 6.875 ms | 6.925 ms | 19.173 ms |
| index_amount_range | 100017 | 214.504 ms | 216.461 ms | 207.569 ms | 314.599 ms |
| index_status_count | 1 | 491.167 ms | 480.304 ms | 482.734 ms | 375.810 ms |
| full_scan_filter | 1 | 897.582 ms | 593.537 ms | 882.669 ms | 839.123 ms |
| group_by_status | 4 | 6.972 ms | 6.851 ms | 12.517 ms | 13.646 ms |
| group_by_status_count_only | 4 | 6.816 ms | 5.453 ms | 15.081 ms | 18.409 ms |
| sum_by_status | 4 | 6.066 ms | 6.571 ms | 23.417 ms | 11.934 ms |
| group_by_customer_having | 47271 | 684.886 ms | 808.857 ms | 726.601 ms | 778.835 ms |
| join_10_outer | 10 | 21.175 ms | 41.496 ms | 36.963 ms | 23.845 ms |
| order_by_pk_full | 1000000 | 4.344 s | 4.321 s | 4.227 s | 4.203 s |
| order_by_pk_index_filter | 8 | 10.445 ms | 11.563 ms | 9.259 ms | 10.589 ms |
| scan_all_narrow | 1000000 | 4.072 s | 4.347 s | 4.036 s | 4.025 s |
| scan_all_wide | 1000000 | 4.377 s | 4.378 s | 4.257 s | 4.355 s |
| in_list | 46 | 21.657 ms | 22.133 ms | 21.851 ms | 20.665 ms |
| needle_in_haystack_pk | 1 | 6.526 ms | 6.803 ms | 5.988 ms | 6.904 ms |
| needle_in_haystack_filter | 1 | 8.890 ms | 9.346 ms | 9.041 ms | 8.193 ms |
| full_scan_sparse_filter | 97 | 3.659 s | 3.617 s | 3.607 s | 3.638 s |
| update_by_index | 8 | 9.736 ms | 9.785 ms | 9.497 ms | 10.802 ms |
| delete_single_row | 1 | 7.881 ms | 7.499 ms | 7.684 ms | 6.855 ms |

## Merge-gate nightly triage (2026-09-11)

PR #780 exposed red nightly results at baseline `0ebf8c715`, independently of
its passing local suite. These are a merge hold, not a claim that the FlatMap
change repairs the nightly nets:

- Stress run **34581663684**: the FDB container exited with code 1 and
  `oomkilled=false`. The concurrent benchmark error collector then panicked:
  `atomic.Value.CompareAndSwap(nil, err)` rejects differing concrete error
  types even when the slot is already occupied. The shared test-only
  `firstStressError` now uses `atomic.Pointer[error]`; both benchmark files use
  it. The mixed-type unit regression failed on the old holder and passed on
  the new one, along with the concurrent case and all seven raw-ingest
  configurations. **The underlying CI FDB container exit remains under
  investigation; fixing its error reporter does not fix that exit.**
- Engine-fuzz run **34581554992**: `FuzzPlanCacheScope_Injective` ran 29,283,265
  iterations then reported `context deadline exceeded` at its 90-second
  boundary. The complete log shows `cmd/fuzzrun` already classified this as
  the Go coordinator cancellation race (golang/go#72104), with its existing
  positive-shape classifier and regression tests; it was not the failure
  that reddened this job. `FuzzPlanner_LimitOverUnion_NoPanic` did redden it:
  the harness panicked on a correctly returned `InvalidLimitOffsetError`
  before planning began. Its exact saved input (`1788cc2b9ac05503`) is
  limit 10, offset -4, byte 0, verified against the corpus SHA-256 prefix.
  That seed is now in `f.Add` and reproduced the panic under uncached Bazel.
  Both offset-bearing topology fuzzers now mask the sign bit to generate
  valid offsets, preserving every nonnegative input and safely mapping
  `MinInt64`. Negative-offset rejection remains independently asserted by
  `TestLogicalLimit_RejectsNegativeOffset`, including -4. No planner or
  constructor behavior is changed by that fixture repair. The exact seed now
  passes, and `FuzzPlanner_Limit_NoPanic` completed 572,561 executions in 15
  seconds. Active fuzzing of the UNION property then exposed a second, genuine
  planner defect with input `(limit=0, offset=-8, branches=0)`: after masking,
  the valid offset is `MaxInt64-7`; limit pushdown gives both UNION legs that
  finite maximum, and `UnionCardinalities` wraps their sum negative before
  `OfCardinality` panics. This exact seed is retained alongside a direct
  cardinality regression.

  The corresponding Java `CardinalitiesProperty.CardinalitiesVisitor` was read
  in full. Its `unionCardinalities` also adds raw `long` values and would throw
  through `Cardinality.ofCardinality` on overflow. Go already closes that Java
  boundary defect for multiplication: `Cardinality.Times` maps an
  unrepresentable bound to unknown, because unknown conservatively weakens a
  proof while wraparound, saturation, or a panic does not. Apply the identical
  policy to addition: add `Cardinality.Plus`, return unknown when either operand
  is unknown or their nonnegative sum exceeds `MaxInt64`, and make both UNION
  bound channels use it. A direct primitive test covers known/unknown operands
  in both orders, zero, the representable `MaxInt64` boundary, and overflow in
  both orders. The UNION regression separately pins a representable `MaxInt64`
  boundary, overflow of only the maximum (the exact fuzz topology), and overflow
  of both minimum and maximum. Skipping limit pushdown only for
  `LIMIT 0` was rejected because it hides this seed while leaving any two large
  finite UNION legs able to panic; wrapping, saturating an upper bound, and
  recovering the panic are unsound. This RFC amendment requires Graefe and
  Torvalds ACK before the production edit, then exact-seed replay, both active
  topology fuzzers, uncached affected suites, `just test`, and a fresh 2-before /
  2-after 1M stress comparison.

  The amendment received both required pre-production reviews (Claude Sonnet,
  2026-09-11): Graefe **ACK**, session
  `4cdbed8a-c3c1-461d-ac54-83e59a50b373`; Torvalds **ACK**, session
  `788c7f07-84cd-4714-b61f-895251bf303a`, conditional on the direct `Plus`
  primitive test described above. That test was added before the production edit.

  Verification of the amendment is non-vacuous. Before `Plus` existed, the
  direct UNION test ran under uncached Bazel and panicked in `OfCardinality` on
  the exact max-only-overflow arm. After the production edit, the primitive and
  UNION tests passed, and all nine retained UNION topology seeds passed. Both
  affected targets then passed in full under uncached Bazel. Fresh active fuzz
  runs passed 570,641 general LIMIT executions and 202,730 UNION executions in
  15 seconds each, after consuming respectively all 11 and all 9 seed inputs.

  The required fresh 1M comparison used one worktree on `/dev/nvme1n1p3`
  (57% used), sequentially before and after. Baseline was
  `0ebf8c7155544d2cd5e7908d10b74f1ca6910964`; the after state was that revision
  with only the two PR production blobs applied: FlatMap
  `230bfc5e1503ebe05f499949e3a7a26f1597dda4` (baseline
  `8e60ff7287ce7c4f552eb03d00aaf0571a4aee9d`) and cardinality
  `02966a7a8d19566f480b7eeb50daff30515015be` (baseline
  `7f3bdfeb17ab9c9d4e022d5e267f606485445d03`).

  | Source | Sample | Test duration | Start load average (1/5/15 min) |
  |---|---:|---:|---|
  | Before | 1 | 202.23 s | 9.33 / 7.45 / 4.58 |
  | Before | 2 | 198.32 s | 2.72 / 5.89 / 4.95 |
  | After | 1 | 198.21 s | 5.04 / 5.21 / 4.84 |
  | After | 2 | 198.03 s | 3.94 / 4.55 / 4.65 |

  Every run had 24 RUN lines, 22 timed result reports, the independent 1M
  `COUNT(*)` assertion, and 11 EXPLAIN lines. Normalized row signatures were
  byte-identical across all four logs (SHA-256
  `f0c64f51135738fb51b434deea91a4a6baef982e0a3e899275078f1d1c222f29`), as
  were EXPLAIN texts (`48ffbbf33c31a7f9696cdbfd4aced3afe3a2591578794df47b86711e76230ccb`).
  The durations do not support a performance-change claim; point lookups were
  6.09–18.24 ms on both source states, above the aspirational 5 ms threshold.
  The first run's completed output initially tripped the verification wrapper
  because it searched for `rows=` rather than the harness's actual `N rows`
  spelling; the corrected parser found exactly 22 reports in that retained log,
  and then verified the same population in all four logs.

  The repository-wide `just test` then passed all 92 targets in 824.879 seconds:
  45 executed in that invocation and 47 were served from Bazel's cache. This is
  not substituted for the uncached affected-target runs above. MD5 verification
  after the suite confirmed that all seven changed planner/fuzz/stress Go files
  matched the bytes hashed before it began.
- RowDiff run **34562900341**: both sweeps lost the FDB connection, exhausted
  their consecutive-INFRA guard, and fell below the seed floor. The downloaded
  forensic artifact is `rowdiff-fdb-forensics` from that run. No wrong-row
  mismatch was reported in the measured prefixes; the incomplete sweeps are
  not a clean engine verdict. Read-only SSH inspection established that both
  runners still had the obsolete age-only sweep, not the corrected script
  already in `infra/cloud-init.yaml`. The `gh-runner-fdb` journal confirms:

  ```text
  Sep 11 05:13:29 killing orphan FDB container 452337ebd901 (foundationdb/foundationdb:7.3.77, running 2106s)
  Sep 11 05:55:21 killing orphan FDB container 6705fe762b67 (foundationdb/foundationdb:7.3.77, running 1909s)
  ```

  These IDs and times match the RowDiff forensic artifact's disappearances.
  The existing `//infra:infra_test` passed uncached, including
  `TestOrphanFDBSweepScript`. The template's rendered script was then
  atomically deployed to both runners; both installed SHA-256 hashes were
  `b0bd6ed8b60646f7aba78df93f2fef9b72d60aa99849094bf9510b2a4c5b70a5`.
  Both sweep timers remained active and both original `Runner.Worker` PIDs
  survived unchanged. Deployment mechanics and future update obligations are
  recorded in `infra/README.md`.

  Post-deployment RowDiff run **34673258982** succeeded on
  `gh-runner-drain-0` from 2026-09-12T04:32:11Z through 09:14:07Z. Its deep
  sweep executed 12,396 of 15,000 seeds within the normal 3h30 budget; its
  later paging sweep used a second FDB container and executed 932 of 5,000
  seeds within the normal 1h10 budget. The deep-sweep container remained live
  until normal teardown while the worker-aware sweep service started 40 times,
  including 35 starts after the 30-minute age threshold, and logged zero
  `killing orphan FDB container` decisions.
  The retained sweeper journal SHA-256 is
  `e9e4fcef288f3a231db9013730695ab89e5bb18ec1cd2004c21808a275c00022`;
  the last-inspect and RowDiff-output artifact SHA-256 values are respectively
  `c2986889bbec626b526ee2cd1569618681a64806385997ac405381b35696738c`
  and `0514b754a43231d2dc65d0dded64220ffb941736d16e99ae5855303561301259`.
  This observed keep decision closes the corrected-timer hold. The separate
  Factory and Coverage OOM causes are resolved below.
- Factory run **34574477084** and Coverage run **34572810594** were interrupted
  by runner shutdown. The later stress job's kernel log identifies an OOM
  kill of the factory process (PID 2391708, about 7 GiB anonymous RSS). Coverage
  stopped during its race phase and did not publish a fresh heartbeat.
- PR CI run **34613512453**: the Bazel suite and race lane passed, but the
  separate `tools/bazelscaleset` module failed
  `TestAdoptedRunnerWatchdogReclaims`: after 10 seconds its terminal watchdog
  had not reclaimed the adopted zombie runner. The repair and regression are
  recorded below; a passed Bazel suite does not cover this module's gate.
- Reconcile run **34604652455** identifies the stale Coverage heartbeat and
  missing required checks on open PRs #486, #579, #745, and #747. Those PRs
  are not authorized merge targets of this task; their owner decisions must
  not be silently substituted by merging or closing them here.

Run URLs are `https://github.com/birdayz/fdb-go/actions/runs/<run ID>`. This
section records the live investigation so the findings cannot disappear
behind the completed FlatMap checkbox.

The Factory OOM was eager corpus retention, not corpus execution: `NewBatch`
kept all 437 parsed family files for the whole generation run, and `Finish`
loaded the complete corpus again for its census. The repair seeds dedup/name
indexes one family at a time with detached strings, loads only families a
batch appends, and computes the census one family at a time while preserving
empty-corpus and cross-family uniqueness guards. Identical 400-seed/1,000-commit
probes reduced maximum live heap from 2,969 MiB to 1,499 MiB while preserving
all manifest counts (2,127 generated/executed, 1,000 committed, 9,150 census
scenarios). Regressions pin detached backing storage, preservation across a
lazy cross-batch append, streaming/full-loader census parity, empty-corpus
rejection, and both cross-family duplicate classes. Mutating away either lazy
load or string detachment reddens the exact regression after one RUN event.

Coverage run **34679494723** exposed a separate classic-runner lifecycle bug:
the kernel selected `Runner.Listener`, while the installed unit's
`OOMPolicy=stop` stopped the service and its `Restart=no` left it dead. Classic
mode now installs `OOMPolicy=continue` with `KillMode=process`; if the listener
dies while `Runner.Worker` survives, the watchdog waits for that worker rather
than starting a second listener that can claim a concurrent job. The infra
shell suite pins both coupled arms and its self-match-safe process probe.

### Standalone-module stdout diagnostic correction

The failing PR step was `GOWORK=off go test ./... -count=1`, not `-race`.
The original log contains no adoption, liveness, or signal diagnostics, so it
cannot identify which individual signal delivery failed. The lifecycle defect
is nevertheless concrete: `watchTerminal` issued one fire-and-forget SIGKILL
and returned without observing process death, while the package's teardown path
already treats signal delivery and observed death as separate states. The
watchdog now remains live and repeats SIGKILL on its poll interval until the
process wait path closes `done`; one failed local syscall or remote command can
no longer strand the runner. A synthetic probe requires two kill requests
before reporting death, so restoring the one-shot return runs that regression once
and fails it once. The original adopted-runner test and the new retry pin passed
100 repetitions together. The regression also verifies its pidfile setup,
retains adoption and signal logs through cleanup, and prints tracked-runner and
`/proc` state on timeout. One waiter owns child reaping; cleanup joins it and
the watchers.

An isolated replay without a package argument exposed a separate deterministic
failure: after the watchdog test passed, `TestMain` reported the parent `go`
process as a leaked child. Go streams its own stdout to the test binary in
this invocation mode (`cmd/go/internal/test/test.go`, `streamOutput`). The
scanner now excludes the live ancestor chain and ignores regular-file/terminal
outputs, which have no pipe EOF to obstruct. Subprocess regressions run an
actual test and its `TestMain` with shared file and shared pipe outputs; both
failed before the fix. Independent tests retain detection of a child writer
and rejection of a pipe reader and regular-file holder. Removing each of the
ancestor, file-mode, and writable-descriptor filters independently reddened
its corresponding regression. This repairs the diagnostic independently of the
verified-death watchdog repair above.

### FDB file-allocation failure captured in final-head CI

CI run **34646187827**, head `6499b924a6f3b439ea85497ad06d574bdcd49940`,
lost the full factory-corpus container `ce0fab07265f` at
2026-09-11T21:07:05Z, 72 seconds after startup. Docker recorded exit code 1,
`OOMKilled=false`, and no container kill event. The retained server trace
contains `AsyncFileKAIOAllocateError`, `UnixErrorCode=4` (`EINTR`), for
`/var/fdb/data/logqueue-...-1.fdq`, followed by `RDQPushAndCommitError`,
`SharedTLogFailed`, and `StopAfterError` (`io_error`, 1510). Stdout says
`Fatal Error: Disk i/o operation failed`. The test then cascaded through
scenario deadlines against the dead database. After copying Docker state,
events, stdout, trace, and test output, the investigator sent SIGQUIT to this
specific test process to stop the cascade and collect its goroutine dump.
That intervention is not the cause of the earlier server exit. The standalone
runner-module step subsequently passed; this does not explain its older timeout.

Reference: FoundationDB tag 7.3.77, commit
`3ea44ce1d9003ad095e408039e1f755c319c4dfb`, matching the image's build label.
Read `fdbrpc/include/fdbrpc/AsyncFileKAIO.actor.h`,
`AsyncFileEIO.actor.h`, and `fdbrpc/Net2FileSystem.cpp`. KAIO's `truncate`
returns `io_error()` on every failed `fallocate` except `EOPNOTSUPP`;
it does not retry `EINTR`. EIO instead dispatches `eio_ftruncate` to its
worker pool. `DISABLE_POSIX_KERNEL_AIO=1` selects that supported backend.
The trace establishes the errno and fatal propagation, not which signal
interrupted the syscall. No claim is made that this alone explains the older
nightly stress exit, Factory OOM, or Coverage interruption.

Decision: default the Go test-container module to
`WithKnob("disable_posix_kernel_aio", "1")` for both tmpfs and on-disk data.
Keep explicit knob overrides available for callers testing KAIO itself.
This is a documented upstream workaround at the disposable server boundary,
not a Go client retry or a swallowed error. It changes no stored/wire bytes
and does not disable durability. Increasing a timeout, recreating an in-use
database, or disabling the server's profiling signals would hide symptoms
rather than remove the faulty allocation route.

The permanent Linux regression `TestRun_InterruptedFileAllocation` runs real
FDB with a container-local seccomp rule returning `EINTR` from `fallocate`.
It requires successful initialization and a committed value read-back on both
tmpfs and disk, plus a real `fallocate` command that proves the errno injection
is active. Required evidence: uncached Bazel red/green, full container-module
and factory-corpus targets, the full suite, and fresh final-head CI/reviews.
The earlier stress comparison predates this fixture-backend change and must
not be represented as measuring it.

Upstream report: [apple/foundationdb#14041](https://github.com/apple/foundationdb/issues/14041).
The pre-production design received Graefe ACK
(`41560737-1d5a-4f5b-8593-a87654c20528`) and Torvalds ACK
(`f86c062c-3e6a-4c00-a31a-ea9c65e4083b`), requiring the explicit override
regression and the full container/factory targets before merge. The default
flip then passed the targeted uncached Bazel run: both filesystem cases
logged their active fault control and committed read-back, and
`TestRun_KAIOOverride` reproduced the fatal allocation error with knob 0.
Before the default flip, both filesystem cases actually executed and failed
initialization under uncached Bazel. These are fault-injection results, not a
claim that every outstanding nightly failure has been explained.

### Sequential stress comparison including the EIO fixture default

The required comparison was repeated after the fixture-backend change; the
older table above does not measure it. Both states were built in the same
worktree and Bazel output root on `/dev/nvme1n1p3` (57% used at the start).
The baseline was `0ebf8c7155544d2cd5e7908d10b74f1ca6910964`. The after state
was that exact revision plus only these production blobs:

- FlatMap: `230bfc5e1503ebe05f499949e3a7a26f1597dda4` (before
  `8e60ff7287ce7c4f552eb03d00aaf0571a4aee9d`)
- cardinality: `02966a7a8d19566f480b7eeb50daff30515015be` (before
  `7f3bdfeb17ab9c9d4e022d5e267f606485445d03`)
- test-container defaults: `dfc0d1c6d42e2197ce4733146fc53ffb647fae5e`
  (before `79916394c9aab0300aedd3a95f9acfe912b7fd2e`)

The three-file patch SHA-256 was
`7340bdc1616961b185700344f2b073de168cec94b62de760ebcfcc1c98586717`.
Each file was MD5-checked after each run. Two baseline runs completed before
the two after runs; no test or container pipeline overlapped them.

| State | Sample | Test duration | Start load average (1/5/15 min) |
|---|---:|---:|---|
| Before | 1 | 198.97 s | 8.12 / 9.88 / 5.68 |
| Before | 2 | 198.18 s | 4.64 / 8.42 / 6.38 |
| After | 1 | 191.99 s | 3.16 / 5.91 / 5.80 |
| After | 2 | 188.32 s | 3.24 / 4.57 / 5.28 |

All four uncached invocations executed 24 RUN lines and passed. Each emitted
22 timed row reports, the independent `COUNT(*) = 1000000` assertion, and 11
EXPLAIN texts. Normalized rows were byte-identical (SHA-256
`f0c64f51135738fb51b434deea91a4a6baef982e0a3e899275078f1d1c222f29`),
as were plans (`48ffbbf33c31a7f9696cdbfd4aced3afe3a2591578794df47b86711e76230ccb`).
This supports no speedup claim: one after `ORDER BY PK` sample was 7.87 s
while the other three were 4.20–4.22 s. It also exposes an unexplained,
monotone point-lookup shift: every after sample (13.70–46.28 ms) was slower
than every before sample (9.17–12.52 ms), despite lower starting load in the
after runs, and every sample exceeded the aspirational 5 ms threshold. Of the
three changed production blobs, the fixture's KAIO-to-EIO switch is the one
with a plausible server-I/O mechanism. Total duration and result/plan identity
do not bound this individual latency. A fixture-only follow-up at the same
base, with three sequential samples per backend, did not reproduce the shift:
three PK lookups per sample were 5.01–18.99 ms under KAIO and 5.92–9.08 ms
under EIO. All six runs executed 24 RUN lines and passed; total durations were
181.37–189.92 s under KAIO and 169.45–170.12 s under EIO. This closes the
latency hold without claiming a speedup: the KAIO range contains one whole
slower sample, and three samples do not price throughput. The options-only
patch SHA-256 was
`0b5def0950bd18d862614557463d2b47079c5e0fe03d27c4901e2de4bb8bde05`.
The complete logs are retained with the PR evidence.
