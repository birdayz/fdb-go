# Client networking: ground truths and design

Rounds 1–3 compared Go designs against each other. This document fixes what
any design must satisfy, proposes one design that follows from it, and defines
the benchmark that decides. Citations are to FoundationDB 7.3.77.

## Ground truths

### Layers

| Layer | Ground truth | Freedom |
|---|---|---|
| User API, idiomatic (`pkg/fdbgo/client`) | Blocking calls that take a `ctx`: `tx.Get(ctx, key) ([]byte, error)`. Concurrency comes from goroutines. | Owned by us; stays Go-idiomatic. |
| User API, compatibility (`pkg/fdbgo/fdb`) | The Apple cgo binding: futures, `Get`/`MustGet`, `Transact`, options, error codes. | None. |
| Semantics | libfdb_c (NativeAPI, RYW): retries, GRV batching, conflict ranges, timeouts, ordering. | None; C++ is the spec. |
| Cluster-visible behaviour | FlowTransport: the bytes, request/reply matching, when frames reach the socket. | Packet boundaries and timing only, held to the bar in T4. |
| Implementation | — | Free: goroutines, locks, atomics. Chosen by measurement. |

### What C++ does

- **C1. Sending is enqueue plus a future.** `Peer::send` appends the packet to
  `unsent` and triggers `dataToSend` (`fdbrpc/FlowTransport.actor.cpp:1038-1043`).
  `getReply` returns a `Future<Reply>` right away (`fdbrpc/fdbrpc.h:742`). The
  caller never learns when the bytes reached the socket. Errors come from the
  reply, or from the connection failing.
- **C2. One writer per connection, with coalescing.** `connectionWriter`
  (`FlowTransport.actor.cpp:704-738`) waits
  `max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now - lastWrite))`
  (10/20 µs, `flow/Knobs.cpp:211-212`). It then writes everything queued in
  chunks of up to `MAX_PACKET_SEND_BYTES` (128 KiB, `:225`) and sleeps until
  more data arrives.
- **C3. GRV batching adapts.** The window is
  `min(EWMA(0.5 × reply latency), GRV_BATCH_TIMEOUT = 5 ms)`, and a batch is
  sent early at `MAX_BATCH_SIZE = 1000` (`fdbclient/NativeAPI.actor.cpp:7351-7365`,
  `fdbclient/ClientKnobs.cpp:91-92`).
- **C4. Timeouts are per transaction and opt-in.** One `timebomb` actor runs
  only when the TIMEOUT option is set; the default is 0, meaning no timer
  (`fdbclient/ReadYourWrites.actor.cpp:1567-1578`). Reads have no per-request
  reply timeout (`loadBalance`).
- **C5. Cancelling is local.** Cancelling a future drops the wait. A request
  that was already queued is still sent.

### What the Go client must satisfy

- **T1. Requests are enqueue plus wait (C1).** The caller enqueues the frame and
  waits for the reply. There is no write acknowledgment and no handoff
  rendezvous with the writer. A write error fails the connection, and every
  pending reply on it fails.
- **T2. `ctx` is local (C5).** Cancelling a `ctx` or hitting its deadline ends
  the caller's wait. It never removes or alters a frame that is already queued.
- **T3. Timeouts cost nothing when unset (C4).** With no TIMEOUT option, the hot
  path arms no timer. With TIMEOUT set, there is one timer per transaction, not
  one per request.
- **T4. Batching is no worse than libfdb_c.** Under the same load, on the same
  host, against the same cluster:
  - frames per packet and server cycles per transaction are no worse than
    libfdb_c's;
  - p50 and p99 are no worse than libfdb_c's.

  A coalescing policy that differs from C2 is allowed only if it meets this bar
  and the deviation is documented.

  Judge T4 at **equal throughput**, where possible, not at equal workers. In a
  closed loop, a client that completes more operations per second puts more
  load on the servers. Load changes batching and per-transaction server cost on
  its own (round 2, `set100`). So compare arms rate-capped to the same ops/s
  (clientbench `-rate`), and use equal-worker cells as the closed-loop view.
