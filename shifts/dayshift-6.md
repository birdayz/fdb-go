# Dayshift-6 Handover

**Date:** 2026-04-12 06:00 — 14:00 CEST
**PRs:** #34 (merged), #35 (pending)
**Branches:** `dayshift-6` (merged), `dayshift-6b` (continuation)

## Objective

Systematic correctness audit of the pure Go FDB client against C++ FoundationDB source. Fix all correctness bugs, document all divergences, port more CGo binding tests.

## What was done

### 1. Transaction correctness audit (transaction.go vs C++ NativeAPI.actor.cpp)

**4 correctness bugs fixed:**

- **Timeout semantics**: Was per-retry (fresh deadline on each OnError reset), now overall budget from creation time. C++ anchors deadline to `creationTime` which is only updated on user-facing `Reset()`, not on internal `OnError` retries. Added `creationTime` field.

- **Resource-constrained backoff**: Proxy memory errors (1042, 1078) now use `RESOURCE_CONSTRAINED_MAX_BACKOFF` (30s) exclusively, ignoring user's `maxRetryDelay`. All other errors use user's `maxRetryDelay` or default 1s. Two mutually exclusive branches matching C++ exactly.

- **Missing retryable error**: `blob_granule_request_failed` (1079) now handled in OnError.

- **GetApproximateSize**: Now includes `sizeof(MutationRef)` (48B) and `sizeof(KeyRangeRef)` (32B) overhead per entry. Previously underestimated by ~45%.

### 2. RYW cache correctness audit (ryw.go vs C++ Atomic.h + ReadYourWritesTransaction.actor.cpp)

**2 correctness bugs fixed:**

- **doAdd result length**: Was `max(len(base), len(param))`, should be `len(param)`. C++ allocates `otherOperand.size()`. Base bytes beyond param length are silently truncated.

- **getRange local writes beyond server boundary**: When server returned `more=true`, local writes beyond the last server key were incorrectly included. Now guards against `serverBoundary`.

### 3. Read path audit (readpath.go vs C++ getExactRange)

- **LimitBytes**: Changed from `UnlimitedBytes` (0x7FFFFFFF) to 80000 (`CLIENT_KNOBS->REPLY_BYTE_LIMIT`).
- **MaxWrongShardRetries**: Bumped from 5 to 50 (C++ is unbounded, relies on tx timeout).

### 4. Other components audited (clean)

