# FoundationDB Record Layer — Go Port

Port the FoundationDB Record Layer from Java to Go, maintaining API compatibility so that:
- Go applications can read/write records created by Java Record Layer applications
- Java applications can read/write records created by Go Record Layer applications
- Both can share the same FDB cluster and data

## Scope

We are porting **`fdb-record-layer-core` only** — the storage engine, indexes, cursors, metadata, and online indexing. All relational/SQL layer features are explicitly out of scope for now:
- UDFs (`PUserDefinedFunction`) — only used by `fdb-relational-core`'s query planner, not by core record layer
- Views (`PView`) — SQL layer concept
- Synthetic record types (`JoinedRecordType`, `UnnestedRecordType`) — query planner feature
- SQL function catalog, semantic analyzer, cascades optimizer
- The entire `fdb-relational-*` module family

These features live in metadata proto fields that protobuf round-trips automatically (unknown fields preserved). We don't need explicit Go types for them until we build a query planner.

## CRITICAL: Keep this file updated

You are co-owner of this project. Treat this codebase like your own — update CLAUDE.md on the fly as architecture evolves, decisions are made, or conventions are established. This file is your memory — if it's wrong or stale, you'll make wrong decisions next session.

## Testing

We require **very high and thorough test coverage**. Every new feature, bug fix, or behavioral change must have tests. Cover edge cases, error paths, and zero-value behaviors — not just the happy path. Tests use testcontainers (real FoundationDB), not mocks.

**All tests MUST call `t.Parallel()`**. Each test must be safe to run concurrently — use unique key prefixes or subspaces for isolation, never rely on shared mutable state. This is critical for fast test execution on beefy CPUs.

**CRITICAL: Container setup MUST have timeouts.** Always use `context.WithTimeout(context.Background(), 2*time.Minute)` when calling `foundationdbtc.Run()` or `container.InitializeDatabase()`. NEVER pass a bare `context.Background()` — blocks forever when Docker is slow.

**CRITICAL: Never run binding stress concurrently with `just test`.** Both create Docker containers. The pre-commit hook runs `just test`, so committing while binding stress runs WILL cause problems. Always wait for one to finish.

**CRITICAL: Test hang root cause (diagnosed swingshift-4).** When `bazelisk test //...` runs, 9 test targets create FDB containers in parallel. If Docker is loaded, some containers start but FDB becomes unreachable mid-test. The chaos test suite has 200 tests with `t.Parallel()` running serially (`-test.parallel=1`). Each test attempts an FDB transaction, times out after 30 seconds, fails, and the next test starts. 200 × 30s = **100+ minutes of cascading timeouts** that look like a hang. `.bazelrc` now has `--local_test_jobs=4` to limit concurrent container creation. If a test suite hangs, check: (1) is the FDB container still alive? (2) are tests cascading through individual timeouts? Kill the test, don't wait.

## Shift system

This project uses a Vollkonti (continuous 24/7) shift system. Run `/vollkonti` to start a shift. Handovers live in `shifts/`. Each shift gets one branch, one PR, merged at end.

## Work tracking & workflow

See `TODO.md` in repo root for tracked issues and improvements. Use checkbox format `- [x]` / `- [ ]` with severity levels.

**Working rhythm:** one thing at a time. Implement a feature, write tests, run `just test`, commit, move on. Don't batch multiple unrelated features into one commit.

**Delegation style:** Act as a principal engineer — design, plan, and manage. Delegate implementation grunt work to subagents aggressively. This preserves context window for architectural decisions and quality review. Self-adjust: do critical/tricky pieces yourself, delegate mechanical/boilerplate work. Provide subagents with full context (file paths, code snippets, patterns to follow) so they can work autonomously. Review their output, fix issues, iterate.

**One large task at a time.** Never run two big implementation subagents in parallel (e.g., two rewrites touching different subsystems). Too easy to lose track of details, miss review, or end up with half-finished work. Small research/exploration agents can run in parallel, but any agent that writes significant code should run alone so you can review its output properly before moving on.

**Build & verify:** always use `just test` (not manual bazelisk commands) — it runs everything and the Bazel cache handles incrementality perfectly.

**Run specific Ginkgo tests via Bazel:**
```sh
bazelisk test //pkg/recordlayer:recordlayer_test --test_arg="--ginkgo.focus=CountIndex" --test_output=streamed
```
Use `--ginkgo.focus=<regex>` to target a specific `Describe`/`It` block. Multiple words work as regex, e.g. `--ginkgo.focus="CountIndex.*delete"`.

