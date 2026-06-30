package fdb

import "fdb.dev/pkg/fdbgo/client"

// TransactionOptions sets options that affect a Transaction, obtained via
// ReadTransaction.Options(). It is an interface so a non-pure-Go backend can
// provide its own implementation (RFC-109): the pure-Go goTransactionOptions
// delegates to the client transaction — no-op'ing options the pure-Go client
// does not model — while the libfdb_c backend forwards each call by raw integer
// opcode (RFC-109 §"Options by raw integer"). Every option is on the interface
// precisely so the cgo backend can faithfully forward even those the pure-Go
// client ignores (e.g. SetServerRequestTracing, SetReadAheadDisable).
type TransactionOptions interface {
	SetTimeout(milliseconds int64) error
	SetRetryLimit(retries int64) error
	SetPriorityBatch() error
	SetPrioritySystemImmediate() error
	SetDebugTransactionIdentifier(id string) error
	SetNextWriteNoWriteConflictRange() error
	SetCausalReadRisky() error
	SetReadYourWritesDisable() error
	EnsureMutationCapacity(n int)
	SetWriteConflictsDisabled()
	SetAccessSystemKeys() error
	SetReadSystemKeys() error
	SetLockAware() error
	SetReadLockAware() error
	SetLogTransaction() error
	SetTransactionLoggingEnable(id string) error
	SetSizeLimit(limit int64) error
	SetMaxRetryDelay(ms int64) error
	SetSnapshotRywEnable() error
	SetSnapshotRywDisable() error
	SetUseGrvCache() error
	SetSkipGrvCache() error
	SetAutoThrottleTag(tag string) error
	SetTag(tag string) error
	SetReportConflictingKeys() error
	SetSpecialKeySpaceRelaxed() error
	SetSpecialKeySpaceEnableWrites() error
	SetRawAccess() error
	SetBypassUnreadable() error
	SetAutomaticIdempotency() error
	SetDebugRetryLogging(loggerName string) error
	SetIncludePortInAddress() error
	SetCausalReadDisable() error
	SetCausalWriteRisky() error
	SetDurabilityRisky() error
	SetDurabilityDatacenter() error
	SetDurabilityDevNullIsWebScale() error
	SetTransactionLoggingMaxFieldLength(maxFieldLength int64) error
	SetServerRequestTracing() error
	SetUsedDuringCommitProtectionDisable() error
	SetReadAheadDisable() error
	SetReadPriorityHigh() error
	SetReadPriorityLow() error
	SetReadPriorityNormal() error
	SetReadServerSideCacheEnable() error
	SetReadServerSideCacheDisable() error
	SetUseProvisionalProxies() error
	SetBypassStorageQuota() error
	SetInitializeNewDatabase() error
	SetAuthorizationToken(token string) error
	SetSpanParent(parent []byte) error
	SetExpensiveClearCostEstimationEnable() error
}

// UnsupportedOptionError is returned by the pure-Go backend for a transaction
// option that alters security / access / idempotency semantics but that the
// pure-Go client does not model. These fail LOUD instead of the old silent no-op:
// silently ignoring such an option is a migration trap — e.g. an ignored
// authorization token means the request is sent unauthenticated (auth bypass), and
// an ignored raw-access flag means a tenant-scoped read instead of the raw
// keyspace. The libfdb_c backend forwards these options normally; pure-Go callers
// that need them must use that backend. Options that fail SAFE when ignored (the
// causal/durability knobs — ignoring keeps the STRONGER guarantee) remain
// accepted-but-ignored and are documented option-by-option in OPTIONS.md. Resolves
// to FDB invalid_option (2007).
type UnsupportedOptionError struct{ Option string }

func (e *UnsupportedOptionError) Error() string {
	return "fdbgo: transaction option " + e.Option + " is not supported by the pure-Go client " +
		"(silently ignoring it would change security/access semantics; use the libfdb_c backend if you need it)"
}

// FDBCode reports the FDB error code (invalid_option, 2007) so callers that map on
// the numeric code treat it like libfdb_c rejecting an option.
func (e *UnsupportedOptionError) FDBCode() int { return 2007 }

// TenantOptionError is returned when a system-key-access option (READ_SYSTEM_KEYS /
// ACCESS_SYSTEM_KEYS) is set on a tenant transaction. C++ setOption throws invalid_option (2007)
// here — "System key access implies raw access", which is incompatible with tenant scoping
// (NativeAPI.actor.cpp:7159-7171). Unlike UnsupportedOptionError, the option IS supported by the
// pure-Go client; it is just invalid in this context.
type TenantOptionError struct{ Option string }

func (e *TenantOptionError) Error() string {
	return "fdbgo: transaction option " + e.Option + " is not valid on a tenant transaction (invalid_option)"
}

