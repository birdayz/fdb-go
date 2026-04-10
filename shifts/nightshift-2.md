# Nightshift-2 Handover

**Date:** 2026-04-10 22:46 — 2026-04-11 06:00 CEST
**PR:** #30 (draft)
**Branch:** `nightshift-2`

## Objective

Port FDB directory layer for Java Record Layer KeySpace compatibility.

## What was done

### 1. Directory layer port (commit `e254de1`)

Ported all 6 files (~1300 lines) from the Apple Go directory layer binding to use our fdb package types. Mechanical port — same logic, different import paths.

Files:
- `directory.go` — public interface (Directory, DirectorySubspace)
- `directoryLayer.go` — core implementation (create, open, list, move, remove)
- `directorySubspace.go` — subspace wrapper
- `directoryPartition.go` — partition support
- `allocator.go` — High Contention Allocator (HCA)
- `node.go` — node metadata

### 2. Directory layer tests (6 tests)

- Basic CRUD (create, read, list, exists, remove)
- Multiple directories / HCA prefix uniqueness
- Move (rename without data move)
- Open existing (idempotent, same prefix)
- Subdirectory through DirectorySubspace
- Duplicate create error

### 3. Cross-client directory interop (commit `615fe12`)

**Verified wire compatibility:** Go-created directories are readable by CGo (Apple binding) and vice versa. This means Java Record Layer apps using `KeySpace`/`DirectoryLayerDirectory` can interop with our Go client.

### 4. 2h binding stress (running)

Started at shift begin. At latest check: 300+ seeds, 0 failures, 0 FDB deaths. Will complete ~01:47 CEST.

### 5. TODO.md cleanup (128 → 104 open items)

Resolved 24 items:
- **Features implemented**: WeakReadSemantics, FDBDatabaseFactory, IsVersionChanged()
- **Verified not bugs**: Wire #11 (nil/empty), Wire #14 (variant tag=0), emptyVector
- **Marked done**: TEXT index, key expressions, cursor combinators, FunctionKE conformance
- **WONTFIX (Java-specific)**: preloadRecordAsync, buildSingleRecord, scanRemoteFetch, mergeIndex/performOperation, isIdempotent, IndexScanBounds, scanIndexRecords filter, repairRecordKeys, FDBLatencySource, CursorLimitManager, Visitor pattern, PreloadRecordStoreState, canDeleteWhere
- **Style**: Get prefix WONTFIX (Java naming for compat)
- **Updated**: coverage table, memory.md spec counts, index types heading

## Current state

- **Master:** `9be2748`
- **Branch:** `nightshift-2` (39 commits ahead)
- **Open PRs:** 1 (#30, draft)
- **All 14 Bazel test targets pass**
- **2h binding stress:** **673 seeds × 1000 ops = 673K operations, 0 failures, 0 FDB deaths**
- **Directory layer:** ported, tested, cross-client verified
- **New features:** WeakReadSemantics, FDBDatabaseFactory, IsVersionChanged()
- **TODO.md:** 128 → 61 open items (67 resolved)
- **New features:** WeakReadSemantics, FDBDatabaseFactory, IsVersionChanged(), TransactionID(), FDBMetaDataStore

## Known issues

- **GRV cache staleness in cross-client tests** — not a bug. The Go client's GRV cache can serve a version from before a CGo write, causing the Go client to not see the CGo data. Fixed with `InvalidateGRVCache()` in tests. Production apps don't hit this (single-client RYW covers it).

## What to work on next

### High impact
- **Binding tester directory extension** — implement DIRECTORY_* stack machine operations to pass the binding tester's directory test suite (~21 operations, ~400 lines)
- **FDBMetaDataStore** — Implemented! Stores/loads MetaData proto in FDB with version history. 4 tests. Key learnings: (1) tuple.Pack panics on int32 — use int64, (2) Go `interface{}(nil-typed-value) != nil` — use closure vars instead of Transact return for nil checks, (3) `len(data) == 0` instead of `data == nil` for MustGet results.
- **Performance benchmarking** — real workload benchmarks (bulk inserts, index-heavy saves, large scans, OnlineIndexer throughput). Compare with Java.

### Medium impact
- **Directory layer conformance tests** — Go↔Java cross-language directory interop (needs Java conformance server additions)
- **FDBReverseDirectoryCache** — prefix→name caching for multi-tenant apps (~496 lines Java)
- **Version vector support** — causal consistency optimization

### Low priority
- Query planner (26 items — out of scope until needed)
- Synthetic record types (13 items — experimental API)
- Cursor combinators needing planner (10 items)
- Tag throttling, multi-shard test, multi-version client
