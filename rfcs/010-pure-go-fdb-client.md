# RFC 010: Pure Go FoundationDB Client

## Status: In Progress

### Progress
- [x] Package structure: `pkg/fdbgo/wire/`, `cmd/extract/`, `cmd/generate/`
- [x] VTable generation (`wire/vtable.go`) — port of `detail::generate_vtable()`, 14 tests verified against C++ unit test assertions
- [x] FDB FlatBuffers writer (`wire/writer.go`) — inline scalars + out-of-line bytes, 9 round-trip tests
- [x] FDB FlatBuffers reader (`wire/reader.go`) — parses footer/vtable/object/out-of-line data
- [x] C++ test vector generator (`wire/testdata/gen_vectors.cpp`) — Bazel cc_binary using FDB's actual `flat_buffers.h` templates, outputs JSON with hex bytes
- [x] **Wire conformance** — 12 C++ conformance tests (byte-identical to `save_members()`): inline scalars, mixed types, strings, bool+double, vector<int32>, empty vector, Optional<int32> present/absent, Optional<string>, vector<string>, nested struct, nested struct with string. 32 tests total.
- [x] Nested structs with own vtable + ool data
- [x] Vector of strings (reverse element order matching C++ end-offset allocation)
- [x] C++ protocol extractor (`cmd/fdb-wire-schema-generator/`) — self-contained Bazel cc_binary with lightweight FDB type stubs. Extracts 30 messages into `wire_schema.json` via `bazelisk build //cmd/fdb-wire-schema-generator:wire_schema` (fully cached). `just wire-schema` copies output to `pkg/fdbgo/wire_schema.json`.
- [ ] Go code generator
- [ ] Transport layer
- [ ] Client logic
- [ ] Public API

## Problem

The Go FDB binding (`github.com/apple/foundationdb/bindings/go/src/fdb`) wraps `libfdb_c` via cgo. This causes:

1. **Build complexity.** cgo requires a C toolchain, breaks cross-compilation, slows incremental builds. CI needs `libfdb_c` installed or vendored.
2. **Runtime overhead.** Every FDB call pins a goroutine to an OS thread (cgo constraint), defeating Go's M:N scheduler. High concurrency → excessive threads.
3. **Debuggability.** Stack traces stop at cgo. pprof/delve can't see into the C client. Diagnosing timeouts requires C debugging tools.
4. **Deployment fragility.** `libfdb_c.so` must version-match the cluster. Wrong `.so` → cryptic failures. Pure Go binary eliminates this.
5. **Toolchain lock-in.** Doesn't work with TinyGo or alternative compilers. Static linking needs extra flags.

## Proposal

Build a pure Go FDB client using a two-stage code generation pipeline:

1. **C++ extractor** — compiles against actual FDB source via Bazel, extracts wire format metadata (vtable layouts, field types, sizes, alignments) into a JSON schema file.
2. **Go generator** — reads the JSON schema, emits optimized Go structs with zero-allocation FDB-format serialization methods.

The client logic (transaction lifecycle, cluster discovery, routing) is handwritten in Go.

## Architecture

```
foundationdb/                  (upstream C++ source, Bazel external dep)
    flow/flat_buffers.h        FDB's custom serialization format
    fdbclient/*Interface.h     Protocol message definitions (322 structs)

pkg/fdbgo/
    cmd/extract/               C++ tool: includes FDB headers, outputs wire_schema.json
    cmd/generate/              Go tool: reads wire_schema.json, emits wire/*.go
    wire_schema.json           Committed schema (regenerate on FDB version upgrade)
    wire/                      GENERATED Go structs + FDB-format serde + handwritten runtime
    transport/                 Handwritten — TCP, TLS, framing, multiplexing
    client/                    Handwritten — transaction lifecycle, routing
    fdb/                       Public API (drop-in replacement)
        subspace/              Vendor or rewrite (already pure Go upstream)
        tuple/                 Vendor or rewrite (already pure Go upstream)
```

### Key insight: let the C++ compiler be the parser

FDB's protocol messages use a custom FlatBuffers-inspired binary format (`flow/flat_buffers.h`). The wire layout depends on field sizes, alignments, and vtable generation — all computed by C++ template metaprogramming at compile time.

Instead of parsing C++ headers with regex (fragile) or reimplementing vtable layout in Go (error-prone), we compile a small C++ extractor against the actual FDB source. The C++ compiler resolves all templates, typedefs, inheritance, `sizeof`, and `alignof`. The extractor outputs JSON describing the exact wire layout. This is the **single source of truth** for wire compatibility.

## Design Decision: Schema Format

We considered three approaches for the intermediate representation between the C++ extractor and Go code generator.

### Option A: Proto-as-IDL

FDB messages described as `.proto` messages with custom field/message options carrying wire layout metadata.

