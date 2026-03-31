# FDB Wire Format: Static Schema vs Runtime Logic

## Static (extracted by C++ → JSON, used by Go codegen)

| Property | Source | Per-type or per-field | Example |
|---|---|---|---|
| VTable | `get_vtable<Fields...>()` | Per type | `[10, 13, 4, 12, 8]` |
| VTable closure | `get_vtableset_impl()` | Per message | All reachable vtables |
| File identifier | `T::file_identifier` | Per message | `8454530` |
| Field count | Template parameter pack `sizeof...(Members)` | Per type | 3 |
| Field trait | `is_scalar<T>`, `is_dynamic_size<T>`, etc. | Per field | `"scalar"`, `"serialize_member"` |
| Field wire size | `fb_size<T>` | Per field | 4, 8, 16 |
| Field alignment | `fb_align<T>` | Per field | 1, 4, 8 |
| Field indirection | `use_indirection<T>` | Per field | true = RelOff, false = inline |
| FakeRoot structure | Always `{6, 8, 4}` | Global | Same for all messages |
| ErrorOr union layout | `{8, 9, 8, 4}` — type at offset 8, value at offset 4 | Global | Same for all replies |
| Optional expansion | 2 vtable slots: type byte + value RelOff | Per optional field | Always the same |

## Logic (ported to Go as code, not generated)

| Behavior | Why it's not static | Example |
|---|---|---|
| MutationRef ClearRange packing | Data-dependent: `equalsKeyAfter(begin, end)` changes field order | Send: `(type, end, empty)` if single-key range |
| MutationRef ClearRange unpacking | Receiver reconstructs: if `end == ""` then `end = begin`, `begin = end[:-1]` | Receive: detect and reconstruct |
| MutationRef checksum | Protocol-version-dependent: `hasMutationChecksum()` appends 4 bytes to param2 | Only FDB 7.3+ with config flag |
| KeyRangeRef single-key optimization | Data-dependent: `equalsKeyAfter(begin, end)` → `(end, empty)` | Same pattern as MutationRef |
| ReplyPromise token | `save/load` traits (NOT `serialize`) — opaque 16-byte blob | Hand-write: just 2×uint64 |
| VersionVector empty size | `dynamic_size_traits::size()` returns 16 for empty (sizeof(size_t) + sizeof(Version)) | Hand-write: `make([]byte, 16)` |
| TagSet serialization | `dynamic_size_traits` with custom save/load | Hand-write if needed |
| VectorRef element format | `serialize_member` elements = nested FlatBuffers objects; `dynamic_size_traits` elements = length-prefixed blobs; `VecSerStrategy::String` = inline packed | Codegen knows trait → picks format |
| Connection PING reply | Ground-truth bytes for `ErrorOr<EnsureTable<Void>>` | Hand-write (48 bytes) |
| Endpoint token adjustment | `getAdjustedEndpoint(n)`: first += n<<32, second.lower32 += n | Hand-write |

## Boundary: what codegen CAN generate from static schema

| Generated code | Input | Output |
|---|---|---|
| VTable constant | `vtable` from JSON | `var Foo_VTable = wire.VTable{...}` |
| VTable closure constant | `vtable_closure` from JSON | `var Foo_VTableClosure = []wire.VTable{...}` |
| File ID constant | `file_identifier` from JSON | `const Foo_FileIdentifier = 8454530` |
| `WriteStruct` call for serialize_member fields | trait=serialize_member, field's own vtable | `obj.WriteStruct(offset, fieldVTable, align, func(inner) { ... })` |
| `WriteBytes` call for dynamic_size fields | trait=dynamic_size | `obj.WriteBytes(offset, data)` |
| Inline scalar write | trait=scalar, size known | `obj.WriteInt64(offset, value)` |
| Optional slots | trait=union_like | type byte + value RelOff (2 slots) |
| `WriteMessageWithVTables` wrapper | closure from JSON | Correct vtable set in output |
| `UnmarshalFDB` reader | vtable slots + traits | `r.ReadInt64(slot)`, `r.ReadBytes(slot)` |
