//go:build cgo

package libfdbc

import cgofdb "github.com/apple/foundationdb/bindings/go/src/fdb"

// txOptions adapts cgofdb.TransactionOptions to fdb.TransactionOptions. All but
// three methods forward 1:1 to the identically-named cgofdb generated setter
// (which sets the option by the same fdb.options integer code under the hood).
// The exceptions are documented at their methods: cgofdb has no setter for
// SkipGrvCache, and WriteConflictsDisabled / EnsureMutationCapacity have no
// libfdb_c analog — those are no-ops here (a documented v1 limitation).
type txOptions struct {
	o cgofdb.TransactionOptions
}

func (o txOptions) SetTimeout(milliseconds int64) error { return o.o.SetTimeout(milliseconds) }
func (o txOptions) SetRetryLimit(retries int64) error   { return o.o.SetRetryLimit(retries) }
func (o txOptions) SetPriorityBatch() error             { return o.o.SetPriorityBatch() }
func (o txOptions) SetPrioritySystemImmediate() error   { return o.o.SetPrioritySystemImmediate() }
func (o txOptions) SetDebugTransactionIdentifier(id string) error {
	return o.o.SetDebugTransactionIdentifier(id)
}

func (o txOptions) SetNextWriteNoWriteConflictRange() error {
	return o.o.SetNextWriteNoWriteConflictRange()
}
func (o txOptions) SetCausalReadRisky() error       { return o.o.SetCausalReadRisky() }
func (o txOptions) SetReadYourWritesDisable() error { return o.o.SetReadYourWritesDisable() }

// EnsureMutationCapacity pre-sizes the pure-Go client's mutation buffer; libfdb_c
// manages its own buffer, so there is nothing to do here.
func (o txOptions) EnsureMutationCapacity(int) {}

// SetWriteConflictsDisabled has no single libfdb_c transaction-option analog (the
// pure-Go client models it as an internal flag); no-op on this backend.
func (o txOptions) SetWriteConflictsDisabled() {}

func (o txOptions) SetAccessSystemKeys() error { return o.o.SetAccessSystemKeys() }
func (o txOptions) SetReadSystemKeys() error   { return o.o.SetReadSystemKeys() }
func (o txOptions) SetLockAware() error        { return o.o.SetLockAware() }
func (o txOptions) SetReadLockAware() error    { return o.o.SetReadLockAware() }
func (o txOptions) SetLogTransaction() error   { return o.o.SetLogTransaction() }
func (o txOptions) SetTransactionLoggingEnable(id string) error {
	return o.o.SetTransactionLoggingEnable(id)
}
func (o txOptions) SetSizeLimit(limit int64) error  { return o.o.SetSizeLimit(limit) }
func (o txOptions) SetMaxRetryDelay(ms int64) error { return o.o.SetMaxRetryDelay(ms) }
func (o txOptions) SetSnapshotRywEnable() error     { return o.o.SetSnapshotRywEnable() }
func (o txOptions) SetSnapshotRywDisable() error    { return o.o.SetSnapshotRywDisable() }
func (o txOptions) SetUseGrvCache() error           { return o.o.SetUseGrvCache() }

// SetSkipGrvCache is a real FDB transaction option, but this cgofdb version
// exposes no typed setter for it (and the raw setOpt is unexported). It is a
// no-op on this backend — a documented v1 divergence from the pure-Go client,
// which honors it. The escape hatch trusts libfdb_c's GRV-cache default.
func (o txOptions) SetSkipGrvCache() error { return nil }

func (o txOptions) SetAutoThrottleTag(tag string) error { return o.o.SetAutoThrottleTag(tag) }
func (o txOptions) SetTag(tag string) error             { return o.o.SetTag(tag) }
func (o txOptions) SetReportConflictingKeys() error     { return o.o.SetReportConflictingKeys() }
func (o txOptions) SetSpecialKeySpaceRelaxed() error    { return o.o.SetSpecialKeySpaceRelaxed() }
func (o txOptions) SetSpecialKeySpaceEnableWrites() error {
	return o.o.SetSpecialKeySpaceEnableWrites()
}
func (o txOptions) SetRawAccess() error            { return o.o.SetRawAccess() }
func (o txOptions) SetBypassUnreadable() error     { return o.o.SetBypassUnreadable() }
func (o txOptions) SetAutomaticIdempotency() error { return o.o.SetAutomaticIdempotency() }
func (o txOptions) SetDebugRetryLogging(loggerName string) error {
	return o.o.SetDebugRetryLogging(loggerName)
}
func (o txOptions) SetIncludePortInAddress() error { return o.o.SetIncludePortInAddress() }
func (o txOptions) SetCausalReadDisable() error    { return o.o.SetCausalReadDisable() }
func (o txOptions) SetCausalWriteRisky() error     { return o.o.SetCausalWriteRisky() }
func (o txOptions) SetDurabilityRisky() error      { return o.o.SetDurabilityRisky() }
func (o txOptions) SetDurabilityDatacenter() error { return o.o.SetDurabilityDatacenter() }
func (o txOptions) SetDurabilityDevNullIsWebScale() error {
	return o.o.SetDurabilityDevNullIsWebScale()
}

func (o txOptions) SetTransactionLoggingMaxFieldLength(maxFieldLength int64) error {
	return o.o.SetTransactionLoggingMaxFieldLength(maxFieldLength)
}
func (o txOptions) SetServerRequestTracing() error { return o.o.SetServerRequestTracing() }
func (o txOptions) SetUsedDuringCommitProtectionDisable() error {
	return o.o.SetUsedDuringCommitProtectionDisable()
}
func (o txOptions) SetReadAheadDisable() error          { return o.o.SetReadAheadDisable() }
func (o txOptions) SetReadPriorityHigh() error          { return o.o.SetReadPriorityHigh() }
func (o txOptions) SetReadPriorityLow() error           { return o.o.SetReadPriorityLow() }
func (o txOptions) SetReadPriorityNormal() error        { return o.o.SetReadPriorityNormal() }
func (o txOptions) SetReadServerSideCacheEnable() error { return o.o.SetReadServerSideCacheEnable() }

func (o txOptions) SetReadServerSideCacheDisable() error {
	return o.o.SetReadServerSideCacheDisable()
}
func (o txOptions) SetUseProvisionalProxies() error { return o.o.SetUseProvisionalProxies() }
func (o txOptions) SetBypassStorageQuota() error    { return o.o.SetBypassStorageQuota() }
func (o txOptions) SetInitializeNewDatabase() error { return o.o.SetInitializeNewDatabase() }
func (o txOptions) SetAuthorizationToken(token string) error {
	return o.o.SetAuthorizationToken(token)
}
func (o txOptions) SetSpanParent(parent []byte) error { return o.o.SetSpanParent(parent) }
func (o txOptions) SetExpensiveClearCostEstimationEnable() error {
	return o.o.SetExpensiveClearCostEstimationEnable()
}
