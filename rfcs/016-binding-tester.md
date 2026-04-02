# RFC 016: FDB Binding Tester (Stack Machine Conformance)

**Status**: Proposed  
**Priority**: CRITICAL  
**Author**: birdy  
**Date**: 2026-04-02

## What is this

FDB ships an official conformance test suite called the "binding tester." It's a Python orchestrator (`bindingtester.py`) that:

1. Generates random instruction sequences and writes them to FDB
2. Runs your tester binary against those instructions
3. Runs a reference tester (e.g., the official Go or Python binding) against the same instructions
4. Compares results byte-for-byte

We implement ONE thing: a `fdb-stacktester` binary that reads instructions from FDB, executes them using our pure Go client API, writes results back. The existing Python harness handles everything else.

Every official FDB binding (C, Go, Java, Python, Ruby) ships a stacktester. Passing the binding tester is the gold standard — it proves byte-identical behavior to the C binding across 66+ operations with random inputs.

## Why CRITICAL

Our ad-hoc test porting (39 tests from `unit_tests.cpp`) already found 3 critical bugs — 13 wrong mutation type wire values, a GetRange key selector bug, and error code mismatches. The binding tester generates thousands of random operation sequences covering edge cases no human writes: deep stack manipulation, interleaved reads and writes, concurrent transactions, key selector clamping, streaming modes, versionstamp packing. It's the difference between "our tests pass" and "we are a conformant FDB binding."

## Architecture

```
bindingtester.py (FDB source, already exists)
  ├── generates instructions → writes to FDB as tuple-packed KVs
  ├── runs: ./fdb-stacktester <prefix> <cluster-file>
  ├── runs: ./reference-stacktester <prefix> <cluster-file>
  └── compares results in FDB — byte-identical or fail
```

We build:
```
cmd/fdb-stacktester/
  main.go            — entry point: parse args, connect, read instructions, run
  machine.go         — StackMachine struct, instruction dispatch loop
  operations.go      — one function per operation category
```

Binary interface (must match what `bindingtester.py` expects):
```
./fdb-stacktester <prefix> <api-version> <cluster-file>
```

That's the entire contract. Read instructions from FDB at `prefix`, execute on stack machine using FDB API version `api-version`, write results back.

## How the stack machine works

### Instructions

Stored in FDB by `bindingtester.py`:
```
Key:   tuple.Pack(prefix, index)     // index = sequence number
Value: tuple.Pack(operation, arg?)   // operation = string, arg = optional
```

The tester reads all instructions via range scan on `prefix`, executes in order.

### Stack

LIFO stack of mixed-type values. Each entry carries the instruction index that pushed it (for result correlation).

### Transaction map

Global `map[string]*Transaction` shared across threads. `NEW_TRANSACTION` creates, `USE_TRANSACTION` switches. Thread-safe via mutex.

### Error handling

All FDB errors become stack values: `tuple.Pack([]byte("ERROR"), []byte("1020"))`. The harness pattern-matches these across bindings.

### Futures

Some operations push a deferred value instead of resolving immediately. `WAIT_FUTURE` resolves the top-of-stack. Our client is synchronous (RPCs block inline), so most values are pushed resolved. Exception: `GET_VERSIONSTAMP` — the versionstamp isn't known until commit.

## Operations (66+)

### Stack manipulation (8)
`PUSH`, `DUP`, `EMPTY_STACK`, `SWAP`, `POP`, `SUB`, `CONCAT`, `LOG_STACK`

### Transaction lifecycle (5)
`NEW_TRANSACTION`, `USE_TRANSACTION`, `COMMIT`, `RESET`, `CANCEL`

### Reads (each with `_SNAPSHOT` and `_DATABASE` variants)
`GET`, `GET_KEY`, `GET_RANGE`, `GET_RANGE_STARTS_WITH`, `GET_RANGE_SELECTOR`, `GET_READ_VERSION`, `GET_COMMITTED_VERSION`, `GET_APPROXIMATE_SIZE`, `GET_VERSIONSTAMP`

