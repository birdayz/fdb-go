// Portions derived from FoundationDB Record Layer (FDBRecordContext.java,
// FDBDatabase.java, FDBDatabaseRunner.java, FDBDatabaseFactory.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2019 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"errors"
	"fmt"
	"sync"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// VersionMutationType represents the type of versionstamp mutation.
// Matches Java's MutationType used in FDBRecordContext.addVersionMutation().
type VersionMutationType int

const (
	// MutationTypeSetVersionstampedValue queues a SET_VERSIONSTAMPED_VALUE mutation.
	MutationTypeSetVersionstampedValue VersionMutationType = iota
	// MutationTypeSetVersionstampedKey queues a SET_VERSIONSTAMPED_KEY mutation.
	MutationTypeSetVersionstampedKey
)

// versionMutation holds a queued versionstamp mutation with its type and value.
type versionMutation struct {
	mutationType VersionMutationType
	value        []byte
}

// ClaimLocalVersion atomically claims the next local version number.
// Goroutine-safe via atomic.Int32 (matches Java's AtomicInteger.getAndIncrement()).
func (rc *FDBRecordContext) ClaimLocalVersion() int {
	return int(rc.localVersion.Add(1) - 1) // returns 0, 1, 2, ...
}

// AddToLocalVersionCache caches a local version for a version key within this transaction.
// Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.addToLocalVersionCache().
func (rc *FDBRecordContext) AddToLocalVersionCache(versionKey []byte, localVersion int) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	if rc.localVersionCache == nil {
		rc.localVersionCache = make(map[string]int)
	}
	rc.localVersionCache[string(versionKey)] = localVersion
}

// GetLocalVersion retrieves a cached local version for the given key.
// Returns (localVersion, true) if found, (0, false) otherwise.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) GetLocalVersion(versionKey []byte) (int, bool) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	v, ok := rc.localVersionCache[string(versionKey)]
	return v, ok
}

// RemoveLocalVersion removes a cached local version entry.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) RemoveLocalVersion(versionKey []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	delete(rc.localVersionCache, string(versionKey))
}

// AddVersionMutation queues a versionstamp mutation to be applied at commit.
// mutationType selects SET_VERSIONSTAMPED_KEY or SET_VERSIONSTAMPED_VALUE.
// The key or value (depending on type) must include the versionstamp placeholder bytes.
// Goroutine-safe via versionMu.
// Returns the previous value for this key, or nil if absent, matching Java's
// FDBRecordContext.addVersionMutation(MutationType, key, value).
func (rc *FDBRecordContext) AddVersionMutation(mutationType VersionMutationType, versionKey []byte, value []byte) []byte {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	if rc.versionMutations == nil {
		rc.versionMutations = make(map[string]versionMutation)
	}
	previous, existed := rc.versionMutations[string(versionKey)]
	if existed && previous.value == nil {
		previous.value = []byte{}
	}
	rc.versionMutations[string(versionKey)] = versionMutation{
		mutationType: mutationType,
		value:        value,
	}
	return previous.value
}

// UpdateVersionMutation queues or updates a versionstamp mutation with a merge function.
// If a mutation for the same key already exists, the merge function decides which value to keep.
// Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.updateVersionMutation(MutationType, key, value, BiFunction).
func (rc *FDBRecordContext) UpdateVersionMutation(mutationType VersionMutationType, versionKey []byte, value []byte, merge func(oldValue, newValue []byte) []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	if rc.versionMutations == nil {
		rc.versionMutations = make(map[string]versionMutation)
	}
	key := string(versionKey)
	if existing, ok := rc.versionMutations[key]; ok && merge != nil {
		merged := merge(existing.value, value)
		rc.versionMutations[key] = versionMutation{
			mutationType: mutationType,
			value:        merged,
		}
	} else {
		rc.versionMutations[key] = versionMutation{
			mutationType: mutationType,
			value:        value,
		}
	}
}

// RemoveVersionMutation removes a queued version mutation.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) RemoveVersionMutation(versionKey []byte) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	delete(rc.versionMutations, string(versionKey))
}

// RemoveVersionMutationsInRange removes all queued version mutations whose key
// falls in [begin, end). Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.removeVersionMutationRange().
func (rc *FDBRecordContext) RemoveVersionMutationsInRange(begin, end fdb.Key) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for k := range rc.versionMutations {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(rc.versionMutations, k)
		}
	}
}

