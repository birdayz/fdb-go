# RFC-261: Fewer goroutines and allocations per transaction

**Status:** Part B accepted for B2 and individually justified B4 sites; B1 and B3 each need
their own design review; Part A deferred (review 2026-10-19, see "Review outcome").
**Item:** TODO.md §0, "Client CPU on paced writes" and "Client CPU per transaction";
supersedes the deferred [RFC-260](260-inline-native-commit.md).
**Spec:** FoundationDB 7.3.77 (`fdbclient/NativeAPI.actor.cpp`, `fdbclient/ReadYourWrites.actor.cpp`).

## Problem

Master `13580f1b7`, tmpfs three-process cluster, `GOMAXPROCS=8`, 7,000 transactions with
every allocation sampled (`GODEBUG=memprofilerate=1`):

| Workload | allocations / txn | bytes / txn | new goroutines / txn |
|---|---|---|---|
| `set100`, closed loop, 8 workers | 63.8 | 6.8 KB | 1.0 (commit) |
| `set100`, paced 2,000/s | 66.0 | 7.0 KB | 1.2 (commit, GRV flush) |
| `get100`, closed loop, 8 workers | 55.9 | 5.3 KB | 0 |

About 5 of each are the benchmark harness. The largest client sites on `set100`:

| allocs | site | what |
|---|---|---|
| 8 | `startNativeCommit` | commit struct, context, two channels, cleanup, weak pointer, goroutine |
| 5 | `rywCache.set` | the write map's first entry |
| 4 | `armTimebombLocked` | per-transaction TIMEOUT timer, closure, weak pointer, cleanup |
| 3 | `readIncarnationLocked` | incarnation and its cancellable context |
| 2 each | GRV request, pending read version, `opContextWithLease`, `CreateTransaction`, `regenerateSpan`, `makeSelfConflictingLocked`, its `slices.Clone` | |

CPU: running the commit on the caller (RFC-260's measurement mutant, no abandonment
handling) cut client CPU to 0.988×/0.937×/0.873× of master at 8 and 32 closed-loop workers
and paced. That is the measured upper bound for Part A. Allocation cuts alone have not
moved CPU measurably before (#951: −12% allocations, CPU change only together with the
conflict-vector work), so Part B's CPU benefit is unproven and is measured per step.

## Part A: the commit runs on the committing goroutine

RFC-260's rejected plan restarted work on abandonment. This design never restarts and
never cancels anything a waiter leaves: each wait is a stage of an explicit state machine,
and whoever owns the machine drives it. The caller drives it; when the public wait ends
early it hands the machine to a goroutine, which resumes the same stage.

### Abandonment signal

`failCapturedIncarnation` records the cause and then cancels the incarnation's context
(`read_incarnation.go`), for Cancel, timeout and turnover alike. That is
`resetPromise.sendError` (`ReadYourWrites.actor.cpp:1381,1405`). The caller's stage waits
add `inc.ctx.Done()` and need no new channel. Reset and owner destruction still stop the
native work through its own context, as now (`NativeAPI.actor.cpp:6172`).

### A1. The read-version fetch as a resumable future

Today a fetch is a call chain whose waits live on the producer's stack:
`cachedOrFetchReadVersion` → `fetchReadVersion` → `grvBatcher.getReadVersion` →
(`admit`, then a window, or the serial path's `flushWithin` → `sendGRVRequest`, a retry loop
over proxy selection, dial, send, reply and backoff) → `publishReply`, `applyGRVReply`, and
`fetchReadVersion`'s deferred publication to `pendingReadVersion`.

It becomes a `grvFetch` object that holds the batch, the attempt state (proxies, epoch,
proxy-change channel, reply handle, backoff) and the stage. `drive(leave <-chan struct{})`
runs stages until the fetch completes or `leave` closes while a stage is waiting; it then
returns `left` with the stage intact. A goroutine continues with `drive(nil)`. Nothing is
cancelled: the request stays on the wire, the batch members still get the reply, and the
pending read version is published once. This is C++'s shape: the commit's `startFuture`
and the shared read-version future outlive the RYW wait (`NativeAPI.actor.cpp:3598-3610,
6578, 6592-6598, 6942-6945`).

Caller cancellation keeps today's meaning (the commit GRV observes the caller's context and
fails with it). The batched path (a window, or a flush by another requester) is already a
channel wait on `req.reply` and needs no change beyond leaving.

### A2. The commit attempt as a state machine

`commitAttempt` holds the selected proxy and epoch, the captured proxy-change channel, the
connection, the reply registration and buffer, the send state and the fence outcome.
Stages:

