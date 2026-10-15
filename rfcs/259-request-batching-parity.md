# RFC-259: Request batching parity with libfdb_c

**Status:** Draft (revision 2; Part A implemented on this branch for review)
**Item:** TODO.md §0, "Server CPU per transaction" (top priority).
**Spec:** FoundationDB 7.3.77 (`libfdb_c`, `fdbrpc`, `flow`).

## Problem

On a 3-node cluster at 8–64 workers, the pure-Go client makes the servers spend 1.5–2.4×
the CPU per transaction that libfdb_c does (TODO.md §0). Both clients send the same
logical requests: the same reads, mutations, conflict ranges and bytes per transaction.
On a single local server the gap reproduces (table below), and it comes from how many GRV
requests, commit batches and packets carry that work. The servers pay per request and per
packet, so smaller batches cost them more.

libfdb_c batches in two places, and Go diverges from both:

1. **GRV batch window.** `readVersionBatcher` (NativeAPI.actor.cpp:7307-7405) opens a
   window of `batchTime` when the first request of a batch arrives (:7354-7356) and sends
   the batch when it closes (:7358-7361) or reaches `MAX_BATCH_SIZE` 1000 (:7351-7353,
   ClientKnobs.cpp:91). `batchTime` starts at 0 (:7333) and converges to half the
   smoothed reply latency, capped at `GRV_BATCH_TIMEOUT` 5 ms (:7363-7366,
   ClientKnobs.cpp:92). A window of 0 is `delay(0)`, which queues a ready task
   (Net2.actor.cpp:1857-1858), so it still collects every request posted before that task
   runs. Go's `grvBatcher.admit` (`pkg/fdbgo/client/grv.go:895`) sends a request at once
   when no GRV is in flight, and otherwise arms `time.AfterFunc(batchTime)`.
2. **Connection write coalescing.** `connectionWriter` (FlowTransport.actor.cpp:704-739)
   waits `delayJittered(max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now - lastWriteTime)))`
   and then writes everything queued for the peer in one call. With the knobs at 10 and
   20 µs (flow/Knobs.cpp:211-212) and a jitter factor of 0.9–1.1 (flow.h:1481-1485,
   Knobs.cpp:54-55), that is 9–22 µs. Go writes a frame on the sender's goroutine when the
   connection is idle (`tryWriteDirect`, `pkg/fdbgo/transport/direct_write.go`), so
   concurrent senders usually produce one packet each.

Go dropped both waits because Go timers cannot express them. When every P is idle the
runtime blocks in `epoll_wait` with a millisecond timeout and rounds any shorter timer up
to 1 ms (`runtime/netpoll_epoll.go`, Go 1.26). Flow arms a boost::asio timer for the next
deadline with `BUSY_WAIT_THRESHOLD` 0 (Net2.actor.cpp:2101-2124, Knobs.cpp:56); on Linux
asio backs it with a timerfd, which Linux does not round to milliseconds.

## Measurements

One local server, so these show the mechanism, not the 3-node numbers. Setup, commands,
run lengths, the prototype patch and per-run results are in
[docs/benchmarks/rfc-259](../docs/benchmarks/rfc-259/README.md): `fdbserver` 7.3.77
pinned to 4 CPUs, clientbench at `36685ebfa` with Go 1.26.9 and `GOMAXPROCS=8` on 8
other CPUs, server cost from `perf stat -p` as the difference between a short and a long
run (`tools/clientbench/scripts/servercost.py`). The prototype's serial rule is not the
one proposed below. Median server cycles per transaction, 3 runs each:

| Workload | Workers | Go today | Prototype: GRV window | + write coalescing | libfdb_c |
| --- | ---: | ---: | ---: | ---: | ---: |
| `grv` | 1 | 171.7k | 166.4k | 155.1k | 148.0k |
| `grv` | 2 | 142.7k | 81.3k | 90.3k | 82.4k |
| `grv` | 4 | 74.6k | 42.2k | 40.2k | 51.7k |
| `grv` | 8 | 39.3k | 25.9k | 26.2k | 32.2k |
| `grv` | 32 | 10.9k | 11.1k | 11.1k | 9.2k |
| `get100` | 1 | 283.0k | 279.5k | 284.9k | 314.8k |
| `get100` | 2 | 229.6k | 241.0k | 261.2k | 180.8k |
| `get100` | 4 | 151.0k | 124.8k | 91.0k | 125.2k |
| `get100` | 8 | 89.0k | 87.6k | 64.7k | 62.2k |
| `get100` | 32 | 42.9k | 39.2k | 33.4k | 25.3k |

- `grv`, 2–8 workers: the GRV window brings server cycles to or below libfdb_c's, with
  lower latency than today (8 workers: 154 → 135 µs mean). In an earlier 8-worker run the
  server's GRV requests per transaction fell from 0.230 to 0.147 (libfdb_c 0.200), from its
  trace-log metrics.
