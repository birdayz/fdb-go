package recordlayer

import (
	"os"
	"strconv"
	"sync"
)

// defaultSharedCacheNodes is a process-wide default for SharedCacheMaxNodes,
// taken from HNSW_SHARED_CACHE_NODES at startup. It lets an operator turn the
// cross-transaction node cache on for every HNSW index without editing each
// index's options; a non-zero per-index SharedCacheMaxNodes still wins. 0 = off.
var defaultSharedCacheNodes = func() int {
	if v := os.Getenv("HNSW_SHARED_CACHE_NODES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}()

// sharedNodeCache is a process-wide, cross-transaction cache of parsed HNSW
// nodes for one index subspace. It exists to keep insert throughput from
// collapsing as the graph grows: without it every transaction starts with a
// cold per-tx cache and re-reads the hot upper layers from FDB on every insert,
// so build time degrades super-linearly (46 vec/s at 1K -> 10.7 at 10K observed).
//
// Correctness scope — single-writer indexing + read-only search, which is how
// HNSW indexes are actually used (one OnlineIndexer builds; queries read):
//   - A read miss populates the entry from the committed FDB value.
//   - A write invalidates the entry; the next read repopulates from FDB. If the
//     writing transaction aborts/retries, the entry is simply gone and the next
//     read fetches the still-committed value — so an abort never leaves stale
//     data behind.
//   - Within a single transaction, read-your-writes is served by the per-tx
//     hnswStorage.cache (which holds this tx's writes), so the shared cache is
//     only ever populated from committed reads.
//
// It deliberately does NOT do cross-process or concurrent-writer coherence: a
// second writer (another process, or a concurrent build) would not invalidate
// this process's entries. HNSW builds are single-writer, so that's acceptable;
// the cache is opt-in (SharedCacheMaxNodes > 0) and off by default. Do not run a
// concurrent build against an index while reads use this cache.
type sharedNodeCache struct {
	mu       sync.RWMutex
	entries  map[string]*parsedNode // nil value == cached negative (key absent)
	maxNodes int
}

func newSharedNodeCache(maxNodes int) *sharedNodeCache {
	return &sharedNodeCache{
		entries:  make(map[string]*parsedNode, 1024),
		maxNodes: maxNodes,
	}
}

// get returns (node, true) on hit (node may be nil for a cached negative).
func (c *sharedNodeCache) get(key string) (*parsedNode, bool) {
	c.mu.RLock()
	n, ok := c.entries[key]
	c.mu.RUnlock()
	return n, ok
}

func (c *sharedNodeCache) put(key string, node *parsedNode) {
	c.mu.Lock()
	if c.maxNodes > 0 && len(c.entries) >= c.maxNodes {
		if _, exists := c.entries[key]; !exists {
			c.evictLocked()
		}
	}
	c.entries[key] = node
	c.mu.Unlock()
}

func (c *sharedNodeCache) invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// clear drops every entry — used when the whole index subspace is cleared.
func (c *sharedNodeCache) clear() {
	c.mu.Lock()
	c.entries = make(map[string]*parsedNode, 1024)
	c.mu.Unlock()
}

// evictLocked drops ~1/16 of entries using Go's randomized map iteration —
// approximate, allocation-free; hot nodes are re-cached on next access.
func (c *sharedNodeCache) evictLocked() {
	drop := c.maxNodes/16 + 1
	for k := range c.entries {
		if drop <= 0 {
			break
		}
		delete(c.entries, k)
		drop--
	}
}

func (c *sharedNodeCache) len() int {
	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	return n
}

// sharedNodeCacheRegistry hands out one sharedNodeCache per index subspace, so
// every per-transaction hnswStorage for the same index shares one cache.
var (
	sharedCacheRegistryMu sync.Mutex
	sharedCacheRegistry   = map[string]*sharedNodeCache{}
)

func getSharedNodeCache(subspaceKey string, maxNodes int) *sharedNodeCache {
	sharedCacheRegistryMu.Lock()
	defer sharedCacheRegistryMu.Unlock()
	c, ok := sharedCacheRegistry[subspaceKey]
	if !ok {
		c = newSharedNodeCache(maxNodes)
		sharedCacheRegistry[subspaceKey] = c
	}
	return c
}
