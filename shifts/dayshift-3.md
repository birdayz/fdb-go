# Dayshift-3 Handover

**Date:** 2026-04-11 06:00 — 14:00 CEST
**PR:** #31 (draft)
**Branch:** `dayshift-3`

## Objective

Performance comparison, optimization, and binding tester correctness.

## What was done

### 1. Go vs Java Record Layer benchmark comparison (commit `6566d10`)

Added benchmark infrastructure: 8 Java benchmark endpoints (`BenchmarkSteps.java`) with 20-iteration JIT warmup + Go comparison test (`benchmark_comparison_test.go`). Both share the same FDB container. `just bench-compare` target added.

### 2. Performance optimizations (6 commits)

1. **Parallelize FDB reads in StoreOpen** — issue store info + index states GetRange simultaneously (was sequential)
2. **Skip proto.Clone for uncached entries** — PassThrough cache entries aren't shared, safe to use directly
3. **Skip metadata version stamp read for uncached stores** — PassThrough doesn't need `\xff/metadataVersion` (saves 1 FDB round-trip per Open)
4. **Pipeline metadata version stamp in cache miss** — fire stamp read before resolving store state
5. **fastUnpack/fastSubspaceUnpack across all 22 production files** — zero-alloc integer decode everywhere, replacing standard `tuple.Unpack`
6. **Pool commit request slices in FDB client** — sync.Pool for MutationRef, KeyRangeRef, PrecomputeSize

**Impact (Go vs Java, 3-run average):**
| Operation | Go (us) | Java (us) | Ratio |
|---|---|---|---|
| LoadRecord | 330 | 546 | **0.61x** |
| StoreOpen | 220 | 273 | **0.81x** |
| ScanRecords | 1,152 | 1,348 | 1.13x |
| SaveRecord | 2,525 | 2,382 | 1.06x |
| ScanIndex | 1,339 | 535 | 2.50x |
| DeleteRecord | 2,533 | 2,131 | 1.13x |
| SaveBatch | 3,657 | 3,517 | 1.04x |

### 3. Critical bug fix: binding tester _DATABASE handling (commit `0a80560`)

**Root cause of all intermittent binding stress failures.** The stacktester was missing `_DATABASE` handling for 4 read operations: `GET_KEY`, `GET_RANGE`, `GET_RANGE_STARTS_WITH`, `GET_RANGE_SELECTOR`. When generated, these ran on the current (potentially errored) transaction instead of `db.Transact()`. Stack misalignment → panics.

After fix: seed 58 passes 10/10 (was ~30% failure rate). 30/30 clean stress run.

### 4. Binding stress health check (commit `ab8b9e1`)

Replaced blind 5-second sleep with proper FDB health check. Polls `fdbcli status minimal` for "available" after configure (up to 30s). Eliminates false failures from slow FDB startup.

### 5. Conformance test expansion (3 commits, 7 new specs)

- 4 Java-writes-Go-evaluates aggregate tests (COUNT, SUM, MIN_EVER, MAX_EVER)
- 2 cross-language delete tests (Go→Java, Java→Go)
- 1 Java-deletes count test

## Current state

- **Master:** `3fabc11`
- **Branch:** `dayshift-3` (22 commits ahead)
- **Open PRs:** 1 (#31, draft)
- **All 13 Bazel test targets pass**
- **Conformance tests:** 430 specs
- **Binding stress:** 30/30 clean run post-fix, 50-seed run in progress
- **Fuzz testing:** 140M fastUnpack executions, 66M roundtrip, 92M continuation = 0 failures

## Known issues

- **ScanIndex 2.5x slower than Java** — structural. JVM JIT optimizes tight cursor loops. Go's per-entry interface dispatch + `IndexEntry` allocation is inherent to the cursor API. Not fixable without redesigning cursor interface.
- **Bazel arg truncation in binding stress** — `-seeds 100` sometimes gets truncated to fewer by Bazel sandboxing. Cosmetic issue, doesn't affect correctness.

## What to work on next

### High priority
- **Merge PR #31** — large PR, consider squash merge
- **Run extended binding stress** (100+ seeds) with the health check to validate at scale

### Medium priority
- **Pool serialization buffers in FDB client** — `CommitTransactionRequest.MarshalFDB()` allocates a fresh buffer every commit. Profiling shows 11% of total allocations. Pool the buffer via sync.Pool with size-tracking.
- **ScanIndex optimization investigation** — the 2.5x gap deserves a Go runtime pprof analysis. The cursor per-entry overhead (IndexEntry + BytesContinuation allocation) might be reducible with pooling or batch processing.

### Low priority
- **FDBReverseDirectoryCache** — ~496 lines Java, needs LocatableResolver/ScopedValue/FDBStoreTimer dependencies ported first
- **KeySpace/KeySpacePath** — 25 Java files, 7K lines. Enterprise key management.
- **Schema validation cross-language** — needs Java conformance server additions
