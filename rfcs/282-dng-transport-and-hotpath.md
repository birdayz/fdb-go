# RFC-282: Enqueue-only connection writer, configurable linger, and client hot-path optimizations

**Status:** Draft
**Reference:** FoundationDB C++ 7.3.77 (`/tmp/fdbsrc`).
**Related:** RFC-259 (GRV batching), RFC-264 (send-wait cost), RFC-269 (monitor ping ownership), PR #991 (single-network-goroutine & D-NG lab).

## Problem

In `pprof` captures (`3,600,000` post-warmup `get100` transactions at 256 workers, `GOMAXPROCS=8`, `go1.27.3 linux/amd64` on a 16-vCPU AMD EPYC-Milan `ccx43` host; PR #991 commit `35705b26a`, `docs/labs/single-network-goroutine/rawhz7/prof/`), the pure-Go client at merge-base `e14dd5bea` spends 35–40% of its CPU and 55% of its heap allocations on internal synchronization and allocation overhead that has no counterpart in `libfdb_c`:

1. **Write-acknowledgment handoff (`pkg/fdbgo/transport`):** Callers wait for `writeLoop` (or a sender-turn / direct write) to confirm the frame reached the socket before waiting for the reply. In C++, `Peer::send` (`fdbrpc/FlowTransport.actor.cpp:1038-1043`) appends the packet to `unsent` and triggers `dataToSend` only when `firstUnsent` is true; `RequestStream::getReply` (`fdbrpc/include/fdbrpc/fdbrpc.h:742`) returns a `Future<Reply>` immediately, and `connectionWriter` (`fdbrpc/FlowTransport.actor.cpp:704-738`) drains `unsent` asynchronously. Waiting for a write acknowledgment adds a second wakeup per RPC (~9% of client CPU, `0.30` vs `0.06` futex/op).
2. **Rigid timer-based coalescing vs. adaptive linger:** C++ `connectionWriter` waits `max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now() - lastWriteTime))` (`10/20 µs`, `fdbrpc/FlowTransport.actor.cpp:708-710`, `flow/Knobs.cpp:211-212`; `BUGGIFY` sets both to `0`, confirming zero delay is wire-valid). In C++, actors share one network thread (`Net2::run`, `flow/Net2.actor.cpp:1506`), so frames accumulate while the thread runs other actors. While Go already exposes `WithCoalesceDelay(min, max)` (including `WithCoalesceDelay(0, 0)` for immediate writes) across `transport`, `client`, and `fdb`, it lacks a Nagle-style adaptive linger mode that flushes idle connections immediately (`sinceLastWrite >= window`) and yields (`runtime.Gosched()`, zero timer syscalls) only during active bursts (`sinceLastWrite < window`).
3. **Unsharded connection reply map (`Conn.pendingMu`):** A single `sync.RWMutex` guards `c.pending` across `PrepareReplyTo`, `Cancel`, and `readLoop`, accounting for **76.7% of all client mutex contention** (`10.65 s` cumulative delay over 3.6M `get100/256` ops), compounded by `readLoop` calling `time.Now()` inside the lock.
4. **Per-transaction timeout node allocation and shared-heap churn:** `ReadYourWritesTransaction::resetTimeout` (`fdbclient/ReadYourWrites.actor.cpp:1567-1578`) runs one `timebomb` actor when `TIMEOUT > 0` (default `0` = `Void()`). On `master` (`pkg/fdbgo/client/read_incarnation.go:215-225`, `timeout_queue.go:45-107`), `armTimeoutLocked` allocates a new `&timeoutNode{tx: weak.Make(tx)}` (including a runtime weak-pointer handle) on every new `Transaction` and locks one of 16 shared `timeoutQueue` shards (`q.mu`) to push/sift the node into `q.heap`, while `disarmTimeoutLocked` (`read_incarnation.go:233-235`) only flips `tx.timeoutArmed = false` and leaves expired/completed `timeoutNode`s in `q.heap` until their deadline arrives. Under `Transact` with `-timeout-ms=5000`, `weak.Make(tx)`, `&timeoutNode` allocation, and shared-heap operations consume **4.4% of total client CPU (`5.71 s`)** and 2 allocs/tx even when transactions finish in ~1 ms.
5. **Hot-path allocations and lock contention in `client`:**
   - `GetPipelined` allocates a `map[*PendingGet]struct{}` (`tx.pendingReads`) even for single-read transactions, eagerly acquires a `5 s` `time.Timer` before `PendingGet.resolve` checks if the reply is already ready, and `snapshotCache.insertPoint` allocates map/slice headers on a single-key read.
   - `latencySketch.addSample` (`pkg/fdbgo/client/ddsketch.go`, port of `DDSketch.h:86`) computes `math.Log` under a single database-wide `sync.Mutex` (**11.4% of mutex contention**).
   - `cachedOrFetchReadVersion` (`pkg/fdbgo/client/read_version_state.go`) allocates `&pendingReadVersion`, `make(chan struct{})`, and `make(chan grvResult, 1)` on every transaction (3 allocs/tx) even when only one goroutine awaits the transaction's GRV.
   - `Transact`'s final commit runs `startTurnover().finish()`, allocating a successor `readIncarnation`, span, and `versionstampCompletion` on a `Transaction` that is discarded immediately after `Transact` returns (9 allocs/tx).

## Proposed Change

### 1. Enqueue-only per-connection writer (`D-NG`) and configurable linger (`pkg/fdbgo/transport`)
- **Enqueue-only send (`T1`, `T2`):** `naWriter` maintains a double-buffered contiguous byte slice (`w.q` ↔ `spare`, pre-sized to 64 KiB) guarded by `w.mu`. Small frames (≤ 4 KiB) are stack-encoded (`var stackBuf [4128]byte`) before acquiring `w.mu` (or encoded directly into `w.q`) so `w.mu` holds only a `copy` and slice advance, with zero `writeFramePool` traffic. `w.sig` (`chan struct{}`, cap 1) is signalled only on an `empty → non-empty` transition (`len(w.q) == 0`, matching `firstUnsent` in `Peer::send`, `FlowTransport.actor.cpp:1042-1043`).
- **Bounded high-water backpressure:** To bound memory if a peer stalls its TCP receive window before `connectionMonitor` (`2 s`) closes the connection, `naSend` blocks when `len(w.q) >= 16 MiB` (`naHighWater`) until `naLoop` drains `w.q`, `ctx.Done()`, or `c.ctx.Done()`. Once a frame is appended to `w.q`, cancelling the caller's `ctx` ends only the caller's reply wait and never un-queues or truncates a frame (`FlowTransport.actor.cpp:1038-1043`).
- **Write errors:** `naLoop` writes `buf` in full; any `conn.Write` error calls `c.failConnection(codedConnClosed(err))`, failing all pending replies with FDB code `1030` (`request_maybe_delivered`, `fdbrpc.h:794-799`).
- **Configurable linger (`WithCoalesceDelay` and new `WithAdaptiveLinger`):**
  - Existing `WithCoalesceDelay(minDelay, maxDelay)` (`transport.DialOption`, `client.Option`, `fdb.Option`) is wired into `naWriter`: it sets `minCoalesce, maxCoalesce = max(minDelay, 0), max(maxDelay, 0)` and clears `adaptiveLinger`. When `min > 0 || max > 0`, `naLoop` waits `coalesceWait(time.Since(c.lastWrite))` via `hrtimer` (`max(min, max - sinceLastWrite)` jittered `0.9–1.1`, `FlowTransport.actor.cpp:708-710`) before draining `w.q`. When `0, 0` (the `D-NG` default), `naLoop` flushes immediately on wakeup.
  - New `WithAdaptiveLinger(window time.Duration)` (`transport.DialOption`, `client.Option`, `fdb.Option`, and `tools/clientbench -adaptive-linger`): configures Nagle-style adaptive linger (`adaptiveLinger = max(window, 0)`, clearing `minCoalesce, maxCoalesce`). If `time.Since(c.lastWrite) >= window` (idle connection), `naLoop` flushes immediately; if `time.Since(c.lastWrite) < window` (active burst), `naLoop` yields (`runtime.Gosched()`, zero timer syscalls) up to `c.lastWrite.Add(window)` before draining `w.q`.
  - The last option applied wins; `Conn.CoalesceDelay()` and new `Conn.AdaptiveLinger()` report the active configuration. `WithLegacyWriter()` retains the legacy `writeLoop` for unit tests of legacy writer-turn/write-ack internals.

### 2. 32-shard `Conn.pending` reply table & per-connection reply tokens (`pkg/fdbgo/transport`)
- Replace `Conn.pendingMu` + `Conn.pending` with 32 cache-line-padded shards (`[32]pendingShard`, `sync.Mutex` + `map[UID]pendingEntry`) indexed by `(token.First ^ (token.First >> 32)) & 31`.
- In `readLoop`, sample `recvAt := time.Now()` before acquiring `shard.mu.Lock()`.
- Mint reply tokens from a per-connection atomic counter (`c.nextReplyToken()`) seeded with `splitmix64()`, avoiding global contention while keeping reply tokens unique per connection.

### 3. Pooled `Transact`-scoped timeout timers (`pkg/fdbgo/client`)
- For `Transaction`s with `TIMEOUT` set (`ReadYourWrites.actor.cpp:1567-1578`) on a production `timeoutQueue` (no test clock/hooks installed), `designArmLocked` bypasses `q.newNode(tx)` (`weak.Make(tx)` + `&timeoutNode` + `q.heap` push) and uses one `designTimerEntry` per transaction.
- `Transact` / `ReadTransact` set `tx.transactScoped = true` and call `tx.releaseTransactTimeout()` on exit, pooling the `designTimerEntry` in `designTimerPool` without allocating `weak.Make(tx)` or `&timeoutNode` per transaction, and skipping `Timer.Reset` when the pooled timer's `nextFire` is still in the future and already at or before `deadline`. Non-`Transact` transactions (or transactions that leave `Transact` with in-flight reads) fall back to a per-transaction weak `time.AfterFunc` (`tx.designTimer` with `weak.Make(tx)`) so unreferenced armed transactions remain GC-collectable; tests that inject custom clocks or hooks continue through `timeoutQueue`.

### 4. Client hot-path allocation & contention reductions (`pkg/fdbgo/client`)
- **`GetPipelined` & `PendingGet`:** Add scalar `pendingReadFirst *PendingGet` on `Transaction` so single-read transactions avoid allocating `map[*PendingGet]struct{}`; embed `keyBuf [64]byte` in `PendingGet`; defer `getTimer(readRPCTimeout)` in `PendingGet.resolve` until after a non-blocking `select` on `p.replyCh`, using Go 1.23+ single-call `Timer.Reset`; add inline single-entry fast path to `snapshotCache.insertPoint`.
- **16-stripe `latencySketch`:** Compute `bucketIndex(v)` (`math.Log`) before locking and stripe `latencySketch` across 16 cache-line-padded shards merged in `stats()`.
- **Inline `pendingReadVersion` & pooled `grvReplyPool`:** Embed `inlinePending` on `readVersionState` with `done chan struct{}` allocated lazily under `readVersionMu` only when a second concurrent waiter joins; pool `chan grvResult` (cap 1) in `grvBatcher.getReadVersion` on the non-cancelled path.
- **Final-commit turnover skip in `Transact`:** Set `tx.transactFinalCommit = true` inside `Database.Transact` so a successful `Commit` skips allocating the unused post-commit successor `readIncarnation`, span, and `versionstampCompletion` (while `OnError` retry or user-called `Commit` outside `Transact` still runs full turnover). Move `shipReadConflicts` after the read-only fast path in `commitAdmitted` (`NativeAPI.actor.cpp:6800-6806`).

## Wire-Compatibility Impact

- **Design invariant:** Frame encoding (`encodeFrameInto`), conflict ranges, error codes (`1030` `request_maybe_delivered` on socket write error), and logical request sets are unchanged; only the client-internal handoff between sender goroutines and the per-connection writer goroutine (`naSend` → `naLoop`) and the configurable flush linger change.
- **Verification scope:**
  - Frame integrity, cancellation invariants (`T1`/`T2`), and write-error propagation (`1030`) are verified by unit and concurrency tests in `pkg/fdbgo/transport:transport_test` (`design_writer_test.go`, `conn_coalesce_test.go`).
  - Normalized logical request parity against `libfdb_c` is covered by `tools/clientbench/wiregolden:wiregolden_test` (which compares normalized logical request transcripts excluding setup traffic and non-deterministic tokens).
  - Packet coalescing boundaries on the socket depend on the configured linger mode (`WithCoalesceDelay` / `WithAdaptiveLinger`), matching `flow/Knobs.cpp:211-212` where `MIN/MAX_COALESCE_DELAY` is a transport performance knob tested at `0` under `BUGGIFY`.

## Prototype Benchmark Evidence (PR #991 Lab, Hetzner `ccx43`, Official `fdbserver` 7.3.77)

- **Trees compared:**
  - **Baseline (`Master (A)`):** merge-base `e14dd5beaa8b6c6486172b5e03786b52f32f7f8b` (`origin/master`).
  - **Prototype (`Step 7`):** commit `c645f6b47b92922f82f9934b925121e16f22dc09` (incremental steps `8582f72c4`..`c645f6b47`; full raw JSONL logs, manifests, and `pprof` profiles archived in PR #991 commit `35705b26a0c7d1e05da78e52f2360572fbf792d6` under `docs/labs/single-network-goroutine/rawhz7/`).
  - **Reference (`libfdb_c (L)`):** official `libfdb_c.so` 7.3.77 (1 network thread) driven by `tools/clientbench -backend=c`.
- **Host, toolchain, and cluster:** Dedicated Hetzner `ccx43` (16 AMD EPYC-Milan vCPUs, 64 GiB RAM, Ubuntu 24.04, kernel 6.8), `go1.27.3 linux/amd64` (`CGO_ENABLED=0` for Go arms, `GOMAXPROCS=8` pinned via `taskset -c 8-15`), against a local 5-process `fdbserver` 7.3.77 cluster (`1` stateless/GRV/commit proxy, `2` transaction logs, `2` `ssd-2` storage servers in `/dev/shm`, pinned via `taskset -c 0-7`).
- **Protocol and sample sizes:** Sequential rotated rounds (`3` rounds per workload/worker cell, rotating arm execution order each round); each run warms up for `10,000` ops (or `10 s`) with `-timeout-ms=5000 -retry-limit=10`, then measures `75,000` completed transactions/round at `get100/8`, `180,000`/round at `get100/64`, `750,000`/round at `get100/256`, and `90,000`/round at `set100/64` and `getset100/64`. Cells below report the 3-round median.
- **Measurement scope:**
  - `ops/s` and `p50 µs`: closed-loop post-warmup transaction completion throughput and median latency.
  - `CPU µs`: client process `Rusage.Utime + Rusage.Stime` delta over the post-warmup measurement window divided by completed transactions.
  - `allocs/op` and `bytes/op`: Go `runtime.ReadMemStats` `Mallocs` and `TotalAlloc` delta over the post-warmup measurement window divided by completed transactions (for `libfdb_c`, reflects only the Go cgo harness; C++ heap allocations inside `libfdb_c.so` are outside `runtime.MemStats`).

| Workload / Workers | `libfdb_c` (`L`) | Master (`A`, `e14dd5bea`) | Prototype `Step 7` (`c645f6b47`, `0 µs` linger) | Prototype `Step 7` (`adap 20 µs`) | Prototype `Step 7` (`fixed 5 µs`) |
|---|---:|---:|---:|---:|---:|
| `get100/8` ops/s (p50 µs / CPU µs) | 6,915 (1,038 / 146.3) | 5,519 (1,465 / 165.4) | **7,222 (1,057 / 119.6)** | 6,265 (1,277 / 124.0) | 6,261 (1,300 / 146.8) |
| `get100/64` ops/s (p50 µs / CPU µs) | 31,608 (1,836 / 71.7) | 49,955 (1,206 / 65.0) | 45,673 (1,384 / 61.7) | 50,044 (1,227 / 53.6) | **50,331 (1,246 / 61.1)** |
| `get100/256` ops/s (p50 µs / CPU µs) | 50,548 (4,502 / 67.2) | 164,892 (1,401 / 40.5) | **259,747 (928 / 25.6)** | **273,388 (888 / 25.0)** | 248,654 (975 / 25.6) |
| `get100/256` Go allocs/op (bytes/op) | 16.0 (664) | 50.2 (4,980) | **22.1 (3,729)** | **22.1 (3,729)** | **22.1 (3,729)** |
| `set100/64` ops/s (p50 µs / CPU µs) | 19,844 (3,150 / 70.1) | 23,132 (2,721 / 108.9) | **26,799 (2,231 / 93.1)** | 25,730 (2,323 / 93.4) | 23,490 (2,688 / 106.0) |
| `getset100/64` ops/s (p50 µs / Go allocs) | 16,001 (3,669 / 17.0) | 18,029 (3,383 / 79.6) | **19,329 (3,224 / 54.5)** | **19,528 (3,200 / 54.5)** | 18,644 (3,318 / 54.5) |

## Executable Spec & Test Plan

- `pkg/fdbgo/transport:transport_test` (`design_writer_test.go`, `conn_coalesce_test.go`):
  - `TestDesignSendIsEnqueueOnly`: proves `T1` across `0` linger, fixed `min,max` delay, and `WithAdaptiveLinger`.
  - `TestDesignCancelNeverUnqueues`: proves `T2` (`ctx` cancel after send never drops or alters queued frames; `ctx` already cancelled before send queues nothing).
  - `TestDesignWriteErrorFailsAllPending`: proves a socket write error fails all pending replies with `ErrConnClosed` / code `1030`.
  - `TestDesignConcurrentSendersFramesWholeOnce`: 16 concurrent senders × 50 frames arrive whole and in per-sender order.
  - `TestLingerOptions`: verifies `WithCoalesceDelay` and `WithAdaptiveLinger` last-wins precedence and `CoalesceDelay()` / `AdaptiveLinger()` reporting.
- `pkg/fdbgo/client`: `timeout_design_unit_test.go`, `coalesce_option_test.go`, `ddsketch_test.go`, and all 31 `transport` + `client` targets under `-race`.
