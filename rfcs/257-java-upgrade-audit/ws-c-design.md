# RFC-257 WS-C — persistent pending writes and indexing sessions

Status: detailed design for review; queue implementation has not begun.
Parent: [RFC-257](../257-java-4.14.2.0-upgrade.md), WS-C.
Java specification: 4.14.2.0 `fdacd162a9c8acfadc49082b89185c823ab8ae4a`.
Starting accepted WS-B tree: `48b50548b11a53bd1452a627689c440535a3d093`.
Published Go HEAD remains `71ccd8cf8b3fd0dbafe283e91171818e36af555e`.
No commit, push, merge, or PR-state changes are authorized.

This is one implementation milestone. Sections are implementation order, not
independently completed portions of WS-C. Preserve the approved Go extensions;
FDB C++ remains 7.3.77. Research is in [ws-c-research](ws-c-research/).
Research output is not an ACK and proposed departures in that output are not
silently accepted by this design.

## 1. Storage, shared split machinery, and cursor semantics

Port `queue/PendingWritesQueue.java` and `PendingWritesQueueEntry.java`, using
the already synchronized protobufs. The generic queue owns supplied entry and
counter subspaces, not indexing policy. Index queues use `(9,indexKey,8)` and
`(9,indexKey,9)` respectively. Envelope version is 1; readers accept older
versions including the protobuf default 0 and reject future versions. Store
`Any`, enqueue epoch milliseconds, and tuple `(incarnation, versionstamp)` with
context-claimed local version ordering. Writer URLs use `type.googleapis.com/`;
reader compatibility follows Java protobuf `Any.is/unpack` (message full name,
not a blanket requirement for that prefix). No hand-maintained proto subset.

Extend the actual split writer to support incomplete primary keys, including
SET_VERSIONSTAMPED_KEY mutations registered on the context, collision detection,
all split suffixes, and existing deletion cancellation. Keep ordinary complete
key behavior and record-version behavior unchanged. Make AddVersionMutation
return its replaced value as Java does, rather than silently accepting repeated
queue chunk keys. Do not change FDB client mutation semantics.

Extract/use raw unsplitting infrastructure beneath record deserialization for
queue payloads; do not route queue protobufs through record metadata. Match
Java KeyValueCursor/KeyValueUnsplitter continuation bytes, reverse iteration,
physical KV scan/byte accounting, split-entry completion across resource limits,
pending lookahead, cached terminal results, and outer logical row limiting.
Keep existing record scan behavior pinned while sharing machinery. Never return
half a payload or advance past a partially assembled entry on continuation.
Existing proto-wrapped continuation magic and legacy raw fallback remain intact.
Queue-specific limits use a separate limit manager around raw assembly; existing
record cursors retain their logical-record accounting. A fetched KV is charged
one scan before append. Append charges key+value bytes, including a buffered
lookahead a second time when consumed (without a second scan charge). Time starts
at queue cursor construction, not transaction creation. Nonthrowing limits finish
the current assembled entry, even when that exceeds the budget; fail-on-limit
throws during assembly and returns no partial entry. Continuation stays at the
last KV belonging to the returned entry, not the buffered next entry. At terminal
state, actual inner SOURCE_EXHAUSTED wins over resource reasons; otherwise scan
wins over bytes, then time, then the inner reason. The outer row limiter returns
RETURN_LIMIT_REACHED at the exact count without probing exhaustion, even at the
last entry. Pin these tagged quirks with both directions and split→split,
split→unsplit, and exact-final-row fixtures. No unmeasured correction to Java's
lookahead accounting is authorized.

The retained JVM/FDB probe `ContinuationSteps.pendingQueueSkip`, invoked by
`pins Java pending queue skip and row continuation semantics`, actually ran
under Bazel against 4.14.2.0. For skip 0, 1, and 5, a three-entry queue with row
limit 2 returns `[1,2]`, RETURN_LIMIT_REACHED, then continuation `[3]`, exhausted;
size remains 3. Java clears the inner skip and adds no outer skip wrapper,
despite its source comment. Match that queue behavior, not the comment; do not
change record-scan skip. Extend this retained oracle for split limits and Any
validation before coding assumptions about either.

Queue traversal and capacity reads are snapshots. The size counter is a
little-endian int64 atomic ADD; absent counter remains distinguishable from
zero, and nonpositive maximum is unlimited. Concurrent capacity is soft.
ClearEntry installs a serializable read conflict over the full packed entry
prefix, removes all chunks, and decrements once. Actual emptiness uses a
serializable limit-one range read, never the size counter. Replay must succeed
before clearing. Malformed envelope, future version, wrong Any type, malformed
payload, and malformed operation data return structured errors with Java
messages and context. The expanded retained JVM oracle has 15 envelope cases:
custom-prefix URLs work, slashless URLs fail, absent/negative versions work,
future versions fail, and malformed envelope/payload failures retain the public
RecordCoreStorageException rather than the HTTP harness's deepest-cause label.
Unknown-only or absent required operation fails unpacking; known UPDATE followed
by unknown 99 (and the reverse) remains UPDATE. Decode the closed proto2 operation
field from wire tags with Java semantics: unknown numeric values remain unknown
fields, known values use last-known occurrence, and no known value is a missing
required field. Do not use Go's open-enum last-numeric-value behavior. Retain
malformed wire validation and unknown fields, and test DELETE_WHERE duplicates too.

## 2. Maintainer contracts and writer dispatch

Extend IndexMaintainer with capability, serialize-update, and replay methods;
standard embedded implementations explicitly refuse unsupported operations.
Production VALUE indexes remain unsupported. Java's separate
StandardIndexMaintainerWithQueue is not a reason to enable all standard indexes.

Vector serialization evaluates/filter-selects old and new entries at enqueue
time and stores packed key/value plus FULL primary key. Replay removes old
entries before inserting new entries using a shared entry-application helper;
trim PK only at storage application. Preserve the existing HNSW/SPFresh update
algorithms. SPFresh is explicitly NOT queue-capable in WS-C: it is a separate
Go extension with its own versionstamped maintenance lifecycle, not Java's
VectorIndexMaintainer entry format. Its adapter returns unsupported, and explicit
queue requests fall back to ordinary write-only builds. Pin that eligibility and
ordinary-build/update behavior. This is a capability boundary, not a new deferred
merge adapter or a change to its algorithms; obtain scoped paper review of this
disposition. HNSW vector entry replay is queue-capable. GuardiANN's backend and
its queue/merge adapter are owned by WS-D.

Sliding capability follows its delegate. Serialize window entry keys and
separate delegated delete/insert Any values after filtering once. Replay uses
WS-B's key-first window algorithm and delegated callbacks, including existing
entry replay, boundary validation, overflow promotion, partitions and ties.
Do not reevaluate predicates for the directly queued old/new callbacks. This
is not a guarantee about indirect eviction/promotion: Java and Go load current
records for those paths and ordinary delegate updates apply current predicates.
Preserve that distinction. Test changed/deleted records before drain, eviction,
overflow promotion, and the existing window-lock → delegate-lock ordering; do
not add a reverse lock edge or invent historical payloads for indirect records.

Branch on queued state at store update dispatch before ordinary write-only
maintenance. Preserve the authoritative state read and its conflicts. Builder
maintainer.Update calls remain direct; the scan must not enqueue itself.
DELETE_WHERE retains every preflight validation, then queues the computed
per-index prefix (not a record prefix guessed later). UPDATE and DELETE_WHERE
share the same incarnation/versionstamp order. Ordinary index actions stay
ordinary, allowing mixed target capabilities.

## 3. Persisted state, eligibility, and session coordination

Add state 4 to decoding and the write-only family while retaining a distinct
queued predicate and explicit ordinary-write-only setters. Audit all state
consumers, including uniqueness, rank, bitmap, build eligibility, replacement
retirement and disabled dispatch. Maximum format becomes 15 ONLY when this
entire lifecycle is complete; default creation remains a separate constant 14.

Policy defaults: no requested queue targets; 100 closeout attempts. A fresh
queued target requires explicit request, capable maintainer, no version columns,
persisted format >=15, and non-mutual mode. All other targets use ordinary
write-only. Continuing builds retain persisted state and reconstruct their
queued target set; current policy must not downgrade or reinitialize them.
Mutual mode refuses queued continuation before overwrite/stamp shortcuts.
The checked MarkIndexWriteOnlyWithQueue API also enforces persisted format >=15,
maintainer capability and absence of version columns before writing state 4.
Calling that explicitly is the direct request; mutual is an indexer-session
constraint, not a property of a standalone setter. Unlike tagged Java's bare
setter, invalid direct requests fail without mutations; retain a JVM reproducer
and document this safety deviation. There is no new unsafe queued setter.
Test format14 and unsupported/versioned maintainers directly, plus same-context
state changes and all authoritative state conflicts.

Validate every target and its stamp, not only the primary. Java IndexingBase's
follower-state equality rejects mixed state-1/state-4 continued builds, even
though fresh eligibility is per target. Preserve that tagged rejection and pin
it with a JVM/FDB oracle; do not adopt the research suggestion to silently accept
mixed resumed states. Fresh mixed-capability builds remain supported. If the
probe refutes this source reading, correct this design before implementation.

Allocate one session UUID outside retry transactions and pass it to heartbeat,
drain and merger phases. Introduce shared transaction preparation: authoritative
target states, method/stamp/block validation, session ownership, and attempt-local
counters/work sets. Cover ordinary, by-index, mutual and preset range paths.
Only publish continuations and follow-up work after successful commit. Loss of
range ownership aborts, never commits maintainer writes under a false success.
Every terminal BuildIndex exit (readable or not, success, failure, cancellation)
attempts best-effort cleanup of this session UUID across all targets, never peer
heartbeats. Own one fresh bounded cleanup transaction/retry context independent
of the canceled work context (30-second maximum), cancel/close it on exit, and
preserve the original result/error. Cleanup failure is diagnostic only and lease
expiry is the fallback. Tests cover every terminal arm and failed cleanup.

Port heartbeat tuple UUID encoding and decoding: current Go string keys are not
Java-compatible and ignore Java peers. Pin actual two-way JVM/FDB exclusion and
wire keys. This encoding transition requires operational quiescence, NOT mixed
old/new builder operation: stop/fence all old Go indexing workers and prevent
restart before admitting upgraded or Java builders. New Go preflight checks all
target heartbeat keyspaces and refuses ANY legacy string or malformed key,
regardless of age; it never silently deletes or dual-writes them. An explicit
administrative cleanup after quiescence clears legacy keys serializably, then
operators admit UUID-only builders. No automatic age-based conversion can prove
an old process is stopped. This deployment prerequisite is recorded in the
upgrade runbook and tested as fail-closed preflight; code cannot fence a legacy
binary that ignores new keys. New writes and ordinary reads are UUID-only.
Java builders must not enter before that cleanup. Match Java's strict
future-skew boundary (`age > -oneDay`), malformed-value diagnostic behavior,
lease checks, and bounded administrative listing/clearing where exposed.

Ordinary Go build scans currently omit Java's explicit record conflict after
snapshot reads. Add it before maintenance so a concurrent writer cannot commit
first and have stale entries restored by the builder. Revalidate blocked/stamp
state each batch. Cover all targets with liveness, not only mutual primary.

## 4. Build follow-up, drain and deferred merge

Build loop order becomes committed build batch → drain discovered targets →
merge requested targets → completion/throttle. The last exhausted build batch
still performs follow-up. Snapshot counter reads are scheduling hints only.

Port the throttled retry iterator with explicit ownership of retries and active
contexts. Each attempt opens a fresh store/cursor and registers a named heartbeat
commit check. Apply an entry completely, then clear/decrement. Publish cursor
continuation only after commit. Commit the final empty/exhausted batch too, so
heartbeat checks cannot disappear when no items remain. Cancellation closes
active contexts, propagates through waits, and prevents a new phase from starting.

Defaults match executable Java: 4-second transaction quota, 100 retries,
commitWhenDone=true, initially unlimited rows/deletes, and queue delete rate
10,000/second. Adaptive failure/success limits follow Java's 90% decrease,
5/4-or-plus-4 increase and 40-success threshold. Exactly one retry owner uses
Runner.OpenContext + explicit context Commit/Cancel, never nested FDBDatabase.Run.
Java retries ALL failures except RunnerClosed, including permanent malformed
payload errors, up to 100 retries (101 attempts). Preserve that budget and final
error. Go context cancellation/deadline stops immediately as the language-level
runner-closure equivalent; test it independently of the retryable FDB whitelist.
Callbacks execute outside registration locks. Build throttling remains separate. Time/write-size checks occur
after complete items; no half-entry commits. Register checks before commit-check
execution, since current context execution snapshots registrations.

Add deferred-maintenance control and IndexingMerger equivalents rather than
pretending all backends have zero work. Per-transaction requests feed per-target
session state, carrying merge limits, time quota, feedback and precommit callback.
Every actual backend write transaction, including retries/final exhausted work,
must consume the heartbeat callback. This is an explicit safety correction to
tagged Java's vector merge path, which receives control but does not consume the
callback; pin a real backend transaction regression when that adapter exists.
HNSW reports no deferred merge work because it mutates its graph synchronously;
SPFresh is outside this adapter and retains its own maintenance lifecycle.
Do not claim either proves Lucene/GuardiANN callback consumption.

Lucene is not Go TEXT. Its absent backend is a pre-existing implementation
dependency, not a completed WS-C feature. Generic queue/merger work does not prove
Lucene callback consumption, AgileContext clear-byte accounting, or suggestion
records without PK. Keep those requirements visible in RFC-257 and the final
scope verdict; do not mark upgrade-wide completion with them unresolved.

## 5. Closeout, overflow, and deletion

For each target independently: drain, then in a fresh retryable transaction
assert actual serializable emptiness, transition readable/unique-pending, clear
heartbeat and build bookkeeping. Retry nonempty/conflict up to configured
attempts; preserve distinct other failures and continue independent targets.
Writer-first conflicts with emptiness; closeout-first conflicts with writer's
state read. Pin both commit orders with explicit transactions, not retries that
hide the first outcome.

Put the queue-empty guard into shared built-index validation so direct readable
setters and MarkReadableIfBuilt cannot bypass the protocol. First reject any
surviving context-buffered version mutation under that queue's entry prefix,
then perform the serializable persisted emptiness read. Inspect the context's
actual pending-mutation registry under its mutex, shared by every store handle,
not a handle-local counter. Existing context-aware clear/cancellation removes
those mutations, so the guard naturally follows surviving writes. Enqueue→checked
readable (same handle and other handle) must fail even with completed build
ranges; enqueue→clear→checked readable must observe the cleared registry.
Do not flush mutations early or replace this with a snapshot size check.

The guard and publication are one critical section, not two separately safe
operations. Add a maintenance RW lock to the existing context-shared
transactionIndexStateView (store-scoped, covering all its indexes), not a second
handle-local state registry. Initialize/bind the view before entering maintenance.
Writer dispatch acquires handle stateMu.RLock then shared maintenance.RLock
before its authoritative state decision, holding both through completion of
ordinary maintenance or buffered enqueue. The batch-locked writer path and
DELETE_WHERE actions use this same non-recursive gate. Checked readability takes
handle stateMu.Lock then shared maintenance.Lock BEFORE reading current state,
checking buffered/persisted work and uniqueness, and retains both through state
publication and readable cleanup. Split setIndexState into lock-owning and
already-locked helpers; no recursive stateMu acquisition. Other high-level state
transitions use the same exclusive gate so cross-handle state publication cannot
race dispatch. All affected high-level read/transition paths must use the common
protocol; tests cover READABLE and READABLE_UNIQUE_PENDING.

Lock order: handle stateMu → shared maintenance → short-lived registry/view
locks and versionMu. Never hold versionMu, context.indexStateMu, or view.mu while
waiting for a handle state lock or maintenance lock. Do not widen the old view.mu
critical section around maintainer calls: delegate state reads reenter it.
Context clear operations used inside maintenance retain their existing lower-level
locks and do not recursively acquire the maintenance gate. Reload takes the
exclusive maintenance gate after stateMu and before its existing registry/view
locks. Initial unpublished handle binding need not acquire a maintenance gate.
Callbacks execute after registration locks are released; overflow only registers
its deferred transition while holding the read gate. Deterministic real-FDB
barriers pause a writer after selecting state4 but before buffering: setter must
wait, then reject; reverse ordering publishes readability first and forces the
writer through ordinary maintenance. Run both schedules on one and two handles,
plus batch and DELETE_WHERE dispatch, without timing-only assertions. Tagged Java's
low-level built check lacks this guard; this is a deliberate safety correction,
not a claim of identical behavior on that unsafe path. Retain the JVM reproducer,
Go regression, call-site explanation and upstream report draft alongside it.
Unchecked administrative setters retain their documented unsafe role and must
not be used by the normal closeout path.

Overflow defaults to disabling, matching Java's actual initializer despite its
comment. Explicit fail mode returns the queue-full error. Disable mode registers
a commit check because maintenance holds a state read lock. Scope hooks by store
subspace and index identity, not bare index name. Commit-check disabling uses the
ordinary cleanup/state transition so peers see authoritative state conflicts.

Reuse context-aware clears for queue/counter cleanup. DeleteAllRecords must clear
pending version mutations in build space 9 as well as persisted data. DeleteStore
must cancel captured overflow/heartbeat callbacks for that store, preserving
WS-B retirement cancellation and header-specific cache invalidation. Test clear,
disable, delete-all, delete-store and delete/recreate with pending uncommitted
queue chunks and hooks; no queue or state resurrection at commit. Preserve
intentional former-index build/lock retention separately.

## 6. Required evidence and acceptance

Retain real-FDB/JVM regressions for every found defect. Storage matrix covers
cross-language enqueue/read/replay, complete split bytes, default/custom Any URLs,
absent/future envelope versions, malformed payloads, enum behavior, incarnation
ordering, same-transaction local ordering, reverse/row/byte/time limits and resume,
capacity races, concurrent clear conflicts, and serializable emptiness.
Lifecycle matrix covers all eligibility arms, persisted resumes/mutual refusal,
multiple targets, filtering once, DELETE_WHERE order, concurrent snapshot builds,
blocking/stamp changes, lost ownership, overflow modes, cancellation, stable IDs,
heartbeat execution on retry/empty final transactions, both closeout commit
orders, direct setter protection, and cleanup after queued writes.

New tests must be enrolled and observed under Bazel. Run formatter, gazelle, mod
tidy, complete just test, uncached affected suites and actual race flag. Freeze
and verify source hashes around final runs. Execute compiled/restored mutations
for conflict edges, commit-bound continuation, heartbeat coverage, cleanup and
state eligibility. Actual design and completed-milestone reviews are separate:
Graefe, Torvalds, independent storage/Java, and scoped SPFresh paper review.
Record exact trees and actual verdicts; research or incomplete reviews are not
ACKs. Publication and CI changes remain subject to user authorization.

## 7. Addendum: a session's start against the index state (found after acceptance)

### 7.1 Revision 1

Found while running the full suite for another workstream: `OnlineIndexer MutualIndexing Mutual
build "four concurrent mutual builders with 500 records"` failed once with `mark readable: index is
not built: "Order$price" has unbuilt ranges` (log kept at
`/var/tmp/fdb-upgrade-recovery/wse8-proto-full-logs/pkg_recordlayer_recordlayer_test.log`).

Cause. `prepareIndexingState` treated every state but WRITE_ONLY as a fresh build and cleared the
targets, READABLE included. Java never does that by default: `IndexingBase.handleStateAndDoBuildIndexAsync`
(IndexingBase.java:184-261) asks `IndexingPolicy.getStateDesiredAction(state)`
(OnlineIndexer.java:1112-1120), whose builder defaults are ifDisabled REBUILD, ifWriteOnly CONTINUE and
ifReadable CONTINUE (:1249-1253), and a READABLE index under CONTINUE is neither cleared nor built
(`shouldBuild = shouldClear || !indexState.isReadable()`). Go had no DesiredAction at all. So a mutual
builder that started after a peer had published cleared the index (range set included) and re-armed
it WRITE_ONLY, and every builder that had already finished its fragments then failed to publish. The
interleaving is pinned deterministically by "a finished builder's publication survives a late peer's
session start". An earlier version of that spec reproduced the exact message before the fix; the
committed spec asserts the late peer's session outcome first, so before the fix it fails there, one
step before the message.

Design, Java's handling in the state transaction: `IndexingPolicy` gains `IfDisabled`, `IfWriteOnly`
and `IfReadable` of type `IndexingDesiredAction` (ERROR, REBUILD, CONTINUE, MARK_READABLE), the zero
value standing for Java's builder default, and `GetStateDesiredAction` (READABLE_UNIQUE_PENDING is
always MARK_READABLE; an unknown state or action is a `RecordCoreError`). `prepareIndexingState`
returns the session start: ERROR refuses with Java's "Index state is not as expected"; MARK_READABLE
publishes without building (only when publication is enabled); REBUILD clears; a READABLE index
otherwise is skipped. A follower whose state differs from the primary's is refused unless its own
action is REBUILD and the session is fresh, in which case it alone is cleared; a fresh session marks
the uncleared targets WRITE_ONLY without clearing them (DISABLED under CONTINUE). (Revision 1
recorded Java's ifMismatchPrevious as unused. That was wrong: `OnlineIndexer.indexingCatcher` reads
it, and 7.2 ports the catcher.) `BuildIndex` on a READABLE index therefore returns 0 records by
default; the two specs that pinned the old clear-and-rebuild are now written against Java's semantics
("rebuilds from READABLE state" asks for REBUILD; "resumes multi-target build" was rewritten in 7.2 to
resume an interrupted build).

