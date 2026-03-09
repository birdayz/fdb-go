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

- [ ] **Reverse scan conformance** — Go reverse scans are only self-tested. Need Java scan step with reverse support.

- [ ] **Fan-out index conformance** — Go fan-out creates multiple index entries per repeated field. Java must produce identical entries.

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

---

## Bugs (found in conformance audit)

### CRITICAL

- [x] **Version values stored as raw bytes instead of tuple-packed Versionstamp** — Fixed: Go stored version values as raw 12-byte FDBRecordVersion bytes. Java's `SplitHelper.unpackVersion()` calls `Tuple.fromBytes()` expecting a tuple-encoded Versionstamp. Caused "Unknown tuple data type 3 at index 5" error. Fix: wrap in `tuple.Tuple{Versionstamp}.Pack()` for complete, `PackWithVersionstamp()` for incomplete.

- [x] **Java conformance server tenant.run() skips version mutation flush** — Fixed: `runInContext` for tenants used `tenant.run()` which auto-commits bypassing `FDBRecordContext.commitAsync()`. Pre-commit hooks (version mutation flush) never fired, so versioned saves silently dropped version data. Fix: use `createTransaction()` + `context.commitAsync().join()`.

- [x] **CompositeKeyExpression does concat, not cross-product** — Fixed: `Evaluate()` now returns `[][]interface{}` (list of key tuples) and `CompositeKeyExpression` computes Cartesian product matching Java's `ThenKeyExpression`.

- [x] **evaluateIndex() always returns 1 entry per record** — Fixed: `evaluateIndex()` now creates one `indexEntry` per returned tuple, enabling fan-out for multi-valued expressions.

### HIGH

- [x] **GetValue() returns zero on !HasNext()** — Fixed: `GetValue()` now panics when `hasNext` is false, matching Java's `IllegalResultValueAccessException`.

- [x] **Build() doesn't validate primary keys** — Fixed: `Build()` now returns `(*RecordMetaData, error)` and validates all record types have primary keys set. All callers updated.

### MEDIUM

- [x] **FDBRecordVersion missing Equal/Less** — Fixed: Added `Equal()`, `Less()`, `String()` methods matching Java's `equals()`/`compareTo()`/`toString()` semantics.

---

## Indexing — conformance gaps

### CRITICAL

- [x] **Index scanning** — `IndexMaintainer.Scan()` and `FDBRecordStore.ScanIndex()` return `RecordCursor[*IndexEntry]` with `TupleRange` support (ALL, AllOf, Between, BetweenInclusive), continuations, row/byte limits, forward/reverse. `IndexEntry.PrimaryKey()` and `IndexValues()` for key extraction.

- [ ] **Index state management** — Java has 4 states: `READABLE`, `WRITE_ONLY`, `DISABLED`, `READABLE_UNIQUE_PENDING` (stored in `IndexStateSpaceKey` subspace). Go has none — all indexes are implicitly READABLE always. Blocks online index builds and disable/rebuild workflows.

- [ ] **Index build support** — Java has `updateWhileWriteOnly`, `isIdempotent`, `addedRangeWithKey`, RangeSet tracking for online builds. Go has none. Cannot build indexes on existing data.

### HIGH

- [ ] **Index management store methods** — Java FDBRecordStore has 15+ index methods missing in Go: `rebuildIndex`, `markIndexReadable`, `markIndexDisabled`, `markIndexWriteOnly`, `getIndexState`, `isIndexReadable`, `isIndexWriteOnly`, `isIndexDisabled`, `clearAndMarkIndexWriteOnly`, `getIndexBuildStateAsync`, etc.

- [x] **Repeated field fan-out** — `FanOut("field")` creates `FieldKeyExpression` with `FanTypeFanOut`, producing one index entry per repeated value. Cross-product with `Concat()` works. Empty repeated field → no entries (matching Java).

- [ ] **Sparse/filtered indexes** — Java `Index` has `IndexPredicate` to selectively index records. Go has no predicate field. Needed for partial indexes.

### MEDIUM

- [ ] **Index types beyond VALUE** — Java has 15+ types: COUNT, COUNT_UPDATES, COUNT_NOT_NULL, SUM, MIN_EVER_TUPLE/LONG, MAX_EVER_TUPLE/LONG, RANK, TIME_WINDOW_LEADERBOARD, VERSION, TEXT, BITMAP_VALUE, PERMUTED_MIN/MAX, MULTIDIMENSIONAL, VECTOR. Go only has VALUE.

