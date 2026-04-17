# Swingshift-23 Overtime Handover

**Date:** 2026-04-17, overtime started ~21:10 CEST (after PR #71 merge), ended ~00:00 CEST (2h)
**PR:** #72 (follow-up to merged PR #71)

## Context

PR #71 was merged mid-shift as a squash commit. User instructed continuing for 2 hours of overtime on a new PR. All 15 overtime commits live on `swingshift-23` branch, rebased to contain only post-merge work.

## What was done (15 commits)

### Java-aligned Directory methods
- `IsLeaf()`, `Parent()`, `Depth()`, `NameInTree()` — matches Java KeySpaceDirectory
- `ToPathString()` (/-separated) and `ToTree()` (ASCII tree) for debugging
- `AddSubdirectories()` variadic convenience
- `FindChildForValue()` with constant-priority two-pass lookup (matches Java)

### Path navigation & comparison
- `Flatten()`, `Directory()` accessors
- `Equal()` — uses `reflect.DeepEqual` for []byte safety
- `IsSameDirectory()` — schema-position comparison

### KeySpace schema
- `KeySpace.Validate()` — tree structure + constant type consistency
- `KeySpace.String()` — pretty-print schema
- Duplicate subdirectory name now panics at construction
- `AddSubdirectory` returns parent for fluent chaining

### PathFromTuple
- Root-level loop now matches `resolveRemaining` — constant-priority two-pass
- Regression test `TestPathFromTuple_ConstantPriority`

### Phase 3: FDBResolver (new)
- Persistent LocatableResolver backed by raw FDB subspace (simpler than Java's ScopedDirectoryLayer, no DirectoryLayer dependency)
- Storage layout: `{"n", name} → int64`, `{"r", value} → name`, `{"c"} → counter`
- Forward/reverse lookup with in-memory cache
- Transactional allocation (safe under cross-process contention)
- `InvalidateCache()`, `CacheSize()` for testing
- **Real bug caught via integration test**: shared `[8]byte` buffer across `tx.Set` calls caused mutation corruption because pure Go FDB client buffers writes by slice reference until commit — the second `PutUint64` overwrote the first write's payload. Fixed by using separate per-write `make([]byte, 8)` buffers.

### Integration tests (FDB container)
- `TestFDBResolver_ResolveAllocatesNew` — counter allocation
- `TestFDBResolver_Persistence` — second resolver sees first's writes (requires `InvalidateGRVCache` between, noted in comment)
- `TestFDBResolver_ReverseLookup` — lookup + cache population
- `TestFDBResolver_CacheManagement` — InvalidateCache + re-read from FDB
- `TestFDBResolver_EmptyStringName` — regression for empty-string sentinel bug
- `TestFDBResolver_ResolverDirectory_EndToEnd` — full Phase 1+2+3 pipeline

### Benchmarks
- Path_Construction: 134ns, 3 allocs
- Path_ToTuple: 35ns, 1 alloc
- Path_ToSubspace: 187ns, 4 allocs
- PathFromTuple: 100ns, 3 allocs
- MemoryResolver_Hit: 16ns, **0 allocs**

## Review history (3 rounds, all LGTM)

**Round 1** (before I'd added FDBResolver):
- rtreeScanCursor / prefixSkipScanCursor IsClosed bug → fixed (added `closed bool` fields)
- InsertBatch nil guard placement → fixed (moved before tx setup)
- TestToTree formatting → fixed (asserts indent consistency)
- `[]byte` panic in Equal/FindChildForValue → fixed (reflect.DeepEqual)
- Constant-priority in FindChildForValue → fixed (two-pass loop)
- Benchmark warm-up silent error → fixed (b.Fatal)

**Round 2**: LGTM with follow-up
- PathFromTuple root loop still single-pass → fixed

**Round 3** (after adding FDBResolver):
- Empty-string sentinel in ReverseLookup → fixed (revResult{name, found} struct)
- ctx parameter silently ignored → documented on both Resolve and ReverseLookup

**Final**: LGTM, ready to merge.

## Current state

- **4844 tests, 81.2% coverage** (slight bump from PR #71's 4806/81.1%)
- **17 test targets pass** (keyspace_test target added)
- **20+ keyspace unit tests** + **6 FDBResolver integration tests**
- **CI green** (run 24587906881)
- **PR #72**: 15 commits, 3 review rounds LGTM

## Known limitations

- `FDBResolver.Resolve` / `ReverseLookup` accept `ctx` for LocatableResolver interface compliance but don't forward it to FDB transactions (documented in code)
- FDBResolver's in-memory cache grows unbounded. For large deployments, consider wrapping with an LRU. Fine for typical usage where unique names are bounded (e.g., per-tenant counts).
- FDBResolver is simpler than Java's ScopedDirectoryLayer — no metadata, no create hooks, no multi-resolver scoping. That's an intentional trade-off; add those layers if needed.

## What to work on next

### HIGH
- [ ] **`frl` CLI** — user wants to design the command hierarchy (RFC 020)

### MEDIUM
- [ ] **KeySpace integration with recordlayer** — wire KeySpace paths into `NewStoreBuilder().SetSubspace(path.ToSubspace())` with a KeySpace-aware convenience API
- [ ] **FDBReverseDirectoryCache Java feature parity** — current FDBResolver does the same thing more simply; decide if full parity is needed
- [ ] **Full Java ScopedDirectoryLayer** — only if enterprise multi-resolver scoping is required (base/node/state/content subspaces, metadata, hooks)

### LOW
- [ ] LRU cap on FDBResolver in-memory cache
- [ ] Proto serialization for KeySpace tree (Java has it for distribution/caching)
- [ ] ResolvedKeySpacePath (Java separates KeySpacePath and ResolvedKeySpacePath — we conflate them in `Path`)

## Why this is in a separate handover

PR #71 was merged as a squash commit mid-shift. The branch `swingshift-23` was deleted on remote. When I continued working, I accidentally committed to `master` locally (commit recovered from reflog, cherry-picked to fresh swingshift-23). The correct flow going forward is: **after a shift PR merges, always `git checkout master && git pull`, then create a new branch for any follow-up work.** Documented in the earlier "master accident" explanation.
