# Pure Go FoundationDB Client

A native Go FoundationDB client — no cgo or `libfdb_c` dependency. It speaks the
FDB 7.3 wire protocol, with **7.3.77** as the implementation and test reference.
It is not a multi-version client; see the [cluster-upgrade guidance](../../docs/upgrade.md).

## Why

The official Go binding (`github.com/apple/foundationdb/bindings/go`) wraps `libfdb_c` via cgo. This creates deployment friction (must ship the C library), cross-compilation pain, and CGo overhead. This package eliminates all of that.

## Status

**Pre-1.0; not declared production-ready.** This is an independent implementation,
not a promise of complete Apple C binding API parity. Some options are unsupported
and others are accepted without effect; consult the [option behavior matrix](fdb/OPTIONS.md)
before migrating. In particular, authorization tokens, automatic idempotency,
conflicting-key reporting and special-key-space operations have support boundaries.
See [project status](../../STATUS.md) for validation scope and
[compatibility](../../docs/compatibility.md) before choosing a deployment.

Implemented areas with tests against the FDB 7.3.77 reference include:
- `Get`, `GetKey`, `GetRange` (multi-shard, all streaming modes), `GetEstimatedRangeSizeBytes`, `GetRangeSplitPoints`
- `Set`, `Clear`, `ClearRange`, all 14 atomic mutation types
- `Transact` with automatic retry for retryable FDB errors, including `tag_throttled` and `cluster_version_changed`
- MVCC conflict detection, `OnError`, exponential backoff with configurable `MaxRetryDelay`
- Coordinator bootstrap, GRV batching, opt-in GRV cache (`USE_GRV_CACHE`, as in libfdb_c), storage server location discovery
- Snapshot reads, Watch (long-poll), Versionstamp, Tenants (CRUD via system keys)
- Transaction options: RYW disable, snapshot RYW disable, size limit, timeout, retry limit, lock-aware
- `GetPipelined` for true request pipelining (no goroutine per Get)
- TLS support (mutual auth + CA cert), QueueModel load balancing (C++ Smoother + server penalty), connection keep-warm
- Read-your-writes cache with full atomic op merging (all 14 types mirror C++ `Atomic.h`)
- `LocalityGetAddressesForKey`, `LocalityGetBoundaryKeys`, `OpenWithConnectionString`, `GetClientStatus`

## Retries and timeouts are UNBOUNDED by default — read this before migrating

A transaction created with no options has **no timeout and no retry limit**. A `Transact` against a down or
unreachable cluster retries until the cluster returns or the caller stops it. Nothing internal stops it.

**This is not a divergence from `libfdb_c` — it is `libfdb_c`'s behaviour, matched deliberately.** The C++
client's per-transaction defaults (`ReadYourWrites.actor.cpp:2078-2082`) are:

```cpp
void ReadYourWritesTransactionOptions::reset(Transaction const& tr) {
	memset(this, 0, sizeof(*this));
	timeoutInSeconds = 0.0;
	maxRetries = -1;
```

and `resetTimeout()` (`:1576-1578`) arms the `timebomb` actor **only** when `timeoutInSeconds` is non-zero, so
by default no timer exists at all. `fdb.options` says the same of the timeout option: *"If set to 0, will
disable all timeouts."* A default-configured `libfdb_c` transaction hangs against a dead cluster exactly as
long as this one does. Do not read "no internal timeout" as a Go weakness to work around — both clients hand
the bound to the caller. That is the FoundationDB contract.

### Where this client is *stricter* than `libfdb_c`

**Bootstrap** — the initial coordinator connection — is internally capped at 60s
(`defaultBootstrapTimeout`, `fdb/database.go:139-153`) whenever the caller supplies no deadline of its own.
`libfdb_c` has no equivalent bound and waits indefinitely. So `fdb.OpenDatabase` against an unreachable cluster
**fails fast** where the C client would not. Only the *live* database — post-open reconnection and the
transaction retry loop — is unbounded.

### What you must do

Pick at least one bound for every transaction. Any of these terminates it:

| Bound | Applies to | Notes |
|---|---|---|
| `Transaction.SetTimeout` / `DatabaseOptions.SetTransactionTimeout` | the whole transaction | Direct analog of `libfdb_c`'s timeout option and C++ `timebomb`. Cancels **in-flight** RPC waits, not just the gaps between them (`client/readpath.go:89-100`, RFC-112); surfaces `transaction_timed_out` (1031). |
| `Transaction.SetRetryLimit` / `DatabaseOptions.SetTransactionRetryLimit` | the retry loop | The most recent error escapes once the cap is hit. |
| `Database.TransactCtx` / `ReadTransactCtx` with a deadline `ctx` | retry loop, backoff, and reads | Go's **extra** bound (RFC-090) — not a substitute for a `libfdb_c` mechanism that does not exist. |

