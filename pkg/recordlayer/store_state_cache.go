package recordlayer

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"

	"github.com/birdayz/fdb-record-layer-go/gen"
)

// FDBRecordStoreStateCache caches store state (header + index states) across
// transactions to avoid repeated reads of the store info key on every store open.
// Matches Java's FDBRecordStoreStateCache interface.
type FDBRecordStoreStateCache interface {
	// Get retrieves store state, using the cache if possible.
	// On cache hit, adds a read conflict on the STORE_INFO key to ensure
	// the transaction fails if concurrent modifications occur.
	// On cache miss, loads fresh state from FDB.
	Get(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*FDBRecordStoreStateCacheEntry, error)

	// Clear removes all cached entries.
	Clear()
}

// StoreExistenceCheck controls how store existence is validated during open.
// Matches Java's FDBRecordStoreBase.StoreExistenceCheck.
type StoreExistenceCheck int

const (
	// ExistenceCheckNone performs no existence check.
	ExistenceCheckNone StoreExistenceCheck = iota
	// ExistenceCheckErrorIfExists fails if the store already exists.
	ExistenceCheckErrorIfExists
	// ExistenceCheckErrorIfNotExists fails if the store does not exist.
	ExistenceCheckErrorIfNotExists
)

// FDBRecordStoreStateCacheEntry holds a cached snapshot of store state along with
// the metadata version stamp that was current when the state was loaded.
// Matches Java's FDBRecordStoreStateCacheEntry.
type FDBRecordStoreStateCacheEntry struct {
	subspaceKey          string // subspace.Bytes() as cache key
	subspace             subspace.Subspace
	recordStoreState     *RecordStoreState
	metaDataVersionStamp []byte // nullable — nil if dirty during load
	shared               bool   // true if entry is shared via a cache (needs clone on use)
}

// GetRecordStoreState returns the cached store state.
func (e *FDBRecordStoreStateCacheEntry) GetRecordStoreState() *RecordStoreState {
	return e.recordStoreState
}

// GetMetaDataVersionStamp returns the metadata version stamp captured at load time.
func (e *FDBRecordStoreStateCacheEntry) GetMetaDataVersionStamp() []byte {
	return e.metaDataVersionStamp
}

// handleCachedState applies read conflicts to ensure cache consistency.
// Adds a read conflict on the STORE_INFO key so the transaction will abort
// if another transaction modifies the store header concurrently.
// Matches Java's FDBRecordStoreStateCacheEntry.handleCachedState().
func (e *FDBRecordStoreStateCacheEntry) handleCachedState(ctx *FDBRecordContext, existenceCheck StoreExistenceCheck) error {
	storeInfoKey := e.subspace.Pack(tuple.Tuple{StoreInfoKey})
	// Add read conflict on the exact store info key.
	// Java uses addReadConflictKey which adds a single-key range.
	if err := ctx.Transaction().AddReadConflictKey(fdb.Key(storeInfoKey)); err != nil {
		return err
	}

	// Validate existence check against cached header.
	return checkStoreHeaderExistence(e.recordStoreState.StoreHeader, existenceCheck)
}

// loadCacheEntry loads store state + metadata version stamp from FDB.
// Fires the metadata version stamp read before resolving store state reads,
// allowing all 3 FDB reads to pipeline.
// Matches Java's FDBRecordStoreStateCacheEntry.load().
func loadCacheEntry(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*FDBRecordStoreStateCacheEntry, error) {
	// Fire metadata version stamp read early (snapshot) so it pipelines
	// with the store state reads. Only resolve after store state is done.
	var metaDataFuture fdb.FutureByteSlice
	if !store.context.dirtyMetaDataVersionStamp.Load() {
		metaDataFuture = store.context.Transaction().Snapshot().Get(fdb.Key(metaDataVersionKey))
	}

	// Load store state (header + index states — fires 2 reads in parallel).
	state, err := loadRecordStoreState(store, existenceCheck, getCachedSubspaceKeys(store.subspace))
	if err != nil {
		return nil, err
	}

	// Resolve metadata version stamp.
	var metaDataVersionStamp []byte
	if metaDataFuture != nil {
		metaDataVersionStamp, err = metaDataFuture.Get()
		if err != nil {
			var fdbErr fdb.Error
			if errors.As(err, &fdbErr) && fdbErr.Code == 1036 {
				store.context.dirtyMetaDataVersionStamp.Store(true)
				// Stamp is dirty — treat as nil.
			} else {
				return nil, err
			}
		}
	}

	return &FDBRecordStoreStateCacheEntry{
		subspaceKey:          string(store.subspace.Bytes()),
		subspace:             store.subspace,
		recordStoreState:     state,
		metaDataVersionStamp: metaDataVersionStamp,
	}, nil
}

