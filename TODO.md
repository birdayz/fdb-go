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

_Binding tester: 200+ seeds × 1000 ops = 0 failures. 78 C binding port tests pass (96% of C test suite)._

### Features

#### HIGH

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
- [ ] **Pool frame read buffers** — `ReadFrame` allocates `make([]byte, payloadLen)` per response. Blocked by zero-copy design (consumers hold slices into buffer). Would need refactored deserialization.
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

### Behavioral Divergences from C++ (audit 2026-04-13)

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

_No known bugs. 2696 Ginkgo specs + 430 conformance specs + 50 chaos tests pass._

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

#### LOW

- [ ] **`isClosed()` on cursor** — Closure state check.
- [ ] **FDBReverseDirectoryCache** — Reverse prefix→name caching (~496 lines Java).
- [ ] **KeySpace/KeySpacePath** — Enterprise key management wrapper on top of FDB directory layer.
- [ ] **Extension options processing** — Advanced FDBMetaDataStore feature for proto extension options.
- [ ] **Schema validation cross-language** — Needs Java conformance server additions for cross-language error comparison.

### Performance

_Go wins 5/8 benchmarks vs Java Record Layer. LoadRecord 0.61x, ScanRecords 0.73x, StoreOpen 0.85x._

#### MEDIUM

- [ ] **Pool proto messages in deserializeAndDiscover** — `rt.newMessage()` allocates a new proto message via reflection on every record scan (77.5MB / 564K allocs in BenchmarkScanRecords). vtprotobuf's `ResetVT()` + `sync.Pool` per record type could eliminate ~9% of scan allocations. Requires careful lifetime management — callers must not hold references to pooled messages.
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
