# RFC-259: Request batching parity with libfdb_c

**Status:** Draft
**Item:** TODO.md §0, "Server CPU per transaction" (top priority).
**Spec:** FoundationDB 7.3.77 (`libfdb_c`, `fdbrpc`, `flow`).

## Problem

At 8–64 workers the pure-Go client makes the servers spend 1.5–2.4× the CPU per
transaction that libfdb_c does (3-node cluster, TODO.md §0). Both clients send the same
logical requests: the same reads, mutations, conflict ranges and bytes per transaction.
The difference is how many requests and packets carry them. The servers pay per request
and per packet, so smaller batches cost them more.

libfdb_c batches in two places, and Go diverges from both:

1. **GRV batch window.** `readVersionBatcher` (NativeAPI.actor.cpp:7307-7405) starts a
   window of `batchTime` when the first request of a batch arrives (:7354-7356) and sends
   the batch when it closes (:7358-7361) or reaches `MAX_BATCH_SIZE` 1000 (:7351-7353,
   ClientKnobs.cpp:91). `batchTime` starts at 0 (:7333) and converges to half the
   smoothed reply latency, capped at `GRV_BATCH_TIMEOUT` 5 ms (:7363-7366,
   ClientKnobs.cpp:92). Go's `grvBatcher.admit`
   (`pkg/fdbgo/client/grv.go:895`) sends a request at once when no GRV is in flight, and
   otherwise arms a `time.AfterFunc(batchTime)`.
2. **Connection write coalescing.** `connectionWriter` (FlowTransport.actor.cpp:704-739)
   waits `max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now - lastWriteTime))`, 10–20 µs
   (flow/Knobs.cpp:211-212), and then writes everything queued for the peer in one call.
   Go writes a frame on the sender's goroutine when the connection is idle
   (`tryWriteDirect`, `pkg/fdbgo/transport/direct_write.go`), so concurrent senders
   usually produce one packet each.

Go dropped both waits because Go timers cannot express them. When every P is idle the
runtime waits in `epoll_wait` with a millisecond timeout and rounds any shorter timer up
to 1 ms (`runtime/netpoll_epoll.go` in Go 1.26). Flow's timers are precise: the reactor
arms a boost::asio deadline timer, which Linux backs with a timerfd
(Net2.actor.cpp:2101-2119).

## Measurements

Local: `fdbserver` 7.3.77 run natively, `single memory`, pinned to 4 CPUs; clientbench
with `GOMAXPROCS=8` on 8 other CPUs. Server cost is `perf stat -p fdbserver` cycles per
transaction, as the difference between a short and a long run. Prototypes were built from
an uncommitted experiment patch; medians of 3.

| Workload, workers | Go today | GRV window on a precise timer | + write coalescing | libfdb_c |
| --- | ---: | ---: | ---: | ---: |
| `grv`, 8 | 40.8k | 24.8k | 29.2k | 33.3k |
| `get100`, 8 | 96.5k | 90.4k | 71.6k | 67.3k |
| `get100`, 32 | 41.8k | 41.6k | 36.5k | 25.8k |

- `grv`, 8 workers, with the GRV window: mean latency 155 → 131 µs, throughput
  51k → 61k tx/s, client CPU 25.1 → 21.9 µs per transaction. Server GRV requests per
  transaction drop by a third.
- `get100`, 1 worker: mean latency is unchanged within run-to-run noise (today 130–138 µs,
  prototypes 131–132 µs). Always waiting in both places, as libfdb_c does, gives 244 µs
  (libfdb_c: 234 µs).
- `grv`, 8 workers: write coalescing undoes part of the GRV gain (24.8k → 29.2k cycles,
  131 → 165 µs). Part B needs a better idle test before it ships.
- `set100`, 8 workers: the commit proxy receives two commit batches per round of 8
  transactions (0.24–0.25 per transaction; libfdb_c 0.126–0.15). Workers in a closed loop
  form convoys; Go splits a convoy into two groups that then stay apart, alternating on
  the log.
  Tracing client events shows two triggers. A lone first GRV and late `time.AfterFunc`
  timers spread one round's commits beyond the proxy's 0.5 ms window
  (`COMMIT_TRANSACTION_BATCH_INTERVAL_FROM_IDLE`, CommitProxyServer.actor.cpp:430-436);
  the GRV window on a precise timer removes this, and with `GOGC=off` gives 0.126 per
  transaction in 5 of 5 runs. Garbage collection is the second trigger: in three traced
  runs, 4 of 6 splits began within 1 ms of a GC cycle start. With `GOGC=100` most runs
  still split. That part belongs to the client CPU item (allocations per transaction), not
  this RFC.
