# RFC-052: Monitor ping on a saturated writeCh must fall through to the liveness check

**Status:** Implemented (FDB C++ dev ACK, Torvalds ACK) — landed on `fix/client-14-monitor-ping-drop`
**Item:** RFC-010 audit #14 (LOW)
**Scope:** `pkg/fdbgo/transport` connection monitor

## Problem

The connection monitor's ping send is already non-blocking (`sendPingWithReply`
uses `select { case c.writeCh <- …: default: … }`), so the original "monitor ping
can block on a full writeCh" phrasing is stale. The **real** bug — what RFC-010 #14
actually calls for — is the *drop-path behavior*:

```go
// sendPingWithReply, drop path (writeCh full):
default:
    replyHandle.Cancel()
    replyHandle.Release()
    close(done)        // <-- returns a CLOSED channel
    return done
```

The monitor's inner loop treats that returned channel as the ping reply:

```go
replyCh := c.sendPingWithReply()
select {
case <-replyCh:   // a CLOSED done fires IMMEDIATELY → "PING reply arrived — alive"
    ...
case <-timer.C:   // bytesReceived liveness check — SKIPPED
case <-c.ctx.Done():
}
```

So when `writeCh` is **saturated**, the monitor reads the closed `done` as "reply
arrived → connection alive" and loops — **without ever running the `bytesReceived`
liveness check.** But a saturated `writeCh` is the signature of a *stuck*
connection: `writeLoop` is blocked flushing to a socket the peer isn't draining,
so the buffer backs up. Exactly when the connection is most likely dead, the
monitor concludes it's healthy and never kills it — it then leaks until the 10 s
TCP keepalive trips (and on transports without keepalive, never). Severity LOW
because keepalive is a backstop and saturation is uncommon, but it defeats the
monitor's whole purpose in the one state that matters.

## Investigation

- `sendPingWithReply` (`conn.go:769`) — drop path closes `done`.
- `connectionMonitor` inner loop (`conn.go:733-761`) — `case <-replyCh:` is the
  "alive" branch; `case <-timer.C:` is the only path to the `bytesReceived` kill.
- RFC-010 #14 prescription: *"On full `writeCh`, fall through to the bytes-received
  liveness check instead of short-circuiting via a closed `done`. Test with a
  saturated `writeCh`."* Confirmed — the fix is the drop-path return value, not the
  blocking (which is already handled).
- The "sent" path is unaffected and already correct: `TestConn_MonitorDeathClosesSocket`
  (RFC-050) proves a frozen-bytes connection whose ping IS sent gets killed.

## Fix

On the drop path, **return `nil`** instead of a closed channel:

```go
default:
    // writeCh is saturated — we could not send the PING. Return a nil channel,
    // NOT a closed one: a closed channel makes the monitor's `case <-replyCh`
    // fire immediately and falsely conclude "alive", skipping the liveness check
    // exactly when a saturated buffer signals a stuck connection. A nil channel
    // is never selected, so the monitor falls through to the timer →
    // bytesReceived check, which kills a genuinely stuck conn.
    replyHandle.Cancel()
    replyHandle.Release()
    return nil
```

`<-nil` in a `select` is never ready (guaranteed Go semantics), so the monitor's
`case <-replyCh` (with `replyCh == nil`) is dead and it waits on `timer.C` →
`bytesReceived`. The ping pending entry is still cancelled/released on drop (no
leak). One-line behavioral change; no API change.

### Follow-up: re-ping after a dropped ping (codex P2)

A `nil` `replyCh` removes the inner loop's only success path. The inner loop can
then only **kill** (frozen bytes) or **continue** (traffic observed) — it returns
to the outer loop (which re-pings) *exclusively* via the now-dead `<-replyCh`
branch. So a connection whose PING was dropped would **never ping again**: it
survives only on incidental traffic and dies on the first idle window, failing
slow-but-live requests. C++ never hits this — `Peer::send` is unbounded, so the
ping always goes out; this is purely a bounded-`writeCh` artifact.

Fix: in the `timer.C` branch, after confirming bytes advanced (not frozen), if
`replyCh == nil` **break to the outer loop** to re-attempt the PING next cycle (by
which point `writeCh` has likely drained), instead of `continue`-ing the inner
loop forever:

```go
startingBytes = current
if replyCh == nil {
    break pingWait // re-attempt PING next outer cycle; don't wait for an impossible reply
}
continue
```

## Performance

None. The drop path runs only when `writeCh` is already full (a rare, degenerate
state), and it now does strictly less (no `close`). The common sent-ping path is
untouched.

## Test plan

- **`TestSendPingWithReply_DropsToNilOnFullWriteCh`** (unit, deterministic): a
  minimal `Conn` with a saturated 1-slot `writeCh` (no `writeLoop` draining it);
  assert `sendPingWithReply()` returns **nil** (not a closed channel) and leaves
  **no** pending reply registered. **Fails on the pre-fix code** (which returns a
  closed non-nil `done`). Combined with Go's guaranteed nil-channel `select`
  semantics — a `nil` `replyCh` is never selected, so the monitor must take the
  `timer.C` → `bytesReceived` branch — this pins the corrected liveness behavior.
- **`TestMonitor_DroppedPingRePingsInsteadOfStalling`** (unit, deterministic):
  pins the codex P2. The single `writeCh` slot is held by a dummy so every PING
  attempt drops; steady `bytesReceived` traffic keeps the liveness check from
  firing; draining the dummy then lets the next attempt enqueue a real PING — which
  appears only if the monitor is still cycling the outer loop. **Fails on the
  pre-fix (continue-only) code** ("monitor never re-pinged … inner loop wedged").
- The sent-ping kill path remains covered by `TestConn_MonitorDeathClosesSocket`.
- `just test` (48 targets) green, `-race`.
