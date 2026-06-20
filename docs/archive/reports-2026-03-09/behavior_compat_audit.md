# Behavioral Compatibility Audit: Go vs Java FDB Record Layer

**Date**: 2026-03-09
**Go source**: `/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/`
**Java source**: `/home/birdy/projects/fdb-record-layer-go/fdb-record-layer/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/`

---

## 1. SaveRecord Behavior

### 1.1 Overall saveRecordInternal flow

**Go**: `SaveRecordWithOptions()` in `store.go:232`
**Java**: `saveTypedRecord()` in `FDBRecordStore.java:536`

| Step | Java | Go | Status |
|------|------|----|--------|
| Extract record type + PK | Yes | Yes | MATCHES |
| Load existing record | `loadExistingRecord()` | `loadWithSplit()` | MATCHES |
| Existence checks | After load | After load | MATCHES |
| Lock validation | After load, before save | **Before load** (line 236) | **DIFFERS** |
| Serialize + save | `serializeAndSaveRecord()` | `saveWithSplit()` | MATCHES |
| Record count (insert only) | `addRecordCount(newRecord, +1)` | `addRecordCount(record, +1)` | MATCHES |
| Update secondary indexes | After save, after count | After save, after count | MATCHES |
| Version save | `recordVersionForSave()` + save | `ClaimLocalVersion()` + save | MATCHES |

- **MATCHES** Record count is only incremented for new inserts (not updates) in both implementations.
- **MATCHES** Both pass the old record's `SizeInfo` to `saveWithSplit`/`clearPreviousRecord` for proper cleanup.
- **MATCHES** Version is created with `IncompleteVersion(localVer)` matching Java's `FDBRecordVersion.incomplete(context.claimLocalVersion())`.

### 1.2 validateRecordUpdateAllowed timing

- **Java**: Called AFTER loading old record, AFTER existence check, BEFORE serialization/save (line 577-578). This means a locked store still allows loading and existence checks to succeed; the error only fires when actually trying to write.
- **Go**: Called FIRST (line 236), before any read. A locked store immediately rejects the call without even checking if the record exists.

**Status**: :x: DIFFERS

**Impact**: Semantic difference in locked stores. Java would return `RecordDoesNotExistError` for a non-existent record in a locked store (if ERROR_IF_NOT_EXISTS). Go would return `StoreIsLockedForRecordUpdatesError` first, masking the existence error. In practice, locked stores are rare and the lock is the dominant concern, but the error precedence differs.

### 1.3 RecordExistenceCheck modes (5 modes)

**Go**: `existence_check.go`
**Java**: `FDBRecordStoreBase.java:394`

| Mode | Go | Java | Status |
|------|-----|------|--------|
| NONE | `RecordExistenceCheckNone` | `NONE` | MATCHES |
| ERROR_IF_EXISTS | `RecordExistenceCheckErrorIfExists` | `ERROR_IF_EXISTS` | MATCHES |
| ERROR_IF_NOT_EXISTS | `RecordExistenceCheckErrorIfNotExists` | `ERROR_IF_NOT_EXISTS` | MATCHES |
| ERROR_IF_RECORD_TYPE_CHANGED | `RecordExistenceCheckErrorIfTypeChanged` | `ERROR_IF_RECORD_TYPE_CHANGED` | MATCHES |
| ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED | `RecordExistenceCheckErrorIfNotExistsOrTypeChanged` | `ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED` | MATCHES |

The boolean methods (`ErrorIfExists()`, `ErrorIfNotExists()`, `ErrorIfTypeChanged()`) all return the same values for each enum variant.

**Status**: :white_check_mark: MATCHES

### 1.4 Save with same PK but different record type

**Java**: The type-changed check uses `oldRecord.getRecordType() != recordType` (reference equality on RecordType). If `RecordExistenceCheckNone`, this silently overwrites.

**Go**: The type-changed check compares `existingTypeName != recordTypeName` (string comparison). With `RecordExistenceCheckNone`, also silently overwrites.

**Status**: :white_check_mark: MATCHES -- both allow cross-type overwrites with NONE, both error on ERROR_IF_RECORD_TYPE_CHANGED.

### 1.5 Save with split records

**Java**: `SplitHelper.saveWithSplit()` uses `SPLIT_RECORD_SIZE = 100_000`. Chunks at suffixes 1, 2, 3, etc.
**Go**: `saveWithSplit()` uses `SplitRecordSize = 100_000`. Same chunk numbering.

