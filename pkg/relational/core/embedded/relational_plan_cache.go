// Portions derived from FoundationDB Record Layer (RelationalPlanCache.java,
// RelationalMetric.java, MultiStageCache.java, QueryCacheKey.java,
// and others),
// Copyright 2021-2025 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package embedded

import (
	"container/list"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
)

// queryPlanCache is what the planner caches plans in: a key's plan is found by
// the schema template's name, the query key (the verbatim scope and the query
// text) and the plan's binding constraints.
type queryPlanCache interface {
	lookup(template string, key cacheKey, bindings queryBindings) (*planCacheEntry, bool)
	store(template string, key cacheKey, bindings queryBindings, entry *planCacheEntry)
	// numEntries is Java's primaryCacheNumEntries, which plan logging reports.
	numEntries() int
	Invalidate()
}

// RelationalPlanCache is Java's RelationalPlanCache (a MultiStageCache): the
// plan cache one engine shares across all of its connections. It has three
// stages, each a size-bounded LRU with a time to live:
//
//   - primary: the schema template's name; entries expire after the TTL
//     without access (Caffeine expireAfterAccess);
//   - secondary: the query key, Java's QueryCacheKey: metadata version,
//     planner options, index states, temporary functions and normalized SQL.
//     Store-specific collected statistics additionally scope database/schema;
//   - tertiary: the plan's equivalence, Java's PhysicalPlanEquivalence: types,
//     equal-input aliases, and exact values consumed by specialized planning.
//
// Secondary and tertiary entries expire the TTL after they were written
// (expireAfterWrite); expiry is lazy, on access. The sizes and TTLs are the
// PLAN_CACHE_* options (Java's RelationalCacheBuilder defaults: 1024 / 10 s,
// 256 / 30 s, 8 / 30 s).
type RelationalPlanCache struct {
	mu      sync.Mutex
	limits  planCacheLimits
	now     func() time.Time
	primary *planCacheStage[string, *planCacheStage[cacheKey, *planCacheStage[string, *planCacheEntry]]]

	// Java's RelationalMetric plan-cache counts (MultiStageCache.reduce).
	primaryMiss, secondaryMiss, tertiaryHit, tertiaryMiss         atomic.Int64
	primaryLRUEviction, secondaryLRUEviction, tertiaryLRUEviction atomic.Int64
}

type planCacheLimits struct {
	primarySize, secondarySize, tertiarySize int
	primaryTTL, secondaryTTL, tertiaryTTL    time.Duration
}

// PlanCacheCounts is a snapshot of a RelationalPlanCache's counters, Java's
// PLAN_CACHE_* RelationalMetric counts.
type PlanCacheCounts struct {
	PrimaryMiss, SecondaryMiss, TertiaryHit, TertiaryMiss         int64
	PrimaryLRUEviction, SecondaryLRUEviction, TertiaryLRUEviction int64
}

// NewRelationalPlanCache builds the engine-wide plan cache from the PLAN_CACHE_*
// options in opts, defaulting each to Java's.
func NewRelationalPlanCache(opts *api.Options) *RelationalPlanCache {
	defaults := api.DefaultOptionValues()
	intOf := func(name api.OptionName) int64 {
		var v any
		if opts != nil {
			v = opts.Get(name)
		}
		if v == nil {
			v = defaults[name]
		}
		switch n := v.(type) {
		case int:
			return int64(n)
		case int32:
			return int64(n)
		case int64:
			return n
		}
		return 0
	}
	ms := func(name api.OptionName) time.Duration { return time.Duration(intOf(name)) * time.Millisecond }
	return newRelationalPlanCache(planCacheLimits{
		primarySize:   int(intOf(api.OptPlanCachePrimaryMaxEntries)),
		secondarySize: int(intOf(api.OptPlanCacheSecondaryMaxEntries)),
		tertiarySize:  int(intOf(api.OptPlanCacheTertiaryMaxEntries)),
		primaryTTL:    ms(api.OptPlanCachePrimaryTimeToLiveMillis),
		secondaryTTL:  ms(api.OptPlanCacheSecondaryTimeToLiveMillis),
		tertiaryTTL:   ms(api.OptPlanCacheTertiaryTimeToLiveMillis),
	}, time.Now)
}

func newRelationalPlanCache(limits planCacheLimits, now func() time.Time) *RelationalPlanCache {
	c := &RelationalPlanCache{limits: limits, now: now}
	c.primary = newPlanCacheStage[string, *planCacheStage[cacheKey, *planCacheStage[string, *planCacheEntry]]](
		limits.primarySize, limits.primaryTTL, true, &c.primaryLRUEviction)
	return c
}

