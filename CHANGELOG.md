# Changelog

All notable changes to `fdb-record-layer-go` are recorded here. Format:
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versioning per `RELEASE.md`
(pre-1.0 `v0.MINOR.PATCH`).

**This project is pre-1.0.** The **Go API may change across minor versions**; the **FDB wire
format must stay compatible with each release's declared Java `fdb-record-layer-core` target** (the
shared-cluster hard line — see `RELEASE.md`). Every entry's **Compatibility** block answers the four
questions a user upgrading between two refs needs: wire format, SQL behaviour, FDB client option
semantics, and required dependency versions.

This changelog starts **2026-06-20**; earlier history is in `git log`. The first tagged release is
**v0.1.0** (2026-08-26). v0.1.0 shipped the `frl` CLI from a parallel nested-module tag,
`cmd/frl/v0.1.0`; from the next release `frl` is a package of the root module and ships under the
project's own `vX.Y.Z` tag, which `go install fdb.dev/cmd/frl@vX.Y.Z` resolves (`RELEASE.md`).

## [Unreleased]

### Compatibility
- **Wire format:** the Java target is upgraded to `fdb-record-layer-core` 4.14.2.0 (RFC-257, in
  progress). Records Java writes through `TransformedRecordSerializer` (compressed, encrypted) are
  read and written. Stores written only by an earlier pre-release Go build are not supported:
  recreate them.
- **SQL behaviour:** follows Java 4.14.2.0 where both engines run a query; see the PR for the
  per-change list.
- **FDB client option semantics:** unchanged since v0.1.0; the honored / `UnsupportedOptionError` /
  safe-no-op classification in `pkg/fdbgo/fdb/OPTIONS.md` still holds against `libfdb_c` 7.3.77.
- **Required versions:** Java `fdb-record-layer-core` **4.14.2.0**, FDB C++ client **7.3.77**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).

