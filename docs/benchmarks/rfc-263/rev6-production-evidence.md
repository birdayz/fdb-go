# RFC-263 revision 6: production implementation checkpoint

**Design accepted; implementation checkpoint, not merge or performance approval.**
Both official design reviewers ACKed `d364c1a5e` before the owner authorized this
production change. The selected canonical-identity renewal is now production code.
At this implementation-proof checkpoint, the 8-worker tail was still a merge blocker.
The later [fixed confirmation](rev6-confirm-results.md) cleared that p999 blocker by
the owner's preregistered rule, with power/epoch/order caveats retained. Full/full-race,
final-HEAD implementation/PR reviews and broader CPU qualification remain separate
owner-run gates. B3a/B3b are not implemented by this checkpoint.

## Implemented contract

- Sixteen stable round-robin producer shards, one globally ordered drainer, fixed
  cohort clock/frontier before every shard scan, eager tickets reserved while holding
  the publication lock, and `running/again` staggered-exit wake handling.
- Logical cancellation and later-key rearm reuse physical lower-bound stubs without
  taking a shard lock. All logical resumptions, including interval continuations,
  preserve C7 ordering. Already-expired C3 arms still record literal 1031 synchronously
  without publishing a new arm, ticket or timer request.
- With a strong transaction receiver and `readErrMu` held, `weak.Make(tx) != n.tx`
  retires the immutable old association and creates/publishes a fresh node, weak handle
  and cleanup on the **same shard**, before ordinary scheduling. Every retained
  future-positive rearm recovers after resurrection; recovery does not wait for a
  drainer to observe weak nil.
- One noncapturing cleanup per association unlinks only its own node when the owner
  becomes unreachable. Old cleanup/ready records never touch a replacement. Weak
  promotion and delivery remain outside shard locks.

The core matches the selected frozen identity prototype except for a descriptive
comment and removal of an unused private schedule argument. Legacy tests establish
transaction-owned logical state under `readErrMu`, inspect all sixteen heaps/root
ready state, and assert lazy physical cancellation rather than silently removing its
old assertions. Allocation pins remain exact: creation 9, first arm 4, live-node rearm
0, cold construction 49 (50 including the first runtime timer); a fresh
association is 5 allocations minus its separately measured 1-allocation owner.

The inherited idle/no-rearm resurrection loss, 100 ns `TIME_EPS` difference and delayed
retention of old cleanup arguments remain documented boundaries. Repeated resurrection
has no fixed bound on the number of retired cleanup arguments before final owner death,
cleanup execution and later GC. No armed-timeout continuity across finalization is claimed.

## Canonical regressions and source preflights

`timeout_queue_regression_unit_test.go` contains **20 top-level tests plus two
resurrection stage subtests**, in the ordinary fast `lease_alloc_unit_test` target.
Every top-level test and both stage subtests actually have RUN/PASS/XML records in the
nominal race, nominal opt and restored opt lanes below. Four controlled real-finalizer
scenarios cover weak-nil dispatch followed by SetTimeout, Reset with a positive default,
pop before weak read, and an immutable ready record captured before GC.

The canonical 20 are explicitly:

```text
TestTimeoutQueueLogicalContinuationFIFO
TestTimeoutQueueRearmAndDisarmNeedNoQueueLock
TestTimeoutQueueStubRepairIsNotActorResumption
TestTimeoutQueueEarlierKeyIsEager
TestTimeoutQueuePopRaceWithRearm
TestTimeoutQueueEagerSequencePublishedWithInsertion
TestTimeoutQueueCrossShardUnequalWakes
TestTimeoutQueueGlobalDrainerCallbackRescans
TestTimeoutQueueShardCohortFrontier
TestTimeoutQueueDeferredHeadDoesNotHideEligibleWork
TestTimeoutQueueStaggeredExitCallbackKeepsWake
TestTimeoutQueueCollectedTransactionUnlinksBeforeWake
TestTimeoutQueueCreateAllocationsWithCleanup
TestTimeoutQueueResurrectionAfterWeakNilDispatch
TestTimeoutQueueResurrectionReset
TestTimeoutQueueResurrectionRenewalAgainstCollectedNode
TestTimeoutQueueExistingNodeRearmAllocations
TestTimeoutQueueFreshAssociationAllocations
TestTimeoutQueueLayoutAndConstructionCosts
TestTimeoutQueueConcurrentArms
```

