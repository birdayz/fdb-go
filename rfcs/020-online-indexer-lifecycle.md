# RFC 020: Online Indexer — Lifecycle, Operations, and `frl` CLI

## Status: Draft

## Problem

Adding a new index to a Record Layer store with 500M records fails today. `checkPossiblyRebuild` on `CreateOrOpen` calls `RebuildIndex` — a single FDB transaction that scans all records and inserts index entries. At 500M records this exceeds the 5-second FDB transaction limit. The service refuses to start.

We have a fully implemented `OnlineIndexer` (BY_RECORDS, BY_INDEX, MULTI_TARGET, MUTUAL strategies) that builds indexes across many small transactions, but there is no operational tooling to invoke it. No CLI, no migration command, no automatic dispatch from `checkPossiblyRebuild`.

This RFC documents the complete OnlineIndexer lifecycle, the gaps vs Java, and proposes the `frl` CLI as the operational interface.

## Background: Index State Machine

Every index in a Record Layer store has one of these states:

```
DISABLED ──→ WRITE_ONLY ──→ READABLE
                 ↑               │
                 └───────────────┘  (rebuild: READABLE → WRITE_ONLY → READABLE)
```

| State | Reads | Writes | Description |
|-------|-------|--------|-------------|
| `DISABLED` | No | No | Index exists in metadata but is not maintained |
| `WRITE_ONLY` | No | Yes | New writes maintain the index; queries don't use it |
| `READABLE` | Yes | Yes | Fully built; queries can scan it |

State is stored in FDB at subspace `[5, indexSubspaceKey]` (INDEX_STATE_SPACE).

## OnlineIndexer Lifecycle

### Phase 1: Mark WRITE_ONLY

```go
markWriteOnly()
```

1. Opens the store (with `SetSkipPossiblyRebuild(true)` to avoid inline rebuild)
2. If index is already WRITE_ONLY → continued build (validates saved stamp)
3. If fresh → calls `ClearAndMarkIndexWriteOnly()`:
   - Clears all index entries
   - Sets state to WRITE_ONLY
   - Writes `IndexBuildIndexingStamp` proto to FDB at `[9, indexSubspaceKey, 2]`

**From this point forward**: all `SaveRecord` / `DeleteRecord` calls on any store instance dispatch to `UpdateWhileWriteOnly()` for this index. New writes are indexed immediately — no data is lost during the build.

### Phase 2: Build (multi-transaction loop)

```go
for {
    gap := rangeSet.FirstMissingRange()
    if gap == nil { break }  // complete

    store.Run(func(tx) {
        records := scan(gap.begin, limit+1)  // limit+1 look-ahead
        for _, rec := range records[:limit] {
            maintainer.Update(nil, rec)       // insert index entries
        }
        rangeSet.InsertRange(gap.begin, boundary)
        store.AddBuildProgress(len(records[:limit]))
    })
}
```

Each iteration is one FDB transaction (~2000 records default). Key design decisions:

**limit+1 pattern**: Scans `limit+1` records but only indexes `limit`. The (limit+1)th record's primary key becomes the exclusive boundary for the range set entry. This prevents the boundary record from being re-scanned in the next transaction — critical for non-idempotent indexes (COUNT, SUM) where double-counting would corrupt data.

**Isolation levels**: Idempotent indexes (VALUE, RANK) use SNAPSHOT isolation — no conflict tracking, faster, fewer retries. Non-idempotent indexes (COUNT, SUM) use SERIALIZABLE — detects concurrent modifications.

**Adaptive throttling**: On transaction failure, halve the limit. On success, maintain current limit. (Gap vs Java: Java can increase limit after sustained success.)

### Phase 3: Mark READABLE

```go
markReadable()
```

1. Transitions index state to `READABLE` (or `READABLE_UNIQUE_PENDING` for unique indexes with violations)
2. Checks for uniqueness violations on unique indexes
3. Clears build metadata: range set at `[6, indexSubspaceKey]`, heartbeats, progress counter

## WRITE_ONLY Dispatch (How New Writes Maintain Building Indexes)

When an index is WRITE_ONLY and the application saves/deletes records:

```go
// store.go — updateOneIndex()
if store.getIndexStateLocked(index.Name) == IndexStateWriteOnly {
    return maintainer.UpdateWhileWriteOnly(oldRecord, newRecord)
}
return maintainer.Update(oldRecord, newRecord)
```

The `UpdateWhileWriteOnly` path checks whether the record's primary key falls within an already-built range (via `IndexingRangeSet.ContainsKey()`). This prevents double-indexing: if the OnlineIndexer hasn't reached this record yet, the write still indexes it; if the OnlineIndexer already indexed it, the write updates the existing entry.

This dispatch happens in every `SaveRecord` / `DeleteRecord` call, on every store instance, across all pods. It's transparent to application code.

## Progress Tracking & RangeSet

### RangeSet