**Java**: `clearPreviousRecord` uses `clearBasedOnPreviousSizeInfo` to decide whether to range-clear or point-clear.
**Go**: `clearPreviousRecord()` also checks `oldSizeInfo.IsSplit || splitLongRecords` for range vs point clear.

**Java**: When `splitLongRecords` is false and record exceeds limit, Java throws.
**Go**: Returns error `"record size %d exceeds limit %d and splitLongRecords is not enabled"`.

**Status**: :white_check_mark: MATCHES

### 1.6 Version format

**Java**: Has two version storage formats:
- **Old format** (format version < 6 or `omitUnsplitRecordSuffix`): Version stored at `subspace[RECORD_VERSION_KEY=8][primaryKey]` with raw bytes
- **New format** (format version >= 6): Version stored inline at `subspace[RECORD_KEY=1][primaryKey][-1]` as packed Tuple(Versionstamp)

**Go**: Always uses the new inline format. Never uses the old `RecordVersionKey` (8) subspace. The `versionKey()` function always constructs `recordsSubspace.pack(primaryKey, -1)`.

**Status**: :warning: LIKELY MATCHES for format version >= 6 (which Go always creates). If Go opens a store originally created by Java with format version < 6, version data would be invisible to Go (stored in subspace 8, Go looks in subspace 1). This is unlikely in practice since Go creates stores at format version 9.

---

## 2. DeleteRecord Behavior

### 2.1 Delete flow

**Go**: `DeleteRecord()` in `store.go:126`
**Java**: `deleteTypedRecord()` in `FDBRecordStore.java:1669`

| Step | Java | Go | Status |
|------|------|----|--------|
| Lock validation timing | After load (line 1681) | **Before load** (line 127) | **DIFFERS** |
| Load existing record | `loadTypedRecord()` | `loadWithSplit()` | MATCHES |
| Return false if not found | Yes | Yes | MATCHES |
| Clear record data | `deleteRecordSplits()` | `deleteSplit()` | MATCHES |
| Decrement count | `addRecordCount(-1)` | `addRecordCount(-1)` | MATCHES |
| Update secondary indexes | `updateSecondaryIndexes(old, null)` | `updateSecondaryIndexes(old, nil)` | MATCHES |
| Version cleanup | Conditional on old/new format | Always inline format | LIKELY MATCHES |

### 2.2 Delete lock validation timing

Same issue as save: Go checks lock FIRST (before loading record), Java checks AFTER loading.

**Status**: :x: DIFFERS -- same timing difference as SaveRecord.

### 2.3 Version cleanup on delete

**Java**:
```java
// Old format: explicit clear + removeVersionMutation
if (useOldVersionFormat()) {
    byte[] versionKey = ...;
    if (oldHasIncompleteVersion) {
        context.removeVersionMutation(versionKey);
    } else if (metaData.isStoreRecordVersions()) {
        ensureContextActive().clear(versionKey);
    }
}
// New format: deleteSplit range-clears including the -1 suffix
```

**Go**: Always clears the inline version key. `deleteSplit()` range-clears the primary key subspace (covering suffix -1). Additionally, explicitly calls `RemoveLocalVersion()` and `RemoveVersionMutation()`.

**Java**: For new format, `deleteSplit` range-clears the PK subspace (covering suffix -1), which matches. Java removes `localVersion` only for incomplete versions. Go always calls `RemoveLocalVersion` and `RemoveVersionMutation`.

**Status**: :warning: LIKELY MATCHES -- Go is more aggressive about cleanup (removing version mutations even if version was complete), but this is harmless since removing a non-existent mutation is a no-op.

### 2.4 Deleting a non-existent record

**Java**: Returns `false` (via `AsyncUtil.READY_FALSE`).
**Go**: Returns `false, nil`.

**Status**: :white_check_mark: MATCHES

### 2.5 DeleteAllRecords

**Java** (`FDBRecordStore.java:1757`):
```java
// Two range clears that skip StoreInfoKey (0) and IndexStateSpaceKey (5):
context.clear(new Range(recordsSubspace().getKey(), indexStateRange.begin));
context.clear(new Range(indexStateRange.end, getSubspace().range().end));
```

