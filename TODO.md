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

### MEDIUM

- [x] **FDBRecordVersion missing Equal/Less** — Fixed: Added `Equal()`, `Less()`, `String()` methods matching Java's `equals()`/`compareTo()`/`toString()` semantics.

---

## Indexing — conformance gaps

### CRITICAL

- [x] **Index scanning** — `IndexMaintainer.Scan()` and `FDBRecordStore.ScanIndex()` return `RecordCursor[*IndexEntry]` with `TupleRange` support (ALL, AllOf, Between, BetweenInclusive), continuations, row/byte limits, forward/reverse. `IndexEntry.PrimaryKey()` and `IndexValues()` for key extraction.

- [x] **Index state management** — 4 states: `READABLE`, `WRITE_ONLY`, `DISABLED`, `READABLE_UNIQUE_PENDING`. Stored in `IndexStateSpaceKey` (5) subspace as tuple-packed int64. Loaded on store Open/CreateOrOpen. `MarkIndexReadable`, `MarkIndexWriteOnly`, `MarkIndexDisabled`, `ClearAndMarkIndexWriteOnly`. DISABLED indexes skip maintenance. Non-scannable indexes reject ScanIndex. Matches Java's wire format and semantics.

- [ ] **Index build support** — Java has `updateWhileWriteOnly`, `isIdempotent`, `addedRangeWithKey`, RangeSet tracking for online builds. Go has none. Cannot build indexes on existing data.

### HIGH

- [x] **Index management store methods** — `GetIndexState`, `IsIndexReadable`, `IsIndexWriteOnly`, `IsIndexDisabled`, `IsIndexScannable`, `MarkIndexReadable`, `MarkIndexWriteOnly`, `MarkIndexDisabled`, `ClearAndMarkIndexWriteOnly`. Still missing: `rebuildIndex`, `getIndexBuildStateAsync`, `markIndexReadableOrUniquePending`.

- [x] **Repeated field fan-out** — `FanOut("field")` creates `FieldKeyExpression` with `FanTypeFanOut`, producing one index entry per repeated value. Cross-product with `Concat()` works. Empty repeated field → no entries (matching Java).

- [x] **Sparse/filtered indexes** — `Index.Predicate` field: function that returns true if a record should be indexed. `StandardIndexMaintainer` skips entries when predicate returns false. Matches Java's `IndexPredicate` concept.

### MEDIUM

- [ ] **Index types beyond VALUE** — Java has 15+ types: COUNT, COUNT_UPDATES, COUNT_NOT_NULL, SUM, MIN_EVER_TUPLE/LONG, MAX_EVER_TUPLE/LONG, RANK, TIME_WINDOW_LEADERBOARD, VERSION, TEXT, BITMAP_VALUE, PERMUTED_MIN/MAX, MULTIDIMENSIONAL, VECTOR. Go only has VALUE.

- [x] **Uniqueness violation tracking** — `ScanUniquenessViolations()` scans `IndexUniquenessViolationsKey` (7) subspace. `ResolveUniquenessViolation()` removes a single entry. Violations written on unique index save failure.

- [x] **Index validation** — `ValidateIndex()` scans all records and index entries to detect orphaned entries (in index but not in records) and missing entries (in records but not in index).

- [ ] **Primary key component deduplication** — Java's `primaryKeyComponentPositions` tracks overlap between PK and index key to avoid redundant storage. Go always appends full PK. **Wire-incompatible** when Java trims PK components from index entries — Go and Java produce different index entry keys. Only triggers when index key expression overlaps with primary key (e.g., index on `Field("name")` with PK `Concat(RecordTypeKey(), Field("name"))`).

- [x] **Bulk index delete** — `DeleteIndexEntries()` clears all entries for a given index. `DeleteIndexEntriesInRange()` clears entries within a tuple range.

- [ ] **Aggregate functions via indexes** — Java has `canEvaluateAggregateFunction()`/`evaluateAggregateFunction()` for COUNT, MIN, MAX, SUM via index maintainers. Go's COUNT is via store atomic mutations, not indexes.

---

## Metadata — conformance gaps

### HIGH

- [x] **ThenKeyExpression** — `CompositeKeyExpression` via `Concat()` now computes Cartesian cross-product matching Java's `ThenKeyExpression` semantics.

- [x] **NestingKeyExpression** — `Nest("field", child)` navigates into nested message fields. `NestFanOut("field", child)` for repeated message fields. Composite nested fields work (e.g., `Nest("flower", Concat(Field("type"), Field("color")))`). Enum fields supported via `int64` conversion.

