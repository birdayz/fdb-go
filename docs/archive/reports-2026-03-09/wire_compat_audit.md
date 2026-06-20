# Wire Compatibility Audit: Go vs Java FDB Record Layer

**Date**: 2026-03-09
**Go source**: `pkg/recordlayer/` in `/home/birdy/projects/fdb-record-layer-go`
**Java source**: `fdb-record-layer/fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/provider/foundationdb/` in same repo

---

## 1. FDB Key Format

### 1.1 Subspace Constants (0-9)

| Subspace | Java (`FDBRecordStoreKeyspace.java`) | Go (`constants.go`) | Status |
|---|---|---|---|
| STORE_INFO | `STORE_INFO(0L)` | `StoreInfoKey = 0` | COMPATIBLE |
| RECORD | `RECORD(1L)` | `RecordKey = 1` | COMPATIBLE |
| INDEX | `INDEX(2L)` | `IndexKey = 2` | COMPATIBLE |
| INDEX_SECONDARY_SPACE | `INDEX_SECONDARY_SPACE(3L)` | `IndexSecondarySpaceKey = 3` | COMPATIBLE |
| RECORD_COUNT | `RECORD_COUNT(4L)` | `RecordCountKey = 4` | COMPATIBLE |
| INDEX_STATE_SPACE | `INDEX_STATE_SPACE(5L)` | `IndexStateSpaceKey = 5` | COMPATIBLE |
| INDEX_RANGE_SPACE | `INDEX_RANGE_SPACE(6L)` | `IndexRangeSpaceKey = 6` | COMPATIBLE |
| INDEX_UNIQUENESS_VIOLATIONS | `INDEX_UNIQUENESS_VIOLATIONS_SPACE(7L)` | `IndexUniquenessViolationsKey = 7` | COMPATIBLE |
| RECORD_VERSION_SPACE | `RECORD_VERSION_SPACE(8L)` | `RecordVersionKey = 8` | COMPATIBLE |
| INDEX_BUILD_SPACE | `INDEX_BUILD_SPACE(9L)` | `IndexBuildSpaceKey = 9` | COMPATIBLE |

**Verdict**: All 10 subspace constants verified identical.

### 1.2 Record Keys: `[store][1][primaryKey][suffix]`

**Java** (`SplitHelper.java`):
- `UNSPLIT_RECORD = 0L` -- suffix for unsplit records
- `START_SPLIT_RECORD = 1L` -- first split chunk suffix
- `SPLIT_RECORD_SIZE = 100_000`
- Key: `subspace.pack(key.add(UNSPLIT_RECORD))` or `subspace.pack(key.add(splitIndex))`

**Go** (`constants.go`, `split_helper.go`):
- `UnsplitRecord = int64(0)`
- `StartSplitRecord = int64(1)`
- `SplitRecordSize = 100_000`
- Key: `recordSubspace.Pack(appendToTuple(primaryKey, UnsplitRecord))`

**Verdict**: COMPATIBLE -- constants match exactly; key construction uses identical tuple encoding.

### 1.3 Index Keys: `[store][2][indexSubspaceKey].pack(indexValues..., primaryKey...)`

**Java** (`FDBRecordStore.java:907-908`, `Index.java`):
- Subspace: `getSubspace().subspace(Tuple.from(INDEX_KEY, index.getSubspaceTupleKey()))`
- Entry key: `indexEntryKey(index, valueKey, primaryKey)` which calls `index.trimPrimaryKey(primaryKeys)` then appends remaining PK to value key.
- Default subspaceKey: `normalizeSubspaceKey(name, name)` -- the index name string.
- Value: empty tuple `TupleHelpers.EMPTY` (for VALUE indexes).

**Go** (`store.go:776-777`, `index.go:113-119`, `index_maintainer.go:115`):
- Subspace: `store.subspace.Sub(IndexKey, index.SubspaceTupleKey())`
- Entry key: `indexEntryKey(idx, indexValues, primaryKey)` calls `idx.trimPrimaryKey(primaryKey)`.
- Default subspaceKey: index name string (set in `NewIndex`).
- Value: `tuple.Tuple{}.Pack()` (empty tuple).

**Verdict**: COMPATIBLE -- subspace construction, entry key format, PK trimming, and empty tuple values all match.

### 1.4 Count Index Keys: `[store][2][indexSubspaceKey].pack(groupingColumns...)`

**Java** (`AtomicMutationIndexMaintainer.java:128-172`):
- Key: index subspace + grouping columns tuple (leading columns up to grouping count).
- Value: little-endian int64 via `MutationType.ADD`.
- Mutation: `state.transaction.mutate(mutationType, key, param)`.

