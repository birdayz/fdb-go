package embedded

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanCache caches Cascades query plans keyed by normalized SQL text.
// Thread-safe for concurrent access. Uses an LRU eviction strategy with a
// configurable maximum size.
//
// LRU order is tracked with a doubly-linked list (front = least recently
// used, back = most recently used) paired with a map from key to list
// element. Promotion on hit/update and eviction of the oldest entry are all
// O(1), matching Java's Caffeine-backed plan cache (RelationalPlanCache /
// MultiStageCache, which uses maximumSize LRU eviction). The previous
// slice-based order tracking linear-scanned on every hit — O(n) under the
// lock — which became a contention point at large cache sizes.
//
// See RFC-029: keys on the full normalized SQL string to eliminate
// hash-collision correctness bugs (previously keyed on uint64 FNV-64a).
// See RFC-033: O(1) LRU via container/list.
type PlanCache struct {
	// Get reorders the LRU list, so the read path needs the exclusive lock
	// anyway — a plain Mutex, not an RWMutex.
	mu      sync.Mutex
	ll      *list.List // values are *lruItem; front = LRU, back = MRU
	items   map[string]*list.Element
	maxSize int
	hits    atomic.Int64
	misses  atomic.Int64
}

type planCacheEntry struct {
	plan       plans.RecordQueryPlan
	scalarSubs []scalarSubqueryBinding
}

// lruItem is the value stored in each list element. It carries its own key
// so eviction (which starts from the list front) can delete the matching
// map entry in O(1).
type lruItem struct {
	key   string
	entry *planCacheEntry
}

// NewPlanCache creates a plan cache with the given maximum number of entries.
// If maxSize <= 0, it defaults to 256.
func NewPlanCache(maxSize int) *PlanCache {
	if maxSize <= 0 {
		maxSize = 256
	}
	return &PlanCache{
		ll:      list.New(),
		items:   make(map[string]*list.Element, maxSize),
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
	el, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		c.misses.Add(1)
		return nil, nil, false
	}
	c.ll.MoveToBack(el)
	entry := el.Value.(*lruItem).entry
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

	if el, exists := c.items[key]; exists {
		// Update in place and promote. Size is unchanged, so no eviction.
		el.Value.(*lruItem).entry = &planCacheEntry{plan: plan, scalarSubs: subs}
		c.ll.MoveToBack(el)
		return
	}

	el := c.ll.PushBack(&lruItem{
		key:   key,
		entry: &planCacheEntry{plan: plan, scalarSubs: subs},
	})
	c.items[key] = el

	// Evict the least recently used entries until back within capacity.
	for c.ll.Len() > c.maxSize {
		oldest := c.ll.Front()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*lruItem).key)
	}
}

// Invalidate clears all cached entries. Must be called when schema
// metadata changes (DDL: CREATE/DROP TABLE, CREATE/DROP INDEX, etc.).
func (c *PlanCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[string]*list.Element, c.maxSize)
}

// Stats returns the cumulative hit and miss counts.
func (c *PlanCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}
