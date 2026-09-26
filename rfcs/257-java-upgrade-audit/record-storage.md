The assigned range requires substantial parity work: persistent pending-write queues, queued online indexing, GuardiANN integration and maintenance, sliding-window correctness, metadata changes, rank-bearing scans, and several lifecycle/API changes. Updating pins and generated protobufs does not complete this upgrade.

This is a source audit. I ran no builds or tests, changed no files, and performed no branch or worktree operations. Implementation must await RFC design review.

**1. Scope and consumption**

| Item | Audited revision or count |
|---|---|
| Go baseline | `e48f5b4965543cd4d99b5578356059e12d969c7c` |
| Java base | `4.12.11.0` → `257aa83cae7f90e18ea6595fdf2cf841ca72e802` |
| Java target | `4.14.2.0` → `fdacd162a9c8acfadc49082b89185c823ab8ae4a` |
| Input | `/var/tmp/query-grind-cast/java-upgrade/audit/record-storage.paths` |
| Assigned paths | **207, all unique** |
| Scoped status | 112 modified, 94 added, 1 deleted |
| Scoped additions/deletions | **23,593 / 1,981** |
| Complete diff | **31,650 lines; 1,690,499 bytes** |
| Population composition | 96 main-source/proto paths, 71 test-source/proto paths, 39 test-fixture paths, 1 Gradle path |

I passed every input path as an exact argument to:

```text
git -C fdb-record-layer diff 4.12.11.0 4.14.2.0 -- <all 207 exact paths>
```

I read the complete output in contiguous manageable chunks, including all added files, test bodies, fixtures, and deletions. Truncated portions encountered during the initial consumption were reread. The complete diff’s SHA-256 is:

```text
79aaa12f0a84fac930f1fa10898bb6949acb6ce101ec5ebfd48d6f787f6a06c0
```

I also read the scoped 49-commit history, relevant commit descriptions, and necessary target implementation context. Go mappings below use `git show` at the baseline SHA; other work was changing pins/generated files in the shared working tree during the audit.

**No assigned net-diff scope remains unread.** I did not read every unchanged line of every modified file, audit every intermediate commit independently, or consume the other researchers’ populations. In particular, the underlying GuardiANN implementation in `fdb-extensions` and the query/value implementations outside this list require the corresponding researchers’ reports.

For compact, revision-specific references below:

- `J/` means `fdb-record-layer-core/src/main/java/com/apple/foundationdb/record/` at the Java target.
- `JT/` means the corresponding `src/test/java/com/apple/foundationdb/record/`.
- `P/` means `fdb-record-layer-core/src/main/proto/`.
- Go references are relative to the Go repository and refer to the baseline, including when a clickable workspace link is provided.

**2. Behavioral work units and Go disposition**

**W1 — Pending-write queue storage and index state: required new port.**

Upstream: `c81651194` / #4309, `6a685875a` / #4293, `244241bd1` / #4350, `03415992b` / #4373, `95644c562` / #4389, `4b0143ac8` / #4410.

The target adds:

- `WRITE_ONLY_WITH_QUEUE`, persisted as **index state code 4**. `isWriteOnly()` now includes ordinary and queued write-only states; separate predicates distinguish them. Neither is scannable.
- **Format version 15**, `WRITE_ONLY_WITH_QUEUE`. Java’s default remains **7**; maximum supported becomes 15.
- A persistent generic queue with keys containing **incarnation and versionstamp**, preserving commit/local-operation order within an incarnation.
- Split payload storage, a protobuf `Any` envelope, version validation, payload-type validation, continuation-based traversal, and atomic little-endian queue-size accounting.
- Snapshot queue traversal and capacity checks. The capacity is a **soft concurrent limit**, not a serializable global quota.
- A serializable emptiness check and explicit read-conflict ranges when clearing an entry, preventing two committed drainers from decrementing the same entry twice.
- Maximum queue size **100,000**, with nonpositive values meaning unlimited.
- Overflow behavior that either fails the user operation or schedules a commit check that disables/clears the index while allowing the user write to commit.

Evidence: `J/IndexState.java`, `J/provider/foundationdb/FormatVersion.java:179,221`, `J/FDBRecordStoreProperties.java:77,90`, `J/provider/foundationdb/queue/PendingWritesQueue.java:196,238,269,287,296,319`, and `P/pending_writes_queue.proto:33`.

The envelope fields are `version=10`, `payload=11`, `enqueue_timestamp=12`; fields 1–4 are reserved. Index payloads in `P/index_build.proto:62` include `UPDATE`, `DELETE_WHERE`, old/new serialized records, old/new index entries with full primary keys, and sliding-window delegated operations. Their protobuf message identities matter because `Any` stores type URLs.

Go currently admits only states 0–3 in [index_state.go](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/index_state.go:16), rejects code 4 at line 62, and treats write-only as equality to state 1. Its supported format ceiling is 14 in [store.go](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/store.go:51). The baseline maintainer interface at `index_maintainer.go:30` has no queue capability, serialization, or replay operations.

Required changes therefore include the state decoder, predicates, state collections, write dispatch, build-state reporting, uniqueness handling, bitmap handling, cleanup, queue storage, and payload handling—not just a constant.

Two target-source details need explicit RFC disposition:

1. The overflow property’s initializer is **true**, despite contradictory documentation. The initializer determines target behavior.
2. The indexer enforces the format-15 eligibility check. The direct `markIndexWriteOnlyWithQueue()` implementation at `FDBRecordStore.java:3780` simply delegates to the state mutation; do not assume it independently applies the indexer’s eligibility rules.

**W2 — Queued online indexing, replay, closeout, and heartbeats: required new port.**

Upstream: #4293, #4350, #4370, #4373, #4389, `328daf0bb` / #4426, `a58671005` / #4465.

Java’s online indexer selects queued maintenance only when requested for that target index and when:

- The maintainer supports it.
- The key contains no record-version columns.
- The store format is at least 15.
- The build is not mutual indexing.

Otherwise it falls back to ordinary write-only maintenance. A resumed build retains its persisted state; changing the request does not silently convert an existing build. Mutual indexing cannot continue an already queued build.

Evidence: `J/provider/foundationdb/IndexingBase.java:263,272,286,314,465`; policy fields and defaults at `OnlineIndexer.java:1216,1228,1262,1547`.

The lifecycle is substantive:

- Store writes serialize updates instead of touching the index.
- `deleteRecordsWhere` queues the **computed index prefix**, preserving its order relative to other queued mutations.
- Builds discover pending work, drain it in separate transactions, and perform deferred maintenance.
- Replay applies updates before clearing queue entries.
- Readable closeout drains, then checks actual queue emptiness and changes index state in the **same transaction**.
- Writers must conflict with the readable transition in the complementary commit ordering.
- Closeout retries drain/check races, with a configurable default of 100 attempts.
- Drain transactions register heartbeat commit checks. Merge control carries a stable session ID and precommit heartbeat callback.
- Cleanup removes queue data and its size counter, in addition to existing build bookkeeping.

Evidence: `FDBRecordStore.java:774,2312`; `IndexingBase.java:382,432,970,1103`; `IndexingPendingWriteQueue.java:79,102,116,187,212`; `IndexingMerger.java:77,97`; `IndexingSubspaces.java`.

The target does **not** enable queuing for every standard index. `StandardIndexMaintainerWithQueue.java:58` supports idempotent nonsynthetic maintainers, but the production standard/value maintainer does not automatically inherit that capability. `ValueIndexMaintainerWithQueue` is a test helper. Vector maintenance supplies a different payload based on evaluated index entries; sliding-window maintenance supplies its own wrapper.

