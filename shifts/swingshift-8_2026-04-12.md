# Swingshift-8 Handover

**Date:** 2026-04-12, started ~16:50, ended ~22:00 CEST
**PR:** #40

## Objective

RYW cache optimization (sorted-keys + two-pointer merge), FDB client correctness fixes (tag throttle, mid-commit proxy detection), GRV cleanup.

## What was done

### 1. RYW cache rewrite — sorted-keys + two-pointer merge (main deliverable)

**Problem:** `getRange` merge was O(N) per batch — linear scan over all writes, hashmap insert, sort. With 10K writes and a narrow scan range, 99.9% of work was wasted.

**Fix:** Lazily-maintained sorted keys index with binary search:
- `hasWritesInRangeLocked`: O(N) → O(log N) — 73ns with 10K writes
- `mergeBatch`: O(W + S log S) → O(k + S) — two-pointer merge, only processes writes in range
- `addClearedRangeLocked`: O(N) → O(log N) — binary search insertion, in-place merge
- `clearRange` writes deletion: O(N) → O(log N + k) — binary search on sorted keys
- Lazy `serverValues` map: skip allocation when no atomics (1.8x faster, 5x fewer allocs)

**Benchmarks (before → after):**

| Benchmark | ns/op | allocs |
|---|---|---|
| MergeBatch (10 writes, 50 server) | 5,411 → **2,944** | 66 → **13** |
| MergeBatch (10K out-of-range, 5 in-range) | 2,482 → **1,419** | 31 → **8** |
| HasWritesInRange (10K writes) | ~10,000+ → **73** | N → **0** |
| AddClearedRange (1000 ranges) | ~1,000+ → **153** | N → **2** |

**Bug found by edge case tests:** Two-pointer merge didn't filter server entries when atomics resolved to "clear" (CompareAndClear). Fixed with `atomicCleared` tracking.

**Bug found by reviewer:** `sortedKeys` not invalidated in `atomic()` when cleared-key resolves to new write, and in `get()` when atomic resolves to clear. Full audit of all 12 modification sites.

7 benchmarks + 14 edge case tests + 1 reviewer-requested reproducer test.

### 2. Tag throttle fix — 500x improvement at 100 TPS

**Bug:** `throttleDuration()` returned the full remaining time until expiry. At 100 TPS with 5s remaining, it waited 5 seconds instead of 10ms.

**Fix:** Return `1/tpsRate` (one TPS slot), capped by remaining. Matches C++ TransactionTag throttle behavior. 2 new test cases.

### 3. Mid-commit proxy change detection (C++ onProxiesChanged)

**Problem:** If proxy topology changes during a commit wait, we waited for the full RPC timeout (~15s) before returning `commit_unknown_result`.

**Fix:** Broadcast channel (`proxiesChanged`) closed on every topology change. Commit monitors this alongside the reply channel. Detected immediately instead of timeout. Matches C++ `onProxiesChanged`.

**Review fix:** Channel captured before `SendFrame` (not after), matching C++ dispatch order. 7 RPC tests + 1 broadcast test.

### 4. GRV cleanup

- Extracted `applyGRVReply()` — deduplicated identical state mutation code in `flush()` and `backgroundRefresher()`
- Removed dead `proxiesGen` field (atomic.Uint64 incremented but never read)

### 5. Location cache deduplication

Extracted shared `queryLocations()` from `refresh()` and `refreshRange()` — both had ~100 lines of identical load-balance loop code. Now 5-line wrappers. -90 lines, zero behavioral change.

### 6. Performance analysis and benchmarks

- Sustained throughput benchmarks: Go 430 MB/s vs CGo 191 MB/s reads (30s sustained)
- `TestBenchmarkSanity`: byte-exact correctness verification for all benchmarked operations
- Root cause analysis: `fdb_future_block_until_ready` uses `sync.Mutex` for cross-thread signaling between Go goroutines and C network thread — 2 context switches per Get
- Raw CGo call overhead measured: 27ns per boundary crossing
- Written `pkg/fdbgo/bench/PERFORMANCE.md` with full breakdown

### 7. Code cleanup

- Location cache: deduplicated `refresh`/`refreshRange` into shared `queryLocations` (-90 lines)
- Fixed `time.After` goroutine leaks in 4 backoff selects (location cache, bootstrap, runner)

### 8. Vollkonti shift system improvements

- Date in filenames: `swingshift-8_2026-04-12.md`
- Active-shift check: `gh pr list --state open` before starting new shift
- Actual timestamps instead of planned windows
- Fixed sort command for shift number extraction

## Current state

- **Branch:** `swingshift-8`, PR #40 (reviewer-approved)
- **All 13 Bazel test targets pass** (cached)
- **Binding stress:** 100/100 API (1000 ops) + 10/10 API (5000 ops) + 50/50 directory (500 ops) = 225K ops, 0 failures
- **Fuzz testing:** All 9 fuzz targets, 342 million executions, 0 crashes
- **Race detector:** Clean on RYW tests
- **CI:** Was red due to Hetzner Object Storage outage — now resolved (~19:30 CEST). Next push should go green.

## Known issues

- Hetzner Object Storage outage makes CI report upload fail. The upload step needs `continue-on-error: true` once the outage resolves (so future outages don't block merges)
- `proxyTagThrottledDuration` accumulated but not sent back to proxy (LOW, tracked in TODO.md)
- `bufio.Reader` on connection read path breaks fault injection tests (read-ahead buffers the reply before killReads arms) — documented, not implemented

## What to work on next

### High priority
- **DatabaseContext refactor** — started with GRV dedup + dead code removal. Next steps: extract TransactionDefaults struct, consolidate load-balance retry pattern between GRV and location cache
- **Hetzner CI fix** — add `continue-on-error: true` to upload step once outage resolves

### Medium priority
- **commitDummyTransaction** — defense-in-depth for commit_unknown_result. Self-conflicting mechanism is primary safety net, but C++ also runs a dummy transaction to confirm original commit status
- **GRV/location onProxiesChanged** — commit path now detects mid-commit proxy changes; GRV and location cache still have one-extra-cycle delay when topology changes mid-retry

### Low priority
- **Frame read buffer pooling** — `ReadFrame` allocates per response. Pooling blocked by zero-copy design (consumers hold slices into buffer). Would need refactored deserialization
- **Smoother-based throttle capacity** — current `1/tpsRate` is correct for single-transaction delay; Smoother would smooth rate updates across GRV replies for sustained capacity estimation
- **secondDelay speculative requests** — C++ sends hedge request to second server after delay