**Go** (`store.go:529`): Clears individual subspaces 1, 2, 3, 4, 6, 7, 8, 9. Then explicitly sets count to 0.

**Java**: Does NOT explicitly reset count to 0 after range clear. The range clear of subspace 4 zeros it. But Java also requires `checkVersion` to have been called first (throws if `recordStoreStateRef.get() == null`).
**Go**: Does NOT check that `checkVersion`/Open was called first. Go always resets the count to 0 via explicit Set (because ClearRange + atomic Add in same tx can leave stale values).

**Status**: :warning: LIKELY MATCHES -- The net effect is the same (all data cleared except store info and index states). Go's explicit count reset is actually more correct for within-transaction consistency. Go missing the `recordStoreStateRef` null check is irrelevant because Go's `storeHeader` is set during Create/Open/CreateOrOpen.

---

## 3. Index Maintenance

### 3.1 Update flow (old->new diff)

**Go**: `StandardIndexMaintainer.Update()` in `index_maintainer.go:67`
**Java**: `StandardIndexMaintainer.update()` in `StandardIndexMaintainer.java:215`

| Aspect | Java | Go | Status |
|--------|------|----|--------|
| Evaluate old entries | `filteredIndexEntries(oldRecord)` | `evaluateIndex(oldRecord)` | MATCHES |
| Evaluate new entries | `filteredIndexEntries(newRecord)` | `evaluateIndex(newRecord)` | MATCHES |
| Common key skip | `commonKeys()` + `removeAll()` | `removeCommonEntries()` | MATCHES |
| Remove old first | Yes (`oldUpdate` before `newUpdate`) | Yes (old loop before new loop) | MATCHES |
| Add new entries | `updateOneKeyAsync(remove=false)` | Direct `tx.Set()` | MATCHES |
| Value stored | `value.pack()` (empty tuple) | `tuple.Tuple{}.Pack()` | MATCHES |

**Status**: :white_check_mark: MATCHES

### 3.2 Uniqueness check flow

**Java** (`StandardIndexMaintainer.java:519`):
1. Scans `state.indexSubspace.range(valueKey)` (prefix range of index key values)
2. For each existing entry, unpacks and extracts PK
3. If PK differs: WRITE_ONLY -> `addUniquenessViolation()` for both PKs; else throw `RecordIndexUniquenessViolation`
4. Adds a pre-commit check via `addIndexUniquenessCommitCheck()`

**Go** (`index_maintainer.go:164`):
1. Scans `PrefixRange(prefixKey)` with `Limit: 1`
2. If entry found, unpacks and extracts PK
3. If PK differs: WRITE_ONLY -> `addUniquenessViolation()` for both PKs; else return `RecordIndexUniquenessViolationError`
4. **Does NOT add a pre-commit check**

Key differences:

**Scan scope**: Java scans ALL entries with the same index value (no limit). Go scans with `Limit: 1`. If there are 3 records with the same unique key, Java detects ALL conflicts (and records violations for all pairs), Go only detects the first one.

**Pre-commit check**: Java adds `addIndexUniquenessCommitCheck()` which validates uniqueness at commit time (catching concurrent inserts). Go has no equivalent -- if two concurrent transactions insert conflicting unique values, Go relies solely on FDB's read-your-writes and won't detect the race until a later transaction.

**Status**: :x: DIFFERS

**Impact**:
1. The `Limit: 1` is mostly fine for READABLE indexes (there should only ever be 0 or 1 conflicting entry). For WRITE_ONLY indexes, Go may miss recording all violation pairs.
2. Missing pre-commit check means Go has a theoretical race condition for unique index violations in concurrent transactions.

### 3.3 Null key handling for uniqueness

**Java**: `indexEntry.keyContainsNonUniqueNull()` -- skips uniqueness check if any key component is null.
**Go**: `indexKeyContainsNull(key)` -- same behavior.

**Status**: :white_check_mark: MATCHES

### 3.4 COUNT index: atomic ADD

**Java** (`AtomicMutationIndexMaintainer.java:125`):
- Groups by `groupPrefixSize` columns
- Uses `MutationType.ADD` with little-endian int64
- Key: `indexSubspace.pack(groupKey)`
- Not idempotent: `isIdempotent()` returns `mutation.isIdempotent()` (false for COUNT)
- `skipUpdateForUnchangedKeys()`: returns `!IndexTypes.COUNT_UPDATES.equals(state.index.getType())`