Tests (Describe "index state desired action"): the resolution table with nil and empty policies and
both refusals; a READABLE index under the default, MARK_READABLE and REBUILD; ERROR for each of the
three states; WRITE_ONLY and DISABLED under their default and the other action; both follower rules;
READABLE_UNIQUE_PENDING with publication off, with violations present, and after they are resolved.
An orphan entry written into each index subspace witnesses whether the session cleared it. Mutations,
each red and restored by cmp: a READABLE index cleared as fresh (3 specs, incl. the reproducer),
the `|| continued` follower term dropped (1 spec), the DISABLED default made CONTINUE (3 specs)
(`/var/tmp/fdb-upgrade-recovery/desired-mut1.log`, `desired-mut2.log`, `desired-mut23.log`).
WHAT RELIED ON THE OLD BEHAVIOUR, found by a census rather than by the failures alone: the
skip outcome was made to return an error (a temporary mutation) and every target that calls
`BuildIndex` was run (recordlayer, chaos, fleet, sqldriver, conformance and the frl command
tests; `/var/tmp/fdb-upgrade-recovery/skip-census.log`), which lists every spec that reaches
the skip, including those that stayed green because the index was maintained on write anyway
and the build they meant to exercise silently did nothing. Each was a build of an index that
was READABLE only because the store was created with it; each now starts where Java's indexer
tests start, from DISABLED (`disableIndexes`, Java's `disableAll`), or, where the spec means a
rebuild of a READABLE index, asks for one with ifReadable REBUILD: the four queued-dispatch
specs, four format-15 lifecycle specs, both mutual heartbeat-renewal specs, the follow-up
liveness fixture (its closeout-rebuild competitor and its build-again spec ask for REBUILD),
"handles empty store", phase 1 of "rebuilds from READABLE state", the five chaos
OnlineIndexer tests (`populateRecords` leaves every index DISABLED), and the frl
interrupted-then-resumed test, which set the index WRITE_ONLY (from READABLE that keeps full
range coverage, so there was never work to resume) and now sets it DISABLED and fails if the
time-limited build is not interrupted. The census re-run afterwards
(`skip-census2.log`) lists only specs that mean to reach the skip: "leaves a READABLE index
alone", phase 3 of "resumes multi-target build", the two follow-up specs before their policy
change (re-run after it: `desired-fixed.log`, recordlayer 3635 of 3636 run, chaos and frl
green), and the four-builder spec, whose late builders legitimately find the index published.
After the fix the four-builder spec and the reproducer ran 40 times uncached under Bazel
(`--runs_per_test=40`), each run "SUCCESS! -- 2 Passed | 0 Failed" (40 of 40; logs of runs 1 and 40 in
`/var/tmp/fdb-upgrade-recovery/desired-loop-logs/`). These logs ran on revision 1's trees, some before
the census edits; 7.2's runs on revision 2's tree supersede them as evidence.

### 7.2 Revision 2: Java's build catcher, and heartbeat admission after the action

The revision 1 gate (`ws-c-addendum-review-v1`) returned three NAKs. Revision 2 addresses them
as follows.

**The catcher (all three reviewers, HIGH).** Java's `OnlineIndexer` runs every build through
`indexingLauncher`/`indexingCatcher` (OnlineIndexer.java:133-274). The catcher reads
`ifMismatchPrevious` on every `PartlyBuiltException`. Go had no catcher, so it returned
`PartlyBuiltError` in cases where Java's default recovers. Revision 2 ports it:
- **Policy fields.** `IndexingPolicy` gains `IfMismatchPrevious` (zero value = Java's builder
  default CONTINUE, `GetIfMismatchPrevious`) and `ForbidRecordScan` (online_indexer.go:75-79,
  :144).
- **Error fields.** `PartlyBuiltError` carries the `Saved` and `Expected` stamps. The new
  `UnexpectedReadableError` is Java's `UnexpectedReadableException`, with `AllReadable`
  (online_indexer_queue.go:37).
- **The loop.** `BuildIndex` (online_indexer.go:999) loops `buildIndexAttempt` (:1138) through
  `indexingCatcher` (:1035). Java counts attempts from one and gives up past
  `INDEXING_ATTEMPTS_RECURSION_LIMIT` = 5, so the most runs is six.
- **On `PartlyBuiltError`,** by `IfMismatchPrevious`:
  - *CONTINUE, single target:*
    - a saved BY_RECORDS or MULTI_TARGET_BY_RECORDS stamp falls back to a records scan;
    - a saved BY_INDEX stamp retries by index from the saved source. The source is resolved
      as Java resolves it, through `Index.decodeSubspaceKey` then `getIndexFromSubspaceKey`
      (`savedSourceIndex`, :1121). A malformed key or an unknown index fails with Java's
      messages. The requested policy and source are kept for the next arm;
    - any other saved method fails.
  - *CONTINUE, multi-target:* fails.
  - *REBUILD:* retries with `IfWriteOnly` REBUILD. The policy is copied, as Java's
    `toBuilder().build()` copies it, so the caller's struct is never written
    (`withIfWriteOnly`, :154).
  - *ERROR or MARK_READABLE:* fails.
- **On an `IndexingValidationError` from a BY_INDEX build:**
  - if a continuation's saved source failed, the requested method is restored with
    REBUILD;
  - otherwise, unless `ForbidRecordScan` or an earlier fallback, the build falls back to a
    records scan.
- **Under a mutual policy, on `UnexpectedReadableError`:** the build ends successfully if
  every target is readable, and otherwise falls back to a records scan.
- **Everything else** is returned.

Details of the port:
- **Choosing the method.** The fallback is Java's sticky `fallbackToRecordsScan`, plus the
  forced stamp overwrite that Java's `getIndexer` gives the records indexer
  (`enforcedStampOverwrite`, ORed into `setIndexingTypeOrThrowForIndex` at :1502).
  `buildsMutually()` and `buildsByIndex()` (:450-456) choose the method in `getIndexer`'s
  order: mutual first, then by index for a single target.
- **Where the policy's own flag still decides.** `oi.mutual` stays wherever Java reads
  `policy.isMutual()` rather than the indexer: the queue refusals (:1499 and
  online_indexer_queue.go:49) and the catcher's mutual arm.
- **One heartbeat per attempt,** created under the attempt's method, as each Java attempt
  builds a new indexer.
- **Declared divergence:** the throttle's adapted limit carries over into the next attempt,
  where Java's new indexer starts again from the configured limit.

**Source-index validation at session time.** `OnlineIndexerBuilder.Build` no longer refuses a
BY_INDEX source that cannot be used. Java validates it in
`IndexingByIndex.validateSourceAndTargetIndexes` after the state transaction, and the catcher
depends on that timing, because the failure is what drives its fallback. The port is
`validateSourceAndTargetIndexes` (:789), with Java's six messages in Java's order.
`buildRangeByIndex`'s two per-range checks are now `IndexingValidationError` too, with Java's
messages "target index is not idempotent" and "source index is not scannable" (:1921, :1929).
`Build` still refuses a source index with several targets or a mutual policy, as Java's builder
does.

**Target states in every build transaction.** `expectedIndexStatesOrThrow`
(online_indexer_queue.go:460) is Java's `IndexingThrottle.expectedIndexStatesOrThrow`
(IndexingThrottle.java:410-440). It runs at the top of `validateBuildSession`:
- all targets WRITE_ONLY: the build continues;
- all scannable: `UnexpectedReadableError{AllReadable: true}`;
- each either WRITE_ONLY or scannable: `UnexpectedReadableError`;
- anything else: `RecordCoreStorageError` "Unexpected index state(s)".

This replaces the Go-only `mutualBuildAlreadyReadable`. A late mutual builder now ends through
the catcher's all-readable arm, as Java's does (mutation m16 below). Behaviour change: a
non-mutual build whose target a peer publishes, or disables, mid-build now fails with Java's
exception type. Before, it failed with `IndexingValidationError` "index state changed during
build".

**Heartbeat admission after the action (Torvalds 2 and 4, storage F2).** A session now checks
heartbeats at two points:
- **At open.** `checkOpenHeartbeats` (online_indexer_queue.go:69) runs before the open-time
  metadata reconciliation, which can rebuild or disable indexes.
  - If the stored metadata version is current, no reconciliation runs. It then refuses only
    legacy and malformed heartbeat keys (`allowMutual`).
  - If reconciliation will run, it demands quiescence.
  - Java reconciles on open too, under a live peer: its indexer opens through `openAsync`
    (IndexingBase.java:129-130), which runs `checkVersion` (FDBRecordStore.java:6015) and
    `checkPossiblyRebuild` (:2689, :4841-4986) without looking at heartbeats. Go refuses the
    reconciliation until the peer's lease expires. (Revisions 1 to 3 said Java's indexer does
    not reconcile on open; that was false.)
- **In `prepareIndexingState`, after the action is resolved and the followers are checked.**
  - Only a session that builds is admitted, and only with `allowMutual = buildsMutually() &&
    continued` (:167). A clear or a fresh WRITE_ONLY mark admits no other live session, mutual
    or not.
  - Java admits mutual peers by the stamp's method even on a clear. Go keeps its invariant
    that nothing destructive happens under a live peer, and declares the difference at the
    call site.
  - Skip, MARK_READABLE and ERROR sessions check nothing, as in Java, where heartbeats are
    first touched in `setIndexingTypeOrThrow` on the build path.
  - Java checks the followers before the heartbeats, so a state mismatch now wins over a
    live peer.

**Queue refusal after the clears (Torvalds 3, storage F3).** The early "Mutual indexing cannot
continue a pending write queue index build" check in `prepareIndexingState` is gone. Java makes
that check only in `setIndexingTypeOrThrow`, after `clearAndMarkIndexWriteOnly`, and Go already
had the same check there (:1499). The consequences:
- a mutual REBUILD of a queued index clears it to WRITE_ONLY and builds, as Java does;
- a DISABLED primary with a queued follower is Java's state-mismatch `ValidationException`,
  not a `RecordCoreError`.

**MARK_READABLE over a queued index (Torvalds 5, storage F4).** A MARK_READABLE session records
no queued targets, in Go as in Java. `markReadable` used to retry `IndexNotBuiltError{PendingWrites}`
up to 100 times with nothing draining in between. It now retries that refusal only for a target
this session drains (`queuedInSession`, :1621), so the failure returns at once.
- *Declared divergence:* Java's store erases the queue as it marks the index readable
  (IndexingSubspaces.java:234-235), losing the queued writes. Go's `MarkIndexReadable` refuses
  a non-empty queue.

**Smaller items:**
- **`IndexVersion`.** `IndexingValidationError` gains `IndexVersion`, set for "Index state is
  not as expected" as Java's `INDEX_VERSION` key is, and `SourceIndexName` for the source
  checks.
- **Stale text corrected:**
  - `BuildIndex`'s and `markWriteOnly`'s doc comments, including the wrong
    `IndexMaintenanceFilter.NONE` note;
  - the BUG6 comment and spec title in bug_bounty3_indexer_test.go;
  - the fleet builder's READABLE_UNIQUE_PENDING comment.
- **`frl index build`.** On a READABLE index it now says so instead of printing "built … (0
  records scanned)". It reports a READABLE_UNIQUE_PENDING publication the same way (pinned in
  `TestIntegration_IndexSetState_ReadableRequiresBuilt`).
- **CHANGELOG.** It now covers:
  - MARK_READABLE only when publication is enabled;
  - READABLE_UNIQUE_PENDING with mark-readable off, which is now a no-op;
  - the heartbeat change;
  - the CLI change;
  - the catcher.

**Tests added or rewritten:**
- **Describe "index state desired action":**
  - a follower cleared alone (a DISABLED primary under CONTINUE keeps its orphan entry);
  - a skip that ignores a mismatched follower;
  - MARK_READABLE made observable: a seeded stamp is erased when publication is on, and kept
    when publication is off or the session skips;
  - MARK_READABLE across two targets;
  - a skip and an ERROR refusal despite a live peer, and a REBUILD refused under one (with
    `IndexVersion` asserted);
  - storage's multi-target window: the primary is published and a follower still carries the
    publisher's heartbeat, and a late exclusive or mutual session skips;
  - a mutual REBUILD refused with a live mutual peer, and a continued mutual build that admits
    one.
- **Describe "build catcher":**
  - a BY_RECORDS continuation reached from a BY_INDEX request;
  - a MULTI_TARGET continuation under the takeover policy;
  - a BY_INDEX continuation from the saved source;
  - a restore of the requested method, with REBUILD, when the saved source is unusable;
  - an unscannable source falling back, or refused under `ForbidRecordScan`;
  - `IfMismatchPrevious` REBUILD, ERROR and MARK_READABLE, with the caller's policy asserted
    unchanged;
  - a multi-target mismatch returned without a retry;
  - `expectedIndexStatesOrThrow`'s four classes;
  - a unit table that drives every catcher arm (19 rows, wrapped errors included).
- **The three BY_INDEX source refusals.** Each now builds by a records scan (the BY_INDEX stamp
  is overwritten, the range is complete, and three entries are counted raw), or returns Java's
  message under `ForbidRecordScan`.
- **The heartbeat admission matrix** (online_indexer_session_test.go) was rewritten against the
  two points above:
  - modes × blockers × states (readable, disabled, write-only, and mismatch for multi-target);
  - the expected outcome of each cell is stated in the comment above it;
  - every refusal or skip is asserted to leave every store byte unchanged.
- **Queued-dispatch specs:**
  - the queued follower of a DISABLED primary is a mismatch;
  - a mutual REBUILD of a queued index builds it and leaves the queue empty, while a mutual
    CONTINUE is refused;
  - MARK_READABLE over a non-empty queue fails in exactly two transactions, keeping the queued
    write.
- **Rewritten specs:**
  - "resumes multi-target build from partial progress" now resumes: a time-limited build
    commits 3 records and stops, and a fresh indexer scans the remaining 7;
  - "rejects a stamp mismatch" now asks for ERROR, and pins that CONTINUE over a BY_INDEX stamp
    naming no source fails with Java's decode message.

**Mutations.** Each was applied with a presence check, and gofumpt'd so that nogo builds it. Each
was run over the indexer focus (363 specs) and restored by `cmp`. Logs are in
`/var/tmp/fdb-upgrade-recovery/wsc-v2/mut-*.log`. Every one reddened at least one spec, as listed:

| mutation | reddened |
|---|---|
| m1 CONTINUE over BY_INDEX fails | 4: the BY_INDEX continuation, the requested-method restore, the stamp-mismatch spec, the arm table |
| m2 `allowMutual` without `&& continued` | 2: the mutual REBUILD refusal, the matrix cell mutual/live/disabled |
| m3 early mutual-queue check restored | 2: both new queued-dispatch specs |
| m4 PendingWrites retried without a drain | 1: the two-transaction MARK_READABLE spec |
| m5 admission before the action | 26: the three live-peer desired-action specs; the matrix's readable and mismatch cells and its mutual/live/write-only cell; the three concurrent mutual-build specs; the mutual heartbeat-renewal spec |
| m6 open-time check always exclusive | 14: the three live-peer desired-action specs; 7 matrix cells with a live blocker; the heartbeat-renewal spec; all three concurrent mutual builds |
| m7 fallback without the forced overwrite | 4: the three source refusals and the unscannable source |
| m8 all-readable reported as some-readable | 1: the classification spec |
| m9 REBUILD writes through the caller's policy | 2 |
| m10 `IndexVersion` dropped | 1 |
| m11 session-time source validation off | 6: all source-refusal specs |
| m12 multi-target falls back | 2 |
| m13 mutual all-readable not treated as done | 1: the arm table |
| m14 `ForbidRecordScan` ignored | 5 |
| m15 attempt limit off by one | 1: the arm table |
| m16 `expectedIndexStatesOrThrow` removed | 2: "four concurrent mutual builders" and "mutual builders with concurrent writes" |

m8 and m13 redden only the unit specs. For a single-target mutual build, "done" and "fall back,
then skip the READABLE index" end the same way. That is why the arm table exists.

**Evidence, all on tree `067b01f286d4c6a57a51f48dbcd9e603746280bb`.**
- **How the tree was taken.** It was written by `git write-tree` from a temporary index after
  `git add -A`. The gated tree differs from it only in this file and the review
  directory's reviewer settings, which `git diff --stat 067b01f2 <gated tree>` shows. The md5 of the two changed sources and the three
  changed spec files was recorded before the runs and re-verified after them
  (`/var/tmp/fdb-upgrade-recovery/wsc-v2/evidence-md5.txt`).
- **`just test`.** "Executed 43 out of 94 tests: 94 tests pass" (`wsc-v2/justtest.log`). The 51
  cache hits are on inputs identical to that tree.
- **Mutations.** The mutation table above was run from the `.good` copies of the two sources,
  which `cmp` equal to that tree's, with the spec files unchanged since.
- **Censuses.** Three censuses were run. Each applied a temporary mutation, ran every target
  that calls `BuildIndex` with per-test output, and was restored by `cmp`. The targets were:
  - recordlayer: 3687 of 3688 specs ran;
  - chaos: 230 `=== RUN`;
  - sqldriver's cardinality, unique-pending and fleet tests: 16 `=== RUN`;
  - conformance's two indexer Describes: 12 of 1554 specs ran;
  - the frl command tests: 374 `=== RUN`;
  - fleet: 14 `=== RUN`.

  A green target in these logs therefore means its tests ran and did not reach the mutated
  outcome (`wsc-v2/census-*/summary.txt`).
  - *Skip made an error:* 9 recordlayer specs and 1 frl test failed. All ten mean to reach the
    skip:
    - the four live-blocker "readable" matrix cells;
    - the five desired-action specs over a READABLE primary;
    - the new frl already-readable check.

    The revision 1 conversions therefore still hold.
  - *MARK_READABLE made an error:* 5 recordlayer specs failed, all of them MARK_READABLE specs
    (READABLE, READABLE_UNIQUE_PENDING, the stamp spec, the two-target spec, the queue spec).
    No other spec in any target publishes without a build, so the READABLE_UNIQUE_PENDING
    change from rebuild to publication is measured, not inferred.
  - *Any catcher recovery (retry or done) made an error:* 16 specs and one Go test failed, all
    in recordlayer:
    - the new catcher and source-refusal specs;
    - the three concurrent mutual builds, whose late builders end through the all-readable arm;
    - five specs over a BLOCKED stamp: "blocked stamp prevents build without policy", "…with
      wrong AllowUnblockID fails", "BlockIndex via OnlineIndexer sets block on stamp", "Format
      15 queued lifecycle cleans and resumes a queued build after blocked", and
      `TestIndexBlockLeaseReadsTheEnvClock`. Their outcome is unchanged: a blocked BY_RECORDS
      stamp raises `PartlyBuiltError`, the CONTINUE arm falls back to a records scan, which
      raises it again until the attempt limit, and the error is then returned, as in Java,
      which likewise runs a blocked build six times.

    No spec in chaos, sqldriver, conformance, frl or fleet reaches a recovery arm.
- **Loop.** The reproducer and the four-builder spec ran 40 times uncached under Bazel
  (`--runs_per_test=40`, `wsc-v2/loop40.log`). All 40 runs report "Ran 2 of" and "SUCCESS! -- 2
  Passed | 0 Failed".

### 7.3 Revision 3: one indexer identity, the session outcome, and Java's error classes

The revision 2 gate (`ws-c-addendum-review-v2`) returned three NAKs. All three confirmed the catcher
against Java arm by arm; the findings were about Go-only behaviour. Revision 3:

