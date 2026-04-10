# fdb-record-layer-go TODO

Actionable work items for the Go port of Apple's FoundationDB Record Layer.
Severity: **CRITICAL** = blocks correctness/compatibility, **HIGH** = important for production quality, **MEDIUM** = improvement, **LOW** = nice-to-have.

Conformance audit performed 2026-03-08 comparing Go implementation method-by-method against Java source at `fdb-record-layer/`. Full API surface review performed 2026-03-16 across 5 areas (store CRUD, indexes, metadata, cursors, DB/context/key expressions).

**Java Record Layer version**: 4.10.6.0 (upgraded from 4.2.6.0 on 2026-03-11). All specs pass (1640 Ginkgo + 70 unit = 1710 total). Java source at `fdb-record-layer/` checked out at tag 4.10.6.0. All 15 proto files synced from Java source.

---

## Bugs

### Correctness audit (2026-04-10) — fdb facade audit

- [x] **HIGH** — `OrEqual` values wrong in `FirstGreaterOrEqual`/`FirstGreaterThan` key selector definitions. Our `range.go` had `FGE={OrEqual:true}` and `FGT={OrEqual:false}`, but the Apple Go binding (and C++ wire protocol) uses the opposite: `FGE={OrEqual:false}`, `FGT={OrEqual:true}`. The Apple C binding does NOT invert OrEqual (confirmed by reading `fdb_c.cpp` source). Root cause was wrong definitions, not a missing inversion. Fixed at the definition level in `range.go` + updated `isTrivialSelector` and `resolveSelector` optimizations. Affected all 4 key selectors through the fdb facade. Found via cross-client interop tests (Go vs CGo). 5 new interop tests added.

### Correctness audit (2026-04-09) — C++ alignment sweep

- [x] **HIGH** — `OnError()` missing 5 retryable error codes. `tag_throttled` (1213), `proxy_tag_throttled` (1223), `transaction_throttled_hot_shard` (1235), `transaction_rejected_range_locked` (1242) fell through to non-retryable default. `cluster_version_changed` (1039) was not handled as MAYBE_COMMITTED (should inject self-conflicts like 1021). Fixed: all 5 codes added to OnError switch, 1039 gets self-conflicting treatment. `wire.FDBError.Retryable()` also updated (had wrong comment: 1039 labeled as `database_locked`, missing 1038/1078/1223/1235/1242). Test added for 1039 self-conflicting.
- [x] **MEDIUM** — `AddReadConflictRange()` missing defensive copies + mutex. Unlike `addReadConflict()` (internal, copies), `AddWriteConflictRange()` (public, delegates to copier), and `addWriteConflict()` (internal, copies), the public `AddReadConflictRange()` stored raw caller slices. Aliasing risk on slice reuse. Fixed: delegate to `addReadConflict()` which copies and locks.
- [x] **MEDIUM** — RYW `getRange` discarded server `more` flag. When clears removed enough server results to bring count below limit, the function returned `more=false` even though the server had more data. Callers (e.g. ranked set traversal) would stop scanning prematurely. Fixed: propagate server `more` flag.

- [x] **HIGH** — Align location cache with C++ `getKeyRangeLocations`. `locateRange()` fetches all overlapping shards at once. `getRange` matches C++ `getExactRange`: `Reverse` flag on `GetKeyServerLocationsRequest`, re-query same shard on `more=true` (no re-locate), `invalidateRange()` clears entire remaining range on `wrong_shard_server`, "fix more" heuristic for reverse shard boundary, zero-rows infinite loop guard.
- [x] **HIGH** — ConnectPacket TCP port reuse ASSERT in CI. FDB server asserts at `FlowTransport.actor.cpp:1569` when a new TCP connection arrives from the same ephemeral port as a recently closed connection (stale Peer entry). Observed in CI under Docker load. Two-part fix: (1) `SetLinger(0)` on TCP sockets — sends RST instead of FIN on close, eliminating TIME_WAIT state and causing immediate Peer cleanup on the server. (2) TCP keepalive (10s interval) for faster dead connection detection under Docker/socat load. The original port=0 fix (sending `CanonicalRemotePort: 0` matching C++ pure-client behavior) was already correct; this addresses the remaining race from ephemeral port reuse.
- [x] **CRITICAL (RESOLVED)** — Pure Go FDB client wire protocol crashed FDB server (SIGSEGV). Three root causes found and fixed. See `pkg/fdbgo/client/CRASH_BUG.md` for full analysis + debugging playbook.

  **Serialization bugs (all fixed):**
  - [x] Bug 1: Nested struct serialization order reversed vs C++.
  - [x] Bug 2: `Optional<T>` fields completely skipped in marshal path.
  - [x] Bug 3: Empty `dynamic_size` fields allocated 0 bytes instead of 4.
  - [x] Bug 4: Generated nil-guards skipped fields C++ always serializes.
  - [x] Bug 5: `Optional<ReadOptions>` — extractor detects struct inner type.
  - [x] Bug 6: `Vector<struct>` — two-pass precomputeSize/writeToBuffer.
  - [x] Bug 7: Field serialize order + KeyRangeRef equalsKeyAfter optimization.

  **Client logic bugs that crashed FDB server (all fixed):**
  - [x] Bug A: Reverse range scan located shards by `begin` key. `\xff\xff` sent to storage server → `getShardKeyRange()` SIGSEGV. Fix: locate by `end` for reverse, clamp to shard boundaries.
  - [x] Bug B: `ClearRange`/`Add*ConflictRange` didn't validate `begin <= end`. Inverted ranges sent to server. Fix: return error 2005 client-side.
  - [x] Bug C: `\xff\xff` system key reads added resolver conflict ranges. Commit proxy `ASSERT(resolvers.size())` failed. Fix: skip conflict ranges for system keys.

  **Ground truth sizes: 10/10 match.** Byte diffs = reply token only (expected). Binding tester: **145 seeds × 1000 ops = 0 failures, 0 FDB deaths.**

  **Remaining (LOW):**
  - [ ] C++ `emptyVector` re-use optimization missing — no test vector currently fails from this.
- [x] **CRITICAL** — `metadata.go`: `Build()` never computes `primaryKeyComponentPositions` for multi-type indexes (`rt.multiTypeIndexes`). Fixed: added loop over `rt.multiTypeIndexes` matching single-type pattern. 2 regression tests. Found 2026-03-26 via 10-agent audit.
- [x] **~~HIGH~~** — `cursor_combinators.go:578-586`: `FlatMapPipelinedCursor` priorOuterCont nil on first outer value — **FALSE ALARM**. `priorOuterCont=nil` correctly means "outer started from beginning." On resume, `outerFactory(nil)` restarts outer, `hasPending=true` causes first outer value to be consumed (not emitted) while inner resumes from saved continuation. No duplicates occur. Verified by detailed trace-through. Found+dismissed 2026-03-26.
- [x] **HIGH** — Missing cross-cutting test matrix. Fixed: `index_registration_matrix_test.go` with 3×7 matrix (21 specs, 1 skipped). **Found and fixed another bug**: `DeleteRecordsWhere` fully cleared multi-type indexes instead of scoping by PK prefix. Added `hasRecordTypeKeyPrefix()` helper + scoped clear for multi-type indexes with RecordTypeKey prefix, error for multi-type without. Matches Java's `canDeleteWhereForIndexOnStoredTypes`.
- [x] **HIGH** — `store_delete_where.go`: `DeleteRecordsWhere` fully cleared multi-type indexes (destroyed non-target type entries) instead of scoping by PK prefix. Fixed: multi-type with RecordTypeKey prefix → scoped clear, without → error. Found 2026-03-26 by test matrix.
- [x] **HIGH** — CI red: SIFT/vector benchmark test files extracted to `pkg/recordlayer/bench/` with own BUILD.bazel. gofmt fixed. Commits `d1d632a`, `3914be8`, `23c4ff7`, `4239e55`.

### Cursor/continuation audit (2026-03-28) — correctness sweep

- [x] **CRITICAL** — `record_key_cursor.go:initIterator()`: Reverse scan continuation always adjusts `begin` instead of `end`. Fixed: check `IsReverse()`, adjust `end` for reverse scans. Regression test added.
- [x] **CRITICAL** — `record_key_cursor.go:111`: Split record continuation + nil `lastPK` on resume → duplicates. Fixed: initialize `lastPK` from continuation on resume. Regression test added.
- [x] **MEDIUM** — `index_scan.go:indexCursor.OnNext()`: Missing `ScannedRecordsLimit` check. Fixed: added check matching `keyValueCursor`/`recordKeyCursor` pattern. Regression test added.

### Index maintenance UPDATE audit (2026-03-28)

- [x] **HIGH** — `text_index_maintainer.go:removeCommonTextEntries()`: Only compared text value at `textPos`, ignoring grouping columns. If group changes but text stays the same, tokens remain in old group's subspace (phantom entries) and are never written to new group (missing entries). Fixed: compare ALL columns via tuple packing.

### OnlineIndexer audit (2026-03-28)

- [x] **LOW** — `index_state.go:clearReadableIndexBuildData()`: Now clears both RangeSet and heartbeats, matching Java's `clearReadableIndexBuildData()` which calls `IndexingHeartbeat.clearAllHeartbeats()`. Prevents stale heartbeats from crashed mutual builders accumulating.
- [x] **MEDIUM** — `indexing_mutual.go:301-314`: Shard boundary bytes that aren't valid tuples cause nil rangeStart/rangeEnd → full-keyspace scan. Fixed: return error instead of silently falling back to nil (matches Java which throws on invalid tuple bytes).

### SaveRecord behavioral audit (2026-03-28) — Java vs Go comparison

- [x] **MEDIUM** — `store.go`: SizeInfo missing version key/value metrics. Fixed: `saveRecordVersion()` now updates sizeInfo with version key/value bytes (KeyCount, KeySize, ValueSize). Incomplete versions subtract 4-byte offset (not durable). `DryRunSaveRecord` also includes version bytes. `loadWithSplit` tracks version keys in split record scan. Matches Java's `SplitHelper.writeVersion()` sizeInfo pattern.
- [x] **LOW** — `store.go`: Fixed double deserialization when `ErrorIfTypeChanged + HasIndexes`. Type check now caches deserialized old record (`cachedOldRT`/`cachedOldMsg`) for reuse by index update. Saves one proto unmarshal per update.

### Hardening audit (2026-03-29) — 5-agent sweep: corruption masking, panics, integer overflow

- [x] **MEDIUM** — `text_index_serializer.go:156,172,188`: `SerializeEntry`/`SerializeEntries` panicked on invalid position lists or empty entries. Fixed: `BunchedSerializer` interface changed to return `([]byte, error)`, all `BunchedMap` callers propagate errors.
- [x] **MEDIUM** — `text_tokenizer.go:360`: `GetTextTokenizer()` panicked on unregistered tokenizer name. Fixed: returns `(TextTokenizer, error)`.
- [x] **MEDIUM** — Integer overflow in `limit + 1` arithmetic across 8 sites (key_value_cursor, online_indexer ×2, indexing_mutual, text_index_maintainer, count_index/index_scan/bitmap_value cursors). Added `saturatingAdd()` helper clamped to `math.MaxInt`. Also fixed subtraction underflow (returned > limit → negative FDB limit treated as unlimited).

### Hardening audit (2026-03-28) — 4-agent sweep: error swallowing, panics, deserialization, concurrency

- [x] **CRITICAL** — `hnsw.go`: 6+ sites ignore `decodeStoredVector()` error (lines ~319, 724, 731, 870, 880, 911). Fixed: critical paths return error, loop candidates skip on decode failure.
- [x] **CRITICAL** — `text_index_maintainer.go:115`: `MustGet()` panics on FDB transaction error. Fixed: returns `(int, error)`, callers propagate.
- [x] **HIGH** — `tuple_ordering.go:tupleElementEndPos()`: Returns positions beyond `len(data)` for fixed-size types. Fixed: bounds checks on all fixed-size types (int, bigint, float32/64, UUID, versionstamp).
- [x] **HIGH** — `key_value_cursor.go:237`: `fastUnpack` error silently ignored on `pendingVersionPK`. Fixed: propagates error.
- [x] **HIGH** — `key_value_cursor.go:326`: `nextKV()` error ignored in `peekVersionKey()`. Fixed: returns `(*FDBRecordVersion, error)`, caller propagates.
- [x] **HIGH** — `rtree_types.go:149,152`: `asInt64()` errors ignored in `MBRFromTuple()`. Fixed: returns `(MBR, error)`, caller propagates.
- [x] **HIGH** — `text_index_maintainer.go:82`: `getTextTokenizerVersion()` panics on non-integer tokenizer version string. Fixed: returns `(int, error)`.
- [x] **HIGH** — `text_tokenizer.go:144`: `Tokenize()` panics on invalid tokenizer version. Fixed: `TextTokenizer` interface methods now return errors, all callers updated.
- [x] **MEDIUM** — `key_expression_proto.go:302` + `key_expression.go:621-622`: `valueToProto()` errors swallowed. Fixed: `Literal()` constructor validates type at build time — unsupported types panic immediately.

- [x] **MEDIUM** — `ranked_set.go:570`: `rsDecodeLong()` returned 0 for short values instead of erroring. Fixed: nil → 0 (legitimate missing key), non-nil short → error (data corruption). All 8 callers updated to propagate errors.
- [x] **MEDIUM** — `count_index_maintainer.go:187`: Count value returned 0 for values shorter than 8 bytes instead of erroring. Fixed: non-empty short values now return error.

---

## Investigate