**Go** (`count_index_maintainer.go:41`):
- Groups by `getGroupingCount()` columns
- Uses `tx.Add()` with little-endian int64
- Key: `indexSubspace.Pack(groupKey)`
- `Update()` always increments/decrements (no common-key skip)

**Status**: :white_check_mark: MATCHES -- For COUNT type, Java's `skipUpdateForUnchangedKeys()` returns true, so it also does the common-key skip in the parent `update()` call. But wait -- Go's `CountIndexMaintainer.Update()` doesn't call `removeCommonEntries()`. This means Go will decrement and re-increment unchanged grouping keys on update, which is functionally equivalent (net zero via atomic ADD) but creates extra mutations.

Revised: :warning: LIKELY MATCHES -- functionally equivalent but Go creates unnecessary atomic mutations on updates where the grouping key doesn't change.

### 3.5 Changing indexed value on save

When a record is updated and the indexed field changes:

**Java**: `update(oldRecord, newRecord)` -> removes old entries first (via `updateIndexKeysFunction(oldRecord, true, ...)`), then adds new entries. Common entries skipped.
**Go**: Same pattern: old entries cleared first, then new entries set. Common entries skipped via `removeCommonEntries()`.

**Status**: :white_check_mark: MATCHES

### 3.6 WRITE_ONLY index behavior

**Java** (`StandardIndexMaintainer.java:255`):
- If idempotent (VALUE indexes): pass-through to `update()`
- If non-idempotent: checks range set to see if PK is in already-built range; only updates if in range

**Go** (`index_maintainer.go:61`):
- `StandardIndexMaintainer.UpdateWhileWriteOnly()`: pass-through to `Update()`
- `CountIndexMaintainer.UpdateWhileWriteOnly()`: pass-through to `Update()`

**Status**: :x: DIFFERS for COUNT indexes

**Impact**: Java's `AtomicMutationIndexMaintainer` is NOT idempotent, so `updateWhileWriteOnly` checks the range set. Go's `CountIndexMaintainer` always updates regardless of the build progress, which can cause double-counting during online index builds. For VALUE indexes, both are correct (idempotent pass-through).

### 3.7 updateSecondaryIndexes: record type change handling

**Java** (`FDBRecordStore.java:706`): When old and new records have different types, Java:
1. Computes old indexes (type-specific + universal + multi-type) and new indexes separately
2. Finds common indexes between old and new
3. For old-only indexes: calls `update(old, null)` (delete from index)
4. For new-only indexes: calls `update(null, new)` (insert into index)
5. For common indexes: calls `update(old, new)` (full update with common-key skip)

**Go** (`store.go:569`): Uses ONE record type (preferring new, fallback to old) and iterates that type's indexes. Does NOT handle the case where old and new have different record types with different index sets.

**Status**: :x: DIFFERS

**Impact**: If a record with PK=42 is saved as type A (with index X), then overwritten as type B (with index Y, no index X), Go would:
- Use type B's indexes only
- Add to index Y correctly
- NOT remove from index X (orphan entries)

Java handles this correctly by computing the index diff between old and new types.

In practice, this only matters when `RecordExistenceCheckNone` is used with cross-type overwrites, which is uncommon.

### 3.8 Key/value size validation

**Java** (`StandardIndexMaintainer.java:684`): `checkKeyValueSizes()` validates that index entry key and value don't exceed FDB limits before writing. Throws `FDBStoreKeySizeException` or `FDBStoreValueSizeException`.

**Go**: No equivalent check. Relies on FDB itself to reject oversized keys/values.

**Status**: :x: DIFFERS -- Go omits pre-write size validation. FDB will reject the transaction at commit time with a less informative error.

---

## 4. Store Open/Create Behavior

### 4.1 Create

**Java**: Creates store header, checks existence first (via `checkAndParseStoreHeader`).
**Go**: `Create()` in `store.go:1386`. Checks `checkStoreExists()`, fails if exists, writes header.

**Status**: :white_check_mark: MATCHES

### 4.2 Open

**Java**: `checkVersion()` validates format version, handles `omitUnsplitRecordSuffix`, checks metadata version, calls `checkPossiblyRebuild()`.
**Go**: `Open()` validates format version, loads index states, calls `checkPossiblyRebuild()`.