// loadRecordStoreState reads store header + index states from FDB.
// Issues both GetRange calls (store info + index states) in parallel using
// FDB's future-based API, then resolves sequentially.
// This is a pure function — it does NOT mutate store fields.
// The caller is responsible for setting store.storeHeader and store.indexStates.
// storeSubspaceKeys caches derived subspace keys for a store's subspace.
// Avoids recomputing the same subspace operations on every Open().
type storeSubspaceKeys struct {
	storeBegin          fdb.Key
	storeEnd            fdb.Key
	indexStateBegin     fdb.Key
	indexStateEnd       fdb.Key
	indexStatePrefixLen int
	expectedInfoKey     fdb.Key
	recordsSubspace     subspace.Subspace // cached subspace.Sub(RecordKey)
}

// subspaceKeysCache caches derived subspace keys across all store instances.
// Uses map+RWMutex instead of sync.Map so the compiler can optimize
// string([]byte) in map lookup (no allocation on the read path).
var (
	subspaceKeysCacheMu sync.RWMutex
	subspaceKeysCacheM  = make(map[string]*storeSubspaceKeys)
)

func getCachedSubspaceKeys(ss subspace.Subspace) *storeSubspaceKeys {
	b := ss.Bytes()
	// Read path: string([]byte) in map index is optimized by the compiler
	// to avoid allocation (temporary string, not escaped).
	subspaceKeysCacheMu.RLock()
	if ks, ok := subspaceKeysCacheM[string(b)]; ok {
		subspaceKeysCacheMu.RUnlock()
		return ks
	}
	subspaceKeysCacheMu.RUnlock()

	// Slow path: create and store.
	ks := newStoreSubspaceKeys(ss)
	key := string(b) // must allocate for map store
	subspaceKeysCacheMu.Lock()
	if existing, ok := subspaceKeysCacheM[key]; ok {
		subspaceKeysCacheMu.Unlock()
		return existing // raced, use winner
	}
	subspaceKeysCacheM[key] = ks
	subspaceKeysCacheMu.Unlock()
	return ks
}

func newStoreSubspaceKeys(ss subspace.Subspace) *storeSubspaceKeys {
	storeBegin, storeEnd := ss.FDBRangeKeys()
	isSubspace := ss.Sub(IndexStateSpaceKey)
	isBegin, isEnd := isSubspace.FDBRangeKeys()
	return &storeSubspaceKeys{
		storeBegin:          storeBegin.FDBKey(),
		storeEnd:            storeEnd.FDBKey(),
		indexStateBegin:     isBegin.FDBKey(),
		indexStateEnd:       isEnd.FDBKey(),
		indexStatePrefixLen: len(isSubspace.Bytes()),
		expectedInfoKey:     ss.Pack(tuple.Tuple{StoreInfoKey}),
		recordsSubspace:     ss.Sub(RecordKey),
	}
}

func loadRecordStoreState(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*RecordStoreState, error) {
	tx := store.context.Transaction()

	// Cache derived subspace keys globally — same for every Open() on the same subspace.
	ks := getCachedSubspaceKeys(store.subspace)

	// Fire both range reads in parallel. Index states use snapshot isolation
	// (matching Java's loadIndexStatesAsync which reads at SNAPSHOT).
	// By issuing both before resolving either, the FDB client can pipeline them.
	storeInfoFuture := tx.GetRange(fdb.KeyRange{Begin: ks.storeBegin, End: ks.storeEnd}, fdb.RangeOptions{Limit: 1})
	indexStatesFuture := tx.Snapshot().GetRange(fdb.KeyRange{Begin: ks.indexStateBegin, End: ks.indexStateEnd}, fdb.RangeOptions{})

	// Resolve store info.
	storeKVs, err := storeInfoFuture.GetSliceWithError()
	if err != nil {
		return nil, fmt.Errorf("failed to read store range: %v", err)
	}

	var exists bool
	var header *gen.DataStoreInfo
	if len(storeKVs) > 0 {
		firstKV := storeKVs[0]
		if !bytes.Equal(firstKV.Key, ks.expectedInfoKey) {
			return nil, &RecordStoreNoInfoButNotEmptyError{FirstKey: firstKV.Key}
		}
		header = &gen.DataStoreInfo{}
		if err := header.UnmarshalVT(firstKV.Value); err != nil {
			return nil, fmt.Errorf("failed to parse store header: %v", err)
		}
		exists = true
	}

	if err := checkStoreHeaderExistence(header, existenceCheck); err != nil {
		return nil, err
	}

	// Resolve index states.
	var indexStates map[string]IndexState
	if exists {
		indexKVs, err := indexStatesFuture.GetSliceWithError()
		if err != nil {
			return nil, fmt.Errorf("failed to load index states: %w", err)
		}
		indexStates = make(map[string]IndexState, len(indexKVs))
		for _, kv := range indexKVs {
			t, err := fastSubspaceUnpack(kv.Key, ks.indexStatePrefixLen)
			if err != nil {
				return nil, fmt.Errorf("failed to unpack index state key: %w", err)
			}
			if len(t) == 0 {
				continue
			}
			indexName, ok := t[0].(string)
			if !ok {
				continue
			}
			valueTuple, err := fastUnpack(kv.Value)
			if err != nil {
				return nil, fmt.Errorf("failed to unpack index state value for %q: %w", indexName, err)
			}
			if len(valueTuple) == 0 {
				continue
			}
			code, ok := valueTuple[0].(int64)
			if !ok {
				continue
			}
			state, err := indexStateFromCode(code)
			if err != nil {
				return nil, fmt.Errorf("invalid index state for %q: %w", indexName, err)
			}
			indexStates[indexName] = state
		}
	} else {
		indexStates = make(map[string]IndexState)
	}

	return &RecordStoreState{
		StoreHeader: header,
		IndexStates: indexStates,
	}, nil
}

