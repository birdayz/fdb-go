# RFC 013: Code Generator v5 — Composable Primitives

## Status: Proposed

## Problem

The current C++ generator (v4, `cmd/fdb-schema-extract/main.cpp`) emits ~150 lines of nearly identical Go code per type: `UnmarshalFDB`, `UnmarshalFromReader`, `MarshalInto`, `MarshalFDB`, `WriteXxx`, `MarshalXxx`, `ParseXxxVector`. 32 files, ~5000 lines, 90% mechanical repetition.

Each code path (`MarshalInto`, `MarshalFDB`, the new `measureEndOff`/`writeDirect`) re-derives the same field traversal independently. Adding a new serialization strategy (like two-pass direct-write) means adding another per-type method with the same field-walking logic duplicated again.

The root cause: the generator treats each type as a special case, emitting bespoke code. It should instead compose a small set of generic primitives and let type-specific layout fall out of recursion.

## Design

### Core Insight

There are exactly **10 field primitives**. Every FDB wire type is a composition of these. The generator's job is to walk a type's field list and emit the correct primitive per field. No type-specific logic.

### Primitive Table

| # | Field Kind | Go Type | Unmarshal | Marshal (ObjectWriter) | Measure (endOff) | WriteDirect |
|---|---|---|---|---|---|---|
| 1 | Scalar | `int64`, `uint32`, `bool`, etc. | `ReadT(slot)` | `PutT(obj[off:], v)` | 0 (inline in object) | `PutT(obj[off:], v)` |
| 2 | UID | `[16]byte` | `ReadUID(slot)` | `copy(obj[off:], uid[:])` | 0 (inline) | `copy(obj[off:], uid[:])` |
| 3 | Bytes (StringRef) | `[]byte` | `ReadBytes(slot)` | `WriteBytes(off, data)` | `align4(4 + len(data))` | write `[len][data][pad]` at OOL pos |
| 4 | RawOOL (VectorRef pre-packed) | `[]byte` | `ReadBytes(slot)` | `WriteRawOOL(off, data)` | `align4(len(data))` | write `[data][pad]` at OOL pos |
| 5 | Nested struct | `T` | recurse `T.UnmarshalFromReader` | recurse `T.MarshalInto` via `WriteStruct` | recurse `T.measureEndOff(endOff)` | recurse `T.writeDirect(dw)` |
| 6 | Optional | `bool` + inner | `ReadUint8(slot) > 0` then inner at `slot+1` | skip if absent | skip if absent | skip if absent |
| 7 | Vector\<struct\> | `[]T` | loop + recurse T | header + loop marshal blobs | header + loop `T.measureEndOff` | header + loop `T.writeDirect` |
| 8 | Vector\<scalar\> | `[]T` | bulk read | `WriteVectorT(off, values)` | `align4(4 + n*sizeof(T))` | bulk write at OOL pos |
| 9 | Variant | tag + per-alt | switch on tag, dispatch per alternative | switch on tag, write per alternative | switch on tag, measure per alternative | switch on tag, write per alternative |
| 10 | VecSer::String | `[]T` | sequential `[len][data]` per field per element | N/A (read-only in practice) | N/A | N/A |

### What the Generator Emits Per Type

Given a type's field list `[(name, kind, goType, slot, maxAlign), ...]`, the generator emits:

```
1. Slot constants
2. VTable + closure + template
3. MaxAlign constant
4. Struct definition                  — field kind → Go type
5. UnmarshalFromReader               — field kind → read primitive
6. UnmarshalFDB                      — NewReader + UnmarshalFromReader
7. MarshalInto(obj *ObjectWriter)    — field kind → write primitive (legacy path)
8. measureEndOff(endOff int) int     — field kind → measure primitive
9. writeDirect(dw *DirectWriter) int — field kind → direct-write primitive
10. MarshalFDB() []byte              — two-pass: measure → alloc → write → FakeRoot/vtables/footer
```

Methods 5-10 are all generated from the **same field list** — the only difference is which column of the primitive table is used. The generator has one loop over fields, with a switch on field kind, emitting the appropriate primitive for the target method.

### Vector\<struct\> — The Missing Primitive

Today, `VectorRef<MutationRef>` is handled by pre-serializing each element via `MarshalStructBlob` (N allocations), then `PackVectorOfStructBlobs` (2 allocations). This is the only reason the commit path has N+4 allocs instead of 1.

