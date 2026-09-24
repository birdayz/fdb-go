# WS-B clear policy, reload ordering and exported snapshots

Tracking: TODO.md, “WS-B clear policy and lock-order correction”.
Previous reviewed Git tree: `dd0956b11e901609edcd5f19d36ef64f04482b8a`.
All four actual correction verdicts were **NAK**. Complete prompts, outputs and
launch logs are retained here as `attachment-*`; logs identify gpt-6-astra/xhigh.
They accepted the initialization correction and reported the defects below.

## Findings, reproductions and fixes

1. **Reload/uniqueness-cleanup deadlock.** Maintenance holds stateMu.RLock while
   deleting the second-last uniqueness violation, then needs the context registry
   for range cleanup. Reload took registry then stateMu, reversing that order.
   Reload now takes stateMu before registry and view.mu. A real-FDB regression
   creates two duplicate records in a unique WRITE_ONLY index, pauses deletion
   before the survivor range read, proves the real transaction has one remaining
   violation, and starts reload. TryRLock observes the queued reload writer;
   TryLock detects whether it improperly holds registry. The buggy run is safely
   unwound by canceling the actual FDB transaction before resuming its read, so
   the test demonstrates the inversion without stranding goroutines. Red and
   green logs retained; reversing lock order again is a compiled mutation kill.

   Initial Open still loads/binds under registry and only takes its unpublished
   handle's stateMu for assignment. Both loadStoreState callers are builder
   paths constructing a new handle. Operational reload uses stateMu→registry→
   view; maintenance's uniqueness cleanup uses that same ordering. Ordinary
   state writes use stateMu→view, and neither view path reacquires registry.

2. **Clear-only transaction leaves caches stale.** Transaction-local clear
   history could only invalidate when that context later loaded a store. A
   fresh context clearing index state and committing without any Open lost the
   history. The expanded warm-cache test reproduced the unchanged committed
   metadata stamp. Context ClearRange now performs necessary invalidation before
   the clear, independent of registration or later loads. Deferred clear history
   and the load-preparation mechanism were removed.

   The range policy is deliberately conservative: unclassified nonempty ranges
   invalidate store-state caches. Already-known data ranges exclude the header
   and index-state region. For a record-only range before any Open, candidate
   record-key prefixes must contain the entire range AND have a valid persisted
   store header; matching packed bytes alone is not sufficient. Missing or
   malformed headers retain invalidation. This preserves ordinary record-clear
   negative controls while preventing metadata mutations from depending on a
   future Open. It is a context-level coherence guarantee, not a change to the
   FDB client's generic transaction semantics or wire encoding.

   Thirteen real-FDB classification cases cover known/unopened record ranges,
   partial record ranges, index data, known/unopened state, partial state,
   header, unknown range, missing/malformed header and empty range. The root
   prefixes contain embedded numeric record-key bytes inside a bytes component,
   requiring validation rather than trusting a spurious candidate boundary.
   Three warm-cache compositions now cover clear after Open, before Open, and
   a clear-only transaction. They check stamp changes, exported state, real
   maintenance, committed populated scans and uncached reopening.

3. **Noncacheable deletion invalidates globally.** DeleteStore's tagged-Java
   header policy was overridden by generic context range invalidation. The
   store-aware deletion path now supplies its already-applied policy to range
   cleanup, while preserving shared views, queued-version removal and local
   version cleanup. Open→DeleteStore and Open→DeleteStore→recreate both failed
   before this fix and now assert an unchanged global stamp for noncacheable
   headers. Cacheable/malformed positive controls remain in the full suite.
   Flipping the deletion policy call is a compiled mutation kill.

4. **Exported snapshots ignore the shared authority.** GetRecordStoreState and
   GetAllIndexStatesMap copied stale per-handle initialization maps. Both now
   copy the common view under its mutex after lazy initialization. Reload tests
   assert both APIs across two handles; clear tests assert removal of the old
   state. The two-handle snapshot regression failed before this correction;
   restoring handle-local snapshot selection is a compiled mutation kill.

Tagged Java FDBRecordContext.clear preserves pending-version range cleanup;
FDBRecordStore.deleteStoreAsync uses the header-aware stamp policy; getRecord-
StoreState returns the operational state authority. The Go context-wide shared
view must remain coherent across handles and exported snapshots as well as
operational getters. The deliberate conservative policy for unclassified
context clears closes the clear-only transaction gap without introducing a
client/wire change or relying on a transaction that no longer exists.

## Verification

Source Git tree **`ba06f34ff8998f61040a491e4f3a3e8b8de7ccf9`**, not a commit.
**6848 tracked/untracked nonignored files** were frozen and verified unchanged
through both full runs and four sequential stress runs. Evidence/documentation
booking follows that freeze.

* **93/93 uncached targets executed and passed**.
* **14/14 actual race-instrumented targets executed and passed**, using
  `--@rules_go//go/config:race`: client, fdb, recordlayer, chaos, conformance,
  cascades/... and executor. Not the full PR relational race set or actual CI.
* Race logs: recordlayer **3426/3427** Ginkgo specs; conformance **1391/1510**.
  Existing exclusions unchanged; no new skips.
* Focused runs: **14** lifecycle/reload/cache/deletion specs passed; subsequent
  **19** classification/cache/snapshot/deletion specs passed, including all
  thirteen classification cases. These populations overlap; do not sum them.
* Four compiled, applied, killed and restored mutations: reload lock inversion,
  omitted unregistered-clear invalidation, handle-local snapshot selection,
  bypassed header-aware deletion policy. Driver, JSON and logs retained.
* Pinned formatter, just gazelle, bazelisk mod tidy and git diff --check passed.

## Fresh stress comparison

Baseline commit **`e48f5b4965543cd4d99b5578356059e12d969c7c`** (true merge-base),
current source tree **`ba06f34ff8998f61040a491e4f3a3e8b8de7ccf9`**.
Sequential on the same filesystem, n=2 each. Every run has **24 RUN/PASS**;
all **26 parsed row-count outputs** match across all four logs, with zero
reported transaction retries. Full logs retain load averages.

| Population | Baseline seconds, n=2 | Current seconds, n=2 | Mean ratio |
|---|---|---|---|
| 100k customers | 6.491369 / 6.591749 | 6.809350 / 6.806064 | 1.0407x |
| 1M orders | 145.160568 / 144.823070 | 147.368591 / 147.232271 | 1.0159x |
| Whole stress test | 172.32 / 172.03 | 174.55 / 174.38 | — |

These replace the preceding tree's 1.0507x/1.0290x in decisions about this source.
No parity claim, no causal explanation of the residual. Prior reviewers found
the preceding residual non-independently-blocking; confirmation on this result
is still required.

## Gate status

Actual correction confirmations remain required; retained NAKs are not ACKs.
WS-C–K and upgrade-wide verification/CI remain unfinished. HEAD remains
`71ccd8cf8b3fd0dbafe283e91171818e36af555e`. No commit, push, merge or PR-state
change authorized or performed.

## Actual final confirmations

All four actual correction reviews ACK Git tree
`48b50548b11a53bd1452a627689c440535a3d093`. See
[complete final verdicts](../ws-b-final-review/README.md). This closes WS-B,
not WS-C–K or upgrade-wide CI. The earlier gate-status paragraph records the
state before those reviews; the ACKs apply to the named tree only.
