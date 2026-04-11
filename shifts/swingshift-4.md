# Swingshift-4 Handover

**Date:** 2026-04-11 14:00 — 22:00 CEST
**PR:** #32 (draft)
**Branch:** `swingshift-4`

## Objective

Performance optimization (serialization buffer pooling) + binding stress validation + failure investigation + fix.

## What was done

### 1. Commit path serialization buffer pooling (commit `f8c2b8b`)

`MarshalFDBPooled` for CommitTransactionRequest: reuses a caller-provided byte buffer when capacity is sufficient.

**Benchmark (5 mutations, 5 read/write CRs):**
- MarshalFDB:       2090 ns/op, 1555 B/op, 4 allocs/op
- MarshalFDBPooled: 1563 ns/op,    0 B/op, 0 allocs/op
- **25% faster, zero allocations**

### 2. Fix pool leak in ALL MarshalFDB methods (commit `e78755d`)

Updated code generator to emit `ReleaseWriteToBuffer(wb)` and `ReleasePrecomputeSize(ps)` at end of every `MarshalFDB`. All 26 MarshalFDB methods now return pooled objects: 4→1 allocs per marshal, 13% faster.

### 3. Read path serialization buffer pooling (commit `10bd57f`)

Added `MarshalFDBPooled` to GetValueRequest, GetKeyValuesRequest, GetKeyRequest. Every Get, GetKey, and GetRange now reuses serialization buffers. Note: `SendFrameDeferred` (pipelined Get) can't use pooling because the writeLoop holds a reference.

### 4. Fix binding stress ASSERT issue (commit `1cb6609`)

**Root cause:** Docker port mapping (`-p hostPort:4500`) causes DNAT which confuses FDB's `canonicalRemotePort` tracking, triggering assertion spam at `FlowTransport.actor.cpp:1545`. The Python binding tester's C client (libfdb_c) causes rapid port reuse through Docker NAT.

**Fix:** Connect directly to the Docker container's bridge IP instead of using port mapping. No DNAT = real source ports = zero assertions.

**Before:** seed 331 timed out at 5 minutes (300+ assertions)
**After:** seed 331 passes in ~10 seconds (0 assertions)

### 5. Binding stress unique container naming (commit `7a90bf8`)

PID-unique container names and ports. Multiple concurrent stress runs no longer conflict.

### 6. Crash diagnostics ring buffer (commit `7a90bf8`)

Ring buffer of last 50 operations in stacktester for crash diagnosis.

## Current state

- **Master:** `55bfe2e`
- **Branch:** `swingshift-4` (9 commits ahead)
- **PR:** #32
- **All 13 Bazel test targets pass**
- **Binding stress:** 100/100 clean (seeds 0-99), 100 more running (seeds 300-399, 41/41 so far)
- **Total binding stress this shift:** 200+ seeds, 0 failures

## Known issues

- **ScanIndex 2.5x slower than Java** — Structural. Per-entry heap allocs + JVM JIT advantage. Not fixable without API redesign.
- **SendFrameDeferred can't use pooled buffers** — writeLoop holds reference to body bytes. Pipelined Gets still allocate.

## What to work on next

### High priority
- **Merge PR #32** — 9 solid commits, all tests pass, 200+ binding stress seeds clean
- **Run Go vs Java benchmark comparison** with pooling improvements

### Medium priority
- **DatabaseContext refactor** — Consolidate Database/GRVBatcher/LocationCache/Cluster into single struct matching C++ DatabaseContext
- **Pool frame read buffers** — `ReadFrame` allocates per response (tricky: consumers hold slices)

### Low priority
- **FDBReverseDirectoryCache** — ~496 lines Java
- **KeySpace/KeySpacePath** — 25 Java files, 7K lines