**The session outcome, not a separate open (all three, MEDIUM).** Revision 2's `frl index build`
opened the store in its own committed transaction to read the index state for its message. That open
reconciled out-of-date metadata without `checkOpenHeartbeats`, bypassing the rule that reconciliation
demands quiescence, and read a state that could change before the build. It is gone. Instead:
- `OnlineIndexer.LastBuildOutcome` (online_indexer.go:508, Go-only, declared in DIVERGENCES.md)
  reports what the last `BuildIndex` did: `Built`, `LeftAlone` (a READABLE primary under CONTINUE,
  or MARK_READABLE with publication disabled), `Published` (MARK_READABLE), `CompletedByPeers` (the
  catcher's mutual all-readable arm), or `None` before a successful call.
- `frl index build` renders it (`indexBuildSummary`, index_write.go:317, pinned for every outcome by
  `TestIndexBuildSummary_OneLinePerOutcome`, which also closes revision 2's unpinned
  READABLE_UNIQUE_PENDING message).
- The fleet counts an index as built only when its session built or published it
  (`builtBySession`, fleet/build.go:270, `TestBuiltBySessionCountsOnlyWhatTheSessionDid`). A tenant
  whose pending index a peer published first reports no work, not "built" with nothing built.
  This closes v1's Torvalds 7.

**One indexer identity (all three, MEDIUM).** Java's heartbeat key is `IndexingCommon.indexerId`,
minted once and shared by every indexer the catcher builds. Revision 2 minted a UUID per attempt, and
its reason ("as each Java attempt builds a new indexer") was right about the object but wrong about
the identity. Now `oi.newHeartbeat` (:474) mints the identity once per `OnlineIndexer`, and every
attempt, the mutual builder and `MergeIndexes` write under it, and the preparation check reads
under it (it writes nothing).
- Without this, a failed best-effort cleanup made the next exclusive attempt see its own leftover key
  as a live peer.
- Pinned by "keeps one heartbeat identity across attempts": the indexer's own leftover heartbeat
  admits a REBUILD, and another indexer's heartbeat still refuses one.

**Java's error classes at the two Go-only state checks (Graefe 3, Torvalds 5, storage 6).**
- Two checks used to return `IndexingValidationError`:
  - the follow-up check of a target that is no longer write-only (online_indexer_queue.go:439);
  - the per-transaction check of a target that moved between WRITE_ONLY and WRITE_ONLY_WITH_QUEUE
    (:516).
- That class sent a BY_INDEX build to the catcher's records-scan fallback, which Java never takes
  there. Both now return `RecordCoreStorageError` "Unexpected index state(s)", the class Java's state
  check raises.
- Pinned by the follow-up specs (three) and "does not fall back from a BY_INDEX build whose target's
  queue state changed mid-build".

**A source index the metadata does not define (Graefe 4, Torvalds 6).** Java resolves the source in
the state transaction and throws `MetaDataException`, which its catcher does not handle. Go now
refuses it before stamping, with Java's "Index <name> not defined" (:172), writing nothing ("refuses
a source index the metadata does not define before writing anything"). The synthetic-type check in
`validateSourceAndTargetIndexes` is unreachable until Go models synthetic record types
(`IsSynthetic` is always false), so five of Java's six messages are reachable today.

**Declared rather than changed:**
- **The error precedence against a live peer (Graefe 5, Torvalds 4).** Go admits a session before any
  of its writes, so that a refused session buffers nothing; the admission matrix's direct cells commit
  the refused transaction to prove it. Java checks each target's stamp before its heartbeat. So with a
  live peer holding the index, Go reports `SynchronizedSessionLockedError` where Java reports the
  `PartlyBuiltException` of a blocked stamp, a MUTUAL stamp under CONTINUE, or any mismatch under
  ERROR. Recorded in DIVERGENCES.md with revision 2's other declared differences:
  - admission on a clear or a fresh mark;
  - open-time quiescence;
  - the throttle limit carried over between attempts;
  - MARK_READABLE over a non-empty queue;
  - the queue-flag state check;
  - `LastBuildOutcome`.
- **A refusal after the state transaction (storage 4).** Source validation runs after the state
  transaction has committed, as in Java. So a `ForbidRecordScan` refusal, or a failure at the attempt
  limit, leaves the target WRITE_ONLY under a BY_INDEX stamp, cleared if its action was REBUILD. The
  three `ForbidRecordScan` specs now assert that state, and the CHANGELOG says it.

**Smaller items:**
- **Builder refusals.** `Build` refuses a source index with several targets or a mutual policy with
  Java's `IndexingValidationError` messages ("Indexing multi targets by a source index is not
  supported (yet)", "Indexing mutually by a source index is not supported (yet)"), pinned in
  "rejects SetSourceIndex with multi-target". The `SetSourceIndex` doc says where each check
  runs.
- **`GetIfMismatchPrevious`** refuses an action outside the enum, as `GetStateDesiredAction` does.
- **`BuildIndex`'s count** documents that it includes every attempt's records.
- **The `PartlyBuiltError` advice** in frl and the fleet no longer says "rerun with the same
  settings". A mismatch that reaches them is blocked, MUTUAL, or a MULTI_TARGET build the takeover
  rules refuse.
- **CHANGELOG corrections:**
  - which partial builds are continued. Revision 3 said one that is not costs six sessions;
    under CONTINUE a saved MUTUAL stamp (blocked or not) and a multi-target build fail at once
    (Java `OnlineIndexer.java:212-213`, Go the catcher's fall-through), a MUTUAL one continues
    only under `TakeoverMutualToSingle`, and only a blocked BY_RECORDS, MULTI_TARGET or BY_INDEX
    stamp, or a MULTI_TARGET one the takeover rules refuse, costs six sessions (revision 4);
  - that live heartbeats still stop a session when reconciliation is pending or a key is legacy or
    malformed;
  - "not scannable" was never a builder refusal;
  - the `RecordCoreStorageError` for a target disabled mid-build.
- **7.1's reproducer sentence** now says an earlier version of the spec reproduced the message.
- **The desired-action Describe's `setup`** now numbers its stores. `specSubspace` is one subspace per
  spec, and a spec that set up several stores silently reopened the first; the new outcome spec
  found it.

**New specs:**
- **"session outcome, identity and mid-build state changes":**
  - every outcome;
  - a mutual build completed by its peers (a committed state change injected before the build's
    second transaction by a counting transactor, `nthTransactHook`);
  - a non-mutual build whose target is published mid-build (`UnexpectedReadableError`) or disabled
    mid-build (`RecordCoreStorageError`), end to end;
  - the queue-flag move;
  - the undefined source;
  - the identity.
- **The two frl and fleet unit tests above.**

**Mutations.** The mutation script now records its perl expression and the resulting diff in each log
(`/var/tmp/fdb-upgrade-recovery/wsc-v3/mut-*.log`). Each mutation was run over the indexer focus (369
specs) and restored by `cmp`. Every one reddened at least one spec:

| mutation | reddened |
|---|---|
| m17 open-time check never exclusive | the matrix's two metadata cells with a live blocker (the other direction of revision 2's m6) |
| m18 a fresh identity per attempt | the identity spec |
| m19a follow-up error back to a validation error | the three follow-up specs |
| m19b queue-flag error back to a validation error | the BY_INDEX queue-flag spec |
| m20 undefined-source check removed | its spec |
| m21 `Published` reported as `Built` | the outcome spec |
| m22 `CompletedByPeers` not reported | the peers spec |

7.1's three mutations were re-run on this tree (Graefe 6, storage nit):
- m71a, a READABLE index cleared as fresh: 20 specs, the reproducer among them;
- m71b, the continued-follower term dropped: 1 spec;
- m71c, the DISABLED default made CONTINUE: 3 specs.

**Evidence, on tree `2b0de4e86c101b28316f4d35b152845d1904a696`** (written from a temporary index after
`git add -A`; the gated tree differs from it only in this file and review-output files of this
gate and of the concurrent WS-J gate, none of them a test input, as `git diff --stat` of the two
trees shows):
- **md5.** Twelve changed sources and specs were md5'd before the runs and verified after
  (`wsc-v3/evidence-md5.txt`, 0 not OK).
- **`just test`:** "Executed 19 out of 94 tests: 94 tests pass" (`wsc-v3/justtest.log`); the cache
  hits are on identical inputs.
- **The three censuses, re-run against revision 3's code** (`wsc-v3/census-*/summary.txt`, each with
  its perl and diff):
  - **Skip:** 10 recordlayer specs and the frl already-readable test. The eleventh is revision 3's
    outcome spec.
  - **MARK_READABLE:** 6 recordlayer specs. The sixth is the outcome spec. frl's summary and the
    fleet mapping are unit-tested, so they do not reach the indexer.
  - **Catch:** 17 recordlayer specs plus `TestIndexBlockLeaseReadsTheEnvClock`. That is the
    revision 2 set, plus the peers spec.
  - **Coverage:** chaos, sqldriver, conformance, frl and fleet reach no recovery arm, with per-target
    RUN counts 230, 16, 12 specs, 375 and 15.
- **The loop.** The reproducer and the four-builder spec: 40 uncached runs under Bazel
  (`--runs_per_test=40`, `wsc-v3/loop40.log`). All 40 runs report "Ran 2 of" and
  "SUCCESS! -- 2 Passed | 0 Failed".

### 7.4 Revision 4: the late cleanup clear, the call's outcome, and Java's context

The revision 3 gate (`ws-c-addendum-review-v3`) returned three NAKs. All three found the catcher,
the phases and every v2 finding resolved, and none asked for a design change. Revision 4:

**A late cleanup clear no longer erases a live heartbeat (storage 1, MEDIUM).** Revision 3's one
identity per `OnlineIndexer` means the next attempt, `BuildIndex` or `MergeIndexes` of an indexer
writes the SAME heartbeat key its previous attempt's bounded cleanup clears. That cleanup returns at
its deadline while a dispatched pure-Go commit is detached and can land later; a clear landing after
the next write would erase a live heartbeat and admit a peer's exclusive REBUILD or fresh mark under
a running session, failing OPEN where revision 2's failure mode was a lockout. The cleanup's premise
("each build uses a new UUID", `ws-c-cleanup/README.md`) went with the per-attempt identity.
- `cleanupHeartbeatAttempt` (online_indexer_queue.go) now READS each key before clearing it, so the
  clear is conditional on no write of the key since its read version: a late commit conflicts with
  the next attempt's write, or is too old. Java awaits its clear before the next session
  (IndexingBase.java:174) and has no late commit.
- Pinned by "a cleanup clear that lands after the next attempt wrote the heartbeat leaves it in
  place": the barrier holds the cleanup's commit past the deadline (a new `holdDispatch` mode, whose
  held commit ignores cancellation and the timeout, as a detached pure-Go commit does), the same
  heartbeat is written, the held commit then lands and must fail with 1020, and the key survives.
  Without the read the late clear commits (m23, the spec's only failure: "the late clear committed:
  <nil>").
- README corrected; the grep for "new UUID" over the design tree finds no other copy.

**False Java claim fixed (Graefe 1).** Three places said Java's indexer does not reconcile on open.
It does: it opens through `openAsync` (IndexingBase.java:129-130), whose `createOrOpenAsync` runs
`checkVersion` (FDBRecordStore.java:6015) and `checkPossiblyRebuild` (:2689, :4841-4986) without
looking at heartbeats. The divergence is "Java reconciles under a live peer; Go refuses until the
lease expires", now stated at `checkOpenHeartbeats`, in DIVERGENCES.md and in 7.2 above. A grep for
"not reconcile on open" / "without index maintenance" (review and research directories excluded)
finds only 7.2's note that the old claim was false.

**The follow-up refusal is declared (Graefe 2, Torvalds 4).** `refreshFollowupHeartbeats` fails the
drain or merge transaction of a target disabled under the session; Java's follow-up skips such a
target (IndexingBase.java:969-972) and commits, and whatever runs next fails: a following build
transaction's state check, or after the last range `markIndexReadable` with an
`IndexNotBuiltException`. The comment no longer says the next build transaction always follows, and
DIVERGENCES.md's section on the session start gains the bullet.

**The retry cost is stated correctly (Graefe 3, Torvalds 2, storage 2).** CONTINUE over a saved
MUTUAL stamp, blocked or not, and a multi-target build fail at once (Java OnlineIndexer.java:212-213;
Go the catcher's fall-through); a MUTUAL one continues only under `TakeoverMutualToSingle`, whose set
is empty by default. Only a blocked BY_RECORDS, MULTI_TARGET or BY_INDEX stamp, or a MULTI_TARGET one
the takeover rules refuse, costs six sessions. CHANGELOG and 7.3 corrected. The attempt limit is now
pinned end to end (storage 4): "blocked stamp prevents build without policy" captures the catcher's
log and asserts sessions 1 to 5 caught and relaunched and the sixth's refusal returned (m32, the
limit lowered to 4, reddens it; the arm table's own row is self-referential and does not).

**The precedence divergence is a rule, not a list (storage 3).** With a live peer and a stamp that
does not match, Go returns `SynchronizedSessionLockedError` on the first attempt; Java returns it
only where its catcher's path ends on a heartbeat check, and otherwise ends as its catcher does. The
DIVERGENCES.md bullet now says that, and names the cases the list missed (MARK_READABLE, a refused
MULTI_TARGET takeover, an unresolvable saved BY_INDEX source).

**`LastBuildOutcome` is the call's (Torvalds 1).** An attempt that indexed records and then failed
into a retry built them, whatever the next attempt finds: a peer may publish in between, and the
attempt that ends the call then leaves the READABLE index alone or ends the mutual build as
completed by its peers. `BuildIndex` now reports `Built` when any failed attempt indexed records,
and the constants' docs say so (`LeftAlone` also names its two cases, one unreachable from frl).
Pinned by "reports a call whose one attempt built before the peers published, ending through the
done arm, as built" (so titled since revision 6; revision 4 called its one attempt "earlier") (the
peers publish before the fourth transaction of a mutual build of limit 1; m24 reddens it). That spec
ends through the catcher's done arm only; the success return, the retry case this finding named, was
left unpinned until revision 5 (7.5). The reset
at the call's entry is pinned too (Graefe 5, Torvalds 1, storage 6): "reports what each session did"
now builds once and then fails on the same indexer, which must report `None` (m25).

**Java's `INDEXER_ID` (Graefe 4, storage v2 #2).** `PartlyBuiltError` and the source-index
`IndexingValidationError` carry `IndexerID`, the indexer's identity, where Java attaches
`INDEXER_ID` (IndexingBase.java:604, :1011-1012, :1171-1174); the state checks, which Java throws
without it (:203-207, :237-241), leave it nil. Pinned by the blocked-stamp spec and the three
ForbidRecordScan specs (m28, m33).

**The source index is the metadata's (storage 5).** Java's policy holds the source index's NAME and
each session compiles the stamp and validates the source from `metaData.getIndex(name)`
(IndexingByIndex.java:68-71, :95-101); Go used the caller's `*Index`, so an object with another
subspace key or version wrote a stamp Java would not. (The scan already read the source by name, so
the resolution does not change it.) Each BY_INDEX attempt now resolves the name against the metadata
first (an undefined name is still refused in the state transaction). Pinned by the spec revision 5
retitles "stamps and validates the metadata's source index, not the caller's object, and names its
heartbeat by the method": the source object has a foreign subspace key and version, and the build
stamps the metadata's key and version (m26); revision 5 gives the object a non-VALUE type under
ForbidRecordScan, so a validation of the object fails the build (7.5).

**Tests that could not catch a regression (Graefe 5, Torvalds 3, 5, 6, storage 4).**
- The unscannable-source ForbidRecordScan spec now asserts the state the refused session leaves: the
  DISABLED target's REBUILD cleared it (its planted orphan key is gone) and left it WRITE_ONLY under
  a BY_INDEX stamp, the CHANGELOG's "(and cleared, if its action was REBUILD)".
- The catcher's arm table gains the unknown `IfMismatchPrevious` action (m31).
- The fleet's per-tenant event is `buildPending` (fleet/build.go), driven by
  `TestBuildPendingReportsWhatTheTenantBuilt` over every arm: nothing pending, all built, one left to
  a peer, all left to peers (no work), and a failure part way. Revision 3 had moved
  `total += n` before the error check, so a failed tenant's `Records` counted the failed index while
  `Indexes` omitted it; it is back after the check (m30 reddens the failure arm).

**Nits.**
- Java names a build's heartbeat by its stamp's method (`IndexingBase.java:457`, the heartbeat's
  `info`, which Go's lock error reports as `ExistingInfo`, a Go addition: Java's
  `SynchronizedSessionLockedException` carries no info key); Go wrote "online index build". The attempt's heartbeat
  now carries the method (`BY_RECORDS`, `BY_INDEX`, `MULTI_TARGET_BY_RECORDS`, `MUTUAL_BY_RECORDS`),
  pinned by the source-index spec, which reads the live heartbeat mid-build (m27).
- `newHeartbeat` passes the indexer's identity to the heartbeat constructor, as Java's constructor
  takes it (IndexingHeartbeat.java:66), instead of minting a UUID and overwriting it: one fewer
  draw from the simulation's randomness per heartbeat.
- `OnlineIndexerBuilder.Build` follows `validateIndexSetting`'s order (OnlineIndexer.java:866-878):
  a source index with more than one target is refused on the count BEFORE duplicates are removed,
  then the mutual-with-source refusal, then the targets' and record types' presence. One target
  added twice with a source index is refused, as in Java (m29).
- The frl and fleet `PartlyBuiltError` advice no longer offers continuations neither tool can run
  (a mutual or same-targets rerun): a build that reaches it is blocked, or was begun by a mutual or
  multi-target build to be finished by the builder that began it, or is rebuilt. An empty message no
  longer renders as "()". Both are pinned for what they must and must not say.
- `buildIndexAttempt`'s doc names the shared identity; 7.3's "the preparation check writes" is
  "reads".
- Kept: the nil-session-heartbeat branches of `checkHeartbeatAdmission` and
  `newMutualIndexBuilder`. Revision 3's reviewers found them reachable only from tests; they are the
  entry of the specs that drive `markWriteOnly`, `prepareIndexingState` and the mutual builder
  directly (17 lines in five test files: `git grep -c -E '\.markWriteOnly\(|\.prepareIndexingState\(|newMutualIndexBuilder\('
  <tree> -- 'pkg/recordlayer/*_test.go'`, method calls of the first two and calls of the
  receiver-less third), which the production path reaches only inside an attempt. Removing them would move a `sessionHeartbeat` assignment into each of those specs for no
  production change.

| mutation | reddened |
|---|---|
| m23 the cleanup clears without reading | the late-clear spec |
| m24 an earlier attempt's records ignored | the earlier-attempt spec |
| m25 the reset at the call's entry removed | the outcome spec |
| m26 the source index not resolved by name | the source-index spec |
| m27 the heartbeat info back to "online index build" | the source-index spec |
| m28 `PartlyBuiltError` without `IndexerID` | the blocked-stamp spec |
| m29 targets counted after the dedup | the builder spec |
| m30 the fleet's `total += n` before the error check | `TestBuildPendingReportsWhatTheTenantBuilt/a_failure_part_way` (go test) |
| m31 an unknown `IfMismatchPrevious` action continues | the arm table |
| m32 the attempt limit lowered to 4 | the blocked-stamp spec |
| m33 the validation error without `IndexerID` | the three ForbidRecordScan specs |

Each Bazel mutation ran the revision 3 focus ("Ran 372 of 3698", the three new specs added); every
log shows marker count 1, bazel exit 3 (a test failure, not a build failure) and "restored"
(`wsc-v4/mut-*.log`, `wsc-v4/run-mutations.sh`, `wsc-v4/mutate.sh`). The unmutated focus passed 372 of
372 (`wsc-v4/focus1.log`).

**Evidence, on tree `9f1cd7090c25c7379ba5fce4b71d38aa9bf1a73d`** (run together with WS-J design v8's, whose files are
in the same hashed set; `ws-j-oracle/evidence-v8.txt` lists all 66 hashes and the driver):
- **Focus.** The revision 3 focus plus the FDBMetaDataStore Describe: "Ran 385 of 3698", all passed
  (`ev8/wsc-focus.log`).
- **The loop.** The reproducer and the four-builder spec, 40 uncached runs (`ev8/wsc-loop40.log`): 40 × "SUCCESS! -- 2
  Passed | 0 Failed", "Executed 1 out of 1 test".
- **`just test`:** "Executed 43 out of 94 tests: 94 tests pass" (`ev8/justtest.log`); the tree snapshot before and after
  the run is the same, and `sha256sum -c` over the hashed files after it reports 0 not OK.
- A first run (`ev8-run1/`) failed to build `//pkg/relational/core/fleet:fleet_test` (nogo: `reflect.DeepEqual` over
  `fleet.Event`, which holds an error); `TestBuildPendingReportsWhatTheTenantBuilt` now compares the fields, m30 was
  re-run against it (red, `wsc-v4/mut-m30.log`), and the whole driver re-ran.

### 7.5 Revision 5: the target object, the retry's outcome, and the standalone merge

The revision 4 gate (`ws-c-addendum-review-v4`) returned three NAKs. All three found the late-clear
fix correct and non-vacuous and every v3 finding addressed; the findings were small gaps beside what
revision 4 fixed. Revision 5:

**The target is the metadata's own object (Graefe 1, Torvalds 3, storage 2; wire).** Java's
`validateIndexSetting` ends with `index != metaData.getIndex(name)` → `MetaDataException("Index <n>
not contained within specified metadata")` (OnlineIndexer.java:880-883), and starts with
`MetaDataException("index must be set")` (:862-864). Go checked the name only, with a plain
`fmt.Errorf`, and then keyed the range set, the heartbeat subspace, the stamp and the clears off the
caller's object, so a same-named object with a foreign subspace key built where Java never looks and
where a Java peer cannot see its heartbeat. `OnlineIndexerBuilder.Build` now refuses a target that is
not the metadata's object, and an empty target list, with Java's class and messages. Pinned by
"rejects a target that is not the metadata's own index object, as Java does" (a same-named object
with a foreign subspace key, and an unknown name; m35) and "rejects empty target indexes" (now the
`MetaDataError`). One existing spec built its indexer from a renamed copy of the metadata with the
ORIGINAL index object; it now takes the renamed metadata's own object
(`online_indexer_user_identifier_test.go`), the only change the refusal required across
`recordlayer_test`, `fleet_test` and the frl tests (all green, `wsc-v5/rl-full.log`). The two
production callers, frl and the fleet, already pass the metadata's own objects (`lookupIndex`,
`PendingIndexes`).

**The retry's outcome is pinned (Torvalds 2, storage 1).** Revision 4's spec for an earlier attempt
that built ended through the catcher's done arm, so the success return, the case v3 Torvalds 1
described, was unpinned: reverting it to the last attempt's outcome stayed green. New spec "reports a
call as built when a retried attempt then finds the index published and leaves it alone": a BY_RECORDS
build of limit 1 indexes a record in its second transaction; before its third a peer changes the
stamp to BY_INDEX, so that transaction fails with a PartlyBuiltError and CONTINUE retries from the
saved source; before the retry's state transaction the peer publishes the index, and the retry
leaves it alone. The call must report `Built`; m34 (the success return reverted to the attempt's
outcome) reddens it and nothing else. A `multiTransactHook` runs a hook before any numbered
transaction; the spec asserts both hooks ran. The constants' docs now say what the value means for
the CALL: `Built` covers an attempt that indexed records and then failed into a retry or into the
catcher's end of a mutual build its peers completed; `CompletedByPeers` adds "and this call indexed
nothing"; `None`, `LeftAlone` and `Published` speak of the call. 7.4's paragraph notes that its spec
ends through the done arm.

