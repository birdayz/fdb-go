package fdb

import "github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/client"

// TransactionOptions is a handle for setting options that affect a
// Transaction. Obtained via Transaction.Options().
type TransactionOptions struct {
	tx *transaction
}

func (o TransactionOptions) SetTimeout(milliseconds int64) error {
	o.tx.inner.SetTimeout(milliseconds)
	return nil
}

func (o TransactionOptions) SetRetryLimit(retries int64) error {
	o.tx.inner.SetRetryLimit(retries)
	return nil
}

func (o TransactionOptions) SetPriorityBatch() error {
	o.tx.inner.SetPriority(client.PriorityBatch)
	return nil
}

func (o TransactionOptions) SetPrioritySystemImmediate() error {
	o.tx.inner.SetPriority(client.PrioritySystemImmediate)
	return nil
}

func (o TransactionOptions) SetDebugTransactionIdentifier(_ string) error {
	return nil
}

func (o TransactionOptions) SetNextWriteNoWriteConflictRange() error {
	o.tx.inner.SetNextWriteNoWriteConflictRange()
	return nil
}

func (o TransactionOptions) SetCausalReadRisky() error {
	o.tx.inner.SetCausalReadRisky(true)
	return nil
}

func (o TransactionOptions) SetReadYourWritesDisable() error {
	o.tx.inner.SetReadYourWritesDisable()
	return nil
}

func (o TransactionOptions) EnsureMutationCapacity(n int) {
	o.tx.inner.EnsureMutationCapacity(n)
}

func (o TransactionOptions) SetWriteConflictsDisabled() {
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
func (o TransactionOptions) SetAccessSystemKeys() error {
	o.tx.inner.SetAccessSystemKeys()
	return nil
}

func (o TransactionOptions) SetReadSystemKeys() error {
	o.tx.inner.SetReadSystemKeys()
	return nil
}

func (o TransactionOptions) SetLockAware() error {
	o.tx.inner.SetLockAware(true)
	return nil
}

// SetReadLockAware allows reads on locked databases. Unlike SetLockAware,
// this does NOT set lock_aware on the commit path — in C++ FDB,
// read_lock_aware only bypasses the locked-database check for reads,
// not commits.
func (o TransactionOptions) SetReadLockAware() error {
	o.tx.inner.SetReadLockAware(true)
	return nil
}

func (o TransactionOptions) SetLogTransaction() error {
	return nil
}

func (o TransactionOptions) SetTransactionLoggingEnable(_ string) error {
	return nil
}

func (o TransactionOptions) SetSizeLimit(limit int64) error {
	o.tx.inner.SetSizeLimit(limit)
	return nil
}

func (o TransactionOptions) SetMaxRetryDelay(ms int64) error {
	o.tx.inner.SetMaxRetryDelay(ms)
	return nil
}

func (o TransactionOptions) SetSnapshotRywEnable() error {
	// libfdb_c models this as a counter (enabledCount++), not a no-op: it undoes one prior
	// SetSnapshotRywDisable. RFC-061.
	o.tx.inner.SetSnapshotRYWEnable()
	return nil
}

func (o TransactionOptions) SetSnapshotRywDisable() error {
	o.tx.inner.SetSnapshotRYWDisable()
	return nil
}

func (o TransactionOptions) SetUseGrvCache() error {
	return nil
}

func (o TransactionOptions) SetAutoThrottleTag(_ string) error {
	return nil
}

func (o TransactionOptions) SetTag(tag string) error {
	o.tx.inner.SetTag(tag)
	return nil
}

func (o TransactionOptions) SetReportConflictingKeys() error {
	return nil
}

func (o TransactionOptions) SetSpecialKeySpaceRelaxed() error {
	return nil
}

func (o TransactionOptions) SetSpecialKeySpaceEnableWrites() error {
	return nil
}

func (o TransactionOptions) SetRawAccess() error {
	return nil
}

func (o TransactionOptions) SetBypassUnreadable() error {
	o.tx.inner.SetBypassUnreadable(true)
	return nil
}

func (o TransactionOptions) SetAutomaticIdempotency() error {
	return nil
}

func (o TransactionOptions) SetDebugRetryLogging(_ string) error {
	return nil
}

func (o TransactionOptions) SetIncludePortInAddress() error {
	return nil
}

func (o TransactionOptions) SetCausalReadDisable() error {
	return nil
}

func (o TransactionOptions) SetCausalWriteRisky() error {
	return nil
}

func (o TransactionOptions) SetDurabilityRisky() error {
	return nil
}

func (o TransactionOptions) SetDurabilityDatacenter() error {
	return nil
}

func (o TransactionOptions) SetDurabilityDevNullIsWebScale() error {
	return nil
}

func (o TransactionOptions) SetTransactionLoggingMaxFieldLength(_ int64) error {
	return nil
}

func (o TransactionOptions) SetServerRequestTracing() error {
	return nil
}

func (o TransactionOptions) SetUsedDuringCommitProtectionDisable() error {
	return nil
}

func (o TransactionOptions) SetReadAheadDisable() error {
	return nil
}

func (o TransactionOptions) SetReadPriorityHigh() error {
	return nil
}

func (o TransactionOptions) SetReadPriorityLow() error {
	return nil
}

func (o TransactionOptions) SetReadPriorityNormal() error {
	return nil
}

func (o TransactionOptions) SetReadServerSideCacheEnable() error {
	return nil
}

func (o TransactionOptions) SetReadServerSideCacheDisable() error {
	return nil
}

func (o TransactionOptions) SetUseProvisionalProxies() error {
	return nil
}

func (o TransactionOptions) SetBypassStorageQuota() error {
	return nil
}

func (o TransactionOptions) SetInitializeNewDatabase() error {
	return nil
}

func (o TransactionOptions) SetAuthorizationToken(_ string) error {
	return nil
}

func (o TransactionOptions) SetSpanParent(_ []byte) error {
	return nil
}

func (o TransactionOptions) SetExpensiveClearCostEstimationEnable() error {
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
