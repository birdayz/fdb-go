# fdb-record-layer-go TODO

Restructured 2026-04-13 (nightshift-9). Previous version: `git show 036697a:TODO.md`.
Correctness audit performed 2026-04-13 against C++ NativeAPI.actor.cpp.

Java Record Layer version: **4.10.6.0**. FDB wire protocol: **7.3.75**.

---

## Pure Go FDB Client (`pkg/fdbgo/`)

### Bugs

- [x] **getKey selector resolution across shard boundaries** — Go sent ONE request and returned the reply key, ignoring `orEqual` and `offset` fields from the `KeySelector` in the reply. C++ loops until `offset==0 && orEqual==true`. In multi-shard clusters, selectors crossing shard boundaries returned wrong keys. Fixed: full resolution loop matching C++. Not caught by tests (single-shard testcontainers).
- [x] **hot_shard/range_locked backoff cap** — Go used `DEFAULT_MAX_BACKOFF` (1s) for `transaction_throttled_hot_shard` (1235) and `transaction_rejected_range_locked` (1242). C++ uses `RESOURCE_CONSTRAINED_MAX_BACKOFF` (30s). Caused over-aggressive retry under hot-shard conditions. Fixed: moved to resource-constrained group.

- [x] **RYW getRange: limit=0 (unlimited) skipped slow path** — `remaining := limit` with `limit=0` caused `for remaining > 0` to never execute. Fixed: `if remaining <= 0 { remaining = math.MaxInt }` matching `readpath.go`. Discovered dayshift-10 multi-shard test.
- [x] **Data race in ensureReadVersion + tx.state** — 44 races: `tx.hasReadVersion` and `tx.state` read by Watch goroutines concurrently with writes from Commit→postCommitReset. Fixed: `readVersionMu` mutex for hasReadVersion/readVersion, `atomic.Int32` for state. Found by race detector (`--@rules_go//go/config:race`). dayshift-14.
- [x] **Data race in Watch vs postCommitReset on conflict slices** — Watch() goroutine calls AddReadConflictKey() (under conflictMu) while Commit()→postCommitReset() clears readConflicts/writeConflicts WITHOUT conflictMu. Fixed: hold conflictMu in postCommitReset() and reset() when clearing conflict slices; also protect self-conflicting copy+append in OnError(). Found by race detector. swingshift-15.
- [x] **getRange `more` flag incorrect across shard boundaries** — When limit is exactly met across multiple shards, `more` was taken from last shard's response (could be `false` even though subsequent shards have data). C++ sets `more = (data.size() == limit)` — always true when limit reached. Fixed: return `more=true` when `remaining <= 0`. Only affects multi-shard clusters. swingshift-15.
- [x] **Wire reader panic on malformed responses** — `Reader.ReadBytes` and 8 similar RelativeOffset-based methods (ReadVectorInt32, ReadVectorUint64, ReadNestedReader, etc.) checked `off < 4` but not `off + 4 > len(r.object)`. Crafted vtable offsets caused out-of-bounds slice panic. Fixed: add upper bounds check to all 9 methods. Found by fuzzing `FuzzParseGetKeyValuesReply`. swingshift-15.
- [x] **OOM amplification from crafted wire count fields** — `ParseKeyValueRefStringVector` and `ReadVectorCount` used raw `uint32` count from wire data in `make([]T, 0, count)`. Crafted `count=0xFFFFFFFF` → 206GB allocation → OOM. Fixed: cap allocation to physical buffer bounds (`bufferSize / minElementSize`). Protects all 37 generated vector parsers + hand-written KV parser. swingshift-15.

- [x] **Connection pool same-port aliasing broke multi-node clusters** — `getOrDialConn` reused an existing TCP connection for any address with the same port number, regardless of IP. In multi-node clusters (3 processes on 10.0.1.10-12:4500), the coordinator connection was returned for GRV proxy and commit proxy requests, sending frames to the wrong process where the endpoint token didn't match → silent drop → GRV timeout. C++ FlowTransport creates one Peer per unique NetworkAddress with no aliasing. Fixed: removed same-port matching and coordinator dial fallback entirely. PR #61.

_Binding tester: 200+ seeds × 1000 ops = 0 failures. 78 C binding port tests pass (96% of C test suite)._

### Features

#### HIGH

- [ ] **C++ ConnectionID dedup** — C++ FlowTransport deduplicates bidirectional connections via ConnectionID exchange in ConnectPacket. When two processes connect to each other simultaneously, the lower-priority connection is dropped. Not needed as a pure client (we never accept incoming connections), but should be implemented if we ever add server-side functionality.

- [x] **`proxyTagThrottledDuration` send path** — Investigated: C++ `CommitProxyInterface.h:318` comments "Not serialized, because this field does not need to be sent to master." The field is reply-only (proxy→client), accumulated correctly in Go. No send path needed. Resolved dayshift-10.

#### LOW

- [ ] **Tenant groups** — Metacluster-only. `tenantGroupTenantIndex`, `tenantGroupMap` (IncludeVersion), group cleanup on delete. Not needed for standalone clusters.
- [ ] **Tenant tombstones** — Metacluster data cluster feature. Prevents tenant ID reuse across metacluster deletions. Not applicable to standalone.
- [ ] **Tenant ID prefix** — Multi-cluster ID partitioning. `tenantIdPrefix` shifts prefix into upper 2 bytes of 8-byte ID. Standalone clusters use prefix=0.
- [ ] **Multi-version client** — Plugin loading for older client protocol versions.
- [ ] **FDB status JSON parsing** — Cluster status monitoring via `\xff\xff/status/json`.
- [ ] **Version vector support** — Causal consistency optimization for multi-region deployments.

### Performance

#### MEDIUM

- [x] **RYW SnapshotCache** — Sorted interval map caches server reads for reuse within a transaction. Repeated getRange/get calls hit cache instead of server. nightshift-12. 22 tests.
- [x] **Pool read conflict buffers** — `addReadConflictForKey`/`addReadConflict` used `make()` per call. Now use shared `conflictBuf` via extracted `conflictBufAlloc` helper (same pool as write conflicts). SaveRecord 101→97, LoadRecord 84→81, DeleteRecord 94→91 allocs. swingshift-18.
- [x] **Pool frame read buffers** — Won't-fix. `ReadFrame` allocates `make([]byte, payloadLen)` per response. Consumers hold slices into the buffer (zero-copy design), so pooling requires copying every slice back out, negating the pool benefit. Investigated dayshift-6c, confirmed dayshift-20.
- [x] **Speculative second request** — All three read paths (sendGetValue, sendGetKey, sendGetRange) now hedge: send to best, timer max(10ms, 2×latency), send to second-best, race. swingshift-11. Primitives in `hedge.go`, QueueModel extensions in `loadbalance.go`.
- [x] **Outbound PING connection monitor** — connectionMonitor goroutine sends PingRequest every 750ms when connection has pending requests but no bytes received. Kills connection after 2s timeout. Matches C++ FlowTransport connectionMonitor(). Implemented dayshift-10.

#### LOW

- [ ] **`net.Buffers` (writev)** — Scatter-gather I/O for frame writes. Low impact now that write coalescing works.
- [ ] **LRU eviction for location cache** — Currently random eviction. Works well enough at 600K entries.
- [ ] **Pre-allocate prefixed keys** — Commit path tenant prefix allocation. Not on read hot path.

### Tests

#### MEDIUM

- [x] **Multi-shard integration tests** — 6 tests across 35-51 shards (dayshift-10): GetRange, GetRangeReverse, paged GetRange, GetKey selector resolution, AtomicAdd, GetEstimatedRangeSize. Uses `WithProcessCount(3)` + `WithKnob("max_shard_bytes", "50000")` + 1MB data + 60s poll for splits.
- [x] **Multi-shard watch survival** — 4 tests: basic, multi-shard concurrent, heavy-write load, cross-shard ClearRange. All across 51 shards. swingshift-11.
- [x] **Multi-shard concurrent writes during DD** — 8 goroutines × 25 ops write large values across 51 shards. Point read + scan cross-check verifies no data loss. nightshift-12.
- [x] **RYW adversarial tests** — 44 tests exercising all 12 atomic mutation types through RYW path, comparing client-side resolution against committed FDB values. Chained atomics, ClearRange+Get, getRange with local writes/atomics (forward+reverse). 0 divergences. swingshift-15.
- [x] **RYW fuzz expanded** — FuzzRYWCache now covers all 12 atomic types (was only Add). 3.7M executions in 60s, 0 failures. swingshift-15.
- [x] **Directory partition tests** — 4 tests for directoryPartition.go (was 0% coverage). Create, child dirs, namespace isolation, data read/write, removal, panic behavior. swingshift-15.
- [x] **Retry/OnError adversarial tests** — Self-conflicting on commit_unknown_result, all 16 retryable error codes, resource-constrained backoff, intersectConflictRanges edge cases. swingshift-15.
- [x] **Concurrent stress tests** — 5 tests: concurrent RYW reads, read-modify-write counter (20 goroutines × 5), concurrent AtomicAdd (20 × 10), parallel range+write, Clear vs Get race. swingshift-15.
- [x] **Full race detector verification** — All 5 test targets clean with `--@rules_go//go/config:race` after fixing Watch/postCommitReset race. swingshift-15.
- [x] **C++ completeness audit (5 subsystems)** — readpath, commitpath, transaction, RYW, GRV+database. All pass. No missing code paths, error codes correct, wire protocol correct, atomic implementations match C++ Atomic.h. swingshift-15.

#### HIGH (client test gaps from C++ audit, swingshift-11)