- **T5. Wire bytes are identical.** Only packet boundaries and timing may differ.
- **T6. The hot path is optimized for a goroutine per operation.** With an
  idiomatic API, each operation is one goroutine blocked on its reply. The cost
  that matters is enqueueing a frame plus waking exactly one waiter when its
  reply arrives. Round 3 showed that wakeups dominate on a VM.

## Design

- **D1, writer (C2, T1).** One writer goroutine per connection.
  - Callers append frames to a mutex-guarded buffer. The writer swaps it out on
    each drain.
  - A caller wakes the writer only when the buffer goes from empty to
    non-empty, through a one-slot channel (the `dataToSend` trigger).
  - The writer then applies the C2 delay, writes everything queued, and sleeps.
  - There is no write acknowledgment; write errors call `failConnection`.
  - This is the NA arm made production-ready.
- **D2, coalescing policy (T4).** Use the C2 delay by default. Ship
  flush-on-empty (NG) only if the libfdb_c baseline shows it meets T4. The
  policy is a transport-internal choice, not a user option.
- **D3, reader (T6).** One reader goroutine per connection.
  - It matches each reply to its waiter by token and completes a one-shot
    channel per request, preallocated or pooled so there is no allocation per
    operation.
  - Callers block on that channel and on `ctx.Done()`.
- **D4, timeouts (C4, T3).**
  - With TIMEOUT unset, nothing is armed.
  - With TIMEOUT set, the transaction holds one deadline. Each wait selects on
    a channel that the transaction's single timer closes.
  - Requests arm no per-request timers.
  - The existing transport reply timeouts that C++ lacks are audited
    separately; this design does not change them.
- **D5, GRV (C3).** Unchanged. It already follows C3.

Rejected, with the evidence in rounds 1–3:
- a single network goroutine (B);
- LockOSThread;
- a per-request GRV;
- a sender-timed delay;
- read no-ack as a standalone change.

## Benchmark

Run on a dedicated Hetzner ccx43 with the round-3 core layout and the official
fdbserver 7.3.77. Use 3 rotated rounds, the workloads get100, set100 and
getset100, and 8, 64 and 256 workers.

| Arm | What |
|---|---|
| L | libfdb_c through clientbench `-tags libfdbc`: the T4 reference |
| A | today's design |
| D | D1 + D3 + D4 with the C2 delay |
| D-NG | D with flush-on-empty |
| D at 4 Ps | D with GOMAXPROCS 4 |

Each arm reports:
- ops/s;
- client CPU per op;
- server cycles per transaction;
- p50 and p99;
- frames per write and storage frames per packet, from tcpdump.

T4 is judged against L.

Tests: the full client and transport suites pass under `-race`. Tests that
assert today's write acknowledgment are replaced with tests for T1 and T2: a
frame is queued exactly once, cancelling never un-queues it, and a write error
fails every pending reply.

## Corrections against 7.3.77

Checked against `/tmp/fdbsrc` at tag 7.3.77.

- **C1, reads.** Client reads do not use `getReply`. `loadBalance` sends with
  `tryGetReply` (`fdbrpc/include/fdbrpc/LoadBalance.actor.h:305,312,812`). That
  is `sendUnreliable` plus `waitValueOrSignal` on the reply or the endpoint's
  disconnect (`fdbrpc/include/fdbrpc/fdbrpc.h:797-815`). A disconnect delivers
  `request_maybe_delivered`, and there is no resend.
  `getReply` (`fdbrpc.h:742`) is the reliable variant: it uses `sendReliable`
  and is resent after a reconnect. C1's conclusion stands either way: the
  caller learns about the reply or the failure, never about the write.
- **C2, the delay.** The delay is jittered: `delayJittered`
  (`FlowTransport.actor.cpp:708-710`, `flow/include/flow/flow.h:1481`) waits a
  uniform 0.9–1.1× the computed time (`flow/Knobs.cpp:54-55`). The writer is
  triggered only by the first unsent packet
  (`Peer::send(..., firstUnsent)`, `FlowTransport.actor.cpp:1038-1043`), which
  is what D1 implements.
