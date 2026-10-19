# RFC-260: Run the native commit on the committing goroutine

**Status:** Deferred. The plan below was reviewed and rejected (2026-10-19); the requirements
for a faithful version are recorded under "Requirements". Not implemented.
**Item:** TODO.md §0, "Client CPU on paced writes" and "Client CPU per transaction".
**Spec:** FoundationDB 7.3.77 (`fdbclient/ReadYourWrites.actor.cpp`, `fdbclient/NativeAPI.actor.cpp`).

## Problem

Every non-dummy `Commit` starts a goroutine (`startNativeCommit`, `native_commit.go`) that
runs the commit GRV, the commit RPC and the outcome bookkeeping, while the caller blocks on
`native.done`. The goroutine is what lets the commit outlive the public wait, but on the
common path nothing ends the wait early, and the goroutine costs two scheduler wake-ups per
commit: `newproc` wakes an idle P although the caller is about to block and run the new
goroutine itself, and the goroutine's `close(done)` wakes another before it exits.

Measured on a three-process local cluster with its data on tmpfs (stable 1 ms commits),
`set100`, `GOMAXPROCS=8`, client CPU per transaction, median of five runs (libfdb_c: three).
A measurement mutant that runs the same code synchronously on the caller (no abandonment
handling); data in
[`results-inline-commit.jsonl`](../docs/benchmarks/rfc-260/results-inline-commit.jsonl):

| | master | inline | libfdb_c |
|---|---|---|---|
| closed loop, 8 workers | 51.5 µs | 50.9 µs (0.988) | 45.9 µs |
| closed loop, 32 workers | 36.3 µs | 34.0 µs (0.937) | 33.1 µs |
| paced 2,000/s, 64 workers | 162.2 µs | 141.6 µs (0.873) | 122.9 µs |

Throughput and latency are unchanged (paced mean 839 → 832 µs). futex calls fall from 11.8
to 9.5 per paced transaction (medians). Paced client CPU goes from 1.32× to 1.15× libfdb_c.

Provenance: `clientbench` built with Bazel from master `e5209b8c6` (client code identical
to `13580f1b7`) and from that tree with the mutant patch (the `go n.run(...)` in
`startNativeCommit` replaced by a call the caller makes after `owned.release()`); the
binaries' SHA-256 prefixes are in the data. Each run is `-iterations`/`-warmup`
20000/2000 (8 workers), 40000/4000 (32) and 24000/4000 (paced, `-rate 2000 -seed 1`),
`GOMAXPROCS=8`, client on CPUs 0–7, under `perf stat` (task-clock, context switches,
futex, epoll_pwait and read syscalls), per transaction including warmup.

## C++ semantics that must hold

- The RYW commit waits on `resetPromise.getFuture() || ryw->tr.commit()`
  (`ReadYourWrites.actor.cpp:1381,1405`; reads first, `:1360`). A timeout, `cancel()` or
  `reset()` fails `resetPromise`, which ends the public wait at once.
- The native `Transaction::commit` actor (`committing`, `NativeAPI.actor.cpp:6942-6945`)
  keeps running after the RYW wait ends. Only `Transaction::resetImpl`, which drops `committing` (`:6172`), or the
  transaction's destruction drops it.
- Caller cancellation reaches the commit's GRV only (existing Go behaviour, unchanged).

So the public wait must still end as soon as the result is selected (`selectResult(cause,
false)`), and the work must still continue after that, until `stop()`.

## Rejected plan

The caller would run the commit and, when the public wait ended early, hand the remaining
stages to a goroutine: restart the commit from the GRV or from proxy selection, take dials
and the unknown-result fence on a goroutine, and send inline. Both reviewers rejected it:

