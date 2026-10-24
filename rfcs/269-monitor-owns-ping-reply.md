# RFC-269: The connection monitor owns its ping reply

**Status:** Draft
**Item:** TODO.md §0, "Join monitor reply waiters on connection close."
**Spec:** FoundationDB 7.3.77 (`/tmp/fdbsrc`); Go line numbers at `54566f2a4`.

## Problem

`connectionMonitor` (`pkg/fdbgo/transport/conn.go:1275`) sends each PING through
`sendPingWithReply` (`conn.go:1391-1432`). That function registers a reply with
`PrepareReply`, enqueues the PING, then starts a goroutine that waits on the reply channel
or `c.ctx.Done()`, cancels and releases the `ReplyHandle`, and closes a `done` channel the
monitor selects on. The goroutine is not in `Conn.loopWG` (`conn.go:170`, `:574-581`).

`Close` (`conn.go:974-983`) runs `failConnection` and then `loopWG.Wait()`. The cancelled
context wakes the monitor and the waiter together. The monitor returns and calls
`loopWG.Done()`; nothing waits for the waiter, so `Close` can return while the waiter still
holds the handle and has not run `Cancel`/`Release`. The `Conn` and `Close` doc comments
promise "After Close returns, zero goroutines are running on this connection"
(`conn.go:143`, `:965-966`); the waiter breaks that promise. The same happens when the
monitor itself kills the connection on a frozen ping (`conn.go:1351-1359`): it returns
while the waiter is still running.

## C++ behavior

1. `connectionMonitor` holds the ping's reply promise as actor state:
   `state PingRequest pingRequest;` (`fdbrpc/FlowTransport.actor.cpp:665`). It waits on
   `pingRequest.reply.getFuture()` in the same `choose` as the timeout
   (`:670-699`, reply arm `:690-695`). The client starts no separate waiter for the
   reply.
2. `PingRequest::reply` is a `ReplyPromise<Void>` (`FlowTransport.actor.cpp:238-246`)
   backed by a `NetSAV` (`fdbrpc/include/fdbrpc/fdbrpc.h:102-125`, `:147`). When the last
   reference is dropped, `~FlowReceiver` removes the local reply endpoint
   (`fdbrpc.h:48-54`).
3. `connectionKeeper` waits on `connectionWriter || reader || connectionMonitor(self) ||
   resetConnection` (`FlowTransport.actor.cpp:882-883`). When any arm ends, the rest are
   cancelled. Cancelling the monitor actor destroys its state, including
   `pingRequest`, so the ping reply endpoint is removed during cancellation itself.
   Nothing that owns the ping reply outlives the monitor.

## Fix

Port the C++ ownership: the monitor goroutine owns the ping reply, and no goroutine is
started for it.

- `sendPingWithReply` returns `(<-chan Response, *ReplyHandle)`: the registered reply
  channel and its handle, or `(nil, nil)` when the PING could not be enqueued. The drop
  path keeps its current behavior: it cancels and releases the registration and returns
  a nil channel, so the monitor's reply arm never fires (RFC-052).
- The monitor's PING wait (the inner loop at `conn.go:1338-1384`) moves into a helper,
  `monitorPing`, which owns the handle through a deferred release. If the reply was not
  received, the defer calls `Cancel` then `Release`; if it was received, only `Release`.
  This is the cleanup the waiter does now (`conn.go:1419-1430`), run on the monitor's own
  stack. The helper returns whether the monitor continues. All exits (reply, re-ping
  after a dropped PING, frozen-bytes kill, context cancellation, panic) run the defer
  before `connectionMonitor` returns and calls `loopWG.Done()`. `Close`'s
  `loopWG.Wait()` therefore returns only after the ping reply is released.
- A teardown response delivered by `failAllPending` counts as a received reply, as it
  does now: the monitor's next step sees the cancelled context and exits.

The order of operations is unchanged: the PING frame, the reply token on the wire, the
timeout and `bytesReceived` checks, and the drop/re-ping fallback.

## Wire-compat impact

None. The PING body (`BuildPingRequest`), its reply token, frame order and timing do not
change. No error code changes. This only changes which goroutine releases the local reply
registration, and when.

## Performance

Removes one goroutine and one `done` channel allocation per sent PING (at most one per
`monitorLoopInterval` per connection with pending work). The reply wait is now a direct
select on the reply channel. Nothing on the request path changes.

## Executable spec

1. **Close waits for the ping owner (new, revert-proven).** A serial test (it swaps the
   package `putReplyChannel` seam, as `reply_pool_test.go` does) runs `connectionMonitor`
   under `loopWG` on a `net.Pipe` connection with one pending request. The test takes the
   PING from `writeCh` and delivers its reply. The hook blocks the release of that reply's
   channel until the test closes a gate. While the release is blocked, `Close` runs in a
   goroutine. The test asserts that `Close` has not returned when the gate opens, and that
   it returns after. On the current code the release runs on the unjoined waiter, so
   `Close` returns while the release is blocked and the test fails. The fixed code
   always passes, because the release runs on the monitor before `loopWG.Done()`. The
   gate opens after a bounded wait. That wait only limits how quickly the reverted code
   is caught; it cannot make the correct code fail.
2. **Cancellation releases the registration before the monitor exits.** With the PING
   outstanding and no reply, the test cancels the connection context (not `Close`, so
   `failAllPending` does not remove the token), waits on `loopWG`, then asserts that the
   ping token is no longer pending. Today the waiter removes it after the join, so this
   check can fail; after the fix it always holds.
3. Existing pins are adapted to the new return shape and keep their assertions:
   `TestSendPingWithReply_DropsToNilOnFullWriteCh` (a nil channel and no registration on
   drop), `TestPingWaiterCancelsOwnedReply` (renamed: cancelling the owner removes the
   registration), `TestPingRegistrationPanicReleasesReply`,
   `TestHeldOutboxControlPrefixAndPostSealReply`, `TestAdmissionPingDropsBehindPendingHead`,
   `TestMonitor_DroppedPingRePingsInsteadOfStalling` and the monitor-death tests.
4. `-race` on `//pkg/fdbgo/transport:transport_test` with the new tests repeated
   (`--runs_per_test=10`).

## Out of scope

- `peer->resetPing` (`FlowTransport.actor.cpp:696-698`) has no Go counterpart; that is a
  separate gap.
- The cold-control outbox worker is already in `loopWG` (`conn.go:575-578`).
- The idle-connection divergence documented in `connectionMonitor` is unchanged.
