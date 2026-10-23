package client

import "time"

// SetReadSystemKeys allows reading \xff prefix system keys.
func (tx *Transaction) SetReadSystemKeys() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadSystemKeys()
}

// SetAccessSystemKeys allows reading AND writing \xff prefix system keys.
func (tx *Transaction) SetAccessSystemKeys() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetAccessSystemKeys()
}

// Set writes a key-value pair.
func (tx *Transaction) Set(key, value []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSet(key, value)
}

// Clear deletes a key.
func (tx *Transaction) Clear(key []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateClear(key)
}

// ClearRange deletes all keys in [begin, end).
// Returns inverted_range (2005) if begin > end; C++ ThreadSafeTransaction::clear
// raises it as the transaction's deferred error (ThreadSafeTransaction.cpp:626-640).
// Zero-width ranges (begin == end) are silently ignored, matching C++.
func (tx *Transaction) ClearRange(begin, end []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateClearRange(begin, end)
}

// Atomic performs an atomic mutation.
func (tx *Transaction) Atomic(op MutationType, key, operand []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateAtomic(op, key, operand)
}

// SetSpanParent injects a parent trace context (FDBTransactionOptions::SPAN_PARENT,
// NativeAPI.actor.cpp:7126): a 33-byte IncludeVersion-serialized SpanContext. The
// transaction's span becomes a child of it (inherit traceID + flags, fresh spanID),
// and the linkage persists across retries (regenerateSpan honors spanParent).
func (tx *Transaction) SetSpanParent(b []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSetSpanParent(b)
}

// WatchActivation returns this incarnation's activation, lazily creating it on first use (a fresh handle
// before any Watch/commit has nil watchAct). Race-safe via CAS so two concurrent Watch() calls share one.
func (tx *Transaction) WatchActivation() *watchActivation {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateWatchActivation()
}

// GetCommittedVersion returns the version at which this transaction committed.
func (tx *Transaction) GetCommittedVersion() (int64, error) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetCommittedVersion()
}

// GetVersionstamp returns the 10-byte versionstamp from the committed transaction.
// Format: [version 8 bytes big-endian][txnBatchId 2 bytes big-endian].
// Must be called after a successful Commit.
func (tx *Transaction) GetVersionstamp() ([]byte, error) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetVersionstamp(lease.inc)
}

// SetReadVersion sets the read version manually.
func (tx *Transaction) SetReadVersion(version int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadVersion(version)
}

// ReadVersionInstant reports when this transaction's current read version was
// obtained from the cluster, i.e. when FDB's 5-second MVCC window opened for it.
// ok is false when no read version is held, or when the version was supplied by
// SetReadVersion (whose window opened at an instant this client never observed).
//
// It exists so a layer above can budget against the window that actually applies.
// Anchoring such a budget on "when I started working" is wrong in both directions:
// a transaction that idles before its first read has not yet opened a window, and
// a retried transaction opened a fresh one. Only the GRV instant tracks it.
//
// Callers MUST honour ok rather than treating a zero time as an anchor.
func (tx *Transaction) ReadVersionInstant() (time.Time, bool) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateReadVersionInstant()
}

// SetTimeout sets a timeout in milliseconds for this transaction.
// The timeout is an overall budget from creation time (or last user Reset),
// NOT per-retry. OnError retries share the same deadline.
// A value of 0 disables the timeout. Matches C++ FDB_TR_OPTION_TIMEOUT.
func (tx *Transaction) SetTimeout(ms int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetTimeout(ms)
}

// SetRetryLimit limits the number of retries in OnError.
// A value of 0 means "don't retry at all" (first error escapes).
// A value of -1 means "unlimited" (default behavior).
// Matches C++ FDB_TR_OPTION_RETRY_LIMIT.
func (tx *Transaction) SetRetryLimit(retries int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetRetryLimit(retries)
}

// GetApproximateSize returns the approximate size of the transaction's mutations
// and conflict ranges in bytes. Matches C++ ReadYourWritesTransaction accounting:
// each mutation includes sizeof(MutationRef), each conflict range includes
// sizeof(KeyRangeRef). For set/atomic with write conflicts, C++ also adds
// sizeof(KeyRangeRef) + 2*len(key) + 1 for the auto-generated write conflict
// range (ReadYourWrites.actor.cpp:2289-2290, :2337-2338).
func (tx *Transaction) GetApproximateSize() (int64, error) {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetApproximateSize()
}

// SetNextWriteNoWriteConflictRange causes the next mutation to NOT add a write
// conflict range. Auto-resets after one mutation. Matches C++
// FDB_TR_OPTION_NEXT_WRITE_NO_WRITE_CONFLICT_RANGE.
func (tx *Transaction) SetNextWriteNoWriteConflictRange() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetNextWriteNoWriteConflictRange()
}

// SetPriority sets the transaction priority for GRV requests.
func (tx *Transaction) SetPriority(p TransactionPriority) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetPriority(p)
}

// SetCausalReadRisky sets the causal-read-risky flag.
// When set, the read version may not reflect the latest committed writes.
func (tx *Transaction) SetCausalReadRisky(v bool) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetCausalReadRisky(v)
}

