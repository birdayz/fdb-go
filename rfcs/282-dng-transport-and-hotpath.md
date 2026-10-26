# RFC-282: Enqueue-only connection writer, configurable linger, and client hot-path optimizations

**Status:** Draft
**Reference:** FoundationDB C++ 7.3.77 (`/tmp/fdbsrc`).
**Related:** RFC-259 (GRV batching), RFC-264 (send-wait cost), RFC-269 (monitor ping ownership), PR #991 (single-network-goroutine & D-NG lab).

## Problem

Under high and low concurrency on multi-core hosts and VMs (`ccx43`, 8 client vCPUs), the pure-Go client spends 35–40% of its CPU and 55% of its heap allocations on internal synchronization and allocation overhead that has no counterpart in `libfdb_c`:

1. **Write-acknowledgment handoff (`pkg/fdbgo/transport`):** Callers wait for `writeLoop` (or a sender-turn / direct write) to confirm the frame reached the socket before waiting for the reply. In C++, `Peer::send` (`fdbrpc/FlowTransport.actor.cpp:1038-1043`) appends the packet to `unsent` and triggers `dataToSend` only when `firstUnsent` is true; `RequestStream::getReply` (`fdbrpc/include/fdbrpc/fdbrpc.h:742`) returns a `Future<Reply>` immediately, and `connectionWriter` (`fdbrpc/FlowTransport.actor.cpp:704-738`) drains `unsent` asynchronously. Waiting for a write acknowledgment adds a second wakeup per RPC (~9% of client CPU, `0.30` vs `0.06` futex/op).
2. **Coalescing delay as a non-configurable/rigid wait:** C++ `connectionWriter` waits `max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY - (now() - lastWriteTime))` (`10/20 µs`, `fdbrpc/FlowTransport.actor.cpp:708-710`, `flow/Knobs.cpp:211-212`; `BUGGIFY` sets both to `0`, confirming zero delay is wire-valid). In C++, actors share one network thread (`Net2::run`, `flow/Net2.actor.cpp:1506`), so frames accumulate while the thread runs other actors. In Go, callers run in parallel across `P`s; a fixed `hrtimer` sleep on an idle connection adds pure latency and a timer wakeup on every hop, whereas callers need configurable linger modes: immediate (`0`), Nagle-style adaptive (`window`, zero timer syscalls), or fixed `min, max` timer coalescing.
3. **Unsharded connection reply map (`Conn.pendingMu`):** A single `sync.RWMutex` guards `c.pending` across `PrepareReplyTo`, `Cancel`, and `readLoop`, accounting for **76.7% of all client mutex contention** (`10.65 s` over 3.6M ops at 256 workers), compounded by `readLoop` calling `time.Now()` inside the lock.
4. **Per-transaction timeout timer churn:** `ReadYourWritesTransaction::resetTimeout` (`fdbclient/ReadYourWrites.actor.cpp:1567-1578`) runs one `timebomb` actor when `TIMEOUT > 0` (default `0` = `Void()`). In Go, arming `SetTimeout` via `weak.Make(tx)` + `time.AfterFunc` on every `Transact` call consumes **4.4% of total client CPU (`5.71 s`)** and 3 allocs/tx even when the transaction completes in 1 ms.
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
- **Configurable linger (`WithCoalesceDelay` and `WithAdaptiveLinger`):**
  - `WithCoalesceDelay(minDelay, maxDelay)` (`transport.DialOption`, `client.Option`, `fdb.Option`): sets `minCoalesce, maxCoalesce = max(minDelay, 0), max(maxDelay, 0)` and clears `adaptiveLinger`. When `min > 0 || max > 0`, `naLoop` waits `coalesceWait(time.Since(c.lastWrite))` via `hrtimer` (`max(min, max - sinceLastWrite)` jittered `0.9–1.1`, `FlowTransport.actor.cpp:708-710`) before draining `w.q`. When `0, 0`, `naLoop` flushes immediately on wakeup.
  - `WithAdaptiveLinger(window time.Duration)` (`transport.DialOption`, `client.Option`, `fdb.Option`): configures Nagle-style adaptive linger (`adaptiveLinger = max(window, 0)`, clearing `minCoalesce, maxCoalesce`). If `time.Since(c.lastWrite) >= window` (idle connection), `naLoop` flushes immediately; if `time.Since(c.lastWrite) < window` (active burst), `naLoop` yields (`runtime.Gosched()`, zero timer syscalls) up to `c.lastWrite.Add(window)` before draining `w.q`.
  - The last option applied wins; `Conn.CoalesceDelay()` and `Conn.AdaptiveLinger()` report the active configuration. `WithLegacyWriter()` retains the legacy `writeLoop` for unit tests of legacy writer-turn/write-ack internals.

### 2. 32-shard `Conn.pending` reply table & per-connection reply tokens (`pkg/fdbgo/transport`)
- Replace `Conn.pendingMu` + `Conn.pending` with 32 cache-line-padded shards (`[32]pendingShard`, `sync.Mutex` + `map[UID]pendingEntry`) indexed by `(token.First ^ (token.First >> 32)) & 31`.
- In `readLoop`, sample `recvAt := time.Now()` before acquiring `shard.mu.Lock()`.
- Mint reply tokens from a per-connection atomic counter (`c.nextReplyToken()`) seeded with `splitmix64()`, avoiding global contention while keeping reply tokens unique per connection.