- **C4, per-request timers.** "Reads have no per-request reply timeout" holds.
  But `loadBalance` does arm per-request timers:
  - a second-request (hedge) delay when a second alternative exists:
    `secondDelay = delay(secondMultiplier × nextTime + BASE_SECOND_REQUEST_TIME)`
    (`LoadBalance.actor.h:553-563`), otherwise `Never()`;
  - a TSS comparison timeout (`:90`).

  The timebomb is `ReadYourWrites.actor.cpp:1567-1574`, and `resetTimeout`, which
  arms nothing for timeout 0, is `:1576-1579`.
- **C3.** The batch-size check is `NativeAPI.actor.cpp:7351` and the latency
  EWMA is `:7364-7365`. Both are as stated.

## Timer audit: what the Go client arms today

Population: every timer, `context.WithTimeout`, `hrtimer` and `AfterFunc` in
non-test `pkg/fdbgo/client` and `pkg/fdbgo/transport` code on the request,
transaction and connection paths. Retry backoffs (C++ `delay(backoff)`) and
background loops (topology, coordinator polling) are left out.

| Scope | Go | C++ counterpart |
|---|---|---|
| Per read (`GetPipelined`) | pooled `time.Timer`, `DefaultRPCTimeout` = 5 s (`transaction.go:940`) | **none**: `tryGetReply` waits for the reply or a disconnect |
| Per read through `sendFrameWithHedge` (`readpath.go:329,577,1093`) | hedge timer `queueModel.secondDelay` (`hedge.go:84`) | `secondDelay` (`LoadBalance.actor.h:560`), only with a second alternative |
| Same | reply timer, 5 s (`hedge.go:166,193`) | **none** |
| Per read (`readpath.go:633` `waitReply`) | reply timer, 5 s | **none** |
| Per location query (`locality.go:572`) | `context.WithTimeout(DefaultRPCTimeout)` | **none**: `getKeyLocation` `loadBalance` to the proxies |
| Per GRV batch | batch window, `hrtimer` (`grv_window.go:29`) | `delay(batchTime)` (`NativeAPI.actor.cpp:7354-7355`) |
| Per commit | none (`rpc.go` `waitReplyOrProxiesChanged`) | none (`tryCommit`) |
| Per transaction, TIMEOUT unset | nothing armed (`armTimeoutLocked` returns at `timeoutNs <= 0`) | nothing (`resetTimeout`) |
| Per transaction, TIMEOUT set | shared `timeoutQueue` node: `weak.Make` + `runtime.AddCleanup` + heap push per arm (`timeout_queue.go:106-140`), one or two arms per transaction (`configureReadTimeout`, `turnoverToken.finish`); plus a `readIncarnation` cancel context | one `timebomb` actor (`ReadYourWrites.actor.cpp:1567`) |
| Per connection | coalescing `hrtimer` (`conn.go`), connection monitor | `delayJittered` in `connectionWriter`, `connectionMonitor` |
| Per dial | `context.WithTimeout(DefaultRPCTimeout)` (`database.go:592`) | connection timeout in `connectionKeeper` |

**Without a C++ counterpart:** the 5 s per-request reply timeouts on reads and
location queries (the five "none" rows). They are documented here only; D4
leaves them unchanged. Each GET therefore still arms one pooled 5 s timer, and
T3's "hot path arms no timer" is met only for TIMEOUT itself.

**D3, reader.** Today's `readLoop` already meets D3:
- each request's reply handle and its size-1 channel come from pools
  (`replyHandlePool`, `replyChanPool`);
- `readLoop` publishes and deletes the token under `pendingMu`;
- the only allocation per reply is the payload.

D3 therefore keeps `readLoop`. The prototype changes D1 and D4 only.

## Prototype of D

`FDBGO_NET_DESIGN=1` selects D (C2 delay); `=ng` selects D-NG (flush when the
queue is non-empty). Both are off by default.

