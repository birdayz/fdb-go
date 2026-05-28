package embedded

import (
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanCache caches Cascades query plans keyed by normalized SQL text.
// Thread-safe for concurrent access. Uses a simple LRU eviction
// strategy with a configurable maximum size.
//
// See RFC-029: keys on the full normalized SQL string to eliminate
// hash-collision correctness bugs (previously keyed on uint64 FNV-64a).
type PlanCache struct {
	mu      sync.RWMutex
	entries map[string]*planCacheEntry
	order   []string // LRU order: most recently used at end
	maxSize int
	hits    atomic.Int64
	misses  atomic.Int64
}

type planCacheEntry struct {
	plan       plans.RecordQueryPlan
	scalarSubs []scalarSubqueryBinding
}

// NewPlanCache creates a plan cache with the given maximum number of entries.
// If maxSize <= 0, it defaults to 256.
func NewPlanCache(maxSize int) *PlanCache {
	if maxSize <= 0 {
		maxSize = 256
	}
	return &PlanCache{
		entries: make(map[string]*planCacheEntry, maxSize),
		order:   make([]string, 0, maxSize),
		maxSize: maxSize,
	}
}

// Get looks up a cached plan by SQL text. The SQL is normalized
// internally (case-folded, whitespace-collapsed, comments stripped)
// before lookup. Returns the plan, scalar subquery bindings, and true
// on a cache hit; nil, nil, false on miss.
func (c *PlanCache) Get(sql string) (plans.RecordQueryPlan, []scalarSubqueryBinding, bool) {
	key := normalizeSQL(sql)

	c.mu.Lock()
	entry, ok := c.entries[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, nil, false
	}
	c.promote(key)
	c.mu.Unlock()

	c.hits.Add(1)
	return entry.plan, entry.scalarSubs, true
}

// Put stores a plan in the cache keyed by normalized SQL text. If the
// cache is at capacity, the least recently used entry is evicted.
func (c *PlanCache) Put(sql string, plan plans.RecordQueryPlan, subs []scalarSubqueryBinding) {
	key := normalizeSQL(sql)

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; exists {
		c.entries[key] = &planCacheEntry{
			plan:       plan,
			scalarSubs: subs,
		}
		c.promote(key)
		return
	}

	for len(c.order) >= c.maxSize {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}

	c.entries[key] = &planCacheEntry{
		plan:       plan,
		scalarSubs: subs,
	}
	c.order = append(c.order, key)
}

// Invalidate clears all cached entries. Must be called when schema
// metadata changes (DDL: CREATE/DROP TABLE, CREATE/DROP INDEX, etc.).
func (c *PlanCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*planCacheEntry, c.maxSize)
	c.order = c.order[:0]
}

// Stats returns the cumulative hit and miss counts.
func (c *PlanCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// promote moves key to the end of the LRU order slice.
// Caller must hold c.mu write lock.
func (c *PlanCache) promote(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			return
		}
	}
}