Key differences:

**omitUnsplitRecordSuffix**: Java reads this from the store header and applies it. Go does not have this concept -- it always uses the suffix. This means Go cannot correctly read stores created at format version < 5 where unsplit records don't have the 0 suffix.

**Status**: :warning: LIKELY MATCHES for stores created at format version >= 5 (Go creates at version 9). :x: DIFFERS for legacy stores created at very old format versions.

### 4.3 CreateOrOpen

**Java**: Uses `checkVersion()` with `StoreExistenceCheck.ERROR_IF_NO_INFO_AND_NOT_EMPTY`.
**Go**: Checks existence, creates if not exists, validates format version + rebuilds if exists.

**Status**: :white_check_mark: MATCHES

### 4.4 checkPossiblyRebuild (metadata version change)

**Java** (`FDBRecordStore.java:4482`):
1. Compares old vs new metadata version
2. If old > new: throws `RecordStoreStaleMetaDataVersionException`
3. Gets indexes to build since old version
4. Uses `UserVersionChecker` (or default) to decide: rebuild inline, WRITE_ONLY, or DISABLED
5. Default: inline for <= 200 records, DISABLED otherwise
6. Updates store header with new format version and metadata version

**Go** (`store.go:683`):
1. Compares old vs new metadata version
2. If new <= old: returns nil (no-op) -- **does NOT error on old > new**
3. Gets indexes to build since old version
4. Uses `IndexRebuildPolicy` (default matches Java's 200-record threshold)
5. Updates store header

**Status**: :x: DIFFERS

**Impact**: Go does NOT detect stale metadata (where the stored version is NEWER than the local code's version). Java throws `RecordStoreStaleMetaDataVersionException`. Go silently proceeds. This could cause data corruption if an older version of Go code opens a store that was evolved by newer code.

### 4.5 Store header format version checks

**Java**: Validates format version with `FormatVersion.validateFormatVersion()`. Minimum version is `MIN_FORMAT_VERSION` (1).
**Go**: Validates `storedVersion > FormatVersionCurrent` (rejects future versions). No minimum version check.

**Status**: :warning: LIKELY MATCHES -- Go's check is simpler but sufficient for the common case (rejecting unknown future versions).

### 4.6 Record count rebuild on version change

**Java**: `checkPossiblyRebuildRecordCounts()` compares stored count key expression with current, rebuilds if changed.
**Go**: Does NOT rebuild record counts on metadata version change. Only rebuilds indexes.

**Status**: :x: DIFFERS -- If the count key expression changes between metadata versions, Go will not detect or rebuild counts.

---

## 5. Cursor Behavior

### 5.1 Limit handling

| Limit Type | Java | Go | Status |
|-----------|------|----|--------|
| Row limit | `valuesLimit` passed to FDB | `ReturnedRowLimit` in `OnNext()` | MATCHES |
| Byte limit | `CursorLimitManager.reportScannedBytes()` | `ScannedBytesLimit` in `OnNext()` | MATCHES |
| Time limit | `CursorLimitManager.tryRecordScan()` | `TimeLimit` in `OnNext()` | MATCHES |
| Scan limit | `CursorLimitManager.tryRecordScan()` | `ScannedRecordsLimit` in `OnNext()` | MATCHES |

**Java** checks limits BEFORE reading (via `tryRecordScan()`). Go checks time and scan limits before reading, byte limit after reading (since byte count isn't known until after). Row limit is checked before reading.

**Status**: :white_check_mark: MATCHES -- both allow at least one record before enforcing out-of-band limits.

### 5.2 Continuation semantics

**Java**: Continuation is the key suffix (relative to subspace prefix) of the last returned key. For forward scans, the continuation key + `\x00` becomes the new begin. For reverse scans, the continuation key becomes the new end (exclusive).

**Go**: Same approach. `wrapContinuation()` returns raw key suffix bytes. Forward: `append(fullKey, 0x00)`. Reverse: `fullKey` as end.

**Serialization format**: Both use TO_OLD (raw bytes) format for production. Go reads both TO_OLD and TO_NEW (proto-wrapped) via `unwrapContinuation()`.

**Status**: :white_check_mark: MATCHES

### 5.3 Split record reassembly during scan

**Java**: Uses `KeyValueUnsplitter` to collect chunks, sort by suffix, reassemble.
**Go**: `readSplitRecord()` collects chunks into `splitChunk` slice, sorts by suffix (insertion sort), validates sequential indices, reassembles.

Both handle:
- Version keys (suffix -1) skipped
- Chunks sorted for reverse scan correctness
- Buffering the first KV of the next record

**Status**: :white_check_mark: MATCHES

### 5.4 Empty result handling

**Java**: When iterator returns false, checks if `valuesSeen >= valuesLimit` to distinguish `RETURN_LIMIT_REACHED` from `SOURCE_EXHAUSTED`.
**Go**: `hasMoreKVs()` peeks at the iterator. If no more KVs AND limit reached, returns `SourceExhausted`.

**Status**: :white_check_mark: MATCHES

### 5.5 FDB-level row limit optimization

**Java**: Passes `limit + 1` to FDB for the peek-ahead check.
**Go**: Passes `ReturnedRowLimit - recordsRead + 1` to FDB. Disables FDB-level limit when `splitLongRecords` is enabled.

**Status**: :white_check_mark: MATCHES

### 5.6 Skip handling

**Java**: Uses `KeySelector.add(-skip)` for reverse scans, `firstGreaterOrEqual` for forward scans. Skip is handled at the FDB range level.
**Go**: Handles skip in `OnNext()` via recursion: skipped records are counted as scanned but not returned. FDB limit includes skip count.

**Status**: :warning: LIKELY MATCHES -- different implementation strategy but equivalent behavior.

---

## 6. Error Conditions

### 6.1 Error type mapping

| Java Exception | Go Error | Status |
|---|---|---|
| `RecordAlreadyExistsException` | `*RecordAlreadyExistsError` | MATCHES |
| `RecordDoesNotExistException` | `*RecordDoesNotExistError` | MATCHES |
| `RecordTypeChangedException` | `*RecordTypeChangedError` | MATCHES |
| `RecordIndexUniquenessViolation` | `*RecordIndexUniquenessViolationError` | MATCHES |
| `StoreIsLockedForRecordUpdates` | `*StoreIsLockedForRecordUpdatesError` | MATCHES |
| `RecordStoreAlreadyExistsException` | `ErrRecordStoreAlreadyExists` | MATCHES |
| `RecordStoreNoInfoAndNotEmptyException` | `ErrRecordStoreNoInfoButNotEmpty` | MATCHES |
| `RecordStoreStaleMetaDataVersionException` | **NOT IMPLEMENTED** | DIFFERS |
| `FDBStoreKeySizeException` | **NOT IMPLEMENTED** | DIFFERS |
| `FDBStoreValueSizeException` | **NOT IMPLEMENTED** | DIFFERS |
| `RecordCoreException("checkVersion must be called...")` | Not applicable (different structure) | N/A |

### 6.2 Missing error conditions in Go

1. **Stale metadata version**: Java throws `RecordStoreStaleMetaDataVersionException` when local metadata version < stored version. Go silently ignores.
2. **Key/value size limits on index entries**: Java validates before write. Go relies on FDB rejection.
3. **checkVersion not called**: Java requires `checkVersion` before `deleteAllRecords`. Go doesn't have this guard (but it's effectively always called since stores are created via builder).

**Status**: :x: DIFFERS for stale metadata detection and key/value size validation.

---

## 7. Edge Cases

### 7.1 Zero-length records

**Java**: A record that serializes to zero bytes would be stored as an empty value at the unsplit key. `loadWithSplit()` returns the empty byte array.
**Go**: Same behavior. `saveWithSplit()` stores empty data at suffix 0. `loadWithSplit()` returns the empty byte array.

**Status**: :warning: LIKELY MATCHES

### 7.2 Empty primary keys

**Java**: An empty tuple `Tuple.from()` as a primary key is technically valid. The subspace key would be just the records subspace prefix + suffix.
**Go**: An empty `tuple.Tuple{}` would work the same way. `primaryKey.Pack()` produces an empty byte array, and the suffix is appended.

**Status**: :warning: LIKELY MATCHES

### 7.3 Nil/null field values in key expressions

**Java**: `Key.Evaluated.NULL` represents a null field value. Stored as `null` in tuple (FDB tuple code `\x00`).
**Go**: `FieldKeyExpression.Evaluate()` on a nil message returns `[][]interface{}{{nil}}`. For an unset field, returns the zero value of the proto field type.

Difference: Go returns the proto zero value (e.g., `""` for string, `int64(0)` for int) for unset fields, while Java returns `null` for fields that have `hasField() == false` (proto2) or are unset message fields.

**Status**: :warning: LIKELY MATCHES for proto3 (where all fields have default values). :mag: NEEDS INVESTIGATION for proto2 optional fields where Java distinguishes "unset" from "zero value".

### 7.4 Maximum record size handling

**Java**: `SplitHelper.saveWithSplit()` handles records up to any size by splitting into 100KB chunks. Without `splitLongRecords`, throws if > 100KB.
**Go**: Same behavior. Returns error if > `SplitRecordSize` without split enabled.

**Status**: :white_check_mark: MATCHES

### 7.5 Transaction size limit handling

Neither Java nor Go explicitly check the 10MB transaction size limit. Both rely on FDB to reject the transaction at commit time.

**Status**: :white_check_mark: MATCHES (both punt to FDB)

### 7.6 Concurrent modification safety

**Java**: Uses `synchronized(state.context)` in `updateOneKeyAsync()` for unique index writes to prevent intra-transaction races. Also adds `addIndexUniquenessCommitCheck()` for cross-transaction safety.
**Go**: No synchronization (Go transactions are typically used single-threaded). No commit checks.

**Status**: :warning: LIKELY MATCHES -- Go's single-goroutine transaction model makes synchronization unnecessary. The missing commit check is a theoretical concern for unique indexes but would only manifest in unusual concurrent-goroutine transaction usage patterns.

---

## Summary

### Critical Issues (:x: DIFFERS)

1. **updateSecondaryIndexes does not handle cross-type overwrites** (`store.go:569`): When saving a record that changes type (same PK, different message type), Go doesn't remove index entries from the old type's indexes. Orphan index entries accumulate.

2. **Stale metadata detection missing** (`store.go:687`): Go does not detect when the stored metadata version is newer than the local version. Should throw an error like Java's `RecordStoreStaleMetaDataVersionException`.

3. **validateRecordUpdateAllowed timing** (`store.go:236,127`): Go validates the lock before loading the existing record. Java validates after load but before write. Changes error precedence.

4. **Unique index pre-commit check missing** (`index_maintainer.go:164`): Java adds `addIndexUniquenessCommitCheck()` for deferred validation. Go has no equivalent. Concurrent transactions could both pass the uniqueness check and commit conflicting values.

5. **COUNT index UpdateWhileWriteOnly** (`count_index_maintainer.go:71`): Go passes through to Update() unconditionally. Java checks the range set for non-idempotent indexes. This can cause double-counting during online COUNT index builds.

6. **Record count rebuild on metadata version change** (`store.go:683`): Go does not rebuild counts when the count key expression changes between metadata versions.

### Medium Issues (:warning: LIKELY MATCHES / Functional but Suboptimal)

7. **Key/value size validation missing on index entries**: Java validates sizes before write; Go relies on FDB rejection with less informative errors.

8. **Old version format not supported**: Go always uses inline version format (suffix -1). Cannot read version data from stores created at format version < 6 using the old `RECORD_VERSION_KEY` (8) subspace.

9. **COUNT index unnecessary mutations**: Go doesn't skip common grouping keys on updates, creating extra no-op atomic mutations.

10. **Skip implementation difference**: Go uses recursive `OnNext()` calls; Java adjusts the FDB `KeySelector`. Equivalent behavior but different performance characteristics for large skips.

### Verified Correct (:white_check_mark: MATCHES)

- RecordExistenceCheck all 5 modes
- Save flow (load old, check, serialize, save, count, index, version)
- Delete flow (load, clear splits, count, index, version cleanup)
- DeleteAllRecords (clears correct subspaces)
- Split record handling (100KB threshold, chunk numbering, reassembly)
- Continuation token format (TO_OLD, bidirectional read)
- Index entry format ([indexValues..., primaryKey...])
- Common-entry skip optimization
- Uniqueness violation recording for WRITE_ONLY indexes
- Cursor limit handling (row, byte, time, scan)
- Subspace constants (0-9)
- Version storage format (Tuple-packed Versionstamp)
- Index state management (READABLE, WRITE_ONLY, DISABLED, READABLE_UNIQUE_PENDING)
- Store header format (protobuf DataStoreInfo)
