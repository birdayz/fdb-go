# fdb-record-layer-go TODO

Restructured 2026-04-13 (nightshift-9). Previous version: `git show 036697a:TODO.md`.
Correctness audit performed 2026-04-13 against C++ NativeAPI.actor.cpp.

Java Record Layer version: **4.10.6.0**. FDB wire protocol: **7.3.75**.

---

## Pure Go FDB Client (`pkg/fdbgo/`)

### Bugs

- [x] **getKey selector resolution across shard boundaries** — Go sent ONE request and returned the reply key, ignoring `orEqual` and `offset` fields from the `KeySelector` in the reply. C++ loops until `offset==0 && orEqual==true`. In multi-shard clusters, selectors crossing shard boundaries returned wrong keys. Fixed: full resolution loop matching C++. Not caught by tests (single-shard testcontainers).
- [x] **hot_shard/range_locked backoff cap** — Go used `DEFAULT_MAX_BACKOFF` (1s) for `transaction_throttled_hot_shard` (1235) and `transaction_rejected_range_locked` (1242). C++ uses `RESOURCE_CONSTRAINED_MAX_BACKOFF` (30s). Caused over-aggressive retry under hot-shard conditions. Fixed: moved to resource-constrained group.

_No known open bugs. Binding tester: 200+ seeds × 1000 ops = 0 failures. 78 C binding port tests pass (96% of C test suite)._

### Features

#### HIGH

- [ ] **`proxyTagThrottledDuration` send path** — Accumulated per-transaction but not sent back to proxy in GRV request metadata. C++ sends it so the proxy can adjust throttle decisions. Not a correctness issue — throttle still works via standard backoff — but reduces throttle tuning accuracy.

#### LOW

- [ ] **Tenant groups** — Metacluster-only. `tenantGroupTenantIndex`, `tenantGroupMap` (IncludeVersion), group cleanup on delete. Not needed for standalone clusters.
- [ ] **Tenant tombstones** — Metacluster data cluster feature. Prevents tenant ID reuse across metacluster deletions. Not applicable to standalone.
- [ ] **Tenant ID prefix** — Multi-cluster ID partitioning. `tenantIdPrefix` shifts prefix into upper 2 bytes of 8-byte ID. Standalone clusters use prefix=0.
- [ ] **Multi-version client** — Plugin loading for older client protocol versions.
- [ ] **FDB status JSON parsing** — Cluster status monitoring via `\xff\xff/status/json`.
- [ ] **Version vector support** — Causal consistency optimization for multi-region deployments.

### Performance

#### MEDIUM

- [ ] **Pool frame read buffers** — `ReadFrame` allocates `make([]byte, payloadLen)` per response. Blocked by zero-copy design (consumers hold slices into buffer). Would need refactored deserialization.
- [ ] **Speculative second request (secondDelay)** — C++ sends a hedge request to a second storage server after ~0.5ms delay to improve p99 latency. Currently we try servers sequentially.
- [ ] **Outbound PING connection monitor** — C++ sends periodic PINGs to detect dead connections in ~2s. We rely on TCP keepalive (10s) + next-RPC detection (~5s). Phase 2 optimization.

#### LOW

- [ ] **`net.Buffers` (writev)** — Scatter-gather I/O for frame writes. Low impact now that write coalescing works.
- [ ] **LRU eviction for location cache** — Currently random eviction. Works well enough at 600K entries.
- [ ] **Pre-allocate prefixed keys** — Commit path tenant prefix allocation. Not on read hot path.

### Tests

#### MEDIUM

- [ ] **Multi-shard GetRange integration test** — Needs multi-node testcontainer support. Single-node = single shard, can't verify cross-shard continuation.

### Behavioral Divergences from C++ (audit 2026-04-13)

