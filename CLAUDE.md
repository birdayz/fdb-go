# FoundationDB Record Layer Go Port

## Project Goal
Port the FoundationDB Record Layer from Java to Go, maintaining API compatibility so that:
- Go applications can read/write records created by Java Record Layer applications
- Java applications can read/write records created by Go Record Layer applications
- Both can share the same FDB cluster and data

## MVP Acceptance Criteria
Successfully implement the [FoundationDB Record Layer Getting Started Guide](https://foundationdb.github.io/fdb-record-layer/GettingStarted.html) in Go with full Java compatibility.

The Go implementation should be able to:
1. Open a record store
2. Save records (protobuf messages)
3. Load records by primary key
4. Be compatible with records written by the Java implementation

## Architecture Decisions

### Package Structure
- `pkg/recordlayer/` - Main Record Layer implementation
- `gen/` - Generated protobuf Go code from Apple's proto definitions
- `proto/` - Apple's original protobuf definitions

### Key Components Ported

1. **FDBDatabase** (struct, not interface)
   - Wraps core `fdb.Database` 
   - Provides `Run()` method with transaction retry logic
   - Equivalent to Java's `com.apple.foundationdb.record.provider.foundationdb.FDBDatabase`

2. **FDBRecordContext** (struct)
   - Wraps core `fdb.Transaction`
   - Provides Record Layer transaction context
   - Equivalent to Java's `FDBRecordContext`

3. **FDBRecordStore** (struct)
   - Main interface for record operations
   - Handles record serialization/deserialization
   - Manages subspaces and key construction

4. **RecordMetaData** (struct)
   - Manages protobuf schema definitions
   - Handles record type metadata

### Protobuf Integration
- Using `buf` for code generation with managed mode
- Apple's original proto files from Record Layer repository
- Generated Go package: `gen`

### Compatibility Strategy
- Use identical subspace constants as Java implementation
- Match Java's key construction exactly (using FDB's tuple encoding)
- Use same protobuf serialization
- Store records in identical format

## Development Status

### Completed ✅
- [x] Project setup with Go modules
- [x] Protobuf code generation with buf
- [x] Core interface definitions (FDBDatabase, FDBRecordContext, etc.)
- [x] FDBDatabase implementation with Run() method and transaction retry logic
- [x] Apple's protobuf definitions imported and compiled
- [x] RecordMetaData with protobuf schema handling and UnionDescriptor support
- [x] FDBRecordStore implementation with LoadRecord and SaveRecord
- [x] Record serialization/deserialization with Java compatibility
- [x] Key construction matching Java exactly (always includes record type index)
- [x] Subspace management and constants matching Java
- [x] Builder pattern matching Java (Create, Open, CreateOrOpen, Build)
- [x] TypedFDBRecordStore with Go generics for type safety
- [x] Java compatibility tests (bidirectional read/write)

### TODO 📋 (Rock-Solid Basic Layer)

#### High Priority - Core CRUD Operations
- [x] Implement DeleteRecord method
- [ ] Implement RecordExists method  
- [ ] Add RecordExistenceCheck enum and enhance SaveRecord
  - `NONE` - No special action (current behavior)
  - `ERROR_IF_EXISTS` - Throw if record already exists (insert-only)
  - `ERROR_IF_NOT_EXISTS` - Throw if record doesn't exist (update-only)
  - `ERROR_IF_RECORD_TYPE_CHANGED` - Throw if existing record has different type

#### Medium Priority - Advanced Features
- [ ] Implement LoadRecordVersion method
- [ ] Add record conflict management methods
  - `AddRecordReadConflict(primaryKey)` - Add read conflict for a record
  - `AddRecordWriteConflict(primaryKey)` - Add write conflict for a record

#### Low Priority - Bulk Operations
- [ ] Implement DeleteAllRecords method
- [ ] Implement CountRecords method
- [ ] Consider implementing KeySpace/KeySpacePath for future enterprise features