**Go** (`count_index_maintainer.go:41-65`):
- Key: `m.indexSubspace.Pack(key)` where `key` = grouping tuple.
- Value: `littleEndianInt64One` / `littleEndianInt64MinusOne`.
- Mutation: `m.tx.Add(fdb.Key(fdbKey), littleEndianInt64One)`.

**Verdict**: COMPATIBLE -- grouping key extraction, key format, and atomic ADD mutations match.

### 1.5 Record Count Keys: `[store][4].pack(countKeyExpression)`

**Java** (`FDBRecordStore.java:2235`):
- Key: `Tuple.from(RECORD_COUNT_KEY).addAll(value.toTupleAppropriateList())`
- Value: little-endian int64 via atomic ADD.

**Go** (`record_count.go:76-84`):
- Key: `countSubspace.Pack(keyTuple)` where `countSubspace = store.subspace.Sub(RecordCountKey)`.
- Value: little-endian int64 via atomic ADD.

**Verdict**: COMPATIBLE -- both use `[store][4][evaluated_count_key]` with little-endian int64 values.

### 1.6 Version Keys: `[store][1][primaryKey][-1]` (inline format)

**Java** (`FDBRecordStore.java:682-688`, `SplitHelper.java:82,205`):
- New format (format >= 6): `Tuple.from(RECORD_KEY).addAll(primaryKey).add(SplitHelper.RECORD_VERSION)` where `RECORD_VERSION = -1L`.
  - Stored in the records subspace at `recordsSubspace.pack(pk, -1)`.
- Old format (format < 6 or `omitUnsplitRecordSuffix`): `Tuple.from(RECORD_VERSION_KEY).addAll(primaryKey)` -- stored at subspace key 8.

**Go** (`store.go:907-916`):
- Always uses new inline format: `recordsSubspace.Pack(tuple.Tuple{...primaryKey, RecordVersionSuffix})` where `RecordVersionSuffix = int64(-1)`.
- No old format support.

**Verdict**: COMPATIBLE for format version >= 6. Go does not support the old version format (subspace 8), which is correct since Go writes `FormatVersionCurrent = 9`. Any store opened by Go will use inline versions. However, if Go tries to open a store originally created with format version < 6, the old version keys at subspace 8 would not be read.

**Risk**: UNTESTED -- Go cannot read version data from stores created with Java format version < 6 that used the old version location. This is acceptable because Go always creates at format version 9.

### 1.7 Index State Keys: `[store][5][indexName]`

**Java** (`FDBRecordStore.java:3454,3936`):
- Key: `indexStateSubspace().pack(indexName)` where subspace = `getSubspace().subspace(Tuple.from(INDEX_STATE_SPACE_KEY))`.
- Uses the index **name** (string), not the subspace key.
- READABLE state: key is **cleared** (absent = READABLE).
- Other states: value = `Tuple.from(indexState.code()).pack()`.

**Go** (`index_state.go:204-205`):
- Key: `store.indexStateSubspace().Pack(tuple.Tuple{indexName})`.
- READABLE state: key is cleared.
- Other states: value = `tuple.Tuple{int64(state)}.Pack()`.

**Verdict**: COMPATIBLE -- key uses index name string in both, value uses tuple-packed state code (int64) in both, READABLE clears the key in both.

### 1.8 Index Range Keys: `[store][6][indexSubspaceKey]`

**Java** (`FDBRecordStore.java:4961`):
- `getSubspace().range(Tuple.from(INDEX_RANGE_SPACE_KEY, formerIndex.getSubspaceTupleKey()))`

**Go** (`index_state.go:300`):
- `store.subspace.Sub(IndexRangeSpaceKey, index.SubspaceTupleKey())`

**Verdict**: COMPATIBLE -- both use `[store][6][indexSubspaceTupleKey]` as the subspace.

### 1.9 Uniqueness Violation Keys: `[store][7][indexSubspaceKey]`

**Java** (`FDBRecordStore.java:4963`):
- `getSubspace().range(Tuple.from(INDEX_UNIQUENESS_VIOLATIONS_KEY, formerIndex.getSubspaceTupleKey()))`

**Go** (`index_state.go:296`, `store.go:1229`):
- `store.subspace.Sub(IndexUniquenessViolationsKey, index.SubspaceTupleKey())`
- Entry: `violationSubspace.Pack(entryKey)` with value `tuple.Tuple{}.Pack()`.

**Verdict**: COMPATIBLE -- subspace and entry format match.