**Java's context keys, completed (Graefe 3, Torvalds 1, storage nit).** Java attaches `INDEXER_ID`
to `SynchronizedSessionLockedException` too (indexing/IndexingHeartbeat.java:111-115), and
`INDEX_VERSION` to `PartlyBuiltException` (IndexingBase.java:1291). `SynchronizedSessionLockedError`
gains `IndexerID` (the refused indexer's identity; `ExistingIndexerID` was there) and
`PartlyBuiltError` gains `IndexVersion`. Pinned by the heartbeat spec "blocks when active heartbeat
from another indexer exists" (m36) and the blocked-stamp spec (m37). The heartbeat `info` claim is
corrected in the code comment and in 7.4: Java's lock exception carries no info key; Go's
`ExistingInfo` is a Go addition.

**The source-index spec discriminates what it claims (Graefe 4).** The scan always read the source by
name, so revision 4's `total == 5` could not fail. The spec is retitled "stamps and validates the
metadata's source index, not the caller's object, and names its heartbeat by the method" and its
source object now also has a non-VALUE type under ForbidRecordScan, so a build whose
`validateSourceAndTargetIndexes` read the object would fail: m38 (the resolution disabled) now fails
the build with "source index is not a VALUE index", where revision 4's m26 reddened only the stamp
assertion. The code comment, the CHANGELOG ("stamps and validates") and 7.4 say what resolution
changes: the stamp and the validation.

**Declared (Graefe 2).** A standalone `MergeIndexes` runs as a session: under the indexer's identity
it is admitted, writes a live heartbeat over a WRITE_ONLY target (which a Java exclusive builder honours and a mutual one does not, 7.7) and
fails over a target disabled under it, where Java's standalone merge writes no heartbeat and checks
no state (`OnlineIndexer.java:409-419`, `IndexingBase.java:1085-1096`, `:969-972`; revision 5 cited
`:1085-1096` as OnlineIndexer's, where those lines are the `IndexStatePrecondition` getters). Revision 5
said this "follows from" section 4; it does not (7.6): section 4 requires every backend write to
consume the heartbeat callback, and Java's callback over a null heartbeat does nothing, so the
session is Go's own choice. DIVERGENCES.md now declares it and replaces "No wire bytes differ"
with the one engine-observable difference, that heartbeat key. The follow-up bullet adds Java's
`SetMarkReadable(false)` case (the session succeeds and leaves the target DISABLED), and the
precedence bullet is scoped to a peer the admission check refuses (a live mutual peer admitted to a
continued mutual session gets the stamp error, as in Java) and names the REBUILD retry among the
paths that end on a heartbeat check (storage nit).

**Nits.**
- The late-clear spec accepts 1007 beside 1020: a held commit landing more than the MVCC window after
  its read version is too old, which also leaves the key alone.
- The fleet arm "every index left to peers" feeds sessions that indexed nothing (`n: 0`): after
  revision 4 a session that indexed records reports `Built`.
- The 17-line census states its pattern: `git grep -c -E
  '\.markWriteOnly\(|\.prepareIndexingState\(|newMutualIndexBuilder\(' <tree> --
  'pkg/recordlayer/*_test.go'` (17 lines in five files at `936a4aba`; the method calls of the first
  two, the receiver-less calls of the third).
- m30 is re-run under Bazel (`//pkg/relational/core/fleet:fleet_test`), its log carrying the file, the
  perl expression and the diff (`wsc-v5/mut-m30.log`: `a_failure_part_way` red, "restored").

| mutation | reddened |
|---|---|
| m34 the success return reports the last attempt's outcome | the retry spec |
| m35 targets checked by name only | the target-object spec |
| m36 `SynchronizedSessionLockedError` without `IndexerID` | "blocks when active heartbeat from another indexer exists" |
| m37 `PartlyBuiltError` without `IndexVersion` | the blocked-stamp spec |
| m38 the source index not resolved by name | the source-index spec, at the build ("the source validation read the caller's object") |
| m30 (re-run under Bazel) the fleet's `total += n` before the error check | `TestBuildPendingReportsWhatTheTenantBuilt/a_failure_part_way` |

Each ran the revision 4 focus plus the `IndexingHeartbeat` Describes ("Ran 374 of 3700"); every log
shows marker count 1, bazel exit 3 and "restored" (`wsc-v5/mut-*.log`, `wsc-v5/run-mutations.sh`,
`wsc-v5/mutate.sh`). The unmutated focus passed 374 of 374 (`wsc-v5/focus1.log`).

Combined evidence with WS-J design v9 (`ws-j-oracle/evidence-v9.txt`, logs in
`/var/tmp/fdb-upgrade-recovery/ev9/`), on the hashed bytes of the whole change set against HEAD (1602
files, `sha256sum -c` after `just test`: 0 not OK): the revision 5 focus with the `IndexingHeartbeat`
and `FDBMetaDataStore` Describes, "Ran 387 of 3700", 387 passed (`ev9/wsc-focus.log`; the 13 above
374 are the `FDBMetaDataStore` specs); the loop pair ("four concurrent mutual builders with 500
records", "a finished builder's publication survives a late peer's session start") 40 uncached runs,
40 "SUCCESS! -- 2 Passed | 0 Failed", 0 FAIL! (`ev9/wsc-loop40.log`); and `just test` on tree
`f2f272dc3dd1accdb3c5abc4185a16d930ced5fc`, equal before and after the run, "Executed 43 out of 94
tests: 94 tests pass." (`ev9/justtest.log`).

### 7.6 Revision 6: duplicate targets by Java's equality, setIndex, and the standalone merge pinned

The revision 5 gate (`ws-c-addendum-review-v5`) returned three NAKs, each with only low findings: every
v4 finding was resolved and the build session unchanged, and the findings were gaps beside what
revision 5 ported. Revision 6:

**Duplicate targets are removed by Java's equality (Graefe 2, Torvalds 1, storage 2).** `Build`
removed duplicates by NAME before the new identity check, so `[own, impostor]` dropped the impostor
and built, and "matches Java's HashSet dedup" was false: Java's `HashSet` uses `Index.equals`
(`Index.java:695-711`: name, type, root, subspace key, both versions, primary-key component
positions, options, predicate), so both objects survive and the impostor is refused
(`OnlineIndexer.java:871-883`). Go now removes duplicates with `Index.equalsJava`, meant as a port of
that equality (as first written it compared the subspace key by dynamic type and value, which is not
Java's: Java normalizes the key on assignment, so an Integer equals a Long and two byte arrays of one
content are equal; it also compared the canonical type and missed two root classes, all corrected in
7.7), keeping the FIRST of equal objects as `new HashSet<>(list)` does, and sorts the targets by name BEFORE
the identity check (`:879`), so the refusal names the alphabetically first foreign target. The
target-object spec pins `[own, impostor]` and `[impostor, own]` (refused), `[own, equal copy]`
(accepted, the copy dropped) and `[equal copy, own]` (refused, the copy kept, as Java keeps it), and the
alphabetical refusal. The redundant `found == nil ||` is gone.

**`setIndex` is Java's (Graefe 3 and 4, Torvalds 3).** Java's `setIndex` throws a
`ValidationException` when targets are already set (even for a null), skips a null, and
`addTargetIndex` checks nothing (`OnlineIndexer.java:668-676`, `:719-722`). Go's `SetIndex` replaced
the list and marked a "single mode" that `Build` refused beside more targets, so `SetIndex(a).
AddTargetIndex(b)` failed (Java builds both), `AddTargetIndex(b).SetIndex(a)` silently built only `a`
(Java throws), and `SetIndex(nil)` panicked at `idx.Name`. `SetIndex` now holds Java's refusal as an
`IndexingValidationError` that `Build` returns first (the builder chains, so the throw is deferred to
the call that can return it) and skips a nil; the single mode is deleted; a nil passed to
`AddTargetIndex` (Java's is `@Nonnull` and fails with a NullPointerException) is reported as "index
must be set" rather than a panic. The spec "rejects SetIndex combined with AddTargetIndex" pinned the
non-Java behaviour and is replaced by "combines SetIndex and AddTargetIndex as Java's builder does"
(both orders, a null after a target, `SetIndex(nil)`, a nil `AddTargetIndex` alone and after a target).

**The standalone merge is pinned and declared completely (Graefe 1, Torvalds 2, storage 1).** The
merge's divergence named no spec, and the only standalone `MergeIndexes` test used a READABLE vector
index that reaches neither branch. New Describe "a standalone MergeIndexes session"
(`indexing_merger_test.go`): over WRITE_ONLY the merge's own transaction commits its heartbeat
(info "explicit index merge", read after that transaction through an `afterTransactHook`) and the
heartbeat is gone afterwards; a live peer's heartbeat refuses the merge with
`SynchronizedSessionLockedError` and is left as it was; a target DISABLED before the merge fails it
with `RecordCoreStorageError` "Unexpected index state(s)"; over a READABLE target the merge runs and
no heartbeat is ever written (the heartbeat step skips it; the spec is so titled since 7.7). DIVERGENCES.md now states the refusal by a live peer (which "admitted
exclusively" only implied), covers a target disabled before the merge as well as under it, cites
`IndexingBase.java:1085-1096` (7.5 is corrected in place), presents the session as Go's choice
rather than a consequence of section 4, names the four specs, and marks the Java builder's honouring
of the key as read from source. Its closing sentence was false: several refusals above it leave
state Java would have changed; it now says so and names the merge's heartbeat as the one key Go writes
that Java would not. The CHANGELOG gains a `MergeIndexes` entry: the API is new on this branch
(`git grep MergeIndexes origin/master` finds nothing), and it now locks out a Java exclusive builder (a mutual one runs beside it, 7.7).

**Nits.** The `IndexBuildOutcomeBuilt` doc again covers every successful call some attempt of which
indexed records (a retry that publishes under MARK_READABLE included; 7.7 adds the attempt that ran
the build and indexed nothing). The v4 spec is retitled "reports
a call whose one attempt built before the peers published, ending through the done arm, as built"
(7.4's reference follows). `nthTransactHook`'s doc is back on its type and `multiTransactHook` has its
own. The CHANGELOG says `SynchronizedSessionLockedError.IndexerID` is the refused indexer's.


**Mutations and evidence (revision 6).** Each ran the revision 5 focus plus the new Describe "a
standalone MergeIndexes session" under Bazel ("Ran 378 of 3704"; the unmutated focus passed 378 of
378, `wsc-v6/focus1.log`); every log shows marker count 1, the perl expression and the applied diff,
bazel exit 3, and "restored" (`wsc-v6/mut-*.log`, `wsc-v6/run-mutations.sh`, `wsc-v6/mutate.sh`):

| mutation | reddened |
|---|---|
| m39 duplicates removed by name again | the target-object spec |
| m40 the targets sorted in reverse before the check | the target-object spec (the alphabetical refusal) and the combined-builder spec, and 13 more specs whose primary index the order decides |
| m41 `SetIndex` without its refusal | the combined-builder spec |
| m42 `Build` without its nil-target guard | the combined-builder spec, [PANICKED] (the nil `AddTargetIndex`) |
| m43 `MergeIndexes` without its session | the WRITE_ONLY, live-peer and DISABLED merge specs |
| m44 the follow-up's non-WRITE_ONLY refusal off | the DISABLED merge spec |

Combined evidence with WS-J design v10 (`ws-j-oracle/evidence-v10.txt`, logs in
`/var/tmp/fdb-upgrade-recovery/ev10/`), on the hashed bytes of the whole change set (1626 files,
`sha256sum -c` after `just test`: 0 not OK): the revision 6 focus with the `FDBMetaDataStore`
Describe, "Ran 391 of 3704", 391 passed (`ev10/wsc-focus.log`); the loop pair, 40 uncached runs, 40
"SUCCESS! -- 2 Passed | 0 Failed" (`ev10/wsc-loop40.log`); and `just test` on tree
`6cef98241e4a652184b9b9dfd6ca0462439c9fe0`, equal before and after, "Executed 41 out of 94 tests: 94
tests pass." (`ev10/justtest.log`).

### 7.7 Revision 7: Java's equality in full, subspace keys compared as Java compares them, and the merge's lock-out scoped

The revision 6 gate (`ws-c-addendum-review-v6`) returned three NAKs, each with only low findings.
All three found that `equalsJava` was not Java's `Index.equals` in three fields; revision 6 had
presented it as a port. Porting the subspace-key comparison properly found the same wrong
comparison at every other place Go compares subspace keys, two of which decide what Go accepts as
meta-data. Revision 7:

**`equalsJava` is Java's `Index.equals` (Graefe 1, Torvalds 1 and 2, storage 1).**
- *Type.* Compared as spelled (`Index.java:703`), not through `CanonicalType()`: `min_ever` is not
  `min_ever_long`, so `[own(min_ever_long), copy typed min_ever]` keeps the copy and is refused, as
  in Java.
- *Subspace key.* Java normalizes the key whenever it is assigned (`Index.java:88-97`, `:132`,
  `:187`, `:222-224`, `:413-415`; `FormerIndex.java:51-58`) with `TupleTypeUtil.
  toTupleEquivalentValue` (`TupleTypeUtil.java:98-124`), and compares the normalized objects with
  `equals`. The new `subspaceKeyIdentity` (`subspace_key_identity.go`) returns a comparable Go value
  that two keys share exactly when Java holds them equal: every integer that fits is one Long; a
  BigInteger strictly inside the Long range is that Long and one AT a bound stays a BigInteger (the
  comparison in `TupleTypeUtil` is strict); a byte array is a ByteString, equal by content (an
  `fdb.Key` too); a Tuple and a List are one List of normalized items; an enum is its number; a
  record version is its versionstamp; a Float is not a Double, and each is equal by `Float.equals`/
  `Double.equals` (every NaN equal, `0.0` not `-0.0`). It is not "pack both and compare the bytes",
  which the reviewers offered as equivalent: packing equates a BigInteger at a Long bound with the
  Long, keeps NaN payloads apart, and panics on a value the encoder has no case for. The index.go
  comment that said "a Long never equals an Integer, and an array equals only itself" is gone, and
  7.6's parenthetical is corrected in place.
- *Root.* `keyExpressionEquals` is Java's `KeyExpression.equals`: an expression equals itself (a
  shared Dimensions root no longer compares unequal), `DimensionsKeyExpression` compares its whole
  key and both sizes (`DimensionsKeyExpression.java:200-213`), and a function expression equals any
  function expression of the same name and arguments, a `CardinalityFunctionKeyExpression`
  included, because `FunctionKeyExpression.equals` tests `instanceof` (`FunctionKeyExpression.java:
  286-300`). Every KeyExpression type in the package has an arm.
- *Predicate (storage 1).* Of Java's `IndexPredicate` classes only `RowNumberWindowPredicate`
  overrides `equals` (`IndexPredicate.java:790-801`); the others are equal only to themselves, and
  `Index(proto)` builds a new one (`:238`). Go's predicate is now equal when both Indexes hold the
  same stored proto (a shallow copy shares it, as Java's copy constructor shares the object), or
  when both are row-number windows (as `IndexPredicate.fromProto` would read them, `:105-121`) with
  equal fields. A Go closure set with `SetPredicate` equals only its own Index: Go cannot compare
  closures, not even for identity. Revision 6's arm comparing closures as values was dead (a func
  is never comparable) and is gone.
- *Tests.* `TestIndexEqualsJavaFieldByField` changes one field at a time (name, type and the alias,
  an equal root and another, an `int` key against the `int64`, another key, both versions, the
  positions changed and absent, options equal in another map, changed and extended, a predicate on
  one side) and the predicate cases (one proto, two equal constant predicates, two equal windows,
  windows of different sizes, a window behind a set constant field, two closures).
  `TestKeyExpressionEqualsArms` covers the new arms, `TestSubspaceKeyIdentity` every
  normalization arm (45 pairs, each in both orders). The OnlineIndexer spec "keeps or refuses a target copy
  as Java's Index.equals does, over normalized keys, every root class and the raw type" pins
  `[own, copy]` for a fresh bytes key, an `int` key against the `int64`, a Dimensions root (all
  built, the copy dropped) and the `min_ever` alias (refused).

**Every subspace-key comparison is Java's.** Go compared keys in four more places, each with a
normalizer that was not Java's (`normalizeSubspaceKey` folded `[]byte` into `string`, kept NaN as a
float map key, and folded only three integer types; `subspaceKeyString` compared `%T:%v`
renderings). All four now use `subspaceKeyIdentity`, and both normalizers are deleted:
- `RecordMetaDataBuilder.Build` checks keys as `MetaDataValidator.validateCurrentAndFormerIndexes`
  does (`MetaDataValidator.java:103-165`): index against index, former index against former index
  (Go had no such check), then index against former index, with Java's messages. It used to REFUSE
  a byte-array key beside a string key of the same content and `0.0` beside `-0.0` (two prefixes in
  each case), and ACCEPT two NaN keys and two former indexes with one key.
- `GetIndexFromSubspaceKey` compares by identity (it still normalizes its argument, which Java's
  does not; stated at the method).
- `MetaDataEvolutionValidator` is Java's `validateCurrentAndFormerIndexes`
  (`MetaDataEvolutionValidator.java:479-555`), with `validateIndex`, `validateFormerIndex` and
  `validateFormerIndexFromIndex` as Java's methods: indexes and former indexes are PAIRED BY
  SUBSPACE KEY across the two meta-data. Go paired indexes by name and looked a former index's
  index up by its former name. So Go ACCEPTED an index that kept its name and moved to another key
  with its versions unchanged, which Java refuses ("index missing in new meta-data"): a store would
  then have served the index, as readable, from a subspace nothing built, and left the old entries
  behind. It refused an index moved to a new key whose old key became a former index, which Java
  accepts; it let `SetAllowMissingFormerIndexNames(true)` admit a renamed former index kept from the
  old meta-data and a replacing former index naming another index, which Java refuses (the flag
  admits only an unnamed replacing former index); and it ran the last-modified checks in the other
  order, with its own messages. Every check now runs in Java's order with Java's message first.
  [Superseded: this held for the index pairing only; 7.8 converted the rest of the validator and
  7.9 the option parsers and the remaining messages.]
- The online indexer's duplicate targets (above).

Measured on both engines: the conformance specs "Subspace-key identity in meta-data validation" (6
shapes through `RecordMetaData.build` and `RecordMetaDataFromProto`) and "Subspace-key pairing in
meta-data evolution" (11 shapes through `MetaDataEvolutionValidator` with its flags; new
conformance steps `buildMetaDataVerdict` and `validateMetaDataEvolutionFlags`) agree on all 17, each
refusal with Java's message as the prefix of Go's (`wsc-v7/jvm-subspace2.log`, "Ran 22 of 1582", 22
passed with the former-index specs below). Run against revision 6's tree (`c7898321`, in a
worktree, with only the spec file and the Java steps added), 16 of the 17 fail
(`wsc-v7/jvm-subspace-base.keep.log`), nine of them on the verdict (four evolution shapes, five build
shapes) and seven on the message only (revision 8 corrects the count; revision 7 said ten).
`TestMetaDataValidatorSubspaceKeys` and `TestEvolutionPairsIndexesBySubspaceKey` pin the same
shapes without the JVM. Tests that pinned Go's own behaviour are corrected to Java's: four evolution
specs (Go's check order, messages and flag reading; among them "refuses a former index name change
even when allowMissingFormerIndexNames is true", which pinned the opposite), the two builder specs
that matched Go's collision messages, `TestBug5_FormerIndexSubspaceKeyTypeChangesOnRoundTrip` (the
stored key is now an int64, as Java's is a Long), the coverage spec of the deleted normalizer, and
the proto-fidelity fixture, whose former index had no subspace key, which Java refuses.

**The subspace key is stored and read as Java's (WS-J v10 Graefe 4, Torvalds 2 and 5, storage 3
and 5, folded here because they are the same comparison's inputs).**
- `Index.SetSubspaceKey` stores `tupleEquivalentValue(key)`, Java's normalization in the forms the
  Go tuple encoder writes, so an `int32`, a narrow unsigned integer, a `[]any` or an enum key packs
  (each used to panic in the encoder), and a nil key is refused as Java's setter refuses it
  (`RecordCoreArgumentError` "Index subspace key cannot be null", kept on the Index and returned by
  the builder's `Build`, the key left as it was). [Superseded: that held for a set made before
  `AddIndex`; 7.8 and 7.9 cover every path, program order, and built meta-data.] `subspaceKeyIdentity(tupleEquivalentValue(k))`
  equals `subspaceKeyIdentity(k)` for every form (pinned), so normalizing on assignment changes no
  comparison.
- A stored FORMER index's key is read as `FormerIndex(proto)` reads it (`FormerIndex.java:51-68`):
  an absent, empty or multi-item key is `RecordCoreError` "subspace key must encode a single item
  tuple", a null item `RecordCoreArgumentError` "FormerIndex initialized with null subspace key". Go
  read all four as a nil key, so it loaded meta-data Java refuses and a store's upgrade then cleared
  the null item's index subspace instead of the dropped index's. It is written as
  `FormerIndex.toProto` writes it: the key always, and no name when there is none (Go wrote an empty
  name, which Java reads as a name). Measured: spec "Former-index subspace keys read as Java reads
  them", 5 shapes, each marshalled and read back through Go's decoder; on revision 6's tree Go loads
  all four malformed ones (`wsc-v7/jvm-former-base.log`, 4 of 5 fail).
- `RecordCoreArgumentError` renders only the fields its site set (`HasSubspaceKey` says a key was
  attached, since the offending key is often nil). Revision 6's error printed `subspace_key=<nil>` for
  the rank scan's error and dropped the other sites' suffixes; `TestRecordCoreArgumentErrorRendersSetFields`
  pins each arm.
- The WS-J spec "refuses each malformed stored subspace key as Java does" now reads Go's side from the
  marshalled bytes and requires the class per shape (`wsc-v7/wsjixk.log`, 1 of 57, passed).

**The merge's lock-out is scoped (Graefe 2, storage 2).** Java's heartbeat check under `allowMutual`
(MUTUAL_BY_RECORDS and SCRUB_REPAIR) only writes its own key (`IndexingHeartbeat.java:88-93`,
`IndexingBase.java:454-457`), and Go's continued mutual session skips every peer (`checkAdmission`).
So the merge's key refuses an exclusive builder (Java's included) and a fresh Go mutual session; a
mutual builder runs beside the merge, and a merge transaction that runs while its heartbeat is
live (an exclusive heartbeat check) fails the MERGE, not the builder, with
`SynchronizedSessionLockedError` (revision 8, MEASURED by the Ginkgo spec "admits a mutual builder
beside the merge, and a merge transaction after it fails, not the builder": the builder is admitted
while the merge's heartbeat is live, the VALUE target's one-transaction merge completes, and the
next merge fails naming the builder, whose heartbeat stays; revision 7 said the merge refuses the
builder); and Java's `getIndexingHeartbeats` lists the
merge's key as a session. DIVERGENCES.md, the CHANGELOG and 7.5/7.6 said a builder "a Java one
included" is refused and that the merge keeps any build from interleaving; each now says which.
DIVERGENCES' closing list says a live MUTUAL peer (Java refuses an exclusive one too) and adds the
WRITE_ONLY ↔ WRITE_ONLY_WITH_QUEUE refusal. "A READABLE target is skipped" read as if the merge did
nothing to a readable index, which is its main use: the merge runs and only the heartbeat step skips
the target; the spec is retitled "merges a READABLE target without a heartbeat", and m59 shows it
can fail.

**Nits.**
- `SetTargetIndexes` copies its list as Java's does (`OnlineIndexer.java:698`); spec "copies the
  list SetTargetIndexes is given, as Java does" (a caller's array with spare capacity, then
  `AddTargetIndex`).
