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

func (o txOptions) SetTimeout(milliseconds int64) error { return convErr(o.o.SetTimeout(milliseconds)) }
func (o txOptions) SetRetryLimit(retries int64) error   { return convErr(o.o.SetRetryLimit(retries)) }
func (o txOptions) SetPriorityBatch() error             { return convErr(o.o.SetPriorityBatch()) }
func (o txOptions) SetPrioritySystemImmediate() error {
	return convErr(o.o.SetPrioritySystemImmediate())
}

func (o txOptions) SetDebugTransactionIdentifier(id string) error {
	return convErr(o.o.SetDebugTransactionIdentifier(id))
}

func (o txOptions) SetNextWriteNoWriteConflictRange() error {
	return convErr(o.o.SetNextWriteNoWriteConflictRange())
}
func (o txOptions) SetCausalReadRisky() error       { return convErr(o.o.SetCausalReadRisky()) }
func (o txOptions) SetReadYourWritesDisable() error { return convErr(o.o.SetReadYourWritesDisable()) }

// EnsureMutationCapacity pre-sizes the pure-Go client's mutation buffer; libfdb_c
// manages its own buffer, so there is nothing to do here.
func (o txOptions) EnsureMutationCapacity(int) {}

// SetWriteConflictsDisabled has no single libfdb_c transaction-option analog (the
// pure-Go client models it as an internal flag); no-op on this backend.
func (o txOptions) SetWriteConflictsDisabled() {}

func (o txOptions) SetAccessSystemKeys() error { return convErr(o.o.SetAccessSystemKeys()) }
func (o txOptions) SetReadSystemKeys() error   { return convErr(o.o.SetReadSystemKeys()) }
func (o txOptions) SetLockAware() error        { return convErr(o.o.SetLockAware()) }
func (o txOptions) SetReadLockAware() error    { return convErr(o.o.SetReadLockAware()) }
func (o txOptions) SetLogTransaction() error   { return convErr(o.o.SetLogTransaction()) }
func (o txOptions) SetTransactionLoggingEnable(id string) error {
	return convErr(o.o.SetTransactionLoggingEnable(id))
}
func (o txOptions) SetSizeLimit(limit int64) error  { return convErr(o.o.SetSizeLimit(limit)) }
func (o txOptions) SetMaxRetryDelay(ms int64) error { return convErr(o.o.SetMaxRetryDelay(ms)) }
func (o txOptions) SetSnapshotRywEnable() error     { return convErr(o.o.SetSnapshotRywEnable()) }
func (o txOptions) SetSnapshotRywDisable() error    { return convErr(o.o.SetSnapshotRywDisable()) }
func (o txOptions) SetUseGrvCache() error           { return convErr(o.o.SetUseGrvCache()) }

// SetSkipGrvCache is a real FDB transaction option, but this cgofdb version
// exposes no typed setter for it (and the raw setOpt is unexported). It is a
// no-op on this backend — a documented v1 divergence from the pure-Go client,
// which honors it. The escape hatch trusts libfdb_c's GRV-cache default.
func (o txOptions) SetSkipGrvCache() error { return nil }

func (o txOptions) SetAutoThrottleTag(tag string) error { return convErr(o.o.SetAutoThrottleTag(tag)) }
func (o txOptions) SetTag(tag string) error             { return convErr(o.o.SetTag(tag)) }
func (o txOptions) SetReportConflictingKeys() error     { return convErr(o.o.SetReportConflictingKeys()) }
func (o txOptions) SetSpecialKeySpaceRelaxed() error    { return convErr(o.o.SetSpecialKeySpaceRelaxed()) }
func (o txOptions) SetSpecialKeySpaceEnableWrites() error {
	return convErr(o.o.SetSpecialKeySpaceEnableWrites())
}
func (o txOptions) SetRawAccess() error            { return convErr(o.o.SetRawAccess()) }
func (o txOptions) SetBypassUnreadable() error     { return convErr(o.o.SetBypassUnreadable()) }
func (o txOptions) SetAutomaticIdempotency() error { return convErr(o.o.SetAutomaticIdempotency()) }
func (o txOptions) SetDebugRetryLogging(loggerName string) error {
	return convErr(o.o.SetDebugRetryLogging(loggerName))
}
func (o txOptions) SetIncludePortInAddress() error { return convErr(o.o.SetIncludePortInAddress()) }
func (o txOptions) SetCausalReadDisable() error    { return convErr(o.o.SetCausalReadDisable()) }
func (o txOptions) SetCausalWriteRisky() error     { return convErr(o.o.SetCausalWriteRisky()) }
func (o txOptions) SetDurabilityRisky() error      { return convErr(o.o.SetDurabilityRisky()) }
func (o txOptions) SetDurabilityDatacenter() error { return convErr(o.o.SetDurabilityDatacenter()) }
func (o txOptions) SetDurabilityDevNullIsWebScale() error {
	return convErr(o.o.SetDurabilityDevNullIsWebScale())
}

func (o txOptions) SetTransactionLoggingMaxFieldLength(maxFieldLength int64) error {
	return convErr(o.o.SetTransactionLoggingMaxFieldLength(maxFieldLength))
}
func (o txOptions) SetServerRequestTracing() error { return convErr(o.o.SetServerRequestTracing()) }
func (o txOptions) SetUsedDuringCommitProtectionDisable() error {
	return convErr(o.o.SetUsedDuringCommitProtectionDisable())
}
func (o txOptions) SetReadAheadDisable() error   { return convErr(o.o.SetReadAheadDisable()) }
func (o txOptions) SetReadPriorityHigh() error   { return convErr(o.o.SetReadPriorityHigh()) }
func (o txOptions) SetReadPriorityLow() error    { return convErr(o.o.SetReadPriorityLow()) }
func (o txOptions) SetReadPriorityNormal() error { return convErr(o.o.SetReadPriorityNormal()) }
func (o txOptions) SetReadServerSideCacheEnable() error {
	return convErr(o.o.SetReadServerSideCacheEnable())
}

func (o txOptions) SetReadServerSideCacheDisable() error {
	return convErr(o.o.SetReadServerSideCacheDisable())
}
func (o txOptions) SetUseProvisionalProxies() error { return convErr(o.o.SetUseProvisionalProxies()) }
func (o txOptions) SetBypassStorageQuota() error    { return convErr(o.o.SetBypassStorageQuota()) }
func (o txOptions) SetInitializeNewDatabase() error { return convErr(o.o.SetInitializeNewDatabase()) }
func (o txOptions) SetAuthorizationToken(token string) error {
	return convErr(o.o.SetAuthorizationToken(token))
}
func (o txOptions) SetSpanParent(parent []byte) error { return convErr(o.o.SetSpanParent(parent)) }
func (o txOptions) SetExpensiveClearCostEstimationEnable() error {
	return convErr(o.o.SetExpensiveClearCostEstimationEnable())
}