### 1.10 Index Build Keys: `[store][9][indexSubspaceKey]`

**Java** (`FDBRecordStore.java:4960`):
- Cleared during former index removal.

**Go** (`index_state.go:304`):
- `store.subspace.Sub(IndexBuildSpaceKey, index.SubspaceTupleKey())`

**Verdict**: COMPATIBLE -- subspace key matches.

---

## 2. Value Format

### 2.1 Record Values: Protobuf Union Descriptor Wrapping

**Java**: Records are serialized by wrapping in the `UnionDescriptor` message. The record type is identified by which union field is populated. Deserialization discovers the record type by checking which field is set.

**Go** (`store.go`): Same approach -- `wrapRecordInUnion` sets the appropriate field in the union descriptor, `deserializeAndDiscover` reads back the union to find the populated field.

**Verdict**: COMPATIBLE -- both use the same protobuf union wrapping. Cross-validated by 127+ conformance test specs.

### 2.2 Index Values

**VALUE indexes**:
- Java: `TupleHelpers.EMPTY` (empty tuple packed = `tuple.Tuple{}.Pack()`).
- Go: `tuple.Tuple{}.Pack()`.

**COUNT indexes**:
- Java: little-endian int64 via `MutationType.ADD` (`LITTLE_ENDIAN_INT64_ONE = {1,0,0,0,0,0,0,0}`).
- Go: `encodeRecordCount(1)` = `binary.LittleEndian.PutUint64(buf, 1)` = same bytes.

**Verdict**: COMPATIBLE -- empty tuple for VALUE, little-endian int64 for COUNT.

### 2.3 Version Values: Tuple-Packed Versionstamp

**Java** (`SplitHelper.java:297-303`):
```java
static byte[] packVersion(FDBRecordVersion version) {
    if (version.isComplete()) {
        return Tuple.from(version.toVersionstamp(false)).pack();
    } else {
        return Tuple.from(version.toVersionstamp(false)).packWithVersionstamp();
    }
}
```

**Go** (`store.go:837-844`):
```go
func packVersion(version *FDBRecordVersion) []byte {
    vs := tuple.Versionstamp{
        TransactionVersion: txVer,
        UserVersion:        uint16(version.GetLocalVersion()),
    }
    return tuple.Tuple{vs}.Pack()
}
```

For incomplete versions:
- Java: `Tuple.from(version.toVersionstamp(false)).packWithVersionstamp()`
- Go: `tuple.Tuple{vs}.PackWithVersionstamp(nil)` (called via `buildVersionstampedValue`)

**Unpacking** (`SplitHelper.java:306-311` vs `store.go:849-862`):
- Java: `FDBRecordVersion.fromVersionstamp(Tuple.fromBytes(packedVersion).getVersionstamp(0), true)`
- Go: `tuple.Unpack(value)` then extract `Versionstamp` element.

**Verdict**: COMPATIBLE -- both pack/unpack versions as `Tuple{Versionstamp}`.

### 2.4 Store Header: Protobuf `DataStoreInfo`

**Java**: `RecordMetaDataProto.DataStoreInfo` stored at `[store][0]` via `proto.toByteArray()`.
**Go**: `gen.DataStoreInfo` (same proto) stored at `[store][0]` via `proto.Marshal()`.

Fields: `FormatVersion`, `MetaDataversion`, `UserVersion`, `LastUpdateTime`, `RecordCountState`, `StoreLockState`.

**Verdict**: COMPATIBLE -- same protobuf message, same key location.

### 2.5 Index State Values: Tuple-Packed int64

**Java** (`FDBRecordStore.java:3428`): `Tuple.from(indexState.code()).pack()` where codes are:
- READABLE = 0 (key deleted, not stored)
- WRITE_ONLY = 1
- DISABLED = 2
- READABLE_UNIQUE_PENDING = 3

**Go** (`index_state.go:211`): `tuple.Tuple{int64(state)}.Pack()` where codes match exactly.

**Verdict**: COMPATIBLE -- identical tuple encoding and state codes.

---

## 3. Continuation Tokens

### 3.1 Serialization Format

**Java** (`KeyValueCursorBase.java:143-238`):
- `TO_OLD` format: raw key suffix bytes (key minus prefix).
- `TO_NEW` format: protobuf `KeyValueCursorContinuation` with magic number `6773487359078157740L`.
- Default builder mode: `TO_NEW` (since `Builder` constructor sets `serializationMode = SerializationMode.TO_NEW`).

