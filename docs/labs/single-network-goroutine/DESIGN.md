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

## Results

Two Hetzner ccx43 boxes (AMD EPYC-Milan, 16 vCPUs, kernel 6.8.0-138), each
with its own single-process-per-role 7.3.77 cluster (`configure new single
memory`), the round-3 core layout, 3 rotated rounds per cell. Medians, with
[min, max] in the raw tables. Compare arms within a box only: the same binaries
on both boxes give A 47.4k vs 36.0k ops/s and L 32.9k vs 36.6k on get100/64
(`rawhz4/cross`), so the boxes differ by up to 30% on latency-bound cells.

- Box 1 (`rawhz4/`, `summarize4.py rawhz4`): L, A, D, D-NG, D at 4 Ps.
  Binaries: Go `faf81872…`, libfdb_c bench `f8731701…`.
- Box 2 (`rawhz5/`, `summarize4.py rawhz5`): L, L4, A, D, D-M, D-M at 4 Ps,
  plus syscalls and captures. Binaries: Go `1aaad7db…`, libfdb_c bench
  `a2cb6e76…`, both from the transport and client source of abe373e8d.
  L4 is libfdb_c with `client_threads_per_version=4` (which disables the local
  client, MultiVersionTransaction.actor.cpp:3095-3097), the external client
  `/usr/lib/libfdb_c.so` copied per thread (:2868-2890), and 4 databases,
  which `createDatabase` assigns to threads round-robin (:3302-3305);
  transactions start on the databases in turn.

### Box 2, closed loop (ops/s; server kcycles/tx; p50 µs)

| cell | L | L4 | A | D | D-M | D-M 4 Ps |
|---|---|---|---|---|---|---|
| get100/8 | 6.3k; 82; 1223 | 10.2k; 296; 748 | 5.4k; 84; 1453 | 5.5k; 83; 1441 | 6.5k; 83; 1227 | 6.6k; 84; 1145 |
| get100/64 | 36.6k; 23; 1567 | 62.7k; 63; 941 | 36.0k; 19; 1770 | 31.8k; 18; 1979 | 48.5k; 21; 1293 | 55.6k; 18; 1096 |
| get100/256 | 57.3k; 13; 3932 | 107k; 33; 2025 | 229k; 14; 1027 | 232k; 13; 1057 | 261k; 15; 920 | 175k; 18; 1345 |
| set100/8 | 2.6k; 262; 3098 | 3.1k; 474; 2285 | 2.7k; 269; 2964 | 2.6k; 283; 3044 | 2.8k; 256; 2739 | 2.8k; 273; 2860 |
| set100/64 | 19.2k; 72; 3130 | 24.2k; 107; 2517 | 24.4k; 86; 2534 | 24.4k; 86; 2531 | 27.4k; 92; 2184 | 28.2k; 85; 2126 |
| set100/256 | 50.5k; 48; 4744 | 75.8k; 58; 3196 | 80.2k; 47; 3017 | 81.3k; 46; 3002 | 82.6k; 48; 2944 | 78.0k; 50; 3100 |
| getset100/8 | 1.8k; 327; 4228 | 2.1k; 538; 3749 | 2.0k; 366; 4063 | 2.0k; 354; 4008 | 2.2k; 360; 3715 | 2.3k; 406; 3450 |
| getset100/64 | 15.7k; 106; 3806 | 19.0k; 152; 3158 | 16.8k; 112; 3638 | 14.9k; 93; 4169 | 17.8k; 109; 3393 | 19.3k; 114; 3143 |
| getset100/256 | 38.0k; 75; 6137 | 55.8k; 86; 4240 | 59.2k; 66; 4118 | 59.8k; 66; 4084 | 61.4k; 68; 3990 | 59.2k; 77; 4103 |

Frames per write (design counter): D 3–22, D-M 1.5–5.6. Box 1 shows the
same ordering for L, A, D and D-NG (D-NG behaves like D-M: get100/64 50.6k,
get100/256 250k).

### What the data says

- **D (C2 delay) loses on latency-bound cells.** get100/64: D 31.8k vs A 36.0k
  (box 2) and 32.0k vs 46.7k (box 1), p50 +12% and +46%. The 10–20 µs C2
  delay is paid on every round trip when the queue is short. It packs the most
  frames per write and has the lowest server cost on those cells (0.83–0.90×
  A), which is what C2 is for, but it is not free for the client.
- **No writer delay is the throughput winner.** On box 2, D-M (at 8 or 4 Ps)
  is the fastest Go arm and has the lowest Go p50 in all 9 cells. Its server
  cost is 0.96–1.07× A's per transaction (1.12× D's at get100/256), from fewer
  frames per packet. On box 1, D-NG (the same flush-on-empty policy with a
  writer goroutine) leads the same way: 1.09× A on get100/64, 1.20× on
  get100/256.
- **The writer goroutine itself costs little.** D-M and D-NG never ran on the
  same box, so there is no direct pair. Against D on box 2 (get100/64), D-M
  makes as many futex calls (0.57/op) and slightly more read+write calls
  (0.51 vs 0.48/op): removing the writer's wakeup is offset by writing more
  often. D-M does not meet T2 under backpressure (section above), so D-NG is
  the shippable form of the same policy.
