package simfdb

import fdb "fdb.dev/pkg/fdbgo/fdb"

// txnOptions implements fdb.TransactionOptions for a simTxn. Only three options carry semantics
// SimFDB models — NextWriteNoWriteConflictRange, ReadYourWritesDisable, WriteConflictsDisabled —
// which alter conflict-range/RYW behavior. Every other option is accepted and ignored: the sim
// has no client-side timeouts, retry loop, priority, tracing, tenancy, or security model (the
// record layer's runner drives retries/timeouts above the backend), so ignoring them is safe
// and never changes persisted bytes or conflict outcomes.
type txnOptions struct{ tx *simTxn }

var _ fdb.TransactionOptions = (*txnOptions)(nil)

// --- semantic options ---

func (o *txnOptions) SetNextWriteNoWriteConflictRange() error {
	o.tx.nextWriteNoConflict = true
	return nil
}

func (o *txnOptions) SetReadYourWritesDisable() error {
	o.tx.rywDisabled = true
	return nil
}
func (o *txnOptions) SetWriteConflictsDisabled() { o.tx.writeConflictsDisabled = true }

// SetBypassUnreadable is FDB_TR_OPTION_BYPASS_UNREADABLE: a read of a key carrying a pending
// versionstamped op returns the operand's placeholder bytes AS WRITTEN instead of throwing
// accessed_unreadable(1036) (client/ryw.go:55-60, applied per read at :515). It is the caller
// asserting it knows the value it gets back is not the value that will be committed.
func (o *txnOptions) SetBypassUnreadable() error {
	return nil
}

// --- accepted-and-ignored options ---

func (o *txnOptions) EnsureMutationCapacity(n int) {}

func (o *txnOptions) SetTimeout(int64) error                          { return nil }
func (o *txnOptions) SetRetryLimit(int64) error                       { return nil }
func (o *txnOptions) SetPriorityBatch() error                         { return nil }
func (o *txnOptions) SetPrioritySystemImmediate() error               { return nil }
func (o *txnOptions) SetDebugTransactionIdentifier(string) error      { return nil }
func (o *txnOptions) SetCausalReadRisky() error                       { return nil }
func (o *txnOptions) SetAccessSystemKeys() error                      { return nil }
func (o *txnOptions) SetReadSystemKeys() error                        { return nil }
func (o *txnOptions) SetLockAware() error                             { return nil }
func (o *txnOptions) SetReadLockAware() error                         { return nil }
func (o *txnOptions) SetLogTransaction() error                        { return nil }
func (o *txnOptions) SetTransactionLoggingEnable(string) error        { return nil }
func (o *txnOptions) SetSizeLimit(int64) error                        { return nil }
func (o *txnOptions) SetMaxRetryDelay(int64) error                    { return nil }
func (o *txnOptions) SetSnapshotRywEnable() error                     { return nil }
func (o *txnOptions) SetSnapshotRywDisable() error                    { return nil }
func (o *txnOptions) SetUseGrvCache() error                           { return nil }
func (o *txnOptions) SetSkipGrvCache() error                          { return nil }
func (o *txnOptions) SetAutoThrottleTag(string) error                 { return nil }
func (o *txnOptions) SetTag(string) error                             { return nil }
func (o *txnOptions) SetReportConflictingKeys() error                 { return nil }
func (o *txnOptions) SetSpecialKeySpaceRelaxed() error                { return nil }
func (o *txnOptions) SetSpecialKeySpaceEnableWrites() error           { return nil }
func (o *txnOptions) SetRawAccess() error                             { return nil }
func (o *txnOptions) SetAutomaticIdempotency() error                  { return nil }
func (o *txnOptions) SetDebugRetryLogging(string) error               { return nil }
func (o *txnOptions) SetIncludePortInAddress() error                  { return nil }
func (o *txnOptions) SetCausalReadDisable() error                     { return nil }
func (o *txnOptions) SetCausalWriteRisky() error                      { return nil }
func (o *txnOptions) SetDurabilityRisky() error                       { return nil }
func (o *txnOptions) SetDurabilityDatacenter() error                  { return nil }
func (o *txnOptions) SetDurabilityDevNullIsWebScale() error           { return nil }
func (o *txnOptions) SetTransactionLoggingMaxFieldLength(int64) error { return nil }
func (o *txnOptions) SetServerRequestTracing() error                  { return nil }
func (o *txnOptions) SetUsedDuringCommitProtectionDisable() error     { return nil }
func (o *txnOptions) SetReadAheadDisable() error                      { return nil }
func (o *txnOptions) SetReadPriorityHigh() error                      { return nil }
func (o *txnOptions) SetReadPriorityLow() error                       { return nil }
func (o *txnOptions) SetReadPriorityNormal() error                    { return nil }
func (o *txnOptions) SetReadServerSideCacheEnable() error             { return nil }
func (o *txnOptions) SetReadServerSideCacheDisable() error            { return nil }
func (o *txnOptions) SetUseProvisionalProxies() error                 { return nil }
func (o *txnOptions) SetBypassStorageQuota() error                    { return nil }
func (o *txnOptions) SetInitializeNewDatabase() error                 { return nil }
func (o *txnOptions) SetAuthorizationToken(string) error              { return nil }
func (o *txnOptions) SetSpanParent([]byte) error                      { return nil }
func (o *txnOptions) SetExpensiveClearCostEstimationEnable() error    { return nil }