- 1 worker: mean latency is unchanged (`get100` 129 → 127 µs; libfdb_c 233 µs). A
  prototype that always waits in both places, as libfdb_c does, measured 244 µs; the GRV
  window and the write delays together account for that difference.
- `get100` gains little from the GRV window, since each transaction sends one GRV and
  one read; packets dominate. Write coalescing brings 4–8 workers to libfdb_c's level but
  raises `grv` latency (8 workers: 135 → 165 µs) and makes `get100` at 2 workers worse.
  Part B needs a better idle test before it ships.
- `set100`, 8 workers (server trace-log metrics and client event traces; raw files not
  kept): the commit proxy receives 0.24–0.25 commit batches per transaction (libfdb_c
  0.126–0.15). Workers in a closed loop move in convoys; Go splits a convoy into
  two groups that then stay apart, alternating on the transaction log. Client event traces
  show two triggers. A lone first GRV and late `time.AfterFunc` expiries spread one round's
  commits beyond the proxy's 0.5 ms window (`COMMIT_TRANSACTION_BATCH_INTERVAL_FROM_IDLE`,
  CommitProxyServer.actor.cpp:430-436, ServerKnobs.cpp:664). With the prototype GRV window
  and `GOGC=off`, commit batches per transaction were 0.126 in 5 of 5 runs. Garbage
  collection is the other trigger: in three traced runs, 4 of 6 splits began within 1 ms
  of a GC cycle start, and with `GOGC=100` most runs still split. That part belongs to the
  client CPU item (allocations per transaction).
- The write-coalescing prototype handed every frame to `writeLoop` and raised client CPU
  by about 7 µs per transaction at 8 workers. Part B avoids that handoff.

At 32 workers libfdb_c's single network thread is saturated (`get100`: 45k tx/s against
Go's 75k) and batches more than its knobs alone would. This RFC targets parity in
mechanism, not in that side effect; `get100` at 32 workers stays about 1.3× after both
parts, and at 2 workers neither part closes the gap.

## Part A: GRV batch window on a precise timer (proposed for implementation)

### A1. `pkg/fdbgo/internal/hrtimer`

A one-shot timer with one owner:

```go
func New() (*Timer, error)            // ErrUnsupported off Linux
func (t *Timer) Reset(d time.Duration) error // arm d from now (d <= 0 arms 1 ns); replaces any expiry
func (t *Timer) Stop() error          // disarm
func (t *Timer) Wait() error          // block until an expiry is consumed; ErrClosed after Close
func (t *Timer) Close() error         // closes the fd and wakes a blocked Wait
```

- Linux: one timerfd (`CLOCK_MONOTONIC`, `TFD_NONBLOCK|TFD_CLOEXEC`) wrapped in an
  `*os.File`, so `Wait` parks in the netpoller; no thread or P is held while waiting. A
  ready timerfd wakes `epoll_wait` regardless of the runtime's millisecond timeout.
- `Reset` and `Stop` may be called from any goroutine while `Wait` is parked:
  `timerfd_settime` re-arms the fd the waiter is polling.
- `Wait` can return for an expiry that a later `Reset` or `Stop` superseded. The owner
  must recheck its own state after every `Wait`. That is the only contract; `hrtimer` does
  not track generations.
- Errors are returned, never retried inside the package. A `Reset` failure leaves the
  timer disarmed.
- Precision is that of a timerfd wake-up plus scheduling of the parked goroutine; there is
  no upper-bound guarantee, and no test asserts one.
- Not chosen: `nanosleep` blocks the M in a syscall (holding its P until `sysmon`
  retakes it) and needs `PR_SET_TIMERSLACK` per thread, since Linux adds up to 50 µs of
  slack by default; spinning costs client CPU. A goroutine per window with a pooled timerfd
  (revision 1) leaked sleepers when a batch filled before its window closed.

### A2. `grvBatcher`

State added under `b.mu`: `windowArmed`, `windowDeadline`, `lastFast` (the previous batch
was sent on the serial fast path), `idleSince` (when `inFlight` last dropped to 0), and a
`window` (the timer behind an interface, so tests drive expiry explicitly) and `now`
(clock, injectable).

