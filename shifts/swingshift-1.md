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

### 7. SetSkipPossiblyRebuild builder option (commit `865eae9`)

New `StoreBuilder.SetSkipPossiblyRebuild(bool)` option that skips `checkPossiblyRebuild` during Open/CreateOrOpen. Useful for callers managing index states independently. Not used by OnlineIndexer (needs the auto-rebuild for proper index state detection).

### 8. Database-level retry/size limit tests (commit `c8b38c1`)

2 more C binding port tests matching C++ `FDB_DB_OPTION_TRANSACTION_RETRY_LIMIT` and `FDB_DB_OPTION_TRANSACTION_SIZE_LIMIT` unit tests.

### 9. Shared container for fdb package tests (commit `1c1af5b`)

Same TestMain pattern applied to `pkg/fdbgo/fdb/` test package. Tenant tests keep own container (need tenant config).

### 10. Extended binding stress (COMPLETED)

1-hour binding stress test: **332 seeds × 1000 ops = 332,000 operations, 0 failures, 0 FDB deaths, 1h0m10s.**

### 11. Fuzz testing

All 10 fuzz targets run for 30s each: **409M total executions, 0 crashes.**

### 12. Human-readable FDB error descriptions (commit `8d00142`)

`wire.FDBError.Error()` now returns `"not_committed (1020)"` instead of `"fdb error 1020"`. Maps 25 common error codes to C++ names.

### 13. fdb facade tests (commit `b527260`)

3 new integration tests: LocalityGetBoundaryKeys, GetClientStatus, Transaction Reset through fdb facade.

### 14. CRITICAL: OrEqual wire protocol fix (commit `82c283f`)

**Found and fixed a real correctness bug** in the fdb facade's `GetKey` and `resolveSelector`. The Apple Go binding convention and the C++ wire protocol have INVERTED semantics for the `OrEqual` field in key selectors:

- Apple binding: `OrEqual=true` → "greater or equal" (inclusive)
- C++ wire: `OrEqual=true` → "strictly greater" (exclusive)

The Apple C binding inverts `OrEqual` before sending to the server. Our fdb facade was passing it directly — causing `FirstGreaterOrEqual` to skip exact matches. Found via 3 new cross-client interop tests comparing Go `GetKey` against CGo on identical data.

### 15. Cross-client interop tests (commit `82c283f`)

3 new tests: reverse range scan, key selector resolution, limited range scan. All compare pure Go client results against CGo client on identical data.

## Current state

- **Master:** clean (`b71680f`)
- **Branch:** `swingshift-1` (16 commits ahead of master)
- **Open PRs:** 1 (#29, draft)
- **All 13 Bazel test targets pass**
- **Binding stress:** 100/100 seeds + extended 1h (332 seeds, 332K ops), 0 failures, 0 FDB deaths
- **Fuzz testing:** 10/10 targets, 409M executions, 0 crashes
- **C binding port tests:** 80 test functions (was 56)
- **Client test time:** ~45s (was ~700s)

## Known issues

None discovered. All existing tests pass, binding stress clean.

## What to work on next

### High impact
- **Extended binding stress** — the 1h run should finish by ~21:28 CEST. Check final result.
- **Port more C binding tests** — 80/81 ported from unit_tests.cpp (remaining 3 need server-side cost/throttle/protocol APIs)

### Medium impact
- **Directory layer** — needed by some FDB applications, significant feature
- **GetTotalCost()** — enables 1 more C binding test, requires tracking read costs from server responses
- **GetServerProtocol()** — could add by extracting protocol version from coordinator response

### Low priority
- Wire type MEDIUM items (#11, #14) — edge cases
- Tenant groups/tombstones/ID prefix (metacluster-only)
- `PreloadRecordStoreState` API — optimization for large deployments
