# RFC 008: Goroutine Safety for FDBRecordStore

## Status: IMPLEMENTED (Phases 1-4)

## Problem

FDB's Go bindings document `Transaction` as **"safe for concurrent use by multiple goroutines."** Users will naturally use goroutines within `db.Run()` callbacks:

```go
db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
    store, _ := NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ss).Open()

    var wg sync.WaitGroup
    for _, record := range batch {
        wg.Add(1)
        go func(r proto.Message) {
            defer wg.Done()
            store.SaveRecord(r) // DATA RACE — crashes or silent corruption
        }(record)
    }
    wg.Wait()
    return nil, nil
})
```

This currently causes data races on every shared mutable field in `FDBRecordContext` and `FDBRecordStore`. Go's race detector catches some (map races → runtime panic), but others silently corrupt data (duplicate version numbers, lost commit hooks, HNSW graph corruption).

## How Java handles this

Java's Record Layer is designed for concurrent async access within a single transaction. `CompletableFuture` pipelines on `ForkJoinPool.commonPool()` can interleave operations on different threads. Java protects shared state at three levels:

### Level 1: FDBRecordContext — atomic/concurrent data structures

| Field | Java type | Mechanism |
|---|---|---|
| `localVersion` | `AtomicInteger` | Lock-free `getAndIncrement()` |
| `localVersionCache` | `ConcurrentSkipListMap` | Fully concurrent sorted map |
| `versionMutationCache` | `ConcurrentSkipListMap` | Fully concurrent sorted map |
| `commitChecks` | `LinkedHashMap` | `synchronized` blocks on all access |
| `postCommits` | `LinkedHashMap` | `synchronized` blocks on all access |
| `dirtyStoreState` | `boolean` | Unprotected (benign — single word) |
| `dirtyMetaDataVersionStamp` | `boolean` | Unprotected (benign — single word) |

### Level 2: FDBRecordStore — reader-writer lock on store state

Java wraps `storeHeader` + `indexStates` in `MutableRecordStoreState` protected by a reader-writer counter (`AtomicLong` with packed read/write counts). All mutations go through `beginRecordStoreStateWrite()` + `synchronized(this)`. Reads go through `beginRecordStoreStateRead()`.

### Level 3: LockRegistry — per-resource async read-write lock

`LockRegistry` is a `ConcurrentHashMap<LockIdentifier, AtomicReference<AsyncLock>>` on `FDBRecordContext`. It provides per-subspace async read-write locks. Any code in the transaction can acquire read or write locks keyed by a `LockIdentifier` (wraps a `Subspace`).

**Users of LockRegistry in Java (within our scope):**

| Component | Lock type | LockIdentifier | Why |
|---|---|---|---|
| `VectorIndexMaintainer.updateIndexKeys()` | WRITE | index partition subspace | Serialize HNSW graph mutations |
| `VectorIndexMaintainer.scan()` | READ | index partition subspace | Prevent writes during kNN scan |
| `MultidimensionalIndexMaintainer.updateIndex()` | WRITE | R-tree partition subspace | Serialize R-tree mutations |
| `MultidimensionalIndexMaintainer.scan()` | READ | R-tree partition subspace | Prevent writes during scan |

