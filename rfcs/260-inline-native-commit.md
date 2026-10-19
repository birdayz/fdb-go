# RFC-260: Run the native commit on the committing goroutine

**Status:** Draft
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
`set100`, `GOMAXPROCS=8`, client CPU per transaction, median of five runs. A measurement
mutant that runs the same code synchronously on the caller (no abandonment handling):

| | master | inline | libfdb_c |
|---|---|---|---|
| closed loop, 8 workers | 51.5 µs | 50.9 µs (0.988) | 45.9 µs |
| closed loop, 32 workers | 36.3 µs | 34.0 µs (0.937) | 33.1 µs |
| paced 2,000/s, 64 workers | 162.2 µs | 141.6 µs (0.873) | 122.9 µs |

Throughput and latency are unchanged (paced mean 839 → 832 µs). futex calls fall from 10.2
to 8.1 per paced transaction. Paced client CPU goes from 1.32× to 1.15× libfdb_c.

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

## Design

The caller runs the native commit itself. At each wait that can outlast the public result,
it also watches the public-wait-ended signal (`native.done`, closed by
`selectResult(cause, false)`). When that fires, or before a wait that inline execution
does not handle, the caller hands the remaining work to a goroutine at the same stage and
returns to `<-native.done` as today. On the common path no goroutine is created.

The commit becomes a small stage machine (`commitAttempt`) with one code path for both
executions; the goroutine runs the remaining stages with no abandonment signal, exactly as
the current goroutine does. Stages, and what inline execution does at each:

| Stage | Blocking wait | Inline |
|---|---|---|
| Commit GRV | read-version fetch or a pending fetch | Uses a context the public-wait-ended signal cancels. If that cancellation alone ends the wait, a goroutine restarts the commit from this stage with today's contexts. Nothing has been sent; a cancelled GRV producer already lets other waiters fetch again. |
| No proxy known | proxy-set change or context | Selects on the signal too; a goroutine restarts from proxy selection. |
| Connection | dial | A pooled connection is taken inline; a dial is handed to a goroutine before it starts. |
| Send | queue backpressure only (256 frames) | Inline, unobserved, as the send is today. |
| Reply | reply, proxy change or context | Selects on the signal too; a goroutine takes over the same reply channel. |
| Unknown result, dummy fence | GRV and a commit | Always on a goroutine. |

`registerBackgroundGoroutine`, the owner cleanup, the weak completion pointer and
`finished` keep their meaning: the registration covers the commit wherever it runs, and the
goroutine that takes over inherits it. A panic is recovered where it happens and re-raised
by the caller, as now.

## Executable spec

- Existing native-commit lifetime tests pass unchanged (`commit_grv_lifetime_test.go`,
  `native_commit_*_test.go`): a commit outlives Cancel and timeout, and Reset or owner
  destruction stops it.
- New, deterministic: for each handoff stage (commit GRV, no proxy, dial, reply, fence),
  the public wait ends on Cancel or timeout while the commit is blocked there, and the
  commit still completes afterwards (its versionstamp or committed version is published,
  or, for Reset, the commit stops). Stages are held with the existing fault dialers and
  seams.
- New: on the success path no goroutine is started (a counter of handoffs stays zero).
- Revert-proof: removing the handoff at the reply stage makes the reply-stage test hang
  (bounded by its context) or fail.
- Race lane on the client package.

## Wire-compatibility impact

None: the same frames are sent in the same order. A GRV producer cancelled by abandonment
can make the handed-off commit fetch its read version again; that is one extra GRV request,
only when the public wait ended during the commit GRV.