The two stage subtests are `popped-before-weak-read` and `immutable-ready-record`
under `TestTimeoutQueueResurrectionRenewalAgainstCollectedNode`. The separate
`TestTimeoutQueueInitializationAllocations` in `timeout_init_alloc_unit_test.go` also
runs in the same ordinary target; it is not one of these canonical 20. The generation
regressions below are another explicitly named population, not part of that count.

The harness has bounded waits, deferred worker completion closes and releases **all**
barriers before bounded group joins. No test is hidden behind a manual/full tag or skip.
`just gazelle` declared the source and passed before validation. Independent C++/semantics
and whole-diff quality preflights ACKed the implementation and migrated assertions;
these are not substitutes for the owner's official final-HEAD/PR gates.

## Whole fast-lane validation

All runs used `just test`, never a target list or test filter. The whole tracked and
untracked input/configuration tree was frozen and checked before/after each lane;
mutations changed only their recorded production bytes and restored exact nominal
bytes in `finally`. No test cache was disabled. Commands, SDK/compiler/linker identities,
source archives, BEP, per-target XML/logs and executed binary hashes are retained.

| Epoch / stage | Result | Actual / cached targets | Elapsed |
|---|---|---:|---:|
| Initial production fast lane | 155/155 pass | 155 / 0 | 213.452 s |
| Completed batch: nominal race | 155/155 pass | 148 / 7 | 814.809 s |
| Completed batch: nominal opt | 155/155 pass | 149 / 6 | 313.845 s |
| No-renewal mutant | 154 pass, 1 expected failure | 108 / 47 | 265.778 s |
| Eager-publication mutant | 154 pass, 1 expected failure | 108 / 47 | 246.348 s |
| Moving-frontier mutant | 154 pass, 1 expected failure | 108 / 47 | 262.970 s |
| Missing-`again` mutant | 154 pass, 1 expected failure | 108 / 47 | 245.654 s |
| Missing-cleanup mutant | 154 pass, 1 expected failure | 108 / 47 | 242.106 s |
| Completed batch: restored opt | 155/155 pass | 108 / 47 | 263.740 s |

Each mutant **compiled** and produced uncached named failures, not merely a nonzero
command exit. Four no-renewal cases bind `RESURRECTION_NODE_NOT_RENEWED`; eager binds
`EAGER_PUBLICATION_FIFO`; frontier binds both `SHARD_COHORT_FRONTIER` and
`DEFERRED_HEAD_COHORT`; missing-`again` binds `STAGGERED_EXIT_LOST_WAKE`; cleanup binds
`GC_CLEANUP_DID_NOT_UNLINK`. Every proof requires the exact RUN, FAIL, XML marker, BEP
status and hashed binary. Restored opt is byte-identical to the nominal opt executable.

Actual Bazel SDK is **Go 1.26.9**, not the host Go. The key regression executable hashes:

- Race: `8da468f9b24eaf3519553a2426dc5e420521afbc3586b780cc5d93ed9ade0f97`.
- Opt/restored opt: `a737268eeeb567076275e94456cf79c1f1b3f0ecbb644ed59de04a3607223d01`.

Bazel server resource receipts prove nice 19 and affinity
`0-1,6-7,11-13,18-19,23`; all lane commands inherit the same reservation. The runner
has four nonempty unit-test groups covering named assertion binding, vacuity/status/
timeout rejection, omitted-zero protobuf SUCCESS decoding, and mutation anchors.

This is **fast-lane race/opt verification, not full-suite or full-race verification**.
Two migrated FDB integration files belong to the owner's later full lane. There were
no performance measurements during this implementation proof batch.

## Preserved invalid proof attempt