### Writes (each with `_DATABASE` variant)
`SET`, `CLEAR`, `CLEAR_RANGE`, `CLEAR_RANGE_STARTS_WITH`, `ATOMIC_OP`

### Conflict tracking (5)
`READ_CONFLICT_RANGE`, `READ_CONFLICT_KEY`, `WRITE_CONFLICT_RANGE`, `WRITE_CONFLICT_KEY`, `DISABLE_WRITE_CONFLICT`

### Error + version (3)
`ON_ERROR`, `SET_READ_VERSION`, `WAIT_FUTURE`

### Tuple operations (9)
`TUPLE_PACK`, `TUPLE_UNPACK`, `TUPLE_RANGE`, `TUPLE_SORT`, `ENCODE_FLOAT`, `ENCODE_DOUBLE`, `DECODE_FLOAT`, `DECODE_DOUBLE`, `TUPLE_PACK_WITH_VERSIONSTAMP`

### Threading (2)
`START_THREAD`, `WAIT_EMPTY`

### Not in scope
Directory layer (`DIRECTORY_*`), tenant operations (`TENANT_*`). These are large subsystems we don't implement. The binding tester supports `--no-directory-ops` to skip these.

## What we need to implement vs already have

### Already have (our client API)
| Operation | Our API |
|---|---|
| `GET` | `tx.Get(ctx, key)` |
| `GET_KEY` | `tx.GetKey(ctx, key, orEqual, offset)` |
| `GET_RANGE` | `tx.GetRange` / `tx.GetRangeReverse` |
| `SET` | `tx.Set(key, value)` |
| `CLEAR` | `tx.Clear(key)` |
| `CLEAR_RANGE` | `tx.ClearRange(begin, end)` |
| `ATOMIC_OP` | `tx.Atomic(op, key, value)` |
| `COMMIT` | `tx.Commit(ctx)` |
| `CANCEL` | `tx.Cancel()` |
| `ON_ERROR` | `tx.OnError(err)` |
| `GET_READ_VERSION` | via GRV batcher |
| `SET_READ_VERSION` | `tx.SetReadVersion(v)` |
| `GET_COMMITTED_VERSION` | `tx.GetCommittedVersion()` |
| `GET_VERSIONSTAMP` | `tx.GetVersionstamp()` |
| Conflict ranges | `tx.AddReadConflictRange/Key`, `tx.AddWriteConflictRange/Key` |
| Snapshot | `tx.Snapshot().Get/GetKey/GetRange` |
| Timeout / retry limit | `tx.SetTimeout`, `tx.SetRetryLimit` |

### Need to add to client API
| Feature | Effort | Notes |
|---|---|---|
| `GetRangeStartsWith(prefix, limit, reverse)` | Trivial | Sugar: `GetRange(prefix, strinc(prefix), ...)` |
| `GetRangeSelector(beginSel, endSel, limit, reverse)` | Small | Raw key selectors — wire support exists |
| `ClearRangeStartsWith(prefix)` | Trivial | Sugar: `ClearRange(prefix, strinc(prefix))` |
| `_DATABASE` variants | Small | Wrap in `Transact()` with implicit retry |
| `DISABLE_WRITE_CONFLICT` | Small | Transaction option flag |
| `GetApproximateSize` | Medium | New wire type |

### Stack machine only (not client API)
| Feature | Effort |
|---|---|
| Stack + dispatch loop | Medium — core framework |
| Tuple pack/unpack | Import official `fdb/tuple` package (pure Go) |
| `LOG_STACK` (write stack to FDB) | Small |
| `START_THREAD` / `WAIT_EMPTY` | Small — goroutine + poll |
| `SUB` with big.Int | Small |
| `ENCODE/DECODE_FLOAT/DOUBLE` | Small |
| Deferred versionstamp | Medium — channel-based |

## Bazel integration

### Build