- [ ] **Uniqueness violation tracking** — Java has `scanUniquenessViolations()`, `clearUniquenessViolations()` in `IndexUniquenessViolationsKey` (7) subspace. Go detects violations but doesn't track them.

- [ ] **Index validation** — Java has `validateEntries()` to detect orphaned/missing entries. Go has none.

- [ ] **Primary key component deduplication** — Java's `primaryKeyComponentPositions` tracks overlap between PK and index key to avoid redundant storage. Go always appends full PK (wastes space but is wire-compatible).

- [ ] **Bulk index delete** — Java has `canDeleteWhere()`/`deleteWhere()` for range-based deletion. Go has none.

- [ ] **Aggregate functions via indexes** — Java has `canEvaluateAggregateFunction()`/`evaluateAggregateFunction()` for COUNT, MIN, MAX, SUM via index maintainers. Go's COUNT is via store atomic mutations, not indexes.

---

## Metadata — conformance gaps

### HIGH

- [x] **ThenKeyExpression** — `CompositeKeyExpression` via `Concat()` now computes Cartesian cross-product matching Java's `ThenKeyExpression` semantics.

- [x] **NestingKeyExpression** — `Nest("field", child)` navigates into nested message fields. `NestFanOut("field", child)` for repeated message fields. Composite nested fields work (e.g., `Nest("flower", Concat(Field("type"), Field("color")))`). Enum fields supported via `int64` conversion.

- [ ] **FormerIndex tracking** — Java tracks deleted indexes with `subspaceKey`, `addedVersion`, `removedVersion`, `formerName`. Needed for schema evolution — prevents subspace key reuse after index deletion.

- [ ] **Schema validation** — Java has `MetaDataValidator` and `MetaDataEvolutionValidator`. Go has no validation on schema changes (primary key changes, version bumps, etc.).

### MEDIUM

- [ ] **Metadata proto serialization** — Java has `toProto()`/`fromProto()` for persisting metadata definitions. Go has none. Needed for storing metadata in FDB itself.

- [ ] **Explicit record type keys** — Java supports `setRecordTypeKey()` to override auto-derived type keys from proto field numbers. Go relies solely on proto field numbers.

- [ ] **Multi-type indexes** — Java has `addMultiTypeIndex()` for indexes spanning multiple record types. Go only has single-type and universal indexes.

- [ ] **Schema evolution version tracking** — Go has `version` field but no `updateRecords()` method to bump version or validate backward compatibility.

- [ ] **Primary key prefix checking** — Java has `primaryKeyHasRecordTypePrefix()` to check if RecordTypeKeyExpression starts all primary keys. Useful for type-specific range queries.

### LOW

- [ ] **Missing key expression types** — 16+ types not in Go: VersionKeyExpression, GroupingKeyExpression, FunctionKeyExpression, LongArithmeticFunctionKeyExpression, OrderFunctionKeyExpression, CollateFunctionKeyExpression, DimensionsKeyExpression, LiteralKeyExpression, SplitKeyExpression, InvertibleFunctionKeyExpression, ListKeyExpression, etc.

- [ ] **Synthetic record types** — Computed/joined/unnested record types. Large feature.

- [ ] **User-defined functions** — `userDefinedFunctionMap` for custom query functions.

- [ ] **Views** — Named query/aggregation views.

- [ ] **Subspace key counter** — `enableCounterBasedSubspaceKeys()` for auto-incrementing index subspace keys.

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

- [ ] **RecordCursor instance methods** — Java has `getNext()`, `asIterator()`, `getCount()`, `first()`, `skip()`, `limitRowsTo()`, `skipThenLimit()`, `mapResult()`, `filterInstrumented()`, `reduce()`. Go has `ForEach()`/`AsList()` as standalone functions only.

### LOW

- [ ] **Visitor pattern** — Java has `RecordCursorVisitor` interface for cursor inspection/instrumentation.
- [x] **Continuation SerializationMode** — Go uses TO_OLD (raw bytes) for writing, accepts both TO_OLD and TO_NEW (proto-wrapped) for reading. Matches Java Record Layer 4.2.6.0 which only supports TO_OLD.

---

## Store — conformance gaps

### HIGH

- [ ] **Store state management** — Java has `loadRecordStoreStateAsync()`, `getRecordStoreState()`, `setStoreLockStateAsync()`, `updateRecordCountStateAsync()`. Go loads state on Build but has no explicit state management API.

