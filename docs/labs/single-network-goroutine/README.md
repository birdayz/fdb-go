# Lab: one network goroutine per process

**Status: experiment. Not a proposal.** It measures whether moving all of the
pure-Go client's connection I/O onto a single goroutine, as flow's single
network thread does (`Net2::run`, flow/Net2.actor.cpp:1506), helps or hurts in
Go. The prototype stays behind an environment switch and is off by default.

## Result

> **Round 2 below supersedes parts of this round.** It traces the server-cost
> increase to smaller packets (a shorter effective coalescing window). The
> `set100`/8 lead did not replicate in 8 rounds. The client CPU gain comes from
> the write side and is captured by a writer-only variant at no server cost.

Round 1 result:

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

---

# Round 2

## Round 2 result

1. **Why B costs the servers more at `get100`/64.** B sends more, smaller
   packets. Per transaction it sends 15% more storage request packets (14.0 →
   12.2 frames per packet) and 7% more GRV requests (22.8 → 21.4 transactions per
   request). Bytes per transaction are identical (292.3 B to storage) and so is
   the connection count (2). The servers' own counters show the result:
   - the storage server makes 15% more run-loop iterations, 11% more read calls
     and 16% more write calls per transaction;
   - the GRV proxy receives 8% more requests.

   The mechanism is the **effective coalescing window**. Today, the writer
   goroutine (`writeLoop`) is parked between batches, so a batch's first frame
   waits for the writer to be woken before the 10–20 µs delay even starts.
   B's loop is already awake under load (that is where its CPU saving comes
   from), so its window is just the nominal delay. GRV batching shrinks for the
   same reason: its window is half the smoothed GRV latency, and B's latency is
   lower.

   Two checks confirm it:
   - **(b), the delay timed from the sender:** today's design with only that
     change sends 12.6 frames per packet and costs the servers +4%, with no
     client gain.
   - **A longer delay for B (15/30 µs):** it recovers about half the server cost
     (31.0 → 29.7 kcycles/tx against today's 28.5) and gives back most of B's
     throughput.

   So the extra server cost is an operating point on the latency/batching
   trade-off, not a cost of having one goroutine.
2. **`set100`/8.** I ran 8 rounds, each running all four arms in rotated order.
   The runs fall into three latency bands: about 7 ms (one commit convoy), 9–11 ms
   and about 16 ms (split convoys). One-convoy runs per arm: today 2/8, B 1/8,
   (a) 2/8, (b) 0/8. **No arm changes which band a run lands in**, as far as
   n = 8 can tell. (b) may make it worse, but this is not significant. Within a
   band the server cost cannot be resolved either: it comes from long-minus-short
   differencing across runs that can land in different bands. Round 1's −40% was
   band luck.

   The band distribution also drifted with server state over the session:
   - `set100`/64 fell from 5.6k ops/s (campaign) to 3.5k ops/s (ladder, 1 h later)
     for every arm alike;
   - one `set100`/256 run of today's design ran at 14k ops/s against 21.7k for the
     rest of that cell.

   The write cells measure the server pipeline, not the client.
3. **The narrower variants.**
   - **(a), one writer goroutine per process:** each connection keeps its
     `readLoop`; sends are queued to the writer without waiting for a write
     acknowledgment. At `get100`/64 it captures **all of B's client CPU gain**
     (0.912–0.915× CPU/op, +4–5% throughput) for **+2–4% server cycles instead of
     +7–10%**. With one writer goroutine per connection and the out-buffer fix
     ("Wbest"), the server cost is **+0.5%** with +5.5% throughput and −8%
     CPU/op.
   - **(b), the delay timed from the sender:** captures none of the CPU gain
     (+1% CPU/op, +1.6% throughput) and reproduces part of the server cost.

   The gain is on the write side. With today's design, a profile at 256 workers
   puts about 24% of client CPU in the send and receive paths:
   - `writeContext` 9.2%: the read sends wait for their write acknowledgment
     (`sendReadFrame` → `SendFrameContextOrdered`);
   - `writeLoop` 6.2%, `readLoop` 4.6%, `sendWithoutAck` 4.5%.

   B spends about 13% there: `netLoop.run` 10.1% and `ngWrite` 3.2%. Keeping
   per-connection readers (a) loses nothing.
4. **Tests with each switch on**, under `-race`: Bazel, `//pkg/fdbgo/transport:all`
   and `//pkg/fdbgo/client:all`, 28 targets, with `--test_env` carrying the switch.
   - **Client package:** all 2,583 tests pass under B, (a) and (b), with no race
     reports. The first B run exposed two real prototype bugs:
     - a flush on a closed connection returned success, which
       `TestPipelinedGet_Resolve_FlushErrorRetries` caught;
     - `CoalesceDelay` reported that a B connection never waits, which
       `TestWithCoalesceDelayReachesEveryConnection` caught.

     Both are fixed. That failure is also the positive control showing the switch
     reached the tests.
   - **Transport package:** under (b) all 190 pass. Under B and (a), 177 pass and
     13 fail. All 13 are tests of today's writer mechanics, whose preconditions B
     removes on purpose. They assert, for example, "the sender taking the writer's
     turn did not happen", turn and tail counters, or call `SetReadBuffer` on the
     closed `net.Conn`. None fails on frame content or order.
5. **The lever ladder** is in the table below. In short:
   - At 256 workers every 8-P B and (a) variant beats today's design on throughput,
     client CPU/op, server cycles/tx and latency together.
   - The best 8-P result is **Wbest at +30% ops/s, −12% CPU/op, −8% server
     kcycles/tx and −32% p99**.
   - With 16 Ps (SMT siblings), Bbest reaches **+39% ops/s and −50% p99**, at +39%
     CPU/op.

   The ceiling is not the loop and not the servers (see "Ceiling").

**Recommendation.** One of the three is worth a real RFC: **(a)**, with fire-and-forget
sends and one writer per connection. Here is why:
- it keeps `readLoop` per connection, the C++ `connectionReader` analogue (the
  owner's ruling on the leader/follower draft);
- it has no single-goroutine ceiling;
- it captures B's whole client-CPU gain without B's server cost at 64 workers;
- it is the best 8-P arm at 256 workers.

The RFC needs a prior screen. Reads would need the no-ack send that TODO.md §0
warns moves timeout/hedge timing ("Do not extend this unqualified change to the
shared read helper"). That interaction has to be designed, not inherited from
this prototype. A screen of today's design plus a read no-ack alone (no new
writer) is the first step. Here it was attempted twice and lost to machine load
both times (below). B itself is not worth an RFC: it adds a single-core ceiling
and an fd outside the runtime poller, and (a) already has its gains. (b) is not
worth an RFC: it changes the window without saving client work.

## Round 2 prototype additions

All of these are off by default. The default code path changes only by nil
checks and one `if` per queued frame.

| switch | effect | where |
|---|---|---|
| `FDBGO_NETWORK_GOROUTINE=writer` | variant (a): the loop owns writes only; each connection keeps its `readLoop`; the socket is `dup`'d for the loop's writes and registered without EPOLLIN | `netgoroutine_linux.go`, `conn.go` |
| `FDBGO_COALESCE_FROM_SENDER=1` | variant (b): `writeLoop` shortens its delay by the time the batch's first frame spent queued (stamped where frames are queued) | `conn.go`, `netgoroutine.go`, `send_admission.go`, `held_outbox.go`, `direct_write.go`, `writer_turn.go` |
| `FDBGO_NETG_LOOPS=K` | connections spread round-robin over K loops | `netgoroutine_linux.go` |
| `FDBGO_NETG_OUTFIX=1` | the out buffer keeps its capacity across writes (fixes round 1's known inefficiency) | `netgoroutine_linux.go` |
| `FDBGO_NETG_RBUF=bytes` | per-connection read buffer | `netgoroutine_linux.go` |
| `FDBGO_NETG_FLUSH_FRAMES=N` | write as soon as N frames are pending (an adaptive cap for a longer delay) | `netgoroutine_linux.go` |
| `FDBGO_EXP_READ_NOACK=1` | today's design, reads sent without a write acknowledgment (screen not completed) | `client/read_send.go` |

Round-2 fixes to B: a flush on a closed connection returns `errConnClosed`, and
`CoalesceDelay` reports the delay a B connection applies.

## Round 2 method

- **Setup.** Same host, cluster layout and pinning as round 1: servers on CPUs
  9/10/11; client on 1–8 with `GOMAXPROCS=8`, except the 4-P arms (CPUs 1–4) and
  the 16-P arms (1–8 plus their SMT siblings 13–20). Native fdbserver 7.3.77,
  restarted on the same data. Binaries were built with `go build -trimpath` from
  this branch, go1.27.3:
  - `clientbench-go` `695170cd…` for goals 1–3;
  - `clientbench-go-ladder` `4dfb561d…` for the ladder.

  Every arm in a table comes from the same binary.
- **Load.** `run_cell.py` waits before every run until load1 ≤ 16 and records
  it. Run load averages are in the raw rows and table headers (10.2–15.9). Other
  tenants still caused bursts inside runs. Those runs remain in the data and
  appear as wide [min, max] ranges: one `get100`/64 round and one `get100`/8 round
  of ladder 2. Medians are over all runs, with no deletion.
- **Wire and server metrics** (goal 1): `wire_campaign.sh`, 2 rotated rounds × 4
  arms at `get100`/64. Each run has two parts:
  - **Server metrics:** a 2.42M-op run with 7 s idle on both sides, giving
    server trace-metric deltas (`srvmetrics.py`: cumulative `GrvProxyMetrics`,
    `ProxyMetrics` and `StorageMetrics` counters, summed `NetworkMetrics`
    intervals).
  - **Packet capture:** a 220k-op loopback capture
    (`tools/clientbench/scripts/wirecapture.sh`, tcpdump in a container on CPUs
    20–23, zero drops). `wirestats.py` reassembles the streams and counts
    packets, bytes and frames per server port and direction, and request kinds by
    file identifier. `wiretrace` supplies the GRV batch sizes.
- **Cells** (goals 2–3): `campaign2.sh`, arms A/B/W/S in a rotated Latin square:
  - `set100`/8: 8 rounds;
  - `get100`/64, `get100`/8 and `set100`/64: 3 rounds each.
- **Ladder:** `ladder.sh` with `ladder_arms.json` (11 arms) and
  `ladder2_arms.json` (combinations). The order rotates each round:
  - ladder 1: 2 rounds;
  - ladder 2: 2 rounds at 256 workers and for `set100`, 4 rounds at
    `get100`/64 and `get100`/8.
- **Tests** (goal 4): commands and logs in `raw2/tests/`. They ran on the tree
  after the two B fixes and before the ladder knobs (`LOOPS`, `OUTFIX`, `RBUF`,
  `FLUSH_FRAMES`, `READ_NOACK`) were added. Those knobs are off by default and
  have no test coverage under `-race`.

## Goal 1: wire and server metrics, `get100`/64

Median [min, max] of 2 runs per arm.

| per transaction | A: today | B: net goroutine | W: one writer (a) | S: sender clock (b) |
|---|---:|---:|---:|---:|
| client connections | 2 | 2 | 2 | 2 |
| transactions per GRV request | 22.8 [22.8, 22.9] | 21.4 [21.2, 21.6] | 22.0 [21.8, 22.3] | 22.1 [21.8, 22.3] |
| storage: client→server packets | 0.0713 [0.0706, 0.0720] | 0.0822 [0.0816, 0.0828] | 0.0831 [0.0819, 0.0843] | 0.0794 [0.0784, 0.0804] |
| storage: frames per client packet | 14.04 [13.91, 14.17] | 12.17 [12.08, 12.26] | 12.04 [11.87, 12.21] | 12.60 [12.44, 12.76] |
| storage: client→server bytes | 292.3 | 292.3 | 292.3 | 292.3 |
| storage: server→client packets | 0.0683 [0.0677, 0.0689] | 0.0782 [0.0776, 0.0788] | 0.0770 [0.0753, 0.0787] | 0.0738 [0.0723, 0.0752] |
| storage server RunLoopIterations | 0.1201 | 0.1386 | 0.1328 | 0.1301 |
| storage server ReadCalls | 0.2049 | 0.2267 | 0.2262 | 0.2168 |
| storage server WriteCalls | 0.0690 | 0.0801 | 0.0798 | 0.0750 |
| storage GetValueQueries | 1.000 | 1.000 | 1.000 | 1.000 |
| GRV proxy TxnRequestIn | 0.0440 | 0.0475 | 0.0464 | 0.0457 |
| stateless RunLoopIterations | 0.1731 | 0.1873 | 0.1819 | 0.1798 |

Spreads of the trace counters are below 0.5%. The full table, with proxy packets
and bytes, is in [raw2/tables-round2.md](raw2/tables-round2.md).

Two observations follow from this table:
- (a) times its window on the loop, as B does, and sends nearly as many storage
  packets (12.0 frames per packet). It sends fewer GRV requests than B (22.0 vs
  21.4 transactions per request), and its server counters fall between A and B.
  That matches its server cost in the cells below (+2–4%, against B's +7–10%).
- GRV RTT at the capture point is 85–86 µs p50 for every arm, against about
  460–480 µs per operation. Most of an operation's time is spent on the client:
  waiting out the GRV batch window and the coalescing delays, and in the run
  queue.

**Delay confirmation**, `get100`/64, `servercost.py`, 1 rep per line, in run order:

| arm, delay | server kcycles/tx | client µs/op | tx/s | mean µs | p99 µs |
|---|---:|---:|---:|---:|---:|
| A, default 10/20 µs | 28.6 | 27.0 | 127,665 | 501 | 978 |
| B, default | 30.9 | 24.8 | 132,882 | 481 | 900 |
| B, 15/30 µs | 29.7 | 24.5 | 128,748 | 497 | 945 |
| A, default | 28.5 | 26.8 | 126,754 | 504 | 1,011 |
| B, 15/30 µs | 29.8 | 24.8 | 128,875 | 496 | 919 |
| B, default | 31.2 | 24.7 | 132,307 | 483 | 892 |

## Goals 2 and 3: the four arms per cell

Median [min, max] over rounds. X/A is the median within-round ratio to today's
design.

| cell | metric | A | B/A | W/A (a) | S/A (b) |
|---|---|---:|---:|---:|---:|
| get100/64 (3 rounds, load 10.9–14.6) | ops/s | 127,105 [126,280, 127,275] | 1.050 [1.050, 1.053] | 1.048 [1.046, 1.051] | 1.016 [1.013, 1.021] |
| | client CPU µs/op | 27.0 | 0.917 [0.916, 0.922] | 0.915 [0.914, 0.918] | 1.009 [1.005, 1.013] |
| | server kcycles/tx | 28.5 | 1.098 [1.093, 1.102] | 1.043 [1.038, 1.062] | 1.044 [1.043, 1.051] |
| | p50 / p99 µs | 476 / 1,017 | 0.962 / 0.889 | 0.951 / 0.937 | 0.982 / 0.981 |
| get100/8 (3 rounds, load 10.9–13.4) | ops/s | 22,999 | 0.973 [0.964, 0.973] | 0.976 [0.971, 0.977] | 1.011 [1.010, 1.019] |
| | client CPU µs/op | 39.5 | 0.975 | 0.963 | 1.019 |
| | server kcycles/tx | 64.6 | 1.045 | 1.028 | 1.040 |
| | p50 / p99 µs | 344 / 418 | 1.024 / 1.054 | 1.021 / 1.041 | 0.989 / 0.999 |
| set100/64 (3 rounds, load 10.4–11.6) | ops/s | 5,611 | 0.992 [0.957, 1.007] | 0.998 | 1.015 |
| | client CPU / server | 37.9 / 124.9 | 1.005 / 0.985 | 1.020 / 0.995 | 0.985 / 0.995 |
| set100/8 (8 rounds, load 10.2–12.8) | ops/s | 714 [456, 1,167] | 0.959 [0.593, 1.740] | 1.013 [0.628, 1.362] | 0.754 [0.559, 1.714] |
| | server kcycles/tx | 559 [266, 955] | 1.027 [0.436, 3.156] | 0.873 [0.623, 2.826] | 1.500 [0.292, 2.548] |

Errors and retries were 0 in every run.

**`set100`/8 by latency band** (long-run p50):

| arm | ~7 ms (one convoy) | ~9–11 ms | ~16 ms | ~7 ms band: ops/s | ~16 ms band: ops/s |
|---|---:|---:|---:|---:|---:|
| A | 2 | 3 | 3 | 1,043 [918, 1,167] | 548 [456, 576] |
| B | 1 | 4 | 3 | 914 | 480 [480, 506] |
| W | 2 | 3 | 3 | 988 [785, 1,191] | 463 [455, 507] |
| S | 0 | 3 | 5 | - | 455 [395, 595] |

The per-run list, per-band server cycles and client CPU are in
`raw2/tables-round2.md`. Within a band, B and W cost the same or slightly more
client CPU per op than A. No arm-level effect on convoy formation is visible.

## The lever ladder

Ratios against today's design (A) in the same round, as the median over rounds:
ops/s, client CPU/op, server kcycles/tx, p50, p99. Below 1 is better except for
ops/s. Absolute values with [min, max] are in `raw2/tables-ladder1.md` and
`raw2/tables-ladder2.md`. B variants are cumulative only where named
("best" = `LOOPS=2` + `OUTFIX`).

**`get100`, 256 workers.** A = 208.5k ops/s, 24.0 µs/op, 18.0 kcycles/tx,
p50/p99 1,116/3,566 µs (ladder 1); 202k (ladder 2).

| step | ops/s | CPU/op | server | p50 | p99 | note |
|---|---:|---:|---:|---:|---:|---|
| A, GOMAXPROCS 4 | 0.843 | 0.748 | 0.841 | 1.195 | 1.068 | |
| A, GOMAXPROCS 16 | 0.847 | 1.674 | 1.643 | 1.345 | 0.766 | today's design gets worse with more Ps |
| B | 1.146 (ladder 2: 1.171) | 0.877 | 0.882 | 0.873 | 0.878 | the loop batches more under load: server cost drops |
| B, GOMAXPROCS 4 | 0.893 | 0.676 | 0.833 | 1.126 | 1.002 | |
| B + OUTFIX | 1.179 | 0.883 | 0.891 | 0.858 | 0.768 | |
| B + 256 KiB read buffer | 1.158 | 0.883 | 0.899 | 0.865 | 0.870 | no effect |
| B + 2 loops | 1.224 | 0.937 | 0.944 | 0.822 | 0.746 | one loop per connection here (2 connections) |
| B + delay 15/30 µs | 1.121 | 0.864 | 0.870 | 0.891 | 0.934 | larger window: fewer packets, less throughput |
| B + delay 30/60 µs + flush at 16 frames | 1.113 | 0.870 | 0.858 | 0.901 | 0.883 | |
| W (a) | 1.257 | 0.888 | 0.918 | 0.808 | 0.804 | ladder 2 |
| Bbest | 1.270 | 0.878 | 0.929 | 0.819 | 0.671 | ladder 2 |
| **Wbest** | **1.302** | **0.881** | **0.924** | **0.794** | **0.681** | best at 8 Ps |
| B, GOMAXPROCS 16 | 1.241 | 1.391 | 0.925 | 0.838 | 0.601 | |
| **Bbest, GOMAXPROCS 16** | **1.388** | 1.389 | 1.027 | 0.780 | **0.501** | 280k ops/s, 9.5 client cores |

**`get100`, 64 workers.** A = 127.2k ops/s (ladder 1); 126.3k (ladder 2, 4 rounds).

| step | ops/s | CPU/op | server | p50 | p99 |
|---|---:|---:|---:|---:|---:|
| A, GOMAXPROCS 4 / 16 | 0.888 / 1.015 | 0.768 / 1.465 | 0.967 / 1.030 | 1.123 / 1.000 | 1.139 / 0.889 |
| B | 1.041 (ladder 2: 1.039) | 0.917 | 1.093 | 0.968 | 0.899 |
| B + OUTFIX | 1.044 | 0.901 | 1.070 | 0.965 | 0.892 |
| B + 2 loops | 1.077 | 0.963 | 1.100 | 0.937 | 0.839 |
| B + delay 15/30 µs | 1.014 | 0.915 | 1.057 | 0.994 | 0.910 |
| B + delay 30/60 + flush at 16 | 0.956 | 0.882 | 0.976 | 1.062 | 0.921 |
| W (a) | 1.047 | 0.912 | 1.023 | 0.951 | 0.982 |
| Bbest | 1.043 | 0.943 | 1.097 | 0.951 | 1.006 |
| **Wbest** | **1.055** | **0.920** | **1.005** | **0.943** | **0.932** |
| Bbest, GOMAXPROCS 16 | 1.100 | 1.342 | 1.110 | 0.931 | 0.786 |

**`get100`, 8 workers** (ladder 2, 4 rounds; A = 23.1k ops/s): B, W, Bbest and
Wbest are all at 0.969–0.972× ops/s, 0.977–0.994× CPU/op and 1.005–1.029×
server cycles. Bbest with 16 Ps is 0.937×, at 1.26× CPU/op. At low concurrency
none of the levers matters.

**Writes** (`set100`/64 and /256, ladder 1 and 2): every arm is within the
server-state drift described under goal 2. The levers do not move the commit
pipeline.

**Did not try, and why:**
- `writev`: B and (a) already copy every pending frame into one buffer and make
  one `write` per connection per flush.
- The waiter reading its own reply: in B the loop is the only reader, and today
  that is the rejected leader/follower design.
- Batched waiter wakes: each reply is one `runtime.ready` whether batched or not,
  and the wasted `wakep` is not reachable from user code.
- One loop per P: this workload has 2 connections, so `LOOPS=2` is already one
  loop per connection.
- A run with transaction timeouts disabled, to size the timeout-queue cost:
  clientbench refuses a 0 timeout.

## Ceiling at `get100`/256

- **Not the loop.** A CPU profile of B at 238k ops/s puts `netLoop.run` at about
  50% of one core.
- **Not the servers.** At 278k ops/s (Bbest, 16 Ps), per-process server CPU over
  the run is 0.56 (stateless), 0.12 (log) and 0.26 (storage) of a core. The
  aggregate server task-clock is 1.0–1.3 cores out of 3.
- **Not raw client CPU.** 5–5.6 of 8 client cores are busy.

The loop is latency-bound: 256 operations in flight at about 0.9 ms each. A GRV
round trip takes about 86 µs at the wire (measured at 64 workers). The rest is
client-side waiting: the GRV batch window (half the smoothed GRV latency), the
coalescing delays, and run-queue time for 256 runnable workers on 8 Ps. Doubling
Ps raises throughput by 8–9% and cuts p99 by 25–32% (B and Bbest), which is
consistent with run-queue delay.

The largest single client cost in the profile is outside transport.
Transaction-timeout arming (`armTimeoutLocked` → `timeoutQueue.newNode` +
`weak.Make`, with `runtime.lock2` on the specials lock) takes **10.7% of client
CPU** in B and 8.3% in today's design
([raw2/profiles](raw2/profiles)). That is the next lever, and it is a client
change, not a transport one.

## Read no-ack screen (not completed)

Arm N is today's design with `FDBGO_EXP_READ_NOACK=1`, against W and Wbest at
`get100`/256, /64 and /8. It ran twice. Both times other tenants drove the load
average to 40–78; one run waited 24 minutes and still measured today's design at
162k instead of about 208k ops/s. The screen was cancelled and its rows were
discarded (kept outside the repository). It is the first thing to rerun before
an RFC on (a). It separates how much of (a)'s gain is the removed write
acknowledgment and how much is the queue-to-writer handoff.

## Round 2 reproduce

1. Cluster as in [start_cluster.md](start_cluster.md); `wiretrace` built with
   `go build -o … ./tools/clientbench/cmd/wiretrace`.
2. Wire and server metrics: `ROUNDS=2 ./wire_campaign.sh`.
3. Arm cells: `PIDS=… ./campaign2.sh`.
4. Ladders: `PIDS=… ./ladder.sh` (ladder 1), then
   `ARMS_FILE=$PWD/ladder2_arms.json OUT=$PWD/raw2/ladder2.jsonl ./ladder.sh`.
5. Tables: `python3 summarize2.py raw2`, then
   `python3 summarize_ladder.py raw2/ladder.jsonl ladder_arms.json` and the same
   for ladder 2.
6. Tests: `bazelisk --output_base=… test //pkg/fdbgo/transport:all //pkg/fdbgo/client:all --@rules_go//go/config:race --test_env=FDBGO_NETWORK_GOROUTINE=1`
   (likewise with `=writer` and `FDBGO_COALESCE_FROM_SENDER=1`).