// HasVersionMutationsInRange checks surviving buffered mutations in [begin, end).
// It shares the registry and mutex used by context-aware clears.
func (rc *FDBRecordContext) HasVersionMutationsInRange(begin, end fdb.Key) bool {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for key := range rc.versionMutations {
		if bytes.Compare([]byte(key), begin) >= 0 && bytes.Compare([]byte(key), end) < 0 {
			return true
		}
	}
	return false
}

// RemoveLocalVersionsInRange removes all cached local versions whose key
// falls in [begin, end). Goroutine-safe via versionMu.
// Matches Java's FDBRecordContext.removeLocalVersionRange().
func (rc *FDBRecordContext) RemoveLocalVersionsInRange(begin, end fdb.Key) {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for k := range rc.localVersionCache {
		kb := []byte(k)
		if bytes.Compare(kb, begin) >= 0 && bytes.Compare(kb, end) < 0 {
			delete(rc.localVersionCache, k)
		}
	}
}

// ClearRange clears transactional data and pending version state together, so
// flushing version mutations at commit cannot restore keys inside the range.
// Matches Java's FDBRecordContext.clear(Range).
func (rc *FDBRecordContext) ClearRange(keyRange fdb.ExactRange) {
	rc.clearRange(keyRange, true)
}

// clearRange permits store-aware callers to supply an already-applied metadata
// invalidation policy, while preserving state projections and version cleanup.
func (rc *FDBRecordContext) clearRange(keyRange fdb.ExactRange, invalidateState bool) {
	begin, end := keyRange.FDBRangeKeys()
	rc.clearRangeWithIndexStateViews(keyRange, invalidateState)
	rc.RemoveVersionMutationsInRange(begin.FDBKey(), end.FDBKey())
	rc.RemoveLocalVersionsInRange(begin.FDBKey(), end.FDBKey())
}

// flushVersionMutations applies all queued versionstamp mutations
// to the underlying FDB transaction. Called before commit.
// Dispatches to SetVersionstampedKey or SetVersionstampedValue based on mutation type.
// Goroutine-safe via versionMu (though typically called after all goroutines are done).
func (rc *FDBRecordContext) flushVersionMutations() {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	for key, mut := range rc.versionMutations {
		switch mut.mutationType {
		case MutationTypeSetVersionstampedKey:
			rc.tx.SetVersionstampedKey(fdb.Key(key), mut.value)
		case MutationTypeSetVersionstampedValue:
			rc.tx.SetVersionstampedValue(fdb.Key(key), mut.value)
		}
	}
}

// HasVersionMutations returns true if there are pending version mutations.
// Goroutine-safe via versionMu.
func (rc *FDBRecordContext) HasVersionMutations() bool {
	rc.versionMu.Lock()
	defer rc.versionMu.Unlock()
	return len(rc.versionMutations) > 0
}

// HasDirtyStoreState returns true if any store state was modified in this transaction.
// When true, cached store state should not be used.
// Goroutine-safe via atomic.Bool.
// Matches Java's FDBRecordContext.hasDirtyStoreState().
func (rc *FDBRecordContext) HasDirtyStoreState() bool {
	return rc.dirtyStoreState.Load()
}

// SetDirtyStoreState marks that store state was modified in this transaction.
// Goroutine-safe via atomic.Bool.
// Matches Java's FDBRecordContext.setDirtyStoreState().
func (rc *FDBRecordContext) SetDirtyStoreState(dirty bool) {
	rc.dirtyStoreState.Store(dirty)
}

// metaDataVersionStampValue is 14 zero bytes: 10 bytes for the global versionstamp
// + 4 bytes for the little-endian offset (0). FDB replaces the first 10 bytes with
// the commit versionstamp when SET_VERSIONSTAMPED_VALUE is used.
// Matches Java's FDBRecordContext.META_DATA_VERSION_STAMP_VALUE.
// Using an array (not slice) to prevent accidental mutation.
var metaDataVersionStampValue [14]byte

// metaDataVersionKey is the FDB system key used to track metadata version changes.
// Matches Java's SystemKeyspace.METADATA_VERSION_KEY = \xff/metadataVersion.
var metaDataVersionKey = append([]byte{0xFF}, []byte("/metadataVersion")...)

// SetMetaDataVersionStamp schedules a SET_VERSIONSTAMPED_VALUE mutation on the
// metadata version key. After commit, this key will contain the commit versionstamp,
// which invalidates any cached store state entries with older stamps.
// Goroutine-safe via atomic.Bool + goroutine-safe FDB transaction.
// Matches Java's FDBRecordContext.setMetaDataVersionStamp().
func (rc *FDBRecordContext) SetMetaDataVersionStamp() {
	rc.dirtyMetaDataVersionStamp.Store(true)
	rc.tx.SetVersionstampedValue(fdb.Key(metaDataVersionKey), metaDataVersionStampValue[:])
}