**Commits:** commit after each feature passes tests. One logical change per commit. Do NOT push unless the user asks.

**Update TODO.md** as work completes — mark items `[x]` with a short note of what was done.

## Wire types — MUST use the generator

**CRITICAL: Never hand-write wire type structs in `pkg/fdbgo/wire/types/`.** All FDB wire types (request/reply structs) MUST be generated via the C++ schema extractor:

1. Register the type in `cmd/fdb-schema-extract/extract.h` (`REGISTER_GO_TYPE`, `REGISTER_FIELD_NAMES`)
2. Add `extractType<T>(outDir, "Name")` in `cmd/fdb-schema-extract/main.cpp`
3. Run `just generate-wire-types` → produces `*_generated.go` files
4. Run `just gazelle` to update BUILD files

The only exception is `keyrangeref_custom.go` which overrides serialization for the `equalsKeyAfter` optimization (documented, matches C++ exactly). All other wire types are generated.

## Stack

- **Language**: Go (see go.mod for version)
- **Database**: FoundationDB via pure Go client (`pkg/fdbgo/fdb`) or Apple CGo binding
- **Serialization**: Protocol Buffers (Apple's original proto definitions)
- **Proto codegen**: buf — use `just generate` to regenerate
- **Build system**: Bazel 9 (via bazelisk), MODULE.bazel + gazelle for BUILD files
- **Go linting**: nogo (runs during Bazel compilation — lint errors are build errors)
- **Task runner**: just (thin wrappers around bazel commands)
- **Testing**: testcontainers-go (real FDB per test, no mocks)

### Package structure
- `pkg/recordlayer/` - Main Record Layer implementation
- `gen/` - Generated protobuf Go code from Apple's proto definitions
- `proto/` - Apple's original protobuf definitions

## Project layout

```
MODULE.bazel                        # Bazel module config (Go + Java deps)
BUILD.bazel                         # Root: gazelle + nogo
.bazelversion                       # Pins Bazel 9
.bazelrc                            # Bazel settings
nogo_config.json                    # nogo analyzer config (lint = build errors)
go.mod                              # github.com/birdayz/fdb-record-layer-go
justfile                            # Task runner (wraps bazel commands)
pkg/recordlayer/                    # Main Record Layer implementation
  BUILD.bazel                       # Generated by gazelle
  store.go, database.go, ...        # Implementation files
  chaos/                            # Model-based chaos testing framework
    fault.go                        # ChaosTransactor, fault types, injection
    model.go                        # StoreModel (in-memory shadow)
    scenario.go                     # Scenario (ties chaosDB + model + verification)
    verify.go                       # Verify() — model vs store comparison
    chaos_test.go                   # Chaos tests (16 tests, targeted + random)
gen/                                # Generated Go code from Apple's proto defs
  BUILD.bazel                       # Generated by gazelle
proto/apple/                        # Apple's original protobuf definitions
  BUILD.bazel                       # proto_library + java_proto_library
conformance/                        # Java compatibility conformance tests
  BUILD.bazel                       # Generated by gazelle (Go test targets)
  java/BUILD.bazel                  # Java conformance server (Bazel-built)
buf.yaml                            # buf module config
buf.gen.yaml                        # buf codegen config
TODO.md                             # Tracked issues and improvements
```

## Running

```sh
just build                    # bazel build //... (includes nogo lint)
just test                     # bazel test //... (fully cached, incremental)
just bench                    # Run all benchmarks (9 benchmarks, ~50s)
just bench-one NAME           # Run specific benchmark by regex
just gazelle                  # Regenerate BUILD files after adding/removing Go files
just generate                 # buf generate (proto codegen — not in Bazel)
just tidy                     # go mod tidy
just clean                    # bazel clean
just binding-stress           # Binding tester: 100 seeds × 1000 ops
just binding-stress 50 500    # Binding tester: 50 seeds × 500 ops
just binding-stress-duration 2h  # Binding tester: run for 2 hours
just binding-stress-directory    # Directory binding tester: 50 seeds × 500 ops
```

### Binding tester stress (`fdb-binding-stress`)

Seeded PRNG fuzz testing of the pure Go FDB client against the official FDB binding tester harness. Each seed generates a deterministic sequence of FDB operations (GET, SET, CLEAR, GET_RANGE, ATOMIC_OP, conflict ranges, etc.) with random keys/values. The harness compares our output against a Python reference client — any divergence is a bug.

**How it works:** For each seed, a fresh FDB Docker container is started, the binding tester inserts instructions, our Go stacktester executes them, then results are compared. All artifacts are collected per-seed.

**Output:** `binding-stress-out/<timestamp>/` containing:
- `report.json` — machine-readable results (updated after every seed, safe to read mid-run)
- `seed-N/tester.log` — binding tester stdout/stderr
- `seed-N/docker.log` — FDB container logs
- `seed-N/fdb-logs/` — FDB XML trace logs (for `addr2line` crash analysis)

**Reproduce a specific seed:**
```sh
bazelisk run //cmd/fdb-binding-stress -- -seeds 1 -ops 1000 -seed-start 146
```

**Debugging FDB crashes:** See `pkg/fdbgo/client/CRASH_BUG.md` for the full playbook — download debug symbols, extract `addr2line` command from FDB trace logs, resolve to source lines.

- **After writing Go code, always run `just build`** to compile + nogo lint. Lint errors are build errors.
- **Run `just test` regularly** — Bazel cache is perfect, so it only reruns what changed. Catches regressions early. Run at minimum after each feature/fix before committing.
- After adding/removing Go files or changing `go.mod`, run `just gazelle` then `bazel mod tidy`.
- Proto codegen stays with `buf generate` — not in Bazel.
- **IMPORTANT**: Always use `bazelisk` (not `bazel`) when running bazel commands directly. The `just` recipes handle this, but if invoking bazel manually, use `bazelisk`.

### Fuzz testing

`fuzz_test.go` contains 7 Go native fuzz targets covering all hand-rolled binary parsers. Seed corpus runs as regression tests under `bazel test`. Continuous fuzzing:

```sh
# Run a specific fuzz target for 60 seconds:
bazelisk run //pkg/recordlayer:recordlayer_test -- \
  -test.run='^$' \
  -test.fuzz='^FuzzFastUnpack$' \
  -test.fuzzcachedir=/tmp/fuzz_cache \
  -test.fuzztime=60s
```

**Available fuzz targets:**

| Target | What it fuzzes | Bugs found |
|---|---|---|
| `FuzzFastUnpack` | Hand-rolled FDB tuple decoder vs `tuple.Unpack` | 2 panics (truncated bytes/int), upstream `tuple.Unpack` panics |
| `FuzzFastUnpackRoundtrip` | Pack→unpack roundtrip consistency | Clean |
| `FuzzDeserializeBunch` | TEXT index custom binary format (varints + tuples) | OOM via crafted varint sizes |
| `FuzzUnwrapContinuation` | Continuation token parser (proto + raw) | Clean |
| `FuzzUninvertBytes` | DESC ordering 7-bit encoder roundtrip | Clean |
| `FuzzDeserializeVector` | HNSW vector binary format | Clean |
| `FuzzCompleteVersionFromBytes` | 12-byte record version parser | Clean |
| `FuzzConcatContinuation` | ConcatCursor proto continuation deserializer | Clean |
| `FuzzFlatMapContinuation` | FlatMapPipelined proto continuation deserializer | Clean |
| `FuzzDedupContinuation` | Dedup cursor proto continuation deserializer | Clean |

**Note:** Upstream `tuple.Unpack` (FDB Go bindings) panics on truncated input — see birdayz/fdb-record-layer-go#2. Our `fastUnpack` is hardened and should be used instead in all deserialization paths.

### Benchmarks

`benchmark_test.go` contains 15 benchmarks covering critical hot paths. Self-initializes FDB via testcontainers if Ginkgo's `SynchronizedBeforeSuite` hasn't run, so benchmarks work standalone.

```sh
just bench                          # All benchmarks (~60s)
just bench-one BenchmarkSaveRecord  # Single benchmark by regex
```

**Available benchmarks:**

| Benchmark | What it measures |
|---|---|
| `BenchmarkSaveRecord` | Single Order save + tx commit |
| `BenchmarkLoadRecord` | Load by primary key |
| `BenchmarkScanRecords` | Forward scan over 100 records |
| `BenchmarkSaveRecordWithIndex` | Save with VALUE index |
| `BenchmarkScanIndex` | Scan 100 VALUE index entries |
| `BenchmarkSaveRecordWithMultipleIndexes` | Save with VALUE + COUNT + SUM |
| `BenchmarkGetRecordCount` | Atomic record count read |
| `BenchmarkSaveLargeRecord` | 50KB record (below split threshold) |
| `BenchmarkSaveSplitRecord` | 250KB record (3 split chunks) |
| `BenchmarkStoreOpen` | Open existing store (uncached) |
| `BenchmarkStoreOpenCached` | Open with state cache enabled |
| `BenchmarkDeleteRecord` | Delete by primary key |
| `BenchmarkSaveRecordWithCountAndIndex` | Save with COUNT + VALUE index |
| `BenchmarkSaveRecordBatch` | 10 records/tx with VALUE index |
| `BenchmarkScanWithContinuation` | Paged scan (100 records, 10 pages, continuations) |

**Baseline numbers** (Ryzen 9 3900X, FDB 7.3.46 testcontainer, 2026-03-28):

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| SaveRecord | 2,176,230 | 3,136 | 101 |
| LoadRecord | 410,904 | 2,906 | 91 |
| ScanRecords (100) | 624,915 | 55,435 | 1,414 |
| SaveRecordWithIndex | 2,187,184 | 3,920 | 117 |
| ScanIndex (100) | 576,417 | 47,240 | 1,485 |
| SaveRecordWithMultipleIndexes | 2,198,939 | 4,916 | 139 |
| GetRecordCount | 406,424 | 3,149 | 89 |
| SaveLargeRecord (50KB) | 2,262,228 | 121,511 | 101 |
| SaveSplitRecord (250KB) | 2,510,222 | 549,871 | 122 |
| StoreOpen | 339,357 | 2,119 | 69 |
| StoreOpenCached | 359,933 | 2,215 | 72 |
| DeleteRecord | 2,159,154 | 2,767 | 90 |
| SaveRecordWithCountAndIndex | 2,189,853 | 4,939 | 131 |

**Go vs Java Record Layer comparison** (same FDB container, 100 iterations + 20 warmup, 2026-04-11):

| Operation | Go (us/op) | Java (us/op) | Ratio | Notes |
|---|---|---|---|---|
| SaveRecord | 2,594 | 2,434 | 1.07x | Goroutine coordination overhead |
| LoadRecord | 338 | 551 | **0.61x** | Go wins — pure Go client reads faster |
| ScanRecords (100) | 1,031 | 1,405 | **0.73x** | Go wins |
| SaveRecordWithIndex | 2,301 | 2,466 | **0.93x** | Go wins |
| ScanIndex (100) | 946 | 661 | 1.43x | JVM JIT tight-loop optimization |
| DeleteRecord | 2,572 | 2,445 | 1.05x | Near parity |
| StoreOpen | 231 | 270 | **0.85x** | Go wins |
| SaveBatch (10/tx) | 3,720 | 3,635 | 1.02x | Near parity |

Go wins 5/8 benchmarks. Uses pure Go FDB client (no CGo). Java uses FDB C binding (CGo). Ratio < 1.0 means Go is faster.

### Debugging Bazel cache invalidation

When builds unexpectedly recompile instead of using the cache:

1. **Find which option changed** (analysis cache): Look for `WARNING: Build option --foo has changed, discarding analysis cache` in output.

2. **Find why actions re-execute** (action cache): Use `--explain` + `--verbose_explanations`:
   ```sh
   bazelisk build //... --explain=/tmp/bazel_explain.log --verbose_explanations
   ```
   Then read `/tmp/bazel_explain.log` — it says exactly why each action ran (e.g. `Effective client environment has changed. Now using PATH=...`).

3. **Common culprits when switching between interactive/CI/Claude Code shells**:
   - `PATH` leaking into actions (fix: `--incompatible_strict_action_env` in `.bazelrc`)
   - `--test_env=VAR` inheriting different values (fix: pin values or remove)
   - `--isatty` / `--terminal_columns` auto-detected by Bazel client (cosmetic, usually not the real issue)

4. **`.bazelrc` uses `--incompatible_strict_action_env`** to prevent shell environment from leaking into build actions. This ensures cache sharing across different shell environments.

## Architecture

### Core types

| Go type | Java equivalent | Purpose |
|---|---|---|
| `FDBDatabase` | `FDBDatabase` | Wraps `fdb.Database`/`fdb.Tenant`, provides `Run()`/`RunWithVersionstamp()` |
| `FDBRecordContext` | `FDBRecordContext` | Wraps `fdb.Transaction`, version mutations, local version cache |
| `FDBRecordStore` | `FDBRecordStore` | Main record CRUD (Save, Load, Delete, Scan), index maintenance |
| `RecordMetaData` | `RecordMetaData` | Proto schema + record type metadata + indexes |
| `TypedFDBRecordStore[T]` | N/A (Go generics) | Type-safe wrapper with auto-type filtering on scan |
| `FDBRecordVersion` | `FDBRecordVersion` | 12-byte version (10 global versionstamp + 2 local) |
| `Index` | `Index` | Index definition (name, type, root expression, subspace key) |
| `StandardIndexMaintainer` | `StandardIndexMaintainer` | VALUE index maintenance (insert/update/delete/scan entries) |
| `VersionIndexMaintainer` | `VersionIndexMaintainer` | VERSION index maintenance (SET_VERSIONSTAMPED_KEY for incomplete) |
| `IndexEntry` | `IndexEntry` | Single entry from index scan (key, value, primary key extraction) |
| `TupleRange` | `TupleRange` | Range specification for index scans (ALL, AllOf, Between) |
| `SizeInfo` | `SplitHelper.SizeInfo` | Track key count/size, value size, split/version flags |
| `FDBDatabaseRunner` | `FDBDatabaseRunnerImpl` | Configurable retry with exponential backoff |
| `RecordContextConfig` | `FDBRecordContextConfig` | Transaction settings (timeout, priority, ID) |
| `FormerIndex` | `FormerIndex` | Tracks deleted indexes for schema evolution safety |

### Java compatibility — non-negotiable

Wire-level compatibility is the whole point. These MUST match Java exactly:
- Subspace constants (`RecordKey = 1`, `IndexKey = 2`, etc.) — all 10 verified
- Key construction (FDB tuple encoding)
- Protobuf serialization format
- Record store header format
- Builder pattern (`Create`, `Open`, `CreateOrOpen`, `Build`)
- Continuation tokens (protobuf-wrapped with magic number `6773487359078157740`)
- Index entry format (`[indexValues..., primaryKey...]` at `IndexKey` subspace, scannable via `ScanIndex`)
- Split record format (100KB chunks at suffixes 1, 2, 3...; unsplit at suffix 0)
- Record version storage (inline at `pk + -1` suffix, format version >= 6)

### Key patterns

```go
// Transaction pattern (matches Java's db.run())
db.Run(ctx, func(rtx *FDBRecordContext) (interface{}, error) {
    store := NewFDBRecordStoreBuilder().
        SetMetaData(metadata).
        SetContext(rtx).
        SetKeySpacePath(path).
        CreateOrOpen()
    return store.SaveRecord(record)
})

// Typed store (Go generics)
typedStore := NewTypedFDBRecordStore[*MyRecord](store)
record, err := typedStore.LoadRecord(ctx, primaryKey)
```

### Pure Go FDB client performance (vs CGo)

Reads: **Go 3.5x faster than CGo** (58us vs 205us single Get). Writes: **parity** (1005us vs 1007us Set+Commit). The read advantage comes from the pure Go network path (no CGo overhead, efficient channel-based multiplexing). Write parity achieved through QueueModel rewrite (C++ Smoother + server penalty) and commit path optimizations. 18 allocs/op on reads is the structural floor — top allocators all pooled or minimal.

TCP connections use `SetLinger(0)` (RST on close, prevents TIME_WAIT port reuse) and `SetKeepAlive(10s)` (faster dead connection detection). This eliminates the FDB server ASSERT crash from stale Peer entries under Docker/CI load.

### FDB constraints to respect

- 5-second transaction time limit → cursors need `TimeScanLimiter` and continuations
- 100KB value size limit → handled by split records (`SetSplitLongRecords(true)`)
- 10MB transaction size limit
- Key size limit (~10KB)

### Java source reference

Java source at `fdb-record-layer/` in repo root (gitignored), checked out at tag **4.10.6.0**. Maven artifact also 4.10.6.0 (MODULE.bazel). All 15 proto files synced from Java source. Key files:
- `FDBRecordStore.java` — core CRUD, counting, save logic (5800+ lines)
- `FDBRecordStoreKeyspace.java` — subspace constants (0-9)
- `SplitHelper.java` — split/unsplit record logic
- `KeyValueCursorBase.java` — continuation token format
- `FDBRecordVersion.java` — version structure
- `StandardIndexMaintainer.java` — VALUE index maintenance
- `VersionIndexMaintainer.java` — VERSION index maintenance
- `RecordMetaData.java` / `RecordMetaDataBuilder.java` — metadata

## Design principles

1. **Compatibility first** — match Java wire format exactly, even when Go idioms differ
2. **C++ is the spec for the FDB client** — if our Go client behaves differently from C++, that's a bug in our code. NEVER skip a test that shows divergence from C++. Fix the Go code to match C++ behavior instead. The entire point is behavioral compatibility.
3. **No mocks** — test against real FDB, catch real bugs
4. **Explicit errors** — never panic in library code, always return errors
5. **Simple code** — no unnecessary abstraction. Three similar lines > premature abstraction
6. **Proto fidelity** — respect protobuf semantics (open enums, field presence, wire compat)
7. **Test hard** — t.Parallel() where possible, cover edge cases, test Java interop
8. **Error types, not sentinels** — see Error handling section below

## Error handling

**Architecture: Java exception class = Go error struct. Always.**

Every Java exception class that we handle maps to a Go `struct` implementing `error`. Use `errors.As()` for matching (Go equivalent of `catch (SpecificException e)`). Never use bare `var ErrFoo = errors.New("...")` sentinel errors.

**Why:** Java exceptions carry structured context at throw sites (primary key, index name, subspace, etc. via `addLogInfo()`). Sentinel errors can't carry context. `errors.As()` is the idiomatic Go mechanism for type-matching + data extraction.

**Pattern:**
```go
// Define: one type per Java exception class
type RecordAlreadyExistsError struct {
    PrimaryKey tuple.Tuple  // matches Java's LogMessageKeys.PRIMARY_KEY
}

func (e *RecordAlreadyExistsError) Error() string {
    return fmt.Sprintf("record already exists: %v", e.PrimaryKey)
}

// Return: always with context
return &RecordAlreadyExistsError{PrimaryKey: pk}

// Match: errors.As() — equivalent of catch (RecordAlreadyExistsException e)
var e *RecordAlreadyExistsError
if errors.As(err, &e) {
    log.Printf("duplicate at PK %v", e.PrimaryKey)
}
```

**Rules:**
- Every error type struct carries the same context fields as the Java exception's `addLogInfo()` keys
- Error message in `Error()` should be descriptive (include context values), but callers must NOT string-match — use `errors.As()` only
- No sentinel `var ErrFoo` variables — they lose context and encourage `errors.Is()` string matching
- For errors that are genuinely message-only (like `MetaDataException` in Java), use a `MetaDataError{Message string}` type — still a struct, still matchable via `errors.As()`
- Wrap with `fmt.Errorf("context: %w", err)` to add call-site context while preserving `errors.As()` unwrapping

**Java → Go exception mapping reference:**

| Java Exception | Go Error Type | Context Fields |
|---|---|---|
| `RecordAlreadyExistsException` | `RecordAlreadyExistsError` | `Message`, `PrimaryKey` |
| `RecordDoesNotExistException` | `RecordDoesNotExistError` | `Message`, `PrimaryKey` |
| `RecordTypeChangedException` | `RecordTypeChangedError` | `Message`, `PrimaryKey`, `ActualType`, `ExpectedType` |
| `RecordStoreAlreadyExistsException` | `RecordStoreAlreadyExistsError` | (none) |
| `RecordStoreDoesNotExistException` | `RecordStoreDoesNotExistError` | (none) |
| `RecordStoreNoInfoAndNotEmptyException` | `RecordStoreNoInfoButNotEmptyError` | `FirstKey` |
| `UninitializedRecordStoreException` | `RecordStoreStateNotLoadedError` | (none) |
| `ScanNonReadableIndexException` | `IndexNotReadableError` | `IndexName`, `CurrentState` |
| `StoreIsLockedForRecordUpdates` | `StoreIsLockedForRecordUpdatesError` | `Reason`, `Timestamp` |
| `StoreIsFullyLockedException` | `StoreIsFullyLockedError` | `Reason`, `Timestamp` |
| `UnknownStoreLockStateException` | `UnknownStoreLockStateError` | `LockStateValue` |
| `StaleMetaDataVersionException` | `StaleMetaDataVersionError` | `LocalVersion`, `StoredVersion` |
| `MetaDataException` | `MetaDataError` | `Message` |
| `MetaDataEvolutionValidatorException` | `MetaDataEvolutionError` | `Message` |
| `UnsupportedFormatVersionException` | `UnsupportedFormatVersionError` | `Version`, `MaxVersion` |
| `RecordSerializationException` | `RecordSerializationError` | `Cause` (with `Unwrap()`) |
| `RecordDeserializationException` | `RecordDeserializationError` | `PrimaryKey`, `Cause` (with `Unwrap()`) |
| `RecordIndexUniquenessViolation` | `RecordIndexUniquenessViolationError` | `IndexName`, `IndexKey`, `PrimaryKey`, `ExistingKey` |
| `IndexKeySizeException` | `IndexKeySizeError` | `IndexName`, `PrimaryKey`, `KeySize`, `Limit` |
| `IndexValueSizeException` | `IndexValueSizeError` | `IndexName`, `PrimaryKey`, `ValueSize`, `Limit` |
| (no Java equivalent) | `IndexNotFoundError` | `IndexName` |
| (no Java equivalent) | `IndexNotBuiltError` | `IndexName` |
| `KeyExpression.InvalidExpressionException` | `KeyExpressionError` | `Message` |

## Proto definitions

**Use Java's proto files directly.** The canonical definitions live at `fdb-record-layer/fdb-record-layer-core/src/main/proto/`. Our `proto/apple/` should mirror those — do NOT hand-maintain a subset and add messages one by one. Copy the full proto when adding new message types.

## Cursor/continuation design

Implemented: `KeyValueCursorContinuation`, `ConcatContinuation` (protobuf-wrapped). Supports forward/reverse scan, byte/row/time limits, split record reassembly, isolation levels. `ConcatCursor` and `MapCursor` combinators available.

Each continuation serializes cursor state to bytes for reconstruction across transaction boundaries. Our continuations must be wire-compatible with Java's.

## Chaos testing

Model-based chaos testing framework at `pkg/recordlayer/chaos/`. Maintains an in-memory model that shadows the real FDB store. After each operation, verifies they agree. Disagreement = bug.

### Architecture

```
ChaosTransactor          wraps fdb.Transactor, injects faults at transaction boundary
StoreModel               in-memory shadow (map of records + COUNT_UPDATES tracker)
Scenario                 ties together chaosDB, cleanDB, model, fault config, PRNG
Verify()                 compares model against real store (records, indexes, counts)
```

**Key insight:** `ChaosTransactor` implements `fdb.Transactor`. One constructor (`NewFDBDatabaseWithTransactor`) in production code enables the entire framework — zero other changes to production code.

### What Verify checks

1. **Record count** — `store.GetRecordCount()` vs `model.Count()`
2. **Record existence** — every model record exists in store
3. **No orphan records** — every store record exists in model
4. **Scan count cross-check** — full scan count matches model
5. **VALUE index entries** — compute expected entries from model (evaluate expression + trim PK), scan actual, bidirectional diff
6. **COUNT/SUM index values** — compute expected per grouping key from model state, scan actual, compare
7. **COUNT_UPDATES index values** — tracked cumulatively in model (increments on every save)

### Fault types

Currently implemented: `FaultCommitUnknown` — simulates FDB error 1021 (`commit_unknown_result`). The transaction commits successfully, then the function is re-executed in a new transaction. Tests whether operations are idempotent under retry.

### How to use

**Targeted fault injection** — inject a specific fault on the next operation:
```go
s := NewScenario(t, testRealDB, md)
s.InjectOnce(FaultCommitUnknown)
s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
s.Verify() // checks all invariants
```

**Random fault injection** — continuous random faults at configured rate:
```go
s := NewScenario(t, testRealDB, md, WithSeed(12345), WithFaults(FaultsRetryHeavy))
// FaultsRetryHeavy = 5% commit-unknown rate
for i := 0; i < 200; i++ {
    s.SaveRecord(...)
    if (i+1)%20 == 0 { s.Verify() }
}
```

**Reproducibility** — same seed = same operations = same faults:
```go
WithSeed(12345)  // deterministic PRNG for both operations and fault injection
```

### Running chaos tests

```sh
# All chaos tests
bazelisk test //pkg/recordlayer/chaos:chaos_test --test_output=streamed

# Specific test
bazelisk test //pkg/recordlayer/chaos:chaos_test --test_arg="-test.run=TestCountIndexCommitUnknown" --test_output=streamed
```

### How to hunt for bugs with a new index type

1. Create metadata with the index: `buildXxxMetadata()` function in `chaos_test.go`
2. Write a targeted test: `InjectOnce(FaultCommitUnknown)` + `SaveRecord` + `Verify()`
3. Write a random stress test: `WithFaults(FaultsRetryHeavy)` + loop of random saves/deletes
4. If `Verify()` needs to check something new, extend `verify.go`
5. Run and read the violation messages — they tell you exactly what diverged

### How to confirm Java-consistent behavior

Use chaos tests to verify our behavior matches Java's. Example: COUNT_UPDATES is non-idempotent under commit-unknown in both Go and Java (`AtomicMutationIndexMaintainer.skipUpdateForUnchangedKeys()` returns `false`). The chaos test documents this as a known limitation rather than a bug.

### Known findings

| Index Type | Commit-Unknown Safe? | Why |
|---|---|---|
| VALUE | Yes | `removeCommonEntries` skips identical entries on retry |
| COUNT | Yes | `removeCommonGroupingKeys` skips unchanged keys |
| SUM | Yes | `removeCommonSumEntries` skips identical (key, value) pairs |
| COUNT_UPDATES | **No** | `skipUpdateForUnchangedKeys=false` — always ADD +1 (matches Java) |
| Record count | Yes | `loadExistingRecord` detects update vs insert |

### Extending the framework

- **New fault type**: Add to `FaultType` enum in `fault.go`, implement in `ChaosTransactor.Transact()`
- **New verification**: Add to `Verify()` in `verify.go` (or add a new `verifyXxx` function)
- **New model tracking**: Extend `StoreModel` in `model.go` (e.g., `CountUpdates` map for event counting)
- **New operations**: Add methods to `Scenario` in `scenario.go` (follows SaveRecord/DeleteRecord pattern)

## Conformance status (updated 2026-04-13)

See `TODO.md` for full gap analysis. Summary:
- **Record Layer**: CRUD, split records, continuation tokens, record versioning, record counting, **all 19 index types** (VALUE, COUNT, COUNT_NOT_NULL, COUNT_UPDATES, SUM, MAX_EVER_LONG, MIN_EVER_LONG, MAX_EVER_TUPLE, MIN_EVER_TUPLE, RANK, VERSION, MAX_EVER_VERSION, PERMUTED_MIN, PERMUTED_MAX, BITMAP_VALUE, TEXT, TIME_WINDOW_LEADERBOARD, MULTIDIMENSIONAL, VECTOR), KeyWithValueExpression covering indexes, index scanning/state/build/rebuild, cursor combinators (concat/map/filter/skip/limit/union/intersection/dedup/flatmap/chained/auto-continuing/fallback), time/byte/record scan limits, MetaDataValidator, MetaDataEvolutionValidator, commit hooks, retry runner, store state management, EvaluateAggregateFunction, EvaluateRecordFunction, FDB directory layer, FDBMetaDataStore
- **FDB Client vs C**: 100% data-path API coverage (all `fdb_transaction_*` read/write/atomic/watch/conflict/versionstamp functions). 78/81 C binding unit tests ported. Correctness audit (nightshift-9 + dayshift-10): 5 divergences fixed (getKey shard resolution, backoff cap, future_version delay, GRV cache per-priority ratekeeper, watch cancellation on Reset, commitDummyTransaction intersects). 5 remaining divergences are design choices or perf optimizations. Missing API: 6 observability/admin functions only.
- **Key gaps**: AtomKE (LOW, Java interface only), synthetic record types, query planner
- **Test counts**: 2670 Ginkgo specs + 430 conformance specs + 53 chaos tests + 78 C binding port tests + 45 correctness tests + 15 Go↔CGo interop tests + 200+ binding tester seeds (0 failures, API + directory)
- **Line coverage**: 80.0% overall, 82.8% (client), 81.3% (record layer). `just coverage` generates HTML report.
- **Fuzz targets**: 11 (10 record layer parsers + FuzzRYWCache for client read-your-writes merge logic)
- **Performance**: Go wins 5/8 benchmarks vs Java Record Layer. Reads 27-39% faster, writes within 2-7%. See comparison table above.