// SetUseGrvCache opts this transaction in to serving its read version from the
// database's GRV cache (USE_GRV_CACHE, 1101). Off by default — a default
// transaction issues a fresh proxy GRV, matching libfdb_c. RFC-104.
func (tx *Transaction) SetUseGrvCache() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetUseGrvCache()
}

// SetSkipGrvCache forces this transaction to bypass the GRV cache even if
// SetUseGrvCache was also set (SKIP_GRV_CACHE, 1102 — skip wins). RFC-104.
func (tx *Transaction) SetSkipGrvCache() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSkipGrvCache()
}

// SetLockAware makes reads and the commit lock-aware, and lifts a read-only
// state left by SetReadLockAware (C++ LOCK_AWARE, NativeAPI.actor.cpp:7071-7079).
func (tx *Transaction) SetLockAware() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetLockAware()
}

// SetReadLockAware makes reads lock-aware and, unless SetLockAware came first,
// makes the transaction read-only: a commit with writes fails 2023 (C++
// READ_LOCK_AWARE, NativeAPI.actor.cpp:7082-7091; checked at :6810).
func (tx *Transaction) SetReadLockAware() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadLockAware()
}

// LockAware reports whether the commit is lock-aware (sets FLAG_IS_LOCK_AWARE,
// bypassing the locked-database check). Mirrors SetLockAware.
func (tx *Transaction) LockAware() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateLockAware()
}

// ReadLockAware reports whether reads bypass the locked-database check.
// Mirrors SetReadLockAware.
func (tx *Transaction) ReadLockAware() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateReadLockAware()
}

// SetSizeLimit sets the maximum transaction size in bytes; a larger commit
// fails 2101. Outside [32, 10_000_000] the limit is not applied and 2006 becomes
// the deferred error every later op returns until reset, as libfdb_c's
// extractIntOption throw lands in deferredError (NativeAPI.actor.cpp:7067-7069).
func (tx *Transaction) SetSizeLimit(limit int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSizeLimit(limit)
}

// SetMaxRetryDelay caps the exponential backoff between retries.
// Value in milliseconds. Matches C++ FDB_TR_OPTION_MAX_RETRY_DELAY.
func (tx *Transaction) SetMaxRetryDelay(ms int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetMaxRetryDelay(ms)
}

// SetReadYourWritesDisable disables RYW for regular (non-snapshot) reads.
// When set, Get/GetRange always read from the server, ignoring uncommitted writes.
// Matches FDB_TR_OPTION_READ_YOUR_WRITES_DISABLE.
//
// libfdb_c forbids setting this option after any read or write: RYW setOptionImpl
// throws client_invalid_operation BEFORE assigning the option
// (ReadYourWrites.actor.cpp:2534-2542), so the option does NOT take effect; the throw
// lands in deferredError (the option call itself succeeds) and every subsequent op
// returns 2000 until reset (RFC-059); this client behaves the same. A clean (pre-op)
// disable applies normally.
func (tx *Transaction) SetReadYourWritesDisable() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetReadYourWritesDisable()
}

// SetWriteConflictsDisabled disables write conflict ranges for all subsequent
// mutations. Use for insert-only batch writes where keys are guaranteed unique
// and all atomic operations commute (ADD, MAX, MIN). Significantly reduces
// commit request size and eliminates conflict buffer allocations.
func (tx *Transaction) SetWriteConflictsDisabled() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetWriteConflictsDisabled()
}

// EnsureMutationCapacity pre-sizes the mutations and writeConflicts slices
// to avoid growth allocations during batch writes. Call before a large batch.
func (tx *Transaction) EnsureMutationCapacity(n int) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateEnsureMutationCapacity(n)
}

// SetSnapshotRYWDisable decrements the snapshot-RYW enable count (stored as the
// disabled-oriented inverse, so it increments here). Matches FDB_TR_OPTION_SNAPSHOT_RYW_DISABLE
// (libfdb_c does enabledCount--). When the net count is > 0 (more disables than enables),
// Snapshot.Get/GetRange/GetKey read from the server, bypassing the RYW cache.
func (tx *Transaction) SetSnapshotRYWDisable() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSnapshotRYWDisable()
}

// SetSnapshotRYWEnable re-enables RYW for snapshot reads, undoing one prior
// SetSnapshotRYWDisable. Matches FDB_TR_OPTION_SNAPSHOT_RYW_ENABLE (libfdb_c does
// enabledCount++). The option is a counter, not a toggle: two disables require two enables to
// re-enable, and an enable from the default pushes the count negative (still enabled).
func (tx *Transaction) SetSnapshotRYWEnable() {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSnapshotRYWEnable()
}

// SnapshotRYWDisableCount reports the net snapshot-RYW-disable count (> 0 means snapshot reads
// bypass the RYW cache). Read-only accessor used to verify database-level option propagation.
func (tx *Transaction) SnapshotRYWDisableCount() int {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSnapshotRYWDisableCount()
}

