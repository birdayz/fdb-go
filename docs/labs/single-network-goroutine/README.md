# Lab: one network goroutine per process

**Status: experiment. Not a proposal.** It measures whether moving all of the
pure-Go client's connection I/O onto a single goroutine, as flow's single
network thread does (`Net2::run`, flow/Net2.actor.cpp:1506), helps or hurts in
Go. The prototype stays behind an environment switch and is off by default.

## Result

- **No arm loses broadly.** At 64 workers, `get100` with the network goroutine
  costs **10% less client CPU per op** and gets **6% more throughput**, with
  lower p50 and p99. At 8 workers it is within about 3% of today's design.
  `set100` at 64 workers is unchanged.
- **The draft's "more handoffs" argument does not hold** for today's code
  ([leader-follower-read.md](../../perf/drafts/leader-follower-read.md),
  "Alternatives considered"). With the default 10/20 µs coalescing delay,
  today's sender already hands every frame to a per-connection `writeLoop`, and
  a per-connection `readLoop` hands every reply to its waiter. The network
  goroutine keeps the reply handoff and replaces the writer handoff with a queue
  push. When the loop is asleep, that push also costs an eventfd write. Under
  load the loop is rarely asleep, and futex calls per op fall (0.61 → 0.49 at
  `get100`/64).
- **Server CPU is mixed.** At `get100`/64 the servers spent **14% more** cycles
  per transaction, consistently across 3 of 3 pairs; the cause has not been
  found. At `set100`/8 they spent **40% less**, with 34% more throughput. That
  cell is known to be bimodal (commit convoys split; TODO.md §0), and n = 3, so
  it is a lead to follow up, not a result.
- **The single goroutine has headroom.** At 142k `get100` ops/s it was busy about
  53% of one core (CPU profile below). It would saturate somewhere above about
  250k ops/s on this machine, and that is a hard ceiling that today's design
  does not have.

## Prototype

