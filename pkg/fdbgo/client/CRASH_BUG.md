# Pure Go Client Crashes FDB Server

## Summary

Our pure Go FDB client sends wire messages that crash the FDB 7.3.77 server
with SIGSEGV. The official CGo client does not crash the server with identical
workloads.

## How to debug FDB server crashes

When fdbserver crashes (SIGSEGV, assertion, etc.), the crash trace is in the
container's XML log. The log contains a ready-made `addr2line` command — you
just need the debug symbols binary from the GitHub release.

```sh
# 1. Copy logs from the (stopped) container
docker cp fdb-test:/var/fdb/logs /tmp/fdb-logs

# 2. Find the crash event
grep 'Type="Crash"' /tmp/fdb-logs/trace.*.xml
# Output includes:
#   Trace="addr2line -e fdbserver.debug -p -C -f -i 0x338113f 0x3380d12 ..."

# 3. Download debug symbols (one-time per FDB version)
#    Find the right file at https://github.com/apple/foundationdb/releases/tag/<version>
curl -sL "https://github.com/apple/foundationdb/releases/download/7.3.77/fdbserver.debug.x86_64.gz" \
  -o /tmp/fdbserver.debug.x86_64.gz
gunzip -f /tmp/fdbserver.debug.x86_64.gz

# 4. Verify BuildID matches (must be identical)
readelf -n /tmp/fdbserver.debug.x86_64 | grep 'Build ID'
# Compare against the binary in the container:
docker cp fdb-test:/usr/bin/fdbserver /tmp/fdbserver
readelf -n /tmp/fdbserver | grep 'Build ID'

# 5. Run the addr2line command from the crash log, using the debug binary
addr2line -e /tmp/fdbserver.debug.x86_64 -p -C -f -i 0x338113f 0x3380d12 0x33809de 0x3380356
# Output: function names, source files, line numbers, with inlines expanded
```

**Key points:**
- The container binary (`/usr/bin/fdbserver`) is **stripped** — `objdump` gives
  you nothing. You need the separate `.debug` file from GitHub releases.
- FDB's crash log literally gives you the `addr2line` command — just replace
  `fdbserver.debug` with the path to your downloaded debug binary.
- Flags: `-C` demangles C++, `-f` prints function names, `-i` expands inlines.
- FDB also logs non-crash errors at Severity="40" — grep for those too.
  `inverted_range`, `wrong_shard_server`, etc. show up there without a crash.

### Wire log capture

Set `FDB_WIRE_LOG=/tmp/wirelog.bin` to capture all frames sent/received.
Dump with `fdb-wirelog-dump`:

```sh
bazelisk build //cmd/fdb-wirelog-dump:fdb-wirelog-dump
bazel-bin/cmd/fdb-wirelog-dump/fdb-wirelog-dump_/fdb-wirelog-dump -hex -last 10 -send-only /tmp/wirelog.bin
```

To decode a specific frame as a GetKeyValuesRequest (or other type), write a
small Go program that reads the wirelog binary format (29-byte header per frame:
1 dir + 8 timestamp + 16 token + 4 bodylen, then body bytes) and calls
`UnmarshalFDB()` on the body.

## Reproduction