### Changed
- Fixed-factor union planning uses fewer temporary allocations in boolean normalization and memo matching.
- A recursive CTE's column list names its columns only for the query that reads the CTE; the recursive branch reads the seed's own names, as in Java 4.14.2.0.
- A recursive CTE keeps the seed's column types and nullability for every iteration, as in Java 4.14.2.0; a recursive row that does not fit (a NULL into a NOT NULL column, another type) is refused with XXXXX.
- SQL accepts EXISTS inside AND/OR/NOT boolean expressions in WHERE and INNER JOIN ON, matching Java's one-row existential witness semantics.
- `frl` is a package of the root module and releases under the project's `vX.Y.Z` tag.
- SQL `LIKE` follows Java 4.14.2.0: wildcards cross newlines, `LIKE NULL` is allowed, and invalid escapes raise 22019/2200B/22025 per row.
- SQL comments follow Java 4.14.2.0: block comments nest, an unterminated one is 42601, and `#` is no longer a comment.
- `OPTIONS (...)` is statement-level only and adds `PLAN RIGHT DEEP` and `ISOLATION LEVEL SNAPSHOT`; `PLAN` and `DEEP` are reserved words.
- A typed, parenthesised or column NULL IN-list item is 0A000 when the list is evaluated, as in Java 4.14.2.0 (a bare NULL stays 42809).
- An ARRAY element is never NULL: a NULL array element is refused with 0A000 as in Java 4.14.2.0 (literal-array `=`/`<>`/`IS DISTINCT FROM` comparisons still accept one).
- Driver parameters are bound as typed constants (int32 INT, int64 LONG, slices ARRAY, uuid.UUID UUID), not spliced into SQL text; named `?x`/`$x` and `IN ?` are supported, extra arguments are ignored and a missing one is 42F02.
- `COALESCE`/`GREATEST`/`LEAST` follow Java 4.14.2.0: at least two arguments, all-NULL and BYTES arguments are 22F00, `COALESCE` evaluates every argument, and results are NOT NULL when Java's are.
- The planner removes duplicate expressions from its memo (Cascades duplicate detection): multi-way joins plan with far fewer tasks, and a six-table join chain or a hub joined to five spokes now plans within the default budget.
- A join of two EXISTS subqueries no longer repeats rows: each existential contributes at most one witness row.
- A lateral-unnest chain answers a WHERE reading any link's element, a WHERE [NOT] EXISTS over the chain and a nested EXISTS reading its last element; an EXISTS beside an unnest of a STRUCT array may read the unnested table (all previously 0AF00).
- A FROM item reading a lateral-unnest chain's element, a chain link separated from its owner by another FROM item, unnests of several tables in one FROM, and an unnest of a lateral derived table's array column now plan (previously 0AF00); source reordering preserves indirect lateral dependencies, including inside correlated subqueries.
- An `AT` unnest's element or ordinal read beside another FROM item no longer fails at execution.
- Multi-table `[NOT] EXISTS` subqueries can read an outer unnest's element and ordinal (previously 0AF00).
- Join planning costs a right-deep foreign-key chain as its left-deep re-association, and splits a range predicate spanning several joined tables per table so each keeps its selective probe.
- A record-layer failure no SQL mapping claims is SQLSTATE XXXXX, as Java's `ExceptionUtil` maps an unclaimed `RecordCoreException` (previously no SQLSTATE).
- `ISOLATION LEVEL SNAPSHOT` admits only SELECT and EXPLAIN/DESCRIBE of a SELECT; DDL, SHOW and DML are 0A000, and a DML plan is refused at SNAPSHOT before it reads, as in Java 4.14.2.0.
- An ungrouped query's COUNT is 0 over no rows and NOT NULL (Java's `adjustCountOnEmpty`), in the select list and HAVING; a HAVING-only aggregate query may project constants (was 42703).
- A table whose quoted lowercase primary-key or vector column names differ from their upper-case spelling gets primary-key scans and vector index plans (previously a full scan, or 0AF00).
- A vector query without `OPTIONS EF_SEARCH` searches with Java's default, min(max(4k, 64), max(k, 400)), instead of 200.
- GuardiANN vector indexes refuse malformed stored values instead of panicking, check the deferred insert cap before writing anything, poison the transaction when a task fails after its removal, and merge or drain every requested index even when an earlier one fails.
- Decimal literals parse as Java's `ParseHelpers.parseDecimal` wherever they stand: a dotless exponent (`1e5`) or an out-of-width integer is XXXXX `For input string: "…"`, and an overflowing `1.0e400` is an infinity (previously 22003 or 0AF00).
- A record constructor's field takes its element's own name (`SELECT (x, y)` is `{X, Y}`, `(x + 1)` is `{_0}`, a repeated name falls back to the position), so a derived record's fields can be read by name; INSERT … SELECT, UPDATE and COALESCE bind a record to its struct type by position, ignoring element names, and an INSERT VALUES element naming another field is XX000, as in Java 4.14.2.0.
- Scalar macro calls take named arguments (`f(b => 1, a => 2)`), bound by name with the declared defaults; a repeated name is 42601, and an unknown name, a missing argument without a default or too many arguments is 42883 `could not find function '…'` (also for table functions), as in Java 4.14.2.0. A macro may take or return a struct type no table stores.
- A window's `OPTIONS EF_SEARCH` accepts `L`/`I` suffixes, refuses a repeat (22F00) and a value beyond int (22000), and an HNSW search uses it as given: below k it returns fewer rows, as in Java 4.14.2.0.

## [v0.1.0] - 2026-08-26

### Added
- **Record-layer metrics exporter** (`pkg/recordlayer/rlmetrics`): `StoreTimer` (the port of Java's
  `FDBStoreTimer`) rendered in the Prometheus text exposition format under `fdb_recordlayer_`, with
  zero new dependencies — the record-layer counterpart to `pkg/fdbgo/fdbmetrics`. Timed events are
  summaries in seconds, counts and byte totals are counters. `recordlayer.Event` gains a `Kind`
  (timed / count / size) porting Java's `Event`-vs-`Count(isSize)` split, and `StoreTimer` gains
  `Add` and `KeysAndValues` from Java. Reachable from the SQL layer via
  `FDBDatabase.SetTimer` or, for `database/sql`-only deployments,
  `sqldriver.EnableStoreTimer(clusterFile)` — one timer per cluster-file key (per process), with no
  per-tenant label by design; see `docs/mt-saas.md` §4 for the cardinality reasoning.

