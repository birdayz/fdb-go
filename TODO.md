# fdb-record-layer-go TODO

Actionable work items for the Go port of Apple's FoundationDB Record Layer.
Severity: **CRITICAL** = blocks correctness/compatibility, **HIGH** = important for production quality, **MEDIUM** = improvement, **LOW** = nice-to-have.

Conformance audit performed 2026-03-08 comparing Go implementation method-by-method against Java source at `fdb-record-layer/`. Full API surface review performed 2026-03-16 across 5 areas (store CRUD, indexes, metadata, cursors, DB/context/key expressions).

**Java Record Layer version**: 4.10.6.0 (upgraded from 4.2.6.0 on 2026-03-11). All 1525 specs pass (1165 unit/integration + 360 conformance). Java source at `fdb-record-layer/` checked out at tag 4.10.6.0. All 15 proto files synced from Java source.

---

## 4.10.6.0 upgrade assessment

Upgraded from 4.2.6.0 → 4.10.6.0 (2026-03-11). 548 commits across 8 minor versions. All 1012 conformance+unit tests pass unchanged. All 15 proto files synced from Java source. Below is a thorough analysis of all changes, organized by priority.

### 1. Wire format / storage changes (MUST address for compatibility)

#### 1a. New FormatVersions (8–14)

Java added 7 new format versions. We must handle them correctly on open/create:

| FmtVer | Name | Feature | Priority |
|--------|------|---------|----------|
| 8 | HEADER_USER_FIELDS | `DataStoreInfo.user_field` — user-defined key→bytes map in store header | **MEDIUM** |
| 9 | READABLE_UNIQUE_PENDING | New `IndexState` for unique indexes with pending violations | **HIGH** |
| 10 | CHECK_INDEX_BUILD_TYPE_DURING_UPDATE | Non-idempotent index build-from-source validation | **LOW** |
| 11 | RECORD_COUNT_STATE | `DataStoreInfo.record_count_state` enum (READABLE/WRITE_ONLY/DISABLED) | **DONE** (already implemented) |
| 12 | STORE_LOCK_STATE | `DataStoreInfo.store_lock_state` with FORBID_RECORD_UPDATE + FULL_STORE | **HIGH** |
| 13 | INCARNATION | `DataStoreInfo.incarnation` (int32) for cross-cluster migration | **MEDIUM** |
| 14 | FULL_STORE_LOCK | Unknown lock states now prevent store opening (stricter validation) | **HIGH** |

- [x] **FULL_STORE lock state + stricter validation (FormatVersion 12+14)** — Implemented: `validateStoreLockState()` on open, `StoreIsFullyLockedError`, `UnknownStoreLockStateError`, `SetBypassFullStoreLockReason()` on builder. `FormatVersionCurrent` bumped to 14. 5 new tests (prevents Open/CreateOrOpen, bypass with matching/wrong reason, clear lock). **HIGH**.
- [x] **READABLE_UNIQUE_PENDING index state (FormatVersion 9)** — Full behavioral parity with Java: `MarkIndexReadable` checks `firstUnbuiltRange` + rejects unique violations, `MarkIndexReadableOrUniquePending` transitions to READABLE_UNIQUE_PENDING when violations exist, `OnlineIndexer` uses the unique-pending variant, build data cleared on READABLE but retained for READABLE_UNIQUE_PENDING. 15 new tests. **HIGH**.
- [x] **Store incarnation field (FormatVersion 13)** — Implemented: `GetIncarnation()`, `UpdateIncarnation(updater)` (must strictly increase). `get_versionstamp_incarnation()` now available via `FunctionKeyExpression`. **MEDIUM**.
- [x] **Header user fields (FormatVersion 8)** — Implemented: `GetHeaderUserField(key)`, `SetHeaderUserField(key, value)`, `ClearHeaderUserField(key)`. **MEDIUM**.
- [ ] **Continuation serialization evolution** — 4.5.x enabled proto-wrapped `AggregateCursorContinuation`. 4.8.x enabled new `KeyValueCursorBaseContinuation` serialization. Our TO_OLD format still works (confirmed by conformance tests). No action needed unless we add aggregate cursors. **LOW**.

#### 1b. Store header proto changes (DataStoreInfo)

New fields in wire format (all optional, safe to round-trip via protobuf):
- `omit_unsplit_record_suffix` (field 6, bool) — already respected in our split logic
- `cacheable` (field 7, bool) — for `MetaDataVersionStampStoreStateCache`
- `user_field` (field 8, repeated UserFieldEntry) — see above
- `record_count_state` (field 9, enum) — **DONE**
- `store_lock_state` (field 10, StoreLockState) — see above
- `incarnation` (field 11, int32) — see above

#### 1c. Subspace layout

**UNCHANGED.** Still 10 subspaces (0–9). No new subspace constants added.

#### 1d. Split records / index entries

**UNCHANGED.** SPLIT_RECORD_SIZE=100KB, UNSPLIT_RECORD=0, START_SPLIT_RECORD=1, RECORD_VERSION=-1. Index entry format unchanged (key=[indexValues..., trimmedPK...], value=empty tuple or tuple-packed for covering).

### 2. New index types (not yet in Go)

| Type | Maintainer | Mutation/Storage | Priority | Notes |
|------|-----------|-----------------|----------|-------|
| TEXT | `TextIndexMaintainer` | BunchedMap token storage | **LOW** | Full-text search with pluggable tokenizers |
| BITMAP_VALUE | `BitmapValueIndexMaintainer` | Position bitmaps (10K–250K bits per entry) | **LOW** | Sparse position indexing |
| PERMUTED_MIN | `PermutedMinMaxIndexMaintainer` | Permuted grouping columns for value-ordered min | **LOW** | Enumerate extrema by value, not group |
| PERMUTED_MAX | `PermutedMinMaxIndexMaintainer` | Same, max variant | **LOW** | Same as above |
| MAX_EVER_VERSION | `AtomicMutationIndexMaintainer` | SET_VERSIONSTAMPED_VALUE | **MEDIUM** | Like MAX_EVER_TUPLE but version-aware |
| MULTIDIMENSIONAL | `MultidimensionalIndexMaintainer` | Hilbert R-tree spatial indexing | **LOW** | Specialized spatial use case |
| VECTOR | `VectorIndexMaintainer` | HNSW graph for similarity search | **LOW** | Large subsystem (4.8–4.9) |
| TIME_WINDOW_LEADERBOARD | `TimeWindowLeaderboardIndexMaintainer` | Time-windowed ranked sets | **LOW** | 12+ classes, entire subsystem |

- [x] **MAX_EVER_VERSION index** — `MaxEverVersionIndexMaintainer` with dual mutation path: `SET_VERSIONSTAMPED_VALUE` (incomplete, with merge function keeping max local version) + `BYTE_MAX` (complete). `UpdateVersionMutation` added to context with merge function support. Metadata validation: GroupingKeyExpression required, exactly 1 VersionKeyExpression in grouped portion, storeRecordVersions required. Aggregate function support via `FunctionNameMaxEver`/`IndexTypeMaxEverVersion`. 18 tests. **MEDIUM**.
- [ ] **TEXT index** — Tokenizer infrastructure, BunchedMap storage, BY_TEXT_TOKEN scan type, 5+ query modes (containsAll/Any/Phrase/Prefix). **LOW** — large scope, specialized.
- [ ] **BITMAP_VALUE index** — Bitmap position storage, BITMAP_VALUE aggregate function. **LOW**.
- [x] **PERMUTED_MIN/MAX indexes** — `permutedMinMaxIndexMaintainer` with dual subspace: primary VALUE index at IndexKey(2) + permuted entries at IndexSecondarySpaceKey(3). Permuted key reorders trailing grouping columns after the value for value-ordered scans. BY_VALUE scans primary, BY_GROUP scans permuted. Delete re-fetches extremum from primary. Aggregate function support via `FunctionNameMin`/`FunctionNameMax`. **Bug fixed by chaos testing**: UPDATE path didn't handle group membership changes (stale permuted entries). Decomposed into insert/remove helpers. 12 unit tests + 4 chaos random tests.
- [ ] **MULTIDIMENSIONAL index** — Hilbert R-tree with configurable node sizes. **LOW**.
- [ ] **VECTOR/HNSW index** — Full HNSW graph (4 distance metrics, RaBitQ quantization, configurable M/ef parameters). Very large. **LOW**.
- [ ] **TIME_WINDOW_LEADERBOARD index** — Sliding time window score tracking. 12+ Java classes. **LOW**.

### 3. New key expression types