// FDBCode reports invalid_option (2007), matching libfdb_c's throw.
func (e *TenantOptionError) FDBCode() int { return 2007 }

// goTransactionOptions is the pure-Go implementation of TransactionOptions,
// delegating to the underlying client transaction.
type goTransactionOptions struct {
	tx *transaction
}

func (o goTransactionOptions) SetTimeout(milliseconds int64) error {
	o.tx.inner.SetTimeout(milliseconds)
	return nil
}

func (o goTransactionOptions) SetRetryLimit(retries int64) error {
	o.tx.inner.SetRetryLimit(retries)
	return nil
}

func (o goTransactionOptions) SetPriorityBatch() error {
	o.tx.inner.SetPriority(client.PriorityBatch)
	return nil
}

func (o goTransactionOptions) SetPrioritySystemImmediate() error {
	o.tx.inner.SetPriority(client.PrioritySystemImmediate)
	return nil
}

func (o goTransactionOptions) SetDebugTransactionIdentifier(_ string) error {
	return nil
}

func (o goTransactionOptions) SetNextWriteNoWriteConflictRange() error {
	o.tx.inner.SetNextWriteNoWriteConflictRange()
	return nil
}

func (o goTransactionOptions) SetCausalReadRisky() error {
	o.tx.inner.SetCausalReadRisky(true)
	return nil
}

func (o goTransactionOptions) SetReadYourWritesDisable() error {
	o.tx.inner.SetReadYourWritesDisable()
	return nil
}

func (o goTransactionOptions) EnsureMutationCapacity(n int) {
	o.tx.inner.EnsureMutationCapacity(n)
}

func (o goTransactionOptions) SetWriteConflictsDisabled() {
	o.tx.inner.SetWriteConflictsDisabled()
}

// SetAccessSystemKeys grants read AND write access to the \xff system keyspace.
// It does NOT imply lock-aware: in the C client ACCESS_SYSTEM_KEYS sets only
// rawAccess (NativeAPI.actor.cpp setOption / ReadYourWrites.actor.cpp), and
// LOCK_AWARE / READ_LOCK_AWARE are independent options. Callers that must commit
// or read against a *locked* database set SetLockAware()/SetReadLockAware()
// explicitly, exactly as a Java/CGo app does — see the tenant operations in
// database.go. (Previously this method auto-set lock-aware, diverging from C;
// that coupling is removed.)
func (o goTransactionOptions) SetAccessSystemKeys() error {
	// C++ setOption throws invalid_option when system-key access is requested on a tenant
	// transaction (NativeAPI.actor.cpp:7159-7171) — system-key access implies raw access, which
	// can't be tenant-scoped. Reject eagerly to match, before mutating any flag.
	if o.tx.inner.TenantId() != client.NoTenantID {
		return &TenantOptionError{Option: "access_system_keys"}
	}
	o.tx.inner.SetAccessSystemKeys()
	return nil
}

func (o goTransactionOptions) SetReadSystemKeys() error {
	if o.tx.inner.TenantId() != client.NoTenantID {
		return &TenantOptionError{Option: "read_system_keys"}
	}
	o.tx.inner.SetReadSystemKeys()
	return nil
}

func (o goTransactionOptions) SetLockAware() error {
	o.tx.inner.SetLockAware(true)
	return nil
}

// SetReadLockAware allows reads on locked databases. Unlike SetLockAware,
// this does NOT set lock_aware on the commit path — in C++ FDB,
// read_lock_aware only bypasses the locked-database check for reads,
// not commits.
func (o goTransactionOptions) SetReadLockAware() error {
	o.tx.inner.SetReadLockAware(true)
	return nil
}

func (o goTransactionOptions) SetLogTransaction() error {
	return nil
}

func (o goTransactionOptions) SetTransactionLoggingEnable(_ string) error {
	return nil
}

func (o goTransactionOptions) SetSizeLimit(limit int64) error {
	o.tx.inner.SetSizeLimit(limit)
	return nil
}

func (o goTransactionOptions) SetMaxRetryDelay(ms int64) error {
	o.tx.inner.SetMaxRetryDelay(ms)
	return nil
}

func (o goTransactionOptions) SetSnapshotRywEnable() error {
	// libfdb_c models this as a counter (enabledCount++), not a no-op: it undoes one prior
	// SetSnapshotRywDisable. RFC-061.
	o.tx.inner.SetSnapshotRYWEnable()
	return nil
}

func (o goTransactionOptions) SetSnapshotRywDisable() error {
	o.tx.inner.SetSnapshotRYWDisable()
	return nil
}

func (o goTransactionOptions) SetUseGrvCache() error {
	o.tx.inner.SetUseGrvCache()
	return nil
}

func (o goTransactionOptions) SetSkipGrvCache() error {
	o.tx.inner.SetSkipGrvCache()
	return nil
}