- [x] **Tenant isolation tests** — Already covered in `fdb/tenant_test.go`: TestTenantCRUD (CRUD lifecycle) + TestTenantIsolation (cross-tenant key invisibility, shared key name different values, range scoping).
- [x] **Watch edge cases** — 3 tests: timeout via context deadline, atomic mutation triggers watch, cancellation. swingshift-11d.
- [x] **Snapshot read isolation (extensive)** — 5 tests: GetAfterClear, GetRangeAfterClearRange, GetRangeDoesNotConflict, GetAfterAtomicAdd, ConflictAsymmetry. swingshift-11d. Still TODO: fuzz target.
- [x] **Transaction retry with RYW** — 4 tests: OnError resets RYW, new read version after OnError, conflict detection across retry, Transact automatic retry. swingshift-11d. Still TODO: fuzz target.
- [x] **Watch + atomic mutations** — TestWatchFiresOnAtomicMutation verifies AtomicAdd triggers watch. swingshift-11d.

### Behavioral Divergences from C++ (audit 2026-04-13, updated swingshift-18)

| # | Area | Type | Description |
|---|---|---|---|
| 1 | ~~`future_version` backoff~~ | ~~BEHAVIOR~~ FIXED | ~~C++ uses `min(FUTURE_VERSION_RETRY_DELAY, maxBackoff)`.~~ Fixed: Go now respects user's `maxRetryDelay`. |
| 2 | ~~`makeSelfConflicting`~~ | ~~BEHAVIOR~~ FIXED | ~~Go used `writeConflicts[0].Begin` in `commitDummyTransaction`.~~ Fixed: `intersectConflictRanges()` matches C++ `intersects()` — picks a key from the overlap of write+read conflict ranges. Falls back to `writes[0].Begin` if no intersection. |
| 3 | ~~Watch cancellation on Reset~~ | ~~MISSING~~ FIXED | ~~C++ cancels pending watches on reset.~~ Fixed: `cancelWatches()` on Reset/Cancel/reset via lazy `watchCtx`. |
| 4 | ~~GRV cache ratekeeper per-priority~~ | ~~BEHAVIOR~~ FIXED | ~~C++ checks per-priority.~~ Fixed: BATCH checks `lastRkBatch`, DEFAULT checks `lastRkDefault`. |
| 5 | ~~RYW SnapshotCache~~ | ~~BEHAVIOR~~ FIXED | ~~C++ caches server reads for reuse within a transaction.~~ Fixed: `snapshotCache` sorted interval map in `ryw_snapshot_cache.go`. nightshift-12. |
| 6 | Auto-reset after commit | DESIGN | C++ no auto-reset at API >= 410. Go `postCommitReset()` clears for reuse. |
| 7 | `getRange` RYW merge | DESIGN | C++ segment-tree `RYWIterator`. Go iterative fetch+merge loop. Functionally equivalent. |
| 8 | QueueModel key | COSMETIC | C++ `endpoint.token.first()`. Go address string. Same identity. |
| 9 | ~~Load balance secondDelay~~ | ~~PERF~~ FIXED | ~~C++ speculative second request. Go sequential.~~ Fixed: `sendFrameWithHedge()` in `hedge.go`. All 3 read paths hedge. swingshift-11. |
| 10 | `FLAG_FIRST_IN_BATCH` | COSMETIC | Not exposed. No behavioral gap. |
| 11 | ~~`reset()` stale flags~~ | ~~BEHAVIOR~~ FIXED | ~~`userSetReadVersion` and `nextWriteNoConflict` not cleared by `reset()`. C++ creates fresh state.~~ Fixed: both cleared in `reset()`. swingshift-18. |
| 12 | ~~`tryCache` SYSTEM_IMMEDIATE~~ | ~~MAINTENANCE~~ FIXED | ~~Dead code fell through to DEFAULT throttle check.~~ Fixed: explicit rejection. swingshift-18. |
| 13 | ~~`commitDummyTransaction` no Set mutation~~ | ~~COSMETIC~~ FIXED | ~~Go only adds conflict ranges.~~ Fixed: now calls `Set(key, "")` matching C++. swingshift-18. |
| 14 | ~~`commitDummyTransaction` fixed backoff~~ | ~~PERF~~ FIXED | ~~Go uses fixed 10ms.~~ Fixed: exponential backoff (10ms → 2x → cap 1s) matching C++ `onError`. swingshift-18. |
| 15 | ~~`commitDummyTransaction` no `CAUSAL_WRITE_RISKY`~~ | ~~PERF~~ FIXED | ~~Go doesn't set CAUSAL_WRITE_RISKY on dummy.~~ Fixed: `causalReadRisky = true` for faster GRV. swingshift-18. |
| 16 | Topology polling vs push | DESIGN | C++ `monitorProxies` long-polls coordinator (push, ~0ms latency). Go polls at 5s steady-state with 200ms rapid bursts on failure. Adequate because proxy changes are rare and failed RPCs trigger immediate kicks. |
| 17 | ~~Location cache over-invalidation~~ | ~~CONSERVATIVE~~ FIXED | ~~Go invalidates entire remaining scan range.~~ Fixed: now invalidates just `[shardBegin, shardEnd)` matching C++ `cx->invalidateCache(locations[shard].range)`. swingshift-18. |
| 18 | Wrong-shard retry cap | CONSERVATIVE | Go caps at `MaxWrongShardRetries=50`. C++ loops unbounded (relies on 5s tx timeout). Go returns error earlier under extreme shard movement. |
| 19 | GRV background refresh | PERF | Go refreshes at fixed 50ms. C++ uses adaptive delay `(grvDelay + latency)/2` (1ms-100ms range). Go is more aggressive (2x more RPCs under low load). |
| 20 | ~~Server selection~~ | ~~PERF/SCALE~~ FIXED | ~~Go selects deterministic min-metric. C++ uses randomized best-of-two.~~ Fixed: "power of two random choices" — pick 2 random candidates, select lower metric. Matches C++ `LOAD_BALANCE_USE_BEST_OF_TWO_RANDOM`. dayshift-20. |
| 21 | Frame checksum | COSMETIC | Go uses XXH3-64. C++ uses CRC32. Both valid, same security properties. |

### Missing C API Surface (audit 2026-04-13)

All data-path functions implemented. Missing are observability/admin only:

| C Function | Category | Assessment |
|---|---|---|
| `fdb_transaction_get_mapped_range` | Niche | Server-side index join. Record Layer doesn't use it. |
| ~~`fdb_transaction_get_tag_throttled_duration`~~ | ~~Observability~~ | ~~Returns cumulative tag-throttle delay.~~ Implemented as `GetTagThrottledDuration()` (dayshift-10). |
| `fdb_transaction_get_total_cost` | Observability | Estimated transaction cost for rate limiting. |
| `fdb_database_force_recovery_with_data_loss` | Admin | DR operation. |
| `fdb_database_create_snapshot` | Admin | Disk-level backup. |
| `fdb_database_get_main_thread_busyness` | N/A | Go has no network thread. |
| `fdb_database_get_server_protocol` | Niche | Multi-version client coordination. |

---

## Record Layer (`pkg/recordlayer/`)

### Bugs

- [x] **AutoContinuingCursor transaction_timed_out not retried** — Error 1031 escaped as non-retryable, killing large scans when FDB's 5-second timeout hit mid-page. Fixed: `isRetryableForContinuation()` treats 1031 as retryable in cursor context (creates new transaction from saved continuation). Java has the same gap. swingshift-18.

- [x] **ensureStoreStateLoaded error swallowing (partial)** — Errors now captured in `stateLoadErr` and propagated from 5 error-returning callers (validateRecordUpdateAllowed, updateSecondaryIndexes, 3 batch methods). 7 no-error methods (GetIndexState, GetUserVersion, etc.) still use fallback — changing their signatures is a breaking API change. nightshift-21.

- [x] **DeleteRecordsWhere leaked index clears to non-target types** — `findMatchingRecordTypes()` only checked PK column count, not type key value. Customer-only indexes were incorrectly cleared when deleting Orders. Fixed: filter by type key VALUE when PKs have RecordTypeKey prefix. swingshift-23.

- [x] **computeIndexDeletePrefix uses arbitrary sample PK** — now uses first matching type from `matchingTypeNames` instead of arbitrary map iteration. Fallback to any type preserved for edge cases. swingshift-23.

### Features

#### OUT OF SCOPE (query planner prerequisites)

These features are only used by the query planner / SQL layer, not by core CRUD:

- [ ] **Synthetic record types** — `JoinedRecordType` (equi-join), `UnnestedRecordType` (repeated message fan-out). Proto fields 12-13 in MetaData. 11+ Java classes. Large feature.
- [ ] **Views** — `PView` in MetaData proto (field 15). SQL layer concept.
- [ ] **User-defined functions** — `PUserDefinedFunction` in MetaData proto (field 14). SQL layer concept.
- [ ] **AggregateCursor** — Accumulator-based aggregation over cursor results.
- [ ] **ComparatorCursor** — Custom comparator ordering.
- [ ] **UnorderedUnionCursor** — Union without order preservation.
- [ ] **MapPipelinedCursor** — Async pipelined map (no Go equivalent of CompletableFuture).
- [ ] **ProbableIntersectionCursor** — Bloom filter intersection.
- [ ] **SizeStatisticsGroupingCursor** — Key/value size tracking.
- [ ] **RecordCursorVisitor pattern** — Cursor tree inspection.

#### MEDIUM