Go’s baseline:

- Dispatches directly to `UpdateWhileWriteOnly` or `Update` in `store.go:1117`.
- Applies `DeleteWhere` immediately in `store_delete_where.go:261`.
- Builds through `maintainer.Update(nil, rec)` in `online_indexer.go:1391`.
- Marks readable without draining in `online_indexer.go:1265`.
- Has existing range sets, heartbeats, commit checks, throttling, and per-target closeout that can be extended.

This is a coordinated lifecycle port. Accepting state 4 before these paths exist would make the persisted state misleading.

**W3 — Sliding-window replay and double-counting fix: required correctness port.**

Upstream: `f9935060a` / #4405, coupled to `a15a17ae5` / #4370.

Target `SlidingWindowIndexMaintainer.java:541` checks whether the exact tracked entry already exists **before** changing window bookkeeping. For an existing entry:

- Count, entries, and boundary remain unchanged.
- A missing boundary is treated as corruption.
- An in-window entry refreshes the delegate.
- An overflow entry leaves the delegate alone.

The old preemptive-delete approach is removed. Ordinary and write-only updates use serialized delete/insert suppliers, and queued updates preserve separately serialized delegated delete and insert operations. Filtering happens when enqueueing and is not reevaluated during replay. The metric changes from `SW_PREEMPTIVE_DELETE_WRITE_ONLY` to `SW_REINSERT_ALREADY_TRACKED`.

Evidence: `J/provider/foundationdb/indexes/SlidingWindowIndexMaintainer.java:369,395,429,471,541,860`.

The baseline Go code still implements preemptive deletion at [sliding_window_index_maintainer.go:322](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/sliding_window_index_maintainer.go:322), and ordinary insertion unconditionally writes the entry and increments a partially filled window at lines 413–451.

Go already serializes the preemptive delete and subsequent update under one lock. That avoids conflating its implementation with Java’s old asynchronous sequencing. It does **not** solve the opposite ordering: a user write indexes a record first, and the online builder later calls ordinary `Update(nil, record)` for the same tracked entry.

The existing Go test at `sliding_window_index_test.go:1241` checks only `UpdateWhileWriteOnly(nil, saved)`. Its valid result assertion should stay; its explanation and coverage are insufficient for the target fix.

The commit description claims base-record rereading and per-record supersession checks. The tagged implementation at lines 541–567 does not do that: it reads the tracked **entry key**. Port the tagged implementation, not that stale description.

**W4 — Vector engine selection, typed options, aliases, and scan behavior: required port, with pre-existing gaps.**

Upstream: `4e3d42bc8` / #4083, `1ce8c71dd` / #4357, `38c05809e` / #4422, `24863e8af` / #4604; planner preference schema from `6d4bf06f0` / #4423.

The vector index becomes engine-selectable:

- Missing engine means HNSW.
- Engine values are case-insensitive.
- GuardiANN is a distinct engine.
- Unknown engines fail.
- Changing an existing index’s engine fails because it changes the disk layout.

Evidence: `VectorIndexEngineKind.java:37,50`; `VectorIndexEngine.java:215,248`.

Shared option concepts use typed keys. For index metadata, the canonical written names deliberately remain the legacy `hnsw*` names, while neutral `vector*` names are accepted aliases. These cover metric, dimensions, statistics settings, RaBitQ enablement, and RaBitQ bits. The scan return-vectors option instead uses `vectorReturnVectors`, with the old HNSW name accepted as an alias.

Duplicate aliases for one logical option are rejected, including equal-valued duplicates. Option evolution compares **parsed effective configuration**, so introducing an explicit default or changing only an alias need not force a rebuild. Engine/structural changes still fail. Metric serialization uses the enum name.

Evidence: `VectorIndexOptionKeys.java:47–164`; `VectorIndexOptionsHelper.java:52,75,121,196`; `VectorIndexScanOptions.java:50,191`; `VectorOptionKey.java`; `VectorIndexMaintainerFactory.java:155`.

The GuardiANN index-option surface includes all of:

- Primary cluster minimum, maximum, hard maximum, and underreplicated maximum.
- Merge peak fraction, minimum child fraction, relative imbalance, split imbalance penalty.
- Replicated target/max writes; replication priority, distance-ratio/z-score weights, minimum statistics sample size.
- Deterministic randomness, sample batch size.
- Insert/delete candidate limits and delete concurrency.
- Split/merge nearest-cluster counts.
- K-means iterations/restarts.
- Reassign neighbor count, collapse duplicate threshold.
- Split/merge, reassign, collapse, and bounce concurrency.
- Construction centroid ring/outward search settings.

Scan options additionally expose candidate-pool factor, maximum clusters, minimum clusters before pruning, distance-ratio cutoff, centroid ring/outward search, and search concurrency.

Stats/concurrency settings and the primary-cluster hard cap are mutable. The remaining structural/configuration settings are checked for effective equality. See `GuardiannVectorIndexEngine.java:84,383`.

Go constructs an HNSW maintainer unconditionally for `IndexTypeVector` in [store.go:1250](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/store.go:1250). `vector_index_maintainer.go:121` immediately parses HNSW configuration; its constructor contains HNSW storage/configuration fields. The distinct `vector_spfresh` branch at `store.go:1278` is a Go extension, not a GuardiANN implementation.

This has a compatibility consequence: a recognized `vector` index carrying the new GuardiANN engine option must not silently proceed through the HNSW branch.

Baseline Go option parsing uses literal HNSW names (`vector_index_maintainer.go:139`), build validation requires the old dimension name (`vector_index_validation.go:41`), and evolution compares raw structural option strings (`metadata_evolution_validator.go:839`). Those mechanisms need the target’s alias/effective-value rules.

Already present: HNSW’s automatic `efSearch` formula at `vector_index_maintainer.go:528` matches the retained target formula. Existing partitioned-vector continuations are relevant preservation contracts, not new proof of compatibility.

Pre-existing gap: Go always returns a null vector payload at `vector_index_maintainer.go:617`. Both Java tags default to returning vectors when RaBitQ is disabled. The option refactor does not originate this gap, but the upgraded shared vector surface must account for it.

**W5 — GuardiANN deferred maintenance, task accounting, leases, and backpressure: required major port.**

Upstream: #4357, `b9ceed781` / #4418, #4465, #4604.

The assigned code supplies more than a search adapter:

- Insert/delete listeners account for deferred tasks transactionally.
- GuardiANN deletion receives the vector as well as its primary key.
- In-transaction maintenance is controlled through `IndexDeferredMaintenanceControl.shouldAutoMergeDuringCommit()`.
- Deferred writes signal that merging is required. A later write that enqueues nothing still checks for an outstanding backlog and restores a lost signal.
- Task counters are sparse **per partition prefix**, maintained by atomic ADD and COMPARE_AND_CLEAR. There is no target global-total counter.
- Secondary subspace prefixes are numeric: **0 task counts, 1 merge leases, 2 delete guard**.
- Negative task counts are treated as corruption: a commit check disables the index and allows that disable to commit.
- Merge runs require a stable session UUID.
- Lease claims and draining occur in separate transactions; a later invocation verifies ownership before draining.
- Lease reads use snapshot isolation. Concurrent claims can commit; subsequent ownership verification determines who drains.
- A delete-guard conflict key closes the claim-versus-deleteWhere race, and prefix deletion clears associated leases/counts.
- Leases use a 60-second window; implausibly far-future timestamps are stale too.
- A merge examines at most 16 candidate prefixes, prefers one already owned, otherwise randomly selects a free candidate.
- Draining is bounded by count and time. Defaults are one task and a four-second quota; the engine must make at least one task’s progress. After a drain, the target requests another pass even if that partition has just emptied.
- Cluster-capacity failures are translated to the vector-index backpressure exception through wrapped causes.