- [x] **MEDIUM** — Package structure: investigated in RFC 004 (rejected multi-package split due to irreducible type cycle). Staying flat + nogo layering enforcement. See `rfcs/004-package-structure-investigation.md`.
- [x] **HIGH** — `index_scan.go:250`: `keyExpressionColumnSize()` panic eliminated. Added `ColumnSize() int` to `KeyExpression` interface (matches Java's `getColumnSize()`), implemented on all 12 expression types, replaced all ~23 callsites, deleted both `keyExpressionColumnSize` and `keyExpressionColumnSizeChecked`.
- [ ] **LOW** — `cursor.go:114`: `GetValue()` panics if called without `HasNext()`. Matches Java's `IllegalResultValueAccessException`. Acceptable precondition — document clearly.
- [ ] **LOW** — `split_key_expression.go:29`: `Split()` constructor panics on `splitSize <= 0`. Acceptable build-time validation — programming error caught early.

---

## 4.10.6.0 upgrade assessment

Upgraded from 4.2.6.0 → 4.10.6.0 (2026-03-11). 548 commits across 8 minor versions. All 1012 conformance+unit tests pass unchanged. All 15 proto files synced from Java source. Below is a thorough analysis of all changes, organized by priority.

### 1. Wire format / storage changes (MUST address for compatibility)

#### 1a. New FormatVersions (8–14)

Java added 7 new format versions. We must handle them correctly on open/create:

| FmtVer | Name | Feature | Priority |
|--------|------|---------|----------|
| 8 | HEADER_USER_FIELDS | `DataStoreInfo.user_field` — user-defined key→bytes map in store header | **MEDIUM** |
| 9 | READABLE_UNIQUE_PENDING | New `IndexState` for unique indexes with pending violations | **HIGH** |
| 10 | CHECK_INDEX_BUILD_TYPE_DURING_UPDATE | Non-idempotent index build-from-source validation | **DONE** |
| 11 | RECORD_COUNT_STATE | `DataStoreInfo.record_count_state` enum (READABLE/WRITE_ONLY/DISABLED) | **DONE** (already implemented) |
| 12 | STORE_LOCK_STATE | `DataStoreInfo.store_lock_state` with FORBID_RECORD_UPDATE + FULL_STORE | **HIGH** |
| 13 | INCARNATION | `DataStoreInfo.incarnation` (int32) for cross-cluster migration | **MEDIUM** |
| 14 | FULL_STORE_LOCK | Unknown lock states now prevent store opening (stricter validation) | **HIGH** |

- [x] **FULL_STORE lock state + stricter validation (FormatVersion 12+14)** — Implemented: `validateStoreLockState()` on open, `StoreIsFullyLockedError`, `UnknownStoreLockStateError`, `SetBypassFullStoreLockReason()` on builder. `FormatVersionCurrent` bumped to 14. 5 new tests (prevents Open/CreateOrOpen, bypass with matching/wrong reason, clear lock). **HIGH**.
- [x] **READABLE_UNIQUE_PENDING index state (FormatVersion 9)** — Full behavioral parity with Java: `MarkIndexReadable` checks `firstUnbuiltRange` + rejects unique violations, `MarkIndexReadableOrUniquePending` transitions to READABLE_UNIQUE_PENDING when violations exist, `OnlineIndexer` uses the unique-pending variant, build data cleared on READABLE but retained for READABLE_UNIQUE_PENDING. 15 new tests. **HIGH**.
- [x] **Store incarnation field (FormatVersion 13)** — Implemented: `GetIncarnation()`, `UpdateIncarnation(updater)` (must strictly increase). `get_versionstamp_incarnation()` now available via `FunctionKeyExpression`. **MEDIUM**.
- [x] **Header user fields (FormatVersion 8)** — Implemented: `GetHeaderUserField(key)`, `SetHeaderUserField(key, value)`, `ClearHeaderUserField(key)`. **MEDIUM**.
- [ ] **Continuation serialization evolution** — 4.5.x enabled proto-wrapped `AggregateCursorContinuation`. 4.8.x enabled new `KeyValueCursorBaseContinuation` serialization. Our TO_OLD format still works (confirmed by conformance tests). No action needed unless we add aggregate cursors. **LOW**.

#### 1b. Store header proto changes (DataStoreInfo)

New fields in wire format (all optional, safe to round-trip via protobuf):
- `omit_unsplit_record_suffix` (field 6, bool) — already respected in our split logic
- `cacheable` (field 7, bool) — for `MetaDataVersionStampStoreStateCache`
- `user_field` (field 8, repeated UserFieldEntry) — see above
- `record_count_state` (field 9, enum) — **DONE**
- `store_lock_state` (field 10, StoreLockState) — see above
- `incarnation` (field 11, int32) — see above

#### 1c. Subspace layout

**UNCHANGED.** Still 10 subspaces (0–9). No new subspace constants added.

#### 1d. Split records / index entries

**UNCHANGED.** SPLIT_RECORD_SIZE=100KB, UNSPLIT_RECORD=0, START_SPLIT_RECORD=1, RECORD_VERSION=-1. Index entry format unchanged (key=[indexValues..., trimmedPK...], value=empty tuple or tuple-packed for covering).

### 2. New index types (not yet in Go)

| Type | Maintainer | Mutation/Storage | Priority | Notes |
|------|-----------|-----------------|----------|-------|
| TEXT | `TextIndexMaintainer` | BunchedMap token storage | **LOW** | Full-text search with pluggable tokenizers |
| BITMAP_VALUE | `BitmapValueIndexMaintainer` | Position bitmaps (10K–250K bits per entry) | **DONE** | 27 unit + 6 conformance |
| PERMUTED_MIN | `PermutedMinMaxIndexMaintainer` | Permuted grouping columns for value-ordered min | **LOW** | Enumerate extrema by value, not group |
| PERMUTED_MAX | `PermutedMinMaxIndexMaintainer` | Same, max variant | **LOW** | Same as above |
| MAX_EVER_VERSION | `AtomicMutationIndexMaintainer` | SET_VERSIONSTAMPED_VALUE | **MEDIUM** | Like MAX_EVER_TUPLE but version-aware |
| MULTIDIMENSIONAL | `MultidimensionalIndexMaintainer` | Hilbert R-tree spatial indexing | **DONE** | 16 tests |
| VECTOR | `VectorIndexMaintainer` | HNSW graph for similarity search | **DONE** | 16 tests |
| TIME_WINDOW_LEADERBOARD | `TimeWindowLeaderboardIndexMaintainer` | Time-windowed ranked sets | **DONE** | 22 tests |

- [x] **MAX_EVER_VERSION index** — `MaxEverVersionIndexMaintainer` with dual mutation path: `SET_VERSIONSTAMPED_VALUE` (incomplete, with merge function keeping max local version) + `BYTE_MAX` (complete). `UpdateVersionMutation` added to context with merge function support. Metadata validation: GroupingKeyExpression required, exactly 1 VersionKeyExpression in grouped portion, storeRecordVersions required. Aggregate function support via `FunctionNameMaxEver`/`IndexTypeMaxEverVersion`. 18 tests. **MEDIUM**.
- [x] **BITMAP_VALUE index** — `bitmapValueIndexMaintainer` with FDB atomic BIT_OR (insert) / BIT_AND + CompareAndClear (delete). Position-aligned bitmaps with configurable entrySize (default 10K, max 250K). BY_GROUP scan with position trimming for non-aligned ranges. Unique index enforcement via snapshot read + conflict keys. BITMAP_VALUE aggregate function. Custom `bitmapKVCursor` (raw bytes, not tuple-packed values). 27 unit tests + 6 conformance specs.
- [x] **TEXT index** — `textIndexMaintainer` with BunchedMap for token→position list storage. `TextIndexBunchedSerializer` with wire-compatible base-128 varint + delta compression (prefix 0x20). `DefaultTextTokenizer` with UAX #29 word segmentation (via `rivo/uniseg`), NFKD normalization, case folding, diacritical removal. `TextTokenizerRegistry` with factory pattern. BY_TEXT_TOKEN scan type via `BunchedMapMultiIterator` + `TextCursor` with time/record scan limits. `EndpointTypePrefixString` for prefix token searches. Tokenizer version tracking per record in secondary subspace. DeleteWhere with PrefixRange + skip handling in Scan. 115 unit tests + 34 integration tests + 7 conformance specs.
- [x] **PERMUTED_MIN/MAX indexes** — `permutedMinMaxIndexMaintainer` with dual subspace: primary VALUE index at IndexKey(2) + permuted entries at IndexSecondarySpaceKey(3). Permuted key reorders trailing grouping columns after the value for value-ordered scans. BY_VALUE scans primary, BY_GROUP scans permuted. Delete re-fetches extremum from primary. Aggregate function support via `FunctionNameMin`/`FunctionNameMax`. **Bug fixed by chaos testing**: UPDATE path didn't handle group membership changes (stale permuted entries). Decomposed into insert/remove helpers. 12 unit tests + 4 chaos random tests.
- [x] **TIME_WINDOW_LEADERBOARD index** — `timeWindowLeaderboardIndexMaintainer` with directory management, per-group sub-directory, multiple ranked sets per time window, PerformWindowUpdate operation, BY_TIME_WINDOW/BY_RANK/BY_VALUE scans, score negation for highScoreFirst, atomic MAX timestamp tracking. Wire-compatible with Java. 22 tests.
- [x] **MULTIDIMENSIONAL index** — Hilbert R-tree spatial indexing. `rtree.go` (insert/delete/scan with overflow/underflow), `rtree_hilbert.go` (N-dimensional Hilbert curve), `rtree_storage.go` (BY_NODE FDB serialization), `rtree_types.go` (Point/MBR/ItemSlot/ChildSlot), `dimensions_key_expression.go` (prefix/dimensions/suffix splitting). 16 tests.
- [x] **VECTOR/HNSW index** — `hnswGraph` with probabilistic multi-layer insert, greedy kNN search, delete with neighbor cleanup. 3 distance metrics (Euclidean, Cosine, InnerProduct). `vectorIndexMaintainer` with `SearchVectorIndex`/`SearchVectorIndexRecords` store APIs. 16 tests.

### 2a. Post-audit fixes for new index types (2026-03-19 audit)

#### TIME_WINDOW_LEADERBOARD — wire-compatible, needs correctness fixes

- [x] **CRITICAL — `PerformWindowUpdate` rebuild is broken** — Fixed: accepts `*FDBRecordStore`, calls `store.RebuildIndex(index)` after DeleteWhere. Matches Java's `UpdateState.save()`.
- [x] **HIGH — `negateScore` overflows at `math.MinInt64`** — Fixed: detects MinInt64, returns `big.Int` matching Java's `TupleHelpers.negate()`. Also handles `*big.Int` → `int64` normalization.
- [x] **HIGH — `negateScoreRange` boundary `<=` vs `<`** — Fixed: changed to `<` matching Java.
- [x] **HIGH — `highScoreFirst` scan checks only low bound** — Fixed: checks both low and high group tuples, falls back to directory default when groups differ. BY_RANK always false.
- [x] **HIGH — `Rebuild.NEVER` + highScoreFirst change should error** — Fixed: returns error matching Java's `RecordCoreException`.
- [x] **HIGH — Missing `evaluateRecordFunction`** — Implemented: RANK (all-time), TIME_WINDOW_RANK. `timeWindowRank()` evaluates entries, finds best contained score, looks up rank in per-window ranked set.
- [x] **HIGH — Missing `evaluateAggregateFunction`** — Implemented: TIME_WINDOW_COUNT (ranked set size), SCORE_FOR_TIME_WINDOW_RANK/ELSE_SKIP (GetNth + un-negate), TIME_WINDOW_RANK_FOR_SCORE (negate + Rank). Wired into canEvaluateAggregate dispatch.
- [x] **MEDIUM — `Rebuild.IF_OVERLAPPING_CHANGED` misses all-time addendum** — Fixed: triggers rebuild on initial directory creation and all-time addition.
- [x] **MEDIUM — Missing `SaveSubDirectory`** — Implemented: `SaveSubDirectory(group, highScoreFirst)` on maintainer. 2 tests.
- [x] **MEDIUM — Silent error swallowing in `newLeaderboardDirectoryFromProto`** — Fixed: returns error on corrupt SubspaceKey.
- [x] **HIGH — No chaos testing** — 15 chaos tests: basic, commit-unknown (insert/overwrite/delete), duplicate scores, multiple windows, highScoreFirst, random+heavy stress (200-300 ops, 5-20% fault rate), all fault types.
- [x] **HIGH — No conformance tests** — 11 conformance specs: Go/Java writes+scan, mixed writes, cross-language delete (2), rank, score update, highScoreFirst wire compat, bounded window filtering, Go-creates-windows-Java-reads, BY_RANK cross-language.
- [x] **HIGH — No OnlineIndexer test** — 2 tests: full build, chunked build with small limit.
- [x] **HIGH — No RebuildIndex test** — 2 tests: explicit rebuild, PerformWindowUpdate ALWAYS rebuild.

#### MULTIDIMENSIONAL — wire-compatible, 5-reviewer audit complete

- [x] **CRITICAL — Node serialization format incompatible** — Fixed: nested list format `(kind, [slot1, slot2, ...])` matching Java's `ByNodeStorageAdapter`. `tuple.getNestedList(1)` compatible.
- [x] **CRITICAL — Intermediate node overflow not handled** — Fixed: cascading `handleIntermediateOverflow()` with `splitRootIntermediate()` and `overflowIntermediate()`. Redistributes child slots among siblings, creates new sibling when all at MaxM.
- [x] **CRITICAL — Intermediate node underflow not handled** — Fixed: cascading `handleIntermediateUnderflow()` with `promoteOnlyChild()` and `fuseIntermediate()`. Merges siblings when all at MinM.
- [x] **HIGH — `propagateMBRUp` incomplete** — Fixed: propagates through ALL intermediate levels. Higher levels updated via `childSlotForIntermediate()`.
- [x] **HIGH — No prefix skip-scan in maintainer** — Fixed: `Scan()` extracts prefix from scanRange, scopes R-tree subspace per prefix.
- [x] **HIGH — Continuation tokens incompatible** — Fixed: `MultidimensionalIndexScanContinuation` proto with `lastHilbertValue` + `lastKey`. Wire-compatible with Java.
- [x] **HIGH — Scan loads everything into memory** — Fixed: row limit support via `ReturnedRowLimit`. Still materializes in-memory but respects limits with proper continuation.
- [x] **HIGH — ItemSlot value double-wrapped** — Fixed: `slot.Value` stored directly (not wrapped in extra tuple).
- [x] **MEDIUM — No `removeCommonEntries` optimization** — Fixed: Update() now calls `removeCommonEntries()` to skip identical entries between old and new records.
- [x] **MEDIUM — Silent deserialization failures** — Fixed: all deserialization paths return typed errors.
- [x] **MEDIUM — `compareHilbertValueAndKey` panics on nil BigInt** — Fixed: nil guards (both nil → tupleCompare, one nil sorts before non-nil).
- [x] **CRITICAL — Zero test coverage on split/fuse** — Fixed: 8 new tests with MaxM=4 (25-60 items) exercising leaf split, intermediate overflow, deep trees, underflow/fuse, MBR predicates, scan continuation, full lifecycle, and maintainer integration.
- [x] **HIGH — No conformance tests** — 6 specs: Go writes/Java scans, Java writes/Go scans, mixed writes, cross-language delete (2), coordinate update. Wire format cross-validated with `MultidimensionalIndexScanBounds`.
- [x] **HIGH — No chaos testing** — 5 chaos tests: basic save, commit-unknown (insert/overwrite/delete), random stress (150 ops, 5% fault rate). Model-based verification computes expected entries from model, scans R-tree, set-based diff.
- [x] **Bug — Overflow/underflow re-fetched stale sibling from FDB** — Fixed: in-memory modified node substituted for its re-fetched copy in all overflow/underflow paths.

5-reviewer audit (2026-03-19) found and fixed 19 additional issues:
- [x] **CRITICAL — Scan MBR predicate at wrong level** — Removed per-item filtering, only child slots.
- [x] **CRITICAL — Delete path walks wrong subtree** — `fetchUpdatePathToLeaf(isInsert)` differentiates insert/delete.
- [x] **CRITICAL — Option constants wrong** — `rtreeMaxM`→`rtreeMaximumM`, `rtreeMinM`→`rtreeMinimumM`.
- [x] **CRITICAL — getDimensionsExpression misses KeyWithValueExpression** — Traverses KWV + Composite wrappers.
- [x] **CRITICAL — numDimensions defaults to 2 on KWV-wrapped index** — Uses `extractDimensionsExpression`.
- [x] **HIGH — Config validation** — `ValidateRTreeConfig` with split ratio constraint.
- [x] **HIGH — Hilbert value continuation bytes** — Two's complement + HV==0 handling.
- [x] **HIGH — ChildSlot HV deserialization drops int64-range values** — Added `int64` case.
- [x] **HIGH — splitRootLeaf slice aliasing** — Deep-copy left/right slots.
- [x] **HIGH — gatherSiblings swallows FDB errors** — Returns errors now.
- [x] **HIGH — Missing storeHilbertValues option** — Parsed from index options.
- [x] **MEDIUM — createsDuplicates missing DimensionsKE case** — Delegates to WholeKey.
- [x] **MEDIUM — normalizeKeyForPositions missing DimensionsKE case** — Delegates to WholeKey.
- [x] **MEDIUM — Empty nodes written instead of deleted** — `writeLeafNode`/`writeIntermediateNode` check.
- [x] **MEDIUM — clearAll ignores PrefixRange error** — Returns error.
- [x] **MEDIUM — HV==0 continuation round-trip** — Writes `[0x00]` instead of empty.
- [x] **LOW — Dead coords variable** — Removed.
- [x] **LOW — Continuation unmarshal errors swallowed** — Returns error cursor.
- [x] **LOW — promoteOnlyChild missing child error** — Returns error.
- 9 new tests: continuation round-trip, row limits, negative/boundary coords, duplicate coords, DeleteAllRecords, RebuildIndex, config validation, tree height transitions, 3D R-tree.
- [x] **MEDIUM — Scan materializes all results** — Fixed: `RTreeIterator` fetches one leaf at a time via explicit stack. `rtreeScanCursor` wraps iterator directly.
- [x] **MEDIUM — No spatial predicate support** — Fixed: `buildMBRPredicate()` extracts dimensional bounds from scanRange, passes to iterator for subtree pruning.
- [x] **MEDIUM — No prefix skip-scan across all prefixes** — Fixed: `prefixSkipScanCursor` enumerates distinct prefixes via FDB key reads + `fdb.Strinc()`. Cross-prefix continuation deferred (proto lacks prefix field).
- [x] **LOW — `propagateMBRUp` always writes parent nodes** — Fixed: compares old/new ChildSlot via `childSlotEqual`, only writes if changed, stops propagation early. Matches Java's `adjustSlotInParent`.
- [x] **HIGH — Cross-language continuation format** — Fixed: Go now wraps `MultidimensionalIndexScanContinuation` inside `FlatMapContinuation` proto, matching Java's `flatMapPipelined` cursor composition. Backward-compatible: reads both `FlatMapContinuation` (Java) and raw format (old Go).

#### VECTOR/HNSW — wire-compatible, needs conformance + additional features

- [x] **CRITICAL — Wire format completely incompatible** — Fixed: per-layer key `(layer, PK)`, COMPACT value `(kind, vectorTuple, neighborsTuple)` matching Java's `CompactStorageAdapter`. Vector serialization: type byte + big-endian float64. Access info subspace for entry point.
- [x] **CRITICAL — Layer assignment non-deterministic** — Fixed: `topLayer(primaryKey, m)` using `splitMixDouble(javaHashCode(pk.Pack()))`. Deterministic per PK, matching Java's `Primitives.topLayer()`.
- [x] **CRITICAL — Delete does NOT repair graph** — Fixed: multi-phase repair via `repairNeighbor()`. Finds candidates from neighbors-of-neighbors, selects best by distance, respects M/MMax limits. Entry point promotion on delete.
- [x] **HIGH — `randomLevel()` can return MaxInt** — Fixed: replaced with `topLayer()` which uses `math.Floor(-math.Log(u) * lambda)` with clamped input (u = 1.0 - splitMixDouble, always > 0).
- [x] **HIGH — No duplicate detection on insert** — Fixed: checks layer 0 existence before inserting.
- [x] **HIGH — Missing prefix partitioning** — Fixed: per-prefix HNSW graphs via `getSubspaceForPrefix()`. `ScanVectorIndexWithPrefix`/`SearchVectorIndexWithPrefix` APIs. 10 tests including cross-group isolation, update between groups, 5-group stress.
- [x] **HIGH — Missing BY_DISTANCE scan type** — Implemented: `ScanVectorIndex()`, `ScanIndexByType(BY_DISTANCE)`, `VectorDistanceScanRange()`. Returns kNN results as cursor with distance in Value. 7 tests.
- [x] **HIGH — Missing write locks** — Implemented in RFC 008: `lockRegistry` on `FDBRecordContext` provides per-subspace RW locks matching Java's `LockRegistry`. `vectorIndexMaintainer.Update()` acquires write lock, `SearchKNN()` acquires read lock. Prevents intra-transaction graph corruption when multiple goroutines share one store.
- [x] **HIGH — Missing Config validation** — Fixed: validates numDimensions >= 1, m in [4,200], mMax in [4,200], mMax0 in [4,300], efConstruction in [100,400].
- [x] **MEDIUM — Only float64 vectors** — Fixed: `deserializeVector` now handles type 0 (DOUBLE/float64), type 1 (SINGLE/float32), type 2 (HALF/float16). `halfToFloat32` implements IEEE 754 half-precision conversion. Go writes DOUBLE; reads all three types for Java interop. 3 tests.
- [x] **MEDIUM — Missing extended neighbor selection heuristic** — Fixed: Algorithm 4 from HNSW paper. `selectNeighbors` uses diversity heuristic for Euclidean (satisfies triangle inequality), simple sort for Cosine/InnerProduct (matching Java's `Primitives.selectCandidates`). `extendCandidates` explores 2nd-degree neighbors. `keepPrunedConnections` fills up to M from discarded. `hnswExtendCandidates`/`hnswKeepPrunedConnections` index options. 9 tests.
- [x] **MEDIUM — Cosine distance can return negative** — Fixed: clamp similarity to [-1, 1] before computing 1-sim. 3 clamping tests.
- [x] **MEDIUM — `vectorIndexMaintainer.Update` creates new graph per entry** — Fixed: single graph instance per maintainer, no PRNG reset.
- [x] **LOW — Missing RaBitQ quantization** — Integrated RaBitQ with HNSW: `UseRaBitQ`/`RaBitQNumExBits` config, quantized storage, `computeDistance` for approximate search, `decodeStoredVector` for heuristic pairwise distances. 12 tests.
- [x] **HIGH — No search quality/recall test** — Fixed: 100 random 8D vectors, brute-force comparison, asserts >= 80% recall for k=10.
- [x] **HIGH — No conformance tests** — 11 specs: Go saves→Java reads/saves more, Java saves→Go reads/saves more, cross-language mixed writes, delete cross-language, batch operations, record counting. Found+fixed 6 wire-format bugs: option names (hnsw* not vector*), metric enum values, node key nesting, access info 5-element format, HNSW subspace (primary not secondary), vector bytes extraction.
- [x] **HIGH — No chaos testing** — 5 chaos tests: basic save, commit-unknown (insert/overwrite/delete), random stress (100 ops, 5% fault rate). Model-based verification: count, self-search, orphan check.
- [x] **HIGH — No high-dimensional vector tests** — Fixed: 50 random 128D vectors, search + distance verification.
- [x] **HIGH — Sequential FDB reads in HNSW search/insert** — Fixed: `loadNodeLayerBatch` pipelines FDB futures (fire all `tx.Get()` before resolving). 2.1x search speedup (16→34 QPS). Transaction-local node cache added. Remaining gap vs Qdrant (19x) is inherent to FDB's network model.

#### VECTOR/HNSW — Java alignment gaps + optimization roadmap (RFC 007)

SIFT-1M benchmark: recall@10=0.998 (excellent), 34 QPS (19x slower than Qdrant). See `rfcs/007-hnsw-performance-optimizations.md` for full analysis.

| # | What | Impact | Category | Status |
|---|---|---|---|---|
| 1 | Inlining storage adapter (upper layers: range-read vs N point-reads) | HIGH perf | Performance | [x] UseInlining config, dispatch by layer |
| 2 | RaBitQ FHT-KAC rotation (centroid bootstrapping deferred) | MEDIUM-HIGH quality | Correctness | [x] FHT-KAC rotator, Java Random compat, transform pipeline, access info wire format |
| 3 | Delete repair sampling (efRepair=64 limit) | MEDIUM perf | Performance | [x] |
| 4 | Dual priority queues (max-heap for results) | LOW perf | Performance | [x] binary-insert |
| 5 | Parallel existence + access info fetch on insert | LOW perf | Performance | [x] |
| 6 | Ring search + outward traversal iterator (BY_DISTANCE pagination) | Feature gap | Feature | [x] vectorSearchCursor with distance-based continuation |
| 7 | Concurrent multi-layer deletion | LOW perf | Performance | [x] pipelined layer reads via FDB futures |
| 8 | ChangeSet incremental writes (needs #1) | LOW perf | Performance | [x] inlining uses per-edge KVs (inherent) |
| 9 | Snapshot reads for sampled vectors (needs #2) | LOW perf | Performance | [x] centroid deferred; search uses tx.Snapshot() (#13) |
| 10 | Configurable fetch limits (maxConcurrentNodeFetches etc.) | LOW perf | Config | [x] 3 Java config fields parsed + round-tripped |

Additional Go-specific optimizations (from RFC 007):

| # | What | Impact | Status |
|---|---|---|---|
| 11 | Persist hnswStorage cache across ops in same transaction | 30-50% batch insert | [x] |
| 12 | Cache parsed node data, not raw bytes | 15-20% search | [x] |
| 13 | Snapshot reads for search (tx.Snapshot()) | HIGH concurrent | [x] |
| 14 | Avoid visited-set string allocation | 5-8% | [x] cached pkBytes |
| 15 | Pool/reuse float64 buffers | 10-12% | [x] pre-alloc slices |
| 16 | GetRange for entire upper layer (greedy descent) | 1-2.5ms saved | [x] preloadLayer |

### 3. New key expression types

| Expression | Proto Message | Purpose | Priority |
|-----------|--------------|---------|----------|
| DimensionsKeyExpression | `Dimensions` | Multidimensional indexing (prefix_size + dimensions_size) | **LOW** |
| LiteralKeyExpression | `Value` | Static literal values (double/float/int64/bool/string/bytes) | **LOW** (already impl'd) |
| FunctionKeyExpression | `Function` | Named function with arguments | **LOW** |
| SplitKeyExpression | `Split` | Split repeated values into groups | **LOW** |
| ListKeyExpression | `List` | Homogeneous expression list (preserving boundaries) | **LOW** |
| AtomKeyExpression | (Java class) | Atom-level expressions | **LOW** |
| CollateFunctionKeyExpression | (Java class) | Locale-aware string sorting | **LOW** |
| OrderFunctionKeyExpression | (Java class) | Custom sort order functions (2024) | **LOW** |
| LongArithmethicFunctionKeyExpression | (Java class) | Arithmetic operations on longs (2024) | **LOW** |
| InvertibleFunctionKeyExpression | (interface) | Bidirectionally invertible functions | **LOW** |

- [x] **FunctionKeyExpression** — Implemented with global registry, proto round-trip, `get_versionstamp_incarnation` built-in. `FDBStoredRecord.Store` field added (matches Java's `FDBRecord.getStore()`). 25 unit tests.
- [x] **SplitKeyExpression** — Batches FanOut results into fixed-size groups. Proto `Split{joined, split_size}`. Overflow-checked. 14 unit tests.
- [x] **ListKeyExpression** — Cross-product with nested tuple wrapping (unlike Concat which flattens). Proto `List{repeated child}`. FDB tuple.Tuple nesting for proper Pack(). 15 unit tests.
- [x] **LongArithmeticFunctionKeyExpression** — 14 arithmetic functions (add, sub, subtract, mul, multiply, div, divide, mod, bitand, bitor, bitxor, bitnot, bitmap_bit_position, bitmap_bucket_offset) via FunctionKeyExpression registry. Overflow-checked (Math.*Exact), null propagation, both-function pattern (sub/subtract). 25 unit tests.
- [x] **OrderFunctionKE + InvertibleFunctionKE** — Implemented: 4 order functions (order_asc_nulls_first/last, order_desc_nulls_first/last) registered in global function registry. TupleOrdering byte encoding with 7-bit inversion for DESC and 0xFE null substitution for NULLS_LAST. Pack/unpack, invert/uninvert, tuple element boundary parsing. 31 tests. **MEDIUM-HIGH**.
- [x] **CollateFunctionKE** — Implemented: `collate_jre` and `collate_icu` registered using `golang.org/x/text/collate`. Supports locale + 3 strength levels (PRIMARY/SECONDARY/TERTIARY). Collators pooled via sync.Pool (not goroutine-safe). NOTE: sort key bytes differ from Java — Go-only clusters work, shared Java/Go clusters should avoid collated indexes. 21 tests. **MEDIUM**.
- [ ] **AtomKE** — Compile-time Java interface, not persisted. No wire format impact. **LOW**.

### 4. New store APIs

- [x] **Store locking APIs** — `SetStoreLockState(state, reason)`, `ClearStoreLockState()`, `OverrideLockSaveRecord()` (skips FORBID_RECORD_UPDATE lock). **HIGH**.
- [x] **Header user fields** — `GetHeaderUserField(key)`, `SetHeaderUserField(key, value)`, `ClearHeaderUserField(key)`. **MEDIUM**.
- [x] **Store state caching** — `FDBRecordStoreStateCache` interface, `MetaDataVersionStampStoreStateCache` implementation (LRU+TTL, \xff/metadataVersion invalidation), `SetStateCacheability()` API, dirty state tracking on context, read conflict on cache hit. 2.2x speedup on store open. 40 tests. **MEDIUM**.
- [x] **Incarnation APIs** — `GetIncarnation()`, `UpdateIncarnation(updater)`. **MEDIUM**.
- [x] **Snapshot version loading** — `LoadRecordVersion(pk, snapshot)` already implemented in `store_version.go`. **LOW**.
- [ ] **PreloadRecordStoreState** — Separate state loading from store creation. **LOW** (optimization).
- [x] **Index build state tracking** — `AddBuildProgress`/`LoadBuildProgress` at `[9][indexSubspaceKey][1]` (atomic ADD). Wired into `buildRange`/`buildRangeByIndex`. 4 tests. **LOW**.
- [x] **DryRunSaveRecord** — Validation (existence, type, lock) without writes. Returns computed record with size info. 4 tests. **LOW**.
- [x] **DryRunDeleteRecord** — Checks record existence without deleting. 3 tests. **LOW**.
- [x] **ScanRecordKeys** — Key-only scan without deserialization (dedup for split records). 5 tests. **LOW**.
- [x] **Index state query APIs** — `IsIndexReadableUniquePending`, `GetWriteOnlyIndexes`, `GetDisabledIndexes`, `GetIndexesToBuildSince`. 9 tests. **LOW**.
- [x] **Uniqueness violation resolution** — `ScanUniquenessViolationsForValue`, `ResolveUniquenessViolationByDeletion`. 6 tests. **LOW**.

### 5. Metadata & schema evolution changes

- [x] **Index predicates (IndexPredicate)** — Proto round-trip implemented: indexFromProto reads Predicate AST, builds evaluator function, stores proto for serialization. indexToProto serializes back. Supports all 5 predicate types (And, Or, Not, Constant, Value) with 9 comparison operators. SetPredicateProto/GetPredicateProto on Index. 52 tests. **MEDIUM**.
- [x] **Index replacement lifecycle** — `GetReplacedByIndexNames()`, replacement-exists validation, chained-replacement rejection. 7 tests. **LOW**.
- [ ] **Synthetic record types** — `JoinedRecordType` (equi-join with outer join support), `UnnestedRecordType` (repeated message fan-out). Proto fields 12-13 in MetaData. 11+ Java classes. **LOW** (large feature, experimental API).
- [ ] **Views** — `PView` in MetaData proto (field 15). Name + SQL definition text. **LOW**.
- [ ] **User-defined functions** — `PUserDefinedFunction` in MetaData proto (field 14). Macro or SQL functions. **LOW**.
- [x] **MetaDataEvolutionValidator enhancements** — Proto syntax/edition check, `hasPresence` consistency, `allowUnsplitToSplit` (already done). All Java checks now covered. **LOW**.
- [x] **MetaDataEvolutionValidator: `allowNoSinceVersion` validation** — Implemented: `SetAllowNoSinceVersion()` builder option. New record types must have `SinceVersion` set (errors if missing unless allowed) and `SinceVersion > oldMetaData.Version()`. Matches Java lines 378-397. 6 new tests (29 total). **HIGH**.
- [x] **MetaDataEvolutionValidator: `SinceVersion` immutability check** — Implemented: `SinceVersion` cannot change on existing record types. Matches Java line 361. **MEDIUM**.
- [x] **MetaDataEvolutionValidator: `primaryKeyComponentPositions` validation** — Implemented: positions cannot be added, dropped, or changed between index versions. Skipped when `allowIndexRebuilds` and version changed. Matches Java lines 649-667. Added `HasPrimaryKeyComponentPositions()`/`PrimaryKeyComponentPositions()` getters on Index. **MEDIUM**.
- [x] **MetaDataValidator enhancements** — Former index version boundary checks, addedVersion ≤ lastModifiedVersion, index replacement chain validation. 11 tests. KeyExpression.Validate() against proto descriptors added (field existence, FanType vs repeatedness, message-without-Nest). Build() validates: no record types, union descriptor oneof, PK/index/universal expressions. 70 new tests. Found 6 latent test bugs. **LOW**.

### 6. New cursor types

- [ ] **AggregateCursor** — Accumulator-based aggregation over cursor results. New continuation format (4.4–4.5). **LOW** (needed for query planner, not basic CRUD).
- [ ] **ComparatorCursor** — Custom comparator ordering. **LOW**.
- [ ] **UnorderedUnionCursor** — Union without order preservation. **LOW**.
- [ ] **SizeStatisticsGroupingCursor** — Key/value size tracking during group operations. **LOW**.
- [ ] **BloomFilterCursorContinuation** — Bloom filter optimization for large result sets. **LOW**.

### 7. New index scan types

- `BY_TEXT_TOKEN` — TEXT index token searches. **LOW**.
- ~~`BY_DISTANCE`~~ — DONE. Implemented via `ScanVectorIndex()` and `ScanIndexByType(BY_DISTANCE)`.
- `BY_TIME_WINDOW` — TIME_WINDOW_LEADERBOARD. **LOW**.

### 8. New aggregate functions

- [x] **MAX_EVER_VERSION** — via MAX_EVER_VERSION index type. Aggregate support via `FunctionNameMaxEver`/`IndexTypeMaxEverVersion`. **MEDIUM**.
- [x] **BITMAP_VALUE, BITMAP_BIT_POSITION, BITMAP_BUCKET_OFFSET** — Already implemented (BITMAP_VALUE aggregate + LongArithmeticFunctionKE covers bit_position/bucket_offset).
- [x] **TIME_WINDOW_RANK, TIME_WINDOW_COUNT** — Already implemented in `evaluateAggregateFunction` and `evaluateRecordFunction` dispatch.

### 9. SQL / Relational layer

Java has 6 separate modules for SQL: `fdb-relational-api`, `fdb-relational-core`, `fdb-relational-jdbc`, `fdb-relational-grpc`, `fdb-relational-server`, `fdb-relational-cli`. Features include: SQL views (`PView`), user-defined functions (`PUserDefinedFunction`), CAST/type coercion, recursive CTEs (PREORDER/POSTORDER), BETWEEN/CASE expressions, COPY command for data import/export, composite aggregates, JOIN with ORDER BY. All built on top of `fdb-record-layer-core`.

**Not a priority until core is flawless.** The SQL layer sits entirely above the record layer — it uses the same store, indexes, cursors, and metadata we're porting. Once core is complete and conformant, SQL becomes a natural extension. No wire format impact from ignoring it now.

Also in Java but out of scope for now: `fdb-record-layer-lucene` (full-text via Lucene), `fdb-record-layer-spatial` (R-tree spatial), `fdb-record-layer-icu` (Unicode collation).

### 10. API/behavioral changes (informational, no action needed unless noted)

- FormatVersion transitioned from constants to enum (4.3) — internal, no wire impact
- Index maintainer factory API customization (4.4) — we don't expose factory API
- OnlineIndexer heartbeat replaced synchronized runner (4.6–4.10) — our Go impl is independent
- Deprecated synchronized indexing APIs removed (4.10) — doesn't affect Go
- URI parsing tightened (4.10) — relational layer, not record layer core
- `PUserDefinedFunction` oneof field renamed (4.10) — same proto field numbers, wire-compatible
- `__ROW_VERSION` pseudo-field (4.8–4.10) — query planner only, doesn't affect storage
- Plan serialization incompatible between 4.8↔4.10 — we don't serialize plans
- Java 21 compatibility (`this-escape` warnings) — Java-only
- AutoCommit support (4.5) — transaction management feature, informational
- Lucene improvements (4.4–4.10) — separate module, not in core record layer

### 11. Version-by-version wire format breaking changes

| Versions | Change | Impact on Go |
|----------|--------|-------------|
| 4.3→4.5 | AggregateCursorContinuation proto format | No impact (we don't have aggregate cursors) |
| 4.5→4.6 | Lucene serialization changes | No impact (we don't have Lucene) |
| 4.7→4.8 | KeyValueCursorBaseContinuation format | No impact (conformance tests pass with TO_OLD) |
| 4.9→4.10 | `__ROW_VERSION` replaces `VersionValue` in plans | No impact (query planner only) |

### 12. Priority summary

**HIGH (wire compat / correctness):**
1. ~~FULL_STORE lock + FormatVersion 14 stricter validation~~ **DONE**
2. ~~READABLE_UNIQUE_PENDING index state~~ **DONE**
3. ~~Store locking APIs (set/clear/override)~~ **DONE**

**MEDIUM (feature completeness):**
4. ~~Store incarnation field + APIs~~ **DONE**
5. ~~Header user fields~~ **DONE**
6. ~~MAX_EVER_VERSION index type~~ **DONE**
7. ~~FunctionKeyExpression (for incarnation)~~ **DONE**
8. ~~Store state caching~~ **DONE**

**LOW (specialized / future):**
9. ~~All 19 index types complete~~ — ~~TEXT~~, ~~BITMAP~~, ~~PERMUTED_MIN/MAX~~, ~~MAX_EVER_VERSION~~, ~~TIME_WINDOW_LEADERBOARD~~, ~~MULTIDIMENSIONAL~~, ~~VECTOR~~ done
10. Remaining key expression types (Dimensions, Collate, Order, Atom, Invertible) — ~~Split~~, ~~List~~, ~~LongArithmetic~~, ~~Function~~ done
11. Synthetic record types (JoinedRecordType, UnnestedRecordType)
12. Views, UDFs
13. New cursor types (Aggregate, Comparator, UnorderedUnion)
14. Query planner features (not ported)

---

## Error handling alignment (2026-03-12 QA audit)

Architectural decision: Java exception class = Go error struct. Use `errors.As()` for matching. No bare sentinels. See CLAUDE.md "Error handling" section for full pattern.

**Naming convention:** Java `FooBarException` → Go `FooBarError` struct. Drop the `Exception` suffix, replace with `Error`. Examples:
- `RecordAlreadyExistsException` → `RecordAlreadyExistsError`
- `ScanNonReadableIndexException` → `IndexNotReadableError` (simplified where Java name is awkward)
- `RecordStoreNoInfoAndNotEmptyException` → `RecordStoreNoInfoButNotEmptyError`

**Pattern:** Always a `type FooError struct { ... }` with context fields matching Java's `addLogInfo()` keys. Never `var ErrFoo = errors.New("...")`. Callers match with `errors.As(err, &e)`, never `errors.Is(err, ErrFoo)`.

### Phase 1: Convert existing sentinels to error types — **DONE**

- [x] **`ErrRecordStoreAlreadyExists`** → `RecordStoreAlreadyExistsError` struct. All return sites migrated.
- [x] **`ErrRecordStoreDoesNotExist`** → `RecordStoreDoesNotExistError` struct. All return sites migrated.
- [x] **`ErrRecordStoreNoInfoButNotEmpty`** → `RecordStoreNoInfoButNotEmptyError` struct with `FirstKey` field.
- [x] **`ErrRecordStoreStateNotLoaded`** → `RecordStoreStateNotLoadedError` struct. 8 return sites migrated.
- [x] **`ErrIndexNotReadable`** → `IndexNotReadableError` struct with `IndexName` + `CurrentState`.
- [x] **`ErrIndexNotFound`** → `IndexNotFoundError` struct with `IndexName`. 5 return sites migrated.
- [x] **`ErrIndexNotBuilt`** → `IndexNotBuiltError` struct with `IndexName`.
- [x] Removed old `ErrRecordAlreadyExists` / `ErrRecordDoesNotExist` / `ErrRecordTypeChanged` sentinel variables and `Is()` methods.
- [x] Updated all call sites: `errors.Is(err, ErrFoo)` → `errors.As(err, &fooErr)`.
- [x] Updated all tests (unit + conformance) to use `errors.As()` pattern.

### Phase 2: Add missing error types for implemented features — **DONE**

- [x] **`MetaDataError`** — defined in `errors.go`. Message-only, matchable via `errors.As()`.
- [x] **`UnsupportedFormatVersionError`** — carries `Version` + `MaxVersion`. Store builder `validateFormatVersion` migrated.
- [x] **`RecordSerializationError`** — wraps proto marshal failures with `Unwrap()`. 2 return sites migrated.
- [x] **`RecordDeserializationError`** — wraps proto unmarshal failures with `Unwrap()`. 6 return sites migrated (store + cursor).
- [ ] **`StaleUserVersionError`** — Java's `RecordStoreStaleUserVersionException` (not thrown in 4.10.6.0 but type exists). Deferred — no throw sites exist.

### Phase 3: Conformance tests for error paths — **DONE**

- [x] **Improve Java conformance server** — catch block now returns structured error JSON with `exceptionClass` and `exceptionFullClass` fields. Go `JavaError` type for type-level assertions. HTTP 200 for step errors (not 500).
- [x] **Record existence errors cross-language** — RecordAlreadyExistsException, RecordDoesNotExistException verified both Go and Java throw equivalent errors.
- [x] **Store lifecycle errors cross-language** — RecordStoreAlreadyExistsException, RecordStoreDoesNotExistException verified both Go and Java.
- [x] **Index scan errors cross-language** — ScanNonReadableIndexException verified on write-only index scan.
- [x] **Store lock errors cross-language** — FORBID_RECORD_UPDATE prevents save in both Go and Java.
- [x] **Cross-language error propagation** — Go creates record, Java insert duplicate gets RecordAlreadyExistsException.
- [x] **Unique index violation cross-language** — 6 conformance specs: READABLE violation detection (Go→Java, Java→Go), index entry scanning, WRITE_ONLY violation wire format with existingKey.
- [ ] **Schema validation cross-language** — deferred (MetaDataValidator gaps need to be addressed first).

---

## Completed (for reference)

- [x] SaveRecord, LoadRecord, DeleteRecord — core CRUD working
- [x] Java compatibility — bidirectional read/write via conformance tests
- [x] TypedFDBRecordStore with Go generics
- [x] Builder pattern (Create, Open, CreateOrOpen, Build)
- [x] RecordExists method
- [x] RecordExistenceCheck enum (NONE, ERROR_IF_EXISTS, ERROR_IF_NOT_EXISTS, ERROR_IF_NO_EXISTING_RECORD)
- [x] Conflict management — AddRecordReadConflict, AddRecordWriteConflict
- [x] Isolation levels — Snapshot vs Serializable reads
- [x] Cursor API — RecordCursor interface with OnNext/Close/Seq/Seq2/SeqWithContinuation
- [x] Key-value cursor — Range iteration, continuation tokens, byte/row limits
- [x] Cursor combinators — Filter, Map, MapErr, Filter2, Limit
- [x] Range scans — ScanRecords, ScanRecordsInRange, forward/reverse, endpoint types
- [x] Key expressions — FieldKeyExpression, RecordTypeKeyExpression, EmptyKeyExpression, CompositeKeyExpression
- [x] Large dataset scanning — 10K sequential + 1K continuation + 1M stress
- [x] Record versioning — FDBRecordVersion (12-byte), inline storage at pk + -1 suffix
- [x] Record counting — atomic ADD mutations, per-type via RecordTypeKeyExpression
- [x] Store state validation — StoreLockState.FORBID_RECORD_UPDATE check (note: FULL_STORE lock state added in 4.10.6.0, see upgrade assessment)
- [x] Split records — saveWithSplit/loadWithSplit/deleteSplit, 100KB chunks, cursor reassembly
- [x] Secondary indexes (VALUE) — StandardIndexMaintainer, unique enforcement, common-entry skip
- [x] Covering indexes (KeyWithValueExpression) — value columns stored in FDB value, 14 unit tests + 5 conformance specs
- [x] Index maintenance — auto-update on Save/Delete/DeleteAllRecords
- [x] Continuation token protobuf wrapping — magic number 6773487359078157740
- [x] Bulk operations — DeleteAllRecords, GetRecordCount/GetSnapshotRecordCount
- [x] Bazel 8 migration — MODULE.bazel, gazelle, nogo (20 analyzers)
- [x] **Bazel 9 upgrade** — upgraded from 8.2.1 to 9.0.1. Bumped rules_java 8→9.6.1, added rules_android 0.7.1, removed archived rules_proto, added explicit protobuf-java-util Maven dep. All 1150 specs pass.
- [x] CI pipeline — GitHub Actions with Bazel build + test
- [x] Subspace constants verified — all 10 match Java exactly (0-9)

---

## Conformance test coverage gaps

The conformance framework (HTTP bridge to Java Record Layer) validates all core features bidirectionally. Every wire-format-sensitive feature has Go↔Java cross-validation.

### CRITICAL — wire format at risk without cross-validation

- [x] **Split record conformance** — 9 specs: Go writes 250KB/150KB/100KB/small/minimal → Java reads; Java writes 250KB/150KB/small → Go reads; overwrite large→small and small→large. Cross-validated.

- [x] **Index entry format conformance** — 5 specs: Go writes → Java scans, Java writes → Go scans, delete removes entry, update changes entry, sorted multi-record scan. Index entries compared field-by-field. Cross-validated.

- [x] **Record version conformance** — 4 specs: Go saves versioned → Java reads, Java saves → Go reads, local version ordering, version update. Cross-validated.

- [x] **Scan/continuation conformance** — 6 specs: Go writes/Java scans, Java writes/Go scans, limit, ordering, empty store, flower details. Cross-validated.

- [x] **Record counting conformance** — 6 specs: Go saves/Java counts, Java saves/Go counts, delete decrements, update doesn't increment, mixed saves, zero baseline. Cross-validated.

### HIGH — remaining gaps

- [x] **Multi-type conformance** — 11 specs + 1 direct store spec: Customer CRUD, cross-write, boundary values, delete non-existent, multiple customers. Cross-validated.

- [x] **Continuation token cross-platform** — 3 specs: Go→Java resume, Java→Go resume, alternating Go/Java. Cross-validated. Go uses TO_OLD (raw bytes) format matching Java Record Layer 4.2.6.0.

- [x] **Reverse scan conformance** — 6 specs: Go writes/Java reverse scans, Java writes/Go reverse scans, limit, forward-reverse mirror, cross-platform continuation resume, empty store. Cross-validated.

- [x] **Fan-out index conformance** — 7 specs: Go writes/Java scans fan-out entries, Java writes/Go scans, multiple records, empty repeated field, delete removes all entries, update changes entries, cross-write. Cross-validated.

### Current conformance coverage

| Feature | Java Steps | Go Tests | Cross-validated |
|---|---|---|---|
| Basic CRUD | saveOrder, loadOrder, deleteOrder, recordExists | 5 test files | YES |
| Existence checks | (via saveOrder) | existence_check_conformance_test.go | YES |
| Isolation levels | (via raw FDB) | isolation_conformance_test.go | YES |
| Conflict detection | (via raw FDB) | conflict_conformance_test.go | YES |
| Record versioning | saveOrderVersioned, loadOrderWithVersion | version_conformance_test.go | YES |
| Record counting | saveOrderCounting, deleteOrderCounting, getRecordCount | count_conformance_test.go | YES |
| Scan/ordering | scanOrders | scan_conformance_test.go | YES |
| Multi-type (Customer) | saveCustomer, loadCustomer, deleteCustomer, customerExists | customer_conformance_test.go | YES |
| Split records | saveSplitOrder, loadSplitOrder | split_conformance_test.go | YES |
| Secondary indexes | saveOrderWithIndex, scanIndex, deleteOrderWithIndex | index_conformance_test.go | YES |
| Continuation tokens | scanOrdersWithContinuation | continuation_conformance_test.go | YES |
| Reverse scan | scanOrdersReverse, scanOrdersReverseWithContinuation | reverse_scan_conformance_test.go | YES |
| Fan-out indexes | saveOrderWithFanOutIndex, scanFanOutIndex, deleteOrderWithFanOutIndex | fanout_index_conformance_test.go | YES |
| Composite index (PK dedup) | saveOrderWithCompositeIndex, scanCompositeIndex | composite_index_conformance_test.go | YES |
| COUNT index | saveOrderWithCountIndex, deleteOrderWithCountIndex, scanCountIndex | count_index_conformance_test.go | YES |
| SUM index | saveOrderWithSumIndex, deleteOrderWithSumIndex, scanSumIndex | sum_index_conformance_test.go | YES |
| RangeSet wire format | rangeSetInsert, rangeSetContains, rangeSetMissingRanges | rangeset_conformance_test.go | YES |
| DeleteAllRecords | deleteAllRecordsWithIndex, countRecordsWithIndex | delete_all_conformance_test.go | YES |
| Store header format | getStoreHeaderRaw, createStoreWithUserVersion | store_header_conformance_test.go | YES |
| Index state persistence | markIndexWriteOnly/Disabled/Readable, getIndexStateRaw | index_state_conformance_test.go | YES |
| Store lifecycle | (reuses existing steps) | store_lifecycle_conformance_test.go | YES |
| MAX_EVER_LONG index | saveOrderWithMaxEverLongIndex, deleteOrderWithMaxEverLongIndex, scanMaxEverLongIndex | min_max_ever_index_conformance_test.go | YES |
| MIN_EVER_LONG index | saveOrderWithMinEverLongIndex, deleteOrderWithMinEverLongIndex, scanMinEverLongIndex | min_max_ever_index_conformance_test.go | YES |
| COUNT_NOT_NULL index | saveOrderWithCountNotNullIndex, deleteOrderWithCountNotNullIndex, scanCountNotNullIndex | count_not_null_index_conformance_test.go | YES |
| COUNT_UPDATES index | saveOrderWithCountUpdatesIndex, deleteOrderWithCountUpdatesIndex, scanCountUpdatesIndex | count_updates_index_conformance_test.go | YES |
| MAX_EVER_TUPLE index | saveOrderWithMaxEverTupleIndex, deleteOrderWithMaxEverTupleIndex, scanMaxEverTupleIndex | min_max_ever_tuple_index_conformance_test.go | YES |
| MIN_EVER_TUPLE index | saveOrderWithMinEverTupleIndex, deleteOrderWithMinEverTupleIndex, scanMinEverTupleIndex | min_max_ever_tuple_index_conformance_test.go | YES |
| CLEAR_WHEN_ZERO | saveOrderWithCountCWZ, deleteOrderWithCountCWZ, scanCountCWZIndex | clear_when_zero_conformance_test.go | YES |
| Covering index (KeyWithValue) | saveOrderWithCoveringIndex, scanCoveringIndex, deleteOrderWithCoveringIndex | covering_index_conformance_test.go | YES |
| DeleteRecordsWhere | saveOrderTypePrefixed, saveCustomerTypePrefixed, deleteRecordsWhereType, countRecordsTypePrefixed, loadOrderTypePrefixed, loadCustomerTypePrefixed, scanIndexTypePrefixed | delete_records_where_conformance_test.go | YES |
| VERSION index | saveOrderWithVersionIndex, deleteOrderWithVersionIndex, scanVersionIndex | version_index_conformance_test.go | YES |
| OnlineIndexer | saveOrderForOnlineBuild, scanIndexAfterOnlineBuild, isIndexReadableAfterBuild | online_indexer_conformance_test.go | YES |
| PERMUTED_MAX index | saveOrderWithPermutedMaxIndex, deleteOrderWithPermutedMaxIndex, scanPermutedMaxByValue, scanPermutedMaxByGroup | permuted_min_max_index_conformance_test.go | YES |
| PERMUTED_MIN index | saveOrderWithPermutedMinIndex, deleteOrderWithPermutedMinIndex, scanPermutedMinByValue, scanPermutedMinByGroup | permuted_min_max_index_conformance_test.go | YES |
| Index scan continuations | scanIndexWithContinuation, saveOrderForIndexContinuation | index_continuation_conformance_test.go | YES |
| Error paths | insertDuplicateOrder, updateNonExistentOrder, openNonExistentStore, createExistingStore, scanNonReadableIndex, saveLocked | error_conformance_test.go | YES |
| Index build state (stamp) | loadIndexingTypeStamp, saveIndexingTypeStampByRecords | index_build_state_conformance_test.go | YES |
| EvaluateAggregateFunction | evaluateCountAggregate, evaluateSumAggregate, evaluateMinAggregate, evaluateMaxAggregate, evaluateMinEverAggregate, evaluateMaxEverAggregate | aggregate_conformance_test.go | YES |
| Unique violations | saveWithUniqueIndex, deleteWithUniqueIndex, scanUniqueIndex, saveDuplicateWithUniqueIndex, markUniqueIndexWriteOnly, saveWithUniqueIndexDuringWriteOnly, scanUniquenessViolations, getUniqueIndexState | unique_violation_conformance_test.go | YES |

### NEW — conformance gaps identified 2026-03-09

- [x] **SUM index conformance** — CRITICAL. 7 specs: Go writes→Java scans, Java writes→Go scans, mixed writes combined sum, Go deletes Java-written record, Java deletes Go-written record, update via Go, update via Java. Cross-validated.
- [x] **RangeSet wire format conformance** — CRITICAL. 4 specs: Go writes full range→Java reads, Java writes full range→Go reads, Go writes partial→Java reads gaps, Java writes partial→Go reads gaps. Wire format `pack(rangeBegin) → rangeEnd` cross-validated.
- [x] **DeleteAllRecords cross-validation** — CRITICAL. 4 specs: Go saves→Go deletes→Java confirms empty, Java saves→Java deletes→Go confirms empty, cross-write→Go deletes→Java confirms, delete→re-save cross-platform. Records + index entries verified cleared.
- [x] **Store header format conformance** — HIGH. 4 specs: Go creates→Java reads raw header, Java creates→Go reads raw header, Go sets userVersion→Java reads, Java sets userVersion→Go reads. Proto wire format cross-validated.
- [x] **Index state persistence across reopen** — HIGH. 4 specs: Go marks WRITE_ONLY→Java reads raw, Java marks WRITE_ONLY→Go reads, Go marks DISABLED→Java reads, WRITE_ONLY→READABLE roundtrip clears entry. Wire format cross-validated.
- [x] **FormerIndex tracking conformance** — N/A. FormerIndex is metadata-only (not persisted in FDB data). Validation happens at Build() time, not wire-format level.
- [x] **Store delete+recreate lifecycle** — HIGH. 3 specs: header preserved across DeleteAllRecords, index state WRITE_ONLY survives DeleteAllRecords, Java deletes→Go re-creates and saves. Cross-validated.
- [x] **MAX_EVER_LONG index conformance** — HIGH. 6 specs: Go writes→both scan, Java writes→both scan, mixed writes, delete irreversibility (Go deletes Java record, Java deletes Go record), update never decreases. Cross-validated.
- [x] **MIN_EVER_LONG index conformance** — HIGH. 6 specs: Go writes→both scan, Java writes→both scan, mixed writes, delete irreversibility (Go deletes Java record, Java deletes Go record), update never increases. Cross-validated.
- [x] **Covering index (KeyWithValueExpression) conformance** — HIGH. 5 specs: Go writes→both scan, Java writes→both scan, cross-language delete, update changes value consistently, mixed writes. Value portion (flower.type) cross-validated. 14 unit tests cover edge cases (splitPoint=0, splitPoint=len(inner), FanOut+covering, continuation).
- [x] **OnlineIndexer conformance** — HIGH. 7 specs: Go saves→Go builds→Java scans, Java saves→Go builds→both scan, chunked build (limit=3), Go online-build vs Java rebuild identical, index state READABLE cross-validated (Java+Go), mixed writes then Go build. Note: Java's OnlineIndexer doesn't support FDB tenants in Maven 4.2.6.0, so Java-builds-index tests skipped.
- [x] **Store header v2 conformance (4.10.6.0 features)** — HIGH. 14 specs: header user fields (Go sets→Java reads, Java sets→Go reads, multiple fields, overwrite), incarnation (Go sets→Java reads, Java sets→Go reads, sequential increments), store lock state (FULL_STORE blocks Java open, bypass with matching reason, wrong reason fails, FORBID_RECORD_UPDATE blocks save, Java locks→Go fails, clear restores access, wire format matches). Cross-validated.
- [x] **MAX_EVER_VERSION index conformance** — HIGH. 7 specs: Go writes/both scan, Java writes/both scan, mixed writes, _EVER delete semantics, later write updates max, cross-language delete persistence, wire format versionstamp bytes match. SET_VERSIONSTAMPED_VALUE dual mutation path cross-validated.
- [ ] ~~**FunctionKeyExpression conformance**~~ — N/A. `get_versionstamp_incarnation` is Go-specific (not a Java built-in). Function registry is local to each implementation.

### Wire compat review gaps (identified 2026-03-11)

**P0 — wire format at risk:**
- [x] **PERMUTED_MIN/MAX conformance** — CRITICAL. 10 specs: Go writes/both scan BY_VALUE+BY_GROUP, Java writes/both scan, mixed writes, Go deletes max written by Java (re-fetch), Java deletes max written by Go (re-fetch), non-extremum delete unchanged, PERMUTED_MIN Go writes/both scan, Java writes/both scan, delete min re-fetch, non-extremum insert unchanged. Dual subspace wire format cross-validated.

**P1 — strengthens confidence:**
- [x] **Index scan continuation cross-language resume** — HIGH. 3 specs: Go→Java resume, Java→Go resume, alternating Go/Java. VALUE index paged scan with 10 entries, limit=3/2 page sizes. Continuation token wire format cross-validated (Go TO_OLD ↔ Java proto-wrapped).
- [x] **RecordMetaData proto serialization cross-language roundtrip** — 21 specs (7 configs × 3 directions). Configs: basic, with_indexes, with_former_indexes, full, with_universal_index, with_record_count, with_explicit_type_key. Go→Java, Java→Go, Go→Java→Go roundtrip. `clearProto2Defaults` normalizes proto2 field presence across Go/Java (including map message values). Java side uses `ExtensionRegistry` for `(record).usage=UNION` option resolution.

**P2 — edge cases:**
- [x] **clearProto2Defaults missing map<K, Message> recursion** — Fixed: added `fd.IsMap() && fd.MapValue().Kind() == protoreflect.MessageKind` case to recurse into map message values.
- [x] **Metadata conformance: explicit record type key config** — Added `with_explicit_type_key` config (int64(42) / 42L). 7 configs × 3 directions = 21 specs now (was 18).
- [x] **Proto field type diversity in test schema** — DONE. `field_type_index_test.go` (16 specs): VALUE indexes on every TypedRecord field type (int32, sint32, sint64, sfixed32, sfixed64, float, double, bool, string, bytes, enum). Tests null handling, composite multi-type indexes, save/delete/scan roundtrip, float special values (±Inf, ±0.0), int32 boundary values (MaxInt32, MinInt32). Cross-language conformance already covered by `typed_record_conformance_test.go` (11 specs). Remaining untested: map (Java rejects), oneof (transparent to storage), repeated message (covered by NestFanOut tests).
- [x] **Store lock + delete operation interaction** — DONE (already implemented). Go has `validateRecordUpdateAllowed()` in all 4 mutation paths (SaveRecord, DeleteRecord, DeleteAllRecords, DeleteRecordsWhere) matching Java exactly. Unit tests cover: DeleteBlockedByLock, DeleteAllBlockedByLock, DeleteRecordsWhere blocked, error precedence (non-existent delete returns false, not lock error). Lock wire format cross-validated by store header conformance tests (14 specs).
- [x] **Index build state wire format (subspace 9)** — MEDIUM. `SaveIndexingTypeStamp`/`LoadIndexingTypeStamp` on store. OnlineIndexer saves BY_RECORDS stamp at `[9][indexSubspaceKey][2]` matching Java's `IndexingBase.setIndexingTypeOrThrow()`. 5 conformance specs: Go→Java, Java→Go, no stamp, persists after READABLE, cleared on rebuild.

---

## Bugs (found in conformance audit)

### CRITICAL

- [x] **Version values stored as raw bytes instead of tuple-packed Versionstamp** — Fixed: Go stored version values as raw 12-byte FDBRecordVersion bytes. Java's `SplitHelper.unpackVersion()` calls `Tuple.fromBytes()` expecting a tuple-encoded Versionstamp. Caused "Unknown tuple data type 3 at index 5" error. Fix: wrap in `tuple.Tuple{Versionstamp}.Pack()` for complete, `PackWithVersionstamp()` for incomplete.

- [x] **Java conformance server tenant.run() skips version mutation flush** — Fixed: `runInContext` for tenants used `tenant.run()` which auto-commits bypassing `FDBRecordContext.commitAsync()`. Pre-commit hooks (version mutation flush) never fired, so versioned saves silently dropped version data. Fix: use `createTransaction()` + `context.commitAsync().join()`.

- [x] **CompositeKeyExpression does concat, not cross-product** — Fixed: `Evaluate()` now returns `[][]interface{}` (list of key tuples) and `CompositeKeyExpression` computes Cartesian product matching Java's `ThenKeyExpression`.

- [x] **evaluateIndex() always returns 1 entry per record** — Fixed: `evaluateIndex()` now creates one `indexEntry` per returned tuple, enabling fan-out for multi-valued expressions.

### HIGH

- [x] **DeleteRecord doesn't cleanup incomplete version mutations** — Fixed: `DeleteRecord` now calls `deleteRecordVersion()` to remove queued version mutations from `FDBRecordContext`, preventing stale version data for deleted records. Matches Java's `deleteTypedRecord` which calls `context.removeVersionMutation()`.

- [x] **DeleteAllRecords doesn't clear all data subspaces** — Fixed: Go only cleared subspaces 1,2,4,8. Java clears all subspaces except 0 (header) and 5 (index state). Missing: 3 (secondary index), 6 (index range), 7 (uniqueness violations), 9 (index build). Fixed to match Java's approach.

- [x] **RecordTypeKeyExpression uses string name instead of integer type key** — Fixed two bugs: (1) `RecordTypeIndex` was a sequential counter (0,1,2...) instead of the proto field number from UnionDescriptor. Java uses `field.getNumber()`. (2) `RecordTypeKeyExpression.Evaluate()` returned the proto message name string (`"Order"`) instead of the integer record type key. Java returns `record.getRecordType().getRecordTypeKey()` which is the proto field number (as `Long`). Fixed by storing a type-key lookup map in the expression, populated at metadata build time.

- [x] **FieldKeyExpression panics on nil message** — Fixed: `Evaluate(nil)` crashed at `msg.ProtoReflect()`. Happens when NestingKeyExpression evaluates a child on an unset message field. Now returns `nil` (null key component) matching Java's behavior of returning `Key.Evaluated.NULL`.

- [x] **GetValue() returns zero on !HasNext()** — Fixed: `GetValue()` now panics when `hasNext` is false, matching Java's `IllegalResultValueAccessException`.

- [x] **Build() doesn't validate primary keys** — Fixed: `Build()` now returns `(*RecordMetaData, error)` and validates all record types have primary keys set. All callers updated.

- [x] **ScannedRecordsLimit checks after read, skipping records on resume** — Fixed: The scan limit check happened after `readNextRecord()`, making the continuation point past the undelivered record. On resume, that record was skipped. Moved check before read, matching Java's `CursorLimitManager.tryRecordScan()` which checks limits pre-read.

### MEDIUM

- [x] **FDBRecordVersion missing Equal/Less** — Fixed: Added `Equal()`, `Less()`, `String()` methods matching Java's `equals()`/`compareTo()`/`toString()` semantics.

- [x] **WRITE_ONLY uniqueness violation tracking in maintainer** — QA audit finding: Java's `StandardIndexMaintainer.checkUniqueness()` writes violation entries to subspace 7 when index is WRITE_ONLY (instead of throwing). Fixed: added `indexStoreContext` interface, `checkUniqueness()` now writes violations when WRITE_ONLY, `Update()` cleans up violations on delete. `RebuildIndex` uses `MarkIndexReadableOrUniquePending`.

- [x] **Record count DISABLED state check** — Fixed: `addRecordCount()` now checks `RecordCountState != DISABLED` before mutating. `GetSnapshotRecordCount()` checks `== READABLE` before querying. `UpdateRecordCountState()` enforces valid transitions (READABLE↔WRITE_ONLY, any→DISABLED, DISABLED is terminal). When transitioning to DISABLED, clears all count data. 5 new tests.

---

## Indexing — conformance gaps

### CRITICAL

- [x] **Index scanning** — `IndexMaintainer.Scan()` and `FDBRecordStore.ScanIndex()` return `RecordCursor[*IndexEntry]` with `TupleRange` support (ALL, AllOf, Between, BetweenInclusive), continuations, row/byte limits, forward/reverse. `IndexEntry.PrimaryKey()` and `IndexValues()` for key extraction.

- [x] **Index state management** — 4 states: `READABLE`, `WRITE_ONLY`, `DISABLED`, `READABLE_UNIQUE_PENDING`. Stored in `IndexStateSpaceKey` (5) subspace as tuple-packed int64. Loaded on store Open/CreateOrOpen. `MarkIndexReadable`, `MarkIndexWriteOnly`, `MarkIndexDisabled`, `ClearAndMarkIndexWriteOnly`. DISABLED indexes skip maintenance. Non-scannable indexes reject ScanIndex. Matches Java's wire format and semantics.

- [x] **Index build support (core)** — RangeSet, IndexingRangeSet, WRITE_ONLY maintenance, OnlineIndexer BY_RECORDS. Remaining: progress tracking, indexing stamps, rebuildIndex, BY_INDEX strategy.

#### Index build sub-tasks (dependency order)

1. **RangeSet** (CRITICAL — foundation for all index building) ✅
   - [x] `RangeSet` type backed by FDB subspace. Wire-compatible with Java's `com.apple.foundationdb.async.RangeSet`.
   - Storage: each key-value = `[subspace.pack(rangeBegin)] → rangeEnd` (raw bytes, NOT packed). Range semantics: `[begin, end)` inclusive-exclusive. Valid key space: `[\x00, \xff)`.
   - [x] `InsertRange(tx, begin, end, requireEmpty bool) bool` — fill gaps in range set. `requireEmpty=true` = atomic test-and-set (returns false if range wasn't empty). `requireEmpty=false` = fill gaps, write-conflict only on gaps actually filled.
   - [x] `Contains(tx, key) bool` — snapshot read + read-conflict on key only.
   - [x] `MissingRanges(tx, begin, end, limit) []Range` — return gaps not yet in set.
   - [x] `IsEmpty(tx) bool` — check if entire `[\x00, \xff)` is missing.
   - [x] `Clear(tx)` — remove all entries.
   - [x] Unit tests: insert, contains, missing ranges, overlapping inserts, abutting ranges, consolidation, empty checks, wire format, incremental build pattern, multi-byte keys.

2. **IndexingRangeSet wrapper** (CRITICAL) ✅
   - [x] `IndexingRangeSet` at store subspace `[6, indexSubspaceKey]` (INDEX_RANGE_SPACE).
   - [x] `FirstMissingRange()`, `ContainsKey(primaryKey)`, `InsertRange(begin, end, requireEmpty)`, `ListMissingRanges()`, `IsComplete()`, `Clear()`.
   - [x] Already cleared on index delete / `ClearAndMarkIndexWriteOnly` (via `clearIndexData`).

3. **WRITE_ONLY index maintenance** (CRITICAL) ✅
   - [x] `IndexMaintainer.UpdateWhileWriteOnly(oldRecord, newRecord)` interface method.
   - [x] `StandardIndexMaintainer.UpdateWhileWriteOnly()` — idempotent VALUE indexes pass through to `Update()`. Matches Java's `isIdempotent() = true`.
   - [x] `updateSecondaryIndexes()` dispatches via `updateOneIndex()`: calls `UpdateWhileWriteOnly` when `IsIndexWriteOnly(idx)`, else `Update`. Matches Java.

4. **OnlineIndexer — BY_RECORDS strategy** (CRITICAL) ✅
   - [x] `OnlineIndexer` type with builder: `SetDatabase`, `SetMetaData`, `SetIndex`, `SetSubspace`, `SetLimit`, `SetRecordTypes`.
   - [x] `BuildIndex(ctx)` — marks WRITE_ONLY → iterates all missing ranges → marks READABLE. Returns total records indexed.
   - [x] `buildRange(ctx)` — finds first missing range via `IndexingRangeSet`, scans records in range, evaluates index + writes entries via `maintainer.Update(nil, rec)`, marks built range with `requireEmpty=true`.
   - [x] Transaction boundaries: each `buildRange` = one transaction. Continuation = last processed PK (matches Java: boundary records re-scanned, safe for idempotent indexes).
   - [x] Record type filtering: `shouldIndexRecord()` checks if record type has this index defined.
   - [x] 8 integration tests: basic build, composite index with PK dedup, empty store, post-build maintenance, small limit chunking, unique index, record type filtering, builder validation.
   - [ ] Progress tracking at `[9, indexSubspaceKey, 1]` (INDEX_BUILD_SPACE) — atomic ADD of records scanned. Not yet implemented (optimization, not wire-format critical).
   - [x] Indexing stamp at `[9, indexSubspaceKey, 2]` — proto `IndexBuildIndexingStamp` for resume detection. `SaveIndexingTypeStamp`/`LoadIndexingTypeStamp` + BY_RECORDS/BY_INDEX methods.
   - [x] **Stamp-aware resume** — `markWriteOnly()` checks if index is already WRITE_ONLY with matching stamp before clearing. Matching stamp → resume build without clearing existing entries (preserves WRITE_ONLY maintenance entries). No stamp + empty range set → write stamp and continue. Stamp mismatch → clear and restart. Matches Java's `IndexingBase.handleIndexingState()` + `setIndexingTypeOrThrow()`. 5 new tests.

5. **rebuildIndex on store** (HIGH — needed for store.Open with new indexes) ✅
   - [x] `FDBRecordStore.RebuildIndex(index)` — clears index data, marks WRITE_ONLY, pre-marks full range in RangeSet, scans all records inline, re-indexes, marks READABLE. Single-transaction path matching Java's `IndexingBase.rebuildIndexAsync()`.
   - [x] 8 tests: basic VALUE index, empty store, stale cleanup, type filtering, range set completion, unique index, uniqueness violation, post-rebuild maintenance.
   - [x] `CreateOrOpen` auto-rebuild: `checkPossiblyRebuild()` compares stored metadata version with current. Uses `GetIndexesToBuildSince(oldVersion)` to find new indexes. Rebuilds inline and updates store header. Matches Java's `FDBRecordStore.checkPossiblyRebuild()`.
   - [x] `addIndexCommon()` on builder: sets `LastModifiedVersion` and `AddedVersion` matching Java's `RecordMetaDataBuilder.addIndexCommon()`. Bumps builder version on each index add.
   - [x] 7 additional tests: version tracking on AddIndex, pre-set version preserved, GetIndexesToBuildSince, auto-rebuild single index, no rebuild on same version, store header version updated, multi-index auto-rebuild.

6. **OnlineIndexer — BY_INDEX strategy** (MEDIUM — optimization, not essential) ✅
   - [x] Build new index from existing readable index instead of scanning all records. `SetSourceIndex(index)` on builder.
   - [x] Uses source index's `ScanIndexRecords` → update target index.
   - [x] Range tracking uses source index entry keys instead of primary keys.
   - [x] Validation: source must be READABLE VALUE index, no duplicates, single record type.
   - [x] BY_INDEX stamp with `SourceIndexSubspaceKey` + `SourceIndexLastModifiedVersion`. 7 tests.

7. **Multi-target index building** (LOW — optimization for bulk schema changes) ✅
   - [x] Build multiple WRITE_ONLY indexes in a single record scan pass. `AddTargetIndex()`/`SetTargetIndexes()` builder methods. MULTI_TARGET_BY_RECORDS stamp with sorted target names. Per-index record type filtering, per-index transaction for markReadable. Targets sorted by name for deterministic primary selection, deduplicated, validated against metadata. 10 tests.
   - [x] All target indexes share the same missing-range tracking (first index's RangeSet).

8. **Mutual/concurrent index building** (LOW — multi-process coordination)
   - [x] Multiple OnlineIndexer processes build different ranges concurrently — `SetMutualIndexing()` / `SetMutualIndexingBoundaries()`, `MUTUAL_BY_RECORDS` stamp, fragment-based prime-step iteration, two-phase FULL→ANY. 8 tests.
   - [x] Heartbeat tracking at `[9, indexSubspaceKey, 7, uuid]` — `IndexingHeartbeat` with `IndexBuildHeartbeat` proto, lease-based stale detection, `SynchronizedSessionLockedError`.
   - [x] `requireEmpty=true` prevents double-processing of ranges (already in RangeSet).
   - [x] **Blocked stamps** — `isTypeStampBlocked()` with permanent and time-expiring blocks via `block`/`blockExpireEpochMilliSeconds`/`blockID` proto fields. `BlockIndex()`/`UnblockIndex()` on OnlineIndexer. `PartlyBuiltError` on blocked stamp. 4 tests.
   - [x] **`areSimilar()` stamp comparison** — `areSimilarStamps()` compares stamps ignoring block fields via `blocklessStampOf()`. Allows resume when only block state differs. 1 test.
   - [x] **`forceStampOverwrite` policy** — `IndexingPolicy.ForceStampOverwrite` forces stamp write on fresh builds, allows overwrite on continued builds when no records scanned. `setIndexingTypeOrThrow()` implements full Java decision tree. 2 tests.
   - [x] **Method conversion on resume** — `ShouldAllowTypeConversionContinue()` on `IndexingPolicy` with `TakeoverType` enum (MultiTargetToSingle, MutualToSingle, ByRecordsToMutual). Matches Java's `IndexingPolicy.shouldAllowTypeConversionContinue()`.
   - [x] **`QueryIndexingStamps`** — Returns stamp map for all target indexes. Nil stamps returned as NONE method. 1 test.
   - [x] **`IndexBuildState`** — Status reporting: index state + records scanned (from build progress counter) + total records (from COUNT index). `LoadIndexBuildState()` on store. 2 tests.

9. **Conformance tests** (CRITICAL — must validate wire compat)
   - [x] Go saves records + Go rebuilds index → Java scans → entries match.
   - [x] Go saves records + Java rebuilds index → Go scans → entries match.
   - [x] Java saves records + Go rebuilds index → Java scans → entries match.
   - [x] Cross-rebuild: Go rebuild and Java rebuild produce identical entries.
   - [x] Go writes WRITE_ONLY records while Go builds → entries consistent. Stamp-aware resume preserves WRITE_ONLY maintenance entries during build. 5 unit tests validate resume/restart/wire-compatibility. Cross-language (Java OnlineIndexer) deferred — requires Java tenant-aware OnlineIndexer step.
   - [x] RangeSet wire format: Go writes ranges → Java reads them (and vice versa). 4 specs in rangeset_conformance_test.go.

### HIGH

- [x] **Index management store methods** — `GetIndexState`, `IsIndexReadable`, `IsIndexWriteOnly`, `IsIndexDisabled`, `IsIndexScannable`, `MarkIndexReadable`, `MarkIndexWriteOnly`, `MarkIndexDisabled`, `ClearAndMarkIndexWriteOnly`, `RebuildIndex`, `MarkIndexReadableOrUniquePending`. Still missing: `getIndexBuildStateAsync`.

- [x] **Repeated field fan-out** — `FanOut("field")` creates `FieldKeyExpression` with `FanTypeFanOut`, producing one index entry per repeated value. Cross-product with `Concat()` works. Empty repeated field → no entries (matching Java).

- [x] **Sparse/filtered indexes** — `Index.Predicate` field: function that returns true if a record should be indexed. `StandardIndexMaintainer` skips entries when predicate returns false. Matches Java's `IndexPredicate` concept.

- [x] **NULL-safe unique index checks** — Skip uniqueness check when index key contains null values. Matches Java's `indexEntry.keyContainsNonUniqueNull()` guard in `StandardIndexMaintainer.updateOneKeyAsync()`. Default `NullStandin.NULL` behavior: null key components bypass uniqueness enforcement.

- [x] **ScanIndexRecords (fetch records from index)** — `ScanIndexRecords()` on store: scans an index, extracts primary keys from entries, fetches the actual records. Returns `RecordCursor[*FDBIndexedRecord]` (wraps both IndexEntry and stored record). Orphan entries (deleted records) are skipped. Matches Java's `scanIndexRecords()` → `fetchIndexRecords()` pipeline.

### MEDIUM

- [x] **COUNT index type** — `CountIndexMaintainer` using FDB atomic ADD. Key = grouping columns only (no PK appended). Value = little-endian int64 count. `GroupingKeyExpression` with `GroupAll()` / `Ungrouped()` / `GroupBy()` factories. `getIndexMaintainer()` dispatches COUNT vs VALUE. `ScanIndex()` delegates to maintainer `Scan()`. 6 integration tests (grouped, delete decrement, update regroup, ungrouped total, range query, reverse scan).
- [x] **SUM index type** — `SumIndexMaintainer` using FDB atomic ADD. Key = grouping columns only (no PK appended). Value = little-endian int64 running sum. Extracts sum value from first grouped (trailing) column, matching Java's `AtomicMutationIndexMaintainer.updateIndexKeys()` which passes `groupedValue` to `getMutationParam()`. Null values skipped. Common-entry skip optimization (both groupKey and sumValue must match). Non-idempotent (UpdateWhileWriteOnly checks range set). 11 integration tests (ungrouped total, grouped, delete decrement, update value, update group, no-op optimization, range query, reverse scan, WRITE_ONLY range check, negative values, rebuild).
- [x] **MAX_EVER_LONG / MIN_EVER_LONG index types** — `MinMaxEverIndexMaintainer` using FDB atomic MAX/MIN. Idempotent, _EVER semantics (deletes are no-ops). Negative values rejected (unsigned comparison). 10 tests (ungrouped, grouped, delete irreversibility, update, rebuild, negatives, empty store).
- [x] **COUNT_NOT_NULL index type** — `CountNotNullIndexMaintainer` using FDB atomic ADD. Like COUNT but skips entries where key expression fields are null (unset proto2 optional). Uses `keyExpressionHasNullField()` for proto field presence detection. Non-idempotent. 6 tests.
- [x] **COUNT_UPDATES index type** — `CountUpdatesIndexMaintainer` using FDB atomic ADD. Like COUNT but deletes are no-ops (count never decrements) and `skipUpdateForUnchangedKeys=false` (always re-counts on update). Tracks total insert+update events. Non-idempotent. 6 tests.
- [x] **MIN/MAX via VALUE index** — `EvaluateAggregateFunction` supports `FunctionNameMin`/`FunctionNameMax` via VALUE indexes. Scans 1 entry forward (MIN) or reverse (MAX). Unlike _EVER variants, reflects deletes. 4 tests.
- [x] **CLEAR_WHEN_ZERO option** — `Index.SetClearWhenZero(true)` enables FDB `CompareAndClear(zero)` after every ADD decrement. Atomically removes entries when count/sum reaches zero. Works with COUNT, COUNT_NOT_NULL, SUM indexes. Matches Java's `IndexOptions.CLEAR_WHEN_ZERO`. 3 tests.
- [x] **MIN_EVER_TUPLE / MAX_EVER_TUPLE index types** — `MinMaxEverTupleIndexMaintainer` using FDB BYTE_MIN/BYTE_MAX mutations with tuple-packed values. Unlike _LONG variants, supports any tuple-encodable type including negatives. Idempotent. Reuses `countKVCursor` with `tupleValues` flag for scanning. 8 tests.
- [x] **RANK index type** — `RankIndexMaintainer` with dual subspace (B-tree + RankedSet skip-list). Wire-compatible with Java's `RankedSet`. Supports BY_VALUE and BY_RANK scans, RankForScore/ScoreForRank queries, grouped and ungrouped modes, CountDuplicates option, JDK/CRC hash functions. 23 tests (6 RankedSet + 17 RankIndex).

- [x] **RANK conformance tests** — 11 specs: BY_VALUE Go→Java/Java→Go/mixed writes, delete cross-language, update cross-language, BY_RANK scan with rank ranges cross-validated, ranked set wire compatibility (Go writes→Java reads by rank, Java writes→Go reads by rank), delete updates ranked set. Cross-validated.

- [x] **RANK aggregate functions** — `EvaluateAggregateFunction` integration for RANK indexes: `COUNT_DISTINCT` (ranked set size), `RANK_FOR_SCORE`, `SCORE_FOR_RANK`, `SCORE_FOR_RANK_ELSE_SKIP` (sentinel on OOB), `COUNT` (unique only). Auto-index-selection + `canEvaluateRankAggregate` + `expressionsEqual`. 7 tests. Record function `RANK` not yet integrated.

- [x] **RANK deleteWhere** — Fixed: `RankIndexMaintainer.DeleteWhere(prefix)` clears both B-tree (primary) and ranked set (secondary) subspaces. Implemented as part of `DeleteRecordsWhere`. **MEDIUM**.

- [x] **RANK preloadForLookup** — `PreloadForLookup()` on `rankedSet` does a single reverse `GetRange(limit=nLevels)` to warm the RYW cache with sparse upper skip-list levels. Called before `Rank()`/`GetNth()` in `RankIndexMaintainer`, `evaluateRankAggregate`, and `timeWindowLeaderboardMaintainer`. Eliminates serial FDB round trips for cached upper levels. Matches Java's `RankedSet.preloadForLookup()`.

- [x] **RANK OnlineIndexer test coverage** — 4 tests: basic build, chunked build (limit=3), post-build maintenance, duplicate scores. Covers RANK index through OnlineIndexer path. **MEDIUM**.

- [x] **RANK reverse BY_RANK scan** — tested, works correctly (rank→score conversion + reverse standard scan). **LOW**.

- [x] **RANK continuation tokens** — tested paginated BY_RANK scan with limit 2, 3 pages. Works through standard cursor path. **LOW**.

- [x] **All 19 index types implemented** — VALUE, COUNT, COUNT_NOT_NULL, COUNT_UPDATES, SUM, MAX_EVER_LONG, MIN_EVER_LONG, MAX_EVER_TUPLE, MIN_EVER_TUPLE, RANK, VERSION, MAX_EVER_VERSION, PERMUTED_MIN, PERMUTED_MAX, BITMAP_VALUE, TEXT, TIME_WINDOW_LEADERBOARD, MULTIDIMENSIONAL, VECTOR.

- [x] **TEXT index audit items (LOW)** — All items from 2026-03-18 audit complete:
  - [x] `commonKeys` deduplication in text update path — `removeCommonTextEntries()` skips unchanged text on update
  - [x] Pipeline parallelism for multi-token updates — assessed: Go's per-index write lock serializes all token updates; Java also serializes multi-entry updates. No benefit from pipelining.
  - [x] `canDeleteWhere` validation — rejects non-empty prefix on non-grouped TEXT indexes
  - [x] `BunchedMap.Get()` read conflict key — fixed: `Get()` now takes `fdb.Transaction`, `entryForKey()` unconditionally adds read conflict key matching Java's "Grand Theory of Conflict Ranges"
  - [x] InstrumentedBunchedMap for timer/metrics — `NewInstrumentedBunchedMap` wraps with StoreTimer hooks (write/delete/read counters). 9 index-level counter events. `textIndexMaintainer` auto-instruments when context has timer. 4 tests.
  - [x] BunchedMap `compact()` / `containsKey()` / single-map `Scan()` — implemented with 12 tests (ContainsKey: 3, Compact: 4, Scan: 5)
  - [x] ByteScanLimiter in TextCursor — KVCallback on streaming iterator fires per raw FDB KV read, textCursor checks ScannedBytesLimit. 2 tests.
  - [x] BunchedMapMultiIterator eager materialization vs streaming — converted to lazy streaming via fdb.RangeIterator (one bunch in memory at a time)

- [x] **VERSION index type** — HIGH. Two phases:

  **Phase 1: Widen `KeyExpression.Evaluate()` signature** (prerequisite)
  - [x] Change `Evaluate(proto.Message)` → `Evaluate(*FDBStoredRecord[proto.Message], proto.Message)` across all expression types
  - Decision: Option 1 (match Java's `evaluateMessage(FDBRecord, Message)` exactly — two params). `record` = top-level context (version etc), `msg` = current message (changes during nesting).
  - [x] Update all call sites: index maintainers pass `(record, record.Record)`, message-only callers pass `(nil, msg)`
  - [x] NestingKeyExpression preserves `record` context while changing `msg` to sub-message (matching Java)
  - [x] All 8 expression types updated: `FieldKeyExpression`, `RecordTypeKeyExpression`, `EmptyKeyExpression`, `CompositeKeyExpression`, `NestingKeyExpression`, `GroupingKeyExpression`, `LiteralKeyExpression`, `KeyWithValueExpression`
  - [x] All 957 existing tests pass unchanged

  **Phase 2: VersionKeyExpression + VERSION index maintainer**
  - [x] `VersionKeyExpression` type: `Evaluate()` reads `record.Version` → returns `tuple.Versionstamp` as key component
  - [x] `VersionIndexMaintainer`: incomplete versionstamps use `SET_VERSIONSTAMPED_KEY` mutation, complete use normal `set()`. Delete: incomplete → `RemoveVersionMutation`, complete → `Clear`.
  - [x] `AddVersionMutation` extended with `VersionMutationType` (KEY vs VALUE) matching Java's `FDBRecordContext.addVersionMutation(MutationType, key, value)`
  - [x] `SaveRecord`/`DeleteRecord` update path: load version for old record when VERSION index exists via `hasVersionIndex()` check
  - [x] Wire format: version stored as Versionstamp in tuple-encoded key (matches Java)
  - [x] Proto serialization: `Version` message in `KeyExpression` proto (roundtrip tested)
  - [x] Conformance tests (VERSION index Go↔Java cross-validation) — 7 specs: Go writes/both scan, Java writes/both scan, mixed writes, cross-language delete (2 specs), cross-language update, same-tx local versions. Uses hex-encoded versionstamp bytes for wire comparison.

- [x] **Uniqueness violation tracking** — `ScanUniquenessViolations()` scans `IndexUniquenessViolationsKey` (7) subspace. `ResolveUniquenessViolation()` removes a single entry. Violations written on unique index save failure.

- [x] **Index validation** — `ValidateIndex()` scans all records and index entries to detect orphaned entries (in index but not in records) and missing entries (in records but not in index).

- [x] **Primary key component deduplication** — `primaryKeyComponentPositions` computed at `Build()` time via `buildPrimaryKeyComponentPositions()`. `indexEntryKey()` calls `trimPrimaryKey()` to omit PK components already in the index key. `getEntryPrimaryKey()` reconstructs the full PK on read. Wire-compatible with Java. Conformance-tested: Go writes → Java scans, Java writes → Go scans, cross-write. 3 conformance specs + 15 unit tests.

- [x] **Bulk index delete** — `DeleteIndexEntries()` clears all entries for a given index. `DeleteIndexEntriesInRange()` clears entries within a tuple range.

- [x] **Aggregate functions via indexes** — `EvaluateAggregateFunction()` on store with auto-index-selection. Supports COUNT, COUNT_NOT_NULL, COUNT_UPDATES, SUM, MIN_EVER, MAX_EVER via atomic mutation indexes, plus MIN/MAX via VALUE indexes. `IndexAggregateFunction` type with name, operand, optional explicit index. `canEvaluateAggregate()` / `isGroupPrefix()` for index matching. 15 tests.

---

## Metadata — conformance gaps

### HIGH

- [x] **ThenKeyExpression** — `CompositeKeyExpression` via `Concat()` now computes Cartesian cross-product matching Java's `ThenKeyExpression` semantics.

- [x] **NestingKeyExpression** — `Nest("field", child)` navigates into nested message fields. `NestFanOut("field", child)` for repeated message fields. Composite nested fields work (e.g., `Nest("flower", Concat(Field("type"), Field("color")))`). Enum fields supported via `int64` conversion.

- [x] **FormerIndex tracking** — `FormerIndex` struct with `SubspaceKey`, `AddedVersion`, `RemovedVersion`, `FormerName`. `RemoveIndex()` on builder creates FormerIndex and removes from all record types. `Build()` validates no subspace key reuse. `GetFormerIndexes()` on metadata.

- [x] **Schema evolution validation** — `MetaDataEvolutionValidator` with builder pattern matching Java's. Validates: version ordering, split record changes, record type preservation (PK immutability, type key immutability), index lifecycle (type/expression/version immutability, FormerIndex tracking), message descriptor evolution (field removal, rename, type change, cardinality change, enum value removal, safe int32→int64 promotion), new required field rejection. 7 configurable options (allowNoVersionChange, allowIndexRebuilds, allowUnsplitToSplit, etc.). 23 tests.

### MEDIUM

- [x] **Metadata proto serialization** — Java has `toProto()`/`fromProto()` for persisting metadata definitions. Implemented in Go.
  - [x] **KeyExpression proto serialization** — `ToKeyExpression()` on all expression types + `KeyExpressionFromProto()` dispatcher. Roundtrip + wire format tests. Matches Java's `KeyExpression.toKeyExpression()`/`fromProto()`. FanType mapping: Go None→SCALAR, FanOut→FAN_OUT, Concatenate→CONCATENATE.
  - [x] **RecordMetaData.toProto()/fromProto()** — `ToProto()` serializes metadata (file descriptor, dependencies, indexes with record type associations, record types with primary keys, former indexes, flags). `RecordMetaDataFromProto()` rebuilds from proto with topological dependency resolution. Index subspace keys tuple-packed. Explicit record type keys via Value proto. Wire roundtrip tested.

- [x] **Explicit record type keys** — `SetRecordTypeKey()` on `RecordTypeBuilder`, `GetRecordTypeKey()` on `RecordType`. Falls back to `RecordTypeIndex` if not set.

- [x] **Multi-type indexes** — `AddMultiTypeIndex(recordTypeNames, index)`. 0 types → universal, 1 type → single-type, 2+ types → multi-type (stored per RecordType, included in `GetIndexesForRecordType`). Matches Java semantics.

- [x] **Schema evolution version tracking** — `SetVersion()` on builder sets metadata version. Used in store header for compatibility tracking.

- [x] **Primary key prefix checking** — `PrimaryKeyHasRecordTypePrefix()` on `RecordMetaData`. Checks all record types' primary keys start with `RecordTypeKeyExpression`, including through `CompositeKeyExpression`.

### LOW

- [ ] **Missing key expression types** — AtomKE (LOW, Java interface only). Done: GroupingKE, LiteralKE, KeyWithValueKE, VersionKE, FunctionKE, SplitKE, ListKE, LongArithmeticKE, DimensionsKE, OrderFunctionKE, CollateFunctionKE. See 4.10.6.0 upgrade assessment §3.

- [ ] **Synthetic record types** — Computed/joined/unnested record types. Large feature.

- [ ] **User-defined functions** — `userDefinedFunctionMap` for custom query functions.

- [ ] **Views** — Named query/aggregation views.

- [x] **Subspace key counter** — `EnableCounterBasedSubspaceKeys()` on builder. Auto-assigns incrementing int64 subspace keys to indexes instead of using index name strings.

- [ ] **Extension options processing** — Processing protobuf schema extension options.

---

## Cursor — conformance gaps

### HIGH

- [x] **ExecuteProperties `skip` field** — `ExecuteProperties.Skip` skips N records before applying row limit. FDB-level limit accounts for skip. Tested with skip-only and skip+row limit.

- [x] **ScannedRecordsLimit** — `ExecuteProperties.ScannedRecordsLimit` enforced in `keyValueCursor.OnNext()`. Returns `ScanLimitReached` with continuation when limit hit.

- [x] **Cursor factory methods** — `Empty[T]()` and `FromList[T](items)` implemented matching Java's `RecordCursor.empty()` and `RecordCursor.fromList()`.

- [x] **RecordCursorResult validation** — `GetValue()` panics on `!HasNext()` matching Java's `IllegalResultValueAccessException`. `HasStoppedBeforeEnd()` helper added.

### MEDIUM

- [ ] **Cursor combinators** — Java has 20+ cursor combinator types. Implemented in Go:
  - [x] `ConcatCursor` — sequential concatenation with proto-wrapped continuations
  - [x] `MapCursor` (MapResultCursor) — value transformation preserving continuations
  - [x] `Empty`, `FromList`, `FromListWithContinuation`, `Filter`, `Skip`, `LimitRows`, `SkipThenLimit`, `OrElse` — basic utilities
  - [x] **Set operations**: `UnionCursor` (ordered merge-union with deduplication), `IntersectionCursor` (ordered merge-intersection). Both support forward/reverse, proto-wrapped continuations, multi-cursor (3+). `ComparisonKeyFunc` for custom comparison keys.
  - [x] `DedupCursor` — adjacent duplicate removal with proto-wrapped `DedupContinuation`. Custom equal/pack/unpack functions.
  - [x] `FlatMapPipelinedCursor` — flat-map with proto-wrapped `FlatMapContinuation`, check value support
  - [x] `ChainedCursor` — procedural iterator with generator function. Raw byte continuations (no proto). Custom encode/decode.
  - [ ] **Aggregation**: `AggregateCursor` with accumulator states
  - [x] `AutoContinuingCursor` — auto-creates new transactions on scan/time/byte/row limits for seamless large-dataset scanning across tx boundaries. Includes retry logic for transient errors.
  - [x] `FallbackCursor` — primary cursor with automatic failover on error. One-shot fallback, passes last successful result to factory.
  - [x] `MapErrCursor` — fallible transform combinator (fn returns (R, error)). 3 tests.
  - [x] `AsListWithContinuation` — pagination helper: drains cursor to slice, returns continuation bytes. 3 tests.

- [ ] **CursorLimitManager** — Java has a separate class for comprehensive limit tracking (record scan, byte scan, time). Go has inline limit logic in keyValueCursor.

- [x] **RecordCursor instance methods** — `First()`, `GetCount()`, `Reduce()` as standalone generic functions. `SkipCursor()`, `LimitRowsCursor()` as cursor wrappers. Matches Java's `first()`, `getCount()`, `reduce()`, `skip()`, `limitRowsTo()`.

### LOW

- [ ] **Visitor pattern** — Java has `RecordCursorVisitor` interface for cursor inspection/instrumentation.
- [x] **Continuation SerializationMode** — Go uses TO_OLD (raw bytes) for writing, accepts both TO_OLD and TO_NEW (proto-wrapped) for reading. Confirmed working with Java Record Layer 4.10.6.0 (all conformance tests pass).

---

## Store — conformance gaps

### HIGH

- [x] **Store state management** — `GetRecordStoreState()` returns store header + index states. `SetStoreLockState()` persists lock state to header. `ReloadRecordStoreState()` forces reload from FDB.

- [x] **DeleteRecordsWhere** — `DeleteRecordsWhere(prefix)` bulk-deletes all records with a PK prefix via range clears (no scanning). Clears records, versions, record counts, and all index entries. Type-specific indexes cleared entirely; universal indexes require aligned leading expression. `DeleteWhere(prefix)` on `IndexMaintainer` interface. RANK indexes clear both B-tree and ranked set subspaces. 10 unit tests + 5 conformance specs (Go deletes/Java verifies, Java deletes/Go verifies, mixed writes, delete+reinsert, Java-written records).

- [x] **Query execution methods** — `EvaluateStoreFunction()` for store-level functions (VERSION), `EvaluateAggregateFunction()` for index aggregates, `EvaluateRecordFunction()` for index record functions. All matching Java's dispatch hierarchy.
  - [x] `CountRecords(ctx, low, high, lowEndpoint, highEndpoint, continuation, scanProperties)` — scan-based record count (not atomic counter). Matches Java's `FDBRecordStore.countRecords()`.
  - [x] `EvaluateRecordFunction(fn, record)` — evaluates index record functions (e.g. RANK) for a specific record. Auto-selects best index. 5 tests.
  - [x] `EvaluateStoreFunction(fn, record)` — evaluates store-level functions. VERSION function returns record version from store context. 6 tests.

- [x] **Per-type record count** — `GetSnapshotRecordCountForRecordType(recordTypeName)` added. Requires `RecordTypeKeyExpression` as count key. Matches Java's `getSnapshotRecordCountForRecordType()`.

### MEDIUM

- [x] **Store statistics** — `EstimateStoreSize()`, `EstimateRecordsSize()`, `EstimateRecordsSizeInRange(TupleRange)`, `EstimateIndexSize(*Index)`, `GetRangeSplitPoints(chunkSize)` using FDB native operations. `TupleRange.ToFDBRange(subspace)` conversion. `FDBRecordContext.GetApproximateTransactionSize()` for 10MB limit monitoring. 12 tests.

- [x] **Format version / user version access** — `GetFormatVersion()`, `GetUserVersion()`, `SetUserVersion()`, `GetMetaDataVersion()`. Persisted in store header.

- [x] **Serializer access** — `GetMetaData()`, `GetIndexMaintainer()` on store. `Context()` and `Subspace()` already exposed.

- [x] **Conformance test for type-changed existence check** — All 5 modes tested including cross-type Order→Customer tests for `ERROR_IF_RECORD_TYPE_CHANGED` and `ERROR_IF_NOT_EXISTS_OR_RECORD_TYPE_CHANGED`.

### LOW

- [x] **Store API surface expansion** — 13 new public methods matching Java: `RecordsSubspace`, `IndexSubspace`, `IndexSecondarySubspace`, `GetReadableIndexes`, `GetEnabledIndexes`, `GetAllIndexStates`, `RebuildAllIndexes`, `VacuumReadableIndexesBuildData`, `DeleteStore`, `FirstUnbuiltRange`, `IsCacheable`, `GetStoreHeader`, `GetAllIndexStatesMap`. 15 tests.
- [x] **Advanced store operations** — `DryRunSaveRecord`, `DryRunDeleteRecord`, `ScanRecordKeys`, `IsIndexReadableUniquePending`, `GetWriteOnlyIndexes`, `GetDisabledIndexes`, `GetIndexesToBuildSince`, `ResolveUniquenessViolationByDeletion`, `ScanUniquenessViolationsForValue`. 24 tests.
- [ ] **Remaining advanced store operations** — Java has `preloadRecordAsync()`, `repairRecordKeys()`. Not yet ported.

- [ ] **Synthetic records** — Java has `loadSyntheticRecord()`. Large feature tied to synthetic record types.

---

## Database / Transaction — conformance gaps

### HIGH

- [x] **FDBDatabaseRunner** — `FDBDatabaseRunner` with `MaxAttempts`, `InitialDelay`, `MaxDelay`, exponential backoff. `RunWithRetry()` wraps transaction execution with configurable retry. Falls back to FDB's native retry when config is nil.

- [x] **FDBRecordContextConfig** — `RecordContextConfig` with `TransactionTimeout`, `Priority`, `TransactionID`. Applied in `Run()`/`RunWithRetry()`.

- [x] **Commit hooks** — `AddCommitCheck()` for pre-commit consistency checks, `AddPostCommit()` for post-commit callbacks. Run in `flushAndCommit()`. Matches Java's `CommitCheckAsync` and `PostCommit` interfaces.

### MEDIUM

- [x] **Timer / instrumentation** — `StoreTimer` with `Event`/`Counter`/`CounterSnapshot` types, nil-safe, goroutine-safe (atomic counters + sync.Map). 9 timed events (Save/Load/Delete/Commit/OpenStore/etc) + 9 count events (key/byte counts). Wired into `FDBRecordContext.Timer()`, `SaveRecordWithOptions`, `LoadRecord`, `DeleteRecord`, `Create/Open/CreateOrOpen`. 32 specs (unit + integration).

- [x] **Transaction priority** — `TransactionPriority` type with `PriorityDefault`, `PriorityBatch`, `PrioritySystemImmediate`. `SetTransactionPriority()` on `FDBRecordContext`.

- [x] **Store state caching** — `MetaDataVersionStampStoreStateCache` + `PassThroughRecordStoreStateCache`. LRU+TTL, `\xff/metadataVersion` invalidation. 40 specs, 2.2x speedup.

- [x] **Read/write version management** — `GetReadVersion()`, `SetReadVersion()` on `FDBRecordContext`. Wraps FDB transaction read version.

- [x] **Conflict key reporting** — `GetConflictingKeys()` on `FDBRecordContext` wraps FDB's conflict range reporting for debugging.

### LOW

- [ ] **FDBDatabaseFactory** — Factory/pooling for database instances.
- [ ] **Weak read semantics** — `WeakReadSemantics` for causal read risky, version staleness bounds.
- [ ] **Directory layer caching** — Multi-tenant keyspace management.
- [ ] **Transaction ID / MDC / logging** — Transaction tracing and structured logging.
- [ ] **Latency injection** — `FDBLatencySource` for testing.

---

## Record versioning — conformance gaps

### MEDIUM

- [x] **Version comparison/ordering** — `Equal()`, `Less()` implemented matching Java's `equals()`/`compareTo()`.

- [x] **Version range methods** — `FirstInDBVersion()`, `LastInDBVersion()`, `FirstInGlobalVersion()`, `LastInGlobalVersion()`, `Next()`, `Prev()`. All matching Java semantics.

- [x] **MIN_VERSION / MAX_VERSION constants** — `MinVersion()` (all zeros), `MaxVersion()` fixed to match Java: bytes 0-8 = 0xFF, byte 9 = 0xFE, bytes 10-11 = 0xFF. Was incorrectly all-0xFE.

### LOW

- [x] **Versionstamp conversion** — `FromVersionstamp()` creates FDBRecordVersion from FDB Versionstamp. `ToVersionstamp()` converts back. Matches Java API.

---

## Behavioral compatibility gaps (found in 2026-03-09 audit)

### CRITICAL

- [x] **updateSecondaryIndexes doesn't handle cross-type overwrites** — Fixed: three-way index partition (old-only/new-only/common) matching Java's `updateSecondaryIndexes()`. Old-type-only index entries are deleted, new-type-only entries are inserted, common entries are updated. 4 tests: cross-type overwrite, round-trip back, same-type sanity, cross-type delete.

- [x] **Stale metadata detection missing** — Fixed: `checkPossiblyRebuild` now returns `StaleMetaDataVersionError` when stored version > local version, matching Java's `RecordStoreStaleMetaDataVersionException`. Also fixed `SetSplitLongRecords`, `SetStoreRecordVersions`, and `SetRecordCountKey` to bump metadata version when value changes, matching Java. 4 tests.

- [x] **Unique index pre-commit check missing** — Fixed: `checkUniqueness` now reads the full prefix range (removed `Limit:1`) so FDB's read-conflict tracking covers the entire index value range. With `Limit:1`, FDB only tracked conflicts up to the first key found, allowing concurrent inserts at higher keys. Now matches Java's `StandardIndexMaintainer.checkUniqueness()` which also reads the full range. 3 tests: concurrent same-key rejection, concurrent different-key success, sequential uniqueness enforcement.

### HIGH

- [x] **COUNT index UpdateWhileWriteOnly skips range set check** — Fixed: `UpdateWhileWriteOnly` now checks `IndexingRangeSet.ContainsKey()` before updating, matching Java's `StandardIndexMaintainer.updateWriteOnlyByRecords()`. Only updates if PK is in the already-built range. Added `isKeyInIndexBuildRange()` to `indexStoreContext`. 4 tests.

- [x] **Record count rebuild on metadata version change** — Fixed: `checkPossiblyRebuildRecordCounts()` compares stored `RecordCountKey` proto against current metadata, independent of version numbers. Clears old counts, rescans all records, updates store header. Runs before the version-gated index rebuild, matching Java's `checkRebuild()` flow. 4 tests: add key, change key, remove key, unchanged key no-op.

- [x] **validateRecordUpdateAllowed timing differs** — Fixed: moved `validateRecordUpdateAllowed()` after record load and existence checks, before write. Now existence/type errors take precedence over lock errors, matching Java's `saveRecordAsync()` and `deleteTypedRecord()`. Delete of non-existent record returns `(false, nil)` even when locked. 2 tests.

- [x] **clearIndexData uses subspace.Range() which misses prefix key** — Fixed: `clearIndexData()` for the index entries subspace now uses `fdb.PrefixRange()` instead of `ClearRange(subspace)`. Go's `subspace.FDBRangeKeys()` returns `[prefix\x00, prefix\xff)` which excludes the exact prefix key. Ungrouped aggregate indexes (COUNT/SUM) store data at the subspace prefix itself (Pack of empty tuple = prefix bytes). Java explicitly uses `Range.startsWith(indexSubspace.pack())` with the comment "startsWith to handle ungrouped aggregate indexes". Found during SUM index rebuild testing.

### MEDIUM

- [x] **Key/value size validation missing on index entries** — Fixed: `checkKeyValueSizes()` validates FDB key (10KB) and value (100KB) limits before writing index entries. Returns `IndexKeySizeError`/`IndexValueSizeError` with index name, primary key, and sizes. Applied in both `StandardIndexMaintainer.Update()` and `CountIndexMaintainer.Update()`. 1 test.

- [x] **COUNT index doesn't skip common grouping keys on update** — Fixed: `CountIndexMaintainer.Update()` now calls `removeCommonGroupingKeys()` to filter unchanged grouping keys before applying -1/+1 atomic mutations. Matches Java's `AtomicMutationIndexMaintainer.updateIndexKeys()` common key filtering.

- [x] **COUNT index conformance tests** — 6 conformance specs: Go writes→both scan, Java writes→both scan, mixed writes combined counts, Go deletes Java-written record, Java deletes Go-written record, update moves counts. Java uses `new GroupingKeyExpression(field("price"), 0)` matching Go's `GroupAll(Field("price"))`.

---

## Go style issues (found in 2026-03-09 audit)

### HIGH

- [x] **RecordCursor interface too wide (5 methods)** — Fixed: slimmed to 2 methods (`OnNext` + `Close`). `Seq`/`Seq2`/`SeqWithContinuation` are now package-level generic functions. Removed 63 identical method implementations across 21 cursor types. Net -900 lines.

- [x] **Panics in library code** — Fixed: converted 5 `FDBRecordVersion` panics to error returns (`GetGlobalVersion`, `GetDBVersion`, `Next`, `Prev`, `ToVersionstamp`). `RecordCursorResult.GetValue()` kept as panic — programming error (matches Java's `IllegalResultValueAccessException`).

### MEDIUM

- [x] **sync.Map misuse in FDBRecordContext** — Fixed: replaced `sync.Map` with plain `map` and `atomic.Int32` with `int32`. `HasVersionMutations()` now uses `len()`.

- [x] **Silent error swallowing in addRecordCount** — Fixed: `addRecordCount()` now returns `error` and callers propagate it. No more silent swallowing.

- [x] **recover() removed from key_value_cursor.go** — Root-caused FDB Go bindings bug: `RangeIterator.Advance()` returns true on empty batch (missing `ri.done = true`), causing `Get()` to panic with index OOB. Fixed upstream via Bazel patch (`patches/fdb-go-range-iterator-done.patch`). No workarounds in our code.

- [x] **store.go too large (2004 lines)** — Split into `store.go` (1134, core CRUD/scanning/state), `store_builder.go` (549, builder/lifecycle/rebuild), `store_typed.go` (228, TypedFDBRecordStore), `store_version.go` (115, version management).

- [x] **cursor.go split** — Split 1202→3 files: `cursor.go` (286, interfaces), `cursor_combinators.go` (735, combinators), `cursor_util.go` (195, utilities).

- [x] **NewRecordMetaData discards Build() error** — Fixed: removed the function entirely. Callers should use `NewRecordMetaDataBuilder()` and `Build()` for proper error handling.

### STYLE (LOW)

- [x] **Get prefix on ~30 trivial accessors** — WONTFIX: keeping `Get` prefix for 1:1 Java naming correspondence. `GetRecordCount()` maps directly to Java's `getRecordCount()`. Go convention sacrificed for porting clarity.

- [x] **interface{} → any** — Fixed: replaced all 524 occurrences of `interface{}` with `any` across 72 files.

---

## Split records — conformance status

**FULLY CONFORMANT.** No gaps found. All constants, save/load/delete/exists methods, SizeInfo tracking, cursor integration, and edge cases match Java behavior. Wire-compatible. Version handling separated into store layer (architectural choice, not a gap).

---

## Infrastructure

- [x] Bazel migration, nogo linting, CI pipeline, justfile — all done
- [ ] **KeySpace/KeySpacePath** — Enterprise key management. LOW priority.
- [x] **ScanLimiter** — TimeScanLimiter, ByteScanLimiter, RecordScanLimiter all enforced in both `keyValueCursor` and `indexCursor`. Time limit uses free initial pass (first record always succeeds). Continuation returned for cross-transaction resumption.

### HIGH — Conformance test restructure

- [x] **Remove Gradle, make conformance fully Bazel-native** — Killed Gradle, flattened `conformance/java/` and `conformance/helpers/` into single `conformance/` directory. Split monolithic ConformanceSteps.java into 22 per-feature step classes with `@ConformanceStep` annotation dispatch. Added auto-rebuild conformance tests exercising `checkPossiblyRebuild()` without `ALWAYS_READABLE_CHECKER`. Removed force-set of IDs after `mergeFrom` in load steps. 211 conformance specs, single BUILD.bazel, zero external tooling.

---

## Test quality gaps (identified 2026-03-10 audit)

### MEDIUM

- [x] **Error path test coverage weak** — Added `error_path_test.go` with 41 specs covering: unique index violation errors (READABLE), IndexValueSizeError/IndexKeySizeError (was 0 tests), key expression validation errors (field not found, FanTypeNone on repeated, nil message, nesting into nil/nonexistent), RangeSet validation (empty key, key too large, inverted range, MissingRanges empty key), ErrRecordStoreStateNotLoaded (SetUserVersion/SetStoreLockState/UpdateRecordCountState), SaveRecord validation (all 5 existence check modes, lock precedence, unknown type, cross-type overwrite), store builder errors (reload non-existent), metadata build errors (missing PK, FormerIndex subspace reuse), error message format assertions, delete error paths. Total unit specs: 624 (was 583).
- [x] **Atomic index maintainer code duplication** — Extracted `indexGroupingCount()`, `evaluateGroupingKeys()`, and `updateWhileWriteOnlyNonIdempotent()` into `atomic_index_helpers.go`. Removed 184 lines of identical code across 6 maintainer files. Remaining per-maintainer logic (mutation semantics, entry types) is genuinely unique.

### LOW

- [x] **`existence_check.go` only 1 of 4 enum values tested** — Actually all 5 values were already tested in `existence_test.go` (ERROR_IF_EXISTS, ERROR_IF_NOT_EXISTS, ERROR_IF_TYPE_CHANGED, ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED). Additional coverage added in `error_path_test.go`.
- [x] **`indexing_range_set.go` dedicated unit tests** — 10 specs in `indexing_range_set_test.go`: empty/full/contains/tuple-packed/first-missing/nil-when-complete/multiple-gaps/clear/requireEmpty-overlap/incremental-build-simulation.
- [x] **Scan limit boundary tests** — 18 specs in `scan_limit_test.go`: byte limit (1-byte, partial, resume, no-limit), scanned records limit (exact, limit-of-1), row limit with SourceExhausted. Also fixed byte scan limit bug: was post-read (discarding boundary record), now pre-read matching Java's CursorLimitManager. Fixed in both keyValueCursor and indexCursor.
- [x] **cursor.go `NoNextReason` helpers tested** — Dedicated specs for all 5 NoNextReason values testing IsOutOfBand/IsSourceExhausted/IsLimitReached, plus 6 specs for RecordCursorResult.HasStoppedBeforeEnd.

---

## Bugs found by edge-case audit (2026-03-10)

All 27 bugs verified by dedicated subagents with reproducing tests (2026-03-10).
Data loss bugs marked **[DATA LOSS 2x]**. Worktree paths relative to `.claude/worktrees/`.

### Cursor combinators — verified in `agent-adb21082`, fixed

- [x] **[DATA LOSS 2x] UnionCursor continues after child hits limit** — Fixed: stop union when any child has OOB limit. File: `merge_cursor.go`.
- [x] **[DATA LOSS 2x] LimitRowsCursor returns EndContinuation (un-resumable)** — Fixed: preserve inner continuation on limit. File: `cursor.go`.
- [x] **[DATA LOSS 2x] OrElseCursor switches to alternative on out-of-band limits** — Fixed: stay UNDECIDED on OOB limits. File: `cursor.go`.
- [x] **[DATA LOSS 2x] IntersectionCursor.weakestNoNextReason() always returns SourceExhausted** — Fixed: proper NoNextReason comparison. File: `merge_cursor.go`.

### Key expressions — verified in `agent-a9e81304`, fixed

- [x] **[DATA LOSS 2x] FieldKeyExpression.Evaluate returns default for unset proto2 fields** — Fixed: check `m.Has(fd)` for proto2 optional, return nil. File: `key_expression.go`.
- [x] **[DATA LOSS 2x] FieldKeyExpression nil message ignores FanType** — Fixed: FanOut returns empty, Concatenate returns `[[[]]]`. File: `key_expression.go`.
- [x] **NestingKeyExpression.Evaluate panics on nil message** — Fixed: nil check returns `[[nil]]`. File: `key_expression.go`.
- [x] **RecordTypeKeyExpression.Evaluate panics on nil message** — Fixed: nil check returns `[[nil]]`. File: `key_expression.go`.

### Record version / context — verified in `agent-a28fc2d7`, fixed

- [x] **FDBRecordVersion.Next()/Prev() no carry across 12 bytes** — Fixed: full 12-byte big-endian carry/borrow. File: `record_version.go`.
- [x] **NewCompleteVersion accepts all-0xFF global version** — Fixed: reject incomplete marker bytes. File: `record_version.go`.
- [x] **WithCommittedVersion on already-complete version** — Fixed: error on already-complete. File: `record_version.go`.
- [x] **[DATA LOSS 2x] CommitWithVersionstamp skips pre-commit checks and post-commit hooks** — Fixed: run pre-commit checks + post-commit hooks. File: `database.go`.

### Store CRUD / split records — verified in `agent-af7e30fd`, fixed

- [x] **SaveRecordWithOptions swallows deserialization errors** — Fixed: propagate deser error in ErrorIfTypeChanged path. File: `store.go`.
- [x] **[DATA LOSS 2x] DeleteRecord destroys data before deserialization check** — Fixed: deserialize BEFORE deleteSplit. File: `store.go`.
- [x] **[DATA LOSS 2x] FDB row limit premature exhaustion with versioning** — Fixed: double FDB limit when IsStoreRecordVersions. File: `key_value_cursor.go`.
- [x] **[DATA LOSS 2x] keyValueCursor exclusive low endpoint uses append(0x00)** — Fixed: use fdb.Strinc(). File: `key_value_cursor.go`.

### Metadata / schema evolution — verified in `agent-a826ca49`, fixed

- [x] **RemoveIndex doesn't increment version** — Fixed: pre-increment version before setting RemovedVersion. File: `metadata.go`.
- [x] **[DATA LOSS 2x] checkPossiblyRebuild doesn't clean up former index data** — Fixed: removeFormerIndexData() clears 6 subspaces. File: `store_builder.go`, `index_state.go`.
- [x] **MetaDataEvolutionValidator rejects index changes with allowIndexRebuilds=true** — Fixed: early return when allowIndexRebuilds && lastModifiedVersion changed. File: `metadata_evolution_validator.go`.
- [x] **validateFormerIndexes: missing unconditional check + wrong operator** — Fixed: unconditional `>` check + conditional `!=`. File: `metadata_evolution_validator.go`.
- [x] **createStoreHeader doesn't persist RecordCountKey** — Fixed: include RecordCountKey in header. File: `store_builder.go`.

### Index maintainers — verified in `agent-a60827f1`, fixed

- [x] **checkUniqueness compares trimmed PK with full PK** — Fixed: use getEntryPrimaryKey() for full PK reconstruction. File: `index_maintainer.go`.
- [x] **[DATA LOSS 2x] checkUniqueness violation entries: double-trimmed PK** — Fixed: same getEntryPrimaryKey() fix resolves both issues. File: `index_maintainer.go`.
- [x] **[DATA LOSS 2x] CountNotNull keyExpressionHasNullField missing NestingKeyExpression** — Fixed: added NestingKeyExpression case. File: `count_not_null_index_maintainer.go`.

### OnlineIndexer — verified in `agent-a3134e5b`
Test file: `agent-a3134e5b/pkg/recordlayer/online_indexer_bug_verify_test.go`

- [x] **[DATA LOSS 2x] OnlineIndexer double-counts boundary records** — Fixed: use Java's `limit+1` look-ahead pattern. Request limit+1 records, index only the first limit, use the (limit+1)th record's PK as the exclusive range boundary. Boundary records never re-scanned. File: `online_indexer.go`.
- [x] **[DATA LOSS 2x] OnlineIndexer skips records when type filter exhausts limit** — Fixed: track `scannedCount` across ALL records (not just indexed ones). Type-filtered records still advance the scan position via the limit+1 look-ahead. File: `online_indexer.go`.

### Bug hunt scoreboard

27 bugs found, 27 fixed. 16 classified as data loss (2x). Current: 949 unit/integration specs, 341 conformance specs (1290 total).

| Agent | Worktree | Bugs | 1x | 2x | Award |
|-------|----------|------|----|----|-------|
| Cursor combinators | `agent-adb21082` | 4 | 0 | 4 | $800 |
| Key expressions | `agent-a9e81304` | 4 | 2 | 2 | $600 |
| Record version/context | `agent-a28fc2d7` | 4 | 3 | 1 | $500 |
| Store CRUD/split | `agent-af7e30fd` | 4 | 1 | 3 | $700 |
| Metadata evolution | `agent-a826ca49` | 5 | 4 | 1 | $600 |
| Index maintainers | `agent-a60827f1` | 3 | 1 | 2 | $500 |
| OnlineIndexer | `agent-a3134e5b` | 2 | 0 | 2 | $400 |
| **Total** | | **27** | **11** | **16** | **$4,100** |

---

## Bug bounty round 2 (2026-03-13)

Second audit focused on arithmetic overflow, off-by-one, error handling, nil safety, and retry semantics. 20 bugs found across 4 test files, all fixed.

### P0 — data loss

- [x] **Empty PK allows range-clearing all records** — `saveWithSplit`/`deleteSplit`/`clearRecordKeyRange` now reject empty primary keys. File: `split_helper.go`.
- [x] **EmptyKeyExpression accepted as primary key** — `Build()` now rejects PK expressions producing 0 columns. File: `metadata.go`.
- [x] **normalizeKeyForPositions missing GroupingKeyExpression** — `DeleteRecordsWhere` failed on universal COUNT indexes. Fixed: delegate to `wholeKey`. File: `key_expression.go`.
- [x] **SUM index negation overflow on MinInt64** — `-math.MinInt64 == math.MinInt64` in two's complement. Now returns error. File: `sum_index_maintainer.go`.

### P1 — incorrect behavior

- [x] **isRetryableError uses type assertion, not errors.As** — Wrapped FDB errors not detected as retryable. Fixed: `errors.As()`. File: `runner.go`.
- [x] **ByteScanLimit off-by-one (> vs >=)** — Allowed one extra record when `bytesScanned == limit`. Fixed in `key_value_cursor.go` and `index_scan.go`.
- [x] **FDB limit overflow: math.MaxInt + 1** — Wraps to MinInt. Added guard in `key_value_cursor.go`, `index_scan.go`, `count_index_maintainer.go`.
- [x] **OnlineIndexer recordsProcessed not reset on retry** — Inflated counts after FDB transaction conflict. Fixed: reset at top of closure. File: `online_indexer.go`.
- [x] **CommitWithVersionstamp swallows vsFuture.Get() errors** — Only requests versionstamp future when mutations exist; propagates errors. File: `database.go`.
- [x] **CountNotNull null check on wrong key portion** — Was checking grouping (leading) portion instead of grouped (trailing). Fixed: `evaluateGroupingKeys` checks trailing columns only. File: `count_not_null_index_maintainer.go`.

### P2 — panics

- [x] **merge_cursor compareField unchecked type assertion** — `int64` vs `string` comparison panics. Fixed: checked assertions with error propagation. File: `merge_cursor.go`.
- [x] **SaveRecord nil proto.Message** — Panics at `ProtoReflect()`. Added nil check. Files: `store.go`, `store_api.go`.
- [x] **IndexEntry nil Index field** — `PrimaryKey()`/`IndexValues()` panic on manually constructed entries. Added nil guard. File: `index_scan.go`.
- [x] **getAggregator unchecked type assertion** — Non-int64 accumulator panics. Fixed: checked assertion. File: `aggregate_function.go`.
- [x] **keyExpressionColumnSize unknown type** — Silently returns 0 instead of erroring. Added `keyExpressionColumnSizeChecked` variant. File: `index_scan.go`.

### P3 — edge cases

- [x] **getEntryPrimaryKey truncated entry** — No length validation before extracting PK from index entry. Added minimum-length check. File: `index.go`.
- [x] **record_key_cursor hasMore not buffered** — `Advance()` result lost on FDB iterator. Added `peekedHasMore` buffer. File: `record_key_cursor.go`.

20 bugs found, 20 fixed. Test files: `bug_bounty_test.go`, `bug_bounty2_test.go`, `bug_bounty3_test.go`, `byte_limit_bug_test.go`. Current: 1065 unit/integration specs, 347 conformance specs (1412 total).

---

## Bugs found by chaos testing (2026-03-14)

Model-based chaos testing framework: in-memory model shadows real FDB store, random operations + fault injection, periodic verification catches divergence. Concurrent stress testing validates snapshot-consistent derived state under multi-goroutine contention.

**Test breakdown:** 71 targeted + 15 random + 5 concurrent = 91 chaos tests.

**Verification checks:** record count, VALUE indexes (including covering index value verification), COUNT indexes, SUM indexes, RANK indexes, PERMUTED_MIN/MAX indexes, VERSION indexes, COUNT_UPDATES (model-based only), MIN/MAX_EVER (model-based only). Concurrent mode uses snapshot-based validation (builds model from store, verifies derivable state only).

**Index types covered:** VALUE, COUNT, SUM, RANK, MAX_EVER, MIN_EVER, COUNT_UPDATES, PERMUTED_MIN, PERMUTED_MAX, VERSION, covering (KeyWithValue) — 7 simultaneously in kitchen sink tests.

**Concurrent stress tests:** 4 workers × 5s, snapshot validation every 1s. Kitchen sink: 6 snapshot-verifiable index types under concurrent load. High contention: 8 workers, 5 PKs.

### Bug found

- [x] **PERMUTED_MIN/MAX Update() doesn't handle group membership changes** — When a record's grouping key changes (e.g., quantity updates), the old group's permuted entry was left stale. Decomposed `Update()` into `updatePermutedForInsert()` and `updatePermutedForRemove()` helpers. UPDATE path now properly processes new groups before primary update, then cleans up old groups after. File: `permuted_min_max_index_maintainer.go`.

---

## Bug bounty round 3 (2026-03-15)

Third audit via 5 parallel subagents targeting: cursor combinators, index maintainers, store operations, online indexer, metadata + expressions.

### Agent 1: Cursor combinators

Root cause: `EndContinuation` is overloaded to mean both "iteration truly done" and "no continuation available." This poisons every combinator that checks `continuation.IsEnd()`.

- [x] **Bug 1: `LimitRowsCursor(n<=0)` leaks inner cursor** — Fixed: close inner cursor before returning Empty. **$100**.
- [x] **Bug 2: `AutoContinuingCursor` infinite loop on EndCont + HasStoppedBeforeEnd** — Documented: matches Java behavior. HasStoppedBeforeEnd + EndContinuation doesn't occur with real cursors (they always provide valid continuations for non-exhaustion stops). **$100**.
- [x] **Bug 3: `ConcatCursors` data loss with EndCont inner cursors** — Documented: matches Java's `ConcatCursorContinuation.isEnd = secondCursor && inner.isEnd()`. Only affects artificial cursors returning values with EndContinuation. **$200**.
- [x] **Bug 4: `FlatMapPipelined` data loss with EndCont inner cursors** — Documented: matches Java. Inner EndContinuation on values = inner exhausted. Same limitation in Java's FlatMapContinuation. **$200**.
- [x] **Bug 5: `ChainedCursor` + `ConcatCursors` pagination data loss** — Documented: ChainedCursor(nil encode) returns EndContinuation for values — same pattern as Bug 3. Real usage always has encode/decode. **$200**.
- [x] **Bug 6: `DedupCursor` drops continuation on EndCont stop** — Documented: matches Java. Pass-through of inner continuation. Real cursors provide valid continuations. **$200**.
- [x] **Bug 7: `ConcatCursors` restarts from beginning on 1st cursor OOB + EndCont** — Documented: matches Java (Java would crash with BufferUnderflowException on empty continuation). Doesn't occur with real cursors. **$200**.
- [x] **Bug 8: `FromListWithContinuation` silently ignores invalid continuation lengths** — Fixed: <4 bytes → error (matches Java's BufferUnderflowException), ≥4 bytes → reads first 4 (matches Java's ByteBuffer.getInt()). **$100**.

Test file: `pkg/recordlayer/bug_bounty3_cursor_test.go`

### Agent 5: Metadata + expressions

- [x] **Bug 9: `bindRecordTypeKeyExpressions` is shallow** — Fixed: recursive type-switch walks all expression types (Grouping, KWV, Nesting, Split, List, Function). Matches Java's recursive `KeyExpression.resolveRecordType()`. **$100**.
- [x] **Bug 10: `Build()` typeKeys map ignores `int32` explicit record type keys** — Fixed: added `int32` case to typeKeys switch. **$100**.
- [x] **Bug 11: `SplitKeyExpression.Evaluate` panics on `splitSize=0`** — Fixed: `Split()` now validates `splitSize > 0`. **$100**.
- [x] **Bug 12: `GroupingKeyExpression` allows `groupedCount > columnSize`** — Fixed: `groupingFromProto` now validates range. **$100**.
- [x] **Bug 13: Former index subspace key type changes through proto round-trip** — Fixed: `normalizeSubspaceKey()` normalizes int/int32/int64 → int64 before comparison. Applied to duplicate type key, index subspace key, and former index checks. **$200**.
- [x] **Bug 14: `RecordTypeKeyExpression.Nest()` type lost on proto round-trip** — Documented: matches Java. `concat(recordTypeKey(), X)` → `ThenKeyExpression` on deser in both Java and Go. Proto format has no RecordTypeKey+nested message. `primaryKeyStartsWithRecordType()` handles both forms. **$100**.
- [x] **Bug 15: `SetRecordCountKey` version bump uses pointer equality** — Fixed: uses `keyExpressionsEqualNilSafe()` structural comparison. **$100**.
- [x] **Bug 16: `isGroupPrefix` uses `FieldNames()` — structural info lost** — Fixed: rewritten to use `normalizeKeyForPositions` + `keyExpressionEquals` for structural comparison. Matches Java's `IndexFunctionHelper.isGroupPrefix()` which uses `KeyExpression.equals()` + `isPrefixKey()`. **$100**.
- [x] **Bug 17: `SplitKeyExpression` accepts negative `splitSize`** — Fixed: same `Split()` validation. **$100**.
- [x] **Bug 18: `ListKeyExpression` empty children lossy proto round-trip** — Fixed: `listFromProto` now accepts 0 children. Matches Java's `ListKeyExpression(RecordKeyExpressionProto.List)` constructor which also accepts empty. **$100**.
- [x] **Bug 19: Evolution validator `fmt.Sprint` confuses `int(5)` with `string("5")`** — Fixed: `subspaceKeyString()` uses `%T:%v` format (type-qualified). All `fmt.Sprint` key comparisons replaced. **$100**.
- [x] **Bug 20: `RecordMetaDataFromProto` silently drops indexes for unknown record types** — Fixed: returns error. Matches Java's `throwUnknownRecordType()`. **$100**.
- [x] **Bug 21: Global function registry has no concurrency protection** — Fixed: `sync.RWMutex` on registry. **$100**.

Test file: `pkg/recordlayer/bug_bounty3_metadata_test.go`

### Agent 2: Index maintainers

- [x] **Bug 25: `MustGet()` panic in PERMUTED_MIN/MAX delete path** — Fixed: `MustGet()` → `Get()` with proper error return. **$100**.
- [x] **Bug 26: COUNT_NOT_NULL without GroupingKeyExpression silently counts nulls** — Fixed: `Build()` validates atomic index types require `GroupingKeyExpression` root. Matches Java's `AtomicMutationIndexMaintainerFactory.getIndexValidator()`. **$100**.
- [x] **Bug 27: SUM index without GroupingKeyExpression silently produces empty index** — Fixed: same Build() validation. **$100**.
- [x] **Bug 28: MIN/MAX_EVER_LONG without GroupingKeyExpression silently produces empty index** — Fixed: same Build() validation. **$100**.
- [x] **Bug 29: `removeCommonGroupingKeys` set semantics on fan-out duplicates** — Documented: matches Java behavior. Java's `List.removeAll()` has the same set-semantics collapse for duplicate grouping keys. Known limitation in both implementations. **$100**.

### Agent 3: Store operations

- [x] **Bug 22: Reverse scan + continuation leaks version to wrong record** — Fixed: `takePendingVersion(currentPK)` now validates PK match. Stale version from continuation boundary discarded. Both unsplit and split paths fixed. **$200**.
- [x] **Bug 23: `TypedFDBRecordStore.LoadRecord` drops Version field** — Fixed: added `Version: storedRecord.Version` to struct literal. **$200**.
- [x] **Bug 24: `TypedFDBRecordStore.SaveRecord` drops Version field** — Fixed: added `Version: storedRecord.Version` to all 3 typed wrapper paths (Load, Save, SaveWithOptions). **$200**.

### Agent 4: Online indexer

- [x] **Bug 30: OnlineIndexer progress tracking undercounts filtered records** — Fixed: count ALL scanned records regardless of type filtering. Matches Java. **$100**.

### Running totals

| Agent | Bugs | $100 | $200 | Total |
|-------|------|------|------|-------|
| Cursor combinators | 8 | 3 | 5 | $1,300 |
| Metadata + expressions | 13 | 12 | 1 | $1,400 |
| Index maintainers | 5 | 5 | 0 | $500 |
| Store operations | 3 | 0 | 3 | $600 |
| Online indexer | 1 | 1 | 0 | $100 |
| **Total** | **30** | **21** | **9** | **$3,900** |

---

## Java vs Go API surface review (2026-03-16)

Full public API comparison across 5 areas. Wire-level compatibility is 100% — all gaps are API surface / feature gaps, not wire format. Go and Java can share the same FDB cluster today.

### RESOLVED: Async API pattern / concurrent batched operations (2026-03-22 → 2026-03-24)

**Original concern:** Java has `*Async` variants returning `CompletableFuture` for every public method, enabling interleaved FDB I/O across multiple records in one transaction.

**Resolution:** RFC 008 made `FDBRecordStore` goroutine-safe. Users can now run concurrent `SaveRecord`/`LoadRecord`/etc. from multiple goroutines within one `db.Run()` callback. FDB's `Transaction` is documented goroutine-safe, so concurrent goroutines naturally pipeline their FDB reads/writes — achieving the same I/O interleaving as Java's `CompletableFuture` model without needing explicit async APIs.

```go
db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
    store, _ := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()
    var wg sync.WaitGroup
    for _, record := range batch {
        wg.Add(1)
        go func(r proto.Message) {
            defer wg.Done()
            store.SaveRecord(r) // goroutine-safe, FDB pipelines I/O
        }(record)
    }
    wg.Wait()
    return nil, nil
})
```

- [x] **HIGH** — Goroutine safety (RFC 008): `FDBRecordContext` uses atomic.Int32, mutex-protected maps, commitMu, lockRegistry. `FDBRecordStore` uses sync.RWMutex for store state. HNSW/R-tree use per-subspace write/read locks. Intra-transaction race test passes with `-race`. 15 subagent reviews confirmed correctness.

### Coverage summary

| Area | Coverage | Key Gaps |
|---|---|---|
| FDBRecordStore (CRUD) | ~90% | Query planning methods, synthetic records |
| Index types | 19/19 | **ALL COMPLETE** |
| IndexMaintainer interface | Core done | `mergeIndex`, `performOperation` (scanUniquenessViolations + validateEntries already shipped on store) |
| MetaData/Schema | ~70% | toProto/fromProto (done), synthetic record types, UDFs, Views, descriptor lookups |
| Cursors/Combinators | ~80% | 15+ combinators done (Concat, Map, Filter, Skip, Limit, Union, Intersection, Dedup, FlatMap, Chained, AutoContinuing, Fallback, MapErr, AsListWithContinuation). Missing: AggregateCursor, UnorderedUnion. |
| ScanProperties/ExecuteProperties | ~95% | `isDryRun`, convenience clear methods |
| Continuations (wire format) | ~90% | Wire-compatible. Go writes TO_OLD, reads both TO_OLD and TO_NEW |
| FDBDatabase/Context/Runner | ~60% | **Async API (see above)**, weak read semantics, MDC, executor control |
| Key expressions | ~95% | All done except AtomKE (Java interface only). OrderFunctionKE, InvertibleFunctionKE, CollateFunctionKE, LongArithmeticKE all shipped. |

### FDBRecordStore — missing public methods

- [ ] **`preloadRecordAsync()`** — Read-ahead optimization. Not applicable to Go's sync model. **LOW**.
- [ ] **`isVersionChanged()`** — Rare introspection. **LOW**.
- [ ] **`buildSingleRecord()`** — Edge case for single-record index builds. **LOW**.
- [ ] **Query planning methods** (~5 methods) — Out of scope until query planner is ported. **LOW**.

### Index API — missing methods on IndexMaintainer interface

- [x] **`scanUniquenessViolations()` / `clearUniquenessViolations()`** — Already implemented as `ScanUniquenessViolations()` / `ScanUniquenessViolationsForValue()` / `ResolveUniquenessViolation()` on store. Maintainer-level variant not needed (store dispatches internally).
- [x] **`validateEntries()`** — Already implemented as `ValidateIndex()` in `index_validation.go` (3-phase: scan records → build expected → diff against actual).
- [ ] **`canDeleteWhere()` with QueryToKeyMatcher** — Go uses structural expression matching instead. **LOW**.
- [ ] **`scanRemoteFetch()`** — Experimental Java feature. **LOW**.
- [ ] **`mergeIndex()` / `performOperation()`** — Generic index operation dispatch. **LOW**.
- [ ] **`isIdempotent()` / `addedRangeWithKey()`** — Internal to Go, not on interface. **LOW**.

### Index types — ALL COMPLETE (19/19)

- [x] **TEXT index** — Done. 115 unit + 34 integration + 7 conformance tests.
- [x] **BITMAP_VALUE index** — Done. 27 unit tests + 6 conformance specs.
- [x] **MULTIDIMENSIONAL index** — Hilbert R-tree spatial indexing. 16 tests.
- [x] **VECTOR/HNSW index** — HNSW graph with 3 distance metrics. 16 tests.
- [x] **TIME_WINDOW_LEADERBOARD** — Done. 22 tests + 11 conformance specs.

### Index scanning — API gaps

- [ ] **`IndexScanBounds` abstraction** — Go takes `TupleRange` directly; Java has `IndexScanBounds` wrapping bounds + comparisons. **LOW**.
- [ ] **`scanIndexRecords` with record type filtering** — Go infers from metadata. **LOW**.

### MetaData — missing public methods

- [x] **`getRecordTypeForDescriptor()` / `getRecordTypeFromRecordTypeKey()`** — Added `GetRecordTypeFromRecordTypeKey()` with normalized integer comparison. Descriptor-based lookup deferred. **LOW**.
- [x] **`getIndexFromSubspaceKey()`** — Added `GetIndexFromSubspaceKey()` with normalized integer comparison. **LOW**.
- [x] **`getUnionDescriptor()` / `getUnionFieldForRecordType()`** — Added `GetUnionDescriptor()` and `GetUnionFieldForRecordType()`. Union descriptor stored during Build(). **LOW**.
- [x] **`commonPrimaryKey()` / `commonPrimaryKeyLength()` static helpers** — Added `CommonPrimaryKey()` (structural equality via keyExpressionEquals) and `CommonPrimaryKeyLength()`. **LOW**.
- [ ] **`getIndexesSince(version)` with RecordType mapping** — Go returns Index list only. **LOW**.
- [x] **`getFormerIndexesSince(version)`** — Added `GetFormerIndexesSince()`. **LOW**.
- [x] **Builder query methods** — Added `GetVersion()`, `IsSplitLongRecords()`, `IsStoreRecordVersions()`, `GetRecordCountKey()`, `GetRecordTypes()` on builder. **LOW**.
- [ ] **`build(false)` skip-validation variant** — Go always validates. **LOW**.
- [ ] **`IndexMaintainerRegistry` pluggable** — Go dispatches from hardcoded switch. **LOW**.
- [ ] **Synthetic record types** — JoinedRecordType, UnnestedRecordType. Large feature. **LOW**.
- [ ] **User-defined functions** — `PUserDefinedFunction` in MetaData proto. **LOW**.
- [ ] **Views** — `PView` in MetaData proto. **LOW**.

### RecordType — missing getters

- [x] **`getIndexes()` / `getMultiTypeIndexes()` / `getAllIndexes()`** — Added `GetIndexes()`, `GetMultiTypeIndexes()`, `GetAllIndexes()` on RecordType. **LOW**.
- [x] **`hasExplicitRecordTypeKey()` / `getRecordTypeKeyTuple()`** — Added `HasExplicitRecordTypeKey()`. Key already accessible via `GetRecordTypeKey()`. **LOW**.
- [ ] **`isSynthetic()`** — No synthetic record support yet. **LOW**.

### Cursor — missing combinators & methods

- [ ] **`UnorderedUnionCursor`** — Union without order preservation. **LOW**.
- [ ] **`MapPipelinedCursor`** — Async pipelined map (no Go equivalent of CompletableFuture). **LOW**.
- [ ] **`filterAsync()`** — Pipelined async filter. Not applicable to Go's sync model. **LOW**.
- [ ] **`mapEffect()` / `mapContinuation()`** — Side-effect map, continuation rewriting. **LOW**.
- [ ] **`forEachResult()` / `forEachAsync()`** — Result-level iteration. **LOW**.
- [ ] **`reduce()` with stop condition** — Conditional reduction. **LOW**.
- [ ] **`AggregateCursor`** — Accumulator-based aggregation. **LOW**.
- [ ] **`ComparatorCursor`** — Custom comparison ordering. **LOW**.
- [ ] **`ProbableIntersectionCursor`** — Bloom filter intersection. **LOW**.
- [ ] **`SizeStatisticsGroupingCursor`** — Key/value size tracking. **LOW**.
- [ ] **`RecordCursorVisitor` pattern** — Cursor tree inspection. **LOW**.
- [x] **`RecordCursorResult.Map()` / `WithContinuation()`** — Added `MapResult[T,R]()` standalone function + `WithContinuation()` method. **LOW**.
- [ ] **`isClosed()` on cursor** — Closure state check. **LOW**.

### ExecuteProperties — missing features

- [ ] **`isDryRun` flag** — Dry-run execution mode. **LOW**.
- [x] **Convenience clear methods** — `ClearRowAndTimeLimits()`, `ClearSkipAndLimit()`, `WithScannedRecordsLimit()`, `WithScannedBytesLimit()`, `WithSkip()`. **LOW**.

### FDBDatabase — missing methods

- [ ] **`openContext()` (6 overloads)** — Go uses Run()/RunWithVersionstamp() exclusively. **LOW**.
- [ ] **`performNoOp()` / `performNoOpAsync()`** — No-op transaction testing. **LOW**.
- [ ] **`clearCaches()` / `close()`** — Cache/lifecycle management. **LOW**.
- [ ] **`FDBDatabaseFactory`** — Database pooling. **LOW**.
- [ ] **`setDatacenterId()` / `getLocalityProvider()`** — Datacenter affinity. **LOW**.

### FDBRecordContext — missing methods

- [ ] **`getConfig()` / `getTransactionId()` / `getTimeoutMillis()`** — Context introspection. **LOW**.
- [ ] **`getTransactionAge()`** — Transaction timing. **LOW**.
- [ ] **`getCommitCheck()` / `removeCommitChecks()`** — Hook management post-add. **LOW**.
- [ ] **`removePostCommit()` / `addPostCloseHook()`** — Hook removal. **LOW**.
- [ ] **`WeakReadSemantics`** — Causal read risky / version staleness bounds. **LOW**.
- [ ] **`getMdcContext()`** — Mapped diagnostic context. **LOW**.

### FDBDatabaseRunner — missing methods

- [ ] **`runAsync()` (5 overloads)** — Go is sync only. **LOW**.
- [ ] **Timer/MDC/WeakReadSemantics getters/setters** — **LOW**.

### Key expressions — 5 missing types

- [x] **`CollateFunctionKeyExpression`** — Implemented (collate_jre/collate_icu). 21 tests.
- [x] **`OrderFunctionKeyExpression`** — Implemented (4 order functions). 31 tests.
- [x] **`DimensionsKeyExpression`** — Multidimensional indexing. **DONE**.
- [x] **`InvertibleFunctionKeyExpression`** — Abstract in Java; evaluation side implemented via OrderFunctionKE. Inverse (for query planning) deferred.
- [ ] **`AtomKeyExpression`** — Atom-level expressions. **LOW**.

### OnlineIndexer — missing config options

- [ ] **`setIndexStatePrecondition()`** — State pre-check. **LOW**.
- [ ] **`setTimeLimitMillis()`** — Per-batch time limits. **LOW**.
- [ ] **`setCommitCheckIntervalCount()`** — **LOW**.
- [ ] **`setMaxWriteRetries()`** — Handled implicitly via FDBDatabaseRunner. **LOW**.
- [ ] **`setDesiredRecordsPerSecond()`** — Rate limiting. **LOW**.
- [ ] **`addStatisticsCollector()`** — Statistics collection. **LOW**.

### Convenience methods — not implemented

- [x] **`getRecordCount()` / `getRecordCount(recordTypeName)`** — Already implemented as `GetRecordCount()`, `GetSnapshotRecordCount()`, `GetSnapshotRecordCountForRecordType()`. **LOW**.
- [x] **`Index.getBooleanOption(key, default)`** — Added `GetBooleanOption()`. **LOW**.
- [x] **`IndexAggregateFunction` constructor helpers** — Added `NewCountAggregateFunction`, `NewSumAggregateFunction`, `NewMin/MaxAggregateFunction`, `NewMin/MaxEverAggregateFunction`. **LOW**.

### Design differences (intentional, not gaps)

These are architectural decisions, not bugs:

- **Async → Sync**: Java uses `CompletableFuture`; Go uses sync + `context.Context`. All pipelined/async cursor variants are N/A.
- **Executor control**: Java exposes thread pools; Go uses goroutines. N/A.
- **Builder query methods**: Java exposes getters on builders; Go uses direct struct fields. Functional equivalent.
- **`RecordCursor` interface width**: Java has 20+ default methods; Go has 2 (OnNext, Close) + standalone generic functions. Same functionality, different ergonomics.

---

## Remaining work buckets (2026-03-11 assessment)

**A. Huge features** — TEXT index (Lucene-style), query planner, synthetic record types. Each is weeks of work.

**B. Niche index types** — ALL COMPLETE. (~~PERMUTED_MIN/MAX~~, ~~MAX_EVER_VERSION~~, ~~BITMAP_VALUE~~, ~~TEXT~~, ~~TIME_WINDOW_LEADERBOARD~~, ~~MULTIDIMENSIONAL~~, ~~VECTOR~~ done.)

**C. Polish** — ~~Timer/instrumentation~~, ~~store state caching~~, ~~dead code removal~~, CursorLimitManager refactor, API cleanup. Important for production but not feature-blocking.

- [x] **[MEDIUM] Store state caching** — `MetaDataVersionStampStoreStateCache` + `PassThroughRecordStoreStateCache`. Validates via `\xff/metadataVersion` versionstamp, handles dirty state, read conflicts on cache hit, proto.Clone on cache-hit path, LRU+TTL eviction. 40 specs, 2.2x benchmark speedup. Files: `store_state_cache.go`, `store_state_cache_test.go`.
- [ ] **[LOW] `FDBDatabase.storeStateCache` field unsynchronized** — Interface field on `FDBDatabase` is not protected by mutex or `atomic.Value`. Safe as long as it's set-once-at-startup before any transactions. If runtime reconfiguration is needed, wrap in `atomic.Value`.
- [ ] **[LOW] TOCTOU duplicate FDB reads on concurrent cache miss** — Two goroutines can miss the cache simultaneously and both load from FDB. Same behavior as Java (Guava cache). Harmless — both writes are idempotent and `addToCache` keeps the newer versionstamp.
- [ ] **[LOW] O(n) LRU eviction scan in store state cache** — `evictIfNeeded()` iterates all entries under mutex. Max 500 entries (default), so bounded. Replace with container/heap if profiling shows contention.
- [x] **[LOW] Dead code removal** — 5-agent parallel scan of entire codebase. Removed 7 items: 2 unused constants (`maxParallelIndexRebuild`, `preloadCacheSize`), 2 unused type aliases (`RecordCursorProto`, `TypedRecordCursor`), 1 unused utility function (`MapErr` in cursor_util.go), 2 dead accessor methods (`GetWholeKey`, `GetRecordTypeIndex` — fields accessed directly). Kept `SetAllowMissingFormerIndexNames`/`SetAllowNoSinceVersion` (Java API surface, wired into validation).

**Next high-value target**: VERSION index — DONE (Phase 1 + Phase 2). Conformance tests remaining.

**D. Build tooling**
- [x] **Add stdlib nogo analyzers** — Added 13 new analyzers (appends, deepequalerrors, defers, directive, errorsas, ifaceassert, nilness, shadow, sigchanyzer, sortslice, stringintconv, timeformat, waitgroup). 20 → 33 total. Zero new findings — codebase was already clean.
- [x] **Add staticcheck to nogo** — All 90 SA analyzers wired into nogo via individual deps on `honnef.co/go/tools` v0.6.1. Uses `_base` config with `only_files` for workspace packages. Disabled: `shadow` (noisy, err shadowing is idiomatic Go), `loopclosure` (Go 1.22+ fixed). Excluded: SA1019 on `metadata_proto.go` (intentional deprecated field use), SA5011 on test files (doesn't understand t.Fatal guards). Fixed: 2 tautological nil comparisons (cursor.go), 6 unused assignments (test files).

---

## Production readiness

### HIGH

- [x] **API surface polish (Phase 1+2)** — RFC 003 Option B executed: unexported 11 concrete index maintainer types, RankedSet/RankedSetConfig, SizeInfo, all format version constants, split/size constants. Added `RankQuerier` interface for chaos test package access. `IndexMaintainer` interface and `RangeSet` kept exported (public API returns / conformance tests). 37 files, ~400 lines changed. Subspace constants kept exported for debugging.
- [ ] **Performance testing under real workloads** — Benchmark key operations (bulk inserts, index-heavy saves, large scans with continuations, split record read/write, OnlineIndexer throughput) under realistic data volumes. Profile hotspots. Compare with Java Record Layer on equivalent workloads where possible.
- [x] **vtprotobuf migration — remaining call sites** — All 26 call sites migrated to `MarshalVT`/`UnmarshalVT` across 9 files: cursor_combinators.go, dedup_cursor.go, merge_cursor.go, indexing_heartbeat.go, index_state.go, key_value_cursor.go, multidimensional_index_maintainer.go, time_window_leaderboard.go, vector_index_maintainer.go. Only fallback `proto.Marshal`/`proto.Unmarshal` remaining are in store.go's `serializeUnion`/`deserialize*` for `proto.Message` interface (VT fast-path already checked via type assertion).
- [x] **Edge case hardening** — 37 specs in `edge_case_test.go`: corrupt store headers (garbage/empty/missing), corrupt record data (load + scan), boundary PKs (0, MinInt64, MaxInt64), empty store ops (load/scan/delete/count/deleteAll), index edge cases (empty scan, nil values, unique nil), continuation pagination, concurrent reads + write-write conflict detection, metadata validation (missing PK, zero-column PK, bad field), builder validation (missing context/metadata/subspace), save/load round-trips (boundary values, long strings, 10x overwrite), split boundary, reopen semantics, index state errors, count semantics, store lock enforcement, FormerIndex tracking.
- [x] **Chaos testing** — Model-based framework at `pkg/recordlayer/chaos/`. ChaosTransactor injects commit-unknown/conflict/timeout faults. 91 tests: 71 targeted (per-index-type fault injection), 15 random (seeded PRNG, up to 2000 ops), 5 concurrent (multi-goroutine contention). Covers VALUE/COUNT/SUM/RANK/PERMUTED/VERSION/COVERING indexes. Found and fixed 1 bug (PERMUTED group membership change). Remaining: network partition simulation, OOM during scans, interrupted index builds.

---

## Build & CI

### MEDIUM

- [x] **FDB client version mismatch between CI and testcontainers** — Bumped CI to 7.3.46 matching testcontainers default.

### LOW

- [x] **CI missing `go mod verify` and format checks** — Added `go mod verify`, `gofmt -l`, and Gazelle drift detection steps.
- [x] **CI missing Gazelle drift detection** — Added to CI build job (runs gazelle, checks git diff).
- [x] **Justfile missing `fmt` and `coverage` targets** — Added `just fmt` and `just coverage`.

---

## Test quality improvements

### MEDIUM

- [x] **~25 implementation files lack dedicated unit tests** — Added dedicated tests for core files: `key_expression_test.go` (144 specs), `cursor_test.go` (42 specs), `scan_properties_test.go` (37 specs), `cursor_util_test.go` (31 specs), `errors_test.go` (56 specs). Remaining untested: index maintainers (well-covered by integration + chaos tests), `record_key_cursor.go`, `store_typed.go`, `record_function.go` (all FDB-dependent), `constants.go`/`endpoint_type.go` (trivial enums/constants).
- [x] **Brittle string-matching error assertions in tests** — Migrated 63 assertions total across 22 test files from `.Error().To(ContainSubstring(...))` to typed `errors.As()` + struct field checks. Round 1: 35 assertions (13 error types). Round 2: 28 assertions (new `KeyExpressionError` type + `MetaDataError` for metadata_proto.go). 31 remaining are genuine internal validation (`fmt.Errorf` with no Java exception mapping).
- [x] **Temp file leak in test suite setup** — Fixed: cleanup in `SynchronizedAfterSuite` via package-level `clusterTmpFilePath` variable.

### LOW

- [x] **Missing cursor combinator edge case tests** — 10 in-memory tests: empty Concat/Union/Intersection, FilterCursor rejects all, filter continuation under limit, MapErrCursor error propagation, LimitRows(0), Skip past all, deep composition Filter(Map(Limit(FromList))), ConcatCursors ordering.
- [x] **Missing continuation token stability tests** — 5 tests: resume after record deletion (skips deleted), resume after insertion (sees new records past cursor), resume index scan after deletion, resume after DeleteAllRecords + re-insert, resume index scan after rebuild. All cross-transaction.
- [x] **Missing schema evolution edge case tests** — 10 tests: multi-version jump (v1→v5) with intermediate index, new index with stale version, combined add+remove with FormerIndex, addedVersion change, lastModifiedVersion decrease, safe type promotions (int32→int64, sint32→sint64, narrowing, cross-type, identity). 44 total evolution validator tests.

---

## Future: Query planner + SQL layer

**Not started. Blocked on: core must be rock solid first.**

Port the full query infrastructure from Java, then the relational/SQL layer on top.

### Phase 1: Cascades query optimizer (~104K lines Java)

The Cascades framework (Graefe 1995) is the cost-based query optimizer — 494 files, 40% of core by itself. Turns logical queries into optimized physical execution plans (index selection, join ordering, predicate pushdown, etc.).

- [ ] **Cascades optimizer framework** — `query/plan/cascades/` — rule-based exploration of equivalent plans, cost estimation, memo structure
- [ ] **Physical plan implementations** — `query/plan/plans/` (74 files, 19K lines) — RecordQueryPlan nodes (index scan, filter, union, intersection, sort, aggregate, etc.)
- [ ] **Query expressions** — `query/expressions/` (35 files, 9K lines) — predicates, comparisons, logical operators for query specification
- [ ] **Planning infrastructure** — `query/plan/planning/` — plan generation, property derivation
- [ ] **Synthetic record planner** — `query/plan/synthetic/` (11 files, 2K lines) — joined/unnested record plan generation
- [ ] **Bitmap plans** — `query/plan/bitmap/` — bitmap index scan plans
- [ ] **Sort plans** — `query/plan/sorting/` — external sort, in-memory sort
- [ ] **Explain** — `query/plan/explain/` — plan visualization/debugging

### Phase 2: Prerequisites from core

- [ ] **Joined record types** — `SyntheticRecordType`, `JoinedRecordType`, `UnnestedRecordType` — virtual records composed from constituents via equi-joins
- [ ] **KeySpace directory layer** — `provider/fdb/keyspace/` (25 files, 7K lines) — hierarchical key management. Note: basic FDB directory layer ported in nightshift-2; KeySpace is the Record Layer wrapper on top.
- [x] **TEXT index** — full-text search with BunchedMap, UAX#29 tokenization, NFKD normalization. 115 unit + 34 integration + 7 conformance tests.
- [x] **Remaining key expression types** — All done: GroupingKE, LiteralKE, KeyWithValueKE, VersionKE, FunctionKE, SplitKE, ListKE, LongArithmeticKE, DimensionsKE, OrderFunctionKE, CollateFunctionKE. Only AtomKE remaining (Java interface only, no wire format).

### Phase 3: Relational / SQL layer (~55K lines Java)

Separate module (`fdb-relational-core` + `fdb-relational-api`). Compiles SQL to RecordLayer query plans.

- [ ] **SQL parser** — SQL AST (`structuredsql/`)
- [ ] **SQL → plan compiler** — `recordlayer/query/` — translates SQL AST to Cascades logical plans
- [ ] **Schema catalog** — `recordlayer/catalog/` — DDL → RecordMetaData mapping, system tables, stored in FDB
- [ ] **Type system** — SQL types ↔ protobuf types mapping
- [ ] **gRPC server** — `fdb-relational-grpc/` + `fdb-relational-server/`

### Phase 4: `database/sql` driver

Go `database/sql` compatible driver. Any Go app using `database/sql` (ORMs, migration tools, existing codebases) just works — swap your Postgres DSN for an FDB one. Wire-compatible with Java JDBC driver: a Java app and a Go app can read/write the same tables in the same FDB cluster simultaneously.

- [ ] **`database/sql` driver registration** — `sql.Register("fdb", ...)`, DSN parsing
- [ ] **`driver.Conn` / `driver.Tx`** — map to `FDBRecordContext` transactions (5s limit awareness)
- [ ] **`driver.Rows`** — cursor-backed result sets with continuation support
- [ ] **`driver.Stmt`** — prepared statements → Cascades plan cache
- [ ] **Query parameter binding** — `?` placeholders → plan parameterization
- [ ] **DDL passthrough** — `CREATE TABLE` / `ALTER TABLE` / `CREATE INDEX` via schema catalog
- [ ] **Type mapping** — Go `sql.Scanner`/`driver.Valuer` ↔ protobuf ↔ FDB tuple types

### Size estimates

| Component | Java files | Java lines | Notes |
|---|---|---|---|
| Cascades optimizer | 494 | 104K | Biggest single chunk |
| Plan implementations | 74 | 19K | Physical execution nodes |
| Query expressions | 35 | 9K | Predicates, comparisons |
| Planning + other | 43 | 15K | Infra, bitmap, sort, explain |
| Relational core | 233 | 41K | SQL→plan compiler |
| Relational API | 88 | 13K | Interfaces, types |
| Relational server/JDBC/gRPC | 31 | small | Thin wrappers |
| **Total** | **~1000** | **~200K** | |

---

## Documentation cleanup

### LOW

- [x] **PORT.md** — Comprehensive porting assessment with subsystem ratings, test coverage, conformance matrix. Updated 2026-03-09.
- [x] **Clean up PHASE1_TEST_GAPS.md** — Deleted stale file.
- [x] **Clean up FDB_CONFLICT_DETECTION.md** — Deleted stale file.

---

## Project Review 2026-03-17

Comprehensive 10-agent quality assessment across test coverage, Java conformance, Go style, error handling, API design, enterprise readiness, code complexity, index quality, cursor/pagination, and build/CI.

### CRITICAL

- [x] **Cursor combinator bugs (12 documented issues)** — Fixed: `EndContinuation` was overloaded for both "iteration done" and "no continuation available." Added `StartContinuation` type (IsEnd=false, ToBytes=nil) matching Java's `RecordCursorStartContinuation`. Added strict validation panics in `NewResultWithValue`/`NewResultNoNext` matching Java's `withNextValue()`/`withoutNextValue()`: value+EndContinuation panics, SourceExhausted↔EndContinuation enforced bidirectionally. Fixed 7 production code sites (keyValueCursor, indexCursor, recordKeyCursor, limitRowsCursor, chainedCursor, unionCursor, intersectionCursor). `HasStoppedBeforeEnd` now checks continuation.IsEnd() matching Java.
- [x] **Observability gaps** — StoreTimer IS already wired into all major production paths: SaveRecord (time+bytes), LoadRecord (time), DeleteRecord (time+bytes), ScanRecords (time), ScanIndex (time), Create/Open/CreateOrOpen (time), GetReadVersion (time), Commit (time), RebuildIndex (time). Nil-safe, zero overhead when disabled. 18 events vs Java's 260+ — remaining gap is breadth (per-subspace, per-index-type granularity), not wiring. 32 unit+integration specs.
- [x] **OnlineIndexer adaptive throttling** — Implemented `indexingThrottle` matching Java's `IndexingThrottle.Booker`: graduated `oneToNineFactor` schedule (90%→80%→70%→50%→10% on consecutive failures), adaptive limit based on actual records scanned at failure time, `recordsPerSecond` rate limiter (default 10,000, cap 999ms inter-tx delay), `handleSuccess` resets. Replaces old simple halving. 40 new unit tests.

### HIGH

- [x] **Index maintainer code duplication** — Consolidated 8 atomic index maintainers into unified `atomicMutationIndexMaintainer` with `AtomicMutation` strategy interface. Each index type (COUNT, SUM, MIN_EVER, MAX_EVER, COUNT_NOT_NULL, COUNT_UPDATES, MIN_EVER_TUPLE, MAX_EVER_TUPLE) is now a struct implementing `AtomicMutation` (getMutationType, getMutationParam, isIdempotent, skipUpdateForUnchangedKeys). Single maintainer, single factory dispatch, eliminated ~1000+ lines of duplication.
- [x] **Metadata builder API footguns** — `RecordMetaDataBuilder.GetRecordType()` now panics with `MetaDataError` for unknown type names, matching Java's `MetaDataException("Unknown record type " + name)`. Previously returned nil causing opaque nil deref on `.SetPrimaryKey()` chains. `KeyExpression.Validate(descriptor)` already implemented (called from `Build()`). Typed store type name derivation deferred (nice-to-have, not a footgun).

---

## HNSW Vector Index Review (2026-03-22)

5-agent deep review of HNSW/vector index implementation. Covers algorithm correctness, Java compatibility, RaBitQ quantization, performance, and test coverage.

Wire format verified correct: subspace layout (data=0, access=1), compact node format, inlining edge format, access info format, vector serialization (HALF/SINGLE/DOUBLE/RABITQ type ordinals) all match Java. Layer assignment via SplitMix64 hash of `javaHashCode(pk.Pack())` matches Java's `SplittableRandom`. RaBitQ/FHT-KAC wire-compatible with Java's `fdb-extensions` (bit packing, FHT butterfly, Givens rotation, Java Random LCG all verified; cross-validated against hardcoded Java reference values).

### Phase 1: Java conformance + RaBitQ extraction (do first)

#### CRITICAL — Java interop (all fixed)

- [x] **PK trimming missing** — Fixed: `Update()` now calls `m.index.TrimPrimaryKey()` before HNSW Insert/Delete. `SearchKNN` reconstructs full PKs via `getEntryPrimaryKey()`.
- [x] **Continuation token format incompatible** — Fixed: now uses `VectorIndexScanContinuation` protobuf matching Java. All entries serialized into continuation for replay on resume.
- [x] **IndexEntry format mismatch** — Fixed: Key = `(prefix..., trimmedPK...)`, Value = `(vectorBytes | nil)` matching Java's `toIndexEntry()`.

#### HIGH — Java behavioral alignment (all fixed)

- [x] **Missing HNSW config parsing** — Fixed: `hnswM`, `hnswMMax`, `hnswMMax0`, `hnswEfConstruction` now parsed from index options.
- [x] **Java requires KeyWithValueExpression** — Documented divergence. Go allows non-KWV for backwards compat. Many existing tests use `Concat()`.
- [x] **`unpackComponents` returns zeros on truncated data** — Fixed: now returns `([]int, error)`. Callers handle error.
- [x] **Non-finite distance returns +Inf, Java throws** — Fixed: `Distance()` now returns `(float64, error)` on non-finite results.

#### Conformance test gaps (must add for Phase 1)

- [x] **No Java kNN search conformance** — Fixed: 2 new cross-language specs (Java searches Go graph, Go searches Java graph). 13 vector conformance tests total.
- [x] **No RaBitQ cross-language byte-level conformance test** — Fixed: 5 conformance specs with Cosine metric (activates RaBitQ immediately). Go→Java write+read, Java→Go write+search, cross-language kNN search. 8D vectors, numExBits=4.
- [x] **`TestMultipleExBitsPrecision` has dead assertions** — Fixed: now tracks improvement count and asserts at least 2/7 transitions show improved self-distance.

#### Architecture — RaBitQ extraction

- [x] **Extract RaBitQ into separate package** — Done: `pkg/rabitq/` with `VectorQuantizer` interface in `pkg/recordlayer/hnsw.go`. HNSW dispatches through interface (`Encode`, `Distance`, `Decode`, `GetTypeByte`). No import cycle: `rabitq` defines its own `Metric` type, `recordlayer` imports `rabitq` only in `vector_index_maintainer.go`. `fht_kac_rotator.go` stays in `recordlayer` (HNSW-core, not RaBitQ-specific).

### Phase 2: Go-side improvements (after Java conformance is solid)

#### HIGH — correctness bugs (not Java-specific)

- [x] **Entry point invisible in inlining mode** — Fixed: sentinel KV at `(layer, pk)` written when saving node with 0 neighbors in inlining mode. `loadNodeLayerInlining` now distinguishes "0 neighbors" from "not found".
- [x] **FDB errors silently swallowed** — Fixed: `existFuture.Get()` and `accessFuture.Get()` in Insert now propagate errors. Reverse connection load errors propagate instead of `continue`.
- [x] **Delete uses computed topLayer, not actual** — Fixed: Delete now scans from `epLayer` down to 0 to find actual layers, instead of computing from PK hash.
- [x] **Inlining `deleteNodeLayerInlining` comment lies** — Fixed: comment now correctly says "clears outgoing edges".

#### Performance

Current: 39 QPS @ 1K vectors (26ms p50), 7.9 QPS @ 10K (135ms p50). 16x gap vs Qdrant is structural — HNSW has O(√N) irreducible sequential FDB round-trips at layer 0. No amount of Go-side optimization closes this. These target ~30-40% improvement within the HNSW architecture.

- [x] **~210KB garbage/query from RaBitQ distance** — Fixed: fused xuc computation into single-pass dot product in `EstimateDistance`, eliminating `make([]float64, dims)` per call (~100KB/query saved). `EncodedVectorFromBytes` `make([]int, dims)` remains (needed for bit unpacking).
- [x] **~~No cross-transaction entry point cache~~** — Removed. Saves 0.08ms (0.5% of 18ms query). Not worth the complexity. Java doesn't do it either. Within-transaction cache already handles repeated reads.
- [x] **Quantizer/Estimator recreated per call** — Fixed by RaBitQ extraction: `VectorQuantizer` interface stored on `HNSWConfig`, dispatched through once-created instance.
- [x] **`deserializeVector` allocates every call** — Not an issue: when quantizer is set, `computeDistance` takes the quantizer path and never calls `deserializeVector`. Raw-vector path allocation is unavoidable but infrequent.
- [x] **distHeap not pre-allocated** — Fixed: backing slice pre-allocated to capacity `ef`.
- [x] **Double Pack() for visited set** — Documented: visited check Pack() is unavoidable (needed before batch load to filter). Heap push already reuses batch result's `pkBytes`. Remaining double-Pack is structural — would need interface change for marginal gain.

#### Additional test coverage

- [x] **No graph structure verification** — Fixed: BFS reachability test verifies all 20 nodes reachable from entry point at layer 0.
- [x] **No delete-then-reinsert same PK test** — Fixed: delete entry point, verify 2 remain, re-insert same PK, verify all 3 found.
- [x] **No all-identical-vectors degenerate case** — Fixed: 5 identical vectors, all returned with distance ~0, distinct PKs.
- [x] **No wrong-dimension query test** — Fixed: 3D query against 128D index returns clear error. Added dimension validation in SearchKNN and scanByDistanceWithParams.
- [x] **No old-position search after update** — Fixed: verifies PK=1 NOT found at old position (0,0) after moving to (100,100).
- [x] **No high-dimensional FDB integration (768D+)** — Fixed: 768D vectors insert/search test with KeyWithValue + vector_data bytes.
- [x] **No OnlineIndexer/RebuildIndex for VECTOR** — Fixed: RebuildIndex test clears index subspace, rebuilds, verifies all records searchable.
- [x] **No CI-run medium-scale test (500+ vectors)** — Fixed: 500-vector test with batch insert, geometric nearest-neighbor verification. Runs in CI (~8s).
- [x] **Chaos tests only use 2D vectors** — Fixed: `TestVectorHighDimRaBitQBasic` (128D, 10 records, full pipeline) + `TestVectorHighDimRaBitQCommitUnknown` (128D, 20 ops, fault injection).

#### Performance: Goroutine-parallel beam search prefetch

- [x] **HIGH — Parallel candidate prefetch in `searchLayerMulti`** — Pops up to 4 candidates per iteration, issues all edge-list reads as pipelined FDB futures via `loadEdgeListsBatch()`, then batch-fetches all unvisited neighbor vectors. **22.5% speedup** (18.2ms → 14.1ms per search, 1000 vectors, 128D, ef=64). `searchLayerGreedy` unchanged (single-candidate greedy descent has no parallel opportunity).

#### Architectural note: HNSW vs IVF on FDB

HNSW is a fundamentally poor fit for networked KV stores due to O(√N) sequential round-trip dependency at layer 0 (data-dependent graph traversal — each hop must wait for previous results). IVF reduces this to O(log N) fixed phases using range reads (FDB's strength). Java also only has HNSW — no IVF. A future `IndexType_VECTOR_IVF` (per RFC 005) would be a Go-only performance feature, not a Java port. See distributed systems analysis in conversation 2026-03-22.

---

## Hardening (2026-03-28)

Systematic hardening of deserialization paths, panic elimination, fuzz testing, and chaos test coverage.

### Phase 1: Panic audit — DONE

- [x] **CRITICAL — `text_index_serializer.go`: all Deserialize methods panicked on malformed input** — `BunchedSerializer` interface changed to return `(T, error)`. All 15 panic sites converted to `BunchedSerializationError` returns. All callers in `bunched_map.go` (30 sites) and `bunched_map_iterator.go` (10 sites) propagate errors. OOM via crafted varint sizes fixed with `buf.Len()` bounds checks. Uses `fastUnpack` instead of `tuple.Unpack` (which itself panics on truncated input — see birdayz/fdb-record-layer-go#2).
- [x] **CRITICAL — `bunched_map_iterator.go`: `SubspaceOf`/`SubspaceTag` panicked on bad FDB keys** — `SubspaceSplitter` interface changed to return `(T, error)`. `TextSubspaceSplitter` returns `BunchedSerializationError`. Iterator sets `iterErr` + `done` on error. `Err()` method added to both `BunchedMapIterator` and `BunchedMapMultiIterator`.
- [x] **HIGH — `tuple_fast.go`: `fastUnpack` panicked on truncated input** — `findTerminator` now returns -1 on missing terminator (was returning garbage from `IndexByte(-1)`). `fastDecodeBytes`/`fastDecodeString`/`fastDecodeInt`/`fastDecodeBigInt` all return errors. `tupleSkip` returns -1 on truncated input. Every type code path bounds-checked. Survives 77M+ fuzz executions.

### Phase 2: Fuzz testing — DONE

7 Go native fuzz targets in `fuzz_test.go`. All pass 30-60s continuous fuzzing (30M-77M executions each). Run via `bazel run //pkg/recordlayer:recordlayer_test -- -test.fuzz='^FuzzName$' -test.fuzzcachedir=/tmp/x -test.fuzztime=60s`. Seed corpus runs as regression tests under normal `bazel test`.

- [x] **`FuzzFastUnpack`** — Cross-validates `fastUnpack` against `tuple.Unpack` on arbitrary bytes. Found 2 panics (truncated bytes/string, truncated integer). **Discovered upstream `tuple.Unpack` panics (birdayz/fdb-record-layer-go#2).**
- [x] **`FuzzFastUnpackRoundtrip`** — Pack valid tuples, unpack with `fastUnpack`, verify roundtrip.
- [x] **`FuzzDeserializeBunch`** — TEXT index custom binary format. Found OOM via crafted varint sizes → added `buf.Len()` bounds checks.
- [x] **`FuzzUnwrapContinuation`** — Continuation token parser. Clean.
- [x] **`FuzzUninvertBytes`** — DESC ordering 7-bit encoder roundtrip. Clean.
- [x] **`FuzzDeserializeVector`** — HNSW vector binary format. Clean.
- [x] **`FuzzCompleteVersionFromBytes`** — 12-byte version parser. Clean.

### Phase 3: Chaos test gaps

- [x] **MEDIUM — BITMAP_VALUE chaos tests** — 8 tests: basic save, multiple records, delete, overwrite, commit-unknown insert/delete, random 200 ops no faults, random 200 ops with 5% commit-unknown. Bitmap verification computes expected bits from model records, scans BY_GROUP, bidirectional diff (missing/orphan entries + individual bit mismatches). BIT_OR/BIT_AND confirmed idempotent under faults.
- [x] **MEDIUM — TEXT index chaos tests** — 9 tests: basic CRUD, commit-unknown (insert/overwrite/delete), same-name update (removeCommonEntries fast path), different-name update, deleteAll, random 100 ops (5% faults), heavy stress 200 ops (20% faults). `verify_text.go` tokenizes model records, scans BY_TEXT_TOKEN, bidirectional diff on (token, PK) pairs. Wired into main `Verify()` dispatch.
- [x] **MEDIUM — MAX_EVER_VERSION chaos tests** — 15 tests: ungrouped basic/commit-unknown (insert/overwrite/delete)/deleteAll/random/heavy/all-faults, grouped basic/commit-unknown (insert/overwrite)/deleteAll/random/heavy. Structural verification: entry count, versionstamp presence in values. BYTE_MAX confirmed idempotent under faults. _EVER semantics: entries persist after delete.
- [x] **MEDIUM — OnlineIndexer under faults** — 5 tests: VALUE index (10%/15%/20% faults, limit=3/5/7), COUNT index (10% faults), all-fault-types (commit-unknown + conflict + tx-too-old). RangeSet's InsertRange(requireEmpty=true) correctly detects already-processed ranges. Verification scans index entries and confirms all records indexed.

### Phase 4: Additional hardening

- [ ] **LOW — Schema validation cross-language conformance** — MetaDataValidator/MetaDataEvolutionValidator cross-language error comparison.
- [x] **LOW — Continuation token fuzzing per cursor type** — 3 new fuzz targets: `FuzzConcatContinuation`, `FuzzFlatMapContinuation`, `FuzzDedupContinuation`. Each exercises proto UnmarshalVT + factory fallback with random bytes. 15s continuous fuzzing each (~22M executions) — all clean. Union/Intersection don't have deserialization factories yet; passthrough combinators (Filter, Skip, Limit, Map) have no continuation parsing to fuzz.

---

## Native Go Client (RFC 010)

Pure Go FDB client eliminating cgo/libfdb_c dependency. See `rfcs/010-pure-go-fdb-client.md`.

### API coverage assessment (updated 2026-04-01)

C binding Transaction has 47 methods, Database has 11. Coverage by category:

| Category | Have | Total | % | Notes |
|---|---|---|---|---|
| Core CRUD | 11 | 11 | **100%** | Get, GetRange (multi-shard), Set, Clear, ClearRange, Commit, GetCommittedVersion, SetReadVersion, GetReadVersion, OnError, Reset |
| Atomic ops | 14 | 14 | **100%** | All 14 mutation types. ADD tested e2e. |
| Database | 4 | 4 | **100%** | Transact, ReadTransact, CreateTransaction, Close |
| Key selectors | 1 | 1 | **100%** | GetKey (all 4 selector types tested) |
| Snapshot | 1 | 1 | **100%** | tx.Snapshot().Get/GetKey/GetRange |
| Explicit conflict ranges | 4 | 4 | **100%** | AddRead/WriteConflictKey/Range |
| Tx lifecycle | 2 | 2 | **100%** | Cancel, GetVersionstamp |
| Watch | 0 | 1 | 0% | WatchValueRequest — needs new wire type |
| Range introspection | 0 | 2 | 0% | GetApproximateSize, GetEstimatedRangeSizeBytes |
| Transaction options | 1 | 1 | **100%** | SetTimeout, SetRetryLimit, priority, lock-aware reads |
| Tenant API | 4 | 4 | **100%** | Tenant facade with Transact/CreateTransaction, tenantId in wire requests |
| Async (Futures) | 0 | ~10 | 0% | FutureByteSlice, FutureNil, FutureKey, etc. |
| Misc | 0 | 3 | 0% | LocalityGetAddressesForKey, RebootWorker, GetClientStatus |

**Overall: ~42/47 Transaction methods = ~89% API surface.**
**By usage weight: ~99%+ of real application needs covered.**

### Known API gaps in `pkg/fdbgo/fdb/` facade

- [x] **HIGH** — `RangeIterator` eagerly loads all results on first `Advance()`. StreamingMode is accepted but ignored. Record layer uses `Iterator()` in hot paths (index scans, cursor combinators). Implement lazy paging with streaming mode support. (PR #12 merged)
- [x] **HIGH** — Tenant support: thread tenantId through all wire requests, location cache tenant-aware. `Tenant` facade with `Transact/CreateTransaction`. (PR #18 merged)
- [x] **MEDIUM** — Watch API: `WatchValueRequest` wire type codegen + `Transaction.Watch()`. Implemented in nightshift-1: wire types (file_id 14747733/3), endpoint 10, long-poll semantics, read conflict on watched key.
- [x] **MEDIUM** — `GetEstimatedRangeSizeBytes` via `WaitMetricsRequest` codegen.

### Record layer integration with pure Go client

**Status: 2305/2309 record layer tests pass (0 fail, 4 skip performance-only).**

Import swap: all `pkg/recordlayer/`, `example/`, `conformance/` use `pkg/fdbgo/fdb`. Tuple and subspace vendored at `pkg/fdbgo/fdb/tuple/` and `pkg/fdbgo/fdb/subspace/`.

#### Done
- [x] **CRITICAL — ConnectPacket canonicalRemotePort** — was sending TCP source port (random ephemeral), causing FDB server assertion crash through socat proxy. C++ pure clients send port=0. Fixed: `CanonicalRemotePort: 0`. Root cause of ALL test hangs.
- [x] **RYW cache** — `ryw.go`: Set/Clear/ClearRange/Atomic tracking, Get/GetRange interception with merge, thread-safe (sync.Mutex). Fixed 770→0 failures.
- [x] **RYW atomic ops** — all mirror C++ Atomic.h exactly: doAdd, doAnd(V2), doOr, doXor, doMax, doMin(V2), doByteMax, doByteMin, doAppendIfFits, doCompareAndClear.
- [x] **API version upgrade** — Min→MinV2, And→AndV2 for API >= 510 (C++ atomicOp).
- [x] **GetVersionstamp in Transact()** — `commitDone` channel initialized, closed after auto-commit.
- [x] **Empty value Set** — `make([]byte, len(value))` instead of `append(nil, value...)`.
- [x] **StreamingMode constants** — fixed all values to match Apple binding.
- [x] **GetRangeSplitPoints** — SplitRangeRequest/Reply wire types generated, endpoint 12.

#### Remaining work — marathon to all-green

##### CRITICAL — CI blockers (3 failures in CI run)

- [x] **HIGH — HNSW 500-vector test timeout — FIXED** — Root cause: batched `Get` calls (N futures pipelined) were 36x slower than CGo because each goroutine did a full send+wait round-trip sequentially. Fix: `GetPipelined` sends the request frame synchronously (no goroutine), defers TCP flush; `PendingGet.Resolve()` flushes once then waits. All N frames go to the write buffer before any flush, so they reach the server in one TCP write. Result: batch-10 latency 11ms → 1.3ms (8.5x improvement). 500-vector test: timeout → passes in 21s. Also added `TCP_NODELAY`, `bufio.Writer` on connection, `maxRelocateRetries=5`.
- [x] **~~MEDIUM~~ RESOLVED — Pure Go client performance gap with CGo** — Was 3-4x slower. nightshift-1 optimizations brought Go to 17% FASTER than CGo on single Get (175µs vs 205µs). See "RESOLVED — Performance" section below.

  **Completed in nightshift-1:**
  - [x] **(a) Dedicated flush goroutine** — writeLoop with channel-based frame coalescing
  - [x] **(c) Sorted location cache** — O(log N) binary search replacing O(N) linear scan
  - [x] **(f) Per-priority GRV batchers** — isolated DEFAULT/BATCH/SYSTEM_IMMEDIATE
  - [x] Buffer pooling (WriteFrame, reply channels, error channels)
  - [x] Fast UID generation (SplitMix64 replacing crypto/rand)
  - [x] Pooled timers (replacing context.WithTimeout per RPC)
  - [x] QueueModel load balancing for storage servers
  - [x] Proxy round-robin for GRV/commit

  **Remaining (LOW — Go already beats CGo on reads):**
  - [ ] **(b) `net.Buffers` (writev)** — scatter-gather I/O. Low impact now.
  - [ ] **(d) LRU eviction** — random eviction works well enough.
  - [ ] **(h) Eliminate goroutine per PendingGet** — already lazy (no goroutine on pipelined path).
  - [ ] **(i) Batch `locate()` for multi-shard Gets** — single-shard clusters don't benefit.
  - [x] **(j) RYW Set→Clear gap** — FALSE ALARM.
  - [ ] **(k) Pre-allocate prefixed keys** — commit path, not read path. Low priority.
  - [ ] **(l) Intern metadataVersionKey** — micro-optimization.
- [x] **HIGH — Tenant CRUD via system keys** — Full 1:1 port of C++ `TenantAPI::createTenantTransaction` / `deleteTenantTransaction`. All codec formats match C++: TenantIdCodec (raw 8-byte BE), TupleCodec<int64_t> (nameIndex, lastTenantId), BinaryCodec (count), ObjectCodec+IncludeVersion (tenantMap), SetVersionstampedValue (lastModification). All checks: `checkTenantMode`, prefix emptiness (create → `tenant_prefix_allocator_conflict`, delete → `tenant_not_empty`), count validation (`cluster_no_capacity`, MAX=1M), name validation (no `\xff` prefix). `applyTenantPrefix` on commit (8-byte BE prefix on mutations/conflict ranges). Test: `TestTenantCRUD` covers create, list, open, read/write through tenant, duplicate create, non-empty delete, clear+delete, double delete.
- [ ] **LOW — Tenant groups** (metacluster-only) — `tenantGroupTenantIndex`, `tenantGroupMap` (IncludeVersion), group cleanup on delete. C++ `TenantMetadataSpecification` defines group subspace at `\xff/tenant/tenantGroup/`. Not needed for standalone clusters.
- [ ] **LOW — Tenant tombstones** (metacluster data cluster feature) — `tenantTombstones` set, `tombstoneCleanupData` (IncludeVersion), `markTenantTombstones` on delete. Prevents tenant ID reuse across metacluster deletions. Not applicable to standalone.
- [ ] **LOW — Tenant ID prefix** (multi-cluster ID partitioning) — `tenantIdPrefix` at `\xff/tenant/idPrefix`, shifts prefix into upper 2 bytes of 8-byte ID (`tenantIdPrefix << 48`). `computeNextTenantId` validates 48-bit space. Standalone clusters use prefix=0.
- [x] **~~CRITICAL~~ — `bootstrap()` hangs forever** — STALE. `gofdbhelper` package was deleted (commit `28c8d0a`). `fdb.OpenDatabase()` already has 60s bootstrap timeout. `client.OpenDatabase()` takes context with caller-controlled deadline.
- [x] **~~CRITICAL~~ — Conformance `SetupTenantEnvironment` hangs** — STALE. Same root cause resolved by gofdbhelper deletion.

##### A) Record layer integration tests
- [x] 2305/2309 pass, 0 fail
- [x] OnlineIndexer limit=1 — PASSES (6s). Was never broken, only timed out when run alongside hanging 500-vector test.
- [x] VectorIndex "medium-scale search with 500 vectors" — FIXED. Was 36x slower than CGo due to missing request pipelining. `GetPipelined` + deferred flush fix brought it from timeout to 21s.
- [ ] million_record — tagged `manual`, never runs in CI.

##### B) Conformance tests
- [x] Conformance uses pure Go client (`gofdb.OpenDatabase` in container_test.go:165).
- [x] Tenant conformance: `createGoTenant` uses native system key CRUD via `db.CreateTenant()` (no fdbcli).

##### C) Chaos tests
- [x] Race test: verified in nightshift-1 (4 Docker tests under `-race`, 0 warnings).
- [x] Chaos tests: PASS with pure Go client (nightshift-1 verification run).

##### D) fdbgo unit tests
- [x] client_test, fdb_test: timeouts set (client_test=eternal/3600s, fdb_test=long/900s). Fixed in nightshift-1.

#### Architecture
- `gofdbhelper` package provides `OpenDatabase`/`CreateTenant` without import cycle.
- `just test` uses `--local_test_jobs=1` to prevent Docker resource exhaustion.
- `just fmt`/`just lint` use Bazel-managed gofumpt.

### Way of working

**C code is the source of truth for the client.** Development is test-driven from the C binding tests:

1. **Port tests** from `bindings/c/test/unit/unit_tests.cpp` (81 tests, 43 ported so far)
2. **Add functionality** only when a test needs it — no speculative API additions
3. **Principles first** — if a test needs a feature, port it COMPLETELY, not a stub
4. **Tests are authoritative** — if Go behavior differs from C, Go is wrong
5. **After tests pass**, launch 5 FDB maintainer subagents to cross-review Go vs C++ source
6. **Track progress** in this TODO.md — mark tests ported, APIs added, bugs found
7. Each ported test references the C source file + line number + GitHub link

This workflow already found 2 critical bugs on first run: 13 wrong mutation type wire values, GetRange key selector skipping first key.

### C binding test port status

Source: `bindings/c/test/unit/unit_tests.cpp` (81 test cases)

**Ported (80 tests) — `c_binding_port_test.go`:**
- [x] GetRange (forward, reverse, limit, empty, streaming modes, exact)
- [x] All 12 atomic ops (ADD, AND, OR, XOR, CompareAndClear, AppendIfFits, Max, Min, ByteMax, ByteMin, SetVersionstampedKey, SetVersionstampedValue) + MultipleAtomicOps
- [x] SetReadVersion old/future, GetCommittedVersion (read-only + write)
- [x] Cancel, AddConflictRange, CommitDoesNotReset, ErrorPredicate
- [x] Timeout (set, get, preserved across reset, disabled, commit check), RetryLimit (set, zero, unlimited)
- [x] GetApproximateSize, ClearRangeAndVerify, LargeValue, EmptyKeyValue
- [x] GetKey with all selectors, ReadYourWrites, Watch
- [x] RYW disable, Snapshot RYW enable/disable (dayshift-1)
- [x] SizeLimit (too small, too large, minimum valid) (dayshift-1)
- [x] Watch with RYW disabled (dayshift-1)
- [x] System key access: cannot read, read with option, cannot write, write with option (dayshift-1)
- [x] Versionstamp invalid index (key, value, too-short), valid boundary offset (swingshift-1)
- [x] Transaction Reset (basic reuse, retry count clear, read version clear, cancel→reset) (swingshift-1)
- [x] GetLocations, write-write conflict detection (swingshift-1)
- [x] Transaction reuse after commit, database-level AccessSystemKeys (swingshift-1)
- [x] OnError retry semantics (retry limit, non-retryable, non-FDB errors) (swingshift-1)

**Next to port (need API additions):**
- [x] **Transaction options** — `SetTimeout`, `SetRetryLimit`, priority, lock-aware reads. Wired through to GRV request. (PR #10 + review rounds)
- [x] **GetRange streaming modes** — lazy RangeIterator with streaming mode support. (PR #12 merged)
- [x] **Tenant** — tenantId threaded through wire requests, location cache tenant-aware, Tenant facade. (PR #18 merged)
- [x] **RYW disable** — `SetReadYourWritesDisable()` intentional no-op (Go client has no client-side write cache).
- [x] **GetRange reverse** — full implementation with `RangeOptions{Reverse: true}`, integration tests.
- [x] **Watch** — `fdb_transaction_watch` (~4 tests). Implemented in nightshift-1: WatchValueRequest wire type + Transaction.Watch() + integration test.
- [x] **GetApproximateSize** — 1 test. TestGetApproximateSize_CPort added in nightshift-1.

**Not applicable (internal/niche):**
- System key read/write (server-level permissions)
- Mapped range (niche feature)
- Blob granule (niche feature)
- Special-key-space (client internals)
- Database reboot/force recovery/snapshot (admin ops)
- Fast alloc thread cleanup (C runtime)
- Network busyness monitoring (C runtime)

**Gold standard target:** FDB binding tester (stack machine, `bindings/bindingtester/`) — 47 core ops, language-agnostic spec. Passing this = officially conformant binding.

### Wire compatibility verification

**Status:** Binding tester passes 145 seeds × 1000 ops (145,000 operations, 0 failures). This covers API behavioral correctness but not byte-level wire identity.

**What the binding tester covers:** GET/SET/CLEAR/GET_RANGE, atomic ops, conflict ranges, key selectors, error handling, ON_ERROR retry, versionstamp ops. Single-node Docker, single-threaded.

**What it does NOT cover:**
- Byte-level serialization identity (our bytes accepted by server ≠ identical to C client bytes)
- Large payloads near FDB limits (100KB values, 10MB transactions)
- All message types (no WATCH, GET_MAPPED_KEY_VALUES, tenant ops)
- Multi-node clusters, proxy failover, shard splits
- Concurrent connections (`--no-threads` only)
- FDB version compatibility (only 7.3.75 tested)

**Tooling to build (priority order):**

- [x] **Differential serialization fuzzer** — DONE. `cmd/fdb-diff-oracle/`: C++ oracle (ObjectWriter) + Go fuzz test. 18 message types, 17 oracle-compared + 1 Go-only (ReplyPromise). 600M+ executions at 10min/target. Branch: `diff-serialization-fuzzer`, PR #7.

  **Bugs found and fixed:**
  - [x] Empty vector-of-struct reloff bug: `CommitTransactionRef.writeToBuffer` guarded `WriteRelativeOffset` with `if len(...) > 0`, skipping reloffs for empty vectors. C++ always writes reloffs to empty-vector sentinels. Fixed by removing guards.

  **Known divergences (harmless):**
  - VTable pack ordering: C++ `std::set<VTable*>` sorts by pointer address (non-deterministic across binaries). Causes soffset differences at every object start. FDB deserializer is order-agnostic — follows soffsets, doesn't care about vtable position.

  **Remaining issues found by fuzzer (not yet fixed):**
  - [ ] **IPAddress variant serialization**: Go's `MarshalFDB` for `IPAddress` doesn't write the variant tag/payload in the `writeToBuffer` path. `NetworkAddress` and `Endpoint` serialize without IP data. Low priority — we never construct NetworkAddress/Endpoint for sending, only parse them from server responses.
  - [ ] **Codegen: stop emitting dead DirectWriter methods**: The C++ extractor emits `blobSize`, `writeBlob`, `measureEndOff`, `writeDirect` methods that are never called (MarshalFDB uses `precomputeSize`+`writeToBuffer` exclusively). The `CommitTransactionRef` versions still contain the empty-vector-reloff bug. Fix the generator to stop emitting these methods — they add ~230 lines of buggy dead code per type.
  - ~~Arena field missing from codegen~~ — FALSE ALARM. `scalar_traits<Arena>::size = 0`, save is a no-op. Arena is FDB's zero-copy memory management: on deserialize, `context.addArena(arena)` transfers buffer ownership so `StringRef` fields can point into raw received bytes without copying. On serialize, Arena contributes zero bytes. Our codegen correctly skips it.
- [x] **Cross-client interop tests** — nightshift-1: 8 tests in bench/interop_test.go (GoWrite/CGoRead, CGoWrite/GoRead, MixedWrite, AtomicAdd, ClearRange, GetRange, Versionstamp, ConflictDetection).
- [ ] ~~Wire proxy comparator~~ — DROPPED. Capturing frames from both clients and diffing doesn't work: GRV values, reply tokens, retry timing, shard cache state all differ between runs. Would need deep semantic normalization, not worth the complexity vs the fuzzer approach.

**Debug tooling (done):**
- [x] `FDB_WIRE_LOG` env var captures all frames to binary file
- [x] `cmd/fdb-wirelog-dump` decodes wire log (hex dump, per-frame)
- [x] `cmd/fdb-binding-stress` — Go tool for multi-seed stress testing with per-seed artifact collection (FDB trace logs, docker logs, tester output, JSON report)
- [x] FDB crash debugging playbook in `pkg/fdbgo/client/CRASH_BUG.md` (addr2line + debug symbols from GitHub releases)

### Next priorities
1. ~~Differential serialization fuzzer~~ — DONE (PR #7)
2. ~~Transaction options (SetRetryLimit, SetTimeout)~~ — DONE (PR #10)
3. ~~GetRange reverse~~ — DONE
4. Watch — port C tests, implement WatchValueRequest
5. Public API package (`pkg/fdbgo/fdb/`) — drop-in replacement surface

### Done

- [x] Wire serde runtime (`pkg/fdbgo/wire/`) — VTable generation, FDB FlatBuffers writer/reader. All type categories: inline scalars, bytes, vectors, optionals, nested structs. Reader handles protocol version prefix. 359 tests.
- [x] Go header parser (`cmd/fdb-wire-schema-generator/`) — parses all FDB C++ headers, extracts 369 protocol messages. Multi-line serializer support. 100% field type resolution (1545/1545).
- [x] Per-message schema files — 369 JSON files in `pkg/fdbgo/wire/schema/`.
- [x] Ground-truth test vectors — 322 JSON files in `pkg/fdbgo/wire/testdata/`. Serialized by FDB's real ObjectWriter inside Docker (`foundationdb/build:rockylinux9-latest`). 47 Interface types skipped (RequestStream vtable crash).
- [x] ~~Go code generator (`cmd/fdb-wire-codegen/`)~~ — **DELETED.** Superseded by AI-driven porting from C++ source via `/port-fdb-type` skill. C++ is the spec; Go types are hand-ported using vtable constants from `vtables_generated.go`.
- [x] ~~Protocol package~~ deleted — replaced by `wire/types/` with C++ extractor-generated vtables + typed structs. Single source of truth.
- [x] Transport layer (`pkg/fdbgo/transport/`) — TCP framing with XXH3-64 checksum, ConnectPacket handshake, multiplexed connections with endpoint token routing. 7 tests.
- [x] Client skeleton (`pkg/fdbgo/client/`) — cluster file parsing, transaction state machine (Set/Clear/Atomic + OnError retry), GRV batcher, locality cache, read/commit path stubs. 12 tests.
- [x] FDB source auto-fetch — `archive_override` in MODULE.bazel, tag 7.3.75. Zero local setup.

### DONE — Connection bootstrap

- [x] **OpenDatabaseCoordRequest → ClientDBInfo** — Coordinator bootstrap fully working. Sends to well-known token UID(-1, 4), receives ClientDBInfo with GRV/commit proxy count. Verified against real FDB 7.3.75 testcontainer through socat proxy.
- [x] **PING keepalive** — Server sends PingRequest on WLTOKEN_PING_PACKET immediately after handshake. Client replies with C++ ground-truth `ErrorOr<EnsureTable<Void>>` bytes (FakeRoot flattens union types). CONNECTION_MONITOR_TIMEOUT=2s.
- [x] **ErrorOr\<T\> response unwrapping** — ErrorOr is a FlatBuffers union (`union_like_traits`): type byte (1-indexed: 0=NONE, 1=Error, 2=T) + value RelativeOffset. FakeRoot flattens union into root object. Detect Error vs ClientDBInfo by vtable field count.
- [x] **ReplyPromise token embedding** — Reply field is a nested struct (vtable {6,20,4}) containing the UID inline at offset 4. Uses `serializable_traits<ReplyPromise>` path.
- [x] **CachedSerialization\<ClientDBInfo\>** — Response uses `serialize_raw` path. The cached bytes ARE the `ErrorOr<EnsureTable<ClientDBInfo>>` blob. No additional unwrapping needed.
- [x] **ConnectPacket IP fix** — IPv4 uses BigEndian (network byte order), not LittleEndian.
- [x] **Correct vtable** — Real vtable from C++ ground-truth test vector: `{22, 49, 20, 24, 28, 4, 32, 36, 40, 44, 48}`. UID fields are 16 bytes INLINE. ReplyPromise is 4-byte RelativeOffset to nested struct.
- [x] **clusterKey** — Must be `"description:id"` only (part before `@`), NOT the full connection string.

### DONE — Proxy address extraction

- [x] **Parse GrvProxyInterface/CommitProxyInterface nested structs** — Full nesting chain decoded from live FDB 7.3.75 response: Proxy[slot3] → Endpoint wrapper → Endpoint inner (UID inline + NetworkAddressList) → NetworkAddress (IP RelOff + port uint16) → IPAddress (RelOff to raw uint32). Correctly extracts `172.21.0.3:PORT` with endpoint tokens.

### DONE — GRV + Read + Write (e2e verified)

- [x] **GetReadVersionRequest/Reply** — GRV batcher with PrepareReply + nested ReplyPromise. Returns real version from FDB 7.3.75.
- [x] **GetKeyServerLocationsRequest** — Storage server address + endpoint token extracted. getAdjustedEndpoint(2).
- [x] **GetValueRequest/Reply** — Reads back values written by C binding.
- [x] **GetKeyValuesRequest/Reply (range reads)** — getAdjustedEndpoint(2), VecSerStrategy::String parsing. limitBytes=INT_MAX.
- [x] **CommitTransactionRequest/CommitID** — Mutations as proper FlatBuffers nested objects. MVCC conflict detection verified (not_committed 1020).
- [x] **Vtable closure infrastructure** — C++ extractor (`emitVTables`), 322 closure files, codegen emits `_VTableClosure` constants, Writer `WriteMessageWithVTables` preserves C++ vtable ordering.

### DONE — VTable extraction: 100% golden match

- [x] `vtables_generated.go` — 13 message types + 7 nested types, all matching golden test vectors byte-for-byte
- [x] `protocol/` vtables — all 13 message types updated to match golden (removed trailing zeros, fixed field offsets for GetReadVersionReply, GetKeyServerLocationsReply, OpenDatabaseCoordRequest)
- [x] C++ extractor fix — `extractRootVTable()` reads root vtable from ObjectWriter bytes (authoritative) instead of TypeVisitor (which misses conditionally-serialized fields like tenantInfo)
- [x] Regression test — `TestVTableConformance` in protocol_test.go verifies all 13 types against vtable JSON files
- [x] Root cause: TypeVisitor had `isSerializing=false`, missing fields gated by `Ar::isSerializing` in FDB's serialize() methods

**After this:** `/port-fdb-type` skill uses these constants to write `MarshalInto` / `UnmarshalFrom` implementations in `pkg/fdbgo/wire/types/`.

---

### CRITICAL — v4 approach: C++ source is the spec, Go ports it mechanically

FDB's wire format is defined by C++ struct `serialize()` methods and the `flat_buffers.h` type trait system. It is NOT an IDL — it's C++ code. Our approach mirrors this directly.

#### Architecture

Two layers:

- **`pkg/fdbgo/wire/`** — Framework. Knows how FDB FlatBuffers primitives are serialized: scalars inline (LE), StringRef as `[len(4)][data]` via RelOff, nested structs via vtable+soffset, Optionals as 2 vtable slots, VectorRef as `[count][RelOffs]`. ObjectWriter, Reader, VTable computation. Written by hand, stable.

- **`pkg/fdbgo/wire/types/`** — One Go file per C++ type that has `serialize()`. Mechanical port of C++ → Go. Each file implements `wire.FDBSerializable` interface: `MarshalInto(*ObjectWriter)`, `UnmarshalFrom(*Reader)`, `TypeVTable()`. References the `wire` package for all byte-level operations.

#### What is static (constants from C++)

VTables are 100% deterministic — computed by the C++ compiler from field types/sizes/alignments. We extract them ONCE using the C++ schema extractor (`cmd/fdb-schema-extract/`), which compiles against real FDB headers and dumps vtables + vtable closures + field traits. These become Go constants.

Per type:
- VTable: `var SpanContextVTable = wire.VTable{10, 29, 4, 20, 28}`
- VTable closure (for top-level messages): all transitively reachable vtables
- Per-field: trait (scalar/dynamic_size/serialize_member/union_like), size, alignment, indirection

See `docs/wire-format-static-vs-logic.md` for the full static-vs-logic split.

#### What is logic (Go code ported from C++)

Each type's `serialize()` method. Most types have a straight `serializer(ar, field1, field2, ...)` — purely mechanical, no branches. A few types (MutationRef, KeyRangeRef) have data-dependent conditional branches that must be ported as Go logic.

#### How types get implemented

**AI-driven via `/port-fdb-type` skill** (`.claude/skills/port-fdb-type.md`):
1. Read the C++ struct definition + `serialize()` method
2. Read the vtable from the C++ extractor output
3. Write a Go file in `pkg/fdbgo/wire/types/` implementing `FDBSerializable`
4. Header comment links back to C++ source file + line number
5. Verify against C++ test vector if available

Each Go file is a direct, traceable port. The C++ source is the spec. No JSON intermediary for the logic — just C++ in, Go out.

**Primitives** (no file needed):
- `int64`, `uint64`, `int32`, `uint32`, `uint8`, `bool`, `float64` — Go builtins
- `UID` — `scalar_traits`, 16 bytes inline. Already `transport.UID{First, Second uint64}`
- `Tag` — `struct_like_traits`, inlined. Define inline where used
- `StringRef` / `Key` / `Value` — `[]byte`
- `Arena` — skip (zero-size)

#### Dependency order

Leaves first, then compound types:
1. `Error`, `SpanContext`, `KeySelectorRef`, `ReadOptions` (no nested serialize_member fields)
2. `MutationRef`, `KeyRangeRef` (have branches but no nested types)
3. `CommitTransactionRef` (contains VectorRef<MutationRef>, VectorRef<KeyRangeRef>)
4. Request/reply types: `GetValueRequest`, `GetKeyValuesRequest`, `CommitTransactionRequest`, etc.

#### Tools

- **C++ schema extractor** (`cmd/fdb-schema-extract/`): pure C++ binary, compiles against real FDB. Extracts vtables, closures, field traits. Run via Docker. Output: per-type constants.
- **`/port-fdb-type` skill**: AI reads C++ source → writes Go file. Mechanical port.
- **`wire.FDBSerializable` interface**: `MarshalInto`, `UnmarshalFrom`, `TypeVTable`. All types implement it uniformly.

### Cleanup — done

- [x] Replace inline vtable literals with types.* constants
- [x] Fix wrong TenantInfo vtable in locality.go
- [x] Encapsulate parseKeyValueVector in wire/types/
- [x] Build voidReply through wire.Writer (WriteRootObject)

### DONE — Port request types to wire/types/

- [x] All 6 request types ported to typed structs with MarshalFDB()
- [x] Shared helpers: WriteReplyPromise, WriteTenantInfo, writeKeySelectorRef
- [x] Zero ObjectWriter calls in client code

### HIGH — Generated wire types: struct + marshal/unmarshal from C++ extractor

**Architecture**: The C++ extractor (`cmd/fdb-schema-extract/`) is the single source of truth for wire layout. It should generate Go structs + UnmarshalFDB + MarshalFields for every message type. C++ serialize() has real logic (branches, conditionals) — the extractor captures the **structural** part (fields, slots, types, nesting), while AI ports only the **logic** part.

**What the extractor should generate per type:**

```go
// Code generated by fdb-schema-extract. DO NOT EDIT.
type GetReadVersionReply struct {
    ProcessBusyTime int32   // slot 0, int, ReadInt32
    Version         int64   // slot 1, Version, ReadInt64
    ...
}
func (m *GetReadVersionReply) UnmarshalFDB(data []byte) error { /* generated */ }
func (m *GetReadVersionReply) MarshalFields(obj *wire.ObjectWriter) { /* generated */ }
func (m *GetReadVersionReply) MarshalFDB() []byte { /* generated, wraps MarshalFields */ }
```

**Zero-allocation principle**: All slot numbers, nesting depth, reader/writer methods baked in at generation time. No `[]FieldDef` slices, no runtime type switches, no intermediate structs for nested types. Generated code is a flat sequence of `r.ReadT(N)` / `obj.WriteT(offset, value)`. Pair types flatten nesting inline — `ReadNestedReader(0)` then `ReadBytes(0)`/`ReadBytes(1)` directly into parent struct fields, no intermediate allocation.

**For types with conditional serialize() logic** (MutationRef, KeyRangeRef, CommitTransactionRequest): AI overrides `MarshalFDB` in a separate `_custom.go` file. Uses the generated struct definition + `MarshalFields` for common fields, adds branching logic on top.

**For compound pair types** (`pair<KeyRangeRef, vector<SS>>`): extractor generates a concrete Go struct per instantiation with flattened fields. ~5 pair instantiations in FDB protocol.

**Status:** Zero `_custom.go` files. All struct definitions, `UnmarshalFDB`, `MarshalInto`, `MarshalFDB`, `MarshalStructBlob`, `WriteNested` are generated by the C++ extractor. Client code constructs generated structs and calls `MarshalFDB()`.

**3 `_helpers.go` files remain** — these are hand-written types disguised as helpers. They contain real structs (`EndpointInfo`, `KeyValue`, `LocationResult`) and parse functions that the extractor doesn't yet generate. Target: zero `_helpers.go`.

- [x] **HIGH — #1: MarshalInto for nested types** — DONE. Generated for all types.
- [x] **HIGH — #4: CommitTransactionRequest full generation** — DONE. Generated with WriteRawOOL for vector fields + nested WriteStruct for CommitTransactionRef.
- [x] **HIGH — #6: Request type MarshalFDB generation** — DONE. All 7 request types flipped to EmitStructs=true. Zero `_custom.go`.
- [x] **HIGH — #2: VecSerStrategy::String typed parser** — `ParseKeyValueRefStringVector()` in `keyvalueref_generated.go`. Helper file eliminated.
- [x] **HIGH — #3: Endpoint nested unmarshal chain** — `ReadEndpointFromSlot()` in `client/endpoint.go` + generated types. Helper file eliminated.
- [x] **HIGH — #5: Pair type decomposition** — `GetKeyServerLocationsReply` generated with proper struct. Helper file eliminated.

**Bugs fixed during this work:**
- Scalar enum fallback: `TraceFlags` (enum uint8_t) mapped to []byte → OOB panic. Fixed: size-based fallback in pushField.
- Standalone<T> trait: `Key`/`Value` (Standalone<StringRef>) classified as serialize_member. Fixed: detect inner type's dynamic_size_traits.
- Standalone<VectorRef<T>> trait: classified as dynamic_size → WriteBytes (wrong). Fixed: detect inner vector_like_traits → WriteRawOOL.
- Nil []byte writes: empty vector fields written as OOL with RelOff → FDB rejected. Fixed: skip nil []byte in generated MarshalFDB/MarshalInto.

### HIGH — Audit findings: remaining hardcoded offsets and missing schema constants

**Hardcoded byte offsets in wire/types/ MarshalInto (should use `int(vt[N])`):**
- [x] `error.go` — Fixed: `WriteUint16` (was `WriteInt32`), matches schema `uint16_t` wire type
- [x] `key_selector_ref.go` — Fixed: regenerated by schema extractor v5, uses `vt[Slot+2]`
- [x] `read_options.go` — Fixed: regenerated by schema extractor v5, uses `vt[Slot+2]`
- [x] `mutation_ref.go` — Fixed: regenerated by schema extractor v5, uses `vt[Slot+2]`
- [x] `key_range_ref.go` — Fixed: custom file already uses `vt[Slot+2]`

**Missing vtable constants for response type parsing (hardcoded slot numbers):**
- [x] `coordinator.go` — Already uses generated constants: `ClientDBInfoSlotGrvProxies`, `ClientDBInfoSlotCommitProxies`, `ClientDBInfoSlotId`, `GrvProxyInterfaceSlotGetConsistentReadVersion`, `CommitProxyInterfaceSlotCommit`
- [x] `network_types.go` — All generated: `EndpointSlotAddresses`, `EndpointSlotToken`, `NetworkAddressSlotIp`, `IPAddressSlotAddr`. No `network_types.go` file remains — replaced by `endpoint_generated.go`, `networkaddress_generated.go`, `ipaddress_generated.go`.
- [x] `endpoint.go` — `ReadEndpointFromSlot` uses schema-driven nested reader chain. No hardcoded byte offsets.
- [x] IPv6 — `ipAddressString` handles `AddrTag=2` (IPv6, 16-byte address). Was previously silent, now returns `::0` for short data.

**Hardcoded endpoint indices (StorageServerInterface/CommitProxyInterface method positions):**
- [x] `readpath.go` — already uses `EndpointGetKey`/`EndpointGetKeyValues`/`EndpointWatchValue`
- [x] `locality.go` — already uses `EndpointGetKeyServerLocations` for getAdjustedEndpoint
- [x] `locality.go` — `ReadEndpointFromSlot(ssR, 2)` → `StorageServerInterfaceSlotField_2`
- [ ] `locality.go` — brute-force slot scanning in parseGetKeyServerLocationsReply (heuristic, acceptable)

**Magic numbers → named constants:**
- [x] `-1` for no-tenant → already `NoTenantID int64 = -1`, no bare `-1` tenant uses remain
- [x] `5*time.Second` timeout → `DefaultRPCTimeout`; bootstrap cap → `BootstrapMaxBackoff`
- [x] `0x7FFFFFFF` limitBytes → already `UnlimitedBytes`, used everywhere
- [x] `5` retry limit → already `MaxWrongShardRetries`
- [x] `coordinator.go` — `slotOffset` already removed; `30s` timeout → `CoordinatorTimeout`
- [x] `MinTenantVersion: -2` → `LatestVersion`; `MaxVersion: -1` → `InvalidVersion`
- [x] `fdbErr.Code == 4` → `ErrOperationFailed`
- [x] `ReadEndpointFromSlot(ssR, 2)` → `StorageServerInterfaceSlotField_2`
- [x] Full StorageServerInterface endpoint enum (0-13) in transaction.go
- [x] `EndpointGetRangeSplitPoints` moved from metrics.go to transaction.go with all other endpoints

### HIGH — Remaining client features

- [x] **Topology monitoring** — Already implemented: topologyMonitor with kick-triggered rapid burst.
- [x] **Storage server routing** — LocationCache.refresh() parses GetKeyServerLocationsReply properly through wire.Reader (replaced IP pattern hack).
- [x] **wrong_shard_server handling** — detect error code 1062 in ErrorOr response, invalidate locality cache, retry with backoff. Integration test via `wrongShardConn` pipe-based TCP proxy + `buildFDBErrorResponse`. Also fixed: `refresh()` now caches shard ranges (was never populating cache), `parseGetKeyServerLocationsReply` parses `KeyRangeRef` as nested struct (was misreading RelOff as bytes). `Error.MarshalInto` fixed: `error_code` is uint16 on wire, not int32.
- [x] **LoadBalance** — nightshift-1: QueueModel-based server selection (latency EMA + inflight tracking), failover with exponential backoff, proxy round-robin.
- [x] **Self-conflicting transaction injection** — `makeSelfConflicting()` for `commit_unknown_result` resolution. OnError(1021) copies write conflicts into read conflicts before reset.
- [x] **Atomic operations serialization** — All 14 mutation types implemented and tested (12 C binding port tests + binding tester 145K ops).

### HIGH — Public API

- [x] **`pkg/fdbgo/fdb/` package** — drop-in replacement API surface: `Database`, `Transaction`, `Transactor`, `ReadTransactor`, `ReadTransaction`, `FutureByteSlice`, `FutureNil`, `FutureKey`, `Key`, `KeyValue`, `KeySelector`, `KeyRange`, `RangeResult`, `RangeOptions`, `StreamingMode`, `Error`, atomic ops, `Snapshot`, `Tenant`. 14 files, 20+ integration tests.
- [x] **Subspace/Tuple** — using upstream `apple/foundationdb/bindings/go` (pure Go, no CGo needed).
- [x] **Transaction options** — `SetTimeout`, `SetRetryLimit`, `SetPrioritySystemImmediate`, `SetCausalReadRisky`, lock-aware reads, GRV priority. (PR #10 + review rounds)
- [x] **GetVersionstamp** — deferred future resolved after commit. (PR #13 merged)

### MEDIUM — Correctness

- [x] **Multi-shard GetRange** — `getRange` now loops across shard boundaries, advancing `begin` past last returned key and re-locating for the next shard. Single-shard continuation tested via `TestGetRangeWithLimit`.
- [ ] **Multi-shard GetRange integration test** — needs multi-node testcontainer support (single-node = single shard, can't verify cross-shard continuation). Track in testcontainers pkg.
- [x] **Snapshot reads** — `tx.Snapshot().Get()` bypasses read conflict ranges.
- [x] **GetKey (key selectors)** — `FirstGreaterOrEqual`, `LastLessThan`, etc. → `GetKeyRequest` to storage server.
- [x] **commit_unknown_result resolution** — self-conflicting via OnError(1021). Unit tested.
- [x] **commit_unknown_result integration test** — `TestCommitUnknownResult_NoDoubleApply` in fault_test.go (faultDialer + killReads).

### ~~HIGH~~ DONE — Custom dialer + fault injection

`DialFunc` support already implemented in `transport.DialWith()`. Tests use `faultDialer` (fault_test.go) for:
- **commit_unknown_result integration test**: `TestCommitUnknownResult_NoDoubleApply` — faultConn.Read drops commit reply → 1021 → self-conflicting retry → no double-apply
- **wrong_shard_server fault injection**: `TestWrongShardServer_FaultInjection`
- **Custom Docker networking**: used by all testcontainer tests via hybrid cluster config
- [x] **API version gating** — `Min→MinV2`, `And→AndV2` for API version >= 510. Already done in fdb/transaction.go.
- [ ] **Metadata version cache** — special handling for `\xff/metadataVersion` key.

### HIGH — Integration test coverage

Every client feature must be tested against a real FDB testcontainer (not mocks, not unit tests). Current unit-only tests that need integration equivalents:

- [x] **Cancel** — `TestCancel` in correctness_test.go
- [x] **OnError retry codes** — `TestTransactRetry` in setget_test.go, `TestExplicitConflictRanges` in correctness_test.go
- [x] **ReadOnlyCommit** — `TestReadOnlyCommitIntegration` in correctness_test.go
- [x] **AddReadConflictRange** — `TestAddReadConflictRange` in correctness_test.go
- [x] **AddWriteConflictRange** — `TestAddWriteConflictRange` in correctness_test.go

### CRITICAL — Refactor Database into DatabaseContext (C++ alignment)

Our Go `Database` is split across `Database`, `GRVBatcher`, `LocationCache`, and `Cluster` — each with independent state. C++ has a single `DatabaseContext` that owns ALL per-database state: GRV cache, batcher, location cache, proxy list, connection pool, throttle tracking, background actors. Our split architecture creates friction every time we port C++ behavior — cross-component state sharing requires manual wiring that would be trivial in a unified struct. Examples: GRV cache update after commit, cache invalidation on reconnect, topology monitoring feeding proxy list + location cache + GRV cache simultaneously. Refactor `Database` to be the single owner, matching C++ `DatabaseContext` structure.

### ~~CRITICAL~~ RESOLVED — Performance: close the 6.4x gap with CGo client

**nightshift-1 final results** (Ryzen 9 3900X, FDB 7.3.75, 2026-04-10):
- Pure Go GetValue: **201,606 ns/op**, 1,581 B/op, 21 allocs/op
- CGo GetValue: **216,581 ns/op**, 392 B/op, 14 allocs/op
- **Go wins 11/14 benchmarks** including Get (7%), Set (21%), GetRange (2.4x), BatchGet/10 (19%)
- CGo leads only on BatchGet/50 (18%), PipelinedGet/50 (29%), RYW (13%)

**Optimizations applied** (nightshift-1 branch):
- Write coalescing: dedicated writeLoop goroutine, channel-based frame coalescing
- Buffer pooling: sync.Pool for WriteFrame buffers, reply/error channels
- Fast UID generation: SplitMix64 PRNG replacing crypto/rand
- Sorted location cache: O(log N) binary search replacing O(N) linear scan
- Per-priority GRV batchers: isolated DEFAULT/BATCH/SYSTEM_IMMEDIATE
- Pooled timers: replaced context.WithTimeout per RPC with sync.Pool'd time.Timer
- QueueModel load balancing: latency EMA + inflight tracking for server selection
- Proxy round-robin: atomic counter for GRV/commit proxy distribution

**Previous baseline** (Ryzen 9 3900X, FDB 7.3.75, 2026-04-01):
- Pure Go GetValue: **1,350,000 ns/op**, 5,490 B/op, 78 allocs/op
- CGo (libfdb_c) GetValue: **210,000 ns/op**, 383 B/op, 13 allocs/op
- Wire unmarshal alone: 58 ns/op (0.004% of total — NOT the bottleneck)

**CPU profile**: 36% syscalls (TCP read/write), 32% goroutine scheduling (park/unpark/findRunnable). 92% of wall time is WAITING, not computing.

**Allocation profile** (78 allocs per Get, top offenders):

| Source | allocs | Fix |
|---|---|---|
| `WriteMessageWithVTables` | 271k | Pre-pack vtable closure at init |
| `ObjectWriter.WriteStruct` | 286k | Pool ObjectWriter buffers |
| `ObjectWriter.WriteBytes` | 221k | Pool OOL buffers |
| `context.WithDeadline` | 211k | Reuse context or pass parent directly |
| `GRVBatcher.GetReadVersion` | 192k | Pool batch channels/timers |
| `vTableSet.add/addOrdered/pack` | 301k | Pre-pack vtable closure at init (STATIC per type) |
| `Conn.PrepareReply` | 119k | Pool reply channels |
| `ReadFrame` | 112k | Pool frame buffers |
| `wire.NewReader` | 142k | Pool or stack-allocate Reader |
| `time.newTimer` | 80k | Timer pool |

#### Hot path analysis (2026-04-01)

By frequency, ranked by impact:

| Priority | Hot path | Frequency | Current perf | Fix |
|---|---|---|---|---|
| **1** | **GRV round trip** | Every tx | 299ns marshal + 81ns unmarshal + ~600µs RTT | **GRV caching** — skip entire RTT for read-only txns |
| **2** | **ParseKeyValueRefStringVector** | Per range read, scales with result size | 622ns/21 allocs (10 KVs), 5.7µs/201 allocs (100 KVs) | Zero-copy: slice into buffer instead of make+copy per KV. Generator fix needed. |
| **3** | **MarshalStructBlob (mutations/ranges)** | Per mutation + per conflict range on commit | ~~303ns/8 allocs~~ → **95ns/1 alloc** | **DONE** — pooled ObjectWriter + zeroPad. 3.4x speedup. |
| **4** | **GetKeyValues request marshal** | Per range read shard | 756ns/8 allocs (4 nested structs) | Already uses arena for nested structs. 8 allocs = pooledWriter + output buf + nested OOL grows. |
| **5** | **GetValue request marshal** | Per key read | 405ns/2 allocs | Already near-optimal (MessageTemplate + pool). 2 allocs = pooledWriter + output buf. |

C++ comparison: unmarshal is zero-alloc zero-copy (StringRef = fat pointer into arena buffer). Our unmarshal is 1 alloc (Reader vtable slice) at 56-80ns for replies. Not worth optimizing — network RTT dominates by 1000x.

#### Wire type benchmarks (Ryzen 9 3900X, 2026-04-01)

Run: `bazelisk run //pkg/fdbgo/wire/types:types_test -- -test.run='^$' -test.bench=. -test.benchmem`

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| Marshal GetValueRequest | 405 | 296 | 2 |
| Marshal GetKeyValuesRequest | 756 | 456 | 8 |
| Marshal CommitTransactionRequest (5 muts) | 1,254 | 3,676 | 8 |
| Marshal GetReadVersionRequest | 299 | 184 | 2 |
| Marshal MutationRef blob | 95 | 80 | 1 |
| Marshal KeyRangeRef blob | 85 | 64 | 1 |
| Unmarshal GetValueRequest | 237 | 384 | 4 |
| Unmarshal GetValueReply | 56 | 96 | 1 |
| Unmarshal GetKeyValuesReply | 62 | 96 | 1 |
| Unmarshal GetReadVersionReply | 81 | 96 | 1 |
| Unmarshal CommitTransactionRequest | 329 | 480 | 5 |
| Roundtrip GetValueRequest | 681 | 681 | 6 |
| ParseKeyValueRefStringVector (10 KVs) | 622 | 960 | 21 |
| ParseKeyValueRefStringVector (100 KVs) | 5,679 | 9,664 | 201 |
| PackVectorOfStructBlobs (5) | 127 | 432 | 2 |

#### Tier 1: Zero-alloc vtable packing — DONE

- [x] **Pre-pack vtable closures at init** — `MessageTemplate` pre-computes packed vtable bytes + O(1) offset lookup. `WriteMessagePacked` fast path. All top-level `MarshalFDB()` uses this. 2 allocs per message (pooledWriter + output buf).
- [x] **Pool `ObjectWriter` for `MarshalStructBlob`** — Sub-object serialization (MutationRef, KeyRangeRef) went from 8 allocs/303ns to 1 alloc/95ns. sync.Pool + zeroPad static buffer.
- [x] **writerArena for nested structs** — Inline [8]ObjectWriter + [8]nestedStruct + [512]byte bump allocator. Zero heap allocs for nested structs in WriteMessagePacked path.

#### Tier 2: Buffer pooling (HIGH, ~30% of allocs)

- [x] **Pool frame write buffers** — nightshift-1: sync.Pool for WriteFrame buffers (*[]byte).
- [ ] **Pool frame read buffers** — `ReadFrame` allocates `make([]byte, payloadLen)` per response. Pool via `sync.Pool`. (Tricky: consumers hold slices into payload.)
- [x] **Pool reply channels** — nightshift-1: sync.Pool for cancelled PrepareReply channels + error channels for SendFrame/Flush.
- [ ] **Pool Reader structs** — `NewReader` allocates a Reader per parse. Pool via `sync.Pool`. (Low priority — 1 alloc at 56ns.)

#### Tier 3: Reduce syscalls and scheduling (HIGH, main latency source)

- [x] **Batch TCP writes** — nightshift-1: writeLoop goroutine with channel-based coalescing. N concurrent writes → 1 flush.
- [x] **Avoid `context.WithDeadline` per RPC** — nightshift-1: replaced with sync.Pool'd time.Timer on 6 hot-path RPC sites.
- [x] **Avoid `time.NewTimer` per RPC timeout** — nightshift-1: timerPool in rpc.go.

#### Tier 4: Reduce round trips (CRITICAL, main latency source)

- [x] **GRV caching** — `grvCache` with 100ms staleness window + `grvBatcher` with adaptive batching. Cache-hit fast path skips GRV RPC entirely. Background refresher, commit feeds cache, ratekeeper throttle cooldown. Matches C++ `MAX_VERSION_CACHE_LAG`.
- ~~**Pipelined GRV + read**~~ — NOT FEASIBLE. Wire protocol requires Version (int64) at marshal time. Storage server uses `req.version` immediately for `waitForVersion()`. No sentinel, no deferred resolution. C++ doesn't pipeline either — `getValue` blocks on `wait(trState->startTransaction())` before sending read.
- [x] **Connection keep-warm** — nightshift-1: warmConnections() pre-dials all proxies after bootstrap.

#### Tier 5: Generated code improvements (HIGH, scales with data size)

- [x] **ParseKeyValueRefStringVector zero-copy** — Already zero-copy: `data[pos:pos+n:pos+n]` slices into buffer. Only 1 allocation for result slice.
- [ ] **Unmarshal nested struct allocs** — Each `ReadNestedReader` heap-allocates a `*Reader` (4-5 allocs for request types). Could use value-type `Reader` returned by value, but requires API change. Low priority — requests are not on the unmarshal hot path.

### LOW — Missing primitives

- [ ] **Vector\<scalar\> primitive (#8)** — Typed `[]int32`, `[]uint64` etc. instead of `[]byte` for `VectorRef<int>`, `VectorRef<uint64>`. No current FDB types need it (all scalar vectors are accessed as raw bytes). Add when a type with a typed scalar vector is needed.

### Phase 2

- [x] **Watch API** — nightshift-1: WatchValueRequest (file_id 14747733), endpoint 10, long-poll semantics.
- [x] **Directory layer** — ported from Apple Go binding (6 files, ~1300 lines). Cross-client interop verified (Go ↔ CGo). 5 tests.
- [ ] **Version vector support** — causal consistency optimization.
- [x] **Tenant API** — Already complete: `Tenant.Transact()`, `CreateTransaction()`, CRUD via system keys.
- [x] **TLS support** — nightshift-1: TLSConfig + DialWithTLS + upgradeTLS.
- [ ] **Tag throttling** — client-side throttle enforcement.

### Phase 3

- [ ] **Multi-version client** — plugin loading for older client versions.
- [ ] **FDB status JSON parsing** — cluster status monitoring.
- [x] **Binding tester** — 145K ops (145 seeds x 1000) + 50 seeds nightshift-1 = 0 failures.

### HIGH — Client code migration to generated structs

Request type `_custom.go` files are eliminated by flipping `EmitStructs=true` in the C++ extractor. The generated `MarshalFDB()` composes nested structs via `MarshalInto`. Client code constructs the struct and calls `MarshalFDB()` instead of standalone `MarshalXxx(...)` functions.

- [x] **GetReadVersionRequest** — flipped to EmitStructs=true, _custom.go deleted, grv.go updated
- [x] **GetValueRequest** — All request types already use generated structs (v5 codegen). Zero _custom.go files except keyrangeref_custom.go (intentional).
- [x] **GetKeyRequest** — All request types already use generated structs (v5 codegen). Zero _custom.go files except keyrangeref_custom.go (intentional).
- [x] **GetKeyValuesRequest** — All request types already use generated structs (v5 codegen). Zero _custom.go files except keyrangeref_custom.go (intentional).
- [x] **GetKeyServerLocationsRequest** — All request types already use generated structs (v5 codegen). Zero _custom.go files except keyrangeref_custom.go (intentional).
- [x] **OpenDatabaseCoordRequest** — All request types already use generated structs (v5 codegen). Zero _custom.go files except keyrangeref_custom.go (intentional).
- [x] **CommitTransactionRequest** — All request types already use generated structs (v5 codegen). Zero _custom.go files except keyrangeref_custom.go (intentional).

### Wire protocol bugs found by 5-agent review (2026-04-01) — ALL RESOLVED by v5 codegen

v5 composable-primitives generator (RFC 013) rewrote all marshal/unmarshal code. These bugs are no longer relevant:

- [x] **CRITICAL #1 — Reader: ReadVectorInt32/ReadVectorUint64 wrong RelOff** — Code already fixed (`r.objPos + int(off) + int(relOffset)`). Additionally, zero callers in v5 generated code — each type has its own `UnmarshalFromReader`.
- [x] **CRITICAL #2 — Reader: ReadOptionalInt32/ReadOptionalString wrong RelOff** — Same: already fixed + zero callers.
- [x] **CRITICAL #3 — Writer: VTable hash key ignores field offsets** — `makeVTableKey` now hashes full vtable content (all uint16 entries as bytes). No collision possible.
- [x] **HIGH #6 — Generated: MarshalInto missing nested struct writes** — `MarshalInto` deleted. v5 uses `measureEndOff`/`writeDirect`/`MarshalFDB` exclusively.
- [x] **HIGH #7 — Extractor: scalar fallback assumes unsigned** — `GoTypeMapping` deleted. v5 extractor uses `classifyField()` with proper C++ type trait inspection.
- [x] **MEDIUM #9 — Reader: ReadUID no bounds check** — Already fixed: `off+16 > len(r.object)` guard added.
- [x] **MEDIUM #10 — Reader: fieldOffset no vtable bounds check** — Already fixed: `byteOff+2 > len(r.vtable)` guard added.
- [x] **MEDIUM #17 — Client: CommitTransactionRef Field_0/1/2/3 fragile** — v5 generates named fields (Mutations, ReadConflictRanges, WriteConflictRanges).

Remaining (still relevant but low impact):
- [x] **MEDIUM #11 — Writer: nil vs empty []byte** — Verified: both nil and `[]byte{}` serialize as 4-byte length=0 entry via `VisitDynamicSize(0)`, matching C++ `StringRef()` behavior. No distinction on wire. Not a bug.
- [x] **MEDIUM #14 — Extractor: variant tag=0 not handled** — Verified: tag=0 means `valueless_by_exception` (no value present). The generated switch correctly falls through — struct fields retain zero values. Not a bug.
- [x] **MEDIUM #15 — VecSerStrategy parser DoS** — Fixed: `make()` capacity clamped to `(len(data)-pos)/minElementSize` in all three parsers (ParseKeyRefStringVector, ParseKeyRangeRefStringVector, ParseKeyValueRefStringVector). Prevents OOM from crafted count values in untrusted wire data.
- [x] **MEDIUM #4 — Client: sendGetValue should use EndpointGetValue constant** — Fixed in nightshift-1 constants cleanup. All endpoint constants now named and centralized.
