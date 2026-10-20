> **STATUS: REJECTED (owner decision).** Diverges structurally from C++ `connectionReader` (one reader owns the socket); its cancellation/failure paths would have no C++ reference to check against — the class of subtle wedge bugs just fixed. A 6–9% read-CPU gain does not justify it. No transport-structure changes: readLoop-per-connection stays as the `connectionReader` analogue.

# RFC (draft, post-launch): reply delivery without a reader handoff

Status: draft for after launch. Owner: perf-lead. Scope: `pkg/fdbgo/transport` (Conn read path). No wire change.

## Problem

After the R16 stack, the pure-Go client beats libfdb_c on CPU for batched writes. Reads are only at parity: read1000 CPU is 0.908× CGo [0.810, 1.019], and user CPU 0.776×, but system CPU is 1.006×.

| per read transaction (traced 200-op window, R16) | Go | libfdb_c |
|---|---:|---:|
| futex | 9.61 | 6.61 |
| nanosleep | 5.31 | 7.35 |
| epoll (Go `epoll_pwait`, CGo `epoll_wait`+`epoll_ctl`) | 3.99 | 16.8 |
| read / recvfrom | 4 | 4.01 |
| write / sendmsg | 2 | 2.0 |

Go now makes fewer syscalls in total. The one category where it makes more is futex: +3 per transaction.

Those futex calls come from the goroutine handoff on every reply. `readLoop` reads a frame and sends it on the waiter's reply channel. The runtime's `ready()` puts the waiter in the `runnext` slot of readLoop's P and then calls `wakep()` unconditionally. `wakep()` wakes an idle M (futex wake); that M spins, `usleep(3)`s in `runqgrab` while trying to steal `runnext` from a running P (the `nanosleep`), finds nothing to steal, and goes back to sleep (futex wait). Meanwhile readLoop parks on its next read, and its own P runs the waiter.

So each RPC pays one useless M wake/spin/sleep cycle. On a VM, where an idle vCPU wake-up costs a VM exit, that is expensive.

## The libfdb_c model, for comparison

flow runs one network thread: `Net2::run` (flow/Net2.actor.cpp:1506). Each iteration:
1. `reactor.sleep` (epoll) until I/O or a timer is ready.
2. `reactor.react`.
3. Drain the ready task queue.

`connectionReader` is an actor on that thread. When it delivers a reply it fulfils the waiting actor's promise, and that actor runs on the same thread, in the same loop iteration. Reply delivery involves no cross-thread handoff at all.

The application thread (the Go caller in CGo) costs one cross-thread signal per request and one per reply:
- `onMainThread` and `Net2::onMainThread` `reactor.wake()` the network thread to send a request.
- `ThreadSingleAssignmentVar` / `BlockUntilReady` wake the caller when the reply arrives.

That is the 6.6 futex/tx above. It is also why libfdb_c needs epoll_wait 14×/tx: one thread multiplexes everything.

## Proposal: the waiter reads its own reply (leader/follower)

Make "the reader" a role, not a goroutine:
- The role is a mutex-protected reader state: the bufio reader, the FrameReader's partial-frame state, and the pending map.
- A goroutine that is about to wait for a reply tries to take the role (`TryLock`).
- While it holds the role, it reads and dispatches frames itself, exactly as readLoop does today:
  - pings are answered;
  - other waiters' replies go to their channels, which is the same handoff as today;
  - its own reply is returned directly, with no channel and no wake.
- When its reply arrives, it releases the role.
- `readLoop` stays. It takes the role whenever nobody else holds it, so unsolicited frames, pings and replies for waiters that are not leading are still read promptly.

For a connection with one RPC in flight (the common case for a single transaction) this removes the reply handoff entirely. With many RPCs in flight it degrades to today's behaviour.

### Handing the role over

- **Blocking.** A leader blocks in the socket read through the netpoller, just as readLoop does. Being woken by netpoll costs the same as readLoop being woken; the saving is the second wake, from readLoop to the waiter.
- **Releasing.** The leader releases the role after it has dispatched the frame carrying its own reply. Any bytes already buffered stay in the shared reader state for the next holder.
- **Readiness of the next holder.** `readLoop` waits for the role. Releasing must hand the role to readLoop without a missed wake-up. Use a role channel of capacity 1 holding a token: whoever holds the token reads, and releasing puts the token back.
  - This trades one wake (readLoop taking the token back) for the one removed. That only pays if readLoop does not have to wake each time.
  - Hence: readLoop takes the token back only when a frame is likely pending, or after a short idle check. This is the design point that needs measurement before committing to it.

### Correctness

**Cancellation (caller ctx, transaction timeout).** A leader blocked in `Read` cannot observe its ctx directly. Two ways to handle this:
1. Set a read deadline equal to the ctx deadline, plus a watcher that sets the deadline to now on cancellation, as the handshake does today.
2. Read non-blocking through `RawConn.Read`, waiting on `rawConn.Read`'s netpoll wait together with ctx. This is the mirror of #827's non-blocking write.

On cancellation the leader must return the role with the partial-frame state intact. A deadline error is therefore not a connection error: the FrameReader resumes mid-frame for the next holder. The current `FrameReader` is not resumable mid-frame; it uses `io.ReadFull` into a fresh payload. This needs an explicit state machine (header / checksum / payload offset), and that is the riskiest part of the change.

**Multiplexed replies.** The leader dispatches every frame through the same `pending` map under `pendingMu`, with the same delete-then-publish discipline as readLoop. A reply for another waiter is delivered to that waiter's channel. A late reply after cancellation is dropped, as today. The leader recognises its own reply by token identity, not by order.

**Connection failure.** A read error, EOF or checksum failure seen by a leader goes through the same `failConnection` path (cancel, close, `failAllPending`). The leader then returns its own coded 1030 error, as the reply channel would have delivered it. A leader that panics releases the role through a deferred recovery, as `recoverLoop` does.

**Liveness monitor.** `bytesReceived` is counted in the shared reader through `socketCounter` (#834), whoever reads. The connection monitor is unchanged.

**Ordering.** One reader at a time, enforced by the role. Frame order on the socket is preserved.

**TLS.** It works with `tls.Conn` for option 1. Option 2 (RawConn) is plaintext-only, like #827. TLS keeps readLoop-only reading.

## Alternatives considered

- **Lower GOMAXPROCS.** This is the user's setting, not the library's. Fewer idle Ps would mean fewer `wakep` wakes. The R18 diagnostic measures the size of this effect for understanding only.
- **Spin-waiting for the reply before parking.** Trades futex for user CPU and is no win on CPU.
- **Avoiding `wakep` in the handoff.** `wakep` is unconditional in `runtime.ready`. Every waiter today is already parked on a channel `select`, so the receiver already lands in `runnext`. No user-level API avoids the wake.
- **A single network goroutine, like flow.** Every request would hand off to it, which is more handoffs, not fewer.

## Expected gain

Up to one futex wake/sleep pair, plus the spinning `nanosleep`, per RPC. That is about 3 futex and 2-3 nanosleep per read transaction, roughly 6-9% of read CPU on the VM. It needs the R18 GOMAXPROCS diagnostic to bound it.

## Test plan

- Unit:
  - a leader reads its own reply with no channel handoff (assert via a counter);
  - a leader cancelled mid-frame hands a partial frame to readLoop, which completes it;
  - a leader that sees another waiter's reply delivers it;
  - a connection failure while leading fails all pending;
  - a panic while leading is contained.
- Race and stress: many concurrent waiters on one connection, with random cancellation, under -race.
- Measure: V7 read1000 and batchset50 against CGo, with the syscall window.