Evidence: `VectorIndexMaintainer.java:373,408,529,627,675`; `VectorIndexTaskCounts.java:108,140,181,201`; `VectorIndexMergeLock.java:63,118,165`; `VectorIndexSecondarySubspaceKeys.java:40`; `GuardiannVectorIndexEngine.java:127,152,174,207`.

The Go baseline online-build loop at `online_indexer.go:824` performs range building and readable closeout, without this merge driver or persistent task-accounting lifecycle. Its vector maintainer has neither engine dispatch nor a deferred-maintenance implementation. Existing SPFresh maintenance does not provide the same keys, payloads, engine behavior, or ownership protocol.

This work depends on the other population’s GuardiANN engine/storage port. That dependency includes #4604’s final split/merge policy and defaults. My coverage establishes the assigned option plumbing, adapter, accounting, and lifecycle requirements; it does not certify the unassigned clustering implementation.

Several #4418 description details differ from the tag: the target uses random free-prefix selection, not the described rendezvous hashing, and the target switch is maintenance control, not the described index option. These differences matter to a faithful port.

**W6 — Replacement-index initialization and retirement: required port with a pre-existing lifecycle gap.**

Upstream: `786b17acb` / #4371.

Target `FDBRecordStore.java:4973` enumerates all indexes changed since the prior metadata version. At lines 5195–5223, a changed/new index declaring replacements receives **DISABLED**, regardless of the user rebuild checker. This prevents an unbuilt replaced index from remaining implicitly readable.

Existing readable indexes are retained until all replacements are genuinely READABLE; READABLE_UNIQUE_PENDING is insufficient. Explicit rebuild behavior and replacement retirement must also be preserved. Target retirement is integrated into state-change/commit handling at lines 2939–2981.

Go’s misleadingly named `GetIndexesToBuildSince()` already returns all modified indexes (`metadata.go:2039`), so Java’s old filtering error should **not** be mechanically copied into the Go diagnosis.

The missing range change is the disabled-state override before the rebuild policy in `store_builder.go:427`. In addition, the inspected Go lifecycle paths do not implement replacement-driven retirement; `GetReplacedByIndexNames()` is used for metadata validation, not retirement. That latter gap predates this range and is exposed by the expanded target contracts.

The upstream description says the old index is passed through the user checker. The final target source instead explicitly selects DISABLED for replaced indexes.

**W7 — Rank included in scan entry values: required new port.**

Upstream: `755489b33` / #4563.

`RankScanBounds` supports BY_RANK and BY_VALUE plus `includeRankAsValue`; other scan types are rejected. When requested, Java computes the actual rank for each entry and substitutes `Tuple(rank)` as its value. Without the option, the existing empty-value behavior remains.

Evidence: `RankScanBounds.java:48`; `RankIndexMaintainer.java:149,263`; `IndexEntry.java:201`; `RankedSetIndexHelper.java:235`.

The rank helper extracts exactly the grouped score columns, excluding appended primary-key components. The rank is the index’s rank, not the result’s ordinal within a bounded scan. Ties and grouping remain governed by ranked-set configuration.

Go’s `ScanByRank` only converts rank bounds into score bounds and delegates a normal scan (`rank_index_maintainer.go:208`). The store API has no include-rank option (`index_scan.go:310`). `RankForScore` already provides the underlying lookup (`rank_index_maintainer.go:436`), and `IndexEntry` exists at `index_scan.go:221`.

No on-disk rank-index rewrite is introduced. The new result-value contract must not change default scans or accidentally include the primary-key suffix in score lookup.

**W8 — Metadata evolution and proto editing: mixed existing behavior and required changes.**

Upstream: `7067ab019` / #4475, `1ea9b0773` / #4399, `13863481a` / #4412.

Three changes need distinct dispositions:

1. **Correct old descriptors during index field renames.** Java now iterates original record types and maps each to its new counterpart (`MetaDataEvolutionValidator.java:698`). Go already does that in `metadata_evolution_validator.go:444`. This specific loop correction is already represented.

   However, Go’s `getTypeRenames()` gives an existing name an identity mapping before comparing record identity (`metadata_evolution_validator.go:191`). The target’s new swap/name-reuse tests therefore still need to be brought across; the broader pre-existing Go mapping can defeat those cases. Java’s `swapTypesWithIndexesRequiresRenamingFields`, at `JT/metadata/MetaDataEvolutionValidatorTest.java:1206`, requires success only after both index type associations and field names are corrected. This remains source-identified work, not an executed failure claim.

2. **Ignored index options.** Java adds an immutable configured set, preserved by builder copying, defaulting empty. Changed-option computation excludes that set before handing a mutable remainder to type-specific validators (`MetaDataEvolutionValidator.java:752,776,1366`). The old one-argument validator entry point is removed and the two-argument method becomes public.

   Go computes changes directly inside `validateIndexOptions()` at `metadata_evolution_validator.go:691`, with no ignored-option configuration. This is a required API/behavior port. Defaults must remain strict; the existence of the option is not permission to ignore changes globally.

3. **Proto-editor enum handling.** Target field-type resolution distinguishes message, enum, and primitive fields at `MetaDataProtoEditor.java:275`. Nested enum references can follow an enclosing message rename instead of incorrectly treating the enum as a message.

   Go has descriptor import/export and absolutization, but no corresponding public rename-editor operation in the inspected metadata builder/store surface. This particular Java editor exception has no direct Go call-site port. It remains an explicit API gap; descriptor round-trip tests must preserve enum references. The surrounding editor cleanup is largely Java refactoring/documentation.

**W9 — Stored-query metadata and temporary functions: required new feature and schema-preservation work.**

Upstream: `53daa4dee` / #4157, `02af41b57` / #4291.

Record metadata now contains named stored queries, each with SQL text and a list of temporary function definitions. Builder loading, addition, retrieval, immutable metadata exposure, and serialization are added.

Evidence: `RecordMetaData.java:712,747,756`; `RecordMetaDataBuilder.java:245,1229`; `P/record_metadata.proto:213,229`.

Go’s baseline metadata model does not represent stored queries. Its protobuf adapter explicitly carries only the previously known unmodeled fields—joined/unnested records, UDFs, and views—and unknown bytes. See [metadata_proto.go:20](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/metadata_proto.go:20) and line 405.

This creates an immediate regeneration hazard:

- Before regeneration, field 16 can survive as unknown protobuf data.
- After regeneration, field 16 becomes known.
- Existing `preservedMetaDataFieldsFromProto()` does not copy it.
- Reconstructing metadata can therefore drop stored queries unless the model/preservation path is updated.

Required work includes metadata round trips and the public model. Stored-query planning/cache warmup and temporary-function execution depend on the relational/query populations. Existing UDF/view runtime omissions are pre-existing; they are dependencies, not evidence that the new stored-query feature is implemented.

**W10 — Aggregate cursor and query-plan wire changes: cursor behavior partly present; schema and runtime coupling required.**

