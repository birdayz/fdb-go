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
	// No-op: the pure Go client has no client-side read-your-writes cache.
	// In the C binding, RYW is a client-side write buffer that lets reads
	// within the same transaction see pending writes without a server
	// round-trip. Disabling RYW tells the C client to skip its local cache.
	// Since we have no local write cache, this is a no-op. Note: reads in
	// our client do NOT see the transaction's own pending writes (they
	// always go to the server which only sees committed data).
	return nil
}

func (o TransactionOptions) SetAccessSystemKeys() error {
	return errNotSupported
}

func (o TransactionOptions) SetReadSystemKeys() error {
	return errNotSupported
}

func (o TransactionOptions) SetLockAware() error {
	o.tx.inner.SetLockAware(true)
	return nil
}

// SetReadLockAware allows reads on locked databases. In FDB, both
// lock_aware and read_lock_aware set CommitTransactionRef.lock_aware
// on the wire. The C binding distinguishes them client-side (lock_aware
// allows commits too), but the wire field is the same.
func (o TransactionOptions) SetReadLockAware() error {
	o.tx.inner.SetLockAware(true)
	return nil
}

func (o TransactionOptions) SetLogTransaction() error {
	return nil
}

func (o TransactionOptions) SetTransactionLoggingEnable(_ string) error {
	return nil
}

func (o TransactionOptions) SetSizeLimit(bytes int64) error {
	o.tx.inner.SetSizeLimit(bytes)
	return nil
}

func (o TransactionOptions) SetMaxRetryDelay(_ int64) error {
	// TODO: configurable max retry delay
	return nil
}

func (o TransactionOptions) SetSnapshotRywEnable() error {
	return nil
}

func (o TransactionOptions) SetSnapshotRywDisable() error {
	return nil
}

func (o TransactionOptions) SetUseGrvCache() error {
	return nil
}

func (o TransactionOptions) SetAutoThrottleTag(_ string) error {
	return nil
}

func (o TransactionOptions) SetTag(_ string) error {
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
type DatabaseOptions struct{}

func (o DatabaseOptions) SetLocationCacheSize(_ int64) error                     { return nil }
func (o DatabaseOptions) SetMaxWatches(_ int64) error                            { return nil }
func (o DatabaseOptions) SetDatacenterId(_ string) error                         { return nil }
func (o DatabaseOptions) SetMachineId(_ string) error                            { return nil }
func (o DatabaseOptions) SetSnapshotRywEnable() error                            { return nil }
func (o DatabaseOptions) SetSnapshotRywDisable() error                           { return nil }
func (o DatabaseOptions) SetTransactionTimeout(_ int64) error                    { return nil }
func (o DatabaseOptions) SetTransactionRetryLimit(_ int64) error                 { return nil }
func (o DatabaseOptions) SetTransactionMaxRetryDelay(_ int64) error              { return nil }
func (o DatabaseOptions) SetTransactionSizeLimit(_ int64) error                  { return nil }
func (o DatabaseOptions) SetTransactionCausalReadRisky() error                   { return nil }
func (o DatabaseOptions) SetTransactionLoggingMaxFieldLength(_ int64) error      { return nil }
func (o DatabaseOptions) SetTransactionReportConflictingKeys() error             { return nil }
func (o DatabaseOptions) SetTransactionAutomaticIdempotency() error              { return nil }
func (o DatabaseOptions) SetTransactionBypassUnreadable() error                  { return nil }
func (o DatabaseOptions) SetTransactionIncludePortInAddress() error              { return nil }
func (o DatabaseOptions) SetTransactionUsedDuringCommitProtectionDisable() error { return nil }
func (o DatabaseOptions) SetUseConfigDatabase() error                            { return nil }
func (o DatabaseOptions) SetTestCausalReadRisky(_ int64) error                   { return nil }
