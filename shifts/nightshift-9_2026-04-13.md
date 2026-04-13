# Nightshift-9 Handover

**Date:** 2026-04-13, started ~22:40 2026-04-12, ended ~02:30 CEST
**PR:** #42

## Objective

FDB client correctness/completeness audit, TODO.md restructure, coverage infrastructure, bug fixes from audit findings.

## What was done

### 1. FDB Client correctness audit (main deliverable)

Full audit of Go FDB client against C++ NativeAPI.actor.cpp and C binding API (fdb_c.h).

**API completeness:** 100% data-path coverage. All `fdb_transaction_*` read/write/atomic/watch/conflict/versionstamp functions implemented. 7 missing functions are observability/admin only (mapped_range, get_tag_throttled_duration, get_total_cost, force_recovery, create_snapshot, get_main_thread_busyness, get_server_protocol).

**Bugs found and fixed:**
- **getKey selector resolution across shard boundaries** — Go sent ONE GetKeyRequest and returned the reply key, ignoring `orEqual` and `offset` from the KeySelector in the reply. C++ loops until `offset==0 && orEqual==true`. Multi-shard clusters would get wrong keys for selectors crossing shard boundaries. Fixed: full resolution loop.
- **hot_shard/range_locked backoff cap** — Used DEFAULT_MAX_BACKOFF (1s) instead of RESOURCE_CONSTRAINED_MAX_BACKOFF (30s). Over-aggressive retry under hot-shard conditions.
- **future_version delay ignoring maxRetryDelay** — Used flat 10ms instead of `min(FUTURE_VERSION_RETRY_DELAY, maxRetryDelay)`.
- **GRV cache per-priority ratekeeper check** — Checked only `lastRkDefault` for all priorities. Now BATCH checks `lastRkBatch`, DEFAULT checks `lastRkDefault`.
- **Watch cancellation on Reset/Cancel** — C++ cancels pending watches via resetPromise. Go now has lazy `watchCtx`/`watchCancel` on Transaction.

**10 behavioral divergences documented** (6 are design choices/cosmetic/perf, 4 fixed).

### 2. commitDummyTransaction (C++ NativeAPI.actor.cpp:4225)

Defense-in-depth synchronization barrier for commit_unknown_result. After MAYBE_COMMITTED errors, runs a dummy transaction that conflicts with the original to confirm it's no longer in-flight. Review found 2 bugs (ReadSnapshot=0 crash, conflict range loss on retry) — both fixed. 3 unit tests added. `isDummy` flag prevents recursion. Loop until ctx cancellation (matching C++).

### 3. onProxiesChanged for GRV/location

GRV `sendGRVRequest` and location `queryLocations` retry loops now listen for `proxiesChanged` in backoff selects. Immediate wake-up on proxy topology change instead of waiting for full exponential backoff. Commit path already had this (swingshift-8).

### 4. TODO.md restructure

From 2282 lines (93% completed items) to ~150 lines with clean sections:
- Pure Go FDB Client (bugs, features, performance, tests, divergence table, missing C API)
- Record Layer (bugs, features, performance, tests)
- Future: Query Planner + SQL Layer
- Infrastructure / CI

### 5. Test coverage infrastructure

- `test-report` tool now accepts `-coverage` flag (LCOV file)
- HTML report includes overall coverage % in summary bar + per-package breakdown table
- CI updated: `bazel coverage` instead of `bazel test` (produces both test results and LCOV)
- `just coverage` recipe generates full report with coverage
- **Current coverage:** 72.4% client, 78.8% record layer

### 6. Test key isolation

All test files using hardcoded FDB keys now use `t.Name()` prefixes:
- `correctness_test.go`: 30 tests (166 insertions)
- `setget_test.go`: 2 tests
- `fault_test.go`: 6 tests
- `fdb_test.go` (facade): 19 tests (87 insertions)

### 7. Other improvements

- `TransactionDefaults` struct extracted from loose `txDefault*` fields
- `sleepCtx` helper: 7 retry delay sites now context-aware
- `SetTag` wired through facade to client layer
- `commitDummyTransaction` loops until ctx cancellation (not hardcoded 10)
- `isRetryable()` helper for error classification
- CI: `continue-on-error` on Hetzner Object Storage upload
- README + RFC 015 updated for commitDummyTransaction + onProxiesChanged
- CLAUDE.md conformance status updated with audit results

## Current state

- **Branch:** `nightshift-9`, PR #42 (reviewer-approved after 1 round)
- **All 13 Bazel test targets pass**
- **Binding stress:** 30 seeds × 1000 ops (post-fix) + 1-hour soak (281+ seeds, 0 failures) + 20 directory seeds (0 failures)
- **Fuzz testing:** ~330M total executions across all 10 fuzz targets, 0 crashes
- **Race detector:** Full client test suite passes with `-race` (0 data races)
- **CI:** Green on all pushes
- **Line coverage:** 72.4% client, 78.8% record layer

## Known issues

- `bazel coverage` and `bazel test` have separate cache namespaces (different compilation flags). Running one invalidates the other's cache. For CI this is fine; for local dev, `just test` stays fast.
- The getKey shard resolution fix can't be tested in single-node testcontainers (single shard). Would need multi-node test infrastructure.

## What to work on next

### HIGH
- **Multi-shard test infrastructure** — Single-node testcontainers can't test cross-shard behavior. Need a multi-node FDB cluster testcontainer (or at least a way to force shard splits in single-node).
- **Increase client coverage from 72% to 80%+** — Lowest files: metrics.go (57%), coordinator.go (62%), endpoint.go (66%). Mostly uncovered error/retry paths that need fault injection.

### MEDIUM
- **RYW SnapshotCache** — C++ caches server reads for reuse within a transaction. Go re-fetches. Correct but more I/O. Architectural change (segment tree vs map).
- **proxyTagThrottledDuration send path** — Accumulated but not sent back to proxy in GRV request metadata.
- **Speculative second request (secondDelay)** — C++ hedge request for p99. Sequential iteration in Go.

### LOW
- **Outbound PING connection monitor** — Detect dead connections in 2s vs 10s TCP keepalive.
- **Frame read buffer pooling** — Blocked by zero-copy design.
