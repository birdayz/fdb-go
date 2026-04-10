# Swingshift-1 Handover

**Date:** 2026-04-10 19:00 — 20:30 (CEST)
**PR:** #29 (draft)
**Branch:** `swingshift-1`

## Objective

Port remaining C binding tests, extended binding stress testing, improve test infrastructure.

## What was done

### 1. Transaction.Reset() (commit `7d453c7`)

Public `Reset()` API matching C++ `ReadYourWritesTransaction::reset()`. Clears all state including retry count/backoff (unlike internal `reset()` used by OnError). Options are preserved across Reset. 4 new tests.

### 2. Versionstamp offset validation (commit `7d453c7`)

**Security fix.** C++ validates SET_VERSIONSTAMPED_KEY/VALUE offsets in `atomicOp()`, returning `client_invalid_operation` (2000) for invalid positions. Without this, invalid offsets cause buffer overflow on the FDB server. We defer validation to `Commit()` since `Atomic()` is void. 3 new tests.

### 3. Database-level transaction defaults (commit `8615792`)

Matches C++ `DatabaseContext::transactionDefaults`. All new transactions inherit:
- `SetTransactionTimeout(ms)`
- `SetTransactionRetryLimit(retries)`
- `SetTransactionMaxRetryDelay(ms)`
- `SetTransactionSizeLimit(limit)`
- `SetDefaultReadSystemKeys()` — allows \xff reads on all transactions
- `SetDefaultAccessSystemKeys()` — allows \xff reads+writes on all transactions

2 new tests (database-level system keys, database-level timeout).

### 4. Test infrastructure: shared FDB container (commit `e54d800`)

**15x speedup.** Added `TestMain` in client package that starts a single FDB testcontainer shared across all 102 test functions. Each test still creates its own `Database` connection for option isolation, but shares the container.

- Before: 96 tests × ~30s/container = ~700s (4 parallel)
- After: 96 tests × ~0.5s/connect = ~45s (4 parallel)

Pre-commit hook time dropped from ~12min to ~2min.

### 5. C binding port tests: 56 → 78 (commits `7d453c7`, `8615792`, `f8c9a74`)

22 new test functions:
- **Versionstamp**: invalid key/value offsets, too-short value, valid boundary
- **Transaction Reset**: basic reuse, retry count clear, read version clear, cancel→reset
- **Infrastructure**: GetLocations, write-write conflict detection
- **Transaction reuse**: auto-reset after commit, database-level AccessSystemKeys
- **OnError semantics**: retry limit enforcement, non-retryable errors, non-FDB errors
- **RYW cache edge cases**: atomic ADD+Get, Clear+Get, Set+Clear+Get, Clear+Set+Get, ClearRange+GetRange, Set new keys+GetRange

### 6. Binding stress: 100/100 seeds, 0 failures

`just binding-stress` (100 seeds × 1000 ops): 0 failures, 0 FDB deaths, 17m54s.

## Current state

- **Master:** clean (`b71680f`)
- **Branch:** `swingshift-1` (5 commits ahead of master)
- **Open PRs:** 1 (#29, draft)
- **All 13 Bazel test targets pass** (total test time ~90s with shared container)
- **Binding stress:** 100/100 seeds × 1000 ops, 0 failures, 0 FDB deaths
- **C binding port tests:** 78 test functions (was 56)
- **Client test time:** ~45s (was ~700s)

## Known issues

None discovered. All existing tests pass, binding stress clean.

## What to work on next

### High impact
- **Port more C binding tests** — 78/81 ported (remaining: `fdb_transaction_get_total_cost` needs server-side cost tracking, `fdb_transaction_get_tag_throttled_duration` needs throttle metrics, `fdb_database_get_server_protocol` needs protocol version API)
- **Binding stress-duration** — run `just binding-stress-duration 2h` for extended reliability validation

### Medium impact
- **Directory layer** — needed by some FDB applications, significant feature
- **GetTotalCost()** — enables 1 more C binding test, requires tracking read costs from server responses
- **OnlineIndexer skip-rebuild option** — `online_indexer.go:677` TODO: store builder option to skip `checkPossiblyRebuild` (matches Java's `IndexMaintenanceFilter.NONE`)

### Low priority
- Wire type MEDIUM items (#11, #14) — edge cases
- Tenant groups/tombstones/ID prefix (metacluster-only)
- `PreloadRecordStoreState` API — optimization for large deployments