The earlier `20261020T141101.777788Z` epoch passed nominal race (155 actual/0 cached)
and nominal opt (149/6), then compiled no-renewal and obtained all four exact named-red
cases. Its runner incorrectly required wrapper exit 1, while `just` correctly preserved
Bazel's tests-failed exit **3** (BEP also 3), so the batch eligibility gate rejected it.
Exact nominal source was restored. Optional mutants/restored green did not run in that
attempt; it is **not** presented as a completed proof batch. The corrected runner accepts
wrapper 1 or 3 only with the required failing BEP/status and unchanged named/uncached
checks, and adds positive/negative exit-3 guard cases. A fresh frozen epoch reran the
whole sequence above. Original logs, source and verdict remain untouched.

## Durable artifact index and reproduction

Raw production artifacts are under
`${FDB_PERF_HOME}/validation/rfc263-production/`, not volatile `/tmp`.
SHA256 values identify exact files relative to that root:

| Artifact | SHA256 |
|---|---|
| `initial-production-3N0168/evidence-files.sha256` | `fd715b88e1c03886e8e596dbf5b055c8569daf2390dc238ebc71a6f1cd09a54c` |
| `initial-production-3N0168/first-fast.log` | `522a59a55d85393ca2377a679d625cd8b5f4220dd90b2d1c231ef37d349668bb` |
| `rev6-production-20261020T141101.777788Z-mutations/artifact-sha256.json` | `ae5579c8dcadcd0a22ce9afb62e714950c80053290923584827d9d9d44cbc162` |
| `first-proof-runner-invalid-attempt.txt` | `66ab3bad9c5fa888a79fda891abae26d67ee17f54fe4184b77ba10d81786e392` |
| `source-reviews.txt` | `1e7a167baca3b7243df0de5b7c400b75439ffdca3df2cb6eb1e3065244f0ddd1` |
| `rev6-production-20261020T143645.124631Z-mutations/artifact-sha256.json` | `7a2569f7042f2c2a90e3f023f20835ddcba7c147c5a9ca4d86d3ddfd12ac66ac` |
| `rev6-production-20261020T143645.124631Z-mutations/nominal-source.tar.gz` | `44cd12d2d492cf15bc0ff028217a9a873ca5ae87832fd1780f72d864f880b406` |
| `rev6-production-20261020T143645.124631Z-mutations/nominal-input-tree.json` | `f7d1f88cdf77582cf629b6969ef3007a9e1ec0dcc1b001b2b12c683554359b71` |
| `rev6-production-20261020T143645.124631Z-mutations/results.json` | `ebc7f8f1f4ac6aec93252707114c4d00818df15c1ad7818abce117246c6408de` |

All **3,820** entries of the completed epoch's recursive artifact index were rehashed
and verified after completion. Its historical as-run runner SHA256 is
`fbec3df8e5ef5e9807990f5682b913d74d872247c94265456fa9fecd20398e26`.
That source ran serial whole lanes and restored mutants even on interruption. The current
portable source has explicit paths/deadlines and additional proof controls; see
[reproduction and source identities](reproduction.md). Current replay:

```sh
flock --close "${FDB_PERF_HOME}/validation/fast.lock" \
  nice -n 19 taskset -c 0,1,6,7,11,12,13,18,19,23 \
  env PYTHONDONTWRITEBYTECODE=1 uv run --no-project python \
  tools/benchcampaign/campaigns/rfc-263/rev6-production-proofs.py \
  --evidence "${FDB_PERF_HOME}/validation/rfc263-production" \
  --deadline-utc "$VALIDATION_DEADLINE_UTC" --mode mutations \
  --optional-mutations eager frontier again cleanup stub-repair logical-order generation-barrier \
  generation-turnover generation-turnover-no-defer
```

Stop all source writers first. The historical epoch used a 17:15 UTC cutoff; portable replay requires a fresh
owner-approved absolute deadline at most two hours away;
performance timing requires a separate explicit exclusive slot, a quiet-settle check,
and its own source/compiler/binary/workload identities. Successful correctness proofs
alone cannot remove a performance blocker; the later fixed confirmation met the owner's
8w p999 rule rather than treating correctness proofs as performance approval.