With the composable model, `Vector<struct>` is just another primitive:

**Wire format** (same as C++ `VectorRef<serialize_member>`):
```
[count(4)][reloff_0(4)][reloff_1(4)]...[pad][blob_0][pad][blob_1]...
```
Each blob is self-contained: `[vtable][pad][soffset+fields][pad][ool_data]`.

**Measure**:
```go
func measureVector(endOff int, elements []T) int {
    vecSize := 4 + len(elements)*4  // header
    for _, e := range elements {
        vecSize = align4(vecSize)
        vecSize += blobSize(T.VTable, e)  // vtable + object + OOL
    }
    return endOff + align4(vecSize)
}
```

**blobSize** for any type T (recursive):
```go
func blobSize(vt VTable, e T) int {
    vtBytes := len(vt) * 2
    objPos := align4(vtBytes)
    objEnd := objPos + int(vt[1])
    oolPos := align4(objEnd)
    oolSize := e.oolSize()  // sum of bytes/rawOOL field sizes
    return oolPos + oolSize
}
```

**WriteDirect**: write header + offset table at vector start, then write each blob inline. Each blob writes its own vtable + soffset + fields + OOL at consecutive positions in the parent buffer. No intermediate `[]byte` per element.

**Impact**: A commit with 100 mutations goes from 104 allocs to 1. The `MarshalStructBlob` function and `PackVectorOfStructBlobs` become dead code for the two-pass path.

### Per-Type MaxAlign

The generator currently hardcodes `maxAlign=8` for types containing int64/uint64/float64/UID fields. This must become a per-type constant emitted alongside the vtable, because the two-pass `WriteObject` needs it for correct end-offset alignment.

```go
const KeySelectorRefMaxAlign = 8  // from C++ scalar_traits alignment
const TenantInfoMaxAlign = 8
```

The maxAlign is already known at extraction time (from C++ `fb_size<T>` and alignment rules). The generator just needs to emit it.

### What Changes vs v4

| Aspect | v4 (current) | v5 (proposed) |
|---|---|---|
| Code paths per type | 6 independent methods, each with own field loop | 6 methods, all from same field list, primitive dispatch |
| Adding new strategy | Add another per-type method + field loop | Add column to primitive table, generator emits it |
| Vector\<struct\> marshal | Pre-serialize each element → pack blobs → N+2 allocs | Inline: measure all → write all → 0 extra allocs |
| Two-pass direct-write | Hand-written per type (prototype) | Generated from same field list as MarshalInto |
| Per-type code volume | ~150 lines | ~100 lines (fewer methods, less redundancy) |
| Total generated code | ~5000 lines | ~3500 lines (Vector<struct> inlined, no standalone marshal helpers) |

### Generated Methods — Keep vs Drop

| Method | Keep? | Why |
|---|---|---|
| `UnmarshalFromReader` | Yes | Hot path for reply parsing. Zero-copy into buffer. |
| `UnmarshalFDB` | Yes | Convenience wrapper (NewReader + UnmarshalFromReader). |
| `MarshalInto(obj *ObjectWriter)` | **Drop** | Superseded by `writeDirect`. Only existed as callback for `WriteStruct`. |
| `MarshalFDB()` | Yes | Entry point. Now calls two-pass (measure → writeDirect) instead of `WriteMessagePacked(MarshalInto)`. |
| `measureEndOff(endOff int) int` | **New** | Pass 1 of two-pass. Pure arithmetic, zero alloc. |
| `writeDirect(dw *DirectWriter) int` | **New** | Pass 2 of two-pass. Writes directly into pre-allocated buffer. |
| `MarshalStructBlob` | **Drop** | Replaced by inline Vector<struct> in two-pass path. |
| `WriteXxx` / `MarshalXxx` helpers | **Drop** | Standalone marshal functions were bridge code. Two-pass inlines everything. |
| `ParseXxxVectorFromReader` | Keep | Needed for vector-of-struct unmarshal. Can optimize to zero-copy later. |
| `ParseXxxStringVector` | Keep | VecSer::String format (read-only). Optimize to zero-copy (slice into buffer). |

### Generator Architecture

The C++ generator's `GoEmitter` currently has separate `emitReads`, `emitMarshalInto`, etc. functions that each loop over fields. Restructure to:

