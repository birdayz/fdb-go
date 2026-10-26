# RFC-280: Client retry and close bounds

**Status:** Implemented

**Base:** parent PR #1004 (RFC-276) merged as `e14dd5bea`; the child was restacked
onto master, retaining RFC-281's hedge budget (#1017). RFC-271 (PR #997) made
`handleConnError` name the failed connection; item 3 changes that code, and
items 1 and 2 touch files RFC-276 also edits.

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
- All located shards start before waiting, as `waitForAll` does (`NativeAPI.actor.cpp:7835-7845,
  8171-8184`); a terminal error cancels and joins the other waiters, not their shared
  connections or database-owned dials. Successful results retain shard order. Size
  workers retry and re-locate only their own subrange (`doGetStorageMetrics`, :7785-7811),
  so a retryable error cannot cancel and hide a sibling's terminal error. Repeated
  singleton retries stay iterative; a location that splits starts child workers for
  its clamped subranges. Split-point retries restart the whole fanout, as C++ does
  (`:8204-8210`).
- Each request is sent the way `loadBalance` sends it (`sendMetricsRequest`): one
  replica per attempt, starting at a random one and skipping replicas the failure
  monitor excludes, with no reply timeout (`LoadBalance.actor.h:595-614`). Once every
  replica has been tried, each further attempt waits a backoff that starts at 10 ms and
  doubles per attempt up to 5 s (`:301-308`, `:721-725`). When every replica is
  excluded, the request sends nothing and waits `allAlternativesFailedDelay` (50 ms,
  growing while failures continue), then reports 1006, unless one of its own replicas
  recovers first, which ends the wait as `okFuture` does (another address recovering
  does not); expiry only permits a probe, while recovery requires the address to
  leave the failed set. The attempt count restarts and the backoff is kept (`:641`). While it
  waits, a replica whose exclusion window ends is dialed, as `connectionKeeper`
  re-probes a failed peer (`FlowTransport.actor.cpp:784-797`); a dial that connects
  marks it alive. The wait cancels its probe waiter on recovery or expiry, without
  cancelling the database-owned shared dial; reconnecting never delays the selected
  recovery or error (`LoadBalance.actor.cpp:48-54`). A location with no servers reports 1006 after the
  all-alternatives-failed delay, so the caller re-locates. This delay also ends when
  the database closes, independently of the request context.
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
the dial-panic backstop only on a panic. A completed, undrained dial cohort referring
to the removed connection is detached under the same lock, so a new sender cannot
acquire the failed connection between the unlock and the close. `Database.Close` can
return while such a connection is still closing; its loops end with the close.

## 4. `TransactCtx` and a dispatched commit: kept