| Expression | Proto Message | Purpose | Priority |
|-----------|--------------|---------|----------|
| DimensionsKeyExpression | `Dimensions` | Multidimensional indexing (prefix_size + dimensions_size) | **LOW** |
| LiteralKeyExpression | `Value` | Static literal values (double/float/int64/bool/string/bytes) | **LOW** (already impl'd) |
| FunctionKeyExpression | `Function` | Named function with arguments | **LOW** |
| SplitKeyExpression | `Split` | Split repeated values into groups | **LOW** |
| ListKeyExpression | `List` | Homogeneous expression list (preserving boundaries) | **LOW** |
| AtomKeyExpression | (Java class) | Atom-level expressions | **LOW** |
| CollateFunctionKeyExpression | (Java class) | Locale-aware string sorting | **LOW** |
| OrderFunctionKeyExpression | (Java class) | Custom sort order functions (2024) | **LOW** |
| LongArithmethicFunctionKeyExpression | (Java class) | Arithmetic operations on longs (2024) | **LOW** |
| InvertibleFunctionKeyExpression | (interface) | Bidirectionally invertible functions | **LOW** |

- [x] **FunctionKeyExpression** — Implemented with global registry, proto round-trip, `get_versionstamp_incarnation` built-in. `FDBStoredRecord.Store` field added (matches Java's `FDBRecord.getStore()`). 25 unit tests.
- [x] **SplitKeyExpression** — Batches FanOut results into fixed-size groups. Proto `Split{joined, split_size}`. Overflow-checked. 14 unit tests.
- [x] **ListKeyExpression** — Cross-product with nested tuple wrapping (unlike Concat which flattens). Proto `List{repeated child}`. FDB tuple.Tuple nesting for proper Pack(). 15 unit tests.
- [x] **LongArithmeticFunctionKeyExpression** — 14 arithmetic functions (add, sub, subtract, mul, multiply, div, divide, mod, bitand, bitor, bitxor, bitnot, bitmap_bit_position, bitmap_bucket_offset) via FunctionKeyExpression registry. Overflow-checked (Math.*Exact), null propagation, both-function pattern (sub/subtract). 25 unit tests.
- [ ] **Other expression types** — DimensionsKE, CollateFunctionKE, OrderFunctionKE, AtomKE, InvertibleFunctionKE. **LOW** — only needed for specialized index types.

### 4. New store APIs

- [x] **Store locking APIs** — `SetStoreLockState(state, reason)`, `ClearStoreLockState()`, `OverrideLockSaveRecord()` (skips FORBID_RECORD_UPDATE lock). **HIGH**.
- [x] **Header user fields** — `GetHeaderUserField(key)`, `SetHeaderUserField(key, value)`, `ClearHeaderUserField(key)`. **MEDIUM**.
- [x] **Store state caching** — `FDBRecordStoreStateCache` interface, `MetaDataVersionStampStoreStateCache` implementation (LRU+TTL, \xff/metadataVersion invalidation), `SetStateCacheability()` API, dirty state tracking on context, read conflict on cache hit. 2.2x speedup on store open. 40 tests. **MEDIUM**.
- [x] **Incarnation APIs** — `GetIncarnation()`, `UpdateIncarnation(updater)`. **MEDIUM**.
- [x] **Snapshot version loading** — `LoadRecordVersion(pk, snapshot)` already implemented in `store_version.go`. **LOW**.
- [ ] **PreloadRecordStoreState** — Separate state loading from store creation. **LOW** (optimization).
- [x] **Index build state tracking** — `AddBuildProgress`/`LoadBuildProgress` at `[9][indexSubspaceKey][1]` (atomic ADD). Wired into `buildRange`/`buildRangeByIndex`. 4 tests. **LOW**.
- [x] **DryRunSaveRecord** — Validation (existence, type, lock) without writes. Returns computed record with size info. 4 tests. **LOW**.
- [x] **DryRunDeleteRecord** — Checks record existence without deleting. 3 tests. **LOW**.
- [x] **ScanRecordKeys** — Key-only scan without deserialization (dedup for split records). 5 tests. **LOW**.
- [x] **Index state query APIs** — `IsIndexReadableUniquePending`, `GetWriteOnlyIndexes`, `GetDisabledIndexes`, `GetIndexesToBuildSince`. 9 tests. **LOW**.
- [x] **Uniqueness violation resolution** — `ScanUniquenessViolationsForValue`, `ResolveUniquenessViolationByDeletion`. 6 tests. **LOW**.

### 5. Metadata & schema evolution changes

- [ ] **Index predicates (IndexPredicate)** — Sparse/filtered indexes with boolean conditions. `shouldIndexThisRecord()` evaluation. We have a simple function-based predicate; Java has a full predicate hierarchy (And/Or/Not/Constant/Value). **LOW** (our function-based approach works, full predicate tree is query-planner level).
- [x] **Index replacement lifecycle** — `GetReplacedByIndexNames()`, replacement-exists validation, chained-replacement rejection. 7 tests. **LOW**.
- [ ] **Synthetic record types** — `JoinedRecordType` (equi-join with outer join support), `UnnestedRecordType` (repeated message fan-out). Proto fields 12-13 in MetaData. 11+ Java classes. **LOW** (large feature, experimental API).
- [ ] **Views** — `PView` in MetaData proto (field 15). Name + SQL definition text. **LOW**.
- [ ] **User-defined functions** — `PUserDefinedFunction` in MetaData proto (field 14). Macro or SQL functions. **LOW**.
- [x] **MetaDataEvolutionValidator enhancements** — Proto syntax/edition check, `hasPresence` consistency, `allowUnsplitToSplit` (already done). All Java checks now covered. **LOW**.
- [x] **MetaDataEvolutionValidator: `allowNoSinceVersion` validation** — Implemented: `SetAllowNoSinceVersion()` builder option. New record types must have `SinceVersion` set (errors if missing unless allowed) and `SinceVersion > oldMetaData.Version()`. Matches Java lines 378-397. 6 new tests (29 total). **HIGH**.
- [x] **MetaDataEvolutionValidator: `SinceVersion` immutability check** — Implemented: `SinceVersion` cannot change on existing record types. Matches Java line 361. **MEDIUM**.
- [x] **MetaDataEvolutionValidator: `primaryKeyComponentPositions` validation** — Implemented: positions cannot be added, dropped, or changed between index versions. Skipped when `allowIndexRebuilds` and version changed. Matches Java lines 649-667. Added `HasPrimaryKeyComponentPositions()`/`PrimaryKeyComponentPositions()` getters on Index. **MEDIUM**.
- [x] **MetaDataValidator enhancements** — Former index version boundary checks, addedVersion ≤ lastModifiedVersion, index replacement chain validation. 11 tests. Remaining: predicate validation, subspaceKey uniqueness. **LOW**.

### 6. New cursor types

- [ ] **AggregateCursor** — Accumulator-based aggregation over cursor results. New continuation format (4.4–4.5). **LOW** (needed for query planner, not basic CRUD).
- [ ] **ComparatorCursor** — Custom comparator ordering. **LOW**.
- [ ] **UnorderedUnionCursor** — Union without order preservation. **LOW**.
- [ ] **SizeStatisticsGroupingCursor** — Key/value size tracking during group operations. **LOW**.
- [ ] **BloomFilterCursorContinuation** — Bloom filter optimization for large result sets. **LOW**.

### 7. New index scan types

- `BY_TEXT_TOKEN` — TEXT index token searches. **LOW**.
- `BY_DISTANCE` — VECTOR index similarity search. **LOW**.
- `BY_TIME_WINDOW` — TIME_WINDOW_LEADERBOARD. **LOW**.

### 8. New aggregate functions

- [x] **MAX_EVER_VERSION** — via MAX_EVER_VERSION index type. Aggregate support via `FunctionNameMaxEver`/`IndexTypeMaxEverVersion`. **MEDIUM**.
- [ ] **BITMAP_VALUE, BITMAP_BIT_POSITION, BITMAP_BUCKET_OFFSET** — for BITMAP_VALUE indexes. **LOW**.
- [ ] **TIME_WINDOW_RANK, TIME_WINDOW_COUNT** — for leaderboard indexes. **LOW**.

### 9. SQL / Relational layer

Java has 6 separate modules for SQL: `fdb-relational-api`, `fdb-relational-core`, `fdb-relational-jdbc`, `fdb-relational-grpc`, `fdb-relational-server`, `fdb-relational-cli`. Features include: SQL views (`PView`), user-defined functions (`PUserDefinedFunction`), CAST/type coercion, recursive CTEs (PREORDER/POSTORDER), BETWEEN/CASE expressions, COPY command for data import/export, composite aggregates, JOIN with ORDER BY. All built on top of `fdb-record-layer-core`.

**Not a priority until core is flawless.** The SQL layer sits entirely above the record layer — it uses the same store, indexes, cursors, and metadata we're porting. Once core is complete and conformant, SQL becomes a natural extension. No wire format impact from ignoring it now.

Also in Java but out of scope for now: `fdb-record-layer-lucene` (full-text via Lucene), `fdb-record-layer-spatial` (R-tree spatial), `fdb-record-layer-icu` (Unicode collation).

### 10. API/behavioral changes (informational, no action needed unless noted)

- FormatVersion transitioned from constants to enum (4.3) — internal, no wire impact
- Index maintainer factory API customization (4.4) — we don't expose factory API
- OnlineIndexer heartbeat replaced synchronized runner (4.6–4.10) — our Go impl is independent
- Deprecated synchronized indexing APIs removed (4.10) — doesn't affect Go
- URI parsing tightened (4.10) — relational layer, not record layer core
- `PUserDefinedFunction` oneof field renamed (4.10) — same proto field numbers, wire-compatible
- `__ROW_VERSION` pseudo-field (4.8–4.10) — query planner only, doesn't affect storage
- Plan serialization incompatible between 4.8↔4.10 — we don't serialize plans
- Java 21 compatibility (`this-escape` warnings) — Java-only
- AutoCommit support (4.5) — transaction management feature, informational
- Lucene improvements (4.4–4.10) — separate module, not in core record layer

### 11. Version-by-version wire format breaking changes

| Versions | Change | Impact on Go |
|----------|--------|-------------|
| 4.3→4.5 | AggregateCursorContinuation proto format | No impact (we don't have aggregate cursors) |
| 4.5→4.6 | Lucene serialization changes | No impact (we don't have Lucene) |
| 4.7→4.8 | KeyValueCursorBaseContinuation format | No impact (conformance tests pass with TO_OLD) |
| 4.9→4.10 | `__ROW_VERSION` replaces `VersionValue` in plans | No impact (query planner only) |

### 12. Priority summary

**HIGH (wire compat / correctness):**
1. ~~FULL_STORE lock + FormatVersion 14 stricter validation~~ **DONE**
2. ~~READABLE_UNIQUE_PENDING index state~~ **DONE**
3. ~~Store locking APIs (set/clear/override)~~ **DONE**

**MEDIUM (feature completeness):**
4. ~~Store incarnation field + APIs~~ **DONE**
5. ~~Header user fields~~ **DONE**
6. ~~MAX_EVER_VERSION index type~~ **DONE**
7. ~~FunctionKeyExpression (for incarnation)~~ **DONE**
8. ~~Store state caching~~ **DONE**

**LOW (specialized / future):**
9. Remaining index types (TEXT, BITMAP, MULTIDIMENSIONAL, VECTOR, LEADERBOARD) — ~~PERMUTED_MIN/MAX~~, ~~MAX_EVER_VERSION~~ done
10. Remaining key expression types (Dimensions, Collate, Order, Atom, Invertible) — ~~Split~~, ~~List~~, ~~LongArithmetic~~, ~~Function~~ done
11. Synthetic record types (JoinedRecordType, UnnestedRecordType)
12. Views, UDFs
13. New cursor types (Aggregate, Comparator, UnorderedUnion)
14. Query planner features (not ported)

---

## Error handling alignment (2026-03-12 QA audit)

Architectural decision: Java exception class = Go error struct. Use `errors.As()` for matching. No bare sentinels. See CLAUDE.md "Error handling" section for full pattern.

**Naming convention:** Java `FooBarException` → Go `FooBarError` struct. Drop the `Exception` suffix, replace with `Error`. Examples:
- `RecordAlreadyExistsException` → `RecordAlreadyExistsError`
- `ScanNonReadableIndexException` → `IndexNotReadableError` (simplified where Java name is awkward)
- `RecordStoreNoInfoAndNotEmptyException` → `RecordStoreNoInfoButNotEmptyError`

**Pattern:** Always a `type FooError struct { ... }` with context fields matching Java's `addLogInfo()` keys. Never `var ErrFoo = errors.New("...")`. Callers match with `errors.As(err, &e)`, never `errors.Is(err, ErrFoo)`.

### Phase 1: Convert existing sentinels to error types — **DONE**

- [x] **`ErrRecordStoreAlreadyExists`** → `RecordStoreAlreadyExistsError` struct. All return sites migrated.
- [x] **`ErrRecordStoreDoesNotExist`** → `RecordStoreDoesNotExistError` struct. All return sites migrated.
- [x] **`ErrRecordStoreNoInfoButNotEmpty`** → `RecordStoreNoInfoButNotEmptyError` struct with `FirstKey` field.
- [x] **`ErrRecordStoreStateNotLoaded`** → `RecordStoreStateNotLoadedError` struct. 8 return sites migrated.
- [x] **`ErrIndexNotReadable`** → `IndexNotReadableError` struct with `IndexName` + `CurrentState`.
- [x] **`ErrIndexNotFound`** → `IndexNotFoundError` struct with `IndexName`. 5 return sites migrated.
- [x] **`ErrIndexNotBuilt`** → `IndexNotBuiltError` struct with `IndexName`.
- [x] Removed old `ErrRecordAlreadyExists` / `ErrRecordDoesNotExist` / `ErrRecordTypeChanged` sentinel variables and `Is()` methods.
- [x] Updated all call sites: `errors.Is(err, ErrFoo)` → `errors.As(err, &fooErr)`.
- [x] Updated all tests (unit + conformance) to use `errors.As()` pattern.

### Phase 2: Add missing error types for implemented features — **DONE**

- [x] **`MetaDataError`** — defined in `errors.go`. Message-only, matchable via `errors.As()`.
- [x] **`UnsupportedFormatVersionError`** — carries `Version` + `MaxVersion`. Store builder `validateFormatVersion` migrated.
- [x] **`RecordSerializationError`** — wraps proto marshal failures with `Unwrap()`. 2 return sites migrated.
- [x] **`RecordDeserializationError`** — wraps proto unmarshal failures with `Unwrap()`. 6 return sites migrated (store + cursor).
- [ ] **`StaleUserVersionError`** — Java's `RecordStoreStaleUserVersionException` (not thrown in 4.10.6.0 but type exists). Deferred — no throw sites exist.

### Phase 3: Conformance tests for error paths — **DONE**

- [x] **Improve Java conformance server** — catch block now returns structured error JSON with `exceptionClass` and `exceptionFullClass` fields. Go `JavaError` type for type-level assertions. HTTP 200 for step errors (not 500).
- [x] **Record existence errors cross-language** — RecordAlreadyExistsException, RecordDoesNotExistException verified both Go and Java throw equivalent errors.
- [x] **Store lifecycle errors cross-language** — RecordStoreAlreadyExistsException, RecordStoreDoesNotExistException verified both Go and Java.
- [x] **Index scan errors cross-language** — ScanNonReadableIndexException verified on write-only index scan.
- [x] **Store lock errors cross-language** — FORBID_RECORD_UPDATE prevents save in both Go and Java.
- [x] **Cross-language error propagation** — Go creates record, Java insert duplicate gets RecordAlreadyExistsException.
- [x] **Unique index violation cross-language** — 6 conformance specs: READABLE violation detection (Go→Java, Java→Go), index entry scanning, WRITE_ONLY violation wire format with existingKey.
- [ ] **Schema validation cross-language** — deferred (MetaDataValidator gaps need to be addressed first).

---

## Completed (for reference)

- [x] SaveRecord, LoadRecord, DeleteRecord — core CRUD working
- [x] Java compatibility — bidirectional read/write via conformance tests
- [x] TypedFDBRecordStore with Go generics
- [x] Builder pattern (Create, Open, CreateOrOpen, Build)
- [x] RecordExists method
- [x] RecordExistenceCheck enum (NONE, ERROR_IF_EXISTS, ERROR_IF_NOT_EXISTS, ERROR_IF_NO_EXISTING_RECORD)
- [x] Conflict management — AddRecordReadConflict, AddRecordWriteConflict
- [x] Isolation levels — Snapshot vs Serializable reads
- [x] Cursor API — RecordCursor interface with OnNext/Close/Seq/Seq2/SeqWithContinuation
- [x] Key-value cursor — Range iteration, continuation tokens, byte/row limits
- [x] Cursor combinators — Filter, Map, MapErr, Filter2, Limit
- [x] Range scans — ScanRecords, ScanRecordsInRange, forward/reverse, endpoint types
- [x] Key expressions — FieldKeyExpression, RecordTypeKeyExpression, EmptyKeyExpression, CompositeKeyExpression
- [x] Large dataset scanning — 10K sequential + 1K continuation + 1M stress
- [x] Record versioning — FDBRecordVersion (12-byte), inline storage at pk + -1 suffix
- [x] Record counting — atomic ADD mutations, per-type via RecordTypeKeyExpression
- [x] Store state validation — StoreLockState.FORBID_RECORD_UPDATE check (note: FULL_STORE lock state added in 4.10.6.0, see upgrade assessment)
- [x] Split records — saveWithSplit/loadWithSplit/deleteSplit, 100KB chunks, cursor reassembly
- [x] Secondary indexes (VALUE) — StandardIndexMaintainer, unique enforcement, common-entry skip
- [x] Covering indexes (KeyWithValueExpression) — value columns stored in FDB value, 14 unit tests + 5 conformance specs
- [x] Index maintenance — auto-update on Save/Delete/DeleteAllRecords
- [x] Continuation token protobuf wrapping — magic number 6773487359078157740
- [x] Bulk operations — DeleteAllRecords, GetRecordCount/GetSnapshotRecordCount
- [x] Bazel 8 migration — MODULE.bazel, gazelle, nogo (20 analyzers)
- [x] **Bazel 9 upgrade** — upgraded from 8.2.1 to 9.0.1. Bumped rules_java 8→9.6.1, added rules_android 0.7.1, removed archived rules_proto, added explicit protobuf-java-util Maven dep. All 1150 specs pass.
- [x] CI pipeline — GitHub Actions with Bazel build + test
- [x] Subspace constants verified — all 10 match Java exactly (0-9)

---

## Conformance test coverage gaps

The conformance framework (HTTP bridge to Java Record Layer) validates all core features bidirectionally. Every wire-format-sensitive feature has Go↔Java cross-validation.

### CRITICAL — wire format at risk without cross-validation

- [x] **Split record conformance** — 9 specs: Go writes 250KB/150KB/100KB/small/minimal → Java reads; Java writes 250KB/150KB/small → Go reads; overwrite large→small and small→large. Cross-validated.

- [x] **Index entry format conformance** — 5 specs: Go writes → Java scans, Java writes → Go scans, delete removes entry, update changes entry, sorted multi-record scan. Index entries compared field-by-field. Cross-validated.

- [x] **Record version conformance** — 4 specs: Go saves versioned → Java reads, Java saves → Go reads, local version ordering, version update. Cross-validated.

- [x] **Scan/continuation conformance** — 6 specs: Go writes/Java scans, Java writes/Go scans, limit, ordering, empty store, flower details. Cross-validated.

- [x] **Record counting conformance** — 6 specs: Go saves/Java counts, Java saves/Go counts, delete decrements, update doesn't increment, mixed saves, zero baseline. Cross-validated.

### HIGH — remaining gaps

- [x] **Multi-type conformance** — 11 specs + 1 direct store spec: Customer CRUD, cross-write, boundary values, delete non-existent, multiple customers. Cross-validated.

- [x] **Continuation token cross-platform** — 3 specs: Go→Java resume, Java→Go resume, alternating Go/Java. Cross-validated. Go uses TO_OLD (raw bytes) format matching Java Record Layer 4.2.6.0.

- [x] **Reverse scan conformance** — 6 specs: Go writes/Java reverse scans, Java writes/Go reverse scans, limit, forward-reverse mirror, cross-platform continuation resume, empty store. Cross-validated.

- [x] **Fan-out index conformance** — 7 specs: Go writes/Java scans fan-out entries, Java writes/Go scans, multiple records, empty repeated field, delete removes all entries, update changes entries, cross-write. Cross-validated.

### Current conformance coverage

| Feature | Java Steps | Go Tests | Cross-validated |
|---|---|---|---|
| Basic CRUD | saveOrder, loadOrder, deleteOrder, recordExists | 5 test files | YES |
| Existence checks | (via saveOrder) | existence_check_conformance_test.go | YES |
| Isolation levels | (via raw FDB) | isolation_conformance_test.go | YES |
| Conflict detection | (via raw FDB) | conflict_conformance_test.go | YES |
| Record versioning | saveOrderVersioned, loadOrderWithVersion | version_conformance_test.go | YES |
| Record counting | saveOrderCounting, deleteOrderCounting, getRecordCount | count_conformance_test.go | YES |
| Scan/ordering | scanOrders | scan_conformance_test.go | YES |
| Multi-type (Customer) | saveCustomer, loadCustomer, deleteCustomer, customerExists | customer_conformance_test.go | YES |
| Split records | saveSplitOrder, loadSplitOrder | split_conformance_test.go | YES |
| Secondary indexes | saveOrderWithIndex, scanIndex, deleteOrderWithIndex | index_conformance_test.go | YES |
| Continuation tokens | scanOrdersWithContinuation | continuation_conformance_test.go | YES |
| Reverse scan | scanOrdersReverse, scanOrdersReverseWithContinuation | reverse_scan_conformance_test.go | YES |
| Fan-out indexes | saveOrderWithFanOutIndex, scanFanOutIndex, deleteOrderWithFanOutIndex | fanout_index_conformance_test.go | YES |
| Composite index (PK dedup) | saveOrderWithCompositeIndex, scanCompositeIndex | composite_index_conformance_test.go | YES |
| COUNT index | saveOrderWithCountIndex, deleteOrderWithCountIndex, scanCountIndex | count_index_conformance_test.go | YES |
| SUM index | saveOrderWithSumIndex, deleteOrderWithSumIndex, scanSumIndex | sum_index_conformance_test.go | YES |
| RangeSet wire format | rangeSetInsert, rangeSetContains, rangeSetMissingRanges | rangeset_conformance_test.go | YES |
| DeleteAllRecords | deleteAllRecordsWithIndex, countRecordsWithIndex | delete_all_conformance_test.go | YES |
| Store header format | getStoreHeaderRaw, createStoreWithUserVersion | store_header_conformance_test.go | YES |
| Index state persistence | markIndexWriteOnly/Disabled/Readable, getIndexStateRaw | index_state_conformance_test.go | YES |
| Store lifecycle | (reuses existing steps) | store_lifecycle_conformance_test.go | YES |
| MAX_EVER_LONG index | saveOrderWithMaxEverLongIndex, deleteOrderWithMaxEverLongIndex, scanMaxEverLongIndex | min_max_ever_index_conformance_test.go | YES |
| MIN_EVER_LONG index | saveOrderWithMinEverLongIndex, deleteOrderWithMinEverLongIndex, scanMinEverLongIndex | min_max_ever_index_conformance_test.go | YES |
| COUNT_NOT_NULL index | saveOrderWithCountNotNullIndex, deleteOrderWithCountNotNullIndex, scanCountNotNullIndex | count_not_null_index_conformance_test.go | YES |
| COUNT_UPDATES index | saveOrderWithCountUpdatesIndex, deleteOrderWithCountUpdatesIndex, scanCountUpdatesIndex | count_updates_index_conformance_test.go | YES |
| MAX_EVER_TUPLE index | saveOrderWithMaxEverTupleIndex, deleteOrderWithMaxEverTupleIndex, scanMaxEverTupleIndex | min_max_ever_tuple_index_conformance_test.go | YES |
| MIN_EVER_TUPLE index | saveOrderWithMinEverTupleIndex, deleteOrderWithMinEverTupleIndex, scanMinEverTupleIndex | min_max_ever_tuple_index_conformance_test.go | YES |
| CLEAR_WHEN_ZERO | saveOrderWithCountCWZ, deleteOrderWithCountCWZ, scanCountCWZIndex | clear_when_zero_conformance_test.go | YES |
| Covering index (KeyWithValue) | saveOrderWithCoveringIndex, scanCoveringIndex, deleteOrderWithCoveringIndex | covering_index_conformance_test.go | YES |
| DeleteRecordsWhere | saveOrderTypePrefixed, saveCustomerTypePrefixed, deleteRecordsWhereType, countRecordsTypePrefixed, loadOrderTypePrefixed, loadCustomerTypePrefixed, scanIndexTypePrefixed | delete_records_where_conformance_test.go | YES |
| VERSION index | saveOrderWithVersionIndex, deleteOrderWithVersionIndex, scanVersionIndex | version_index_conformance_test.go | YES |
| OnlineIndexer | saveOrderForOnlineBuild, scanIndexAfterOnlineBuild, isIndexReadableAfterBuild | online_indexer_conformance_test.go | YES |
| PERMUTED_MAX index | saveOrderWithPermutedMaxIndex, deleteOrderWithPermutedMaxIndex, scanPermutedMaxByValue, scanPermutedMaxByGroup | permuted_min_max_index_conformance_test.go | YES |
| PERMUTED_MIN index | saveOrderWithPermutedMinIndex, deleteOrderWithPermutedMinIndex, scanPermutedMinByValue, scanPermutedMinByGroup | permuted_min_max_index_conformance_test.go | YES |
| Index scan continuations | scanIndexWithContinuation, saveOrderForIndexContinuation | index_continuation_conformance_test.go | YES |
| Error paths | insertDuplicateOrder, updateNonExistentOrder, openNonExistentStore, createExistingStore, scanNonReadableIndex, saveLocked | error_conformance_test.go | YES |
| Index build state (stamp) | loadIndexingTypeStamp, saveIndexingTypeStampByRecords | index_build_state_conformance_test.go | YES |
| EvaluateAggregateFunction | evaluateCountAggregate, evaluateSumAggregate, evaluateMinAggregate, evaluateMaxAggregate, evaluateMinEverAggregate, evaluateMaxEverAggregate | aggregate_conformance_test.go | YES |
| Unique violations | saveWithUniqueIndex, deleteWithUniqueIndex, scanUniqueIndex, saveDuplicateWithUniqueIndex, markUniqueIndexWriteOnly, saveWithUniqueIndexDuringWriteOnly, scanUniquenessViolations, getUniqueIndexState | unique_violation_conformance_test.go | YES |

### NEW — conformance gaps identified 2026-03-09

- [x] **SUM index conformance** — CRITICAL. 7 specs: Go writes→Java scans, Java writes→Go scans, mixed writes combined sum, Go deletes Java-written record, Java deletes Go-written record, update via Go, update via Java. Cross-validated.
- [x] **RangeSet wire format conformance** — CRITICAL. 4 specs: Go writes full range→Java reads, Java writes full range→Go reads, Go writes partial→Java reads gaps, Java writes partial→Go reads gaps. Wire format `pack(rangeBegin) → rangeEnd` cross-validated.
- [x] **DeleteAllRecords cross-validation** — CRITICAL. 4 specs: Go saves→Go deletes→Java confirms empty, Java saves→Java deletes→Go confirms empty, cross-write→Go deletes→Java confirms, delete→re-save cross-platform. Records + index entries verified cleared.
- [x] **Store header format conformance** — HIGH. 4 specs: Go creates→Java reads raw header, Java creates→Go reads raw header, Go sets userVersion→Java reads, Java sets userVersion→Go reads. Proto wire format cross-validated.
- [x] **Index state persistence across reopen** — HIGH. 4 specs: Go marks WRITE_ONLY→Java reads raw, Java marks WRITE_ONLY→Go reads, Go marks DISABLED→Java reads, WRITE_ONLY→READABLE roundtrip clears entry. Wire format cross-validated.
- [x] **FormerIndex tracking conformance** — N/A. FormerIndex is metadata-only (not persisted in FDB data). Validation happens at Build() time, not wire-format level.
- [x] **Store delete+recreate lifecycle** — HIGH. 3 specs: header preserved across DeleteAllRecords, index state WRITE_ONLY survives DeleteAllRecords, Java deletes→Go re-creates and saves. Cross-validated.
- [x] **MAX_EVER_LONG index conformance** — HIGH. 6 specs: Go writes→both scan, Java writes→both scan, mixed writes, delete irreversibility (Go deletes Java record, Java deletes Go record), update never decreases. Cross-validated.
- [x] **MIN_EVER_LONG index conformance** — HIGH. 6 specs: Go writes→both scan, Java writes→both scan, mixed writes, delete irreversibility (Go deletes Java record, Java deletes Go record), update never increases. Cross-validated.
- [x] **Covering index (KeyWithValueExpression) conformance** — HIGH. 5 specs: Go writes→both scan, Java writes→both scan, cross-language delete, update changes value consistently, mixed writes. Value portion (flower.type) cross-validated. 14 unit tests cover edge cases (splitPoint=0, splitPoint=len(inner), FanOut+covering, continuation).
- [x] **OnlineIndexer conformance** — HIGH. 7 specs: Go saves→Go builds→Java scans, Java saves→Go builds→both scan, chunked build (limit=3), Go online-build vs Java rebuild identical, index state READABLE cross-validated (Java+Go), mixed writes then Go build. Note: Java's OnlineIndexer doesn't support FDB tenants in Maven 4.2.6.0, so Java-builds-index tests skipped.
- [x] **Store header v2 conformance (4.10.6.0 features)** — HIGH. 14 specs: header user fields (Go sets→Java reads, Java sets→Go reads, multiple fields, overwrite), incarnation (Go sets→Java reads, Java sets→Go reads, sequential increments), store lock state (FULL_STORE blocks Java open, bypass with matching reason, wrong reason fails, FORBID_RECORD_UPDATE blocks save, Java locks→Go fails, clear restores access, wire format matches). Cross-validated.
- [x] **MAX_EVER_VERSION index conformance** — HIGH. 7 specs: Go writes/both scan, Java writes/both scan, mixed writes, _EVER delete semantics, later write updates max, cross-language delete persistence, wire format versionstamp bytes match. SET_VERSIONSTAMPED_VALUE dual mutation path cross-validated.
- [ ] ~~**FunctionKeyExpression conformance**~~ — N/A. `get_versionstamp_incarnation` is Go-specific (not a Java built-in). Function registry is local to each implementation.

### Wire compat review gaps (identified 2026-03-11)

**P0 — wire format at risk:**
- [x] **PERMUTED_MIN/MAX conformance** — CRITICAL. 10 specs: Go writes/both scan BY_VALUE+BY_GROUP, Java writes/both scan, mixed writes, Go deletes max written by Java (re-fetch), Java deletes max written by Go (re-fetch), non-extremum delete unchanged, PERMUTED_MIN Go writes/both scan, Java writes/both scan, delete min re-fetch, non-extremum insert unchanged. Dual subspace wire format cross-validated.

**P1 — strengthens confidence:**
- [x] **Index scan continuation cross-language resume** — HIGH. 3 specs: Go→Java resume, Java→Go resume, alternating Go/Java. VALUE index paged scan with 10 entries, limit=3/2 page sizes. Continuation token wire format cross-validated (Go TO_OLD ↔ Java proto-wrapped).
- [x] **RecordMetaData proto serialization cross-language roundtrip** — 21 specs (7 configs × 3 directions). Configs: basic, with_indexes, with_former_indexes, full, with_universal_index, with_record_count, with_explicit_type_key. Go→Java, Java→Go, Go→Java→Go roundtrip. `clearProto2Defaults` normalizes proto2 field presence across Go/Java (including map message values). Java side uses `ExtensionRegistry` for `(record).usage=UNION` option resolution.

**P2 — edge cases:**
- [x] **clearProto2Defaults missing map<K, Message> recursion** — Fixed: added `fd.IsMap() && fd.MapValue().Kind() == protoreflect.MessageKind` case to recurse into map message values.
- [x] **Metadata conformance: explicit record type key config** — Added `with_explicit_type_key` config (int64(42) / 42L). 7 configs × 3 directions = 21 specs now (was 18).
- [x] **Proto field type diversity in test schema** — DONE. `field_type_index_test.go` (16 specs): VALUE indexes on every TypedRecord field type (int32, sint32, sint64, sfixed32, sfixed64, float, double, bool, string, bytes, enum). Tests null handling, composite multi-type indexes, save/delete/scan roundtrip, float special values (±Inf, ±0.0), int32 boundary values (MaxInt32, MinInt32). Cross-language conformance already covered by `typed_record_conformance_test.go` (11 specs). Remaining untested: map (Java rejects), oneof (transparent to storage), repeated message (covered by NestFanOut tests).
- [x] **Store lock + delete operation interaction** — DONE (already implemented). Go has `validateRecordUpdateAllowed()` in all 4 mutation paths (SaveRecord, DeleteRecord, DeleteAllRecords, DeleteRecordsWhere) matching Java exactly. Unit tests cover: DeleteBlockedByLock, DeleteAllBlockedByLock, DeleteRecordsWhere blocked, error precedence (non-existent delete returns false, not lock error). Lock wire format cross-validated by store header conformance tests (14 specs).
- [x] **Index build state wire format (subspace 9)** — MEDIUM. `SaveIndexingTypeStamp`/`LoadIndexingTypeStamp` on store. OnlineIndexer saves BY_RECORDS stamp at `[9][indexSubspaceKey][2]` matching Java's `IndexingBase.setIndexingTypeOrThrow()`. 5 conformance specs: Go→Java, Java→Go, no stamp, persists after READABLE, cleared on rebuild.

---

## Bugs (found in conformance audit)

### CRITICAL

- [x] **Version values stored as raw bytes instead of tuple-packed Versionstamp** — Fixed: Go stored version values as raw 12-byte FDBRecordVersion bytes. Java's `SplitHelper.unpackVersion()` calls `Tuple.fromBytes()` expecting a tuple-encoded Versionstamp. Caused "Unknown tuple data type 3 at index 5" error. Fix: wrap in `tuple.Tuple{Versionstamp}.Pack()` for complete, `PackWithVersionstamp()` for incomplete.

- [x] **Java conformance server tenant.run() skips version mutation flush** — Fixed: `runInContext` for tenants used `tenant.run()` which auto-commits bypassing `FDBRecordContext.commitAsync()`. Pre-commit hooks (version mutation flush) never fired, so versioned saves silently dropped version data. Fix: use `createTransaction()` + `context.commitAsync().join()`.

- [x] **CompositeKeyExpression does concat, not cross-product** — Fixed: `Evaluate()` now returns `[][]interface{}` (list of key tuples) and `CompositeKeyExpression` computes Cartesian product matching Java's `ThenKeyExpression`.

- [x] **evaluateIndex() always returns 1 entry per record** — Fixed: `evaluateIndex()` now creates one `indexEntry` per returned tuple, enabling fan-out for multi-valued expressions.

### HIGH

- [x] **DeleteRecord doesn't cleanup incomplete version mutations** — Fixed: `DeleteRecord` now calls `deleteRecordVersion()` to remove queued version mutations from `FDBRecordContext`, preventing stale version data for deleted records. Matches Java's `deleteTypedRecord` which calls `context.removeVersionMutation()`.

- [x] **DeleteAllRecords doesn't clear all data subspaces** — Fixed: Go only cleared subspaces 1,2,4,8. Java clears all subspaces except 0 (header) and 5 (index state). Missing: 3 (secondary index), 6 (index range), 7 (uniqueness violations), 9 (index build). Fixed to match Java's approach.

- [x] **RecordTypeKeyExpression uses string name instead of integer type key** — Fixed two bugs: (1) `RecordTypeIndex` was a sequential counter (0,1,2...) instead of the proto field number from UnionDescriptor. Java uses `field.getNumber()`. (2) `RecordTypeKeyExpression.Evaluate()` returned the proto message name string (`"Order"`) instead of the integer record type key. Java returns `record.getRecordType().getRecordTypeKey()` which is the proto field number (as `Long`). Fixed by storing a type-key lookup map in the expression, populated at metadata build time.

- [x] **FieldKeyExpression panics on nil message** — Fixed: `Evaluate(nil)` crashed at `msg.ProtoReflect()`. Happens when NestingKeyExpression evaluates a child on an unset message field. Now returns `nil` (null key component) matching Java's behavior of returning `Key.Evaluated.NULL`.

- [x] **GetValue() returns zero on !HasNext()** — Fixed: `GetValue()` now panics when `hasNext` is false, matching Java's `IllegalResultValueAccessException`.

- [x] **Build() doesn't validate primary keys** — Fixed: `Build()` now returns `(*RecordMetaData, error)` and validates all record types have primary keys set. All callers updated.

- [x] **ScannedRecordsLimit checks after read, skipping records on resume** — Fixed: The scan limit check happened after `readNextRecord()`, making the continuation point past the undelivered record. On resume, that record was skipped. Moved check before read, matching Java's `CursorLimitManager.tryRecordScan()` which checks limits pre-read.

### MEDIUM

- [x] **FDBRecordVersion missing Equal/Less** — Fixed: Added `Equal()`, `Less()`, `String()` methods matching Java's `equals()`/`compareTo()`/`toString()` semantics.

- [x] **WRITE_ONLY uniqueness violation tracking in maintainer** — QA audit finding: Java's `StandardIndexMaintainer.checkUniqueness()` writes violation entries to subspace 7 when index is WRITE_ONLY (instead of throwing). Fixed: added `indexStoreContext` interface, `checkUniqueness()` now writes violations when WRITE_ONLY, `Update()` cleans up violations on delete. `RebuildIndex` uses `MarkIndexReadableOrUniquePending`.

- [x] **Record count DISABLED state check** — Fixed: `addRecordCount()` now checks `RecordCountState != DISABLED` before mutating. `GetSnapshotRecordCount()` checks `== READABLE` before querying. `UpdateRecordCountState()` enforces valid transitions (READABLE↔WRITE_ONLY, any→DISABLED, DISABLED is terminal). When transitioning to DISABLED, clears all count data. 5 new tests.

---

## Indexing — conformance gaps

### CRITICAL

- [x] **Index scanning** — `IndexMaintainer.Scan()` and `FDBRecordStore.ScanIndex()` return `RecordCursor[*IndexEntry]` with `TupleRange` support (ALL, AllOf, Between, BetweenInclusive), continuations, row/byte limits, forward/reverse. `IndexEntry.PrimaryKey()` and `IndexValues()` for key extraction.

- [x] **Index state management** — 4 states: `READABLE`, `WRITE_ONLY`, `DISABLED`, `READABLE_UNIQUE_PENDING`. Stored in `IndexStateSpaceKey` (5) subspace as tuple-packed int64. Loaded on store Open/CreateOrOpen. `MarkIndexReadable`, `MarkIndexWriteOnly`, `MarkIndexDisabled`, `ClearAndMarkIndexWriteOnly`. DISABLED indexes skip maintenance. Non-scannable indexes reject ScanIndex. Matches Java's wire format and semantics.

- [x] **Index build support (core)** — RangeSet, IndexingRangeSet, WRITE_ONLY maintenance, OnlineIndexer BY_RECORDS. Remaining: progress tracking, indexing stamps, rebuildIndex, BY_INDEX strategy.

#### Index build sub-tasks (dependency order)

1. **RangeSet** (CRITICAL — foundation for all index building) ✅
   - [x] `RangeSet` type backed by FDB subspace. Wire-compatible with Java's `com.apple.foundationdb.async.RangeSet`.
   - Storage: each key-value = `[subspace.pack(rangeBegin)] → rangeEnd` (raw bytes, NOT packed). Range semantics: `[begin, end)` inclusive-exclusive. Valid key space: `[\x00, \xff)`.
   - [x] `InsertRange(tx, begin, end, requireEmpty bool) bool` — fill gaps in range set. `requireEmpty=true` = atomic test-and-set (returns false if range wasn't empty). `requireEmpty=false` = fill gaps, write-conflict only on gaps actually filled.
   - [x] `Contains(tx, key) bool` — snapshot read + read-conflict on key only.
   - [x] `MissingRanges(tx, begin, end, limit) []Range` — return gaps not yet in set.
   - [x] `IsEmpty(tx) bool` — check if entire `[\x00, \xff)` is missing.
   - [x] `Clear(tx)` — remove all entries.
   - [x] Unit tests: insert, contains, missing ranges, overlapping inserts, abutting ranges, consolidation, empty checks, wire format, incremental build pattern, multi-byte keys.

2. **IndexingRangeSet wrapper** (CRITICAL) ✅
   - [x] `IndexingRangeSet` at store subspace `[6, indexSubspaceKey]` (INDEX_RANGE_SPACE).
   - [x] `FirstMissingRange()`, `ContainsKey(primaryKey)`, `InsertRange(begin, end, requireEmpty)`, `ListMissingRanges()`, `IsComplete()`, `Clear()`.
   - [x] Already cleared on index delete / `ClearAndMarkIndexWriteOnly` (via `clearIndexData`).

3. **WRITE_ONLY index maintenance** (CRITICAL) ✅
   - [x] `IndexMaintainer.UpdateWhileWriteOnly(oldRecord, newRecord)` interface method.
   - [x] `StandardIndexMaintainer.UpdateWhileWriteOnly()` — idempotent VALUE indexes pass through to `Update()`. Matches Java's `isIdempotent() = true`.
   - [x] `updateSecondaryIndexes()` dispatches via `updateOneIndex()`: calls `UpdateWhileWriteOnly` when `IsIndexWriteOnly(idx)`, else `Update`. Matches Java.

4. **OnlineIndexer — BY_RECORDS strategy** (CRITICAL) ✅
   - [x] `OnlineIndexer` type with builder: `SetDatabase`, `SetMetaData`, `SetIndex`, `SetSubspace`, `SetLimit`, `SetRecordTypes`.
   - [x] `BuildIndex(ctx)` — marks WRITE_ONLY → iterates all missing ranges → marks READABLE. Returns total records indexed.
   - [x] `buildRange(ctx)` — finds first missing range via `IndexingRangeSet`, scans records in range, evaluates index + writes entries via `maintainer.Update(nil, rec)`, marks built range with `requireEmpty=true`.
   - [x] Transaction boundaries: each `buildRange` = one transaction. Continuation = last processed PK (matches Java: boundary records re-scanned, safe for idempotent indexes).
   - [x] Record type filtering: `shouldIndexRecord()` checks if record type has this index defined.
   - [x] 8 integration tests: basic build, composite index with PK dedup, empty store, post-build maintenance, small limit chunking, unique index, record type filtering, builder validation.
   - [ ] Progress tracking at `[9, indexSubspaceKey, 1]` (INDEX_BUILD_SPACE) — atomic ADD of records scanned. Not yet implemented (optimization, not wire-format critical).
   - [x] Indexing stamp at `[9, indexSubspaceKey, 2]` — proto `IndexBuildIndexingStamp` for resume detection. `SaveIndexingTypeStamp`/`LoadIndexingTypeStamp` + BY_RECORDS/BY_INDEX methods.
   - [x] **Stamp-aware resume** — `markWriteOnly()` checks if index is already WRITE_ONLY with matching stamp before clearing. Matching stamp → resume build without clearing existing entries (preserves WRITE_ONLY maintenance entries). No stamp + empty range set → write stamp and continue. Stamp mismatch → clear and restart. Matches Java's `IndexingBase.handleIndexingState()` + `setIndexingTypeOrThrow()`. 5 new tests.

5. **rebuildIndex on store** (HIGH — needed for store.Open with new indexes) ✅
   - [x] `FDBRecordStore.RebuildIndex(index)` — clears index data, marks WRITE_ONLY, pre-marks full range in RangeSet, scans all records inline, re-indexes, marks READABLE. Single-transaction path matching Java's `IndexingBase.rebuildIndexAsync()`.
   - [x] 8 tests: basic VALUE index, empty store, stale cleanup, type filtering, range set completion, unique index, uniqueness violation, post-rebuild maintenance.
   - [x] `CreateOrOpen` auto-rebuild: `checkPossiblyRebuild()` compares stored metadata version with current. Uses `GetIndexesToBuildSince(oldVersion)` to find new indexes. Rebuilds inline and updates store header. Matches Java's `FDBRecordStore.checkPossiblyRebuild()`.
   - [x] `addIndexCommon()` on builder: sets `LastModifiedVersion` and `AddedVersion` matching Java's `RecordMetaDataBuilder.addIndexCommon()`. Bumps builder version on each index add.
   - [x] 7 additional tests: version tracking on AddIndex, pre-set version preserved, GetIndexesToBuildSince, auto-rebuild single index, no rebuild on same version, store header version updated, multi-index auto-rebuild.

6. **OnlineIndexer — BY_INDEX strategy** (MEDIUM — optimization, not essential) ✅
   - [x] Build new index from existing readable index instead of scanning all records. `SetSourceIndex(index)` on builder.
   - [x] Uses source index's `ScanIndexRecords` → update target index.
   - [x] Range tracking uses source index entry keys instead of primary keys.
   - [x] Validation: source must be READABLE VALUE index, no duplicates, single record type.
   - [x] BY_INDEX stamp with `SourceIndexSubspaceKey` + `SourceIndexLastModifiedVersion`. 7 tests.

7. **Multi-target index building** (LOW — optimization for bulk schema changes) ✅
   - [x] Build multiple WRITE_ONLY indexes in a single record scan pass. `AddTargetIndex()`/`SetTargetIndexes()` builder methods. MULTI_TARGET_BY_RECORDS stamp with sorted target names. Per-index record type filtering, per-index transaction for markReadable. Targets sorted by name for deterministic primary selection, deduplicated, validated against metadata. 10 tests.
   - [x] All target indexes share the same missing-range tracking (first index's RangeSet).

8. **Mutual/concurrent index building** (LOW — multi-process coordination)
   - [ ] Multiple OnlineIndexer processes build different ranges concurrently.
   - [ ] Heartbeat tracking at `[9, indexSubspaceKey, 7, uuid]`.
   - [ ] `requireEmpty=true` prevents double-processing of ranges.
   - [x] **Blocked stamps** — `isTypeStampBlocked()` with permanent and time-expiring blocks via `block`/`blockExpireEpochMilliSeconds`/`blockID` proto fields. `BlockIndex()`/`UnblockIndex()` on OnlineIndexer. `PartlyBuiltError` on blocked stamp. 4 tests.
   - [x] **`areSimilar()` stamp comparison** — `areSimilarStamps()` compares stamps ignoring block fields via `blocklessStampOf()`. Allows resume when only block state differs. 1 test.
   - [x] **`forceStampOverwrite` policy** — `IndexingPolicy.ForceStampOverwrite` forces stamp write on fresh builds, allows overwrite on continued builds when no records scanned. `setIndexingTypeOrThrow()` implements full Java decision tree. 2 tests.
   - [x] **Method conversion on resume** — `ShouldAllowTypeConversionContinue()` on `IndexingPolicy` with `TakeoverType` enum (MultiTargetToSingle, MutualToSingle, ByRecordsToMutual). Matches Java's `IndexingPolicy.shouldAllowTypeConversionContinue()`.
   - [x] **`QueryIndexingStamps`** — Returns stamp map for all target indexes. Nil stamps returned as NONE method. 1 test.
   - [x] **`IndexBuildState`** — Status reporting: index state + records scanned (from build progress counter) + total records (from COUNT index). `LoadIndexBuildState()` on store. 2 tests.

9. **Conformance tests** (CRITICAL — must validate wire compat)
   - [x] Go saves records + Go rebuilds index → Java scans → entries match.
   - [x] Go saves records + Java rebuilds index → Go scans → entries match.
   - [x] Java saves records + Go rebuilds index → Java scans → entries match.
   - [x] Cross-rebuild: Go rebuild and Java rebuild produce identical entries.
   - [x] Go writes WRITE_ONLY records while Go builds → entries consistent. Stamp-aware resume preserves WRITE_ONLY maintenance entries during build. 5 unit tests validate resume/restart/wire-compatibility. Cross-language (Java OnlineIndexer) deferred — requires Java tenant-aware OnlineIndexer step.
   - [x] RangeSet wire format: Go writes ranges → Java reads them (and vice versa). 4 specs in rangeset_conformance_test.go.

### HIGH

- [x] **Index management store methods** — `GetIndexState`, `IsIndexReadable`, `IsIndexWriteOnly`, `IsIndexDisabled`, `IsIndexScannable`, `MarkIndexReadable`, `MarkIndexWriteOnly`, `MarkIndexDisabled`, `ClearAndMarkIndexWriteOnly`, `RebuildIndex`, `MarkIndexReadableOrUniquePending`. Still missing: `getIndexBuildStateAsync`.

- [x] **Repeated field fan-out** — `FanOut("field")` creates `FieldKeyExpression` with `FanTypeFanOut`, producing one index entry per repeated value. Cross-product with `Concat()` works. Empty repeated field → no entries (matching Java).

- [x] **Sparse/filtered indexes** — `Index.Predicate` field: function that returns true if a record should be indexed. `StandardIndexMaintainer` skips entries when predicate returns false. Matches Java's `IndexPredicate` concept.

- [x] **NULL-safe unique index checks** — Skip uniqueness check when index key contains null values. Matches Java's `indexEntry.keyContainsNonUniqueNull()` guard in `StandardIndexMaintainer.updateOneKeyAsync()`. Default `NullStandin.NULL` behavior: null key components bypass uniqueness enforcement.

- [x] **ScanIndexRecords (fetch records from index)** — `ScanIndexRecords()` on store: scans an index, extracts primary keys from entries, fetches the actual records. Returns `RecordCursor[*FDBIndexedRecord]` (wraps both IndexEntry and stored record). Orphan entries (deleted records) are skipped. Matches Java's `scanIndexRecords()` → `fetchIndexRecords()` pipeline.

### MEDIUM

- [x] **COUNT index type** — `CountIndexMaintainer` using FDB atomic ADD. Key = grouping columns only (no PK appended). Value = little-endian int64 count. `GroupingKeyExpression` with `GroupAll()` / `Ungrouped()` / `GroupBy()` factories. `getIndexMaintainer()` dispatches COUNT vs VALUE. `ScanIndex()` delegates to maintainer `Scan()`. 6 integration tests (grouped, delete decrement, update regroup, ungrouped total, range query, reverse scan).
- [x] **SUM index type** — `SumIndexMaintainer` using FDB atomic ADD. Key = grouping columns only (no PK appended). Value = little-endian int64 running sum. Extracts sum value from first grouped (trailing) column, matching Java's `AtomicMutationIndexMaintainer.updateIndexKeys()` which passes `groupedValue` to `getMutationParam()`. Null values skipped. Common-entry skip optimization (both groupKey and sumValue must match). Non-idempotent (UpdateWhileWriteOnly checks range set). 11 integration tests (ungrouped total, grouped, delete decrement, update value, update group, no-op optimization, range query, reverse scan, WRITE_ONLY range check, negative values, rebuild).
- [x] **MAX_EVER_LONG / MIN_EVER_LONG index types** — `MinMaxEverIndexMaintainer` using FDB atomic MAX/MIN. Idempotent, _EVER semantics (deletes are no-ops). Negative values rejected (unsigned comparison). 10 tests (ungrouped, grouped, delete irreversibility, update, rebuild, negatives, empty store).
- [x] **COUNT_NOT_NULL index type** — `CountNotNullIndexMaintainer` using FDB atomic ADD. Like COUNT but skips entries where key expression fields are null (unset proto2 optional). Uses `keyExpressionHasNullField()` for proto field presence detection. Non-idempotent. 6 tests.
- [x] **COUNT_UPDATES index type** — `CountUpdatesIndexMaintainer` using FDB atomic ADD. Like COUNT but deletes are no-ops (count never decrements) and `skipUpdateForUnchangedKeys=false` (always re-counts on update). Tracks total insert+update events. Non-idempotent. 6 tests.
- [x] **MIN/MAX via VALUE index** — `EvaluateAggregateFunction` supports `FunctionNameMin`/`FunctionNameMax` via VALUE indexes. Scans 1 entry forward (MIN) or reverse (MAX). Unlike _EVER variants, reflects deletes. 4 tests.
- [x] **CLEAR_WHEN_ZERO option** — `Index.SetClearWhenZero(true)` enables FDB `CompareAndClear(zero)` after every ADD decrement. Atomically removes entries when count/sum reaches zero. Works with COUNT, COUNT_NOT_NULL, SUM indexes. Matches Java's `IndexOptions.CLEAR_WHEN_ZERO`. 3 tests.
- [x] **MIN_EVER_TUPLE / MAX_EVER_TUPLE index types** — `MinMaxEverTupleIndexMaintainer` using FDB BYTE_MIN/BYTE_MAX mutations with tuple-packed values. Unlike _LONG variants, supports any tuple-encodable type including negatives. Idempotent. Reuses `countKVCursor` with `tupleValues` flag for scanning. 8 tests.
- [x] **RANK index type** — `RankIndexMaintainer` with dual subspace (B-tree + RankedSet skip-list). Wire-compatible with Java's `RankedSet`. Supports BY_VALUE and BY_RANK scans, RankForScore/ScoreForRank queries, grouped and ungrouped modes, CountDuplicates option, JDK/CRC hash functions. 23 tests (6 RankedSet + 17 RankIndex).

- [x] **RANK conformance tests** — 11 specs: BY_VALUE Go→Java/Java→Go/mixed writes, delete cross-language, update cross-language, BY_RANK scan with rank ranges cross-validated, ranked set wire compatibility (Go writes→Java reads by rank, Java writes→Go reads by rank), delete updates ranked set. Cross-validated.

- [x] **RANK aggregate functions** — `EvaluateAggregateFunction` integration for RANK indexes: `COUNT_DISTINCT` (ranked set size), `RANK_FOR_SCORE`, `SCORE_FOR_RANK`, `SCORE_FOR_RANK_ELSE_SKIP` (sentinel on OOB), `COUNT` (unique only). Auto-index-selection + `canEvaluateRankAggregate` + `expressionsEqual`. 7 tests. Record function `RANK` not yet integrated.

- [x] **RANK deleteWhere** — Fixed: `RankIndexMaintainer.DeleteWhere(prefix)` clears both B-tree (primary) and ranked set (secondary) subspaces. Implemented as part of `DeleteRecordsWhere`. **MEDIUM**.

- [ ] **RANK preloadForLookup** — Java prefetches sparse upper skip-list levels into the RYW cache before `getNth`/`rank` calls, reducing FDB round trips. Go does sequential level-by-level reads. No correctness impact, but significant performance gap for deep ranked sets. **LOW**.

- [x] **RANK OnlineIndexer test coverage** — 4 tests: basic build, chunked build (limit=3), post-build maintenance, duplicate scores. Covers RANK index through OnlineIndexer path. **MEDIUM**.

- [x] **RANK reverse BY_RANK scan** — tested, works correctly (rank→score conversion + reverse standard scan). **LOW**.

- [x] **RANK continuation tokens** — tested paginated BY_RANK scan with limit 2, 3 pages. Works through standard cursor path. **LOW**.

- [ ] **Index types beyond implemented** — Java has more types: TEXT, BITMAP_VALUE, MULTIDIMENSIONAL, VECTOR, TIME_WINDOW_LEADERBOARD. (PERMUTED_MIN/MAX and MAX_EVER_VERSION done.) See 4.10.6.0 upgrade assessment §2.

- [x] **VERSION index type** — HIGH. Two phases:

  **Phase 1: Widen `KeyExpression.Evaluate()` signature** (prerequisite)
  - [x] Change `Evaluate(proto.Message)` → `Evaluate(*FDBStoredRecord[proto.Message], proto.Message)` across all expression types
  - Decision: Option 1 (match Java's `evaluateMessage(FDBRecord, Message)` exactly — two params). `record` = top-level context (version etc), `msg` = current message (changes during nesting).
  - [x] Update all call sites: index maintainers pass `(record, record.Record)`, message-only callers pass `(nil, msg)`
  - [x] NestingKeyExpression preserves `record` context while changing `msg` to sub-message (matching Java)
  - [x] All 8 expression types updated: `FieldKeyExpression`, `RecordTypeKeyExpression`, `EmptyKeyExpression`, `CompositeKeyExpression`, `NestingKeyExpression`, `GroupingKeyExpression`, `LiteralKeyExpression`, `KeyWithValueExpression`
  - [x] All 957 existing tests pass unchanged

  **Phase 2: VersionKeyExpression + VERSION index maintainer**
  - [x] `VersionKeyExpression` type: `Evaluate()` reads `record.Version` → returns `tuple.Versionstamp` as key component
  - [x] `VersionIndexMaintainer`: incomplete versionstamps use `SET_VERSIONSTAMPED_KEY` mutation, complete use normal `set()`. Delete: incomplete → `RemoveVersionMutation`, complete → `Clear`.
  - [x] `AddVersionMutation` extended with `VersionMutationType` (KEY vs VALUE) matching Java's `FDBRecordContext.addVersionMutation(MutationType, key, value)`
  - [x] `SaveRecord`/`DeleteRecord` update path: load version for old record when VERSION index exists via `hasVersionIndex()` check
  - [x] Wire format: version stored as Versionstamp in tuple-encoded key (matches Java)
  - [x] Proto serialization: `Version` message in `KeyExpression` proto (roundtrip tested)
  - [x] Conformance tests (VERSION index Go↔Java cross-validation) — 7 specs: Go writes/both scan, Java writes/both scan, mixed writes, cross-language delete (2 specs), cross-language update, same-tx local versions. Uses hex-encoded versionstamp bytes for wire comparison.

- [x] **Uniqueness violation tracking** — `ScanUniquenessViolations()` scans `IndexUniquenessViolationsKey` (7) subspace. `ResolveUniquenessViolation()` removes a single entry. Violations written on unique index save failure.

- [x] **Index validation** — `ValidateIndex()` scans all records and index entries to detect orphaned entries (in index but not in records) and missing entries (in records but not in index).

- [x] **Primary key component deduplication** — `primaryKeyComponentPositions` computed at `Build()` time via `buildPrimaryKeyComponentPositions()`. `indexEntryKey()` calls `trimPrimaryKey()` to omit PK components already in the index key. `getEntryPrimaryKey()` reconstructs the full PK on read. Wire-compatible with Java. Conformance-tested: Go writes → Java scans, Java writes → Go scans, cross-write. 3 conformance specs + 15 unit tests.

- [x] **Bulk index delete** — `DeleteIndexEntries()` clears all entries for a given index. `DeleteIndexEntriesInRange()` clears entries within a tuple range.

- [x] **Aggregate functions via indexes** — `EvaluateAggregateFunction()` on store with auto-index-selection. Supports COUNT, COUNT_NOT_NULL, COUNT_UPDATES, SUM, MIN_EVER, MAX_EVER via atomic mutation indexes, plus MIN/MAX via VALUE indexes. `IndexAggregateFunction` type with name, operand, optional explicit index. `canEvaluateAggregate()` / `isGroupPrefix()` for index matching. 15 tests.

---

## Metadata — conformance gaps

### HIGH

- [x] **ThenKeyExpression** — `CompositeKeyExpression` via `Concat()` now computes Cartesian cross-product matching Java's `ThenKeyExpression` semantics.

- [x] **NestingKeyExpression** — `Nest("field", child)` navigates into nested message fields. `NestFanOut("field", child)` for repeated message fields. Composite nested fields work (e.g., `Nest("flower", Concat(Field("type"), Field("color")))`). Enum fields supported via `int64` conversion.

- [x] **FormerIndex tracking** — `FormerIndex` struct with `SubspaceKey`, `AddedVersion`, `RemovedVersion`, `FormerName`. `RemoveIndex()` on builder creates FormerIndex and removes from all record types. `Build()` validates no subspace key reuse. `GetFormerIndexes()` on metadata.

- [x] **Schema evolution validation** — `MetaDataEvolutionValidator` with builder pattern matching Java's. Validates: version ordering, split record changes, record type preservation (PK immutability, type key immutability), index lifecycle (type/expression/version immutability, FormerIndex tracking), message descriptor evolution (field removal, rename, type change, cardinality change, enum value removal, safe int32→int64 promotion), new required field rejection. 7 configurable options (allowNoVersionChange, allowIndexRebuilds, allowUnsplitToSplit, etc.). 23 tests.

### MEDIUM

- [x] **Metadata proto serialization** — Java has `toProto()`/`fromProto()` for persisting metadata definitions. Implemented in Go.
  - [x] **KeyExpression proto serialization** — `ToKeyExpression()` on all expression types + `KeyExpressionFromProto()` dispatcher. Roundtrip + wire format tests. Matches Java's `KeyExpression.toKeyExpression()`/`fromProto()`. FanType mapping: Go None→SCALAR, FanOut→FAN_OUT, Concatenate→CONCATENATE.
  - [x] **RecordMetaData.toProto()/fromProto()** — `ToProto()` serializes metadata (file descriptor, dependencies, indexes with record type associations, record types with primary keys, former indexes, flags). `RecordMetaDataFromProto()` rebuilds from proto with topological dependency resolution. Index subspace keys tuple-packed. Explicit record type keys via Value proto. Wire roundtrip tested.

- [x] **Explicit record type keys** — `SetRecordTypeKey()` on `RecordTypeBuilder`, `GetRecordTypeKey()` on `RecordType`. Falls back to `RecordTypeIndex` if not set.

- [x] **Multi-type indexes** — `AddMultiTypeIndex(recordTypeNames, index)`. 0 types → universal, 1 type → single-type, 2+ types → multi-type (stored per RecordType, included in `GetIndexesForRecordType`). Matches Java semantics.

- [x] **Schema evolution version tracking** — `SetVersion()` on builder sets metadata version. Used in store header for compatibility tracking.

- [x] **Primary key prefix checking** — `PrimaryKeyHasRecordTypePrefix()` on `RecordMetaData`. Checks all record types' primary keys start with `RecordTypeKeyExpression`, including through `CompositeKeyExpression`.

### LOW

- [ ] **Missing key expression types** — Remaining: DimensionsKE, AtomKE, CollateFunctionKE, OrderFunctionKE, InvertibleFunctionKE. Done: GroupingKE, LiteralKE, KeyWithValueKE, VersionKE, FunctionKE, SplitKE, ListKE, LongArithmeticKE. See 4.10.6.0 upgrade assessment §3.

- [ ] **Synthetic record types** — Computed/joined/unnested record types. Large feature.

- [ ] **User-defined functions** — `userDefinedFunctionMap` for custom query functions.

- [ ] **Views** — Named query/aggregation views.

- [x] **Subspace key counter** — `EnableCounterBasedSubspaceKeys()` on builder. Auto-assigns incrementing int64 subspace keys to indexes instead of using index name strings.

- [ ] **Extension options processing** — Processing protobuf schema extension options.

---

## Cursor — conformance gaps

### HIGH

- [x] **ExecuteProperties `skip` field** — `ExecuteProperties.Skip` skips N records before applying row limit. FDB-level limit accounts for skip. Tested with skip-only and skip+row limit.

- [x] **ScannedRecordsLimit** — `ExecuteProperties.ScannedRecordsLimit` enforced in `keyValueCursor.OnNext()`. Returns `ScanLimitReached` with continuation when limit hit.

- [x] **Cursor factory methods** — `Empty[T]()` and `FromList[T](items)` implemented matching Java's `RecordCursor.empty()` and `RecordCursor.fromList()`.

- [x] **RecordCursorResult validation** — `GetValue()` panics on `!HasNext()` matching Java's `IllegalResultValueAccessException`. `HasStoppedBeforeEnd()` helper added.

### MEDIUM

- [ ] **Cursor combinators** — Java has 20+ cursor combinator types. Implemented in Go:
  - [x] `ConcatCursor` — sequential concatenation with proto-wrapped continuations
  - [x] `MapCursor` (MapResultCursor) — value transformation preserving continuations
  - [x] `Empty`, `FromList`, `FromListWithContinuation`, `Filter`, `Skip`, `LimitRows`, `SkipThenLimit`, `OrElse` — basic utilities
  - [x] **Set operations**: `UnionCursor` (ordered merge-union with deduplication), `IntersectionCursor` (ordered merge-intersection). Both support forward/reverse, proto-wrapped continuations, multi-cursor (3+). `ComparisonKeyFunc` for custom comparison keys.
  - [x] `DedupCursor` — adjacent duplicate removal with proto-wrapped `DedupContinuation`. Custom equal/pack/unpack functions.
  - [x] `FlatMapPipelinedCursor` — flat-map with proto-wrapped `FlatMapContinuation`, check value support
  - [x] `ChainedCursor` — procedural iterator with generator function. Raw byte continuations (no proto). Custom encode/decode.
  - [ ] **Aggregation**: `AggregateCursor` with accumulator states
  - [x] `AutoContinuingCursor` — auto-creates new transactions on scan/time/byte/row limits for seamless large-dataset scanning across tx boundaries. Includes retry logic for transient errors.
  - [x] `FallbackCursor` — primary cursor with automatic failover on error. One-shot fallback, passes last successful result to factory.
  - [x] `MapErrCursor` — fallible transform combinator (fn returns (R, error)). 3 tests.
  - [x] `AsListWithContinuation` — pagination helper: drains cursor to slice, returns continuation bytes. 3 tests.

- [ ] **CursorLimitManager** — Java has a separate class for comprehensive limit tracking (record scan, byte scan, time). Go has inline limit logic in keyValueCursor.

- [x] **RecordCursor instance methods** — `First()`, `GetCount()`, `Reduce()` as standalone generic functions. `SkipCursor()`, `LimitRowsCursor()` as cursor wrappers. Matches Java's `first()`, `getCount()`, `reduce()`, `skip()`, `limitRowsTo()`.

### LOW

- [ ] **Visitor pattern** — Java has `RecordCursorVisitor` interface for cursor inspection/instrumentation.
- [x] **Continuation SerializationMode** — Go uses TO_OLD (raw bytes) for writing, accepts both TO_OLD and TO_NEW (proto-wrapped) for reading. Confirmed working with Java Record Layer 4.10.6.0 (all conformance tests pass).

---

## Store — conformance gaps

### HIGH

- [x] **Store state management** — `GetRecordStoreState()` returns store header + index states. `SetStoreLockState()` persists lock state to header. `ReloadRecordStoreState()` forces reload from FDB.

- [x] **DeleteRecordsWhere** — `DeleteRecordsWhere(prefix)` bulk-deletes all records with a PK prefix via range clears (no scanning). Clears records, versions, record counts, and all index entries. Type-specific indexes cleared entirely; universal indexes require aligned leading expression. `DeleteWhere(prefix)` on `IndexMaintainer` interface. RANK indexes clear both B-tree and ranked set subspaces. 10 unit tests + 5 conformance specs (Go deletes/Java verifies, Java deletes/Go verifies, mixed writes, delete+reinsert, Java-written records).

- [x] **Query execution methods** — `EvaluateStoreFunction()` for store-level functions (VERSION), `EvaluateAggregateFunction()` for index aggregates, `EvaluateRecordFunction()` for index record functions. All matching Java's dispatch hierarchy.
  - [x] `CountRecords(ctx, low, high, lowEndpoint, highEndpoint, continuation, scanProperties)` — scan-based record count (not atomic counter). Matches Java's `FDBRecordStore.countRecords()`.
  - [x] `EvaluateRecordFunction(fn, record)` — evaluates index record functions (e.g. RANK) for a specific record. Auto-selects best index. 5 tests.
  - [x] `EvaluateStoreFunction(fn, record)` — evaluates store-level functions. VERSION function returns record version from store context. 6 tests.

- [x] **Per-type record count** — `GetSnapshotRecordCountForRecordType(recordTypeName)` added. Requires `RecordTypeKeyExpression` as count key. Matches Java's `getSnapshotRecordCountForRecordType()`.

### MEDIUM

- [x] **Store statistics** — `EstimateStoreSize()`, `EstimateRecordsSize()`, `EstimateRecordsSizeInRange(TupleRange)`, `EstimateIndexSize(*Index)`, `GetRangeSplitPoints(chunkSize)` using FDB native operations. `TupleRange.ToFDBRange(subspace)` conversion. `FDBRecordContext.GetApproximateTransactionSize()` for 10MB limit monitoring. 12 tests.

- [x] **Format version / user version access** — `GetFormatVersion()`, `GetUserVersion()`, `SetUserVersion()`, `GetMetaDataVersion()`. Persisted in store header.

- [x] **Serializer access** — `GetMetaData()`, `GetIndexMaintainer()` on store. `Context()` and `Subspace()` already exposed.

- [x] **Conformance test for type-changed existence check** — All 5 modes tested including cross-type Order→Customer tests for `ERROR_IF_RECORD_TYPE_CHANGED` and `ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED`.

### LOW

- [x] **Store API surface expansion** — 13 new public methods matching Java: `RecordsSubspace`, `IndexSubspace`, `IndexSecondarySubspace`, `GetReadableIndexes`, `GetEnabledIndexes`, `GetAllIndexStates`, `RebuildAllIndexes`, `VacuumReadableIndexesBuildData`, `DeleteStore`, `FirstUnbuiltRange`, `IsCacheable`, `GetStoreHeader`, `GetAllIndexStatesMap`. 15 tests.
- [x] **Advanced store operations** — `DryRunSaveRecord`, `DryRunDeleteRecord`, `ScanRecordKeys`, `IsIndexReadableUniquePending`, `GetWriteOnlyIndexes`, `GetDisabledIndexes`, `GetIndexesToBuildSince`, `ResolveUniquenessViolationByDeletion`, `ScanUniquenessViolationsForValue`. 24 tests.
- [ ] **Remaining advanced store operations** — Java has `preloadRecordAsync()`, `repairRecordKeys()`. Not yet ported.

- [ ] **Synthetic records** — Java has `loadSyntheticRecord()`. Large feature tied to synthetic record types.

---

## Database / Transaction — conformance gaps

### HIGH

- [x] **FDBDatabaseRunner** — `FDBDatabaseRunner` with `MaxAttempts`, `InitialDelay`, `MaxDelay`, exponential backoff. `RunWithRetry()` wraps transaction execution with configurable retry. Falls back to FDB's native retry when config is nil.

- [x] **FDBRecordContextConfig** — `RecordContextConfig` with `TransactionTimeout`, `Priority`, `TransactionID`. Applied in `Run()`/`RunWithRetry()`.

- [x] **Commit hooks** — `AddCommitCheck()` for pre-commit consistency checks, `AddPostCommit()` for post-commit callbacks. Run in `flushAndCommit()`. Matches Java's `CommitCheckAsync` and `PostCommit` interfaces.

### MEDIUM

- [x] **Timer / instrumentation** — `StoreTimer` with `Event`/`Counter`/`CounterSnapshot` types, nil-safe, goroutine-safe (atomic counters + sync.Map). 9 timed events (Save/Load/Delete/Commit/OpenStore/etc) + 9 count events (key/byte counts). Wired into `FDBRecordContext.Timer()`, `SaveRecordWithOptions`, `LoadRecord`, `DeleteRecord`, `Create/Open/CreateOrOpen`. 32 specs (unit + integration).

- [x] **Transaction priority** — `TransactionPriority` type with `PriorityDefault`, `PriorityBatch`, `PrioritySystemImmediate`. `SetTransactionPriority()` on `FDBRecordContext`.

- [x] **Store state caching** — `MetaDataVersionStampStoreStateCache` + `PassThroughRecordStoreStateCache`. LRU+TTL, `\xff/metadataVersion` invalidation. 40 specs, 2.2x speedup.

- [x] **Read/write version management** — `GetReadVersion()`, `SetReadVersion()` on `FDBRecordContext`. Wraps FDB transaction read version.

- [x] **Conflict key reporting** — `GetConflictingKeys()` on `FDBRecordContext` wraps FDB's conflict range reporting for debugging.

### LOW

- [ ] **FDBDatabaseFactory** — Factory/pooling for database instances.
- [ ] **Weak read semantics** — `WeakReadSemantics` for causal read risky, version staleness bounds.
- [ ] **Directory layer caching** — Multi-tenant keyspace management.
- [ ] **Transaction ID / MDC / logging** — Transaction tracing and structured logging.
- [ ] **Latency injection** — `FDBLatencySource` for testing.

---

## Record versioning — conformance gaps

### MEDIUM

- [x] **Version comparison/ordering** — `Equal()`, `Less()` implemented matching Java's `equals()`/`compareTo()`.

- [x] **Version range methods** — `FirstInDBVersion()`, `LastInDBVersion()`, `FirstInGlobalVersion()`, `LastInGlobalVersion()`, `Next()`, `Prev()`. All matching Java semantics.

- [x] **MIN_VERSION / MAX_VERSION constants** — `MinVersion()` (all zeros), `MaxVersion()` fixed to match Java: bytes 0-8 = 0xFF, byte 9 = 0xFE, bytes 10-11 = 0xFF. Was incorrectly all-0xFE.

### LOW

- [x] **Versionstamp conversion** — `FromVersionstamp()` creates FDBRecordVersion from FDB Versionstamp. `ToVersionstamp()` converts back. Matches Java API.

---

## Behavioral compatibility gaps (found in 2026-03-09 audit)

### CRITICAL

- [x] **updateSecondaryIndexes doesn't handle cross-type overwrites** — Fixed: three-way index partition (old-only/new-only/common) matching Java's `updateSecondaryIndexes()`. Old-type-only index entries are deleted, new-type-only entries are inserted, common entries are updated. 4 tests: cross-type overwrite, round-trip back, same-type sanity, cross-type delete.

- [x] **Stale metadata detection missing** — Fixed: `checkPossiblyRebuild` now returns `StaleMetaDataVersionError` when stored version > local version, matching Java's `RecordStoreStaleMetaDataVersionException`. Also fixed `SetSplitLongRecords`, `SetStoreRecordVersions`, and `SetRecordCountKey` to bump metadata version when value changes, matching Java. 4 tests.

- [x] **Unique index pre-commit check missing** — Fixed: `checkUniqueness` now reads the full prefix range (removed `Limit:1`) so FDB's read-conflict tracking covers the entire index value range. With `Limit:1`, FDB only tracked conflicts up to the first key found, allowing concurrent inserts at higher keys. Now matches Java's `StandardIndexMaintainer.checkUniqueness()` which also reads the full range. 3 tests: concurrent same-key rejection, concurrent different-key success, sequential uniqueness enforcement.

### HIGH

- [x] **COUNT index UpdateWhileWriteOnly skips range set check** — Fixed: `UpdateWhileWriteOnly` now checks `IndexingRangeSet.ContainsKey()` before updating, matching Java's `StandardIndexMaintainer.updateWriteOnlyByRecords()`. Only updates if PK is in the already-built range. Added `isKeyInIndexBuildRange()` to `indexStoreContext`. 4 tests.

- [x] **Record count rebuild on metadata version change** — Fixed: `checkPossiblyRebuildRecordCounts()` compares stored `RecordCountKey` proto against current metadata, independent of version numbers. Clears old counts, rescans all records, updates store header. Runs before the version-gated index rebuild, matching Java's `checkRebuild()` flow. 4 tests: add key, change key, remove key, unchanged key no-op.

- [x] **validateRecordUpdateAllowed timing differs** — Fixed: moved `validateRecordUpdateAllowed()` after record load and existence checks, before write. Now existence/type errors take precedence over lock errors, matching Java's `saveRecordAsync()` and `deleteTypedRecord()`. Delete of non-existent record returns `(false, nil)` even when locked. 2 tests.

- [x] **clearIndexData uses subspace.Range() which misses prefix key** — Fixed: `clearIndexData()` for the index entries subspace now uses `fdb.PrefixRange()` instead of `ClearRange(subspace)`. Go's `subspace.FDBRangeKeys()` returns `[prefix\x00, prefix\xff)` which excludes the exact prefix key. Ungrouped aggregate indexes (COUNT/SUM) store data at the subspace prefix itself (Pack of empty tuple = prefix bytes). Java explicitly uses `Range.startsWith(indexSubspace.pack())` with the comment "startsWith to handle ungrouped aggregate indexes". Found during SUM index rebuild testing.

### MEDIUM

- [x] **Key/value size validation missing on index entries** — Fixed: `checkKeyValueSizes()` validates FDB key (10KB) and value (100KB) limits before writing index entries. Returns `IndexKeySizeError`/`IndexValueSizeError` with index name, primary key, and sizes. Applied in both `StandardIndexMaintainer.Update()` and `CountIndexMaintainer.Update()`. 1 test.

- [x] **COUNT index doesn't skip common grouping keys on update** — Fixed: `CountIndexMaintainer.Update()` now calls `removeCommonGroupingKeys()` to filter unchanged grouping keys before applying -1/+1 atomic mutations. Matches Java's `AtomicMutationIndexMaintainer.updateIndexKeys()` common key filtering.

- [x] **COUNT index conformance tests** — 6 conformance specs: Go writes→both scan, Java writes→both scan, mixed writes combined counts, Go deletes Java-written record, Java deletes Go-written record, update moves counts. Java uses `new GroupingKeyExpression(field("price"), 0)` matching Go's `GroupAll(Field("price"))`.

---

## Go style issues (found in 2026-03-09 audit)

### HIGH

- [x] **RecordCursor interface too wide (5 methods)** — Fixed: slimmed to 2 methods (`OnNext` + `Close`). `Seq`/`Seq2`/`SeqWithContinuation` are now package-level generic functions. Removed 63 identical method implementations across 21 cursor types. Net -900 lines.

- [x] **Panics in library code** — Fixed: converted 5 `FDBRecordVersion` panics to error returns (`GetGlobalVersion`, `GetDBVersion`, `Next`, `Prev`, `ToVersionstamp`). `RecordCursorResult.GetValue()` kept as panic — programming error (matches Java's `IllegalResultValueAccessException`).

### MEDIUM

- [x] **sync.Map misuse in FDBRecordContext** — Fixed: replaced `sync.Map` with plain `map` and `atomic.Int32` with `int32`. `HasVersionMutations()` now uses `len()`.

- [x] **Silent error swallowing in addRecordCount** — Fixed: `addRecordCount()` now returns `error` and callers propagate it. No more silent swallowing.

- [x] **recover() removed from key_value_cursor.go** — Root-caused FDB Go bindings bug: `RangeIterator.Advance()` returns true on empty batch (missing `ri.done = true`), causing `Get()` to panic with index OOB. Fixed upstream via Bazel patch (`patches/fdb-go-range-iterator-done.patch`). No workarounds in our code.

- [x] **store.go too large (2004 lines)** — Split into `store.go` (1134, core CRUD/scanning/state), `store_builder.go` (549, builder/lifecycle/rebuild), `store_typed.go` (228, TypedFDBRecordStore), `store_version.go` (115, version management).

- [x] **cursor.go split** — Split 1202→3 files: `cursor.go` (286, interfaces), `cursor_combinators.go` (735, combinators), `cursor_util.go` (195, utilities).

- [x] **NewRecordMetaData discards Build() error** — Fixed: removed the function entirely. Callers should use `NewRecordMetaDataBuilder()` and `Build()` for proper error handling.

### STYLE (LOW)

- [ ] **Get prefix on ~30 trivial accessors** — `GetRecordType()`, `GetIndex()`, `GetValue()`, `GetContinuation()`, etc. Go convention: drop `Get` for simple field reads.

- [x] **interface{} → any** — Fixed: replaced all 524 occurrences of `interface{}` with `any` across 72 files.

---

## Split records — conformance status

**FULLY CONFORMANT.** No gaps found. All constants, save/load/delete/exists methods, SizeInfo tracking, cursor integration, and edge cases match Java behavior. Wire-compatible. Version handling separated into store layer (architectural choice, not a gap).

---

## Infrastructure

- [x] Bazel migration, nogo linting, CI pipeline, justfile — all done
- [ ] **KeySpace/KeySpacePath** — Enterprise key management. LOW priority.
- [x] **ScanLimiter** — TimeScanLimiter, ByteScanLimiter, RecordScanLimiter all enforced in both `keyValueCursor` and `indexCursor`. Time limit uses free initial pass (first record always succeeds). Continuation returned for cross-transaction resumption.

### HIGH — Conformance test restructure

- [x] **Remove Gradle, make conformance fully Bazel-native** — Killed Gradle, flattened `conformance/java/` and `conformance/helpers/` into single `conformance/` directory. Split monolithic ConformanceSteps.java into 22 per-feature step classes with `@ConformanceStep` annotation dispatch. Added auto-rebuild conformance tests exercising `checkPossiblyRebuild()` without `ALWAYS_READABLE_CHECKER`. Removed force-set of IDs after `mergeFrom` in load steps. 211 conformance specs, single BUILD.bazel, zero external tooling.

---

## Test quality gaps (identified 2026-03-10 audit)

### MEDIUM

- [x] **Error path test coverage weak** — Added `error_path_test.go` with 41 specs covering: unique index violation errors (READABLE), IndexValueSizeError/IndexKeySizeError (was 0 tests), key expression validation errors (field not found, FanTypeNone on repeated, nil message, nesting into nil/nonexistent), RangeSet validation (empty key, key too large, inverted range, MissingRanges empty key), ErrRecordStoreStateNotLoaded (SetUserVersion/SetStoreLockState/UpdateRecordCountState), SaveRecord validation (all 5 existence check modes, lock precedence, unknown type, cross-type overwrite), store builder errors (reload non-existent), metadata build errors (missing PK, FormerIndex subspace reuse), error message format assertions, delete error paths. Total unit specs: 624 (was 583).
- [x] **Atomic index maintainer code duplication** — Extracted `indexGroupingCount()`, `evaluateGroupingKeys()`, and `updateWhileWriteOnlyNonIdempotent()` into `atomic_index_helpers.go`. Removed 184 lines of identical code across 6 maintainer files. Remaining per-maintainer logic (mutation semantics, entry types) is genuinely unique.

### LOW

- [x] **`existence_check.go` only 1 of 4 enum values tested** — Actually all 5 values were already tested in `existence_test.go` (ERROR_IF_EXISTS, ERROR_IF_NOT_EXISTS, ERROR_IF_TYPE_CHANGED, ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED). Additional coverage added in `error_path_test.go`.
- [x] **`indexing_range_set.go` dedicated unit tests** — 10 specs in `indexing_range_set_test.go`: empty/full/contains/tuple-packed/first-missing/nil-when-complete/multiple-gaps/clear/requireEmpty-overlap/incremental-build-simulation.
- [x] **Scan limit boundary tests** — 18 specs in `scan_limit_test.go`: byte limit (1-byte, partial, resume, no-limit), scanned records limit (exact, limit-of-1), row limit with SourceExhausted. Also fixed byte scan limit bug: was post-read (discarding boundary record), now pre-read matching Java's CursorLimitManager. Fixed in both keyValueCursor and indexCursor.
- [x] **cursor.go `NoNextReason` helpers tested** — Dedicated specs for all 5 NoNextReason values testing IsOutOfBand/IsSourceExhausted/IsLimitReached, plus 6 specs for RecordCursorResult.HasStoppedBeforeEnd.

---

## Bugs found by edge-case audit (2026-03-10)

All 27 bugs verified by dedicated subagents with reproducing tests (2026-03-10).
Data loss bugs marked **[DATA LOSS 2x]**. Worktree paths relative to `.claude/worktrees/`.

### Cursor combinators — verified in `agent-adb21082`, fixed

- [x] **[DATA LOSS 2x] UnionCursor continues after child hits limit** — Fixed: stop union when any child has OOB limit. File: `merge_cursor.go`.
- [x] **[DATA LOSS 2x] LimitRowsCursor returns EndContinuation (un-resumable)** — Fixed: preserve inner continuation on limit. File: `cursor.go`.
- [x] **[DATA LOSS 2x] OrElseCursor switches to alternative on out-of-band limits** — Fixed: stay UNDECIDED on OOB limits. File: `cursor.go`.
- [x] **[DATA LOSS 2x] IntersectionCursor.weakestNoNextReason() always returns SourceExhausted** — Fixed: proper NoNextReason comparison. File: `merge_cursor.go`.

### Key expressions — verified in `agent-a9e81304`, fixed

- [x] **[DATA LOSS 2x] FieldKeyExpression.Evaluate returns default for unset proto2 fields** — Fixed: check `m.Has(fd)` for proto2 optional, return nil. File: `key_expression.go`.
- [x] **[DATA LOSS 2x] FieldKeyExpression nil message ignores FanType** — Fixed: FanOut returns empty, Concatenate returns `[[[]]]`. File: `key_expression.go`.
- [x] **NestingKeyExpression.Evaluate panics on nil message** — Fixed: nil check returns `[[nil]]`. File: `key_expression.go`.
- [x] **RecordTypeKeyExpression.Evaluate panics on nil message** — Fixed: nil check returns `[[nil]]`. File: `key_expression.go`.

### Record version / context — verified in `agent-a28fc2d7`, fixed

- [x] **FDBRecordVersion.Next()/Prev() no carry across 12 bytes** — Fixed: full 12-byte big-endian carry/borrow. File: `record_version.go`.
- [x] **NewCompleteVersion accepts all-0xFF global version** — Fixed: reject incomplete marker bytes. File: `record_version.go`.
- [x] **WithCommittedVersion on already-complete version** — Fixed: error on already-complete. File: `record_version.go`.
- [x] **[DATA LOSS 2x] CommitWithVersionstamp skips pre-commit checks and post-commit hooks** — Fixed: run pre-commit checks + post-commit hooks. File: `database.go`.

### Store CRUD / split records — verified in `agent-af7e30fd`, fixed

- [x] **SaveRecordWithOptions swallows deserialization errors** — Fixed: propagate deser error in ErrorIfTypeChanged path. File: `store.go`.
- [x] **[DATA LOSS 2x] DeleteRecord destroys data before deserialization check** — Fixed: deserialize BEFORE deleteSplit. File: `store.go`.
- [x] **[DATA LOSS 2x] FDB row limit premature exhaustion with versioning** — Fixed: double FDB limit when IsStoreRecordVersions. File: `key_value_cursor.go`.
- [x] **[DATA LOSS 2x] keyValueCursor exclusive low endpoint uses append(0x00)** — Fixed: use fdb.Strinc(). File: `key_value_cursor.go`.

### Metadata / schema evolution — verified in `agent-a826ca49`, fixed

- [x] **RemoveIndex doesn't increment version** — Fixed: pre-increment version before setting RemovedVersion. File: `metadata.go`.
- [x] **[DATA LOSS 2x] checkPossiblyRebuild doesn't clean up former index data** — Fixed: removeFormerIndexData() clears 6 subspaces. File: `store_builder.go`, `index_state.go`.
- [x] **MetaDataEvolutionValidator rejects index changes with allowIndexRebuilds=true** — Fixed: early return when allowIndexRebuilds && lastModifiedVersion changed. File: `metadata_evolution_validator.go`.
- [x] **validateFormerIndexes: missing unconditional check + wrong operator** — Fixed: unconditional `>` check + conditional `!=`. File: `metadata_evolution_validator.go`.
- [x] **createStoreHeader doesn't persist RecordCountKey** — Fixed: include RecordCountKey in header. File: `store_builder.go`.

### Index maintainers — verified in `agent-a60827f1`, fixed

- [x] **checkUniqueness compares trimmed PK with full PK** — Fixed: use getEntryPrimaryKey() for full PK reconstruction. File: `index_maintainer.go`.
- [x] **[DATA LOSS 2x] checkUniqueness violation entries: double-trimmed PK** — Fixed: same getEntryPrimaryKey() fix resolves both issues. File: `index_maintainer.go`.
- [x] **[DATA LOSS 2x] CountNotNull keyExpressionHasNullField missing NestingKeyExpression** — Fixed: added NestingKeyExpression case. File: `count_not_null_index_maintainer.go`.

### OnlineIndexer — verified in `agent-a3134e5b`
Test file: `agent-a3134e5b/pkg/recordlayer/online_indexer_bug_verify_test.go`

- [x] **[DATA LOSS 2x] OnlineIndexer double-counts boundary records** — Fixed: use Java's `limit+1` look-ahead pattern. Request limit+1 records, index only the first limit, use the (limit+1)th record's PK as the exclusive range boundary. Boundary records never re-scanned. File: `online_indexer.go`.
- [x] **[DATA LOSS 2x] OnlineIndexer skips records when type filter exhausts limit** — Fixed: track `scannedCount` across ALL records (not just indexed ones). Type-filtered records still advance the scan position via the limit+1 look-ahead. File: `online_indexer.go`.

### Bug hunt scoreboard

27 bugs found, 27 fixed. 16 classified as data loss (2x). Current: 949 unit/integration specs, 341 conformance specs (1290 total).

| Agent | Worktree | Bugs | 1x | 2x | Award |
|-------|----------|------|----|----|-------|
| Cursor combinators | `agent-adb21082` | 4 | 0 | 4 | $800 |
| Key expressions | `agent-a9e81304` | 4 | 2 | 2 | $600 |
| Record version/context | `agent-a28fc2d7` | 4 | 3 | 1 | $500 |
| Store CRUD/split | `agent-af7e30fd` | 4 | 1 | 3 | $700 |
| Metadata evolution | `agent-a826ca49` | 5 | 4 | 1 | $600 |
| Index maintainers | `agent-a60827f1` | 3 | 1 | 2 | $500 |
| OnlineIndexer | `agent-a3134e5b` | 2 | 0 | 2 | $400 |
| **Total** | | **27** | **11** | **16** | **$4,100** |

---

## Bug bounty round 2 (2026-03-13)

Second audit focused on arithmetic overflow, off-by-one, error handling, nil safety, and retry semantics. 20 bugs found across 4 test files, all fixed.

### P0 — data loss

- [x] **Empty PK allows range-clearing all records** — `saveWithSplit`/`deleteSplit`/`clearRecordKeyRange` now reject empty primary keys. File: `split_helper.go`.
- [x] **EmptyKeyExpression accepted as primary key** — `Build()` now rejects PK expressions producing 0 columns. File: `metadata.go`.
- [x] **normalizeKeyForPositions missing GroupingKeyExpression** — `DeleteRecordsWhere` failed on universal COUNT indexes. Fixed: delegate to `wholeKey`. File: `key_expression.go`.
- [x] **SUM index negation overflow on MinInt64** — `-math.MinInt64 == math.MinInt64` in two's complement. Now returns error. File: `sum_index_maintainer.go`.

### P1 — incorrect behavior

- [x] **isRetryableError uses type assertion, not errors.As** — Wrapped FDB errors not detected as retryable. Fixed: `errors.As()`. File: `runner.go`.
- [x] **ByteScanLimit off-by-one (> vs >=)** — Allowed one extra record when `bytesScanned == limit`. Fixed in `key_value_cursor.go` and `index_scan.go`.
- [x] **FDB limit overflow: math.MaxInt + 1** — Wraps to MinInt. Added guard in `key_value_cursor.go`, `index_scan.go`, `count_index_maintainer.go`.
- [x] **OnlineIndexer recordsProcessed not reset on retry** — Inflated counts after FDB transaction conflict. Fixed: reset at top of closure. File: `online_indexer.go`.
- [x] **CommitWithVersionstamp swallows vsFuture.Get() errors** — Only requests versionstamp future when mutations exist; propagates errors. File: `database.go`.
- [x] **CountNotNull null check on wrong key portion** — Was checking grouping (leading) portion instead of grouped (trailing). Fixed: `evaluateGroupingKeys` checks trailing columns only. File: `count_not_null_index_maintainer.go`.

### P2 — panics

- [x] **merge_cursor compareField unchecked type assertion** — `int64` vs `string` comparison panics. Fixed: checked assertions with error propagation. File: `merge_cursor.go`.
- [x] **SaveRecord nil proto.Message** — Panics at `ProtoReflect()`. Added nil check. Files: `store.go`, `store_api.go`.
- [x] **IndexEntry nil Index field** — `PrimaryKey()`/`IndexValues()` panic on manually constructed entries. Added nil guard. File: `index_scan.go`.
- [x] **getAggregator unchecked type assertion** — Non-int64 accumulator panics. Fixed: checked assertion. File: `aggregate_function.go`.
- [x] **keyExpressionColumnSize unknown type** — Silently returns 0 instead of erroring. Added `keyExpressionColumnSizeChecked` variant. File: `index_scan.go`.

### P3 — edge cases

- [x] **getEntryPrimaryKey truncated entry** — No length validation before extracting PK from index entry. Added minimum-length check. File: `index.go`.
- [x] **record_key_cursor hasMore not buffered** — `Advance()` result lost on FDB iterator. Added `peekedHasMore` buffer. File: `record_key_cursor.go`.

20 bugs found, 20 fixed. Test files: `bug_bounty_test.go`, `bug_bounty2_test.go`, `bug_bounty3_test.go`, `byte_limit_bug_test.go`. Current: 1065 unit/integration specs, 347 conformance specs (1412 total).

---

## Bugs found by chaos testing (2026-03-14)

Model-based chaos testing framework: in-memory model shadows real FDB store, random operations + fault injection, periodic verification catches divergence. Concurrent stress testing validates snapshot-consistent derived state under multi-goroutine contention.

**Test breakdown:** 71 targeted + 15 random + 5 concurrent = 91 chaos tests.

**Verification checks:** record count, VALUE indexes (including covering index value verification), COUNT indexes, SUM indexes, RANK indexes, PERMUTED_MIN/MAX indexes, VERSION indexes, COUNT_UPDATES (model-based only), MIN/MAX_EVER (model-based only). Concurrent mode uses snapshot-based validation (builds model from store, verifies derivable state only).

**Index types covered:** VALUE, COUNT, SUM, RANK, MAX_EVER, MIN_EVER, COUNT_UPDATES, PERMUTED_MIN, PERMUTED_MAX, VERSION, covering (KeyWithValue) — 7 simultaneously in kitchen sink tests.

**Concurrent stress tests:** 4 workers × 5s, snapshot validation every 1s. Kitchen sink: 6 snapshot-verifiable index types under concurrent load. High contention: 8 workers, 5 PKs.

### Bug found

- [x] **PERMUTED_MIN/MAX Update() doesn't handle group membership changes** — When a record's grouping key changes (e.g., quantity updates), the old group's permuted entry was left stale. Decomposed `Update()` into `updatePermutedForInsert()` and `updatePermutedForRemove()` helpers. UPDATE path now properly processes new groups before primary update, then cleans up old groups after. File: `permuted_min_max_index_maintainer.go`.

---

## Bug bounty round 3 (2026-03-15)

Third audit via 5 parallel subagents targeting: cursor combinators, index maintainers, store operations, online indexer, metadata + expressions.

### Agent 1: Cursor combinators

Root cause: `EndContinuation` is overloaded to mean both "iteration truly done" and "no continuation available." This poisons every combinator that checks `continuation.IsEnd()`.

- [x] **Bug 1: `LimitRowsCursor(n<=0)` leaks inner cursor** — Fixed: close inner cursor before returning Empty. **$100**.
- [x] **Bug 2: `AutoContinuingCursor` infinite loop on EndCont + HasStoppedBeforeEnd** — Documented: matches Java behavior. HasStoppedBeforeEnd + EndContinuation doesn't occur with real cursors (they always provide valid continuations for non-exhaustion stops). **$100**.
- [x] **Bug 3: `ConcatCursors` data loss with EndCont inner cursors** — Documented: matches Java's `ConcatCursorContinuation.isEnd = secondCursor && inner.isEnd()`. Only affects artificial cursors returning values with EndContinuation. **$200**.
- [x] **Bug 4: `FlatMapPipelined` data loss with EndCont inner cursors** — Documented: matches Java. Inner EndContinuation on values = inner exhausted. Same limitation in Java's FlatMapContinuation. **$200**.
- [x] **Bug 5: `ChainedCursor` + `ConcatCursors` pagination data loss** — Documented: ChainedCursor(nil encode) returns EndContinuation for values — same pattern as Bug 3. Real usage always has encode/decode. **$200**.
- [x] **Bug 6: `DedupCursor` drops continuation on EndCont stop** — Documented: matches Java. Pass-through of inner continuation. Real cursors provide valid continuations. **$200**.
- [x] **Bug 7: `ConcatCursors` restarts from beginning on 1st cursor OOB + EndCont** — Documented: matches Java (Java would crash with BufferUnderflowException on empty continuation). Doesn't occur with real cursors. **$200**.
- [x] **Bug 8: `FromListWithContinuation` silently ignores invalid continuation lengths** — Fixed: <4 bytes → error (matches Java's BufferUnderflowException), ≥4 bytes → reads first 4 (matches Java's ByteBuffer.getInt()). **$100**.

Test file: `pkg/recordlayer/bug_bounty3_cursor_test.go`

### Agent 5: Metadata + expressions

- [x] **Bug 9: `bindRecordTypeKeyExpressions` is shallow** — Fixed: recursive type-switch walks all expression types (Grouping, KWV, Nesting, Split, List, Function). Matches Java's recursive `KeyExpression.resolveRecordType()`. **$100**.
- [x] **Bug 10: `Build()` typeKeys map ignores `int32` explicit record type keys** — Fixed: added `int32` case to typeKeys switch. **$100**.
- [x] **Bug 11: `SplitKeyExpression.Evaluate` panics on `splitSize=0`** — Fixed: `Split()` now validates `splitSize > 0`. **$100**.
- [x] **Bug 12: `GroupingKeyExpression` allows `groupedCount > columnSize`** — Fixed: `groupingFromProto` now validates range. **$100**.
- [x] **Bug 13: Former index subspace key type changes through proto round-trip** — Fixed: `normalizeSubspaceKey()` normalizes int/int32/int64 → int64 before comparison. Applied to duplicate type key, index subspace key, and former index checks. **$200**.
- [x] **Bug 14: `RecordTypeKeyExpression.Nest()` type lost on proto round-trip** — Documented: matches Java. `concat(recordTypeKey(), X)` → `ThenKeyExpression` on deser in both Java and Go. Proto format has no RecordTypeKey+nested message. `primaryKeyStartsWithRecordType()` handles both forms. **$100**.
- [x] **Bug 15: `SetRecordCountKey` version bump uses pointer equality** — Fixed: uses `keyExpressionsEqualNilSafe()` structural comparison. **$100**.
- [x] **Bug 16: `isGroupPrefix` uses `FieldNames()` — structural info lost** — Fixed: rewritten to use `normalizeKeyForPositions` + `keyExpressionEquals` for structural comparison. Matches Java's `IndexFunctionHelper.isGroupPrefix()` which uses `KeyExpression.equals()` + `isPrefixKey()`. **$100**.
- [x] **Bug 17: `SplitKeyExpression` accepts negative `splitSize`** — Fixed: same `Split()` validation. **$100**.
- [x] **Bug 18: `ListKeyExpression` empty children lossy proto round-trip** — Fixed: `listFromProto` now accepts 0 children. Matches Java's `ListKeyExpression(RecordKeyExpressionProto.List)` constructor which also accepts empty. **$100**.
- [x] **Bug 19: Evolution validator `fmt.Sprint` confuses `int(5)` with `string("5")`** — Fixed: `subspaceKeyString()` uses `%T:%v` format (type-qualified). All `fmt.Sprint` key comparisons replaced. **$100**.
- [x] **Bug 20: `RecordMetaDataFromProto` silently drops indexes for unknown record types** — Fixed: returns error. Matches Java's `throwUnknownRecordType()`. **$100**.
- [x] **Bug 21: Global function registry has no concurrency protection** — Fixed: `sync.RWMutex` on registry. **$100**.

Test file: `pkg/recordlayer/bug_bounty3_metadata_test.go`

### Agent 2: Index maintainers

- [x] **Bug 25: `MustGet()` panic in PERMUTED_MIN/MAX delete path** — Fixed: `MustGet()` → `Get()` with proper error return. **$100**.
- [x] **Bug 26: COUNT_NOT_NULL without GroupingKeyExpression silently counts nulls** — Fixed: `Build()` validates atomic index types require `GroupingKeyExpression` root. Matches Java's `AtomicMutationIndexMaintainerFactory.getIndexValidator()`. **$100**.
- [x] **Bug 27: SUM index without GroupingKeyExpression silently produces empty index** — Fixed: same Build() validation. **$100**.
- [x] **Bug 28: MIN/MAX_EVER_LONG without GroupingKeyExpression silently produces empty index** — Fixed: same Build() validation. **$100**.
- [x] **Bug 29: `removeCommonGroupingKeys` set semantics on fan-out duplicates** — Documented: matches Java behavior. Java's `List.removeAll()` has the same set-semantics collapse for duplicate grouping keys. Known limitation in both implementations. **$100**.

### Agent 3: Store operations

- [x] **Bug 22: Reverse scan + continuation leaks version to wrong record** — Fixed: `takePendingVersion(currentPK)` now validates PK match. Stale version from continuation boundary discarded. Both unsplit and split paths fixed. **$200**.
- [x] **Bug 23: `TypedFDBRecordStore.LoadRecord` drops Version field** — Fixed: added `Version: storedRecord.Version` to struct literal. **$200**.
- [x] **Bug 24: `TypedFDBRecordStore.SaveRecord` drops Version field** — Fixed: added `Version: storedRecord.Version` to all 3 typed wrapper paths (Load, Save, SaveWithOptions). **$200**.

### Agent 4: Online indexer

- [x] **Bug 30: OnlineIndexer progress tracking undercounts filtered records** — Fixed: count ALL scanned records regardless of type filtering. Matches Java. **$100**.

### Running totals

| Agent | Bugs | $100 | $200 | Total |
|-------|------|------|------|-------|
| Cursor combinators | 8 | 3 | 5 | $1,300 |
| Metadata + expressions | 13 | 12 | 1 | $1,400 |
| Index maintainers | 5 | 5 | 0 | $500 |
| Store operations | 3 | 0 | 3 | $600 |
| Online indexer | 1 | 1 | 0 | $100 |
| **Total** | **30** | **21** | **9** | **$3,900** |

---

## Java vs Go API surface review (2026-03-16)

Full public API comparison across 5 areas. Wire-level compatibility is 100% — all gaps are API surface / feature gaps, not wire format. Go and Java can share the same FDB cluster today.

### Coverage summary

| Area | Coverage | Key Gaps |
|---|---|---|
| FDBRecordStore (CRUD) | ~83% | `preloadRecordAsync`, query planning methods, synthetic records |
| Index types | 14/19 | TEXT, BITMAP_VALUE, MULTIDIMENSIONAL, VECTOR, TIME_WINDOW_LEADERBOARD |
| IndexMaintainer interface | Core done | `scanUniquenessViolations`, `validateEntries`, `mergeIndex`, `performOperation` |
| MetaData/Schema | ~70% | toProto/fromProto (done), synthetic record types, UDFs, Views, descriptor lookups |
| Cursors/Combinators | ~53% | Intersection (done), UnorderedUnion, MapPipelined, async variants |
| ScanProperties/ExecuteProperties | ~95% | `isDryRun`, convenience clear methods |
| Continuations (wire format) | ~90% | Wire-compatible. Go writes TO_OLD, reads both TO_OLD and TO_NEW |
| FDBDatabase/Context/Runner | ~60% | Async API (by design), weak read semantics, MDC, executor control |
| Key expressions | ~80% | CollateFunctionKE, OrderFunctionKE, DimensionsKE, InvertibleFunctionKE |

### FDBRecordStore — missing public methods

- [ ] **`preloadRecordAsync()`** — Read-ahead optimization. Not applicable to Go's sync model. **LOW**.
- [ ] **`isVersionChanged()`** — Rare introspection. **LOW**.
- [ ] **`buildSingleRecord()`** — Edge case for single-record index builds. **LOW**.
- [ ] **Query planning methods** (~5 methods) — Out of scope until query planner is ported. **LOW**.

### Index API — missing methods on IndexMaintainer interface

- [ ] **`scanUniquenessViolations()` / `clearUniquenessViolations()`** — On maintainer interface (store-level `ScanUniquenessViolationsForValue` already exists). **LOW**.
- [ ] **`validateEntries()`** — Index entry validation cursor (store-level `ValidateIndex` exists). **LOW**.
- [ ] **`canDeleteWhere()` with QueryToKeyMatcher** — Go uses structural expression matching instead. **LOW**.
- [ ] **`scanRemoteFetch()`** — Experimental Java feature. **LOW**.
- [ ] **`mergeIndex()` / `performOperation()`** — Generic index operation dispatch. **LOW**.
- [ ] **`isIdempotent()` / `addedRangeWithKey()`** — Internal to Go, not on interface. **LOW**.

### Index types — 5 missing

- [ ] **TEXT index** — Tokenizer infrastructure, BunchedMap storage, BY_TEXT_TOKEN scan. Large scope. **LOW**.
- [ ] **BITMAP_VALUE index** — Bitmap position storage, BITMAP_VALUE aggregate function. **LOW**.
- [ ] **MULTIDIMENSIONAL index** — Hilbert R-tree spatial indexing. **LOW**.
- [ ] **VECTOR/HNSW index** — 4 distance metrics, RaBitQ quantization. Very large. **LOW**.
- [ ] **TIME_WINDOW_LEADERBOARD** — Sliding time window score tracking. 12+ Java classes. **LOW**.

### Index scanning — API gaps

- [ ] **`IndexScanBounds` abstraction** — Go takes `TupleRange` directly; Java has `IndexScanBounds` wrapping bounds + comparisons. **LOW**.
- [ ] **`scanIndexRecords` with record type filtering** — Go infers from metadata. **LOW**.

### MetaData — missing public methods

- [x] **`getRecordTypeForDescriptor()` / `getRecordTypeFromRecordTypeKey()`** — Added `GetRecordTypeFromRecordTypeKey()` with normalized integer comparison. Descriptor-based lookup deferred. **LOW**.
- [x] **`getIndexFromSubspaceKey()`** — Added `GetIndexFromSubspaceKey()` with normalized integer comparison. **LOW**.
- [x] **`getUnionDescriptor()` / `getUnionFieldForRecordType()`** — Added `GetUnionDescriptor()` and `GetUnionFieldForRecordType()`. Union descriptor stored during Build(). **LOW**.
- [x] **`commonPrimaryKey()` / `commonPrimaryKeyLength()` static helpers** — Added `CommonPrimaryKey()` (structural equality via keyExpressionEquals) and `CommonPrimaryKeyLength()`. **LOW**.
- [ ] **`getIndexesSince(version)` with RecordType mapping** — Go returns Index list only. **LOW**.
- [x] **`getFormerIndexesSince(version)`** — Added `GetFormerIndexesSince()`. **LOW**.
- [ ] **Builder query methods** — Getters for configured values during build (isSplitLongRecords, getVersion, etc). **LOW**.
- [ ] **`build(false)` skip-validation variant** — Go always validates. **LOW**.
- [ ] **`IndexMaintainerRegistry` pluggable** — Go dispatches from hardcoded switch. **LOW**.
- [ ] **Synthetic record types** — JoinedRecordType, UnnestedRecordType. Large feature. **LOW**.
- [ ] **User-defined functions** — `PUserDefinedFunction` in MetaData proto. **LOW**.
- [ ] **Views** — `PView` in MetaData proto. **LOW**.

### RecordType — missing getters

- [x] **`getIndexes()` / `getMultiTypeIndexes()` / `getAllIndexes()`** — Added `GetIndexes()`, `GetMultiTypeIndexes()`, `GetAllIndexes()` on RecordType. **LOW**.
- [x] **`hasExplicitRecordTypeKey()` / `getRecordTypeKeyTuple()`** — Added `HasExplicitRecordTypeKey()`. Key already accessible via `GetRecordTypeKey()`. **LOW**.
- [ ] **`isSynthetic()`** — No synthetic record support yet. **LOW**.

### Cursor — missing combinators & methods

- [ ] **`UnorderedUnionCursor`** — Union without order preservation. **LOW**.
- [ ] **`MapPipelinedCursor`** — Async pipelined map (no Go equivalent of CompletableFuture). **LOW**.
- [ ] **`filterAsync()`** — Pipelined async filter. Not applicable to Go's sync model. **LOW**.
- [ ] **`mapEffect()` / `mapContinuation()`** — Side-effect map, continuation rewriting. **LOW**.
- [ ] **`forEachResult()` / `forEachAsync()`** — Result-level iteration. **LOW**.
- [ ] **`reduce()` with stop condition** — Conditional reduction. **LOW**.
- [ ] **`AggregateCursor`** — Accumulator-based aggregation. **LOW**.
- [ ] **`ComparatorCursor`** — Custom comparison ordering. **LOW**.
- [ ] **`ProbableIntersectionCursor`** — Bloom filter intersection. **LOW**.
- [ ] **`SizeStatisticsGroupingCursor`** — Key/value size tracking. **LOW**.
- [ ] **`RecordCursorVisitor` pattern** — Cursor tree inspection. **LOW**.
- [x] **`RecordCursorResult.Map()` / `WithContinuation()`** — Added `MapResult[T,R]()` standalone function + `WithContinuation()` method. **LOW**.
- [ ] **`isClosed()` on cursor** — Closure state check. **LOW**.

### ExecuteProperties — missing features

- [ ] **`isDryRun` flag** — Dry-run execution mode. **LOW**.
- [x] **Convenience clear methods** — `ClearRowAndTimeLimits()`, `ClearSkipAndLimit()`, `WithScannedRecordsLimit()`, `WithScannedBytesLimit()`, `WithSkip()`. **LOW**.

### FDBDatabase — missing methods

- [ ] **`openContext()` (6 overloads)** — Go uses Run()/RunWithVersionstamp() exclusively. **LOW**.
- [ ] **`performNoOp()` / `performNoOpAsync()`** — No-op transaction testing. **LOW**.
- [ ] **`clearCaches()` / `close()`** — Cache/lifecycle management. **LOW**.
- [ ] **`FDBDatabaseFactory`** — Database pooling. **LOW**.
- [ ] **`setDatacenterId()` / `getLocalityProvider()`** — Datacenter affinity. **LOW**.

### FDBRecordContext — missing methods

- [ ] **`getConfig()` / `getTransactionId()` / `getTimeoutMillis()`** — Context introspection. **LOW**.
- [ ] **`getTransactionAge()`** — Transaction timing. **LOW**.
- [ ] **`getCommitCheck()` / `removeCommitChecks()`** — Hook management post-add. **LOW**.
- [ ] **`removePostCommit()` / `addPostCloseHook()`** — Hook removal. **LOW**.
- [ ] **`WeakReadSemantics`** — Causal read risky / version staleness bounds. **LOW**.
- [ ] **`getMdcContext()`** — Mapped diagnostic context. **LOW**.

### FDBDatabaseRunner — missing methods

- [ ] **`runAsync()` (5 overloads)** — Go is sync only. **LOW**.
- [ ] **Timer/MDC/WeakReadSemantics getters/setters** — **LOW**.

### Key expressions — 5 missing types

- [ ] **`CollateFunctionKeyExpression`** — Locale-aware string sorting. **LOW**.
- [ ] **`OrderFunctionKeyExpression`** — Custom sort order functions. **LOW**.
- [ ] **`DimensionsKeyExpression`** — Multidimensional indexing. **LOW**.
- [ ] **`InvertibleFunctionKeyExpression`** — Bidirectional function evaluation. **LOW**.
- [ ] **`AtomKeyExpression`** — Atom-level expressions. **LOW**.

### OnlineIndexer — missing config options

- [ ] **`setIndexStatePrecondition()`** — State pre-check. **LOW**.
- [ ] **`setTimeLimitMillis()`** — Per-batch time limits. **LOW**.
- [ ] **`setCommitCheckIntervalCount()`** — **LOW**.
- [ ] **`setMaxWriteRetries()`** — Handled implicitly via FDBDatabaseRunner. **LOW**.
- [ ] **`setDesiredRecordsPerSecond()`** — Rate limiting. **LOW**.
- [ ] **`addStatisticsCollector()`** — Statistics collection. **LOW**.

### Convenience methods — not implemented

- [ ] **`getRecordCount()` / `getRecordCount(recordTypeName)`** — Java convenience wrappers around `evaluateAggregateFunction`. **LOW**.
- [x] **`Index.getBooleanOption(key, default)`** — Added `GetBooleanOption()`. **LOW**.
- [ ] **`IndexAggregateFunction` constructor helpers** — Go requires manual struct construction. **LOW**.

### Design differences (intentional, not gaps)

These are architectural decisions, not bugs:

- **Async → Sync**: Java uses `CompletableFuture`; Go uses sync + `context.Context`. All pipelined/async cursor variants are N/A.
- **Executor control**: Java exposes thread pools; Go uses goroutines. N/A.
- **Builder query methods**: Java exposes getters on builders; Go uses direct struct fields. Functional equivalent.
- **`RecordCursor` interface width**: Java has 20+ default methods; Go has 2 (OnNext, Close) + standalone generic functions. Same functionality, different ergonomics.

---

## Remaining work buckets (2026-03-11 assessment)

**A. Huge features** — TEXT index (Lucene-style), query planner, synthetic record types. Each is weeks of work.

**B. Niche index types** — BITMAP_VALUE, MULTIDIMENSIONAL, VECTOR. Not needed day one. (~~PERMUTED_MIN/MAX~~, ~~MAX_EVER_VERSION~~ done.)

**C. Polish** — ~~Timer/instrumentation~~, ~~store state caching~~, ~~dead code removal~~, CursorLimitManager refactor, API cleanup. Important for production but not feature-blocking.

- [x] **[MEDIUM] Store state caching** — `MetaDataVersionStampStoreStateCache` + `PassThroughRecordStoreStateCache`. Validates via `\xff/metadataVersion` versionstamp, handles dirty state, read conflicts on cache hit, proto.Clone on cache-hit path, LRU+TTL eviction. 40 specs, 2.2x benchmark speedup. Files: `store_state_cache.go`, `store_state_cache_test.go`.
- [ ] **[LOW] `FDBDatabase.storeStateCache` field unsynchronized** — Interface field on `FDBDatabase` is not protected by mutex or `atomic.Value`. Safe as long as it's set-once-at-startup before any transactions. If runtime reconfiguration is needed, wrap in `atomic.Value`.
- [ ] **[LOW] TOCTOU duplicate FDB reads on concurrent cache miss** — Two goroutines can miss the cache simultaneously and both load from FDB. Same behavior as Java (Guava cache). Harmless — both writes are idempotent and `addToCache` keeps the newer versionstamp.
- [ ] **[LOW] O(n) LRU eviction scan in store state cache** — `evictIfNeeded()` iterates all entries under mutex. Max 500 entries (default), so bounded. Replace with container/heap if profiling shows contention.
- [x] **[LOW] Dead code removal** — 5-agent parallel scan of entire codebase. Removed 7 items: 2 unused constants (`maxParallelIndexRebuild`, `preloadCacheSize`), 2 unused type aliases (`RecordCursorProto`, `TypedRecordCursor`), 1 unused utility function (`MapErr` in cursor_util.go), 2 dead accessor methods (`GetWholeKey`, `GetRecordTypeIndex` — fields accessed directly). Kept `SetAllowMissingFormerIndexNames`/`SetAllowNoSinceVersion` (Java API surface, wired into validation).

**Next high-value target**: VERSION index — DONE (Phase 1 + Phase 2). Conformance tests remaining.

**D. Build tooling**
- [x] **Add stdlib nogo analyzers** — Added 13 new analyzers (appends, deepequalerrors, defers, directive, errorsas, ifaceassert, nilness, shadow, sigchanyzer, sortslice, stringintconv, timeformat, waitgroup). 20 → 33 total. Zero new findings — codebase was already clean.
- [x] **Add staticcheck to nogo** — All 90 SA analyzers wired into nogo via individual deps on `honnef.co/go/tools` v0.6.1. Uses `_base` config with `only_files` for workspace packages. Disabled: `shadow` (noisy, err shadowing is idiomatic Go), `loopclosure` (Go 1.22+ fixed). Excluded: SA1019 on `metadata_proto.go` (intentional deprecated field use), SA5011 on test files (doesn't understand t.Fatal guards). Fixed: 2 tautological nil comparisons (cursor.go), 6 unused assignments (test files).

---

## Production readiness

### HIGH

- [x] **API surface polish (Phase 1+2)** — RFC 003 Option B executed: unexported 11 concrete index maintainer types, RankedSet/RankedSetConfig, SizeInfo, all format version constants, split/size constants. Added `RankQuerier` interface for chaos test package access. `IndexMaintainer` interface and `RangeSet` kept exported (public API returns / conformance tests). 37 files, ~400 lines changed. Subspace constants kept exported for debugging.
- [ ] **Performance testing under real workloads** — Benchmark key operations (bulk inserts, index-heavy saves, large scans with continuations, split record read/write, OnlineIndexer throughput) under realistic data volumes. Profile hotspots. Compare with Java Record Layer on equivalent workloads where possible.
- [ ] **Edge case hardening** — Systematic audit of behaviors at boundaries: max key/value sizes, transaction size limits (10MB), 5-second transaction timeout under load, empty stores, nil/zero-value inputs, corrupt or truncated data, concurrent schema evolution, partial failures mid-commit. The kind of bugs that only surface in production.
- [x] **Chaos testing** — Model-based framework at `pkg/recordlayer/chaos/`. ChaosTransactor injects commit-unknown/conflict/timeout faults. 91 tests: 71 targeted (per-index-type fault injection), 15 random (seeded PRNG, up to 2000 ops), 5 concurrent (multi-goroutine contention). Covers VALUE/COUNT/SUM/RANK/PERMUTED/VERSION/COVERING indexes. Found and fixed 1 bug (PERMUTED group membership change). Remaining: network partition simulation, OOM during scans, interrupted index builds.

---

## Build & CI

### MEDIUM

- [x] **FDB client version mismatch between CI and testcontainers** — Bumped CI to 7.3.46 matching testcontainers default.

### LOW

- [x] **CI missing `go mod verify` and format checks** — Added `go mod verify`, `gofmt -l`, and Gazelle drift detection steps.
- [x] **CI missing Gazelle drift detection** — Added to CI build job (runs gazelle, checks git diff).
- [x] **Justfile missing `fmt` and `coverage` targets** — Added `just fmt` and `just coverage`.

---

## Test quality improvements

### MEDIUM

- [ ] **~25 implementation files lack dedicated unit tests** — Core files like `cursor.go`, `ranked_set.go`, `split_helper.go`, `database.go`, `key_expression.go`, `key_value_cursor.go`, `index_maintainer.go`, `scan_properties.go`, and various index maintainers (`count_index_maintainer.go`, `version_index_maintainer.go`, `rank_index_maintainer.go`, etc.) have no `_test.go` counterparts. They're exercised indirectly via integration tests, but direct unit tests would catch regressions faster and document expected behavior at the unit level.
- [x] **Brittle string-matching error assertions in tests** — Migrated 63 assertions total across 22 test files from `.Error().To(ContainSubstring(...))` to typed `errors.As()` + struct field checks. Round 1: 35 assertions (13 error types). Round 2: 28 assertions (new `KeyExpressionError` type + `MetaDataError` for metadata_proto.go). 31 remaining are genuine internal validation (`fmt.Errorf` with no Java exception mapping).
- [x] **Temp file leak in test suite setup** — Fixed: cleanup in `SynchronizedAfterSuite` via package-level `clusterTmpFilePath` variable.

### LOW

- [x] **Missing cursor combinator edge case tests** — 10 in-memory tests: empty Concat/Union/Intersection, FilterCursor rejects all, filter continuation under limit, MapErrCursor error propagation, LimitRows(0), Skip past all, deep composition Filter(Map(Limit(FromList))), ConcatCursors ordering.
- [x] **Missing continuation token stability tests** — 5 tests: resume after record deletion (skips deleted), resume after insertion (sees new records past cursor), resume index scan after deletion, resume after DeleteAllRecords + re-insert, resume index scan after rebuild. All cross-transaction.
- [x] **Missing schema evolution edge case tests** — 10 tests: multi-version jump (v1→v5) with intermediate index, new index with stale version, combined add+remove with FormerIndex, addedVersion change, lastModifiedVersion decrease, safe type promotions (int32→int64, sint32→sint64, narrowing, cross-type, identity). 44 total evolution validator tests.

---

## Future: Query planner + SQL layer

**Not started. Blocked on: core must be rock solid first.**

Port the full query infrastructure from Java, then the relational/SQL layer on top.

### Phase 1: Cascades query optimizer (~104K lines Java)

The Cascades framework (Graefe 1995) is the cost-based query optimizer — 494 files, 40% of core by itself. Turns logical queries into optimized physical execution plans (index selection, join ordering, predicate pushdown, etc.).

- [ ] **Cascades optimizer framework** — `query/plan/cascades/` — rule-based exploration of equivalent plans, cost estimation, memo structure
- [ ] **Physical plan implementations** — `query/plan/plans/` (74 files, 19K lines) — RecordQueryPlan nodes (index scan, filter, union, intersection, sort, aggregate, etc.)
- [ ] **Query expressions** — `query/expressions/` (35 files, 9K lines) — predicates, comparisons, logical operators for query specification
- [ ] **Planning infrastructure** — `query/plan/planning/` — plan generation, property derivation
- [ ] **Synthetic record planner** — `query/plan/synthetic/` (11 files, 2K lines) — joined/unnested record plan generation
- [ ] **Bitmap plans** — `query/plan/bitmap/` — bitmap index scan plans
- [ ] **Sort plans** — `query/plan/sorting/` — external sort, in-memory sort
- [ ] **Explain** — `query/plan/explain/` — plan visualization/debugging

### Phase 2: Prerequisites from core

- [ ] **Joined record types** — `SyntheticRecordType`, `JoinedRecordType`, `UnnestedRecordType` — virtual records composed from constituents via equi-joins
- [ ] **KeySpace directory layer** — `provider/fdb/keyspace/` (25 files, 7K lines) — hierarchical key management
- [ ] **TEXT index** — full-text search with tokenization
- [ ] **Remaining key expression types** — ~10 unported expression types from `metadata/expressions/`

### Phase 3: Relational / SQL layer (~55K lines Java)

Separate module (`fdb-relational-core` + `fdb-relational-api`). Compiles SQL to RecordLayer query plans.

- [ ] **SQL parser** — SQL AST (`structuredsql/`)
- [ ] **SQL → plan compiler** — `recordlayer/query/` — translates SQL AST to Cascades logical plans
- [ ] **Schema catalog** — `recordlayer/catalog/` — DDL → RecordMetaData mapping, system tables, stored in FDB
- [ ] **Type system** — SQL types ↔ protobuf types mapping
- [ ] **gRPC server** — `fdb-relational-grpc/` + `fdb-relational-server/`

### Phase 4: `database/sql` driver

Go `database/sql` compatible driver. Any Go app using `database/sql` (ORMs, migration tools, existing codebases) just works — swap your Postgres DSN for an FDB one. Wire-compatible with Java JDBC driver: a Java app and a Go app can read/write the same tables in the same FDB cluster simultaneously.

- [ ] **`database/sql` driver registration** — `sql.Register("fdb", ...)`, DSN parsing
- [ ] **`driver.Conn` / `driver.Tx`** — map to `FDBRecordContext` transactions (5s limit awareness)
- [ ] **`driver.Rows`** — cursor-backed result sets with continuation support
- [ ] **`driver.Stmt`** — prepared statements → Cascades plan cache
- [ ] **Query parameter binding** — `?` placeholders → plan parameterization
- [ ] **DDL passthrough** — `CREATE TABLE` / `ALTER TABLE` / `CREATE INDEX` via schema catalog
- [ ] **Type mapping** — Go `sql.Scanner`/`driver.Valuer` ↔ protobuf ↔ FDB tuple types

### Size estimates

| Component | Java files | Java lines | Notes |
|---|---|---|---|
| Cascades optimizer | 494 | 104K | Biggest single chunk |
| Plan implementations | 74 | 19K | Physical execution nodes |
| Query expressions | 35 | 9K | Predicates, comparisons |
| Planning + other | 43 | 15K | Infra, bitmap, sort, explain |
| Relational core | 233 | 41K | SQL→plan compiler |
| Relational API | 88 | 13K | Interfaces, types |
| Relational server/JDBC/gRPC | 31 | small | Thin wrappers |
| **Total** | **~1000** | **~200K** | |

---

## Documentation cleanup

### LOW

- [x] **PORT.md** — Comprehensive porting assessment with subsystem ratings, test coverage, conformance matrix. Updated 2026-03-09.
- [x] **Clean up PHASE1_TEST_GAPS.md** — Deleted stale file.
- [x] **Clean up FDB_CONFLICT_DETECTION.md** — Deleted stale file.