func (c *RelationalPlanCache) newSecondary() *planCacheStage[cacheKey, *planCacheStage[string, *planCacheEntry]] {
	return newPlanCacheStage[cacheKey, *planCacheStage[string, *planCacheEntry]](
		c.limits.secondarySize, c.limits.secondaryTTL, false, &c.secondaryLRUEviction)
}

func (c *RelationalPlanCache) newTertiary() *planCacheStage[string, *planCacheEntry] {
	return newPlanCacheStage[string, *planCacheEntry](c.limits.tertiarySize, c.limits.tertiaryTTL, false, &c.tertiaryLRUEviction)
}

// stages returns the template's secondary stage and the key's tertiary stage,
// creating either when absent; count says whether a creation counts as
// Java's PRIMARY_MISS / SECONDARY_MISS (a lookup's do, a store's do not).
func (c *RelationalPlanCache) stages(template string, key cacheKey, count bool) *planCacheStage[string, *planCacheEntry] {
	now := c.now()
	secondary, ok := c.primary.get(template, now)
	if !ok {
		if count {
			c.primaryMiss.Add(1)
		}
		secondary = c.newSecondary()
		c.primary.put(template, secondary, now)
	}
	tertiary, ok := secondary.get(key, now)
	if !ok {
		if count {
			c.secondaryMiss.Add(1)
		}
		tertiary = c.newTertiary()
		secondary.put(key, tertiary, now)
	}
	return tertiary
}

func (c *RelationalPlanCache) lookup(template string, key cacheKey, bindings queryBindings) (*planCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stage := c.stages(template, key, true)
	now := c.now()
	var best *planCacheEntry
	var bestKey string
	for el := stage.ll.Front(); el != nil; {
		next := el.Next()
		item := el.Value.(*planCacheStageItem[string, *planCacheEntry])
		if stage.expired(item, now) {
			stage.remove(item.key)
		} else if item.value.constraint.accepts(bindings) && cachePlanPrecedes(item.value, best) {
			best, bestKey = item.value, item.key
		}
		el = next
	}
	if best == nil {
		c.tertiaryMiss.Add(1)
		return nil, false
	}
	stage.get(bestKey, now)
	c.tertiaryHit.Add(1)
	return &planCacheEntry{plan: best.plan, scalarSubs: best.scalarSubs, outputLabels: slices.Clone(best.outputLabels)}, true
}

func (c *RelationalPlanCache) store(template string, key cacheKey, bindings queryBindings, entry *planCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	constraint := bindings.constraint()
	stored := &planCacheEntry{plan: entry.plan, scalarSubs: entry.scalarSubs, outputLabels: slices.Clone(entry.outputLabels), constraint: constraint}
	c.stages(template, key, false).put(constraint.key(), stored, c.now())
}

func (c *RelationalPlanCache) numEntries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.primary.len(c.now())
}

// Invalidate drops every plan.
func (c *RelationalPlanCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.primary.clear()
}

// Counts returns the cache's counters.
func (c *RelationalPlanCache) Counts() PlanCacheCounts {
	return PlanCacheCounts{
		PrimaryMiss: c.primaryMiss.Load(), SecondaryMiss: c.secondaryMiss.Load(),
		TertiaryHit: c.tertiaryHit.Load(), TertiaryMiss: c.tertiaryMiss.Load(),
		PrimaryLRUEviction: c.primaryLRUEviction.Load(), SecondaryLRUEviction: c.secondaryLRUEviction.Load(),
		TertiaryLRUEviction: c.tertiaryLRUEviction.Load(),
	}
}

// planCacheStage is one stage: an LRU of at most maxSize entries whose
// entries expire ttl after they were written, or after they were last read
// when expireAfterAccess. A zero ttl never expires. evictions counts entries
// the size bound evicted (Java's RemovalCause.SIZE). Not safe for concurrent
// use; RelationalPlanCache holds its lock around every call.
type planCacheStage[K comparable, V any] struct {
	ll                *list.List // of *planCacheStageItem; front = least recently used
	items             map[K]*list.Element
	maxSize           int
	ttl               time.Duration
	expireAfterAccess bool
	evictions         *atomic.Int64
}

type planCacheStageItem[K comparable, V any] struct {
	key   K
	value V
	stamp time.Time
}