```sh
# 1. Start FDB
docker rm -f fdb-test 2>/dev/null
docker run -d --name fdb-test -p 4500:4500 foundationdb/foundationdb:7.3.77
sleep 5
docker exec fdb-test fdbcli --exec "configure new single memory tenant_mode=optional_experimental"
echo "docker:docker@127.0.0.1:4500" > /tmp/fdb-test.cluster

# 2. Install Python binding
pip3 install "foundationdb>=7.3,<7.3.99"

# 3. Set up bindingtester harness
BTDIR=$HOME/.cache/bazel/_bazel_birdy/*/external/foundationdb+/bindings/bindingtester
mkdir -p /tmp/bt-run/bindingtester
cp -r $BTDIR/* /tmp/bt-run/bindingtester/
sed -i "s|sys.path\[:0\].*||" /tmp/bt-run/bindingtester/__init__.py
sed -i "s|import util|from bindingtester import util|" /tmp/bt-run/bindingtester/__init__.py
sed -i "s|from fdb import LATEST_API_VERSION|LATEST_API_VERSION = 730|" /tmp/bt-run/bindingtester/__init__.py

# 4. Run with crashing seed using OUR binary
just build
STACKTESTER=$(realpath bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester)
cd /tmp/bt-run
PYTHONPATH=/tmp/bt-run python3 bindingtester/bindingtester.py \
  --cluster-file /tmp/fdb-test.cluster \
  --test-name api --api-version 730 \
  --num-ops 100 --seed 6 --timeout 30 \
  --no-threads --no-tenants \
  $STACKTESTER
# Result: FDB crashes with SIGSEGV, binary hangs on dead connection

# 5. Restart FDB, run SAME seed with CGo stacktester — passes fine
docker rm -f fdb-test; docker run -d --name fdb-test -p 4500:4500 foundationdb/foundationdb:7.3.77
sleep 5; docker exec fdb-test fdbcli --exec "configure new single memory tenant_mode=optional_experimental"
cd /tmp/bt-run
PYTHONPATH=/tmp/bt-run python3 bindingtester/bindingtester.py \
  --cluster-file /tmp/fdb-test.cluster \
  --test-name api --api-version 730 \
  --num-ops 100 --seed 6 --timeout 30 \
  --no-threads --no-tenants \
  /tmp/cgo-stacktester/stacktester
# Result: PASS, 0 errors, FDB alive
```

## Known crashing seeds

Tested with `--num-ops 100 --api-version 730 --no-threads --no-tenants`:

| Seed | Instructions | Before fix | After fix |
|------|-------------|------------|-----------|
| 1    | ~31,000     | CRASH      | PASS      |
| 2    | ~10,000     | PASS       | PASS      |
| 3    | ~24,000     | PASS       | PASS      |
| 5    | ~26,000     | PASS       | PASS      |
| 6    | ~8,700      | CRASH      | PASS      |
| 7    | ~26,000     | PASS       | PASS      |
| 8    | ~22,000     | CRASH      | PASS      |
| 10   | ~19,000     | CRASH      | PASS      |

**All 8 seeds now pass with 0 errors and FDB alive.**

## What we ruled out

### Wire format round-trip validation
Added marshal-then-unmarshal check to every CommitTransactionRequest before
sending. All commits pass round-trip validation — zero mismatches in mutation
type, key data, or value data across 2180 commits. The FlatBuffers structure
is internally consistent.

### Python harness causing the crash
Ran the harness with `/bin/true` as the tester binary (inserts instructions
but does nothing). FDB stays alive. The crash is caused by our binary's
network traffic, not by the test data.

### Commit path specifically
The last logged commits before crash are normal SET (type=0) and ClearRange
(type=1) mutations with no exotic types. No atomic mutations were in flight.

## FDB trace at crash time

From `/var/fdb/logs/trace.*.xml`:

```
Time="...582.753" Type="CodeCoverage" File="fdbrpc/FlowTransport.actor.cpp" Line="749"
  Comment="We didn't write everything, so apparently the write buffer is full. Wait for it to be nonfull"

Time="...582.813" Type="Crash" Signal="11" Name="Segmentation fault"
  Trace="addr2line -e fdbserver.debug -p -C -f -i 0x234cc19 0x234c22f 0x234bdc3 0x5543d98 0x32b43cc"
```

The crash happens immediately after "write buffer is full" in FlowTransport —
FDB is trying to send a response back to our client and crashes during
response serialization or buffer management.

## Likely root cause

Our FlatBuffers serialization produces structurally valid messages (round-trip
passes) but something is semantically wrong that causes the FDB server to
generate an oversized or malformed response. Candidates:

1. **GetKeyValues request with wrong parameters** — limit, reverse flag, or
   key selector encoding might differ from C client, causing FDB to attempt
   an enormous range scan response that overflows internal buffers.

2. **GetKey request with bad selector encoding** — wrong orEqual/offset
   encoding could cause FDB to resolve to an unexpected key, leading to a
   pathological response.

3. **Request framing or endpoint token** — if we send a request to the wrong
   endpoint (e.g., GetValue body to GetKeyValues endpoint), the server
   deserializes garbage and crashes.

4. **Connection protocol mismatch** — if our framing, protocol version
   negotiation, or PING handling differs from the C client, the server might
   misparse subsequent messages.

## Root cause found (2026-04-03)