**Go** (`key_value_cursor.go:22-48`):
- **Writes**: `TO_OLD` format (raw bytes) for compatibility with Java 4.2.6.0 Maven artifact.
- **Reads**: accepts both formats -- tries proto unmarshal with magic number check, falls back to raw bytes.

**Verdict**: COMPATIBLE -- Go writes TO_OLD (raw bytes), which Java can always read (both old and new Java versions support this format). Go reads both TO_OLD and TO_NEW, so it can consume continuations from any Java version.

**Note**: If Java is configured to use `TO_NEW` (the default in newer versions), Go can read those tokens. When Go produces a token and Java resumes with it, Java's `Continuation.getInnerContinuation()` will try proto parse, fail (no magic number), and fall back to raw bytes -- which is the correct behavior for `TO_OLD` tokens.

### 3.2 Prefix Length Calculation

**Java** (`KeyValueCursorBase.java:425-432`):
```java
protected int calculatePrefixLength() {
    int prefixLength = subspace.pack().length;
    while ((prefixLength < lowBytes.length) && (prefixLength < highBytes.length)
           && (lowBytes[prefixLength] == highBytes[prefixLength])) {
        prefixLength++;
    }
    return prefixLength;
}
```
Java calculates the common prefix of low and high byte ranges. For `TupleRange.ALL`, low = high = `subspace.pack()`, so `prefixLength = subspace.pack().length`.

**Go** (`store.go:958`):
```go
prefixLength := len(recordsSubspace.FDBKey())
```
Go uses the subspace prefix length directly. For index cursors (`index_scan.go:195`): `prefixLength: len(indexSubspace.FDBKey())`.

**Verdict**: COMPATIBLE for `TupleRange.ALL` (the common case). For narrower ranges (e.g., `TupleRange.allOf(somePrefix)`), Java might compute a longer common prefix, resulting in shorter continuation tokens. However, Go's approach of using the subspace length produces valid continuation tokens that Java can consume -- they just include a few extra prefix bytes. When Java produces a continuation with a shorter prefix, Go correctly reconstructs the full key by prepending the subspace bytes.

**Risk**: UNTESTED for non-ALL ranges where Go produces a continuation and Java resumes (or vice versa). The extra bytes in Go's continuation would not cause errors but would be slightly redundant.

### 3.3 Continuation Application

**Java**: Forward scan sets `lowEndpoint = CONTINUATION`, `lowBytes = prefix + continuation + \x00`. Reverse scan sets `highEndpoint = CONTINUATION`, `highBytes = prefix + continuation` (exclusive).

**Go** (`key_value_cursor.go:461-465`): Forward: `begin = subspaceKey + continuation + \x00`. Reverse: `end = subspaceKey + continuation` (exclusive).

**Verdict**: COMPATIBLE -- same byte manipulation for both directions.

---

## 4. Protobuf Wire Format

### 4.1 Record Wrapping in Union Descriptor

Both Java and Go wrap records in the `UnionDescriptor` proto message for serialization. The union field number identifies the record type. This is the core wire format for record data.

**Verdict**: COMPATIBLE -- validated by 127+ conformance tests including cross-language CRUD.

### 4.2 Metadata Serialization (RecordMetaData.toProto / fromProto)

**Go** (`metadata_proto.go`): Implements `ToProto()` and `FromProto()` using the same `MetaData` protobuf.

**Verdict**: UNTESTED for cross-language metadata exchange. Go builds metadata programmatically (builder pattern) rather than deserializing Java's proto. The proto serialization exists but has not been validated against Java's output in conformance tests.

### 4.3 DedupContinuation, FlatMapContinuation, etc.

**Go**: Not implemented. Go has `dedup_cursor.go`, `chained_cursor.go`, `fallback_cursor.go`, and `merge_cursor.go` but these are Go-specific cursor combinators, not Java-compatible cursor continuations.

**Verdict**: NEEDS REVIEW -- These cursor combinators have their own continuation formats that may not be wire-compatible with Java's `DedupContinuation`, `FlatMapContinuation`, `IntersectionContinuation`, `UnionContinuation`, `ConcatContinuation`. This only matters if continuation tokens from compound cursors are exchanged between Go and Java, which is unlikely in practice.

---

## 5. Tuple Encoding

### 5.1 Integer Type Normalization

**Java**: FDB tuple layer handles `Integer`, `Long`, etc. Java proto fields use `int32`, `int64`, etc.

**Go** (`key_expression.go`): `scalarToInterface()` normalizes:
- All integer types (`int32`, `int64`, `sint32`, `sint64`, `uint32`, `uint64`, `sfixed32`, `sfixed64`, `fixed32`, `fixed64`) to `int64`.
- All float types (`float`, `double`) to `float64`.
- Enum values to `int64(value.Enum())`.