- [ ] **Query execution methods** — Java has `countRecords()`, `evaluateIndexRecordFunction()`, `evaluateStoreFunction()`, `evaluateAggregateFunction()`. Go has none.

- [ ] **Per-type record count** — Java has `getSnapshotRecordCountForRecordType()`. Go has `GetSnapshotRecordCount()` (total only, though per-type counting works via RecordTypeKeyExpression internally).

### MEDIUM

- [ ] **Store statistics** — Java has `estimateStoreSizeAsync()`, `estimateRecordsSizeAsync()`. Go has none.

- [ ] **Format version / user version access** — Java has `getFormatVersion()`, `getUserVersion()`, `setUserVersion()`. Go has no version introspection.

- [ ] **Serializer access** — Java has `getSerializer()`, `getContext()`, `getKeyspacePath()`, `getIndexMaintainerRegistry()`. Go exposes `context` and `subspace` but not others.

- [ ] **Conformance test for type-changed existence check** — `conformance/existence_check_conformance_test.go` covers 4 of 5 modes. Add Java cross-validation for `ERROR_IF_RECORD_TYPE_CHANGED`.

### LOW

- [ ] **Advanced store operations** — Java has `dryRunSaveRecordAsync()`, `preloadRecordAsync()`, `repairRecordKeys()`. Go has none.

- [ ] **Synthetic records** — Java has `loadSyntheticRecord()`. Large feature tied to synthetic record types.

---

## Database / Transaction — conformance gaps

### HIGH

- [ ] **FDBDatabaseRunner** — Java has configurable retry with `maxAttempts`, `initialDelayMillis`, `maxDelayMillis`, `ExponentialDelay` backoff. Go delegates entirely to FDB's native `Transact()` retry with no control over retry parameters.

- [ ] **FDBRecordContextConfig** — Java has builder for transaction settings: transaction ID, timeout, priority, MDC context, tags, tracing, weak read semantics. Go's FDBRecordContext is minimal (just tx + go context).

- [ ] **Commit hooks** — Java has `CommitCheckAsync` (pre-commit consistency checks) and `PostCommit` (post-commit actions) interfaces. Go has none.

### MEDIUM

- [ ] **Timer / instrumentation** — Java has comprehensive `FDBStoreTimer` with event counters and timing throughout all operations. Go has no instrumentation.

- [ ] **Transaction priority** — Java has `FDBTransactionPriority` enum: `BATCH`, `DEFAULT`, `SYSTEM_IMMEDIATE`. Go has none.

- [ ] **Store state caching** — Java has `FDBRecordStoreStateCache` to avoid redundant header reads. Go loads state on demand without caching.

- [ ] **Read/write version management** — Java has `getReadVersion()`, `setReadVersion()`, `getReadVersionAsync()`. Go has none.

- [ ] **Conflict key reporting** — Java has `reportConflictingKeys()`, `getNotCommittedConflictingKeys()` for debugging. Go has none.

### LOW

- [ ] **FDBDatabaseFactory** — Factory/pooling for database instances.
- [ ] **Weak read semantics** — `WeakReadSemantics` for causal read risky, version staleness bounds.
- [ ] **Directory layer caching** — Multi-tenant keyspace management.
- [ ] **Transaction ID / MDC / logging** — Transaction tracing and structured logging.
- [ ] **Latency injection** — `FDBLatencySource` for testing.

---

## Record versioning — conformance gaps

### MEDIUM

- [ ] **Version comparison/ordering** — Java has `compareTo()`, `equals()`, `hashCode()`. Go has none. Needed for sorting versions and using them in collections.

- [ ] **Version range methods** — Java has `firstInDBVersion()`, `lastInDBVersion()`, `firstInGlobalVersion()`, `lastInGlobalVersion()`, `next()`, `prev()`. Go has none. Needed for version-based range queries.

- [ ] **MIN_VERSION / MAX_VERSION constants** — Java defines these as sentinel values. Go has none.

### LOW

- [ ] **Versionstamp conversion** — Java has `fromVersionstamp()`/`toVersionstamp()` explicit converters. Go handles this differently (context-level mutations) which works but lacks the explicit API.

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

- [ ] **Clean up PORT.md** — 57KB, contains inaccurate claims. Update or delete.
- [ ] **Clean up PHASE1_TEST_GAPS.md** — Many "CRITICAL GAP" items now resolved. Update or delete.
- [ ] **Clean up FDB_CONFLICT_DETECTION.md** — Implementation notes captured in code/tests. Consider deleting.