| Stage | Wait | Leaving |
|---|---|---|
| GRV | `grvFetch` (A1) | the fetch is handed over with the attempt |
| no proxy | captured proxy change or context | resumed with the same channel, then the fence; never a new selection |
| connection | pooled lookup inline; a dial is a stage | the dial's wait is resumed |
| send | non-blocking enqueue; on a full queue, an admission wait | the frame is either enqueued (owned by the writer) or not; resuming continues the same wait; never enqueued twice |
| reply | reply, proxy change, context | the same reply channel |
| fence | `commitDummyTransaction` (its own GRV and commit) | resumed; the original commit is never resent |

### A3. Ownership

- One executor at a time. A handoff transfers the attempt once and does not finish it.
- `startNativeCommit` registers the commit under `readErrMu` and returns; the caller
  releases the execution lease and then drives the attempt (no inline stage under a lock).
- One terminal path releases operands, reply handle and buffers, closes `finished`, calls
  `db.wg.Done` and publishes completion.
- What exists only for detached work is created at handoff: the native context, the owner
  cleanup and the weak completion pointer. The common path allocates none of them.
- A panic is recovered where it happens; first selection keeps the public result.

### Expected effect

On a cached GRV and pooled connection: no goroutine and about 8 fewer allocations per
commit. CPU: up to the mutant's 0.873× paced and 0.937× at 32 workers.

## Part B: allocation cuts that do not depend on Part A

Each lands separately, with an allocation pin (`testing.AllocsPerRun` on a unit path) and
a paired CPU campaign; any step without a measurable CPU or allocation benefit is dropped.

- **B1. TIMEOUT.** One timer per database driving a heap of transaction deadlines,
  instead of a timer, closure, weak pointer and cleanup per transaction. C++ arms a
  `delay` per transaction on the network thread's timer queue; the heap keeps the same
  deadline and firing order. Up to 4 allocations.
- **B2. Write map.** A first-write representation that defers building the map (TODO
  §0 already names it). Up to 5 on single-write transactions.
- **B3. Incarnation context.** Create the cancellable context when an operation first
  needs a `Done` channel. Up to 2.
- **B4. Smaller sites.** The GRV request's reply channel and pending read version
  (pooled), span regeneration, the self-conflicting range's copy.

## Cost and risk

Part A rewrites the GRV fetch and the commit path, the client's most correctness-critical
code; the read path's GRV shares A1. Estimate: several days of implementation, the full
deterministic test list from RFC-260 ("Requirements"), the differential, chaos and race
lanes, and several review rounds. Part B is a sequence of small, independently measured
changes.

## Questions for review

1. Is Part A worth its risk for the measured upper bound (−13% paced, −6% at 32 workers)?
2. Is A1's leave/resume protocol faithful, including the serial path, caller cancellation
   and the fence?
3. Should Part B go first, given that allocation cuts have not moved CPU alone before?

## Review outcome

Both reviewers (C++ client, code quality) recommended **B only**, at `0c07d0a84`.

- **Part A is deferred.** Leave/resume is directionally faithful (the GRV producer and
  future are kept, the no-proxy wait and fence continue, nothing is resent), but the
  ownership protocol is incomplete:
  - the native-stop state must exist from admission and transfer atomically, so Reset
    before, during or after a handoff stops the work and nothing revives it
    (`NativeAPI.actor.cpp:6172`); incarnation cancellation alone cannot tell "keep native
    work" from "destroy it";
  - a synchronous dial, and the fence's dummy transaction with its retries and backoff,
    cannot surrender their stacks; each needs a real resumable mechanism and an owner;
  - leaving a shared GRV wait must not take over someone else's producer: serial
    batches, member replies and pending-version publication need explicit owners;
  - send admission must keep the enqueued/not-enqueued boundary.

  The tests must cover the boundary races: Reset during registration and transfer,
  leaving while a reply arrives, caller cancellation during a GRV transfer, panics, DB
  Close, and exactly-once release.
- **B order.** B2 first, then each B4 site on its own measured evidence. Allocation-only
  wins are reported as such.
- **B1** needs its own design: C++'s `timebomb` uses `creationTime + timeout`, rechecks
  the time and schedules with a 100 µs margin (`ReadYourWrites.actor.cpp:1567-1578`).
  Timeout replacement and disabling, the sticky first cause, Reset isolation, the weak
  owner lifetime and shutdown must hold, and stale heap entries must never affect a
  successor (`:2691-2703, 2735-2754`). Heap contention and retention are measured, not
  only allocations.
- **B3** must create an already-cancelled context when cancellation came first, with
  that race and Reset pinned.
- **Pooling** (B4) needs proof that no late producer publishes into a reused object.