`FDBGO_NETWORK_GOROUTINE=1`, read once at package init, Linux only, plaintext
TCP only (a TLS connection keeps today's loops). Wire bytes are unchanged:
frames are built by the same `encodeFrame`, and the same coalescing delay
(`coalesceDelay`, 10/20 µs defaults) is applied per connection.

| file | lines | what |
|---|---:|---|
| `pkg/fdbgo/transport/netgoroutine_linux.go` | 456 | the loop |
| `pkg/fdbgo/transport/netgoroutine.go` | 40 | the send entry |
| `pkg/fdbgo/transport/netgoroutine_other.go` | 13 | non-Linux stub |
| `conn.go`, `write_context.go`, `direct_write.go` | +40 | hooks |

**The loop.** There is one goroutine per process with one epoll set. It holds
every connection socket, an eventfd (flow's `reactor.wake`) and a timerfd (the
coalescing delay). The goroutine parks in the Go netpoller on the epoll fd
itself (`os.File.SyscallConn().Read`), so it is an ordinary goroutine, not a
thread blocked in `epoll_wait`. Each iteration:

1. drain the submission queue (mutex plus slice swap);
2. write every connection whose delay has elapsed, non-blocking, arming
   EPOLLOUT for a remainder;
3. set `sleeping`, re-check the queue, then `epoll_wait(0)` or park;
4. read each ready socket until EAGAIN, verify checksums, and dispatch frames.

Frames are dispatched exactly as `readLoop` does: pings are answered,
ENDPOINT_NOT_FOUND is handled, and a reply is published to its waiter's channel
under `pendingMu`, then deleted.

**Registration.** After the normal handshake, the socket is `dup`'d to the loop
and the `net.Conn` is closed. That takes the original fd out of the runtime's
netpoller, so idle Ms are not woken for data nobody reads there. `readLoop` and
`writeLoop` are not started. `connectionMonitor` still runs.

**Senders.** Every send entry point (`SendFrame*`, `writeContext`,
`sendWithoutAck`, `tryEnqueue`) keeps cold-dial permit order, encodes the frame
on the caller's goroutine, and pushes it to the loop's queue. If the loop is
asleep it also writes the eventfd. A send never waits for the socket write, as
libfdb_c never does; `Flush*` is a no-op.

**Teardown.** `failConnection` queues a close. The loop then deregisters the
socket, closes the dup and calls `loopWG.Done`. A read or write error fails the
connection through the same `failConnection` and `failAllPending` path as
today. Error paths are crude: a panic on the loop ends the process (no recover), frames sent after teardown are dropped, and their
waiters are failed with the usual coded 1030.

**Third arm not built.** Leader/follower, from the draft, was not prototyped. It
needs a resumable frame reader and a role handover that keeps `readLoop` from
holding the role while it is blocked in `Read`. With one RPC in flight,
`readLoop` holds the role almost all the time, so a naive version measures
today's design. The client also receives replies directly from the reply channel at many sites
(11 non-test receive expressions in `pkg/fdbgo/client` alone), so routing waiters
through a leader read is not cheap. The owner also rejected
the draft.

Known inefficiency in arm B, which biases against it: after a full write,
`st.out` is resliced from its advanced base, so the out buffer loses capacity and
reallocates. This shows as about 15–30% more GC cycles per run with the same
malloc count per op.

## Method

- **Cluster.** Native fdbserver 7.3.77, three processes (stateless, transaction,
  storage), `configure new single memory`, each pinned to its own CPU (9, 10,
  11). See [start_cluster.md](start_cluster.md). No testcontainer.
- **Client.** `tools/clientbench` Go backend, one binary for both arms (only the
  environment variable differs). It was built with `go build -trimpath`, not
  Bazel. Toolchain go1.27.3, base `origin/master` `54566f2a4` plus this
  prototype. Arm A therefore includes the prototype's nil checks on the send
  path. Settings: `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, `taskset -c 1-8`,
  closed loop. Identities are in [raw/identity.txt](raw/identity.txt).
  The measured binary predates the committed code by one edit: it had two
  `recover()` blocks on the loop (one re-panicked, one failed the connection),
  removed for the repository's `norecover` lint. No path that did not panic changed.
- **Cells.** `get100` and `set100` at 8 and 64 workers.
  [run_cell.py](run_cell.py) runs arms in the order **A B A B A B** (3 pairs per
  cell). Each slot has a short run and a long run (about 25 s long, about 8 s
  short, iterations in [campaign.sh](campaign.sh)).
  - Server cycles per transaction come from `perf stat -p` on the three
    fdbservers, long minus short, as `servercost.py` does.
  - Client metrics come from the long run. CPU/op comes from the harness's
    getrusage snapshots around the measured window; latency p50/p99 comes from
    `samples.csv`.
  - Errors are terminal errors; retries are transaction-callback retries.
- **Machine.** A shared 24-thread Ryzen 3900X. Other agents ran Bazel lanes
  during the session. A first campaign, started at load 40–58, was cancelled and
  discarded: `get100`/8 ranged from 5.7k to 21k ops/s with p99 up to 9.7 ms.
  For the retained campaign, `run_cell.py` waits before every run until the
  1-minute load average is ≤ 16. Measured runs started at load **10.8–15.8**,
  recorded per run in `raw/cells.jsonl`. Client and servers were on disjoint
  CPUs, but not isolated from other tenants.
- **Syscalls.** A separate A B A B run per cell, with client-side `perf stat`
  tracepoints (`raw_syscalls:sys_enter`, `futex`, `epoll_pwait`, `epoll_wait`,
  `nanosleep`, `read`, `write`, `timerfd_settime`, context switches), long minus
  short, per operation. There was no `strace` on the box.
- **Tables.** Cells show median [min, max] over the 3 repetitions. B/A is the
  median [min, max] of the three within-pair ratios. The tables are regenerated
  by `python3 summarize.py raw`.

## Results

### get100, 8 workers (load1 12.6–15.4)

| metric | A: today | B: network goroutine | B/A (paired) |
|---|---:|---:|---:|
| ops/s | 23,053 [23,049, 23,077] | 22,466 [19,612, 22,502] | 0.975 [0.850, 0.976] |
| p50 µs | 343 [342, 343] | 350 [350, 354] | 1.022 [1.022, 1.034] |
| p99 µs | 422 [419, 430] | 459 [434, 1,678] | 1.067 [1.028, 4.004] |
| client CPU µs/op | 39.2 [38.9, 39.5] | 38.4 [37.3, 38.5] | 0.975 [0.958, 0.980] |
| client user / sys µs/op | 27.8 / 11.5 | 25.8 / 12.4 | 0.918 / 1.093 |
| server kcycles/tx | 66.4 [65.1, 67.0] | 68.3 [33.8, 74.8] | 1.019 [0.520, 1.127] |
| errors / retries | 0 / 0 of 1.8M | 0 / 0 of 1.8M | |

The worst B slot (19.6k ops/s, p99 1.7 ms) was the first B run. The 33.8k
server-cycle value is a short/long differencing artifact of one slot.

### get100, 64 workers (load1 11.6–15.8)

| metric | A: today | B: network goroutine | B/A (paired) |
|---|---:|---:|---:|
| ops/s | 125,999 [125,042, 126,878] | 134,096 [132,991, 134,933] | 1.064 [1.048, 1.079] |
| p50 µs | 483 [479, 486] | 456 [456, 459] | 0.943 [0.937, 0.958] |
| p99 µs | 993 [992, 1,016] | 895 [843, 913] | 0.903 [0.830, 0.919] |
| client CPU µs/op | 26.5 [26.2, 26.6] | 23.9 [23.4, 24.0] | 0.901 [0.881, 0.915] |
| client user / sys µs/op | 22.6 / 3.9 | 19.8 / 3.9 | 0.880 / 0.998 |
| server kcycles/tx | 26.6 [26.4, 27.4] | 30.2 [30.2, 30.3] | 1.138 [1.105, 1.145] |
| errors / retries | 0 / 0 of 9M | 0 / 0 of 9M | |

### set100, 8 workers (load1 11.6–14.0)

| metric | A: today | B: network goroutine | B/A (paired) |
|---|---:|---:|---:|
| ops/s | 897 [709, 1,001] | 1,127 [950, 1,204] | 1.339 [1.126, 1.343] |
| p50 µs | 10,088 [6,761, 11,293] | 6,756 [6,669, 6,963] | 0.661 [0.617, 0.999] |
| p99 µs | 17,265 [16,733, 21,719] | 15,033 [10,804, 17,270] | 0.795 [0.626, 0.898] |
| client CPU µs/op | 69.5 [64.4, 79.7] | 62.7 [58.7, 69.2] | 0.868 [0.844, 0.974] |
| server kcycles/tx | 624.8 [591.0, 822.2] | 376.9 [346.7, 579.2] | 0.603 [0.587, 0.704] |
| errors / retries | 0 / 0 of 45k | 0 / 0 of 45k | |

This cell is bimodal. A one-convoy run costs about 350–430k server cycles at
about 6.7 ms p50. A split run costs 580–900k (TODO.md §0). One of three B runs
looks split (579k); A's runs are all at 591k or above.

### set100, 64 workers (load1 10.8–11.6)

| metric | A: today | B: network goroutine | B/A (paired) |
|---|---:|---:|---:|
| ops/s | 5,619 [5,583, 5,681] | 5,646 [5,570, 5,681] | 1.000 [0.998, 1.005] |
| p50 µs | 10,817 [10,731, 10,846] | 10,792 [10,784, 10,862] | 1.001 [0.998, 1.005] |
| p99 µs | 20,887 [20,783, 20,988] | 20,831 [20,719, 21,019] | 0.997 [0.997, 1.001] |
| client CPU µs/op | 37.3 [36.2, 37.7] | 37.8 [37.6, 38.1] | 1.022 [0.998, 1.044] |
| server kcycles/tx | 123.6 [121.3, 124.3] | 125.8 [121.4, 126.6] | 1.017 [1.001, 1.018] |
| errors / retries | 0 / 0 of 450k | 0 / 0 of 450k | |

[raw/tables.md](raw/tables.md) has the full tables, including cores busy and
user/sys spread.

### Client syscalls per operation

Each value is the median of 2 runs per arm, long minus short. In arm B, `write`
includes eventfd wakes and `read` includes eventfd/timerfd drains. `epoll_wait`
is the loop's own non-blocking probe; `epoll_pwait` is the Go runtime's
netpoller.

| cell | arm | all | futex | epoll_pwait | epoll_wait | nanosleep | read | write | timerfd_settime | ctx-sw |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| get100/8 | A | 8.09 | 3.09 | 2.13 | 0.00 | 0.42 | 1.49 | 0.40 | 0.55 | 2.57 |
| get100/8 | B | 9.87 | 2.89 | 2.29 | 1.77 | 0.42 | 1.24 | 0.71 | 0.54 | 2.51 |
| get100/64 | A | 2.29 | 0.61 | 0.72 | 0.00 | 0.05 | 0.54 | 0.14 | 0.21 | 0.44 |
| get100/64 | B | 2.47 | 0.49 | 0.68 | 0.42 | 0.05 | 0.41 | 0.23 | 0.19 | 0.37 |
| set100/8 | A | 13.52 | 4.35 | 3.52 | 0.00 | 1.07 | 2.71 | 0.73 | 1.04 | 4.74 |
| set100/8 | B | 23.75 | 5.69 | 5.23 | 4.52 | 1.41 | 3.30 | 1.97 | 1.44 | 6.59 |
| set100/64 | A | 3.32 | 1.02 | 0.96 | 0.00 | 0.23 | 0.63 | 0.17 | 0.28 | 0.96 |
| set100/64 | B | 3.51 | 0.84 | 0.86 | 0.60 | 0.20 | 0.46 | 0.28 | 0.24 | 0.82 |

The `set100`/8 per-op counts carry idle-time costs and differ in convoy state,
so compare them only within one run. Raw values are in `raw/syscalls.jsonl`.

### Where the loop spends time

A CPU profile of arm B at `get100`/64, from one 10.6 s run at 142k ops/s
([raw/profile-B-get100-64-top.txt](raw/profile-B-get100-64-top.txt)), puts
5.56 s cumulative in `netLoop.run`, about 53% of one core:

- socket `write` (with flushDue): 2.07 s;
- `readConn`: 1.99 s, of which parse and dispatch is 1.20 s;
- submission `apply`: 0.59 s.

## Interpretation

**Handoffs per RPC are the same or fewer, not more.** Today's design already
pays two goroutine handoffs per RPC at the default coalescing delay: sender to
`writeLoop`, and `readLoop` to waiter. The network goroutine pays the same reply
handoff. On the send side it pays a queue push, plus an eventfd write only when
the loop is asleep.

At 8 workers the loop is often asleep. Syscalls rise (eventfd writes plus the
loop's `epoll_wait(0)` probe) while futex calls fall only slightly. User CPU
drops by 8%, system CPU rises by 9%, and the net change is about −2.5% CPU/op.

At 64 workers the loop is nearly always busy. Pushes rarely need a wake, and
one goroutine serves every connection instead of a reader and a writer per
connection. Futex calls
and context switches per op fall, user CPU per op falls by 12%, and throughput
rises by 6%.

**What it costs.**
- One goroutine is a throughput ceiling (about 2× headroom here).
- At `get100`/64 the servers spend 14% more cycles per transaction.
  Per-connection coalescing is the same rule, but its clock starts when the loop
  dequeues the frame, not when the caller sends it. A wire trace (wire-goldens
  or wirediff) is needed to see whether packets per transaction changed. This
  has not been done.
- The design is a large structural change: an fd outside the runtime poller,
  manual epoll, timerfd and eventfd. Its correctness surface (cancellation,
  TLS, teardown) is far bigger than this prototype covers. The only correctness
  evidence here is clientbench's value checks and persisted-write validation,
  with 0 errors and 0 retries over 22.6M operations. No test suite was run with
  the switch on.

**`set100`/8.** The 40% server-cycle drop matches the known convoy-split
mechanism: the network goroutine delivers a convoy's commit replies in one
iteration and batches the next commits together. It deserves a targeted repeat
with more pairs and a wire trace before anyone relies on it.

## Is per-transaction ownership worth an RFC?

**Not on this evidence.** Per-transaction ownership means one transaction's
operations are serialized and transactions run in parallel. In Go, a
transaction's operations already run on its caller's goroutine. What this lab
measured is the cost of handing frames and replies between caller goroutines and
connection I/O goroutines.

Per-transaction ownership does not remove those handoffs: replies on a shared
socket still need one reader to demultiplex them. The gain seen here came from
the opposite direction: fewer, busier I/O goroutines that amortize wakes under
concurrency.

Two follow-ups are better spent than an RFC:
1. A wire trace of `get100`/64 for the server-cycle regression.
2. A larger `set100`/8 sample for the convoy effect.

Either could point to a narrower change, such as a per-process writer, or
coalescing that is clocked on the sender. Such a change would not mean
restructuring transport.

## Reproduce

1. Start the cluster as in [start_cluster.md](start_cluster.md).
2. Build the client:
   `go build -trimpath -o /var/tmp/netg-bench/bin/clientbench-go ./tools/clientbench/cmd/clientbench`.
3. Run the campaign: `PIDS=<three fdbserver pids> ./campaign.sh`.
4. Regenerate the tables: `python3 summarize.py raw`.

The campaign took about 35 minutes plus load waits.
