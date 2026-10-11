// Portions derived from FoundationDB Record Layer (RelationalPlanCache.java,
// MultiStageCache.java),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"container/list"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
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
	items   map[cacheKey]map[string]*list.Element
	maxSize int
	hits    atomic.Int64
	misses  atomic.Int64
}

// cacheKey is the statement key: a COMPARABLE struct of the verbatim scope and
// the literal-stripped text, so a lookup visits only that statement's variants.
type cacheKey struct {
	scope string
	sql   string
}

type planCacheEntry struct {
	plan         plans.RecordQueryPlan
	scalarSubs   []PlannedScalarSubquery
	outputLabels []string
	constraint   queryBindingConstraint
	planHash     uint64
}

// lruItem carries its statement key and constraint variant, so eviction from
// the list front deletes the matching map entry in O(1).
type lruItem struct {
	key     cacheKey
	variant string
	entry   *planCacheEntry
}

// NewPlanCache creates a plan cache with the given maximum number of plans.
func NewPlanCache(maxSize int) *PlanCache {
	if maxSize <= 0 {
		maxSize = 256
	}
	return &PlanCache{
		ll:      list.New(),
		items:   make(map[cacheKey]map[string]*list.Element, maxSize),
		maxSize: maxSize,
	}
}

// Get looks up a literal-free statement's plan.
func (c *PlanCache) Get(scope, sql string) (plans.RecordQueryPlan, []PlannedScalarSubquery, bool) {
	entry, ok := c.lookup("", cacheKey{scope: scope, sql: sql}, queryBindings{})
	if !ok {
		return nil, nil, false
	}
	return entry.plan, entry.scalarSubs, true
}

// Put stores a literal-free statement's plan.
func (c *PlanCache) Put(scope, sql string, plan plans.RecordQueryPlan, subs []PlannedScalarSubquery) {
	c.store("", cacheKey{scope: scope, sql: sql}, queryBindings{}, &planCacheEntry{plan: plan, scalarSubs: subs})
}

// Invalidate clears all cached entries. Must be called when schema
// metadata changes (DDL: CREATE/DROP TABLE, CREATE/DROP INDEX, etc.).
func (c *PlanCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[cacheKey]map[string]*list.Element, c.maxSize)
}

// Stats returns the cumulative hit and miss counts.
func (c *PlanCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// Len returns the number of entries currently held by the cache. O(1).
func (c *PlanCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