- **Multi-tenant SaaS operator guide** (`docs/mt-saas.md`): the tenancy model (one database path per
  tenant, subspace-per-database, and why native FDB tenants are not used), the trust boundary and
  `RESTRICT_DDL_TO_SESSION_DATABASE`, the five per-statement quotas and how to arm them (none is
  DSN-settable), per-tenant observability, the pure-Go TLS certificate requirement, and the
  tenant-facing SQL contract. Every claim carries a `file:line` citation; the page is on
  `pkg/docscheck`'s `livingDocs` list so its version anchors are drift-guarded.
- Statement-wide **memory byte budget** for the SQL executor: opt-in `OptMaxStatementMemoryBytes`
  bounds every cardinality-growing buffer by bytes (not just the 100k-row `MaterializationLimit`);
  breach → SQLSTATE `54F01`. Default `0` = unlimited (RFC-130).
- A documentation-consistency CI guard (`pkg/docscheck`) that fails the build if a living doc drifts
  from the `MODULE.bazel` / `go.mod` version pins or reintroduces a known contradiction (RFC-131/132).
- A public **FDB client option matrix** (`pkg/fdbgo/fdb/OPTIONS.md`) classifying every `Set*` option
  as honored / `UnsupportedOptionError` / safe no-op, each with its `libfdb_c` 7.3.77 reference, plus
  a completeness guard that fails CI if an option is added without a matrix row (RFC-133).
- A **panic-boundary release gate** (RFC-134): a `norecover` nogo analyzer fails the build if a
  `recover()` is added outside the documented panic→error boundary allowlist (`docs/panic-audit.md`
  §2), plus docscheck guards that keep the four input-boundary fuzz nets wired and the doc in lockstep
  with the allowlist. Makes the "untrusted input → error, never crash" discipline self-enforcing.

### Changed
- **FDB C++ wire-protocol baseline bumped 7.3.75 → 7.3.77** (RFC-152). Patch bump within the 7.3
  line: no wire-format, error-code, `ClientKnobs`, RYW, or serialization change (regenerated
  `pkg/fdbgo/wire/types/*_generated.go` differ only in the version-string header comment). The only
  client-relevant upstream delta is PR #12935 (a `peer->disconnect` arm in `waitValueOrSignal` so
  `loadBalance` fails an in-flight request immediately on peer disconnect instead of waiting out the
  failure-monitor lag); the pure-Go client already has this structurally (single-owner connection:
  `readLoop` EOF → `failConnection` → `failAllPending`), pinned by
  `TestPeerDisconnect_FailsInFlightReplyImmediately`. No production-code behaviour change.
- SQL `LIMIT`/`OFFSET` now flows through a single uniform `RecordQueryLimitPlan` + continuation
  envelope, including for nested derived tables (RFC-128).

