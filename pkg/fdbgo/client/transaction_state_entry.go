package client

import "time"

func (tx *Transaction) SetReadSystemKeys() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadSystemKeys()
}

func (tx *Transaction) SetAccessSystemKeys() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetAccessSystemKeys()
}

func (tx *Transaction) Set(key, value []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSet(key, value)
}

func (tx *Transaction) Clear(key []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateClear(key)
}

func (tx *Transaction) ClearRange(begin, end []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateClearRange(begin, end)
}

func (tx *Transaction) Atomic(op MutationType, key, operand []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateAtomic(op, key, operand)
}

func (tx *Transaction) SetSpanParent(b []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSetSpanParent(b)
}

func (tx *Transaction) WatchActivation() *watchActivation {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateWatchActivation()
}

func (tx *Transaction) GetCommittedVersion() (int64, error) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetCommittedVersion()
}

func (tx *Transaction) GetVersionstamp() ([]byte, error) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetVersionstamp(lease.inc)
}

func (tx *Transaction) SetReadVersion(version int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadVersion(version)
}

func (tx *Transaction) ReadVersionInstant() (time.Time, bool) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateReadVersionInstant()
}

func (tx *Transaction) SetTimeout(ms int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetTimeout(ms)
}

func (tx *Transaction) SetRetryLimit(retries int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetRetryLimit(retries)
}

func (tx *Transaction) GetApproximateSize() (int64, error) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetApproximateSize()
}

func (tx *Transaction) SetNextWriteNoWriteConflictRange() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetNextWriteNoWriteConflictRange()
}

func (tx *Transaction) SetPriority(p TransactionPriority) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetPriority(p)
}

func (tx *Transaction) SetCausalReadRisky(v bool) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetCausalReadRisky(v)
}

func (tx *Transaction) SetUseGrvCache() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetUseGrvCache()
}

func (tx *Transaction) SetSkipGrvCache() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSkipGrvCache()
}

func (tx *Transaction) SetLockAware(v bool) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetLockAware(v)
}

func (tx *Transaction) SetReadLockAware(v bool) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadLockAware(v)
}

func (tx *Transaction) LockAware() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateLockAware()
}

func (tx *Transaction) ReadLockAware() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateReadLockAware()
}

func (tx *Transaction) SetSizeLimit(limit int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSizeLimit(limit)
}

func (tx *Transaction) SetMaxRetryDelay(ms int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetMaxRetryDelay(ms)
}

func (tx *Transaction) SetReadYourWritesDisable() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadYourWritesDisable()
}

func (tx *Transaction) SetWriteConflictsDisabled() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetWriteConflictsDisabled()
}

func (tx *Transaction) EnsureMutationCapacity(n int) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateEnsureMutationCapacity(n)
}

func (tx *Transaction) SetSnapshotRYWDisable() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSnapshotRYWDisable()
}

func (tx *Transaction) SetSnapshotRYWEnable() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSnapshotRYWEnable()
}

func (tx *Transaction) SnapshotRYWDisableCount() int {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSnapshotRYWDisableCount()
}

func (tx *Transaction) SetSnapshotRYWDisableCount(n int) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSnapshotRYWDisableCount(n)
}

func (tx *Transaction) BypassUnreadable() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateBypassUnreadable()
}

func (tx *Transaction) CausalReadRisky() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateCausalReadRisky()
}

func (tx *Transaction) SetTenantId(id int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetTenantId(id)
}

func (tx *Transaction) TenantId() int64 {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateTenantId()
}

func (tx *Transaction) SetTag(tag string) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSetTag(tag)
}

func (tx *Transaction) SetAutoThrottleTag(tag string) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSetAutoThrottleTag(tag)
}

func (tx *Transaction) Tags() []string {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateTags()
}

func (tx *Transaction) ReadTags() []string {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateReadTags()
}

func (tx *Transaction) GetTagThrottledDuration() float64 {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetTagThrottledDuration()
}

func (tx *Transaction) AddReadConflictRange(begin, end []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateAddReadConflictRange(begin, end)
}

func (tx *Transaction) AddReadConflictKey(key []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateAddReadConflictKey(key)
}

func (tx *Transaction) AddWriteConflictRange(begin, end []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateAddWriteConflictRange(begin, end)
}

func (tx *Transaction) AddWriteConflictKey(key []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateAddWriteConflictKey(key)
}

func (tx *Transaction) SetBypassUnreadable(v bool) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetBypassUnreadable(v)
}
