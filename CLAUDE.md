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
- [x] Basic FDBDatabase implementation with Run() method
- [x] Apple's protobuf definitions imported and compiled

### In Progress 🚧
- [ ] FDBDatabase transaction retry logic
- [ ] RecordMetaData protobuf schema handling
- [ ] FDBRecordStore implementation
- [ ] Record serialization/deserialization

### TODO 📋
- [ ] LoadRecord implementation
- [ ] Record saving functionality
- [ ] Key construction matching Java exactly
- [ ] Subspace management
- [ ] Getting Started guide implementation in Go
- [ ] Java compatibility tests

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