In libfdb_c the transaction ends the wait for a dispatched commit: RYW `commit` waits on `resetPromise.getFuture() || tr.commit()`
(`ReadYourWrites.actor.cpp:1381`, `:1405`), so a timeout or `cancel` returns
`transaction_timed_out` or `transaction_cancelled` at once. The native commit is held by
`Transaction::committing` (`NativeAPI.actor.cpp:6942-6945`) and runs on; only `reset`
or destruction drops it (`:6172`, `:5486`). Go does the same: timeout and `Cancel`
end the public wait but leave the native commit running; `Reset` ends the public
wait and drops the native commit
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
  - `TestHandleConnErrorClosesOutsideConnMu`: the failed connection's completed dial
    cohort is detached before failure publication; `Close` blocks until released,
    while `connMu` is free, the pool empty and the address failed.
  - `TestMetricsRecoveryDoesNotWaitForOtherProbe`: a recovered replica gets the
    request while a different replica's dial remains stalled and database-owned.
  - `TestMetricsRequestWakesOnRecovery`: with the storm delay at 30 s, a request whose
    only replica failed parks (a test hook reports it and keeps the replica excluded,
    so no probe dials it) and stays parked, without restarting its wait, when another
    address recovers; it reaches its replica within 5 s of that replica's recovery.
  - `TestMetricsRequestSkipsEndpointNotFound`: a replica whose metrics endpoint was
    reported not found before the request, or by an ENDPOINT_NOT_FOUND packet while it
    waits, gets no further request, and the call reports 1006.
  - `TestMetricsRequestBrokenPromiseTriesNextReplica`: an in-band `broken_promise`
    from the first replica marks its endpoint not found, and the second replica's reply
    completes the call.
  - `TestMetricsRequestSkipsExcludedReplica`: an excluded replica is never dialed: the
    available one gets the first attempt, answers `request_maybe_delivered`, and gets
    the second too, whichever replica the random start picked.
  - `TestMetricsRequestProbesExcludedReplica`: with the storm delay at 30 s and no other
    traffic, a request whose replica failed reaches it once its exclusion window ends.
  - `TestMetricsRequestBacksOffPerRequest`: with two replicas answering
    `request_maybe_delivered`, the third request waits at least 10 ms, the fourth 20 ms,
    and so on.
  - `TestMetricsRequestWithoutServers`: an empty replica list reports 1006.
  - `TestMetricsRequestAllFailedDelay`: empty and endpoint-exhausted lists enter the
    all-failed delay on both senders, remain waiting, and honor request cancellation.
  - `TestMetricsRequestDatabaseCloseInterruptsWait` and
    `TestMetricsRequestDatabaseCloseInterruptsAllFailedDelay`: closing only the database
    lifetime interrupts backoff, all-excluded, empty-list and exhausted-endpoint waits
    while the request context stays live.
  - `TestMetricsShardsFailFast`: both public metrics operations dispatch two shards
    before either replies; a terminal error on the second ends the call with the first
    reply withheld, cancels its registration and leaves the connection open.
  - `TestMetricsShardsPreserveOrder`: reversed shard replies sum the size and assemble
    split points in shard order, including the internal boundary.
  - `TestMetricsShardRetryKeepsSibling`: wrong-shard, all-alternatives-failed and
    future-version retries retain the sibling's original waiter and cached location;
    its terminal error cancels the first shard's withheld relocation or retry reply.
  - `TestMetricsShardRelocationSplitsWorker`: relocation splits a failed worker into
    two child shards, clamps replacement outer bounds to the worker's range, retains
    the original sibling, and sums literal child/sibling replies to 57 bytes.
  - `TestMetricsRecoveryIgnoresUnrelatedAfterExclusionExpires`: an expired exclusion
    and stalled probe do not make an unrelated recovery count as the request's own;
    an observed actual-live transition ends the wait without cancelling the shared dial.
    Recovery tests use a controlled 30 s delay, not production timers or storm-clock seeds.
  - `TestMetricsRequestWaitsForSlowReply`: a reply that comes after more than the old
    5 s timeout is used, with one request sent.
- `client`, test-full (`client_test`, FDB in a container, replies rewritten by the
  `simDialer`):
  - `TestPipelinedGetReplyTimeoutSends`: a read whose replies are all lost fails with
    `transaction_too_old` after 11 sends, a pipelined Get after 12, as STATUS.md's
    "Per-RPC 5 s read timeouts" paragraph now says.
  - `TestMetricsRetryWrongShardWithoutCap`: 55 `WaitMetrics` replies, then 55
    `SplitRange` replies, are `wrong_shard_server`; both calls succeed.
  - `TestMetricsRetryIsPaced`: every `WaitMetrics`, then every `SplitRange`, reply
    severs its connection; with a 2 s transaction timeout each call makes at most 30
    rounds and fails with `transaction_timed_out`.
  - `TestMappedSelectorReadsCapReplyTimeouts`: every `GetMappedKeyValues` reply is
    dropped, with a 20 ms reply timeout; both loops and a public positive-skip read
    send 11 mapped requests and return `transaction_too_old`.

Compiled mutation results for the behavior-changing guards are recorded in the PR,
alongside the preserved earlier red-to-green and repeated-race evidence.

## Compatibility

No bytes change. A size estimate or split-point request no longer fails with 1006
after 50 rounds, and no longer gives up on a replica after 5 s; it waits like
libfdb_c, bounded by the context and the transaction timeout. A positive-skip mapped read that keeps timing out returns a retryable
`transaction_too_old` instead of looping.
