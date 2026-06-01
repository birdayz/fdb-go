# RFC-050: Connection shutdown — one `failConnection` path + SimTransport

**Status:** Draft
**Item:** RFC-010 audit #6 (HIGH)
**Scope:** `pkg/fdbgo/transport` connection lifecycle + an in-process fake server for deterministic tests

## Problem

`transport.Conn` has two connection-shutdown bugs (RFC-010 audit #6). Both are
**races/leaks** that only manifest on connection death, so they need a
*deterministic* in-process harness to test — hence the gating on "SimTransport".

**Bug 1 — `Close`/cancel strands a `SendFrame`/`Flush` caller.** `SendFrame`
enqueues a `writeReq{errCh}` into `c.writeCh`, then blocks on `err := <-errCh`
with **no** `ctx.Done()` escape (`conn.go:293`). `writeLoop` exits on
`<-c.ctx.Done()` (`conn.go:351`) **without draining `writeCh` or notifying the
queued `errCh`s**. So any frame still buffered in `writeCh` when `Close()` fires
strands its sender on `<-errCh` *forever* — `Close()` returns (it only
`loopWG.Wait()`s the three loops), but the external caller hangs. Same for
`Flush` (`conn.go:326`).

**Bug 2 — `connectionMonitor` declares the conn dead without closing the socket
or failing pending.** On a frozen-bytes timeout it calls only `c.cancel()` and
returns (`conn.go:670`). `cancel()` does **not** interrupt `readLoop`'s blocking
`fr.Read(c.conn, …)` — only closing the socket does. `readLoop`'s
`defer c.conn.Close()` can't run because `readLoop` is still blocked in `Read`.
Result: a **deadlock of cleanup** — the fd and the `readLoop` goroutine leak until
the 10 s TCP keepalive eventually trips the read, and pending replies aren't
failed until then. C++ `connectionMonitor` throwing `connection_failed` makes
`connectionKeeper` close the socket immediately.

There is **no in-process test harness** for `Conn` lifecycle today: every existing
transport test is frame/parser-level, and the client's `wrongShardConn`
(`fault_test.go`) is a MITM proxy over a *real* FDB container — it can't drive a
deterministic Close-vs-SendFrame race or a monitor timeout without Docker and
wall-clock waits.

## Investigation

- `Close()` (`conn.go:421`) does `cancel()` → `conn.Close()` → `loopWG.Wait()`.
  It correctly unblocks `readLoop` (socket close) and `writeLoop` (ctx). The gap
  is purely the *external* `SendFrame`/`Flush` callers blocked on `errCh`, which
  no path notifies.
- The client treats **any** non-nil transport error from `Send`/`SendFrame`/
  `SendAndWait` as a connection failure → `handleConnError` + retry
  (`broken_promise → request_maybe_delivered`). So `failConnection`'s delivered
  error only needs to be **non-nil**; we don't need a new error code.
- C++ model (`FlowTransport` `connectionKeeper`): a single close path closes the
  socket, sends `connection_failed` to all outstanding reply promises, and fails
  queued unsent writes. We mirror that with one `failConnection(err)`.
- The `errChanPool` stale-value hazard (audit #13): if a `SendFrame` abandons
  `errCh` via a new `ctx.Done()` case while `writeLoop` later sends to it, a
  pooled channel could carry a stale buffered value. Avoided by **not** returning
  `errCh` to the pool on the `ctx.Done()` path (leak that one channel — it's a
  terminal, once-per-dead-conn event).

## Fix

### 1. One `failConnection(err)` path
```go
var errConnClosed = errors.New("connection closed")

func (c *Conn) failConnection(err error) {
    c.closeOnce.Do(func() {
        c.cancel()            // signal ctx — unblocks SendFrame/Flush/SendAndWait selects
        _ = c.conn.Close()    // unblocks readLoop's blocking Read → no fd/goroutine leak
        c.failAllPending(err) // meaningful error to in-flight replies, exactly once
    })
}
```
`closeOnce sync.Once` ensures the cancel + socket-close + `failAllPending(err)`
trio runs **exactly once with the first caller's error**, regardless of how many
of the three callers fire. All three now route through it — there is one path:
- **`Close()`** → `failConnection(errConnClosed)` then `loopWG.Wait()`.
- **`connectionMonitor`** death → `failConnection(errConnClosed)` then return
  (this is the Bug 2 fix — adds the missing `conn.Close()`).
- **`readLoop`** read error → `failConnection(err)` then return. This **replaces**
  `readLoop`'s bare `defer c.cancel()` / `defer c.conn.Close()` / inline
  `failAllPending(err)`. Because `closeOnce` gates the trio, only the first of
  {monitor death, readLoop error, Close} delivers to the pending map; the others
  are no-ops. (Note: single-delivery to a given pending reply is guaranteed by the
  pending-map + `pendingMu` + delete-as-you-go in `failAllPending` itself — that
  invariant is unchanged and is what makes even a stray concurrent caller safe;
  `closeOnce` additionally guarantees the *meaningful* error is the one delivered,
  rather than a later "use of closed connection" read error.)

### 2. `SendFrame`/`Flush` escape on `ctx.Done()` (Bug 1)
Wait on `errCh` **or** `ctx.Done()`; on the `ctx.Done()` path return
`errConnClosed` and do **not** pool `errCh`:
```go
select {
case err := <-errCh:
    errChanPool.Put(errCh)
    return err
case <-c.ctx.Done():
    return errConnClosed // errCh deliberately NOT pooled (stale-value hazard, audit #13)
}
```
This is the **complete** Bug 1 fix: `failConnection`'s `cancel()` fires
`ctx.Done()`, unblocking every stranded sender — whether its frame is still in the
`writeCh` buffer or it enqueued after `writeLoop` already exited. No `writeLoop`
drain is added: with this arm, a drain would be redundant for the hang, and it
could only ever send to an `errCh` still resident in `writeCh` (never one already
consumed-and-pooled, since a sender pools only on the `errCh` path *after*
dequeue) — so it adds risk and complexity for zero correctness benefit. Cut it.

### 3. Make the monitor cadence injectable (test determinism)
Add unexported `monitorLoopInterval` / `monitorTimeout` `time.Duration` fields,
defaulted to the current `750ms` / `2s` inside the dial path **before** the
goroutines start. Wiring is an unexported functional option on an unexported
`dialWith(...opts)`; the public `DialWithTLS` calls it with no opts (signature
unchanged). Internal tests pass `withMonitorCadence(tiny, tiny)` so the
monitor-death test runs in tens of ms, not ~3.5 s. No production timing change.

### 4. In-process fake server for tests (net.Pipe, no new package)
A ~40-line test helper (internal `package transport` test file): `net.Pipe()` for
the conn pair, a goroutine that performs the server side of the ConnectPacket
handshake (read client's 44 bytes, write ours via the existing `ConnectPacket`
API), then runs in a controllable mode — respond to PINGs, go silent (drive the
monitor), stop reading (so the synchronous pipe blocks `SendFrame`), or close
abruptly (drive `readLoop`'s error path). The conn is fed to `dialWith` via a
`DialFunc` returning the client pipe end. **No** `transporttest` package — the
full seeded multi-mode SimTransport is speculative until C4 needs it (YAGNI);
build it then.

## Performance

- Hot path unchanged: the normal `SendFrame` still takes the `<-errCh` case; the
  added `select` arm costs nothing measurable. `failConnection`/`closeOnce` only
  run on connection death.
- Removes a leak (fd + goroutine held for up to 10 s after monitor-detected
  death) — a net resource improvement under churn.
- Monitor cadence fields are read once per loop; no change to production timing.

## Test plan

Deterministic, in-process, `-race`, no Docker (via the `net.Pipe` fake-server helper):
- **`TestConn_CloseUnblocksBlockedSendFrame`**: stall the server (stop reading) so
  a `SendFrame` blocks in `writeLoop`/`writeCh`; from another goroutine call
  `Close()`; assert the `SendFrame` returns `errConnClosed` within a deadline (it
  hangs forever on master). Same for `Flush`.
- **`TestConn_MonitorDeathClosesSocketAndFailsPending`**: server completes the
  handshake then goes silent; with a tiny monitor cadence, assert (a) a pending
  `SendAndWait` reply is failed, (b) `IsClosed()` becomes true, (c) `loopWG`
  drains (no leaked `readLoop`) within a deadline. On master the socket is never
  closed, so `readLoop` stays blocked past the deadline.
- **`TestConn_ServerAbruptCloseFailsPending`**: server closes mid-RPC; assert
  `failAllPending` fires and `Close()` is clean (regression for the readLoop path
  now routed through `failConnection`).
- **`TestConn_FailConnectionIdempotent`**: the **real** `readLoop`-error path (via
  abrupt server close) racing a monitor-triggered `failConnection` and an explicit
  `Close()` — i.e. the genuine three-way race, not a strawman calling
  `failConnection` directly. Assert no panic, exactly-once teardown, every pending
  reply delivered exactly once, clean `loopWG.Wait()`. `-race`.
- **`TestConn_NoErrChStalePoolValue`**: drive the Close-vs-`SendFrame` race in a
  tight loop; assert a `SendFrame` that observes its own `errCh` never receives a
  value belonging to a *different* call — pins that the `ctx.Done()` path must not
  return `errCh` to the pool (audit #13). (No drain exists, so there is no
  drain-side stale-pool path to cover.)
- Existing transport + client suites stay green (`just test`, 48 targets), `-race`.
