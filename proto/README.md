# Protocol Buffer Definitions

This directory contains the Protocol Buffer definitions for the FDB Record Layer Go port.

## Directory Structure

```
proto/
├── apple/           # Official Apple FDB Record Layer protos
│   ├── record_key_expression.proto
│   ├── record_metadata_options.proto
│   ├── record_metadata.proto
│   ├── record_query_plan.proto
│   └── tuple_fields.proto
└── README.md

conformance/
└── record_layer_demo.proto   # Test/conformance demo schema (Go: fdb.dev/internal/pb/demopb)
```

## Proto Organization

### Apple Protos (`proto/apple/`)

These are the official Protocol Buffer definitions from Apple's FDB Record Layer project.
They define the core metadata, query planning, and indexing structures.

**Source**: https://github.com/FoundationDB/fdb-record-layer/tree/main/fdb-record-layer-core/src/main/proto

**Package**: `com.apple.foundationdb.record`

**Do NOT modify** these protos - they must remain compatible with the Java Record Layer implementation.

### Conformance Test Protos (`conformance/proto/`)

These are test-specific Protocol Buffer definitions used for conformance testing and examples.

**Package**: `com.apple.foundationdb.record`

**Example Messages**:
- `Order` - Sample order record with flower and price
- `Customer` - Sample customer record
- `Flower` - Nested message with type and color
- `UnionDescriptor` - Union of record types for testing

These protos can be modified freely for testing purposes.

## Code Generation

Generated Go code is placed in `pkg/recordlayer/metadatapb/` (public Record Layer metadata closure) and `internal/pb/...` (all internal/conformance protos).

### Generate All Protos

```bash
buf generate
```

This will:
1. Compile both Apple and conformance protos
2. Generate Go code with `protoc-gen-go`
3. Place output in `pkg/recordlayer/metadatapb/` and `internal/pb/<area>/`; the demo schema goes to
   `internal/pb/demopb` (`fdb.dev/internal/pb/demopb`), which production code never imports

### Buf Configuration

- **buf.yaml** - Workspace configuration defining both proto modules
- **buf.gen.yaml** - Code generation configuration

The workspace setup allows conformance protos to import Apple protos while keeping them organizationally separate.

## Import Guidelines

### From Go Code

```go
import "fdb.dev/internal/pb/demopb"

// Use generated types
order := &demopb.Order{
    OrderId: proto.Int64(1001),
    Price:   proto.Int32(100),
}
```

### From Proto Files

Conformance protos can import Apple protos directly:

```protobuf
import "record_metadata_options.proto";

message MyMessage {
    option (com.apple.foundationdb.record.record).usage = UNION;
    // ...
}
```

## Adding New Test Protos

1. Add `.proto` file to `conformance/proto/`
2. Import Apple protos if needed for annotations
3. Run `buf generate`
4. Use generated code from `internal/pb/...` (or `pkg/recordlayer/metadatapb`)

## Linting

Apple protos may show lint warnings - **this is expected and OK**. They follow Java protobuf conventions which differ from Go conventions.

To lint only conformance protos:
```bash
cd conformance/proto && buf lint
```
