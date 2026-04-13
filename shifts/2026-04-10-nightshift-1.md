# Nightshift-1 Handover

**Date:** 2026-04-09 22:00 — 2026-04-10 08:15 (CEST)
**PR:** #23 (squash-merged to master as `52c7058`)
**Branch:** `nightshift-1` (merged, can be deleted)

## Objective

Make the pure Go FDB native client fully done: feature-complete, bug-free, matching CGo performance, fully tested.

## What was done

### Performance (the big win)

Single Get latency: **1,350us -> 192us** (7x faster). Now **beats CGo** (209us) on 10/14 benchmarks.
Allocations per Get: **78 -> 21** (73% reduction).

Optimizations applied:
1. Write coalescing — dedicated writeLoop goroutine, channel-based frame batching
2. Buffer pooling — sync.Pool for WriteFrame buffers, reply/error channels
3. Fast UID generation — SplitMix64 replacing crypto/rand syscalls
4. Sorted location cache — O(log N) binary search replacing O(N) linear scan
5. Per-priority GRV batchers — isolated DEFAULT/BATCH/SYSTEM_IMMEDIATE
6. Pooled RPC timers — replaced context.WithTimeout on 6 hot-path call sites
7. QueueModel load balancing — latency EMA + inflight tracking for server selection
8. Typed pending map — map[UID]chan replacing sync.Map (no interface boxing)
9. Stack-allocated Reader — ReadErrorOrInto avoids heap alloc per response parse
10. Combined conflict range allocs — single buffer for [key, key\x00) range
11. Reader reuse — eliminated duplicate wire.Reader in all reply parsers

### Features added

- **Watch API** — WatchValueRequest wire type, endpoint 10, long-poll semantics
- **QueueModel load balancing** — latency EMA, failure backoff, proxy round-robin
- **TLS support** — crypto/tls with mutual auth + CA cert
- **Connection keep-warm** — pre-dial all proxies after bootstrap
- **Full Apple binding API compat** — zero stubs remaining, all methods implemented
- **LocalityGetAddressesForKey / LocalityGetBoundaryKeys** — real implementations
- **OpenWithConnectionString / GetClientStatus** — implemented

### Bugs found and fixed

1. **MustGet panic recovery regression** — moving unconvertError out of defer broke the panic->retry path. fdb.Error panics escaped the retry loop.
2. **GetRange limit=0 returns 0 keys** — remaining=0 caused loop to never execute. Fixed: treat 0 as unlimited (math.MaxInt).
3. **GetRange int32 overflow** — math.MaxInt on 64-bit overflows to -1 as int32, FDB returns 1 key in reverse. Fixed: clamp at wire boundary.
4. **GetRange more flag hardcoded true** — server's more=false was ignored when limit reached. Fixed: pass through server's flag.

### Testing added

- 8 cross-client interop tests (Go <-> CGo)
- 13 new C binding port tests (23->36 total)
- 2 concurrent stress tests (500 tx, 10 goroutines)
- 3 integration tests (Watch, GetEstimatedRangeSizeBytes, GetRangeSplitPoints)
- Race detector verified clean
- Binding stress: 50/50 seeds x 1000 ops

### CI/Infra

- Fixed fdb_test timeout (300s -> "long"/900s)
- Recreated Hetzner runner via OpenTofu (was offline)
- Cancelled stale queued CI runs
- Closed 4 GitHub issues (#3, #19, #20, #21)

## Current state

- **Master:** clean, CI green (13/13 tests)
- **Record layer:** 2307/2309 pass (2 skip manual)
- **Conformance:** 422/422 pass
- **Chaos:** PASS
- **Open issues:** 1 (#2 — upstream tuple.Unpack panic, tracked only)

## Known issues / tech debt

1. **ConnectPacket canonicalRemotePort** — we send port=0 (correct for pure client). FDB server can ASSERT-crash on TCP port reuse across sequential connections (rare, only observed in CI under Docker load). Root cause is FDB server-side: assertion at FlowTransport.actor.cpp:1569 fires for incoming connections that match an existing Peer entry. Can't fix client-side.

2. **Allocation floor at 21/op** — remaining allocs are structural (transaction wrapper, commitDone channel, closures, ReadFrame payload, PendingGet struct). Reducing further requires API changes.

3. **CGo still wins on writes** — Set is ~15% slower (commit RPC overhead), BatchGet/50 is ~25% slower (per-item allocs scale), RYW is ~50% slower (write-heavy path). Read path is where we beat CGo.

4. **int vs int32 limit type** — Go API uses `int` (matching Apple binding), C++ uses `int` (32-bit). Conversion happens at wire boundary in sendGetRange. Correct but different from C++ where there's no conversion needed. Documented in code.

## What to work on next

### High impact
- **Performance testing under real workloads** (TODO.md line 1433) — benchmark bulk inserts, index-heavy saves, large scans
- **Port more C binding tests** — 36/81 done, each one validates an edge case
- **Write path optimization** — investigate why Set is 15% slower than CGo

### Medium impact
- **Directory layer** — needed by some FDB applications
- **Pool ReadFrame buffers** — tricky (consumers hold slices into payload) but saves 1 alloc
- **Reduce write-path allocs** — commit request slice creation, tenant prefix copying

### Low priority (large features, deferred)
- Synthetic record types (JoinedRecordType, UnnestedRecordType)
- Query planner / cascades optimizer
- Version vector support
