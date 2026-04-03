# Pure Go Client Crashes FDB Server

## Summary

Our pure Go FDB client sends wire messages that crash the FDB 7.3.75 server
with SIGSEGV. The official CGo client does not crash the server with identical
workloads.

## Reproduction

```sh
# 1. Start FDB
docker rm -f fdb-test 2>/dev/null
docker run -d --name fdb-test -p 4500:4500 foundationdb/foundationdb:7.3.75
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
docker rm -f fdb-test; docker run -d --name fdb-test -p 4500:4500 foundationdb/foundationdb:7.3.75
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

| Seed | Instructions | Our client | CGo client |
|------|-------------|------------|------------|
| 1    | ~31,000     | CRASH      | not tested |
| 2    | ~10,000     | PASS       | -          |
| 3    | ~24,000     | PASS       | -          |
| 5    | ~26,000     | PASS       | -          |
| 6    | ~8,700      | CRASH      | PASS       |
| 7    | ~26,000     | PASS       | -          |
| 8    | ~22,000     | CRASH      | not tested |
| 10   | ~19,000     | CRASH      | not tested |

The crash is 100% reproducible for a given seed — seed 6 crashes every attempt (5/5).

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

### Root cause: codegen layout divergence

The issue is in `pkg/fdbgo/wire/writer.go` / `writer_direct.go` — the
vtable packing, object alignment, or OOL data placement differs from
C++'s ObjectSerializer. Need byte-by-byte comparison of a simple
`GetKeyServerLocationsRequest` between our Go output and C++ output.

## Next steps

1. Fix: `Commit()` calls `ensureReadVersion(ctx)` before building the request.
2. Verify: TenantID serialization — 0 vs -1 discrepancy needs investigation.
3. Re-run seed 6 after fix to confirm FDB no longer crashes.
4. Wire capture framework is in place for future debugging.

## Files involved

- `pkg/fdbgo/wire/types/*_generated.go` — FlatBuffers marshal/unmarshal (codegen)
- `pkg/fdbgo/client/commitpath.go` — CommitTransactionRequest construction
- `pkg/fdbgo/client/readpath.go` — GetValue/GetKey/GetKeyValues construction
- `pkg/fdbgo/client/grv.go` — GetReadVersion construction
- `pkg/fdbgo/transport/conn.go` — Frame serialization and sending
- `cmd/fdb-stacktester/` — Binding tester stack machine
