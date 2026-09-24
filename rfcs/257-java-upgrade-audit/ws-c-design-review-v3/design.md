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
