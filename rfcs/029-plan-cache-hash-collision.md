# RFC 029 — Plan Cache Hash Collision Fix

**Status:** Implemented
**Scope:** Fix P0.2 — plan cache keyed on uint64 hash serves wrong plans on collision.

## Problem

`PlanCache.Get(sqlHash uint64)` uses FNV-64a of the normalized SQL text as the sole map key. The stored `sql` field is never compared on lookup. Two distinct SQL strings that collide to the same uint64 hash → cache returns the wrong physical plan → wrong query results (silent data corruption).

FNV-64a has 64 bits of output. By the birthday paradox, the collision probability reaches 50% at ~5 billion distinct queries. For a long-running service caching thousands of distinct query shapes over weeks, this is a real production risk — not theoretical. The fix is trivial and eliminates the class of bug entirely.

### Scalar subquery staleness (non-issue)

TODO.md P0.2 also flags "cached `scalarSubs` pre-evaluated subquery results go stale when parameters change." Investigation shows this is not a current bug:

1. `scalarSubqueryBinding` stores a physical **plan**, not a result.
2. `fetchPage()` re-evaluates each scalar subquery plan via `executor.EvaluateScalarSubquery()` on every page fetch.
3. Different SQL text (different literal values) → different normalized SQL → different cache key → no stale plan.
4. Same SQL text → same plan structure → same scalar sub plan structure → correct reuse.

The scalar sub architecture is already correct. The only bug is the hash collision on lookup.

## Fix

**Key the cache map on the normalized SQL string, not the uint64 hash.**

### API change

```go
// Before
Get(sqlHash uint64) (plans.RecordQueryPlan, []scalarSubqueryBinding, bool)
Put(sqlHash uint64, sql string, plan plans.RecordQueryPlan, subs []scalarSubqueryBinding)

// After
Get(sql string) (plans.RecordQueryPlan, []scalarSubqueryBinding, bool)
Put(sql string, plan plans.RecordQueryPlan, subs []scalarSubqueryBinding)
```

Both methods normalize the SQL internally via `normalizeSQL()`. Callers pass raw SQL; the cache owns normalization. This eliminates the `QueryHash()` function from the cache hot path (it becomes dead code for cache purposes, retained for other uses if any).

### Internal representation

```go
type PlanCache struct {
    mu      sync.RWMutex
    entries map[string]*planCacheEntry // key: normalized SQL
    order   []string                   // LRU order: most recently used at end
    maxSize int
    hits    atomic.Int64
    misses  atomic.Int64
}

type planCacheEntry struct {
    plan       plans.RecordQueryPlan
    scalarSubs []scalarSubqueryBinding
}
```

### Performance

Go's `map[string]` hashes internally using a fast runtime hash (aeshash on amd64). The total cost is `normalizeSQL` + `map[string]` lookup, equivalent to the current `normalizeSQL` + `FNV-64a` + `map[uint64]` lookup. For 256 entries the difference is unmeasurable.

The LRU `promote()` remains O(n) with a linear scan — identical to current behavior. P2.1 (LRU O(1) via `container/list`) is a separate item.

### Caller changes

`cascades_generator.go:planSelectCascades` — replace `QueryHash(sqlText)` + `Get(sqlHash)` / `Put(sqlHash, sqlText, ...)` with direct `Get(sqlText)` / `Put(sqlText, ...)`.

### Test changes

- Update unit tests to use `Get(sql)` / `Put(sql, ...)` API.
- Add a new test: two SQL strings that produce the same FNV-64a hash must return their own plans (collision resistance).
- Existing `TestFDB_PlanCacheCorrectness` integration test: update `QueryHash` assertions to use normalized string comparison instead.

## What about `QueryHash`?

`QueryHash()` was deleted — no production or test callers remain after the cache key change. `normalizeSQL()` remains in `query_hash.go` and is called internally by the cache. Tests were rewritten to exercise `normalizeSQL` directly.

## Non-goals

- P2.1 (LRU O(1)) — separate item, out of scope.
- Multi-tier cache (Java's 3-stage Caffeine) — out of scope per RFC-024.
- Schema-version-aware keys — existing `Invalidate()` on DDL is sufficient for now.