Upstream: `296c13989` / #4468, `1b124cecc` / #4463, `4c41cce8f` / #4600, `12a0a326d` / #4550, `f8f7e9569` / #4593, `2da82fb53` / #4625, `b2b930437` / #4318, `daaf0f2e6` / #4397, #4423.

The assigned `AggregateCursor` drops its legacy serialization-mode branch. Continuations consistently use the aggregate envelope, including partial aggregate state when resource limits interrupt a group. Invalid serialized continuations fail.

Evidence: `J/cursors/aggregate/AggregateCursor.java:69,82,149,246`.

Go already saves partial state on non-exhaustion stops (`query/executor/streaming_cursors.go:324`) and parses an aggregate envelope. However, [continuation.go:860](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/query/executor/continuation.go:860) explicitly documents a **Go-private accumulator layout**, with shape checks rejecting incompatible payloads. Sharing the protobuf envelope does not make Java and Go continuations interchangeable. That is a pre-existing compatibility limitation.

All assigned plan-schema deltas must be accounted for:

| Target schema change | Implication |
|---|---|
| `PValue` field 64: `PArrayAggValue` | New aggregate value; child, ignore-nulls flag, and in-call limit. |
| `PValue` field 58 reserved | Retired compile-time row-number-high-order value must not be reused. |
| Plan field 25 reserved | Old streaming-aggregation representation retired. |
| Plan field 38 becomes the unified streaming-aggregation plan | Preserve field identity; do not reinterpret old field 25 as equivalent bytes. |
| Streaming aggregate’s old default-on-empty field removed | Coordinate execution/default-on-empty behavior with query owner. |
| Aggregate-index fields 7 and 8 | Result type and value-based entry-to-record reconstruction. Existing copier fallback remains represented. |
| New covering-index-value plan, field 41 | Decoder/schema support is distinct from planner emission. |
| UDF macro argument names/default-argument payloads | Preserve absence/provided distinctions and values. |
| Explode `zero_based_ordinality=3` | New serialized execution choice. |
| Planner vector-engine preference field 15 | None/HNSW/GuardiANN preference must survive config serialization. |

Evidence: `P/record_query_plan.proto:288,293,296,490,1748,1754,1777,1801,1886,2271`; `P/record_planner_config.proto:39,63`.

Go’s aggregate enum contains COUNT/SUM/MIN/MAX/AVG only (`query/plan/cascades/expressions/group_by.go:14`), and its continuation state is designed around those aggregates. `ARRAY_AGG`, including partial-list continuation state and limit/null semantics, is required work coordinated with the query researcher.

Go’s aggregate-index and covering-index plans hold their existing result/layout state (`query/plan/plans/aggregate_index.go:65`; `covering_index_scan.go:39`); they do not contain the new entry-to-record value field. The assigned storage-layout tests are relevant contracts for that work: VALUE key/value split, aggregate grouping/value storage, permuted secondary ordering, and default rank-entry shape.

The function-key-expression change is an adaptation to positional `CallSiteArguments`, not a new key encoding. Its broader named/default argument semantics belong to the function/query implementation work.

I am not claiming coverage of those unassigned query/value implementations.

**W11 — Header-aware store deletion and cache coherence: required port, with a pre-existing defect.**

Upstream: `ab19a6a93` / #4354.

The new Java deletion API reads the header serializably:

- Missing or noncacheable header: no metadata-version-stamp bump.
- Cacheable header: bump.
- Malformed header: conservatively bump.
- Always mark store state dirty and clear the store.

Evidence: `J/provider/foundationdb/FDBRecordStore.java:1886`. The deprecated deletion API retains unconditional invalidation.

Go’s [DeleteStore](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/store_api.go:145) only clears the prefix. It performs neither dirty-state marking nor stamp invalidation. That is already short of the old Java cache contract, while conditional header-aware invalidation is the range-introduced improvement.

The corresponding Go facilities already exist: `database.go:1166,1193` and the store-state cache’s header conflict handling at `store_state_cache.go:71`. Port the transactional behavior into the synchronous Go API as appropriate; Java’s future-returning shape does not require a new Go asynchronous framework.

The production cache-class changes themselves are documentation and equivalent Duration API substitutions.

**W12 — Decryption retry joins decompression retry: pre-existing serializer gap plus required target behavior.**

Upstream: `27e918eb8` / #4290.

Target `TransformedRecordSerializer.java:336` retries the complete decrypt/decompress pipeline from original serialized bytes. It now covers encryption-only failures too. Each attempt starts with fresh transformation state. Retry exhaustion preserves the failure cause; successful retry can still produce the configured diagnostic failure when `failOnDeserializeReattempt` is enabled. Inner record deserialization remains outside this retry loop.

Go’s record path calls `serializeUnion()` and direct protobuf decoding (`store.go:648,2052,2121`). The inspected store/builder surface does not install a transformed serializer, compression/decryption pipeline, or corresponding retry configuration.

This is not an already implemented fix. Transformed serialization is a pre-existing feature gap; the additional retry contract must be included when closing that shared-surface gap. The range does not introduce a new transformed-record wire encoding.

**W13 — Lock-registry cleanup: required lifecycle adaptation.**

Upstream: `80db56e84` / #4545.

Java now removes a completed lock entry only when it is still the newest registration and its entire dependent work chain is complete. Releasing the latest reader must not forget an older reader or queued writer.

Evidence: `J/locking/AsyncLock.java:38,65,102`; `J/locking/LockRegistry.java:177,191`.

Go uses a map of `*sync.RWMutex` at `database.go:1287`. `getOrCreate()` adds entries, and unlock methods retrieve them from the map again; nothing removes them. The Java future chain is language-specific, but the newly bounded registry lifetime has a direct Go resource-lifecycle analogue.

A port must preserve holders and waiters before deleting an entry. Deleting merely on an unlock would create two live locks for the same key. This is a scoped lifecycle change, not authorization for an unrelated performance campaign.

**W14 — Client knobs: new API, backend-dependent implementation required.**

Upstream: `1c0d2c893` / #4488; `0644df2ed` / #4560 adds external-client documentation.

The factory adds named/typed client knobs, startup application, application to an initialized native client, retrieval, and pre-initialization clearing. Known names are validated even through the string API. Unknown nonblank names without `=` use the escape hatch.

The 19 typed knobs cover packet limits/warnings, TLS handshake/connections, commit/GRV proxy connections, failed-endpoint retry, connection logging, reconnection timing, location-cache peer eviction, and proxy-list cache clearing.

Validation distinguishes int32, int64, double, boolean, and string. Integer parsing supports Java decode forms but excludes `#`; boolean accepts true/false or an int32 representation. Preserve the actual accepted forms rather than substituting an arbitrary “strict” parser.

Evidence: `FDBClientKnob.java:88–211,270`; `FDBDatabaseFactoryImpl.java:153,260,282,292,297,317`.

Go’s factory at `database.go:129` has only database/cache construction. It opens through the pure-Go/libfdb_c seam at `pkg/internal/fdbclient/open_*.go`; there is no corresponding knob API or startup application path.

The configuration/API work is new parity work. Native-only effects need an explicit backend contract; a pure-Go implementation must not silently claim to honor an unsupported native knob. The JVM-specific external-client warning has no direct Go runtime port.

**W15 — Metrics, session diagnostics, and registry behavior: mixed port/no analogue.**

Upstream: `c7f4e83e2` / #4289, `21f64a1f3` / #3990, `0275f9cb4` / #4541, plus queue/vector/deletion commits.

