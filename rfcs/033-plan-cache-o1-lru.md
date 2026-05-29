# RFC-033: O(1) Plan Cache LRU

**Status:** Implemented
**Item:** P2.1 (TODO.md — "fix before scaling operations")

## Problem

`PlanCache` (`pkg/relational/core/embedded/plan_cache.go`) tracks LRU order in a
`[]string` slice. Every cache **hit** calls `promote(key)`, which:

1. Linear-scans `c.order` to find the key — **O(n)**.
2. Splices it out with `append(c.order[:i], c.order[i+1:]...)` — **O(n)** memory move.
3. Re-appends at the tail.

All of this runs under `c.mu` (a write lock, taken even on the read path because
`Get` calls `promote`). At the default 256 entries this is invisible. But the
cache size is operator-tunable via `PLAN_CACHE_PRIMARY_MAX_ENTRIES`, documented
up to 1024. At 1024 entries a hot read-mostly workload — exactly the case the
cache exists to serve — turns every hit into a ~1024-element scan + slice
rewrite under a global write lock. That is a concurrency contention point that
gets worse the more useful the cache is.

Eviction in `Put` has the same shape: `c.order = c.order[1:]` re-slices (and
leaks the backing array head until the slice is regrown), and the existing-key
update path also calls the O(n) `promote`.

## Investigation

**Java reference.** Java's plan cache (`fdb-relational-core` —
`RelationalPlanCache` → `MultiStageCache`) is built on **Caffeine**
(`Caffeine.newBuilder().recordStats().maximumSize(size)`,
`MultiStageCache.java:140`). Caffeine is an O(1) amortized LRU/W-TinyLFU cache.
So Go's O(n) slice LRU is a **Go-only divergence** from Java's O(1) eviction —
exactly the kind of "Go-only invention" CLAUDE.md says to treat as suspect. The
fix realigns Go with Java's complexity class.

**Go's standard answer.** The canonical O(1) LRU in Go is a `container/list`
doubly-linked list (intrusive node carries the key+value) paired with a
`map[string]*list.Element` for O(1) lookup. `MoveToBack`, `PushBack`,
`Remove`, and `Front` are all O(1). This is the same structure Go's own
historical `groupcache`/`lru` packages use.

## Fix

Replace `entries map[string]*planCacheEntry` + `order []string` with:

```go
type PlanCache struct {
    mu      sync.Mutex          // single lock; Get mutates LRU so RWMutex bought nothing
    ll      *list.List          // *lruItem, front = LRU, back = MRU
    items   map[string]*list.Element
    maxSize int
    hits, misses atomic.Int64
}

type lruItem struct {
    key   string
    entry *planCacheEntry
}
```

- **Get hit:** `el := c.items[key]; c.ll.MoveToBack(el)` — O(1).
- **Put new:** `el := c.ll.PushBack(&lruItem{key, entry}); c.items[key] = el`,
  then while `c.ll.Len() > maxSize` evict `c.ll.Front()` (remove from both
  list and map) — O(1) per op.
- **Put existing:** update the item's `entry` in place, `MoveToBack` — O(1).
- **Invalidate:** new list + new map — O(1) alloc, same as today.

`promote()` (the O(n) scan) is **deleted**. The `RWMutex` becomes a plain
`Mutex`: the read path already takes the write lock (it reorders the list), so
the read/write distinction never bought concurrency — dropping it removes a
misleading signal, no behavior change.

Public API (`Get`, `Put`, `Invalidate`, `Stats`, `NewPlanCache`) and all
semantics (normalized-SQL keying, LRU eviction order, stats counting) are
unchanged. This is a pure internal data-structure swap.

## Performance

- promote/evict: **O(n) → O(1)**. At maxSize=1024 the per-hit work drops from a
  1024-element scan + slice rewrite to three pointer assignments.
- No extra allocation on the hot path (Get hit allocates nothing; today's
  `promote` also allocated nothing but did O(n) work). `Put` allocates one
  `*list.Element` + `*lruItem` per new key — comparable to today's slice
  append amortized, and no array-copy on eviction.
- Lock hold time on `Get` shrinks from O(n) to O(1), directly addressing the
  contention concern in the TODO item.
- `BenchmarkPlanCache_Hit` should improve or hold; we add a large-cache hit
  benchmark to make the O(n)→O(1) win measurable.

## Test plan

- All existing `plan_cache_test.go` tests must pass unchanged — they pin the
  LRU semantics (eviction order, promote-on-hit, promote-on-update, stats,
  concurrency, normalization, scalar-sub bindings).
- `BenchmarkPlanCache_HitLargeCache` (maxSize=1024) demonstrates the hit cost is
  independent of cache size — the old code's cost scaled with entry count; the
  new code's does not.
- `TestPlanCache_InterleavedEvictionOrder` pins promote-on-update interacting
  with eviction (the bug a naive rewrite introduces: evicting the just-updated
  key instead of the genuine LRU victim).

### Correctness / race hardening

- `TestPlanCache_DifferentialModel` — a long randomized Get/Put/Invalidate
  sequence (5 cache sizes × 4 seeds × 4000 ops) run against an independent
  reference LRU (`lruOracle`), asserting after **every** operation that the two
  agree on membership and hit/miss verdicts, that a hit returns the plan stored
  under that key, and that the internal list/map invariants hold (`checkInvariants`:
  `ll.Len() == len(items)`, no duplicate keys, map↔element consistency,
  `Len() <= maxSize`). Any recency-tracking divergence from a textbook LRU fails
  immediately.
- `TestPlanCache_RaceSameKey` / `TestPlanCache_RaceInvalidate` — `-race` stress.
  The first reads the values returned by `Get` (plan + bindings) **after** the
  lock is released while other goroutines update those same keys, proving the
  central correctness claim: `Put` swaps in a fresh `*planCacheEntry` rather than
  mutating in place, so a pointer captured under the lock is immutable and
  race-free. The second races `Invalidate` against Get/Put/Stats. Both verified
  clean under `go test -race`.
- Edge cases: `TestPlanCache_MaxSizeOne` (degenerate single-slot),
  `TestPlanCache_EvictionExactBoundary` (no eviction at maxSize, exactly one at
  maxSize+1), `TestPlanCache_NilPlanStored`, `TestPlanCache_UpdateReplacesSubs`.

### Planner-level benchmark (what the cache actually buys)

`BenchmarkPlannerPlanVsCache` isolates the cache's value at the inner layer —
full Cascades planning vs a warm cache hit, **no FDB, no execution**, SQL parsed
once up front (parsing happens on both hit and miss paths in production, so it's
excluded from both). Measured numbers:

| Scenario | Full planning (miss) | Cache hit | Speedup | allocs/op |
|----------|---------------------:|----------:|--------:|----------:|
| point_lookup   |   571 µs |  0.69 µs |  ~820× | 8061 → 3 |
| index_equality |   639 µs |  0.84 µs |  ~760× | 9268 → 3 |
| index_range    |   815 µs |  0.67 µs | ~1200× | 11796 → 3 |
| group_by_agg   |   281 µs |  0.83 µs |  ~340× | 4402 → 3 |
| two_table_join | 1874 µs |  1.34 µs | ~1400× | 28188 → 3 |

A cache hit costs sub-µs to ~1.3 µs (`normalizeSQL` + map lookup + `MoveToBack`)
versus 0.28–1.87 ms for full Cascades planning — a 300×–1400× saving and a
collapse from thousands of allocations to 3. This is precisely the hot path
P2.1 makes O(1): every one of these hits previously also paid the O(n) slice
scan under the lock.
