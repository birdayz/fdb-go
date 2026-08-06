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
| `DatabaseOptions` | Mostly honored (timeouts/limits/retry); some accepted-but-ignored; two **rejected** (see below) |

## TransactionOptions on the pure-Go backend

The three tables below are machine-checked against `options.go`. `pkg/docscheck`'s
`TestAPIParityTablesMatchOptionsGo` parses every option method body, classifies it
from the statements alone as reject / no-op / honored, and fails the build when the
classification disagrees with this page or when an option appears in neither. An
option added or reclassified in code therefore cannot leave this page stale — which
matters, because this page has been wrong in exactly that way before. Names may be
written with or without the leading setter prefix.

### Honored — the option does real work

`Timeout`, `RetryLimit`, `PriorityBatch`, `PrioritySystemImmediate`,
`NextWriteNoWriteConflictRange`, `CausalReadRisky`, `ReadYourWritesDisable`,
`EnsureMutationCapacity`, `WriteConflictsDisabled`, `AccessSystemKeys`,
`ReadSystemKeys`, `LockAware`, `ReadLockAware`, `SizeLimit`, `MaxRetryDelay`,
`SnapshotRywEnable`, `SnapshotRywDisable`, `UseGrvCache`, `SkipGrvCache`, `Tag`,
`AutoThrottleTag`,
`BypassUnreadable`, `SpanParent`.

### Rejected — returns `*UnsupportedOptionError` (FDB `invalid_option`, 2007)

These alter **security / access / idempotency** semantics and fail **unsafe** if
silently ignored, so the pure-Go backend rejects them rather than implying a
guarantee it cannot keep. The `libfdb_c` backend forwards them normally — use it
if you need them.

| Option | Why it must not be a silent no-op |
|---|---|
| `SetAuthorizationToken` | The request would be sent **unauthenticated** — auth bypass / wrong tenant scoping. |
| `SetRawAccess` | Bypasses tenant-mode scoping; a silent no-op would tenant-scope a read meant for the raw keyspace (wrong data on a shared cluster). *Stricter than libfdb_c, which rejects raw-access only under a tenant and otherwise no-ops it; the pure-Go backend rejects unconditionally (fail-safe).* |
| `SetAutomaticIdempotency` | Caller expects auto idempotency IDs so a `commit_unknown_result` is safely retryable; the pure-Go client does not generate them. |
| `SetReportConflictingKeys` | Caller sets it to read the conflicting ranges back out of `\xff\xff/transaction/conflicting_keys/` after a `not_committed`. The pure-Go client sets neither the commit-request field nor the special-key read-back, so a no-op leaves the caller with empty results and no signal. |
| `SetBypassStorageQuota` | Caller is forcing a write through a full storage quota. The pure-Go client never sets `FLAG_BYPASS_STORAGE_QUOTA`, so a no-op ships the commit *without* the bypass and it is rejected with `storage_quota_exceeded` — the requested write-admission is dropped silently. |
| `SetSpecialKeySpaceRelaxed` | Relaxes libfdb_c's one-module-per-read restriction on special-key reads (`special_keys_cross_module_read` 2112 / `special_keys_no_module_found` 2113). The pure-Go backend has no special-key-space module, so there is no restriction to relax and the reads it is relaxed *for* cannot succeed either. |
| `SetSpecialKeySpaceEnableWrites` | Not a permission bit: in libfdb_c it arms `specialKeySpace->commit()`, the step that translates writes to `\xff\xff/management/...` into real configuration mutations (without it, `special_keys_write_disabled` 2114). The pure-Go backend has neither the module nor that commit step, so a no-op reports "writes enabled" while the intended cluster change silently does not happen. |

The database-level defaults reject for the same reasons, keeping the taxonomy
identical on both surfaces: `SetTransactionReportConflictingKeys`,
`SetTransactionAutomaticIdempotency`.

### Accepted but ignored — no-op (fails **safe**)

These are tracing/hints/priority, or relaxations whose absence keeps the
**stronger** guarantee (ignoring a durability/causal *relaxation* simply keeps full
durability / strong consistency). They are accepted as no-ops:

`DebugTransactionIdentifier`, `LogTransaction`, `TransactionLoggingEnable`,
`TransactionLoggingMaxFieldLength`,
`DebugRetryLogging`, `IncludePortInAddress`, `ServerRequestTracing`,
`ReadAheadDisable`, `ReadPriorityHigh`, `ReadPriorityLow`, `ReadPriorityNormal`,
`ReadServerSideCacheEnable`, `ReadServerSideCacheDisable`, `UseProvisionalProxies`,
`InitializeNewDatabase`, `ExpensiveClearCostEstimationEnable`,
`UsedDuringCommitProtectionDisable`, `CausalReadDisable`, `CausalWriteRisky`,
`DurabilityRisky`, `DurabilityDatacenter`, `DurabilityDevNullIsWebScale`.

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