Changes include:

- Typed context-session keys and sets of updated readable, ordinary write-only, and queued write-only indexes.
- Reuse of existing timer events in `EventKeeperTranslator`, including correct count/event dispatch.
- HNSW read instrumentation during writes, proper byte totals, and nullable-timer handling.
- Queue writes/clears/size/overflow-disable counters.
- Vector references read, task enqueue/execute, negative-task-count disable counters.
- Delete-store wait instrumentation and diagnostic log keys.
- Duplicate maintainer registrations now throw instead of warning and selecting one.

Evidence: `FDBRecordContext.java:1564,1588,1603`; `ContextSessionKey.java:45,51,58`; `EventKeeperTranslator.java:44,66`; `HnswVectorIndexEngine.java:108,126,288`; `VectorIndexInstrumentation.java:45`; `FDBStoreTimer.java:775,904`; `IndexMaintainerFactoryRegistryImpl.java:74`.

Go already has context session storage (`database.go:691`) and typed timer events (`store_timer.go:126,253,263`). It does not populate the new index-update diagnostic sets. HNSW has separate optional I/O statistics (`hnsw.go:1599`); that is not equivalent to the target store-timer wiring.

A target anomaly must be recorded: **both write-only session-key constants use `"writeOnlyIndexesUpdated"`**, and equality is name-based. They therefore collide. The target tests do not establish independent sets for those two constants. The RFC must explicitly dispose of this source fact rather than silently asserting the intended separation works.

The duplicate-factory change has no active Go registration analogue: Go’s maintainer selection is a switch, and unknown types already fail (`store.go:1282`). It does not justify creating a registry framework.

**W16 — Java-only changes, test relocation, and build handoff.**

The remaining changes include SpotBugs annotations, a private singleton constructor, equivalent Duration APIs, import cleanup, Java collection construction, documentation, and some `Stream.toList()` returns becoming unmodifiable Java lists.

The assigned Gradle file adds test-fixture publication/consumption, GuardiANN test-fixture dependencies, checksum-verified SIFT test data wiring, lazy task registration, and service-file duplicate inclusion before merging. The latter couples to service-loader/factory tests. Build integration belongs to the controller; I did not execute downloads or tasks.

I verified **29 fixture destinations byte-for-byte against their base test-source counterparts**: 15 Java files and 14 protobuf files. The two collation bases are relocations with class renaming/concrete-test extraction; their test logic is retained, with the key-expression evaluation helper made local. Seven package-info files are package documentation. `DeleteStoreMode` is a new test helper selecting the two deletion APIs.

**3. Regression contracts, expectations, and compatibility**

The following are required focused regression contracts after RFC approval. They are not claims that tests have passed.

| Area | Required contract |
|---|---|
| State/format | Round-trip all five states; code 4 is write-only and unscannable; default-readable omission remains; maximum 15 is distinct from the default creation target; old-format queue requests fall back. |
| Queue wire | Cross-language `Any` identities and field numbers; split payloads; incarnation/versionstamp ordering; malformed/future-version/wrong-type rejection; row/byte-limit continuation behavior. |
| Queue concurrency | Concurrent enqueue accounting; two drainers cannot commit duplicate removal; both commit orderings of enqueue versus readable closeout; retry does not lose a mutation or double-decrement size. |
| Queue lifecycle | CRUD, repeated updates, DELETE_WHERE ordering, resume, mixed queued/direct targets, unsupported/versioned/mutual fallbacks, uniqueness violations during drain, queue cleanup, overflow disable versus failure. |
| Heartbeats | Every drain/merge transaction’s commit check completes before commit, including the final exhausted batch; active build heartbeat protection remains throughout draining. |
| Sliding window | Both write-before-build and build-before-write; ordinary `Update(nil, record)` replay; partially filled/full/overflow windows; same-key delegate refresh; changed key/partition; queued replay; count, boundary, entry list, and delegate contents agree. |
| GuardiANN | Engine dispatch; target options/defaults; task-count accounting and follow-up tasks; sparse zero removal; negative-count disable; two-phase claim/drain; lease expiry/future timestamps; deleteWhere races; count/time budgets; backpressure; backlog signal restoration. |
| Vector scans | Old/new aliases; duplicate alias rejection; raw and RaBitQ return-vector choices; fresh/resumed scans; partition prefixes and full primary keys; nil timers; byte counters measure bytes. |
| Replacement indexes | Fresh/existing stores, different rebuild policies, multiple replacements, unique-pending replacement, explicit rebuild, retirement without another metadata-version bump. |
| Rank | BY_VALUE and BY_RANK with/without rank values; ties; grouped ranks; bounded scans return absolute ranks; appended PK columns do not contaminate score lookup; unchanged default wire layout. |
| Metadata | Rename/reuse/swap cases; default strict validation; ignored options copied independently; other changed options still rejected; nested enum references; stored queries/temp functions survive load/build/save. |
| Aggregate/query schema | Every new/reserved field accounted for; no reuse of retired tags; partial aggregate state survives interruption; ArrayAgg state/limit/null contracts coordinated with query owner; old copier fallback and new value reconstruction agree. |
| Delete/cache | Missing, noncacheable, cacheable, malformed headers; delete/recreate in one and separate transactions; cached open/read versus deletion in both commit orders; dirty-state and stamp behavior. |
| Serializer | Encryption-only transient failure; decrypt then decompress failure; fresh original bytes each attempt; retry limits; diagnostic fail-on-successful-retry; preserved cause. |
| Locks/knobs | Out-of-order reader release and queued writers; map cleanup without split ownership; knob parsing/name validation and startup/initialized-client application. |

Relevant upstream tests include `OnlineIndexerPendingWriteQueueTest`, all three `PendingWritesQueue*Test` classes, sliding-window tests around lines 1165–1483 and 2178–2670, `RankIndexEntryRankTest`, `DeleteStoreTest`, and the GuardiANN lease/count/merge/backpressure suites. The test path ledger below identifies all assigned test changes.

Expectation and ledger changes need particular care:

- **Concrete defect-enshrining test:** [vector_index_test.go:2216](/home/birdy/projects/fdb-record-layer-go/pkg/recordlayer/vector_index_test.go:2216) asserts null payloads for an ordinary unquantized scan. Its setup at line 2164 does not enable RaBitQ or request suppressed vectors. Java base already returns vectors by default (`VectorIndexMaintainer.java:392–404` at the base); target retains that behavior in `VectorIndexOptionsHelper`. This is a **pre-existing Go defect**, not a newly introduced Java semantic change. Replace that expectation with an explicit return-vectors contract; do not refresh arbitrary outputs.
- **Sliding-window test limitation:** the test at `sliding_window_index_test.go:1241` protects a valid result through the old workaround. Preserve its assertion and add the opposite ordering/direct-build case. Do not weaken counts, membership, or delegate assertions.
- **Format decision:** `format_version_split_test.go:110–122` and `DIVERGENCES.md:14` explicitly protect the prior decision to keep Go’s default at 14. Raising the supported maximum must not implicitly raise the default through `formatVersionDefault = formatVersionMaxSupported`. The RFC must revisit the default separately, preserving old-reader/rolling-upgrade cases. Java target still defaults to 7.
- **Metric expectation:** retire the preemptive-delete metric in favor of the tracked-reinsert metric where the target does. This needs deliberate metric-contract updates.
- **Continuation claims:** Go’s aggregate continuation payload remains explicitly private. No schema regeneration or successful protobuf unmarshal proves cross-language resume.
- **New GuardiANN tests are not automatically strong enough:** the assigned “concurrent” suite uses one inserter in its configured scenario, and a nonnegative backpressure-count assertion does not prove backpressure occurred. Its recall threshold is not permission to weaken existing Go recall expectations.
- I did not find a scoped Go assertion intentionally requiring sliding-window count inflation or stale cache reuse. That absence should not be replaced with an invented test finding.

