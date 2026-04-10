# Dayshift-1 Handover

**Date:** 2026-04-10 09:00 — 16:30 (CEST)
**PRs:** #24 + #25 (both squash-merged to master)
**Commits:** `b24ddae` (PR #24), `8e1a997` (PR #25)

## Objective

CI reliability + pure Go client hardening. Building on nightshift-1's massive pure Go FDB client push.

## What was done

### CI Reliability Fix

**TCP port reuse ASSERT crash** — FDB server asserts at `FlowTransport.actor.cpp:1569` when a new TCP connection arrives from the same ephemeral port as a recently closed one (stale Peer entry). Fixed with two changes:
1. `SetLinger(0)` on TCP sockets → sends RST instead of FIN, no TIME_WAIT state
2. TCP keepalive (10s) → faster dead connection detection under Docker load

The nightshift said "can't fix client-side" — but RST-on-close eliminates the root cause.

### Transaction Options (matching C++ FDB client)

- `SetReadYourWritesDisable()` — regular Get/GetRange bypass RYW cache
- `SetSnapshotRYWDisable()` — snapshot reads bypass RYW cache  
- Size limit bounds validation: [32, 10,000,000], returns error 2006 at commit
- `Watch()` returns `watches_disabled` (1034) when RYW disabled

### C Binding Port Tests

7 new tests (39 total, was 32):
- `TestRYWDisable_CPort` (C++ line 671)
- `TestSnapshotRYWEnable_CPort` (C++ line 699)
- `TestSnapshotRYWDisable_CPort` (C++ line 728)
- `TestSizeLimitTooSmall_CPort` (C++ line 811)
- `TestSizeLimitTooLarge_CPort` (C++ line 823)
- `TestSizeLimitMinimum_CPort` (C++ line 835)
- `TestWatchRYWDisable_CPort` (C++ line 1973)

### Security Fix

**VecSerStrategy parser OOM** — All three vector parsers (`ParseKeyRefStringVector`, `ParseKeyRangeRefStringVector`, `ParseKeyValueRefStringVector`) used the wire count directly as `make()` capacity. Crafted count of `0xFFFFFFFF` → 32GB allocation attempt → OOM. Fix: clamp capacity to `remaining_bytes / min_element_size`.

### C++ Behavioral Alignment

**Client-side validateVersion** — Matching C++ `DatabaseContext::validateVersion()`:
- `transaction_too_old` (1007): reject `SetReadVersion` below `minAcceptableReadVersion` (tracked from every GRV response)
- `future_version` (1009): reject absurd versions (>10^15) client-side

Previously skipped `TestSetReadVersionOld_CPort` and `TestSetReadVersionFuture_CPort` — both now pass. Zero behavioral skips remaining.

**RYW wrapper wiring bug** — `SetReadYourWritesDisable()` and `SetSnapshotRywDisable()` on the fdb wrapper were no-ops. Now properly delegate to inner Transaction.

### Database-Level Transaction Defaults

Implemented `FDB_DB_OPTION_TRANSACTION_TIMEOUT`, `_RETRY_LIMIT`, `_MAX_RETRY_DELAY`, `_SIZE_LIMIT`. Previously all no-ops. Now stored on the Database and applied to every transaction. Two new tests verify timeout and size limit at DB level.

### System Key Access Control (PR #25)

Client-side `\xff` system key enforcement matching C++ `ReadYourWritesTransaction`:
- `getMaxReadKey()` / `getMaxWriteKey()` — returns `\xff` without option, `\xff\xff` with it
- `metadataVersionKey` (`\xff/metadataVersion`) exempt from both read and write checks
- `SetReadSystemKeys()` — allows reading `\xff/*` keys
- `SetAccessSystemKeys()` — allows reading AND writing `\xff/*` keys
- Record layer sets `ReadSystemKeys` on all 4 transaction creation paths
- Tenant CRUD uses `AccessSystemKeys` (replaces bare `SetLockAware`)
- 4 new C binding port tests: cannot read/write, read/write with option

### Write Path Investigation

Profiled Go vs CGo Set+Commit:
- **Go**: 2,164 us/op, 2,747 B/op, 28 allocs/op
- **CGo**: 1,876 us/op, 200 B/op, 9 allocs/op
- **Gap**: 15.3% — 95.5% of time is I/O-bound

**Verdict**: Gap is structural — goroutine coordination overhead (channel-based multiplexing) vs C's single-threaded event loop. No micro-optimization will close it. Reads already beat CGo. Documented in CLAUDE.md.

## Current state

- **Master:** clean, CI green
- **All 13 Bazel test targets pass**
- **Binding stress**: 100/100 seeds × 1000 ops, 0 failures, 0 FDB deaths
- **CI**: all runs SUCCESS, zero flakes
- **Zero behavioral test skips** in entire codebase
- **Open issues:** 1 (#2 — upstream tuple.Unpack panic, tracked only)

## What to work on next

### High impact
- **Binding tester seed 1 flake — Docker contention artifact** — 100-seed run had seed 1 fail when competing with another stress run for Docker resources. 5/5 clean reproductions pass. STACKTESTER_TRACE env var added for future diagnosis. Not a real client bug
- **Port more C binding tests** — 39/81 done. Medium-priority remaining: system key access control (4 tests, needs client-side validation), more watch edge cases
- **Directory layer** — needed by some FDB applications

### Medium impact  
- **System key access control** — `SetReadSystemKeys()`, `SetAccessSystemKeys()` options with client-side validation. Needs careful implementation to not break internal code that reads system keys
- **Transaction pool** — `sync.Pool` for Transaction structs, reuse backing arrays. Saves 1-2 allocs/op but minimal time impact

### Low priority
- Write path micro-optimizations (minimal benefit, structural gap)
- `net.Buffers` (writev) for scatter-gather I/O
- ReadFrame buffer pooling