### Fixed
- **Record-layer instrumentation was measuring the wrong things, or nothing at all.**
  `Counter.Increment` wrote its amount into the cumulative-value field as well as the count, so every
  count event reported a bogus duration (Java's `Counter.increment` touches `count` alone). The scan
  events timed cursor *construction* rather than each record produced, which for a lazy cursor
  measures an allocation and reports one occurrence per scan regardless of size (Java instruments the
  cursor's `onNext`). Scan events also sat on entry points the query engine does not use, leaving the
  executor's record scans, aggregate/group index scans, vector scans and primary index scans
  uncounted. And `EventCommit` was recorded only by `CommitWithVersionstamp`, so every commit on the
  SQL path — autocommit and explicit `BeginTx` alike — went unrecorded. Instrumentation only; no
  wire-format, plan or result change.
- **Legacy Java store layouts are now fully readable, writable, and auto-upgraded** — closes the
  `FormatVersion` < 6 / `omit_unsplit_record_suffix` wire-compatibility gap that was previously a
  *silent* data-correctness bug (Go accepted an old-format store header but only understood the modern
  inline layout, so it would silently fail to see a legacy store's record versions and unsplit
  records). Go now mirrors Java's `FDBRecordStore.useOldVersionFormat()` end-to-end:
  record versions are read/written in the separate `RecordVersionKey(8)` subspace for stores below
  `SAVE_VERSION_WITH_RECORD` (format 6), and unsplit records are read/written at the bare primary key
  (no `0` suffix) when `omit_unsplit_record_suffix` is set — across load, scan, `scanRecordKeys`,
  `recordExists`, save, update, delete, and `deleteRecordsWhere`. On open, Go also performs Java's
  transactional format upgrade (`checkRebuild`/`addConvertRecordVersions`): it bumps the stored
  `FormatVersion`, sets `omit_unsplit_record_suffix` for a non-splitting store created before format 5,
  and moves versions from subspace 8 to their inline `pk + -1` location when upgrading a splitting
  store past format 6. Pinned by FDB integration tests that lay down each legacy layout and assert
  byte-level read/write/scan/delete and migration parity with Java. (Closes the `TODO.md` "no read
  path for format-version-<6 record versions / unsplit records" gap surfaced by the RFC-131 audit.)
- SQL pagination no longer treats a non-terminal `StartContinuation` as end-of-results (a latent
  early-truncation bug); exhaustion is decided off `IsEnd()`, not byte-emptiness (RFC-127).
- Pure-Go FDB client: `Get`/`GetRange` read-conflict ranges are clamped to the data actually returned
  and filtered through the RYW overlay, matching `libfdb_c` (no under-conflict; RFC-121).
- `go test ./...` is clean from a fresh checkout: the Bazel-runfiles-only suites are build-tagged so a
  plain `go test` no longer panics (RFC-129), and the heavy million-row stress benchmarks under
  `pkg/relational/sqldriver/stress` now carry a `stress` build tag, so a plain `go test ./...` skips
  them instead of spinning up million/ten-million-row FDB workloads (they still run via Bazel's
  `manual` stress target and the nightly stress workflow).
- Pure-Go FDB client: three **database-level transaction defaults** that change read semantics are now
  honored instead of silently dropped — `SetSnapshotRywDisable`/`Enable` (a cumulative counter,
  matching `libfdb_c`), `SetTransactionBypassUnreadable`, and `SetTransactionCausalReadRisky`. They
  propagate to each new transaction via `applyTxDefaults` and are replayed idempotently across retries
  (RFC-133).

### Compatibility
- **Wire format:** unchanged for modern stores — records, indexes, versions, continuations, and split
  records remain byte-identical to Java `fdb-record-layer-core` 4.12.11.0. **Newly closed gap:** Go now
  reads *and* writes legacy Java store layouts (`FormatVersion` < 6 record versions in the
  `RecordVersionKey(8)` subspace, and `omit_unsplit_record_suffix` bare-key unsplit records) and
  performs Java's on-open format upgrade — previously a silent read gap (see Fixed). A Go client can
  now safely share a cluster with legacy Java stores in either direction.
- **SQL behaviour:** net additions only (memory-budget option; the LIMIT-envelope and pagination
  fixes correct latent bugs, they don't change correct-query results).
- **FDB client option semantics:** now documented option-by-option in `pkg/fdbgo/fdb/OPTIONS.md`
  (honored / `UnsupportedOptionError` / safe no-op, vs `libfdb_c` 7.3.77). **One behavioural change:**
  three database-level defaults (`snapshot_ryw_disable`/`enable`, `transaction_bypass_unreadable`,
  `transaction_causal_read_risky`) that were previously silent no-ops now take effect on each new
  transaction — a caller that set them and relied on them being ignored will now see them applied
  (this is the faithful `libfdb_c` behaviour). The unsafe access/auth/quota family still fails loud
  with `UnsupportedOptionError`; no option's *wire* behaviour changed (RFC-133).
- **Required versions:** Java `fdb-record-layer-core` **4.12.11.0**, FDB C++ client **7.3.77**, Go
  **1.26.x** (the `MODULE.bazel` / `go.mod` pins; the CI doc-guard enforces docs match them).