- [x] **MetaDataEvolutionValidator gaps vs Java** — All Java checks implemented. (1) Index record type scope validation. (2) Type rename map. (3) Full IndexValidatorRegistry (TEXT, RANK, PERMUTED, VECTOR, MULTIDIMENSIONAL + base). (4) Former index addedVersion edge case (when old index doesn't exist, validate addedVersion > old metadata version). 27 tests total.

#### LOW

- [x] **`isClosed()` on cursor** — `IsClosed() bool` added to `RecordCursor[T]` interface. All 38 cursor types implement it. swingshift-23.
- [ ] **FDBReverseDirectoryCache** — Reverse prefix→name caching (~496 lines Java).
- [ ] **KeySpace/KeySpacePath** — Phase 1 done (core types, path nav, reverse resolution, range queries, 11 tests in `pkg/recordlayer/keyspace/`). Phase 2: LocatableResolver + ScopedDirectoryLayer. Phase 3: FDBReverseDirectoryCache. See `docs/design-keyspace.md`. swingshift-23.
- [ ] **Extension options processing** — Advanced FDBMetaDataStore feature for proto extension options.
- [ ] **Schema validation cross-language** — Needs Java conformance server additions for cross-language error comparison.

### Performance

_Go wins 5/8 benchmarks vs Java Record Layer. LoadRecord 0.61x, ScanRecords 0.73x, StoreOpen 0.85x._

#### MEDIUM

- [ ] **Pool proto messages in deserializeAndDiscover** — `rt.newMessage()` allocates via reflection per record (77.5MB / 564K allocs in BenchmarkScanRecords, ~9%). BUT: messages escape to user code via `FDBStoredRecord.Record`, so pooling isn't safe without API changes (copy-on-return or explicit release). Only viable if scan API returns copies or if users opt-in. Low priority given the constraint.
- [x] **Pre-allocate tuple in fastDecodeTuple** — Pre-allocate with `make(tuple.Tuple, 0, len(b)/5)`. BenchmarkScanIndex: 815 → 715 allocs/op (-12.3%). nightshift-16.

### Tests

_Comprehensive: 2307 Ginkgo + 429 conformance + 50 chaos + 7 fuzz targets._

No open test items.

---

## Infrastructure / CI

- [x] **Hetzner Object Storage upload `continue-on-error`** — Added nightshift-9. Outages no longer block PR merges.
- [x] **Benchmark CI with PR comparison** — RFC 018. `cmd/bench-report` Go tool + `just bench-ci` recipe + CI workflow. Master pushes upload baseline to S3, PRs get benchstat-style comparison posted as PR comment (edit-in-place via marker comment). nightshift-12.
- [x] **CI test cache invalidation fix** — bench-ci step used `bazelisk test` with different flags, overwriting test action cache. Fixed: bench recipes use `bazelisk run` (build + execute directly). Test step: 50s → 4.7s on cached runs. dayshift-14.
- [x] **Bench-report false positive reduction** — Raised threshold from 5% to 10%. Only flags timing regressions when allocs/bytes also changed (timing-only deltas = VM noise). dayshift-14.
- [x] **FDBMetaDataStore conformance test** — 3 specs: Go→Java, Java→Go, history cross-language. Uses non-tenant mode with unique subspace prefixes. dayshift-14.
- [x] **govulncheck in CI** — `govulncheck ./...` step after build/test (informational, continue-on-error). Current findings: 2 vulns in github.com/docker/docker (testcontainers transitive dep, no fix available). `just vulncheck` for local use. swingshift-18.
- [x] **Multi-node cluster test** — 3-container FDB cluster regression test (172.16.1.{2,3,4}:4500) with Go client CRUD. Verifies connection pool correctness for multi-node clusters. swingshift-18.
- [x] **Binding stress testcontainers migration** — Replaced raw Docker CLI calls with testcontainers module. Eliminates manual polling, 3s sleeps, and fragile container lifecycle. swingshift-18.
- [x] **Binding stress cluster file path fix** — Testcontainers migration (swingshift-18) used relative cluster file path, but Python binding tester runs with `cmd.Dir=/tmp/bt-run`. Relative path resolved wrong → error 1515 on all seeds. Fixed: `filepath.Abs()`. nightshift-19.
- [ ] **Throughput benchmarks fail on single-node testcontainer** — `BenchmarkThroughputInsertBatchConcurrent128` overwhelms the FDB testcontainer (128 goroutines × concurrent transactions). Two issues: (1) GRV cache staleness causes "record store does not exist" on first goroutines after setup; fix: `InvalidateGRVCache()` after store creation. (2) FDB 5-second transaction timeout under load causes "context deadline exceeded". Fix: either skip in `just bench` or use a larger cluster. `just bench-ci` excludes throughput benchmarks and works fine.
- [x] **CI Node.js 20 deprecation** — Updated nightshift-21: checkout v4→v5, upload-artifact v4→v7 across all 4 workflows. All actions now Node.js 24 compatible. GitHub deadline: 2026-06-02.
- [x] **Evaluate nilaway nogo linter** — Evaluated nightshift-21. 4 findings, all false positives (nil slice `[0:]`, map iteration over own keys). Core library clean. Already run `nilness` from x/tools. Not adding — poor signal/noise ratio.

---

## Relational / SQL Layer (`pkg/relational/`)

**Started nightshift-24 (2026-04-18).** Port of Java's `fdb-relational-*` modules. Goal: full SQL over FoundationDB, wire-compatible with Java.

### Status quo — Phase 0-2 quality assessment (dayshift-34, 2026-04-19)

**What we have that's solid**

- 13,238 lines of relational test code, 34 test files, ~200 integration tests hitting real FDB
- 1587/1587 real SQL statements from Java's yamsql corpus parse cleanly — strong signal the grammar is the same
- Parser is literally the Java grammar vendored verbatim (`RelationalLexer.g4` + `RelationalParser.g4`) — no translation risk
- 22 files in `pkg/relational/` actually import `pkg/recordlayer` — real wiring, not a parallel universe
- `metadata` wraps `recordlayer.RecordMetaData`, `catalog` is FDB-backed via `recordlayer.FDBRecordStore`, `embedded.execInsert/Update/Delete` call `SaveRecord`/`DeleteRecord` directly. INSERT round-trips a dynamic protobuf through the actual record-layer store.
- 52 `// matching Java` / `// ported from Java` markers in the code trace the lineage
- Catalog subspace layout matches Java's `SystemTableRegistry`

**What's weak / fragile**

1. **Zero cross-language conformance tests for SQL.** `conformance/` has Java↔Go round-trips for record-layer operations (18 conformance files), but nothing for the SQL layer. We have no proof that `SELECT ... FROM t` returns byte-identical rows from Go and Java. We only test Go → Go.
2. **The yamsql corpus test only verifies that statements *parse*** — nothing verifies that they execute identically. Parsing is the easy half.
3. **NULL semantics were silently broken in 7 places until dayshift-34.** `UPPER(NULL)` returned `''`, `NULL > 5` returned true, `NULL + 5` errored. Caught only because a dedicated NULL test was written. Raises the question: what else is silently wrong that we haven't stumbled into?
4. **No SQL-level fuzz testing.** Record-layer parsers have 24 fuzz targets; SQL has zero.
5. **`connection.go` is 5498 lines.** Half the execution engine in one file. Not a correctness issue but a review/maintenance red flag.
6. **Hand-rolled plans, not Cascades.** Execution paths (proto scan, CTE, JOIN) have three diverging evaluators that were near-duplicates until the scalar function cores were unified dayshift-34. Anything else that diverges is latent inconsistency.
7. **No cascading index scan planning.** We always do table scan + filter. Java uses indexes. Performance gap is unknown but probably large.

**Concrete Java-alignment gaps worth testing before trusting**

- [ ] Feed the yamsql **execution** corpus (not just parse) through both engines and diff result sets — "SQL semantic equivalence" below is the unchecked item.
- [ ] Run Go's `INSERT INTO t ...` then read it back via Java's JDBC connector. Run the reverse. Both unchecked.
- [ ] Plan cache key stability (Go hash == Java hash) is unchecked. Doesn't block correctness but blocks shared RPC caching.

**Wiring with core FDB layer — this part is solid**

- `recordlayer.RecordMetaData` / `recordlayer.FDBRecordStore` are the only paths to FDB. No shadow storage, no mocks.
- Dynamic proto messages built at `CREATE TABLE` via `dynamicpb` go through the same `SaveRecord` path as static proto records — same wire format, same split handling, same index maintainers.
- Catalog uses `recordlayer.NewStoreBuilder()` like any other consumer.
- Same FDB transaction model (`db.Run`, `ctx.Transact`), same conflict resolution.

**Bottom line:** the integration with core FDB is real and correct. The Java behavioral equivalence is asserted by construction (same grammar, same wire format, ported code patterns) but not *verified* end-to-end. For something we want bidirectional Java interop on, that's the biggest gap to close — and it's straightforward to do with a Java JDBC round-trip harness in `conformance/`.

### Next-shift priority list — 5-agent QA audit (dayshift-34)

Parallel audits across conformance / Go style / testing / correctness / architecture surfaced these. Ordered roughly by impact.

**HIGH — correctness / Java conformance bugs**

- [x] **`NOT` of UNKNOWN returns TRUE.** `evalExprPredicate` NotExpression does `!v` on a bool that already collapsed NULL→false, so `NOT (x = NULL)` → TRUE. Same pattern in `NOT LIKE NULL`, `NOT BETWEEN NULL`. ✅ swingshift-35: introduced `triBool` Kleene type; all predicate evaluators now have Tri variants that preserve UNKNOWN. Bool wrappers collapse at filter boundary.
- [x] **Div/0 divergence between evaluator paths.** ✅ swingshift-35: unified on SQL-standard error. `applyArithmeticOp` now delegates to `applyMathOp` — one canonical implementation.
- [x] **`%` operator missing in proto path.** ✅ swingshift-35: added to `applyMathOp`.
- [x] **`COUNT(col)` counts NULLs.** ✅ swingshift-35: increment moved inside non-null check; `COUNT(*)` still counts every row (special-cased by `aggArg == ""`).
- [x] **`SUM`/`AVG` of empty-or-all-NULL group returns 0, not NULL.** ✅ swingshift-35: use `counts[i] > 0` as "non-null seen" gate; synth empty-set row added for ungrouped queries.
- [x] **`SUM` silently treats string columns as 0.** ✅ swingshift-35: `toFloat64` result now checked; non-numeric → `ErrCodeInvalidParameter`.
- [x] **Mixed-type equality via stringification.** ✅ swingshift-35: `valuesEqual` + `compareValues` reject cross-type comparisons; only numeric↔numeric coercion preserved.
- [x] **Catalog subspace incompatible with Java.** ✅ swingshift-35: `DefaultCatalogSubspace()` now packs tuple `(NULL, NULL, int64(0))` = `0x000014` matching Java's `KeySpaceDirectory(SYS, NULL) → (SYS, NULL) → (CATALOG, LONG, 0L)`. Byte-prefix pinned in `TestDefaultCatalogSubspaceBytes`.

**MEDIUM**

- [x] `CAST(NULL AS <type>)` ✅ swingshift-35: early-return `nil, nil` in `castValue`.
- [x] `ABS(math.MinInt64)` overflow ✅ swingshift-35: return `ErrCodeInvalidParameter` rather than wrap.
- [x] `LEFT` / `RIGHT` / `SUBSTRING` float length arg ✅ swingshift-35: `toIntegerArg` helper accepts int64 and whole-valued float64, rejects fractional/non-numeric.
- [x] `ResetSession` leaks ✅ swingshift-35: now rolls back activeTx, clears ctes + schemaCache, restores defaultSchema.
- [ ] Nested SELECT / derived-table write to the same `ctes` map without a scope stack — outer sees leaked inner CTE names.
- [x] **Production `fmt.Errorf` sites dropping `ErrorCode`** ✅ swingshift-35: ~30 sites across metadata/catalog/embedded/keyspace/ddl swept to `api.NewErrorf`/`api.WrapErrorf`. Added `api.WrapErrorf` helper for the `%w`-wrapping idiom.
- [ ] **8 panics in `api/datatype_*.go`.** Requires `DataType` interface signature change (WithNullable/Resolve). Punted — full interface-breaking refactor, next shift.
- [ ] `MetaDataEvolutionValidator` exists in `recordlayer` but is **not wired** into `SaveSchemaTemplate` — concurrent Java+Go writes can silently create incompatible schema template evolutions.
- [ ] **DISTINCT SUM never accumulates** (surfaced by swingshift-35 reviewer). `aggregateMapRows` / proto path increment `counts[i]` inside the DISTINCT branch but never add to `sums[i]`. Removing the `|| aggDistinct` guard now correctly emits NULL for all-NULL DISTINCT groups, but SUM(DISTINCT col) on a non-empty group also emits NULL — broken. Needs its own distinct-value-sum pass.

**Architecture / design**

- [ ] **Split `connection.go`** (5498 lines, 120 functions) into ~12 files (`exec_select.go`, `exec_join.go`, `exec_dml.go`, `exec_ddl.go`, `exec_sys.go`, `select_parts.go`, `aggregate.go`, `eval_expr.go`, `eval_predicate.go`, `functions.go`, …). Mechanical, no behavioral risk.
- [ ] **Break up `evalScalarFunctionCallCore`** (576-line switch). Split by family (`evalStringFns`, `evalMathFns`, `evalDateFns`, `evalCastFn`) via `map[string]funcImpl` dispatch.
- [ ] **Add a `Planner` / `Plan` seam** before Phase 4. `execSelect*` walks the ANTLR parse tree directly; when Cascades lands, there's nowhere to plug in. Define `type Planner interface { Plan(parseTree) (Plan, error) }` + `type Plan interface { Execute(ctx, Transaction) (ResultSet, error) }` and ship a one-impl `NaivePlanner` wrapping today's code.
- [ ] **Fix `api.Transaction` substitutability.** `store_catalog.checkOpenTxn` and `fdb_transaction.unwrapFDB` do concrete-type assertions, defeating the interface. Add `Unwrap() any` (matches Java's `unwrap<T>`) or a capability interface — otherwise a future remote/gRPC backend is rejected at runtime.
- [ ] Typed enums for `joinType` / `aggFunc` (currently magic strings).

**Testing gaps (highest ROI item first)**

- [ ] **Java↔Go SQL conformance harness.** `conformance/sql/` directory with `.sql` + `.json` expected-output files; drive both Go (`sql.Open("fdbsql", ...)`) and Java (`EmbeddedRelationalConnection` via the Bazel-built conformance server) against the same inputs; diff result sets. Seed with the existing yamsql corpus (1587 statements already parse — just execute them and diff). Also run write-in-Go / read-in-Java round-trips (and reverse) to catch wire-format drift — would have caught the catalog subspace bug above. Opt-in target (`just conformance-sql`), gated behind `@manual` to stay out of default `bazelisk test //...`.
- [ ] **Zero fuzz targets in `pkg/relational/`** (record-layer has 24). Add `FuzzParse(sql)`, `FuzzEvalExpr(tree)`, `FuzzContinuationToken`, `FuzzSchemaTemplateProto`.
- [ ] **Error-path coverage is ~0.2%** (2 error assertions vs 862 success in `embedded_fdb_test.go`). Add tests for type mismatch on INSERT, NOT NULL violation, missing schema, invalid SQL at execute time, duplicate CREATE DATABASE, PK conflict.

### Core requirements

1. **1:1 aligned with Java.** Package names, class/struct names, behavior, wire format — mirror Java unless there is a very good reason. Catalog storage, plan cache keys, protobuf encodings, SQL dialect must be bit-compatible.
2. **Usable from `database/sql`.** Primary public entry is a `database/sql/driver.Driver` registered under name `fdbsql`. Users write `sql.Open("fdbsql", dsn)`. Non-negotiable.
3. **Embedded first.** Start with in-process execution (equivalent to Java's `EmbeddedRelationalConnection`). gRPC remote / standalone server comes later.
4. **Keep the parser dialect identical.** Use the same `RelationalLexer.g4` / `RelationalParser.g4` grammar files; regenerate with `antlr4-go/antlr4`. No dialect drift.

### Scope map (Java → Go)

| Java module | Go package | Role |
|---|---|---|
| `fdb-relational-api` | `pkg/relational/api` | Interfaces, options, error codes, type system (`DataType`), metadata types (`Table`, `Column`, `Index`, `Schema`, `SchemaTemplate`), struct/array helpers |
| `fdb-record-layer-core/query/plan/cascades` | `pkg/recordlayer/plan/cascades` | **Cascades optimizer.** Expressions, Values, Predicates, Rules, Matching, Typing, Memo/References, Cost model. ~104K LOC in Java — by far the largest item. |
| `fdb-record-layer-core/query/plan/plans` | `pkg/recordlayer/plan/plans` | Physical plan nodes (`RecordQueryPlan` subclasses). Some overlap with what we already have in `pkg/recordlayer/`. |
| `fdb-relational-core/antlr/*.g4` | `pkg/relational/core/parser` | ANTLR4 lexer/parser (same `.g4` files, regenerated for Go) |
| `fdb-relational-core/recordlayer/query` | `pkg/relational/core/query` | `SemanticAnalyzer`, `PlanGenerator`, `LogicalOperator`, `QueryExecutor` |
| `fdb-relational-core/recordlayer/query/cache` | `pkg/relational/core/cache` | `RelationalPlanCache` (3-tier, TTL) |
| `fdb-relational-core/recordlayer/catalog` | `pkg/relational/core/catalog` | `RecordLayerStoreCatalog`, system tables, schema versioning |
| `fdb-relational-core/recordlayer/metadata` | `pkg/relational/core/metadata` | `RecordLayerSchemaTemplate`, `RecordLayerTable`, `RecordLayerIndex`, `RecordLayerColumn` |
| `fdb-relational-core/recordlayer/ddl` | `pkg/relational/core/ddl` | `ConstantAction` pattern for CREATE/DROP/ALTER |
| `fdb-relational-core/recordlayer/structuredsql` | `pkg/relational/core/structuredsql` | Fluent SQL AST (lower priority) |
| `fdb-relational-core/recordlayer` (conn/stmt/resultset impls) | `pkg/relational/core/embedded` | `EmbeddedConnection`, `EmbeddedStatement`, `RecordLayerResultSet` |
| `fdb-relational-jdbc` | `pkg/relational/sqldriver` | `database/sql/driver.Driver` adapter (embedded mode, and later gRPC client) |
| `fdb-relational-grpc` | `pkg/relational/grpc` *(later)* | gRPC service stubs + protobuf wire |
| `fdb-relational-server` | `cmd/frl-server` *(later)* | Standalone SQL server binary |
| `fdb-relational-cli` | `cmd/frl` *(later)* | Interactive SQL shell |

### Architectural decisions

**Why a `database/sql/driver` adapter instead of building natively against `database/sql`:**

Java's API is JDBC-extending (`RelationalConnection extends java.sql.Connection`). Strict 1:1 means we keep an internal Go API that mirrors Java's method surface — then wrap it with a thin `database/sql/driver` adapter. Users get both: `sql.Open("fdbsql", ...)` for portability + direct access to the Go-native API via type assertion or a package-level `Open()` for FDB-specific features (options, struct/array types, continuations, fluent SQL).

**Why the cascades planner lives in `pkg/recordlayer/plan/cascades`, not `pkg/relational/core`:**

Matches Java's layout. Cascades is a planning framework over `RecordQuery`, reusable by anyone writing queries against the record layer — not intrinsic to SQL. The SQL layer *consumes* it.

**DSN format:**

```
fdbsql:///PATH                             # embedded, default cluster file
fdbsql:///PATH?cluster_file=/etc/.../fdb.cluster
fdbsql://HOST:PORT/PATH                    # remote gRPC (later)
```

Matches JDBC URL shape. Path semantics match Java's `RelationalConnection.getPath()`.

**Transaction model:**

- `sql.DB` auto-commit → each statement is its own FDB transaction (matches Java `autoCommit=true` default).
- `sql.DB.BeginTx()` → explicit `FDBRecordContext` for the lifetime of the `sql.Tx`.
- Isolation level `sql.LevelSerializable` only (FDB semantics). Lower levels return `driver.ErrBadConn` or equivalent — do not silently downgrade.
- `context.Context` propagation is mandatory (5 s FDB transaction limit; users must get `context.DeadlineExceeded` back).

**Type mapping (`driver.Value`):**

| SQL type | Go `driver.Value` | Notes |
|---|---|---|
| BOOLEAN | `bool` | |
| INTEGER / BIGINT | `int64` | Java widens to int64 same way |
| FLOAT / DOUBLE | `float64` | |
| STRING / VARCHAR | `string` | UTF-8 |
| BYTES | `[]byte` | |
| TIMESTAMP | `time.Time` | Map to Java's tuple encoding |
| UUID | `[16]byte` / `uuid.UUID` | TBD — match Java SQL UUID |
| STRUCT | custom type | Implement `driver.Valuer` and `sql.Scanner`; expose `pkg/relational/api.Struct` |
| ARRAY | custom type | Same |
| NULL | `nil` | |

Versionstamps and continuations require custom types that users access via type assertion on `*sql.Rows` or a `pkg/relational` helper.

### Phases

Phases are ordered by **dependency**, not priority. Phase 0–3 are the minimum viable SQL engine (CRUD against pre-existing stores via hand-written plans). Phase 4 is where the hard work is. Everything downstream of Phase 4 is straightforward.

#### Phase 0 — Skeleton & foundations

- [x] **pkg/relational/api foundations** (nightshift-24):
  - [x] `ErrorCode` — all 70 SQLSTATE codes from Java's enum, `Error` struct (code + message + cause + context), `errors.As` matching, `WithContext` immutable
  - [x] `DataType` hierarchy — full port: `BooleanType`/`IntegerType`/`LongType`/`FloatType`/`DoubleType`/`StringType`/`BytesType`/`VersionType`/`UUIDType`/`NullType`/`UnknownType`/`VectorType`/`ArrayType`/`EnumType`/`StructType`/`UnresolvedType`; JDBC type-code mapping; singleton primitives
  - [x] `Options` — 30-name map with parent chaining, immutable With/Builder, defaults mirroring Java's static block
  - [x] `KeySet`, `Continuation` (+ Reason enum), `Row` (+ RowIterable)
  - [x] `Metadata` base + `Visitor` + `Column`/`Table`/`Index`/`View`/`InvokedRoutine`/`SchemaTemplate`/`Schema` interfaces
- [x] **pkg/relational/api Driver / Connection / Statement / ResultSet** (nightshift-24) — lean Go-idiomatic shape; ctx on every call; typed errors; WasNull + Continuation + ByName accessors; ColumnNullable constants pinned to JDBC values.
- [x] **pkg/relational/api remaining interfaces** (nightshift-24) — `DatabaseMetaData`, `Array`, `Struct`, `ArrayMetaData`, `StructMetaData`, `DirectAccessStatement`, `ParseTreeInfo`, `WithMetadata`. All ported as lean Go-idiomatic shapes.
- [x] **pkg/relational/api SqlTypeNamesSupport** (nightshift-24) — name ↔ JDBC code ↔ DataType mappings used by parser + ResultSetMetaData.
- [ ] **pkg/relational/api/fluentsql** — (deferred; shell only until after Phase 7)
- [x] **Interop with existing `pkg/recordlayer` types — decided** (nightshift-24): follow Java's layering.
  - `pkg/recordlayer.RecordMetaData` = storage-engine schema (proto + indexes). Unchanged.
  - `pkg/recordlayer.Index` = storage-engine index definition (root expression, subspace key, options). Unchanged.
  - `pkg/relational/api.SchemaTemplate` / `api.Index` = interface-level metadata surface used by SQL machinery.
  - Bridge impls (coming in Phase 2): `pkg/relational/core/metadata.RecordLayerSchemaTemplate` and `RecordLayerIndex` satisfy the `api.*` interfaces by wrapping `recordlayer.RecordMetaData` / `recordlayer.Index`. No circular dependencies — `recordlayer` is oblivious to the relational types.
  - Matches Java's `fdb-relational-core.recordlayer.RecordLayerSchemaTemplate` wrapping `fdb-record-layer-core.RecordMetaData` 1:1.
- [ ] **Proto definitions** — copy `fdb-relational-*` proto files from Java source into `proto/apple/relational/` (`record_layer_context.proto`, catalog messages, etc.). Regenerate via `just generate`.
- [x] **pkg/relational/sqldriver skeleton** (nightshift-24) — `sql.Register("fdbsql", …)`, DSN parser (embedded + remote shapes), `Driver`/`Connector` satisfying `driver.Driver`/`driver.DriverContext`/`driver.Connector`. Connect returns `ErrCodeUnsupportedOperation` (plumbing ready; embedded impl arrives in Phase 5).

#### Phase 1 — Parser (ANTLR4)

- [x] **Vendor the grammar** (nightshift-24) — `RelationalLexer.g4` + `RelationalParser.g4` copied verbatim to `pkg/relational/core/parser/grammar/`. Package skeleton + regen instructions in `pkg/relational/core/parser/doc.go`.
- [x] **Integrate antlr4-go** (dayshift-25) — `github.com/antlr4-go/antlr/v4@v4.13.1` pinned in `go.mod`, `use_repo` entry in `MODULE.bazel`. `just generate-parser` downloads the ANTLR 4.13.2 tool jar, runs lexer then parser with `-lib` to resolve `tokenVocab`, outputs to `pkg/relational/core/parser/gen/`. One-line patch to the lexer grammar (removed the Java-action-only `notifyListeners` call) with a NOTE comment for future re-sync.
- [x] **Port QueryParser wrapper** (dayshift-25) — `parser.Parse(sql string) (IRootContext, error)` with collecting `ErrorListener` that turns every ANTLR syntax error into one "line:col: msg" line of an `*api.Error` with `ErrCodeSyntaxError`. `caseInsensitiveCharStream` wraps `antlr.InputStream` and upper-cases `LA()` while preserving original source for `GetText()`. 9 unit-test functions (happy paths across DDL/DML/transactions, mixed-case, single errors, line:col formatting, stray-char rejection, multi-error ordering, case-folding, EOF passthrough).
- [x] **Parser corpus smoke test** (dayshift-25) — `just smoke-yamsql` walks the 178 `.yamsql` files in `fdb-record-layer/yaml-tests/src/test/resources/`, extracts every schema-template / query pair, skips yamsql-harness macros (`!! ... !!`), sentinels (`SHOULD ERROR`), and `error:`-marked expected-fail entries. **1587 / 1587 real statements parse cleanly.** Gated by the `yamsql` build tag so Bazel's sandbox doesn't need to see the Java submodule.
- [ ] **Parser tree-shape conformance tests** — stretch goal. Feed the same SQL corpus through both parsers and diff the trees (or pick representative corners). Requires a JSON serialiser on both sides. Not a blocker for Phase 2 — semantic analyzer tests will catch tree-shape regressions indirectly.

#### Phase 2 — Type system + metadata storage

- [x] Port `DataType` — done in Phase 0 (nightshift-24)
- [x] Port `SchemaTemplate` / `Schema` / `Table` / `Column` / `Index` interfaces — done in Phase 0 (nightshift-24)
- [x] **Concrete `SchemaTemplate` / `Table` / `Column` / `Index` structs** (dayshift-25) — `pkg/relational/core/metadata/` wraps `*recordlayer.RecordMetaData`. `NewRecordLayerSchemaTemplate` / `NewRecordLayerSchemaTemplateWithVersion` materialise tables + flat index-name list eagerly. Proto-to-DataType mapping mirrors Java's `fromProtoType` (including UUID short-circuit and `NullableArrayTypeUtils.describesWrappedArray` unwrap, proto2-label-based nullability). `Accept()` cascades through tables → indexes → columns → routines → views, matching Java's `RecordLayerSchemaTemplate.accept()`. `api.Schema` grew delegated `Tables`/`Views`/`Indexes`/`InvokedRoutines` methods (Go has no default methods). `IntermingleTables()` and `IsSparse()` (via predicate != nil) both match Java. No known Java divergences on the primary path.
- [x] **Builder for SchemaTemplate** (nightshift-28) — `pkg/relational/core/metadata.Builder` builds `RecordLayerSchemaTemplate` from SQL-level table/column/PK definitions without a pre-compiled .proto file. Builds `FileDescriptorProto` dynamically (no union descriptor — sidesteps global proto registry for dynamically-created types). Wired into `CREATE SCHEMA TEMPLATE` SQL via `EmbeddedConnection.execCreateSchemaTemplate`.
- [x] **VALUE index support in schema template builder** (nightshift-29) — `Builder.AddIndex()` + `buildIndexKeyExpression()`; `execCreateSchemaTemplate` handles `IndexOnSourceDefinition` clauses (two-pass: tables first, indexes second). `CREATE INDEX name ON table (cols)` wired end-to-end.
- [x] **EmbeddedConnection optional driver interfaces** (nightshift-29) — `ConnBeginTx`, `SessionResetter`, `Validator`, `ConnPrepareContext`, `QueryerContext`; static checks for all. `embeddedStmt.Query` now delegates to `QueryContext`.
- [x] **QueryContext with SHOW DATABASES + SHOW SCHEMA TEMPLATES** (nightshift-29) — `execShowDatabases`/`execShowSchemaTemplates` backed by catalog `ListDatabases`/`ListTemplates`. `staticRows` + `emptyRows` driver.Rows impls. FDB integration tests for both.
- [x] **Catalog storage layer (interface + in-memory impl)** (swingshift-26) — `api.StoreCatalog` + `api.SchemaTemplateCatalog` + `api.Transaction` interfaces ported from Java; `InMemoryStoreCatalog` + `InMemorySchemaTemplateCatalog` + `InMemoryTransaction` in `pkg/relational/core/catalog/`. 5 Java-compliance fixes applied during self-audit + review (SaveSchema template-existence check, error-code disambiguation, LoadSchema UNDEFINED_SCHEMA collapse, CatalogValidator port, RepairSchema TOCTOU doc). 17 tests + 4 benchmarks. `CatalogDatabaseMetaData` JDBC-style introspection backed by StoreCatalog (Schemas / Tables / Columns / IndexInfo / PrimaryKeys, SQL LIKE patterns, JDBC column + sort orders). Gomock convention adopted in api/; `just generate` runs all codegen (proto + mocks + gazelle), CI diff-checks.
- [x] **Catalog storage layer (FDB-backed)** (nightshift-27) — `RecordLayerStoreCatalog` + `RecordLayerStoreSchemaTemplateCatalog` + `FDBTransaction` in `pkg/relational/core/catalog/`. Mirrors Java's SystemTableRegistry subspace layout. Full CRUD + listing + RepairSchema + DeleteDatabase. 17 FDB integration tests + 3 DeleteDatabase tests.
- [x] **INSERT INTO ... VALUES execution** (nightshift-29) — `execInsert`: literal-only VALUES (decimal, string, null, bool), `dynamicpb`-backed dynamic messages, schema loaded from catalog, record saved to FDB store. UnionDescriptor auto-generated in schema template builder; `metadata.Build()` falls back to `dynamicpb.NewMessage` for types not in global proto registry. `?schema=` DSN option wires `SetSchema`. 3 FDB integration tests.
- [x] **SELECT * FROM table execution** (nightshift-29) — `execSelect`: navigates ANTLR SELECT parse tree to extract table name, calls `ScanRecordsByType`, buffers rows, converts proto fields to `driver.Value` via `protoValueToDriver`. `defaultSchema` field in `EmbeddedConnection` so `ResetSession` restores DSN-provided schema instead of clearing it. 1 FDB integration test (insert 2 rows + SELECT * verifies 2 rows returned).
- [x] **DELETE FROM table [WHERE col = value] execution** (nightshift-29) — `execDelete`: scan+filter+DeleteRecord. `evalPredicate` handles simple `col = constant` equality. `evalConstant` factored out and shared with INSERT. 1 FDB integration test.
- [x] **UPDATE table SET col = val [WHERE col = val] execution** (nightshift-29) — `execUpdate`: scan+filter+clone+set+SaveRecord. `evalExpr` for SET expressions. Full CRUD now implemented. 1 FDB integration test. — `execDelete`: scan+filter+DeleteRecord. `evalPredicate` handles simple `col = constant` equality. `evalConstant` factored out and shared with INSERT. `valuesEqual` normalises int64/float64. 1 FDB integration test. — `execSelect`: navigates ANTLR SELECT parse tree to extract table name, calls `ScanRecordsByType`, buffers rows, converts proto fields to `driver.Value` via `protoValueToDriver`. `defaultSchema` field in `EmbeddedConnection` so `ResetSession` restores DSN-provided schema instead of clearing it. 1 FDB integration test (insert 2 rows + SELECT * verifies 2 rows returned).
- [x] **System tables** — `INFORMATION_SCHEMA.SCHEMATA`, `TABLES`, `COLUMNS`, `INDEXES` implemented (nightshift-30). Computed on-the-fly from catalog state via `execSysSchemata`, `execSysTables`, `execSysColumns`. Queries require double-quoted identifiers (`"INFORMATION_SCHEMA"."TABLES"`) due to ANTLR grammar keyword limitations. 3 FDB integration tests.
- [x] **SELECT COUNT(*) aggregate** (nightshift-30) — `checkCountStar` detects the aggregate in SELECT list; `execSelect` scans + counts matching rows; returns single-row result with column `COUNT(*)`. Works with WHERE. 1 FDB integration test (count all, count with WHERE).
- [x] **Compound WHERE (AND/OR/NOT + range comparisons)** (nightshift-30) — `evalExprPredicate` recursive dispatcher handles `LogicalExpressionContext` (AND/OR with short-circuit), `NotExpressionContext`, and `PredicatedExpressionContext`. `evalComparisonPredicate` handles `=`, `!=`, `<>`, `<`, `>`, `<=`, `>=`. 3 FDB integration tests (AND, OR, range).
- [x] **ORDER BY + LIMIT in SELECT** (nightshift-30) — post-scan in-memory sort via `sort.SliceStable`; `compareValues` handles int64/float64/string/bool with NULL-sorts-last. `LIMIT n` truncates after sort. `extractSelectParts` refactored to return `*selectQuery` struct. 3 FDB integration tests (ASC, DESC, LIMIT).
- [x] **SELECT DISTINCT** (nightshift-31) — `simpleTable.DISTINCT()` detection in `extractSelectParts`; `rowKey()` string-serializes rows for dedup; deduplicated before ORDER BY + LIMIT. 1 FDB integration test (4 rows → 2 distinct values).
- [x] **WHERE col IN (val1, val2, ...) / NOT IN** (nightshift-31) — `evalInPredicate` handles InPredicateContext from Predicate() slot on PredicatedExpressionContext; evaluates each constant, short-circuits on first match; NOT IN negates. 2 FDB integration tests. **Known limitation**: `NULL NOT IN (...)` returns true (should be NULL/unknown per SQL standard — fixing requires 3-valued logic propagation).
- [x] **WHERE col IS [NOT] NULL / IS TRUE / IS FALSE** (nightshift-31) — `evalIsNullPredicate` handles IsExpressionContext; uses `ProtoReflect().Has()` for proto2 optional presence (unset = NULL). 1 FDB integration test (IS NULL + IS NOT NULL).
- [x] **WHERE LIKE / NOT LIKE** (nightshift-31) — `evalLikePredicate` + `likeMatchRunes` recursive % / _ pattern matching. `stripStringLiteralQuotes` for SQL string literal unescaping. 2 FDB integration tests (LIKE + NOT LIKE).
- [x] **WHERE BETWEEN / NOT BETWEEN** (nightshift-31) — `evalBetweenPredicate` inclusive range via `compareValues`. 2 FDB integration tests (BETWEEN + NOT BETWEEN).
- [x] **Schema evolution validator** — `RelationalSchemaEvolutionValidator` in `pkg/relational/core/ddl/`. Validates: no table removal, no column removal, no type changes, no column reordering; additions allowed. Wired into `SaveSchemaTemplateConstantAction.Execute()`. 6 unit tests. dayshift-32.
- [x] **GROUP BY + aggregate functions** — `SELECT col, COUNT(*)/SUM/MIN/MAX/AVG FROM t GROUP BY col`; HAVING clause; ORDER BY on aggregates; bare aggregates without GROUP BY. 4 FDB integration tests. dayshift-32.
- [x] **Scalar expressions in SELECT** — `SELECT id, amount * 2 AS doubled FROM t`; arithmetic / column references in projection via evalExpr. 1 FDB integration test. dayshift-32.
- [x] **Catalog read conflict fix** — cachedLoadSchema reads catalog via separate auto-commit tx when inside explicit user transaction; prevents spurious FDB 1020 not_committed errors under parallel DDL. dayshift-32.
- [x] **Arithmetic in UPDATE SET** — `evalExpr` extended with `MathExpressionAtomContext` + `FullColumnNameExpressionAtomContext`; `SET col = col + N` now works. 1 FDB integration test. dayshift-32.
- [x] **GROUP BY + aggregate functions** — `SELECT col, COUNT(*)/SUM/MIN/MAX/AVG FROM t GROUP BY col`; in-memory grouping; mixed group-col + aggregate SELECT lists. 1 FDB integration test. dayshift-32.
- [x] **LIMIT OFFSET** — `LIMIT n OFFSET m` via grammar GetLimit()/GetOffset(); applied post-sort/group. 1 FDB integration test. dayshift-32.
- [x] **CASE WHEN THEN END** — searched CASE (conditions via evalExprPredicate) and simple CASE (compareValues). ELSE optional. 1 FDB integration test. dayshift-32.
- [x] **String functions** — UPPER, LOWER, LENGTH/LEN, TRIM, ABS; nested calls chain. dayshift-32.
- [x] **CONCAT, CONCAT_WS, NULLIF** — CONCAT(s1,s2,...), CONCAT_WS(sep,...), NULLIF(a,b). 1 FDB integration test. dayshift-32.
- [x] **Generalized WHERE comparisons** — evalComparisonPredicate uses evalExprAtom on both sides; functions/arithmetic now allowed in WHERE (e.g., WHERE price * 2 > 50). 1 FDB integration test. dayshift-32.
- [x] **INFORMATION_SCHEMA WHERE filtering** — filterSysRows helper reuses evalHaving on col→value map; applies to SCHEMATA, TABLES, COLUMNS, INDEXES. 1 FDB integration test. swingshift-33.
- [x] **UNION ALL / UNION DISTINCT** — execQueryBodyRows + execUnion handle recursive UNION trees. execSelectQuery/execSelectQueryFull refactor splits routing from FDB scan. 2 FDB integration tests. swingshift-33.
- [x] **INSERT INTO ... SELECT** — execInsertSelect evaluates QueryExpressionBody (incl. UNION), maps source→target columns via convertToProtoValue. 1 FDB integration test. swingshift-33.
- [x] **CAST(expr AS type)** — DataTypeFunctionCallContext in evalSpecificFunction; castValue helper for BIGINT/INTEGER/FLOAT/DOUBLE/STRING/BOOLEAN. swingshift-33.
- [x] **SUBSTRING/SUBSTR, REPLACE, IF/IIF** — string/conditional functions; BinaryComparisonPredicateContext now handled in evalExprAtom (comparisons as values). 1 integration test. swingshift-33.
- [x] **FLOOR/CEIL/CEILING/ROUND/MOD/POWER/POW/SIGN** — math functions. 1 integration test. swingshift-33.
- [x] **compound HAVING (AND/OR/NOT)** — logical operators in HAVING clause via evalHaving recursion. swingshift-33.
- [x] **INNER JOIN and LEFT OUTER JOIN** — execSelectJoin: nested-loop join, ON condition via evalHaving on merged map, SELECT * across both tables, ORDER BY/LIMIT. Detects LEFT/RIGHT grammar ambiguity (keywords are in keywordsCanBeId). 2 integration tests. swingshift-33.
- [x] **RIGHT OUTER JOIN** — correct unmatched-right-row detection via per-row matchedRight[] boolean slice. 1 integration test. swingshift-33.
- [x] **JOIN + GROUP BY / aggregates** — GROUP BY with COUNT/SUM/MIN/MAX/AVG, COUNT(DISTINCT), HAVING all work in JOIN queries (map-based in-memory grouping). 1 integration test. swingshift-33.
- [x] **COUNT(DISTINCT col)** — distinct-set tracking per group (map[string]struct{}); works with and without GROUP BY. 1 integration test. swingshift-33.
- [x] **GREATEST/LEAST** — multi-argument GREATEST(a,b,c)/LEAST(a,b,c) scalar functions; NULL-argument skipping. 1 integration test. swingshift-33.
- [x] **filterSysRows compound WHERE** — now routes through evalPredicateOnMapExpr so AND/OR/NOT/IS NULL/LIKE/IN/BETWEEN all work in INFORMATION_SCHEMA WHERE clauses. swingshift-33.
- [x] **Subquery IN / NOT IN** — `WHERE col IN (SELECT ...)` / `WHERE col NOT IN (SELECT ...)`; proto path + map/JOIN path both supported; ctx+conn threaded through evalPredicate/evalExprPredicate/evalInPredicate. dayshift-34.
- [x] **EXISTS / NOT EXISTS subquery** — `WHERE EXISTS (SELECT ...)` / `WHERE NOT EXISTS (SELECT ...)`; ExistsExpressionAtomContext handled at expression level. dayshift-34.
- [x] **CTE (WITH clause)** — `WITH name AS (SELECT ...) SELECT ...`; CTEs materialized in order at execSelect start; chaining (CTE B references CTE A) works. dayshift-34.
- [x] **SELECT without FROM** — `SELECT 1+2, 'hello'`; constant expression row, no catalog access. dayshift-34.
- [x] **INSERT VALUES with expressions** — `INSERT INTO t VALUES (1+2, UPPER('foo'))`; evalExpr replaces evalLiteralExpr for INSERT value columns. dayshift-34.
- [x] **Derived tables (subquery in FROM)** — `SELECT name FROM (SELECT id, name FROM t WHERE ...) AS alias`; materialised into temporary CTE slot. dayshift-34.
- [x] **Scalar functions in map eval** — evalExprAtomOnMap now handles FunctionCallExpressionAtomContext via evalScalarFunctionCallOnMap; all scalar functions work in JOIN ON/WHERE, CTE WHERE/SELECT, derived table filters. CTE projection evaluates projExprs via evalExprOnMap. NULL NOT IN map path fixed (was returning true). EXISTS added to evalHaving. dayshift-34.
- [x] **CASE WHEN + CAST in map eval** — evalSpecificFunctionOnMap mirrors evalSpecificFunction for CTE/JOIN/derived-table contexts. dayshift-34.
- [x] **ctx+conn threading through evalExpr stack** — evalExpr/evalExprAtom/evalScalarFunctionCall/evalSpecificFunction/predicate helpers all take ctx+conn as first params. Enables subqueries inside CASE conditions and scalar function args. Removes three context.TODO() placeholders. dayshift-34.
- [x] **Aggregates on CTEs + derived tables** — aggregateMapRows method extracted from execSelectJoin and reused in execSelectFromCTE. Also fixes latent bug: JOIN+GROUP BY+ORDER BY+LIMIT previously returned early, silently ignoring ORDER BY/LIMIT. dayshift-34.
- [x] **Unify proto + map evaluators** — evalScalarFunctionCallCore + evalSpecificFunctionCore hold the full ~350-line dispatch, parameterised on an exprEvaluator adapter (and a predicateEvaluator for CASE WHEN). The four public functions are now thin wrappers. New scalar / CASE functions only need to be added once. dayshift-34.
- [x] **Multi-table FROM (implicit cross join)** — `SELECT a.x, b.y FROM a, b WHERE a.id = b.id`. Extra comma-separated sources become INNER joinClause entries with no ON condition; WHERE provides the predicate. 1 integration test. dayshift-34.
- [x] **JOIN on CTE** — confirmed working: `SELECT ... FROM T INNER JOIN cte ON T.id = cte.id` uses scanTableToMaps CTE shortcut. 1 integration test. dayshift-34.
- [x] **ORDER BY expression (CTE / JOIN paths)** — `ORDER BY UPPER(name)`, `ORDER BY a + b`, etc. Parser stores the expression when it's not a plain column/aggregate; sort sites pre-compute expression keys from map rows and sort via indexes. The proto / single-table-scan path returns a clear error since msgs aren't retained past projection. 1 integration test. dayshift-34.

#### Phase 3 — Semantic analysis (parse tree → logical plan)

- [ ] **Port `LogicalOperator` hierarchy** — SELECT, INSERT, UPDATE, DELETE, CTE, UNION, etc. Match Java names.
- [ ] **Port `SemanticAnalyzer`** — ANTLR visitor that walks parse tree, resolves identifiers against catalog, infers types, produces `LogicalOperator` tree. Also extracts prepared-statement parameters.
- [ ] **Error surfacing** — column-not-found, type-mismatch, ambiguous-ref, etc. Match Java `ErrorCode`s.

#### Phase 4 — Cascades optimizer (the big one)

**This is ~104K LOC in Java, ~500 files. It will not fit in one shift. Break it into sub-phases and plan across many shifts.**

- [ ] **4.0 — Foundation types**
  - [ ] `Type` / `TypeRepository` / `Typed` — type inference + constraint propagation
  - [ ] `Value` hierarchy — `AbstractValue`, `FieldValue`, `ConstantValue`, `ArithmeticValue`, `CastValue`, `BooleanValue`, `AggregateValue`, ~77 value classes
  - [ ] `QueryPredicate` hierarchy — `ComparisonPredicate`, `AndPredicate`, `OrPredicate`, `NotPredicate`, `ComparisonRange(s)`, `MatchesValue`
  - [ ] `Simplification` — value simplification, predicate simplification (~30 classes)
  - [ ] `Comparisons` / `Comparison` — `Comparisons.Type`, `Comparisons.Comparison`, `Comparisons.SimpleComparison`, etc.
- [ ] **4.1 — Relational expressions**
  - [ ] `RelationalExpression`, `RelationalExpressionWithChildren`, `RelationalExpressionWithPredicates`
  - [ ] Logical exprs: `LogicalFilterExpression`, `LogicalProjectionExpression`, `LogicalSortExpression`, `LogicalTypeFilterExpression`, `LogicalUnionExpression`, `LogicalDistinctExpression`, `LogicalIntersectionExpression`, `SelectExpression`
  - [ ] DML exprs: `InsertExpression`, `UpdateExpression`, `DeleteExpression`, `TableFunctionExpression`
- [ ] **4.2 — Matching engine**
  - [ ] `BindingMatcher` DSL — structural pattern + constraints
  - [ ] `graph/` matchers, `structure/` matchers
  - [ ] `PlannerBindings`
- [ ] **4.3 — Memo & references**
  - [ ] `Reference` (= Cascades "group") — equivalence class of `RelationalExpression`s
  - [ ] Implicit DAG via `Reference` pointers (no explicit memo)
  - [ ] `PlanContext`, `CascadesRuleCall`
- [ ] **4.4 — Cost model**
  - [ ] `CascadesCostModel` — heuristic comparator matching Java
  - [ ] Cardinality estimation hooks, `properties/` package (~25 classes)
- [ ] **4.5 — Rules**
  - [ ] Rule base classes (`CascadesRule`, `CascadesRuleCall`)
  - [ ] Data access rules (`AbstractDataAccessRule`, `AggregateDataAccessRule`, `PrimaryScanRule`, index scan rules)
  - [ ] Implementation rules (`ImplementFilterRule`, `ImplementSortRule`, `ImplementDistinctRule`, `ImplementNestedLoopJoinRule`, `ImplementRecursiveDfsJoinRule`, `ImplementStreamingAggregationRule`, etc.)
  - [ ] Decomposition rules (`InComparisonToExplodeRule`, `DecorrelateValuesRule`)
  - [ ] Optimization rules (`MergeFetchIntoCoveringIndexRule`, `PushPredicateThroughDistinctRule`, `MergeFetchIntoTypeFilterRule`, etc.)
  - [ ] Finalization rules (`FinalizeExpressionsRule`)
  - [ ] **~69 rules total.** Port in batches, pick representative tests from Java's rule test suite.
- [ ] **4.6 — Planner driver**
  - [ ] `CascadesPlanner` — task stack, EXPLORE phase → OPTIMIZE phase
  - [ ] `PlannerEvent` debug hooks
  - [ ] Integration with `RecordMetaData` + index availability
- [ ] **4.7 — Correctness tests**
  - [ ] Port enough of Java's planner test suite to validate rule-by-rule equivalence
  - [ ] Add a **plan equivalence harness**: run same SQL through Go and Java planners in a container, diff the plans.

#### Phase 5 — Query execution

- [ ] **`PlanGenerator`** — `LogicalOperator → RelationalExpression` adapter
- [ ] **`QueryExecutor`** — executes a `RecordQueryPlan` against a `FDBRecordStore`, returns `RecordCursor`
- [ ] **`RecordLayerResultSet`** — wraps cursor, implements `api.ResultSet`
- [ ] **Continuation support** — cursor continuation → SQL-level cursor state; match Java encoding
- [ ] **Prepared parameter binding** — `PreparedParams` substitutes `?` at evaluation time

#### Phase 6 — DDL

- [x] **`ConstantAction`** base + executor (nightshift-27)
- [x] **`MetadataOperationsFactory`** + `RecordLayerMetadataOperationsFactory` (nightshift-27) — full wiring: FDB store create/delete via `RelationalKeyspace`; CreateDatabase/DropDatabase/CreateSchema/DropSchema/SaveSchemaTemplate/DropSchemaTemplate; 12 unit tests + 3 FDB integration tests
- [x] **`EmbeddedConnection` DDL execution** (nightshift-28) — SQL DDL (CREATE/DROP DATABASE/SCHEMA) parsed via ANTLR, dispatched to factory, executed in FDB auto-commit transactions; wired into `fdbsql` driver; 8 unit tests + 4 FDB integration tests
- [ ] Individual actions: `CreateTableAction`, `CreateIndexAction`, `DropTableAction`, `DropIndexAction`, `SetStoreStateAction`, etc.
- [ ] Integration with online indexer (CREATE INDEX triggers background build)

#### Phase 7 — Plan cache

- [ ] Port `RelationalPlanCache` — 3-tier (primary/secondary/tertiary) with per-tier TTL + max-entries
- [ ] `QueryCacheKey` — SQL + param types + catalog version
- [ ] `PhysicalPlanEquivalence` — deduplicates semantically identical plans
- [ ] Async eviction

#### Phase 8 — `database/sql/driver` adapter (`pkg/relational/sqldriver`)

- [x] **`Driver`** — registered as `fdbsql`, parses DSN, constructs embedded `Connection` (nightshift-28)
- [x] **`Connector`** — lazy-init, holds cluster client + options (nightshift-28)
- [x] **`Conn`** `driver.Conn` (Prepare/Close/Begin), `driver.ExecerContext`, `driver.Pinger` — nightshift-28. `driver.ConnBeginTx`, `driver.ConnPrepareContext`, `driver.SessionResetter`, `driver.Validator` deferred (phase 8 complete path)
- [ ] **`Stmt`** implementing `driver.Stmt`, `driver.StmtExecContext`, `driver.StmtQueryContext`, `driver.NamedValueChecker`
- [ ] **`Rows`** implementing `driver.Rows`, `driver.RowsColumnTypeDatabaseTypeName`, `driver.RowsColumnTypeNullable`, `driver.RowsColumnTypeLength`, `driver.RowsColumnTypePrecisionScale`, `driver.RowsColumnTypeScanType`
- [ ] **`Result`** implementing `driver.Result` (LastInsertId is always an error — FDB has no auto-inc; match Postgres driver convention)
- [ ] **`Tx`** implementing `driver.Tx`
- [ ] **Value conversion** — `driver.Value` ⇄ `api.DataType` values, including structs and arrays
- [ ] **Custom scanner/valuer** — `Struct`, `Array`, `Versionstamp`, `Continuation`
- [ ] **Integration test matrix**
  - [x] `sql.Open("fdbsql", dsn)` + `db.Ping()` (nightshift-28)
  - [x] `db.ExecContext` for DDL (CREATE DATABASE/SCHEMA/SCHEMA TEMPLATE + DROP) (nightshift-28)
  - [x] `db.QueryContext` + `rows.Scan` for SELECT (nightshift-29: SELECT * FROM table via ScanRecordsByType)
  - [x] `db.PrepareContext` + parameterized exec/query (nightshift-30) — `substituteParams()` replaces `?` positional placeholders before parsing. String escape (`''`→`'`) handled in both directions. 11 unit tests + 2 FDB integration tests (basic, apostrophe round-trip).
  - [ ] `db.BeginTx` + Commit/Rollback
  - [ ] Context cancellation mid-query
  - [ ] Concurrent connections from shared `sql.DB`

#### Phase 9 — gRPC server + remote driver *(later)*

- [ ] Port `fdb-relational-grpc/` protobuf definitions
- [ ] `cmd/frl-server` — standalone server binary, TLS, auth
- [ ] Remote `sqldriver` path: DSN host:port → gRPC client

#### Phase 10 — CLI *(later)*

- [ ] `cmd/frl` SQL shell — history, EXPLAIN, formatted output. Use `liner` or `go-prompt`.

### Java compatibility conformance (continuous)

- [ ] **Catalog wire format** — extract a schema via Go, load with Java, run a SELECT. Round-trip.
- [ ] **Plan cache key stability** — Java cache key hash = Go cache key hash (for RPC caching).
- [ ] **System table contents** — `SELECT * FROM INFORMATION_SCHEMA.TABLES` returns byte-identical rows from Go and Java against the same store.
- [ ] **SQL semantic equivalence** — feed the yamsql test corpus through both engines; require identical result sets for read queries.
- [ ] **FRL perf comparison — Go vs Java** — we have a Go-vs-Java benchmark table for the record layer (see CLAUDE.md), but nothing yet for the relational / SQL layer. Once Phase 5 (embedded Connection) + Phase 3 (semantic analyzer) land enough to run a real SELECT, stand up the same comparison harness for common SQL workloads (simple SELECT, secondary-index SELECT, INSERT, aggregate, prepared statement with parameters). Drive both via the same `java-jdbc-connector` vs `database/sql` test rig; measure latency, allocs, throughput. Goal: parity or better, same posture as the record-layer numbers.

### Non-goals (explicit)

- UDFs (`PUserDefinedFunction`) — out of scope until planner is done
- Views (except trivial `SELECT *`-over-base-table) — deferred
- Synthetic record types (`JoinedRecordType`, `UnnestedRecordType`) — deferred
- Java SQL function catalog / semantic analyzer rules that depend on it — simplify or defer
- Callable statements, holdable/scrollable result sets, savepoints — Java throws `SQLFeatureNotSupportedException`; we do the same
- LOB types (`Blob`, `Clob`, `NClob`, `SQLXML`) — same, unsupported

### Risks & open questions

1. **Cascades port scope is enormous.** 104K LOC Java → probably 80K+ Go after de-Java-isms. Many shifts; needs sub-RFCs for each rule family. Alternative considered and **rejected**: hand-rolled heuristic planner would break Java plan-cache-key compatibility and mean divergent optimizer behavior forever.
2. **ANTLR-go performance.** Java's ANTLR runtime is well-tuned; antlr4-go/antlr4 is less mature. Parse-hot-path benchmarking required before Phase 1 sign-off.
3. **Go generics vs. Java wildcards — decide before Phase 4.0.** Cascades is heavily generic (`Value<T>`, `RelationalExpression<T>`, `BindingMatcher<? extends T>`). The two candidate shapes for the Go port:
   - (a) Interface hierarchies with `any` / explicit type assertions (matches how our current record layer handles index expressions). Lower compile-time safety, smaller API surface.
   - (b) Generic structs + constraint interfaces. Higher safety, but Go generics do not have wildcard bounds — `Matcher[? extends Value]` becomes awkward. Requires rewriting the matcher DSL.
   - **Decision:** go with (a) initially. Revisit in Phase 4.5 (rules) if the lack of compile-time type safety causes correctness bugs. Documenting here so Phase 4.0 foundation types don't drift.
4. **`database/sql` impedance mismatch.** `driver.Value` is a closed set (bool/int64/float64/string/[]byte/time.Time/nil). Struct/array/enum/versionstamp need custom `Scanner`/`Valuer` types; users must opt in explicitly. Document in a `pkg/relational/sqldriver` package doc comment.
5. **Catalog migration.** If we get the catalog wire format wrong once, users' production data needs migration. Write conformance tests for catalog read-back **before** writing the catalog writer.
6. **Testing the planner.** No FDB call-site validates plan quality end-to-end beyond correctness. Need yamsql runner + an `EXPLAIN` diff harness against Java.
7. **ANTLR grammar license.** Java's `RelationalLexer.g4` / `RelationalParser.g4` are MIT-licensed (original Positive Technologies MySQL grammar) with Apple copyright addition (Apache 2.0). Vendoring them into Go needs a `LICENSE` note in `pkg/relational/core/parser/grammar/`; both licenses are permissive and compatible.