- The nil-target guard sits after the two source-index `ValidationException`s, where Java's
  NullPointerException falls (at the sort or the metadata check, `OnlineIndexer.java:879-880`, since
  Java's `ArrayList` admits the null); its comment said the NPE came from `addTargetIndex`.
- The `IndexBuildOutcomeBuilt` doc covers an attempt that ran the build and indexed nothing (an
  empty store, `online_indexer.go`'s last return).
- The mutation driver greps `[PANICKED!]` and `--- FAIL` too.
- The CHANGELOG's equality list names the positions and says how each field is compared.

**Mutations (revision 7).** Each ran under Bazel with the plain tests of `recordlayer_test` and the
focus "OnlineIndexer|a standalone MergeIndexes session|MetaDataEvolutionValidator|
RecordMetaDataBuilder|subspaceKeyIdentity" (unmutated: "Ran 459 of 3706", 459 passed, no `--- FAIL`,
`wsc-v7/focus1.log`). Every log shows marker count 1, the perl expression and the applied diff, bazel
exit 3, and "restored"; the nine mutated files were compared with their `.good` copies afterwards
(`wsc-v7/mut-*.log`, `run-mutations.sh`, `mutate.sh`, which now greps `--- FAIL` and `[PANICKED!]`):

| mutation | reddened |
|---|---|
| m45 a byte-array key's identity tagged as a string | `TestSubspaceKeyIdentity`, `TestMetaDataValidatorSubspaceKeys` |
| m46 a BigInteger at a Long bound made that Long | `TestSubspaceKeyIdentity` |
| m47 NaN payloads kept apart | `TestSubspaceKeyIdentity`, `TestMetaDataValidatorSubspaceKeys` |
| m48 `equalsJava` over `CanonicalType()` again | `TestIndexEqualsJavaFieldByField`; the target-copy spec |
| m49 no Dimensions arm | `TestKeyExpressionEqualsArms`; the target-copy spec |
| m50 a cardinality expression unequal to a function of its name | `TestKeyExpressionEqualsArms` |
| m51 any proto with a window field read as a window | `TestIndexEqualsJavaFieldByField` |
| m52 the evolution validator's index map keyed by name | `TestEvolutionPairsIndexesBySubspaceKey` |
| m53 a kept former index's name check under `allowMissingFormerIndexNames` again | the corrected evolution spec |
| m54 no last-modified decrease check | the two decrease specs |
| m55 no former-against-former key check | `TestMetaDataValidatorSubspaceKeys` |
| m56 an absent former-index key loaded | `TestFormerIndexProtoAsJava` |
| m57 `SetSubspaceKey` stores the raw key | `TestSetSubspaceKeyNormalizesAsJava` |
| m58 `SetTargetIndexes` shares the caller's array | the list-copy spec |
| m59 the heartbeat step does not skip a READABLE target | the READABLE merge spec, and the two follow-up liveness specs |
| m60 `subspace_key` always rendered | `TestRecordCoreArgumentErrorRendersSetFields` |

**WS-J code folded in, on the same tree.** Two WS-J v10 findings that are code are in this change set
too, so that one evidence run covers it: the loader reads an index's options with its type, before
the root and the key, as `Index(proto)` does (`Index.java:198-221`; a repeated option beside an empty
key is now reported as the repeated option, on both engines, the WSJIXK spec's fourth shape); and
`LegacyUnionTemplateError` marks the (d) error where it is, keeping every wrapper, and only for the SQL
family (`AmbiguousLegacyUnionError.HeldBy`, the union or table holding UnionDescriptor), with no
rename remedy for a template already stored (`Stored`). They are WS-J's to review; ws-j-design.md
v11 records them.

**Evidence (revision 7), combined with WS-J design v11** (`ws-j-oracle/evidence-v11.txt`, logs in
`/var/tmp/fdb-upgrade-recovery/ev11/`). The code was hashed before the runs (every non-document file
of the change set, 1056 files, `ev11/src.sha`, 0 not OK after `just test`), and the final set, with
the documents and the one test added afterwards (`TestReusedIndexMessageKeepsAnEmptySubspaceKey`),
again before the last runs (`ev11/src2.sha`, 1127 files, 0 not OK after them). The revision 7
focus (revision 6's with the FDBMetaDataStore, MetaDataEvolutionValidator and RecordMetaDataBuilder
Describes), "Ran 600 of 3706", 600 passed (`ev11/wsc-focus.log`); the JVM meta-data specs, the 22 of
this revision among them, "Ran 57 of 1582", 57 passed (`ev11/jvm-md.log`); `just test` on tree
`8b5de696df158b5f06d38d543b994cf5f8478398`, "Executed 44 out of 94 tests: 94 tests pass."
(`ev11/justtest.log`); and on the final set, uncached, the whole `recordlayer_test` ("Ran 3705 of
3706", 3705 passed, every plain test passing), `docscheck_test`, `metadata_test` and `catalog_test`,
"Executed 4 out of 4 tests: 4 tests pass." (`ev11/final-targets.log`).

### 7.8 Revision 8: every message of the validators Java's, a nil key refused on every path, and the merge's lock-out measured

Revision 7's gates (`ws-c-addendum-review-v7/`) NAKed on low findings of one class: text that said
Java was matched where it was matched only in part.

**The validators' messages are Java's, and the checks that differed in verdict are Java's.** 7.7 said
"every check now runs in Java's order with Java's message first", which held for the index pairing
and not for `validateIndex`'s later arms. Revision 8 converts the whole evolution validator, and the
sweep found three verdict differences behind the message ones.
- Messages: every `MetaDataEvolutionError` now starts with Java's message and carries Go's detail
  after it in parentheses: `validateIndex` ("index type changed", "new index removes record type",
  "new index adds record type that is not newer than old meta-data", "index key expression changed",
  "index key expression does not match required", "new index changes/drops/adds primary key
  component positions", "field renames result in inconsistent index definition for multi-type
  index"), the record-type checks ("record type removed from meta-data", "record type since version
  changed", "record type primary key changed", "record type primary key does not match required",
  "record type key changed", "record type name changed", "new record type is missing since
  version", "new record type has since version older than old meta-data"), the field and message
  checks ("field removed from message descriptor", "required field added to record type", "field
  renamed", "field is no longer deprecated", "field type changed", "required field is no longer
  required", "repeated field is no longer repeated", "field changed whether default values are
  stored if set explicitly", "enum removes value", "message descriptor proto syntax changed"), and
  the option checks ("index adds uniqueness constraint", "index option changed", "text tokenizer
  changed", "text tokenizer version downgraded", "rank levels changed", "rank hash function
  changed", "rank count duplicate changed", "permuted size changed", "rtree minM changed" for minM
  and, as Java writes it, for maxM, "rtree splitS changed", "rtree storage changed", "rtree store
  Hilbert values changed", "rtree use node slot index changed", "attempted to change immutable
  vector index option"). Three have no Java counterpart and keep Go's text: a union or message
  descriptor present on one side only (Java's descriptors are never null) and the SPFresh option
  check (a Go-only index type). Measured by a census of the validator's `Message:` literals against
  the `MetaDataException` literals of `MetaDataEvolutionValidator.java`, `IndexValidator.java` and
  the index maintainer factories: those three are the only Go messages without a Java prefix.
  `MetaDataValidator`'s former-index version messages are Java's too ("Former index X has added
  version N which is greater than the removed version M", and the two meta-data version messages),
  and so is the index version message ("Index X has added version ...").
- Roots and primary keys are compared by `keyExpressionEquals`, Java's `KeyExpression.equals`, not
  by proto: a field's null interpretation is in the proto and not in `FieldKeyExpression.equals`
  (`FieldKeyExpression.java:406-410`), so a root that changes only that is admitted, as Java admits
  it (MEASURED: the shape "the root changed only in its null interpretation", Java valid, Go valid;
  Go refused it before).
- A field's checks run in Java's order (`MetaDataEvolutionValidator.java:283-328`: name,
  deprecation, type, then the label, then the enum values and the message), and the label is
  checked as Java checks it: a required field that is no longer required, a repeated field that is
  no longer repeated, and a change of presence are refused, and nothing else is. So an optional
  field made required is ADMITTED, as Java admits it (MEASURED, the shape "an optional field made
  required", Java valid), where Go refused every change of cardinality.
- The RANK, R-tree and vector option checks compare EFFECTIVE values, as Java's do
  (`RankIndexMaintainerFactory.java:75-100`, `MultidimensionalIndexMaintainerFactory.java:143-193`,
  `VectorIndexOptionsHelper.java:120-147`): an option set to its default where it was unspecified,
  or the reverse, is no change. Go compared raw strings and refused it. The vector metric is
  compared by the name the parser recognizes, and an unrecognized name is kept as its text, since
  the lenient parser reads it as the default metric without its being that metric.
- Record types and changed options are walked by name (Java walks HashMaps and HashSets), so the
  violation a message names does not depend on Go's map order
  (`TestRecordTypeViolationsNamedInNameOrder`, 32 runs).
Measured on both engines: the new conformance Describe "Field and record-type changes in meta-data
evolution" (9 shapes over the demo records file edited one field at a time: a field renamed, its
type changed, int32 widened to int64, a field removed, a required field added, an optional field
made required, a repeated field made optional, an enum value removed, a primary key changed), and
five more shapes in "Subspace-key pairing in meta-data evolution" (the index type changed, the root
changed, the root changed only in its null interpretation, the index no longer covering a record
type, the index covering a record type that is not newer); every spec now requires Java's WHOLE
message as the prefix of Go's (7.7 asserted a fixed prefix per shape, which stopped before the
index names), so a name order that differs from Java's would fail. `wsc-v8/jvm-subspace2.log`:
"Ran 36 of 1596", 36 passed. Primary key component positions are computed by the builder and not
stored, so no proto shape expresses them; `TestEvolutionComparesRootsAsJavaDoes` pins "positions
dropped" and the existing specs the other two.

**A nil subspace key is refused on every path.** 7.7 said a nil key is "refused as Java's setter
refuses it", which held only for a set made before `AddIndex`. Now: the refusal is kept on the
Index and returned by EVERY `Build` that includes it, a set after `AddIndex` included; it is
STICKY, since Java's throw ends the program's sequence of calls (`SetSubspaceKey(nil).
SetSubspaceKey("k")` is refused, the key unchanged); a typed nil (`*big.Int`, `*FDBRecordVersion`)
is the null it is in Java (`tupleEquivalentValue` maps it to nil; packing a nil `*big.Int` used to
panic); an index built as a struct literal is keyed by its name in `addIndexCommon`, as every Java
constructor keys it (it had a nil key, was maintained under the null item, saved with no key, and
became a former index Go wrote and could not read back); and a former index with a nil key is
refused by `Build` with Java's "FormerIndex initialized with null subspace key" (nothing in Go
builds one, but `FormerIndex.SubspaceKey` is exported). An `fdb.Key` key is copied as a `[]byte`
is. `TestNilSubspaceKeyIsRefusedOnEveryPath` pins each path. An index stored with NO root is
refused as Java's `Index(proto)` refuses it, "Exactly one root must be specified for an index",
before its key is read (a WS-J finding, the same loader; both engines, the WSJIXK spec's fifth
shape), and that is now the text of every malformed key expression's refusal.

**The merge's lock-out, measured.** 7.7 and the CHANGELOG said the merge's next transaction
"refuses" the mutual builder; the merge's check is exclusive and the builder's writes only its own
key, so it is the MERGE that fails. The Ginkgo spec "admits a mutual builder beside the merge, and a
merge transaction after it fails, not the builder" measures it: a mutual builder started after the
merge's first transaction (the merge's heartbeat live) is admitted; a VALUE target's merge is one
transaction, so that merge completes; the next merge fails with `SynchronizedSessionLockedError`
naming the builder, whose heartbeat stays. DIVERGENCES.md, the CHANGELOG and 7.7 now say so.

**Smaller.** The comments that justified comparison-time normalization by "Go stores the key as it
was given" are corrected (`SetSubspaceKey` normalizes; `FormerIndex.SubspaceKey` is exported and may
hold any value, which is why comparisons still normalize). `Build`'s collision messages keep two
parts that are Go's, declared at the code and in the CHANGELOG: the pair is named in name order where
Java uses its HashMap's, and a `[]byte` or list key renders with `%v` (a ByteString's `toString`
includes its identity hash, which nothing can reproduce). `GetIndexFromSubspaceKey` states the one
argument its caller passes that Java would miss: a BY_INDEX stamp naming a source index keyed by a
nested tuple, which Java cannot resume and Go resumes (DIVERGENCES.md, "BY_INDEX resume over a
nested-tuple source key"; `TestSavedSourceIndexResolvesANestedTupleKey`). Revision 7's mutation m56
ran against a `metadata_proto.go` from before the WS-J option reorder; its mutated function,
`formerIndexFromProto`, is byte-identical in that file and in the reviewed tree (19 lines, the same
sha256), so its result stands. Revision 7's `just test` ran while the tree moved from `8b5de696` to
`768e2870`, a move of five documents only (`git diff --stat`), so its result stands for the code.

**WS-J code folded in, on the same tree:** `AmbiguousLegacyUnionError.HeldBy` is found by
reachability from the union (a STRUCT nested in a STRUCT, a new WSJTT shape, and its value asserted
per shape), and the absent-root refusal above. ws-j-design.md v12 records them.


**Mutations (revision 8)**, each applied by `perl` to a copy of the candidate file, its marker counted
in the same run (1 for every one), the focused `recordlayer_test` run under Bazel (every plain test
plus the OnlineIndexer, merge, evolution-validator, builder and identity Describes), the file
restored and compared (`wsc-v8/`: `mutate.sh`, `run-mutations.sh`, `run-mutations.out`, each
`mut-*.log` with its perl, marker count, applied diff and red set, and `*.full.log`). The unmutated
baseline is green: "Ran 462 of 3709", 462 passed, every plain test passing (`wsc-v8/baseline.log`; a
first attempt whose baseline was red, because the absent-root refusal had broken four tests that
built index protos without a root, was cancelled, the tests corrected to give their protos a root as
Java requires, and every mutation re-run). Each mutation reddens:

| mutation | reddens |
|---|---|
| m61 roots compared by proto | `TestEvolutionComparesRootsAsJavaDoes` |
| m62 `Build` skips the indexes' key refusals | `TestNilSubspaceKeyIsRefusedOnEveryPath`, `TestSetSubspaceKeyNormalizesAsJava` |
| m63 the refusal not sticky | `TestNilSubspaceKeyIsRefusedOnEveryPath` |
| m64 no name key for a struct literal | the same |
| m65 a typed nil `*big.Int` passes | the same |
| m66 no refusal of a nil former-index key | the same |
| m67 an absent root loads | `TestStoredIndexSubspaceKeyIsReadAsJavaReadsIt` |
| m68 `HeldBy` looks at direct holders only | `TestStoredMetaDataWithAnAmbiguousLegacyUnionIsRefused` |
| m69 Go's former-index version message | `TestBuildFormerIndexVersionMessagesAreJavas` and the builder spec |
| m70 Go's record-type scope message | the evolution spec "rejects index that drops a record type" |
| m71 to m76 `equalsJava` without its name, root, key, versions, positions or option values | `TestIndexEqualsJavaFieldByField` (each) |
| m77 every cardinality change refused | the optional-to-repeated and label specs |
| m78 RANK options compared as raw strings | "admits an option set to its default where it was unspecified" |
| m79 the vector metric compared through the lenient parser | the metric spec and `TestVectorOptionsComparedByEffectiveValue` |
| m80 record types walked in map order | `TestRecordTypeViolationsNamedInNameOrder` |

The JVM specs of this revision, on the candidate's code: `wsc-v8/jvm-subspace2.log` ("Ran 36 of
1596", 36 passed: the 6 build shapes, 16 pairing shapes and 5 former-index shapes, and the 9 field
shapes), `wsc-v8/jvm-wsjixk.log` (the fifth WSJIXK shape) and `wsc-v8/jvm-wsjtt.log` ("Ran 8 of 59",
the nested STRUCT shape among them). The combined evidence run of this revision and WS-J design v12
is `ws-j-oracle/evidence-v12.txt`.

### 7.9 Revision 9: the option checks parse as Java's maintainers parse, and the maintainers write what those options say

Revision 8's gates (`ws-c-addendum-review-v8/`) NAKed on one medium finding held by all three
lenses: 7.8 sent the RANK and R-tree option checks through Go's config parsers, and two of those
parsers were not Java's, so the "effective value" admitted changes Java refuses and, worse, the
maintainers behind them wrote other bytes than Java for the same options. The low findings were
of 7.8's class: claims wider than the code.

**The parsers are Java's, and so are the bytes.**
- RANK (`parseRankedSetConfig`, `RankedSetIndexHelper.getConfig`): the hash function by EXACT
  name among Java's four, `JDK`, `CRC`, `RANDOM` and `MURMUR3` (Guava's murmur3_32, seed 0,
  `murmur3Hash`), an unknown name refused with Java's `RecordCoreArgumentException` "hash function
  not found: X"; the level count by `Integer.parseInt` (`javaParseInt`, Unicode decimal digits in
  the BMP included) and `setNLevels`' range ("levels must be between 2 and 8",
  `IllegalArgumentException`); count-duplicates by `Boolean.parseBoolean`. Go folded MURMUR3 and
  RANDOM into JDK in the maintainer (a Java MURMUR3 index maintained by Go put scores on other
  levels, and the other engine's deletes then corrupted the counts) and in the option check (so
  unset→MURMUR3 was admitted). The maintainers of RANK and TIME_WINDOW_LEADERBOARD now refuse an
  unparsable configuration when made, as Java's constructors do, and the option check reads both
  configurations first, the old one first, so an option neither parser takes refuses the change
  with the parser's own class, as Java's check does.
- R-tree (`parseRTreeConfig`, `MultiDimensionalIndexHelper.getConfig`): minM, maxM and splitS by
  `Integer.parseInt` with no range (Java's builder takes any int), the storage by
  `RTree.Storage.valueOf` ("No enum constant ..."), then the Hilbert flag READ ONLY WHEN THE
  STORAGE OPTION IS SET, absent then meaning false (`MultiDimensionalIndexHelper.java:62-65`),
  then the node slot index flag, both by `parseBoolean`. So `{rtreeStorage}` stores no Hilbert
  values in leaf slots and `{rtreeStoreHilbertValues: false}` alone stores them, where Go did the
  opposite of both, in the check and in the leaf bytes.
- The R-tree maintainer now HONOURS the storage and node slot index options, which Go ignored
  (`rtree_storage.go`): BY_SLOT, one key-value pair per slot under the node's key, key
  `(nodeId, kind, slot key...)` and value `(slot value...)` as `BySlotStorageAdapter` writes them,
  a leaf's slots sorted by Hilbert value and key on read when the values are not stored; and the
  node slot index, `(level, largest Hilbert value, largest key's items..., childId)` with an
  empty value in the index's secondary subspace under the indicator 0, one entry per child slot of
  every intermediate node, a leaf's level being 0 (`NodeSlotIndexAdapter`,
  `MultidimensionalIndexMaintainer.getNodeSlotIndexSubspace`). Go maintains the index as the
  difference an insert or delete made to the intermediate nodes it touched and finds its leaf from
  the root, where Java's change sets write it slot by slot and look leaves up through it; the stored
  entries are the same (DIVERGENCES.md, "How Go maintains an R-tree's node slot index"). `deleteWhere`
  clears the node slot index under its prefix as Java's does. Go's configuration refusals
  (`ValidateRTreeConfig`) are declared (DIVERGENCES.md, "R-tree configurations Go refuses to
  maintain").
- Booleans everywhere: `Index.GetBooleanOption`, `IsUnique`, `IsClearWhenZero`, the text options
  and RANK's idempotence read "true" in any case, as `Boolean.valueOf`; `unique: "TRUE"` made a
  unique index for Java and not for Go. A text index's tokenizer version is `parseInt`'d when
  present, the empty string included, with Java's MetaDataException, at build and in the check.
  The vector options' parsing (their `vector*` aliases, `vectorEngine`, Java's metric names and
  booleans) is WS-D's typed option catalog (ws-d-design.md, the catalog paragraph); 7.8's vector
  claim holds for the `hnsw*` canonical names only.

**Measured on both engines** (`wsc-v9/conf*.log`): the Describe "Index option changes in meta-data
evolution" (30 option changes through Java's validator and Go's; same class, Java's whole message
the prefix of Go's; among them MURMUR3 and RANDOM refused, a lower-case hash name refused as
`RecordCoreArgumentException`, levels in Arabic-Indic digits admitted and " 6" refused as
`NumberFormatException`, the Hilbert option set beside storage refused and alone admitted, maxM
reported as "rtree minM changed" as Java writes it, `unique: "TRUE"` refused as a new uniqueness
constraint); "Text tokenizer version parsed at build" (4 shapes through both loaders); "RANK ranked
set per hash function" (the same 120 records saved by Java and by Go leave BYTE-IDENTICAL ranked
sets for unset, JDK, CRC and MURMUR3, whose level-1 populations differ, 8, 9 and 7; Java reads the
ranks Go wrote with RANDOM; an unknown name refused by both writers); and "MULTIDIMENSIONAL index
R-tree options" (six option sets, BY_SLOT with and without Hilbert values and with the node slot
index, BY_NODE storage alone, the Hilbert option alone, BY_NODE with the node slot index: Java and
Go write, delete and scan the same records in turn, and after every step both scans equal the live
records and the stored pairs are checked from the raw bytes, the pair kind per node, null or stored
Hilbert values, and one node slot index entry per non-root node naming it; Java's deletes find
their leaves through the entries Go wrote). "Ran 46 of 1642", 46 passed. The Go unit specs "RTree
storage layouts and the node slot index" run every combination of layout, node slot index and
Hilbert storage through 300 inserts and repeated deletes to empty, over nodes of 2 to 4 slots, and
check the whole stored tree after each step against the layout and the index derived by walking it.

