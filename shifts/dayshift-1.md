# Dayshift-1 Handover

**Date:** 2026-04-10 09:00 — ongoing (CEST)
**PR:** #24 (draft, dayshift-1 branch)
**Branch:** `dayshift-1` (4 commits ahead of master)

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

### Write Path Investigation

Profiled Go vs CGo Set+Commit:
- **Go**: 2,164 us/op, 2,747 B/op, 28 allocs/op
- **CGo**: 1,876 us/op, 200 B/op, 9 allocs/op
- **Gap**: 15.3% — 95.5% of time is I/O-bound

**Verdict**: Gap is structural — goroutine coordination overhead (channel-based multiplexing) vs C's single-threaded event loop. No micro-optimization will close it. Reads already beat CGo. Documented in CLAUDE.md.

## Current state

- **dayshift-1 branch:** 4 commits ahead of master, CI should be green
- **All 13 Bazel test targets pass** (verified by pre-commit hooks on each commit)
- **Binding stress test**: 100/100 seeds × 1000 ops pass, 0 failures, 0 FDB deaths (17m48s)
- **CI**: 8+ runs SUCCESS (all green, zero flakes)
- **PR #24**: draft, ready for review

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
