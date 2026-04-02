# RFC 016: FDB Binding Tester (Stack Machine Conformance)

**Status**: Proposed  
**Author**: birdy  
**Date**: 2026-04-02

## What is this

FDB ships an official conformance test suite called the "binding tester." It's a stack machine. A Python test generator writes a sequence of operations as key-value pairs into FDB. Each binding implementation reads the operations, executes them using its API, and writes results back. A harness compares results across all bindings (Python, Java, Go, C, Ruby). If your results match, your binding is conformant.

The official Go binding already passes this test: `bindings/go/src/_stacktester/stacktester.go`. We port it to our pure Go client. When it passes, we're officially conformant — same behavior as the C binding, verified by the same test infrastructure.

## Why it matters

Our ad-hoc test porting (39 tests from `unit_tests.cpp`) already found 3 critical bugs. The binding tester is 10x more comprehensive — 66+ operations, including edge cases the unit tests don't cover: streaming modes, key selector resolution with prefix clamping, versionstamp packing, concurrent transactions, error propagation as tuples. It's the difference between "our tests pass" and "we are a conformant FDB binding."

## How the stack machine works

### Instructions

Stored in FDB as key-value pairs:

```
Key:   tuple.Pack(prefix, index)     // index = operation sequence number
Value: tuple.Pack(operation, arg?)   // operation = string, arg = optional
```

The tester reads all instructions via range scan, executes them in order.

### Stack

LIFO stack of mixed-type values. Each entry carries the instruction index that pushed it (for result correlation). Values can be: `int64`, `[]byte`, `string`, `float32`, `float64`, `*big.Int`, or a pending future.

### Transaction map

Global `map[string]Transaction` shared across threads. `NEW_TRANSACTION` creates one, `USE_TRANSACTION` switches the active one. Thread-safe.

### Error handling

All FDB errors become stack values: `tuple.Pack([]byte("ERROR"), []byte("1020"))`. The test harness pattern-matches these across bindings.

### Futures

Some operations push a future onto the stack instead of resolving immediately. `WAIT_FUTURE` resolves the top-of-stack future. If it errors, it becomes an `ERROR` tuple.

## Operations (66+)

### Stack manipulation (8)
`PUSH`, `DUP`, `EMPTY_STACK`, `SWAP`, `POP`, `SUB`, `CONCAT`, `LOG_STACK`

### Transaction lifecycle (5)
`NEW_TRANSACTION`, `USE_TRANSACTION`, `COMMIT`, `RESET`, `CANCEL`

### Reads (9, each with `_SNAPSHOT` and `_DATABASE` variants = 27)
`GET`, `GET_KEY`, `GET_RANGE`, `GET_RANGE_STARTS_WITH`, `GET_RANGE_SELECTOR`, `GET_READ_VERSION`, `GET_COMMITTED_VERSION`, `GET_APPROXIMATE_SIZE`, `GET_VERSIONSTAMP`

### Writes (5, each with `_DATABASE` variant = 10)
`SET`, `CLEAR`, `CLEAR_RANGE`, `CLEAR_RANGE_STARTS_WITH`, `ATOMIC_OP`

### Conflict tracking (5)
`READ_CONFLICT_RANGE`, `READ_CONFLICT_KEY`, `WRITE_CONFLICT_RANGE`, `WRITE_CONFLICT_KEY`, `DISABLE_WRITE_CONFLICT`

### Error + version (3)
`ON_ERROR`, `SET_READ_VERSION`, `WAIT_FUTURE`

### Tuple operations (8)
`TUPLE_PACK`, `TUPLE_UNPACK`, `TUPLE_RANGE`, `TUPLE_SORT`, `ENCODE_FLOAT`, `ENCODE_DOUBLE`, `DECODE_FLOAT`, `DECODE_DOUBLE`, `TUPLE_PACK_WITH_VERSIONSTAMP`

### Threading (2)
`START_THREAD`, `WAIT_EMPTY`

### Misc (1)
`UNIT_TESTS` — binding-specific smoke tests (watches, locality, options)