- **commitpath.go**: Clean. Tenant prefix, lock-aware flags, mutation encoding all correct.
- **grv.go**: Solid. Cache, batching, priority separation, ratekeeper tracking match C++.
- **database.go**: Transact/ReadTransact retry loop, option inheritance correct.
- **transport/**: Frame format, XXH3-64 checksum, handshake all correct.
- **failure_monitor.go**: Simple and correct.
- **coordinator.go**: Bootstrap protocol correct.
- **topology.go**: Reasonable approximation of C++ long-poll.

### 5. QueueModel divergence documented (loadbalance.go)

C++ uses continuous exponential decay (Smoother, T=2s) with server-reported penalty. We use discrete EMA (α=0.1) without penalty. Different algorithm, same functional goal. Documented in README.md.

### 6. C binding tests ported

11 new tests added (80 → 92 total):
- TestSetTimeout_OverallBudget, TestSetTimeout_ResetRestartsTimer
- TestResourceConstrainedBackoff_CPort, TestBlobGranuleRetryable_CPort
- TestRYWDoAddResultLength_CPort
- TestGetAddressesForKey_CPort, TestErrorPredicateRetryableNotCommitted_CPort
- TestGetEstimatedRangeSizeBytes_CPort, TestGetRangeSplitPoints_CPort
- TestClearRangeInverted_CPort, TestClearRangeZeroWidth_CPort
- TestPostCommitReset_CPort

All deterministic — no `time.Sleep()` in timeout tests.

### 7. Documentation

- `pkg/fdbgo/README.md`: Full divergence table (10 entries) covering all known differences from C++
- Updated test count to 92 C binding port tests
- `TODO.md`: Updated API coverage table (100% minus RebootWorker)
- `.claude/commands/vollkonti.md`: Added `--allow-empty` trick for PR creation

## Current state

- **Branch:** `dayshift-6` merged, `dayshift-6b` (15 commits ahead of master)
- **PRs:** #34 merged, #35 LGTM (3 review rounds, all items addressed)
- **All 13 Bazel test targets pass**
- **93 C binding port tests** (was 80), **25 fdb layer tests**, **16 interop tests**
- **2307 Ginkgo specs** pass (record layer)
- **430 conformance specs** pass
- **50 chaos tests** pass
- **Binding stress:** 30/30 API seeds + 3/3 directory seeds, 0 failures

### Benchmarks (no regressions)

| Benchmark | Go ns/op | CGo ns/op | Ratio |
|---|---|---|---|
| Get/100B | 57,588 | 205,885 | **0.28x** (Go 3.6x faster) |
| Set/100B | 1,006,420 | 1,007,046 | **1.00x** (parity) |

### 8. QueueModel rewrite (PR #35 / dayshift-6b)

Complete rewrite of `loadbalance.go` to match C++ `QueueModel` + `Smoother`:
- **Smoother**: continuous exponential decay (eFoldingTime=2.0s) replacing discrete EMA
- **Server penalty**: tracks penalty from server replies, counts penalty > 1.001 as "bad"
- **future_version backoff**: exponential growth 1→2→4→8s with `increaseBackoffTime` guard
- **Delta threading**: `startRequest` delta passed through to `endRequest` for balanced smoothOutstanding

### 9. Additional fixes (PR #35)

- **FutureKeyArray defer close**: goroutine missing `defer close(f.done)` — panic would hang callers
- **getKey boundary short-circuit**: matching C++ — `\xFF\xFF` with offset>0 → immediate return
- **Error descriptions**: added `blob_granule_request_failed` (1079) to description map
- **Vollkonti process**: documented "don't merge early, work until shift ends"

## Known issues

- **RYW getRange architecture**: Map-based merge with over-fetch heuristic vs C++'s segment-tree `RYWIterator`. Edge case: `serverMore=true` + all results locally cleared → `more=false`, silently truncates scan. Documented trade-off; proper iterator rewrite is a larger effort.
- **QueueModel**: Uses different algorithm from C++ (EMA vs continuous decay Smoother). Missing server penalty signal. Functionally correct but suboptimal under asymmetric load.

## What to work on next

### High priority
- ~~**QueueModel rewrite**~~ — DONE (PR #35). Smoother + penalty + futureVersion backoff + delta threading + server penalty wiring.
- **RYW getRange proper iterator** — replace map-based merge with segment-tree approach matching C++ `RYWIterator`. Fixes the truncation edge case when serverMore=true and all fetched results are locally cleared.
- **Pool frame read buffers** — `ReadFrame` allocates `make([]byte, payloadLen)` per response. Pool via `sync.Pool`. Tricky lifecycle: `body` slice references pooled buffer, consumer must release after processing. Sized pool (256B/1KB/4KB/16KB/64KB) would help.

### Medium priority
- **`getKey` boundary short-circuit** — return `""` or `\xFF\xFF` without network round trip for edge selectors.
- **Tag throttle duration tracking** — implement `cx->throttledTags` from GRV reply for proper `tag_throttled` backoff.
- **DatabaseContext refactor** — consolidate Database/GRVBatcher/LocationCache/Cluster.

### Low priority
- **`onProxiesChanged` mid-commit race** — monitor topology changes during commit for faster `commit_unknown_result` detection.
- **secondDelay speculative requests** — C++ sends a hedge request to a second server after a delay. Optimization, not correctness.
- **Multi-node testcontainer** — multiple FDB processes for multi-shard testing.