```protobuf
message GetValueRequest {
    option (fdb) = { file_identifier: 8454530, vtable: [18,36,4,12,...] };
    bytes key = 1 [(fdb_field) = { vtable_slot: 0, wire_size: 4 }];
    int64 version = 2 [(fdb_field) = { vtable_slot: 1, wire_size: 8 }];
}
```

| | |
|---|---|
| **Pro** | Standard tooling (buf lint, IDE navigation, protoc --decode) |
| **Pro** | Proto descriptor API in Go (`protoreflect`) for the generator |
| **Con** | **Type gaps.** Proto has no 8-bit or 16-bit integers. UID (16 bytes fixed) and KeyRange (two byte seqs, no vtable) don't map to proto primitives. Custom options must override the proto type — the proto type becomes a lie, the option is the truth. |
| **Con** | **Proto wire format is irrelevant.** We never send these protos over the wire. Proto is designed for data interchange; we're using it as a codegen schema language. |
| **Con** | **Verbose.** Every field needs both a proto type AND options annotating the real wire semantics. Redundant. |
| **Con** | **Build dependency.** Requires protoc/buf in the build chain just for the schema format itself. |

### Option B: Custom proto (WireSchema message)

A purpose-built proto defines `WireSchema`, `MessageType`, `Field` etc. as data messages. The FDB messages are data inside the proto, not proto messages themselves.

```protobuf
message WireSchema {
    repeated MessageType messages = 1;
}
message MessageType {
    string name = 1;
    uint32 file_identifier = 2;
    repeated uint32 vtable = 3;
    repeated Field fields = 4;
}
```

| | |
|---|---|
| **Pro** | No type gaps — our own `FieldType` enum covers all FDB types (UINT8, UID, etc.) |
| **Pro** | Schema is the truth, not annotations on top of a different type system |
| **Con** | Still requires protoc to compile the WireSchema proto definition |
| **Con** | C++ extractor needs protobuf C++ library to serialize output |
| **Con** | Messages are "data in a proto" — less readable than direct representation |

### Option C: Custom JSON — chosen

Plain JSON with the exact FDB type system. No external schema language.

```json
{
  "fdb_version": "7.3.43",
  "protocol_version": "0x0FDB00B074000000",
  "messages": [{
    "name": "GetValueRequest",
    "file_identifier": 8454530,
    "reply_type": "GetValueReply",
    "vtable": [18, 36, 4, 12, 20, 24, 28, 32, 8],
    "object_size": 36,
    "fields": [
      {"name": "key",     "type": "bytes",   "vtable_slot": 0, "wire_size": 4, "inline": false},
      {"name": "version", "type": "int64",   "vtable_slot": 1, "wire_size": 8, "inline": true},
      {"name": "tags",    "type": "optional", "inner": "TagSet", "vtable_slot": 2},
      {"name": "reply",   "type": "uid",     "vtable_slot": 3, "wire_size": 16, "inline": true}
    ]
  }]
}
```

