package fdb

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"

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
// accepted-but-ignored and are documented in API_PARITY.md. Resolves to FDB
// invalid_option (2007).
type UnsupportedOptionError struct{ Option string }

func (e *UnsupportedOptionError) Error() string {
	return "fdbgo: transaction option " + e.Option + " is not supported by the pure-Go client " +
		"(silently ignoring it would change security/access semantics; use the libfdb_c backend if you need it)"
}

// FDBCode reports the FDB error code (invalid_option, 2007) so callers that map on
// the numeric code treat it like libfdb_c rejecting an option.
func (e *UnsupportedOptionError) FDBCode() int { return 2007 }

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
	o.tx.inner.SetAccessSystemKeys()
	return nil
}

func (o goTransactionOptions) SetReadSystemKeys() error {
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
	return nil
}

func (o goTransactionOptions) SetSpecialKeySpaceRelaxed() error {
	return nil
}

func (o goTransactionOptions) SetSpecialKeySpaceEnableWrites() error {
	return nil
}

func (o goTransactionOptions) SetRawAccess() error {
	// Fails unsafe if ignored: raw access bypasses tenant-mode scoping; a silent
	// no-op would tenant-scope a read meant for the raw keyspace.
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
	return nil
}

func (o goTransactionOptions) SetInitializeNewDatabase() error {
	return nil
}

func (o goTransactionOptions) SetAuthorizationToken(_ string) error {
	// Fails unsafe if ignored: the request would be sent UNAUTHENTICATED (auth
	// bypass / wrong tenant scoping). The most dangerous silent no-op of the set.
	return &UnsupportedOptionError{Option: "authorization_token"}
}

func (o goTransactionOptions) SetSpanParent(_ []byte) error {
	return nil
}

func (o goTransactionOptions) SetExpensiveClearCostEstimationEnable() error {
	return nil
}

// DatabaseOptions is a handle for setting options that affect a Database.
type DatabaseOptions struct {
	db *internalDB
}

func (o DatabaseOptions) SetLocationCacheSize(_ int64) error { return nil }
func (o DatabaseOptions) SetMaxWatches(_ int64) error        { return nil }
func (o DatabaseOptions) SetDatacenterId(_ string) error     { return nil }
func (o DatabaseOptions) SetMachineId(_ string) error        { return nil }
func (o DatabaseOptions) SetSnapshotRywEnable() error        { return nil }
func (o DatabaseOptions) SetSnapshotRywDisable() error       { return nil }
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
func (o DatabaseOptions) SetTransactionCausalReadRisky() error                   { return nil }
func (o DatabaseOptions) SetTransactionLoggingMaxFieldLength(_ int64) error      { return nil }
func (o DatabaseOptions) SetTransactionReportConflictingKeys() error             { return nil }
func (o DatabaseOptions) SetTransactionAutomaticIdempotency() error              { return nil }
func (o DatabaseOptions) SetTransactionBypassUnreadable() error                  { return nil }
func (o DatabaseOptions) SetTransactionIncludePortInAddress() error              { return nil }
func (o DatabaseOptions) SetTransactionUsedDuringCommitProtectionDisable() error { return nil }
func (o DatabaseOptions) SetUseConfigDatabase() error                            { return nil }
func (o DatabaseOptions) SetTestCausalReadRisky(_ int64) error                   { return nil }