The no-context `Database.Transact` runs on `context.Background()` to stay drop-in compatible with the Apple Go
binding, so a `ctx` deadline is not available to it — bound it with a timeout or retry limit, or use
`TransactCtx`.

Migrating from `libfdb_c`: if you set a transaction timeout there, keep setting it here — it works the same
way. If you set nothing there, you were already unbounded there, and you are equally unbounded here.

Pinned by `client/unbounded_default_pin_test.go` (default is unbounded; each of the three bounds terminates)
and `fdb/unbounded_default_pin_test.go` + `fdb/database_bootstrap_test.go` (no internal deadline on the bare
`Transact` path; the 60s bootstrap cap). Full detail: `go doc fdb.dev/pkg/fdbgo`.

## Architecture

```
pkg/fdbgo/
├── client/          Transaction lifecycle, retry logic, read/commit paths
├── transport/       TCP framing, ConnectPacket handshake, connection multiplexing
├── wire/            FDB FlatBuffers framework: Writer, Reader, VTable computation
│   └── types/       One file per FDB message type + vtables_generated.go
└── cmd/             (at repo root: cmd/fdb-schema-extract/)
```

### `wire/` — Serialization framework

FDB uses a custom FlatBuffers-like binary format (NOT Google FlatBuffers). Each serialized message has:
- A **vtable** — array of uint16 field offsets, determines where each field lives in the object body
- An **object body** — fixed-size struct with fields at vtable-specified offsets
- **Out-of-line data** — variable-length fields (strings, vectors, nested structs) referenced via RelativeOffsets

The `wire` package implements this format:
- `Writer` / `ObjectWriter` — serialize messages. Handles vtable emission, soffset computation, RelativeOffset patching, nested struct allocation, OOL data.
- `Reader` — deserialize messages. Navigates FakeRoot wrapper, reads fields by vtable slot index. Self-describing: reads the vtable from the wire data, so forward/backward compatible.
- `VTable` — the type alias `[]uint16`. Generated constants in `vtables_generated.go`.
- `ReadErrorOr` — unwraps FDB's `ErrorOr<T>` union responses.
- `MarshalStructBlob` / `PackVectorOfStructBlobs` — serialize vector elements for `VectorRef<serialize_member>`.
- `WriteRootObject` — for `union_like_traits` types (ErrorOr) where the root object IS the union (no FakeRoot wrapper).

### `wire/types/` — Message types

Each FDB message type (request or reply) has a Go file with:
- A struct with typed fields
- `MarshalFDB()` for requests — constructs the full wire message using vtable-derived offsets
- `UnmarshalFDB()` / `UnmarshalFrom()` for replies — reads fields via `wire.Reader`

Generated vtable constants describe the pinned FDB wire layout. Regeneration alone
is not an upgrade guarantee: protocol changes can also require new types and
behavior, followed by compatibility testing against the new reference.

Shared write helpers (`WriteReplyPromise`, `WriteTenantInfo`, `writeKeySelectorRef`) encapsulate common nested struct patterns.

### `vtables_generated.go` — The bridge between C++ and Go

This is the single source of truth for wire layout. Generated by `cmd/fdb-schema-extract/`, a C++ binary that compiles against real FDB headers and extracts:

- **VTable constants** — field byte offsets, computed by C++ template metaprogramming (`get_vtable<Fields...>()`)
- **VTable closures** — all vtables transitively reachable from a message type
- **File identifiers** — per-message type hashes
- **Slot constants** — Reader slot indices with field names (e.g., `ClientDBInfoSlotGrvProxies = 0`)

28 types extracted. Slot indices computed mechanically from C++ traits (union_like = 2 slots, everything else = 1). Field names parsed from C++ source via `name_capture.cpp`.

### `transport/` — TCP layer

- `Conn` — multiplexed FDB connection. Multiple concurrent requests share one TCP connection, matched by endpoint tokens (128-bit UIDs).
- `ConnectPacket` — FDB handshake (protocol version negotiation, IPv4/IPv6).
- Frame format: `[packetLen(4)][xxh3Checksum(8)][destToken(16)][body]`.
- PING keepalive: responds to server PINGs, sends outbound PINGs via `connectionMonitor` (matches C++ FlowTransport). Detects dead connections in ~3.5s.

### `client/` — Transaction lifecycle