// GetMetaDataVersionStamp reads the metadata version stamp at snapshot isolation.
// Returns nil if the stamp was written in this transaction (dirty) or doesn't exist.
// On ACCESSED_UNREADABLE errors (FDB code 1036), marks the stamp as dirty and returns nil.
// Goroutine-safe via atomic.Bool + goroutine-safe FDB transaction.
// Matches Java's FDBRecordContext.getMetaDataVersionStampAsync().
func (rc *FDBRecordContext) GetMetaDataVersionStamp() ([]byte, error) {
	if rc.dirtyMetaDataVersionStamp.Load() {
		return nil, nil
	}
	val, err := rc.tx.Snapshot().Get(fdb.Key(metaDataVersionKey)).Get()
	if err != nil {
		// Check for ACCESSED_UNREADABLE (1036) — the versionstamped value was
		// written in this transaction and can't be read back yet.
		// Matches Java's handle() which catches this specific error code.
		var fdbErr fdb.Error
		if errors.As(err, &fdbErr) && fdbErr.Code == 1036 {
			rc.dirtyMetaDataVersionStamp.Store(true)
			return nil, nil
		}
		// For genuine errors (network, transaction_too_old, etc.), propagate.
		return nil, err
	}
	if val == nil {
		return nil, nil
	}
	return val, nil
}

// buildVersionstampedValue builds the value for SET_VERSIONSTAMPED_VALUE mutation.
// Matches Java's SplitHelper.packVersion(): wraps an incomplete versionstamp in
// a Tuple and uses PackWithVersionstamp to produce bytes with the offset appended.
// After FDB commit, the stored value is a packed Tuple containing the completed versionstamp.
func buildVersionstampedValue(version *FDBRecordVersion) ([]byte, error) {
	vs := tuple.Versionstamp{
		UserVersion: uint16(version.GetLocalVersion()),
	}
	// TransactionVersion is all 0xFF for incomplete versionstamps (placeholder)
	copy(vs.TransactionVersion[:], incompleteGlobalVersionMarker[:])
	return tuple.Tuple{vs}.PackWithVersionstamp(nil)
}

// lockRegistry provides per-key read-write locks within a transaction context.
// Matches Java's LockRegistry (ConcurrentHashMap<LockIdentifier, AtomicReference<AsyncLock>>).
// Used by tree-structured indexes (HNSW, R-tree) to serialize mutations.
//
// An entry lives while anyone holds or waits for its lock, and is removed when
// the last of them releases (Java 4.14 #4545: a completed lock is removed only
// when no registered work depends on it). Removing it any earlier would let a
// later caller create a second live lock for the same key.
type lockRegistry struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

// lockEntry is one key's lock and the number of holders and waiters it has,
// counted under lockRegistry.mu.
type lockEntry struct {
	rw   sync.RWMutex
	refs int
}

// acquire registers one more holder or waiter of key's lock.
func (r *lockRegistry) acquire(key string) *lockEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks == nil {
		r.locks = make(map[string]*lockEntry)
	}
	e, ok := r.locks[key]
	if !ok {
		e = &lockEntry{}
		r.locks[key] = e
	}
	e.refs++
	return e
}

// held is key's entry, which the caller holds.
func (r *lockRegistry) held(key string) *lockEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.locks[key]
	if !ok {
		panic(fmt.Sprintf("recordlayer: unlock of an unlocked key %q", key))
	}
	return e
}

// release drops one holder of key's lock, removing the entry with the last.
func (r *lockRegistry) release(key string, e *lockEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(r.locks, key)
	}
}

// size is the number of live entries.
func (r *lockRegistry) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.locks)
}

// WriteLock acquires an exclusive lock for the given key.
// Matches Java's FDBRecordContext.doWithWriteLock(LockIdentifier).
func (r *lockRegistry) WriteLock(key string) {
	r.acquire(key).rw.Lock()
}

// WriteUnlock releases the exclusive lock for the given key.
func (r *lockRegistry) WriteUnlock(key string) {
	e := r.held(key)
	e.rw.Unlock()
	r.release(key, e)
}

// ReadLock acquires a shared lock for the given key.
// Matches Java's FDBRecordContext.doWithReadLock(LockIdentifier).
func (r *lockRegistry) ReadLock(key string) {
	r.acquire(key).rw.RLock()
}

// ReadUnlock releases the shared lock for the given key.
func (r *lockRegistry) ReadUnlock(key string) {
	e := r.held(key)
	e.rw.RUnlock()
	r.release(key, e)
}
