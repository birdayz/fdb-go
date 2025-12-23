# FoundationDB Conflict Detection - Critical Insights

## Summary

Successfully implemented and tested `AddRecordReadConflict` and `AddRecordWriteConflict` methods. All 11 conflict tests pass.

## Key Discovery: Read-Only Transactions Don't Check Conflicts

**The most critical insight:** FoundationDB **does not check for conflicts on read-only transactions**. This is a performance optimization - if a transaction doesn't write anything, there's nothing to roll back, so conflict checking is unnecessary.

### Example

```go
// This transaction will NEVER conflict, even with AddReadConflictRange!
tx1, _ := db.CreateTransaction()
tx1.Get(fdb.Key("target")).Get()  // Read the key
tx1.AddReadConflictRange(...)     // Add conflict range
tx1.Commit().Get()                // ✅ ALWAYS SUCCEEDS (read-only)
```

```go
// This transaction WILL conflict if another transaction writes
tx1, _ := db.CreateTransaction()
tx1.Get(fdb.Key("target")).Get()        // Read the key
tx1.AddReadConflictRange(...)           // Add conflict range
tx1.Set(fdb.Key("marker"), []byte("x")) // ✅ Now it's a read-write transaction!
tx1.Commit().Get()                      // ❌ FAILS if target was modified
```

## Testing Pattern: Use Raw Transactions, Not Retry Wrappers

### Java Pattern (from fdb-record-layer)

Java tests use `openContext()` which creates **raw transactions without retry logic**:

```java
// CORRECT: Raw transaction for conflict testing
try (FDBRecordContext context1 = database.openContext()) {
    context1.addReadConflictRange(...);
    // ... operations ...

    try (FDBRecordContext context2 = database.openContext()) {
        context2.saveRecord(...);
        context2.commit();  // Commits successfully
    }

    assertThrows(FDBStoreTransactionConflictException.class,
        context1::commit);  // Asserts conflict
}
```

```java
// WRONG: Using run() with retry logic masks conflicts
database.run(context -> {
    context.addReadConflictRange(...);
    // Channel close happens here
    return null;
});
// Retries automatically on conflict, closes channels again → PANIC!
```

### Go Pattern (our implementation)

```go
// CORRECT: Raw transaction for conflict testing
tx1, _ := db.CreateTransaction()
rtx := recordlayer.NewFDBRecordContext(tx1)
fdbStore, _ := recordlayer.NewStoreBuilder().
    SetContext(rtx).
    SetMetaDataProvider(metaData).
    CreateOrOpen()

fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})

// CRITICAL: Add a write to make it a read-write transaction
rtx.Transaction().Set(fdb.Key("marker"), []byte("tx1"))

err := rtx.Commit()  // Check for conflict error (code 1020)
```

```go
// WRONG: Using Run() with retry logic
env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (interface{}, error) {
    fdbStore.AddRecordReadConflict(...)
    close(tx1Started)  // First attempt
    // ... conflict occurs ...
    // Retry happens automatically
    close(tx1Started)  // Second attempt → PANIC: close of closed channel
    return nil, nil
})
```

## Conflict Range Semantics

### AddRecordReadConflict

Adds a **read conflict range** on the record's key:
- **Effect**: Transaction fails if another transaction **writes** to this key
- **Use case**: "I care if this record changes, even though I didn't read it"
- **Example**: Atomic operations like `add()` that don't create read conflicts by default

```go
// TX1: Adds read conflict (treats as if read)
fdbStore.AddRecordReadConflict(tuple.Tuple{orderID})
tx1.Set(fdb.Key("marker"), []byte("x"))  // Make it read-write

// TX2: Writes to same record
fdbStore.SaveRecord(order)

// Result: TX1 commit fails with NOT_COMMITTED (code 1020)
```

### AddRecordWriteConflict

Adds a **write conflict range** on the record's key:
- **Effect**: Transactions that **read** this key will conflict with us
- **Use case**: "I want to invalidate other transactions that read this key, even though I didn't write it"
- **Example**: Logical invalidation operations

```go
// TX1: Reads the record (creates implicit read conflict)
storedRecord, _ := fdbStore.LoadRecord(tuple.Tuple{orderID})
tx1.Set(fdb.Key("marker"), []byte("x"))  // Make it read-write

// TX2: Adds write conflict (treats as if written)
fdbStore.AddRecordWriteConflict(tuple.Tuple{orderID})
tx2.Set(fdb.Key("marker"), []byte("y"))  // Make it read-write
tx2.Commit()

// Result: TX1 commit fails with NOT_COMMITTED (code 1020)
```

## Implementation Details

### Conflict Range Calculation

From `pkg/recordlayer/store.go:1217-1231`:

```go
func (s *FDBRecordStore) AddRecordReadConflict(primaryKey tuple.Tuple) {
    // Compute full record key: recordSubspace + (recordTypeIndex,) + primaryKey
    recordKey := s.constructRecordKey(primaryKey)

    // Create range covering this single record
    // Java: TupleRange.allOf(primaryKey) creates RANGE_INCLUSIVE endpoints
    conflictRange := fdb.KeyRange{
        Begin: recordKey,
        End:   append(recordKey, 0xFF),  // Exclusive end
    }

    s.context.Transaction().AddReadConflictRange(conflictRange)
}
```

This matches Java's `TupleRange.allOf()` behavior exactly.

### Key Subspace Structure

Records are stored at:
```
[recordSubspace] + [RECORD_KEY=1] + [recordTypeIndex] + [primaryKey]
```

For a record with `orderID=40001`:
```
Begin: [..., 1, 0, 40001]       # Inclusive start
End:   [..., 1, 0, 40001, 0xFF] # Exclusive end
```

## Test Results

All 11 conflict tests pass:

```
Ran 11 of 71 Specs in 71.499 seconds
SUCCESS! -- 11 Passed | 0 Failed | 0 Pending | 60 Skipped
```

### Test Coverage

1. ✅ AddRecordReadConflict causes conflicts when another transaction writes
2. ✅ AddRecordReadConflict does NOT conflict with reads
3. ✅ AddRecordWriteConflict causes conflicts when another transaction reads
4. ✅ AddRecordWriteConflict is self-consistent within same transaction
5. ✅ Multiple conflicts on same key are idempotent
6. ✅ Conflicts on different keys are independent
7. ✅ Conflict ranges cover record keys correctly
8. ✅ Conflicts work with SaveRecord operations
9. ✅ Conflicts work with DeleteRecord operations
10. ✅ SERIALIZABLE isolation level creates read conflicts
11. ✅ SNAPSHOT isolation level does NOT create read conflicts

## References

- **Java Implementation**: `FDBRecordStore.java:1217-1231`
- **Java Tests**: `OnlineIndexerConflictsTest.java`, `FDBRecordStoreConcurrentTestBase.java`
- **FDB Documentation**: https://apple.github.io/foundationdb/developer-guide.html#conflict-ranges
- **Go Implementation**: `pkg/recordlayer/store.go`
- **Go Tests**: `conformance/conflict_conformance_test.go`, `conformance/isolation_conformance_test.go`