Tracks which primary key ranges have been indexed. Wire-compatible with Java.

```
FDB Key:     [6, indexSubspaceKey].Pack(tuple{rangeBeginBytes})
FDB Value:   rangeEndBytes (raw bytes, NOT tuple-packed)
Valid space: [0x00, 0xFF)
```

Example after building records with PKs 0x00–0x50 and 0x80–0xFF:
```
Key: 0x00 → Value: 0x50
Key: 0x80 → Value: 0xFF
```

`InsertRange` automatically consolidates adjacent ranges. `FirstMissingRange` returns the first gap.

### Progress Counter

Atomic counter at `[9, indexSubspaceKey, 1]`. Incremented via `MutationType.ADD` after each chunk. Readable via `LoadBuildProgress()` for progress reporting.

### Indexing Stamp

Proto at `[9, indexSubspaceKey, 2]`. Records the build method (BY_RECORDS, BY_INDEX, etc.), target index names, and source index info. Used on resume to validate compatibility — prevents a BY_INDEX build from accidentally resuming a half-done BY_RECORDS build.

## Crash Recovery

If the OnlineIndexer process dies mid-build:

1. Index remains WRITE_ONLY — reads don't use it, new writes maintain it
2. RangeSet in FDB preserves exact progress (which ranges were built)
3. Progress counter preserves record count
4. Indexing stamp preserves method and targets

On next run:

1. `markWriteOnly()` detects `continuedBuild=true` (index still WRITE_ONLY)
2. Loads saved stamp, validates method compatibility
3. `FirstMissingRange()` finds where to resume
4. Build loop continues from that boundary

No manual recovery needed. All state is in FDB.

## Build Strategies

### BY_RECORDS (default)

Scans all records in primary key order. Indexes every record matching the index's record types.

```go
rl.NewOnlineIndexerBuilder().
    SetStore(store).
    SetIndex(index).
    SetLimit(2000).
    BuildIndex(ctx)
```

### BY_INDEX

Uses an existing READABLE VALUE index as the scan source instead of the primary record store. Faster when the source index covers the same record types.

```go
rl.NewOnlineIndexerBuilder().
    SetStore(store).
    SetIndex(targetIndex).
    SetSourceIndex(sourceIndex).
    BuildIndex(ctx)
```

Validation: source index must not create duplicates, must cover the same record type subset.

### MULTI_TARGET

Single pass over records, building multiple indexes simultaneously. Range set uses the alphabetically first target index for boundaries.

```go
builder := rl.NewOnlineIndexerBuilder().SetStore(store)
builder.AddTargetIndex(index1)
builder.AddTargetIndex(index2)
builder.AddTargetIndex(index3)
builder.BuildIndex(ctx)
```

### MUTUAL (concurrent)

Multiple processes build the same index in parallel. Divides key space into fragments based on FDB shard boundaries.

```go
rl.NewOnlineIndexerBuilder().
    SetStore(store).
    SetIndex(index).
    SetMutualIndexing().
    BuildIndex(ctx)
```

**Fragment iteration**: random start + coprime step guarantees every process visits every fragment:
```
fragmentFirst = rand(fragmentCount)
fragmentStep  = findCoprimeStep(fragmentCount)  // prime not dividing count
next = (current + step) % count
```

**Phases**:
1. FULL — only completely unbuilt fragments
2. ANY — any fragment with gaps
3. RECOVER — all fragments touched, check for completion

**Heartbeat coordination**: each process writes a heartbeat to FDB (TTL-based). Multiple mutual builders allowed concurrently. A non-mutual builder blocks all mutual builders.

**Limitation**: single-node FDB (testcontainers, dev) has no shard boundaries → 1 fragment → no concurrency benefit.

## Current Behavior: `checkPossiblyRebuild`

When `CreateOrOpen` detects a metadata version increase:

```go
// store.go — checkPossiblyRebuild()
if newVersion > storedVersion {
    indexes := metaData.GetIndexesToBuildSince(storedVersion)
    for _, idx := range indexes {
        store.RebuildIndex(idx)  // ← single transaction, blows up at scale
    }
    // update stored version
}
```

`RebuildIndex` clears the index, marks WRITE_ONLY, scans ALL records in one transaction, inserts ALL entries, marks READABLE. Fine for <10K records. Timeout at 500M.

## Gaps vs Java

| Gap | Description | Impact | Effort |
|-----|-------------|--------|--------|
| **No large-store dispatch** | `checkPossiblyRebuild` always uses inline `RebuildIndex`, never OnlineIndexer | Service won't start if new index added to large store | Medium |
| **No automatic strategy selection** | Must manually choose BY_RECORDS/BY_INDEX/MULTI/MUTUAL | UX annoyance | Medium |
| **No automatic fallback** | If BY_INDEX fails validation, errors instead of falling back to BY_RECORDS | Operator must retry with different config | Low |
| **No adaptive limit increase** | Limit decreases on failure but never recovers | May run slower than needed after transient errors | Low |
| **No `frl` CLI** | No operational tooling for index lifecycle management | Manual FDB intervention required | Medium |