**The crashing frame is a CommitTransactionRequest with `ReadSnapshot=0`.**

Decoded frame #92 (the last SEND before FDB segfaults):

```
ReadSnapshot: 0          ← INVALID (should be a real GRV version)
Mutations: 0             ← empty commit
ReadConflictRanges: 0
WriteConflictRanges: 0
TenantID: 0              ← should be -1 (NoTenantID)
Flags: 0
```

A commit with `ReadSnapshot=0` and no mutations. FDB server crashes trying
to process this — version 0 is never valid.

### How this happens

1. `Transact(fn)` runs `fn(tx)` which does reads (fetches GRV) + writes
2. `tx.Commit()` succeeds — sends proper ReadSnapshot from GRV
3. `postCommitReset()` clears `tx.readVersion` and `tx.hasReadVersion`
4. Next `Transact(fn)` call (e.g., LOG_STACK writing stack entries):
   - `fn(tx)` only does `tx.Set()` calls (no reads) → ReadSnapshot stays 0
   - `tx.Commit()` sends commit with `ReadSnapshot=0` → FDB CRASH

The C++ client avoids this because `commitMutations()` always has a valid
read version — the C++ Transaction holds onto the read version future from
the previous GRV, and commit waits on it. Our `postCommitReset()` clears it
too aggressively.

### Update: frame #92 is NOT a commit — it's GetKeyServerLocationsRequest

The crashing frame has `fileID=0x8b8968 = 9144680` which is
`GetKeyServerLocationsRequestFileID`, not `CommitTransactionRequestFileID`.
The endpoint suffix `0x9c` is shared between commit and location requests
on a single-node cluster.

### Update: marshal round-trips correctly but layout differs from C++

Our `MarshalFDB()` → `UnmarshalFDB()` round-trip passes for ALL tested
inputs. But FDB server crashes parsing the same bytes. This means:

**Our FlatBuffers layout is self-consistent but different from C++.**

The codegen (RFC 013) produces a format where our reader and writer agree,
but it does NOT match the C++ FlatBuffers layout that FDB server expects.
The CGo client (using `libfdb_c.so`) produces the correct C++ layout.

### Root cause: three codegen/runtime layout bugs

**Bug 1 (FIXED):** Nested struct serialization order was reversed.
Our codegen emitted nested structs in reverse field order; C++ processes
them in forward serialization order. Fixed in main.cpp `emitMeasureEndOff`,
`emitWriteDirect`, `emitMarshalFDB`.

**Bug 2 (FIXED):** Optional<T> fields were completely skipped in marshal.
FieldKind::Optional was not handled in measureEndOff, writeDirect, or
MarshalFDB emit paths. Fixed: presence tag + WriteBytesOOL for value.

**Bug 3 (TODO):** Empty dynamic_size fields not serialized.
C++ `PrecomputeSize::visitDynamicSize` allocates 4 bytes for empty
fields (the empty vector sentinel). Our `MeasureBytesOOL(endOff, nil)`
returns `endOff` unchanged (0 bytes). Additionally, C++ has an
`emptyVector` optimization: only the FIRST empty vector allocates 4
bytes, subsequent ones re-use the same offset.
Fix: port C++ empty vector sentinel logic to `writer_direct.go`.

**Bug 4 (TODO):** Generated nil-guards skip fields that C++ serializes.
Our codegen emits `if m.Field != nil { ... }` guards that skip empty
DynamicSize/VectorLike fields. C++ `serializer(ar, ...)` visits EVERY
field regardless of whether it's empty. This means our vtable offsets
for empty fields are 0 (field not present) while C++ has non-zero
offsets pointing to the empty vector sentinel.
Fix: generated code must always serialize every field, matching C++.
The nil-guard should only suppress the OOL data, not the reloff.

**Bug 5 (TODO):** CommitTransactionRequest Go output is LARGER than C++
(+40/+88 bytes). Likely a Vector<struct> blob sizing issue — our
`blobSize()`/`writeBlob()` may produce different padding than C++.
Need to compare vector-of-struct serialization against C++ step by step.

**Bug 6 (TODO — vtable packing):** VTableSet.pack() produces 50 bytes
for a 5-vtable closure, C++ produces 52 bytes. 2-byte padding difference.
Low priority since it only affects total buffer size, not field layout.
May self-resolve when bugs 3-4 are fixed (different content changes alignment).