// checkStoreHeaderExistence validates the existence check against the header.
func checkStoreHeaderExistence(header *gen.DataStoreInfo, check StoreExistenceCheck) error {
	switch check {
	case ExistenceCheckErrorIfExists:
		if header != nil {
			return &RecordStoreAlreadyExistsError{}
		}
	case ExistenceCheckErrorIfNotExists:
		if header == nil {
			return &RecordStoreDoesNotExistError{}
		}
	}
	return nil
}

// PassThroughRecordStoreStateCache is a no-op cache that always loads from FDB.
// This is the default behavior. Matches Java's PassThroughRecordStoreStateCache.
type PassThroughRecordStoreStateCache struct{}

var passThroughInstance = &PassThroughRecordStoreStateCache{}

// PassThroughStoreStateCache returns the singleton pass-through cache.
func PassThroughStoreStateCache() FDBRecordStoreStateCache {
	return passThroughInstance
}

// Get always loads fresh state from FDB. Skips the metadata version stamp
// read since PassThrough has no cache to validate against.
func (c *PassThroughRecordStoreStateCache) Get(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*FDBRecordStoreStateCacheEntry, error) {
	state, err := loadRecordStoreState(store, existenceCheck, getCachedSubspaceKeys(store.subspace))
	if err != nil {
		return nil, err
	}
	return &FDBRecordStoreStateCacheEntry{
		subspaceKey:      string(store.subspace.Bytes()),
		subspace:         store.subspace,
		recordStoreState: state,
	}, nil
}

// Clear is a no-op.
func (c *PassThroughRecordStoreStateCache) Clear() {}

// MetaDataVersionStampStoreStateCache caches store state and validates entries
// using FDB's metadata version stamp (a system key that changes on every mutation).
// Matches Java's MetaDataVersionStampStoreStateCache.
type MetaDataVersionStampStoreStateCache struct {
	mu          sync.Mutex
	entries     map[string]*cacheItem // subspace bytes → cache item
	maxSize     int
	expireAfter time.Duration
}

type cacheItem struct {
	entry      *FDBRecordStoreStateCacheEntry
	lastAccess time.Time
}

// MetaDataVersionStampStoreStateCacheOption configures the cache.
type MetaDataVersionStampStoreStateCacheOption func(*MetaDataVersionStampStoreStateCache)

// WithMaxSize sets the maximum number of entries in the cache.
// Default: 500 (matches Java's default).
func WithMaxSize(n int) MetaDataVersionStampStoreStateCacheOption {
	return func(c *MetaDataVersionStampStoreStateCache) {
		c.maxSize = n
	}
}

// WithExpireAfterAccess sets the TTL for cache entries since last access.
// Default: 1 minute (matches Java's default).
func WithExpireAfterAccess(d time.Duration) MetaDataVersionStampStoreStateCacheOption {
	return func(c *MetaDataVersionStampStoreStateCache) {
		c.expireAfter = d
	}
}

