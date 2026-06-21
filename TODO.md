# TODOs

FoundationDB Record Layer — Go Port. Java version: **4.12.11.0**. FDB wire protocol: **7.3.75**.

Current state: 46 test targets, 639+ SQL tests passing, 270 yamsql scenarios, 508 cross-engine specs, 105 fuzz targets, ~65 Cascades rules, 41 plan types (36 executor-wired), 48 value types, 9 predicate types. Unified Cascades task stack (REWRITING + PLANNING). Winner-based plan selection with per-ordering properties.

---

# NEXT

Post RFC-115/116/117/118. The pure-Go client (`pkg/fdbgo`) is launch-ready on correctness + wire
compatibility; everything here is polish/parity/infra — **none gates adoption**. Priority order below;
details live in the phase/section the pointer names. Client items are fresh `fdb-client-engineer` RFC
cycles; query-engine items are `query-engine`/`todo-worker` cycles with a Graefe ACK gate.

1. **[x] C3 (conformance) — Ride their test designs: port FDB's adversarial workloads. COMPLETE.** Cycle /
   AtomicOps / ConflictRange / Serializability / FuzzApiCorrectness reimplemented as scenario +
   invariant specs driving the Go client against testcontainers + `SimTransport` (C4/RFC-118).
   **Increment 1 DONE:** Cycle workload — pure-client serializability oracle (RFC-119, PR #308).
   **Increment 2 DONE:** Cycle under injected wire faults via SimTransport (RFC-120, PR #309).
   **Increment 3 DONE:** Cycle under `process_behind (1037)` + `wrong_shard (1001)` faults (RFC-122,
   PR #320) — 1037 the fixed/QueueModel read row, 1001 its own relocate+invalidate ring-survival
   assertion (flake-free: budget exhaustion → retryable 1007).
   **Increment 4 DONE:** Cycle under a dropped commit reply → `commit_unknown_result (1021)` (RFC-123,
   PR #321) — the faithful commit-path fault (1021 is client-minted from an ambiguous RPC, so a dropped
   reply, not a synthetic error; `not_committed` deliberately NOT injected — unfaithful on an applied
   commit, already exercised by the workload's real conflicts). Drives `commitDummyTransaction` +
   `onError(1021)` self-conflicting retry; ring survives whether or not the dropped commit applied.
   **Increment 5 DONE:** AtomicOps workload (RFC-124, PR #322) — atomic-op + unique-per-attempt
   companion log in one txn; per-group `sum(log)==sum(ops)` oracle holds exactly, healthy AND under the
   commit-drop fault (proving atomic-op+log commit atomically even under ambiguous commits). A probe
   confirmed the same atomic op double-applies under 1021 (faithful — no idempotency IDs), which is why
   the fresh-per-attempt logKey is load-bearing. Serializability gap is already covered by Cycle.
   **Increment 6 DONE:** ConflictRange workload (RFC-125, PR #323) — a two-directional read-conflict-range
   oracle on key-selector getRange, driven through the real `fdb` facade. A concurrent writer (tr2) commits
   between a pinned reader's (tr3) read version and its commit; the oracle is `resultChanged ⟹ foundConflict`
   (under-conflict = `t.Fatalf`, the serializability teeth, revert-proven) with over-conflicts SAFE/counted
   (Go's getKey-then-range selector union is architecturally wider than C++'s combined `addConflictRange`).
   Proved NO under-conflict across the full offset/onEqual/reverse/limit space (deterministic: evaluated=120
   resultChanged=75); guard-key isolation (`maxOffset+1`, proven bound) keeps every resolution in-prefix.
   FDB-C-dev + Torvalds ACK (RFC + impl + delta), codex + @claude + CI green.
   **Increment 7 (FINAL) DONE:** FuzzApiCorrectness (RFC-126, PR #324). The RFC pivoted under review:
   the proposed error-contract fuzzer was NAK'd as padding (Go's error contract is already pinned at
   fixed points + differentials), so the `ExceptionContract` was used as an *audit checklist* — which
   surfaced real, libfdb_c-confirmed wire-contract divergences where Go silently accepted input
   libfdb_c/Java reject: `getRange` row `limit < -1` → `range_limits_invalid (2012)`;
   AddRead/WriteConflictRange + getRangeSplitPoints endpoint `> maxKey` → `key_outside_legal_range
   (2004)` (with the read/write `maxReadKey`-vs-`maxWriteKey` asymmetry); and the metric-op early-return
   precedence (inverted 2005 → cancelled 1025 → poison 2000 → timed_out 1031 → maxKey 2004). Each
   revert-proven + pinned by red→green differentials / deterministic unit tests. Also fixed a
   pre-existing flake in the RFC-121 conflict differentials (conservative-resolver false-positive 1020,
   proven via libfdb_c hitting it too → retry). FDB-C-dev + Torvalds + /code-review + codex + @claude +
   CI all green.
   **C3 COMPLETE:** Cycle (+ read/commit faults), AtomicOps, ConflictRange, FuzzApiCorrectness
   (error-contract axis), Serializability (via Cycle) all covered. Detail: "Native fdbgo client" → C3.
2. **[ ] RFC-056 continuation item 3 — ongoing `/hunt-divergences`.** Standing differential-axis hunt
   vs libfdb_c (atomic-op edges across `Atomic.h`, error-code/option semantics, key/tuple/versionstamp
   encoding). RFC-059→067 closed. Detail: conformance section, "Fresh differential axes".
3. **[ ] C2-followup — confirm RFC-057's lazy iterator closed the go-vs-cgo 1007-rate** near the 5s
   MVCC edge (profiling, not a fix). Detail: conformance section, "C2-followup".
4. **[ ] Query-engine "one query path" unification.** Route `buildSelectShell`/SimpleTable builder +
   INSERT…SELECT through `visitSelectGroupBy`, delete the legacy builder (CLAUDE.md "no parallel
   pipelines" endgame). Graefe-gated. Detail: "vs Java" follow-ups (RFC-079b + RFC-084) + §7.6 history.
5. **[ ] 7.7 follow-up — BLOCKED.** Replace the `isSimpleResidualCompensation` allowlist with Java's
   exploratory-yield re-optimization. Blocked on Go compensation re-optimization handling
   IN-explode/correlated/index-only shapes. Detail: §7.7.
6. **[ ] Parallelize `//conformance` off Ginkgo** [LOW PRIO]. Detail: "Test infra (low priority)".
7. **[~] Java target bump to 4.12.11.0 (from the 4.11 series; RFC-135).** Mechanical bump landed (pins + proto
   sync + regen + version-target docs; `record_query_plan.proto` removed `PVersionValue`/reserved tag
   38, `PExistsPredicate`→`PExistentialValuePredicate`, added `PExistsValue.value` +
   `PRecordQueryExplodePlan.with_ordinality` — all `gen/`-only on the Go side, schema pinned by
   `docscheck.TestPlanProtoSchemaMatches412`). **Behavioural parity = the R-items below, each its own
   RFC, landed one at a time. Verify Java 4.12 actually supports each before treating as parity vs
   allowed Go-extension.**
   - **[ ] R1** — metadata-evolution field renames (`allow{Field,DeprecatedFieldRenames,Undeprecating}` +
     `RenameFieldsVisitor`) vs Java `MetaDataEvolutionValidator`. Gate: Torvalds + codex + @claude.
   - **[x] R2 — DONE** — indexer 4.12 changes. **(a) DONE (RFC-137):** erase-indexing-metadata-after-readable —
     `markReadable` now erases scanned-records(1)/type-stamp(2)/heartbeat(7) per Java
     `eraseAllIndexingDataButTheLockAndRangeSet`; added `SetMarkReadable(bool)` (Java `buildIndex(markReadable)`
     parity) so build-state can be inspected pre-readable. Torvalds+codex ACK. **(b) DONE (RFC-138):**
     `SetEnforcedPostTransactionDelay(ms)` — fixed per-transaction delay replacing records-per-second
     when >0 (Java `setEnforcedPostTransactionDelay` #4229). **(c) DONE (RFC-139):** typed-record build-range
     preset (#4244) — `computeRecordsRange` (over the indexed types; null if any lacks a record-type PK
     prefix or is synthetic) + `maybePresetRecordsRange` marks the out-of-range gaps `[nil,begin)`+`[end,nil)`
     built before multi-target/mutual builds, with byte-exact `begin=low.Pack()` / `end=high.Pack()+0xff`
     bounds (Torvalds NAK caught strinc-vs-`0xff`; codex P1 caught the build loop couldn't unpack the
     `+0xff` end — fixed via `unpackRangeEndBoundary`/raw-boundary mark; codex P2 caught non-integer
     record-type keys — preset now gives up for them); added `RecordType.PrimaryKeyHasRecordTypePrefix()` +
     `IsSynthetic()`. **Follow-up (pre-existing, out of scope):** Go's `RecordTypeKeyExpression` only
     encodes integer record-type keys (`int/int32/int64`) and silently falls back to the message type
     NAME for string/bytes explicit `SetRecordTypeKey` — a wire divergence from Java (which encodes every
     key type); the R2c preset already guards against it but the encoding itself should be fixed.
     **N/A:** `SlidingWindowIndexMaintainer` (+163, #4233-adjacent) — pure metrics
     instrumentation for an HNSW window-decorator index type Go does not have; index-scrub rangeSet fix
     (#4226) — Go has no scrubber. Gate: Torvalds + codex + @claude.
   - **[ ] R3** — parser grammar: `AT ordinality` table source, `functionNameKeyword` in
     `scalarFunctionName` (`RelationalParser.g4`). Gate: **Graefe** + Torvalds.
   - **[ ] R4** — EXISTS in the projection list (`PExistsValue.value`). Gate: **Graefe** + Torvalds.
   - **[ ] R5** — `AT ordinality` array unnest (`PRecordQueryExplodePlan.with_ordinality`). Gate:
     **Graefe** + Torvalds.
   - **[ ] R6** — `CARDINALITY()` function + index support. Gate: **Graefe** + Torvalds.
   - **[ ] R7** — LEFT/RIGHT OUTER JOIN reclassification (verify Go-extension vs Java-now-supported) +
     boolean-simplification/null/outer-join fixes. Gate: **Graefe** + Torvalds.
   - **[~] R8** — conformance rebaseline from a live 4.12.11.0 run. **Partial in the bump PR:** the 7
     RFC-082 annotations 4.12 lifted were reclassified to keep the conformance gate green (4 Java bug-fixes
     → plain equivalence; `left_outer_join_basic` + `where_case_returns_bool_probe` lifted → plain
     equivalence; `bare_bool_where_rejected` → JavaSucceedsGoRejects). **Remaining:** full corpus re-sweep,
     reclassify cross-engine specs/comments encoding lifted 4.11 limits, flip `SQL_CONFORMANCE.md` /
     `CASCADES_DIVERGENCE.md`, clear the `DIVERGENCES.md` rebaseline banner. Gate: Torvalds + codex + @claude.

> **Prior wave closed:** D1 (RFC-118 SimTransport), B2 (RFC-109 escape hatch), the RFC-056 lazy GetKey
> iterator (RFC-057), the GRV-cache divergence (RFC-104), and B1/CI-off-the-box (untracked, owner
> decision). The `[x]` bullets below are that wave.

> **CI: the single self-hosted box is intentional — NOT a tracked problem.** We work locally + sequentially;
> the slowness during the RFC-115→117 merge wave was a one-off (four PRs squeezed through one runner at once).
> Don't re-file a "second / ephemeral runner" or "CI reproducibility off the box" item. (The §7 CI-volume
> tofu/cloud-init is fail-safe dead-ish code — `prevent_destroy` protects the box and nothing auto-applies —
> harmless to leave; revisit only if the box actually starts failing on disk.)

> **C3 (RFC-056 lazy GetKey iterator) — DONE (RFC-057):** `rywSegCursor` replaced the materializing
> `buildSegmentsLocked` (55,437× faster at N=100k, behavior-identical). The residual go-vs-cgo 1007-rate near
> the 5 s MVCC edge is characterized (RFC-056 #235, TODO `C2-followup`) as accepted perf/timing, not a wire
> bug. Don't re-file.

- [x] **D1 — `SimTransport`** (frame-level fault injection) — DONE (RFC-118; FDB-C-dev + Torvalds +
  /code-review ACK; PR gauntlet codex/@claude/CI pending push). One rule-driven proxy-frame loop
  (`simConn` + a per-frame intercept callback) consolidates the bespoke `wrongShardConn`/`dropReplyConn`;
  faithful inline-error injection via the `ErrorOr<reply>`(tag=value) channel real FDB uses for read
  errors (`types.MarshalErrorOrInlineError`). Closes the four C4 deferred Phase-0 test gaps below.
- [x] **B2 — libfdb_c escape hatch** — DONE (RFC-109, PR #295). `BackendDatabase` interface
  (`pkg/fdbgo/fdb/backend.go`) + a CGo-backed impl over `cgofdb` (`pkg/fdbgo/libfdbc/backend.go`),
  selected at BUILD time via the `libfdbc` build tag (`pkg/fdbgo/fdbclient`, netgo/netcgo idiom) —
  NOT runtime config, because libfdb_c's network thread is process-global + unrecoverable so there is
  no live switch between backends anyway (FDB-C-dev + Torvalds vetted; hardened across 11 codex rounds).

> Shipped this session (stacked on `master`, merging bottom-up #303→#304→#305/#306):
> **RFC-116** (#305) GRV/watch/locate operation-span attribution; **RFC-117** (#306)
> `Optional<primitive-scalar>` wire codegen. Both FDB-C-dev + Torvalds + /code-review + codex green.

---

## Client launch-readiness — prioritized stack (2026-06-13)

The pure-Go FDB client (`pkg/fdbgo`) is the launch target. The RFC-010 wire-correctness audit
is essentially complete (14/15 + 1 false positive; RFC-050/051/052/072 + RYW RFC-055/056/057/058/
065/098 all Implemented; read-path reply-timeout shipped in PR #288). The items below are the
remaining launch-readiness work, ordered by priority — **Go-code correctness first, escape hatch
last** (it's a pre-launch safety net, not a blocker for adoption). Driven one at a time via the
`fdb-client-engineer` skill (RFC → FDB-C-dev + Torvalds + codex review → implement → review-clean),
each on its own stacked branch.

1. **[x] GRV cache parity — `USE_GRV_CACHE` opt-in (default off), client correctness.** DONE
   (RFC-104; also fixed the `updateMinAcceptable` MAX→MIN divergence = the filed "RFC-056 item 2").
   `M` ·
   fdb-client-review. Go's `grvCache` is ALWAYS-ON; C++ serves cached read versions only when the
   app sets `USE_GRV_CACHE` (gate `NativeAPI.actor.cpp:7505`, default false `:6148`). Demonstrated
   wrong answer: a Go txn served a cached version OLDER than a libfdb_c-committed seed → seed keys
   invisible. Add the `USE_GRV_CACHE` tx/db option; gate `tryCache` + the background refresher on
   it; match `:7504-7518`. Revisit RFC-096's cache-carried `locked` check if this closes. (Full
   detail in the "GRV cache is ALWAYS-ON" entry below.)
2. **[x] Retry-predicate fidelity — `fdb.IsRetryable` vs `client.isRetryable`.** DONE (RFC-105):
   no bug — pinned each to its C++ analog + deleted the dead 4th predicate `wire.FDBError.Retryable`.
   `S` ·
   fdb-client-review. The two predicates list different code sets. The fix is NOT naive unification:
   in C++ `fdb_error_predicate(RETRYABLE)` ≠ `Transaction::onError`'s set (1039 predicate-retryable
   but not onError-retried; 1006 the reverse). Make each match its OWN C++ predicate, share the
   per-code facts, pin both against the C++ source.
3. **[x] Resource limits / backpressure (multi-tenant launch safety).** DONE (RFC-106a) — clean
   tri-ACK (Graefe + Torvalds + codex), HEAD `a396227e`. `M` · query-engine-gated. Statement timeout
   (per-request ctx deadline → 54F01), scan-limit options wired to `ExecuteProperties` with Java
   semantics + `FailOnScanLimitReached`, `MAX_ROWS`/result-byte caps, SQLSTATE 54F01 mapping. The
   completeness work (9 codex rounds) swept the out-of-band/scan-limit dimension across every leaf
   cursor, buffered operator, DML path (atomic abort, no partial mutation), executor stream wrapper,
   value drain helper, and cursor iterator — none silently truncates. The per-query MEMORY byte budget
   is split to **RFC-106b** (deferred: needs every cardinality-growing buffer charged + a CI lint that
   also covers the out-of-band handling for new leaf cursors / drains). (TODO-production P1.9.)
4. **[x] Make CI gates real.** DONE (RFC-107) — Torvalds ACK + codex clean, HEAD `b1779f49`. `M`.
   New `nightly-stress.yml` (query-generated stress labels + no-op guard, latency reported not gated);
   `client-fuzz` job fuzzing all 23 `//pkg/fdbgo` Fuzz targets Bazel-natively (faithful to the cgo/
   MODULE.bazel patch) + the 8 unfuzzed diff-oracle reply types; `//pkg/fdbgo/client+transport+fdb`
   added to the PR `-race` gate. The review caught + fixed two silent-pass footguns: a `docker info`
   preflight on EVERY FDB-driving gate (else `FDB not available` skips → exit 0 → green with no
   coverage), and `steps.<id>.outcome != 'skipped'` guards so a skipped preflight can't publish an
   empty report. (Also fixed the `codex` CLI hang via a new `codexreview` tool in the codex-review
   skill — root cause: `codex exec` blocks on open stdin.) (TODO-production P1.6.)
5. **[~] CI reproducibility — off the single Hetzner box. UNTRACKED (owner decision, 2026-06-18).**
   The single self-hosted box is intentional: we work locally + sequentially; the RFC-115→117
   merge-wave slowness was a one-off (four PRs through one runner), not cache thrash (warm cache
   confirmed). Don't re-file a 2nd/ephemeral-runner or CI-reproducibility item. See the `# NEXT`
   CI note for the full rationale. Revisit only if the box actually starts failing on disk. (Was
   TODO-prod P1.8.)
6. **[x] libfdb_c escape hatch (Backend interface + CGo-backed impl) — DONE (RFC-109, PR #295).**
   `BackendDatabase` interface + a CGo-backed impl over `cgofdb`, selected at BUILD time via the
   `libfdbc` build tag (not runtime config: libfdb_c's network thread is process-global + unrecoverable
   so a live backend switch is impossible anyway). FDB-C-dev + Torvalds vetted; 11 codex rounds. (Was TODO-production P2.2.)

## Known gaps

### [ ] fdbgo/client: GetAddressesForKey always emits `ip:port`; C++ defaults to ip-only unless `include_port_in_address` is set (surfaced by the RFC-133 option-matrix review, 2026-06-20)

`Transaction.GetAddressesForKey` (`transaction.go:2167-2176`) unconditionally returns `endpointAddress` = `ip:port` (`endpoint.go:36`). libfdb_c defaults the address format to **ip-only**, appending `:port` only when `FDB_TR_OPTION_INCLUDE_PORT_IN_ADDRESS` is set (`NativeAPI.actor.cpp:5747`). So a Go client's `GetAddressesForKey` returns port-suffixed addresses where a C client returns bare IPs by default — an output-format divergence on a rarely-used locality call. `include_port_in_address` is a **tx-only** option (no DB-default form), independent of the RFC-133 DB-default work; pre-existing. Fix is its own small wire-compat change (honor the option, default to ip-only) — flag, not gating.

### [x] recordlayer: legacy format-version-<6 record versions / unsplit records — DONE (2026-06-20)

Go now mirrors Java's `FDBRecordStore.useOldVersionFormat()` end-to-end. Record versions are
read/written in the legacy `RecordVersionKey = 8` subspace for stores below `SAVE_VERSION_WITH_RECORD`
(format 6), and unsplit records are read/written at the bare primary key (no `0` suffix) when
`omit_unsplit_record_suffix` is set — across load, scan, `scanRecordKeys`, `recordExists`, save,
update, delete, and `deleteRecordsWhere` (`store.omitUnsplitRecordSuffix()` / `store.useOldVersionFormat()`
derive the layout from the store header exactly as Java's `checkVersion()`). On open, Go performs
Java's transactional format upgrade (`maybeUpgradeFormatVersion` ⇒ `checkRebuild` /
`addConvertRecordVersions`): bumps `FormatVersion`, sets `omit_unsplit_record_suffix` for a
non-splitting store created before format 5, and moves versions from subspace 8 to the inline
`pk + -1` location when upgrading a splitting store past format 6. Previously Go accepted an old-format
header but only understood the modern inline layout, so it would **silently** miss a legacy store's
versions and unsplit records — a data-correctness bug on the wire-compat hard line. Pinned by
`pkg/recordlayer/legacy_format_test.go` (lays down each legacy layout in FDB and asserts byte-level
read/write/scan/delete/migration parity). Was surfaced by the RFC-131 doc-drift audit.

### [x] fdbgo/client: Get/GetRange over-conflict vs libfdb_c — RFC-121 DONE (PR #319; conflict-range audit 2026-06-19)

Two confirmed serializability-outcome divergences (both SAFE over-conflicts — Go aborted where C/Java
committed, never the reverse), now FIXED. **D1:** GetRange added the full requested `[begin,end)` read-
conflict, not clamped to the data actually returned on a limited/`more` read (C++ clamps to
`keyAfter(lastKey)` — ReadYourWrites.actor.cpp:271-274 / NativeAPI.actor.cpp:4576-4579). **D2:**
Get/GetRange added the read-conflict unconditionally, not skipping keys served by a local independent
write (RFC-058 had wired this RYW filter into GetKey only — ReadYourWrites.actor.cpp:328/342). Fix
routed Get/GetRange conflict generation through the RYW overlay + extent-clamp (`rangeConflictExtent`,
`conflictForKeyLocked`). **Plus a follow-up codex caught:** the streaming `RangeResult.Iterator()` read
later batches under snapshot (no conflict), which became an UNDER-conflict once D1 clamped the first
batch — fixed so every batch is a serializable read adding its own clamped conflict (the C-client
per-batch model). Pinned by red→green differentials + `FuzzDifferential_ConflictOutcome` (63k+ execs)
+ `TestDifferential_GetRangeIteratorConflict_RFC121`, all guarding the under-conflict direction at
`t.Fatalf` severity. Full gauntlet green (FDB-C-dev + Torvalds + /code-review + codex + @claude + CI).
`rfcs/121-get-getrange-conflict-ryw-clamp.md`.

### [ ] fdbgo/client: system-key DB-default applied to a tenant txn — tenant audit (2026-06-19); user-path FIXED

The tenant audit confirmed the WIRE path is byte-perfect (prefix = bigEndian64(id), prepend-at-commit,
TenantInfo, key-size all match C++). One behavioral divergence (#6) was FIXED: `SetReadSystemKeys`/
`SetAccessSystemKeys` on a tenant transaction now return invalid_option (2007), matching C++
`setOption` (NativeAPI.actor.cpp:7159-7171). **Remaining edge:** the DB-LEVEL default path is not
covered. `CreateTransaction` seeds DB defaults (incl. a READ_SYSTEM_KEYS/ACCESS_SYSTEM_KEYS DB
default) while `tenantId == NoTenantID`, and `SetTenantId` runs *after* — so a tenant txn created
under a DB that defaults system-key access silently keeps the flags, where C++ rejects. Fix needs a
check at `SetTenantId` time (reject if system-key flags already set) or at use time; `SetTenantId`
returns void today, so it's a signature/ordering change — deferred. Rare (a DB-wide system-key
default + tenants is unusual). Also documented in-code: the D3 `stripTenantPrefix` clamp divergence
(unreachable — the commit proxy guarantees prefixed boundaries; comment at `locality.go`).

### [ ] fdbgo/client: special-key-space (`\xff\xff/...`) unimplemented — locality audit D1 (2026-06-19)

Go has NO special-key-space module; every `\xff\xff/...` read hits the `maxReadKey()` gate and
returns `key_outside_legal_range` (2004). C++ `ReadYourWritesTransaction::get/getRange` intercept
`specialKeys.contains(key)` and route to `specialKeySpace` BEFORE the maxReadKey gate
(`ReadYourWrites.actor.cpp:1634-1637, 1716-1721`); `DatabaseContext` registers ~30 modules
(`NativeAPI.actor.cpp:1591, 1621-1815`): `\xff\xff/status/json`, `/cluster_file_path`,
`/connection_string`, `/worker_interfaces/`, `/transaction/conflicting_keys`,
`/transaction/{read,write}_conflict_range`, management/configuration, etc. All work via
libfdb_c/Java; all fail with 2004 in Go. It LOUDLY rejects (returns an error, not silent
corruption), but the entire surface is a feature gap. `REPORT_CONFLICTING_KEYS` already noted
elsewhere; this is the broader gap. The `\xff` system-key gating itself is faithful (maxReadKey =
`\xff`/`\xff\xff` matches C++ `getMaxReadKey`). The `SetSpecialKeySpace*`/`SetReportConflictingKeys`
option setters are silent no-ops (`fdb/options.go`). Low-frequency for a record-layer port, but it
is real cross-client surface. D2 (address `:tls`/IPv6 formatting) was FIXED; D3 (INCLUDE_PORT_IN_ADDRESS
no-op — matches api≥630 default, not a real divergence), D4 (`ParseClusterString` whitespace not
collapsed like C++ `trim()`), D5 (IPv6 coordinator round-trip not re-normalized in `ClusterFile.String`;
first-vs-last `@` split on malformed input) are low-impact edges.

### [ ] fdbgo/client: watch-path divergences (D1/D2/D3/D5) — found by the quality-grind watch audit (2026-06-19); D4 fixed

The watch audit fixed **D4** (WatchPoll now retries the SS poll-signals — watch_cancelled/process_behind/
timed_out/future_version — instead of breaking the watch). Four remaining, ranked:

- **D1 [concrete, fixable] — no `too_many_watches` (1032) limit.** C++ `Transaction::watch`
  (`NativeAPI.actor.cpp:5694`) calls `increaseWatchCounter()` (`:2175`) which throws `too_many_watches`
  when `outstandingWatches >= DEFAULT_MAX_OUTSTANDING_WATCHES = 1e4` (`ClientKnobs.cpp:120`, settable to
  `ABSOLUTE_MAX_WATCHES=1e6` via `MAX_WATCHES`); `decreaseWatchCounter()` runs when the watch resolves/
  errors (`:5679`). Go has NO outstanding-watch counter — watches are unbounded; 1032 is never thrown;
  `MAX_WATCHES` is a no-op. Fix: a `db.outstandingWatches atomic.Int64` + `maxOutstandingWatches`,
  increment at `WatchSetup` (return 1032 if at the limit), decrement on EVERY watch exit (fire/error/
  cancel) — the lifecycle is the tricky part. Test with a low limit via a `MAX_WATCHES` option.
- **D2 [architectural — RFC] — watch registered at READ version, not commit-gated.** C++ defers the
  SS-side watch to AFTER commit via `setupWatches()` in `commitAndWatch` (`NativeAPI.actor.cpp:6418`,
  `:6909`), at `committedVersion>0 ? committedVersion : readVersion`. Go's `WatchPoll` registers at
  `tx.readVersion` immediately, with ZERO commit coordination (`commitpath.go` has no watch handling).
  A Go watch is live before its transaction commits. Deep architectural gap.
- **D3 [architectural — RFC] — no RYW pending-write watch semantics.** C++ `RYWImpl::watch`
  (`ReadYourWrites.actor.cpp:1284`) keeps a `watchMap` + `triggerWatches`/`onChangeTrigger` so a watch
  on a key with a differing same-tx pending write fires IMMEDIATELY. Go folds the pending write into the
  baseline (via `tx.ryw.get`) but has no watchMap/immediate-fire — the watch's baseline becomes the
  post-write value and it long-polls for the *next* change (wrong fire point).
- **D5 [small] — cancel returns `context.Canceled`, not `transaction_cancelled` (1025); failed commit
  doesn't cancel watches; stale comment.** `reset()→cancelWatches()` cancels the watch *context*, so
  in-flight watches return `ctx.Err()` not an FDBError 1025 (C++ `resetPromise.sendError(1025)`). And
  (tied to D2) a failed commit never tears down the watch (C++ `cancelWatches(e)`, `:6926`). Also the
  comment at `transaction.go:1595` ("Watch() calls are NOT cancelled by Reset()") contradicts the actual
  `reset()→cancelWatches()` path — cleanup.

### [ ] fdbgo/client: missing `makeSelfConflicting` (`\xFF/SC/<uuid>` synthetic conflict range at commit) — needs its own `fdb-client-engineer` RFC (commit-path wire/behavior; found by the quality-grind OnError audit, 2026-06-19)

C++ `Transaction::commitMutations` adds a synthetic self-conflict range to a commit whose write
and read conflict ranges don't already intersect: `if (!causalWriteRisky &&
!intersects(write_conflict_ranges, read_conflict_ranges)) makeSelfConflicting()`
(`NativeAPI.actor.cpp:6858-6860`), where `makeSelfConflicting()` (`:5952`) pushes a single
`\xFF/SC/<deterministicRandom()->randomUniqueID()>` range into BOTH read and write conflict sets.
(There is a SECOND, idempotency-id-based `\xFF/SC/<idempotencyId>` add at `:6850-6856` for the
automatic-idempotency feature — distinct, gate on `tr.idempotencyId`.) Go has neither: a write-only
commit (read conflicts empty → no intersection) ships WITHOUT the synthetic range, and
`commitDummyTransaction`'s `intersectConflictRanges` (`commitpath.go:250-265`) falls back to
`writes[0].Begin` — a real user key — where C++'s dummy uses the synthetic key
(`NativeAPI.actor.cpp:6744-6750`).

**Two effects:** (a) Go's commit-request conflict-range vector diverges from libfdb_c for the same
write-only transaction (request-frame semantic difference — not persisted bytes, but affects the
resolver); (b) Go's commit_unknown_result dummy conflicts on a real user key, so a concurrent writer
of that key can false-conflict the dummy, where C++'s synthetic UUID key never collides with real
traffic. PARTIALLY mitigated today: Go's `OnError(1021/1039)` copies writeConflicts→readConflicts on
the RETRY (`transaction.go:1850`), so the retry is self-conflicting via a different mechanism — but
the original commit's wire shape and the dummy's key choice still diverge.

**Why a dedicated RFC, not a grind fix:** the commit_unknown_result ↔ makeSelfConflicting ↔
commitDummyTransaction interaction is subtle (each attempt mints a FRESH random UID, so it is NOT
simple retry-idempotency), it touches the commit path + wire shape, and it can't be cleanly
differential-tested at the data plane (conflict ranges go to the resolver, not storage — a
fault-injection test that triggers commit_unknown_result is needed). Port `makeSelfConflicting` +
the `intersects(write, read)` gate faithfully under FDB-C-dev DESIGN review; pin with a Go-side
commit-request unit test (write-only commit includes a `\xFF/SC/` range in both sets) + a
SimTransport commit_unknown_result behavioral test.

### [ ] fdbgo/client: transaction-level options are PRESERVED across `onError` retry; C++ resets them to DB defaults — needs its own RFC (found by the quality-grind options audit, 2026-06-19)

C++ `Transaction::resetImpl` (`NativeAPI.actor.cpp:6166`, called by `tr.reset()` on the RYW onError
path, `ReadYourWrites.actor.cpp:1417`) does `trState = trState->cloneAndReset(...)`, and
`cloneAndReset` (`:3515`) builds a FRESH `TransactionState` whose `options` are DB-default-constructed
— it copies the old options ONLY `if (!cx->apiVersionAtLeast(16))` (ancient APIs). So for every modern
app, a retry RESETS `priority`→DEFAULT, `causalReadRisky`→0 (grvFlags), `lockAware`→`cx->lockAware`,
tx-level `sizeLimit`→DB default, `tags`→empty, `snapshotRYWDisableCount`→DB default, then re-applies
ONLY the persistent options (timeout/retry_limit/max_retry_delay/auth_token, `persistent="true"` in
`fdb.options`). Go's `reset()` (`transaction.go:2481`, comment ~`:2528`) instead PRESERVES
priority/causalReadRisky/lockAware/readLockAware/sizeLimit/tags/snapshotRYWDisableCount — the comment
asserts this "matches C++", which `cloneAndReset` disproves.

Wire-visible on the retry: a transaction-level `SetPriorityBatch`/`SetCausalReadRisky`/`SetLockAware`
keeps sending its flags on the retry GRV/commit where libfdb_c reverts to the DB default.
**Why an RFC, not a grind fix:** the faithful fix re-seeds the tx-level options from the DB defaults on
reset (factor out CreateTransaction's seeding, call it from reset, preserve only the 4 persistent
options) — a change to the hot retry path with per-option DB-default subtleties (lockAware→cx default,
not false; causalReadRisky consistency), and the existing code deliberately chose the wrong behavior, so
it needs FDB-C-dev design review. Pin with a unit test (set a tx-level option → reset → assert reverted
to DB default; persistent options survive).

**Other options-audit findings (silent no-ops where C++ acts — `fdb/options.go`):** `REPORT_CONFLICTING_KEYS`
(sets `commit.report_conflicting_keys`; Go field exists at `committransactionref_generated.go` slot 4
but always false), transaction `TAG`/`AUTO_THROTTLE_TAG` (never populate the GRV/commit/read `Tags`
slot — tag throttling non-functional; also no `tag_too_long`/`too_many_tags` validation),
`READ_SERVER_SIDE_CACHE_*` + `READ_PRIORITY_*` (set `ReadOptions.cacheResult`/`.type`; Go no-ops),
`INITIALIZE_NEW_DATABASE` (forces readVersion=0), `USE_PROVISIONAL_PROXIES` (GRV flag bit 2). Per the
conformance principle, the silently-ignored ones should at least LOUDLY reject (UnsupportedOptionError)
rather than no-op — but each is a small feature, scoped separately.

**GRV / read-version audit (same grind) — NO consistency divergence found** (version-vector is OFF by
default, `ServerKnobs.cpp:39`, so Go's empty `ssLatestCommitVersions`/`maxVersion` is exactly correct;
read-version reuse, `read_snapshot`, 1007 aging all match). Latency/observability findings only:
- **Write-only commits omit `CAUSAL_READ_RISKY` on the commit-path GRV.** C++ `tryCommit` does
  `startTransaction(GetReadVersionRequest::FLAG_CAUSAL_READ_RISKY)` (`NativeAPI.actor.cpp:6578`) — a
  write-only/no-prior-read commit doesn't need full causal consistency for its `read_snapshot`. Go's
  commit path (`transaction.go:1507`) calls plain `ensureReadVersion` → `grvFlags()`, setting the flag
  only if the USER did. Effect: an extra TLog epoch-confirmation round-trip per write-only commit
  (latency/throughput, NOT consistency — the read_snapshot is equally valid). **Infra implication, why
  not a grind fix:** Go's `grvBatcherIndex` keys batchers only on the PRIORITY mask, NOT the risky flag
  (unlike C++'s `readVersionBatcher`, keyed by full flags) — so adding the flag would mix risky/non-risky
  GRVs in one batch. The faithful fix re-keys the GRV batcher on the risky flag + threads it through the
  commit-path `ensureReadVersion`; deliberate, FDB-C-dev-reviewed.
- `SetReadVersion` accepts `v<=0` / double-set silently where libfdb_c `setVersion` throws →
  `CATCH_AND_DIE` aborts the process (`NativeAPI.actor.cpp:5519`, `fdb_c.cpp:932`). Go's graceful
  defer-to-1007 is arguably BETTER (no panic in library code per CLAUDE.md) — leave as a documented,
  intentional divergence, don't copy the abort.
- Dropped GRV-reply observability (no consistency impact): `proxyTagThrottledDuration` (the
  `getTagThrottledDuration()` accumulator), the `metadataVersion` reply cache (Go does a real read of
  `\xff/metadataVersion` — correct, one extra round-trip), `midShardSize` (no clear-range cost estimator).

**Minor OnError/knob-audit findings (same grind, low priority — note, don't necessarily fix):**
hedge `secondDelay` uses a fixed `2.0×primary-latency` where C++ uses a runtime-adaptive
`secondMultiplier (≥1.0) × second-best latency + BASE_SECOND_REQUEST_TIME(0.5ms)`
(`loadbalance.go:70` vs `LoadBalance.actor.h:560`; p99 hedge timing only); GRV batcher lacks C++'s
`MAX_BATCH_SIZE=1000` force-flush (`NativeAPI.actor.cpp:7351`; >1000 concurrent GRVs/window wait the
full window); GRV `batchTime` floors at 100µs where C++ has no floor.

### [x] fdbgo/wire: register WatchValueRequest/Reply in the schema extractor (pre-existing gap, surfaced by RFC-115 §6) — DONE (branch `wire/watchvalue-extractor-registration`, stacked on #303)

`cmd/fdb-schema-extract/main.cpp` has no `extractType<WatchValueRequest>()` /
`extractType<WatchValueReply>()` (37 other types are registered). The committed
`pkg/fdbgo/wire/types/watchvalue*_generated.go` were produced out-of-band (commit `52c70585`),
so `just generate-wire-types` (which `rm`s `*_generated.go` then restores only extractor-emitted
types) DROPS them — a regen footgun. RFC-115 §6 restored them after its regen; the proper fix is
to register both in `main.cpp` (`extractType<WatchValueRequest>(outDir, "WatchValueRequest")`,
same for the reply) so a regen reproduces them. WatchValueReply also carries an inline
`Optional<Error>`, so re-emitting it picks up the §6 union fix too. Not caught by per-PR CI
(`just generate` ≠ `just generate-wire-types`). Verify the re-emitted bytes are wire-identical
to the committed files before landing.

**DONE.** Registered both in the extractor (`extract.h` REGISTER_FIELD_NAMES + `REGISTER_GO_TYPE(ReplyPromise<WatchValueReply>)`;
`main.cpp` `extractType<>`); a regen now PRODUCES them. The regen surfaced — and this branch also fixes — **two
deeper extractor wire bugs** the registration depended on:
1. **`Optional<UID>` mis-emitted as `[]byte`.** `scalar_traits<UID>` (flow/IRandom.h) ⇒ UID is a fixed 16-byte
   scalar, so `Optional<UID>` (the `debugID` on requests) must be `[16]byte` (a bare 16-byte OOL scalar behind
   the union RelativeOffset, C++ `SaveAlternative` flat_buffers.h:848), not a length-prefixed vector. Added an
   `Optional<scalar>` codegen path (restricted to UID — the lone fixed-array struct-scalar). Fixed `DebugID` on
   `WatchValueRequest`/`GetReadVersionRequest`/`CommitTransactionRequest`/`StorageServerInterface`/`TenantMapEntry`/
   `ReadOptions`. Verified byte-faithful vs the C++ oracle (un-skipped `debugID`: 4M+ execs, 0 mismatches).
   (Correction to the note above: `WatchValueReply` has NO `Optional<Error>` — it's just `{version int64, cached bool}`.)
2. **`ReadOptions` field-name mis-registration → a live client bug.** The old `REGISTER_FIELD_NAMES(ReadOptions,
   "type","cacheResult","lockAware")` mis-mapped the slots: C++ serialize order is
   `(type, cacheResult, debugID, consistencyCheckStartVersion, lockAware)`, so the generated "LockAware" (slot 2-3)
   was actually `debugID` (Optional<UID>) and the real `lockAware` is a bool at slot 6. The client
   (`readpath.go`) set the debugID field thinking it was lockAware → **lock-aware reads never actually requested
   lock-aware**. Fixed the registration (5 names, serialize order) + the client (`ReadOptions{LockAware: true}`);
   the round-trip unit tests now assert the real bool.

**Follow-up — DONE (RFC-117, commit `b5bdbc00`):** **`Optional<primitive-scalar>` codegen.**
`Optional<int64>`/`<Version>`/`<bool>` were mis-emitted as `[]byte`; the extractor now emits a typed bare
scalar (value encode/decode at the union RelativeOffset, shared with the Variant scalar arm). Regen flipped
only `ReadOptions.consistencyCheckStartVersion` `[]byte`→`int64`; un-skipped in `cmd/fdb-diff-oracle`
(`TestDiffReadOptions`, C++ byte-truth). The UID `[16]byte` array path is unchanged.

### [x] fdbgo/client: stamp the GRV/watch/locate requests with a trace SpanContext — DONE (RFC-116)

RFC-115 §4 stamped the per-op child SpanContext on reads + the tx span on commit, but the GRV,
watch, and getKeyServerLocations requests still carried a ZERO/raw SpanContext. **RFC-116** closes
all three, faithfully to the C++ (NOT the naive "thread a representative tx span" — that would put a
tx traceID on the GRV wire, which C++ never does):
- **GRV** is batched; the GetReadVersionRequest carries the `readVersionBatcher` **fresh-root** span
  (`NativeAPI.actor.cpp:7334/7345/7385/7238`), zero-traceID unsampled unless a sampled tx joins the
  batch (then a brand-new random root via `addLink`). Per-tx spans are local links, never on the wire.
- **locate** stamps the `getKeyLocation` child (`:3017/3037`, derived once in `refresh`, reused
  across proxy retries — `basicLoadBalance` reuse).
- **watch** stamps the `watchValue` child (`:3933/3965`, derived once in `WatchPoll`).
Closed codex's P2 on PR #303. Commits `16847239` (GRV), `a6f08a2a` (locate), `7fdfd24d` (watch).

### [x] fdbgo/client: read-path RPC reply timeout is retryable, not a terminal leak (C++ divergence) — FIXED (PR #288)

Shipped in PR #288 (merge `48106b7d`). `waitReply` (rpc.go) now returns an internal
`errReplyTimeout` sentinel (distinct from caller-ctx cancellation); the three read paths
(`getValue`/`getKey`/`getRange`) re-send on it (bounded by `maxReadTimeoutRetries=10`) and on
exhaustion surface a RETRYABLE `transaction_too_old` (1007) — matching libfdb_c's `loadBalance`,
which has NO per-read client timeout (re-sends a slow-but-alive server until reply or read-version
aging). `getKey` uses three separate budgets (timeout / shard / progress). The commit path keeps
its own `commit_unknown_result` semantics. Found by the 10M SPFresh soak (died at 4.9M records on
the old terminal leak). Pinned by `readpath_timeout_test.go` (deterministic via a reply-dropping
dialer). Gates: FDB C++ dev + Torvalds + codex + @claude all ACK on the final HEAD.

### [x] TOP — SPFresh churn flake on MASTER: live record not findable after concurrent churn (094.3 race)

**ROOT-CAUSED + FIXED on the 094.4 branch (PR #283): the csplit pause-window orphan.**
The fingerprint (`membership=[393217] fine 393217@cell 2 state=0` — membership and
posting entry both present, centroid ACTIVE, search still misses) is the capped-read
truncation shape: the query path fetches postings with a 4×Lmax+1 cap
(`spfresh_query.go`), while the invariant checks read uncapped. On master, a posting
that ballooned past the cap while a pending coarse split PAUSED fine-split issuance
(`spfreshCSplitPaused` skip in the insert probe) never got its split task re-filed —
it survived quiescence oversized, and any record whose entry sorted past the cap was
live-but-unfindable. Fixed by the pause-window repair (csplit move re-files split
tasks for moved oversized ACTIVE rows, commit a55fec70), pinned deterministically in
`spfresh_cascade_test.go` ("csplit move re-files split tasks…"). Verified: 45/45
focused runs green on the branch vs ~1-in-8 red on master. The churn test now also
asserts post-quiescence that every ACTIVE posting is within the 4×Lmax envelope (the
search-visibility bound) and its failure diag includes posting size vs cap + sidecar
presence, so either silent-miss shape self-diagnoses on any recurrence.

### [x] CORRECTNESS FIXED — re-enumerated indexed multi-way joins (was: NULL / 0 rows)

**Symptom (fixed).** A 3-way *indexed chain* join planned through the RFC-042 L3
index-NLJ re-enumeration path returned wrong results that depended on the
FROM-order: one order returned 200 rows all-NULL, the opposite order returned 0
rows (correct is 200 rows, all `t1.id = 1`). 2-way joins and non-indexed *star*
3-way joins (`TestFDB_ThreeTableFrom`) were always correct.

**Root cause (pointer-level instrumented).** `PartitionSelectRule` misrouted the
*spanning* join predicate (e.g. `t3.t2_id = t2.id`, one alias in each partition
half) into the **lower** partition. Java's classification keys on
`uppersDependingOnLowersAliases`, computed from `getCorrelationOrder()` —
**quantifier** correlations. Go's flat-seed join quantifiers are independent
scans with **no quantifier-level correlations** (the joins are plain predicates),
so `uppersDependingOnLowers` is *always empty* and the spanning predicate always
fell to the "can do in lower" branch. That yields a degenerate **Case-1
cross-product** partition whose lower result is a `{_0}` literal placeholder
(discarding the real columns) and whose pushed-down filter evaluates against
unbound upper aliases → wrong rows. The physical FlatMap then merges via
`JoinMergeResultValue`, which cannot resolve columns nested under `_0` → NULL.

**Fix (shipped).** `PartitionSelectRule` now rejects the degenerate partition: a
predicate routed to the lower that references an UPPER alias cannot be evaluated
there, so the whole partition is skipped (`rule_partition_select.go`, "Reject
degenerate partitions" guard). The valid associativities — where the spanning
predicate stays at the join level — then win identically for every FROM-order.
Both orders now return 200 correct rows; deterministic; full suite green.
`multiway_join_index_probe_test.go` was a plan-shape-only fake checkbox (never
executed the query) — now retrofitted with **row-correctness** assertions for
both FROM-orders, which is the load-bearing check.

**Remaining (cost-optimality, NOT correctness) — RFC-042.** Under the big→small
FROM-order the re-enumerated `(t2⋈t3)` sub-product still prefers a cross-product
NLJ over the index probe (the index-probe alternative either loses on cost or
flows a sub-product result the parent predicate can't SARG), so that order
full-scans the 200-row T3 instead of index-probing it. Correct, just slower. Full
byte-identical FROM-order invariance for N≥3 (the `TestFDB_MultiwayJoinOrder_Probe`
goal) depends on closing this cost gap + FROM-order-deterministic winner selection.
Likely levers: the index-probe cardinality cost (criterion #2 — make the FlatMap
inner range over the index-scan wrapper so `maxDataAccessCardinality` reflects the
probe), and making re-enumerated sub-products flow a flat `JoinMergeResultValue`
so the index-probe variant is both cheaper AND resolvable.

- [ ] **Re-verify `joinOptimizationProbesScenario` (RFC-082 cross-engine exclusion) against RFC-042 (@claude flag).** The A3 builder is excluded from `crossEngineScenarios` with the note "Go's join enumeration is still non-deterministic on some arithmetic-predicate shapes — a 3-way / arithmetic-join can return a different ROW COUNT across runs." That row-count *nondeterminism* (a correctness flake) is NOT the item tracked above — line 11-40 is the now-FIXED FROM-order-dependent (but per-order deterministic) bug, and line 42 is cost-optimality (correct results, just slower). So either the exclusion note is stale (the row-count flake was the fixed PartitionSelectRule bug → the scenario may be re-enableable cross-engine now) or there is a genuinely-still-nondeterministic join-enum shape that needs its own root-cause. Verify with a focused multi-run of the probe shapes; if still nondeterministic, the Go-only yamsql coverage for `join_optimization_probes` is itself flaky (same code path) and must be pinned, not just excluded cross-engine. Out of scope for RFC-082 (conformance determinism); tracked here for the RFC-042 follow-up.

### vs Java (correctness/feature parity)

- [x] **Correlated filter without index.** Fixed in 56874f23 — ImplementFilterRule sets innerAlias on RecordQueryPredicatesFilterPlan. All correlated paths (scalar subquery, EXISTS, JOIN) work without indexes. 14+ integration tests verify.
- [x] **RIGHT/FULL OUTER JOIN.** Done in RFC-036. (The old "only LEFT OUTER" note was stale — RIGHT already worked via operand-swap normalization in `cascades_translator.go`, pinned by `TestFDB_RightJoin`.) FULL OUTER added as a Go-only query extension: Java's SQL layer has **no** outer joins at all (`visitOuterJoin` is a no-op, zero tests), so LEFT/RIGHT/FULL are all read-path-only extensions with **zero wire-format impact** — Java apps still read/write the same records. FULL OUTER is implemented exclusively by the materialized NLJ cursor (`streaming_cursors.go`): LEFT-OUTER outer loop + a `matchedInner` bitmap + a drain phase emitting unmatched inner rows NULL-padded on the left. Routed away from the correlated FlatMap path (cannot observe global inner-match state); FULL+EXISTS rejected with a clear error. 9 FDB integration tests (all four row classes, NULL-key 3VL, many-to-many, large-inner hash+drain, WHERE-above-join, determinism, RIGHT NULL-key regression). Graefe+Torvalds ACK.
- [x] **Correlated scalar subquery shapes widened.** Non-aggregate (ORDER BY + LIMIT), multi-table inner FROM (JOINs), multi-column validation, deep-walk replaceScalarSubqueryRef. GROUP BY/HAVING rejected with clear errors (PredicatePushDownRule AliasMap conflict). CorrelatedExistsError propagation fixed.
- [ ] **No *general-purpose* window functions — and Java has none either.** Investigation (RFC-045): Java's relational layer has **no** general streaming window operator. The general `windowClause` is commented out in Java's grammar ("don't want to deal with them now"); `LAG`/`LEAD` are grammar tokens with **no** value class; `RankValue implements Value.IndexOnlyValue` (computable only from a rank/leaderboard index, never over a result set). The **only** working window function in Java is `ROW_NUMBER() OVER (... ORDER BY <distance>) <= K` via `QUALIFY`, used exclusively for **vector/HNSW K-NN search**. So "match Java's window functions" ≡ "finish the vector/HNSW relational parity" — tracked as **Phase 9** below. General windowing over plain tables would be a *Go-only extension Java lacks entirely* (allowed if wire-compat holds + deep tests), not parity — deferred, not in Phase 9.
- [x] **GROUP BY/HAVING in correlated scalar subqueries.** Done in RFC-047 — a Go-only read-side extension (Java rejects correlated scalar subqueries at the grammar level entirely; zero wire impact). The stale "PredicatePushDownRule AliasMap.Compose conflict" blocker no longer applies: GroupByExpression is already a push-down barrier (no case in `pushPredicateToExpression`) and the panicking `AliasMap.Compose` has no production callers. `buildCorrelatedScalar` now builds GROUP BY (+ HAVING) into the inner plan and caps with `LIMIT 1`; the scalar contract is FirstOrDefault (first group + LEFT-OUTER NULL-on-empty), NOT a runtime cardinality assertion (Graefe). Empty input → 0 groups → NULL falls out naturally (vs no-GROUP-BY COUNT → 0). Group keys + aggregate operands resolve via the semantic scope (`ResolveIdentifier`), scalar column named with the bare operand to avoid an embedded-`.` qualifier mis-parse. 42803 enforced via `validateGroupByProjection`; multi-column + EXISTS-in-HAVING + unresolvable-expr-arg/key rejected. 23 FDB integration probes (incl. EXPLAIN-pins-StreamingAgg, empty→NULL contrast, expression group key, join+GROUP BY, determinism 10×).
  - [x] **Follow-up: `ORDER BY` over grouped output in a correlated scalar subquery.** Done in RFC-085 — a Go-only read-side extension. The interim rejection is gone; `ORDER BY` + `GROUP BY` now inserts a `LogicalSort` over the post-aggregate row (between the aggregate and the FirstOrDefault `LIMIT 1`) so the multi-group choice is deterministic. Sort keys resolve to the **exact** datum key the aggregate cursor emits (`groupedScalarSortKeys`, single-source: group keys → bare-upper, aggregates → the materialised alias) — translateSort/FieldValue do exact-case lookup, so a mismatched key would silently sort every row equal. ORDER BY a column that is neither grouped nor a *selected* aggregate is rejected loudly (no silent-nil sort). Wired in BOTH aggregate paths (hasRealAgg + group-key-only). **Sub-fix (same exact-case-datum-key bug class):** a qualified projection (`SELECT o.amount`) and a qualified ORDER BY key in the **non-aggregate** single-table path used to keep the `o.` qualifier and resolve to NULL / miss the sort — now stripped to the bare key (mirroring the join-vs-single-table convention at :910). Pinned by `ordered_grouped_scalar_subquery_fdb_test.go` (ASC/DESC group choice, determinism 10×, loud reject, qualified projection + qualified key) and `quality_probes_test.go` (order_by_with_group_by_deterministic, ASC+DESC SUM per group).
  - [x] **Follow-up (single-source): expression/constant-argument aggregate that meets a *differing* aggregate via HAVING in a correlated scalar subquery.** DONE — the addendum unified producer and consumer on **one** canonicaliser (`canonicalAggName`, called by both `buildCorrelatedScalar` and `rewriteAggregateValue`), so the two name schemes can no longer drift; the prior fail-safe rejection is gone for single-source. The last silent-wrong corner (nested-arithmetic args like `SUM((amount+10)*2)` returning NULL → dropped groups) was a *separate* root cause — an inverted `!isArith` guard in `translateAggregate` that preferred a lossy text reparse over the resolved operand — fixed in RFC-048 (4dc3276c): the resolved `AggregateOperands[i]` is now always the source of truth. Works now (single-source): `SELECT COUNT(1) … HAVING COUNT(*)` both directions; `SELECT SUM(a*2) … HAVING SUM(a*3)`; decimal-literal args (`SUM(a*1.5)`); nested-arith args (`SUM((a+10)*2)`). `COUNT(DISTINCT 1)` correctly still rejected (DISTINCT unsupported here). Pinned by `quality_probes_test.go` (count_constant_with_having_works, expression_aggregate_in_having_works, decimal_literal_aggregate_arg_in_having, nested_arithmetic_aggregate_arg_in_having). **Residual (join only):** over a JOIN an expression-argument aggregate in HAVING is still rejected (the operand binds to the wrong quantifier through the parser round-trip) — pinned by `join_expression_aggregate_in_having_rejected`.
- [x] **🚩 IN over an indexed column drops the outer projection (wrong result schema).** Fixed in **RFC-070**. Root cause was two defects: (1) `MergeProjectionAndFetchRule`'s fallback dropped the projection when the fetch's child was an InJoin (not a coverable index scan), leaking a bare `InJoin` ([ID,A]) into the root projection group where it won on cost; (2) `physicalProjectionWrapper`/`physicalFetchFromPartialRecordWrapper` `WithChildren` didn't relink a compound-join inner during extraction (left `Project([id], InJoin(<nil>))` / `Fetch(<nil>)`), because of an `isLeafReplaceable` gate — same gate RFC-069 removed from the in-memory sort wrapper. Fix: fallback retains the projection; the two transparent caps relink unconditionally. `SELECT id FROM t WHERE a IN (1,7)` → `Project([ID], InJoin(IndexScan(IDX_A,[=])))`; `SELECT id+100 ...` (was 0 rows) → `{101,107}`. Pinned by `TestFDB_INProj_OuterProjectionOverInJoin` (indexed+unindexed, multi-column, expression-projection, 8× determinism). Graefe+Torvalds ACK.
  - [ ] **Follow-up (RFC-070): `pushValue`-into-covering-result-value modeling gap.** Java's `MergeProjectionAndFetchRule` yields a bare `fetchPlan.getChild()` because `RecordQueryFetchFromPartialRecordPlan.pushValue` rewrites the projected value into the covering plan's own result value. Go's `WithCovering` only sets a flag (the scan still flows the full partial record), so Go compensates with a thin outer `Project`. Pushing the value into the covering result value would let both rule branches collapse to a bare child yield, matching Java. Cosmetic/architectural — current behaviour is correct.
  - [ ] **Follow-up (RFC-070): other transparent unary wrappers over joins.** `Map`, `Distinct`, `Limit`, `TypeFilter`, `FirstOrDefault`, `DefaultOnEmpty` still gate `WithChildren` on `isLeafReplaceable` and could exhibit the same nil-inner-over-join bug if a rule ever builds them with a placeholder inner over a join. Not currently reachable via SQL (projections route through `LogicalProjectionExpression`, not `Map`); the **blanket** gate removal is unsafe — it regressed `TestFDB_AggregateIndexUsage` by dropping the eq-filter on aggregation/DML wrappers (which embed filter semantics in their own plan). Each wrapper needs individual analysis if/when reachable.
- [x] **DML does not execute through Cascades (parallel pipeline).** Fixed as **P0.4** — all DML now executes through Cascades (`planDML`); the naive `execStatement` DML path is deleted. See P0.4.
- [x] **🚩 `INSERT … SELECT … GROUP BY` wrote the wrong columns (spurious 23505).** Fixed in **RFC-084**. A plain GROUP BY SELECT builds a bare `LogicalAggregate` with NO Project (standalone derives its schema from the physical plan), so as an insert source its datum was keyed by the aggregate's own canonical names (`G`, `SUM(V)`) — `buildInsertRecord` maps by TARGET field name, found none, left every field unset → each grouped row collapsed to the same all-default record → second group collided → spurious 23505. Java accepts this exact shape (`insert_select_java.yaml:60`). Fix: `wrapBareAggregateInsertSource` wraps the bare aggregate in the canonical post-aggregate Project (reusing `buildPostAggregateProjection` — visible-only via `ac.visible`, canonical-named to match the runtime datum key, in SELECT order), filling `ProjectedValues` with upper-canonical `FieldValue` refs; `alignInsertSelectColumns` then sets target aliases positionally. A sole `SELECT COUNT(*)` (tracked as `sq.countStar` with empty `aggCols`) is synthesised into the wrap so `INSERT INTO t SELECT COUNT(*) [GROUP BY g]` is aligned too. Pinned by `groupby_insert_select_fdb_test.go` (core/was-23505, multi-aggregate Java shape, COUNT(*) scalar+GROUP BY, lowercase arg, AS-aliases, reordered SELECT, ungrouped HAVING-over-non-visible `keys==0`, qualified-stays-loud, HAVING-strip-Project path, determinism 10×). Graefe + Torvalds ACK (RFC + impl).
  - [ ] **Follow-up (RFC-084): qualified aggregate operand on the insert-source path computes NULL.** `INSERT … SELECT g, SUM(s.v) … GROUP BY g` leaves the qualified aggregate's operand unresolved (`AggregateOperands=[nil]`) so it sums NULL; the wrap therefore SKIPS qualified-operand sources (a `.` in the canonical agg/group-key name) to avoid silently inserting NULL — they stay at the original loud 23505. Fix the operand resolution on this path (then drop the skip + flip `qualified_source_stays_loud` to assert correct rows).
  - [ ] **Follow-up (RFC-084 / RFC-079): unify INSERT…SELECT onto `visitSelectGroupBy`.** The one-query-path end-state MOVES this coercion into the Insert expression and **deletes** `wrapBareAggregateInsertSource` (no third parallel coercion path) — per Graefe's condition. Tracked with the RFC-079 SimpleTable-builder unification.
- [x] **🚩 Aggregate result-type derivation diverges from Java: `AVG(x)→DOUBLE`. DONE — RFC-083.** `AggregateValue.Type()` now types `AVG → NullableDouble` (function-determined, matching Java `AVG_*→DOUBLE`); SUM/MIN/MAX stay operand-derived, COUNT→LONG. The "ZERO new code / existing IsPromotable check" framing was **inaccurate** (no plan-time promotion check existed — `IsPromotable` had zero callers; the only enforcement was a runtime band-aid), so the fix is three coordinated parts: (A) the AVG `Type()` arm + collapse the duplicate AVG→DOUBLE SQL-name encodings onto it (`valueTypeName`/`aggregateResultType` route through `Type()`); (B) a **plan-time promotion guard** at the INSERT…SELECT chokepoint (`checkInsertSelectPromotable`, the first production `IsPromotable` caller) keyed on aggregate **provenance** — `LogicalProject.AggregateSlots` (captured pre-rewrite via `containsAggregate`) for computed exprs like `AVG(v)+1`, and name-resolution against the producing `LogicalAggregate` for bare `AVG(v)` (whose projection slot carries a nil value) — so `AVG→BIGINT` is rejected 22000 **even over an empty source** (emergent from the lattice, not a materialized float); `rewriteAggregateValue` now preserves `Typ: av.Type()` (was discarding it as UnknownType); (C) converge the runtime converters — remove `ConvertToProtoValue`'s whole-float→int64 coercion (VALUES double→BIGINT now rejects 22000), and give `goToProtoValue` the promotable INT/LONG→FLOAT/DOUBLE widenings + an **emergent 22000 fallthrough** (also fixes the adjacent `SUM(BIGINT)→DOUBLE` gap that used to error). Pinned: `values_test` AVG-type pins, flipped both `ConvertToProtoValue` whole-float unit tests, new `goToProtoValue` widening/reject tests, `avg_double_insert_fdb_test.go` (scalar/empty-source/`AVG+1`-empty/`→DOUBLE`/`SUM→BIGINT`/`SUM→DOUBLE`/plain-arith/VALUES double-reject/index-presence EXPLAIN). insert_select.yaml corpus corrected. Ripple guard holds (AVG never lowered to `Sum/Count` ArithmeticValue division; no aggregate index → streams). Full `just test` green. Graefe+Torvalds ACK'd RFC (v4) + impl.
  - [ ] **Follow-up (RFC-083): replace the guard + `AggregateSlots` marker with Java's `PromoteValue` projection nodes** — the single mechanism that both rejects-at-plan and widens-at-runtime, dissolving the dual lattice-encoding (guard + converters) and the load-bearing "aggregate-slot ⇒ guard" coupling (Graefe's end-state). Subsumes reliably typing `FieldValue`/`ArithmeticValue` projections, which then closes the **residual deferred cases**: bare-column `SELECT double_col → BIGINT` over an empty source, and `UPDATE … SET int_col = <double-expr>` — both currently rely on the runtime converter (correct for non-empty rows, miss the 0-row case).
  - [ ] **Follow-up (RFC-083): bare GROUP BY-aggregate INSERT…SELECT source.** `INSERT … SELECT g, AVG(v) … GROUP BY g` has a `LogicalAggregate` as the insert Source (no `LogicalProject`), so the guard can't read column order and defers it (runtime rejects the non-empty case). Also observed a possible PK-mapping/grouping anomaly on that execution path (a 23505 where the rows shouldn't collide) — investigate separately.
  - [ ] **Adjacent (separate index-type bug): `GetIndexTypeName` hardcodes `MIN_EVER_LONG`/`MAX_EVER_LONG`** — MIN/MAX over a non-long operand needs `MIN_EVER_TUPLE` (Java `permuted_min/max`).
- [x] **🚩 TODO 7.6-union-remap — aggregate UNION branch with a mismatched output alias drops rows (pre-existing executor gap).** Fixed for STREAMING aggregates in **RFC-078**: (1) `executeUnorderedUnion` (executor_new_plans.go) now remaps later branches' columns to the first branch's names by position — it previously concatenated branch cursors with NO normalization at all (unlike the ordered `RecordQueryUnionPlan`/`executeUnionStreaming`); (2) `planColumnNamesWithMD` (executor.go) reports a `RecordQueryStreamingAggregationPlan`'s output names (group keys + alias-or-canonical) instead of descending through `GetInner()` to the input scan. `SELECT u.x FROM (SELECT COUNT(*) AS x FROM a UNION ALL SELECT COUNT(*) AS y FROM b) u` now returns both counts (was `[2, NULL]`). Pinned by `TestFDB_UnionAggregateColumnRemap`. Graefe + Torvalds ACK.
  - [x] **Follow-up (RFC-078) c — FIXED in RFC-080: re-enable the union-as-join-leg / derived-table aggregate case for UNGROUPED aggregates.** The gate's `LogicalAggregate` case is hit only by a *bare* aggregate branch (no Project). Graefe's review caught that a bare aggregate can be GROUPED (an unaliased, all-visible `SELECT g, COUNT(*) FROM t GROUP BY g` skips `buildSelectShell`'s stripping Project). Only the UNGROUPED sub-shape is safe to normalize: an ungrouped aggregate produces **no** aggregate-index candidate (`tryAggregateIndexCandidate` returns nil when `groupingCount == 0`, `cascades_generator.go`), so it always plans as StreamingAgg, which flows every aggregate under its alias (RFC-078). So `unionBranchNormalizable`'s `LogicalAggregate` arm relaxed from `false` to `len(Aggregates) >= 1 && len(GroupKeys) == 0`. `TestFDB_UnionJoinLeg` case (3) flipped clean-error→correct-rows. Pinned by `TestFDB_UnionScalarAggregateAlias` (single + multi ungrouped unions read by name + no-AggregateIndex invariant), `TestFDB_UnionGroupedAggregateStillGated` (grouped union, which DOES plan as AggregateIndex, stays gated), `TestUnionBranchNormalizable_AggregateArity`. plandiff byte-identical. Graefe + Torvalds ACK.
    - [x] **Follow-up (a) — GROUPED bare aggregate union by name — FIXED in RFC-081.** A bare GROUPED aggregate union branch (`SELECT g, COUNT(*) FROM a GROUP BY g UNION ALL …` read by name) plans as `AggregateIndex` (single agg) or `MultiIntersection`/`StreamingAgg` (multi agg). The fix was *reporting*, not cursor changes: the AggregateIndex and MultiIntersection cursors already write rows keyed by their output names (group cols + canonical aggregate name; a bare aggregate is always unaliased, so no alias to carry). Added `RecordQueryAggregateIndexPlan.OutputColumnNames()` + `planColumnNamesWithMD` arms for AggregateIndex (group cols + `CanonicalAggColumnName`) and MultiIntersection (result-value field names, verbatim), then dropped the `len(GroupKeys) == 0` clause → gate is now `len(Aggregates) >= 1`. `TestFDB_UnionGroupedAggregate` (single + multi grouped union join legs, mismatched group-key names → correct rows; EXPLAIN-pins AggregateIndex), `TestPlanColumnNames_{AggregateIndexReportsOutputSchema,MultiIntersectionReportsResultValueNames}`, `TestAggregateIndexPlan_OutputColumnNames`, gate unit test grouped→true. plandiff byte-identical. Graefe + Torvalds ACK.
      - [ ] **Sub-follow-up (codex): DIVERGENT-NAMED aggregate union branches.** A bare aggregate whose output name differs between the logical leg schema (`aggregateOutputColumns`, raw text) and the physical row key (`aggResultName`/AggregateIndex canonical) NULLs when union-remapped by name. Divergent forms: qualified operand (`SUM(t.c)`→`SUM(C)`), constant (`COUNT(1)`/`COUNT(NULL)`→`COUNT(*)`), expression (`SUM(a*b)`), DISTINCT. RFC-081 GATES all of them **in the GROUPED case** via `aggregateNamesStableForUnion` (whitelist `COUNT(*)`/`FUNC(bare-col)`; clean error, `TestFDB_UnionQualifiedAggregateGated` + `TestFDB_UnionGroupedCountConstantGated`). UNGROUPED branches are left as RFC-080 (always StreamingAgg, not re-gated, to avoid regressing working ungrouped legs); any ungrouped divergent form (e.g. bare ungrouped `SUM(t.c)`/`COUNT(NULL)`) is a pre-existing RFC-080 latent NULL, fixed by the same naming-unification below. To OPEN them: unify aggregate output naming so the logical schema and the physical row key agree for every form (strip qualifier consistently + reconcile count-star normalization between StreamingAgg and AggregateIndex), then relax the whitelist. NOTE: a separate pre-existing bug — `SELECT u.*` star-expansion over an aggregate union join leg mis-derives the aggregate column name (NULL) even for ALIASED aggregates (Project-topped) — is orthogonal to the gate and also needs fixing. Trivial cleanup (@claude): `deriveColumnsFromAggregateIndex` (cascades_generator.go) builds the canonical `FUNC(col)`/`FUNC(*)` name inline (a third copy alongside `CanonicalAggColumnName` + the cursor) — for schema-metadata column-type derivation, not row-key naming, so it doesn't interact with the union remap, but it should call `aggIdx.CanonicalAggColumnName()` to complete the single-source consolidation.
  - [x] **(b) ordered-union projection-alias — FIXED in RFC-079.** A UNION branch projecting a post-aggregate EXPRESSION with an alias (`SELECT COUNT(*)+1 AS x FROM a UNION ALL SELECT COUNT(*)+1 AS y FROM b`, read by name) returned `[NULL,NULL]` — the legacy `buildSelectShell` builder (the UNION-branch path) built the post-agg projection with `nil` aliases, dropping the `AS x`. Fixed by extracting the projection-building loop into one shared `buildPostAggregateProjection` helper called by both `visitSelectGroupBy` (modern) and `buildSelectShell` (legacy) — one source of alias truth. Pinned by `TestFDB_UnionAggregateExprAlias` + `TestBuildLogicalPlan_PostAggExprAlias_CarriesAlias`. Modern path plandiff byte-identical. Graefe + Torvalds ACK.
  - [ ] **Follow-up (RFC-087, Graefe): reject aggregate-in-scalar-context at PLAN time.** `WHERE COUNT(*) > 0` reaches `AggregateValue.Evaluate` at row eval; RFC-087 made it a clean runtime `AggregateEvalError` → 42803 (was an uncaught goroutine crash on master — Graefe confirmed). Java rejects this at semantic-analysis / plan time ("unable to eval an aggregation function with eval()"). Detect an aggregate in a per-row scalar predicate (WHERE / JOIN-ON / projection-not-under-GROUP BY) during planning and reject there, matching Java exactly. Runtime 42803 is the safety net; plan-time is the parity fix.
  - [ ] **Follow-up (RFC-087, Graefe): thread `ComparisonKeyFunc` error channel.** The 5 executor merge/sort comparison-key sites (`intersectionCompKeyFunc`, `multiIntersectionCompKeyFunc`, `mergeSortCursor.isBetter`/`extractKey`, executor.go:1391) `panic(err)` on a stray key-eval error — pre-existing behaviour (no recover before/after RFC-087), and keys are pre-projected field refs so the typed-error family is unreachable today. To make it airtight, give `ComparisonKeyFunc` an `error` return and thread it (ripples into wire-adjacent `merge_cursor.go`). Low priority — not reachable from current SQL.
  - [ ] **Follow-up (RFC-088, Graefe condition): converge `validateGroupByProjection`'s existence check onto the semantic resolver.** Java does NO standalone existence check for GROUP BY keys — `SemanticAnalyzer.resolveIdentifier` over the full multi-source scope already guarantees existence, and `validateGroupByAggregates` enforces only the algebraic 42803 rule (key must be grouped-or-aggregated). Go currently runs a SECOND, hand-rolled existence oracle (`tableFields` = union of all source descriptor field names, bare-name match) that is deliberately qualifier-blind, so it would false-ACCEPT a wrong-qualifier key (`e.dname` where dname is on the joined dept) — SAFE today ONLY because the precise resolver runs first at every call site (top-level `resolveColumnName` ~L1002; correlated-scalar GROUP-BY-key resolution in `buildCorrelatedScalar`), an ordering invariant now pinned by a code comment at `validateGroupByProjection` and by `TestFDB_GroupByWrongQualifierRejected`. End-state: route existence through `resolver.ResolveIdentifier` and leave `validateGroupByProjection` enforcing only 42803, removing the duplicate oracle and the ordering dependency.
  - [ ] **Cleanup (RFC-079 follow-up b): unify the SimpleTable logical builder onto `visitSelectGroupBy`.** The "one query path" endgame (CLAUDE.md "no parallel pipelines"). `buildSelectShell`/`buildLogicalPlanForSelect` is a second SELECT builder reached by plain-table SELECTs, derived tables, AND UNION branches; it has repeatedly drifted from the modern `visitSelectGroupBy` (the RFC-079 alias bug was one such drift). Route ALL of its callers through `PlanVisitor.visitSelectGroupBy` and delete the legacy builder. Larger than a single-bug fix (multiple callers, full regression surface) — Graefe's condition: must unify the WHOLE SimpleTable builder, not graft a special case onto the union entry.

### Beyond Java (Go-only improvements)

- [x] **Full Graefe Memo with cross-group merging.** Done in RFC-037 — union-find group merging (the Cascades-paper "merge two groups discovered to be one", §2 + §3.5), a Go-only extension beyond Java (which, like the pre-RFC Go memo, only interns at insertion time). `Reference` gains a monotonic `id` + `forwardedTo` + path-compressed `Canonical()`; every state-bearing method resolves the receiver to canonical, so a merged-away (loser) Reference transparently forwards — no in-flight task, Quantifier, or binding is rewritten. `GetRangesOver()` resolves at the single chokepoint (444 sites). `Memo.Integrate` hooks the REWRITING yield path: when a yielded expression equals a member of a different group, the two merge (survivor = lower id, deterministic), folding members + exploration state, repointing the topology index, invalidating correlation caches up the DAG, and recursively re-merging parents (paper's bottom-up recursion). Scoped to REWRITING (PLANNING winners/partial-matches embed raw References — guarded by `mergeable`); ancestor/descendant (idempotence) merges skipped to avoid DAG cycles. Wire compat untouched (read-path-only sharing). Merge fires through the real planner (`TestMemoMerge_FiresThroughRealPlanner`); 9 merge unit tests + determinism 50×/10×; 46/46 targets green; stress-1M unchanged. Graefe+Torvalds ACK (NAK'd v1 on in-flight-task stranding + cache staleness + index repoint + upward re-merge — all fixed in v2). **Reach caveat (honest):** the merge is correct and fires, but its practical reach is narrow today — the memo's interning/equivalence is alias-sensitive, and rule-rewritten equivalents mint fresh quantifier aliases, so equivalent sub-expressions intern to *different* child References and rarely surface as merge candidates (measured: exactly one merge on a K-branch equivalent UNION regardless of K; ~2% planner-time delta; no execution-time effect — same plan). Broad merging (and any real speedup / multi-way-join-order benefit) is **gated on alias-namespace unification (item 7.1 below)**; this PR lands the correct merge *infrastructure*, not a present-day perf win. Remaining (Future Work): **alias-normalized equivalence (7.1) — the lever**; reduction-rule-triggered merges (§3.6); PLANNING-phase merging; cost-model exploitation of shared sub-products for full N-way join-order optimality.
  - **PR-A landed the lever (RFC-038 epic / RFC-039 + RFC-040).** The reach caveat is now closed: the memo's structural-equivalence compare sites use alias-aware `expressions.MemoEqual` (faithful port of Java `Reference.isMemoizedExpression`) on top of the RFC-040 foundation (alias-aware `EqualsWithoutChildren` + alias-invariant `HashCodeWithoutChildren`). Rule-rewritten equivalents that differ only in fresh quantifier aliases now intern/merge into the SAME Reference — proven by `memo_activation_test` (K=6 alias-variant filters → 1 shared Reference, was K distinct). Zero plan-shape regression (plandiff conformance green), 10/10 deterministic, stress-1M before/after within noise. Graefe+Torvalds ACK. Still ahead in the epic: **PR-C** join-order enumeration (associativity/commutativity, capped) and **PR-D** cost selection + the e2e "multi-way join ordering proven" test (N-table join, EXPLAIN-pinned optimal order ≠ FROM-order, shared sub-products merged).
  - **PR-C scope corrected (RFC-074).** PR-C was framed as "efficient ≥5-way enumeration via sub-product interning (collapse the dual merge values)." **Measurement falsified the premise:** collapsing `JoinMergeResultValue`/`JoinMergeAllValue` to one canonical type does NOT reduce `distinctRefs`/`tasksRun` (N=5 stays 127k tasks / 171 refs) — the duality is a ~2× constant, not the exponential. The exponential is that logically-equivalent join sub-products land in SEPARATE memo References (even identical scans fork ×3) and never coalesce: `mergeable` (memo_merge.go:84) refuses once a group `HasWinnersOrMatches`, and `OptimizeGroup` interleaves `SetWinner` with `Integrate` yields, so a group holds a winner before its equivalent twin is born. RFC-074 now ships ONLY the **merge-value collapse** — a correct Go-only-divergence removal + prerequisite for single-type interning, **behavior-preserving (NOT a budget fix)**. Graefe re-ruled.
  - **PR-C2 — the actual ≥5-way budget fix (NEW, separate RFC).** Java does NOT solve the blowup by merging-under-winners (RFC-037 broad merge is a Go-only extension Java lacks); it **bounds the bipartition lattice at the source** via `shouldDeferCrossProducts` + `shouldJoinRightDeep` (Java `PartitionSelectRule.java:92,122`) and builds legs in a canonical interning-friendly form. Port/enable that pruning into `rule_partition_select.go` (the hooks exist — `ShouldJoinRightDeep`/`ShouldDeferCrossProducts` — verify defaults + why a pure connected chain isn't bounded). 1:1 Java-aligned. Do NOT decouple exploration from optimization (Java interleaves identically) and do NOT extend broad-merge-under-winners. Graefe-ruled.
- [x] **Correlated scalar subqueries.** Go-only extension — Java rejects at grammar level. Implemented via FlatMap with JoinTypeLeftOuter.

---

## Production readiness (Graefe review, 2026-05-28)

The Cascades architecture is solid — task stack, two-phase REWRITING+PLANNING, 16-criteria cost model, match-candidate infra all well-ported. The production risks are all at the **boundaries**: planner↔executor, executor↔runtime, system↔operator. Priority tiers below.

### P0 — fix before deploying anywhere (correctness/availability)

- [x] **🚩 P0.4 DML executes through Cascades.** Fixed in RFC-035 — all DML (INSERT VALUES/SELECT, UPDATE, DELETE) routes through `planDML` → Cascades executor; `planOne` no longer branches on exec mode and the naive `execStatement` DML path (`execInsert`/`execUpdate`/`execDelete`/`execInsertSelect`, `pkPushdownCursor`) is deleted. INSERT VALUES reuses the Explode operator (RecordConstructor→Array→Explode→Insert) with plan-time arity/NOT-NULL/coercion; UPDATE SET RHS resolves to Values; DELETE/UPDATE WHERE gets EXISTS/scalar-subquery support via `upgradeDMLWhereWithCatalog`; INSERT…SELECT maps projection→target positionally and materializes (Halloween-safe). `IsUpdate()` derived from physical plan type; `RowsAffected` counted (Java's countUpdates); DML respects explicit transactions via `runInTx`. Fixed a latent non-correlated-EXISTS semi-join bug that also affected SELECT. QueryContext rejects update plans before executing (use Exec) — documented divergence in DIVERGENCES.md. Corner-case tests in `dml_cascades_fdb_test.go`. Graefe+Torvalds ACK (direction + implementation).
- [x] **P0.1 NLJ memory bomb.** Fixed in PR #203 — `CollectAllBounded` with configurable materialization limit (default 100K rows) on all 6 `CollectAll` sites. `MaterializationLimitExceededError` typed error. All cursor leaks on error path fixed. 11 regression tests. RFC-028.
- [x] **P0.2 Plan cache serves wrong plans.** Fixed in RFC-029 — cache keys on normalized SQL string directly (was uint64 FNV-64a hash with no text comparison on hit → collision = wrong plan). Scalar subquery staleness was a non-issue: `scalarSubqueryBinding` stores plans not results, re-evaluated per page fetch. `QueryHash` retained for tests only.
- [x] **P0.3 No context cancellation in executor.** Fixed in RFC-030 — `ctx.Err()` checks at the top of every cursor OnNext loop and drain function (44 sites across 19 files). `autoContinuingCursor` was the worst offender (created new FDB transactions on cancelled contexts). All cursor combinators, executor cursors, utility drains, DML drains, legacy query path drains, and iterator adapters now respect Go context cancellation. 24 unit tests verify prompt cancellation.

### P1 — fix before relying on the optimizer for real workloads (plan quality)

- [x] **P1.1 Wire statistics from FDB.** Fixed in RFC-031 — `fetchTableStatistics` was already wired (nightshift-100) but had two bugs: used read-write transactions for read-only stats (wasted commit), and fabricated equal-distribution counts for intermingled schemas. Fixed: `FDBDatabase.RunRead()` for no-commit snapshot reads, dropped intermingled fallback (returns nil → safe DefaultStatistics). E2E FDB integration test proves full pipeline: count maintenance → stats read → cost model → plan selection → execution.
- [x] **P1.2 Complete QOV-based FieldValue migration.** Fixed in RFC-032 — all 10 `stripAlias*` calls deleted (8 NLJ rule, 2 PushFilterBelowJoinRule). Predicates now retain `FieldValue(QOV(correlationId), "column")`; filters use `PredicatesFilterPlanWithAlias` so the executor binds each row under its correlation alias and resolves via `evaluateCorrelated` — zero string manipulation. `executePredicatesFilter` binds the inner alias whenever present (was gated on params only). Root cause exposed: `PartitionBinarySelectRule` (Java inner-join rule) fired on LEFT OUTER joins, pushing nullable-side predicates pre-join; guarded with `JoinInner`. `mergeRows` string qualification untouched (operates on executor row maps, not planner Values — separate concern). All 46 targets pass; determinism verified.

### P2 — fix before scaling operations

- [x] **P2.1 Plan cache LRU is O(n) per hit.** Fixed in RFC-033 — replaced the slice-based LRU order tracking (linear scan + slice splice in `promote()` on every hit, under the lock) with a `container/list` doubly-linked list + `map[string]*list.Element`. Promote-on-hit/update and eviction are now O(1), matching Java's Caffeine-backed cache. `RWMutex` downgraded to `Mutex` (the read path always mutated the list, so the read lock was a lie). `BenchmarkPlanCache_HitLargeCache` confirms position-independent O(1) hits at maxSize=1024; all existing LRU-semantics tests pass unchanged + new interleaved-eviction test.
- [x] **P2.3 Intersection cursors don't resume mid-stream (codebase-wide).** Fixed in **RFC-071**. `DecodeIntersectionContinuation` (exact inverse of `buildIntersectionContinuation`) splits the per-child `IntersectionContinuation` proto into START/MID/END resume states; `executeIntersection` and `executeMultiIntersection` create each child cursor accordingly (fresh / resume-from-bytes / `Empty`) via the shared `buildIntersectionChildCursors`, then use `IntersectionResume`/`IntersectionMultiResume`. `started` is now tracked **per child** in `mergeChildState` (matching Java's `KeyedMergeCursorState`, not derived from cursor-level state) and seeded from the decode, so a resumed mid-stream child can't be re-encoded as START. The loud guard is dropped. Also fixed a latent continuation-timing bug the paged test caught: both cursors captured the continuation *after* the post-match advance, losing every other match on resume (`[2,4,6]`→`[2,6]`) — now captured before. Pinned by white-box paged-resume tests (no dup/loss, asymmetric exhaustion, no-common, 3-child, both cursors) + decode round-trip/error/nil tests in `intersection_resume_test.go`. Graefe+Torvalds+@claude+codex ACK (v1 NAK'd — Graefe caught a limit-before-first-row child silently terminating the intersection + held-match loss on out-of-band stops, driving the full Java `MergeCursorState` consume-model port; @claude caught `intersectionMultiCursor` returning bare END on a limit instead of checkpointing; codex caught a decode child-count validation gap for 1/2-child tokens). Surfaced by @claude + codex on PR #249; landed as PR #252.
  - [ ] **Follow-up (RFC-071, Go-only optimization beyond Java): skip re-scanning discarded non-matching rows on intersection resume.** Because the cached per-child continuation sits at the last *consumed* (matched) position (faithful to Java `MergeCursorState`), an out-of-band stop resumes a child from there and re-scans the non-matching rows discarded since its last match (bounded by the inter-match gap; the whole prefix-to-first-match for a never-matched child). Correct (no dup/no loss) and Java-faithful, but for very sparse intersections under a tight per-page limit the re-scan is wasted work and — pathologically — could fail to make progress within one page. Tracking the position just *before* the currently-held candidate (so resume re-reads only it) would eliminate the re-scan; this diverges from Java's model, so it's a Go-only read-path optimization, not parity. Flagged by codex on PR #252.
- [x] **P2.2 Operational debuggability.** Fixed in RFC-034 — `PlanGenerationLogger` hook (nil = silent) emits one `PlanGenerationInfo` per Cascades planning call: SQL (truncated, rune-safe), plan hash (`plans.PlanHash`), plan explain, planning duration, cache event (hit/miss/skip/inconclusive), cache size, slow-query flag, error. Go analog of Java's `RelationalLoggingUtil` + `PlanGenerator` finally block; wired into `planSelectCascades` (real query) and `planDML` via a shared `planLogScope` with a named-return defer. EXPLAIN re-entry suppressed via `logMetrics bool`. No scalar "estimated cost" — the Cascades cost model is a comparator, not a number (matches Java; logs plan hash + explain instead). Threshold default sourced from the canonical `api.OptLogSlowQueryThresholdMicros` (single source of truth); `OptLogQuery` left intentionally unwired (no SLF4J level concept in Go — handler owns level + sampling), documented at `options.go:55`. Sampling is the handler's responsibility. 11 unit tests + 2 FDB integration tests (DML Skip event, SELECT miss-then-hit through the public driver). Graefe ACK, Torvalds ACK.

---

## Stress test 1M baseline (2026-05-27)

**Run command:** `bazelisk test //pkg/relational/sqldriver/stress:stress_test --test_output=streamed --test_arg="--test.run=TestFDB_Stress_1M$" --test_arg="--test.v"`

| Query | Rows | Time | Threshold |
|-------|------|------|-----------|
| pk_lookup_first | 1 | 1.5ms | <5ms |
| pk_lookup_middle | 1 | 1.5ms | <5ms |
| pk_lookup_last | 1 | 1.7ms | <5ms |
| index_customer_eq (8 rows) | 8 | 9.1ms | <10ms |
| index_amount_range (100K rows) | 100017 | 196ms | |
| index_status_count | 1 | 362ms | |
| full_scan_count | 1000000 | 3.1s | ~3s/1M |
| full_scan_filter | 1 | 534ms | |
| group_by_status | 4 | 5.25s | |
| group_by_status_count_only | 4 | 1.9ms | |
| sum_by_status | 4 | 2.0ms | |
| group_by_customer_having | 47271 | 107ms | |
| join_10_outer | 10 | 4.1ms | |
| order_by_pk_full (1M rows) | 1000000 | 3.33s | ~3s/1M |
| order_by_pk_index_filter | 8 | 3.4ms | |
| scan_all_narrow (1M rows) | 1000000 | 3.33s | ~3s/1M |
| scan_all_wide (1M rows) | 1000000 | 3.66s | ~3s/1M |
| in_list | 46 | 10ms | <10ms |
| needle_in_haystack_pk | 1 | 2.0ms | <5ms |
| needle_in_haystack_filter | 1 | 2.4ms | <5ms |
| full_scan_sparse_filter | 97 | 3.0s | ~3s/1M |
| update_by_index | 8 | 4.0ms | |
| delete_single_row | 1 | 2.3ms | |

All 23 subtests PASS. Total: 170.7s (incl. bulk insert ~2:28).

---

## Phase 8: Planner architecture cleanup (Graefe review findings)

### 8.1 Evaluate `pushDataAccessTasks` as CascadesRule — RESOLVED (keep procedural)

Graefe flagged this as procedural code that should be a rule. After investigation: **the procedural approach is architecturally correct.** `pushDataAccessTasks` operates on Reference-level PartialMatch state, not expression types — CascadesRules require expression-type pattern matching. Java uses explicit `TransformMatchPartition` task types for the same reason: this is task-level logic, not rule-level. Go's direct method call in `ExploreExprTask.Run()` is simpler and equivalent. No change needed.

### 8.2 Verify `promoteByDataAccessCost` heuristic absorbed — VERIFIED

`promoteByDataAccessCost` was deleted in eb94291a (dead code cleanup). Its heuristic (prefer lower-cardinality data access) IS absorbed into `PlanningCostModelLess` at `planning_cost_model.go:191–208` — Criterion #2: `maxDataAccessCardinality`, lower wins. This fires via `stampOrderingWinners(ref, costModel)` after every data access insertion. The cost model uses the same `findExpressionsByType` + `maxDataAccessCardinality` comparison. No heuristic was dropped.

### 8.3 Document `maxRoundsPerRef = 10` cap — DONE

Added comment at `unified_tasks.go:59` explaining: prevents divergence from rule cycles (A→B→A) that produce distinct-but-equivalent members. Java relies on memo dedup for fixpoint; Go's per-Reference dedup is weaker, so pathological rule interactions can produce new members indefinitely. 10 rounds >> typical 2–3 needed, safely under MaxTasks budget.

---

## Phase 7: Cascades alignment — close remaining Java divergences

### 7.1 Unify alias namespaces — DONE

Quantifier aliases now match table aliases at creation. Three band-aids removed: `rightAliasSet`, `planContainsJoin`, `collectPlanAliases` (−114 lines). Root-cause fix in `mergeRows`: bare inner keys overwrote qualified keys from nested joins (missing `!exists` guard). 46/46 tests, 15/15 determinism.

### 7.2 Port matching infrastructure for index intersections — DONE

`IndexIntersectionRule` deleted (Go-only REWRITING-phase rule). Replaced with match-based PLANNING-phase intersection via `WithPrimaryKeyIntersector` in `intersector_primary_key.go`. Wired into `pushDataAccessTasks` with guards: candidate cap (4), match cap (8), restricted-scan filter, idempotency via `hasIntersectionFinal`. Two regressions found and fixed: IS NULL correctness (zero-coverage matches created incorrect intersections, fixed by filtering to restricted scans only) and MaxTasks (intersection logic ran N times per Reference, fixed by idempotency guard). 46/46 tests, 10/10 determinism.

### 7.3 Convert remaining predicateReferencesAlias sites — DONE

All 8 `predicateReferencesAlias` calls in the NLJ rule converted to `GetCorrelatedToOfPredicate` correlation-set checks. Function deleted. Root-cause fix: `qualifyBareFieldValue` in EXISTS builder now produces QOV-based FieldValues instead of flat strings. `walkPredicateFieldValues`/`fieldValueAliasAndCol` survive in push-filter/push-projection rules (handle both QOV and flat FieldValues for unit test compatibility).

### 7.4 FlatMap wrapper correlation propagation — NOT NEEDED (Graefe confirmed)

Graefe confirmed: `GetCorrelatedToWithoutChildren()` returning empty is correct for BOTH joins AND correlated subqueries. Correlations flow through quantifier children in both cases. `JoinMergeResultValue.Children()` does NOT need QOV nodes.

For correlated scalar subqueries (Go-only extension, Java rejects at grammar level), the correct Cascades architecture is:
1. `ForEachNullOnEmpty` quantifier (already exists: `ForEachNullOnEmptyQuantifier`)
2. `RecordQueryFirstOrDefaultPlan` with NULL default (already exists)
3. Correlated `BuildScalar` fallback (needs: full inner plan with outer scope, correlation predicate extraction)
4. NLJ rule: detect NullOnEmpty → wrap inner with FirstOrDefault + FlatMap

NLJ wrapper correlation propagation (walks predicates) is already correct and active.

### 7.5 + 7.6 (HOLISTIC — RFC-077): Source-anchored join result + structural interning

**Bundled per maintainer decision (2026-06-04):** 7.5 (structural interning key) and 7.6
(source-anchored field pull-up) are two facets of ONE change — retire the opaque, name-keyed
join-merge apparatus (`JoinMergeResultValue`/`JoinMergeAllValue`, `composeFieldOverJoinMerge`,
the string `mergeQuantifierAlias`) for **anchored access**: the translator + re-enumeration emit
`RecordConstructorValue` of `FieldValue(QOV(legAlias), col)`, resolved by the existing
`composeFieldOverConstructor`. RFC-073 GATED 7.6 on 7.5 (a circular "anchor only the binary join =
split-brain"); doing them as one migration breaks that deadlock, and **7.5's structural interning
falls out for free** — the anchored RC is canonical (one type, alias-set-keyed), so it interns
structurally via RFC-039/040 `MemoEqual`, retiring the synthetic string `mergeQuantifierAlias`
(measured load-bearing today *because* the merge is opaque; anchoring removes that).

**Design unlock (RFC-077):** Go's `RecordConstructorValue.Evaluate` produces a NAME-keyed map
(`values.go:2148`), so Go uses **name-based anchored resolution** — NOT Java's full ordinal-substrate
machinery (`FieldValue.ofOrdinalNumber`). Smaller, cleaner, Go-adapted (the sanctioned
"diverge when strictly better + clean" path). `composeFieldOverConstructor` simplifies field
accesses at plan time so the RC rarely survives to runtime; consumers reading the old
bare+`ALIAS.COL` keys (`cascades_generator.go:1890` column derivation, `executor.go:1434 mergeRows`,
`streaming_cursors.go`) move to the anchored RC's field keys. This addresses Torvalds' RFC-073
NAK (the Evaluate-shape change) via the name-keyed-map + compile-time-simplification design.

7.5/7.6 history (the prior split, RFC-073's deferred analysis, the Graefe direction + Torvalds NAK)
is preserved in `rfcs/073-source-anchored-join-result.md`; RFC-077 supersedes it as the holistic
plan.

**Status update (2026-06-05):** F3 split the bundle (Graefe ruling: 7.5 now, 7.6 deferred on column
threading). 7.5 IMPLEMENTED — and the documented root-cause was CORRECTED by an implementation spike:
the interning was NOT defeated by an alias-sensitive candidate-narrowing hash (the hash is already
alias-invariant, RFC-074; `memoizeNonLeaf` already uses alias-aware `MemoEqual`). The real
alias-sensitive sites are `Reference.Insert`/`InsertFinal`, which dedup alias-IDENTITY only — a
Go-vs-Java divergence (Java's `containsInMemo` is alias-aware). Fix: a GATED alias-aware `MemoEqual`
dedup tier in `Insert`/`InsertFinal`, opted into via `SelectExpression.InternsAliasAware()` (merge
re-enumeration selects only — gating avoids over-deduping CTE column-rename selects, which silently
read NULL when collapsed because Go's column derivation resolves some references by quantifier-alias
IDENTITY, unlike Java's ordinal/group model; this is the RESOLUTION-model axis, NOT alias-namespace
naming, which 7.1 already unified). `mergeQuantifierAlias` +
`mergeAliasPrefix` deleted; the merge quantifier now gets a plain `uniqueId`. Verified by a
deterministic chain task-count gate (±2%, pinned 3-chain 8999 / 4-chain 30593; naive un-gated uniqueId
DOUBLES the 4-chain to 60044) + full suite green + 5× determinism. The opaque-type retirement
(JoinMergeAllValue/Seed/composeFieldOverJoinMerge) and anchored RC remain 7.6, deferred on column
threading (F3). See RFC-077 "Precise root-cause — CORRECTED".

**7.6 DONE (2026-06-05, RFC-077 v4):** column threading landed in the 7.6 core (#259); this follow-up
(a) anchors EVERY reachable join-leg shape — correlated scalar subqueries (incl. dotted scalarCol),
derived tables / aggregate subqueries / CTE references as join legs, recursive-CTE legs (outer +
recursive-branch self-reference), Sort/Distinct/Union/Aggregate legs — and (b) DELETES the opaque
`JoinMergeAllValue`/`JoinMergeSeedValue`/`Seed`/`composeFieldOverJoinMerge`, migrating all consumers
to the source-anchored `RecordConstructorValue`. Decisive root-cause: the core's `tableColumns` was
case-SENSITIVE while the SQL path upper-cases table names, so the core's anchoring was DORMANT
(`resolveRecordType` now case-insensitive). Proven no-fallback by a panic-probe over the entire SQL
production surface; chain budget gate unchanged (anchored interns identically); plandiff
byte-identical. See RFC-077 v4.

- [x] **7.5 + 7.6 (RFC-077) — DONE.** 7.5 merged (#258), 7.6 core merged (#259), 7.6 retirement
  (anchor-all + delete opaque types) on `feat/7.6b-retire-opaque-merge`.

### 7.7 Retire `ImplementIndexScanRule` — unify on the data-access/`Compensation` path (RFC-045 follow-up)

- [x] **DONE (RFC-076 v5, 2026-06-05).** `ImplementIndexScanRule` + both registrations + its 3 test
  files deleted; shared helpers extracted to `scan_match_helpers.go`. Sequence: 3b template-aware
  costing → 3a constraint-pass activation + stub-chain costing → deletion + **data-access compensation
  materialization** (the v3/v4 premise missed that the data-access path never materialized its residual
  `Compensation.apply` LOGICAL filter into a physical plan during PLANNING, so the index scan was
  dropped to a full scan for the indexed-eq + non-indexed-residual shape; `pushDataAccessTasks` now
  realizes the unambiguously-safe simple residual as a physical filter, guarded against IN / correlated
  / index-only / vector-or-aggregate-inner / join-leg shapes — see `isSimpleResidualCompensation` +
  `refHasCorrelatedMatch`). `validateNoIndexOnlyResidual` KEPT (still load-bearing). Full suite green,
  plandiff byte-identical, determinism 5×. The data-access/`Compensation` path is now the sole scan
  producer, as in Java. Original analysis retained below.
- [ ] **Follow-up (Graefe v5 ACK condition): replace the `isSimpleResidualCompensation` allowlist with
  Java's exploratory-yield re-optimization.** Java yields data-access compensations via
  `FinalYields.yieldUnknownExpression` — a non-`RecordQueryPlan` lands in the *exploratory* set and is
  re-optimized by the normal PLANNING loop, so EVERY compensation shape is realized uniformly. Go's
  `pushDataAccessTasks` only `InsertFinal`s, so `implementDataAccessCompensation` + the
  `isSimpleResidualCompensation` allowlist stand in for that primitive. The allowlist is correct and
  each exclusion is pinned today, but it will rot the moment a new compensation shape appears with no
  allowlist arm (falls through to the dead-final-member path → silent no-plan). The honest fix is a Go
  `yieldUnknown`/exploratory-insert that re-optimizes all compensations and shrinks the allowlist to
  nothing — BLOCKED on Go's compensation re-optimization correctly handling IN-explode / correlated /
  index-only shapes (a naive exploratory-insert re-breaks them today, which is why the allowlist exists).

Go reaches a physical index scan / filter via THREE producers that bypass `Compensation`: the
data-access/compensation match path (`predicate_multi_map.go`), the Go-only `ImplementIndexScanRule`
(a fusion of Java's `ImplementPhysicalScanRule` + candidate matching that iterates predicates
directly), and `ImplementFilterRule` (synthesizes a `RecordQueryPredicatesFilterPlan` over the inner
winner). Java has ONE path (`AbstractDataAccessRule` → `toEquivalentPlan`) and enforces "index-only
value can't be a residual" ONCE via `Compensation.isImpossible()`. Because Go's extra rules don't
route through `Compensation`, RFC-045 enforces the index-only compensatability guard at multiple
layers: `valueContainsUncompensatable` (match path) + the residual-skip loop in
`ImplementIndexScanRule.OnMatch` (implement-index path) + a final-plan validation
`validateNoIndexOnlyResidual` in `Planner.Plan` (the `ImplementFilterRule` leak can't be guarded at
the rule — removing its member collapses the filter Reference and breaks the data-access intersection
memo, so the leaking *final* plan is rejected with `UnplannableIndexOnlyResidualError` instead).
All are load-bearing and pinned (`TestVectorPlan_QualifyPlansToVectorScan`,
`TestImplementIndexScanRule_SkipsIndexOnlyResidual`, `TestVectorPlan_MetricMismatchDoesNotMatchVector`),
so there is **no live bug** — but the layering is a smell whose root is the duplicated paths. Root fix
(Graefe-endorsed): retire `ImplementIndexScanRule` and route `ImplementFilterRule`'s filter
implementation through a single data-access rule backed by `Compensation`, at which point the
implement-layer guard AND the final-plan validation delete themselves and the property is enforced
once, as in Java. See DIVERGENCES.md "ImplementIndexScanRule is a Go-only second index-scan path".
  - **RFC-076 v3 ACK'd (Graefe + Torvalds), committed `75bf8d17`. v2's leaf-matching diagnosis was
    FALSIFIED by empirical reproduction.** Disabling `ImplementIndexScanRule` + tracing shows the
    match infra fires correctly (leaf scan↔scan `EqualsWithoutChildren=true`; `matchSingleSourceAgainstSelect`
    binds the predicate to the candidate Placeholder; `pushDataAccessTasks` fires) — the gap is that
    every seed-match path builds its MatchInfo with `maxMatchMap=nil`, so `PartialMatch.PullUp`
    (`partial_match.go:117`) returns nil → `CompensateCompleteMatch` → `ImpossibleCompensation` →
    `DataAccessForMatchPartition` skips → ZERO scans. `ImplementIndexScanRule` is the SOLE producer.
    `ComputeMaxMatchMap` (`max_match_map.go:167`) exists but is never called by the seeds.
  - **WIP STASHED (`git stash list` → top of stack on this branch).** Implemented the data-access
    completion per the Graefe-confirmed Java recipe: wire `ComputeMaxMatchMap` into the seed paths
    (leaf uses an identity map over the candidate result value; intermediate uses query/candidate
    result values + `NewAliasMapValueEquivalence`), residual compensation (re-apply unmatched
    predicates as filters via `OfPredicateCompensation` — Java produces the match even when fully
    residual), an IN-sargable guard (an IN comparison is NOT a contiguous range — leave it to the
    explode/InJoin path), and per-ref `AdjustPartialMatchesForRef` in `pushDataAccessTasks` (matches
    are seeded in PLANNING exploration, after the dead phase-start `AdjustMatches`, so ordering parts
    are only computed at consume time). **Validated:** full cascades unit suite GREEN with the rule
    enabled; 12/16 cited shape tests green with the rule disabled.
  - **REMAINING (multi-shift, per-feature vs Java — bigger than v2 stated):** broad `just test`
    exposes that the new (Java-correct) matches diverge from the rule's plans: (1) Go cost/Pareto
    pruning lets a non-unique index beat the unique one + breaks index intersection (`plangen`
    `UniqueIndexPointLookupPreferred`, `EndToEnd_IndexIntersection`); (2) `wrapScanPlanWithCoverage`
    (`abstract_data_access_rule.go:345`) doesn't propagate the candidate `unique` flag that
    `OrderedIndexScanRule` sets; (3) vector index-only-residual: a metric mismatch no longer raises
    `UnplannableIndexOnlyResidualError` (4 `TestVectorPlan_*`); (4) **DELETE over-deletes** →
    `TestFDB_DeleteOldAndLowValue` panic (correctness); (5) sort-elim ordering parts now computed but
    the satisfaction→ordered-scan→`RemoveSort` chain is incomplete (4 `TestSortElim_*`); (6) covering
    full-index-scan vs table scan (`TestPlanHarness` covering/range). Grind each rule-disabled,
    red-first, aligned to Java/plandiff; do NOT one-off guess (a `boundCount==0` guard diverged from
    Java and broke a Java-aligned unit test). THEN retire the rule + guard + final-plan validation.
    `ImplementFilterRule` STAYS (faithful Java port). Separate PR from RFC-077.
  - **RFC-076 v4 (2026-06-04): step 1 DONE (5 correctness fixes, Graefe+Torvalds ACK), full retirement
    in progress.** The data-access path is now correct for every FDB-tested shape (dual-correlation
    joins, simple joins, aggregate eq-filter, vector residuals). Full rule retirement needs: (3a)
    activate the dormant ordering-constraint pass (`constraintOnly` never set true → `PushRequestedOrderingThrough*`
    inert); (3b) template-aware costing (a nil-inner `Fetch` shell hides its inner from the cost model
    → join-order flip on `TestFDB_JoinSelPred_Repro`). See RFC-076 "v4 amendment" for the sequenced plan
    + the ref-resolving (not magic-constant) 3b. `validateNoIndexOnlyResidual` STAYS (now load-bearing
    via the DistanceRank residual). **Step-2 cleanup TODO (file/do during retirement, by the retirement
    PR): stop SEEDING `AggregateIndexMatchCandidate` partial matches onto non-GroupBy refs** in the
    leaf/intermediate match rules, so the agg-skip type-switch — currently duplicated 4× (`planner.go:465`
    data-access boundary [new], `rule_implement_index_scan.go` [dies with the rule], `rule_streaming_agg_from_index.go`,
    `rule_aggregate_data_access.go`) — collapses to one. Torvalds flagged the boundary guard as a
    defensible transition shim, NOT the permanent design; the don't-seed fix is the root cause.

### 7.6 — MERGED into 7.5+7.6 (RFC-077)

7.6 (source-anchored field pull-up / retire `composeFieldOverJoinMerge`) is no longer a separate
item: it is the same change as 7.5 (anchored RC retires the opaque merge → structural interning
falls out). See the holistic **7.5 + 7.6 (RFC-077)** entry above. RFC-073's deferred analysis is
the historical record.

---

## Phase 9: Vector / HNSW relational SQL parity (RFC-045)

**Context.** The record-layer / Cascades core of vector search is already ported and FDB-tested:
the HNSW graph (`hnsw.go`), the index maintainer (`vector_index_maintainer.go`), RaBitQ
quantization (`pkg/rabitq`), HNSW stats (`hnsw_stats.go`), `vec_math.go` / `fht_kac_rotator.go`,
chaos verification (`chaos/verify_vector.go`), and integration tests
(`vector_index_test.go`, `rabitq_test.go`, `hnsw_stats_test.go`, `bench/sift_benchmark_test.go`).
The Cascades *values* (`value_row_number.go` + `value_*_distance_row_number.go` seeds,
`value_row_number_high_order.go`), the match candidate (`vector_index_match_candidate.go`, 232 LOC),
and a `DistanceRank` comparison stub all exist. The SQL **grammar** is complete:
`vectorIndexDefinition` (`CREATE VECTOR INDEX … USING HNSW … PARTITION BY … OPTIONS(…)`),
`qualifyClause`, `overClause`, `windowSpec`, `nonAggregateWindowedFunction(ROW_NUMBER …)`.

**The gap = the relational front-end + Cascades wiring** (the "just not relational bits"):

**Status: DONE (RFC-045, Graefe+Torvalds ACK).** 9.1–9.4 all landed, tested, green. The full
SQL vector K-NN read path works end-to-end: a partitioned HNSW index +
`SELECT … WHERE <partition> QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY
euclidean_distance(vec, q)) <= K` plans to a BY_DISTANCE vector index scan and executes
against real FDB returning the k nearest records (`TestFDB_VectorSearch_QualifyE2E`). Also
fixed a latent vector-scan PK-extraction bug. **Known follow-up:** an *unpartitioned* vector
index + WHERE-less QUALIFY does not yet match the candidate (Java's vector search is always
partitioned) — fails to plan rather than returning wrong results; revisit if needed.

- [x] **9.1 DDL: `CREATE VECTOR INDEX … USING HNSW … PARTITION BY … OPTIONS(…)`** → metadata vector
  `Index` (type `vector`, HNSW options). No `vectorIndexDefinition` handler exists in `pkg/relational`
  today. Wire-compat: the index metadata + on-disk HNSW format must match Java byte-for-byte (core
  already does; DDL must produce the same `Index` proto + options).
- [x] **9.2 Query front-end: `QUALIFY ROW_NUMBER() OVER (PARTITION BY … ORDER BY <distance>(vec, q)) <= K`.** Done — walk.go builds DistanceValue + RowNumberValue; predicates.TransformRowNumberDistanceRankMaybe ports transformComparisonMaybe; QUALIFY lowers to a DistanceRank ComparisonPredicate.
  No `qualifyClause` handling and no window-function→Value visitor exist (`grep QualifyClause` → 0 hits;
  `extractFunctionNameFromCall` only returns the *name* string). Build the distance-specialized
  `RowNumberValue` (Euclidean / Cosine / Dot-product / EuclideanSquare) from the parse tree, fleshing
  out the seed value classes; port `RowNumberValue.transformComparisonMaybe` so `ROW_NUMBER() <= K`
  rewrites into a `DistanceRankValueComparison(queryVector, k, efSearch, isReturningVectors)`.
- [x] **9.3 Cascades wiring + vector physical plan.** Done — (9.3a) tryVectorIndexCandidate enumerates the candidate + ExpandVectorIndex builds the distance placeholder + valuesMatchColumn matches it; (9.3b) ToScanPlan splits partition prefix from the DistanceRank binding; (9.3c) RecordQueryVectorIndexPlan + executeVectorIndexScan dispatch BY_DISTANCE; physicalVectorIndexScanWrapper + the index-only compensatability guard (valueContainsUncompensatable via values.IsIndexOnly on the match path + the residual-skip loop in ImplementIndexScanRule) make the vector scan the sole physical winner — the DistanceRank predicate, being index-only, is never lowered to a residual filter, exactly as Java's match-then-implement does. Three pieces (Torvalds catch — not a single
  branch): **(9.3a)** add a vector branch to the match-candidate enumeration (next to
  `NewValueIndexScanMatchCandidate` at `plan_context_builder.go:46` + the metadata-driven builder in
  the embedded layer) so a `vector`-type index yields the candidate; **(9.3b)** rework
  `VectorIndexScanMatchCandidate.ToScanPlan` (`vector_index_match_candidate.go:200`, today a generic
  `NewRecordQueryIndexPlan`) to split partition-equality `ComparisonRange`s from the single
  distance-rank comparison (which rides as an *equality-shaped* range, à la Java
  `toVectorIndexScanComparisons`); **(9.3c)** introduce a vector-aware physical plan that threads
  query-vector/k/`ef_search`/`isReturningVectors` and at execution dispatches **BY_DISTANCE** via
  `ScanIndexByType`/`ScanVectorIndex` → `ScanByDistance` (`index_scan.go:338-345`) — without it the
  plan does a BY_VALUE scan that errors at `index_scan.go:269`.
- [x] **9.4 E2E proof.** Done — `TestFDB_VectorSearch_QualifyE2E` (sqldriver, real FDB): builds a partitioned vector schema, inserts vectors, EXPLAIN-pins the BY_DISTANCE vector scan for the full QUALIFY SQL query, executes it, and asserts the top-2 nearest records. (yamsql port + `ef_search`/OR-of-two-KNN/`42F21`-in-WHERE coverage remain as nice-to-have follow-ups.) Original plan: Port Java's `window-function-documentation-queries.yamsql` (KNN top-K via
  `QUALIFY`, `ef_search` option, `<`/`<=`, OR-of-two-KNN) as the Go conformance/yamsql scenario, plus an
  FDB integration test that `EXPLAIN`-pins the vector index scan (not a full-scan fallback) and asserts
  row + distance correctness. Window-functions-in-`WHERE` must error (Java: `42F21`).

Constraints to mirror from Java's `VectorIndexScanMatchCandidate`: exactly one distance-rank per query;
the index MUST be partitioned and the query MUST supply partition keys; the SQL distance fn MUST match the
index `metric`; ORDER BY must be ascending; `ROW_NUMBER()` is INDEX-ONLY (refuse without a matching index).
`@API(EXPERIMENTAL)` in Java — landed Jan–Mar 2026 (Java's 4.11 series).

- [x] **9.5 Multi-partition vector scan (partial partition prefix).** Done in RFC-046 — `vectorMultiPartitionCursor` ports Java's `flatMapPipelined(prefixSkipScan, scanSinglePartition)`: `findNextPartition` skip-scans the distinct partition prefixes, `searchOnePartition` runs one HNSW search per partition, per-partition top-K concatenated, full cross-partition `FlatMapContinuation` resume. Planner: `ComputeBoundParameterPrefixMap` keeps the equality prefix + always the DistanceRank binding (no nil-query-vector on a partial prefix); `parametersRequiredForBinding={distanceAlias}` (the full-prefix guard dropped, matching Java's `VectorIndexExpansionVisitor`). Partition inequality left unconsumed → residual (documented; endpoint-into-skip-scan is a perf follow-up). Graefe+Torvalds ACK. Pinned by `TestVectorPlan_PartialPrefixPlansMultiPartition`, `TestVectorPlan_PartitionInequalityNotConsumedIntoPrefix`, FDB E2E `TestFDB_VectorSearch_MultiPartition_{Fanout,InequalityResidual,Pagination}`. DIVERGENCES.md "Vector scan multi-partition" closed.

## Native fdbgo client — conformance & differential testing (RFC-010 Phase 1+)

RFC-010 Phase 0 (the wire-correctness fires: #1 inline reply error, #2 wrong_shard_server code,
#3 pipelined retry, #5 hedge queue-model leak, #8 ErrorOr union parse) landed. These three items
close the testing/conformance gaps its prevention plan (P5/P7) calls for.

### RFC-010 audit findings (the original 15 — correctness fires)

The execution list for the Codex source audit (`TODO_client.md`); full detail + C-conformance
reasoning per item in `rfcs/010-native-client-correctness.md`. **14 landed, 0 open, 1 false positive**
(#6 conn-shutdown via RFC-050, #11 TLS via RFC-051 closed the last two; updated 2026-06-13).

- [x] **#1** inline `LoadBalancedReply.error` decoded on read parsers (Phase 0)
- [x] **#2** `ErrWrongShardServer` 1062 → 1001 + anti-self-confirming fault test (Phase 0)
- [x] **#3** pipelined `Get` shares full classify→invalidate→retry; 1006 surfaced correctly (Phase 0)
- [x] **#4** tenant commit builder uses a scratch `[]MutationRef` — no in-place mutation of `tx.mutations`, no double-prefix on rebuild (build-twice regression; Torvalds + FDB-C ACK)
- [x] **#5** hedge loser/timeout/cancel QueueModel deltas released (Phase 0)
- [x] **#6 — HIGH.** Conn shutdown — fixed in RFC-050. One `failConnection(err)` path (`sync.Once`: cancel ctx + close socket + `failAllPending`) is the single teardown, used by `Close`, `connectionMonitor` death, and `readLoop` read errors. **(1)** `SendFrame`/`Flush` now wait on `errCh` **or** `ctx.Done()` (and deliberately don't pool `errCh` on the `ctx.Done()` path — audit #13 stale-value hazard), so a sender whose frame is still queued when `writeLoop` exits no longer hangs forever. **(2)** `connectionMonitor` death now calls `failConnection` — adding the missing `conn.Close()` that unblocks `readLoop`'s blocking `Read` (the old bare `cancel()` leaked the fd + goroutine until the 10 s TCP keepalive). Single-delivery to a pending reply still comes from the pending-map + `pendingMu` + delete-as-you-go; `closeOnce` only guarantees the meaningful error wins. SimTransport scope: built the in-process `net.Pipe` fake-server test harness #6 needs (handshake + stall / go-silent / abrupt-close modes) and made the monitor cadence injectable (unexported `withMonitorCadence` on an unexported `dialWith`; public signatures unchanged); the full seeded multi-mode SimTransport is deferred to C4 (YAGNI). 6 deterministic in-process `-race` tests (the two core ones verified failing on the pre-fix code: stranded-sender hang + monitor-no-socket-close leak). FDB-C + Torvalds ACK.
- [x] **#7 — MEDIUM.** Honor the "methods safe for concurrent use" contract — fixed in RFC-049. Writers already appended under `conflictMu`; the unprotected readers/clears now do too: `Commit` validation + read-only check snapshot `mutations`/`len(writeConflicts)` under the lock and **thread that validated snapshot into the marshal** (so a `Set` racing `Commit` can't ship an *unvalidated* mutation to the proxy — FDB-C catch); `buildCommitTransactionRequest`/`commitDummyTransaction` snapshot the conflict headers under the lock (append-only + `conflictBuf`-only-grows ⇒ snapshot-and-release is race-free for them); `GetApproximateSize` iterates **under** the lock (not a released snapshot — it can race `Commit`'s in-contract auto-reset, which `[:0]`-reuses the backing arrays); `mutations[:0]` clears moved inside `conflictMu` in reset/postCommitReset; `addWriteConflict*` moved the `nextWriteNoConflict`/`writeConflictsDisabled` gate inside the lock (the one-shot flag is read+cleared on the `Set` path → two concurrent `Set`s raced). `Set`/`Clear`/`ClearRange`/`Atomic` now publish the mutation + its write-conflict range **atomically** under one `conflictMu` acquisition (codex catch — the old two-lock split let a `Commit` snapshot ship a mutation *without* its conflict range → a missed conflict; this also subsumes the `nextWriteNoConflict` fix and drops `Set` from two locks to one). Contract doc narrowed: option setters (`SetXxx`) + `Reset` are configure-before-use, not concurrent-safe (matches `fdb_transaction_set_option`); RYW lost-update stays documented-not-safe. 6 deterministic concurrency tests (verified failing on the pre-fix code) + tenant no-alias sentinel + validated-snapshot pin + Set-atomicity invariant. FDB-C + Torvalds + codex review.
- [x] **#8** `ReadErrorOr` parses the union tag (not field count); error code uint16 (Phase 0)
- [x] **#9** rename `isSystemKey` → `isSpecialKey` (tests `\xff\xff` special-key space; behavior unchanged)
- [x] **#10** decoupled `ACCESS_SYSTEM_KEYS` from `LOCK_AWARE` in `fdb/options.go` (C sets them
  independently — confirmed NativeAPI 7159 / RYW 2557 / TenantManagement). Facade no longer
  auto-sets lock-aware; each `fdb/database.go` tenant call site sets the exact C++ options (writes
  ACCESS+LOCK_AWARE; OpenTenant READ_SYSTEM_KEYS+READ_LOCK_AWARE; ListTenants
  READ_SYSTEM_KEYS+LOCK_AWARE). Behavior change: external callers
  relying on the implicit coupling must set `SetLockAware` explicitly (as a Java/CGo app must) — only
  observable on a *locked* DB; wire-safe (lock-aware is a commit flag, not persisted bytes).
  Pinned by `TestSetAccessSystemKeys_DoesNotImplyLockAware` (facade unit test, fails if the coupling returns).
- [x] **#11 — MEDIUM.** TLS wired end-to-end — fixed in RFC-051. `ParseClusterString` parses the `:tls` coordinator suffix (faithful to C++ `NetworkAddress::parse`: strip `(fromHostname)` then `:tls` when len>4; uniform-cluster, mixed rejected) → `ClusterFile.UseTLS`; `database` carries `tlsConfig *tls.Config` and `getOrDialConn` dials TLS; `resolveTLSConfig` loads `FDB_TLS_{CERTIFICATE,KEY,CA}_FILE` (→ `/etc/foundationdb/{cert,key}.pem` default) into a standard config, C++-precedence-faithful. **Go-idiomatic user-facing API (bradfitz review):** `transport.Dial(ctx, addr, *tls.Config, dialFn)` — the non-nil config is the *only* "use TLS" signal (nil = plaintext), so the silent-downgrade footgun is gone by construction (the `useTLS bool` + `DialWith`/`DialWithTLS` overloads + bespoke `transport.TLSConfig` are deleted); `fdb.OpenDatabase(clusterFile, WithTLSConfig(*tls.Config), WithDialFunc(...))` functional options, precedence explicit > `FDB_TLS_*` env > default; `upgradeTLS` clones + fills `ServerName`/`MinVersion` only if unset. 6 deterministic tests incl. a real in-process mutual-TLS handshake (FDB ConnectPacket inside the tunnel) + wrong-CA/missing-client-cert rejects. FDB-C + Torvalds + bradfitz ACK. Follow-ups: per-address TLS flag (dual-listen), `FDB_TLS_VERIFY_PEERS` rule DSL, `FDB_TLS_PASSWORD`/encrypted keys, FDB-TLS testcontainer e2e.
- [x] **#13 — LOW (concurrency-sensitive).** Fixed in **RFC-072**. The reply channel is now returned to the pool exactly on the no-send-can-race paths: `Release()` pools it on the success path (caller received, no `Cancel`); `Cancel()` pools iff it won the `delete` race and nils `h.ch` so `Release` never double-pools; `SendAndWait` pools on success and via `cancelPending` (delete + pool-iff-won) on timeout, leaving the rare race-loser to GC (it may hold a stale buffered value). The false "readLoop returns it after dispatch" comment is corrected — readLoop only delivers. Pinned by `reply_pool_test.go` (won/lost-race + success + no-double-pool, `-race`-clean) via a `putReplyChannel` seam (deterministic, not `sync.Pool`-reuse-dependent). Full multi-goroutine timeout-vs-delivery race coverage awaits `SimTransport` (C4). FDB-C + Torvalds ACK.
- [x] **#14 — LOW.** Monitor ping on a saturated `writeCh` — fixed in RFC-052. The send was already non-blocking (`select … default`), but the drop path returned a **closed** `done`, which the monitor read as `case <-replyCh:` "PING reply arrived → connection alive" — so a *stuck* connection (writeLoop blocked on an undrained socket ⇒ `writeCh` saturates) falsely passed as alive and never reached the `bytesReceived` liveness check (the one state where the monitor must act). Fix: the drop path returns **nil** (never selected in the monitor's `select`) so it falls through to the timer → `bytesReceived` kill — faithful to C++ `connectionMonitor`, whose liveness verdict is solely bytes-received (the ping-reply arm only restarts the cycle; C++ `Peer::send` is an unbounded buffer with no "couldn't send" path). Pinned by `TestSendPingWithReply_DropsToNilOnFullWriteCh` (verified failing on the pre-fix closed-`done`); the sent-path kill stays covered by `TestConn_MonitorDeathClosesSocket`. FDB-C + Torvalds ACK.
- [x] **#15** range-iterator next-begin via `keyAfter` helper that copies (no alias/scribble of `lastKey`); spare-capacity unit pin
- **#12 — FALSE POSITIVE.** Locality never panics (invariant guarantees non-empty); add a defensive guard at most.

We **cannot** run FDB's deterministic simulation: Sim2 is a hermetic single-threaded Flow event
loop with an in-memory network and no external socket, so a real Go client can't join it, and
server-side BUGGIFY edge-case injection exists only inside Sim2. But three of FDB's real,
externally-usable artifacts CAN be exercised against a testcontainer cluster our Go client
mutated. (Determinism for our OWN retry/LB/wire-error paths — `PendingGet.Resolve`'s
flush/transport/timeout arms, the codex 1006 drop-between-dial-and-send race, transparent
wrong-shard retry — comes from a seeded in-process `SimTransport` fake server behind
`transport.DialFunc`, extending the existing `wrongShardConn`; tracked as a separate Phase-1 item.)

- [x] **C1. Ride their oracle — FDB `ConsistencyCheck` after Go-client writes.** DONE
  (`pkg/fdbgo/conformance/consistencycheck_test.go`). `RunCluster(3, double, ssd)` →
  pure-Go client writes 1000 keys → wait replication-healthy → run FDB's one-shot
  `fdbserver -r consistencycheck` role → parse its JSON trace and assert it completed
  (`ConsistencyCheck_FinishedCheck`), examined data, and emitted **no** Severity-40
  inconsistency/`TestFailure` event. **Double redundancy is required** — under single
  redundancy the checker's replica comparison is a no-op (one copy per shard). Anti-vacuity:
  require `GetKeyValuesStream` reads (one per replica per shard) **>** `FirstValidServer`
  baselines (one per shard) — i.e. some single shard was read on ≥2 replicas, which a bare
  "≥2 reads total" count can't prove (N single-replica shards defeat it). `FirstValidServer`/
  `CheckCustomReplica` fire even under single redundancy and do NOT prove a comparison. The
  process exit code is unreliable (exits 0 even on inconsistency), so detection is by trace
  event: any Sev40 `ConsistencyCheck_*` (catch-all), the SevInfo `InconsistentStorageMetrics`,
  and Sev40 `TestFailure` reasons containing "inconsistent". Detection logic pinned by a
  deterministic unit test (`TestParseConsistencyTrace`) since the live run is always clean.

- [x] **C2. Ride their client — differential vs the official C binding (`libfdb_c`).** Landed in
  **RFC-053 (PR #231)**. Differential harness in `pkg/fdbgo/bench` (reuses the dual-client fixture):
  L2 write battery (byte-identical persisted state — Set shapes incl. exactly-VALUE_SIZE_LIMIT, every
  atomic on a missing key pinning the Min→MinV2/And→AndV2 upgrade, SetVersionstampedValue offset,
  key-at-KEY_SIZE_LIMIT boundary) and L3 read parity (GetRange chunking-invariance across
  StreamingModes/limits/reverse + GetKey selector parity, read-version-pinned). Proven to have teeth
  (reverting Min→MinV2 fails it byte-exactly). **Surfaced & fixed FOUR real client divergences**, each
  pinned with a fail-pre-fix test: SetVersionstampedKey spurious write-conflict range; client-side
  key/value size-limit enforcement (set/atomic reject at commit, clear clamps/drops); raw-access key
  limit set by ACCESS_SYSTEM_KEYS/READ_SYSTEM_KEYS (not just RAW_ACCESS); raw-access slack gated off
  for tenant txns. Reviewed by FDB-C-dev + Torvalds + codex (3 P2s) + @claude.
  **Follow-up RFC-054: `FuzzDifferential`** — random op sequences through both clients,
  byte-identical persisted state (RYW coalescing, atomic accumulation, clear/overwrite
  ordering); 40s burst = 8068 execs, 0 mismatches.
  **Follow-up RFC-055: RYW-read differential (Get/GetRange)** — found+fixed a getRange
  merge bug that dropped empty-value pending keys.
  <details><summary>original spec</summary>
  The C
  binding is the client FDB simulation-tests on every CI run, so matching it is the closest we get
  to inheriting that coverage (RFC-010 prevention P5, corrected). Run the SAME operations through
  our Go client and `libfdb_c` against the same testcontainer cluster. **CRITICAL: compare at the
  DATA plane, never the wire.** Request frames are legitimately NOT byte-identical — reply-promise
  UIDs, read/committed versions, trace/span IDs, GRV batching, mutation/conflict ordering, and
  range chunk boundaries all vary per client. So:
    - **Writes → byte-exact on PERSISTED bytes.** Write the same logical mutation via each client,
      read the raw keys/values back out of FDB, assert byte-identical: key/tuple encoding, value &
      record format, index entries, version at `pk+\xff`, split chunking, continuation-token bytes
      + magic `6773487359078157740`. This is the cross-client compatibility hard line — where
      byte-identity is both *required* (Java/Go share a cluster) and *achievable* (the persisted
      format is spec-fixed; control-plane randomness never touches it).
    - **Reads → semantic, control-plane excluded.** Same key/range + a pinned read version →
      compare returned value / merged KV set + order / error CODE (not message). Ignore reply
      tokens; don't compare the literal version number (compare the data it produced); merge range
      chunks before comparing. Under deliberate concurrency, compare error CLASSES, not exact codes.
    - **Continuations → mutually resumable** (a Go-produced continuation resumes correctly when fed
      back; byte-equal where the format is fully spec-pinned). Any *data-plane* byte difference is a
      real wire-compat bug, NOT a tolerance to normalize away.
  </details>

- [ ] **C2-followup. RYW key-selector + read-version correctness audit (RFC-056).** Remaining
  RYW read-resolution divergences from libfdb_c surfaced by the RFC-055 differential:
  (2) a go-vs-cgo read-version
  staleness asymmetry (go=`transaction_too_old(1007)` while cgo succeeds on the SAME pinned read
  version near the 5s MVCC edge). **Characterized (RFC-056 #235): PERF/TIMING, not a wire/
  behavioral divergence** — both clients correctly return 1007 once a read version genuinely ages
  past the 5s window; go just reaches that edge sooner under CPU starvation because its getKey
  does more per call (the materializing `buildSegmentsLocked` vs libfdb_c's lazy iterator), and
  the differential pins one version then issues 28 selectors on it. So behavioral identity HOLDS;
  the real fix is the lazy iterator (continuation item 1 below), which reduces the per-call work
  at the source. The differential is already robust (retries the transient 1007 with a fresh
  version via the canonical `gofdb.IsRetryable` predicate — `differential_getkey_ryw_test.go`).
  REMAINING: profile go-getKey 1007-rate vs cgo to confirm item-1 closes it. See rfcs/055.
  - [x] **(1) `Transaction.GetKey` ignores pending writes** — FIXED (RFC-056): faithful port of
    C++ `resolveKeySelectorFromCache` over a merged segment view (`pkg/fdbgo/client/ryw_getkey.go`:
    `rywSegmentIterator`/`buildSegmentsLocked` + `getKeyRYW`'s unknown-range server-read-remerge
    loop), wired into `Transaction.GetKey` (+ the base↔resolved RANGE read-conflict, fixing the
    old single-key conflict) and `Snapshot.GetKey` (writes visible by default via
    `includeWrites=!snapshotRYWDisabled`). A merged-GetRange shortcut was verified-WRONG on
    `{orEqual, offset>1}` — not used. Pinned by `ryw_getkey_test.go` + the
    `TestDifferential_GetKeyRYW` differential (pending Set/Clear/ClearRange vs libfdb_c) + corpus
    seeds. **Two deferred sub-edges, same root** (the rywCache doesn't preserve per-key op-type
    — it eagerly folds resolved atomics into plain entries and moves a matched CompareAndClear
    into the cleared list; faithfully closing either needs a write-map that retains op-type, like
    C++'s):
    (a) **phantom offset slot** — a PENDING atomic that resolves to no value (CompareAndClear, or
    an atomic on a locally-cleared range) is modeled as absent; libfdb_c keeps it as a "phantom"
    is_kv slot COUNTED in the offset walk. The getKey differential is scoped to non-atomic pending
    writes until then.
    (b) **conflict-range filtering** — C++ `updateConflictMap` SUBTRACTS independent-write/cleared
    segments from the getKey read-conflict (no DB read there). Go keeps the FULL base↔resolved
    range: it OVER-conflicts on those segments (extra retries, always SAFE) rather than risk a
    missed conflict on a folded dependent atomic (an UNSAFE under-conflict — a naive
    `!hasAtomics` filter was attempted and reverted after codex showed it drops the conflict for a
    Get-folded atomic). The full range is strictly better than the old single-key conflict (which
    under-conflicted). Exact filtering deferred with the op-type preservation above.
  - [x] **RYW applyAtomic on present-empty values** — FIXED: the chain conflated `nil` (absent)
    with present-empty, so a V2 op after `Xor(k,"")` took the absent→operand path (`Min(k,"0")`
    → 0x30 vs libfdb_c 0x00). The get/merge chains now keep present-empty non-nil (nil reserved
    for absent), mirroring C++ `Optional.present()`. Pinned by
    `TestRYWGetRange_V2AtomicOnPresentEmpty`.
  - (3) **versionstamped-pending read = unreadable.** A SetVersionstampedKey/Value pending on a
    key reads as ABSENT in Go pre-commit (Get→nil, GetRange→omit); C++ marks it `is_unreadable`
    and THROWS `accessed_unreadable`. Go has no unreadable state — approximated as absent,
    consistently across ALL base states: storage-absent, locally cleared, a pending plain Set,
    and a non-nil storage value the pending stamp shadows. `atomic()` refuses to eager-fold a
    versionstamp into a plain entry, and `resolveAtomics` short-circuits the chain to
    `unresolved` (terminal, dominant over cleared) so both read paths exclude the key and drop
    any stale storage value. Pinned by `TestRYW_VersionstampedAbsentNoPhantom` +
    `TestRYW_VersionstampedOverClearedOrPlainNoPhantom`. Full C++ parity (THROW on read) still
    needs an explicit unreadable concept — part of the RFC-056 audit.

- [ ] **RFC-056 continuation — ordered, ONE AT A TIME (do 1, then 2, then 3).** After the merged
  getKey-RYW core (#235), three follow-ups remain. Both 1 and 2 WILL be done (sequentially, not
  batched); 3 is the ongoing hunt.

  1. **[x] DONE (RFC-057).** Lazy `rywSegCursor` replaced the materializing
     `buildSegmentsLocked`: getKey cost is now FLAT in cache size — **57 ms / 39 MB →
     1 µs / 816 B at N=100k (55,437×)**, measured before/after (Torvalds's "no benchmark =
     no merge" gate). Behavior-identical: a 4000-state equivalence property-test oracled
     against the retained materializer + the RFC-056 differential + a 94k-exec fuzz burst,
     all green. `next`/`prev` are a single merged-boundary `skip` (no view desync). The
     original plan:
     **Lazy/windowed segment iterator for getKey-RYW.** `buildSegmentsLocked`
     (`pkg/fdbgo/client/ryw_getkey.go`) MATERIALIZES the whole merged-segment partition of
     [allKeysBegin, maxKey) — O(writes + cacheKeys) per resolution attempt — whereas libfdb_c's
     `RYWIterator` is LAZY (a steppable zip of the write-map + snapshot-cache sub-iterators).
     Port the lazy cursor (skip/next/prev computing each segment on demand, no full
     materialization), so getKey cost is bounded by the walk distance, not the cache size. This
     ALSO shrinks **item (2)** below: less work per getKey under heavy parallel-container load →
     less likely to drift past the 5s MVCC window mid-loop → fewer transient
     `transaction_too_old(1007)`. Validate with a profiling probe: go-getKey wall-clock +
     1007-rate vs libfdb_c, before/after; confirm resolution stays byte-identical
     (`TestDifferential_GetKeyRYW` + unit tests green). Then this de-flakes item (2) at the source
     rather than only via the differential's retry.

  2. **[x] DONE (RFC-058).** Op-type-preserving write-map closed BOTH sub-edges. Added `absent`
     (phantom) + `dependent` (DEPENDENT_WRITE, carried unchanged through folds like C++
     `isDependent()` reading the immutable stack bottom) to `rywEntry`; a matched CompareAndClear
     now stays a write-map entry (never moved to `cleared`). The differential **disproved the
     original framing of (a)**: getKey is a limit-1 range read in C++ (`read(GetKeyReq)` =
     `getRangeValue`/`getRangeValueBack`), so a phantom is COUNTED in the offset walk but SKIPPED
     at the landing — not "counted and landed on." Modeled as `segPhantom` (count-in-walk +
     directional skip-at-landing); the old `segEmpty` under-counted for offset>1, a naive `segKV`
     wrongly landed on it. Also fixed a pre-existing fold-path bug the same differential caught
     (`doMax(_,"")`→nil misread as absent by a later CompareAndClear). (b) Ported `updateConflictMap`
     (ReadYourWrites.actor.cpp:335) as `conflictRangesLocked` — the getKey read-conflict now
     SUBTRACTS INDEPENDENT writes + cleared ranges (safe now that op-type is preserved; the naive
     `!hasAtomics` filter codex NAK'd on #235 is impossible here). Proof: getKey differential
     re-enabled for pending CAC/atomics + 92k-exec fuzz (sub-edge a); a deterministic commit-order
     `TestDifferential_GetKeyConflict` whose INDEPENDENT/CLEARED cases FAIL without the filter and
     pass with it (sub-edge b). FDB-C-dev + Torvalds ACK on the RFC. Original (a)/(b) text:
     (a) **phantom-slot offset counting** — a PENDING atomic that resolves to no value
         (CompareAndClear, or an atomic on a locally-cleared range) is an `is_kv` "phantom" slot
         COUNTED in the getKey offset walk in libfdb_c, but Go currently models it as absent. With
         op-type preserved, count it. (Re-enable pending-atomic shapes in the getKey differential.)
     (b) **exact `updateConflictMap` conflict filtering** — getKey's read-conflict should SUBTRACT
         independent-write + cleared segments (no DB read there); Go currently keeps the
         conservative FULL base↔resolved range (safe over-conflict). With op-type preserved, the
         subtraction is safe (a naive `!hasAtomics` filter was UNSAFE — it dropped the conflict
         for a Get-folded dependent atomic; codex caught it on #235 → reverted). Port
         `updateConflictMap` (ReadYourWrites.actor.cpp:335) faithfully and pin with a conflict
         differential (concurrent write inside the range must conflict identically in both clients).

  3. **Fresh differential axes (`/hunt-divergences`).** Probe axes still uncompared vs libfdb_c:
     atomic-op edge cases across ALL of `Atomic.h` (empty / missing / present-empty operand per
     op); error-code + option semantics (RAW_ACCESS / ACCESS_SYSTEM_KEYS / snapshot-RYW); key
     encoding / tuple packing / versionstamp-offset validation. Each closed axis is more "absolute
     proof we're identical to the C client."
     - [x] **[RFC-059 — MERGED #238] RYW-disable-after-op poison.** Differential characterization
       corrected the earlier (imprecise) framing: NOT a per-read overlap check, NOT an
       option-set-time error. libfdb_c's `setOption(READ_YOUR_WRITES_DISABLE)` after any read or
       write throws `client_invalid_operation` deferred via `deferredError`, so the option call
       succeeds but EVERY subsequent op (regular + snapshot reads/GetKey, GetRange, GetReadVersion,
       GetEstimatedRangeSizeBytes, GetRangeSplitPoints, Commit) returns 2000 — the whole txn is
       poisoned. Go was silently permissive (returned 0). RFC-059 ports the poison
       (`Transaction.rywPoisonErr` set on disable-after-op, gated uniformly at `ensureReadVersion` +
       the metrics path; a `hadRead` signal covers the GetPipelined non-caching read). Pinned by
       `TestDifferential_RYWDisableAfterOp` + `TestCommit_RYWPoisonBeatsTimeout`. Reviewed by
       FDB-C++ dev + Torvalds + codex + @claude.
     - [x] **[RFC-060 — MERGED #239] tuple-codec byte-identity differential.** The tuple/key encoding is the wire
       hard line but has ZERO differential coverage vs libfdb_c's codec. `pkg/fdbgo/fdb/tuple` is a
       near-verbatim port (core encode/decode byte-identical by inspection) but adds go-only
       hot-path helpers (`PackWithPrefix`/`Pack1WithPrefix`/`Pack1ConcatWithPrefix`/
       `PackConcatWithPrefix`/`Packer.AppendInto`/`packerPool`) absent from libfdb_c that build the
       actual index/record keys on the wire. Prove `gotuple.Pack() == cgotuple.Pack()` across all
       type codes + boundaries (int size-limit boundaries, big.Int >8 bytes + leading-0xff
       zero-fill, float/double sign-bit flip, nil-escaping in bytes/strings/nested, versionstamp
       offset), the go-only helpers vs canonical `cgotuple.Pack()`, cross-client Unpack, and an
       end-to-end FDB wire round-trip. cgotuple is itself pinned to the cross-language
       `tuples.golden`, so this transitively pins go to the golden vectors.
     - [x] **[RFC-061 — MERGED #240] SNAPSHOT_RYW_ENABLE/DISABLE counter.** Found via the
       transaction-option-semantics survey, confirmed differentially: libfdb_c models snapshot
       RYW as an integer counter (ENABLE++, DISABLE--, bypass iff <=0, default 1), but Go used a
       boolean with `SetSnapshotRywEnable()` a no-op — so `disable→enable` left snapshot reads
       stuck bypassing RYW (go silently too permissive). Fixed: `snapshotRYWDisableCount int`
       (zero-value-safe inverse: DISABLE++, ENABLE--, bypass iff >0; preserved across reset as a
       persistent option). Pinned by `TestDifferential_SnapshotRYWReenable` (10 sequences, 3
       red→green + a counter-vs-boolean discriminator + negative-count axis + RYW-disable
       dominance).
     - [x] **[RFC-062 — MERGED #241] atomic-fold width/edge differential.** Atomic fold semantics
       are the wire hard line; the existing differential only used 8-byte operands on missing keys.
       Added a differential across operand/base widths + edge operands for all 12 ops. KEY finding
       (teeth-check): tx.Set/tx.Atomic ship RAW mutations (server folds at commit), so Go's
       client-side fold (doAdd/doMin/…) runs ONLY on in-txn reads — a commit-then-read-back test
       passed even with doAdd broken. Restructured to read WITHIN the txn (exercises the fold) +
       committed read-back (server fold/wire). Verify-and-pin (fold is a faithful port); teeth
       confirmed on doAdd (6 rows) + doByteMin (4 rows). Found+fixed a test-isolation bug (go/cgo
       shared a key → missing-key fold saw the other's committed value).
     - [x] **[RFC-063 — MERGED #242] versionstamp-mutation differential.** SetVersionstampedKey/Value
       were excluded from the fuzz differential; only a Go-only interop check + an offset-0 Value
       case existed. Added masked (10-byte stamp zeroed) go-vs-cgo differentials: VersionstampedKey
       (offset 0 / after-prefix / mid-key / binary), VSValueOffsets (non-zero offsets), tuple
       PackWithVersionstamp (offset + 2-byte user-version preservation), GetVersionstamp parity
       (10-byte, == materialized stamp), error/boundary (tight-valid offset+10==body vs off-by-one
       reject, negative, past-body, too-small, empty → 2000 go==cgo), multi-op. Mask offset is
       template-derived + length/surround/non-zero guards (Torvalds). Teeth: loosening
       validateVersionstampOffset by 1 reddens offbyone_reject. The differential CORRECTED a
       reviewer assumption: two versionstamped ops in one txn get the SAME stamp (txn-level, not
       per-op batch id; user differentiates via tuple user version).
       - [x] **Follow-up (tenant +8 offset) — DONE + found a BIGGER bug.** Built the tenant
         differential harness in `pkg/fdbgo/bench` (`differential_tenant_test.go`: shared tenant on
         both clients; TenantVersionstampedKey masked read-back + raw full-key +8 assertion,
         TenantVersionstampedValue value-offset-NOT-adjusted, TenantVersionstampErrors boundary).
         The +8 offset adjustment (`commitpath.go`) was already correct (go==cgo). But the harness
         immediately surfaced a REAL cross-client wire-compat divergence: the tenant `nameIndex` and
         `lastId` are `TupleCodec<int64_t>` (minimal-width); `tenant_crud.go` hard-coded the fixed
         9-byte form (`0x1C`+8) for both pack AND unpack, so a Go client could NOT open/list/delete a
         tenant created by libfdb_c/Java (`OpenTenant` failed "expected 9 bytes, got 2"), nor create
         a tenant after one (couldn't decode the C-written `lastId`). Fixed the codec to FDB's real
         minimal-width tuple-int encoding (Tuple.cpp:204-227); reads legacy 9-byte values too.
         Pinned by `TestDifferential_TenantCrossClientCRUD` (go↔cgo create/open/write/read/list) +
         `tenant_crud_internal_test.go` (FDB-spec vectors, round-trip, legacy decode, errors).
     - [x] **[RFC-064 — MERGED #243] explicit conflict-range API differential.** AddReadConflictRange/
       Key + AddWriteConflictRange/Key feed the resolver (isolation) but had no differential coverage
       (RFC-058 covered only getKey-DERIVED conflict ranges). Empirically NO divergence — edges
       (inverted→2005, empty→accept, oversized→accept) match go==cgo (the C++ NativeAPI source has no
       release inverted-check, but the C binding cgo uses returns 2005 — the differential is the spec,
       not the source). Pinned the conflict OUTCOME: read-conflict range/key (A fails 1020 iff probe
       inside, half-open r0 incl / r9 excl), write-conflict range/key (a concurrent reader fails iff
       inside A's write-conflict), snapshot-read-no-conflict, self-write+read-conflict. Reused RFC-058
       pinning (both A+B SetReadVersion(vSetup), transient→retry, fresh prefix/attempt, bounded) →
       flake-free (5 runs). Teeth: empty key-conflict range → key_exact_r5 diverges. Oversized
       committed-truncation is unobservable (keys > maxKeySize are unwritable).
     - [x] **[RFC-065] getKey boundary resolution — REAL BUG FIXED.** The existing
       getKey differentials cover the keyspace INTERIOR + clamp off-prefix results, masking the
       EDGES. A boundary probe found a real divergence: a BACKWARD selector (lastLess*) at/past
       maxReadKey (\xff) wrongly returned \xff itself instead of the greatest key < \xff. Root
       cause: resolveKeySelectorFromCache (ryw_getkey.go) short-circuited EVERY off-end seek to
       readThroughEnd, ignoring direction; C++ it.skip() clamps to the last segment and only sets
       readThroughEnd after the walk for offset>1. Fix: direction-aware off-end branch — forward
       keeps readThroughEnd; backward repositions onto the last segment and resolves backward.
       Pinned by TestDifferential_GetKeyBoundary (pinned-version differential: lastLess*(maxReadKey)
       asserted < maxReadKey, empty/large-offset/past-max edges). Teeth: re-introducing the
       unconditional shortcut reddens LLT/LLE_maxReadKey. Only the RYW path had it; rywDisabled
       delegates to the server.
     - [x] **[RFC-067 — MERGED #247] error-CODE differential → TRANSACTION_SIZE_LIMIT + 4 linked fixes.**
       A fresh error-CODE differential (`TestDifferential_ErrorCodes`, comparing the FDB error code
       each client returns for the same size/legal-range triggers) found a REAL write-path divergence:
       the Go client did NOT enforce `TRANSACTION_SIZE_LIMIT` by default — it committed >10 MB txns
       that libfdb_c rejects client-side with `transaction_too_large` (2101). C++ defaults every txn's
       sizeLimit to the 10 MB knob (NativeAPI:6133); Go's `0=disabled` default left no enforcement.
       Fix: default to the knob. Four more linked fixes surfaced via review + differential: (2) online-
       indexer lessen-work codes (Torvalds — wrong numbers, missing 2101, made latent-live by the
       limit; now matches Java `IndexingThrottle.lessenWorkCodes` 1:1); (3) commit-validation ORDER
       (codex — read-only fast path + per-mutation-before-size; then the full eager-vs-deferred model:
       key/value-size + versionstamp-offset are EAGER first-invalid-op-wins, txn-size DEFERRED; pinned
       by `TestDifferential_VersionstampValidationOrder`, 8 cases); (4) `metadataVersionKey` write
       contract (codex+FDB-C+++Torvalds — a blanket `continue` silently committed every illegal mvk
       mutation where libfdb_c returns 2000/2004; replaced with the exact C++ gate; pinned by
       `TestDifferential_MetadataVersionKey`, 8 cases); (5) size the VALIDATED snapshot not the live
       buffer (codex — a Set racing Commit could fail a small commit for an unshipped mutation; pinned
       by `TestApproximateCommitSize_SizesSnapshotNotLiveBuffer`). Also fixed a pre-existing
       differential-harness flake: pinned-version range reads now retry the transient 1007 (stale pin
       under parallel-container load) instead of `t.Fatalf` (pinned by
       `TestDifferential_PinnedRangeRetriesStaleVersion`). Reviewed clean by FDB-C++ dev + Torvalds +
       codex (per-commit deltas + full review) + @claude.

- [x] **GRV `locked` enforcement — DONE (RFC-096, FDB-C++ + Torvalds ACK on RFC; found by the
  RFC-095 reply ground-truth net).** The Go client silently read LOCKED databases where C++/Java
  refuse with `database_locked` (1038): `parseGetReadVersionReply` discarded `rep.locked`. Now
  enforced per C++ (`NativeAPI.actor.cpp:7425-7426`): `locked` threads from the batched GRV reply
  to every waiting transaction; the per-txn check at the `extractReadVersion` analog
  (transaction.go ensureReadVersion) returns 1038 unless `lockAware || readLockAware` (both C++
  options set `options.lockAware`, `:7077-7091`). The shared cache updates BEFORE the check (C++
  `:7409` precedes `:7425`), and — because Go's GRV cache is ALWAYS-ON unlike C++'s opt-in
  USE_GRV_CACHE (divergence filed below) — `locked` rides the cache (`grvCache.lastLocked`,
  stored only on version-CAS acceptance so a stale reply can't fail-open; Torvalds condition) and
  cache hits flow through the same per-txn check. Pinned by
  `TestFDB_DatabaseLocked_ReadPathEnforcement` (dedicated container; real `\xff/dbLocked` lock
  via the C++ `lockDatabase` mechanics; arms: fresh-fetch 1038, warm-cache 1038, LOCK_AWARE ok,
  READ_LOCK_AWARE ok, unlock+poll recovery) — revert-proven red without the check — plus the
  production-parser `locked` assert in the `GetReadVersionReply_locked` reply vector.

- [x] **GRV cache is ALWAYS-ON in Go; opt-in (USE_GRV_CACHE) in C++ — DONE (RFC-104).** Closed:
  the cache is now opt-in, default off. Cache READS are gated on the transaction's `useGrvCache`
  (`SetUseGrvCache`/USE_GRV_CACHE 1101; `SetSkipGrvCache`/SKIP_GRV_CACHE 1102, skip wins) at
  `grv.go:284` and the background refresher only starts on the first opted-in request
  (`grv.go:293`) — matching C++ `NativeAPI.actor.cpp:7504-7517` (gate `:7505`, default false
  `:6148`). The opted-in cached path fail-opens on `locked` exactly as C++ does (`:7514-7516`), so
  RFC-096's `lastLocked` ride-along — which existed ONLY to compensate for the previous always-on
  cache — was removed (`grv.go:38-45`). The RFC-098 wrong-answer (a default Go txn serving a
  version older than a libfdb_c-committed seed) no longer reproduces: a DEFAULT Go read now sees
  cgo-committed data directly. Pinned by `TestFDB_GRVCache_OptInOnly`,
  `TestFDB_GRVCache_RefresherStartsOnOptInMiss`, `TestFDB_GRVCache_SkipOverridesUse`
  (`client/grv_cache_optin_test.go`) + `TestDifferential_GRVCacheDefaultSeesCgoSeed`
  (`bench/differential_grvcache_test.go`). Differential-test causality comments already rewritten
  to "key-ownership hygiene, not a workaround" (`bench/differential_unreadable_test.go`).

- [ ] **C3. Ride their test designs — port FDB workloads as scenario + invariant specs.** FDB's
  `fdbserver/workloads/*.actor.cpp` (Cycle, AtomicOps, ConflictRange, Serializability,
  FuzzApiCorrectness, …) are unrunnable for us (Sim2-only), but each scenario + invariant is
  language-agnostic. Port the adversarial designs — e.g. Cycle: maintain a ring of pointer K/Vs,
  hammer it concurrently (+faults), verify the ring stays unbroken — to drive our client against
  testcontainers (and later `SimTransport`). Reimplement the harness; reuse the proven scenarios.
  Extends the existing `pkg/recordlayer/chaos` model-based approach + `cmd/fdb-binding-stress`.

- [x] **C4. Deferred Phase-0 test gaps — DONE (RFC-118 SimTransport).** All four closed with
  revert-proven regressions (`client/simtransport_test.go`, migrated `client/fault_test.go`):
    - **Inline `LoadBalancedReply.error` on `parseGetKeyReply` / `parseGetKeyValuesReply` / `parseGetValueReply`** —
      the `TestWrongShardServer_*` tests now inject through the faithful inline channel
      (`ErrorOr<reply>` tag=value + nested inline error, `types.MarshalErrorOrInlineError`), the way
      real FDB delivers a read wrong-shard. (RFC-115 §6 had already fixed the `Optional<Error>`
      marshal — the "generated writer mis-marshals" caveat above was stale.)
    - **`PendingGet.Resolve` flush-error arm** — a `Close()`d real conn → `Flush()` returns
      `errConnClosed` deterministically (`TestPipelinedGet_Resolve_FlushErrorRetries`).
    - **Range wrong-shard mid-scan (`more=true`), fwd+rev** — `flipMoreReply` forces a continuation,
      1001 injected on the continuation frame; asserts no dup/drop (`TestSimRangeWrongShardMidScan`).
    - **`future_version` (1009) / `process_behind` (1037) → QueueModel backoff** — inline 1009/1037
      on a read advances `failedUntil` + grows `futureVersionBackoff`
      (`TestSimInlineFutureVersion_QueueModelBackoff`; single-SS asserts QueueModel state, the cause).

---

## Test infra (low priority)

- [ ] **Parallelize the whole `//conformance` suite via stdlib `t.Parallel` (drop Ginkgo). [LOW PRIO — RFC-082 follow-up]**

  **Goal.** Cut the Go↔Java conformance suite wall time (~122s today) by running *every* cross-engine
  check concurrently, uniformly — no bespoke fan-out. Today only the two SQL loops are parallel
  (each via its own hand-rolled goroutine pool); the ~40 FDB conformance families run serially.

  **Hard constraint: bazel-only.** CI is `bazelisk test //...`, which runs each `go_test` binary
  **once, directly** (serial invocation). So the only available parallelism is **in-process**.
  Ginkgo cannot parallelize in-process — its only parallel mode is the `ginkgo --procs=N` CLI, which
  spawns N worker *processes* (each would spin its own FDB container → the 290-failure resource
  exhaustion already observed) and runs **outside** `bazel test` (loses result caching + the Java
  server's bazel runfiles). Therefore the suite must move **off Ginkgo onto stdlib `testing` +
  `t.Parallel()`**, run with `-test.parallel=N` (bazel `go_test` honors this in-process, cached,
  runfiles intact). This also finally aligns the suite with the house rule ("All tests MUST call
  `t.Parallel()`") — it's the lone serial holdout.

  **Measured profile (121.6s wall, 112s in specs; `ginkgo-report.json` from a `--nocache_test_results`
  run):** container+DB startup ~10s (serial floor); `RunSql Harness` (SeedRunCorpus, ~1620 entries)
  36s — **already** 8-Java-server parallel; `yamsql A3` (859 specs) 20s — **already** 8-server
  parallel; **~40 FDB conformance families ≈ 56s — SERIAL, on the single global Java server.**

  **The load-bearing finding — the ceiling is JVM count, not Go concurrency.** The suite is
  Java-JVM-throughput-bound and JVM count is **memory-capped on CI** (16 JVMs is exactly what caused
  the earlier conformance CI timeout; 8 is the safe ceiling). The SQL work already runs 8-way — that
  56s combined is `total_java_work / 8_servers`; unifying the two pools into one does **not** speed it
  up (same work, same servers). So the **SQL floor is ~56s @ 8 JVMs**, and the rewrite's real win is
  folding the **56s serial FDB tail** (currently on *one* server, sequential) **into** that parallel
  window → **~122s → ~70-75s (~1.7x) @ 8 JVMs**. Beating ~70s needs **more JVMs** (memory), not more
  parallelism. "Everything is parallelizable" is true mechanically, but does not buy 8x here.

  **Approach (incremental, safe).** stdlib `Test*` funcs coexist with Ginkgo's `TestConformance` in
  one package (they share globals; Go runs the sequential Ginkgo blob first, then the `t.Parallel`
  batch together) — so migrate **family-by-family** with a green + spec/assertion-**count-parity**
  gate after each (silent coverage drops are the exact CLAUDE.md failure mode). Steps: (1) move
  container + Go DB + a pool of N Java servers into `TestMain` (all servers spawned before any test →
  preserves the "no JVM spawn during a query" GRV-lag discipline); Gomega assertions stay verbatim via
  `g := NewWithT(t)`; `BeforeEach` → a setup helper; nested `Describe` → flat test names / `t.Run`
  subtests. (2) Convert each FDB family (already UUID-tenant-isolated → inherently parallel-safe).
  (3) Convert A3 + SeedRunCorpus to `t.Run(..., t.Parallel())` subtests and **delete** the hand-rolled
  worker pools + `precomputed` map + `results[]` — this is the "stop special-casing A3" cleanup.
  (4) `-test.parallel=N` via the `go_test` `args`. Keep the FDB-1020 conflict-retry (shared catalog).
  Benchmark stays gated (`CONFORMANCE_RUN_BENCHMARK`). Query-engine-adjacent → needs Graefe +
  Torvalds + @claude + codex.

  **Cheaper alternative (no rewrite, ~zero risk, ~1.3x):** just raise the existing SQL pool 8→12
  (`CONFORMANCE_A3_POOL_SIZE` / `CONFORMANCE_SEED_PARALLELISM`) if the CI runner's memory allows —
  shaves the SQL floor without touching the green, reviewed suite. The FDB tail stays serial.

  **Why low prio.** The suite is green and freshly reviewed; ~1.7x for a ~32k-line mechanical rewrite
  of wire-compat-critical tests is a weak risk/reward, and the real speed lever (JVM count) is
  memory-bound regardless. Do the cheap JVM-count bump first if speed is ever urgent.

## Exploration: a second, FDB-native vector index (Go-only — NOT Java parity)

- [x] **Explore an FDB-native ANN index for a high-latency networked KV store — REALIZED by SPFresh (RFC-094).**
  *Status: the headline question ("build an FDB-native ANN index for this substrate, and which?") is
  answered — **SPFresh**, the top candidate below, is built, shipped, and SQL-exposed; the authoritative
  tracker is `rfcs/094-spfresh-status.md`. The OTHER candidates below (DiskANN/Vamana, batched beam
  search, atomic-append build) remain **parked alternatives/additions**, NOT blocked-on or
  needed-by SPFresh — future ideas on file, not open SPFresh work.* This is a deliberate Go-only extension, NOT a Java-parity item —
  Java has no such index, so it is allowed under "query reach may exceed Java" **only if** it ships as
  a separate index type with deep test coverage. **Wire-format tradeoff (must be stated up front):** a
  new on-disk graph/posting-list layout is *wire format*; Java's `VectorIndexMaintainer` cannot
  read/write it, so this index is **Go-built and Go-read only** — it forfeits cross-engine sharing for
  that index. That is the cost of admission, not a free lunch.

  **Motivation.** The existing HNSW index is now **100% Java-faithful** (the Go-only cross-transaction
  `sharedNodeCache` was removed for compliance — see `hnsw.go`). Being faithful, it inherits Java's
  latency profile on FDB: classic HNSW assumes O(1) RAM and does 50–200 *sequential, data-dependent*
  pointer-chasing reads per op; on FDB every hop is a ~0.3–0.5 ms round-trip, so search/build are
  round-trip-bound (block profile: `Transact` ~35% + `Commit` ~24% of build time; `fdbserver` <1 core;
  client ~7/24 cores). Java hides this with async `CompletableFuture` fan-out; Go's synchronous client
  cannot. The honest fix is not more caching bolted onto HNSW — it's an index whose *algorithm* fits a
  networked KV store.

  **Candidates (ranked by fit / payoff):**
  - **SPFresh** — *in-place incremental update for disk-based ANN* (LIRE/centroid-partitioned posting
    lists + lightweight rebalancing). Most interesting for THIS substrate: it directly targets the
    build/freshness + concurrent-writer problem we hit (the single-writer lock + FDB-1020 conflict
    storm on shared graph nodes). Posting-list partitions map cleanly onto FDB subspaces; updates are
    local to a partition → far less cross-writer contention than HNSW's shared adjacency mutation.
  - **DiskANN / Vamana** — single flat graph, higher degree + long-range edges → a search touches
    *fewer* nodes with *more* neighbors each, amortizing per-read latency. Pairs with PQ/**RaBitQ
    (already in-tree, `pkg/rabitq`)** for in-memory distance, fetching full vectors only for finalists.
  - **SPANN** — cluster + posting-list; turns the random-access graph walk into a few large
    `GetRange` reads (one round-trip for many keys — exactly what FDB is good at). Recall/locality
    tradeoff vs a navigable graph.
  - **Batched beam search** — *not a new index*: keep HNSW but expand the whole `ef` frontier in one
    batched multi-get per round instead of node-at-a-time, collapsing N sequential hops into log-depth
    batched rounds. **Wire-neutral** (no format change) → the cheapest real query-latency win and a
    good first step regardless of which index above we pick. Could even land on the existing HNSW.
  - **FDB-native build primitive — atomic-append neighbor lists.** If adding an edge is an FDB atomic
    mutation (no read-modify-write), writers don't register a read-conflict range on the neighbor →
    no 1020 storm → concurrent multi-writer build becomes correct *and* fast without the single-writer
    lock. Applicable to HNSW or a new index.

  **Outcome:** SPFresh was chosen, prototyped, and shipped (RFC-094) — that step is **done**. The one
  genuinely-still-open, wire-neutral idea from the candidates above is **batched beam search** on the
  existing HNSW (collapse N sequential hops into batched rounds — the cheapest query-latency win, no
  format change); DiskANN/Vamana and the atomic-append build primitive remain unscoped parked
  alternatives. None is open SPFresh work.

- [x] **fdbgo/wire: `TestPrecomputeSize_GetReadVersionRequest` never runs in CI and fails when run.**
  — DONE (RFC-095, wire ground-truth net repair). The hand test was stale (it omitted the 8-byte
  fake-root object C++ `save_helper` allocates) — deleted; the production serializer is pinned
  byte-exactly instead. The repair went much further than the original item; the net was dead on
  every axis and, once running, caught **three real bugs**: (1) generated marshal omitted the
  RelativeOffset for EMPTY vector-of-struct fields where C++ writes the shared-empty offset
  (`flat_buffers.h:964` unconditional write) — Go commit-request bytes diverged from libfdb_c;
  (2) `parseSplitRangeReply` decoded ZERO split points from every real reply (splitPoints is a
  FlatBuffers offset-vector, not an inline blob) — production `GetRangeSplitPoints` never worked,
  the e2e tolerated empty; (3) `parseCommitReply` read a conflict-shaped
  `CommitID{version: invalidVersion}` as a SUCCESSFUL commit (C++ throws not_committed,
  `NativeAPI.actor.cpp:6726`; latent — proxy only sends that shape under report_conflicting_keys).
  (`parseWaitMetricsReply`'s envelope-`UnmarshalFDB` was originally claimed as a 4th bug; Torvalds'
  mutation probe disproved it — correct by layout, ErrorOr's value offset coincides with FakeRoot's
  field 0; the rewrite to the canonical `ReadErrorOrInto` walk stands as hygiene only.)
  Also: extractor pins reply-promise tokens (deterministic vectors), emits reply-direction vectors
  for all 9 reply types the client parses (field-value asserted against the PRODUCTION parsers in
  `client/reply_ground_truth_test.go`), generator now reproduces the hand-fixes that lived in
  DO-NOT-EDIT files (KeyRangeRef swap-inversion, OOM cap), bazel data deps added + every skip in
  the net is now a Fatalf, orphan `wire/conformance_test.go` + dead justfile recipes deleted.

## SPFresh — tracked in RFC-094 (status)

All SPFresh tracking — current state, shipped work, open items, frozen
performance, and measured-negative levers — is consolidated in the authoritative
tracker **`rfcs/094-spfresh-status.md`**. The former "multi-tenant scale-out" and
"recall at scale" sections (every item closed) moved there; the SQL surface is
Phase 9 above (shipped).

Open work (detail + file:line in the RFC):
- **Tier 1:** SPFresh has no chaos/model-based fault coverage — the whole
  lifecycle incl. RFC-104 refinement is untested under injected faults and
  refiner-vs-rebalancer concurrency (highest-value gap); refresh
  `SPFRESH_OPERATIONS.md` for the refinement loop (stale wrt RFC-104).
- **Tier 2:** changelog chunking for >~267M-vector single-store builds
  (`spfresh_build.go:120`); a reference maintenance worker looping sweep+refine on
  a cadence (today they're library entry points a deployment must wire).
- **SQL nice-to-haves:** yamsql vector port, `ef_search` FDB behavioral test,
  OR-of-two-KNN execution test, window-in-`WHERE` `42F21` rejection.
