# Pure-Go FDB client — option honored / unsupported / no-op matrix

This documents how every client option (`pkg/fdbgo/fdb/options.go`) behaves in the **pure-Go**
backend versus Apple's **`libfdb_c`** (the spec; verified against the C++ source at tag **7.3.75**).
It exists so a user never *mistakenly believes a libfdb option is active when the pure-Go backend
ignores it*.

## The design rule

The pure-Go client splits options into three behaviours, by one principle — **would silently
ignoring this option change what the caller gets?**

- **Honored** — implemented; takes effect.
- **`UnsupportedOptionError`** (FDB error 2007) — the option's silent omission would change
  **access, authorization, idempotency, storage-quota, or a conflict read-back** the caller relies
  on. The pure-Go client does **not** implement these, so rather than silently grant/deny something,
  it **fails loudly**. (`report_conflicting_keys`, `raw_access`, `automatic_idempotency`,
  `bypass_storage_quota`, `authorization_token`, and their database-default twins.)
- **Accepted & ignored (silent no-op)** — accepting the call but doing nothing is provably safe,
  because the option is one of:
  - a **no-op in `libfdb_c` too** (the C client also ignores it — `default: break`, no consumers;
    some are Deprecated);
  - **strictly safer when ignored** — the option *weakens* a guarantee (durability / causal
    consistency / commit dedup-on-fault) and ignoring it keeps the **stronger** behaviour;
  - **honored-in-C but an availability/perf/telemetry/locality hint** — it never changes query
    results, durability, isolation, conflict ranges, or key visibility (only e.g. which replica or
    proxy answers, trace output, a cache size).

The asymmetry is deliberate: the pure-Go client errors on the *grant/deny* family and accepts-and-
ignores the *hint / strictly-safer* family. The contracts are pinned by tests: the **unsafe family**
must return `*UnsupportedOptionError` (`fdb_test.go` — the reject-unsupported-options + DB-default
cases); **honored** options take effect (`options_internal_test.go`); and a **completeness guard**
(`options_matrix_test.go`, `TestOptionMatrix_DocumentsEveryOption`) fails CI if any `Set*` method is
missing a row in this file, so the matrix can't silently fall behind the code.

C++ references are into the FoundationDB 7.3.75 source: `T::setOption` =
`fdbclient/NativeAPI.actor.cpp` `Transaction::setOption` (~:6948); `RYW::setOptionImpl` =
`fdbclient/ReadYourWrites.actor.cpp` (~:2534); `DB::setOption` =
`fdbclient/NativeAPI.actor.cpp` `DatabaseContext::setOption` (~:2114).

## Transaction options (`goTransactionOptions`)

