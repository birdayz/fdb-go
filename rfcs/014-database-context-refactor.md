# RFC 014: DatabaseContext Refactor

**Status**: Proposed  
**Author**: birdy  
**Date**: 2026-04-02  
**Reviews**: 5-persona review (Round 1), 5-FDB-pro cross-examination against C++ source (Round 2)

## Problem

Our Go client splits per-database state across four independent structs:

```
Database           — owns Cluster, GRVBatcher, LocationCache
  Cluster          — owns connPool, dbInfo (topology), coordinators
  GRVBatcher       — owns cachedVersion, pending batches, background refresher
  LocationCache    — owns shard→server entries
```

C++ has a single `DatabaseContext` that owns ALL of this. Our split creates three problems:

1. **Cross-component wiring on every feature.** Commit path needs to update GRV cache (`tx.db.grvBatcher.UpdateCachedReadVersion`). Read path needs to invalidate locations (`tx.db.locationCache.Invalidate`). Reconnect needs to invalidate GRV cache AND clear connection pool. Each new behavior requires threading state through the chain.

2. **No topology monitoring.** C++ runs `monitorClientDBInfoChange` as a background actor on DatabaseContext. When proxy lists change, it fires `proxiesChangeTrigger`, which the GRV batcher reacts to. We have zero topology monitoring — a proxy change means RPCs fail until the user restarts.

3. **Lifecycle leaks.** `GRVBatcher.backgroundRefresher` is lazily started but `Stop()` does a non-blocking channel send — goroutine may outlive `Database.Close()`. No `WaitGroup`, no context cancellation.

4. **Transaction retry bugs.** `Transact()` creates a new `Transaction` each loop iteration. `retryCount` and `backoff` are lost on retry — backoff never escalates. C++ reuses the same transaction object across retries, preserving `numErrors` and growing backoff. Also: backoff constants are 10x too high (100ms vs C++ 10ms), and several retryable error codes are missing.

## Design: Unified database state

### C++ alignment principle

Match C++ `DatabaseContext` field layout and responsibility boundaries where possible. Diverge only when Go idioms or performance require it. Every divergence is called out with rationale.

### Naming decision

C++ calls this `DatabaseContext`. In Go, the `Context` suffix universally means `context.Context`. To avoid confusion:

- **`database`** (unexported) — the real state owner, analogous to C++ `DatabaseContext`
- **`Database`** (exported) — the public handle, analogous to C++ `Database` (= `Reference<DatabaseContext>`)

This matches Go convention: unexported implementation, exported API surface.

### C++ DatabaseContext field map → Go

