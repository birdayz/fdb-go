# fdb-record-layer-go TODO

Actionable work items for the Go port of Apple's FoundationDB Record Layer.
Severity: **CRITICAL** = blocks correctness/compatibility, **HIGH** = important for production quality, **MEDIUM** = improvement, **LOW** = nice-to-have.

Conformance audit performed 2026-03-08 comparing Go implementation method-by-method against Java source at `fdb-record-layer/`. Coverage: ~28% of Java FDBRecordStore API surface (40/144 public methods).

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
- [x] Store state validation — StoreLockState.FORBID_RECORD_UPDATE check
- [x] Split records — saveWithSplit/loadWithSplit/deleteSplit, 100KB chunks, cursor reassembly
- [x] Secondary indexes (VALUE) — StandardIndexMaintainer, unique enforcement, common-entry skip
- [x] Index maintenance — auto-update on Save/Delete/DeleteAllRecords
- [x] Continuation token protobuf wrapping — magic number 6773487359078157740
- [x] Bulk operations — DeleteAllRecords, GetRecordCount/GetSnapshotRecordCount
- [x] Bazel 8 migration — MODULE.bazel, gazelle, nogo (20 analyzers)
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
   - [ ] Indexing stamp at `[9, indexSubspaceKey, 2]` — proto `IndexBuildIndexingStamp` for resume detection. Not yet implemented.

5. **rebuildIndex on store** (HIGH — needed for store.Open with new indexes) ✅
   - [x] `FDBRecordStore.RebuildIndex(index)` — clears index data, marks WRITE_ONLY, pre-marks full range in RangeSet, scans all records inline, re-indexes, marks READABLE. Single-transaction path matching Java's `IndexingBase.rebuildIndexAsync()`.
   - [x] 8 tests: basic VALUE index, empty store, stale cleanup, type filtering, range set completion, unique index, uniqueness violation, post-rebuild maintenance.
   - [x] `CreateOrOpen` auto-rebuild: `checkPossiblyRebuild()` compares stored metadata version with current. Uses `GetIndexesToBuildSince(oldVersion)` to find new indexes. Rebuilds inline and updates store header. Matches Java's `FDBRecordStore.checkPossiblyRebuild()`.
   - [x] `addIndexCommon()` on builder: sets `LastModifiedVersion` and `AddedVersion` matching Java's `RecordMetaDataBuilder.addIndexCommon()`. Bumps builder version on each index add.
   - [x] 7 additional tests: version tracking on AddIndex, pre-set version preserved, GetIndexesToBuildSince, auto-rebuild single index, no rebuild on same version, store header version updated, multi-index auto-rebuild.

6. **OnlineIndexer — BY_INDEX strategy** (MEDIUM — optimization, not essential)
   - [ ] Build new index from existing readable index instead of scanning all records.
   - [ ] Uses source index's `ScanIndexRecords` → update target index.
   - [ ] Range tracking uses source index entry keys instead of primary keys.
   - [ ] Validation: source must be READABLE VALUE index, no duplicates.

