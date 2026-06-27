# FDB Binding Tester (Stack Machine)

Pure Go implementation of the FDB binding tester stack machine, used with
FoundationDB's `bindingtester.py` for conformance testing against the C client.

## Architecture

```
bindingtester.py (FDB-provided Python harness)
  ├─ Generates random instruction sequences
  ├─ Writes instructions to FDB as tuple-packed values
  ├─ Runs our binary: ./fdb-stacktester <prefix> <api-version> <cluster-file>
  ├─ Our binary reads instructions, executes them, writes LOG_STACK results
  └─ Python compares our results against its own execution (byte-identical)
```

## Operations implemented

All operations required by `--test-name api`:

- **Stack**: PUSH, DUP, EMPTY_STACK, SWAP, POP, SUB, CONCAT, LOG_STACK
- **Transactions**: NEW_TRANSACTION, USE_TRANSACTION, COMMIT, RESET, CANCEL
- **Reads**: GET, GET_KEY, GET_RANGE, GET_RANGE_STARTS_WITH, GET_RANGE_SELECTOR
  (each with _SNAPSHOT and _DATABASE variants)
- **Writes**: SET, CLEAR, CLEAR_RANGE, CLEAR_RANGE_STARTS_WITH, ATOMIC_OP
  (each with _DATABASE variant)
- **Versions**: GET_READ_VERSION(_SNAPSHOT), SET_READ_VERSION, GET_COMMITTED_VERSION, GET_VERSIONSTAMP
- **Conflicts**: READ_CONFLICT_RANGE, READ_CONFLICT_KEY, WRITE_CONFLICT_RANGE, WRITE_CONFLICT_KEY, DISABLE_WRITE_CONFLICT
- **Tuples**: TUPLE_PACK, TUPLE_UNPACK, TUPLE_RANGE, TUPLE_SORT, ENCODE_FLOAT, ENCODE_DOUBLE, DECODE_FLOAT, DECODE_DOUBLE, TUPLE_PACK_WITH_VERSIONSTAMP
- **Misc**: ON_ERROR, GET_APPROXIMATE_SIZE, GET_ESTIMATED_RANGE_SIZE, GET_RANGE_SPLIT_POINTS, WAIT_FUTURE, WAIT_EMPTY, START_THREAD, UNIT_TESTS

Not implemented (out of scope): Tenant operations.

## Directory layer operations

All 21 DIRECTORY_* operations are implemented for `--test-name directory`:

- **Management**: CREATE_SUBSPACE, CREATE_LAYER, CHANGE, SET_ERROR_INDEX
- **CRUD**: CREATE_OR_OPEN, CREATE, OPEN (_DATABASE, _SNAPSHOT variants)
- **Move**: MOVE, MOVE_TO (_DATABASE variant)
- **Remove**: REMOVE, REMOVE_IF_EXISTS (_DATABASE variant)
- **Query**: LIST, EXISTS (_DATABASE, _SNAPSHOT variants)
- **Subspace**: PACK_KEY, UNPACK_KEY, RANGE, CONTAINS, OPEN_SUBSPACE
- **Logging**: LOG_SUBSPACE, LOG_DIRECTORY
- **Misc**: STRIP_PREFIX

## Key learnings

### API version must be passed explicitly

`bindingtester.py` determines the API version from the tester's entry in
`known_testers.py`. Unknown testers default to `min_api_version=0`, causing
the harness to pass random API versions (often 0) to the binary. Always pass
`--api-version 730` when invoking `bindingtester.py` with our tester.

### Tenant cleanup always runs

`bindingtester.py` unconditionally clears the tenant management keyspace
(`\xff\xff/management/tenant/map/`) before each test, even with `--no-tenants`.
The FDB cluster must have `tenant_mode=optional_experimental` configured, or
this cleanup fails with error 2136 ("Tenants have been disabled").

### Performance: pure Go client is slower than CGo

With `--num-ops 100`, the harness generates 5,000-30,000 instructions depending
on `max_keys` (random 100-10000). Each FDB operation requires a network round
trip (~1-2ms with pure Go client vs ~0.2ms with CGo). For 27K instructions
with ~5K FDB operations, execution takes 30-120 seconds vs <5 seconds with the
C client. Use `--timeout 300` or higher.

### Transaction reuse after commit

The C client auto-clears mutations after commit (in `tryCommit`), allowing
`SET → COMMIT → SET → COMMIT` without explicit `RESET`. Our client's
`postCommitReset()` matches this behavior. This is critical — the preload
phase relies on it.

### `fdb.Key` type on stack

The FDB tuple layer's `Unpack()` can return `fdb.Key` (a `[]byte` alias) for
certain elements. The stack machine's `popBytes()` must handle this type in
addition to `[]byte` and `string`.

## Running locally (fastest iteration)

```sh
# 1. Start FDB
docker run -d --name fdb-test -p 4500:4500 foundationdb/foundationdb:7.3.77
sleep 3
docker exec fdb-test fdbcli --exec "configure new single memory; configure tenant_mode=optional_experimental"
echo "docker:docker@127.0.0.1:4500" > /tmp/fdb-test.cluster

# 2. Install Python FDB binding (must match FDB version)
pip3 install "foundationdb>=7.3,<7.3.99"

# 3. Set up bindingtester
BTDIR=$HOME/.cache/bazel/_bazel_birdy/*/external/foundationdb+/bindings/bindingtester
mkdir -p /tmp/bt-run/bindingtester
cp -r $BTDIR/* /tmp/bt-run/bindingtester/
sed -i "s|sys.path\[:0\].*||" /tmp/bt-run/bindingtester/__init__.py
sed -i "s|import util|from bindingtester import util|" /tmp/bt-run/bindingtester/__init__.py
sed -i "s|from fdb import LATEST_API_VERSION|LATEST_API_VERSION = 730|" /tmp/bt-run/bindingtester/__init__.py

# 4. Build and run
just build
STACKTESTER=$(realpath bazel-bin/cmd/fdb-stacktester/fdb-stacktester_/fdb-stacktester)
cd /tmp/bt-run
PYTHONPATH=/tmp/bt-run python3 bindingtester/bindingtester.py \
  --cluster-file /tmp/fdb-test.cluster \
  --test-name api \
  --api-version 730 \
  --num-ops 100 \
  --timeout 300 \
  --no-threads \
  --no-tenants \
  $STACKTESTER
```

## Running via Bazel (Docker-in-Docker)

```sh
bazelisk test //cmd/fdb-stacktester/bindingtester:bindingtester_test \
  --test_output=streamed --test_timeout=900
```

This builds a Docker image with Python + FDB binding + our binary, starts an
FDB container via testcontainers, and runs `bindingtester.py` inside the tester
container. Slower due to Docker image build overhead.

## Known divergences from C client

Tracked for RFC:
- `GetApproximateSize` returns raw byte sum; C++ includes per-entry overhead
  (~12 bytes/mutation, ~16 bytes/conflict range). Binding tester only checks
  the value is non-negative, so this works.
- `OnError` missing 3 retryable error codes: `tag_throttled` (1213),
  `proxy_tag_throttled` (1223), `blob_granule_request_failed` (1079).
- No `RESOURCE_CONSTRAINED_MAX_BACKOFF` (30s) for proxy memory errors.
- `SetVersionstampedKey` atomic ops don't auto-suppress write conflicts like C++.
