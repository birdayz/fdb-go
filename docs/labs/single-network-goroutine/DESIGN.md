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
