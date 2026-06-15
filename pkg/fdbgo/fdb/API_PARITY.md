# FDB Go Facade — API Parity with the Apple Go Binding

This package (`pkg/fdbgo/fdb`) is a drop-in-compatible API with the
[Apple FoundationDB Go binding](https://github.com/apple/foundationdb/tree/main/bindings/go).
The Apple binding wraps CGo `libfdb_c`; this facade can run on **either** a pure-Go
client (no CGo) **or** the `libfdb_c` backend (build tag `libfdbc`, RFC-109).

This document is **honest about the surface**: it distinguishes options that are
*honored*, options that are *accepted but ignored*, and options that are
*rejected with an error* on the pure-Go backend. (The earlier "✅ Full parity"
table counted silent no-op setters as implemented — a migration trap this split
removes.)

## Type-level parity

| Type | Status |
|---|---|
| `Transaction` / `ReadTransaction` | Methods present; read/write/atomic/range/watch/versionstamp all real wire traffic |
| `Snapshot` | Full |
| `Database` | Superset (`+ InvalidateGRVCache`, `OpenTenantById`) |
| Key selectors / `subspace` / `tuple` / `directory` | Full |
| `TransactionOptions` | **Present, but see the three tables below** |
| `DatabaseOptions` | Mostly honored (timeouts/limits/retry); some accepted-but-ignored |

## TransactionOptions on the pure-Go backend

### Honored — the option does real work

`Timeout`, `RetryLimit`, `PriorityBatch`, `PrioritySystemImmediate`,
`NextWriteNoWriteConflictRange`, `CausalReadRisky`, `ReadYourWritesDisable`,
`EnsureMutationCapacity`, `WriteConflictsDisabled`, `AccessSystemKeys`,
`ReadSystemKeys`, `LockAware`, `ReadLockAware`, `SizeLimit`, `MaxRetryDelay`,
`SnapshotRywEnable`, `SnapshotRywDisable`, `UseGrvCache`, `SkipGrvCache`, `Tag`,
`BypassUnreadable`.

### Rejected — returns `*UnsupportedOptionError` (FDB `invalid_option`, 2007)

These alter **security / access / idempotency** semantics and fail **unsafe** if
silently ignored, so the pure-Go backend rejects them rather than implying a
guarantee it cannot keep. The `libfdb_c` backend forwards them normally — use it
if you need them.

| Option | Why it must not be a silent no-op |
|---|---|
| `SetAuthorizationToken` | The request would be sent **unauthenticated** — auth bypass / wrong tenant scoping. |
| `SetRawAccess` | Bypasses tenant-mode scoping; a silent no-op would tenant-scope a read meant for the raw keyspace (wrong data on a shared cluster). |
| `SetAutomaticIdempotency` | Caller expects auto idempotency IDs so a `commit_unknown_result` is safely retryable; the pure-Go client does not generate them. |

### Accepted but ignored — no-op (fails **safe**)

These are tracing/hints/priority, or relaxations whose absence keeps the
**stronger** guarantee (ignoring a durability/causal *relaxation* simply keeps full
durability / strong consistency). They are accepted as no-ops:

`DebugTransactionIdentifier`, `LogTransaction`, `TransactionLoggingEnable`,
`TransactionLoggingMaxFieldLength`, `AutoThrottleTag`, `ReportConflictingKeys`,
`DebugRetryLogging`, `IncludePortInAddress`, `ServerRequestTracing`, `SpanParent`,
`ReadAheadDisable`, `ReadPriorityHigh`/`Low`/`Normal`,
`ReadServerSideCacheEnable`/`Disable`, `UseProvisionalProxies`,
`BypassStorageQuota`, `InitializeNewDatabase`, `ExpensiveClearCostEstimationEnable`,
`UsedDuringCommitProtectionDisable`, `CausalReadDisable`, `CausalWriteRisky`,
`DurabilityRisky`, `DurabilityDatacenter`, `DurabilityDevNullIsWebScale`,
`SpecialKeySpaceRelaxed`, `SpecialKeySpaceEnableWrites` (the special-key-space
module itself is absent — see below).

## Out of scope on the pure-Go backend

- **NetworkOptions / `StartNetwork`** — the whole network-options layer
  (trace files, knobs, TLS-via-option-API; Apple exposes ~48) is not implemented.
  TLS is configured via `WithTLSConfig` / the cluster `:tls` suffix + `FDB_TLS_*`
  instead (RFC-051).
- **Special-key-space module** (`\xff\xff/status/json`,
  `\xff\xff/transaction/conflicting_keys`, …) — absent.
- **Multi-version / external client** — by design (pure-Go is single-version).
- `Database.RebootWorker` — returns `errNotSupported`; `GetMainThreadBusyness`
  absent; `LocalityGetBoundaryKeys` honors its `readVersion` arg (RFC-111 P1.6).

## Cross-client compatibility

Wire/data compatibility (the hard line) is enforced by the go-vs-cgo differential
suite (`pkg/fdbgo/bench`) and the Java 4.11.1 interop tests: read/write, all 16
atomics, key-selector resolution, reverse ranges, versionstamps, conflicts,
tenants, watches, and the directory layer all produce byte-identical persisted
state to `libfdb_c`.
