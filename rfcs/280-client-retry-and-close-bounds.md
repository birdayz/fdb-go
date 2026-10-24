# RFC-280: Client retry and close bounds

**Status:** Implemented

**Base:** stacked on PR #1004 (RFC-276), which builds on RFC-271 (PR #997, merged).
RFC-271 rewrote `handleConnError` to name the failed connection; item 3 changes that
code, and items 1 and 2 touch files RFC-276 also edits.

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
- Each request is sent the way `loadBalance` sends it (`sendMetricsRequest`): the
  replicas the failure monitor does not exclude, in order, with no reply timeout
  (`LoadBalance.actor.h:595-614`). A round in which no replica answered backs off from
  10 ms, doubling to 5 s (`LOAD_BALANCE_*_BACKOFF`). When every replica is excluded,
  the request sends nothing and waits `allAlternativesFailedDelay` (50 ms, growing
  while failures continue), then reports 1006, unless one of its own replicas recovers
  first, which ends the wait as `okFuture` does (another address recovering does not).
- An in-band `broken_promise` or `request_maybe_delivered` reply is not a response, as
  in `checkAndProcessResult` (`:344-377`): the request marks a `broken_promise`
  endpoint not found and goes on to the next replica in the same round.
- Replicas whose endpoint was reported not found are dropped before every round
  (`failMon.liveServers`), as `loadBalance` checks each alternative's state per
  attempt (`LoadBalance.actor.h:600-601`); with none left the request reports 1006
  after the delay, so the caller re-locates.
- The 5 s per-replica reply timeout goes: `loadBalance` has none, and with no cap it
  would keep a request that a slow replica answers in more than 5 s from ever ending.

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
the dial-panic backstop only on a panic. Between the unlock and the close, a sender can
still take the failing connection from a completed dial whose send cohort has not
drained; its send fails as on any connection that dies after lookup, and the read
retries. `Database.Close` can likewise return while such a connection is still closing;
its loops end with the close.

## 4. `TransactCtx` and a dispatched commit: kept

In libfdb_c the transaction ends the wait for a dispatched commit: RYW `commit` waits on `resetPromise.getFuture() || tr.commit()`
(`ReadYourWrites.actor.cpp:1381`, `:1405`), so a timeout or `cancel` returns
`transaction_timed_out` or `transaction_cancelled` at once. The native commit is held by
`Transaction::committing` (`NativeAPI.actor.cpp:6942-6945`) and runs on; only `reset`
or destruction drops it (`:6172`, `:5486`). Go does the same: a timeout, `Cancel` or
`Reset` ends the public wait and leaves the native commit running
(`client/native_commit.go`; `TestCommitWait_TransactionInterrupts`,
`TestCommitWait_TransactionInterruptionLeavesNativeFenceAlive`).

So does cancelling the commit future: `fdb_future_cancel` (`bindings/c/fdb_c.cpp:180-182`)
cancels the main-thread actor that holds the RYW commit
(`ThreadSafeTransaction.cpp:666-671`), while `committing` keeps the native commit
running. The closest Go analogue, `TransactCtx`'s context, deliberately does not end the
wait (RFC-090): it would return a Go context error for a commit that may have applied,
and the retry loop would stop there (`TestCommitWait_CallerContextStaysDetached`). That
stays, recorded as a difference from `fdb_future_cancel`. The `TransactCtx` doc says so
and names the bound that matches libfdb_c, the transaction timeout or `Cancel`.

## Tests

- `client`, fast lane (`conn_location_unit_test`):
  - `TestHandleConnErrorClosesOutsideConnMu`: the failed connection's `Close` blocks
    until released; `connMu` must be free meanwhile, the pool empty and the address
    failed.
  - `TestMetricsRequestWakesOnRecovery`: with the storm delay at 30 s, a request whose
    only replica failed parks (a test hook reports it) and stays parked, without
    restarting its wait, when another address recovers; it reaches its replica within
    5 s of that replica's recovery.
  - `TestMetricsRequestSkipsEndpointNotFound`: a replica whose metrics endpoint was
    reported not found before the request, or by an ENDPOINT_NOT_FOUND packet while it
    waits, gets no further request, and the call reports 1006.
  - `TestMetricsRequestBrokenPromiseTriesNextReplica`: an in-band `broken_promise`
    from the first replica marks its endpoint not found, and the second replica's reply
    completes the call.
  - `TestMetricsRequestSkipsExcludedReplica`: an excluded replica listed first is not
    dialed; the request reaches the available one.
  - `TestMetricsRequestWaitsForSlowReply`: a reply that comes after more than the old
    5 s timeout is used, with one request sent.
- `client`, test-full (`client_test`, FDB in a container, replies rewritten by the
  `simDialer`):
  - `TestMetricsRetryWrongShardWithoutCap`: 55 `WaitMetrics` replies, then 55
    `SplitRange` replies, are `wrong_shard_server`; both calls succeed.
  - `TestMetricsRetryIsPaced`: every `WaitMetrics`, then every `SplitRange`, reply
    severs its connection; with a 2 s transaction timeout each call makes at most 30
    rounds and fails with `transaction_timed_out`.
  - `TestMappedSelectorReadsCapReplyTimeouts`: every `GetMappedKeyValues` reply is
    dropped, with a 20 ms reply timeout; both loops send 11 requests and return
    `transaction_too_old`.

Each fails with its fix reverted.

## Compatibility

No bytes change. A size estimate or split-point request no longer fails with 1006
after 50 rounds, and no longer gives up on a replica after 5 s; it waits like
libfdb_c, bounded by the context and the transaction timeout. A positive-skip mapped read that keeps timing out returns a retryable
`transaction_too_old` instead of looping.