## Proposed Solution: `frl` CLI

A command-line tool for Record Layer operational tasks. The app registers its schema (metadata builder function), and `frl` operates on the FDB data.

### Index Commands

```sh
# List all indexes with their current state
frl index list
# NAME                        TYPE    STATE        LAST_MODIFIED
# meter_by_slug               VALUE   READABLE     v3
# event_by_customer_meter     VALUE   READABLE     v5
# usage_sum                   SUM     READABLE     v5
# new_index                   VALUE   WRITE_ONLY   v14  (building: 45% complete)

# Build a single index (BY_RECORDS, blocks until done)
frl index build new_index --limit 5000

# Build using an existing index as source
frl index build new_index --source event_by_customer_meter

# Build all non-READABLE indexes
frl index build --all-pending

# Concurrent build (run on multiple pods/processes)
frl index build new_index --mutual

# Check build progress
frl index status new_index
# State:     WRITE_ONLY
# Strategy:  BY_RECORDS
# Progress:  234,567 / ~500,000,000 records (0.05%)
# Ranges:    [0x00, 0x1A) built
# Rate:      ~12,000 records/sec
# ETA:       ~11.5 hours

# Manual state transitions
frl index disable new_index
frl index mark-write-only new_index
```

### Store Commands

```sh
# Store metadata and health
frl store info
# Metadata version:  14
# Format version:    6
# Record count:      500,234,567
# Split records:     enabled
# Store versions:    enabled

# Validate schema evolution (old stored vs new local)
frl store validate
# ✓ Record type Customer: unchanged
# ✓ Record type UsageEvent: PK unchanged
# ✓ Index usage_sum: unchanged  
# ✓ Index new_index: ADDED (will need build)
# ✗ Index old_index: REMOVED (needs FormerIndex tracking)
```

### Schema Registration

The app provides a schema function that `frl` invokes:

```go
// In the app's storage package:
func BuildMetadata() (*rl.RecordMetaData, error) {
    builder := rl.NewRecordMetaDataBuilder().
        SetRecords(storev1.File_store_proto)
    // ... PKs, indexes ...
    return builder.Build()
}
```

`frl` imports this and uses it to construct the metadata. No separate config file — the Go code IS the schema definition.

### Deployment Workflow

```
1. Developer adds new index to metadata builder
2. PR merged, new binary deployed
3. Pods start with Build() + SetSkipPossiblyRebuild — index is WRITE_ONLY
4. Operator runs: frl index build new_index --mutual
   (or: k8s Job runs it)
5. Build completes → index is READABLE
6. New queries automatically use the index
```

No downtime. No blocking startup. New writes are indexed from the moment the new binary deploys (WRITE_ONLY dispatch). The build job catches up historical data in the background.

### Production Checklist for Adding an Index

| Step | Who | What | Downtime? |
|------|-----|------|-----------|
| 1 | Developer | Add index to metadata builder, bump version | No |
| 2 | CI | Tests pass (inline rebuild works for test data) | No |
| 3 | Deploy | New binary rolls out, `CreateOrOpen` sees new version | No |
| 4 | Auto | `checkPossiblyRebuild` marks index WRITE_ONLY (skip inline for large stores) | No |
| 5 | Auto | All new writes maintain the WRITE_ONLY index | No |
| 6 | Operator | `frl index build <name> --mutual` from k8s Job | No |
| 7 | Auto | Build completes → READABLE | No |
| 8 | Auto | Queries start using the index | No |

Zero downtime throughout. The only manual step is kicking off the build (step 6), which could also be automated via a controller that watches for non-READABLE indexes.

## Non-Goals

- **Query planner integration** — `frl` is an operations tool, not a query engine
- **PK migration** — changing primary keys requires dual-write application-level migration, not OnlineIndexer
- **Automatic strategy selection** — keep it explicit in v1; add heuristics later
- **GUI** — CLI first; web dashboard can come later

## Open Questions

1. **Should `checkPossiblyRebuild` auto-dispatch to OnlineIndexer?** Or should it always fail fast for large stores and require explicit `frl index build`? Failing fast is safer — no surprise multi-hour blocking on startup.

2. **Schema registration model**: should `frl` link against the app's Go package (like a test binary), or should the schema be serialized to a file that `frl` reads? Linking is simpler but couples the tool to the app's build.

3. **Should MUTUAL mode be the default for `frl index build`?** Single-builder mode is simpler to reason about but slower. MUTUAL is always safe (degrades to single-builder on single-node FDB).

4. **Progress persistence for ETA**: the atomic counter tracks records scanned, but estimating total records requires a separate `GetRecordCount()` call. Should `frl` cache this estimate?
