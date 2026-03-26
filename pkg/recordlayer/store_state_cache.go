package recordlayer

import (
	"bytes"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"

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

// loadCacheEntry loads store state + metadata version stamp from FDB in parallel.
// Matches Java's FDBRecordStoreStateCacheEntry.load().
func loadCacheEntry(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*FDBRecordStoreStateCacheEntry, error) {
	// Load store state (header + index states).
	state, err := loadRecordStoreState(store, existenceCheck)
	if err != nil {
		return nil, err
	}

	// Read metadata version stamp at snapshot isolation.
	metaDataVersionStamp, err := store.context.GetMetaDataVersionStamp()
	if err != nil {
		return nil, err
	}

	return &FDBRecordStoreStateCacheEntry{
		subspaceKey:          string(store.subspace.Bytes()),
		subspace:             store.subspace,
		recordStoreState:     state,
		metaDataVersionStamp: metaDataVersionStamp,
	}, nil
}

// loadRecordStoreState reads store header + index states from FDB.
// This is a pure function — it does NOT mutate store fields.
// The caller is responsible for setting store.storeHeader and store.indexStates.
func loadRecordStoreState(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*RecordStoreState, error) {
	exists, header, err := store.checkStoreExists()
	if err != nil {
		return nil, err
	}

	if err := checkStoreHeaderExistence(header, existenceCheck); err != nil {
		return nil, err
	}

	var indexStates map[string]IndexState
	if exists {
		var err error
		indexStates, err = readIndexStates(store.context.Transaction(), store.subspace)
		if err != nil {
			return nil, err
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

// Get always loads fresh state from FDB.
func (c *PassThroughRecordStoreStateCache) Get(store *FDBRecordStore, existenceCheck StoreExistenceCheck) (*FDBRecordStoreStateCacheEntry, error) {
	return loadCacheEntry(store, existenceCheck)
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

	if existing, ok := c.entries[key]; ok {
		newer := getNewerEntry(existing.entry, entry)
		if newer.recordStoreState.StoreHeader != nil && newer.recordStoreState.StoreHeader.GetCacheable() {
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
