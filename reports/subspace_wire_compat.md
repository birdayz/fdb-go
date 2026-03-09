# Subspace & Key Construction Wire Compatibility Report

Date: 2026-03-09 (updated — full deep review)

## 1. Subspace Constants Comparison

All 10 subspace constants are defined in Go's `pkg/recordlayer/constants.go` and match
Java's `FDBRecordStoreKeyspace.java` enum exactly:

| Java Enum Name                       | Java Value | Go Constant Name            | Go Value | Match |
|--------------------------------------|------------|------------------------------|----------|-------|
| `STORE_INFO`                         | `0L`       | `StoreInfoKey`               | `0`      | YES   |
| `RECORD`                             | `1L`       | `RecordKey`                  | `1`      | YES   |
| `INDEX`                              | `2L`       | `IndexKey`                   | `2`      | YES   |
| `INDEX_SECONDARY_SPACE`              | `3L`       | `IndexSecondarySpaceKey`     | `3`      | YES   |
| `RECORD_COUNT`                       | `4L`       | `RecordCountKey`             | `4`      | YES   |
| `INDEX_STATE_SPACE`                  | `5L`       | `IndexStateSpaceKey`         | `5`      | YES   |
| `INDEX_RANGE_SPACE`                  | `6L`       | `IndexRangeSpaceKey`         | `6`      | YES   |
| `INDEX_UNIQUENESS_VIOLATIONS_SPACE`  | `7L`       | `IndexUniquenessViolationsKey` | `7`   | YES   |
| `RECORD_VERSION_SPACE`               | `8L`       | `RecordVersionKey`           | `8`      | YES   |
| `INDEX_BUILD_SPACE`                  | `9L`       | `IndexBuildSpaceKey`         | `9`      | YES   |

**Verdict: All constants match exactly. No issues.**

## 2. SplitHelper Constants Comparison

| Java Constant                  | Java Value | Go Constant            | Go Value   | Match |
|-------------------------------|-----------|------------------------|-----------|-------|
| `SplitHelper.UNSPLIT_RECORD`  | `0L`      | `UnsplitRecord`        | `int64(0)` | YES  |
| `SplitHelper.START_SPLIT_RECORD` | `1L`   | `StartSplitRecord`     | `int64(1)` | YES  |
| `SplitHelper.RECORD_VERSION`  | `-1L`     | `RecordVersionSuffix`  | `int64(-1)` | YES |
| `SplitHelper.SPLIT_RECORD_SIZE` | `100_000` | `SplitRecordSize`    | `100_000`  | YES  |

**Verdict: All split helper constants match exactly.**

## 3. Subspace Construction Patterns Comparison

### 3.1 Records Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Base | `getSubspace().subspace(Tuple.from(RECORD_KEY))` | `store.subspace.Sub(RecordKey)` |
| Record key | `recordsSubspace().pack(primaryKey.add(UNSPLIT_RECORD))` | `recordsSubspace.Pack(appendToTuple(primaryKey, UnsplitRecord))` |
| Version key (new fmt) | `Tuple.from(RECORD_KEY).addAll(pk).add(RECORD_VERSION)` | `recordsSubspace.Pack(pk + RecordVersionSuffix)` |

**Match: YES.** `subspace.Sub(X).Pack(T)` produces the same bytes as `subspace.subspace(Tuple.from(X)).pack(T)`.

### 3.2 Store Info Key

| Operation | Java | Go |
|-----------|------|-----|
| Read/Write | `getSubspace().pack(STORE_INFO_KEY)` | `store.subspace.Pack(tuple.Tuple{StoreInfoKey})` |
| Value format | `storeHeader.toByteArray()` (protobuf) | `proto.Marshal(storeInfo)` |

**Match: YES.** Both pack STORE_INFO (0) as a tuple element and store protobuf-serialized `DataStoreInfo`.

### 3.3 Index Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Per-index | `getSubspace().subspace(Tuple.from(INDEX_KEY, index.getSubspaceTupleKey()))` | `store.subspace.Sub(IndexKey, index.SubspaceTupleKey())` |

**Match: YES.**

### 3.4 Index State Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Base | `getSubspace().subspace(Tuple.from(INDEX_STATE_SPACE_KEY))` | `store.subspace.Sub(IndexStateSpaceKey)` |
| Per-index key | `indexStateSubspace().pack(indexName)` | `indexStateSubspace().Pack(tuple.Tuple{indexName})` |
| Value format | `Tuple.from(state.code()).pack()` | `tuple.Tuple{int64(state)}.Pack()` |

**Match: YES.**

### 3.5 Index Secondary Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Per-index | `getSubspace().subspace(Tuple.from(INDEX_SECONDARY_SPACE_KEY, index.getSubspaceTupleKey()))` | `store.subspace.Sub(IndexSecondarySpaceKey, index.SubspaceTupleKey())` |

**Match: YES.**