- **D1** (`pkg/fdbgo/transport/na_writer.go`): per-connection writer.
  - Callers append the encoded frame to a mutex-guarded byte buffer. They signal
    a one-slot channel only when the buffer goes from empty to non-empty
    (`dataToSend`).
  - The writer waits the jittered C2 delay (D only), swaps the buffer and makes
    one `Write`.
  - Send paths, flushes and pings never wait for a write. A write error fails
    the connection.
  - Cold-dial permit order is kept.
  - TLS connections use the same writer.
  - `transport.WithLegacyWriter()` pins today's writer for tests of its mechanics.
- **D4** (`pkg/fdbgo/client/timeout_design.go`): with TIMEOUT set, the
  transaction holds one `time.AfterFunc`, reset on re-arm and stopped on disarm.
  - `Transact` and `ReadTransact` disarm on return.
  - The timer's callback holds the transaction through one `weak.Pointer`,
    made on the first arm. A dropped armed transaction is therefore
    collectable, as C++'s timebomb holds only the reset promise.
  - There is no `runtime.AddCleanup` and no shared heap per arm.
  - A successful `Commit` leaves the timeout armed: the committed transaction
    publishes an armed successor (`TestTimeoutSuccessor*`, the C++ timebomb
    keeps running).
  - An earlier variant disarmed on commit and held the transaction strongly.
    `TestTimeoutSuccessor*` caught the semantic change, and the variant was
    dropped.
  - Tests that drive a manual timeout queue (custom clock, timer or hooks) keep
    the queue path.

## D-M: sender writes, no writer goroutine

`FDBGO_NET_DESIGN=m` selects D-M: D3 and D4 as in D, with D1 replaced by a
writer lock instead of a writer goroutine (`mSend`, `mDrainAndUnlock` in
`na_writer.go`).

- A sender appends its frame to the connection buffer, then `TryLock`s the
  writer lock.
  - If it gets the lock, it swaps the buffer out and writes it, which includes
    its own frame and every frame appended since. It repeats until the buffer
    is empty, unlocks, and checks the buffer once more.
  - If it does not get the lock, it returns: the holder writes its frame.
- There is no C2 delay. A delay would put the sender to sleep or need a timer
  goroutine, which is D again. Batching comes only from frames appended while a
  write is in progress, as in D-NG.

Conditions D-M must meet, and whether the prototype meets them:

| Condition | Met | How |
|---|---|---|
| No stranded frame: an appended frame is written without a further send | yes | the holder drains until empty, then rechecks after unlocking; an appender that loses `TryLock` after the holder's last check retries it |
| Order: frames go out in append order | yes | one buffer, swapped and written under the lock |
| Write error fails the connection and every pending reply (T1) | yes | the holder calls `failConnection`; `design_writer_test.go` covers this for `m` |
| `ctx` ends the caller's wait (T2) | **no** | the holder is inside `write(2)`. On a full socket it blocks there, writing other callers' frames, and its `ctx` cannot interrupt the syscall |
| No write acknowledgment (T1) | **no**, under backpressure | the sender that wins the lock returns only when the socket has taken every queued frame: that is a write acknowledgment for the whole queue |

The two failures are not test artifacts. Under `-race`, every other
transport and client target passes with `m`; these fail (`race6-m-*`):

- `TestNativeCommit_ReplyBeforeWriteAcknowledgment` (4 subtests): "socket
  write returned before the real reply completed the native actor". The commit
  reply arrives while the committing sender is still blocked in its write.
- `TestNativeCommit_WriteBackpressureOwnerRetirement` (4 subtests): the native
  actor cannot exit before the gated socket writer is released.
- `TestFallbackStartsEndAtTransportSuspension/flush` and
  `TestFallbackReadyBeginStartsEndBeforeConsumingError`: a warm `end` request
  is serialized behind `begin`'s blocked flush.

The cure is a non-blocking write: on `EAGAIN`, hand the unwritten tail to a
goroutine that waits for the socket to drain. That is today's A: the sender's
direct write plus a handoff to the writer. D-M is therefore A without the
handoff, and it is correct only while sockets never fill. It is measured to
show what the writer goroutine costs, not as a candidate.