## Publication-validation follow-ups

These later frozen epochs use the rebased **157-target fast lane**, not the earlier
155-target population or the full/full-race lane. They run whole `just test` commands
with no target list or test filter, nice 19, the reserved CPU mask and `fast.lock`.
Tracked/untracked/config inputs are checked before and after every lane; each mutant
compiles, has the exact uncached named FAIL/XML marker, and is restored byte-for-byte.

| Epoch / stage | Result | Actual / cached targets |
|---|---|---:|
| `bounded-generation-20261020T2140Z` / `nominal-opt` | 157/157 pass | 6 / 151 |
| `bounded-generation-20261020T2140Z` / `nominal-race` | 157/157 pass | 148 / 9 |
| `bounded-generation-20261020T2140Z` / `mutant-stuck-barrier` | 156 pass, 1 expected failure | 148 / 9 |
| `bounded-generation-20261020T2140Z` / `restored-opt` | 157/157 pass | 2 / 155 |
| `gate2-ordering-20261020T2056Z` / `nominal-opt` | 157/157 pass | 1 / 156 |
| `gate2-ordering-20261020T2056Z` / `mutant-stub-repair` | 156 pass, 1 expected failure | 108 / 49 |
| `gate2-ordering-20261020T2056Z` / `restored-stub-repair-opt` | 157/157 pass | 108 / 49 |
| `gate2-ordering-20261020T2056Z` / `mutant-logical-order` | 156 pass, 1 expected failure | 108 / 49 |
| `gate2-ordering-20261020T2056Z` / `restored-logical-order-opt` | 157/157 pass | 108 / 49 |
| `turnover-failure-20261020T2125Z` / `nominal-opt` | 157/157 pass | 2 / 155 |
| `turnover-failure-20261020T2125Z` / `nominal-race` | 157/157 pass | 148 / 9 |
| `turnover-failure-20261020T2125Z` / `mutant-with-defer` | 156 pass, 1 expected failure | 148 / 9 |
| `turnover-failure-20261020T2125Z` / `restored-with-defer-opt` | 157/157 pass | 2 / 155 |
| `turnover-failure-20261020T2125Z` / `mutant-without-defer` | 156 pass, 1 expected failure | 2 / 155 |
| `turnover-failure-20261020T2125Z` / `restored-without-defer-opt` | 157/157 pass | 2 / 155 |

- Stub repair that incorrectly consumes a ticket fails
  `TestTimeoutQueueStubRepairIsNotActorResumption` at `STUB_REPAIR_TOOK_SEQUENCE`.
- Reversing logical-ready order fails `TestTimeoutQueueLogicalContinuationFIFO` at
  `CONTINUATION_FIFO`. Each has its own restored green lane.
- Suppressing the delivery-pause signal fails `TestTimeoutPopThenRearm` at
  `TIMEOUT_QUEUE_WAIT: TIMEOUT_GENERATION_DELIVERY_PAUSE: beforeDeliver did not park`
  in 5.02 seconds, without a global test timeout.
- A later review found a distinct fatal-mid-turnover path: bounded worker waiting
  could abort before publication, leaving cleanup's `Cancel` blocked on `readReady`.
  The test now owns an immediate `sync.OnceFunc(token.finish)` defer. Normal phase
  ordering and existing stale-arm/cause assertions are unchanged.

The paired turnover controls use **identical temporary test-only Cancel watchdogs**
and force the same main completion wait to time out. Only removal of the publication
defer differs; neither watchdog is in production or in the nominal test source:

| Control | Named case | Duration | Cancel-blocked / unpublished markers |
|---|---|---:|---|
| with-defer | `TestTimeoutPopThenReset/mid-turnover` | 5.02 s | False / False |
| without-defer | `TestTimeoutPopThenReset/mid-turnover` | 10.03 s | True / True |