- **Admission.** On the first pending request:
  - **Serial fast path:** if `inFlight == 0` and (`lastFast` or
    `now - idleSince >= batchTime`), detach and send at once (the existing `case 1` path,
    bounded by the caller's context). A fresh batcher qualifies.
  - Otherwise open the window: `windowDeadline = now + batchTime`, arm the timer for
    `batchTime`, both under `b.mu` at admission. A window of 0 still goes through expiry,
    so requests that arrive before the worker runs join it, as with Flow's `delay(0)`.
- **Detach** (window expiry, `MAX_BATCH_SIZE` or fast path) clears `windowArmed` and stops
  the timer. `lastFast` records whether this batch took the fast path.
- **Expiry.** Each batcher owns one `hrtimer.Timer` and one worker goroutine, started on
  its first window through `db.registerBackgroundGoroutine` (so no `wg.Add` races Close).
  The worker loops on `Wait`; after each return it takes `b.mu` and detaches only if a
  window is armed and `now >= windowDeadline`, then sends the batch on a new goroutine, as
  `time.AfterFunc` did. Superseded and early wake-ups do nothing.
- **Close.** `context.AfterFunc(db.ctx, timer.Close)` wakes the worker, which detaches any
  pending requests, sends them (the RPC fails on the closed database, so every waiter gets
  an error), and exits. Any other `Wait` error ends the worker the same way. On exit the
  worker clears `b.window`, so the next window creates a new timer, or, once Close has
  begun and registration fails, skips the window and sends the batch at once.
- **Fallback.** If `hrtimer.New` fails, or off Linux, the window uses `time.AfterFunc` as
  today, with the same deadline check in its callback.
- A batch of one sent by window expiry runs under the database context, as today's
  timer-flushed batches do and as libfdb_c runs every batch to its reply
  (NativeAPI.actor.cpp:7384-7387); a caller that cancels returns at once. Only the fast
  path runs the RPC on the caller, bounded by its context.
- `MAX_BATCH_SIZE`, `observeLatency` (matches :7363-7366), per-flags batchers
  (`forFlags`; `cx->versionBatcher[flags]`, NativeAPI.actor.cpp:7571) and tag
  aggregation stay as they are.

**Accepted divergence: the serial fast path.** libfdb_c waits for every first request. Go
skips the window only when the previous batch also skipped it, or when no GRV has been in
flight for a full window. That keeps a client that issues one GRV at a time from paying
half a GRV round trip per transaction, while any overlap sends the batcher back to
windows: a request that arrives while another GRV is in flight always waits, and so does
the next first request after it. The rule is a heuristic over recent history; it cannot
know whether another request is about to arrive.

### Executable spec

Unit tests on `grvBatcher` with a fake window and clock (no wall-clock assertions):
- a fresh batcher sends the first request at once; repeated serial requests keep doing so;
- a request that arrives while a GRV is in flight opens a window, and the next first
  request after that batch also waits, until the batcher has been idle for `batchTime`;
- two staggered requesters (the second always arriving while the first is in flight)
  converge to one GRV per round instead of alternating singletons;
- `MAX_BATCH_SIZE` detaches at once and stops the armed window; an expiry after that, or
  before the deadline, detaches nothing;
- a zero window detaches on expiry, not on admission;
- Close with requests pending in a window delivers an error to each waiter and the worker
  exits; a window opened after Close has begun sends at once;
- with the real timerfd window, an expiry detaches and sends the batch.

Existing tests that set `b.timer` or `batchTime = time.Hour`
(`grv_batch_unit_test.go`, `grv_batch_flags_test.go`) move to the fake window; the
idle-after-a-multi-request-batch case (`grv_batch_unit_test.go:56-66`) changes to expect a
window. `hrtimer` tests: `Wait` never returns before the armed time (monotonic clock);
`Reset` while parked re-arms; `Stop` while parked prevents the expiry; `Close` wakes a
parked `Wait` with `ErrClosed`; `d <= 0` fires; no upper-bound assertion.

The PR reports server cycles per transaction for `grv` and `get100` at 1, 2, 4, 8 and 32
workers, with 1-worker mean latency within 5% of today.

### Wire compatibility

None. `GetReadVersionRequest` is built from the batch exactly as today (`flushWithin`):
`transactionCount`, flags, tags and span context. Only the batch boundaries move, as they
do in libfdb_c.

## Part B: connection write coalescing (proposed, not yet for implementation)

`connectionWriter`'s delay, applied without a `writeLoop` handoff:

- **Sender as flusher.** A sender appends its frame to the connection's unsent buffer. If
  no flush is pending, it becomes the flusher: it waits
  `max(10 µs, 20 µs - since last write)` on its connection's `hrtimer`, then writes
  everything queued in one call. Other senders append and go straight to waiting for their
  replies. `writeLoop` keeps the partial-write tail, as it does today.
- **Idle fast path (accepted divergence).** The frame is written at once when no other
  request on the database is awaiting a reply, so a lone caller pays no delay. libfdb_c
  always waits at least 9 µs.

Part B needs its own design review before implementation: it changes the ownership rules
in `transport/conn.go` (`writeMu`, queued counts, tails, cancellation of a queued frame).

## Not in scope

- `BROADCAST_BATCH_SIZE` (20, ClientKnobs.cpp:93; NativeAPI.actor.cpp:7384-7387) paces how
  libfdb_c hands one reply to its waiters on the client. It sends nothing to the server.
- GRV proxy batching (GrvProxyServer.actor.cpp) is server-side. This RFC reduces the
  requests the GRV proxy receives; it does not change how the proxy batches them.
- Allocations, encoding and the no-write commit path: TODO.md §0, "Client CPU per
  transaction". Fewer allocations also mean fewer GC cycles, the remaining trigger for
  split commit convoys.
- Server knobs. The benchmark clusters run with defaults, as production does.