### Ground truth test infrastructure

`cmd/fdb-schema-extract/main.cpp` now emits `testdata.json` with C++
ObjectWriter serialized bytes for 10 test vectors. Go test
`types/ground_truth_test.go` compares MarshalFDB byte-for-byte.

Current status: 10/10 sizes match. Byte differences only in ReplyPromise token
(expected — C++ test uses random tokens, Go test uses zero).

## RESOLVED (2026-04-04)

### Root cause: two bugs in readpath.go + missing client-side validation

**Bug A — \xff\xff system keys sent to storage server:**
`getRangeOneShard()` always located shards by `begin` key, even for reverse scans.
When the binding tester issued a reverse range with begin=`\xff\xff`, we sent
`\xff\xff` directly to the storage server. The server's `getShardKeyRange()`
tried to look up `\xff\xff` in its shard map and SEGFAULTED.

Fix: For reverse scans, locate by `end` key (matching C++ `getExactRange`).
Clamp begin/end to shard boundaries before sending. Skip if empty after clamping.

**Bug B — inverted ranges in commits:**
`ClearRange()`, `AddReadConflictRange()`, `AddWriteConflictRange()`, and
`getRangeDir()` did not validate begin <= end. The C++ client returns
`inverted_range` (error 2005) for these cases. Without validation, inverted
ranges were included in the commit and the server rejected the entire commit.

Fix: Validate begin <= end in all conflict range operations. Return error 2005.
Skip adding read conflict ranges for inverted ranges in `getRangeDir()`.

### addr2line output from crash

```
storageserver.actor.cpp:4581 — getShardKeyRange(data, req.begin)
  → data->shards.rangeContaining(sel.getKey()) with sel.key = \xff\xff
  → SIGSEGV (key not in any shard)
```

## Why FDB server doesn't guard against this

The storage server does not validate that incoming keys fall within its shard
boundaries before dereferencing the shard map iterator. In `getShardKeyRange()`
(`storageserver.actor.cpp:4499`):

```cpp
auto i = sel.isBackward() ? data->shards.rangeContainingKeyBefore(sel.getKey())
                          : data->shards.rangeContaining(sel.getKey());
if (!i->value()->isReadable())       // ← dereferences without bounds check
    throw wrong_shard_server();
ASSERT(selectorInRange(sel, i->range()));
```

`rangeContaining()` on a key outside all shards (`\xff\xff`) returns an invalid
iterator. The code dereferences it immediately — no bounds check, no
`wrong_shard_server` thrown. A defensive check here would prevent the crash, but
FDB's design assumes the **C++ client is the only caller**. The wire protocol is
an internal RPC between trusted components, not a public API. The C++ client
guarantees keys are clamped to shard boundaries (via `getExactRange`) before they
ever hit a storage server.

Same pattern for inverted ranges: the commit proxy trusts that the client already
validated `begin <= end` — `fdb_transaction_add_conflict_range()` and
`fdb_transaction_clear_range_impl()` reject inverted ranges client-side so the
server never sees them.

This is a general FDB design principle: **the wire protocol is trusted**. If you
speak it, you're expected to speak it correctly. Server-side validation is minimal
because the only legitimate client (`libfdb_c.so`) enforces invariants before
serialization. Our pure Go client broke that contract by sending keys and ranges
the C++ client would never produce.

This is arguably a server hardening gap — throwing `wrong_shard_server` instead
of segfaulting would be strictly better — but it's low priority for the FDB team
since no legitimate client triggers it.

## Files involved

- `pkg/fdbgo/client/readpath.go` — shard clamping, reverse scan location
- `pkg/fdbgo/client/locality.go` — LocationResult with shard boundaries
- `pkg/fdbgo/client/transaction.go` — inverted range validation
- `cmd/fdb-stacktester/operations.go` — error handling for validation
- `pkg/fdbgo/wire/types/*_generated.go` — FlatBuffers marshal/unmarshal (codegen)
- `pkg/fdbgo/client/commitpath.go` — CommitTransactionRequest construction
- `pkg/fdbgo/transport/conn.go` — Frame serialization and sending
- `cmd/fdb-stacktester/` — Binding tester stack machine
