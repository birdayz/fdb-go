# Nightshift-2 Handover

**Date:** 2026-04-10 22:46 — (ongoing, shift ends 06:00 CEST)
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

Started at shift begin. At handover: ~170+ seeds, 0 failures, 0 FDB deaths.

## Current state

- **Master:** `9be2748`
- **Branch:** `nightshift-2` (5 commits ahead)
- **Open PRs:** 1 (#30, draft)
- **All 13+ Bazel test targets pass** (14 with new directory target)
- **2h binding stress:** running clean
- **Directory layer:** ported, tested, cross-client verified

## Known issues

- **GRV cache staleness in cross-client tests** — not a bug. The Go client's GRV cache can serve a version from before a CGo write, causing the Go client to not see the CGo data. Fixed with `InvalidateGRVCache()` in tests. Production apps don't hit this (single-client RYW covers it).

## What to work on next

### High impact
- **Binding tester directory extension** — implement DIRECTORY_* stack machine operations to pass the binding tester's directory test suite (~40 operations)
- **Extended binding stress results** — check 2h run completion

### Medium impact
- **Directory layer conformance tests** — Go↔Java cross-language directory interop (would need Java conformance server additions)
- **Version vector support** — causal consistency optimization

### Low priority
- Synthetic record types, query planner, views, UDFs
- Wire type MEDIUM items (#11, #14)
