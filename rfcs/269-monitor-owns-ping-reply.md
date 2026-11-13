# RFC-269: The connection monitor owns its ping reply

**Status:** Implemented
**Item:** TODO.md §0, "Join monitor reply waiters on connection close."
**Spec:** FoundationDB 7.3.77 (`/tmp/fdbsrc`); Go line numbers at `54566f2a4`.

## Problem

`connectionMonitor` (`pkg/fdbgo/transport/conn.go:1275`) sends each PING through
`sendPingWithReply` (`conn.go:1391-1435`). That function registers a reply with
`PrepareReply`, enqueues the PING, then starts a goroutine that waits on the reply channel
or `c.ctx.Done()`, cancels and releases the `ReplyHandle`, and closes a `done` channel the
monitor selects on. The goroutine is not in `Conn.loopWG` (`conn.go:170`, `:574-581`).

`Close` (`conn.go:974-983`) runs `failConnection` and then `loopWG.Wait()`. The cancelled
context wakes the monitor and the waiter together. The monitor returns and calls
`loopWG.Done()`; nothing waits for the waiter, so `Close` can return while the waiter still
holds the handle and has not run `Cancel`/`Release`. The `Close` doc promises "After Close
returns, zero goroutines are running on this connection" (`conn.go:965-966`), and the
`Conn` doc says the same (`conn.go:143`); the waiter breaks both. The same happens when the
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
4. When the monitor throws `connection_failed` on a frozen ping (`:674-680`), the
   generated catch path destroys the actor state before it publishes the error:
   `this->~State(); ...->sendErrorAndDelPromiseRef(error);`
   (`flow/actorcompiler/ActorCompiler.cs:387-391`). The ping reply is therefore released
   before `connectionKeeper` sees the failure and tears the connection down
   (`FlowTransport.actor.cpp:889-897`, `conn->close()` at `:989`). Nothing that owns the ping reply outlives the
   monitor, on any exit.

## Fix

Port the C++ ownership: the monitor goroutine owns the ping reply, and no goroutine is
started for it.

- `sendPingWithReply` returns `(<-chan Response, *ReplyHandle)`: the registered reply
  channel and its handle, or `(nil, nil)` when the PING could not be enqueued. The drop
  path keeps its current behavior: it cancels and releases the registration and returns
  a nil channel, so the monitor's reply arm never fires (RFC-052).
- The monitor's PING wait (the inner loop at `conn.go:1338-1384`) moves into a helper,
  `monitorPing`, which returns one of three outcomes: continue monitoring, stop (context
  cancelled), or dead (frozen bytes). When the handle is non-nil, the helper defers its
  cleanup, `releasePing`: `TakeReadyOrCancel` then `Release` if no reply was received,
  `Release` alone if one was. This is the cleanup the waiter does now
  (`conn.go:1419-1430`), run on the monitor's own stack. `TakeReadyOrCancel`
  (`conn.go:74-92`) takes a reply published after the wait ended, so the channel is
  pooled on every exit: when `Close` cancels the context before `failAllPending`
  publishes the teardown response, the ctx arm can win, and a plain `Cancel` would find
  the token gone and leave the channel to GC. A dropped PING has a nil handle, so no defer is installed and nil is never
  dereferenced; the re-ping exit, reachable only after a drop, has nothing to release.
  With a handle, the defer runs on every exit: reply, context cancellation, frozen
  bytes, and a panic.
- On the dead outcome, `connectionMonitor` calls `failConnection(errConnClosed)` after
  `monitorPing` returns, so the ping reply is released before teardown, as in C++ step 4.
  Today `failConnection` runs first, inside the wait loop (`conn.go:1351-1359`). A panic
  takes the same order: `monitorPing`'s defer runs during the unwind, before
  `recoverLoop` calls `failConnection` (`conn.go:1034-1040`). `connectionMonitor` calls
  `loopWG.Done()` last, so `Close`'s `loopWG.Wait()` returns only after the ping reply is
  released.
- A teardown response delivered by `failAllPending` counts as a received reply, as it
  does now: the monitor's next step sees the cancelled context and exits.
- Comments: the `loopWG` field comment (`conn.go:170`) and the `Close` doc
  (`conn.go:964-973`) name every joined goroutine: readLoop, writeLoop, the connection
  monitor (which owns the ping reply) and the cold-control loop. The `sendPingWithReply`
  doc and its drop-path comment (`conn.go:1388-1411`) describe the (channel, handle)
  return instead of the removed waiter.

The order of operations is unchanged: the PING frame, the reply token on the wire, the
timeout and `bytesReceived` checks, and the drop/re-ping fallback.

## Wire-compat impact

None. The PING body (`BuildPingRequest`), its reply token, frame order and timing do not
change. No error code changes. This only changes which goroutine releases the local reply
registration, and when. The frozen-bytes kill still delivers the same coded teardown to
every other pending reply; only the ping's own registration is removed first.

## Performance

Removes one goroutine and one `done` channel allocation per sent PING (at most one per
`monitorLoopInterval` per connection with pending work). The reply wait is now a direct
select on the reply channel. Nothing on the request path changes.