func (o goTransactionOptions) SetAutoThrottleTag(_ string) error {
	return nil
}

func (o goTransactionOptions) SetTag(tag string) error {
	o.tx.inner.SetTag(tag)
	return nil
}

func (o goTransactionOptions) SetReportConflictingKeys() error {
	// Fails unsafe if ignored: the caller sets this to read back the conflicting key ranges via
	// the \xff\xff/transaction/conflicting_keys/ special-key module after a not_committed. The
	// pure-Go client implements neither the commit-request field (CommitTransactionRef
	// .report_conflicting_keys, which the proxy reads — CommitProxyServer.actor.cpp:2448) nor the
	// special-key read-back, so silently no-op'ing would leave the caller with empty/erroring
	// results and no signal. Report it, matching SetRawAccess/SetAuthorizationToken; the libfdb_c
	// backend forwards it normally.
	return &UnsupportedOptionError{Option: "report_conflicting_keys"}
}

func (o goTransactionOptions) SetSpecialKeySpaceRelaxed() error {
	return nil
}

func (o goTransactionOptions) SetSpecialKeySpaceEnableWrites() error {
	return nil
}

func (o goTransactionOptions) SetRawAccess() error {
	// Fails unsafe if ignored: raw access bypasses tenant-mode scoping; a silent
	// no-op would tenant-scope a read meant for the raw keyspace. NOTE: this is
	// intentionally stricter than libfdb_c, which rejects RAW_ACCESS only when a
	// tenant is set and otherwise accepts it as a no-op (NativeAPI.actor.cpp). We
	// reject unconditionally — fail-safe and simpler; a no-tenant caller that set it
	// defensively gets a clear error rather than a silent mismatch.
	return &UnsupportedOptionError{Option: "raw_access"}
}

func (o goTransactionOptions) SetBypassUnreadable() error {
	o.tx.inner.SetBypassUnreadable(true)
	return nil
}

func (o goTransactionOptions) SetAutomaticIdempotency() error {
	// Fails unsafe if ignored: the caller expects automatic idempotency IDs so a
	// commit_unknown_result can be safely retried; the pure-Go client does not
	// generate them, so report it rather than imply a guarantee it can't keep.
	return &UnsupportedOptionError{Option: "automatic_idempotency"}
}

func (o goTransactionOptions) SetDebugRetryLogging(_ string) error {
	return nil
}

func (o goTransactionOptions) SetIncludePortInAddress() error {
	return nil
}

func (o goTransactionOptions) SetCausalReadDisable() error {
	return nil
}

func (o goTransactionOptions) SetCausalWriteRisky() error {
	return nil
}

func (o goTransactionOptions) SetDurabilityRisky() error {
	return nil
}

func (o goTransactionOptions) SetDurabilityDatacenter() error {
	return nil
}

func (o goTransactionOptions) SetDurabilityDevNullIsWebScale() error {
	return nil
}

func (o goTransactionOptions) SetTransactionLoggingMaxFieldLength(_ int64) error {
	return nil
}

func (o goTransactionOptions) SetServerRequestTracing() error {
	return nil
}

func (o goTransactionOptions) SetUsedDuringCommitProtectionDisable() error {
	return nil
}

func (o goTransactionOptions) SetReadAheadDisable() error {
	return nil
}

func (o goTransactionOptions) SetReadPriorityHigh() error {
	return nil
}

func (o goTransactionOptions) SetReadPriorityLow() error {
	return nil
}

func (o goTransactionOptions) SetReadPriorityNormal() error {
	return nil
}

func (o goTransactionOptions) SetReadServerSideCacheEnable() error {
	return nil
}

func (o goTransactionOptions) SetReadServerSideCacheDisable() error {
	return nil
}

func (o goTransactionOptions) SetUseProvisionalProxies() error {
	return nil
}

func (o goTransactionOptions) SetBypassStorageQuota() error {
	// Fails unsafe if ignored: the caller is forcing a write through a full storage quota. The
	// pure-Go client never sets FLAG_BYPASS_STORAGE_QUOTA (0x4) on the commit request
	// (commitpath.go), so a silent no-op ships the commit WITHOUT the bypass and it is rejected
	// with storage_quota_exceeded — the requested write-admission is dropped with no signal.
	// Report it, matching SetRawAccess/SetReportConflictingKeys; the libfdb_c backend forwards it.
	return &UnsupportedOptionError{Option: "bypass_storage_quota"}
}

func (o goTransactionOptions) SetInitializeNewDatabase() error {
	return nil
}

func (o goTransactionOptions) SetAuthorizationToken(_ string) error {
	// Fails unsafe if ignored: the request would be sent UNAUTHENTICATED (auth
	// bypass / wrong tenant scoping). The most dangerous silent no-op of the set.
	return &UnsupportedOptionError{Option: "authorization_token"}
}