- Write coalescing raised client CPU by about 7 µs per transaction at 8 workers in the
  prototype, which handed every frame to `writeLoop`. Part B below avoids that handoff.

At 32 workers libfdb_c's single network thread is saturated (44k tx/s against Go's 73k)
and batches more than its knobs alone would. This RFC targets parity in mechanism, not in
that side effect.

## Part A: GRV batch window on a precise timer (proposed for implementation)

### A1. `pkg/fdbgo/internal/hrtimer`

```go
// Sleep blocks the calling goroutine for d with microsecond precision.
// d <= 0 returns at once.
func Sleep(d time.Duration)
```

- Linux: a pool of timerfds (`CLOCK_MONOTONIC`, `TFD_NONBLOCK|TFD_CLOEXEC`), each wrapped
  in an `*os.File` so reads park in the netpoller. `Sleep` takes one, arms it with
  `timerfd_settime` (relative) and reads the 8-byte expiry count. A ready timerfd wakes
  `epoll_wait` whatever its millisecond timeout, so no thread or P is held while waiting.
  The pool is bounded; excess timerfds are closed.
- If `timerfd_create` fails (for example, `EMFILE`) and on other platforms: `time.Sleep`,
  which is today's precision.
- Not chosen: `nanosleep` holds a P for the duration and needs `PR_SET_TIMERSLACK` on
  each thread, since Linux adds up to 50 µs of slack by default; spinning costs client
  CPU. In the prototype, timerfd and `nanosleep` with 1 ns slack gave the same latency
  and client CPU.

### A2. `grvBatcher.admit`

- The first request of a batch starts the window: a goroutine runs
  `hrtimer.Sleep(batchTime)` and then flushes the batch if it is still pending. It replaces
  `time.AfterFunc`; `timerSeq` keeps rejecting a stale expiry as it does today.
  `batchTime` 0 (the initial value) flushes at once, like Flow's `delay(0)`.
- `MAX_BATCH_SIZE` and `observeLatency` stay as they are; both already match C++.
- **Serial fast path (accepted divergence).** A request is sent at once when no GRV is in
  flight and the previous batch held at most one request. Then the client is issuing GRVs
  one at a time, a window cannot collect anything, and libfdb_c would add half a GRV
  round trip of latency (1 worker: 244 µs against 132 µs). The first request after a
  multi-request batch always waits, so a convoy is never split by its first member.

### Executable spec

- Unit tests on `grvBatcher` (`grv_batch_unit_test.go`): the serial fast path dispatches
  at once; after a batch of two or more, the next first request waits for the window; a
  request that arrives while a GRV is in flight waits; a stale window expiry is ignored;
  `MAX_BATCH_SIZE` still detaches at once.
- `hrtimer` tests: `Sleep` never returns early (monotonic clock); `d <= 0` returns at once;
  concurrent sleepers do not share a timerfd; the fallback path works. No test asserts an
  upper bound on sleep time.
- Benchmark evidence on the PR: server cycles per transaction for `grv` and `get100` at
  1, 8 and 32 workers, and 1-worker mean latency within 5% of today.

### Wire compatibility

None. `GetReadVersionRequest` is unchanged: its `transactionCount`, flags, tags and span
context are built from the batch exactly as today. Only the batch boundaries move, as they
do in libfdb_c.

## Part B: connection write coalescing (proposed, not yet for implementation)

`connectionWriter`'s delay, applied without a `writeLoop` handoff:

- **Sender as flusher.** A sender appends its frame to the connection's unsent buffer. If
  no flush is pending, it becomes the flusher: it sleeps
  `max(10 µs, 20 µs - since last write)` on `hrtimer`, then writes everything queued in one
  call. Other senders append and go straight to waiting for their replies. `writeLoop`
  keeps the partial-write tail, as it does today.
- **Idle fast path (accepted divergence).** The frame is written at once when no other
  request on the database is awaiting a reply, so a lone caller pays no delay. libfdb_c
  always waits at least 10 µs.

Part B needs its own design review before implementation: it changes the ownership rules
in `transport/conn.go` (`writeMu`, queued counts, tails, cancellation of a queued frame).

## Not in scope

- Allocations, encoding and the no-write commit path: TODO.md §0, "Client CPU per
  transaction". Fewer allocations also mean fewer GC cycles, the remaining trigger for
  split commit convoys.
- Server knobs. The benchmark clusters run with defaults, as production does.
