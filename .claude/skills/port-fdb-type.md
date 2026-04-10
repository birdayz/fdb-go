---
name: port-fdb-type
description: Port an FDB C++ wire type to Go by implementing the FDBSerializable interface. Reads the C++ serialize() method, ports branching logic, writes the Go file, verifies against test vectors.
user-invocable: true
---

# Port FDB Type to Go

Port a single FDB C++ wire type to Go by implementing the `wire.FDBSerializable` interface.

## Input

The user provides a type name, e.g., `/port-fdb-type MutationRef`.

## Steps

### 1. Find the C++ source

Search for the type's `serialize()` method in the FDB C++ source:
```
/home/birdy/projects/foundationdb/fdbclient/include/fdbclient/FDBTypes.h
/home/birdy/projects/foundationdb/fdbclient/include/fdbclient/StorageServerInterface.h
/home/birdy/projects/foundationdb/fdbclient/include/fdbclient/CommitProxyInterface.h
/home/birdy/projects/foundationdb/fdbclient/include/fdbclient/GrvProxyInterface.h
/home/birdy/projects/foundationdb/fdbclient/include/fdbclient/CoordinationInterface.h
/home/birdy/projects/foundationdb/fdbclient/include/fdbclient/Tracing.h
```

Read the full struct definition including:
- Field declarations (types and names)
- `serialize(Ar& ar)` method with ALL branches
- Any `serializable_traits` or `dynamic_size_traits` specializations
- Base class serialize methods (e.g., `LoadBalancedReply::penalty`)

### 2. Load the JSON schema

Check if the C++ schema extractor has produced a schema:
```
/tmp/fdb-schemas-v4/{TypeName}.json
```
OR from the existing schemas:
```
pkg/fdbgo/wire/schema/{TypeName}.json
```

The schema provides: vtable, vtable_closure, per-field trait/size/align/indirection.

### 3. Write the Go implementation

Create file: `pkg/fdbgo/wire/types/{type_name}.go`

The file must:

```go
package types

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"

// {TypeName}VTable is the vtable from the C++ schema extractor.
var {TypeName}VTable = wire.VTable{...}  // from JSON

type {TypeName} struct {
    // Go fields matching the C++ struct.
    // Use exact Go types: []byte for StringRef/Key/Value,
    // int64 for Version, uint8 for enum types, bool for bool,
    // [16]byte or wire.UID for UID, etc.
}

func (m *{TypeName}) TypeVTable() wire.VTable {
    return {TypeName}VTable
}

func (m *{TypeName}) MarshalInto(obj *wire.ObjectWriter) {
    // Port the C++ serialize() method to Go.
    // For each field, call the appropriate ObjectWriter method:
    //   scalar → obj.WriteInt64(offset, value), obj.WriteBool(offset, value), etc.
    //   dynamic_size (StringRef) → obj.WriteBytes(offset, value)
    //   serialize_member → obj.WriteStruct(offset, fieldVTable, align, func(inner) { field.MarshalInto(inner) })
    //   union_like (Optional) → obj.WriteOptionalXxxPresent/Absent(typeOffset, valueOffset, ...)
    //
    // If the C++ has conditional branches:
    //   Port the if/else logic exactly.
    //   Use the same conditions (protocol version, data checks).
    //   Each branch calls the same WriteXxx methods with different field values.
}

func (m *{TypeName}) UnmarshalFrom(r *wire.Reader) error {
    // Port the C++ deserialization.
    // For each field, call the appropriate Reader method:
    //   scalar → r.ReadInt64(slot), r.ReadBool(slot), etc.
    //   dynamic_size → r.ReadBytes(slot)
    //   serialize_member → r.ReadNestedReader(slot) then field.UnmarshalFrom(nested)
    //   union_like → check r.ReadUint8(typeSlot), then r.ReadBytes(valueSlot)
    //
    // If the C++ has conditional deserialization:
    //   Port the reconstruction logic (e.g., KeyRangeRef end=="" → reconstruct from begin).
    return nil
}
```

### 4. Key rules for porting C++ → Go

- `StringRef` / `Key` / `Value` / `KeyRef` / `ValueRef` → `[]byte`
- `int` / `int32_t` → `int32`
- `uint32_t` → `uint32`
- `int64_t` / `Version` → `int64`
- `uint64_t` → `uint64`
- `uint8_t` / enums → `uint8`
- `bool` → `bool`
- `double` → `float64`
- `UID` → `[16]byte` or struct with First/Second uint64 (scalar, size=16, inline)
- `Optional<T>` → pointer or presence flag + value
- `VectorRef<T>` → `[]T` or `[]byte` depending on element type
- `Arena` → skip (zero-size field)

- Vtable offsets: use `vt[slot+2]` to get the byte offset in the object.
- `MarshalInto` writes fields using `obj.WriteXxx(int(vt[slot+2]), value)`.
- Struct fields with `serialize_member` trait: use `obj.WriteStruct(offset, fieldVT, align, callback)`.
- Dynamic size fields: use `obj.WriteBytes(offset, data)`.

### 5. Verify

If a test vector exists (`pkg/fdbgo/wire/testdata/{TypeName}.json`):
- Unmarshal the test vector bytes
- Re-marshal and compare (should produce identical bytes for default-constructed values)

Run: `bazelisk test //pkg/fdbgo/wire/types:types_test`

### 6. Reference files

- Interface definition: `pkg/fdbgo/wire/serializable.go`
- ObjectWriter: `pkg/fdbgo/wire/writer.go`
- Reader: `pkg/fdbgo/wire/reader.go`
- Static vs logic split: `docs/wire-format-static-vs-logic.md`
- C++ schema extractor: `cmd/fdb-schema-extract/main.cpp`
- Extracted schemas: `/tmp/fdb-schemas-v4/*.json`