func (o goTransactionOptions) SetSpanParent(parent []byte) error {
	// RFC-115 §4: inject a parent trace context (33-byte IncludeVersion-serialized
	// SpanContext) so this transaction's span links to the caller's trace.
	return o.tx.inner.SetSpanParent(parent)
}

func (o goTransactionOptions) SetExpensiveClearCostEstimationEnable() error {
	return nil
}

// DatabaseOptions is a handle for setting options that affect a Database.
type DatabaseOptions struct {
	db *internalDB
}

func (o DatabaseOptions) SetLocationCacheSize(_ int64) error { return nil }

// SetMaxWatches caps the number of concurrently-outstanding watches for this Database; a new watch
// over the cap fails with too_many_watches (1032). Matches FDB_DB_OPTION_MAX_WATCHES (default 10000).
func (o DatabaseOptions) SetMaxWatches(n int64) error {
	if o.db != nil {
		o.db.inner.SetMaxWatches(n)
	}
	return nil
}
func (o DatabaseOptions) SetDatacenterId(_ string) error { return nil }
func (o DatabaseOptions) SetMachineId(_ string) error    { return nil }
func (o DatabaseOptions) SetSnapshotRywEnable() error {
	if o.db != nil {
		o.db.txDefaults.snapshotRywDisableNet--
	}
	return nil
}

// SetSnapshotRywDisable is an honored DB default (codex #331): libfdb_c disables snapshot
// read-your-writes on every new transaction, changing snapshot reads after a local write — a
// read-semantics change, not a hint. Propagate it via txDefaults rather than silently dropping it.
func (o DatabaseOptions) SetSnapshotRywDisable() error {
	if o.db != nil {
		o.db.txDefaults.snapshotRywDisableNet++
	}
	return nil
}

func (o DatabaseOptions) SetReadSystemKeys() error {
	if o.db != nil {
		o.db.txDefaults.readSystemKeys = true
	}
	return nil
}

func (o DatabaseOptions) SetTransactionTimeout(ms int64) error {
	if o.db != nil {
		o.db.txDefaults.timeout = ms
	}
	return nil
}

func (o DatabaseOptions) SetTransactionRetryLimit(retries int64) error {
	if o.db != nil {
		o.db.txDefaults.retryLimit = retries
		o.db.txDefaults.hasRetryLimit = true
	}
	return nil
}

func (o DatabaseOptions) SetTransactionMaxRetryDelay(ms int64) error {
	if o.db != nil {
		o.db.txDefaults.maxRetryDelay = ms
	}
	return nil
}

func (o DatabaseOptions) SetTransactionSizeLimit(bytes int64) error {
	if o.db != nil {
		o.db.txDefaults.sizeLimit = bytes
	}
	return nil
}

// SetTransactionCausalReadRisky is an honored DB default (FDB C++ dev review, #331): it sets the
// GRV causal-read-risky flag on every new transaction, relaxing the read version (a read-version /
// staleness change). The per-tx SetCausalReadRisky IS honored, so the DB default must propagate too
// rather than be silently dropped (unlike causal_write_risky, whose per-tx form is a fail-safe no-op).
func (o DatabaseOptions) SetTransactionCausalReadRisky() error {
	if o.db != nil {
		o.db.txDefaults.causalReadRisky = true
	}
	return nil
}
func (o DatabaseOptions) SetTransactionLoggingMaxFieldLength(_ int64) error { return nil }

// DB-level defaults for the options the per-transaction setters reject (RFC-111 P1.3): keep the
// fail-loud taxonomy consistent across both surfaces — a DB default that silently swallows
// report_conflicting_keys / automatic_idempotency would re-open the same migration trap the
// per-tx SetReportConflictingKeys/SetAutomaticIdempotency now guard against.
func (o DatabaseOptions) SetTransactionReportConflictingKeys() error {
	return &UnsupportedOptionError{Option: "report_conflicting_keys"}
}

func (o DatabaseOptions) SetTransactionAutomaticIdempotency() error {
	return &UnsupportedOptionError{Option: "automatic_idempotency"}
}

// SetTransactionBypassUnreadable is an honored DB default (codex #331): libfdb_c sets
// bypass_unreadable on every new transaction, turning accessed_unreadable failures into reads —
// observable behaviour, not a hint. Propagate it via txDefaults.
func (o DatabaseOptions) SetTransactionBypassUnreadable() error {
	if o.db != nil {
		o.db.txDefaults.bypassUnreadable = true
	}
	return nil
}
func (o DatabaseOptions) SetTransactionIncludePortInAddress() error              { return nil }
func (o DatabaseOptions) SetTransactionUsedDuringCommitProtectionDisable() error { return nil }
func (o DatabaseOptions) SetUseConfigDatabase() error                            { return nil }
func (o DatabaseOptions) SetTestCausalReadRisky(_ int64) error                   { return nil }
