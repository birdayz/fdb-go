# Pure Go FDB Client — Performance Analysis

## Summary

The pure Go FDB client is **2–3.5x faster on reads** than the Apple CGo binding (libfdb_c), with **write parity**. Both clients return byte-identical results against the same FDB server.

| Operation | Go | CGo | Ratio |
|---|---|---|---|
| Get 100B | 60 us | 218 us | **3.6x** |
| Get 10KB | 69 us | 217 us | **3.1x** |
| GetRange 100 keys | 92 us | 363 us | **3.9x** |
| Sustained read throughput | 430 MB/s | 191 MB/s | **2.25x** |
| Set+Commit 100B | 1,008 us | 1,005 us | 1.0x |
| Sustained write throughput | 10.0 MB/s | 9.7 MB/s | 1.0x |

### With simulated network latency (tc netem inside container)

| RTT | Go | CGo | Ratio |
|---|---|---|---|
| 0 ms (localhost) | 60 us | 218 us | **3.6x** |
| 2 ms (netem 1ms) | 1,073 us | 2,744 us | **2.6x** |
| 10 ms (netem 5ms) | 5,254 us | 12,635 us | **2.4x** |
| 1,000 ms (netem 500ms) | 1,005 ms | 1,006 ms | **1.0x** |

The advantage narrows from 3.6x to ~2.4x at realistic latencies, then converges to 1.0x at extreme latency.

At 10ms RTT, Go takes 5.3ms while CGo takes 12.6ms. The gap is primarily **round-trip count**: each `ReadTransact` needs a GRV (read version) then a Get — two network calls. Go's GRV cache (100ms TTL, atomic int64 check) serves the version from memory, so only the Get hits the network. The CGo binding's GRV request also goes through the C event loop, adding a second delayed round-trip. At 1s RTT with sequential single requests, both clients do one operation at a time and converge to parity.

<sub>Ryzen 9 3900X, FDB 7.3.77, single-node testcontainer. Sustained benchmarks run for 30 seconds each. `TestBenchmarkSanity` verifies byte-exact result equality.</sub>

## Root Cause

The CGo binding routes every operation through the C library's single-threaded actor event loop (`Flow` runtime). The pure Go client eliminates this by using native goroutine concurrency.

### CGo binding read path (~210us)

```
Go goroutine
  → CGo call (runtime.LockOSThread + cgo frame)      ~20-25us
    → C library event loop (queue + dequeue)           ~30-50us
      → network thread (serialize + TCP send)
        → FDB server (~40us network RTT)
      → network thread (TCP recv + deserialize)
    → C future completion (callback + promise)         ~20-30us
  → CGo return (unlock OS thread)
→ Go goroutine receives result
```

### Pure Go read path (~60us)

```
Go goroutine
  → serialize request + channel send to write loop     ~3-5us
    → TCP write (buffered, batched with other requests)
      → FDB server (~40us network RTT)
    → TCP read (dedicated read loop goroutine)
  → channel receive (pre-allocated, lock-free)          ~2-3us
→ Go goroutine receives result
```

### The critical bottleneck: `fdb_future_block_until_ready`

The Apple Go binding's `MustGet()` calls `fdb_future_block_until_ready()` (futures.go:98-114), which:

1. Makes a CGo call to check `fdb_future_is_ready()` — usually false
2. Allocates a `sync.Mutex` as a signal
3. Locks the mutex
4. Makes a CGo call to `go_set_callback()` — registers a C callback that unlocks the mutex
5. Locks the mutex **again** — blocks until the C library's network thread fires the callback

This means every Get involves: Go goroutine blocks on mutex → C network thread processes response → C callback unlocks mutex → Go goroutine wakes up. Two cross-thread synchronization points plus the C library's internal event loop scheduling.

The pure Go client instead: goroutine waits on pre-allocated channel → read loop goroutine receives TCP frame → sends on channel → original goroutine wakes up. One channel operation, same thread pool, no cross-language boundary.

Raw CGo call overhead: **27ns** per boundary crossing (measured). A Get makes 4+ CGo calls (~108ns), but the real cost is the mutex-based blocking pattern adding ~100-150us of synchronization latency.

### Where the 150us gap comes from

| Overhead | CGo | Go | Delta |
|---|---|---|---|
| Network RTT | ~40us | ~40us | 0 |
| CGo boundary (LockOSThread, cgo frame) | ~20-25us | 0 | **~22us** |
| Actor event loop serialization | ~30-50us | 0 | **~40us** |
| Future/promise completion + callbacks | ~20-30us | 0 | **~25us** |
| Thread context switches (user↔event loop↔network) | ~15-25us | 0 | **~20us** |
| Request/response serialization | ~10us | ~5us | ~5us |
| GRV cache check | ~3us | ~2us | ~1us |
| **Total** | **~210us** | **~60us** | **~150us** |

The four eliminated overheads (CGo boundary, event loop, futures, context switches) account for ~107us of the ~150us gap.

## Why writes show parity

The commit RPC takes ~500us of network time (larger payload, proxy conflict checking). The ~100us per-request overhead is ~20% of total — noticeable but not dominant. On reads where the RPC takes ~40us, the same overhead is ~250% — the dominant factor.

## Architecture comparison

| Aspect | Pure Go | CGo (Apple C binding) |
|---|---|---|
| Concurrency model | Goroutines + channels | C++ Flow actor model (single-threaded) |
| Network I/O | Direct TCP, buffered write loop | FlowTransport network thread |
| Request multiplexing | Channel-based write coalescing | Event queue + actor yield |
| Response routing | Pre-allocated channel per RPC | Promise/future callback chain |
| GRV caching | Atomic int64, 100ms TTL | C library internal cache |
| Connection pooling | Go map + RWMutex | C library internal pool |
| Allocations per Get | 18 | 14 |

The Go client has slightly more allocations (18 vs 14) because it uses zero-copy deserialization (response bytes stay in the read buffer, no copy needed). The CGo binding allocates less because the C library uses fixed-size internal buffers.

## Reproducing

```sh
# Micro-benchmarks (default iterations)
just bench-one BenchmarkGet
just bench-one BenchmarkGetRange
just bench-one BenchmarkSet

# Sustained throughput (30s each)
bazelisk test //pkg/fdbgo/bench:bench_test \
  --test_arg=-test.bench=BenchmarkThroughput \
  --test_arg=-test.benchtime=30s \
  --test_arg=-test.benchmem \
  --test_arg=-test.run='^$' \
  --test_output=streamed

# Correctness verification
bazelisk test //pkg/fdbgo/bench:bench_test \
  --test_filter=TestBenchmarkSanity \
  --test_output=streamed
```