- `Database` — connection manager. `Transact()` with automatic retry.
- `Transaction` — buffered mutations, read conflict tracking, GRV, commit.
- `GRVBatcher` — batches concurrent `GetReadVersion` calls into single RPC.
- `LocationCache` — maps keys to storage server addresses via `GetKeyServerLocationsRequest`.
- Connection pool with port-match reuse (handles Docker address translation).

## FDB version compatibility

This client targets **FDB 7.3.x**. The wire format is defined by C++ code, not an IDL. Compatibility with other versions requires:

1. Matching the **protocol version** in the ConnectPacket handshake
2. Matching the **vtable layouts** for all message types we send
3. Handling any **new fields** in responses (the Reader is forward-compatible — unknown vtable entries are ignored)

### Upgrading to a new FDB version

```
# 1. Checkout the FDB source at the target version
cd /path/to/foundationdb
git checkout 7.4.0  # or whatever

# 2. Rebuild the C++ vtable extractor
cd /path/to/this-repo
bash cmd/fdb-schema-extract/build.sh /path/to/foundationdb pkg/fdbgo/wire/types/vtables_generated.go

# 3. Diff the generated output
git diff pkg/fdbgo/wire/types/vtables_generated.go

# 4. For each changed type:
#    - If only vtable VALUES changed (field offsets shifted): no Go code changes needed,
#      the MarshalFDB methods use int(vt[N]) which adapts automatically.
#    - If SLOT COUNT changed (field added/removed): update the MarshalFDB method to
#      write/skip the new field. Update the slot constant references.
#    - If a NEW message type was added: create a new Go file in wire/types/.

# 5. Update the protocol version constant in transport/
#    (ProtocolVersion73 → ProtocolVersion74)

# 6. Run tests against a testcontainer at the new version
```

The key insight: **vtable constants are the only C++ build-time artifact**. Everything else is Go code that references these constants. The C++ extractor bridges C++'s compile-time template metaprogramming (which Go can't do) into static Go constants.

### What changes between FDB versions

| What | Frequency | Impact |
|---|---|---|
| Field offsets within a type | Rare (layout algorithm is stable) | Automatic — vtable constants absorb it |
| New fields added to existing types | Occasional | Add the field to MarshalFDB, or leave absent (zero = Optional not present) |
| New message types | Rare | New Go file in wire/types/ |
| Protocol version number | Every major version | Update `ProtocolVersion` constant |
| Endpoint indices (method ordering in interfaces) | Very rare | Update `Endpoint*` constants in transaction.go |
| Serialization logic changes (conditional branches) | Very rare | Update MarshalFDB method logic |

### Structures in the pinned protocol

- The FlatBuffers wire format itself (vtable + soffset + RelativeOffset)
- The FakeRoot wrapping pattern
- The ErrorOr union flattening
- UID layout (always 16 bytes)
- ReplyPromise structure
- TCP framing and checksum format

These describe the pinned reference, not a guarantee that future FDB releases
retain them. Cross-version support needs source review and executable validation.

## Testing

```sh
just test       # fast lane: unit + bounded integration tests
just test-full  # all Bazel targets, including heavy client and differential suites
```

FDB-backed tests use testcontainers-go (Docker required). Use results for the exact
commit and lane rather than historical test counts or coverage percentages.
The fast lane excludes heavy client, differential and stress targets; see
[STATUS.md](../../STATUS.md) for scope. No fresh full-suite pass is asserted here.

## Benchmarks

The Go-vs-libfdb_c benchmarks live in [`bench/`](bench/bench_test.go); methodology and reproduction instructions are in [`bench/PERFORMANCE.md`](bench/PERFORMANCE.md). Earlier published speedup and write-parity claims were withdrawn: they do not describe the current default client behavior.

## Fault injection

The client supports a custom `DialFunc` for testing failure scenarios against real FDB. Same pattern as `http.Transport.DialContext` — no mocks, no artificial interfaces.

```go
// faultConn wraps net.Conn to inject failures at the TCP level.
type faultConn struct {
    net.Conn
    killReads atomic.Bool
}

func (f *faultConn) Read(b []byte) (int, error) {
    if f.killReads.Load() {
        return 0, io.EOF  // simulate network failure
    }
    return f.Conn.Read(b)
}

// Inject the custom dialer before connecting.
cluster := client.NewClusterFromConfig(cf)
cluster.SetDialFunc(func(ctx context.Context, network, addr string) (net.Conn, error) {
    conn, err := net.DialTimeout(network, addr, 5*time.Second)
    if err != nil {
        return nil, err
    }
    fc := &faultConn{Conn: conn}
    // Store fc somewhere so the test can arm/disarm it later
    return fc, nil
})
cluster.Connect(ctx)

// Later, arm the fault:
fc.killReads.Store(true)   // next Read → EOF → connection dies
// ... commit happens, server processes it, but reply is lost ...

// Disarm and reconnect:
fc.killReads.Store(false)
```