Compatibility decisions:

1. **State 4 and format 15 prevent a bump-only rollout.** Older binaries cannot safely participate in queued builds. Keep format support, feature selection, and deployment activation distinct.
2. **Intermediate queued formats changed within the range.** #4350 and #4370 explicitly warn that active experimental queues from intermediate revisions can be incompatible. Base 4.12.11.0 has no such queue, but intermediate deployments need an explicit disable/rebuild or migration decision.
3. **GuardiANN is a new persistent layout**, not an HNSW option that old Go can ignore. Engine identity must govern access before any mutation.
4. **Stored-query field 16 needs explicit preservation immediately upon regeneration.**
5. **Plan tags 25 and 58 are retired.** Do not silently reinterpret their payloads or reuse the numbers.
6. **Rank-bearing values change scan output only when requested**, not the stored rank-index layout.
7. **Serializer retry and header-aware deletion do not introduce new record encodings**, but their transactional/error behavior changes.
8. **The accepted structured `PromoteValue` defect is re-armed and must be closed in this upgrade.** That is a controller-owned mandatory dependency; this report does not independently claim coverage of its paths.

No expectation weakening, bulk golden refresh, new bug-hunt campaign, or unrelated performance work is authorized by this audit. The five restricted hunts remain unapproved. Go-only query extensions remain allowed.

**4. Exhaustive path ledger**

Every entry below was included in the complete diff read. `Wn` refers to the work unit above. `T` means test source reviewed; runtime verification is still outstanding. `J-only` means the assigned delta is Java-specific/mechanical or documentation. `F-identical` means byte-identical relocation verified against the base counterpart.

Prefixes expand literally:

```text
C  = fdb-record-layer-core/
J  = C/src/main/java/com/apple/foundationdb/record/
F  = J/provider/foundationdb/
I  = F/indexes/
P  = C/src/main/proto/
T  = C/src/test/java/com/apple/foundationdb/record/
TF = T/provider/foundationdb/
TI = TF/indexes/
TP = C/src/test/proto/
X  = C/src/testFixtures/java/com/apple/foundationdb/record/
XP = C/src/testFixtures/proto/
```

Main/build paths, **1–97**:

```text
001 C/fdb-record-layer-core.gradle                                  W16; controller build handoff
002 J/AsyncLoadingCache.java                                        J-only: equivalent Duration API
003 J/ByteArrayContinuation.java                                    J-only: annotation removal
004 J/FDBRecordStoreProperties.java                                 W1: overflow policy and capacity
005 J/IndexEntry.java                                               W7: withValue
006 J/IndexState.java                                               W1: code 4 and predicates
007 J/KeyRange.java                                                 J-only: annotation removal
008 J/MutableRecordStoreState.java                                  W1: state-predicate integration
009 J/PlanSerializationContext.java                                 J-only: annotation/private constructor
010 J/RecordCursor.java                                             J-only: annotation removal
011 J/RecordMetaData.java                                           W9: stored queries
012 J/RecordMetaDataBuilder.java                                    W9: stored-query builder/load
013 J/RecordStoreState.java                                         W1: queued-state treatment
014 J/cursors/MapWhileCursor.java                                   J-only: annotation removal
015 J/cursors/aggregate/AggregateCursor.java                         W10: unified continuations
016 J/locking/AsyncLock.java                                        W13: dependent-work completion
017 J/locking/LockRegistry.java                                     W13: safe cleanup
018 J/logging/LogMessageKeys.java                                   W1/W4/W14/W15: diagnostics
019 J/metadata/Index.java                                           W6 documentation; annotation cleanup
020 J/metadata/IndexOptions.java                                    W4/W5: vector option surface
021 J/metadata/IndexValidator.java                                  W8: changed-option API
022 J/metadata/MetaDataEvolutionValidator.java                       W8: rename fix and ignored options
023 J/metadata/expressions/FunctionKeyExpression.java                W10: positional call-site adapter
024 J/provider/common/DynamicMessageRecordSerializer.java           J-only: annotation removal
025 J/provider/common/TransformedRecordSerializer.java              W12: decrypt/decompress retries
026 J/provider/common/TransformedRecordSerializerState.java         J-only: annotation removal
027 F/ContextSessionKey.java                                        W15: typed diagnostics; key collision
028 F/EventKeeperTranslator.java                                    W15: canonical timer events
029 F/FDBClientKnob.java                                            W14: typed knob catalog
030 F/FDBDatabaseFactory.java                                       W14: knob API/external-client docs
031 F/FDBDatabaseFactoryImpl.java                                   W14: parsing/application/lifecycle
032 F/FDBDatabaseRunnerImpl.java                                    J-only: annotation removal
033 F/FDBExceptions.java                                            W5: cycle-safe cause traversal
034 F/FDBRecordContext.java                                         W15: typed session/set API
035 F/FDBRecordStore.java                                           W1/W2/W6/W11/W15
036 F/FDBRecordStoreBase.java                                       W1: state-predicate API adaptation
037 F/FDBRecordVersion.java                                         J-only: annotation removal
038 F/FDBStoreTimer.java                                            W1/W5/W11/W15: metrics
039 F/FormatVersion.java                                            W1: format 15
040 F/IndexBuildState.java                                          W1: both write-only states
041 F/IndexDeferredMaintenanceControl.java                          W2/W5: session and precommit callback
042 F/IndexFunctionHelper.java                                      W1: readable predicate; same selection rule
043 F/IndexMaintainer.java                                          W1/W2: queue capability/replay contract
044 F/IndexMaintainerFactoryRegistryImpl.java                        W15: duplicate registration failure
045 F/IndexingBase.java                                             W2: eligibility/drain/closeout/heartbeat
046 F/IndexingByIndex.java                                          W1/W2: state integration
047 F/IndexingCommon.java                                           W2: queued targets; Java immutable lists
048 F/IndexingMerger.java                                           W2/W5: merge identity and heartbeat callback
049 F/IndexingPendingWriteQueue.java                                W1/W2: drain/overflow handling
050 F/IndexingSubspaces.java                                        W1/W2: queue subspaces and cleanup
051 F/IndexingThrottle.java                                         W2: pending-work discovery
052 F/MetaDataProtoEditor.java                                      W8: enum resolution/refactoring
053 F/OnlineIndexer.java                                            W2: queue policy and drain-attempt API
054 F/RankScanBounds.java                                           W7: rank-valued scans
055 F/VectorIndexScanBounds.java                                    J-only: documentation/file header
056 F/VectorIndexScanComparisons.java                               W4: return-vectors option binding
057 F/VectorIndexScanOptions.java                                   W4: typed options/aliases/proto validation
058 I/BitmapValueIndexMaintainer.java                               W1: write-only predicate
059 I/GuardiannVectorIndexEngine.java                               W4/W5: engine/options/maintenance
060 I/HnswVectorIndexEngine.java                                    W4/W15: extracted engine/options/metrics
061 I/MaintenanceControlRegister.java                               W5: merge-required signaling
062 I/PrefixTaskCount.java                                          W5: prefix/count data
063 I/RankIndexMaintainer.java                                      W7: rank projection and score slicing
064 I/RankedSetIndexHelper.java                                     W7: exposed lookup helper
065 I/SlidingWindowIndexMaintainer.java                             W2/W3: queue wrapper and replay fix
066 I/StandardIndexMaintainer.java                                  W1: uniqueness/state predicates
067 I/StandardIndexMaintainerWithQueue.java                          W2: opt-in record-payload replay
068 I/TaskCountRegister.java                                        W5: accounting callback
069 I/TaskEventRegister.java                                        W5: event composition
070 I/TextIndexMaintainer.java                                      J-only: annotation removal
071 I/TextIndexMaintainerFactory.java                               W8: public changed-option method
072 I/VectorIndexClusterTooLargeException.java                       W5: backpressure exception
073 I/VectorIndexEngine.java                                        W4/W5: engine contract/dispatch
074 I/VectorIndexEngineKind.java                                    W4: engine identity/default/parsing
075 I/VectorIndexHelper.java                                        W4: engine-neutral option access
076 I/VectorIndexInstrumentation.java                               W15: shared byte/key counters
077 I/VectorIndexMaintainer.java                                    W2/W4/W5: queue/engine/merge lifecycle
078 I/VectorIndexMaintainerFactory.java                              W4/W8: engine-aware validation
079 I/VectorIndexMergeLock.java                                     W5: leases and delete conflicts
080 I/VectorIndexOptionKeys.java                                    W4: complete typed option catalog
081 I/VectorIndexOptionsHelper.java                                 W4: aliases/effective values/defaults
082 I/VectorIndexSecondarySubspaceKeys.java                          W5: numeric persistent prefixes
083 I/VectorIndexTaskCounts.java                                    W5: sparse transactional task counts
084 I/VectorOptionKey.java                                          W4: typed parsing/name identity/hash
085 F/keyspace/DirectoryLayerDirectory.java                         J-only: annotation removal
086 F/keyspace/KeySpaceDirectory.java                               J-only: annotation removal
087 F/keyspace/LocatableResolver.java                               J-only: annotation removal
088 F/queue/PendingWritesQueue.java                                 W1: persistent queue
089 F/queue/PendingWritesQueueEntry.java                             W1: typed entry API
090 F/queue/package-info.java                                       W1 documentation
091 F/storestate/MetaDataVersionStampStoreStateCache.java            W11 documentation only
092 F/storestate/MetaDataVersionStampStoreStateCacheFactory.java     J-only: equivalent Duration API
093 P/index_build.proto                                             W1/W2: operation/delegate payloads
094 P/pending_writes_queue.proto                                    W1: versioned Any envelope
095 P/record_metadata.proto                                        W9: stored-query schema
096 P/record_planner_config.proto                                   W4/W10: engine preference
097 P/record_query_plan.proto                                       W10: aggregate/value/plan/UDF/ordinality wire
```