## Executable spec

Tests 1-5 drive `connectionMonitor` itself, started under `loopWG` on a `net.Pipe`
connection with one dummy pending request so it pings; the test plays the writer by
reading the PING from `writeCh`. The tests swap the package `putReplyChannel` seam to
observe the ping channel's release, so they run serially, like `reply_pool_test.go`. A
test context records the ping's reply channel from the monitor's own wait. On every
path below, the release is a `putReplyChannel` call on that channel: `Release` after a
received or taken reply, or a cancel while the token is still registered. Tests 1-5
fail on the reverted code; test 6 fails when `releasePing` uses a plain `Cancel`.

1. **Close waits for the reply owner.** The test delivers the PING's reply. The seam
   blocks that channel's release until a gate opens, and `Close` runs in a goroutine
   meanwhile. The test asserts that `Close` has not returned when the gate opens, and
   that it returns afterwards. Reverted, the release runs on the unjoined waiter, so
   `Close` returns while it is blocked. The gate opens after a bounded wait, which only
   limits how fast the reverted code is caught; the fixed code cannot fail it, because
   the blocked release is on the monitor, before `loopWG.Done()`.
2. **Close waits for an outstanding PING's owner.** No reply: `Close`'s own teardown
   ends the wait. The test context holds the monitor inside its wait's `Done` call
   (after recording, before the select picks an arm) until `Close`'s `failAllPending`
   has published the teardown response and removed the token. The select then takes
   either arm: the reply arm, or the ctx arm followed by `TakeReadyOrCancel` taking the
   response. Both release through `Release` without holding `pendingShard(token).mu`, so a
   `Close` still running can only be waiting on the monitor's join. The release is gated as
   in test 1; the test asserts that `Close` has not returned when the gate opens, returns
   afterwards, and leaves no registration. Reverted, the waiter releases after `Close`
   returns, or its `Cancel` finds the token already removed and never reaches the seam.
3. **Cancellation releases on the monitor.** With the PING outstanding and no reply, the
   test cancels the connection context directly (not `Close`, so `failAllPending` leaves
   the token registered and the cancel must pool the channel). The seam blocks that
   release behind a gate. As in test 1, the gate stays closed for a bounded wait for the
   monitor-exit signal; the test asserts that `connectionMonitor` has not returned when
   the gate opens. The cancel holds `pendingShard(token).mu` inside the seam, so nothing in
   the test takes that shard lock until the gate is open. Afterwards it asserts that the
   ping token is no longer registered. Reverted, the waiter's `Cancel` blocks and the
   monitor returns within the wait anyway.
4. **A frozen ping releases before teardown.** No reply and no traffic, so the timeout
   fires. When the seam sees the ping channel's release, it records `IsClosed()`. The
   test asserts that the release happened and that the connection was still open at
   that moment, then that the monitor closed it (`Done()` fires). Reverted,
   `failConnection` runs first and removes the token, so the release comes later (on
   the waiter's receive path) or not at all; either way the test fails, without any
   timing window.
5. **A panic after the handoff releases before teardown.** The test context panics in
   the monitor's wait after the ping is registered, inside `monitorPing`. The test makes
   the same assertions as test 4, then checks that `recoverLoop` closed the connection.
   Reverted, `recoverLoop` tears down while the waiter still holds the reply, so the
   release comes after `IsClosed()` or not at all; if the panic lands on the unjoined
   waiter instead, it is unrecovered and fails the test binary.
6. **`releasePing` pools a published reply.** A unit test registers a reply, publishes
   and deletes it as `failAllPending` does, calls `releasePing(h, false)`, and requires
   the channel pooled exactly once and no registration left; a second case does the
   same with the token still registered. With a plain `Cancel`, the published case
   pools nothing.
7. Existing pins are adapted to the new return shape:
   `TestSendPingWithReply_DropsToNilOnFullWriteCh` and
   `TestAdmissionPingDropsBehindPendingHead` (a nil channel, a nil handle, and no
   registration on drop), `TestHeldOutboxControlPrefixAndPostSealReply` (it receives on
   the returned channel and releases the returned handle as the monitor would), and
   `TestMonitor_DroppedPingRePingsInsteadOfStalling` plus the monitor-death tests
   (unchanged; they drive `connectionMonitor`). `TestPingWaiterCancelsOwnedReply` is
   replaced by test 3, which drives the monitor rather than the handle.
   `TestPingRegistrationPanicReleasesReply` keeps its first half, a panic during
   registration that leaves no registration. Its second half (the same connection can
   ping again and the reply completes) receives on the returned channel and releases
   the handle.
8. Validation: `just test` (the fast lane, with nogo and every dependent target) and
   `just test-race` (the same set under `-race`), reporting executed versus cached
   targets.

## Out of scope

- `peer->resetPing` (`FlowTransport.actor.cpp:696-698`) has no Go counterpart; TODO.md
  §0 tracks it.
- The cold-control outbox worker is already in `loopWG` (`conn.go:575-578`).
- The idle-connection divergence documented in `connectionMonitor` is unchanged.