### Not in scope (skip)
Directory layer operations (`DIRECTORY_*`), tenant operations (`TENANT_*`). These are large subsystems we don't implement.

## What we need to implement

### Already have
| Operation | Our API | Notes |
|---|---|---|
| `GET` | `tx.Get(ctx, key)` | ✓ |
| `GET_KEY` | `tx.GetKey(ctx, key, orEqual, offset)` | ✓ |
| `GET_RANGE` | `tx.GetRange(ctx, begin, end, limit)` | ✓ forward + reverse |
| `SET` | `tx.Set(key, value)` | ✓ |
| `CLEAR` | `tx.Clear(key)` | ✓ |
| `CLEAR_RANGE` | `tx.ClearRange(begin, end)` | ✓ |
| `ATOMIC_OP` | `tx.Atomic(op, key, value)` | ✓ all 14 types (fixed) |
| `COMMIT` | `tx.Commit(ctx)` | ✓ |
| `RESET` | via `OnError` path | ✓ |
| `CANCEL` | `tx.Cancel()` | ✓ |
| `ON_ERROR` | `tx.OnError(err)` | ✓ |
| `GET_READ_VERSION` | via GRV batcher | ✓ |
| `SET_READ_VERSION` | `tx.SetReadVersion(v)` | ✓ |
| `GET_COMMITTED_VERSION` | `tx.GetCommittedVersion()` | ✓ |
| `GET_VERSIONSTAMP` | `tx.GetVersionstamp()` | ✓ |
| `READ_CONFLICT_RANGE/KEY` | `tx.AddReadConflictRange/Key()` | ✓ |
| `WRITE_CONFLICT_RANGE/KEY` | `tx.AddWriteConflictRange/Key()` | ✓ |
| Snapshot reads | `tx.Snapshot().Get/GetKey/GetRange` | ✓ |

### Need to add
| Operation | What's missing | Effort |
|---|---|---|
| `GET_RANGE_STARTS_WITH` | `tx.GetRangeStartsWith(prefix, limit, reverse)` — sugar for `GetRange(prefix, strinc(prefix), ...)` | Trivial |
| `GET_RANGE_SELECTOR` | `tx.GetRangeSelector(beginSel, endSel, limit, reverse)` — raw key selectors | Small — wire support exists, need API |
| `CLEAR_RANGE_STARTS_WITH` | `tx.ClearRangeStartsWith(prefix)` — sugar for `ClearRange(prefix, strinc(prefix))` | Trivial |
| `_DATABASE` variants | Execute operation with implicit transaction + retry | Small — wrap in `Transact()` |
| `GET_APPROXIMATE_SIZE` | New wire type `GetApproximateSizeRequest` | Medium |
| `DISABLE_WRITE_CONFLICT` | Transaction option | Small |
| `WAIT_FUTURE` | Futures on stack — need async model | Medium — see below |
| `START_THREAD` | Goroutine with independent stack machine | Small |
| `WAIT_EMPTY` | Poll range until empty | Trivial |
| `LOG_STACK` | Write stack contents to FDB | Small |
| Tuple operations | Need FDB tuple layer in pure Go | Medium — use `github.com/apple/foundationdb/bindings/go/src/fdb/tuple` or port |

### The futures question

The official Go stacktester uses `FutureByteSlice.MustGet()` which blocks. Our pure Go client returns values directly (no futures — RPCs block inline). For the binding tester:

- Most operations can push the resolved value immediately (no future needed)
- `GET_VERSIONSTAMP` is the exception — the versionstamp isn't known until commit. The official Go binding uses a `FutureKey` that resolves after commit.
- `WAIT_FUTURE` on a non-future value is a no-op

Simplest approach: push resolved values for everything except versionstamp. For versionstamp, push a deferred value (channel or callback) that resolves on `WAIT_FUTURE`.

### Tuple layer

The binding tester heavily uses tuple pack/unpack. We need either:
1. Import the official Go tuple package (`fdb/tuple`) — it's pure Go, no cgo dependency
2. Use our `fastUnpack` from recordlayer (already hardened against panics)