### 3. Pooled `Transact`-scoped timeout timers (`pkg/fdbgo/client`)
- For `Transaction`s with `TIMEOUT` set (`ReadYourWrites.actor.cpp:1567-1578`), `designArmLocked` uses one `designTimerEntry` per transaction.
- `Transact` / `ReadTransact` set `tx.transactOwned = true` and call `tx.releaseTransactTimeout()` on exit, pooling the `designTimerEntry` in `designTimerPool` without allocating `weak.Make(tx)` or `time.AfterFunc` per transaction, and skipping `Timer.Reset` when the pooled timer is already scheduled at or before `deadline + timeoutSlack`. Non-`Transact` transactions retain `weak.Make(tx)` so unreferenced armed transactions remain GC-collectable.

### 4. Client hot-path allocation & contention reductions (`pkg/fdbgo/client`)
- **`GetPipelined` & `PendingGet`:** Add scalar `pendingReadFirst *PendingGet` on `Transaction` so single-read transactions avoid allocating `map[*PendingGet]struct{}`; embed `keyBuf [64]byte` in `PendingGet`; defer `getTimer(readRPCTimeout)` in `PendingGet.resolve` until after a non-blocking `select` on `p.replyCh`, using Go 1.23+ single-call `Timer.Reset`; add inline single-entry fast path to `snapshotCache.insertPoint`.
- **16-stripe `latencySketch`:** Compute `bucketIndex(v)` (`math.Log`) before locking and stripe `latencySketch` across 16 cache-line-padded shards merged in `stats()`.
- **Inline `pendingReadVersion` & pooled `grvReplyPool`:** Embed `inlinePending` on `readVersionState` with `done chan struct{}` allocated lazily under `readVersionMu` only when a second concurrent waiter joins; pool `chan grvResult` (cap 1) in `grvBatcher.getReadVersion` on the non-cancelled path.
- **Final-commit turnover skip in `Transact`:** Set `tx.transactFinalCommit = true` inside `Database.Transact` so a successful `Commit` skips allocating the unused post-commit successor `readIncarnation`, span, and `versionstampCompletion` (while `OnError` retry or user-called `Commit` outside `Transact` still runs full turnover). Move `shipReadConflicts` after the read-only fast path in `commitAdmitted` (`NativeAPI.actor.cpp:6800-6806`).

## Wire-Compatibility Impact

- **None on frame bytes, conflict ranges, error codes, or request set.** Verified via 1-worker `tcpdump` capture (`1.00` GRV/tx, `2.004` proxy frames/tx, `847.6 B`/tx matching `libfdb_c`) and `tools/clientbench/wiregolden` (`blind_set` golden identical).
- Packet coalescing boundaries depend on the configured linger mode (`WithCoalesceDelay` / `WithAdaptiveLinger`), matching `flow/Knobs.cpp:211-212` where `MIN/MAX_COALESCE_DELAY` is a transport performance knob tested at `0` under `BUGGIFY`.

## Benchmark Summary (Hetzner `ccx43`, 3 Rotated Rounds, Official `fdbserver` 7.3.77)

| Workload / Workers | `libfdb_c` (`L`) | Master (`A`) | RFC-282 (`0 µs` linger) | RFC-282 (`adap 20 µs`) | RFC-282 (`fixed 5 µs`) |
|---|---:|---:|---:|---:|---:|
| `get100/8` ops/s (p50 µs / CPU µs) | 6,915 (1,038 / 146.3) | 5,519 (1,465 / 165.4) | **7,222 (1,057 / 119.6)** | 6,265 (1,277 / 124.0) | 6,261 (1,300 / 146.8) |
| `get100/256` ops/s (p50 µs / CPU µs) | 50,548 (4,502 / 67.2) | 164,892 (1,401 / 40.5) | **259,747 (928 / 25.6)** | **273,388 (888 / 25.0)** | 248,654 (975 / 25.6) |
| `get100/256` allocs/op (bytes/op) | 16.0 (664) | 50.2 (4,980) | **22.1 (3,729)** | **22.1 (3,729)** | **22.1 (3,729)** |
| `set100/64` ops/s (p50 µs / CPU µs) | 19,844 (3,150 / 70.1) | 23,132 (2,721 / 108.9) | **26,799 (2,231 / 93.1)** | 25,730 (2,323 / 93.4) | 23,490 (2,688 / 106.0) |
| `getset100/64` ops/s (p50 µs / allocs) | 16,001 (3,669 / 17.0) | 18,029 (3,383 / 79.6) | **19,329 (3,224 / 54.5)** | **19,528 (3,200 / 54.5)** | 18,644 (3,318 / 54.5) |

## Executable Spec & Test Plan

- `pkg/fdbgo/transport:transport_test` (`design_writer_test.go`, `conn_coalesce_test.go`):
  - `TestDesignSendIsEnqueueOnly`: proves `T1` across `0` linger, fixed `min,max` delay, and `WithAdaptiveLinger`.
  - `TestDesignCancelNeverUnqueues`: proves `T2` (`ctx` cancel after send never drops or alters queued frames; `ctx` already cancelled before send queues nothing).
  - `TestDesignWriteErrorFailsAllPending`: proves a socket write error fails all pending replies with `ErrConnClosed` / code `1030`.
  - `TestDesignConcurrentSendersFramesWholeOnce`: 16 concurrent senders × 50 frames arrive whole and in per-sender order.
  - `TestLingerOptions`: verifies `WithCoalesceDelay` and `WithAdaptiveLinger` last-wins precedence and `CoalesceDelay()` / `AdaptiveLinger()` reporting.
- `pkg/fdbgo/client`: `timeout_design_unit_test.go`, `coalesce_option_test.go`, `ddsketch_test.go`, and all 31 `transport` + `client` targets under `-race`.