| # | Area | Type | Description |
|---|---|---|---|
| 1 | ~~`future_version` backoff~~ | ~~BEHAVIOR~~ FIXED | ~~C++ uses `min(FUTURE_VERSION_RETRY_DELAY, maxBackoff)`.~~ Fixed: Go now respects user's `maxRetryDelay`. |
| 2 | `makeSelfConflicting` | BEHAVIOR | C++ adds random key to `\xFF/SC/` when write/read conflicts don't intersect. Go uses `writeConflicts[0].Begin` in `commitDummyTransaction`. Functionally equivalent for common cases. |
| 3 | ~~Watch cancellation on Reset~~ | ~~MISSING~~ FIXED | ~~C++ cancels pending watches on reset.~~ Fixed: `cancelWatches()` on Reset/Cancel/reset via lazy `watchCtx`. |
| 4 | ~~GRV cache ratekeeper per-priority~~ | ~~BEHAVIOR~~ FIXED | ~~C++ checks per-priority.~~ Fixed: BATCH checks `lastRkBatch`, DEFAULT checks `lastRkDefault`. |
| 5 | RYW SnapshotCache | BEHAVIOR | C++ caches server reads for reuse within a transaction. Go re-fetches on every `getRange` with writes/clears. Correct but more I/O for repeated reads of the same range. |
| 6 | Auto-reset after commit | DESIGN | C++ no auto-reset at API >= 410. Go `postCommitReset()` clears for reuse. |
| 7 | `getRange` RYW merge | DESIGN | C++ segment-tree `RYWIterator`. Go iterative fetch+merge loop. Functionally equivalent. |
| 8 | QueueModel key | COSMETIC | C++ `endpoint.token.first()`. Go address string. Same identity. |
| 9 | Load balance secondDelay | PERF | C++ speculative second request. Go sequential. p99 only. |
| 10 | `FLAG_FIRST_IN_BATCH` | COSMETIC | Not exposed. No behavioral gap. |

### Missing C API Surface (audit 2026-04-13)

All data-path functions implemented. Missing are observability/admin only:

| C Function | Category | Assessment |
|---|---|---|
| `fdb_transaction_get_mapped_range` | Niche | Server-side index join. Record Layer doesn't use it. |
| `fdb_transaction_get_tag_throttled_duration` | Observability | Returns cumulative tag-throttle delay. Enforcement works, reporting missing. |
| `fdb_transaction_get_total_cost` | Observability | Estimated transaction cost for rate limiting. |
| `fdb_database_force_recovery_with_data_loss` | Admin | DR operation. |
| `fdb_database_create_snapshot` | Admin | Disk-level backup. |
| `fdb_database_get_main_thread_busyness` | N/A | Go has no network thread. |
| `fdb_database_get_server_protocol` | Niche | Multi-version client coordination. |

---

## Record Layer (`pkg/recordlayer/`)

### Bugs

_No known bugs. 2307 Ginkgo specs + 429 conformance specs + 50 chaos tests pass._

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

No open performance items.

### Tests

_Comprehensive: 2307 Ginkgo + 429 conformance + 50 chaos + 7 fuzz targets._

No open test items.

---

## Future: Query Planner + SQL Layer

**Not started. Blocked on: core must be rock solid first.**

### Phase 1: Cascades query optimizer (~104K lines Java)

| Component | Java files | Java lines |
|---|---|---|
| Cascades optimizer | 494 | 104K |
| Plan implementations | 74 | 19K |
| Query expressions | 35 | 9K |
| Planning + other | 43 | 15K |

### Phase 2: Relational / SQL layer (~55K lines Java)

| Component | Java files | Java lines |
|---|---|---|
| Relational core | 233 | 41K |
| Relational API | 88 | 13K |
| Server/JDBC/gRPC | 31 | small |

### Phase 3: `database/sql` driver

Go `database/sql` compatible driver. Swap your Postgres DSN for an FDB one.

---

## Infrastructure / CI

- [x] **Hetzner Object Storage upload `continue-on-error`** — Added nightshift-9. Outages no longer block PR merges.

No open infrastructure items.
