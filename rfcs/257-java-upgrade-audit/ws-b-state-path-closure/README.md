# WS-B state-path closure and final delta review input

Historical checkpoint: the ensuing delta reviews found further shared-state
coherence defects. See [coherence correction](../ws-b-coherence-closure/) for
fixes, newer verified trees and fresh measurements. The ratios here describe
this checkpoint only, not the later source.

Predecessors: [NAKs](../ws-b-implementation-review/) and
[intermediate review fixes](../ws-b-review-fixes/).
Tracking: TODO.md, “WS-B state-path closure and final delta review input”.
This closes the listed implementation/test work, not the reviewer gates or
migration. No new ACK is claimed here. No publication is authorized/performed.

## State-path acceptance

Added real-FDB acceptance for the previously missing routes:
- Executor value, aggregate and vector physical plans each return one actual
  result as a positive control, reject WRITE_ONLY after another same-context
  handle clears the index, propagate canceled-transaction 1025, and conflict
  with a concurrent enable after refusing DISABLED (1020). Six parallel
  subtests in TestExecutePlanTransactionalIndexState, existing Bazel-enrolled
  filtered_index_execution_guard_test.go. The initial aggregate positive
  control rejected a malformed test row type COUNT(COUNT); the corrected
  COUNT(*) plan runs, rather than bypassing result-type validation.
- Rank/count function selection, explicit/automatic, cold/cacheable warmed:
  eight refused-selection conflicts with concurrent rebuild/enable. The
  existing four live-result, same-context-disable and cancellation cases remain.
- Lazy Build transition invalidates an independently populated cache. The
  test proves admission before mutation, checks stamp dirtiness, commits, and
  reopens against that same cached entry. Malformed-header initialization fails
  without writing index state or dirtying context/stamp. Existing persisted
  record-update lock regression remains.
- A bulk-deletion handle opened while disabled deletes the actual index row
  created by a second same-context handle's rebuild. After commit, a fresh
  handle sees readable state, absent record, and no orphan index rows.
- SearchSPFreshIndex previously swallowed cancellation while returning an
  unreadable-index error. One retained red→green spec establishes a real nearest
  result, same-context disable refusal, then exact cancellation 1025. The API
  now uses the error-returning state helper. Index data remains snapshot-isolated;
  routing, pruning, replication, LIRE, budgets, re-ranking and wire bytes are
  unchanged. RFC094 and SPANN §3.2.3 / SPFresh §3.1–3.2 were read. A paper-author
  review is required for this source change, in addition to the three WS-B gates.

## Write-path cost and dispatch correction

The post-shared-view CPU profile (retained state-view-write.cpu and pprof text)
priced the remaining cost: readIndexState accumulated 11.72 sampled CPU seconds;
8.45s came from creating GetReadVersion futures and 1.90s from waiting. Work in
those futures is accounted separately: client.GetReadVersion accumulated 16.69s.
These are profile costs, not a call-site census or an additive wall-time claim.

Moved transaction liveness validation to the per-record index-update boundary.
Per-index decisions retain explicit state-key read conflicts and context-shared
state; they no longer allocate GRV futures for every candidate and dispatch.
The all-disabled case still validates cancellation, with two retained tests for
readable/disabled index updates. The batched path enters the same boundary.
Direct public scans/functions/maintainer state checks retain their own checks.

A second dispatch finding surfaced: an index disabled between candidate
selection and dispatch could still receive Update. Java's per-index switch has
a DISABLED no-op. Go now does too. A deterministic two-handle reproducer failed
before the fix by recreating an index row; the post-fix test asserts no rows.

### Stress test 1M baseline

Both sides use the same filesystem (11% used), sequential execution, two samples
per side; loads and timestamps are in the full logs. Baseline commit is
`e48f5b4965543cd4d99b5578356059e12d969c7c`, the true merge-base. Current source is
Git **tree** `c7f9e5f9f14340d7452abc65ac8eb701493ca3ea`, not a commit. All four
runs have exactly 24 RUN and 24 PASS lines, identical query row counts and zero
40001 retries. Source hashes stayed unchanged throughout. These are fresh
measurements, not reuse of the earlier baseline samples.

| Population | Baseline sample 1 / 2 | Current sample 1 / 2 |
|---|---|---|
| 100,000 customer inserts | 6.442271s / 6.497939s | 6.834997s / 6.814969s |
| 1,000,000 order inserts | 144.129140s / 144.354861s | 146.944183s / 147.170663s |
| Whole named stress test | 171.00s / 171.24s | 174.08s / 174.22s |

Orders average ratio is 1.0195x; customers 1.0548x. This eliminates the measured
per-index-future overhead, but is NOT parity or a zero-overhead claim. Earlier
~8.7% orders overhead described a different tree and is superseded for decisions
about this tree. All per-query durations and EXPLAIN/row checks are retained in
the logs; no assertion was relaxed to obtain the result. Remaining marginal
cost includes new serializable lifecycle protections absent from the baseline;
that observation does not prove every residual difference is attributable to
those protections. Final reviewers must assess this measured result explicitly.

## Mutation verification

Four mutations were each applied, formatting-checked, compiled, executed and
killed, then restored byte-for-byte:
1. Omit state read conflicts: refused function-selection/bulk conflict tests fail.
2. Isolate state views between handles: stale-handle function/bulk tests fail.
3. Omit per-record liveness: canceled index-update boundary tests fail.
4. Admit unreadable executor indexes: real executor route tests fail.

Scripts, manifests and complete logs are retained. Two attempts at mutation 2
were rejected by nogo (nilness, then formatting); they are retained and NOT
credited as mutation kills. The final run compiles all four and requires actual
test execution/failure, not merely a nonzero build status. Earlier component
red→green and mutation evidence remains in the linked component directories.

## Frozen verification

6733 tracked/untracked nonignored files were frozen before the full/stress/race
runs. Exact file set and SHA256 values were verified unchanged afterward,
including after mutation restoration. This evidence booking comes afterward:
it changes documents/logs, not the verified Go/build source.

- state-path-full-uncached.log: **93/93 targets executed, 93 pass**.
- state-path-race-full.log: **14/14 targets executed, 14 pass**, with actual
  `--@rules_go//go/config:race`. Scope: client, fdb, recordlayer, chaos,
  conformance, cascades/... and executor. This is not the PR relational race set.
- Retained full race logs: recordlayer **3399/3400 Ginkgo** and **2110 Go RUN/PASS**;
  conformance **1391/1510 Ginkgo**, **70 Go RUN/PASS**; executor **1952 Go RUN/PASS**.
  One existing opt-in million-record Ginkgo exclusion and 119 conformance
  target exclusions remain. No new skip was added.
- All six real executor route subtests also passed in their focused uncached run.
- just gazelle, bazelisk mod tidy, pinned gofumpt and git diff --check passed.

No background jobs remain at evidence capture. Full outputs and SHA256 manifest
are retained here. Publication HEAD remains
`71ccd8cf8b3fd0dbafe283e91171818e36af555e`.

## Gate status

The prior three NAKs remain the last recorded verdicts until actual final-tree
delta confirmations complete. The independent SPFresh lens is also required.
No milestone or migration checkbox is marked done by this evidence. WS-C–K,
final upgrade-wide review/verification and actual CI remain separate unfinished
obligations. Push/commit/PR-state authorization has not been granted.