- Restarting the commit GRV on abandonment sends a second GRV. C++ keeps the `committing`
  actor and with it the commit's start future (`NativeAPI.actor.cpp:6578,6592-6598,
  6942-6945`); `startTransaction` reuses an existing read-version future (`:3598-3610`).
  A second GRV adds traffic and can change the read snapshot, and
  `commit_grv_lifetime_test.go` requires one GRV for Cancel and timeout.
- Restarting proxy selection after a no-proxy wait could find a proxy and send the
  original commit. Today the captured proxy-change wait resolves through the
  unknown-result fence, as C++ races `onProxiesChanged` and fences (`:6646-6649,
  6730-6750, 6773`).
- An inline send blocked on a full write queue would hold the public caller after
  `native.done` closed. Today the goroutine absorbs that wait.
- `startNativeCommit` holds `readErrMu`, and its caller holds the execution lease, so
  inline stages must start only after both are released.

## Requirements

A faithful version needs every wait the commit makes, the commit GRV included, to be
detachable without cancelling or replaying anything:

- **GRV.** The commit usually produces its own read version: in one Go execution trace of
  the inline mutant (paced, not retained) about 0.8
  GRV waits per transaction happen inside the commit, 0.55 of them on the batcher's
  serial path, which runs the RPC on the requester (`flushWithin`, `sendGRVRequest` with
  its proxy, dial, send, reply and backoff waits). Handing off before a GRV that is not
  yet available would hand off almost every commit. The GRV fetch has to become
  resumable: a waiter that leaves keeps the producer, its request and its future.
- **Send.** A non-blocking enqueue with a fallback, or a detachable admission wait, with
  an explicit enqueued/not-enqueued boundary: never enqueue twice, never abandon an owned
  buffer.
- **No proxy.** Resume the captured proxy-change wait and its fence; never select anew.
- **Ownership.** One executor at a time; a handoff transfers the attempt once (proxy,
  epoch, proxy-change channel, reply registration, send state, fence outcome) without
  finishing it. One terminal path releases the operands, reply handle and buffers, stops
  the cleanup and context, closes `finished` and calls `db.wg.Done`. No closure retains
  the public transaction. Public-result selection keeps its first-selection rule; a
  panic after public cancellation must not overwrite the result.
- **Tests.** The existing lifetime tests unchanged, plus deterministic cases for: GRV
  producer and shared-waiter handoff (one request, original version adopted), caller
  cancellation versus Cancel/timeout and Reset; a full send queue with a stalled peer
  (public return first, then exactly one commit; Reset and Close while blocked);
  no-proxy handoff then a topology change (fence, no new send); reply/handoff races and
  pending dials; every unknown-result entry; Reset, owner collection, Close and panics
  around a handoff with exactly-once release; and zero handoffs on a cached GRV with a
  pooled connection.

## Other measurements

Exploratory runs on the original cluster (log on disk), paced `set100` at 2,000/s with 64
workers unless noted, in
[`results-exploratory.jsonl`](../docs/benchmarks/rfc-260/results-exploratory.jsonl) (84
runs; some labels were reused, so only the last run under a label is retained, and the
busy-spin campaign's own baseline is lost). These are measured contributors, not a full
cost attribution:

- The write delay (`coalesceWait`, a `timerfd` read through the netpoller): with
  `-coalesce-delay 0,0` a paced write costs 117–119 µs against libfdb_c's 111–114 µs
  (master median 147 µs). libfdb_c's timers are serviced by its network thread's reactor
  (`Net2.actor.cpp:1550-1564` takes the sleep from the task queue, `:2111-2119` waits on the
  ASIO timer), so they need no cross-thread wake-up; Go's `timerfd` wait parks one
  goroutine and wakes it through the netpoller.
- Spinning instead of the timer: with `runtime.Gosched` 163–164 µs (worse), a plain spin
  143 µs (the spin itself costs the delay).
- The commit taking the writer's turn: 143.6 µs paced alone; combined with the inline
  mutant it is 0.845× master paced but 1.08× at 8 closed-loop workers on the tmpfs cluster
  (main data, `inline-turn-mutant`).
- Parking before the first read of the timer or the socket deadlocks: `RawConn.Read`
  resets the poll descriptor (`prepareRead`), discarding a readiness edge recorded since
  the last read.
