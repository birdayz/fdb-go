# RFC-280: Client retry and close bounds

**Status:** Implemented

**Base:** stacked on PR #1004 (RFC-276), itself on PR #997 (RFC-271). RFC-271 rewrites
`handleConnError` to name the failed connection; item 3 changes that code, and items 1
and 2 touch files RFC-276 also edits.

**Items** (the Go FDB client entries of the 2026-10-24 quality review, open PR #994):

1. `GetEstimatedRangeSizeBytes` and `GetRangeSplitPoints` time out each replica after
   5 s and end with a non-retryable `all_alternatives_failed` (1006); C++ retries 1006
   and wrong-shard without a cap.
2. A mapped-range read with a positive skip re-sends on reply timeout without the
   10-re-send cap the other reads have (`mappedrange_selectors.go`).
3. `handleConnError` closes the connection under `connMu`, and a TLS socket close in
   `failConnection` can block for up to 5 s.
4. A `TransactCtx` deadline does not bound a dispatched commit (`transaction.go`,
   documented at `fdb/database.go`). Decide whether to keep it.

## 1. Storage metrics retry without a cap

C++ `doGetStorageMetrics` catches `wrong_shard_server` and `all_alternatives_failed`,
invalidates the cache, waits `WRONG_SHARD_SERVER_DELAY` and starts over; it catches
`future_version` the same way (`fdbclient/NativeAPI.actor.cpp:7799-7811`).
`getRangeSplitPoints` is a `loop` with the same catch (`:8162`, `:8204-8208`). Neither
counts attempts: only the transaction timeout ends them. Its `loadBalance` has no reply
timeout. A round where every alternative is failed waits `allAlternativesFailedDelay`
(`fdbrpc/LoadBalance.actor.cpp:28-46`, `LoadBalance.actor.h:617-639`); a round whose
alternatives answer with errors backs off from 10 ms (`:682-686`).

Go gives up after `MaxWrongShardRetries` (50) rounds with a non-retryable 1006
(`metrics.go`). Each replica that does not answer in 5 s is skipped, so a slow shard
reaches that cap after 50 × replicas × 5 s and then fails a call C++ would still be
waiting on.

Change:

- Both loops retry until success, a non-retryable error or the operation context ends.
  The context carries the transaction timeout (RFC-112), as C++'s `waitOrError` does.
- A round in which no replica answered ends in `db.allAlternativesFailed`, which waits
  C++'s jittered `allAlternativesFailedDelay` (50 ms, growing toward 1 s while failures
  continue) before reporting 1006. Without it, an uncapped loop of instant failures
  would spin on the 10 ms wrong-shard delay. Replicas the failure monitor holds as
  failed are skipped first, as the reads do (`failMon.liveServers`).
- The 5 s per-replica reply timeout stays: it moves to the next replica, which
  `loadBalance`'s second request approximates. With no cap it no longer turns a slow
  shard into an error.

This differs from the range reads' deliberate cap (DIVERGENCES.md, "Range-read retry
bounds"). A read retries at one read version, and C++ itself ends that with
`transaction_too_old` once the version ages. A size estimate or split-point request
without a tenant runs at the latest version and has no such end, so a cap would be an
error C++ never returns.

## 2. Mapped selector reads cap reply timeouts

`getMappedRangeSelectors` and `getMappedRangeSelectorExact` re-send a timed-out request
forever. The other reads count re-sends and return a retryable `transaction_too_old`
after `maxReadTimeoutRetries` (10), as `rangeScanImpl` does (DIVERGENCES.md, "Range-read
retry bounds"). Both loops get the same counter. Their wrong-shard handling is
unchanged.

## 3. Close the failed connection outside `connMu`

`handleConnError` removes the pooled connection, marks the address failed and closes
the connection, all under `connMu`. `transport.Conn.Close` closes the socket; a TLS
socket first sends close_notify under a 5 s write deadline (`crypto/tls` `Conn.Close`),
and every dial and pool lookup waits on `connMu` meanwhile.

Change: the removal and the failure mark stay under `connMu`, so a redial cannot slip
between them (RFC-271); the close runs after the unlock. Other closes under `connMu`
are left: `Database.Close` and a dial that lost to `Close` happen during shutdown, and
the dial-panic backstop only on a panic.

## 4. `TransactCtx` and a dispatched commit: kept

libfdb_c has no context. What ends its wait for a dispatched commit is the transaction:
RYW `commit` waits on `resetPromise.getFuture() || tr.commit()`
(`ReadYourWrites.actor.cpp:1381`, `:1405`), so a timeout or `cancel` returns
`transaction_timed_out` or `transaction_cancelled` at once. The native commit is held by
`Transaction::committing` (`NativeAPI.actor.cpp:6942-6945`) and runs on; only `reset`
or destruction drops it (`:6172`, `:5486`). Go does the same: a timeout, `Cancel` or
`Reset` ends the public wait and leaves the native commit running
(`client/native_commit.go`; `TestCommitWait_TransactionInterrupts`,
`TestCommitWait_TransactionInterruptionLeavesNativeFenceAlive`).

`TransactCtx`'s context is Go's own (RFC-090). Making it end the wait would return a
Go context error for a commit that may have applied, and the retry loop would stop
there; RFC-090 chose to keep an in-flight commit's outcome
(`TestCommitWait_CallerContextStaysDetached`). That stays. The `TransactCtx` doc now
names the libfdb_c bound, the transaction timeout, instead of saying that cancellation
in libfdb_c releases the wait.

## Tests

- `client`, fast lane (`conn_location_unit_test`): `TestHandleConnErrorClosesOutsideConnMu`.
  The failed connection's `Close` blocks until released; `connMu` must be free
  meanwhile, the pool empty and the address failed.
- `client`, test-full (`client_test`, FDB in a container, replies rewritten by the
  `simDialer`):
  - `TestMetricsRetryWrongShardWithoutCap`: `MaxWrongShardRetries` + 5 `WaitMetrics`
    replies, then as many `SplitRange` replies, are `wrong_shard_server`; both calls
    succeed.
  - `TestMetricsRetryIsPaced`: every `WaitMetrics` reply severs its connection for 3 s;
    the call ends at the deadline after at most 40 rounds.
  - `TestMappedSelectorReadsCapReplyTimeouts`: every `GetMappedKeyValues` reply is
    dropped, with a 20 ms reply timeout; both loops return `transaction_too_old` after
    11 requests.

Each fails with its fix reverted.

## Compatibility

No bytes change. A size estimate or split-point request no longer fails with 1006
after 50 rounds; it waits like libfdb_c, bounded by the context and the transaction
timeout. A positive-skip mapped read that keeps timing out returns a retryable
`transaction_too_old` instead of looping.
