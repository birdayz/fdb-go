# Swingshift-4 Handover

**Date:** 2026-04-11 14:00 — 22:00 CEST
**PR:** #32
**Branch:** `swingshift-4`

## Objective

Performance optimization (serialization buffer pooling) + binding stress validation + test infrastructure fixes.

## What was done

### 1. Commit path serialization buffer pooling

`MarshalFDBPooled` for CommitTransactionRequest: 25% faster, 0 allocs (was 4 allocs, 1555 B/op).

### 2. Fix pool leak in ALL MarshalFDB methods

Updated code generator to emit `ReleaseWriteToBuffer`/`ReleasePrecomputeSize`. All 26 MarshalFDB methods: 4→1 allocs, 13% faster.

### 3. Generated MarshalFDBPooled for all wire types

Code generator now emits `MarshalFDBPooled` alongside `MarshalFDB` for all 24 generated types. Read path (GetValue, GetKeyValues, GetKey) wired to use pooled variants.

### 4. Fix binding stress ASSERT issue

Docker port mapping caused FDB `canonicalRemotePort` assertion spam. Fix: connect via container bridge IP (no DNAT). **300/300 seeds pass (0-99, 300-399, 500-599).**

### 5. Root cause analysis: test hang bug

**Ultimate root cause: 481GB leaked Docker volumes filled disk to 97%.** FDB rate limiter throttled all writes to 0 TPS. Combined with missing context timeouts, tests hung for 100+ minutes.

**5 Whys:**
1. Tests hang → Go FDB client retries against throttled FDB
2. FDB throttles → TPSLimit=0 from rate limiter detecting low disk
3. Disk full → 481GB of leaked Docker anonymous volumes (5208 volumes!)
4. Volumes leak → testcontainers create anonymous volumes, never pruned
5. No timeout → container setup used bare `context.Background()`

**Fixes:**
- `docker volume prune` freed 449.5GB (97%→49% disk usage)
- 2-minute timeout on ALL container setup contexts (6 test files)
- `--local_test_jobs=4` in `.bazelrc` limits parallel container creation
- Documented in CLAUDE.md

### 6. Binding stress unique container naming + crash diagnostics

PID-unique container names. Ring buffer of last 50 operations for crash diagnostics.

## Current state

- **Branch:** `swingshift-4` (12 commits ahead of master)
- **PR:** #32
- **All 13 Bazel test targets pass** (101 seconds after disk cleanup)
- **Binding stress:** 300/300 clean (seeds 0-99, 300-399, 500-599)
- **Disk:** `/` at 49% (was 97%), `/home` at 90% (was 100%)

## Known issues

- **Docker volume leak is recurring** — testcontainers create anonymous volumes that persist. Run `docker volume prune -f` periodically. Consider adding to pre-commit hook or a cron job.
- **ScanIndex 2.5x slower than Java** — structural, per-entry heap allocs

## What to work on next

### High priority
- **Merge PR #32** — 12 solid commits, all tests pass
- **Add `docker volume prune` to test cleanup** — prevent volume leak recurrence

### Medium priority
- **DatabaseContext refactor** — consolidate Database/GRVBatcher/LocationCache/Cluster
- **Pool frame read buffers**

### Low priority
- **FDBReverseDirectoryCache**, **KeySpace/KeySpacePath**
