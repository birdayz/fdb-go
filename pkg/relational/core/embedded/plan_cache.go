package embedded

import (
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanCache caches Cascades query plans keyed by normalized SQL hash.
// Thread-safe for concurrent access. Uses a simple LRU eviction
// strategy with a configurable maximum size.
type PlanCache struct {
	mu      sync.RWMutex
	entries map[uint64]*planCacheEntry
	order   []uint64 // LRU order: most recently used at end
	maxSize int
	hits    atomic.Int64
	misses  atomic.Int64
}

type planCacheEntry struct {
	sql        string // original SQL for debugging
	plan       plans.RecordQueryPlan
	scalarSubs []scalarSubqueryBinding
	hash       uint64
}

// NewPlanCache creates a plan cache with the given maximum number of entries.
// If maxSize <= 0, it defaults to 256.
func NewPlanCache(maxSize int) *PlanCache {
	if maxSize <= 0 {
		maxSize = 256
	}
	return &PlanCache{
		entries: make(map[uint64]*planCacheEntry, maxSize),
		order:   make([]uint64, 0, maxSize),
		maxSize: maxSize,
	}
}

// Get looks up a cached plan by SQL hash. Returns the plan, scalar
// subquery bindings, and true on a cache hit; nil, nil, false on miss.
func (c *PlanCache) Get(sqlHash uint64) (plans.RecordQueryPlan, []scalarSubqueryBinding, bool) {
	c.mu.Lock()
	entry, ok := c.entries[sqlHash]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, nil, false
	}
	c.promote(sqlHash)
	c.mu.Unlock()

	c.hits.Add(1)
	return entry.plan, entry.scalarSubs, true
}

// Put stores a plan in the cache. If the cache is at capacity, the
// least recently used entry is evicted.
func (c *PlanCache) Put(sqlHash uint64, sql string, plan plans.RecordQueryPlan, subs []scalarSubqueryBinding) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If already present, update in place and promote.
	if _, exists := c.entries[sqlHash]; exists {
		c.entries[sqlHash] = &planCacheEntry{
			sql:        sql,
			plan:       plan,
			scalarSubs: subs,
			hash:       sqlHash,
		}
		c.promote(sqlHash)
		return
	}

	// Evict LRU if at capacity.
	for len(c.order) >= c.maxSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}

	c.entries[sqlHash] = &planCacheEntry{
		sql:        sql,
		plan:       plan,
		scalarSubs: subs,
		hash:       sqlHash,
	}
	c.order = append(c.order, sqlHash)
}

// Invalidate clears all cached entries. Must be called when schema
// metadata changes (DDL: CREATE/DROP TABLE, CREATE/DROP INDEX, etc.).
func (c *PlanCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[uint64]*planCacheEntry, c.maxSize)
	c.order = c.order[:0]
}

// Stats returns the cumulative hit and miss counts.
func (c *PlanCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// promote moves sqlHash to the end of the LRU order slice.
// Caller must hold c.mu write lock.
func (c *PlanCache) promote(sqlHash uint64) {
	for i, h := range c.order {
		if h == sqlHash {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, sqlHash)
			return
		}
	}
}