Test paths, **98–168**:

```text
098 T/IndexStateTest.java                                           T/W1: five-state predicate matrix
099 T/LockRegistryTest.java                                         T/W13: old-location deletion
100 T/RecordStoreStateTest.java                                     T/W1: queued-state treatment
101 T/cursors/AsyncLockCursorTest.java                              T/W13: moved helper import
102 T/locking/LockRegistryTest.java                                 T/W13: relocated and expanded contracts
103 T/metadata/MetaDataEvolutionValidatorBuilderTest.java            T/W8: ignored-option builder behavior
104 T/metadata/MetaDataEvolutionValidatorTest.java                   T/W8: renames/swaps/ignored options
105 T/metadata/expressions/CollateFunctionKeyExpressionJRETest.java   T/W16: concrete test extraction
106 T/provider/common/TransformedRecordSerializerTest.java          T/W12: retry cases
107 T/provider/common/text/TextCollatorJRETest.java                  T/W16: concrete test extraction
108 TF/FDBClientKnobTest.java                                       T/W14: names/types
109 TF/FDBDatabaseFactoryImplTest.java                              T/W14: knob validation/lifecycle
110 TF/FDBDatabaseTest.java                                         T/W14/W15: factory/timer behavior
111 TF/FDBRecordContextTest.java                                    T/W15: typed session keys/sets
112 TF/FDBRecordStoreCountRecordsTest.java                          T/W1: predicate substitutions
113 TF/FDBRecordStoreIndexTest.java                                 T/W1/W2/W15: states/routing/session tracking
114 TF/FDBRecordStoreOpeningTest.java                               T/W1/W11: predicates/deletion API
115 TF/FDBRecordStorePerformanceTest.java                           T/W11: deletion API migration
116 TF/FDBRecordStoreRepairHeaderTest.java                          T/W1: maximum-format sentinel
117 TF/FDBRecordStoreReplaceIndexTest.java                          T/W6: replacement lifecycle
118 TF/FDBRecordStoreTest.java                                      T/W11: deletion API migration
119 TF/FDBRecordStoreUniqueIndexTest.java                           T/W1/W2: predicates/queue helper
120 TF/IndexMaintainerFactoryRegistryImplTest.java                   T/W15: duplicate rejection
121 TF/MetaDataProtoEditorUnitTest.java                             T/W8: editor/enum behavior
122 TF/OnlineIndexerBuildIndexTest.java                             T/W1: predicate substitutions
123 TF/OnlineIndexerIndexFromIndexTest.java                         T/W1: predicate substitutions
124 TF/OnlineIndexerMergeTest.java                                  T/W2: heartbeat callbacks
125 TF/OnlineIndexerMultiTargetTest.java                            T/W1/W2: predicates/takeover coverage
126 TF/OnlineIndexerMutualTest.java                                 T/W1: predicate substitutions
127 TF/OnlineIndexerPendingWriteQueueTest.java                      T/W1/W2: full queue/indexer suite
128 TF/OnlineIndexerSimpleTest.java                                 T/W1: predicate substitutions
129 TF/OnlineIndexerTest.java                                       T/W1/W2: helpers/predicates
130 TF/OnlineIndexerUniqueIndexTest.java                            T/W1/W11: predicates/deletion API
131 TF/RecordTypeKeyTest.java                                       T/W1: predicate substitutions
132 TF/VectorIndexScanComparisonsTest.java                          T/W4: return-vectors key
133 TF/cursors/SortCursorTests.java                                 J-only test collection/import replacement
134 TI/GuardiannVectorIndexBackPressureTest.java                     T/W5: capacity behavior
135 TI/GuardiannVectorIndexConcurrentMergeTest.java                  T/W5: concurrent scenario/structure/recall
136 TI/GuardiannVectorIndexMergeTest.java                            T/W5: merge driver
137 TI/GuardiannVectorIndexTest.java                                 T/W4/W5: engine/options
138 TI/HnswVectorIndexIndexingTest.java                              T/W2/W4: online build
139 TI/HnswVectorIndexTest.java                                      T/W4: HNSW/shared-suite refactoring
140 TI/IndexEntryStorageLayoutTest.java                             T/W7/W10: raw/scan entry layouts
141 TI/RankIndexEntryRankTest.java                                  T/W7: rank-valued scan results
142 TI/SlidingWindowIndexMetricsTest.java                           T/W3/W15: metric change
143 TI/SlidingWindowIndexTest.java                                  T/W2/W3: replay/queue/window behavior
144 TI/SlidingWindowTestHelpers.java                                T/W2/W3: queue/window helpers
145 TI/SlidingWindowWithPredicateTest.java                          J-only import reorder
146 TI/TaskEventRegisterTest.java                                   T/W5: callback composition
147 TI/TextIndexTest.java                                           T/W11: deletion API migration
148 TI/ValueIndexMaintainerWithQueue.java                            T/W2: test-only opt-in maintainer
149 TI/VectorIndexEngineTestSuite.java                              T/W2/W4: shared engine/queue contracts
150 TI/VectorIndexMergeLockTest.java                                T/W5: ownership/expiry/delete races
151 TI/VectorIndexOptionKeysTest.java                               T/W4: aliases/catalog
152 TI/VectorIndexScanOptionsTest.java                              T/W4: options/proto duplicates
153 TI/VectorIndexSecondarySubspaceKeysTest.java                     T/W5: key layout
154 TI/VectorIndexTaskCountsTest.java                               T/W5: counters/sparsity/corruption
155 TI/VectorIndexTestBase.java                                     T/W4/W5: common test scaffolding
156 TI/VersionIndexTest.java                                        T/W11: both deletion APIs
157 TF/keyspace/DataInKeySpacePathUtilTest.java                      T/W1: maximum-format sentinel
158 TF/queue/PendingWritesQueueConcurrencyTest.java                  T/W1: concurrent queue behavior
159 TF/queue/PendingWritesQueueSizeTest.java                         T/W1: counter behavior
160 TF/queue/PendingWritesQueueTest.java                             T/W1: payload/order/limits/continuations
161 TF/recordrepair/RecordValidatorTest.java                         T/W1: maximum-format sentinel
162 TF/recordrepair/ScanRecordKeysTest.java                          T/W1: maximum-format sentinel
163 TF/runners/throttled/ThrottledIteratorTest.java                   T/W2: commit-check completion contract
164 TF/storestate/DeleteStoreTest.java                              T/W11: deletion/cache/conflict suite
165 TF/storestate/FDBRecordStoreStateCacheTest.java                  T/W11: deletion-test extraction/refactor
166 TF/storestate/FDBRecordStoreStateCacheTestUtils.java             T/W11: extracted cache helpers
167 TP/pending_writes_queue_test.proto                              T/W1: queue payload fixture
168 TP/test_records_swap.proto                                     T/W8: record-swap fixture
```