### TODO 📋 (Advanced Features)
- [ ] **Record Version Support** - Cross-transaction optimistic concurrency control
  - Implement FDBRecordVersion struct (12-byte: 10-byte global + 2-byte local)
  - Add versionstamp operations (SET_VERSIONSTAMPED_VALUE) to FDBRecordContext
  - Support complete/incomplete versionstamp states  
  - Add conditional save/update operations with expected version
  - Implement LoadRecordVersion method
  - Add version key storage/cleanup in save/delete operations
  - Enable optimistic locking for long-running workflows beyond 5-second FDB transaction limit
  - Provide etag-like semantics for API services (AIP-154 compliance)

### TODO 📋 (Post-Basic Layer)
- [ ] Cursor API for scanning large datasets
- [ ] Continuation handling for cross-transaction operations
- [ ] ScanLimiter implementations (especially TimeScanLimiter for 5-second transaction limits)
- [ ] Index support and query operations
- [ ] Performance optimizations

## Implementation Notes

### Subspace Constants (Java → Go)
```go
// These MUST match Java values exactly
const (
    RecordKey = 1      // Where records are stored
    IndexKey = 2       // Where indexes are stored
    // ... other constants TBD
)
```

### Transaction Pattern
```go
// Java: db.run(context -> { ... })
// Go: db.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) { ... })
```

### Builder Pattern
Java's four terminal methods are exactly replicated:
- `Create()` - creates store, fails if exists
- `Open()` - opens existing store, fails if doesn't exist  
- `CreateOrOpen()` - creates if needed, opens if exists
- `Build()` - returns store without checking database state (advanced/unsafe)

### Key Insights from Rust Record Layer Implementation
From [Rust FDB Record Layer discussion](https://forums.foundationdb.org/t/rust-fdb-record-layer-work-in-progress-repository/3765):

**Cursor/Continuation Requirements**:
- Cursors must handle FDB's 5-second transaction limits via `TimeScanLimiter`
- Continuations serialize/deserialize cursor state across transactions
- Java uses protobuf for continuations - we should maintain compatibility

**Critical Missing Features** (Post-MVP):
- **Scan operations with continuations** - for large result sets across transaction boundaries
- **ScanLimiter implementations** - especially `TimeScanLimiter` for transaction limits
- **Cursor API** - for streaming large datasets without memory overload

**Design Validation**:
- Our compatibility-first approach is better than Rust's type-safety-first approach for interoperability
- Centralized transaction state in `FDBRecordContext` is correct
- Must handle FDB constraints (100KB values, key limits) at Record Layer level

### Cursor/Continuation Protobuf Schema
Java Record Layer uses protobuf for cursor continuations: `fdb-record-layer-core/src/main/proto/record_cursor.proto`

Key continuation types we'll need to implement:
- `FlatMapContinuation` - for flat map operations
- `IntersectionContinuation` - for intersecting multiple cursors  
- `UnionContinuation` - for union operations on cursors
- `ConcatContinuation` - for concatenating cursor results
- `SizeStatisticsContinuation` - for size statistics with partial results
- Various specialized continuations for complex query operations

Each continuation serializes cursor state to bytes, allowing reconstruction across transaction boundaries.

### Build Requirements
- Go 1.21+
- buf CLI for protobuf generation
- FoundationDB client libraries (for runtime)

## Testing Strategy
1. Unit tests for each component
2. Integration tests against real FDB cluster
3. Compatibility tests:
   - Go writes → Java reads
   - Java writes → Go reads
   - Shared record store operations

## Repository Structure
```
├── pkg/recordlayer/     # Go Record Layer implementation
├── gen/                 # Generated protobuf Go code  
├── proto/              # Apple's protobuf definitions
├── buf.yaml            # Buf configuration
├── buf.gen.yaml        # Buf code generation config
├── go.mod              # Go module definition
└── CLAUDE.md           # This file
```