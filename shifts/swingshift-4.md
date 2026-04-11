# Swingshift-4 Handover

**Date:** 2026-04-11 14:00 — 22:00 CEST
**PR:** #32 (draft)
**Branch:** `swingshift-4`

## Objective

Performance optimization (serialization buffer pooling) + binding stress validation + failure investigation.

## What was done

### 1. Commit path serialization buffer pooling (commit `f8c2b8b`)

`MarshalFDBPooled` for CommitTransactionRequest: reuses a caller-provided byte buffer when capacity is sufficient. Eliminates the per-commit buffer allocation.

**Benchmark (5 mutations, 5 read/write CRs):**
- MarshalFDB:       2090 ns/op, 1555 B/op, 4 allocs/op
- MarshalFDBPooled: 1563 ns/op,    0 B/op, 0 allocs/op
- **25% faster, zero allocations**

### 2. Fix pool leak in ALL MarshalFDB methods (commit `e78755d`)

Updated code generator (`cmd/fdb-schema-extract/main.cpp`) to emit `ReleaseWriteToBuffer(wb)` and `ReleasePrecomputeSize(ps)` at end of every `MarshalFDB`. Previously, `NewPrecomputeSize()` got objects from pool but never returned them — effectively a pool leak. Same for `WriteToBuffer`.

**Impact on ALL 26 MarshalFDB callers:**
- CommitTransactionRequest: 4→1 allocs (13% faster)
- GetValueRequest: was ~2-3 allocs → 1 alloc
- GetKeyValuesRequest: was ~2-3 allocs → 1 alloc

### 3. Binding stress unique container naming (commit `7a90bf8`)

Container name and port are now PID-unique (`fdb-stress-<pid>`, port `4500+pid%1000`). Multiple concurrent stress runs no longer conflict on Docker container name/port.

### 4. Crash diagnostics ring buffer (commit `7a90bf8`)

Added ring buffer of last 50 operations to stacktester. On `popInt64` type mismatch panic, dumps the last 50 operations for diagnosis without needing full trace.

### 5. Binding stress failure investigation

**Seeds investigated:** 331, 347, 350

**Root cause:** FDB server `canonicalRemotePort == peerAddress.port` ASSERT spam at `FlowTransport.actor.cpp:1545`. Triggered by TCP port reuse when both the Python binding tester (C client/libfdb_c) and our Go client connect to the same Docker-mapped FDB container. The ASSERT is non-fatal (doesn't crash, thanks to our `SetLinger(0)` fix) but produces massive log spam that slows the FDB server to the point of timeout (5 minutes).

**Key evidence:**
- Seeds 0-249 (50/50 pass) ran BEFORE concurrent Docker activity
- Seeds 300+ failed during concurrent builds/stress runs
- Seed 331 reproduces on rerun but shows ZERO stacktester output — Python tester hangs during "Inserting test into database" phase
- 300+ assertion lines per run, all from FlowTransport.actor.cpp:1545
- `fdb_alive=True` on all failures (FDB survived but was too slow)
- Our Go client tests (`TestSetGet`) pass fine — only the dual-client (Python+Go) scenario triggers it

**NOT a Go client bug.** Environmental Docker networking issue.

## Current state

- **Master:** `55bfe2e`
- **Branch:** `swingshift-4` (5 commits ahead)
- **PR:** #32
- **All 13 Bazel test targets pass**
- **Binding stress:** 50/50 clean (seeds 200-249, before concurrent activity). Current Docker environment has canonicalRemotePort ASSERT issue that blocks new runs.

## Known issues

- **canonicalRemotePort ASSERT spam blocks binding stress** — FDB 7.3.75 (and 7.3.46) assert when the Python C client causes TCP port reuse under Docker. Previous shifts ran stress successfully on the same FDB version, suggesting it's load/timing dependent. May need: (a) Docker host networking instead of port mapping, (b) FDB version without this ASSERT, (c) single-client mode (skip Python reference comparison).
- **ScanIndex 2.5x slower than Java** — Structural. Per-entry IndexEntry heap allocation + interface dispatch + tuple slice allocations. JVM JIT optimizes tight cursor loops better. Not fixable without API redesign.

## What to work on next

### High priority
- **Fix binding stress ASSERT issue** — Try Docker host networking (`--network host`) instead of port mapping. The port mapping creates NAT that confuses FDB's peer tracking. Or switch to FDB 7.4+ which may have relaxed this ASSERT.
- **Merge PR #32** — serialization pooling is solid, all tests pass

### Medium priority
- **Pool read path MarshalFDB** — Same pattern as commit path: add `MarshalFDBPooled` to GetValueRequest, GetKeyValuesRequest, GetKeyRequest. Mechanical but reduces allocations on every read.
- **DatabaseContext refactor** — Consolidate Database/GRVBatcher/LocationCache/Cluster into single struct matching C++ DatabaseContext.

### Low priority
- **Pool frame read buffers** — `ReadFrame` allocates per response. Tricky because consumers hold slices into payload.
- **FDBReverseDirectoryCache** — ~496 lines Java
- **KeySpace/KeySpacePath** — 25 Java files, 7K lines