7. **Multi-target index building** (LOW — optimization for bulk schema changes)
   - [ ] Build multiple WRITE_ONLY indexes in a single record scan pass.
   - [ ] All target indexes share the same missing-range tracking (first index's RangeSet).

8. **Mutual/concurrent index building** (LOW — multi-process coordination)
   - [ ] Multiple OnlineIndexer processes build different ranges concurrently.
   - [ ] Heartbeat tracking at `[9, indexSubspaceKey, 7, uuid]`.
   - [ ] `requireEmpty=true` prevents double-processing of ranges.

9. **Conformance tests** (CRITICAL — must validate wire compat)
   - [x] Go saves records + Go rebuilds index → Java scans → entries match.
   - [x] Go saves records + Java rebuilds index → Go scans → entries match.
   - [x] Java saves records + Go rebuilds index → Java scans → entries match.
   - [x] Cross-rebuild: Go rebuild and Java rebuild produce identical entries.
   - [ ] Go writes WRITE_ONLY records while Java builds → entries consistent.
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
- [ ] **Index types beyond VALUE/COUNT/SUM/MIN_EVER/MAX_EVER** — Java has more types: COUNT_UPDATES, COUNT_NOT_NULL, MIN_EVER_TUPLE, MAX_EVER_TUPLE, RANK, TIME_WINDOW_LEADERBOARD, VERSION, TEXT, BITMAP_VALUE, PERMUTED_MIN/MAX, MULTIDIMENSIONAL, VECTOR.

- [x] **Uniqueness violation tracking** — `ScanUniquenessViolations()` scans `IndexUniquenessViolationsKey` (7) subspace. `ResolveUniquenessViolation()` removes a single entry. Violations written on unique index save failure.

- [x] **Index validation** — `ValidateIndex()` scans all records and index entries to detect orphaned entries (in index but not in records) and missing entries (in records but not in index).

- [x] **Primary key component deduplication** — `primaryKeyComponentPositions` computed at `Build()` time via `buildPrimaryKeyComponentPositions()`. `indexEntryKey()` calls `trimPrimaryKey()` to omit PK components already in the index key. `getEntryPrimaryKey()` reconstructs the full PK on read. Wire-compatible with Java. Conformance-tested: Go writes → Java scans, Java writes → Go scans, cross-write. 3 conformance specs + 15 unit tests.

- [x] **Bulk index delete** — `DeleteIndexEntries()` clears all entries for a given index. `DeleteIndexEntriesInRange()` clears entries within a tuple range.

- [x] **Aggregate functions via indexes** — `EvaluateAggregateFunction()` on store with auto-index-selection. Supports COUNT, SUM, MIN_EVER, MAX_EVER via atomic mutation indexes. `IndexAggregateFunction` type with name, operand, optional explicit index. `canEvaluateAggregate()` / `isGroupPrefix()` for index matching. Function name constants: `FunctionNameCount`, `FunctionNameSum`, `FunctionNameMinEver`, `FunctionNameMaxEver`, `FunctionNameMin`, `FunctionNameMax`. 11 tests.

---

## Metadata — conformance gaps

### HIGH

- [x] **ThenKeyExpression** — `CompositeKeyExpression` via `Concat()` now computes Cartesian cross-product matching Java's `ThenKeyExpression` semantics.

- [x] **NestingKeyExpression** — `Nest("field", child)` navigates into nested message fields. `NestFanOut("field", child)` for repeated message fields. Composite nested fields work (e.g., `Nest("flower", Concat(Field("type"), Field("color")))`). Enum fields supported via `int64` conversion.

- [x] **FormerIndex tracking** — `FormerIndex` struct with `SubspaceKey`, `AddedVersion`, `RemovedVersion`, `FormerName`. `RemoveIndex()` on builder creates FormerIndex and removes from all record types. `Build()` validates no subspace key reuse. `GetFormerIndexes()` on metadata.

- [ ] **Schema evolution validation** — Java has `MetaDataEvolutionValidator` for old→new schema changes (field drops, type changes, index changes). Go has `MetaDataValidator` checks at Build time (duplicate type keys, duplicate subspace keys, PK fan-out, former index version ordering) but no evolution validation.

### MEDIUM

- [x] **Metadata proto serialization** — Java has `toProto()`/`fromProto()` for persisting metadata definitions. Implemented in Go.
  - [x] **KeyExpression proto serialization** — `ToKeyExpression()` on all expression types + `KeyExpressionFromProto()` dispatcher. Roundtrip + wire format tests. Matches Java's `KeyExpression.toKeyExpression()`/`fromProto()`. FanType mapping: Go None→SCALAR, FanOut→FAN_OUT, Concatenate→CONCATENATE.
  - [x] **RecordMetaData.toProto()/fromProto()** — `ToProto()` serializes metadata (file descriptor, dependencies, indexes with record type associations, record types with primary keys, former indexes, flags). `RecordMetaDataFromProto()` rebuilds from proto with topological dependency resolution. Index subspace keys tuple-packed. Explicit record type keys via Value proto. Wire roundtrip tested.

- [x] **Explicit record type keys** — `SetRecordTypeKey()` on `RecordTypeBuilder`, `GetRecordTypeKey()` on `RecordType`. Falls back to `RecordTypeIndex` if not set.

- [x] **Multi-type indexes** — `AddMultiTypeIndex(recordTypeNames, index)`. 0 types → universal, 1 type → single-type, 2+ types → multi-type (stored per RecordType, included in `GetIndexesForRecordType`). Matches Java semantics.

- [x] **Schema evolution version tracking** — `SetVersion()` on builder sets metadata version. Used in store header for compatibility tracking.

- [x] **Primary key prefix checking** — `PrimaryKeyHasRecordTypePrefix()` on `RecordMetaData`. Checks all record types' primary keys start with `RecordTypeKeyExpression`, including through `CompositeKeyExpression`.

### LOW

- [ ] **Missing key expression types** — 15+ types not in Go: VersionKeyExpression, FunctionKeyExpression, LongArithmeticFunctionKeyExpression, OrderFunctionKeyExpression, CollateFunctionKeyExpression, DimensionsKeyExpression, LiteralKeyExpression, SplitKeyExpression, InvertibleFunctionKeyExpression, ListKeyExpression, etc. (GroupingKeyExpression done.)

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

- [ ] **CursorLimitManager** — Java has a separate class for comprehensive limit tracking (record scan, byte scan, time). Go has inline limit logic in keyValueCursor.

- [x] **RecordCursor instance methods** — `First()`, `GetCount()`, `Reduce()` as standalone generic functions. `SkipCursor()`, `LimitRowsCursor()` as cursor wrappers. Matches Java's `first()`, `getCount()`, `reduce()`, `skip()`, `limitRowsTo()`.

### LOW

- [ ] **Visitor pattern** — Java has `RecordCursorVisitor` interface for cursor inspection/instrumentation.
- [x] **Continuation SerializationMode** — Go uses TO_OLD (raw bytes) for writing, accepts both TO_OLD and TO_NEW (proto-wrapped) for reading. Matches Java Record Layer 4.2.6.0 which only supports TO_OLD.

---

## Store — conformance gaps

### HIGH

- [x] **Store state management** — `GetRecordStoreState()` returns store header + index states. `SetStoreLockState()` persists lock state to header. `ReloadRecordStoreState()` forces reload from FDB.

- [ ] **Query execution methods** — Java has `evaluateIndexRecordFunction()`, `evaluateStoreFunction()`, `evaluateAggregateFunction()`. Go has none.
  - [x] `CountRecords(ctx, low, high, lowEndpoint, highEndpoint, continuation, scanProperties)` — scan-based record count (not atomic counter). Matches Java's `FDBRecordStore.countRecords()`.

- [x] **Per-type record count** — `GetSnapshotRecordCountForRecordType(recordTypeName)` added. Requires `RecordTypeKeyExpression` as count key. Matches Java's `getSnapshotRecordCountForRecordType()`.

### MEDIUM

- [x] **Store statistics** — `EstimateStoreSize()` and `EstimateRecordsSize()` using FDB `GetEstimatedRangeSizeBytes()`.

- [x] **Format version / user version access** — `GetFormatVersion()`, `GetUserVersion()`, `SetUserVersion()`, `GetMetaDataVersion()`. Persisted in store header.

- [x] **Serializer access** — `GetMetaData()`, `GetIndexMaintainer()` on store. `Context()` and `Subspace()` already exposed.

- [ ] **Conformance test for type-changed existence check** — `conformance/existence_check_conformance_test.go` covers 4 of 5 modes. Add Java cross-validation for `ERROR_IF_RECORD_TYPE_CHANGED`.

### LOW

- [ ] **Advanced store operations** — Java has `dryRunSaveRecordAsync()`, `preloadRecordAsync()`, `repairRecordKeys()`. Go has none.

- [ ] **Synthetic records** — Java has `loadSyntheticRecord()`. Large feature tied to synthetic record types.

---

## Database / Transaction — conformance gaps

### HIGH

- [x] **FDBDatabaseRunner** — `FDBDatabaseRunner` with `MaxAttempts`, `InitialDelay`, `MaxDelay`, exponential backoff. `RunWithRetry()` wraps transaction execution with configurable retry. Falls back to FDB's native retry when config is nil.

- [x] **FDBRecordContextConfig** — `RecordContextConfig` with `TransactionTimeout`, `Priority`, `TransactionID`. Applied in `Run()`/`RunWithRetry()`.

- [x] **Commit hooks** — `AddCommitCheck()` for pre-commit consistency checks, `AddPostCommit()` for post-commit callbacks. Run in `flushAndCommit()`. Matches Java's `CommitCheckAsync` and `PostCommit` interfaces.

### MEDIUM

- [ ] **Timer / instrumentation** — Java has comprehensive `FDBStoreTimer` with event counters and timing throughout all operations. Go has no instrumentation.

- [x] **Transaction priority** — `TransactionPriority` type with `PriorityDefault`, `PriorityBatch`, `PrioritySystemImmediate`. `SetTransactionPriority()` on `FDBRecordContext`.

- [ ] **Store state caching** — Java has `FDBRecordStoreStateCache` to avoid redundant header reads. Go loads state on demand without caching.

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

- [ ] **RecordCursor interface too wide (5 methods)** — `Seq`, `Seq2`, `SeqWithContinuation` have identical implementations across 10+ cursor types, creating ~500 lines of duplicated boilerplate. Should slim to 2 methods (`OnNext` + `Close`) and make `Seq`/`Seq2`/`SeqWithContinuation` free functions. Follows `io.Reader` + `io.ReadAll` pattern.

- [ ] **Panics in library code** — `FDBRecordVersion` has 5 panics (`GetGlobalVersion`, `GetDBVersion`, `Next`, `Prev`, `ToVersionstamp`). `RecordCursorResult.GetValue()` panics on `!HasNext()`. Library code should return errors, never panic.

### MEDIUM

- [ ] **sync.Map misuse in FDBRecordContext** — `localVersionCache` and `versionMutations` use `sync.Map` but `FDBRecordContext` wraps a single-threaded FDB transaction. Plain `map` would be faster and simpler. `HasVersionMutations()` iterates the entire map just to check emptiness.

- [ ] **Silent error swallowing in addRecordCount** — `record_count.go:64` swallows key expression evaluation errors. Java logs but doesn't fail either, but Go should at minimum log.

- [ ] **recover() catches all panics in key_value_cursor.go** — `nextKV()` uses `recover()` to defend against FDB `RangeIterator.Get()` panic, but catches ALL panics including nil pointer dereferences and OOB indexing, making bugs invisible.

- [ ] **store.go too large (1736 lines)** — Should split into `store.go` (CRUD), `store_builder.go` (builder/lifecycle), `store_typed.go` (TypedFDBRecordStore), `store_version.go` (version management).

- [ ] **cursor.go too large (1514 lines)** — Should split into `cursor.go` (interface/result), `cursor_combinators.go` (filter/skip/limit), `cursor_util.go` (AsList/First/Reduce/ForEach).

- [ ] **NewRecordMetaData discards Build() error** — `metadata.go:534` does `md, _ := builder.Build()`, returns nil on invalid schema with no indication. Should return error or be removed.

### STYLE (LOW)

- [ ] **Get prefix on ~30 trivial accessors** — `GetRecordType()`, `GetIndex()`, `GetValue()`, `GetContinuation()`, etc. Go convention: drop `Get` for simple field reads.

- [ ] **interface{} → any** — ~15 uses of `interface{}` should use modern `any` alias.

---

## Split records — conformance status

**FULLY CONFORMANT.** No gaps found. All constants, save/load/delete/exists methods, SizeInfo tracking, cursor integration, and edge cases match Java behavior. Wire-compatible. Version handling separated into store layer (architectural choice, not a gap).

---

## Infrastructure

- [x] Bazel migration, nogo linting, CI pipeline, justfile — all done
- [ ] **KeySpace/KeySpacePath** — Enterprise key management. LOW priority.
- [x] **ScanLimiter** — TimeScanLimiter, ByteScanLimiter, RecordScanLimiter all enforced in both `keyValueCursor` and `indexCursor`. Time limit uses free initial pass (first record always succeeds). Continuation returned for cross-transaction resumption.

---

## Documentation cleanup

### LOW

- [x] **PORT.md** — Comprehensive porting assessment with subsystem ratings, test coverage, conformance matrix. Updated 2026-03-09.
- [x] **Clean up PHASE1_TEST_GAPS.md** — Deleted stale file.
- [x] **Clean up FDB_CONFLICT_DETECTION.md** — Deleted stale file.