| C++ field | Go field | Notes |
|---|---|---|
| `connectionRecord` | `clusterFile *ClusterFile` | Immutable after creation |
| `clientInfo` (AsyncVar\<ClientDBInfo\>) | `dbInfo atomic.Pointer[DBInfo]` | Atomic swap on topology change |
| `commitProxies` / `grvProxies` | Inside `DBInfo` | Updated atomically together |
| `proxiesLastChange` | `proxiesGen atomic.Uint64` | Monotonic counter |
| `locationCache` (CoalescedKeyRangeMap) | `locCache locationCache` | Embedded, size-capped |
| `locationCacheSize` | `locCacheMaxSize int` | Default 600,000 (C++ `LOCATION_CACHE_EVICTION_SIZE`) |
| `cachedReadVersion` / `lastGrvTime` / `lastProxyRequestTime` | `grvCache grvCache` | Embedded sub-struct, atomics |
| `versionBatcher` (map\<flags, Batcher\>) | `grvBatcher grvBatcher` | Embedded; single batcher (see Divergence #1) |
| `server_interf` | `connPool` | Connection pool (Go-specific, see Divergence #2) |
| `failedEndpointsOnHealthyServersInfo` | `failedEPs failedEndpoints` | Endpoint failure tracking |
| `minAcceptableReadVersion` | `minReadVersion atomic.Int64` | Cluster-switch safety (see §minReadVersion) |
| `throttledTags` | defer | Not needed for current API |
| `ssVersionVectorCache` | defer | Must clear on proxy change when implemented |
| `watchMap` / `outstandingWatches` | defer | Watch API (Phase 2) |
| `changeFeedCache` | defer | Change feeds |
| Counters (`cc`, ...) | `metrics Metrics` | Counters for observability |
| `connected` (Future\<Void\>) | `connected chan struct{}` | Closed on first successful connect |

### Struct layout

```go
// database is the per-database state container (unexported).
// Matches C++ DatabaseContext in fdbclient/DatabaseContext.h.
// All per-database state lives here: topology, caches, connections, batchers.
//
// Safe for concurrent use by multiple goroutines after creation.
type database struct {
    // Immutable after creation.
    clusterFile *ClusterFile
    dialFn      DialFunc   // nil = net.DialTimeout

    // Topology: atomically swapped on coordinator refresh.
    // C++: clientInfo (AsyncVar<ClientDBInfo>)
    dbInfo     atomic.Pointer[DBInfo]
    proxiesGen atomic.Uint64
    // Kick this channel to trigger an immediate topology refresh.
    // Sent on broken_promise / connection_failed during proxy RPCs.
    topologyKick chan struct{} // buffered(1), non-blocking send

    // Connection pool. C++ uses FlowTransport; we need explicit pool.
    connMu   sync.RWMutex
    connPool map[string]*transport.Conn

    // Location cache. C++: CoalescedKeyRangeMap<Reference<LocationInfo>>.
    // NOT invalidated on proxy change — proxies and storage servers
    // are independent (confirmed: C++ monitorClientDBInfoChange and
    // updateProxies never call invalidateCache).
    // Invalidated on: wrong_shard_server, all_alternatives_failed.
    locCache locationCache

    // GRV cache + batcher. C++: cachedReadVersion, versionBatcher.
    grvCache   grvCache
    grvBatcher grvBatcher

    // Endpoint failure tracking. C++: failedEndpointsOnHealthyServersInfo.
    failedEPs failedEndpoints

    // Safety: reject read versions below this.
    // C++: minAcceptableReadVersion. See §minReadVersion for semantics.
    minReadVersion atomic.Int64

    // Lifecycle.
    ctx       context.Context
    cancel    context.CancelFunc
    closeOnce sync.Once
    connected chan struct{}
    wg        sync.WaitGroup

    // Metrics (optional, zero-value = no-op).
    metrics Metrics
}
```

### Sub-structs (embedded, not allocated)

Sub-structs are embedded directly in `database` — same lifetime, no independent heap allocation, cache-line friendly. They access parent state through method arguments, not stored back-pointers.

```go
// grvCache holds the cached read version state.
// C++: DatabaseContext fields cachedReadVersion, lastGrvTime,
//      lastProxyRequestTime, lastRkBatchThrottleTime, lastRkDefaultThrottleTime.
//
// C++ does NOT explicitly invalidate this cache on proxy change — it relies
// on natural expiry via MAX_VERSION_CACHE_LAG (100ms). We match this behavior.
type grvCache struct {
    version          atomic.Int64 // monotonic (CAS loop, matches C++ guarded store)
    lastTime         atomic.Int64 // UnixNano
    lastProxyContact atomic.Int64 // UnixNano
    lastRkDefault    atomic.Int64 // ratekeeper throttle
    lastRkBatch      atomic.Int64
}

// grvBatcher batches concurrent GetReadVersion calls.
// C++: DatabaseContext::VersionBatcher + readVersionBatcher actor.
//
// Methods receive *database as argument — no stored back-pointer.
type grvBatcher struct {
    mu        sync.Mutex
    pending   []grvRequest
    batchTime time.Duration
    timer     *time.Timer
}

// locationCache maps key ranges to storage server endpoints.
// C++: CoalescedKeyRangeMap<Reference<LocationInfo>>.
//
// Methods receive *database as argument — no stored back-pointer.
// Size-capped to maxSize entries (C++ default: LOCATION_CACHE_EVICTION_SIZE = 600,000).
// Random eviction on overflow, matching C++ setCachedLocation behavior.
type locationCache struct {
    mu      sync.RWMutex
    entries []locationEntry
    maxSize int // default 600_000
}

// failedEndpoints tracks endpoints that failed on healthy servers.
// C++: failedEndpointsOnHealthyServersInfo.
// Keyed by endpoint (addr+token), not just address.
// Set when endpoint fails but server process is healthy (e.g. SS bounced,
// got new endpoints). Grace period before re-resolution.
type failedEndpoints struct {
    mu      sync.RWMutex
    entries map[endpointKey]failureInfo
}

type failureInfo struct {
    startTime   time.Time
    lastRefresh time.Time
}
```

### Transaction

```go
// Transaction is per-transaction state.
// C++: TransactionState + Transaction.
type Transaction struct {
    db    *database // C++: TransactionState::cx
    state txState

    readVersion      int64
    hasReadVersion   bool
    committedVersion int64
    txnBatchId       uint16

    mutations      []Mutation
    readConflicts  []KeyRange
    writeConflicts []KeyRange

    retryCount int
    backoff    time.Duration
}
```

Key change: `db *Database` (public wrapper) → `db *database` (direct state). No indirection chain.

### Database (public API handle)

```go
// Database is the public API entry point.
// C++: Database is a Reference<DatabaseContext> — a handle, not state.
//
// Safe for concurrent use by multiple goroutines.
type Database struct {
    db *database
}

func OpenDatabase(ctx context.Context, clusterFilePath string) (*Database, error)
func (d *Database) Transact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error)
func (d *Database) ReadTransact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error)
func (d *Database) CreateTransaction() *Transaction
func (d *Database) Close() error
```

## Transaction retry loop

### Critical fix: reuse transaction across retries

**Bug in current code**: `Transact()` creates a new `Transaction` each loop iteration. After `OnError` increments `retryCount` and computes `backoff`, the loop creates a fresh `Transaction` — retry state is lost, backoff never escalates.

**C++ behavior**: `onError` calls `reset()` on the same transaction object. `reset()` preserves `numErrors` and `backoff`. Only `fullReset()` clears them. `Database::run()` reuses the same transaction.

**Fix**: Create the transaction once, `OnError` resets it in place (already does), loop reuses it.

```go
func (d *Database) Transact(ctx context.Context, fn func(tx *Transaction) (any, error)) (any, error) {
    tx := d.CreateTransaction()
    for {
        result, err := fn(tx)
        if err != nil {
            if retryErr := tx.OnError(err); retryErr != nil {
                return nil, retryErr // non-retryable
            }
            continue // tx has been reset in place, retryCount/backoff preserved
        }

        if err := tx.Commit(ctx); err != nil {
            if retryErr := tx.OnError(err); retryErr != nil {
                return nil, retryErr
            }
            continue // for commit_unknown_result: self-conflicting applied
        }

        return result, nil
    }
}
```

### OnError: match C++ error code handling

C++ `Transaction::onError` (NativeAPI.actor.cpp:7734) handles two groups:

**Group A** — commit-related (exponential backoff):
| Error code | Const | C++ handles | Go handles |
|---|---|---|---|
| 1020 | `not_committed` | Yes | Yes |
| 1021 | `commit_unknown_result` | Yes (+ self-conflicting) | Yes |
| 1031 | `database_locked` | Yes | **Add** |
| 1037 | `proxy_memory_limit_exceeded` | Yes | **Add** |
| 1038 | `grv_proxy_memory_limit_exceeded` | Yes | **Add** |
| 1039 | `process_behind` | Yes | **Add** |
| 1040 | `batch_transaction_throttled` | Yes | **Add** |
| 1041 | `tag_throttled` | Yes | defer (no tag throttle support) |

**Group B** — version-related (fixed delay, no growth):
| Error code | Const | C++ handles | Go handles |
|---|---|---|---|
| 1007 | `transaction_too_old` | Yes (fixed delay) | Yes (but uses growing backoff — **fix**) |
| 1009 | `future_version` | Yes (fixed 10ms delay) | **Add** |

C++ also resets backoff to zero when `onProxiesChanged()` fires (proxy failover). We add a `resetBackoff()` call in the topology kick path so the next retry doesn't waste time backing off against a dead proxy.

### Backoff formula: match C++ CLIENT_KNOBS

C++ `getBackoff` (NativeAPI.actor.cpp:6088):
```
DEFAULT_BACKOFF        = 0.01s  (10ms)
BACKOFF_GROWTH_RATE    = 2.0
DEFAULT_MAX_BACKOFF    = 1.0s
Formula: return backoff * random[0,1), then backoff = min(backoff * 2, maxBackoff)
```

Current Go: base=100ms, max=5s — **10x and 5x too high**.

Fixed Go:
```go
const (
    defaultBackoff    = 10 * time.Millisecond  // C++: DEFAULT_BACKOFF
    backoffGrowthRate = 2.0                     // C++: BACKOFF_GROWTH_RATE
    maxBackoff        = 1 * time.Second         // C++: DEFAULT_MAX_BACKOFF
    futureVersionDelay = 10 * time.Millisecond  // C++: FUTURE_VERSION_RETRY_DELAY
)

func (tx *Transaction) nextBackoff() time.Duration {
    // C++ pattern: return current * jitter, then grow for next time.
    delay := time.Duration(float64(tx.backoff) * rand.Float64())
    tx.backoff = min(time.Duration(float64(tx.backoff)*backoffGrowthRate), maxBackoff)
    return delay
}
```

For `transaction_too_old` and `future_version`: fixed delay `min(10ms, maxBackoff)`, no backoff growth. Matches C++ `FUTURE_VERSION_RETRY_DELAY`.

### Self-conflicting: current approach is correct but different from C++

C++ `makeSelfConflicting()` (NativeAPI.actor.cpp:5952) adds a single random UUID key to both read AND write conflict ranges **pre-commit**. Go copies ALL write conflicts into read conflicts **post-error** in `OnError`.

Go's approach is semantically correct for retry-will-conflict: if the original committed, the retry conflicts against it. It's more conservative (larger conflict surface) but not incorrect. We keep our approach.

### GRV cache update after commit

After successful commit, `committedVersion` feeds the GRV cache. C++ does this in `tryCommit` reply handler (NativeAPI.actor.cpp:6657).

```go
// In Transaction.Commit(), after successful commit reply:
if tx.committedVersion > 0 {
    tx.db.grvCache.update(time.Now(), tx.committedVersion)
    // Monotonic: grvCache.version only increases (CAS loop).
}
```

## Background goroutines

### 1. Topology monitor

**C++**: `monitorClientDBInfoChange` — watches `clientInfo->onChange()` (push-based).

**Go**: Single goroutine. Polls coordinator periodically AND on-demand when kicked by RPC failures.

```go
func (db *database) topologyMonitor() {
    defer db.wg.Done()
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            db.refreshTopology()
        case <-db.topologyKick:
            db.refreshTopology()
        case <-db.ctx.Done():
            return
        }
    }
}

func (db *database) kickTopology() {
    select {
    case db.topologyKick <- struct{}{}:
    default:
    }
}
```

On successful refresh with changed proxies:
1. Bump `proxiesGen` (BEFORE swapping `dbInfo` — ordering invariant for concurrent reads)
2. Atomic-swap `dbInfo`
3. Do NOT invalidate GRV cache — C++ relies on natural expiry via `MAX_VERSION_CACHE_LAG` (100ms). Explicit invalidation would cause an unnecessary cache-miss storm on every proxy rotation.
4. Do NOT invalidate location cache — storage servers are independent of proxies. Confirmed: C++ `monitorClientDBInfoChange` and `updateProxies` never call `invalidateCache`.

```go
func (db *database) refreshTopology() {
    newInfo, err := db.fetchClientDBInfo()
    if err != nil {
        return // retry on next tick or kick
    }
    old := db.dbInfo.Load()
    if old != nil && old.equal(newInfo) {
        return // no change
    }
    // ORDER MATTERS: bump generation BEFORE swapping dbInfo.
    db.proxiesGen.Add(1)
    db.dbInfo.Store(newInfo)
    // Neither GRV cache nor location cache invalidated here.
    // GRV cache expires naturally (100ms). Location cache maps to
    // storage servers, not proxies.
}
```

**Note on C++ `proxiesChangeTrigger`**: C++ fires this trigger on proxy change, which wakes all operations blocked in `choose { when(onProxiesChanged()) ... }`. Those operations reset their backoff to zero and immediately retry with new proxies. Our Go equivalent: the kick channel unblocks the topology monitor, and the next RPC to a new proxy succeeds. However, we must also reset transaction backoff when proxies change — see §Backoff formula.

### 2. GRV background refresher

**C++**: `backgroundGrvUpdater` — lazily spawned on first cache hit. Uses adaptive delay: computes `min(MAX_PROXY_CONTACT_LAG - elapsed, MAX_VERSION_CACHE_LAG - grvDelay - elapsed)`.

**Go**: Same lazy spawn via `sync.Once`. Uses fixed tick at `maxVersionCacheLag/2` (50ms). This is coarser than C++ adaptive delay — over-refreshes when cache is fresh, but never under-refreshes by more than 50ms. Uses `db.ctx` for cancellation. Registered with `db.wg` for clean shutdown.

**Divergence**: Fixed tick vs adaptive delay. Acceptable: the efficiency cost is one extra GRV RPC per 50ms when cache is very fresh. Not a correctness issue. Can optimize later.

### 3. Connection monitor (outbound PING)

**C++**: `connectionMonitor` in FlowTransport — sends PING at `CONNECTION_MONITOR_LOOP_TIME` intervals, declares `connection_failed` after `CONNECTION_MONITOR_TIMEOUT` (~2s) with no response.

**Go current**: Responds to inbound PINGs from server, but does NOT send outbound PINGs. Dead connection detection only on next RPC timeout (~5s) or TCP keepalive (~2min).

**Phase 1 fix**: Add outbound PING goroutine per connection. Send PING every 1s, mark connection dead if no response within 2s. Matches C++ `CONNECTION_MONITOR_TIMEOUT`. Dead connections evicted from pool proactively.

## Connection failure handling

### Dead connection eviction

When an RPC gets `io.EOF`, `broken_promise`, or `connection_failed`:
1. Evict the dead connection from `connPool`
2. If error occurred during a proxy RPC, call `db.kickTopology()`
3. Return retryable error to caller

```go
func (db *database) handleConnError(addr string, err error) {
    db.connMu.Lock()
    if c, ok := db.connPool[addr]; ok {
        c.Close()
        delete(db.connPool, addr)
    }
    db.connMu.Unlock()
}
```

Topology kick is the caller's responsibility (read path knows if it was talking to a proxy or storage server). `handleConnError` only handles pool eviction.

### Read path: replica failover with backoff

C++ `loadBalance` (LoadBalance.actor.h) is a sophisticated actor: skips known-failed endpoints, tracks queue depth, races a second request if first is slow. We implement a simpler but correct version:

```go
func (tx *Transaction) getValue(ctx context.Context, key []byte) ([]byte, error) {
    for attempt := 0; attempt < MaxWrongShardRetries; attempt++ {
        servers, err := tx.db.locCache.locate(tx.db, ctx, key)
        if err != nil { return nil, err }

        for _, srv := range servers {
            val, err := tx.sendGetValue(ctx, srv, key)
            if err == nil {
                return val, nil
            }
            if isWrongShardServer(err) {
                tx.db.locCache.invalidate(key)
                break // re-locate
            }
            if isAllAlternativesFailed(err) || isConnectionError(err) {
                tx.db.locCache.invalidate(key)
                tx.db.handleConnError(srv.Address, err)
                break // re-locate
            }
            // Try next replica for transient errors.
            continue
        }
    }
    return nil, fmt.Errorf("all servers unreachable for key after %d attempts", MaxWrongShardRetries)
}
```

**Phase 2 improvement**: Add `allAlternativesFailedDelay` — when ALL replicas of a shard fail, wait with adaptive backoff (C++ `ALTERNATIVES_FAILURE_MIN_DELAY` to `ALTERNATIVES_FAILURE_MAX_DELAY`) for at least one replica to recover, rather than failing the transaction immediately. Critical for graceful rolling restarts.

## Location cache

### Invalidation triggers (C++ cross-reference)

| Trigger | C++ does? | Go does? |
|---|---|---|
| `wrong_shard_server` (1062) | Yes — per-key | Yes |
| `all_alternatives_failed` (1000) | Yes — per-key | Yes |
| `connection_failed` on storage server | Yes (in checkpoint/changefeed paths) | Phase 2 |
| Proxy change | **No** | No (correct) |
| Endpoint-failed-on-healthy-server (proactive) | Yes (`checkOnlyEndpointFailed` at lookup time) | Phase 2 |
| `MACHINE_ID`/`DATACENTER_ID` option change | Yes (full clear) | N/A (no options support) |
| Cluster connection switch | Yes (full clear) | N/A (no switchable) |

### Size cap and eviction

C++ caps at `LOCATION_CACHE_EVICTION_SIZE` = 600,000 entries. On overflow, random eviction in `setCachedLocation`. Our `locationCache` must match:

```go
func (lc *locationCache) add(entry locationEntry) {
    lc.mu.Lock()
    defer lc.mu.Unlock()
    if len(lc.entries) >= lc.maxSize {
        // Random eviction matching C++ behavior.
        idx := rand.Intn(len(lc.entries))
        lc.entries[idx] = lc.entries[len(lc.entries)-1]
        lc.entries = lc.entries[:len(lc.entries)-1]
    }
    lc.entries = append(lc.entries, entry)
}
```

### Reverse lookup (Phase 2)

C++ `getCachedLocation(tenant, key, Reverse::True)` uses `rangeContainingKeyBefore(key)` for backward scans. Our current `Locate()` is forward-only. Reverse scans may hit wrong cache entry on shard boundary. Add `Reverse` parameter when reverse range reads are needed.

## minReadVersion semantics

**RFC Round 1 had this wrong.** Corrected per C++ cross-examination:

C++ `minAcceptableReadVersion` is a **cluster-switch safety guard**, not a post-commit monotonicity guard. It's initialized to `MAX_INT64` and updated to `min(current, grv_response_version)` on every GRV response (NativeAPI.actor.cpp:7287, 3871). It prevents a client that switches cluster files from using `SetReadVersion(old_version)` against the wrong cluster.

C++ does **NOT** update `minAcceptableReadVersion` after commit. The RFC's earlier claim of "max(committed+1, current)" was incorrect.

For a non-switchable client (which ours is), this is low risk. Phase 2: add the validation on `SetReadVersion` with correct semantics matching C++.

## GRV cache: no explicit invalidation on proxy change

**RFC Round 1 called for explicit `grvCache.invalidate()` on proxy change. Round 2 cross-examination found this is wrong.**

C++ `monitorClientDBInfoChange` fires `proxiesChangeTrigger` but **never** touches `cachedReadVersion`. C++ `updateProxies` clears `ssVersionVectorCache` but not the GRV cache. The GRV cache naturally expires via `MAX_VERSION_CACHE_LAG` (100ms).

Explicit invalidation would cause a brief cache-miss storm on every proxy rotation — unnecessary because:
1. The cached version is still valid (it came from the cluster, not a specific proxy)
2. The next background refresh (≤50ms away) will contact the new proxy
3. 100ms natural expiry is short enough that stale versions are transient

We match C++: no explicit GRV cache invalidation on proxy change.

## Divergences from C++ (justified)

### Divergence #1: Single GRV batcher, not per-flags

C++ has `map<uint32_t, VersionBatcher>` — one batcher per `{priority, causallyReadRisky, lockAware}` flag combination.

We use a single batcher. When we add priority or causal consistency, key on the full flags word (`uint32`). Premature complexity for zero current benefit.

**Risk**: Adding causal consistency without per-flags batching → stale version for causal read. Mitigation: add per-flags batching in the same PR.

### Divergence #2: Explicit connection pool

C++ uses `FlowTransport` (process-global). Go needs explicit `map[string]*transport.Conn` — no process-global transport, explicit `net.Conn` lifecycle required.

### Divergence #3: Polling + kick topology, not push

C++ push-based via `AsyncVar::onChange()`. Go: 5s polling + error-driven kick.

**Key behavioral difference**: C++ in-flight proxy RPCs race against `onProxiesChanged()`. When proxies change mid-commit, C++ cancels the reply and throws `request_maybe_delivered` (becomes `commit_unknown_result`). Go has no equivalent race construct — in-flight RPCs to dead proxies wait for timeout, then fail normally. The kick channel ensures the *next* attempt uses fresh proxies.

**Blast radius**: first RPC to dead proxy fails (timeout or connection error) → topology kick → refresh → next RPC succeeds. Typical: 1 failed transaction + ~100ms recovery.

### Divergence #4: No QueueModel / locality-based load balancing

Round-robin proxy selection. Single-DC: no impact. Multi-DC: ~33% cross-DC reads for storage servers. Commit proxies (3-5 total) unaffected.

### Divergence #5: Embedded sub-structs vs heap objects

C++ uses `Reference<T>` (heap, refcounted). Go embeds sub-structs (same lifetime, cache-friendly, no GC pressure). Methods receive `*database` as argument — no back-pointers, no circular refs, no two-phase init.

### Divergence #6: GRV background refresher fixed tick vs adaptive delay

C++ computes precise sleep duration based on `grvDelay` (running average of GRV latency). Go uses fixed 50ms tick. Over-refreshes when cache is fresh, but never under-refreshes by >50ms. Efficiency cost: ~1 extra GRV RPC per 50ms when cache is very fresh. Not correctness.

### Divergence #7: GRV batcher upper bound 10ms vs C++ 5ms

C++ `GRV_BATCH_TIMEOUT` = 5ms. Go clamps to 10ms. Minor latency difference. Fix to 5ms.

## Migration plan

### Phase 1: Structural merge + correctness fixes (this RFC)

1. Create `database` struct absorbing `Cluster` + `Database` + `GRVBatcher` + `LocationCache`
2. Embed `grvCache`, `grvBatcher`, `locationCache` as sub-structs (no back-pointers)
3. `Database` becomes thin public handle holding `*database`
4. `Transaction.db` → `*database` (direct)
5. **Fix `Transact()`: reuse transaction across retries** (preserves `retryCount`, `backoff`)
6. **Fix backoff constants**: base=10ms, max=1s (match C++ `CLIENT_KNOBS`)
7. **Fix backoff for `transaction_too_old`**: fixed delay, no growth (match C++)
8. **Add retryable error codes**: `future_version`, `process_behind`, `proxy_memory_limit_exceeded`, `grv_proxy_memory_limit_exceeded`, `database_locked`, `batch_transaction_throttled`
9. `OpenDatabase(ctx, path)` — caller controls bootstrap timeout
10. `Close()` idempotent via `sync.Once`
11. Topology monitor goroutine with kick channel
12. Dead connection eviction from pool on error
13. `context.Context` + `sync.WaitGroup` lifecycle for all goroutines
14. Location cache size cap (600K, random eviction)
15. **No explicit GRV cache invalidation on proxy change** (match C++ natural expiry)
16. Delete `Cluster` struct

### Phase 2: Availability hardening

- `allAlternativesFailedDelay` — wait for replica recovery instead of failing transaction
- Outbound PING connection monitor (detect dead connections in ~2s)
- `failedEndpoints` tracking with grace period and re-resolution rate limiting
- `minReadVersion` validation on `SetReadVersion` (cluster-switch guard)
- Reverse lookup parameter on location cache
- `connection_failed` / `broken_promise` → location cache invalidation for storage servers
- Transaction options (`SetTimeout`, `SetRetryLimit`)
- `transactionIdAllocator` for trace correlation
- `Metrics` struct with counters

### Phase 3: Future features (separate RFCs)

- Per-flags GRV batchers (causal consistency, priority)
- Push-based topology monitoring (streaming coordinator protocol)
- QueueModel + locality load balancing
- `allAlternativesFailed` quorum-wait (wait for 1 replica to recover)
- Per-proxy tracking, speculative second request
- Watch API, tag throttling, tenant support

## Files affected

| File | Change |
|---|---|
| `client/database.go` | Rewrite — `database` (unexported) + `Database` (public handle) + `Transact` fix |
| `client/cluster.go` | **Delete** — absorbed into `database` |
| `client/grv.go` | Rewrite — `grvCache` + `grvBatcher`, methods take `*database`, fix batch upper bound |
| `client/locality.go` | Rewrite — `locationCache` with size cap + random eviction |
| `client/transaction.go` | Update — fix backoff (10ms/1s), add error codes, fix `transaction_too_old` delay |
| `client/readpath.go` | Update — `tx.db` direct, replica failover with conn eviction |
| `client/commitpath.go` | Update — `grvCache.update()` after commit |
| `client/coordinator.go` | Update — method on `database` |
| `client/topology.go` | **New** — topology monitor + kick channel |
| `client/*_test.go` | Update — new construction, verify backoff escalation |

## Naming convention

| C++ | Go | Rationale |
|---|---|---|
| `DatabaseContext` | `database` (unexported) | "Context" suffix = `context.Context` in Go |
| `Database` (= `Reference<DatabaseContext>`) | `Database` (exported) | Public handle |
| `TransactionState` | fields on `Transaction` | No separate refcounted state needed |
| `cachedReadVersion` | `grvCache.version` | Grouped with related fields |
| `locationCache` | `locCache` | Avoid stutter |
| `versionBatcher` | `grvBatcher` | More descriptive |
| `monitorClientDBInfoChange` | `topologyMonitor` | Go naming |
| `proxiesLastChange` | `proxiesGen` | Short, clear |
| `DEFAULT_BACKOFF` | `defaultBackoff` | Go const naming |
| `LOCATION_CACHE_EVICTION_SIZE` | `locCacheMaxSize` | Go naming |
| `GRV_BATCH_TIMEOUT` | `grvBatchTimeout` | Go naming |

## Success criteria

1. All existing tests pass with zero behavior change
2. `Database` public API unchanged (plus `ctx` on `OpenDatabase`)
3. No `Cluster` type in API
4. Clean shutdown: `Close()` idempotent, returns after all goroutines exit
5. Topology changes detected: 5s (polling) or sub-second (kick on error)
6. GRV cache: natural expiry only, no explicit invalidation on proxy change (matches C++)
7. Location cache: not invalidated on proxy change, size-capped at 600K
8. Dead connections evicted from pool on error
9. **`Transact` reuses transaction — backoff escalates across retries**
10. **Backoff: 10ms base, 1s max, 2x growth (matches C++ `CLIENT_KNOBS`)**
11. **All C++ retryable error codes handled** (`future_version`, `process_behind`, etc.)
12. Thread safety documented on `Database` and `database`

## Review findings incorporated

### Round 1: 5-persona review

| # | Finding | Source | Resolution |
|---|---|---|---|
| 1 | Don't invalidate location cache on proxy change | FDB author | Fixed |
| 2 | Error-driven topology refresh | FDB author, prod user | Added: `topologyKick` |
| 3 | `proxiesGen` before `dbInfo` swap | FDB author | Fixed ordering |
| 4 | `OpenDatabase` needs `context.Context` | Rob Pike, prod user | Fixed |
| 5 | Rename `DatabaseContext` | Torvalds | `database` (unexported) |
| 6 | No back-pointers | Torvalds | Methods take `*database` arg |
| 7 | `Close()` idempotent | Prod user | `sync.Once` |
| 8 | Specify retry loop | Record Layer | Specified with `OnError` semantics |
| 9 | Document thread safety | Rob Pike | Godoc added |
| 10 | Dead connection eviction | Prod user | `handleConnError` |

### Round 2: 5-FDB-pro cross-examination against C++ source

| # | Finding | Source | Resolution |
|---|---|---|---|
| C1 | **Transact creates new tx — backoff never escalates** | Tx lifecycle | **Fixed**: reuse tx across retries |
| C2 | **Backoff 10x too high (100ms vs 10ms), max 5x too high** | Tx lifecycle | **Fixed**: 10ms base, 1s max |
| C3 | **Missing: `future_version`, `process_behind`, 5 more error codes** | Tx lifecycle | **Fixed**: all added |
| H1 | Dead connections never evicted | Connections | **Fixed**: `handleConnError` |
| H2 | All-alternatives-failed → immediate failure | Connections | Phase 2: `allAlternativesFailedDelay` |
| H3 | No outbound PING — 5s detection vs C++ 2s | Connections | Phase 2: connection monitor |
| H4 | No backoff reset on proxy change | Topology | Noted; backoff reset on topology change |
| M1 | **GRV cache: RFC invalidated on proxy change, C++ doesn't** | GRV | **Fixed**: removed, natural expiry only |
| M2 | **`minReadVersion` semantics wrong** | GRV | **Fixed**: corrected to cluster-switch guard |
| M3 | Location cache unbounded | Location | **Fixed**: 600K cap + random eviction |
| M4 | No reverse lookup on location cache | Location | Phase 2 |
| M5 | Background refresher fixed tick vs adaptive | GRV | Documented as Divergence #6 |
| M6 | Batch upper bound 10ms vs C++ 5ms | GRV | **Fixed**: noted, change to 5ms |
| — | C++ `onProxiesChanged` resets backoff to zero | Topology | Noted; add backoff reset on proxy change |
| — | `ssVersionVectorCache` must clear on proxy change | Topology | Noted; deferred field, will clear when added |
| — | `isProxyAddr` check fragile | Topology | Fixed: caller kicks, not `handleConnError` |
| — | `proxiesGen` ordering rationale differs from C++ | Topology | Corrected: Go-specific, not C++ match |
| — | Self-conflicting differs from C++ (broader, post-error) | Tx lifecycle | Documented; semantically correct |
| — | Location cache O(n) linear scan | Location | Phase 2: interval tree or sorted + binary search |
