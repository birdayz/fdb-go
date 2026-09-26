# WS-C implementation finding: multi-target follow-up liveness

Addresses the remaining follow-up finding in the NAK of tree
`2a4d33046f8640673643ea989709665f425cbbc5`. Implementation fixes are locally
verified; actual final-tree delta gate verdicts are still required.

Java 4.14.2.0 `IndexingBase.drainIndexes` / `mergeIndexes` launch target operations
via `AsyncUtil.whenAll`; each passes its heartbeat updater to the drainer/merger
pre-commit callback. Go's sequential target processing must not leave the other
owned targets idle until their leases expire. Each callback now refreshes every
still-owned target. An admitted build also validates every remaining target's
expected state and method/block stamp. Scannable targets are already published
(possibly by a mutual peer) and must not have heartbeats recreated. Standalone
`MergeIndexes` has no admitted build stamp and retains that distinction.

`validateBuildTarget` shares state/stamp/heartbeat validation with ordinary batch
work. The current drain target retains its stricter queued-state validation.
The merger callback uses the same all-target follow-up refresh on its actual
transaction. No future GuardiANN/Lucene child-transaction proof is claimed.

## Regressions and mutations

Raw logs: `/var/tmp/fdb-upgrade-recovery/ws-c/`.

Six real-FDB specs in the Bazel-registered
`pkg/recordlayer/online_indexer_followup_test.go`:

* Long drain and long merger-follow-up cases use two queued vector targets and
  an ordinary target, with a deterministic clock exceeding the 30-second lease.
  Eight real queue clears advance simulated time by five seconds each, forcing
  separate quota-limited drain transactions. After commits, single-target
  builders explicitly permitted to take over a multi-target build must still be
  refused for **both** queued and ordinary followers.
* The merger case uses eight synchronous HNSW merge invocations and validates
  callback/liveness wiring, **not deferred backend work**.
* Sequential closeout must publish all targets, leave all queues empty with the
  eight expected vector rows, and not recreate a published target's heartbeat.
* Follower blocking and disablement must fail both drain and merge follow-up;
  the current target's eight queued entries remain after failed transactions.

`followup-target.log`: 6/3604 specs passed under Bazel.
`followup-drain-mutation.log` and `followup-merge-mutation.log`: restoring
current-target-only refresh built and ran each selected three-case population;
all three failed in each run. `followup-retirement-mutation.log`: treating
published targets as still owned built and failed both closeout cases. All
mutations were verified present and originals restored.

## Final source verification for all five review fixes

* `followup-uncached-full.log`: the full `just test` target set, with cache
  disabled (`bazelisk test //... --test_tag_filters=-stress --nocache_test_results`),
  **93 executed / 93 passed**, 1017.923 seconds.
* `followup-final-race.log`: actual `--@rules_go//go/config:race`, uncached,
  **183/3604 Ginkgo specs** plus the target's ordinary Go tests and fuzz seeds
  (**2143 `=== RUN` lines**, including subtests/seeds). All pass. Filter covers
  heartbeat, pending/queued writes, throttled iterator, deferred maintenance,
  mutual, format 15, versionstamped, merger, follow-up and session fencing.
  Not an all-Ginkgo-suite race claim.
* `followup-final-jvm-race.log`: actual race instrumentation, uncached,
  **9/1523 JVM conformance specs**, passing. Includes the 12 asserted nested-Any
  boundary verdicts and format-15 both-engine write evidence.
* The 35-file source/BUILD manifest `final-delta-source.sha256` was checked
  unchanged after all three runs. This is a freeze manifest, not a count of
  changed files: it also includes pre-existing untracked migration sources.
* The cleanup correction in `../ws-c-cleanup/README.md` is included: an owned
  readiness wait ends on its deadline even when future cancellation is ineffective.
  Its regression kills a mutation reverting to blocking `Get` plus cancellation
  (`cleanup-detached-mutation.log`, one executed case).

Other fix evidence: `../ws-c-admission/`, `../ws-c-nested-any/`, and
`../ws-c-mutual-renewal/`. Initial review verdicts remain NAK until actual delta
confirmation. No CI-green, migration-complete, or merge-ready claim. No commit,
push, PR change, publication, or merge performed.