Both controls first fail `TIMEOUT_QUEUE_WAIT: stale arm delivery during mid-turnover`.
Only the no-defer control additionally records
`TIMEOUT_QUEUE_WAIT: TURNOVER_CLEANUP_CANCEL_BLOCKED_WITHOUT_FINISH` and
`TURNOVER_CLEANUP_UNPUBLISHED_WITHOUT_FINISH`: actual `Cancel` blocks until the
watchdog publishes the token, then joins it. The with-defer XML case forbids both
extra markers. Exactly one uncached target fails in each control; no global timeout
is accepted as this witness. These are test-case durations, not a five-second bound
on the whole lane; the harness also has a separate bounded cleanup-group join.

Five generation leaves have RUN/PASS/XML/binary bindings in each nominal/restored
green lane: `TestTimeoutPopThenRearm`, `TestTimeoutPopThenReset/mid-turnover`,
`TestTimeoutPopThenReset/after-publication`, `TestTimeoutRecordThenReset` and
`TestTimeoutPopThenCancel`. Raw driver bytes, applied sources, source/SDK identities,
BEP/XML/logs and binaries are sealed under `${FDB_PERF_HOME}/validation/rfc263-production/`:

| Epoch artifact index | SHA256 |
|---|---|
| `bounded-generation-20261020T2140Z/artifact-sha256.json` | `f8e592984325035398f4874fde0fd99d00c8ca5b4df6bae76a64a32c8dadd7ca` |
| `gate2-ordering-20261020T2056Z/artifact-sha256.json` | `bbaa095015cb429f92214cff1efbf1c8e408510ce982ac2347c3b2e5955379ef` |
| `turnover-failure-20261020T2125Z/artifact-sha256.json` | `eeccae88557661fa881ea2046ca992b4b49b79af18d039ef8cea2a47cd067a2c` |

The portable replay now includes both independent ordering mutants and all three
bounded-generation controls. Historical as-run hashes remain unchanged; the new
portable SHA256 values and invocations are in [reproduction.md](reproduction.md).
These proofs do not clear CPU, full/full-race, final-HEAD review, CI or merge gates.

## PR #973 timeout-fixture repair and expanded race run

The stale `timedTx` fixture stored deadline fields without configuring the timeout
incarnation. The repair uses the same configuration path as `SetTimeout`; a new
real-FDB transaction test checks both interruption mappings to literal 1031 **before**
`Get` can mask missing configuration through its separate expiry check, then checks
public `Get`. Production code is unchanged.

At private fix `c4eb12818d27f519841981efff0e7e5b99396f39`, **Go 1.26.9** whole normal
`just test-full` passed **210/210 targets (54 actual, 156 cached)**. All 27 client
targets passed in normal and expanded race runs: `client_test` executed in each,
the other 26 were cached, and all seven timeout-mapping cases plus the new real-FDB
control passed. The expanded **test-full-under-race run remains RED: 194/197 pass,
three out-of-scope failures (72 actual targets, 125 cached)**:

- `bench_test`: upstream Apple binding `futureStringSlice.Get` aborted on
  `checkptr: misaligned pointer conversion`; not a DATA RACE report or differential
  mismatch. Bench is excluded from every defined race scope by design.
- `guardiann_peel_timing_test`: CPU-only k-means/peel timing under race instrumentation
  exceeded its 900-second budget; no client frames.
- `guardiann_peel_fixtures_test`: a race-slowed 20.2-second real-FDB drain violated its
  no-retry assertion. This fixture passed the normal full lane; the failure contains
  no client frames, and its observer did not preserve the retry's error code.

The owner judged these three expanded-run failures outside the defined gates and
not attributable to RFC-263. They are retained, not relabeled green; no BUILD tags
changed. Raw BEP/log/XML and target censuses are under
`${FDB_PERF_HOME}/validation/rfc263-production/pr973-map-timeout-root4832/`.
These Go 1.26.9 results do not verify the later Go 1.26.10 configuration. After the
separate security-toolchain PR #974 merges, the owner's official race and full lanes
on the exact final rebased commit remain merge gates.