| Option (method) | Pure-Go | libfdb_c (C++) | Unsafe if ignored? | Notes |
|---|---|---|---|---|
| `SetTimeout` | **honored** | RYW:2569 sets `timeoutsEnabled`/`operationTimeout` | n/a | wires the tx deadline |
| `SetRetryLimit` | **honored** | RYW:2574 `maxRetries` | n/a | |
| `SetMaxRetryDelay` | **honored** | T:7062 `maxBackoff` | n/a | |
| `SetSizeLimit` | **honored** | T:7067 `sizeLimit` | n/a | |
| `SetPriorityBatch` | **honored** | T:6969 `PRIORITY_BATCH` | n/a | GRV priority |
| `SetPrioritySystemImmediate` | **honored** | T:6963 `PRIORITY_SYSTEM_IMMEDIATE` | n/a | |
| `SetCausalReadRisky` | **honored** | T:6958 GRV `FLAG_CAUSAL_READ_RISKY` | n/a | |
| `SetReadYourWritesDisable` | **honored** | RYW:2536 `readYourWritesDisabled` | n/a | |
| `SetNextWriteNoWriteConflictRange` | **honored** | RYW:2551 drops next write's conflict range | n/a | |
| `SetWriteConflictsDisabled` | **honored** | (Go-modeled) | n/a | |
| `SetAccessSystemKeys` | **honored** (+tenant guard) | RYW:2557 read+write system keys; T:7161 `raw_access`, throws on tenant | n/a | tenant guard mirrors `hasTenant`==false |
| `SetReadSystemKeys` | **honored** (+tenant guard) | RYW:2564 read system keys | n/a | |
| `SetLockAware` | **honored** | T:7072 `lockAware` | n/a | |
| `SetReadLockAware` | **honored** | T:7082 `lockAware` read-only | n/a | |
| `SetSnapshotRywEnable` | **honored** (counter) | RYW:2591 `snapshotRYWEnabled++` | n/a | true counter, matches C |
| `SetSnapshotRywDisable` | **honored** (counter) | RYW:2596 `snapshotRYWEnabled--` | n/a | |
| `SetUseGrvCache` | **honored** | T:7145 `USE_GRV_CACHE` | n/a | Go GRV cache is opt-in |
| `SetSkipGrvCache` | **honored** | T:7155 skip; skip wins | n/a | |
| `SetTag` | **honored** | T:7123 `addTag` | n/a | |
| `SetBypassUnreadable` | **honored** | RYW:2611 `bypassUnreadable` | n/a | |
| `SetSpanParent` | **honored** | T:7128 span link | n/a | trace span linkage |
| `SetReportConflictingKeys` | **`UnsupportedOptionError`** | T:7135 `reportConflictingKeys` | **UNSAFE** | Go has no conflicting-key read-back; errors rather than silently drop it |
| `SetRawAccess` | **`UnsupportedOptionError`** | T:7161 `rawAccess` | **UNSAFE** | would change which keyspace/tenant a read targets |
| `SetAutomaticIdempotency` | **`UnsupportedOptionError`** | T:7196 generates idempotency id | **UNSAFE** | silently dropping the idempotency contract is dangerous on retry |
| `SetBypassStorageQuota` | **`UnsupportedOptionError`** | T:7173 `bypassStorageQuota` commit flag | **UNSAFE** | quota bypass must not be silently assumed |
| `SetAuthorizationToken` | **`UnsupportedOptionError`** | T:7177 sets `authToken` | **UNSAFE** | most dangerous — silently unauthenticated |
| `SetCausalWriteRisky` | accept & ignore | T:6973 `causalWriteRisky=true` | strictly-safer | ignoring keeps `makeSelfConflicting()` + commit-unknown retry (NativeAPI:6858/6734) — **stronger** durability/retry-safety |
| `SetCausalReadDisable` | accept & ignore | **no consumer — `default: break` in both T and RYW** | safe | C ignores it too |
| `SetDurabilityRisky` | accept & ignore | **`default: break`, no consumer** (Deprecated) | safe | C ignores it too |
| `SetDurabilityDatacenter` | accept & ignore | **`default: break`, no consumer** | safe | C ignores it too |
| `SetDurabilityDevNullIsWebScale` | accept & ignore | **`default: break`, no consumer** (Deprecated) | safe | C ignores it too |
| `SetUseProvisionalProxies` | accept & ignore | T:7099 `FLAG_USE_PROVISIONAL_PROXIES`, throws on tenant | availability-hint, strictly-safer | **honored in C** but ignoring → default (don't use provisional proxies); only reduces availability during recovery, never changes results/durability/isolation/visibility |
| `SetReadAheadDisable` | accept & ignore | RYW:2545 `readAheadDisabled` (Deprecated in fdb.options) | safe (perf) | Go has no read-ahead |
| `SetUsedDuringCommitProtectionDisable` | accept & ignore | RYW:2598 disables a concurrent-op-during-commit *debug* detector | safe | Go never implemented the protection; disabling a non-existent guard is a no-op |
| `SetReadPriorityHigh` / `SetReadPriorityLow` / `SetReadPriorityNormal` | accept & ignore | `ReadType` storage-scheduling hint | safe (perf hint) | does not change results |
| `SetReadServerSideCacheEnable` / `SetReadServerSideCacheDisable` | accept & ignore | `cacheResult` storage hint | safe (perf hint) | |
| `SetExpensiveClearCostEstimationEnable` | accept & ignore | metrics estimation | safe (telemetry) | |
| `SetDebugTransactionIdentifier` | accept & ignore | T:6996 trace log id | safe (telemetry) | |
| `SetLogTransaction` | accept & ignore | T:7027 trace log | safe (telemetry) | |
| `SetTransactionLoggingEnable` | accept & ignore | T:6992 id + log | safe (telemetry) | |
| `SetTransactionLoggingMaxFieldLength` | accept & ignore | T:7037 trace field cap | safe (telemetry) | |
| `SetServerRequestTracing` | accept & ignore | T:7051 trace id | safe (telemetry) | |
| `SetDebugRetryLogging` | accept & ignore | RYW:2580 retry-log name | safe (telemetry) | |
| `SetAutoThrottleTag` | accept & ignore | T:7117 add throttle tag | safe (throttle hint) | (`SetTag` is honored; the *auto-throttle* aspect is the hint) |
| `SetIncludePortInAddress` | accept & ignore | deprecated / default-on ≥630 | safe | |
| `SetSpecialKeySpaceRelaxed` | accept & ignore | RYW:2603 relaxes special-key-space range checks | safe | Go has no special-key-space module |
| `SetSpecialKeySpaceEnableWrites` | accept & ignore | RYW:2607 | safe | same — no module |
| `SetInitializeNewDatabase` | accept & ignore | T:6950 RV=0 + causalWriteRisky | safe | db-bootstrap tool only; never used by app code |

## Database options (`DatabaseOptions`)

| Option (method) | Pure-Go | libfdb_c (C++) | Unsafe if ignored? | Notes |
|---|---|---|---|---|
| `SetTransactionTimeout` | **honored** (tx default) | DB:2156 default → per-tx timeout | n/a | applied to each tx |
| `SetTransactionRetryLimit` | **honored** (tx default) | DB default | n/a | |
| `SetTransactionMaxRetryDelay` | **honored** (tx default) | DB default | n/a | |
| `SetTransactionSizeLimit` | **honored** (tx default) | DB default | n/a | |
| `SetReadSystemKeys` | **honored** (tx default) | DB default → READ_SYSTEM_KEYS | n/a | |
| `SetTransactionReportConflictingKeys` | **`UnsupportedOptionError`** | DB default twin | **UNSAFE** | mirrors the tx option |
| `SetTransactionAutomaticIdempotency` | **`UnsupportedOptionError`** | DB default twin | **UNSAFE** | |
| `SetTransactionCausalReadRisky` | accept & ignore | DB default → CAUSAL_READ_RISKY | strictly-safer | every tx keeps full causal consistency the caller opted to relax |
| `SetSnapshotRywEnable` / `SetSnapshotRywDisable` | accept & ignore | DB:2156 default counter | safe | Go's per-tx default already matches C's (enabled); net result identical |
| `SetTransactionBypassUnreadable` | accept & ignore | DB default | safe | per the tx-level row |
| `SetTransactionIncludePortInAddress` | accept & ignore | DB default | safe | |
| `SetTransactionUsedDuringCommitProtectionDisable` | accept & ignore | DB default | safe | |
| `SetTransactionLoggingMaxFieldLength` | accept & ignore | DB default | safe (telemetry) | |
| `SetLocationCacheSize` | accept & ignore | DB:2123 cache size | safe (perf) | |
| `SetMaxWatches` | accept & ignore | DB:2138 watch cap | safe (resource cap) | never changes data |
| `SetDatacenterId` | accept & ignore | DB:2142 `clientLocality` dcId | safe (locality) | affects replica preference, not results |
| `SetMachineId` | accept & ignore | DB:2126 `clientLocality` | safe (locality) | |
| `SetUseConfigDatabase` | accept & ignore | DB:2161 `useConfigDatabase` | safe (tools-only) | |
| `SetTestCausalReadRisky` | accept & ignore | DB:2166 test knob | safe (test-only) | |