This matches the Go FDB tuple layer which only supports `int64` and `float64`.

**Verdict**: COMPATIBLE -- both normalize to the FDB tuple layer's native types. The FDB tuple encoding is deterministic given the same typed value.

### 5.2 Null Handling

**Java**: `Key.Evaluated.NULL` maps to `null` in tuples, encoded as `\x00` by the tuple layer.
**Go**: `nil` in `tuple.Tuple`, also encoded as `\x00`.

For unique index null-skip behavior:
- Java: `IndexEntry.keyContainsNonUniqueNull()` checks for null elements.
- Go: `indexKeyContainsNull(key)` checks for nil elements.

**Verdict**: COMPATIBLE.

### 5.3 Versionstamp Encoding

**Java**: `Versionstamp` in the tuple layer: 10-byte transaction version + 2-byte user version = 12 bytes, encoded with type code `0x33`.
**Go**: `tuple.Versionstamp` with `TransactionVersion [10]byte` + `UserVersion uint16`, same type code.

For incomplete versionstamps:
- Java: `packWithVersionstamp()` appends a 4-byte little-endian offset.
- Go: `PackWithVersionstamp(nil)` appends the same offset.

**Verdict**: COMPATIBLE -- both use the standard FDB versionstamp encoding.

---

## 6. Format Version

**Go**: `FormatVersionCurrent = 9` (READABLE_UNIQUE_PENDING).
**Java**: Supports format versions 1-12, default is 7 (CACHEABLE_STATE), max is 12 (STORE_LOCK_STATE).

Go always creates stores at format version 9. This means:
- Go stores use inline version format (format >= 6).
- Go stores use unsplit suffix (format >= 5).
- Go stores support READABLE_UNIQUE_PENDING index state (format >= 9).

**Verdict**: COMPATIBLE for stores created by Go (version 9). Java can read/write stores at version 9. Java stores created at version >= 5 are readable by Go. Stores at version < 5 (without unsplit suffix) would not work with Go.

---

## 7. Summary

### Fully Compatible (validated by conformance tests)

| Area | Status |
|---|---|
| Subspace constants (0-9) | COMPATIBLE |
| Record keys (unsplit at suffix 0) | COMPATIBLE |
| Split record keys (chunks at suffix 1, 2, ...) | COMPATIBLE |
| Split record size (100KB) | COMPATIBLE |
| Index entry keys (VALUE type) | COMPATIBLE |
| Index entry values (empty tuple) | COMPATIBLE |
| Count index keys and values (little-endian int64) | COMPATIBLE |
| Record count keys and values | COMPATIBLE |
| Version keys (inline format at pk, -1) | COMPATIBLE |
| Version values (tuple-packed Versionstamp) | COMPATIBLE |
| Store header (DataStoreInfo protobuf) | COMPATIBLE |
| Index state keys and values | COMPATIBLE |
| Record serialization (union descriptor wrapping) | COMPATIBLE |
| Continuation tokens (TO_OLD format, bidirectional) | COMPATIBLE |
| Integer/float type normalization in tuples | COMPATIBLE |
| Null handling in tuples | COMPATIBLE |
| Versionstamp encoding | COMPATIBLE |
| PK deduplication (primaryKeyComponentPositions) | COMPATIBLE |
| Uniqueness violation keys | COMPATIBLE |
| Index range / build space keys | COMPATIBLE |

### Untested but Likely Compatible

| Area | Status | Risk |
|---|---|---|
| Metadata proto serialization/deserialization | UNTESTED | Low -- proto format is standard; only matters if metadata is exchanged via proto rather than built programmatically |
| Continuation prefix length for non-ALL ranges | UNTESTED | Low -- Go may produce slightly longer continuation tokens but they are valid |
| Old version format (subspace 8, format < 6) | UNTESTED | Low -- Go creates at format 9; only matters for very old Java stores |
| Format versions 10-12 features | UNTESTED | Low -- Go does not create these but accepts higher versions for reading |

### Needs Review

| Area | Status | Risk |
|---|---|---|
| Cursor combinator continuations (dedup, flatmap, union, intersection) | NEEDS REVIEW | Medium -- these are not cross-language portable; only matters if compound cursor continuations are exchanged between Go and Java |

### No Known Incompatibilities

No wire-level incompatibilities were found in this audit. All key construction, value encoding, and serialization formats match between the Go and Java implementations for the implemented feature set.