### What you can test

| Scenario | How |
|---|---|
| `commit_unknown_result` (1021) | Kill reads after commit frame sent — server commits but client sees EOF |
| Network partition | Kill reads indefinitely — all RPCs timeout |
| Slow network | Add `time.Sleep` in Read — triggers context deadline |
| Connection reset | Return `net.ErrClosed` from Read/Write |

### How self-conflicting works (commit_unknown_result)

When `OnError` receives error 1021, the transaction MAY have committed on the server. To prevent double-apply on retry:

1. `OnError(1021)` copies all write conflict ranges into the read conflict set
2. Transaction is reset, `Transact()` retries the user function
3. On the retry's commit, the commit proxy checks: "did any read conflict ranges change since read version?"
4. Since the ORIGINAL commit wrote to those ranges, the check fails → `not_committed` (1020)
5. The retry does NOT apply mutations — no double-apply

This achieves the same safety as C++ `NativeAPI::makeSelfConflicting()`. Additionally, `commitDummyTransaction` runs a synchronization barrier before returning `commit_unknown_result` — a separate transaction that conflicts with the original, confirming it's no longer in-flight at the commit proxy. Both mechanisms combined match C++ exactly. `TestCommitUnknownResult_NoDoubleApply` drops one atomic-ADD commit reply, checks that the barrier completes before error 1021 returns, and accepts counter 10 or 15 (not 20). It does not exercise `OnError` retries or prove application-level exactly-once behavior.

## Historical C++ comparison (audited 2026-04-12)

This table records an earlier source audit, not a current exhaustive divergence
inventory or a claim that all correctness bugs are fixed. Consult the current
[option matrix](fdb/OPTIONS.md), [project status](../../STATUS.md) and client
source/tests when evaluating behavior.

| Area | C++ behavior | Go behavior | Impact |
|---|---|---|---|
| Self-conflicting (1021) | `commitDummyTransaction` + `makeSelfConflicting` random range at `\xFF/SC/` | `commitDummyTransaction` (sync barrier) + copy write→read conflicts in `OnError` | Matching C++: dummy confirms original is out of system, self-conflicting prevents double-apply |
| Auto-reset after commit | No auto-reset at API >= 410 | `postCommitReset()` clears state for reuse | Design choice: Go API expects tx reuse after commit |
| `onProxiesChanged` | Wakes commit/GRV/location on proxy topology change | `proxiesChanged` broadcast wakes commit reply + GRV/location backoff loops | Matching C++: immediate wake-up on proxy failover |
| `FLAG_FIRST_IN_BATCH` | Commit flag for priority ordering | Not exposed | Missing API surface, no behavioral gap |
| `getRange` RYW merge | Segment-tree `RYWIterator` with demand-fetch + SnapshotCache | Iterative fetch+merge with SnapshotCache (sorted interval map) | Matching C++: server reads cached and reused within a transaction. SnapshotCache + iterative merge loop. |
| `getKey` boundary short-circuit | Returns `""` or `\xFF\xFF` without network | Same (implemented dayshift-6b) | Matching C++ |
| `tag_throttled` custom delay | Uses `cx->throttledTags` + TAG_THROTTLE_RECHECK_INTERVAL | `tagThrottles.maxDuration` with same capping | Matching C++: max(backoff, min(7s, tagDuration)) |
| `proxy_tag_throttled` accumulated delay | Tracks `proxyTagThrottledDuration`, sends back to proxy | Tracks duration but not yet sent back to proxy in GRV request | Rate feedback incomplete (LOW); throttle still works via standard backoff |
| QueueModel key | `endpoint.token.first()` (uint64) | Address string (host:port) | Cosmetic; same server identity in practice |
| Load balance secondDelay | Speculative second request after delay to hedge slow servers | `sendFrameWithHedge()` — race best + second-best server with max(10ms, 2x latency) delay | Matching C++: all 3 read paths (getValue, getKey, getRange) hedge |

## Adding a new request/response type

1. Add the C++ type to `cmd/fdb-schema-extract/main.cpp` (include header, add `extractType<T>()` call)
2. If the type has field names in its `serialize()` method, add the source file + type name to `name_capture.cpp`
3. Rebuild: `bash cmd/fdb-schema-extract/build.sh /path/to/fdb pkg/fdbgo/wire/types/vtables_generated.go`
4. Create `wire/types/my_type.go` with struct + `MarshalFDB()` / `UnmarshalFrom()` using the generated vtable and slot constants
5. Wire it into the client code
6. Test against real FDB