### 3.6 Record Count Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Write count | `getSubspace().pack(Tuple.from(RECORD_COUNT_KEY).addAll(subkey))` | `store.subspace.Sub(RecordCountKey).Pack(keyTuple)` |
| Read count | `tr.get(getSubspace().pack(subkey))` where subkey = `Tuple.from(RECORD_COUNT_KEY).addAll(...)` | `store.subspace.Sub(RecordCountKey).Pack(countKey)` |
| Value format | little-endian int64, atomic ADD | little-endian int64, atomic ADD |

**Match: YES.** Java's `getSubspace().pack(Tuple.from(4).addAll(subkey))` equals Go's
`store.subspace.Sub(4).Pack(subkey)` because both result in `[storePrefix][tupleEncode(4)][tupleEncode(subkey...)]`.

### 3.7 Index Range Subspace (RangeSet)

| Operation | Java | Go |
|-----------|------|-----|
| Per-index | `getSubspace().subspace(Tuple.from(INDEX_RANGE_SPACE_KEY, index.getSubspaceTupleKey()))` | `storeSubspace.Sub(IndexRangeSpaceKey, index.SubspaceTupleKey())` |

**Match: YES.**

### 3.8 Uniqueness Violations Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Per-index | `getSubspace().subspace(Tuple.from(INDEX_UNIQUENESS_VIOLATIONS_KEY, index.getSubspaceTupleKey()))` | `store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())` |

**Match: YES.**

### 3.9 Index Build Subspace

| Operation | Java | Go |
|-----------|------|-----|
| Per-index | `getSubspace().subspace(Tuple.from(INDEX_BUILD_SPACE_KEY, index.getSubspaceTupleKey()))` | `store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey())` |

**Match: YES.**

### 3.10 Record Version Key (Old vs New Format)

| Operation | Java | Go |
|-----------|------|-----|
| Old format key | `Tuple.from(RECORD_VERSION_KEY).addAll(primaryKey)` | Not implemented (Go only supports new format) |
| New format key | `Tuple.from(RECORD_KEY).addAll(primaryKey).add(SplitHelper.RECORD_VERSION)` | `recordsSubspace.Pack(pk + RecordVersionSuffix)` |

**Match: New format YES. Old format not implemented in Go.** Go always uses the new inline
version format (format version >= 6). The `RecordVersionKey=8` constant is defined but only
used in `DeleteAllRecords` for clearing the old version subspace (defensive). This is
acceptable since Go creates stores with format version 9.

## 4. Bugs Found

### 4.1 BUG: `ClearRange(subspace.Sub(...))` misses exact prefix key

**Severity: Medium**
**Affected files:** `store.go`, `record_count.go`

Go's `subspace.FDBRangeKeys()` returns `[prefix\x00, prefix\xFF)` which **excludes the exact
prefix key**. Three call sites use `ClearRange(store.subspace.Sub(key))` which silently fails
to clear data stored at the exact prefix:

1. **`store.go:566` -- `DeleteAllRecords`**: Clears each subspace with `ClearRange(sub)`.
   For `RecordCountKey` (4) with ungrouped counting, the count value is stored at
   `[storePrefix][tupleEncode(4)][tupleEncode()]` = the exact subspace prefix. The ClearRange
   misses this key. However, the code then explicitly `Set(fdbKey, encodeRecordCount(0))`
   on line 576-577, so the bug is **masked** for ungrouped counting. But for grouped counting
   (RecordTypeKeyExpression), any count at the exact prefix would leak.

2. **`store.go:884` -- `checkPossiblyRebuildRecordCounts`**: Same issue. Clears count data
   before rebuild using `ClearRange(subspace.Sub(RecordCountKey))`. Ungrouped count at exact
   prefix survives the clear. The subsequent rebuild writes fresh counts via `Set`, so stale
   data is overwritten rather than cleared. This is a **latent correctness risk** -- if the
   rebuild logic changes, the stale key could cause incorrect counts.

3. **`record_count.go:171` -- `UpdateRecordCountState` to DISABLED**: Clears all count data
   when transitioning to DISABLED state. Uses `ClearRange(subspace.Sub(RecordCountKey))`.
   Ungrouped count key survives. **This is a real bug**: after DISABLED transition, the count
   key still exists in FDB but the code assumes it's gone.

**Java's approach:** Java uses `Range.startsWith(getSubspace().pack(Tuple.from(RECORD_COUNT_KEY)))`
which returns `[prefix, strinc(prefix))` and correctly includes the exact prefix key.
Java also uses two continuous range clears in `deleteAllRecords` that avoid the gap entirely.

**Fix:** Use `fdb.PrefixRange(subspace.Sub(key).Bytes())` instead of
`ClearRange(subspace.Sub(key))` at all three locations. This was already done correctly in
`clearIndexData` (line 293 of `index_state.go`) for the same reason.

### 4.2 NOTE: `DeleteAllRecords` uses per-subspace clearing vs Java's two-range approach

**Severity: Low (cosmetic, functionally equivalent after fix 4.1)**

Java's `deleteAllRecords()`:
```java
Range indexStateRange = indexStateSubspace().range();
context.clear(new Range(recordsSubspace().getKey(), indexStateRange.begin));
context.clear(new Range(indexStateRange.end, getSubspace().range().end));
```