| | |
|---|---|
| **Pro** | **Exact representation.** `uint8` is `"uint8"`. `uid` is `"uid"`. No type system to fight. |
| **Pro** | **Zero build dependencies.** No protoc, no buf. C++ extractor does `printf`/nlohmann-json. Go generator does `encoding/json`. |
| **Pro** | **Trivially readable.** Open the file, see the layout. Diffable between FDB versions. |
| **Pro** | **Simpler extractor.** C++ just prints JSON — no protobuf C++ library dependency. |
| **Pro** | **Simpler generator.** `json.Unmarshal` into Go structs, walk and emit code. No proto descriptor API. |
| **Con** | No built-in schema validation (generator validates on read — it's the only consumer). |
| **Con** | String field names — typos caught at generator runtime, not compile time. |
| **Con** | Less "standard" — but it's an internal build artifact between two tools we own. |

### Decision

**JSON.** The schema is an internal build artifact. One tool writes it (C++ extractor), one tool reads it (Go generator). Nobody else touches it. Proto adds a type system that doesn't match FDB's types, a build dependency that doesn't serve us, and verbosity that obscures the wire layout. JSON is honest, zero-dependency, and trivially debuggable. Type safety lives in the generated `.go` files, not in the schema format.

## Package Layout

```
pkg/fdbgo/
├── wire/                 GENERATED + handwritten serde runtime
│   ├── messages.go       Generated: Go structs for each protocol message
│   ├── serialize.go      Generated: MarshalFDB/UnmarshalFDB per struct
│   ├── registry.go       Generated: file_identifier → factory + reply mapping
│   ├── reader.go         Handwritten: FDB FlatBuffers deserializer
│   ├── writer.go         Handwritten: FDB FlatBuffers serializer
│   └── vtable.go         Handwritten: vtable layout primitives
│
├── transport/            Handwritten: TCP/TLS, framing, multiplexing
│   ├── conn.go           Connection: framing, XXH3 checksum, read/write loops
│   ├── handshake.go      ConnectPacket exchange, protocol version negotiation
│   ├── endpoint.go       Endpoint token routing, pending response map
│   └── pool.go           Connection pool (one per peer address)
│
├── client/               Handwritten: NativeAPI in Go
│   ├── database.go       Cluster discovery, ClientDBInfo monitoring
│   ├── transaction.go    Transaction state machine, mutations, conflict ranges
│   ├── grv.go            Adaptive GRV batching
│   ├── locality.go       Key→storage server routing, cache invalidation
│   ├── commit.go         Commit path, commit_unknown resolution
│   ├── loadbalance.go    Replica selection, QueueModel, failover
│   ├── retry.go          OnError, exponential backoff with jitter
│   └── watch.go          Watch implementation (Phase 2)
│
└── fdb/                  Drop-in public API
    ├── fdb.go            APIVersion, OpenDatabase, Database, Transaction
    ├── future.go         FutureByteSlice, FutureNil, FutureKey, etc.
    ├── options.go        NetworkOptions, DatabaseOptions, TransactionOptions
    ├── range.go          RangeResult, RangeIterator, RangeOptions
    ├── tenant.go         Tenant
    ├── types.go          Key, KeyValue, KeySelector, KeyRange, Error
    ├── subspace/         Vendor or rewrite (already pure Go upstream)
    └── tuple/            Vendor or rewrite (already pure Go upstream)
```

### Dependency DAG (strict, no cycles)

```
wire  ←  transport  ←  client  ←  fdb
  ↑                                 ↑
  └── zero external deps            └── what users import
      (only stdlib)
```

- **`wire/`** depends on nothing (stdlib only). Generated code and handwritten serde runtime in same package — no import ceremony, helpers stay unexported.
- **`transport/`** depends on `wire/` only. Frames, sends, receives wire messages.
- **`client/`** depends on `transport/` + `wire/`. All the NativeAPI logic.
- **`fdb/`** depends on `client/`. Thin wrapper matching the apple/fdb API surface. This is the only package users import.

### Why `fdb/` is separate from `client/`

The public API has specific types (`FutureByteSlice`, `Transactor` interface, `RangeResult`) and compatibility constraints dictated by the existing Go binding. The client logic shouldn't be shaped by API compatibility — it should be shaped by what's correct. Thin adapter layer in `fdb/` translates between the internal client types and the public API.

### Codegen tools

```
pkg/fdbgo/
    cmd/extract/             C++ Bazel cc_binary (depends on @foundationdb)
    cmd/generate/            Go tool (reads JSON, emits wire/*.go)
    wire_schema.json         Committed build artifact
```

## Component 1: JSON Wire Schema

The schema file committed to the repo. Regenerated only when upgrading FDB versions. Contains everything the Go generator needs to produce correct, optimized serialization code.

### Schema structure

```json
{
  "fdb_version": "7.3.43",
  "protocol_version": "0x0FDB00B074000000",
  "messages": [
    {
      "name": "GetValueRequest",
      "file_identifier": 8454530,
      "reply_type": "GetValueReply",
      "base_classes": ["TimedRequest"],
      "vtable": [18, 36, 4, 12, 20, 24, 28, 32, 8],
      "object_size": 36,
      "fields": [
        {
          "name": "key",
          "type": "bytes",
          "vtable_slot": 0,
          "wire_size": 4,
          "wire_alignment": 4,
          "inline": false
        },
        {
          "name": "version",
          "type": "int64",
          "vtable_slot": 1,
          "wire_size": 8,
          "wire_alignment": 8,
          "inline": true
        },
        {
          "name": "tags",
          "type": "optional",
          "inner": "TagSet",
          "vtable_slot": 2,
          "wire_size": 4,
          "wire_alignment": 4,
          "inline": false
        },
        {
          "name": "reply",
          "type": "reply_promise",
          "vtable_slot": 3,
          "wire_size": 16,
          "wire_alignment": 8,
          "inline": true
        },
        {
          "name": "span_context",
          "type": "struct",
          "inner": "SpanContext",
          "vtable_slot": 4,
          "wire_size": 4,
          "wire_alignment": 4,
          "inline": false
        },
        {
          "name": "options",
          "type": "optional",
          "inner": "ReadOptions",
          "vtable_slot": 5,
          "wire_size": 4,
          "wire_alignment": 4,
          "inline": false
        },
        {
          "name": "ss_latest_commit_versions",
          "type": "struct",
          "inner": "VersionVector",
          "vtable_slot": 6,
          "wire_size": 4,
          "wire_alignment": 4,
          "inline": false
        }
      ]
    }
  ],
  "enums": [
    {
      "name": "MutationType",
      "underlying_type": "uint8",
      "values": [
        {"name": "SetValue", "value": 0},
        {"name": "ClearRange", "value": 1},
        {"name": "AddValue", "value": 2}
      ]
    }
  ]
}
```

### Field types

| Type string | C++ origin | Go output | Wire behavior |
|---|---|---|---|
| `"int8"` | `int8_t` | `int8` | 1 byte inline |
| `"int16"` | `int16_t` | `int16` | 2 bytes inline |
| `"int32"` | `int32_t` | `int32` | 4 bytes inline |
| `"int64"` | `int64_t`, `Version` | `int64` | 8 bytes inline |
| `"uint8"` | `uint8_t` | `uint8` | 1 byte inline |
| `"uint16"` | `uint16_t` | `uint16` | 2 bytes inline |
| `"uint32"` | `uint32_t` | `uint32` | 4 bytes inline |
| `"uint64"` | `uint64_t` | `uint64` | 8 bytes inline |
| `"bool"` | `bool` | `bool` | 1 byte inline |
| `"double"` | `double` | `float64` | 8 bytes inline |
| `"bytes"` | `Key`, `Value`, `StringRef` | `[]byte` | relative offset → len-prefixed |
| `"string"` | `std::string` | `string` | relative offset → len-prefixed |
| `"uid"` | `UID` | `[16]byte` | 16 bytes inline |
| `"optional"` | `Optional<T>` | `*T` | relative offset → 1-byte flag + value |
| `"vector"` | `VectorRef<T>`, `std::vector<T>` | `[]T` | relative offset → count + elements |
| `"map"` | `std::map<K,V>` | `map[K]V` | relative offset → count + pairs |
| `"struct"` | nested message | nested struct | relative offset → vtable-based |
| `"union"` | `std::variant<Ts...>` | interface + switch | type byte + relative offset |
| `"pair"` | `std::pair<A,B>` | struct{A,B} | inline or offset depending on size |
| `"enum"` | C++ enum | typed int | underlying type inline |
| `"reply_promise"` | `ReplyPromise<T>` | `UID` in Go struct | 16 bytes inline (endpoint token) |
| `"key_range"` | `KeyRange` | `KeyRange` struct | dynamic-size (two len-prefixed seqs) |

The `"inner"` field names the element/value type for compound types. The `"inline"` flag indicates whether the field is written directly into the object (scalars, UIDs) or via a relative offset to out-of-line data (bytes, vectors, structs).

## Component 2: C++ Protocol Extractor

A Bazel `cc_binary` that `#include`s FDB protocol headers and uses the template visitor pattern to extract wire layout metadata. Outputs `wire_schema.json`.

### How it works

FDB's `serializer(ar, field1, field2, ...)` dispatches to two paths based on the archiver type. When `is_fb_visitor = true`, the archiver receives the field list as a variadic template parameter pack. FDB already uses this for vtable collection (`InsertVTableLambda`). We write a similar visitor that extracts type metadata:

```cpp
// Simplified — real implementation handles all type categories
struct SchemaExtractor {
    static constexpr bool is_fb_visitor = true;
    static constexpr bool isDeserializing = false;
    static constexpr bool isSerializing = false;

    json current_message;  // nlohmann/json or manual printf

    template <class... Members>
    void operator()(const Members&... members) {
        // get_vtable<Members...>() returns the exact vtable FDB would use
        const auto* vt = detail::get_vtable<Members...>();
        current_message["vtable"] = json::array();
        for (size_t i = 0; i < vt->size(); i++) {
            current_message["vtable"].push_back((*vt)[i]);
        }
        current_message["object_size"] = (*vt)[1];

        int slot = 0;
        detail::for_each([&](const auto& member) {
            using M = std::decay_t<decltype(member)>;
            json field;
            field["vtable_slot"] = slot++;
            field["wire_size"] = detail::_SizeOf<M>::size;
            field["wire_alignment"] = detail::_SizeOf<M>::align;
            classify_type<M>(field);  // sets type, inner, inline
            current_message["fields"].push_back(field);
        }, members...);
    }
};

// Per-message extraction
template <class T>
json extract_message() {
    json msg;
    msg["name"] = demangle(typeid(T).name());
    msg["file_identifier"] = FileIdentifierFor<T>::value;

    SchemaExtractor extractor;
    T instance{};
    instance.serialize(extractor);
    msg["vtable"] = extractor.current_message["vtable"];
    msg["object_size"] = extractor.current_message["object_size"];
    msg["fields"] = extractor.current_message["fields"];
    return msg;
}

int main() {
    json schema;
    schema["fdb_version"] = "7.3.43";
    schema["protocol_version"] = currentProtocolVersion().versionWithFlags();
    schema["messages"] = json::array();

    schema["messages"].push_back(extract_message<GetValueRequest>());
    schema["messages"].push_back(extract_message<GetValueReply>());
    schema["messages"].push_back(extract_message<CommitTransactionRequest>());
    // ... ~50 messages needed for a basic client

    std::cout << schema.dump(2) << std::endl;
    return 0;
}
```

### What the extractor gives us for free

- **Exact vtable layout.** `generate_vtable()` sorts fields by size descending and computes aligned offsets. The C++ compiler computes it identically to what the FDB server uses.
- **Correct sizeof/alignof.** `_SizeOf<Optional<TagSet>>` resolves through templates. No guessing.
- **Type resolution.** `Version` → `int64_t`, `Key` → `StringRef`, `Standalone<StringRef>` → bytes. The compiler does it.
- **Inherited fields.** `TimedRequest`, `LoadBalancedReply` base class fields appear in the serializer call. The visitor sees them.

### Bazel integration

```python
cc_binary(
    name = "extract",
    srcs = ["extract.cpp"],
    deps = [
        "@foundationdb//flow",
        "@foundationdb//fdbclient:fdbclient_lib",
        "@nlohmann_json//:json",
    ],
)

genrule(
    name = "extract_wire_schema",
    tools = [":extract"],
    outs = ["wire_schema.json"],
    cmd = "$(location :extract) > $@",
)
```

### Extractor scope: ~50 messages, not 322

A basic client only needs messages for:
- Coordination: `OpenDatabaseCoordRequest/Reply` (~4 messages)
- GRV: `GetReadVersionRequest/Reply` (~2 messages)
- Reads: `GetValueRequest/Reply`, `GetKeyValuesRequest/Reply`, `GetKeyRequest/Reply` (~6 messages)
- Writes: `CommitTransactionRequest/Reply` + `MutationRef` + `CommitTransactionRef` (~6 messages)
- Watches: `WatchValueRequest/Reply` (~2 messages, Phase 2)
- Key location: `GetKeyServerLocationsRequest/Reply` (~2 messages)
- Infrastructure: `Endpoint`, `NetworkAddress`, `UID`, `SpanContext`, `VersionVector` (~10 types)
- Interface structs: `CommitProxyInterface`, `GrvProxyInterface`, `StorageServerInterface` (~5)
- ClientDBInfo (~1)

The remaining ~270 are internal server-to-server messages (TLog, DataDistributor, Restore, etc.) that a client never sends or receives.

## Component 3: Go Code Generator

A Go tool that reads `wire_schema.json` and emits optimized Go source files into `wire/`.

### Generated output

For each message in the schema:

**Go struct:**
```go
type GetValueRequest struct {
    Key        []byte
    Version    int64
    Tags       *TagSet
    Reply      UID       // ReplyPromise token
    SpanCtx    SpanContext
    Options    *ReadOptions
    SSVersions VersionVector
}
```

**Optimized FDB-format serializer** with baked-in constants:
```go
// GENERATED — do not edit
// Source: wire_schema.json (extracted from foundationdb tag v7.3.43)

const getValueRequest_fileID uint32 = 8454530
const getValueRequest_objectSize = 36

// Vtable offsets — literal constants from C++ extractor
const (
    getValueRequest_off_key         = 4   // vtable[2]
    getValueRequest_off_version     = 12  // vtable[3]
    getValueRequest_off_tags        = 20  // vtable[4]
    getValueRequest_off_reply       = 24  // vtable[5]
    getValueRequest_off_spanContext = 28  // vtable[6]
)

func (m *GetValueRequest) MarshalFDB(buf []byte) ([]byte, error) {
    // Single allocation: vtable size + object size + variable-length data
    // All offsets are integer literals — compiler inlines completely
    // Two-pass: measure variable-length fields, allocate once, write everything
    ...
}

func (m *GetValueRequest) UnmarshalFDB(data []byte) error {
    // Root offset + file_id validation
    // Read vtable pointer from object
    // Each field: object_base + literal_offset → value
    // Bounds checks eliminated by compiler for known-size reads
    ...
}

func (m *GetValueRequest) FileIdentifier() uint32 { return 8454530 }
```

**Registry:**
```go
var MessageRegistry = map[uint32]func() Message{
    8454530: func() Message { return &GetValueRequest{} },
    // ...
}

var ReplyTypes = map[uint32]uint32{
    8454530: 12346,  // GetValueRequest → GetValueReply
    // ...
}
```

### Serde runtime (`wire/reader.go`, `wire/writer.go`)

Handwritten, lives in the same package as generated code. The generated code calls these helpers directly (no import needed):

```go
// writer.go — builds FDB-format serialized messages
type fdbWriter struct {
    buf []byte
    off int
}

func (w *fdbWriter) writeScalar(objectBase, offset int, data []byte)
func (w *fdbWriter) writeBytes(objectBase, offset int, data []byte)
func (w *fdbWriter) writeOptional(objectBase, offset int, present bool, writeValue func())
func (w *fdbWriter) writeVector(objectBase, offset int, count int, writeElem func(i int))
func (w *fdbWriter) finish(vtableData []byte, fileIdentifier uint32) []byte

// reader.go — deserializes FDB-format messages
type fdbReader struct {
    data       []byte
    objectBase int
    vtableBase int
}

func newFDBReader(data []byte) (fdbReader, error)
func (r *fdbReader) readScalar(offset, size int) []byte
func (r *fdbReader) readBytes(offset int) []byte
func (r *fdbReader) readOptional(offset int) ([]byte, bool)
func (r *fdbReader) readVector(offset int) (count int, elemData []byte)
```

The vtable layout is baked into the generated code as constants. The runtime just reads/writes at known offsets — no schema lookup, no maps, no reflection.

### Bazel wiring

```python
genrule(
    name = "generate_wire",
    srcs = [":wire_schema.json"],
    tools = ["//pkg/fdbgo/cmd/generate"],
    outs = ["wire/messages.go", "wire/serialize.go", "wire/registry.go"],
    cmd = "$(location //pkg/fdbgo/cmd/generate) -schema $(location :wire_schema.json) -out $(RULEDIR)/wire/",
)
```

## Component 4: Transport Layer

Handwritten Go package. TCP connections to FDB processes.

### Wire framing

```
Non-TLS:  [4B LE length][8B XXH3-64 checksum][16B endpoint UID][FDB-serialized body]
TLS:      [4B LE length][16B endpoint UID][FDB-serialized body]
```

- Length field = payload size (excludes itself, excludes checksum)
- Checksum = `XXH3_64bits(payload)` — only on non-TLS connections
- Endpoint UID = destination token (two LE uint64s)
- Body = FDB-format serialized message (generated code)

### Connection handshake

Both sides exchange a `ConnectPacket` (44 bytes, `#pragma pack(push, 1)`):

```
Offset  Size  Field
0       4     connectPacketLength (uint32 LE) = 40
4       8     protocolVersion (uint64 LE, with objectSerializerFlag bit 60 set)
12      2     canonicalRemotePort (uint16 LE)
14      8     connectionId (uint64 LE)
22      4     canonicalRemoteIp4 (uint32 LE)
26      2     flags (uint16 LE, bit 0 = FLAG_IPV6)
28      16    canonicalRemoteIp6 (only if FLAG_IPV6)
```

Protocol version compatibility: top 48 bits must match (`& 0xFFFFFFFFFFFF0000`). Bottom 16 bits are free. The `objectSerializerFlag` (`0x1000000000000000`) signals FlatBuffers serialization (always set in modern FDB).

TLS: full TLS handshake happens first (not StartTLS), then ConnectPacket exchange. Go's `crypto/tls` works directly.

### Request/response multiplexing

FDB uses **endpoint tokens** (UIDs), not sequence numbers. Each `ReplyPromise<T>` creates a one-shot local endpoint with a random 16-byte token. The response is routed back to that token.

```go
type Connection struct {
    conn    net.Conn
    pending sync.Map  // UID → chan Message
}

func (c *Connection) Send(ctx context.Context, dest Endpoint, msg Message) (<-chan Message, error) {
    replyToken := newUID()
    ch := make(chan Message, 1)
    c.pending.Store(replyToken, ch)
    // Serialize: [endpoint token][message with reply token embedded]
    // Write frame: [length][checksum?][payload]
    ...
    return ch, nil
}

func (c *Connection) readLoop() {
    for {
        token, data := c.readFrame()
        if ch, ok := c.pending.LoadAndDelete(token); ok {
            ch.(chan Message) <- deserialize(data)
        }
    }
}
```

Responses are wrapped in `ErrorOr<T>`: first 4 bytes = error code (0 = success, then T follows; nonzero = FDB error, no payload).

## Component 5: Client Logic

Handwritten Go. Reimplements `NativeAPI.actor.cpp` (~6,800 lines, ~75 ACTOR functions) in idiomatic Go.

### 5.1 Cluster discovery

Read `fdb.cluster` → connect to coordinators → send `OpenDatabaseCoordRequest` (long-poll) → receive `ClientDBInfo` with proxy addresses. Background goroutine monitors for topology changes.

### 5.2 GRV batching

Adaptive dynamic batching matching Java's `readVersionBatcher`:
- Collect requests over a dynamic window: `batchTime = 0.1 * (replyLatency * 0.5) + 0.9 * batchTime`
- Send single `GetReadVersionRequest` to GRV proxy
- Fan out version to all waiting goroutines

Static batching kills latency. The adaptive window is critical.

### 5.3 Read path

```
Transaction.Get(key)
  → getReadVersion() (batched GRV)
  → locateKey(key) (locationCache → CommitProxy GetKeyServerLocations if miss)
  → loadBalance(StorageServers, GetValueRequest{key, version})
  → on wrong_shard_server: invalidate cache, retry after delay
```

`loadBalance` picks server by locality + QueueModel latency estimation. Fails over to replicas. ~350 lines in Java — significant but contained.

### 5.4 Write path

Mutations buffered locally. On `Commit()`:
- Size check (10MB limit)
- Self-conflicting range injection (for `commit_unknown_result` resolution)
- Send `CommitTransactionRequest` to commit proxy (AtMostOnce — no auto-retry)
- On conflict (`not_committed`): return retryable error
- On `commit_unknown_result`: dummy transaction to resolve status

### 5.5 Atomic operations

Just mutation types in the commit payload — `MutationRef.Type` enum:
`SetValue(0)`, `ClearRange(1)`, `AddValue(2)`, `And(3)`, `Or(4)`, `Xor(5)`, `AppendIfFits(6)`, `Max(7)`, `Min(8)`, `SetVersionstampedKey(9)`, `SetVersionstampedValue(10)`, `ByteMin(11)`, `ByteMax(12)`, `MinV2(13)`, `AndV2(14)`, `CompareAndClear(15)`.

Client appends `MutationRef{type, key, operand}` to the transaction. Proxy/SS apply semantics. No client-side read needed.

Note: API version >= 510 remaps `Min→MinV2`, `And→AndV2` silently.

### 5.6 Transaction retry (`OnError`)

Retryable errors (exponential backoff with jitter):
- `not_committed` (1020) — conflict
- `commit_unknown_result` (1021)
- `transaction_too_old` (1007) — shorter delay
- `future_version` (1009) — shorter delay
- `database_locked` (1039)
- `proxy_memory_limit_exceeded` (1042)
- `batch_transaction_throttled` (1051)
- `tag_throttled` (1213)
- `process_behind` (1037)

All others: non-retryable, propagate to caller.

### 5.7 Watches (Phase 2)

Long-poll to storage server via `WatchValueRequest`. SS holds connection open until value changes. Go maps this to a goroutine blocked on channel receive. Deduplication per-Database (multiple transactions watching same key share one SS poll).

## Component 6: Public API

Drop-in replacement for `github.com/apple/foundationdb/bindings/go/src/fdb`:

```go
package fdb

func APIVersion(version int) error
func OpenDatabase(clusterFile string) (Database, error)

type Database struct { ... }
func (db Database) CreateTransaction() (Transaction, error)
func (db Database) Transact(func(Transaction) (interface{}, error)) (interface{}, error)
func (db Database) ReadTransact(func(ReadTransaction) (interface{}, error)) (interface{}, error)

type Transaction struct { ... }
func (t Transaction) Get(key KeyConvertible) FutureByteSlice
func (t Transaction) Set(key KeyConvertible, value []byte)
func (t Transaction) Clear(key KeyConvertible)
func (t Transaction) ClearRange(er ExactRange)
func (t Transaction) GetRange(r Range, options RangeOptions) RangeResult
func (t Transaction) Commit() FutureNil
func (t Transaction) OnError(e Error) FutureNil
func (t Transaction) Watch(key KeyConvertible) FutureNil
// + all 12 atomic ops, Snapshot(), GetCommittedVersion(), GetVersionstamp(), etc.

type Tenant struct { ... }
func (t Tenant) CreateTransaction() (Transaction, error)
func (t Tenant) Transact(func(Transaction) (interface{}, error)) (interface{}, error)
```

The `subspace` and `tuple` packages are already pure Go in the upstream binding and can be vendored directly.

### Record layer API surface

Our record layer uses the following:
- All 12 atomic ops (ADD, BYTE_MAX, SET_VERSIONSTAMPED_KEY, etc.)
- `Tenant` (CreateTransaction, Transact)
- `Snapshot` reads
- `RangeResult` with `Iterator()` / `GetSliceWithError()`
- All `KeySelector` constructors
- All `StreamingMode` constants
- `Subspace` (Sub, Pack, Unpack, Bytes, FDBRangeKeys, Contains)
- `Tuple` (Pack, Unpack, Versionstamp)

## Build Pipeline

```
┌─────────────────────┐                        ┌────────────────────┐
│ FDB C++ source      │                        │ Generated Go code  │
│ (@foundationdb)     │                        │ wire/*.go          │
│                     │                        └────────────────────┘
│ flow/flat_buffers.h │                                 ▲
│ fdbclient/*         │                                 │
└─────────────────────┘                                 │
         │                                              │
         ▼                                              │
┌──────────────────────┐    ┌───────────────┐    ┌──────────────────┐
│ cmd/extract (C++)    │───▶│ wire_schema   │───▶│ cmd/generate     │
│ Bazel cc_binary      │    │ .json         │    │ (Go)             │
│                      │    │ (committed)   │    │ reads JSON,      │
│ outputs JSON         │    └───────────────┘    │ emits wire/*.go  │
└──────────────────────┘                         └──────────────────┘
```

Bazel orchestrates the pipeline:
1. `cc_binary` compiles `cmd/extract` against FDB source
2. `genrule` runs extractor → `wire_schema.json`
3. `genrule` runs `cmd/generate` on `wire_schema.json` → `wire/*.go`
4. `go_library` compiles generated + handwritten Go code in `wire/`
5. Gazelle manages BUILD files for all Go packages

The `wire_schema.json` is committed to the repo. Regeneration happens only when upgrading FDB versions.

## Testing Strategy

### Protocol conformance (highest priority)

Write a C++ test that serializes each message type with known field values using FDB's `ObjectWriter`, captures the raw bytes, and asserts our Go `MarshalFDB()` produces identical output. Runs as a Bazel test. This is the strongest possible guarantee — same C++ code path as the real FDB server.

Alternative: capture real wire traffic between `libfdb_c` and an FDB cluster via TCP proxy. Deserialize with both clients. Field-by-field comparison.

### Binding tester

FDB's official binding test suite (`bindings/bindingtester`). 47 core API operations + 21 directory ops. Stack-machine based. The existing Go stack tester is 962 lines. We implement the same stack tester against our API.

We need `scripted` + `api` tests. Directory tests needed only in Phase 2.

### Transaction semantics

Run the record layer's own test suite against the pure Go client. 894 Ginkgo specs + 270 conformance specs covering CRUD, split records, atomic indexes, continuations, versioning. If those pass, the client is correct for our use case.

### Fuzz testing

- Fuzz `fdbReader` with random bytes → no panics
- Fuzz deserialization of each message type with mutated captured traffic
- Fuzz `ConnectPacket` parser

### Serialization round-trip

For each generated message type: populate all fields → `MarshalFDB()` → `UnmarshalFDB()` → assert equal. Runs as a Go test.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| FDB custom FlatBuffers format underdocumented | Serialization bugs | C++ extractor uses identical code path as FDB server; C++ round-trip tests |
| NativeAPI.actor.cpp subtleties (GRV batching, version vectors, locality) | Correctness | Port incrementally; test against real cluster at each step; binding tester |
| VTable layout divergence | Wire incompatibility | VTable computed by C++ compiler, baked into JSON, not reimplemented |
| Protocol changes between FDB versions | Client breakage | Pin to release tags; protocol stable within major version; re-extract on upgrade |
| No multi-version client (Phase 3) | Can't do rolling cluster upgrades | Match client version to cluster version (same as any other client library) |
| TLS cipher suite mismatch | Connection failures | Go `crypto/tls` covers standard suites; test against TLS-enabled clusters |

## Scope

### Phase 1 (v1)
- C++ protocol extractor → JSON wire schema
- Go code generator → optimized wire/*.go
- FDB serialization runtime (FlatBuffers format)
- TCP/TLS transport with multiplexing
- Core transaction API: Get, GetKey, GetRange, Set, Clear, ClearRange, Commit, OnError
- All 12 atomic mutation types
- GRV batching (adaptive)
- Key locality caching and routing
- Cluster discovery and topology monitoring
- Transaction retry logic
- Tenant API
- Drop-in compatible public API
- Subspace + Tuple (vendor from upstream, already pure Go)

### Phase 2
- Watch API
- Directory layer
- Version vector support (causal consistency optimization — correct without it, just slower)
- Query-based tag throttling (client-side throttle enforcement)
- Performance parity benchmarking vs cgo client

### Phase 3
- Multi-version client (plugin loading for older client versions)
- FDB status JSON parsing
- Backup/restore protocols

## Milestones

**M1 — Extractor + Codegen (4 weeks)**
C++ extractor compiles against FDB source, outputs `wire_schema.json`. Go generator produces compilable `wire/` package. Serialization round-trip tests pass. C++ conformance tests validate byte-identical output.

**M2 — Transport + Handshake (3 weeks)**
TCP connection, ConnectPacket exchange, protocol version negotiation, message framing, XXH3 checksum, request/response multiplexing via endpoint tokens. Can connect to a real FDB cluster.

**M3 — Read Path (4 weeks)**
Cluster discovery (coordinator → ClientDBInfo), GRV (adaptive batching), key locality routing, `Get` and `GetRange`. Reads work against a real cluster.

**M4 — Write Path + Retry (4 weeks)**
`Set`, `Clear`, `ClearRange`, `Commit` with conflict detection. All 12 atomic ops. `OnError` with retry logic. Self-conflicting transaction injection. Passes basic binding tester.

**M5 — Tenant + Full API (3 weeks)**
Tenant support. Snapshot reads. RangeOptions. GetKey (key selectors). Full binding tester compliance (scripted + api). GetVersionstamp. GetCommittedVersion.

**M6 — Hardening (4 weeks)**
Fuzz testing. TLS support. Connection failover. Benchmarking vs cgo client. Run record layer test suite. Production readiness review.

**Total: ~22 weeks**

## References

- [FoundationDB Source](https://github.com/apple/foundationdb) (tag 7.3.43)
- `flow/flat_buffers.h` — FDB custom serialization format (~1400 lines)
- `flow/flat_buffers.cpp` — `generate_vtable()` implementation
- `flow/serialize.h` / `ObjectSerializer.h` — `ObjectWriter`/`ObjectReader` + `ISerializeSource`
- `fdbrpc/FlowTransport.actor.cpp` — framing, handshake, `sendPacket`, `scanPackets`
- `fdbclient/NativeAPI.actor.cpp` — client transaction lifecycle (~6800 lines, ~75 actors)
- `fdbclient/CommitProxyInterface.h` — 25 message types
- `fdbclient/StorageServerInterface.h` — 48 message types
- `bindings/go/src/fdb/` — existing Go binding (API surface to match)
- `bindings/bindingtester/` — official binding test suite