Option 1 is simpler. The official tuple package is standalone.

## Architecture

```
cmd/fdb-stacktester/
  main.go          — entry point: parse args, connect, run
  machine.go       — StackMachine: stack, transaction map, instruction dispatch
  operations.go    — one function per operation category
```

### Entry point

```
fdb-stacktester --prefix <prefix> --cluster-file <path> [--api-version 730]
```

Reads instructions from `tuple.Range(prefix)`, executes them, writes results. Compatible with the Python test harness (`bindingtester.py`).

### Stack machine

```go
type StackMachine struct {
    db      *client.Database
    prefix  []byte
    trName  string
    trMap   map[string]*client.Transaction  // shared across threads
    trMu    sync.RWMutex
    stack   []stackEntry
    lastVer int64
    wg      sync.WaitGroup
}

type stackEntry struct {
    value any       // int64, []byte, string, float32, float64, *big.Int, deferredValue
    idx   int       // instruction number
}
```

### Result format

Matches the official Go stacktester exactly:
- Successful value → push raw value
- FDB error → push `tuple.Pack([]byte("ERROR"), []byte(strconv.Itoa(code)))`
- Missing key → push `[]byte("RESULT_NOT_PRESENT")`

## Implementation plan

### Phase 1: Core operations (covers ~80% of instructions)

1. Stack machine framework (`StackMachine`, instruction dispatch loop)
2. Stack operations (PUSH, DUP, POP, SWAP, EMPTY_STACK, SUB, CONCAT)
3. Transaction management (NEW_TRANSACTION, USE_TRANSACTION)
4. All read operations (GET, GET_KEY, GET_RANGE + STARTS_WITH + SELECTOR variants)
5. All write operations (SET, CLEAR, CLEAR_RANGE + STARTS_WITH)
6. ATOMIC_OP dispatch (reflection-based, matching official Go tester)
7. COMMIT, RESET, CANCEL, ON_ERROR
8. Version operations (GET_READ_VERSION, SET_READ_VERSION, GET_COMMITTED_VERSION)
9. Conflict ranges (READ/WRITE_CONFLICT_RANGE/KEY)
10. Snapshot + database variants
11. LOG_STACK
12. Tuple operations (import official tuple package)

### Phase 2: Threading + futures

13. START_THREAD (goroutine with independent StackMachine)
14. WAIT_EMPTY (poll loop)
15. WAIT_FUTURE + deferred versionstamp
16. GET_VERSIONSTAMP with deferred resolution

### Phase 3: Missing APIs

17. GET_RANGE_SELECTOR (raw key selector API)
18. GET_APPROXIMATE_SIZE (new wire type)
19. DISABLE_WRITE_CONFLICT (transaction option)
20. UNIT_TESTS (binding-specific smoke tests)

### Phase 4: Integration with test harness

21. Build as standalone binary: `cmd/fdb-stacktester/`
22. Test against Python harness: `bindingtester.py --test-name api --num-ops 1000`
23. CI integration

## How to run

Once implemented:

```sh
# Generate test instructions (Python, part of FDB source)
python bindingtester/bindingtester.py \
  --cluster-file /path/to/fdb.cluster \
  --test-name api \
  --num-ops 1000 \
  --tester-binary ./fdb-stacktester

# Or run standalone against pre-generated instructions
./fdb-stacktester --prefix '\x01test' --cluster-file /path/to/fdb.cluster
```

## Success criteria

1. Stack machine executes all Phase 1 operations correctly
2. Results match official Go binding's stacktester for the same instruction set
3. `bindingtester.py --test-name api` passes with 0 mismatches
4. `bindingtester.py --test-name scripted` passes
5. Threading tests pass (concurrent stack machines, shared transaction map)

## What this proves

Passing the binding tester means: for every operation the test generates, our pure Go client produces the same result as the C binding. Not "similar" — byte-identical after tuple packing. This is the strongest possible conformance guarantee FDB offers for client bindings.