```starlark
# cmd/fdb-stacktester/BUILD.bazel
go_binary(
    name = "fdb-stacktester",
    srcs = ["main.go", "machine.go", "operations.go"],
    deps = ["//pkg/fdbgo/client", ...],
)
```

### Test

A `go_test` or `sh_test` that:
1. Starts FDB testcontainer
2. Runs `bindingtester.py` from FDB source (already fetched via `archive_override` in MODULE.bazel at tag 7.3.75)
3. Points it at our `fdb-stacktester` binary

```starlark
sh_test(
    name = "binding_conformance_test",
    srcs = ["run_binding_test.sh"],
    data = [
        "//cmd/fdb-stacktester",
        "@foundationdb+//:bindingtester",  # Python harness
    ],
)
```

### Python dependency

`bindingtester.py` needs:
- Python 3
- `foundationdb` Python package (pip install)
- A running FDB cluster (testcontainer provides this)

Options for Bazel:
1. **`rules_python`** — Bazel-native Python with pip dependencies. Clean but setup overhead.
2. **Host Python** — `sh_test` that calls system `python3`. Simpler, less hermetic.
3. **Docker** — Run `bindingtester.py` inside a container that has everything. Most hermetic.

Option 2 is pragmatic for now. The harness is a single script with one dependency.

### Running manually

```sh
# Build the stacktester
bazelisk build //cmd/fdb-stacktester

# Start FDB (or use existing cluster)
# Run the binding tester
python3 bindingtester/bindingtester.py \
  --cluster-file /path/to/fdb.cluster \
  --test-name api \
  --num-ops 1000 \
  --tester-binary bazel-bin/cmd/fdb-stacktester/fdb-stacktester
```

## Implementation plan

### Phase 1: Stack machine + core operations

1. `cmd/fdb-stacktester/main.go` — args, connect, read instructions, dispatch
2. `machine.go` — `StackMachine` struct, stack, transaction map
3. Stack ops: PUSH, DUP, POP, SWAP, EMPTY_STACK, SUB, CONCAT, LOG_STACK
4. Transaction: NEW_TRANSACTION, USE_TRANSACTION, COMMIT, RESET, CANCEL
5. Reads: GET, GET_KEY, GET_RANGE + STARTS_WITH + SELECTOR
6. Writes: SET, CLEAR, CLEAR_RANGE + STARTS_WITH, ATOMIC_OP
7. Version: GET_READ_VERSION, SET_READ_VERSION, GET_COMMITTED_VERSION
8. Conflicts: READ/WRITE_CONFLICT_RANGE/KEY
9. ON_ERROR
10. Snapshot + database variants
11. Tuple: import official `fdb/tuple`, wire up TUPLE_PACK/UNPACK/RANGE/SORT

### Phase 2: Threading + futures + missing APIs

12. START_THREAD, WAIT_EMPTY
13. WAIT_FUTURE + deferred GET_VERSIONSTAMP
14. ENCODE/DECODE_FLOAT/DOUBLE
15. TUPLE_PACK_WITH_VERSIONSTAMP
16. Add missing client APIs: GetRangeStartsWith, ClearRangeStartsWith, GetRangeSelector
17. DISABLE_WRITE_CONFLICT transaction option
18. _DATABASE variants (wrap in Transact)

### Phase 3: Integration

19. `sh_test` that runs `bindingtester.py` against our binary via testcontainer
20. `--test-name api --num-ops 1000` — random API test
21. `--test-name scripted` — hand-written scenarios
22. CI integration

## Success criteria

1. `bindingtester.py --test-name api --num-ops 1000` passes with 0 mismatches
2. `bindingtester.py --test-name scripted` passes
3. Threading tests pass (START_THREAD, concurrent transactions)
4. All results byte-identical to official Go binding's stacktester

## What this proves

Passing the binding tester means: for every operation the test generates, our pure Go client produces byte-identical results to the C binding. Not "similar" — identical tuple-packed bytes in FDB. This is the strongest conformance guarantee FDB offers for client bindings. Every official binding (C, Go, Java, Python, Ruby) passes it.