// SetSnapshotRYWDisableCount SETS the snapshot-RYW disable counter to n (vs the ++/-- of
// SetSnapshotRYWDisable/Enable). It seeds the per-tx counter to a database default. Setting (not
// incrementing) is idempotent under the retry replay of applyTxDefaults — matching libfdb_c, whose
// reset() re-seeds snapshotRywEnabled = db->snapshotRywEnabled each attempt rather than accumulating.
func (tx *Transaction) SetSnapshotRYWDisableCount(n int) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetSnapshotRYWDisableCount(n)
}

// BypassUnreadable reports whether FDB_TR_OPTION_BYPASS_UNREADABLE is set. Read-only accessor used
// to verify database-level option propagation.
func (tx *Transaction) BypassUnreadable() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateBypassUnreadable()
}

// CausalReadRisky reports whether the GRV causal-read-risky flag is set. Read-only accessor used to
// verify database-level option propagation.
func (tx *Transaction) CausalReadRisky() bool {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateCausalReadRisky()
}

// SetTenantId sets the tenant for this transaction. All operations will
// be scoped to the tenant's key space. Use NoTenantID (-1) for no tenant.
func (tx *Transaction) SetTenantId(id int64) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetTenantId(id)
}

// TenantId returns the current tenant ID for this transaction.
// Returns NoTenantID (-1) if no tenant is set.
func (tx *Transaction) TenantId() int64 {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateTenantId()
}

// SetTag adds a tag to this transaction for tag-based throttling, matching
// FDBTransactionOptions::TAG (NativeAPI.actor.cpp:7115-7118).
//
// Check order follows TagSet::addTag (TagThrottle.actor.cpp:35-39) exactly:
// length is validated first, then the count — and the count is checked BEFORE
// the duplicate test, so adding a duplicate to an already-full set still fails
// with too_many_tags rather than silently succeeding.
func (tx *Transaction) SetTag(tag string) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSetTag(tag)
}

// SetAutoThrottleTag adds a tag that is additionally eligible for ratekeeper
// auto-throttling, matching FDBTransactionOptions::AUTO_THROTTLE_TAG
// (NativeAPI.actor.cpp:7120-7124): the tag goes into BOTH the transaction tag
// set and the read tag set. The transaction set drives the GRV request and the
// commit tagSet; the read set is what C++ samples onto per-storage-server read
// requests.
func (tx *Transaction) SetAutoThrottleTag(tag string) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateSetAutoThrottleTag(tag)
}

// Tags returns the transaction tag set in insertion order. Insertion order is
// the wire order for the commit tagSet, so tests can assert it directly.
func (tx *Transaction) Tags() []string {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateTags()
}

// ReadTags returns the auto-throttle (read) tag subset in insertion order.
func (tx *Transaction) ReadTags() []string {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateReadTags()
}

// GetTagThrottledDuration returns the total time this transaction was delayed
// by proxy tag throttling across all GRV requests. Matches C++
// Transaction::getTagThrottledDuration() (NativeAPI.actor.cpp:7594).
func (tx *Transaction) GetTagThrottledDuration() float64 {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateGetTagThrottledDuration()
}

// AddReadConflictRange adds an explicit read conflict range [begin, end).
// If any key in this range is modified by another transaction between
// this transaction's read version and commit, the commit will fail.
// Returns only inverted_range (2005); key_outside_legal_range is deferred, as in libfdb_c (RFC-268).
func (tx *Transaction) AddReadConflictRange(begin, end []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateAddReadConflictRange(begin, end)
}

// AddReadConflictKey adds a read conflict on a single key. Like AddReadConflictRange it is filtered
// through the RYW write-map (C++ addReadConflictRange over [key, keyAfter(key)) → updateConflictMap,
// ReadYourWrites.actor.cpp:1986): a self-written independent key adds no conflict; rywDisabled adds
// the full single-key conflict directly (:1979). An illegal key defers 2004 (RFC-268).
func (tx *Transaction) AddReadConflictKey(key []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateAddReadConflictKey(key)
}

// AddWriteConflictRange adds an explicit write conflict range [begin, end).
// Returns inverted_range (2005) if begin > end. As in libfdb_c, an endpoint past the
// maximum write key is not returned here: key_outside_legal_range (2004) becomes the
// deferred error that the next operation and Commit return.
func (tx *Transaction) AddWriteConflictRange(begin, end []byte) error {
	lease := tx.enterState()
	defer lease.release()
	return tx.stateAddWriteConflictRange(begin, end)
}

// AddWriteConflictKey adds a write conflict on a single key. A key past the maximum
// write key defers key_outside_legal_range (2004) as AddWriteConflictRange does.
func (tx *Transaction) AddWriteConflictKey(key []byte) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateAddWriteConflictKey(key)
}

// SetBypassUnreadable mirrors FDB_TR_OPTION_BYPASS_UNREADABLE
// (ReadYourWrites.actor.cpp:2611-2613): reads of keys with pending
// versionstamped writes return the write-map value with the placeholder
// bytes as written instead of failing with accessed_unreadable (1036);
// SVK's unmodified-unreadable candidate range reads through to storage.
// RFC-098.
func (tx *Transaction) SetBypassUnreadable(v bool) {
	lease := tx.enterState()
	defer lease.release()
	tx.stateSetBypassUnreadable(v)
}