- **D-M at 4 Ps** halves client CPU per op at 64 workers (24 vs 48 µs/op) and is
  the fastest Go arm there; at 256 workers 4 Ps saturate (175k vs 261k).
- **L4 buys latency with server and client cost.** It is 1.1–1.9× L's
  throughput and 0.5–0.9× its p50, but each thread has its own connections, so
  batching splits: storage frames per packet 4.2 vs 32 (get100/64), server
  cycles per transaction 1.1–3.6× L, client CPU per op 1.2–2.7× L, 12–17
  syscalls/op vs 3–5. At 256 workers it is still under half of every Go arm
  on get100 (107k vs 229k+).

### set100/64 server cycles: load confound, not a wire difference (#173)

- Closed loop, equal workers: A is +21% (box 2: 86 vs 72 kc/tx) to +24%
  (box 1, cross run: 87 vs 70) over L, at 1.25–1.31× L's throughput.
- Rate-capped to 18,000 ops/s (Poisson arrivals, below L's closed-loop 19k):
  box 2 L 137, A 140 (1.02×), D 136 (0.98×), D-M 148 (1.08×), L4 188 (1.32×);
  box 1 L 127, A 142 (1.14×), D 143 (1.13×), D-NG 150 (1.19×). The +21% shrinks
  to +2% and +14%. The residual is box-dependent and follows how many frames a
  write carries (D-NG/D-M, 1.4–1.5 frames per write, cost most).
- Per transaction the requests are identical. One worker, set100, tcpdump:
  1.00 transactions per GRV request, 2.004 vs 2.005 proxy frames per
  transaction, 847.6 vs 847.7–847.8 proxy bytes per transaction, for L, A, D,
  D-NG and D-M on both boxes. The one-worker `blind_set` golden is identical
  too (`tools/clientbench/wiregolden`).
- Packing differs at equal workers: L sends 12.5–13.3 commit frames per proxy
  segment, A 7.8–8.8, D 7.7, D-M 3.6, D-NG 3.2. L packs more because its single
  network thread is the bottleneck, so more frames wait per write. That
  packing difference, not request content, is what moves the server cost.

### T4 verdict

Against the better of L and L4 per metric, on box 2, at equal workers, with a
3% tolerance (server kc/tx / p50 / p99, ratio to the better):

| cell | A | D | D-M | D-M 4 Ps |
|---|---|---|---|---|
| get100/8 | ✗ 1.01/1.94/2.08 | ✗ 1.01/1.93/2.03 | ✗ 1.01/1.64/1.54 | ✗ 1.02/1.53/1.74 |
| get100/64 | ✗ 0.85/1.88/1.58 | ✗ 0.78/2.10/1.83 | ✗ 0.91/1.37/1.28 | ✗ 0.78/1.16/1.01 |
| get100/256 | ✗ 1.05/0.51/0.35 | ✓ 1.00/0.52/0.29 | ✗ 1.12/0.45/0.33 | ✗ 1.32/0.66/0.47 |
| getset100/8 | ✗ 1.12/1.08/1.19 | ✗ 1.08/1.07/1.22 | ✗ 1.10/0.99/0.96 | ✗ 1.24/0.92/0.99 |
| getset100/64 | ✗ 1.06/1.15/1.06 | ✗ 0.87/1.32/1.12 | ✗ 1.03/1.07/0.97 | ✗ 1.07/1.00/1.02 |
| getset100/256 | ✓ 0.88/0.97/0.93 | ✓ 0.88/0.96/0.89 | ✓ 0.91/0.94/0.85 | ✗ 1.02/0.97/1.07 |
| set100/8 | ✗ 1.03/1.30/1.08 | ✗ 1.08/1.33/1.00 | ✗ 0.98/1.20/0.96 | ✗ 1.04/1.25/1.02 |
| set100/64 | ✗ 1.19/1.01/1.04 | ✗ 1.19/1.01/0.98 | ✗ 1.28/0.87/0.89 | ✗ 1.18/0.84/0.91 |
| set100/256 | ✗ 0.98/0.94/1.17 | ✗ 0.95/0.94/1.05 | ✗ 0.98/0.94/1.06 | ✗ 1.05/0.97/1.40 |

The bar is not met by any arm, and it is not one bar: L has the best server
cost and L4 the best latency, and no libfdb_c configuration has both. Read
against each separately:

- Against L alone, D-M meets latency in every cell (p50 0.23–0.97×, p99
  0.30–0.87×) and server cost within 3% in 6 of 9 cells; it misses server cost
  on get100/256 (1.12×), getset100/8 (1.10×) and set100/64 (1.28× closed loop,
  1.08× at equal ops).
- Against L4, every Go arm has lower server cost in every cell (D-M 0.28–0.86×
  L4's kc/tx) but higher latency at 8 and 64 workers on get100 (D-M p50 1.64×
  and 1.37×). Per-transaction latency at low concurrency is where Go is
  behind: 100 reads per transaction on one goroutine cost more client time per
  read than libfdb_c spends spread over 4 network threads.
- D, the design as specified, is the most frugal on the servers but trails D-M
  on throughput and latency at every 8- and 64-worker cell.