- [x] **FormerIndex tracking** — `FormerIndex` struct with `SubspaceKey`, `AddedVersion`, `RemovedVersion`, `FormerName`. `RemoveIndex()` on builder creates FormerIndex and removes from all record types. `Build()` validates no subspace key reuse. `GetFormerIndexes()` on metadata.

- [ ] **Schema validation** — Java has `MetaDataValidator` and `MetaDataEvolutionValidator`. Go has no validation on schema changes (primary key changes, version bumps, etc.).

### MEDIUM

- [ ] **Metadata proto serialization** — Java has `toProto()`/`fromProto()` for persisting metadata definitions. Go has none. Needed for storing metadata in FDB itself.

- [x] **Explicit record type keys** — `SetRecordTypeKey()` on `RecordTypeBuilder`, `GetRecordTypeKey()` on `RecordType`. Falls back to `RecordTypeIndex` if not set.

- [x] **Multi-type indexes** — `AddMultiTypeIndex(recordTypeNames, index)`. 0 types → universal, 1 type → single-type, 2+ types → multi-type (stored per RecordType, included in `GetIndexesForRecordType`). Matches Java semantics.

- [x] **Schema evolution version tracking** — `SetVersion()` on builder sets metadata version. Used in store header for compatibility tracking.

- [x] **Primary key prefix checking** — `PrimaryKeyHasRecordTypePrefix()` on `RecordMetaData`. Checks all record types' primary keys start with `RecordTypeKeyExpression`, including through `CompositeKeyExpression`.

### LOW

- [ ] **Missing key expression types** — 16+ types not in Go: VersionKeyExpression, GroupingKeyExpression, FunctionKeyExpression, LongArithmeticFunctionKeyExpression, OrderFunctionKeyExpression, CollateFunctionKeyExpression, DimensionsKeyExpression, LiteralKeyExpression, SplitKeyExpression, InvertibleFunctionKeyExpression, ListKeyExpression, etc.

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

- [ ] **Cursor combinators** — Java has 20+ cursor combinator types completely missing in Go:
  - **Set operations**: `UnionCursor`, `IntersectionCursor`, `DedupCursor`
  - **Composition**: `FlatMapPipelinedCursor`, `ConcatCursor`, `ChainedCursor`
  - **Aggregation**: `AggregateCursor` with accumulator states
  - **Control flow**: `FallbackCursor`, `AutoContinuingCursor`, `RecursiveCursor`
  - **Transformation**: `MapPipelinedCursor`, `MapResultCursor`, `MapWhileCursor`, `SkipCursor`
  - **Utilities**: `EmptyCursor`, `ListCursor`, `IteratorCursor`, `FutureCursor`, `LazyCursor`

- [ ] **CursorLimitManager** — Java has a separate class for comprehensive limit tracking (record scan, byte scan, time). Go has inline limit logic in keyValueCursor.

- [x] **RecordCursor instance methods** — `First()`, `GetCount()`, `Reduce()` as standalone generic functions. `SkipCursor()`, `LimitRowsCursor()` as cursor wrappers. Matches Java's `first()`, `getCount()`, `reduce()`, `skip()`, `limitRowsTo()`.

### LOW

- [ ] **Visitor pattern** — Java has `RecordCursorVisitor` interface for cursor inspection/instrumentation.
- [x] **Continuation SerializationMode** — Go uses TO_OLD (raw bytes) for writing, accepts both TO_OLD and TO_NEW (proto-wrapped) for reading. Matches Java Record Layer 4.2.6.0 which only supports TO_OLD.

---

## Store — conformance gaps

### HIGH

- [x] **Store state management** — `GetRecordStoreState()` returns store header + index states. `SetStoreLockState()` persists lock state to header. `ReloadRecordStoreState()` forces reload from FDB.

- [ ] **Query execution methods** — Java has `countRecords()`, `evaluateIndexRecordFunction()`, `evaluateStoreFunction()`, `evaluateAggregateFunction()`. Go has none.

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

## Split records — conformance status

**FULLY CONFORMANT.** No gaps found. All constants, save/load/delete/exists methods, SizeInfo tracking, cursor integration, and edge cases match Java behavior. Wire-compatible. Version handling separated into store layer (architectural choice, not a gap).

---

## Infrastructure

- [x] Bazel migration, nogo linting, CI pipeline, justfile — all done
- [ ] **KeySpace/KeySpacePath** — Enterprise key management. LOW priority.
- [ ] **ScanLimiter** — TimeScanLimiter, ByteScanLimiter, RecordScanLimiter composability. Important for production.

---

## Documentation cleanup

### LOW

- [x] **Clean up PORT.md** — Deleted stale 57KB file.
- [x] **Clean up PHASE1_TEST_GAPS.md** — Deleted stale file.
- [x] **Clean up FDB_CONFLICT_DETECTION.md** — Deleted stale file.