**Every message of the validators, and every walk.**
- The field-renaming visitor's refusals are Java's ("field not found in source descriptor", "field
  not found in target descriptor", "parent field is not of message type", and "parent field not
  found", which neither engine can reach), and an expression it cannot rename is Java's
  `RecordCoreArgumentException`, not a meta-data error.
- `Build`'s remaining Go texts are Java's: "No record types defined in meta-data", "Index X has added
  version N which is greater than the last modified version M" (checked as Java checks it, without
  Go's guard that skipped unset versions), and the two replacement messages. `Build`'s index and
  record-type checks, `RecordTypesForIndex`, the text option walk, the rename walk (the old union's
  fields in order, as Java walks them) and the base option walk are in a fixed order, each pinned by
  a test that runs 32 times (`TestTypeRenameNamedInUnionOrder`, `TestChangedIndexOptionsNamedInNameOrder`,
  `TestIndexRecordTypeViolationNamedInNameOrder`, and 7.8's record-type test). [Superseded: three
  record-type loops of `Build` still walked the map with Go's texts, and the key-expression
  validation was Go's text in a `MetaDataError`; 7.10 converts them.]
- The union-less path of the evolution validator is DELETED: every built `RecordMetaData` has a
  union (the builder refuses a records file without one, "Union descriptor is required"), so its
  typed-key rename inference and message walk were reachable only by tests that set the union to
  nil. A `RecordMetaData` never built is refused ("meta-data has no union descriptor"), and 7.8's
  "union descriptor present on one side only" message goes with it; the only evolution message
  without a Java prefix left is the SPFresh option check's. [Superseded: "meta-data has no union
  descriptor" is the other, as 7.10 says.]

**A nil subspace key, in program order and on every path.**
- A refusal is recorded at the set with its place in program order, and `Build` returns the FIRST
  fault in program order among the builder's own faults and the refusals of every index ever
  handed to its `AddIndex`, one later removed or refused as a duplicate included [Superseded: an
  `AddIndex` naming an unknown record type dropped the index, and `RemoveIndex` of an unknown name
  recorded no fault; 7.10 closes both] (7.8 checked the
  builder's faults first and the current indexes after, in name order, and lost a removed index's
  refusal).
- A refused set changes nothing, the explicit mark included (Go marked it before refusing). On an
  index of already-built meta-data it is recorded and reported by `Index.SubspaceKeyError`, with no
  `Build` to return it (DIVERGENCES.md, "A refused SetSubspaceKey on an index of built meta-data").
- A typed nil pointer to a generated enum is Java's null, not a panic.
- The misplaced doc comments are restored to their functions, and the double normalization goes.

**A rootless index is never built or written.** `Build` refuses an index with no root ("Index X has
no root expression"; Go-only as a refusal, DIVERGENCES.md), `indexToProto` refuses to write one, and
the loader's refusal, with a nesting without its parent and a then of fewer than two children, is
Java's `KeyExpression.DeserializationException` (`KeyExpressionDeserializationError`, with Java's
texts), so the specs assert the class (WS-J's WSJIXK spec and `TestKeyExpressionDeserializationErrorsAreJavas`).

**Upgrade note.** A struct-literal index maintained by an earlier Go build keeps its entries under
the null item and reads its name-keyed subspace, empty, after the upgrade; the CHANGELOG says to
rebuild it. So do Go-written RANK indexes with MURMUR3 or RANDOM and multidimensional indexes with
`{rtreeStorage}` or `{rtreeStoreHilbertValues: false}` alone. [Superseded: incomplete; 7.10's list replaces it.]

**BY_INDEX nested-tuple resume** is from Java's source, not measured, as DIVERGENCES.md now says.

**Evidence.** The combined run of this revision, WS-J design v13 and the WS-E v11 oracle is
`ws-j-oracle/evidence-v13.txt` (driver `ev13.sh`, records in `ev13/`), on the frozen candidate
copy, bytes bound before and after (1039 files hashed and verified, the other two of the 1041 the
change set's deletions): the whole `rfc257_oracle_test` twice, 60 of 60 each; the JVM specs of this
revision with the existing RANK, MULTIDIMENSIONAL, TEXT and TIME_WINDOW_LEADERBOARD specs, 185 of
1642, all passed; the whole `recordlayer_test` uncached, "Ran 3715 of 3716", 3715 passed, 0 plain
failures; `just test`, "Executed 94 out of 94 tests: 94 tests pass", nothing cached; and the
metadata, docscheck and catalog targets, 3 of 3.

**Mutations (revision 9)**, on the frozen candidate copy (`fdb-wsc9`, tree
`3b1587fafbd31525e918fbb44e71e394bf97c44c`), each applied by `perl` to a copy of the candidate
file, `gofumpt`ed, its marker counted in the same run (1 for every one), the target run under
Bazel uncached, the file restored and compared (`wsc-v9/final/`: `run.sh`, `mutate.sh`,
`run.out`, each `mut-*.log` with its perl, marker count, applied diff, bazel exit and red set, and
`*.full.log`). Before the first mutation the driver compares every candidate file with its
`good/` copy and stops on a difference. The unmutated baselines are green: the whole
`//pkg/recordlayer:recordlayer_test`, "Ran 3715 of 3716", 3715 passed, every plain test passing
(`baseline.log`; the one skip is the environment-gated million-record spec), and the four JVM
Describes of this revision, "Ran 46 of 1642", 46 passed (`baseline-conf.log`). Every mutation
exits 3 (tests failed, not a build failure) and reddens:

| mutation | reddens |
|---|---|
| m81 MURMUR3 hashed as JDK | `TestRankedSetHashFunctionsAreJavas` |
| m82 an unknown hash name read as JDK | `TestRankedSetConfigParsesAsJavaDoes` |
| m83 the level count unchecked | the same |
| m84 an absent Hilbert flag beside the storage option read as the default (true) | `TestRTreeConfigParsesAsJavaDoes` |
| m85 BY_SLOT written as BY_NODE | the four BY_SLOT combinations of "RTree storage layouts and the node slot index" |
| m86 node slot index levels off by one | the four node-slot-index combinations of the same Describe |
| m87 a promoted leaf root keeps its index entry | the same four |
| m88 a BY_SLOT leaf without Hilbert values read unsorted | the two BY_SLOT combinations without Hilbert values |
| m89 booleans parsed case-sensitively | `TestJavaParseBooleanMatchesBooleanParseBoolean`, `TestRankedSetConfigParsesAsJavaDoes`, `TestRTreeConfigParsesAsJavaDoes` |
| m90 ASCII digits only | `TestJavaParseIntMatchesIntegerParseInt` |
| m91 `Build` returns the LAST fault in program order | `TestNilSubspaceKeyIsRefusedOnEveryPath`, `TestValidateRecordsMatchesJava` |
| m92 an added index's refusal not recorded | `TestNilSubspaceKeyIsRefusedOnEveryPath`, `TestSetSubspaceKeyNormalizesAsJava` |
| m93 type renames walked in map order | `TestTypeRenameNamedInUnionOrder` |
| m94 changed base options walked in map order | `TestChangedIndexOptionsNamedInNameOrder` |
| m95 a typed nil enum pointer not normalized (panics) | `TestNilSubspaceKeyIsRefusedOnEveryPath` |
| m96 a refused set marks the key explicit | the same |
| m97 a rootless index builds | `TestKeyExpressionDeserializationErrorsAreJavas` |
| m98 the root refusal an untyped error | the same, and `TestStoredIndexSubspaceKeyIsReadAsJavaReadsIt` |
| m99 an empty tokenizer version read as absent | `TestTextTokenizerVersionIsParsedAsJavaParsesIt` |
| m100 the rename visitor's Go message | the renameFields spec "errors when the target descriptor lacks the field number" |
| m101 an index's record types walked in map order | `TestIndexRecordTypeViolationNamedInNameOrder` |
| m102 never-built meta-data validated | the union-validation spec "validateUnion refuses meta-data that was never built" (panics) |
| m103 a rootless index serialized | `TestKeyExpressionDeserializationErrorsAreJavas` |
| m104 the RANDOM hash not drawn through the DST seam | `TestRandomRankHashReplaysUnderASeededEnv` |

The JVM side, the same driver after the unit mutations, each mutation run against the four JVM
Describes of this revision (`//conformance:conformance_test`, the same focus as the baseline), each
exiting 3:

| mutation | reddens |
|---|---|
| c1 MURMUR3 hashed as JDK | "RANK ranked set per hash function", hash MURMUR3 |
| c2 an absent Hilbert flag beside the storage option read as the default (true) | the evolution spec "rtree Hilbert set beside storage", and the R-tree option sets "BY_NODE storage alone stores no Hilbert values" and "BY_SLOT without Hilbert values" |
| c3 BY_SLOT written as BY_NODE | the three BY_SLOT option sets |
| c4 node slot index levels off by one | the two option sets with the node slot index |
| c5 the node slot index not maintained | the same two |

c4 was expected to stay green on the JVM (the raw check verifies each entry's child id, not its
level); it reddens at the step "Go deleted the even ids", "node slot index children": Go's delete
clears the entries of a changed node under the level it computes, so with the level off by one the
old entries stay behind and the raw check finds child ids listed more than once. The unit spec
covers the level directly (m86).

A preliminary run of the same list over an earlier copy (`wsc-v9/`, superseded) found three
defects in the instrument or the suite, each fixed before this run: m81 left every test green
(`bazel exit 0`: no unit test reached MURMUR3; `TestRankedSetHashFunctionsAreJavas`, with Guava's
vectors, was added), and m89 and m91 did not compile (`FAILED TO BUILD`, `bazel exit 1`: their
perl produced Go the compiler rejected; both expressions were corrected). That is why the table
above rests on each log's exit code, 3 for every mutation, which a build failure cannot produce.


### 7.10 Revision 10: every builder fault in program order, Build's record-type checks Java's, and the upgrade list complete

Revision 9's three NAKs (`ws-c-addendum-review-v9/`) agree: every revision-8 finding is resolved
and the new parsers and bytes are Java's, and each remaining finding is a claim wider than the code.
Revision 10 makes the code match the claims. Where a claim cannot be made true, it narrows the
claim. The 7.9 paragraphs these change are marked [Superseded].

**Every builder fault in program order.**
- `AddIndex` naming an unknown record type records the index in `addedIndexes` before it records
  "Unknown record type X". A `SetSubspaceKey` refusal made before that call is then the fault
  `Build` returns, as Java threw at the set. A refusal made after it is not.
- `RemoveIndex` of a name no index has is refused with Java's "No index named X defined"
  (`RecordMetaDataBuilder.java:1199-1203`), recorded in program order (Go ignored it). A second
  removal of one index is such a name.
- Pinned by `TestNilSubspaceKeyIsRefusedOnEveryPath`, "a refused index handed to AddIndex with an
  unknown record type" and "removing a name no index has", both orders each.

**`Build`'s record-type checks are Java's, in Java's order, over the record types in name order.**
The walk goes over the record types by name; Java walks a HashMap, which DIVERGENCES.md declares.
The checks run in this order:
- A record type without a primary key: "Record type X must have a primary key". Java's `build`
  refuses it before it validates anything (`RecordMetaDataBuilder.java:1480-1491`). Go said
  "record type "X" has no primary key set".
- The union's oneof: "Union descriptor has more than one oneof" and "Union descriptor oneof must
  contain every field" (`MetaDataValidator.java:68-78`). Go's texts began in lower case.
- "No record types defined in meta-data".
- Then, per record type, `validateRecordType`'s checks in its order (`:80-101`), and all of them
  before any index is validated:
  - the primary key validated against the descriptor;
  - "Primary key for X can generate more than one entry";
  - "Same record type key K used by both X and Y", the later type first, as Java's `put` returns
    the earlier one;
  - "Record type X has since version of N which is greater than the meta-data version M".

  Three of these were separate loops over the map, with Go's texts.
- Key-expression validation throws Java's `KeyExpression.InvalidExpressionException`
  (`KeyExpressionError`), unwrapped, with Java's texts (`FieldKeyExpression.java:145-172`,
  `KeyWithValueExpression.validate`, `SplitKeyExpression.validate`):
  - "Descriptor X does not have field: f";
  - "f is not repeated with FanType.FanOut" (or Concatenate);
  - "f is repeated with FanType.None";
  - "Child expression of covering expression returns too few columns" (Java's `getMessage`; its
    counts are log info);
  - "Must have a single key before splitting";
  - "Must produce multiple values for splitting".

  A message field read as a scalar is Java's `Query.InvalidExpressionException`, a new
  `QueryInvalidExpressionError`: "f is a nested message, but accessed as a scalar". Go wrapped Go
  texts in a `MetaDataError` naming the record type and index.
- [Superseded → 7.11: one Go-only refusal remains; the nesting into a scalar is Java's class and text.] Two refusals stay Go-only, both declared in DIVERGENCES.md, "Build's record-type checks: the
  order, and two Go-only refusals":
  - an empty primary key, whose split clear range would be the whole records subspace;
  - a nesting into a scalar field. Its text is protobuf's `getMessageType` refusal, and Java's class
    for it, `UnsupportedOperationException`, has no Go type.

  [Superseded → 7.11: three messages, "message descriptor presence changed" the third.] "meta-data has no union descriptor" is, beside the SPFresh option check's, the evolution
  message without a Java prefix.
- Pinned by `TestBuildRecordTypeChecksAreJavasInNameOrder`. It runs nine shapes 32 times each, and
  each asserts the whole text and exactly one of Java's two classes [Superseded → 7.11: each shape names its class]. Its shapes are:
  - two types without keys;
  - a missing key beside a since version;
  - a fan-out key;
  - a key validated before its fan-out check;
  - one key on three types;
  - two since versions;
  - a type's key collision before its since version;
  - an earlier type's last check before a later type's first;
  - record types before indexes.

  `TestKeyValidationTextsAreJavas` pins every validation text and class.

**PERMUTED_MIN/MAX's `permutedSize` is read as Java reads it, and `Build` runs Java's validator.**
- `PermutedSizeOption` is `getPermutedSize` (`PermutedMinMaxIndexMaintainer.java:107-113`): absent is
  refused ("permuted size not specified"), and present is `Integer.parseInt`.
- The maintainer is made through it and fails as Java's constructor does.
- `Build` runs `PermutedMinMaxIndexMaintainerFactory`'s validator in its order (`:66-80`):
  - "index type requires grouping", and "... at least 1 fields";
  - "version key not possible in index type";
  - the size read, then "permuted size cannot be negative" and "permuted size cannot be larger
    than grouping size".
- The query side reads as Java's `AggregateIndexMatchCandidate.getPermutedCount` and
  `AggregateIndexExpansionVisitor` do: absent is 0, present is `Integer.parseInt`. That covers the
  executor's `permutedAggregateGroupingLayout` and the relational candidate builder.
- The chaos model reads as the maintainer does.
- An earlier Go used `strconv.Atoi` in all four places. It maintained an absent or unparsable size
  at 0 and built every index Java's validator refuses.
- Pinned by `TestPermutedSizeIsReadAsJavaReadsIt`: both types; the validator's refusals with class
  and text; an Arabic-Indic digit maintained at 2; the maintainer's own refusal. The executor
  layout test gains the digit case.

**A Then is flat, as Java's is.** [Superseded in part → 7.11: `RecordTypeKey().Nest` and a Then of fewer than two children.]
- `Concat` is Java's `ThenKeyExpression` constructor: a composite child contributes its children
  (`ThenKeyExpression.add`, `:264-271`).
- `thenFromProto` decodes and flattens the children before it refuses fewer than two
  (`ThenKeyExpression(Then)`, `:77-86`), so `Then[Then(a, b)]` loads as `(a, b)`. Go refused it.
- The three Go-side flatteners are gone: `concatFlattening`, the relational generator's
  `concatFlat`, and the rename visitor's struct literal. They were workarounds for `Concat` not
  flattening. `GroupBy` keeps its loop, which it needs to tell one child from several.
- Pinned in `TestKeyExpressionDeserializationErrorsAreJavas`: two nested stored shapes load flat,
  are written back flat, and `Concat` flattens.

**RANDOM ranked sets.**
- A failed read of the randomness source fails the insert. The hash type returns an error, where
  a zero hash put the key on every level. Pinned by
  `TestRandomRankHashFailsTheWriteOnAFailedRead`.
- The comments claiming a delete "re-tosses" levels are corrected: neither engine calls the hash
  on remove (`RankedSet.java:382` is the only `getKeyHash` call, and see its comment at
  `:487-491`).

**R-tree.**
- Root split and root promotion change the fetched root's struct in place. They no longer build a
  new struct and copy its fetch state by hand, which was the hazard.
- Leaves are deleted through `deleteLeafNode`. `deleteNode`, which does not maintain the node slot
  index, is called only inside the storage layer.
- The layout oracles no longer share the production encoder:
  - The unit spec builds each expected node slot key itself.
  - The JVM spec "MULTIDIMENSIONAL index R-tree options" decodes every intermediate node's child
    slots from the raw bytes (both layouts), whichever engine wrote them. It computes each child's
    level from the root [Superseded → 7.11: upward from the leaves, and the tree's shape checked], builds `(level, largestHV, largestKey..., childId)` without Go, and
    requires the stored index to equal that set, whole keys compared. It also requires exactly one
    child slot per node but the root.
- The BY_SLOT whole-node rewrite is declared in DIVERGENCES.md.

**Upgrade list.** [Superseded → 7.11: withdrawn under the owner's ruling on pre-release data.] The CHANGELOG's "UPGRADE: indexes to rebuild" names every index kind whose
entries an earlier Go wrote under another meaning of its options:
- multidimensional indexes with `rtreeStorage`, with `rtreeStoreHilbertValues` without storage, or
  with a true `rtreeUseNodeSlotIndex`, whichever engine created them;
- RANK and TIME_WINDOW_LEADERBOARD with MURMUR3 or RANDOM;
- `rankNLevels` outside [2, 8], which now refuses every write until corrected;
- `unique`, `rankCountDuplicates`, `clearWhenZero` and the text options spelled other than
  "true";
- a `permutedSize` that is not a plain decimal;
- struct-literal indexes.

The CHANGELOG's option heading is scoped to leave out the vector index's `hnsw*` booleans (WS-D),
and its vector-check sentence to the `hnsw*` names.

**Nits.** `validateVectorIndexOptions`'s doc comment is back on its function. The m84 and c2 rows of
7.9 are relabelled to what their diff does: with `rtreeStorage` set, an absent Hilbert flag is read
as the default. This revision adds the mutation for the other half: the flag read when
`rtreeStorage` is absent. 7.9's stray "(`bazel exit 0`)" is placed.

**Evidence.** Two runs of the same driver (`wsc-v10/evidence.sh`, and `evidence2.sh`, which only
names its record directory), each on the frozen candidate copy `fdb-wsc10`. Each records the base
HEAD (`71ccd8cf8`), the status, the changed files, and the sha256 of every changed file before its
first step, and it verifies them after its last step.

- **`ev/`, on the mutation tree `b3b0581c`.** 1817 of 1819 files hashed (the other two are
  deletions), and 1817 verified. The whole `conformance_test` ran uncached: "Ran 1523 of 1642",
  1523 passed (the 119 skipped are the suite's own filters). The whole `rfc257_oracle_test`, 61 of
  61. Then `just test`: "Executed 94 out of 94 tests", 93 passed. The one failure was
  `GroupAliasIdentityJavaProbe` in `conformance_test`, which had passed in the uncached run
  minutes earlier:
  - The target's pinned answer to `SELECT id AS "x", v AS "X" FROM sort_t UNION ALL ... ORDER BY
    "x"` came back with its legs interleaved.
  - The target's plan, measured by the probe, is an unordered union with no ordering operator:
    `SCAN([IS SORT_T]) | MAP (...) ⊎ SCAN([IS SORT_T]) | MAP (...)`. Its trailing ORDER BY
    attaches to the right leg, whose primary scan already delivers it.
  - Its `UnorderedUnionCursor` returns rows as its legs deliver them, and its class comment says
    two runs may order them differently. So the pinned order was a timing fact, not an answer.
  - The probe now pins the target's plan exactly and its rows as a multiset, and Go's answer
    exactly. Focused and uncached, it passed 10 of 10 (`--runs_per_test=10`).
  - [Superseded → 7.11: the sweep was over SQL text; 7.11 sweeps the plan operator.] No other UNION probe in `conformance/` pins an order the target does not define. Seven
    files carry a UNION ALL probe. Besides this one:
    - `yamsql_cross_engine_conformance_test.go` compares its unions unordered;
    - `union_trailing_orderby_java_probe_test.go` asserts subsequences;
    - `run_sql_conformance_test.go` classes the target's order as intermittent;
    - `sum_overflow_join_leg_java_probe_test.go` and `ws_e_probe_conformance_test.go` pin at
      most one distinct row;
    - `dotted_and_recursive_seed_java_probe_test.go` pins a recursive union, whose cursor is
      sequential by construction (`RecursiveUnionCursor`, class comment).
- **`ev2/`, on the candidate tree `512ae248`.** This is `b3b0581c` plus that probe fix and the
  WS-F v4 oracle (conformance and plandiff test files only, `git diff --name-only`; no file the
  mutations above touch). 1833 of 1835 files hashed, and 1833 verified.
  - The whole `conformance_test`, uncached: "Ran 1523 of 1642", 1523 passed.
  - The whole `rfc257_oracle_test`, uncached: 61 of 61.
  - `just test`: "Executed 7 out of 94 tests: 94 tests pass". [Superseded → 7.11: this list was read from test logs that later runs overwrote, and was not retained; 7.11's evidence writes its list to a file.] The seven executed are the targets
    the change reaches (`conformance_test`, `rfc257_oracle_test`, `rabitq_architecture_test`,
    `docscheck_test`, `plandiff_test`, `yamsql_test` and `factory-run_test`, read from the
    test logs written during the run). The other 87 were served from Bazel's cache of `ev/`'s
    `just test` over identical inputs, where each of them had executed and passed.
  - Load, before and after each step, is in `ev2/*.uptime` (2.65 to 10.07).

**Mutations (revision 10)**, on the frozen candidate copy (`fdb-wsc10`, tree
`b3b0581c271c87204b52a32b651b20e31341e6f1`). The instrument is 7.9's (`wsc-v10/`: `run.sh`,
`mutate.sh`, `run.out`, each `mut-*.log` with its perl, marker count, applied diff, bazel exit and
red set, and `*.full.log`):
- each mutation applied by `perl` to a copy of the candidate file and `gofumpt`ed;
- its marker counted in the same run, 1 for every one;
- the target run under Bazel uncached;
- the file restored and compared.

Before the first mutation, the driver compares every candidate file with its `good/` copy and
stops on a difference; after the last, it compares them again ("ALL-DONE", no difference). The
unmutated baselines are green:
- the whole `//pkg/recordlayer:recordlayer_test`, "Ran 3715 of 3716", 3715 passed, no plain test
  failing (`baseline-unit.log`);
- `//pkg/recordlayer/query/executor:executor_test` and `//pkg/relational/core/embedded:embedded_test`,
  "Executed 2 out of 2 tests" (`baseline-plain.log`);
- the four JVM Describes of 7.9's run, "Ran 46 of 1642", 46 passed (`baseline-conf.log`).

Every mutation exits 3 (tests failed, not a build failure) and reddens:

| mutation | reddens |
|---|---|
| n1 `AddIndex` with an unknown type drops the index | `TestNilSubspaceKeyIsRefusedOnEveryPath`, "a refused index handed to AddIndex with an unknown record type" |
| n2 `RemoveIndex` of an unknown name silent | the builder spec "removing a non-existent index is refused, as Java refuses it", and `TestNilSubspaceKeyIsRefusedOnEveryPath`, "removing a name no index has" |
| n3 the record types walked in map order | four shapes of `TestBuildRecordTypeChecksAreJavasInNameOrder`, and the validation spec "Build rejects duplicate record type keys" |
| n4 the since-version check first | `TestBuildRecordTypeChecksAreJavasInNameOrder`, "a type's checks in validateRecordType's order" |
| n5 the key collision's names swapped | that shape and "one record type key on three types", and "Build rejects duplicate record type keys" |
| n6 the primary key's validation error wrapped | `TestBuild_PrimaryKeyReferencingNonExistentField` |
| n7 a missing field in Go's text | `TestAmbiguousLegacyUnionRefusalComesAfterTheTargetsFaults`, three `TestBuild_` tests, the TEXT spec "validates every covered descriptor: missing", and "Build fails when index references non-existent field" |
| n8 a message read as a scalar with the key-expression class | `TestKeyValidationTextsAreJavas`, `TestValidateField_MessageTypeFieldWithoutNest`, `TestBuild_IndexOnMessageFieldWithoutNest` |
| n9 an absent `permutedSize` read as 0 | `TestPermutedSizeIsReadAsJavaReadsIt` |
| n10 a size above the grouping accepted | the same |
| n11 ASCII digits only in the maintainer | the same |
| n12 the permuted validator not run at `Build` | the same |
| n13 ASCII digits only in the executor | `TestPermutedAggregateGroupingLayout`, "an Arabic-Indic digit" |
| n14 ASCII digits only in the relational candidate | `TestAggregateIndexCandidate_DeclinesNonzeroPermutedSize` |
| n15 `Concat` keeps a composite child | `TestKeyExpressionDeserializationErrorsAreJavas` |
| n16 a stored Then counted before it is flattened | the same |
| n17 a failed RANDOM read hashed as 0 | `TestRandomRankHashFailsTheWriteOnAFailedRead` |
| n18 root split into a new struct | the four node-slot-index combinations of "RTree storage layouts and the node slot index" |
| n19 root promotion into a new struct | the same four |
| n20 the node slot key's level and Hilbert value swapped | the same four |
| n21 the Hilbert flag read without `rtreeStorage` | `TestRTreeConfigParsesAsJavaDoes` |

The JVM side, the same driver after the unit mutations, each run against the baseline's focus
(`//conformance:conformance_test`), each exiting 3:

| mutation | reddens |
|---|---|
| c6 the node slot key's level and Hilbert value swapped (n20) | "Java and Go share the tree", BY_NODE and BY_SLOT with the node slot index |
| c7 the Hilbert flag read without `rtreeStorage` (n21) | the evolution spec "rtree Hilbert false without storage", and "Java and Go share the tree: Hilbert false without storage keeps them" |
| c8 the node slot key's largest key dropped | the two node-slot-index specs of c6 |

c6 and c8 are the cases the independent decode exists for: with the production encoder in the
oracle, both sides of the comparison would have moved together. Each reddens at the comparison of
the stored node slot index with the set decoded from the raw nodes
(`multidimensional_options_conformance_test.go:264`, "node slot index entries"), at the step "Go
deleted the even ids", the first step at which Go writes.

### 7.11 Revision 11: Java's index validators index by index, the Then's arity, map fields, and pre-release data

Revision 10's three NAKs (`ws-c-addendum-review-v10/`) find every revision-9 finding resolved and
raise Lows of one kind, a claim wider than the code, plus an index-validation order that is not
Java's. Revision 11 changes the code where a claim can be made true, and takes the owner's ruling
of 2026-09-24 on data only pre-release Go builds wrote (umbrella RFC, "Verification and review
gates" item 9). The 7.10 sentences it changes are marked [Superseded → 7.11].

**Pre-release data (owner ruling).** The CHANGELOG's "UPGRADE: indexes to rebuild" list and its
scattered rebuild instructions are withdrawn. The Compatibility section says instead that data only
a pre-release Go build wrote is unsupported: this build reads everything as Java 4.14.2.0 reads it,
and nothing migrates, detects or repairs such data. That answers both lists' findings (a
PERMUTED_MIN/MAX index or a `rankNLevels` that no longer loads [Superseded → 7.12: such meta-data
loads; its writes are refused] cannot be corrected in place, since
both engines' evolution checks refuse the option change): such a store is recreated, as any
pre-release store is.

**`RecordTypeKey().Nest(x)` is `Concat(RecordTypeKey(), x)`.** Java's `RecordTypeKeyExpression` has
no children; Go's carried an optional nested expression, wrote it as a Then only when serialized,
compared every record type key equal (`keyExpressionEquals`), and did not flatten a nested Then.
`Nest` now builds the Then and leaves its receiver unchanged. The `nested` field and every branch
on it are gone (the evaluator's four paths, `ToKeyExpression`, `countVersionColumns`,
`createsDuplicatesRec`, the primary-key translation and the rename visitor), and so is the exported
`GetNestedExpression`. A per-type record count read (`GetSnapshotRecordCountForRecordType`) now
refuses a nested count key, as Java's does, where it read the count at the type key alone.
[Superseded → 7.12: Java's never reads the count key; the port reads a COUNT index.] Pinned
by `TestBug6_RecordTypeKeyNestIsTheThenJavaWrites`: the three shapes of the NAK, each the flat Then
Java writes, byte for byte, read back equal, and `Nest(a)` unequal to `Nest(b)`.

**A Then of fewer than two children is Java's refusal, in program order.** Java's list constructor
throws `RecordCoreException` "Then must have at least 2 children" where the Then is built
(`ThenKeyExpression.java:63-65`). `Concat` has no error channel, so it records the place of that
throw in program order (`arityFaultSeq`, the counter builder faults use); a Then built from a
refused one keeps the earlier place, and `GroupBy` carries it through its own flattening.
`firstFault` walks every key expression handed to the builder (each index added, each primary key,
the record count key) [Superseded → 7.12: it walked the current keys only] and returns the earliest as `RecordCoreError`, beside the other builder
faults. Go had stored a one-child Then that neither engine's loader reads back. The loader's two
Java texts lose Go's suffixes: "Then must have at least 2 children" and "Exactly one root must be
specified for an index" (Go appended "(got N)" and "(found N)"), and their test compares the whole
text. Pinned by `TestThenOfFewerThanTwoChildrenIsRefusedWhereJavaThrows`: nine shapes (a one-child
and an empty primary key, an index root, inside a grouping, grouped by, inside a nesting,
flattened into a Then of two, a universal index, the count key), a Then of two that builds, and
program order both ways.

**Key validation is Java's for map fields and for a nesting into a scalar.**
- A map field is repeated, as protobuf-java's `isRepeated()` says; Go asked `IsList()`. So
  `Field("m")` over a map is Java's "m is repeated with FanType.None" (Go said "m is a nested
  message, but accessed as a scalar", another class).
- Nesting into a field that is not a message is protobuf-java's `UnsupportedOperationException`,
  whose text, measured on the conformance JVM, names the field: "This field is not of message type.
  (com.apple.foundationdb.record.Order.price)". It is Go's `UnsupportedOperationError`. 7.10 said
  Go had no type for the class; it had one (`errors.go`). The divergence entry, the CHANGELOG
  sentence and 7.10's are corrected.
- [Superseded → 7.12: the refusal is gone and the entries are fanned out; 7.13 corrects the order
  they are visited in.] One key Java builds is refused, declared in DIVERGENCES.md ("A proto map
  field is not fanned out in a key expression"): a nesting that fans out a map's entries, after
  every check Java makes, because Go's evaluator reads a field as repeated only when it is a list
  and would read the map as one message (and panic).
- MEASURED [Superseded → 7.13: the spec holds 8 shapes at revision 13]: the conformance spec "Key
  validation at build, as Java builds" gives six shapes to both
  loaders (`buildMetaDataAnyVerdict`, a new JVM step reporting any exception with its full class
  name, since the two `InvalidExpressionException`s share a simple name) and requires the same
  class and the same whole text, or Java valid and Go's declared refusal.

**`Build` validates indexes as Java's `MetaDataValidator` does, index by index.** 7.10 left the
permuted validator after the subspace-key, former-index and version checks, and the atomic and TEXT
validators with it. `validateCurrentAndFormerIndexes` (`index_validator.go`) is Java's
(`MetaDataValidator.java:103-165`): for each index in name order (Java's HashMap order is declared),
its validator — the key validated against each record type it covers with the validator's check of
the fields, then the added version against the last modified, then the type's own checks — then its
subspace key against the indexes before it, its versions against the meta-data version, and its
replacements; then each former index; then a key an index and a former index share. A windowed
VECTOR index's decorator checks come first and then the VECTOR validator's steps, as Java's
decorator ends by running it.

The type validators are Java's, with Java's texts and classes (`KeyExpressionError` for
`KeyExpression.InvalidExpressionException`, `MetaDataError` for `MetaDataException`), one Go function
per Java check (`validateGrouping`, `validateNotGrouping`, `validateNotVersion`,
`validateVersionKey`, `validateVersionInGroupedKeys`, `validateStoresRecordVersions`,
`validateNotUnique`, `validateNoValue`):
- VALUE: no grouping, no version. Go had no VALUE validator.
- The atomic types, over the mutation the type and `clearWhenZero` choose: the grouping the
  mutation needs, "index type does not support non-group fields; use COUNT_NOT_NULL", "index type
  only supports single field", a version only in MAX_EVER_VERSION's grouped key, "index type does
  not support clearWhenZero", and per record type an integer last field for SUM and the
  `_EVER_LONG` types ("index type only supports integer field", the fixed kinds excluded as Java
  excludes them). Go checked only that the root was a grouping, with Go's text. COUNT_NOT_NULL with
  `clearWhenZero` is among Java's mutations without values, and that is ported as Java has it.
- RANK and TIME_WINDOW_LEADERBOARD: a grouping with a grouped column, no version. Go had none.
- PERMUTED_MIN/MAX: 7.9's validator, now in this place.
- BITMAP_VALUE: "index type needs grouped position" and, per record type, an integer position
  ("index type only supports integer position key", the fixed kinds admitted).
- TEXT: no version, not unique, no value, the tokenizer (an empty name is "unrecognized text
  tokenizer", as Java's `getOption` returns "" and not null) and its version, and per record type
  the text field. Go wrapped each text as "text index "X": ...".
- VERSION: no grouping, record versions, one version column, not unique.
- MULTIDIMENSIONAL: no grouping, no version, a dimensions key covering exactly the key
  (`validateStructure`). Go had none.
- A MAX_EVER_VERSION index no longer requires record versions: that check was Go's alone, and its
  comment named Java's validator for it.
- Two declared, in DIVERGENCES.md: Go does not refuse an index type it does not maintain ("Build
  does not refuse an index type Go does not maintain"), since a Java store may hold a module's index
  (Lucene); and the VECTOR validator's structural half is not ported [Superseded → 7.12's
  documentation paragraph: a plain VECTOR index runs neither half at `Build`; the option half runs
  only for a windowed one]. The owner's ruling removes
  the reason that entry was open, so it is WS-D's, beside the rest of the vector index's alignment,
  and the entry says so.

MEASURED: the conformance spec "Index validation at build, as Java builds" gives 30 shapes to both
loaders and requires the same class and whole text, or both valid: a shape per check above
[Superseded → 7.12: not for every check; 7.12 adds 17], the
per-index order (a validator before a shared subspace key, an index's own checks before a later
index's key, the added version before the type's checks, the type's checks before the meta-data
version, an index before a former index, former indexes before an index sharing a key with one),
and the bare roots Java's `Index(proto)` wraps. Two measured facts corrected Go's tests: both
loaders read a RANK root that is not a grouping as one grouped column, so it builds; and a bare
COUNT, which Java's loader wraps with every column grouped, is then refused by its 4.14.2.0
validator, where a Go test said "metadata Java opens" (now
`TestBareCountIsWrappedThenRefusedAsJavaRefusesIt`).

Go's own tests built shapes Java refuses, and are rewritten to Java's shapes: 38
TIME_WINDOW_LEADERBOARD roots and the chaos leaderboard's become `Ungrouped(...)`, as the
conformance leaderboard's already was; 8 RANK roots in tests and 2 in the simfdb hunt profiles
become `Ungrouped(Field("price"))`; COUNT over a grouped field and COUNT_NOT_NULL over `GroupAll`
become `GroupAll` and `Ungrouped`; the R-tree evolution tests get a dimensions key; the store's
delete-scoping test uses a SUM, which has the grouped column that test needs. The chaos model now
applies COUNT_NOT_NULL's null check to the grouped part (it counted every record, which only the
`GroupAll` shape it used could hide).

**The builder's two remaining program-order gaps.** `GetRecordType` of an unknown name records
Java's "Unknown record type X" in program order and returns a builder over a record type the
meta-data does not hold; it panicked with Go's text. `AddMultiTypeIndex` with several names
resolves them all before it adds the index, as a Java program resolves each `getRecordType` before
`addMultiTypeIndex`, so an unknown type is the fault and the index is not added (pinned by
`TestAddMultiTypeIndexResolvesItsRecordTypesFirst`).

**`bitmapValueEntrySize` is read as Java reads it.** `BitmapValueEntrySizeOption` is Java's
constructor: `Integer.parseInt`, 10000 when absent, `RecordCoreArgumentError` "entry size option is
too large" above 250000. Go used `strconv` and fell back to 10000 on every refusal, so "٢" was
maintained at 10000 where Java maintains it at 2. A size of zero or below is refused where the
index is used (Java fails at its first write), declared. The chaos model reads it the same way.
Pinned by `TestBitmapValueEntrySizeIsReadAsJavaReadsIt`.

**Classes asserted by value.** `TestBuildRecordTypeChecksAreJavasInNameOrder`, the shape (d) test's
`want` and `TestPermutedSizeIsReadAsJavaReadsIt` name each case's class (key expression, number
format or meta-data) and assert it; 7.10's "exactly one of two" let n6 pass.
`TestKeyValidationTextsAreJavas` names each case's class among three.

**Texts.** `QueryInvalidExpressionError`'s comment says Java's class is an
`IllegalStateException` (`Query.java:273`). `Build`'s stale "validate primary key and index
expressions" comment is gone with the code it described. 7.10's count of evolution messages
without a Java prefix is three: "meta-data has no union descriptor", SPFresh's option check, and
"message descriptor presence changed".

**The ordered-union sweep, by plan operator.** 7.10 swept SQL "UNION ALL" text; the defect class is
a target plan containing `⊎` whose rows are pinned in order. `git grep -c '⊎'` over
`conformance/*_test.go` finds three files (positive control: the same command finds 23 lines with
"UNION" in the first): `fromless_select_java_probe_test.go` (1, the GroupAlias probe 7.10 fixed),
`ws_e_probe_conformance_test.go` (1, which pins at most one row) and
`ws_f_probe_conformance_test.go` (4, WS-F's oracle, whose ordered comparisons WS-F's design
revises under its own gate). The `union` probe of the fromless file (`SELECT 1 AS n UNION ALL
SELECT 2 AS n`, rows `[[1],[2]]`) pins an order, and holds it deterministically: both legs are
value cursors, always ready, and `UnorderedUnionCursor` takes the first ready child in list order
(`UnorderedUnionCursor.java:76-87`).

**The R-tree JVM oracle's wording and shape check.** Its levels are counted up from the leaves,
through each node's first child (7.10 said "from the root"). Its structure check was the total
count of child slots plus set equality with the index; it now also requires that every stored node
but one is named as a child exactly once, that every named child is stored, and that a node's
children are all of one level.

**Corrections to 7.10's record.**
- n7's red set also holds `TestKeyValidationTextsAreJavas`, which its log shows red.
- n17's red was not a failed assertion: the mutated `Add` went on past the failed draw to its nil
  transaction and panicked (`java_parse_test.go:339`), which ended the test binary. The test now
  recovers that panic and fails with it, so the mutation reddens this test alone.
- n3 mutated only `validateRecordType`'s loop; revision 11 adds r20 for the missing-primary-key
  loop.
- The 10-of-10 rerun of the GroupAlias probe had no retained log, and ev2's list of seven executed
  targets was read from test logs later runs overwrote. Revision 11's evidence reruns the probe ten
  times with its log kept (`ev/groupalias10.log`) and writes `just test`'s executed targets to a
  file (`ev/justtest-executed.txt`).

**Evidence.** On the frozen copy `fdb-wsc10` at tree `d3615120a8b695ecc976f3f01590fed29f1b75df`
(`wsc-v11/evidence.sh`, record `wsc-v11/ev/`): the head, status, the 1870 changed files and the
sha256 of the 1868 that exist (two are deletions) before the first step, all 1868 verified after
the last, the index tree unchanged. Bazel's test cache was on (the owner's direction); the change
reaches `pkg/recordlayer`, so every target it reaches executed.
- The whole `conformance_test`: "Ran 1559 of 1678", 1559 passed (the 119 skipped are the suite's
  own filters).
- The whole `rfc257_oracle_test`: 63 of 64. The failure is a WS-J spec, "WS-J stored index protos
  read as Java reads them", "a COUNT root that is not a grouping": it compared Java's
  single-index reader (`Index(proto)`, no validation) with Go's whole load, and the bare COUNT
  root that both readers wrap with every column grouped is now refused by both validators (the
  JVM spec "Index validation at build, as Java builds", "a count over a bare field"). The spec
  now checks Java's read (a grouping, one column grouped) and Go's refusal with the same class and
  text.
- The GroupAlias probe, ten runs (`--runs_per_test=10`, `ev/groupalias10.log`): all passed.
- `just test`: "Executed 45 out of 94 tests: 93 tests pass and 1 fails locally", the one being
  `rfc257_oracle_test` above; the 45 executed are listed in `ev/justtest-executed.txt`.
- Load over the steps (`ev/*.uptime`): 2.25 to 16.77.

The rerun (`ev2/`) is on tree `98dd04f59a2f1a6ef22833a0a22c5057fb3553b6`, which is `d3615120` plus
the two test files changed after it (`java_parse_test.go`'s recover, 7.11's corrections; the
oracle spec's COUNT case), `git diff --stat d3615120 98dd04f5`: `rfc257_oracle_test` and
`recordlayer_test`, "Executed 2 out of 2 tests: 2 tests pass", the tree unchanged. The gate tree
differs from `98dd04f5` by this document only [Superseded → 7.12: and the 13 revision-10 review
files].

**Mutations (revision 11)**, on the same frozen copy at `d3615120`, with 7.10's instrument
(`wsc-v11/run.sh`, `mutate.sh`, `run.out`, `good/`, each `mut-r*.log` with its perl, marker
count, applied diff, bazel exit and red set, and `*.full.log`). The good copies matched before the
first mutation and after the last ("tree unchanged", "ALL-DONE"). Baselines: the whole
`recordlayer_test`, "Ran 3712 of 3713", 3712 passed; the two JVM Describes, "Ran 36 of 1678", 36
passed. Every mutation exits 3 and reddens:

| mutation | reddens |
|---|---|
| r1 `Nest` keeps a child | six `RecordTypeKey` specs (column size, evaluation, field names, fan-out, version count), both fast-path tests, `TestTranslatePrimaryKeyToValues`, `TestBug6_RecordTypeKeyNestIsTheThenJavaWrites`, `TestRecordTypeKeyExpressionRoundtrip/with_nested` |
| r2 `Concat` records no arity fault | BUG5b, `TestThenOfFewerThanTwoChildrenIsRefusedWhereJavaThrows` |
| r3 `firstFault` ignores the arity fault | the same two |
| r4 `GroupBy` drops a refused Then's fault | `TestThenOfFewerThanTwoChildrenIsRefusedWhereJavaThrows` ("grouped by a one-child Then") |
| r5 a map field not repeated | all five map shapes of "Key validation at build, as Java builds" |
| r6 a map fan-out admitted | "a map field fanned out into its entry's value" |
| r7 a nesting into a scalar with the key-expression class | `TestKeyValidationTextsAreJavas`, `TestValidateNesting_ParentFieldNotAMessage` |
| r8 the type's checks before the added-version check | "the added version before the type's checks" |
| r9 the subspace key before the validator | "a validator before a shared subspace key" |
| r10 former indexes before indexes | "an index before a former index" |
| r11 no VALUE validator | the two VALUE shapes and "the type's checks before the meta-data version" |
| r12 `clearWhenZero` unchecked | "count updates cleared when zero" |
| r13 COUNT_NOT_NULL cleared when zero read as having values | "count not null cleared when zero over a grouped field" |
| r14 the atomic long check admits the fixed kinds | "a sum of an sfixed32" |
| r15 the bitmap position check refuses them | "a bitmap over an sfixed32 position" |
| r16 no multidimensional structure check | "a multidimensional index without dimensions" |
| r17 `GetRecordType` panics | the two "GetRecordType of an unknown type" builder specs [Superseded → 7.12: three specs, one of them `[PANICKED!]`] |
| r18 a multi-type index added before its types are resolved | 27 multi-type specs of the suite and 8 plain tests, `TestAddMultiTypeIndexResolvesItsRecordTypesFirst` among them (a duplicate registration in every multi-type build) |
| r19 an entry size past the maximum accepted | `TestBitmapValueEntrySizeIsReadAsJavaReadsIt` |
| r20 the missing-primary-key loop in map order | `TestBuildRecordTypeChecksAreJavasInNameOrder` |
| r21 an empty tokenizer name read as the default | `TestTextIndexWithAnEmptyTokenizerNameIsRefused` |
| r22 MAX_EVER_VERSION requires record versions | the MAX_EVER_VERSION maintenance specs [Superseded → 7.12: its guard refused every non-unique MAX_EVER_VERSION index, so all 16 specs reddened; only "builds without SetStoreRecordVersions, as Java does" tells the two behaviours apart] |

No mutation survived, so none found a test gap; they confirm each change has a test that fails
without it. Later revisions use red→green on the changed tests, plus a mutation only where a test
compares two values that could move together (the owner asked for less of this instrument).

### 7.12 Revision 12: the keys handed to the builder, two key validators, the per-type count, and Java's bare texts

Revision 11's three NAKs (`ws-c-addendum-review-v11/`) find every revision-10 finding resolved
but half of storage finding 2, and raise Lows: claims wider than the code, one v10 finding half
resolved, and two key validators and one store method that are not Java's. Revision 12 lands on
the migration branch (`upgrade/java-4.14.2.0`, the owner's one branch for all migration code), on
top of `a33336527`. The 7.11 sentences it changes are marked [Superseded → 7.12].

**Every key handed to the builder is walked (graefe 2, torvalds 2, storage nit).** `SetPrimaryKey`
and `SetRecordCountKey` keep each key they are handed (`handedKeys`), and `firstFault` walks those
as well as the keys the builder holds, so a refused Then that a later set replaced, or that was
handed to the placeholder type `GetRecordType("Nope")` returns, is the fault Java threw at its
`concat`. Pinned by three new shapes of `TestThenOfFewerThanTwoChildrenIsRefusedWhereJavaThrows`.
The walk is also where FunctionKeyExpression.create's refusals surface (WS-J; `FunctionExpr` and
`CardinalityExpr` record them as `Concat` records its arity fault), so it is
`keyConstructionFault`, not `thenArityFault`.

**`DimensionsKeyExpression.validate` and `CardinalityFunctionKeyExpression.validate` are ported
(storage 1).** Java's (`DimensionsKeyExpression.java:89-101`) refuses a prefix and dimensions wider
than the whole key ("dimensions declared a prefix size and number of dimensions that are together
larger than the number of columns in the index") and a dimension column whose field is not of
protobuf type int64 ("the declared dimension columns have to be of type INT64"); a dimension column
that reads no field throws IndexOutOfBoundsException or NullPointerException in Java, which Go
reports as the INT64 refusal (at the site). Java's cardinality validator
(`CardinalityFunctionKeyExpression.java:156-161`) refuses a duplicate-producing argument ("The
CARDINALITY() argument must produce a single value.") and validates the argument, which Go skipped
(the embedded type fell to the switch's default). A MULTIDIMENSIONAL index over an `int32` or
`sint64` dimension no longer builds.

**`GetSnapshotRecordCountForRecordType` is Java's (graefe 4, torvalds 3, storage nit).** Java's
(`FDBRecordStore.java:2431-2453`) never reads the record count key: a COUNT index on the type
alone answers, then a universal COUNT index grouped by record type read at the type's key, and
with neither it throws RecordCoreException "Require a COUNT index on X". Go read the count key
alone and called that Java's. The port does what Java does; Go's tests that read a count key's
per-type group now read it as Java's `getSnapshotRecordCount(recordType(), value)` does
(`GetSnapshotRecordCount(tuple.Tuple{typeKey})`). MEASURED on the JVM: conformance "RFC-257 a
record type's count comes from a COUNT index, as Java's", three modes (a count key grouped by
type: "Require a COUNT index on Order", RecordCoreException, in both; a COUNT index on the type:
3; a universal COUNT index by type: 3), and `recordlayer_test` "per-type counts come from a COUNT
index".

**Java's bare texts (graefe 1, torvalds nit c, storage nit).** `LoggableException` does not render
its log info into `getMessage`, so each of these is now Java's text alone: "unrecognized text
tokenizer" (Go appended ": X"), "tokenizer version could not be parsed as int" (Go appended the
index and option), "unknown tokenizer version" (Go appended the tokenizer, version and bounds),
"sliding window index delegate has multiple types", "sliding window index is on synthetic record
types", "sliding window index requires a RowNumberWindowPredicate" and "need to specify the number
of dimensions" (Go appended "(index X)"). The tests compare the whole text.

**The JVM shapes cover every check 7.11 names (graefe 1).** "Index validation at build, as Java
builds" gains 17 shapes: TEXT's unknown tokenizer, a tokenizer version that is not a number and
one above the tokenizer's, a value, a number as the text field and a repeated string; VERSION's two
versions, no version and uniqueness; MAX_EVER_VERSION's version in the grouping key and no version
in the grouped key; the dimensions key wider than its key, over int32 fields, over int64 fields
(builds) and not covering a key-with-value's key; CARDINALITY over a fanned-out field and over a
missing field. Two checks are still not a JVM shape, each for a stated reason: a TEXT index with no
field after its grouping, where Java's strict guard falls through to `List.get` and throws
IndexOutOfBoundsException and Go returns the guard's text (declared at `validateTextIndexFields`);
and the multidimensional validator's own width check, which Java's key validation reaches first
with the dimensions key's text (the shape "dimensions wider than their key" measures that order).

**A map field's entries are fanned out, and a group is a message (storage nit, torvalds nit d).**
Go refused a nesting over a map field (declared in DIVERGENCES.md); the refusal is gone and the
evaluator fans the entries out as Java does, each entry the entry message protobuf-java reads
(key = 1, value = 2), visited in key order where Java visits the message's order (each entry
yields its own index entries, so no stored byte depends on it) [Superseded → 7.13: wrong; two
entries can write one key, and the last write stays]. A proto2 group is a message to key
validation and to the evaluator, as protobuf-java's MESSAGE java type says; Go refused a nesting
into one. The DIVERGENCES entry "A proto map field is not fanned out in a key expression" is
deleted. MEASURED: "Key validation at build, as Java builds" builds a map fan-out and a group
nesting in both loaders and refuses a group read as a scalar with Java's text, and "Map and group
key expressions are maintained as Java maintains them" saves the same records through both engines
and compares every index key-value pair.

**Documentation (graefe 3, torvalds 1, storage 2 and nits).** The CHANGELOG's `rankNLevels` line
no longer says the option can be corrected: such an index, which only an earlier Go build wrote,
refuses every write (such meta-data loads, contrary to 7.11's "no longer loads"). The "Rolling
upgrade" advice and the `value_expression` rebuild instruction are withdrawn under the ruling. The
Compatibility note names v0.1.0, since the ruling covers old releases. The umbrella RFC's item 8
no longer lists the legacy-records migration item 9 withdrew, and says the workstreams land on one
branch. DIVERGENCES: the record-type checks' title says one Go-only refusal, as its body does; the
VECTOR entry names the call site of the windowed option check (`validateIndex`) and says a plain
VECTOR index runs no validator half at `Build`; the entry "Build does not refuse an index type Go
does not maintain" excepts VECTOR. `index_validator.go` cites the VECTOR entry by its title (it
cited one that does not exist).

**Nits.** The chaos bitmap model checks that an index whose entry size the maintainer refuses holds
no entry (it returned no violation without looking), pinned by
`TestBitmapRefusedEntrySizeHoldsNoEntry` through both arms. `TestTranslatePrimaryKeyToValues`'s
nested-record-type-key assertion was vacuous (the field it named is not in the row); it now pins
that `RecordTypeKey().Nest(f)` translates as `concat(recordType(), f)` and that two such keys over
different fields differ. The R-tree conformance spec checks the tree's shape in all its arms, not
only the two with a node slot index. The WS-J residue the reviews list (the literal-carrier
widening a rebind admits for templates an earlier Go build stored) is withdrawn with WS-J's code.

VERIFIED (Bazel, test cache on, logs under `/var/tmp/fdb-upgrade-recovery/`) [Superseded → 7.13:
the two green logs hold Bazel summaries only, so the spec counts below are not shown by them, and
neither names its tree; "15 JVM shapes" lists 12, the other 3 being the per-type count's modes
named separately]:
- `wsc12-rl.txt`: `recordlayer_test` ("Ran 3712 of 3713", all passed), `chaos_test`, and the
  `pkg/recordlayer/query/...` and `pkg/relational/core/...` targets: 27 of 27 pass.
- `wsc12-conf.txt`: `conformance_test` "Ran 1632 of 1751", 1632 passed; `rfc257_oracle_test` 64 of
  64.
- `wsc12-a.txt`: the JVM Describes "Key validation at build", "Index validation at build" and the
  per-type count, "Ran 56", 56 passed, before the map and group shapes were added (the full run
  above includes them).
- Red on the old code, `wsc12-red-rl.txt` and `wsc12-red-conf.txt`: the changed tests run on
  `a33336527`'s tree (the frozen copy `/home/birdy/projects/fdb-wsc10`). Red there, green here: the
  three new `firstFault` shapes; the per-type count spec (Go and JVM, all three modes); the three
  TEXT texts in `recordlayer_test` and `TestTextTokenizerVersionIsParsedAsJavaParsesIt`;
  `TestBitmapRefusedEntrySizeHoldsNoEntry`; and 15 JVM shapes (dimensions wider than their key and
  over int32 fields, both cardinality shapes, the three TEXT texts, the map fan-out, the group
  nesting and the group read as a scalar at build, and both maintenance specs). Green on both, as
  expected: the shapes whose checks Go already made, and `TestTranslatePrimaryKeyToValues` (its
  rewrite removes a vacuous assertion rather than a bug).
- No mutation was run (the owner's direction: red→green on the changed tests instead). The one
  paired comparison in the change, the maintenance specs' Go-versus-Java key-value lists, is not two
  values that move together: each side is written by its own engine.

Twenty-three `multidimensional_index_test.go` specs, one `online_indexer_test.go` spec and the
chaos multidimensional tests built dimensions over `price` and `quantity`, `int32` fields Java's
dimensions key refuses; they now use `coord_x` and `coord_y` (`int64`), and a prefix over
`quantity` keeps it.


### 7.13 Revision 13: map entries in the record's wire order, one per-type count, and the evidence that names its tree

Revision 12's three NAKs (`ws-c-addendum-review-v12/`, commit `426da82a5`) find every revision-11
finding resolved and raise one Medium each from Torvalds and storage, and Lows. Revision 13 lands on
the migration branch on top of `51c3f90eb`. The 7.11 and 7.12 sentences it changes are marked
[Superseded → 7.12] or [Superseded → 7.13].

**Map entries are indexed in the order the record's bytes hold them (storage 1, Medium).** 7.12 said
the order of a map's entries changes no stored byte. It does: two entries can write one key, and the
last write stays (a covering VALUE index whose key two entries share, a TEXT group two entries share
a token in). Java reads a stored record as a DynamicMessage, which keeps a map field as the entry
list in parse order, and a record Java saves from a generated message is serialized in the map's
own order, so in Java a record's entries are always visited in its bytes' order. Go now does the
same (`record_wire_map_order.go`):
- a record Go decodes from stored bytes keeps them (`recordWire`, only for a type that reaches a
  map field), and `evaluateMap` reads its entries back from them in wire order, a key written twice
  included, each entry with its key and value set as the Go map holds them;
- a record Go saves is evaluated in key order, and `serializeUnion` now writes a type that reaches a
  map with the deterministic marshal, which sorts map entries the same way; vtproto's `MarshalVT`,
  which a generated type with a map would take, writes Go's random map order;
- a decoded message changed after it was loaded (its map no longer what its bytes hold) is
  evaluated in key order, as a message about to be saved.
Every site that builds an `FDBStoredRecord` from stored bytes carries the wire (the three cursor
arms, `LoadRecord`, the old record of `SaveRecord`, `DeleteRecord` and the batch save).
MEASURED, the JVM spec "Map entries are maintained in the record's wire order, as Java maintains
them": raw records with entries out of key order, a key written twice and an entry with no value;
Java saves them with a covering index `KeyWithValue(NestFanOut(m, Concat(value, key)), 1)`; Go builds
the index online over the records Java wrote and over the raw bytes themselves. Java keeps both
entries of the twice-written key, stores the last entry's value for the shared key (`a`, not `b`),
and reads the missing value as 0; Go's two builds equal Java's six key-value pairs. Red on
`51c3f90eb` (5 pairs, `b` stored); unit pins in `record_wire_map_order_test.go` (wire order,
duplicates, each kind of change to a loaded map, a map inside a repeated message, a missing value,
the sorted write, and a panicking `MarshalVT` the serializer must not call).

**One port of the per-type count, and its CLI caller (torvalds 1 and 2, graefe 2, storage 2).**
`snapshotRecordCountForRecordType(name, filter)` is Java's `getSnapshotRecordCountForRecordType(name,
filter)` (FDBRecordStore.java:2431-2453), the one port: the public method passes Java's `TRUE` filter
and raises "Require a COUNT index on X", and the index-rebuild count passes the
being-built-index filter and falls through, as Java's catch does (FDBRecordStore.java:5071-5075).
The rebuild copy, which declined a record type key that is not an integer, is deleted. That decline
was not only there: `singleRecordTypeWithPrefixKey`, the rebuild's emptiness probe, its per-index
record range and the online indexer's build preset (`computeRecordsRange`) all gave up on a string
or bytes type key, which reaches the record keys verbatim (`recordTypeKeyOf`), where Java orders the
key tuples with `Tuple.compareTo`, the packed bytes' order. All four now take any type key, and the
preset's range-set bytes for such a key are Java's. `recordTypeKeyInt64` is gone. Pinned:
`RebuildRecordCountSelection` "counts a string-keyed record type from a COUNT index grouped by
record type" and "scopes the probe to a string-keyed record type's range", and
`TestComputeRecordsRange`'s string-key arm, each red on `51c3f90eb`. `frl record count --type`
reads a record count key that is the record type key at the type's key, as Java's
`getSnapshotRecordCount(recordType(), value)`, and otherwise the per-type method; a
`RecordCoreError` gets advice naming the missing index, and the dead "recordCountKey is nil" string
match is gone. Integration tests count by a type count key and by a COUNT index, and pin the refusal
over an ungrouped count key (the first two red on `51c3f90eb`). An unknown record type is Java's
`MetaDataError` "Unknown record type X" everywhere Go reported one (`unknownRecordTypeError`,
`SaveRecord`, the batch save, the aggregate functions); the unreachable Java-text branch in the
per-type count is gone.

**The dimensions validator (torvalds 4).** The loop guards a negative position, which panicked (a
negative `prefix_size` is valid proto). The comment and 7.12 misdescribed Java: the validated field
list has no entry for a literal or version column, so the fields after one take its place in both
engines, and Java throws only for a position outside the list; the dead `fields[i] == nil` check is
gone. Three JVM rows: dimensions after a literal read the next fields (both build), dimensions
past the fields and a negative prefix (Java's `IndexOutOfBoundsException`, Go's declared INT64
refusal). `TestValidateDimensionsPositionsIndexTheFieldList` red on `51c3f90eb` for the negative
prefix.

**The VECTOR option text (graefe 3, torvalds nit a).** The refusal is Java's
`MetaDataException("incorrect index options", cause)`: the message alone, the parse failure as the
cause, which `MetaDataError` now carries (`Cause`, `Unwrap`), a `NumberFormatError` for a value that
does not parse. The windowed-vector specs compare the whole message and the cause's class; the
sliding-window texts ("delegate has multiple types", "does not support unique indexes") are compared
whole; "need to specify the number of dimensions" gets a spec. The synthetic-record-types arm cannot
fire (Go models no synthetic type, its comment says so) and has no test. DIVERGENCES' VECTOR entry
states the text and that the option list is Go's until WS-D ports Java's engine-aware parse.

**Documentation (graefe 1, torvalds 3, storage 3).** The CHANGELOG no longer lists the map refusal,
counts 50 shapes in "Index validation at build" (at this revision), and gains entries for the map
and group keys and their wire order, the dimensions and CARDINALITY validation, the per-type count's
COUNT index (a behaviour change), non-integer type keys, the Java texts, and index predicates over a
group. 7.11's map refusal, its "six shapes" and its VECTOR sentence are marked; 7.12's order claim
and its VERIFIED block are marked.

**Nits.**
- The per-type count spec asserts Java's class, `com.apple.foundationdb.record.RecordCoreException`.
- `SetPrimaryKey`'s `builder != nil` guard is gone: every `RecordTypeBuilder` is built with one.
- "negative and boundary coordinates" uses int64's extremes (the dimensions are INT64) and keeps
  int32's as ordinary values.
- `TestTranslatePrimaryKeyToValues` requires the `id` side to translate before comparing.
- The umbrella RFC's one-branch ruling is dated 2026-09-24; item 9 records the owner's evidence
  direction (red→green and Java-versus-Go, mutation only for values that move together).
- The planner leaves a map fan-out index out of matching; no Go query reaches that match: Java
  matches it only for a `QueryComponent` that reads the map (`mapMatches`), Go's query surface is SQL
  alone, and neither engine's relational types have a map (`DataType.Code`). Measured over `pkg` and
  `cmd` Go files: `type QueryComponent`, `MapMatches` and `mapMatches` have no hit; the control
  `RecordQueryPlan` has hits. Stated at `proto_field_type.go`'s map arm.
- An index predicate's field path now steps into a group (`resolveFieldPath`, torvalds nit g, which
  was real): Java's `FieldValue` reads a group as a message. Pinned by
  `TestValuePredicateReadsAGroupsField` and a row of "Map and group key expressions are maintained
  as Java maintains them".
- `wsc12-pt.txt`, a red development log of the per-type count, is moved to `dev/`; it was never
  evidence.

**Revision 11's evidence nits.**
- The GroupAlias probe: rerun ten times with each run's log kept
  (`evidence/wsc13-groupalias10/run_N_of_10-test.log`): every run "Ran 1 of 1763 Specs", 1 passed.
- `ev2/` recorded its tree before the run only. It is not recoverable; revision 13's evidence
  records the tree each run used, and replaces it.
- "the mutation reddens this test alone" (n17, 7.11) is withdrawn, not re-measured: under the
  owner's evidence direction no mutation is run for it, and nothing rests on the sentence (the test
  is red on the tree before its fix, as 7.10 recorded).

**Evidence.** `/var/tmp/fdb-upgrade-recovery/save-evidence.sh` refuses a Bazel run that executed no
test (cached, or failed to build) and saves the console and each test log with `TREE`: the HEAD, the
index tree (`git write-tree`) and the sha256 of the working tree's difference from the index. The red
runs are on the frozen copy `fdb-wsc10`, reset to `51c3f90eb`'s tree (`4262fab36`) plus the changed
test files.

VERIFIED (Bazel, test cache on; `evidence/<name>/` under `/var/tmp/fdb-upgrade-recovery/`, each with
`console.txt`, the test logs and `TREE`):
- `wsc13-rl-green`, index tree `4262fab36` plus working diff `fe3f95ab…`: `//pkg/recordlayer/...`,
  `//cmd/frl/...` and `//pkg/relational/core/...`, "Executed 24 out of 34 tests: 34 tests pass";
  `recordlayer_test` "Ran 3724 of 3725 Specs", 3724 passed, 1 skipped.
- `wsc13-conf-green`, the same tree and diff: `conformance_test` "Ran 1644 of 1763 Specs", 1644
  passed (the 119 skipped are the suite's own filters); `rfc257_oracle_test` 64 of 64.
  Only `DIVERGENCES.md` and this document changed while these two ran, and neither is an input of
  those targets.
- `wsc13-mapwire-green` and `wsc13-mapwire-red`: the wire-order spec, green here (Java's six pairs,
  printed) and red on `51c3f90eb`'s tree.
- `wsc13-rl-red` (`recordlayer_test` on `51c3f90eb`'s tree with the changed test files, "Ran 3724 of
  3725", 7 specs and 2 unit tests red): the two string-keyed rebuild specs, the unknown-type texts
  (three specs), the two windowed-vector option specs, `TestComputeRecordsRange`'s string key and
  `TestValidateDimensionsPositionsIndexTheFieldList`'s negative prefix.
- `wsc13-frl-green` and `wsc13-frl-red`: the record-count tests, 8 of 8 here; on `51c3f90eb`'s tree
  the two new count tests fail ("Require a COUNT index on Order"; the unwrapped Customer refusal).
- `wsc13-conf-red` and `wsc13-conf-red-dims` (`conformance_test` on `51c3f90eb`'s tree with the
  changed spec files): red, the group-predicate row, the wire-order spec and the negative dimensions
  prefix, which panics there (`index out of range [-1]`); green on both, the rows whose behaviour
  predates this revision (dimensions after a literal, and past the fields, where Java's text is
  "Index 1 out of bounds for length 1").
- `wsc13-groupalias10`: above.
- No mutation was run. The paired comparisons this revision adds are Go against Java (the wire-order
  and predicate specs), each side written by its own engine.