func newPlanCacheStage[K comparable, V any](maxSize int, ttl time.Duration, expireAfterAccess bool, evictions *atomic.Int64) *planCacheStage[K, V] {
	if maxSize < 1 {
		maxSize = 1
	}
	return &planCacheStage[K, V]{
		ll: list.New(), items: map[K]*list.Element{}, maxSize: maxSize,
		ttl: ttl, expireAfterAccess: expireAfterAccess, evictions: evictions,
	}
}

func (s *planCacheStage[K, V]) expired(it *planCacheStageItem[K, V], now time.Time) bool {
	return s.ttl > 0 && now.Sub(it.stamp) >= s.ttl
}

// get returns the live value of key, promoting it.
func (s *planCacheStage[K, V]) get(key K, now time.Time) (V, bool) {
	el, ok := s.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	it := el.Value.(*planCacheStageItem[K, V])
	if s.expired(it, now) {
		s.remove(key)
		var zero V
		return zero, false
	}
	if s.expireAfterAccess {
		it.stamp = now
	}
	s.ll.MoveToBack(el)
	return it.value, true
}

// put writes key's value and evicts the least recently used entries past the
// size bound.
func (s *planCacheStage[K, V]) put(key K, value V, now time.Time) {
	if el, ok := s.items[key]; ok {
		it := el.Value.(*planCacheStageItem[K, V])
		it.value, it.stamp = value, now
		s.ll.MoveToBack(el)
		return
	}
	s.items[key] = s.ll.PushBack(&planCacheStageItem[K, V]{key: key, value: value, stamp: now})
	for s.ll.Len() > s.maxSize {
		oldest := s.ll.Front()
		s.ll.Remove(oldest)
		delete(s.items, oldest.Value.(*planCacheStageItem[K, V]).key)
		s.evictions.Add(1)
	}
}

func (s *planCacheStage[K, V]) remove(key K) {
	if el, ok := s.items[key]; ok {
		s.ll.Remove(el)
		delete(s.items, key)
	}
}

// len is the number of live entries; it drops the expired ones.
func (s *planCacheStage[K, V]) len(now time.Time) int {
	for el := s.ll.Front(); el != nil; {
		next := el.Next()
		if it := el.Value.(*planCacheStageItem[K, V]); s.expired(it, now) {
			s.ll.Remove(el)
			delete(s.items, it.key)
		}
		el = next
	}
	return s.ll.Len()
}

func (s *planCacheStage[K, V]) clear() {
	s.ll.Init()
	s.items = map[K]*list.Element{}
}

// Java's StableSelectorCostModel chooses the smallest plan hash when multiple
// cached specializations admit the current bindings.
func cachePlanPrecedes(candidate, current *planCacheEntry) bool {
	return current == nil || plans.PlanHash(candidate.plan) < plans.PlanHash(current.plan)
}

func (c *PlanCache) lookup(_ string, key cacheKey, bindings queryBindings) (*planCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *list.Element
	var entry *planCacheEntry
	for el := c.ll.Front(); el != nil; el = el.Next() {
		item := el.Value.(*lruItem)
		if item.key.scope == key.scope && item.key.sql == key.sql && item.entry.constraint.accepts(bindings) && cachePlanPrecedes(item.entry, entry) {
			best, entry = el, item.entry
		}
	}
	if best == nil {
		c.misses.Add(1)
		return nil, false
	}
	c.ll.MoveToBack(best)
	c.hits.Add(1)
	return &planCacheEntry{plan: entry.plan, scalarSubs: entry.scalarSubs, outputLabels: slices.Clone(entry.outputLabels)}, true
}

func (c *PlanCache) store(_ string, key cacheKey, bindings queryBindings, entry *planCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	constraint := bindings.constraint()
	key.equivalence = constraint.key()
	stored := &planCacheEntry{plan: entry.plan, scalarSubs: entry.scalarSubs, outputLabels: slices.Clone(entry.outputLabels), constraint: constraint}
	if el, ok := c.items[key]; ok {
		el.Value.(*lruItem).entry = stored
		c.ll.MoveToBack(el)
		return
	}
	c.items[key] = c.ll.PushBack(&lruItem{key: key, entry: stored})
	for c.ll.Len() > c.maxSize {
		oldest := c.ll.Front()
		delete(c.items, oldest.Value.(*lruItem).key)
		c.ll.Remove(oldest)
	}
}

func (c *PlanCache) numEntries() int { return c.Len() }
