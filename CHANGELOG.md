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
`cmd/frl/v0.1.0`; from v0.2.0 `frl` is a package of the root module and ships under the
project's own `vX.Y.Z` tag, which `go install fdb.dev/cmd/frl@vX.Y.Z` resolves (`RELEASE.md`).

## [Unreleased]

### Breaking changes
Each is described in full under Compatibility or Changed below.
- **Pure-Go client retry observers:** retry log handlers receive the caller's context;
  transaction cancellation is reported via `fdb_error_code`, not `context.Cause`.
  This changes OnError/public-operation observers: retained contexts and their children
  are no longer canceled by operation cleanup or transaction reset/timeout. Caller
  values, deadline, `Err` and `Cause` are unchanged. Native-fence logs retain their
  existing worker completion/Reset lifetime. `fdb_error_code` remains the actual retry
  input; no cancellation events are synthesized, and nonretryable 1025/1031 do not
  imply that every cancellation is logged.
- **SQL storage:** the relational key space is Java's `RelationalKeyspaceProvider` layout; SQL data
  written by v0.1.0 is not migrated (`docs/upgrade.md`). A database path is `/DOMAIN/DATABASE`, and
  its domain must be registered once per process (`sqldriver.RegisterDomainIfNotExists`).
- **Collated indexes** fail with `GoOnlyCollationError` unless the store opts into Go-only bytes
  with `StoreBuilder.SetGoOnlyCollation(true)` (#790).
- **VECTOR indexes** read value column 0 as serialized vector bytes (#815), are refused at build
  without a dimension count or with a structure Java refuses (#809), and accept only Java's metric
  names and options (#804).
- **TEXT indexes:** the default tokenizer handles a word-final capital sigma and supplementary-plane
  text as Java does; rebuild Go-written TEXT indexes over such text (#803).
- **Pure-Go client TLS** (`:tls` cluster files with `FDB_TLS_*`, RFC-277): verification follows
  libfdb_c 7.3.77. The CA file is the only trust root (no CA file trusts nothing; the system
  roots never count), and no host name is checked. A set but empty path variable means no file
  and no default. A bad setting fails the open with `*client.TLSConfigError` (`tls_error`, 2107).
- **Pure-Go client:** API versions outside 510-730 are refused with 2203 (#823).
  `SetLockAware`/`SetReadLockAware` take no argument, a read-lock-aware commit with writes fails
  with 2023 unless the transaction is also lock-aware, and `SetSizeLimit` outside
  [32, 10,000,000] is a deferred error (#820). `SetTimeout` outside [0, INT_MAX] is a deferred
  2006 (#799).
- **Record layer (RFC-275):**
  - `TransactionSizeWarningError` is removed. Past `TransactionSizeWarnBytes`, saves and deletes
    succeed, and the warning is logged and counted as `CountTransactionSizeWarning`.
  - A corrupt store header is a `RecordCoreStorageError` ("Error reading version") that unwraps to
    the parse error.
  - A TEXT index whose text column is not a string fails the save with `ClassCastError`, as Java
    does.
  - A BY_RANK bound that is not a number also fails with `ClassCastError`.
  - Primary-key expressions declaring version columns are refused with `MetaDataError`
    ("Version in primary key not supported"), as Java does. The chosen record version is
    visible to function-wrapped primary keys and maintained-count keys; an
    incomplete value in their ordinary tuple encoders returns `IncompleteVersionstampError`.
    Dry runs share primary-key preparation but do not maintain counts. Count-key checks run
    only when a count is maintained.
- **`keyspace` (RFC-275):**
  - `AddSubdirectory` and `AddSubdirectories` return an `*ArgumentError` under Java's sibling
    rules, instead of panicking on a duplicate name.
  - `ResolverFunc`, `KeySpace.Path` and `Path.Add` take a `context.Context`.
  - `PathFromTuple` fails on an element that no subdirectory of a non-leaf directory holds.
  - `FindChildForValue` matches a stored value: a resolver directory is found by its `int64`,
    not by its string name.
- **Continuations:** restart paged queries across this release; plan changes and cursor
  encoding corrections do not preserve old tokens. See `docs/upgrade.md`.
- **Record layer:** `FDBDatabase.Run` and its variants make at most 10 attempts by default, and a
  body that commits the context it was handed is refused. A typed record store reads and writes
  only its record type's union field, `NewTypedRecordStore` takes `baseStore` and `recordType` and returns an error, and every
  save resolves the record type by the record's descriptor (#842, #852).
  `FindRegisteredMessageType` is replaced by `GeneratedMessageType` (#842).
- **SQL driver:** an unknown DSN parameter, or a DSN boolean other than `true`/`false`, fails at
  open with 22023 (#799).
- **SQL driver transactions (RFC-272):** `TxOptions{ReadOnly: true}` is refused with 0A000 (it
  was ignored). Inside an explicit transaction a statement whose context ends (its deadline, a
  cancel, the statement timeout) cancels the FDB transaction: the statement returns the context's
  error and the transaction is over, so later statements and `Commit` return 25F01. The
  `BeginTx` context now bounds the transaction (`DIVERGENCES.md`).
- **Go API:** `functions.ApplyMathOp`, `ApplyBitOp`, `IsTruthy`, `LiteralMatchesPKKind`,
  `ToIntegerArg`, `ToFloat64`, `AddInt64Checked`/`SubInt64Checked`/`MulInt64Checked` and
  `values.JavaFloatToString` are removed; nothing outside their tests called them (RFC-272).
- **Go API moves:** the gomock mocks moved to `pkg/relational/api/apimock` (#810), the demo protos
  to `fdb.dev/gen/demopb` (#842), and index-predicate normalization, the plan walkers and the
  R-tree to `pkg/recordlayer/indexpredicate`, `pkg/recordlayer/query/plan/plans` and
  `pkg/async/rtree`. The planner census left the public `values`, `cascades`, `executor` and
  `core/query` packages (#836). `keyspace.FDBResolver` is removed. The executor's
  `api.ResultSet` adapter (`RecordLayerResultSet`, `ColumnDef`, `NewResultSetMetaData`) moved
  to `pkg/relational/core/resultset`, and the executor returns `executor.ExecutionError` kinds
  (or `values` errors) where it returned SQLSTATE `api.Error`s; SQL sees the same SQLSTATE and `api.Error` message,
  without the executor's wrapping context in the error string (RFC-274).
- **frl:** `frl` is a package of the root module; retiring the nested module requires
  the post-publication retraction procedure in `RELEASE.md`.

### Compatibility
- **Version-dependent record keys (RFC-275):** native `VersionKey()` primary-key columns are
  unsupported, as in Java; formerly accepted Go-only metadata is now refused. Complete supplied
  versions participate in maintained-count keys and reach primary-key functions such as
  ordering wrappers. Incomplete values reaching ordinary key encoders are refused with
  `IncompleteVersionstampError`. Existing null-version counters and unsupported primary-key
  metadata are not migrated automatically; migrate or recreate affected pre-release stores
  before mixed-engine use. Tuple encodings and unaffected stores are unchanged.
- **Collation:** collated index access and writes that evaluate collated primary/count keys now fail with `GoOnlyCollationError` unless the store explicitly opts into Go-only bytes with `StoreBuilder.SetGoOnlyCollation(true)`; existing keys are not migrated.
- **Collation metadata:** fan-out follows the function arguments as in Java, permitting scalar collations in primary keys.
- **Wire format:** the Java target is upgraded to `fdb-record-layer-core` 4.14.2.0 (RFC-257). Records Java writes through `TransformedRecordSerializer` (compressed, encrypted) are
  read and written. Stores written only by an earlier pre-release Go build are not supported:
  recreate them.
- **SQL behaviour:** follows Java 4.14.2.0 where both engines run a query; see Changed for the
  per-change list.
- **FDB client option semantics:** the honored / `UnsupportedOptionError` / safe-no-op
  classification in `pkg/fdbgo/fdb/OPTIONS.md` still holds against `libfdb_c` 7.3.77. Changed since
  v0.1.0: `SetReadLockAware` and `SetSizeLimit` (see Changed), and `Transaction.SetTimeout` outside
  [0, INT_MAX] records a deferred 2006 (`invalid_option_value`) like libfdb_c, where a negative
  value used to disable the timeout (0 still disables it).
- **Continuation tokens:** key-value cursor tokens (index, COUNT/atomic, BITMAP_VALUE and record scans) strip Java's range common prefix (`KeyValueCursorBase.calculatePrefixLength`), so a token is valid only for the range that minted it, as in Java. Restart paged queries across this encoding correction (#871).
- **DSN booleans are strictly `true`/`false`** (any case): a bare `?flag`, `1`/`0`, `t`/`f`,
  `yes`/`no` and `on`/`off` now fail at open with 22023.
- **Required versions:** Java `fdb-record-layer-core` **4.14.2.0**, FDB C++ client **7.3.77**, Go
  **1.27.3** or newer (`go.mod` enforces this minimum).
- **Pure-Go client TLS (RFC-277):** a `:tls` cluster file with `FDB_TLS_*` now verifies the
  server as libfdb_c 7.3.77 does. Trust comes only from `FDB_TLS_CA_FILE`; with no CA file no
  server is trusted, where the system roots used to be. The host name is no longer checked, so
  a server certificate needs no IP SANs. `FDB_TLS_VERIFY_PEERS` (default `Check.Valid=1`)
  decides which peers to accept, and `FDB_TLS_PASSWORD` decrypts a PBES2 PKCS#8 key. A path
  variable set but empty means no file and no default. A setting that cannot be used (a file,
  a password, a verify-peers string) fails the open with `*client.TLSConfigError`, which reads
  as `tls_error` (2107). `WithTLSConfig` configs keep Go's own verification.
- **Go 1.27 runtime defaults** apply to applications built with this module, because their
  `go` line must be at least 1.27.3:
  - `x509sslcertoverrideplatform=1`: on macOS and Windows, a nonempty `SSL_CERT_FILE` or
    `SSL_CERT_DIR` replaces the platform certificate verifier. Pure-Go client TLS connections
    through a `WithTLSConfig` config with nil `RootCAs` are affected; the `FDB_TLS_*` path
    never reads system roots (RFC-277). Add `godebug x509sslcertoverrideplatform=0`
    to the application's `go.mod` to keep the previous behavior.
  - `tracebacklabels=1`: tracebacks include goroutine labels.
  - Legacy GODEBUG values removed in Go 1.27 abort program startup if set.

### Added
- Record Layer explicit-version saves: `SaveRecordWithVersion`, `OverrideLockSaveRecordWithVersion`
  and the typed store's `SaveRecordWithVersion`, with Java's `VersionstampSaveBehavior` (RFC-275).
- The pure-Go client reads `FDB_TLS_VERIFY_PEERS` (libfdb_c's verify-peers grammar,
  `transport.ParseVerifyPeers`) and `FDB_TLS_PASSWORD` (encrypted PBES2 PKCS#8 keys) (RFC-277).
- Pure-Go `fdb.MappedRangeReader` capability with byte-driven batch iteration for exact primary ranges; the libfdb_c backend does not expose it.
- Record Layer remote-fetch index scans: `ScanIndexRecordsWithFetchMethod` with `USE_REMOTE_FETCH` and `USE_REMOTE_FETCH_WITH_FALLBACK`, `ScanIndexRemoteFetch` and `BuildSingleRecord`, with Java's `scan_remote_fetch_entry` event and `remote_fetch` count.
- The SQL driver DSN sets `max_rows`, `execution_scanned_rows_limit`, `execution_scanned_bytes_limit`,
  `execution_time_limit`, `max_statement_memory_bytes`, `transaction_timeout`, and the planner knobs
  `plan_right_deep`, `disable_planner_rewriting` and `disabled_planner_rules`. The transaction timeout
  and tags now apply to every statement transaction, autocommit and read-only ones included.

### Changed
- Record layer (RFC-275), in full:
  - Native version columns in primary keys are refused at metadata construction and load,
    as in Java. The chosen version is visible to function-wrapped primary keys and
    maintained-count expressions. Incomplete values in ordinary key encoders return
    `IncompleteVersionstampError`; dry runs share primary-key preparation and return the
    chosen version without count maintenance.
  - `CheckTransactionSize` returns only `TransactionSizeExceededError`. The Go-only warning
    threshold logs at WARN and counts `CountTransactionSizeWarning` once per transaction.
  - Header parse failures are `RecordCoreStorageError{Message: "Error reading version", Cause}`.
  - The TEXT maintainer reads its text column as Java's `Tuple.getString` does. A non-string is a
    `ClassCastError` and a position past the key an `IndexOutOfBoundsError`; null and `""` are
    still skipped.
  - BY_RANK bounds, SCORE_FOR_RANK and SCORE_FOR_TIME_WINDOW_RANK ranks and the arguments of the
    long-arithmetic key functions are taken with Java's `Number.longValue()`. Every Go integer
    carrier is accepted, with `uint64`/`*big.Int` keeping their low 64 bits, and floats are
    truncated with NaN as 0. A float bound used to be an unbounded endpoint and now selects a
    rank; a bound that is not a number is a `ClassCastError`, and a null rank answers null.
- `keyspace` (RFC-275), in full:
  - `AddSubdirectory`/`AddSubdirectories(...) error` refuse:
    - a duplicate name;
    - a same-type sibling unless both are distinct constants (a NULL directory counts as a
      constant);
    - an equal constant (bytes by content, floats as `Double.equals`).
  - `PathFromTuple` and `FindChildForValue` match a stored value in declaration order. An element
    no subdirectory of a non-leaf directory holds, or any element under a root without
    subdirectories, is an `*ArgumentError`.
  - `ResolverFunc`, `KeySpace.Path` and `Path.Add` take the caller's context.
- SQL driver transactions (RFC-272): `BeginTx` with `TxOptions{ReadOnly: true}` fails with 0A000
  (Java accepts `setReadOnly` and ignores it; database/sql requires a driver that does not enforce
  it to refuse it). The `BeginTx` context bounds the transaction: when it ends, the FDB transaction
  is cancelled and database/sql rolls it back. A statement whose context ends inside an explicit
  transaction (deadline, cancel, statement timeout, also while its rows are open) returns the
  context error and ends the transaction; later statements and `Commit` return 25F01 wrapping the
  context error, and its writes are discarded. `Commit` on an ended transaction is 25F01.
  `DIVERGENCES.md` "Driver transactions" has the comparison with Java. `functions.ApplyMathOp`,
  `ApplyBitOp`, `IsTruthy`, `LiteralMatchesPKKind`, `ToIntegerArg`, `ToFloat64` and
  `Add/Sub/MulInt64Checked` and `values.JavaFloatToString` are removed.
- Development builds use Bazel 9.3.1, Bazelisk 1.30.0, and updated stable BCR build rules.
- Require Go 1.27 (minimum toolchain Go 1.27.3).
- Go 1.27.3 includes the GO-2026-6809 `crypto/x509` fix used by pure-Go client TLS.
- Staticcheck 2026.2 retires SA5011 (upstream `5161aaa18`); nogo keeps x/tools `nilness`, which checks proven-nil uses but does not replace SA5011's dereference-then-later-nil-check coverage. Its upstream replacement is tracked in TODO.md.
- Remote-fetch index scans avoid per-row raw/pending wrappers and read-only local-version key repacking. A fixed 1,000-row benchmark reduces remote-fetch median allocations from 35,988 to 21,915 per scan (39.10%) and bytes from 3,381,658 to 2,640,724 (21.91%); returned records and entries remain independently owned. See [RFC-258 PR4 measurements](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/rfc-258-pr4-allocations.md) for scope and reproduction; unquiet timings are informational only.
- Record scans follow Java's scanTypedRecords: sizes include inline versions, every key-value is charged, scan, byte and time limits finish the record that crosses them, and `FDBStoredRecord.VersionedInline` is reported. A page ending exactly at its row limit now stops with `ReturnLimitReached` and a resumable token, so the next page is empty. `CountRecords` follows Java's countRecords: it ignores `ReturnedRowLimit` and `TimeLimit`, and on unsplit stores with inline versions counts key-values (about twice the records).
- SQL pages under `EXECUTION_SCANNED_ROWS_LIMIT`, `EXECUTION_SCANNED_BYTES_LIMIT` or a time limit that end in a table scan now finish the record that crosses the limit, so a page returns one row more than a single-key-value budget. `ExecutionStats.RecordsScanned` counts a table scan's key-values, so a table with `STORE_ROW_VERSIONS` charges two per row.
- The optional pure-Go `MappedRangeSkipReader` resolves positive Skip client-side without integer wrapping or mapping skipped records; mapped requests use plain selectors. Snapshot and CGo facade transactions do not expose this capability.
- Index scans (ordinary, COUNT/SUM, MIN/MAX_EVER_TUPLE, MAX_EVER_VERSION and BITMAP_VALUE) follow Java's row-limit tokens, scan-attempt counters and transaction-anchored time limits.
- Ordinary, COUNT/SUM, MIN/MAX_EVER_TUPLE, MAX_EVER_VERSION and BITMAP_VALUE index scans apply `ExecuteProperties.Skip` as Java's selector offsets (it was ignored); a skip saturates at `MaxInt32-1` instead of wrapping as Java's forward `MaxInt32` does.
- Issued record loads charge scan bytes when the raw read completes, including loads discarded by a failed index cursor. `ScanLimiterState.WaitForIssuedRecordLoadCharges` waits for those existing charges without issuing or materializing another record.
- SQL `INDEX_FETCH_METHOD`, when set, is stamped on every BY_VALUE index plan and selects remote fetch, as in Java; unset, Go keeps SCAN_AND_FETCH where Java defaults to USE_REMOTE_FETCH_WITH_FALLBACK.
- `cascades.MatchCandidate.ToScanPlan` takes the `PlanContext`, as Java's `toEquivalentPlan` does; new `api.Options.IsSet`, `FDBRecordContext.APIVersion` and `recordlayer.SupportsRemoteFetch`.
- `ScanIndex` and `ScanIndexByType` reject any other scan type on BITMAP_VALUE and TEXT indexes with Java's `RecordCoreException` messages (`ScanIndexByType` used to scan them), and report unsupported scan types as `*RecordCoreError`.
- Update Go dependencies for archive containment, bounded group parsing, CEL JSON field privacy, and OpenTelemetry diagnostic privacy; these SDK updates do not patch the host Docker daemon.
- `EndpointTypeContinuation` is internal; scan callers supply the original range endpoints and pass the continuation separately.
- Commit has no synthetic reply timeout: transaction Cancel/timeout end the public wait while native GRV/commit/fence work remains owned by the transaction; Reset, owner collection, and database close drop that work.
- The client handles FDB's ENDPOINT_NOT_FOUND reply as libfdb_c does: a request to a role the server no
  longer hosts fails at once as maybe-delivered instead of waiting for the RPC timeout, without closing
  the connection, and later reads, GRVs, location lookups and commits skip that endpoint.
- CI no longer runs `go test` or `go vet`: every test runs through Bazel. The GitHub-hosted
  `hosted-smoke.yml` workflow is retired; its secret scan, Bazel-anchor check and fresh-clone
  `go build ./...` moved into `ci.yml`'s `go install lane` job, and the wire-type oracle runs as
  `bazelisk test //cmd/fdb-diff-oracle:fdb-diff-oracle_test`.
- Tenant commits fence uncertain delivery in the tenant's keyspace, preventing a late commit after error 1021; all commits propagate a failed fence's error instead of returning an unfenced `commit_unknown_result`.
- **Breaking:** as in `libfdb_c`, a `SetReadLockAware` transaction fails a commit with writes with
  2023 unless `SetLockAware` is also set, and `SetSizeLimit` outside [32, 10,000,000] (0 included)
  fails every later operation with 2006, not only the commit. `client.Transaction.SetLockAware` and
  `SetReadLockAware` take no argument.
- `fdb.APIVersion` and `client.WithAPIVersion` refuse a version outside 510-730
  (`client.MinAPIVersion`-`client.MaxAPIVersion`) with `api_version_not_supported` (2203), the
  Apple Go binding's code; 730 is libfdb_c 7.3.77's maximum and 510 this client's floor. Before,
  `fdb.APIVersion` accepted 731 and above and answered 2201 below 510, and `client.WithAPIVersion`
  accepted any non-zero version. A different version after one is selected stays 2201.
- The R-tree takes any `rtreeMinimumM`, `rtreeMaximumM` and `rtreeSplitS`, as Java does; `rtree.ValidateRTreeConfig` is gone. A configuration that cannot keep the node invariants fails where Java's does (`rtree.VerifyError`, `IndexOutOfBoundsError`, `NegativeArraySizeError`, `ArithmeticError`), and a non-root node outside [minM, maxM] is refused on read (`javanum.IllegalStateError`, also `recordlayer.IllegalStateError`). An index an earlier Go build wrote under a configuration whose splits leave nodes out of that range (for example minM 4, maxM 6, splitS 2) now fails on read, as it does in Java: redefine it under options whose splits keep the range (minM at most ⌊(maxM+1)/2⌋), since a rebuild under the same options fails again.
- `TransformedRecordSerializer` encrypts and decrypts under every AES transformation Java's serializer can run with its 16-byte IV (CBC and PCBC with any SunJCE padding, CTS, CTR, CFB and OFB of 1 to 16 whole bytes, the `AES_128`/`AES_192`/`AES_256` forms and the NIST AES OIDs, as measured on JDK 21), not only `AES/CBC/PKCS5Padding`. Other names still fail with "encryption error" or "decryption error".
- `fdb` futures behave like libfdb_c's: `Cancel` on a future that is not ready completes it with
  `operation_cancelled` (1101) (it was a no-op), a pipelined `Get` future becomes ready while only
  `IsReady` is polled (it stayed not-ready until `Get`), and one future may be read from several
  goroutines.
- `KeyStoreSerializationKeyManager` reads JCEKS key stores, as Java's `KeyStore.getInstance(File, password)` detects them, besides PKCS12: AES keys sealed with PBEWithMD5AndTripleDES, Java's errors for a wrong store or key password. Ported from pyjks (MIT; see NOTICE).
- An FDB error inside a catalog operation (DDL, `ApplyMetadataOperation`,
  `fleet.RestoreTemplateVersion`) now carries the same SQLSTATE as anywhere else, for example 53F00
  for a transaction timeout or 40001 for transaction_too_old, instead of XX000. The mapper is
  exported as `exceptionutil.TranslateFDBError` (`pkg/relational/core/exceptionutil`).
- SQL planning has no task budget by default, as in Java (was 150,000 tasks, 250,000 right-deep); the
  caller's context bounds it, and `MAX_TOTAL_TASK_COUNT`, `MAX_TASK_QUEUE_SIZE` and
  `MAX_NUM_MATCHES_PER_RULE_CALL` opt into Java's caps (54F02 when exhausted).
- `MAX_NUM_MATCHES_PER_RULE_CALL` counts the bindings Java's matcher makes per rule call (per plan
  partition, quantifier, combination or permutation) where it counted Go's root bindings, the
  planner's swapped-quantifier firing included. A bound can now trip where it planned, or plan
  where the swapped firing tripped it; a trip reports the bound+1 Java reports (RFC-274).
- `OnlineIndexer.MergeIndexes` holds no session, as Java's `mergeIndex`: it writes no heartbeat (which blocked Java's exclusive builders), checks no index state, and proceeds under a live peer and over a DISABLED target.
- `plandiff.SeedCorpus`, `SeedRunCorpus`, `ApplyRFC082Divergences` and `IsKnownRed` moved to the
  testonly package `plandiff/seedcorpus`; `plandiff` keeps the `Query` and `RunQuery` types.
- **Breaking (API):** the gomock `Mock*` types move from `pkg/relational/api` to the Bazel
  `testonly` package `pkg/relational/api/apimock`, so production binaries no longer link
  `go.uber.org/mock`. Migrate `api.NewMockX(ctrl)` → `apimock.NewMockX(ctrl)`.
- The Cascades planner plans about 2x faster with the same plans: memo deduplication rejects
  candidates on group signatures before comparing them, reuses published correlation snapshots
  instead of recomputing them, and allocates less (a 150,000-task planning run: 41 s to 17 s,
  13.3 GB to 5.6 GB allocated).
- Go API moves, following Java's packages: index-predicate normalization is
  `pkg/recordlayer/indexpredicate` (was in the cascades planner), `FinalizePlan` and the plan
  walkers are in `pkg/recordlayer/query/plan/plans`, and the R-tree is `pkg/async/rtree`
  (Java's `async.rtree`). `pkg/recordlayer` no longer imports the planner.
- **Breaking (API):** the planner's test-only census left the public `values`, `cascades`,
  `executor` and `core/query` APIs for the testonly `pkg/internal/querycensus`:
  `SetLegIdentityCensusEnabled`, `SetOrderingComparisonCensusEnabled`, `LegIdentityCensusEnabled`,
  and every `Record*`/`Note*`, `Assert*`, `Format*`/`Dump*`, `Reset*`, `*Floors` and snapshot
  function and type. `cascades.OrderingComparisonSite` and `query.UnnestLegMintSite`/
  `UnnestLegMintArm` moved to `values`, and the census enums no longer have `String()`.
  What remains public is the nil-by-default hook: `values.CensusHooks`, `OrderingHooks`,
  `SetCensusHooks`, `Census`, `OrderingCensus` and `CensusQOVShape`.
- **Breaking (storage):** the relational key space is Java's `RelationalKeyspaceProvider` layout byte for byte. The catalog store is `(NULL, NULL, 0)`; a schema's record store is `(domain, database, schema)`, where the domain is a directory of the FDB directory layer and the database and schema names are interned in the domain's interning layer. Go and Java open each other's databases and schemas in a shared cluster. Data written by earlier Go builds is not migrated: recreate it.
- `pkg/fdbgo/fdb/directory` runs on any `fdb.WritableTransaction` (the libfdb_c backend and the simulator included; `UnsupportedBackendError` is gone) and `NewDirectoryLayerWithRandom` injects the prefix allocator's random source.
- **Breaking (storage):** a VECTOR (HNSW or GuardiANN) index reads its vector as Java does: value column 0 as serialized vector bytes (`vectorcodec.Serialize`), later value columns ignored. Numeric value columns, the old way to spell a vector, now fail a save and a delete with the new `ClassCastError`; an empty or unknown-type vector fails with the new `IndexOutOfBoundsError`. Recreate such an index over a bytes vector column. The SPFresh index is unchanged.
- **Breaking (storage/metadata):** a VECTOR index is refused at build as Java's `VectorIndexValidator` refuses it: without a dimension count ("need to specify the number of dimensions"; Go's 128 default is gone), or with a grouping root, a unique option, a version column, a root that is not a `KeyWithValueExpression` ("vector index type must use top key with value expression") or a fan-out. Recreate such an index with `KeyWithValue(root, splitPoint)` and an explicit `hnswNumDimensions`.
- A `collate_jre`/`collate_icu` key expression no longer counts as a fan-out, as in Java (`CollateFunctionKeyExpression.createsDuplicates` delegates to its argument): a collated primary key, `CARDINALITY` argument, online-build source index and vector partition column are accepted, and a collated VALUE index's scans count as distinct records; `Split(collate(x))` is refused.
- **Breaking (storage/metadata):** a VECTOR index's `hnswMetric`/`vectorMetric` must name one of Java's `Metric` constants (`EUCLIDEAN_METRIC`, `EUCLIDEAN_SQUARE_METRIC`, `COSINE_METRIC`, `DOT_PRODUCT_METRIC`); the lower-case `cosine`, `inner_product` and `euclidean` are refused when the meta-data is built (`MetaDataError` "incorrect index options"), as in Java. With `vectorindex` linked, a plain VECTOR index is now also refused at build for an option under both its name and alias, an unknown engine ("unknown vector index engine"), an unparsable number, Java's `Config` bounds and GuardiANN parse failures. Recreate an index that used them.
- **Breaking:** a relational database path is `/DOMAIN/DATABASE`, as in Java, and its domain must be registered once per process before use: `sqldriver.RegisterDomainIfNotExists("FRL")` (Java's `RelationalKeyspaceProvider.instance().registerDomainIfNotExists`). The driver registers none; `frl` registers `FRL`, as Java's server and CLI do. A path outside the registered domains (including a one-segment `/name`) is INVALID_PATH (08F01) on CREATE DATABASE, on a schema's store and when connecting to a schema; a malformed path is 08F01 `invalid database path '…'` (was 22023). `/__SYS` needs no domain.
- An array result column's `DatabaseTypeName` is `ARRAY`, as Java's `getColumnTypeName` (it was the element's type name), and its scan type is not a scalar. `api.WithResultSetMetaDataObserver` gives a query's result-set metadata, whose `ColumnDataType` carries a struct column's declared type name and fields and an array column's element type.
- A SQL query block is one Select, as in Java 4.14.2.0: predicates reach a derived table's or CTE's access paths, a computed column without an alias is named by its position (`_0`), and EXPLAIN shows `Map(…, {…})` where it showed `Project(…)`.
- A SQL function call binds its arguments as Java does (a one-row values source pushed into the body), so its body's predicates reach index scans and joins of calls plan in Java's order.
- A WHERE over a LEFT JOIN with a projected EXISTS reading the null-supplied side, and an EXISTS over a correlated array of scalars under a join, answer instead of failing.
- Fixed-factor union planning uses fewer temporary allocations in boolean normalization and memo matching.
- A recursive CTE's column list names its columns only for the query that reads the CTE; the recursive branch reads the seed's own names, as in Java 4.14.2.0.
- A recursive CTE keeps the seed's column types and nullability for every iteration, as in Java 4.14.2.0; a recursive row that does not fit (a NULL into a NOT NULL column, another type) is refused with XXXXX.
- A SQL function call and a recursive CTE name a repeated or unnamed column by its position (`_0`, `_1`, …), as in Java 4.14.2.0; a SQL function whose name is not ASCII can be called.
- `UPDATE … RETURNING` and `DELETE … RETURNING` answer the modified rows through `Query`, as in Java 4.14.2.0 (an UPDATE's `"old"` and `"new"` records); on `Exec` such a statement is refused with 42F61, and a statement without a result set on `Query` with 02F01, before it runs.
- A boolean ARRAY parameter binds; a join comparing two rows' `__ROW_VERSION` through a version index runs.
- SQL accepts EXISTS inside AND/OR/NOT boolean expressions in WHERE and INNER JOIN ON, matching Java's one-row existential witness semantics.
- An EXISTS subquery whose own source reuses an outer name reads its inner source, as in Java 4.14.2.0 (Java's inner shadow). Shapes Go declined with 0A000 now answer: an inner source named like an outer source or an outer UNNEST frame, an early JOIN ON naming an alias a later inner leg reuses (the ON resolves left to right, so the later leg does not capture it), NOT EXISTS or a projected EXISTS over a middle subquery whose outer-only conjunct sits beside a nested EXISTS, an outer reference in an OUTER join's ON (or in an inner ON before a later RIGHT join) inside the subquery, which stays in its ON, and a correlated UNION ALL body, whose branches keep their correlations.
- A parenthesised statement (`(SELECT …)`, `((SELECT …) UNION ALL SELECT …)`) is the query it encloses, as in Java; it was 0AF00 `could not build logical plan`. Java's corpus files `union.yamsql` and `union-empty-tables.yamsql` pass.
- A SELECT with several projected EXISTS, or a projected EXISTS beside a WHERE (or ON) EXISTS, answers as Java; it was 0AF00 `Cascades planner could not plan query`. Java's corpus file `exists-in-select.yamsql` passes.
- A qualified star over an enclosing query's source (`… EXISTS (SELECT B.* FROM A WHERE …)`) expands to its correlated columns, and a qualified star beside other items under GROUP BY (`SELECT A.*, A1 … GROUP BY A1, A2, A3`) is expanded and checked column by column, as in Java (was 42703 / 42803).
- IN-list type errors follow Java: a list of literals of different types (`IN (1, 2.5)`, `IN (1, 3000000000)`) is 42804 `Elements of array literal are not of identical type!`; a computed list promotes its items (`IN (2 + 1, 2.5)` answers) and one that cannot is 22000; a probe the list's element type cannot take (`n IN ('a', 'b')` over a BIGINT) is 22000 (was 42804). A comparison is an IN item (`d IN (3 < 4)`, was 0AF00).
- A JOIN on a comma-separated FROM source (`FROM t1, t1.refs AS r JOIN t2 ON r = t2.id`, `FROM a, b JOIN c ON …`) is accepted, as in Java; it was 0A000. A RIGHT or FULL join there stays refused. Java's corpus file `right-deep-plan-tests.yamsql` passes.
- A WITH nested inside a recursive CTE's body (`WITH RECURSIVE x AS (WITH RECURSIVE y AS (…) SELECT … FROM y UNION ALL …)`) runs, as in Java; it was 0AF00. Java's corpus file `recursive-cte.yamsql` passes.
- SQL over `/__SYS?schema=CATALOG` reads the catalog's tables `SCHEMAS`, `DATABASES` and `TEMPLATES`, as in Java: the catalog's schema template is built from table definitions, so its stored metadata is Java's (was 42F01 / `no schema metadata available`). An invalid table or column name in CREATE TABLE is 42602 before its types or primary key are checked. Java's corpus files `catalog.yamsql` and `create-drop.yamsql` pass.
- A table of metadata loaded from Java whose record-type name uses a non-canonical escape (`___T6__2__UNESCAPED`) answers to its decoded name (`"___T6.__UNESCAPED"`) in every statement, as Java's template keys tables by decoded name. Java's corpus file `valid-identifiers.yamsql` passes.
- Saving a record larger than a single value without split long records fails with Java's RecordCoreException text `Record is too long to be stored in a single value; consider split_long_records`, reported as XXXXX (was an unmapped error). Java's corpus file `large-record-fails.yamsql` passes.
- A FROM name that is no table, view or CTE but names an enclosing query's FROM item (`… FROM t AS x WHERE EXISTS (SELECT 1 FROM x …)`) reads that item's table or CTE again, as Java's findCteMaybe does (was 0AF00).
- In a block with GROUP BY or aggregates, a QUALIFY naming a column that is not a grouping key (an aggregate's operand included) is Java's 42703 "Attempting to query non existing column X": Java resolves QUALIFY against the aggregate's output. Go answered 42803 or 0AF00.
- An EXISTS in HAVING is refused with Java's 42803 "Invalid reference to non-grouping expression", after the subquery resolves (an ambiguous reference inside it stays 42702); Go refused it with a planner 0AF00.
- QUALIFY in a block with GROUP BY or aggregates filters the aggregate's output, conjoined with HAVING, as in Java; Go applied it to the input rows (`SELECT COUNT(*) FROM a WHERE … QUALIFY 1 = 0` answered `[0]` where Java answers no rows). A correlated QUALIFY inside EXISTS is answered instead of refused with 0AF00. An aggregate call in QUALIFY, where Java fails internally, is answered by Go (DIVERGENCES.md).
- An EXISTS whose grouped body has a HAVING that reads the enclosing row (`EXISTS (SELECT a2 FROM a GROUP BY a2 HAVING COUNT(*) > b.b1)`) is answered as in Java; Go refused it with 0AF00.
- A star over an enclosing source in a grouped select list (`EXISTS (SELECT A.*, B.* FROM A GROUP BY A1, A2, A3)` with `B` from the outer query) is answered as Java answers it; Go refused it with 0A000. Java's corpus file `select-a-star.yamsql` passes.
- A recursive CTE's self-reference can be read inside an EXISTS of its own recursive leg (`FROM r AS c, e … WHERE NOT EXISTS (SELECT … FROM c …)`); it reads the previous level's rows, as in Java. Go refused it. Java's corpus file `documentation-queries/with-documentation-queries.yamsql` passes.
- Names that differ only in case are distinct, as in Java. Quoted columns `"COLUMN"`, `"column"` and `"cOLumN"` in one table are accepted (Go refused them at CREATE) and an INSERT keeps each value in its own column (it used to fold them onto one). Tables `"Table1"` and `"TaBlE1"` each resolve to themselves, in queries and in index and function bodies. Under CASE_SENSITIVE_IDENTIFIERS a user scalar function call keeps its case (`f3` beside `F3`). Java's corpus file `keyword-case-insensitivity.yamsql` passes.
- The `CASE_SENSITIVE_IDENTIFIERS` connection option is honoured: an unquoted identifier keeps its case, as if quoted, as in Java (Go ignored the option). Tables whose names differ only in case resolve exactly in the semantic catalog. Java's corpus file `setup-with-connection-options.yamsql` passes.
- An IN list item that fails to evaluate (`a IN (1 / 0, 3)`) raises when the query runs (22012), even over an empty table, as Java's does; it was 0AF00 `Cascades translation failed`.
- An unknown column is 42703 `Attempting to query non existing column X`, Java's text, with the reference as written (`T.X`, `Q.ID` for an unknown qualifier) in every clause and in UPDATE's SET; a qualified star over no source and a JOIN USING column missing from either side are `Unknown reference X`.
- A query block reports its faults in Java's clause order: a join's ON first, then the WHERE, then the select list (an unknown function in the select list included).
- `frl` is a package of the root module and releases under the project's `vX.Y.Z` tag. The nested
  module `fdb.dev/cmd/frl` is scheduled for retraction (`cmd/frl/v0.1.1`) only after the root
  v0.2.0 release is available as proxy `@latest` (`RELEASE.md`); no retraction is asserted yet. A module that requires `fdb.dev/cmd/frl` drops it with
  `go get fdb.dev/cmd/frl@none` before upgrading `fdb.dev`, or the build reports an ambiguous import.
- SQL `LIKE` follows Java 4.14.2.0: wildcards cross newlines, `LIKE NULL` is allowed, and invalid escapes raise 22019/2200B/22025 per row.
- SQL comments follow Java 4.14.2.0: block comments nest, an unterminated one is 42601, and `#` is no longer a comment.
- `OPTIONS (...)` is statement-level only and adds `PLAN RIGHT DEEP` and `ISOLATION LEVEL SNAPSHOT`; `PLAN` and `DEEP` are reserved words.
- A typed, parenthesised or column NULL IN-list item is 0A000 when the list is evaluated, as in Java 4.14.2.0 (a bare NULL stays 42809).
- An ARRAY element is never NULL: a NULL array element is refused with 0A000 as in Java 4.14.2.0 (literal-array `=`/`<>`/`IS DISTINCT FROM` comparisons still accept one).
- Driver parameters are bound as typed constants (int32 INT, int64 LONG, slices ARRAY, uuid.UUID UUID), not spliced into SQL text; named `?x`/`$x` and `IN ?` are supported, extra arguments are ignored and a missing one is 42F02.
- A NULL-strict expression over NULL is NULL when a predicate is simplified, as in Java 4.14.2.0: `WHERE COALESCE(NOT CAST(NULL AS BOOLEAN), TRUE, 1 / 0 = 1)` answers every row (was 22012), and EXPLAIN shows `CAST(NULL AS T)` as `NULL` and a value `NOT x` as `NOT x`.
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
- SQL text or a string parameter that is not valid UTF-8 is refused with 22021 (previously its bad bytes were replaced by U+FFFD); a record holding such a string is refused on save with `InvalidUTF8StringError` (22021 through SQL), including an update of a record stored that way, until the string is repaired. Bound byte and VECTOR parameters are copied at bind time.
- A bound `time.Time` is the TIMESTAMP value of its instant, its UTC text to the second (sub-second precision is dropped); it was the date text when the value was midnight in its own zone, so `2024-01-01T00:00+02:00` bound as `'2023-12-31'`. Into a DATE-spelled column it now stores `'2024-01-01 00:00:00'`, and `WHERE d = ?` compares that text; bind a day as `CAST(? AS DATE)` (the instant's UTC day). A time whose UTC year is outside 0000-9999 is refused with 22008 naming the parameter. `[]time.Time` binds as ARRAY<TIMESTAMP>. DATE and TIMESTAMP result columns scan as `string`.
- A DATE meeting a TIMESTAMP in a comparison, an IN list, CASE, COALESCE, IFNULL, GREATEST or LEAST is promoted to its midnight, so `CAST('2024-01-01' AS DATE) = CAST('2024-01-01 00:00:00' AS TIMESTAMP)` is TRUE (was FALSE, comparing text); GREATEST/LEAST accept DATE and TIMESTAMP values (were 22000/22F00). A DATE- or TIMESTAMP-spelled column is STRING and still compares text.
- `CAST(string AS DOUBLE/FLOAT)` is Java's `Double.parseDouble`/`Float.parseFloat`: a signed `NaN`, `+Infinity`, the `d`/`D`/`f`/`F` suffixes and out-of-range magnitudes (`1e400` is Infinity) are accepted, `nan`, `inf`, `infinity` and `1_000` are refused (22F3H, Java's message), FLOAT rounds once to binary32, and a NaN is Java's canonical NaN, so Go and Java write the same record bytes and index key for it (Go wrote `0x7ff8000000000001`, which Java's index probe missed and a UNIQUE index admitted beside Java's NaN).
- `EmbeddedConnection.SetOption` (reach it through `sql.Conn.Raw`) sets one connection option, checked against Java's option contracts (22023 for a wrong type or range), and lasts until the connection returns to the pool; `SetOptions` replaces the whole set. The DSN accepts `dry_run` and `isolation_level_snapshot`, and an unknown DSN parameter is now an error (22023) naming it and listing the accepted ones (it was ignored). A result set keeps the options it started with across its pages.
- Distance functions, `ROW_NUMBER`, `RANK` and the record-type, incarnation and CASE-selector values are typed nullable, as their Java classes are (each could evaluate to NULL while typed NOT NULL); a result column over one reports nullable.
- Record layer: the Cascades planner enforces Java's REWRITING prune. A group crosses into PLANNING only as its one winning final, and REWRITING costs alternatives over single-final children. Either violation is now a planner error (`RewritingCrossingError`, `RewritingPruneError`) where a group used to carry several members across. A union whose legs merge into a group that had not yet been explored plans again; that shape could previously leave the union unfinalized.
- Record layer: an index filtered by a Go closure (`Index.SetPredicate`) no longer answers queries. Its entries cover only the records the closure admits. An ordered full scan, a range or an equality probe of it returned only those records, so rows were silently missing (`SELECT email FROM t ORDER BY email`). Queries now read the base records; the index is still maintained.
- Record layer: the Cascades planner's comparator and the rule calls' `CostModel()` rank with the planner context's configuration and metadata when no statistics are set, as Java's rule calls do; they used to rank as if the context were empty. A SQL self-join's inner leg can now probe an index where it read a primary-key range (`ON a.did = b.did WHERE a.eid < b.eid`). Record-layer callers who hold a continuation across this change, or across the IN-union changes below, should restart paged queries: a continuation minted by a plan that changed is refused when the new plan's outermost cursor is an index or ranged scan, and is not detected (as in Java) when it is an unfiltered primary scan or an in-join reading an in-union's continuation.
- Record layer: an IN-union ranges over every plan of its ordering partition, as Java's does, instead of one pinned plan. A merge over a non-covering index therefore reads covering entries and fetches above the merge (`Fetch(InUnion(IndexScan(…, COVERING)))`, Java's `INUNION … | FETCH`). A keyed set operation now pushes below its fetch when every leg covers its comparison keys; before, those keys never translated. An extracted in-union whose child does not provide its merge order fails planning with `InUnionChildOrderingError`, wrapped as a `PlannerInvariantViolationError`. SQL plans in the corpus are unchanged, because the sorted IN-join still wins where both apply.
- An `IN (…)` with no ORDER BY is never an IN-union merge, as in Java 4.14.2.0, whose rule skips an unordered request: it is an IN-join (`WHERE o.id IN (2, 3) AND NOT EXISTS (…)` was `InUnion(FlatMap(…))`). Record layer: `plans.NewRecordQueryInUnionPlan` takes the in-union's size as its last argument (`plans.UnboundedInUnionSize` for no limit), `NewRecordQueryInUnionPlanWithBindingAliases` is removed in favour of `NewRecordQueryInUnionPlanWithBindingAliasesAndMaxSize`, and `ImplementInUnionRule` fails a rule call that has no planner context instead of assuming size 0.
- An IN-union merge refuses more than 24 child executions, the product of its IN lists' distinct sizes, with XXXXX "too many IN values" before it reads, as Java 4.14.2.0's relational layer does (`IN` of 25 values, or 5 × 5 over two lists, ordered so that only a merge serves it). Record layer: an in-union takes its maximum from `PlannerConfiguration.AttemptFailedInJoinAsUnionMaxSize`, whose default is Java's 0, so a planned in-union over any IN list now fails with `RecordCoreError` "too many IN values" unless the caller raises it; a hand-built `NewRecordQueryInUnionPlanWithBindingAliases` plan is unbounded.
- `col IN (…) ORDER BY id` over an index that does not name `id` in its key plans no IN-union merge, as Java 4.14.2.0 builds none: the per-value index reads are sorted (`InMemorySort(InJoin(…))`), rows unchanged. A relational index entry holds `id` after its record type, and Java's index match stops at that record type. An index that names `id` (`CREATE INDEX … ON t (col, id)`) keeps the merge, and `col = ? ORDER BY id` still reads the index without a sort (DIVERGENCES.md).
- Record layer: `plans.NewRecordQueryExplodePlanWithOrdinalityBase` and `expressions.NewExplodeExpressionWithOrdinalityBase` build an ordinality explode numbered from 0, as Java's three-argument constructors do; SQL `AT` stays 1-based.
- `col IS NOT DISTINCT FROM x`, with either operand first, is an index or primary-key scan bound, as in Java 4.14.2.0 (#4598). `IS NOT DISTINCT FROM NULL` reads the null key (it was a full scan with a filter). A sparse `WHERE col IS NOT NULL` index serves it only for a non-NULL literal or bound value. EXPLAIN shows the bound as `≡` (`IndexScan(I2, [≡])`). Record-layer callers: a continuation from the old full-scan plan is refused by the new index plan, so restart such paged queries across this change.
- `COALESCE`, `GREATEST` and `LEAST` promote every argument to the common type, as Java's `VariadicFunctionValue` does: `COALESCE(int_col, double_col)` returns a DOUBLE value when the INTEGER argument is chosen (it returned the integer), and EXPLAIN shows the promotions (`COALESCE(_current.A#1, PROMOTE(0 TO BIGINT))`).
- A projected `AND`, `OR`, `NOT` or `EXISTS` is a nullable BOOLEAN column whatever its operands, as in Java 4.14.2.0 (`SELECT NOT FALSE` and `SELECT TRUE AND TRUE` reported NOT NULL).
- `OPTIONS (LOG QUERY)` and the `LOG_QUERY` connection option set `PlanGenerationInfo.LogQuery` on the statement's planning record (they were parsed and ignored). Java logs such a record at INFO, as it logs a slow one.
- An explicit transaction no longer aborts (40001) because an index it did not scan changed state: planning and plan revalidation read index states without a read conflict, as in Java; a scanned index's state change still conflicts. `FDBRecordStore.GetAllIndexStates` conflicts on the whole index-state subspace, as Java's `getAllIndexStates`; `PeekIndexStates` reads without one.
- A NaN equality on a float index or primary-key column (`d = CAST('NaN' AS DOUBLE)`, a bound NaN, `d IN (NaN, …)`) uses the index and returns every stored NaN (it used a full scan, and an IN-join over the index failed with "exact indexed NaN equivalence is unsupported"), including when later index columns are also bound (`d IN (NaN) AND g IN (1, 2)`), which are then checked per index entry. An ORDER BY over a later index column sorts.
- `MIN`/`MAX` over DOUBLE and FLOAT return the NaN operand with its own bits, as Java's `Math.min`/`Math.max` (Go returned `0x7ff8000000000001`).
- Cached statements preserve each execution's BOOLEAN values, NaN payloads and signed zeros (`… AND ?` bound `false` previously returned the rows of `true`).
- `CAST(… AS DATE)` reads every text `CAST(… AS TIMESTAMP)` reads (RFC 3339 with an offset included, surrounding whitespace trimmed), and both refuse a UTC year outside 0000-9999 (22F3H); the date-part functions trim too (`YEAR(' 2024-01-01')` is 2024, was 22023).
- Driver parameters: a `*uuid.UUID` binds as UUID (a nil one as NULL; it panicked), `[N]byte` and named byte slices as BYTES, database/sql null wrappers by their payload (`sql.NullInt64` is LONG), and an array from its static element type (an empty `[]int` is the untyped empty array); a nested array or unsupported type is 22023 naming the parameter.
- A SUM or COUNT aggregate index answers alone, as Java 4.14.2.0's does: a group whose rows were all deleted or moved away reads 0, a group whose values are all NULL has no SUM or `COUNT(col)` row, and a group whose last non-NULL value was removed reads SUM 0. The DDL no longer adds a `<index>__GROUP_COUNT` index beside a grouped SUM or `COUNT(col)` index, so Go-created metadata holds exactly the declared indexes; recreate schemas an earlier build created. An ungrouped SUM or COUNT index serves the ungrouped aggregate, and an index name ending in `__GROUP_COUNT` is accepted.
- A table whose name escapes (`"MY$TABLE"`, `"foo.table$nested"`) plans primary-key ranges, secondary-index and aggregate-index scans as any other table does (previously a full scan with a residual): the plan carries the record type's stored name, and EXPLAIN shows the SQL name, as Java 4.14.2.0 does.
- A column whose name escapes (`"c$1"`, `"i.d"`) plans as any other column: its indexes and primary key are used for ranges and ORDER BY, aggregate indexes over it answer, and `CREATE INDEX … ON t ("c$1")` is accepted (previously 42703).
- An aggregate index serves a coarser grouping than its own, as Java 4.14.2.0's does: the query's grouping columns in any order, a grouping column bound by an equality (`SELECT SUM(v) FROM t WHERE g = 1` over an index grouped by `g` is a point read), and otherwise a roll-up of the SUM, COUNT or MIN/MAX_EVER index to its matched grouping prefix (`WHERE g > 0`, or `GROUP BY a` over an index grouped by `a, b`).
- A table whose quoted lowercase primary-key or vector column names differ from their upper-case spelling gets primary-key scans and vector index plans (previously a full scan, or 0AF00).
- A vector query without `OPTIONS EF_SEARCH` searches with Java's default, min(max(4k, 64), max(k, 400)), instead of 200.
- **Breaking (behaviour):** `FDBDatabase.Run`, `RunWithWeakReads`, `RunWithVersionstamp`, `RunRead` and `FDBDatabaseRunner.RunWithRetry` retry as Java's transaction runner does: at most 10 attempts (`FDBDatabase.SetMaxAttempts`, `SetRetryDelays`), each a fresh transaction whose backend retry limit is 0, retried while any cause in the error is retriable, with Java's randomized exponential delay between attempts (recorded as `EventRetryDelay`). They used to retry without bound. Weak reads apply to the first attempt only. A context that ends between attempts returns an error wrapping both the context's error and the last attempt's. A transactor implementing `AttemptTransactor` receives each attempt's identity; `SetAttemptObserver` reports each attempt. The online indexer bounds its transactions with `OnlineIndexerBuilder.SetMaxAttempts` (default the database's). A SQL DDL statement runs in one attempt, as in Java: a conflicting DDL fails with 40001 instead of re-executing. An SPFresh write that meets a split in progress (`SPFreshSplitWindowError`, a retryable 1020) does not use up attempts; SPFresh's background lifecycles keep the client's unbounded loop (DIVERGENCES.md).
- **Breaking (behaviour):** a body that commits the context `Run` (or a variant, or `RunWithRetry`) handed it is refused before anything commits, with `RecordContextNotActiveError` ("Transaction is no longer active.", a `RecordCoreStorageError`); `Run` commits. A context refuses a second commit the same way.
- Pure-Go client: a transaction with no read version when it commits fetches one with `CAUSAL_READ_RISKY`, as libfdb_c does, so that GRV skips the proxy's last-commit wait.
- Pure-Go client: a commit made while no commit proxy is known waits for the proxy set to change (bounded by its context) and then fails with `commit_unknown_result` (1021), as libfdb_c does. The Go-only error 1200 (`ErrAllProxiesUnreachable`) is gone, and code 1200 now has its canonical FDB name, `recruitment_failed`.
- `fdb.Database.TransactCtx` / `ReadTransactCtx` and the tenant forms return the error the transaction body returned (or panicked with), chain intact, when the retry loop gives up on that error's code, as the Apple binding does; they returned a bare `fdb.Error{Code}`.
- HNSW graphs are written byte-for-byte as Java writes them, for the same inserts and deletes:
  - a pruned neighbour list keeps its survivors in list order;
  - a list at exactly mMax is pruned;
  - a prune never extends candidates;
  - a delete repair prunes every candidate and samples at Java's rate;
  - a re-inserted neighbour moves to the end of its list;
  - a layer search starts from its entry's stored vector;
  - a pairwise distance between an encoded and a plain vector is a RaBitQ estimate.

  Graphs written by earlier Go builds stay readable but differ in neighbour lists.
- GuardiANN on a trained index with an extra-bit count RaBitQ cannot use refuses only where Java constructs the quantizer (an insert of a new key, a search, a task write or task body); a delete that queues nothing still runs.
- HNSW and GuardiANN cosine distance is Java's: a zero vector is at +Inf and a non-finite input is NaN, with no clamping, and distances order as Java's `Double.compare`. HNSW node caches live for one operation, a vector search read-locks its partition while it searches, and HNSW sample keys carry a tuple UUID as Java's do (older entries are still read). SPFresh keeps its clamped cosine.
- The plan cache is shared by every database and schema of one schema template, as Java's `RelationalPlanCache` is, and stored queries are planned into it when a connector starts.
- Planner: disabling the rule name `ConditionalCascadesRule` (`DISABLED_PLANNER_RULES`) turns off every conditional rule chain, as in Java; it was ignored.
- An IN-union over an IN list whose items planning cannot evaluate evaluates them when the query runs, as Java does; it failed with "no planning-time values".
- **Breaking (API):** `keyspace.FDBResolver` is removed; it had no users. The resolver interface and `ResolverDirectory` remain.
- GuardiANN vector indexes refuse malformed stored values instead of panicking, check the deferred insert cap before writing anything, poison the transaction when a task fails after its removal, and merge or drain every requested index even when an earlier one fails.
- GuardiANN k-means fits reuse their buffers and RaBitQ scorers across restarts, compute a k = 2 restart's seed objectives once per fit and sum centroids four vectors per pass, with the same fits: the outlier peel's worst case at the most restarts it admits uses a third of the CPU. `rabitq.Scorer.Reset` reuses a scorer for another query.
- Decimal literals parse as Java's `ParseHelpers.parseDecimal` wherever they stand: a dotless exponent (`1e5`) or an out-of-width integer is XXXXX `For input string: "…"`, and an overflowing `1.0e400` is an infinity (previously 22003 or 0AF00).
- A record constructor's field takes its element's own name (`SELECT (x, y)` is `{X, Y}`, `(x + 1)` is `{_0}`, a repeated name falls back to the position), so a derived record's fields can be read by name; INSERT … SELECT, UPDATE and COALESCE bind a record to its struct type by position, ignoring element names, and an INSERT VALUES element naming another field is XX000, as in Java 4.14.2.0.
- Scalar macro calls take named arguments (`f(b => 1, a => 2)`), bound by name with the declared defaults; a repeated name is 42601, and an unknown name, a missing argument without a default or too many arguments is 42883 `could not find function '…'` (also for table functions), as in Java 4.14.2.0. A macro may take or return a struct type no table stores.
- A window's `OPTIONS EF_SEARCH` accepts `L`/`I` suffixes, refuses a repeat (22F00) and a value beyond int (22000), and an HNSW search uses it as given: below k it returns fewer rows, as in Java 4.14.2.0.
- The top-level query is a sort over its block, as in Java 4.14.2.0: EXPLAIN shows fewer `Map` operators, index scans that hold every projected column are covering, and an IN list over an unindexed column runs as Java's per-row explode.
- A disjunction over a key (`id = 1 OR id = 3`, `NOT BETWEEN`) merges its legs' ordered scans as Java does, without a sort.
- An `IN (…) ORDER BY` merge keeps rows that tie on a projected sort key (Java drops them), and an IN-union over several IN lists runs every combination.
- `col IN (…) ORDER BY col` runs as Java's sorted IN-join with no sort, ascending, descending or followed by the rows' own order in either direction; over a non-covering index the IN-join runs under the fetch where Java merges an IN-union (DIVERGENCES.md).
- A WHERE conjunct on a LEFT JOIN's preserved side narrows that side's scan (a key equality is a point lookup), a conjunct rejecting the null-extended row turns the join inner, and an EXISTS over the null-supplied side runs inside its join, as in Java 4.14.2.0.
- A version index is never a covering scan, as in Java 4.14.2.0: `ORDER BY` a column with ties returns them in primary-key order through the plain index.
- A `COALESCE` folds while planning only when its first argument is NULL or a BOOLEAN literal, as in Java 4.14.2.0, so its other arguments are evaluated as written: `WHERE COALESCE(1, 1/0) = 1` raises 22012 (it answered every row).

- **Breaking (storage):** the default TEXT tokenizer lowercases a word-final capital sigma to `ς` and drops a word with no letter or digit in the Basic Multilingual Plane, as Java's `DefaultTextTokenizer` does; rebuild Go-written TEXT indexes over Greek or supplementary-plane text.
- Runnable SQL/typed-store quickstarts check errors; default cluster lookup follows C++ precedence and routine SQL warm-up logs only at Debug.
- Scalar literals and prepared values reuse immutable SQL plans through per-execution constant pools and checked type, equality and specialization constraints.
- **Breaking:** a typed record store works over any metadata's union and reads and writes only its record type's union field, as Java's `TypedRecordSerializer`: loading, deleting or overwriting a record of another type fails with `RecordDeserializationError` ("Specified union field was not set"), and so does `ScanRecords` at the first such record (it skipped them); saving one fails with `RecordSerializationError`. A Go type the metadata does not decode the record type as is refused on load ("Builder does not match union type"; a save of it, typed or not, fails first with `MetaDataError` "descriptor did not match record type"), and one of another message at construction ("fieldDescriptor and getter do not match"). `NewTypedRecordStore` takes `baseStore` and `recordType` (its demo-union wrap/unwrap functions are gone) and returns an error.
- A struct value buffered in a continuation resumes with the store metadata's descriptor, as Java's does; a compiled Go message that only shares its full name no longer decodes it. **Breaking:** `recordlayer.FindRegisteredMessageType(name)` is replaced by `GeneratedMessageType(descriptor)`, which answers only the compiled type whose descriptor that is. A buffered struct value whose type is in neither the metadata nor the plan now fails the resume instead of decoding through the global registry.
- **Breaking:** the demo schema (`record_layer_demo.proto`: `Order`, `Customer`, `Flower`, `TypedRecord`, `UnionDescriptor`, the `Color` enum) moved from `fdb.dev/gen` to `fdb.dev/gen/demopb`, so the driver and `frl` no longer register `com.apple.foundationdb.record` demo messages.
- `RecordMetaDataFromProto` takes options (`MetaDataProtoOption`): `WithLocalFileDescriptor`, Java's `RecordMetaDataBuilder.setLocalFileDescriptor`, makes meta-data loaded from its stored proto use the compiled (local) descriptors and the local union's record type keys once they validate as an evolution of the stored ones (checked by `WithEvolutionValidator`'s validator, Java's `setEvolutionValidator`, else the default), so compiled records can be saved; such meta-data refuses `ToProto`. `RecordMetaData.GetRecordTypeForDescriptor` is Java's `getRecordTypeForDescriptor`.
- **Breaking:** every save (untyped, pipelined, batch and dry run) resolves the record type by the record's descriptor, as Java's does: a compiled message saved into meta-data loaded without `WithLocalFileDescriptor`, or a same-named message of another shape, fails with `MetaDataError` "descriptor did not match record type" (it was matched by name).
- `fdb` `GetRange` sends its first batch at the call, as Apple's Go binding does: the rows reflect the transaction's writes as of that call, and commit waits for every issued range read (also one never consumed) and keeps its read conflict, as libfdb_c does. `GetSliceWithError` over more than batch 1's bytes (about 4 KB in the default mode) now takes two sequential round trips, batch 1 and then the rest, as libfdb_c does. `client.Transaction.StartGetRange` starts such a read and returns at once; `Database.Metrics()` (on `fdb.Database` too) reports `TransactionGetRangeRequests`. A watch whose read fails no longer leaves a read conflict.

### Fixed
- Record layer (RFC-275):
  - An online build no longer conflicts on writes to records of a type its target indexes do not
    cover. Java filters by record type before it adds the read conflict.
  - `GetNth` returns nil past level 0 of a ranked set without duplicate counting.
  - Key-space matching follows Java's declaration order.
  - The behavior changes listed under Breaking changes are Java's (`RecordCoreStorageException`,
    `ClassCastException`, `RecordCoreArgumentException`).
- SQL driver (RFC-272): a failed connector initialization (an FDB open failure) is retried by the
  next `Connect` instead of failing the `*sql.DB` for good, and the initialization runs detached
  from the `Connect` that starts it: a `Connect` whose deadline ends first returns its context
  error while the initialization (and its stored-query warm-up) completes for the next one. `ResetSession` also resets the statement timeout, result-byte cap,
  fail-on-scan-limit flag, plan and execution-stats loggers and slow-query threshold, so a
  pooled connection's next borrower no longer inherits them. A panic in any exported connection,
  transaction, result-set or DirectAccess entry is the internal error (XX000) and closes the
  connection, which the pool then discards. A stale `driver.Tx` no longer ends the newer
  transaction on its connection, and `Commit` on an ended transaction is 25F01 instead of a
  second commit. A self-returning `driver.Valuer` (or any cycle through Valuers, pointers and
  arrays) is 22023 instead of a stack overflow. Parentheses inside string literals, quoted
  identifiers and comments no longer count toward the 500-level nesting limit. Per-borrow
  options belong in `SetOption`: `SetOptions` sets what `ResetSession` restores, so code that
  copied the earlier `docs/mt-saas.md` recipe (`SetOptions` per tenant borrow) hands one
  borrow's options to the next borrower; the recipe now uses `SetOption`.
- SQL driver (#1013): a panic while an ended context aborts a transaction (on the context's own
  goroutine, outside every entry point) is logged and closes the connection instead of crashing
  the process. Enum, UUID, `range()` bound, 42602 invalid-name, UUID index-predicate and
  decimal-literal errors render their message once (`XX000: Invalid enum value for the enum type
  BOGUS`, not three times); a schema template's invalid-name error no longer carries its
  `build file descriptor: table …` context, as in Java. New `api.WrapStatedError` builds such an
  error: its message states the cause, which still unwraps.
- GROUP BY duplicate-key detection (42702) compares expression keys by their resolved value in
  value context, as Java does: keys differing only in literal case are two keys
  (`GROUP BY c = 'a', c = 'A'` was refused; Java declines it in its planner), `<>`/`!=` and other
  spellings of one comparison are still refused, and a key that does not resolve reports its own
  error (42703 for an undefined column) instead of 42702. With parameters the bound values decide
  (`amount = ?, amount = ?`: 42702 for equal values, two keys otherwise; a cached plan serves only
  the values it was planned with); Java's answer there is unmeasured (RFC-272).
- Built-in TEXT tokenization, casing/name lookup and Go-only collated keys use frozen Unicode-15 data, so compiler upgrades preserve existing terms, positions and keys; pre-existing Java differences remain explicit in `docs/compatibility.md` (RFC-265).
- Ordinary, covering and fetch query index scans over one physical range pass Skip to the index cursor, as Java's plans do: skipped entries no longer count against scan-record or byte limits, and their records are not loaded. Multi-range and residual key-filter scans, and the mapped reads of remote-fetch plans, still skip once above the combined cursor.
- Range-read shard lookups ask a commit proxy once for the shards a read needs, as libfdb_c does, instead of filling the cache for the whole range first; a reverse read over more uncached shards than one lookup returns no longer repeats the same location request until its deadline, and `GetLocations` with a limit of 0 returns every shard.
- Byte-limited range and mapped-range reads that start in a shard with nothing left to read return the same batch as libfdb_c: rows from the next shard and, when its reply is complete, from the shard after it, read with the bytes left. A read-your-writes range read remembers where a batch finished its shard and starts the next batch there, and one without local writes in its range caps its storage request at 80000 bytes, as libfdb_c does.
- Pipelined index record scans return a head that completes during mapping issuance before another source pull can fail, matching Java's post-insertion readiness ordering. A still-pending head retains source-error-first behavior (#915).
- Record-store load, save and delete timers count missing records and failed completions, including nested loads in saves, deletes and dry runs; erroring orphan index fetches increment the bad-index-entry counter, as Java does.
- The published test report counts individual test cases again: CI downloads remote-cached test logs, every shard is counted, and a missing log fails the report instead of counting one test per target.
- The client's read-your-writes key index stays sorted across mutations, avoiding a full re-sort on interleaved range reads; arbitrary key insertion and deletion still shift a linear slice suffix.
- The CGo backend and raw C reference share serialized database-handle ownership, preventing concurrent cache writes during startup and premature destruction of retained handles.
- Cursor continuations encode an empty suffix when a key equals its prefix, as in Java (#868).
- The client's `OpenDatabaseCoordRequest` carries the coordinator list, hostnames and known `ClientDBInfo` id, byte for byte as libfdb_c 7.3.77. Without the list, a restarted coordinator that heard from a Go client first stopped serving every client, libfdb_c included, and Go transactions waited until their deadline. The client now long-polls the coordinators for topology changes, as libfdb_c does, instead of polling every 5 s.
- `tools/clientbench` provides separate Go/CGo measurement executables, counterbalanced campaigns and paired latency/window/drained/shutdown CPU analysis with reproducible R3-design instructions; native builds require the explicit `libfdbc` tag.
- Record scans with an inclusive high bound end at `pack(high)+\xFF`, as Java's `TupleRange.toRange`, not at `strinc(pack(high))`.
- Key-value cursors reject invalid endpoint pairs (e.g. a TREE_END low, mismatched PREFIX_STRING bounds) with an error, as Java's `TupleRange.toRange` does, instead of scanning the whole subspace.
- Record and COUNT/atomic cursors implement PREFIX_STRING ranges.
- Reverse `ScanRecordsByType` scans preserve empty non-nil continuations as positions before the range prefix, returning no rows as Java does.
- Key-value cursor continuations on narrowed ranges are byte-identical to Java's, and resuming a Java token stays inside the range. Previously Go resuming a Java token could return rows outside the range, and Java resuming a Go token rebuilt a key with the shared prefix doubled. `TupleRange.Prepend` turns an open bound into the inclusive prefix as Java does, and atomic aggregate indexes (COUNT, SUM, ...) accept `BY_GROUP` scans.

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