Fixture paths, **169–207**:

```text
169 X/TestHelpers.java                                             F-identical
170 X/UnstoredRecord.java                                          F-identical
171 X/metadata/expressions/CollateFunctionKeyExpressionTestBase.java W16: renamed base/local evaluation helper
172 X/metadata/expressions/package-info.java                        W16: package documentation
173 X/package-info.java                                            W16: package documentation
174 X/provider/common/RollingTestKeyManager.java                    F-identical
175 X/provider/common/package-info.java                            W16: package documentation
176 X/provider/common/text/AllSuffixesTextTokenizer.java            F-identical
177 X/provider/common/text/TextCollatorTestBase.java                W16: renamed base/concrete test extraction
178 X/provider/common/text/TextSamples.java                         F-identical
179 X/provider/common/text/package-info.java                        W16: package documentation
180 X/provider/foundationdb/DeleteStoreMode.java                    W11/W16: deletion-mode test helper
181 X/provider/foundationdb/FDBRecordStoreConcurrentTestBase.java    F-identical
182 X/provider/foundationdb/FDBRecordStoreTestBase.java              F-identical
183 X/provider/foundationdb/FormatVersionTestUtils.java              F-identical
184 X/provider/foundationdb/indexes/TextIndexTestUtils.java          F-identical
185 X/provider/foundationdb/indexes/package-info.java                W16: package documentation
186 X/provider/foundationdb/package-info.java                        W16: package documentation
187 X/test/FDBDatabaseExtension.java                                F-identical
188 X/test/TestKeySpace.java                                        F-identical
189 X/test/TestKeySpacePathManager.java                              F-identical
190 X/test/TestKeySpacePathManagerExtension.java                     F-identical
191 X/util/RandomSecretUtil.java                                    F-identical
192 X/util/RandomUtil.java                                          F-identical
193 X/util/package-info.java                                        W16: package documentation
194 XP/test_records_1.proto                                         F-identical
195 XP/test_records_3.proto                                         F-identical
196 XP/test_records_4.proto                                         F-identical
197 XP/test_records_4_wrapper.proto                                 F-identical
198 XP/test_records_5.proto                                         F-identical
199 XP/test_records_bytes.proto                                     F-identical
200 XP/test_records_enum.proto                                      F-identical
201 XP/test_records_grouped_parent_child.proto                       F-identical
202 XP/test_records_join_index.proto                                F-identical
203 XP/test_records_multi.proto                                     F-identical
204 XP/test_records_text.proto                                      F-identical
205 XP/test_records_tuple_fields.proto                              F-identical
206 XP/test_records_with_header.proto                               F-identical
207 XP/test_records_with_union.proto                                F-identical
```

The 29 `F-identical` entries are an exhaustive relocation set, not a sampled classification. The other ten fixture paths are separately classified above.

**5. Prioritized port sequence and dependencies**

| Priority | Concrete work | Coupling |
|---|---|---|
| **Design gate** | Review the upgrade RFC, storage/continuation compatibility, default-format decision, target-source anomalies, and regression expectations. | Required before implementation. Controller’s structured PromoteValue closure remains mandatory. |
| **P0 correctness** | Port tracked-entry sliding-window insertion; correct deletion/cache invalidation; preserve stored queries when protobufs become known. | W3, W11, W9. These are independently actionable after design review and should not wait for a full GuardiANN implementation. |
| **P0 storage safety** | Make vector engine identity govern access; prevent GuardiANN metadata from entering HNSW maintenance. Implement complete engine/options semantics. | W4 and the other population’s GuardiANN storage work. A temporary refusal is not feature completion. |
| **P1 complete lifecycle** | Implement queue storage/payloads, state 4, format support, dispatch, deleteWhere, replay, drain/readable races, overflow, cleanup, and heartbeat integration. | W1/W2, commit hooks, versionstamps, split storage, uniqueness; sliding/vector payloads depend on W3/W4. |
| **P1 major feature** | Implement GuardiANN adapter, persistent accounting, task callbacks, leases/delete guard, bounded merges, backpressure and disable behavior. | W5 plus complete underlying GuardiANN implementation and W2 merge/heartbeat integration. Do not substitute SPFresh. |
| **P1 metadata lifecycle** | Add replacement initialization/retirement; ignored-option API; close rename-identity cases exposed by target regressions. | W6/W8; state transitions and commit checks. |
| **P2 shared query surface** | Add rank-valued scans; stored-query runtime; ArrayAgg and its continuation state; plan/config/UDF/ordinality schema integration. | W7/W9/W10, coordinated with query/relational owners. Preserve allowed Go-only query extensions. |
| **P2 API/lifecycle completeness** | Adapt lock cleanup, client knob configuration/backends, diagnostic sets and metrics. Account explicitly for transformed serializer support and retry semantics. | W12–W15. Java-only deltas require no Go framework. |
| **Verification gate** | Execute the focused contracts above and review every changed expectation individually. | Only after implementation authorization. No blanket golden refresh or parity claim from compilation alone. |

All 207 assigned paths are accounted for. The outstanding work is implementation and regression verification following RFC review; this audit supplies neither a review ACK nor runtime parity certification.