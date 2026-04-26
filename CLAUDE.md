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

**Pacing is NEVER the model's call.** During a vollkonti shift, do not autonomously slow down, "let things settle," "find a stopping point," or rationalize coasting on grounds of marginal value, PR size, milestone reached, or reviewer empathy. Stops are EXTERNAL signals only: user intervention, mid-shift check-in trigger, wind-down clock at T+7:30. Heuristics from human-paced codebase practice ("keep PRs focused," "don't pile on changes," "let big work rest before review") DO NOT apply here — the system is designed for continuous output. If you catch yourself reasoning about whether to slow down, the answer is no — keep working.

The failure modes that trigger this rule, and what's wrong with each:

1. **Trained heuristics from human SWE practice** (don't pile on changes, keep PRs focused, let things rest) — valid for shared codebases with reviewer fatigue and merge conflicts; misapplied here.
2. **Marginal-value reasoning** ("each new commit is incrementally less valuable") — unbounded; ALWAYS feels true at some point. Not a permission to coast.
3. **Reviewer-empathy projection** (imagining reviewer frustration with a 70+ commit PR) — PR shape is the reviewer's scoping problem, not your pre-emptive constraint.
4. **Milestone pattern-matching** (a major thing landed, "breathe") — milestones are points on the path, not stopping signals.
5. **Conservative-default bias** (small diffs feel safe; big diffs feel risky) — CI is the safety net. Stop relying on self-restraint.

## Work tracking & workflow

See `TODO.md` in repo root for tracked issues and improvements. **TODO.md is the authoritative priority list** — flat priority buckets `## CRITICAL > ## HIGH > ## MEDIUM > ## LOW`, items as `- [x]` / `- [ ]`.

**Priority discipline at shift start:**

1. Read the latest handover for shift state, last-PR feedback, and the next-shift follow-ups it lists.
2. Read TODO.md and identify the **highest unchecked priority bucket that has open items** (CRITICAL first, then HIGH, then MEDIUM, then LOW).
3. Pick work from THAT bucket. The handover's "Next-shift follow-ups" are *suggestions and context*, not the priority list. They are usually MEDIUM/LOW tactical clean-ups; finishing them is fine, but it is **never a substitute for unchecked CRITICAL/HIGH items**.
4. If the handover's follow-ups happen to align with the highest-priority bucket — great, do them. If they're lower-priority than what's open in CRITICAL/HIGH, do the CRITICAL/HIGH item first.
5. **When a CRITICAL or HIGH item is sitting unchecked, you do NOT get to spend a shift on MEDIUM follow-ups.** That's the failure mode this rule exists to prevent.

**Order of operations within a shift:** CRITICAL > HIGH > MEDIUM > LOW. Finish what you start before climbing the priority ladder downward — a half-done CRITICAL item is worse than a finished HIGH one. But starting a fresh MEDIUM when CRITICAL is still open is wrong.

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
  keyspace/                         # KeySpace directory tree abstraction (Phase 1)
  chaos/                            # Model-based chaos testing framework
    fault.go                        # ChaosTransactor, fault types, injection
    model.go                        # StoreModel (in-memory shadow)
    scenario.go                     # Scenario (ties chaosDB + model + verification)
    verify.go                       # Verify() — model vs store comparison
    chaos_test.go                   # Chaos tests (16 tests, targeted + random)
pkg/relational/core/embedded/       # SQL engine (driver.Conn impl + executors)
  connection.go                     # EmbeddedConnection struct + driver-layer methods
  select_dispatch.go                # execSelect / execSelectQuery entry points
  select_query_full.go              # Main single-table SELECT executor
  select_parser.go                  # parse-tree → selectQuery
  select_helpers.go                 # row-map helpers shared by SELECT paths
  join.go                           # Nested-loop INNER/LEFT/RIGHT JOIN
  aggregate.go                      # Hash-aggregate + GROUP BY
  cte_scan.go / recursive_cte.go    # CTE consumers + recursive materialisation
  union.go                          # UNION ALL / UNION DISTINCT
  insert.go / update_delete.go / ddl.go  # DML + DDL executors
  pk_pushdown.go / pk_prefix_pushdown.go
  secondary_index_pushdown.go / in_list_pushdown.go
  like_prefix_pushdown.go / covering_index.go
  order_by.go                       # ORDER BY elimination + reverse-scan direction
  where_extractors.go               # Shared AND-leaf predicate extractors
  system_tables.go / system_rows.go # INFORMATION_SCHEMA + driver.Rows shims
  scope.go / scalar_subquery.go     # Lexical scopes + uncorrelated-subquery cache
  utilities.go / tri_bool.go        # Pure helpers + Kleene three-valued logic
  stmt.go                           # driver.Stmt impl
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
just bench                    # Run all benchmarks (17 record layer benchmarks)
just bench-one NAME           # Run specific benchmark by regex
just gazelle                  # Regenerate BUILD files after adding/removing Go files
just generate                 # buf generate (proto codegen — not in Bazel)
just tidy                     # go mod tidy
just clean                    # bazel clean
just verify                   # Pre-merge: build + test + race detector + fuzz smoke
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

`fuzz_test.go` contains 12 Go native fuzz targets covering all hand-rolled binary parsers and protobuf deserialization. Seed corpus runs as regression tests under `bazel test`. Continuous fuzzing:

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
| `FuzzDeserializeAndDiscover` | Union wire format record type discovery (protowire) | Clean |
| `FuzzDeserializeRecord` | Union wire format targeted record extraction (protowire) | Clean |
| `FuzzRYWCache` | RYW cache Set/Clear/ClearRange/AtomicAdd vs map model (forward + reverse range) | Model bug found during development (ClearRange boundary) |
| `FuzzPackIntoEquivalence` | `PackWithPrefixInto`/`Pack1Into`/`PackInt64Into`/`PackConcatInto` vs allocating equivalents | Clean |
| `FuzzLikePrefixStrinc` | LIKE-prefix byte-level successor used by the SQL range-scan pushdown | Clean |
| `FuzzLikePatternToPrefix` | LIKE-pattern prefix extractor (with ESCAPE handling) — cross-checked against likeMatch | Clean |
| `FuzzLikeMatch` | likeMatch (no escape) vs regex oracle | Clean |
| `FuzzLikeMatchEscape` | likeMatch (with escape rune) vs regex oracle | Trailing-escape bug found nightshift-48 (was matched as literal; fixed to "malformed → no match") |
| `FuzzSimplifyValue_ArithmeticTree` | cascades SimplifyValue on a 3-leaf arithmetic composite — no panic, idempotent, fully-folded result | Clean (22.5M execs/20s, swingshift-50) |
| `FuzzSimplifyValue_CastChain` | cascades SimplifyValue on CAST(CAST(x AS X) AS Y) — no panic + idempotency | Clean (swingshift-50) |
| `FuzzMaximumType_Properties` | cascades type lattice — symmetry, idempotence, closure invariants of MaximumType over primitive Type pairs | Clean (22.1M execs/15s, swingshift-52) |
| `FuzzSimplify_PredicateTree` | cascades QueryPredicate-level Simplify driver under random AND/OR/NOT shapes — no panic + idempotent under both DefaultSimplifyRules and NormalizationRules | Clean (2.5M execs/15s, swingshift-50) |

**Note:** `FuzzRYWCache` is in `pkg/fdbgo/client/ryw_fuzz_test.go`, `FuzzPackIntoEquivalence` is in `pkg/fdbgo/fdb/tuple/tuple_test.go`, `FuzzLikePrefixStrinc` / `FuzzLikePatternToPrefix` / `FuzzApplyMathOp` / `FuzzApplyBitOp` are in `pkg/relational/core/embedded/embedded_test.go`, `FuzzLikeMatch` / `FuzzLikeMatchEscape` are in `pkg/recordlayer/query/plan/cascades/predicates/comparisons_test.go`, `FuzzSimplifyValue_ArithmeticTree` / `FuzzSimplifyValue_CastChain` are in `pkg/recordlayer/query/plan/cascades/values/values_fuzz_test.go`, `FuzzMaximumType_Properties` is in `pkg/recordlayer/query/plan/cascades/values/type_test.go`, `FuzzSimplify_PredicateTree` is in `pkg/recordlayer/query/plan/cascades/predicate_simplify_fuzz_test.go` (all others in `pkg/recordlayer/fuzz_test.go`). Run with `bazelisk run //pkg/fdbgo/client:client_test -- -test.fuzz='^FuzzRYWCache$'`.

**Note:** Upstream `tuple.Unpack` (FDB Go bindings) panics on truncated input — see birdayz/fdb-record-layer-go#2. Our `fastUnpack` is hardened and should be used instead in all deserialization paths.

### Benchmarks

`benchmark_test.go` contains 17 benchmarks covering critical hot paths. Self-initializes FDB via testcontainers if Ginkgo's `SynchronizedBeforeSuite` hasn't run, so benchmarks work standalone.

```sh
just bench                          # All benchmarks
just bench-one BenchmarkSaveRecord  # Single benchmark by regex
```

**Available benchmarks:**

| Benchmark | What it measures |
|---|---|
| `BenchmarkSaveRecord` | Single Order save + tx commit |
| `BenchmarkSaveRecordBuild` | Save with Build() — lazy state load |
| `BenchmarkLoadRecord` | Load by primary key |
| `BenchmarkLoadRecordBuild` | Load with Build() — no state load needed |
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

**Baseline numbers** (Ryzen 9 3900X, FDB 7.3.75 testcontainer, 2026-04-16):

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| SaveRecord | 1,008,148 | 6,403 | 80 |
| SaveRecordBuild | 1,009,295 | 3,916 | 39 |
| LoadRecord | 183,800 | 5,133 | 68 |
| LoadRecordBuild | 63,150 | 2,697 | 27 |
| ScanRecords (100) | 644,543 | 103,105 | 1,295 |
| SaveRecordWithIndex | 1,008,642 | 7,455 | 97 |
| ScanIndex (100) | 558,350 | 77,684 | 673 |
| SaveRecordWithMultipleIndexes | 1,010,416 | 10,960 | 121 |
| GetRecordCount | 192,033 | 5,613 | 80 |
| SaveLargeRecord (50KB) | 1,030,898 | 133,965 | 85 |
| SaveSplitRecord (250KB) | 1,142,246 | 727,766 | 118 |
| StoreOpen | 215,583 | 3,638 | 52 |
| StoreOpenCached | 122,947 | 3,707 | 53 |
| DeleteRecord | 1,018,145 | 5,553 | 77 |
| SaveRecordWithCountAndIndex | 1,013,301 | 10,728 | 111 |
| SaveRecordBatch (10/tx) | 1,894,226 | 35,711 | 354 |
| ScanWithContinuation | 2,053,684 | 145,860 | 2,078 |

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

Java source at `fdb-record-layer/` in repo root (gitignored), checked out at tag **4.11.1.0**. Maven artifacts also 4.11.1.0 (MODULE.bazel) — fdb-record-layer-core + fdb-relational-api + fdb-relational-core. All 15 proto files synced from Java source. Key files:
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
9. **NEVER paper over bugs** — if a test silently tolerates an error via early-return ("typed Java error acceptable, transport is not"), that test isn't testing anything. Fix the underlying bug or pin the actual expected behaviour explicitly. Tolerance gates compound: every shift after the original "papered over" gets to inherit the bug AND the false positive. Strict assertions surface real failures. swingshift-52 found two long-standing bugs — fdb-relational schema-URL parsing + NOT NULL syntax restriction — that had been silently tolerated since planSql was first written, because every conformance test had an `if got.Err != nil { return }` early-return. Don't do this.

## Java↔Go conformance gotchas (fdb-relational)

These are the integration constraints that bit us hard in swingshift-52. Add to this list when you find more.

- **Schema MUST be on the JDBC URL, not via `setSchema()`.** fdb-relational's `EmbeddedRelationalDriver.connect()` reads the schema from the URL query string (`?schema=NAME`, case-insensitive — see `RecordLayerStorageCluster#parseConnectionQueryString`). Calling `Connection.setSchema()` on the JDBC wrapper does NOT propagate to `EmbeddedRelationalConnection.currentSchemaLabel`. Every subsequent `executeQuery` / `executeUpdate` would fail with `RelationalException: No Schema specified` — the check is in `AbstractEmbeddedStatement#executeInternal`.
- **DDL needs the system "CATALOG" schema.** `CREATE SCHEMA TEMPLATE` / `CREATE DATABASE` / `CREATE SCHEMA` must run against `jdbc:embed:/__SYS?schema=CATALOG`. Without `?schema=CATALOG`, even DDL fails with "No Schema specified" because `executeInternal` rejects null-schema unconditionally. fdb-relational's own tests do this (`SchemaTemplateRule#beforeEach` calls `setSchema("CATALOG")` first).
- **`NOT NULL` is reserved for `ARRAY` column types in `CREATE TABLE`.** Plain `id BIGINT NOT NULL` fails with `RelationalException: NOT NULL is only allowed for ARRAY column type`. Primary-key columns are implicitly NOT NULL, so just write `id BIGINT, ..., PRIMARY KEY (id)` and trust the implicit constraint.
- **`VALUES` clause is restricted in fdb-relational SQL.** `SELECT ... FROM (VALUES (...)) AS T(col)` raises a syntax error. Use real tables for end-to-end tests; in-line constant tables aren't a portable substitute.
- **fdb-relational uppercases identifiers.** Column metadata returned from JDBC has uppercase names (`ID` not `id`). Pin uppercase in cross-engine assertions.
- **No INTEGER/FLOAT auto-promotion at INSERT time.** `INSERT INTO T (i) VALUES (42)` where `i INTEGER` fails with `SemanticException: A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable`. Bare numeric literals are typed BIGINT (for ints) and DOUBLE (for decimals); narrowing requires an explicit `CAST(42 AS INTEGER)` / `CAST(1.5 AS FLOAT)`.
- **BYTES literal syntax: `X'<hex>'`** (uppercase X) per `RelationalLexer.g4#HEXADECIMAL_LITERAL`. Lowercase `x'...'` is rejected. Base64 form is `B64'<base64>'`.
- **`blob` is reserved.** Use a different column name for BYTES columns — `BLOB`, `TINYBLOB`, `MEDIUMBLOB`, `LONGBLOB` are all keywords. `payload`, `data`, `bytes_value` are fine.
- **Each `runSql` / `runWithSetup` call uses a fresh ephemeral schema.** State (rows, schema) does NOT persist between calls. For INSERT-then-SELECT round-trip tests use `runWithSetup(setupSqls[], querySql)` — both phases share the schema.
- **`API_VERSION_7_3` does not exist.** fdb-record-layer 4.11.1.0's `APIVersion` enum has only `6_3`, `7_0`, `7_1` — there is no 7.3 enum. The FDB SERVER 7.3.75 supports older API versions. Don't try to bump unless we upgrade fdb-record-layer.
- **`setAPIVersion` throws if client already started.** When the conformance server runs many test suites in one process, a sibling test may init FDB first; `setAPIVersion` then throws `RecordCoreException("API version cannot be changed after client has already started")`. The shared driver-init helper catches this specific exception and proceeds.
- **`GROUP BY <col>` is not supported by fdb-relational 4.11.1.0's planner.** Returns `UnableToPlanException: Cascades planner could not plan query`. Bare `SELECT count(*)` (no GROUP BY) works. Wait until the planner ports the relevant rule (RFC-022 §4.5 Batch B) before adding GROUP BY corpus entries.
- **`LIMIT N` clause is not supported in SQL.** Returns `RelationalException: LIMIT clause is not supported.` — pagination is exposed as a JDBC `Statement.setMaxRows` knob, not SQL syntax. Don't add `... LIMIT N` corpus entries until this changes.
- **`SELECT DISTINCT` is not supported by the planner.** Returns `UnableToPlanException`, same as GROUP BY. Wait until RFC-022 §4.5 Batch B rules port the distinct rule.
- **Common SQL scalar functions (lower/upper/length/...) are NOT registered.** Returns `RelationalException: Unsupported operator <name>`. fdb-relational's function registry is small in 4.11.1.0; CASE expressions and basic arithmetic work, but most string/date/numeric helpers don't.
- **UUID columns report JDBC type-name `"OTHER"`, not `"UUID"`.** JDBC's standard for vendor-specific types is `Types.OTHER`; UUID's getColumnTypeName() returns "OTHER". UUID values come through as `java.util.UUID` instances; our encoder converts via `toString()` (e.g. `"00000000-0000-0000-0000-000000000042"`). UUID literal syntax is `CAST('<text>' AS UUID)` — no dedicated UUID literal in the grammar.
- **`INFORMATION_SCHEMA.*` is GO-side only — fdb-relational doesn't implement it at all.** Our Go embedded engine handles `INFORMATION_SCHEMA.SCHEMATA / TABLES / COLUMNS` (see `pkg/relational/core/embedded/system_tables.go`); fdb-relational 4.11.1.0's parser rejects `SELECT ... FROM INFORMATION_SCHEMA.TABLES` with a syntax error. Track A4 (system-table byte-equivalence) is therefore Go-only today and gated on adding INFORMATION_SCHEMA support to fdb-relational upstream — NOT something a Go-side shift can fix unilaterally without divergence.
- **Explicit `JOIN ... ON` is broken in fdb-relational 4.11.1.0.** Even fully-qualified column names (`Customers.cid`) raise `RelationalException: Attempting to query non existing column CUSTOMERS.CID` from the JOIN ON clause's column resolution. Use comma-separated table list with `WHERE` instead: `FROM Customers, Sales WHERE Customers.cid = Sales.cid`. (Inner-join semantics only — LEFT/RIGHT/FULL OUTER need explicit JOIN syntax and are therefore unavailable.)
- **Multi-column `ORDER BY` is unsupported by fdb-relational 4.11.1.0's Cascades planner.** Both `ORDER BY a, b` (all ASC) and mixed `ORDER BY a ASC, b DESC` raise `UnableToPlanException: Cascades planner could not plan query`. Single-column ORDER BY only. Re-add multi-column corpus entries when the planner ports the relevant rule.
- **`BYTES` columns report JDBC type-name `"BINARY"`, not `"BYTES"`.** JDBC's standard for variable-length byte arrays is `Types.BINARY`; fdb-relational's `ResultSetMetaData.getColumnTypeName()` returns "BINARY". Our Go embedded driver matches via `jdbcTypeNameForFD(BytesKind) → "BINARY"`. Pin uppercase "BINARY" in cross-engine assertions on byte-array columns.
- **`MIN(s)` / `MAX(s)` over non-numeric columns is unsupported by fdb-relational 4.11.1.0.** `MIN(name)` over a STRING column raises `VerifyException: unable to encapsulate aggregate operation due to type mismatch(es)`. Numeric MIN / MAX only. Lexicographic min / max over strings or bytes needs a `SELECT name FROM t ORDER BY name LIMIT 1` rewrite (which itself isn't supported either — LIMIT N is JDBC-only).
- **Bare `WHERE flag` on a BOOLEAN column is rejected by fdb-relational** with `RelationalException: expected BooleanValue but got FieldValue`. Boolean columns must be compared explicitly: `WHERE flag = TRUE`. The grammar accepts `WHERE flag` but the planner rejects FieldValue-as-predicate downstream. Our Go embedded engine matches this strictness (`evalComparisonPredicateTri` rejects FullColumnName-as-predicate explicitly).
- **`ORDER BY <alias>` is unsupported by fdb-relational's planner.** `SELECT v AS amount FROM t ORDER BY amount` raises `UnableToPlanException`. The ORDER BY clause must reference the underlying column name, not a SELECT-list alias. Same applies to ORDER BY on non-projected columns (`SELECT name FROM t ORDER BY sortkey` where `sortkey` isn't in the projection).
- **SQL-standard apostrophe-escape (`''`) diverges between engines.** `INSERT INTO t VALUES ('it''s here')` produces stored value `it's here` in Go (correct SQL-standard unescape) but `it''s here` (literal doubled apostrophe) in fdb-relational. fdb-relational's lexer / parser doesn't unescape `''` at INSERT time. Cross-engine corpus entries should avoid apostrophe-in-string literals until upstream aligns.
- **Bit-shift operators `<<` / `>>` are tokenized but not implemented.** fdb-relational 4.11.1.0's lexer/parser accept `<<` and `>>`, but the function registry has no evaluator — the planner returns `RelationalException: Unsupported operator <<`. Bitwise AND / OR / XOR (`& | ^`) work fine; only shifts are missing.
- **Floating-point exponent literal requires uppercase `E`.** Per `RelationalLexer.g4#EXPONENT_NUM_PART`, scientific notation is `1.5E10` — `1.5e10` is rejected as a syntax error. The Go embedded engine accepts both; cross-engine corpus entries should use uppercase `E`.
- **`IS TRUE` / `IS FALSE` predicate forms are unsupported on BOOLEAN columns.** fdb-relational 4.11.1.0 rejects `WHERE flag IS TRUE` at the planner. The supported form is `WHERE flag = TRUE`.
- **Division-by-zero error messages differ.** Java's stock `ArithmeticException` says `"/ by zero"`; Go's eval says `"division by zero"`. Cross-engine error-substring assertions should use the case-insensitive substring `"zero"`.
- **CAST-overflow error messages differ.** Java's CastValue says `"Value out of range for INT"`; Go says `"value … out of range for INTEGER"`. Common substring: `"out of range"`.
- **`IN (..., NULL, ...)` lists diverge.** SQL standard + Go propagate UNKNOWN through three-valued logic, so `WHERE x IN (1, NULL)` matches `x=1` and yields UNKNOWN otherwise (`NOT IN (1, NULL)` therefore yields UNKNOWN ⇒ empty result). fdb-relational 4.11.1.0 rejects NULL in the IN list outright with `"NULL values are not allowed in the IN list"`. The divergence is one-sided (Java rejects, Go accepts) — not expressible in `ExpectErrorContains` (would need both engines to fail) and not a positive entry (Java rejects). No cross-engine corpus entry possible until one side aligns.

### Go embedded SQL engine — Java conformance status

The plandiff Go-vs-Java result-set harness lives in two halves:

- `conformance/run_sql_conformance_test.go` — **the cross-engine equivalence assertion**. Drives every `plandiff.SeedRunCorpus()` entry through Java AND Go and asserts byte-equal column metadata + row values. Adding a new test case is just appending `{Name, SchemaTemplate, SetupSqls, Query}` to the corpus; no baseline RowSet to capture, no test wiring.
- `pkg/relational/conformance/plandiff/go_runner_test.go` — Go-side runner smoke test (no Java dependency). Each corpus entry must execute through the embedded engine without error. Strict equivalence is asserted in conformance/, not here.

`RunQuery` carries no `Expected` field — pinned baselines are redundant given Java is pinned at Maven 4.11.1.0 and Go is what we control. Cross-engine drift surfaces immediately on either side.

As of swingshift-52, **all 170 corpus entries pass strict end-to-end equivalence** (column names + JDBC type names + row values, all strict, per-entry). The corpus also includes negative entries (`ExpectErrorContains`-tagged) that assert BOTH engines reject a query AND the error messages contain a (case-insensitive) substring — catching silent acceptance on one side, the kind of divergence success-only harnesses miss. When new gaps emerge, they're surfaced via `isGoFeatureGap` skips, never silent passes.

Conformance gaps **closed** in swingshift-52:

- ✅ **Identifier case** — JDBC normalizer at `staticRows.Columns()` uppercases unquoted identifiers (`id` → `ID`).
- ✅ **Qualified column stripping** — same normalizer drops `alias.` prefix in projected columns (`t.id` → `ID`, `u.name` → `NAME`).
- ✅ **Anonymous projection naming** — same normalizer emits synthetic `_N` (zero-based position) for any expression that isn't a simple identifier (`COUNT(*)` → `_0`, `id+10` → `_1`, `CASE ... END` → `_<pos>`).
- ✅ **JDBC type-name plumbing** — embedded driver implements `RowsColumnTypeDatabaseTypeName`. `staticRows.colTypes` populated from proto FieldDescriptor in the SELECT path; aggregate columns default to BIGINT (covers SUM/MIN/MAX/COUNT over integral types).
- ✅ **Empty result-set type inference** — same driver method, no longer dependent on first-row value inspection.
- ✅ **`UUID` column type — full end-to-end** — DDL parser accepts `key UUID`; metadata builder emits the `tuple_fields.UUID` proto sub-message reference (`.com.apple.foundationdb.record.UUID` with sfixed64 most/least bits, matching Java's `Type.uuidType` lowering); `CAST(string AS UUID)` validates and returns the canonical 36-char form; `ConvertToProtoValue` encodes the canonical string into the proto UUID message via `most_significant_bits` / `least_significant_bits` derived big-endian from the 16-byte UUID; `ProtoValueToDriver` reverses on read; `jdbcTypeNameForFD` reports `OTHER` (matching `java.sql.Types.OTHER` for UUID). See `pkg/relational/core/functions/proto_value.go`.
- ✅ **Engine-side type inference for arithmetic / CAST / CASE projection results** — `inferProjectionJDBCType` walks the projection-expression AST in `select_helpers.go` and returns the JDBC type name for `x + y`, `id + 10`, `CAST(... AS UUID)`, `CASE WHEN ... THEN 'low' END`, etc. Uses the `jdbcTypeMax` lattice (DOUBLE > FLOAT > BIGINT > INTEGER for numerics, identity for non-numeric same-type pairs). Plumbed through the JOIN executor (per-source type map) and the CTE-scan executor (cte.colTypes propagated through derived-table materialisation). The runner-side value-based inference fallback was deleted — engine-side inference covers every corpus entry.

The projection-name + type-name transformation is applied at the driver-output boundary (`staticRows.Columns()` and `ColumnTypeDatabaseTypeName`), NOT at the internal `.cols` field. Internal callers (CTE row-map keying, ORDER BY resolution against unqualified names, alias remapping) read `.cols` directly and bypass normalization, preserving correctness of internal lookups.

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

## Conformance status (updated 2026-04-17)

See `TODO.md` for full gap analysis. Summary:
- **Record Layer**: CRUD, split records, continuation tokens, record versioning, record counting, **all 19 index types** (VALUE, COUNT, COUNT_NOT_NULL, COUNT_UPDATES, SUM, MAX_EVER_LONG, MIN_EVER_LONG, MAX_EVER_TUPLE, MIN_EVER_TUPLE, RANK, VERSION, MAX_EVER_VERSION, PERMUTED_MIN, PERMUTED_MAX, BITMAP_VALUE, TEXT, TIME_WINDOW_LEADERBOARD, MULTIDIMENSIONAL, VECTOR), KeyWithValueExpression covering indexes, index scanning/state/build/rebuild, **OnlineIndexer** (BY_RECORDS, BY_INDEX, MULTI_TARGET, MUTUAL strategies; adaptive throttle; `WriteOnlyIfTooLargePolicy`; conflict avoidance; see RFC 020), cursor combinators (concat/map/filter/skip/limit/union/intersection/dedup/flatmap/chained/auto-continuing/fallback), time/byte/record scan limits, MetaDataValidator, MetaDataEvolutionValidator (full IndexValidatorRegistry), commit hooks, retry runner, store state management, EvaluateAggregateFunction, EvaluateRecordFunction, FDB directory layer, FDBMetaDataStore
- **FDB Client vs C**: 100% data-path API coverage (all `fdb_transaction_*` read/write/atomic/watch/conflict/versionstamp functions). 93 C binding unit tests ported. 10-area C++ conformance audit (dayshift-20): **18/21 divergences fixed** (server selection power-of-two random, ensureReadVersion race fix, plus all prior fixes). 3 remaining: auto-reset after commit (design), wrong-shard retry cap (conservative), GRV background refresh timing (perf). Missing API: 6 observability/admin functions only.
- **Key gaps**: AtomKE (LOW, Java interface only), synthetic record types, query planner/SQL layer (deferred — hardening first)
- **Test counts**: 2817 Ginkgo specs + 438 conformance specs + 220 chaos tests + 93 C binding port tests + 34 correctness tests + 15 Go↔CGo interop tests + 200+ binding tester seeds (0 failures, API + directory)
- **Line coverage**: 81.0% overall. `just coverage` generates HTML report.
- **Race detector**: CI runs race detector on all 5 FDB test targets. Locally: `just race-all`.
- **Fuzz targets**: 51 (`grep -rE "^func Fuzz" pkg/` to enumerate). Coverage is layered: 13 record-layer parsers (continuations, tuples, version, key expressions, metadata), 9 wire reply parsers (`pkg/fdbgo/client/parse_fuzz_test.go`), 7 wire-type round-trip / panic-fuzz targets (`pkg/fdbgo/wire/types/marshal_fuzz_test.go`: SplitRangeRequest/Reply, WaitMetricsRequest, WatchValueRequest from swingshift-50; KeyRangeRef_SingleKeyOptimization, GetKeyRequest, GetKeyServerLocationsReply from dayshift-51), 2 wire Reader constructor/ErrorOr fuzzers, FuzzRYWCache (read-your-writes cache vs map model), tuple PackIntoEquivalence, LIKE pattern (prefix + match no-escape + match escape) plus arithmetic/bit-op math, SQL parser (3 targets) + plandiff (2 targets) + catalog template + cascades simplify (3 targets — Value arithmetic / cast / predicate tree) + cascades type lattice (FuzzMaximumType_Properties — symmetry / idempotence / closure invariants, swingshift-52).
- **Performance**: Go wins 5/8 benchmarks vs Java Record Layer. Reads 27-39% faster, writes within 2-7%. See comparison table above.