// NewMetaDataVersionStampStoreStateCache creates a new cache with optional configuration.
func NewMetaDataVersionStampStoreStateCache(opts ...MetaDataVersionStampStoreStateCacheOption) *MetaDataVersionStampStoreStateCache {
	c := &MetaDataVersionStampStoreStateCache{
		entries:     make(map[string]*cacheItem),
		maxSize:     500,
		expireAfter: time.Minute,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Get retrieves store state, using the cache when the metadata version stamp matches.
// Matches Java's MetaDataVersionStampStoreStateCache.get().
func (c *MetaDataVersionStampStoreStateCache) Get(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*FDBRecordStoreStateCacheEntry, error) {
	ctx := store.context

	// Fast-fail: if this transaction has already mutated store state, skip cache.
	// Matches Java: context.hasDirtyStoreState() → cache miss.
	if ctx.HasDirtyStoreState() {
		return loadCacheEntry(store, existenceCheck)
	}

	subKey := string(store.subspace.Bytes())

	c.mu.Lock()
	existing := c.getIfPresent(subKey)
	c.mu.Unlock()

	if existing == nil {
		// No cached entry — load from FDB and potentially cache.
		entry, err := loadCacheEntry(store, existenceCheck)
		if err != nil {
			return nil, err
		}
		if entry.recordStoreState.StoreHeader != nil && entry.recordStoreState.StoreHeader.GetCacheable() {
			c.addToCache(subKey, entry)
		}
		return entry, nil
	}

	// Cached entry exists — check if metadata version stamp still matches.
	currentStamp, err := ctx.GetMetaDataVersionStamp()
	if err != nil {
		return nil, err
	}

	if currentStamp == nil || existing.GetMetaDataVersionStamp() == nil ||
		!bytes.Equal(currentStamp, existing.GetMetaDataVersionStamp()) {
		// Version mismatch — cache miss, reload.
		entry, err := loadCacheEntry(store, existenceCheck)
		if err != nil {
			return nil, err
		}
		if currentStamp != nil {
			if entry.recordStoreState.StoreHeader != nil && entry.recordStoreState.StoreHeader.GetCacheable() {
				c.addToCache(subKey, entry)
			} else {
				c.invalidateOlderEntry(subKey, currentStamp)
			}
		}
		return entry, nil
	}

	// Version matches — cache hit!
	if err := existing.handleCachedState(ctx, existenceCheck); err != nil {
		return nil, err
	}
	existing.shared = true // Shared via cache — must clone on use
	return existing, nil
}

// Clear removes all cached entries.
func (c *MetaDataVersionStampStoreStateCache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]*cacheItem)
	c.mu.Unlock()
}

// getIfPresent returns the cached entry if it exists and hasn't expired. Caller holds mu.
func (c *MetaDataVersionStampStoreStateCache) getIfPresent(key string) *FDBRecordStoreStateCacheEntry {
	item, ok := c.entries[key]
	if !ok {
		return nil
	}
	if time.Since(item.lastAccess) > c.expireAfter {
		delete(c.entries, key)
		return nil
	}
	item.lastAccess = time.Now()
	return item.entry
}

// addToCache adds or merges a cache entry. Keeps the newer versionstamp.
// Matches Java's MetaDataVersionStampStoreStateCache.addToCache().
func (c *MetaDataVersionStampStoreStateCache) addToCache(key string, entry *FDBRecordStoreStateCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry.shared = true // Will be returned to multiple callers from cache

	if existing, ok := c.entries[key]; ok {
		newer := getNewerEntry(existing.entry, entry)
		if newer.recordStoreState.StoreHeader != nil && newer.recordStoreState.StoreHeader.GetCacheable() {
			newer.shared = true
			c.entries[key] = &cacheItem{entry: newer, lastAccess: time.Now()}
		} else {
			delete(c.entries, key)
		}
	} else {
		c.entries[key] = &cacheItem{entry: entry, lastAccess: time.Now()}
		c.evictIfNeeded()
	}
}

// invalidateOlderEntry removes a cached entry if its versionstamp is older
// than the given stamp. Matches Java's invalidateOlderEntry().
func (c *MetaDataVersionStampStoreStateCache) invalidateOlderEntry(key string, stamp []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if existing, ok := c.entries[key]; ok {
		if existing.entry.metaDataVersionStamp == nil ||
			bytes.Compare(existing.entry.metaDataVersionStamp, stamp) < 0 {
			delete(c.entries, key)
		}
	}
}

// getNewerEntry returns the entry with the larger (unsigned) metadata versionstamp.
// Matches Java's MetaDataVersionStampStoreStateCache.getNewerEntry().
func getNewerEntry(a, b *FDBRecordStoreStateCacheEntry) *FDBRecordStoreStateCacheEntry {
	if a.metaDataVersionStamp == nil {
		return b
	}
	if b.metaDataVersionStamp == nil {
		return a
	}
	if bytes.Compare(a.metaDataVersionStamp, b.metaDataVersionStamp) >= 0 {
		return a
	}
	return b
}

// evictIfNeeded removes the oldest accessed entry if over capacity. Caller holds mu.
func (c *MetaDataVersionStampStoreStateCache) evictIfNeeded() {
	if len(c.entries) <= c.maxSize {
		return
	}
	// Find oldest accessed entry.
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, item := range c.entries {
		if first || item.lastAccess.Before(oldestTime) {
			oldestKey = k
			oldestTime = item.lastAccess
			first = false
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}