Standard VALUE, COUNT, SUM, RANK, VERSION indexes do NOT use `LockRegistry`. They are safe because:
- VALUE/COUNT/SUM use FDB atomic mutations (`tx.Add()`, `tx.Max()`) — commutative, order-independent
- RANK's ranked set does read-then-write on FDB keys, but Java's `updateSecondaryIndexes` pipeline serializes per-record (one record's index updates complete before the next starts)
- VERSION writes to `FDBRecordContext.versionMutationCache` (`ConcurrentSkipListMap`)

## Current Go state — what's broken

### FDBRecordContext (database.go)

| Field | Go type | Goroutine-safe? | Risk | Fix |
|---|---|---|---|---|
| `localVersion` | `int32` | NO | **HIGH** — duplicate versions | `atomic.Int32` |
| `localVersionCache` | `map[string]int` | NO | **HIGH** — runtime panic | `sync.Map` or mutex |
| `versionMutations` | `map[string]versionMutation` | NO | **HIGH** — runtime panic | `sync.Map` or mutex |
| `commitChecks` | `[]CommitCheckFunc` | NO | **MEDIUM** — lost hooks | mutex |
| `postCommits` | `[]PostCommitFunc` | NO | **MEDIUM** — lost callbacks | mutex |
| `dirtyStoreState` | `bool` | NO | LOW — benign | `atomic.Bool` |
| `dirtyMetaDataVersionStamp` | `bool` | LOW | LOW — benign | `atomic.Bool` |
| `tx` | `fdb.Transaction` | YES | — | Already safe |
| `ctx` | `context.Context` | YES | — | Already safe |
| `timer` | `*StoreTimer` | set-once | LOW | — |

### FDBRecordStore (store.go)

| Field | Go type | Goroutine-safe? | Risk | Fix |
|---|---|---|---|---|
| `storeHeader` | `*gen.DataStoreInfo` | NO | **HIGH** — corrupted header | `sync.RWMutex` |
| `indexStates` | `map[string]IndexState` | NO | **HIGH** — runtime panic | `sync.RWMutex` |
| `overrideLock` | `bool` | NO | **HIGH** — security bypass | Pass as parameter (match Java) |
| `context` | `*FDBRecordContext` | ref immutable | — | — |
| `metaData` | `*RecordMetaData` | immutable | — | — |
| `subspace` | `subspace.Subspace` | immutable | — | — |

### Index maintainers

All maintainers are stateless (fresh instance per `getIndexMaintainer()` call, no caching — same as Java). The shared state they mutate is on `FDBRecordContext` (version caches, mutations) which needs the fixes above.

**HNSW** (`vectorIndexMaintainer`) has additional mutable state:
- `storageCache map[string]*hnswStorage` — per-maintainer, but fresh instance each call, so isolated
- `hnswStorage.cache map[string]*parsedNode` — per-storage, also fresh each call

Since `getIndexMaintainer()` creates fresh instances, two goroutines get independent maintainer/storage/cache instances. No shared maintainer state. But HNSW graph mutations are NOT safe for concurrent access — two goroutines inserting into the same HNSW graph interleave read-modify-write FDB operations. This requires the LockRegistry equivalent.

**RANK** (`rankIndexMaintainer`) has similar concerns — ranked set operations do read-then-write on FDB keys. Concurrent RANK updates for different records on the same index could interleave. Java serializes these through the `updateSecondaryIndexes` pipeline, not through LockRegistry.

## Proposed fix

### Phase 1: Make shared state goroutine-safe

**FDBRecordContext:**

```go
type FDBRecordContext struct {
    tx                fdb.Transaction
    ctx               context.Context
    localVersion      atomic.Int32                    // was int32
    localVersionCache sync.Map                        // was map[string]int  (key→local version)
    versionMutations  sync.Map                        // was map[string]versionMutation
    commitMu          sync.Mutex                      // protects commitChecks + postCommits
    commitChecks      []CommitCheckFunc
    postCommits       []PostCommitFunc
    dirtyStoreState            atomic.Bool             // was bool
    dirtyMetaDataVersionStamp  atomic.Bool             // was bool
    locks             lockRegistry                    // NEW: per-subspace RW locks
    timer             *StoreTimer
}
```

**`lockRegistry`** — Go equivalent of Java's `LockRegistry`:

```go
type lockRegistry struct {
    mu    sync.Mutex
    locks map[string]*sync.RWMutex
}

func (r *lockRegistry) WriteLock(key string) {
    r.getOrCreate(key).Lock()
}

func (r *lockRegistry) WriteUnlock(key string) {
    r.getOrCreate(key).Unlock()
}

func (r *lockRegistry) ReadLock(key string) {
    r.getOrCreate(key).RLock()
}

func (r *lockRegistry) ReadUnlock(key string) {
    r.getOrCreate(key).RUnlock()
}

func (r *lockRegistry) getOrCreate(key string) *sync.RWMutex {
    r.mu.Lock()
    defer r.mu.Unlock()
    if r.locks == nil {
        r.locks = make(map[string]*sync.RWMutex)
    }
    if m, ok := r.locks[key]; ok {
        return m
    }
    m := &sync.RWMutex{}
    r.locks[key] = m
    return m
}
```

**FDBRecordStore:**

```go
type FDBRecordStore struct {
    context            *FDBRecordContext
    metaData           *RecordMetaData       // immutable
    subspace           subspace.Subspace      // immutable
    stateMu            sync.RWMutex           // protects storeHeader + indexStates
    storeHeader        *gen.DataStoreInfo
    indexStates        map[string]IndexState
    indexRebuildPolicy IndexRebuildPolicy     // immutable
    storeStateCache    FDBRecordStoreStateCache // immutable ref
    // overrideLock removed — pass as parameter to saveRecordInternal (match Java)
}
```

### Phase 2: Wire LockRegistry into index maintainers

Add lock methods to `indexStoreContext`:

```go
type indexStoreContext interface {
    // ... existing methods ...

    // AcquireWriteLock acquires an exclusive lock for the given subspace key.
    // Used by tree-structured indexes (HNSW, R-tree) to serialize mutations.
    // Matches Java's FDBRecordContext.doWithWriteLock(LockIdentifier).
    AcquireWriteLock(key string)
    ReleaseWriteLock(key string)
    AcquireReadLock(key string)
    ReleaseReadLock(key string)
}
```

`FDBRecordStore` implements by delegating to `context.locks`:

```go
func (store *FDBRecordStore) AcquireWriteLock(key string) {
    store.context.locks.WriteLock(key)
}
// ... etc
```

`vectorIndexMaintainer.Update()` acquires write lock:

```go
func (m *vectorIndexMaintainer) Update(oldRecord, newRecord *FDBStoredRecord[proto.Message]) error {
    lockKey := string(m.hnswSubspace.Bytes())
    m.store.AcquireWriteLock(lockKey)
    defer m.store.ReleaseWriteLock(lockKey)
    // ... existing insert/delete logic
}
```

`vectorIndexMaintainer.SearchKNN()` acquires read lock:

```go
func (m *vectorIndexMaintainer) SearchKNN(...) ([]VectorSearchResult, error) {
    lockKey := string(m.hnswSubspace.Bytes())
    m.store.AcquireReadLock(lockKey)
    defer m.store.ReleaseReadLock(lockKey)
    // ... existing search logic
}
```

### Phase 3: Prove race-freedom

**Go race detector**: run full test suite with `-race`:

```sh
bazelisk test //... --test_arg="-race"
```

This catches all data races at runtime. Any test that exercises concurrent access will fail if we missed a shared field.

**Concurrent stress test**: add a dedicated test that spawns N goroutines doing `SaveRecord` + `SearchVectorIndex` + `LoadRecord` + `DeleteRecord` concurrently within one transaction:

```go
func TestConcurrentSaveWithinTransaction(t *testing.T) {
    db.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
        store, _ := NewStoreBuilder()...Open()

        var wg sync.WaitGroup
        // 10 concurrent writers
        for i := 0; i < 10; i++ {
            wg.Add(1)
            go func(id int) {
                defer wg.Done()
                store.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(id)), ...})
            }(i)
        }
        // 5 concurrent readers
        for i := 0; i < 5; i++ {
            wg.Add(1)
            go func() {
                defer wg.Done()
                store.ScanRecords(...)
            }()
        }
        wg.Wait()
        return nil, nil
    })
}
```

Run with `-race` and `-count=100` to surface intermittent races.

**Static analysis**: use `go vet` (already in nogo) and consider adding `staticcheck` SA2000 (checks for `sync.Mutex` value copies).

## Which indexes need locking

| Index type | Needs LockRegistry? | Why |
|---|---|---|
| VALUE | No | Stateless — `tx.Set()`/`tx.Clear()` on independent keys |
| COUNT, SUM, MIN_EVER, MAX_EVER | No | FDB atomic mutations — commutative, order-independent |
| COUNT_NOT_NULL, COUNT_UPDATES | No | Same — atomic mutations |
| MIN_EVER_TUPLE, MAX_EVER_TUPLE | No | Same — atomic mutations |
| RANK | **Yes** | Ranked set does read-modify-write on skip list — concurrent updates cause lost updates. Confirmed by TestConcurrentSave200. Write lock on secondarySubspace. |
| VERSION | No | Writes to `FDBRecordContext.versionMutations` which will be `sync.Map`. Atomic operations only. |
| MAX_EVER_VERSION | No | Same — writes to version mutation cache |
| TEXT | No | BunchedMap operations on independent key ranges per record |
| BITMAP_VALUE | No | Stateless — atomic OR mutations on bitmap entries |
| PERMUTED_MIN, PERMUTED_MAX | No | Stateless — `tx.Set()`/`tx.Clear()` on independent keys |
| TIME_WINDOW_LEADERBOARD | **Yes** | Uses ranked set (same concern as RANK). Write lock on secondarySubspace. |
| **VECTOR (HNSW)** | **Yes** | Graph mutations are read-modify-write on shared structure |
| **MULTIDIMENSIONAL (R-tree)** | **Yes** | Tree mutations are read-modify-write on shared structure |

Summary: only tree-structured indexes (HNSW, R-tree) need write locks. RANK and TIME_WINDOW_LEADERBOARD might need them if we parallelize index updates in the future, but are safe under current sequential execution.

## Migration / compatibility

- **Wire format**: no change. This is purely in-process synchronization.
- **API**: `SaveRecord` signature unchanged. `overrideLock` parameter change is internal.
- **Performance (single-goroutine)**: `atomic.Int32` and `sync.Map` have negligible overhead vs plain types when uncontended. `sync.RWMutex` in LockRegistry: zero cost when not used (only HNSW/R-tree acquire locks).
- **Performance (concurrent)**: enables a pattern that was previously broken. Any overhead from synchronization is better than data corruption.

## Implementation order

1. `FDBRecordContext` — atomic fields + `sync.Map` + commit mutex (highest risk, fixes crashes)
2. `FDBRecordStore` — `sync.RWMutex` for store state + remove `overrideLock` field
3. `lockRegistry` on `FDBRecordContext` + `indexStoreContext` interface methods
4. `vectorIndexMaintainer` — acquire write/read locks
5. Concurrent stress test with `-race`
6. Run full test suite with `-race` to prove no remaining races

## Resolved questions

1. **`sync.Map` vs mutex-protected map**: Chose mutex-protected maps (`sync.Mutex`). Simpler to reason about for range-delete operations, and version caches are write-heavy during saves. `sync.Map` is optimized for the opposite pattern.

2. **Should we parallelize `updateSecondaryIndexes`?** Not yet. Deferred until measured need. If we do, RANK and TIME_WINDOW_LEADERBOARD would need LockRegistry protection.

3. **`overrideLock` refactor**: Matched Java exactly — `overrideLock` is now a parameter to `saveRecordInternal()`, not a mutable field. `SaveRecordWithOptions` passes `false`, `OverrideLockSaveRecord` passes `true`. Same as Java's `saveTypedRecord(..., boolean overrideLock)`.

## Open questions

1. **R-tree `Scan` read lock**: Java holds a read lock for the entire scan cursor lifetime. Go's lazy cursor model makes this harder — the caller would need to hold the lock while iterating. Currently only `Update` has write locks; scan on snapshot reads gets a consistent FDB view. Track if this causes issues.

## References

- Java `LockRegistry`: `fdb-record-layer-core/.../locking/LockRegistry.java`
- Java `AsyncLock`: `fdb-record-layer-core/.../locking/AsyncLock.java`
- Java `FDBRecordContext` concurrency: `ConcurrentSkipListMap`, `AtomicInteger`, `synchronized` blocks
- Java `MutableRecordStoreState`: `AtomicReference` + reader-writer counter via `AtomicLong`
- FDB Go bindings: `Transaction` is documented goroutine-safe
- Go race detector: `go test -race`