This does exactly 2 FDB ClearRange operations covering subspaces 1-4 and 6-9, skipping 0
(STORE_INFO) and 5 (INDEX_STATE_SPACE).

Go's `DeleteAllRecords()`:
```go
for _, key := range []int{1, 2, 3, 4, 6, 7, 8, 9} {
    tx.ClearRange(store.subspace.Sub(key))
}
```

This does 8 FDB ClearRange operations. Functionally equivalent (after fixing 4.1), but more
FDB operations per transaction. Java's approach is more efficient and avoids the FDBRangeKeys
gap naturally by using raw key boundaries.

## 5. Index Entry Key Construction

### 5.1 Standard VALUE Index

| Aspect | Java | Go |
|--------|------|-----|
| Entry key | `(indexValues..., trimmedPK...)` | `indexEntryKey(idx, values, pk)` |
| Entry value | empty tuple bytes | `tuple.Tuple{}.Pack()` |
| PK dedup | `primaryKeyComponentPositions` | `primaryKeyComponentPositions` |
| Subspace | `[store][2][indexSubspaceKey]` | `[store][2][indexSubspaceKey]` |

**Match: YES.** Verified through conformance tests (147 specs).

### 5.2 COUNT/SUM Aggregate Indexes

| Aspect | Java | Go |
|--------|------|-----|
| Entry key | `(groupingValues...)` or empty tuple for ungrouped | Same |
| Entry value | little-endian int64, atomic ADD | Same |
| Ungrouped key | exact subspace prefix (`subspace.Pack(tuple.Tuple{})`) | Same |

**Match: YES.**

## 6. Missing Functionality (Intentional)

### 6.1 Old Version Format (RECORD_VERSION_SPACE = 8)

Go does not implement the old version format where versions are stored at
`[store][8][primaryKey]`. Java supports both formats via `useOldVersionFormat()` which checks
`formatVersion < SAVE_VERSION_WITH_RECORD` or `omitUnsplitRecordSuffix`. Go always uses the
new inline format at `[store][1][pk][-1]`.

**Risk: Low.** Go creates stores at format version 9, which always uses the new format. A Go
client opening a Java store created at format version < 6 would not read old-format versions.

### 6.2 `omitUnsplitRecordSuffix` Flag

Java supports `omitUnsplitRecordSuffix` for backward compatibility with very old stores that
don't append the `UNSPLIT_RECORD` suffix (0) to unsplit record keys. Go always appends suffix
0, matching format version >= 5 behavior.

**Risk: Low.** Same reasoning as above.

### 6.3 `IndexingSubspaces` Sub-keys Within INDEX_BUILD_SPACE

Java's `IndexingSubspaces.java` defines sub-keys within `INDEX_BUILD_SPACE` (9):
- `INDEX_BUILD_LOCK_KEY = 0L`
- `INDEX_BUILD_SCANNED_RECORDS = 1L`
- `INDEX_BUILD_TYPE_VERSION = 2L`
- `INDEX_SCRUBBED_INDEX_RANGES_ZERO = 3L`
- `INDEX_SCRUBBED_RECORDS_RANGES_ZERO = 4L`
- `INDEX_SCRUBBED_RECORDS_RANGES = 5L`
- `INDEX_SCRUBBED_INDEX_RANGES = 6L`
- `INDEX_BUILD_HEARTBEAT_PREFIX = 7L`

Go's `clearIndexData` clears the entire `INDEX_BUILD_SPACE` subspace per index. Java's
`clearIndexData` is more selective: it calls `eraseAllIndexingDataButTheLock()` which
preserves the lock sub-key (0L).

**Risk: Low.** Go's approach is more aggressive (clears locks too). This only matters if
someone tries to coordinate index builds across Go and Java clients simultaneously.

### 6.4 `rebuildAllIndexes` Not Implemented in Go

Java has `rebuildAllIndexes()` which clears INDEX_KEY, INDEX_SECONDARY_SPACE_KEY,
INDEX_RANGE_SPACE_KEY, INDEX_UNIQUENESS_VIOLATIONS_KEY subspaces globally, then rebuilds all
indexes. Go only has per-index `RebuildIndex()`.

**Risk: Low.** Per-index rebuild is sufficient for all current use cases.

## 7. Summary

### What's Correct
- All 10 subspace constants: exact match
- All 4 split helper constants: exact match
- All subspace construction patterns: equivalent byte output
- Index entry key construction: verified via 147 conformance test specs
- Record count encoding (little-endian int64): exact match
- Index state encoding (tuple-packed int64): exact match
- Store header format (protobuf DataStoreInfo): exact match
- Version key format (tuple-packed Versionstamp): exact match

### What Needs Fixing
1. **BUG (Medium):** Three `ClearRange(subspace.Sub(...))` calls miss exact prefix keys.
   Most impactful in `UpdateRecordCountState(DISABLED)`. Fix: use `fdb.PrefixRange()`.

### What's Intentionally Different
1. Old version format not supported (Go always uses format version 9)
2. `omitUnsplitRecordSuffix` not supported (Go always appends suffix)
3. Build space lock preservation not implemented (Go clears everything)
4. `rebuildAllIndexes` not implemented (per-index rebuild only)