```cpp
// One unified field walker that dispatches to the target method.
void emitFieldOp(const FieldDesc& fd, EmitTarget target) {
    switch (fd.kind) {
    case Scalar:
        switch (target) {
        case Unmarshal:  fprintf(f, "m.%s = r.%s(%sSlot%s)\n", ...);
        case Measure:    /* nothing — inline */
        case WriteDirect: fprintf(f, "binary.LittleEndian.Put%s(obj[off:], m.%s)\n", ...);
        }
        break;
    case Bytes:
        switch (target) {
        case Unmarshal:  fprintf(f, "m.%s = r.ReadBytes(%sSlot%s)\n", ...);
        case Measure:    fprintf(f, "endOff = wire.MeasureBytesOOL(endOff, m.%s)\n", ...);
        case WriteDirect: fprintf(f, "if m.%s != nil { %sPos = dw.WriteBytesOOL(m.%s) }\n", ...);
        }
        break;
    case NestedStruct:
        switch (target) {
        case Unmarshal:  fprintf(f, "if nr, err := r.ReadNestedReader(...); err == nil { m.%s.UnmarshalFromReader(nr) }\n", ...);
        case Measure:    fprintf(f, "endOff = m.%s.measureEndOff(endOff)\n", ...);
        case WriteDirect: fprintf(f, "%sPos := m.%s.writeDirect(dw)\n", ...);
        }
        break;
    case VectorStruct:
        switch (target) {
        case Measure:    fprintf(f, "endOff = measureVectorStruct(endOff, m.%s, ...)\n", ...);
        case WriteDirect: fprintf(f, "%sPos = writeVectorStruct(dw, m.%s, ...)\n", ...);
        }
        break;
    // ... etc
    }
}
```

Each generated method is: header + iterate fields calling `emitFieldOp(fd, target)` + footer. One loop, one switch, all methods.

### Migration Plan

1. **Add MaxAlign constant** per type to generator output.
2. **Add `measureEndOff` + `writeDirect`** generation using the primitive table. Verify byte-identical output against existing `MarshalFDB` for all 32 types via test.
3. **Switch `MarshalFDB`** to use two-pass (`measureEndOff` → `writeDirect`) instead of `WriteMessagePacked(MarshalInto)`.
4. **Add Vector\<struct\> primitive** to generator. CommitTransactionRef's mutations/conflict ranges become `[]MutationRef` / `[]KeyRangeRef` instead of pre-serialized `[]byte`.
5. **Drop `MarshalInto`**, `MarshalStructBlob`, `PackVectorOfStructBlobs`, standalone `WriteXxx`/`MarshalXxx` helpers. They become dead code.
6. **Add zero-copy `ParseXxxStringVector`** — generator emits sub-slice instead of `make+copy`. Drops 100-KV parse from 201 allocs to 1.

Each step is independently verifiable. Step 3 is the big switch — everything before it is additive.

### Performance Target

After full migration:

| Operation | Current | Target |
|---|---|---|
| GetKeyValuesRequest marshal | 660ns / 2 allocs | ~365ns / 1 alloc |
| CommitTransactionRequest (100 muts) | ~12,000ns / 108 allocs | ~2,000ns / 1 alloc |
| MutationRef blob (standalone) | 95ns / 1 alloc | N/A (inlined into commit) |
| ParseKeyValueRefStringVector (100 KVs) | 5,679ns / 201 allocs | ~3,000ns / 1 alloc |
| GetValueReply unmarshal | 56ns / 1 alloc | 56ns / 1 alloc (already optimal) |

### What Stays Hand-Written

1. **`wire/writer_direct.go`** — `DirectWriter` struct + `WriteBytesOOL`, `WriteRawOOL`, `WriteObject`, `PatchRelOff`, `MeasureBytesOOL`, `MeasureRawOOL`, `MeasureObject`. These are the runtime primitives. ~100 lines. Stable.

2. **`wire/reader.go`** — `Reader` struct + all `ReadT` methods. Already stable.

3. **`networkaddress_helpers.go`** — Endpoint reader chain (Go-side composition logic, no wire format knowledge). Will be eliminated when variant support + IPv6 is generated.

4. **Interface types** (CommitProxyInterface, GrvProxyInterface, StorageServerInterface) — `isDeserializing` post-processing (endpoint derivation). Needs `_custom.go` override.

5. **Client code** (`readpath.go`, `commitpath.go`, `grv.go`) — constructs generated structs, calls `MarshalFDB()`, parses replies. No wire format knowledge.
