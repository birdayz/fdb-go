# RFC 009: HNSW Parallel Prefetch

## Status: Rejected (see Findings)

## Problem

HNSW beam search at layer 0 is bottlenecked by sequential FDB round-trips. Each iteration of the main loop does:

```
1. Pop closest candidate from min-heap
2. loadNodeLayerDispatch(tx, 0, candidate.pk)     → 1 FDB Get (blocking)
3. loadNodeLayerBatchDispatch(tx, 0, unvisited)    → 1 pipelined batch (blocking)
4. Compute distances, update heap/results
5. goto 1
```

Steps 2–3 block on I/O before step 4 can run. With efSearch=200, this produces **38–58 sequential round-trip batches**, each ~0.3–0.5ms → **~15–25ms of pure I/O wait** in a 30ms search. The CPU work (distance computation) is <1% of wall-clock time.

Java's `Search.beamSearchLayer()` uses `CompletableFuture` chains with `AsyncUtil.whileTrue()`. Each iteration fetches the candidate node, gets unvisited neighbors, then calls `fetchNeighborhoodReferences` (bounded by `maxNumConcurrentNodeFetches`, default 16) to batch-fetch neighbor vectors. **This is still a sequential loop** — `whileTrue` waits for each iteration to complete before starting the next. Java does NOT overlap I/O across iterations.

Go's FDB bindings are synchronous. `tx.Get(key).Get()` blocks until the result arrives. There is no `Future.thenCompose()` equivalent. The current Go code is strictly serial: pop → fetch → compute → repeat.

## Goal

Overlap FDB I/O with computation by prefetching the next N candidates' data while processing the current one. Target: **2–3x search QPS improvement** (34 → 70–100 QPS) on the 1K×128D benchmark.

## Constraints

1. **Single FDB transaction** — all reads must go through the same `fdb.ReadTransaction`. FDB Go bindings are thread-safe for reads: multiple goroutines can call `tx.Get()` concurrently on the same transaction.
2. **No behavior change** — same recall, same results (modulo float rounding), same wire format.
3. **Deterministic** — given the same graph state, produce the same results regardless of goroutine scheduling. This is achievable because we only parallelize I/O, not the algorithm's decision logic.
4. **Bounded goroutines** — don't spawn efSearch goroutines. Use a fixed-size prefetch window.

## Design

### Core idea: Prefetch window

Instead of processing one candidate at a time, maintain a **prefetch window** of the next W candidates. While processing candidate[i], fire `tx.Get()` for candidates[i+1..i+W]'s node data in goroutines. By the time we need candidate[i+1], its data is likely already cached.

### Architecture

```
                     ┌─────────────────────────────────────────────┐
                     │            searchLayerMulti                  │
                     │                                             │
                     │  candidates: min-heap (by distance)         │
                     │  results:    sorted slice (ef capacity)     │
                     │  visited:    map[string]bool                │
                     │                                             │
                     │  prefetcher: hnswPrefetcher                 │
                     │    ├── window: [W]prefetchSlot              │
                     │    ├── tx: fdb.ReadTransaction              │
                     │    └── storage: *hnswStorage                │
                     │                                             │
                     │  Loop:                                      │
                     │    1. Pop candidate from heap               │
                     │    2. prefetcher.Get(candidate)             │
                     │       → returns node (likely cache hit)     │
                     │    3. Collect unvisited neighbors            │
                     │    4. prefetcher.PrefetchBatch(neighbors)   │
                     │       → fires tx.Get() without blocking     │
                     │    5. prefetcher.ResolveBatch(neighbors)    │
                     │       → blocks until all futures ready      │
                     │    6. Compute distances, update heap        │
                     │    7. prefetcher.PrefetchNodes(top W heap)  │
                     │       → speculative: load next candidates   │
                     └─────────────────────────────────────────────┘
```

### hnswPrefetcher

```go
type hnswPrefetcher struct {
    tx      fdb.ReadTransaction
    storage *hnswStorage
    layer   int

    // In-flight futures: pk-key → FutureByteSlice.
    // Populated by Prefetch*, consumed by Get/ResolveBatch.
    // Futures that resolve get moved into storage.cache.
    inflight map[string]fdb.FutureByteSlice
}
```

Key operations:

**`Prefetch(pk tuple.Tuple)`** — If pk is not in cache and not already in-flight, fire `tx.Get(key)` and store the `FutureByteSlice` in `inflight`. Non-blocking. Called speculatively for upcoming heap candidates.

**`PrefetchBatch(pks []tuple.Tuple)`** — Same as Prefetch, but for a slice. Fires all Gets, stores all futures. Non-blocking.

**`Get(pk tuple.Tuple) (vecBytes, neighbors, error)`** — Check cache → check inflight (resolve future, move to cache) → cold miss (blocking `tx.Get()`). This is the only blocking call.

**`ResolveBatch(pks []tuple.Tuple) []nodeResult`** — For each pk: check cache, else resolve inflight future (or cold-fetch). Returns all results. Equivalent to current `loadNodeLayerBatch` but leverages prefetched futures.

### Why NOT goroutines

The initial TODO.md item suggests goroutines. I think that's wrong. Here's why:

FDB's Go bindings already return `FutureByteSlice` from `tx.Get()` — the network thread starts the read immediately. You don't need goroutines to achieve I/O parallelism. You just need to **call `tx.Get()` early** (before you need the result) and **call `.Get()` late** (when you actually need it). This is exactly how `loadNodeLayerBatch` already works within a single batch.

The insight is to extend this pattern **across beam search iterations**: fire Gets for nodes you'll need in future iterations, not just the current one.

Goroutines add complexity (synchronization, panic recovery, ordering guarantees) for zero additional I/O parallelism. The FDB client network thread handles all the parallelism we need.

### Modified searchLayerMulti

```go
func (g *hnswGraph) searchLayerMulti(tx fdb.ReadTransaction, query []float64,
    epPK tuple.Tuple, epVecBytes []byte, ef, layer int) ([]hnswCandidate, error) {

    // ... (init candidates heap, visited set, results) ...

    pf := newHnswPrefetcher(tx, g.storage, layer)

    for candidates.Len() > 0 {
        closest := heap.Pop(candidates).(distItem)
        if len(results) >= ef && closest.dist > results[len(results)-1].dist {
            break
        }

        // Step 1: Get current node — likely a prefetch cache hit.
        _, neighbors, err := pf.Get(closest.pk)
        if err != nil {
            continue
        }

        // Step 2: Collect unvisited neighbors.
        toFetch := toFetch[:0]
        for _, nbPK := range neighbors {
            key := string(nbPK.Pack())
            if visited[key] { continue }
            visited[key] = true
            toFetch = append(toFetch, nbPK)
        }

        // Step 3: Fire Gets for all unvisited neighbors (non-blocking).
        pf.PrefetchBatch(toFetch)

        // Step 4: Resolve all neighbor futures (blocks until all ready).
        batchResults := pf.ResolveBatch(toFetch)

        // Step 5: Compute distances, update heap/results.
        for _, r := range batchResults {
            if r.err != nil { continue }
            dist := g.computeDistance(query, r.vecBytes)
            if len(results) < ef || dist < results[len(results)-1].dist {
                heap.Push(candidates, distItem{pk: r.pk, dist: dist, pkBytes: r.pkBytes})
                // ... binary-insert into results ...
            }
        }

        // Step 6: Speculative prefetch — peek top W candidates from heap.
        // These are the nodes we'll process next, so start fetching now.
        pf.PrefetchTopCandidates(candidates, W)
    }

    return results, nil
}
```

### Prefetch window size (W)

Java defaults: `maxNumConcurrentNodeFetches=16`. But that's for all in-flight fetches across the entire `forEach` call (node fetch + neighbor fetch combined).

For Go, the prefetch window for **speculative node loads** should be smaller because:
- Each speculative prefetch also triggers a neighbor batch fetch → amplification
- Wasted reads cost FDB storage server CPU
- Diminishing returns: by W=4, most next-iteration data is warm

**Proposed default: W=4** (configurable via `HNSWConfig.PrefetchWindow`).

### What about the neighbor batch fetch?

The current code fires all neighbor Gets at once via `loadNodeLayerBatch` → resolves them in sequence. This is already pipelined within a single iteration. No change needed.

The optimization is in the **cross-iteration** gap: between finishing iteration N's distance computation and starting iteration N+1's node load. With prefetch, node N+1's data is already in-flight (or resolved) by the time we need it.

### What about searchLayerGreedy?

Same pattern applies. Greedy search has even less branching (single best neighbor), so prefetching the best neighbor's node data while computing distances for the rest is straightforward. But the benefit is smaller (upper layers are already preloaded via `preloadLayer`).

**Proposed**: Apply prefetch to greedy search only when preloading is disabled or the layer is too large to preload entirely. Low priority.

## Expected impact

### I/O pattern change

**Before (sequential):**
```
iter 1: [Get node] [wait] [Get 16 neighbors] [wait] [compute]
iter 2: [Get node] [wait] [Get 14 neighbors] [wait] [compute]
iter 3: [Get node] [wait] [Get 12 neighbors] [wait] [compute]
...
```

**After (prefetched):**
```
iter 1: [Get node+prefetch 4] [Get 16 nbrs] [wait all] [compute + prefetch next 4]
iter 2: [Get node=cache hit]  [Get 14 nbrs] [wait all] [compute + prefetch next 4]
iter 3: [Get node=cache hit]  [Get 12 nbrs] [wait all] [compute + prefetch next 4]
...
```

The `[Get node]` step becomes a cache hit for ~75% of iterations (W=4 covers 4 out of ~every 5 pops). The `[wait]` after the node load disappears for those iterations.

### Estimated speedup

- Node load latency: ~0.3ms per miss × ~40 iterations = ~12ms saved at 75% hit rate → **~9ms saved**
- Total search p50: 30ms → ~21ms → **~1.4x speedup** from node prefetch alone
- Combined with neighbor batch prefetch across iterations (bonus: neighbors of prefetched nodes are already in cache from their iteration): **~2x plausible**

This is conservative. The real win depends on how well the heap ordering predicts which candidates will be popped next. In practice, the top of a min-heap is stable — candidates inserted early with small distances tend to stay near the top.

## Alternatives considered

### 1. Goroutine pool processing N candidates in parallel

Pop N candidates, dispatch to N goroutines, each fetches + computes independently, merge results.

**Problem**: Breaks algorithm correctness. Beam search termination depends on `closest.dist > farthest_result.dist`. If you process N candidates concurrently, the results set is being modified concurrently, making the termination condition racy. You'd need locks, which serializes the critical section anyway.

**Problem**: Visiting the same neighbor from two concurrent candidates → double work, double FDB reads. Need a concurrent visited set → more synchronization.

Not worth the complexity. FDB future-based prefetch achieves the same I/O overlap without any of these issues.

### 2. 2-hop prefetch (RFC 007 §3.3)

Speculatively fetch neighbors-of-neighbors. With M=16, that's up to 256 speculative reads per iteration. Most are wasted (never visited). Trades massive read amplification for marginal latency reduction.

**Verdict**: Too wasteful. The W=4 candidate prefetch is much more targeted (only prefetches nodes we're very likely to process).

### 3. Full graph preload into memory

Cache the entire layer 0 in memory across transactions. Eliminates all FDB I/O during search.

**Problem**: Layer 0 grows with N. At 1M vectors × 128D × 8 bytes = ~1GB for vectors alone, plus neighbor lists. Not practical.

**Where it works**: Upper layers (already implemented via `preloadLayer`). Could extend to a cross-transaction LRU cache for hot layer-0 neighborhoods (RFC 007 §3.2), but that's a separate RFC.

## Findings (2026-03-28): Prefetch is a non-optimization

### Diagnostic results

Implemented the prefetcher and added instrumentation. Results across 200, 1000, and 10000 vector benchmarks:

| Metric | Per search |
|---|---|
| `pf.get()` calls | ~64 (= efSearch) |
| Cache hits | ~63 |
| Inflight (prefetch) hits | **0** |
| Cold misses | 1 (entry point) |

**Zero prefetch hits.** Every candidate popped from the heap was already in the per-transaction `storage.cache`, loaded during a prior iteration's `loadNodeLayerBatch` call (it was fetched as a neighbor of an earlier candidate).

### Why the premise was wrong

The RFC assumed step 2 (`loadNodeLayerDispatch` for the candidate's node) was a blocking FDB read. In reality, it's a **cache hit** — the candidate was previously discovered as a neighbor of another node and loaded into `storage.cache` by `loadNodeLayerBatch`.

The HNSW beam search explores nodes that were discovered as neighbors. `loadNodeLayerBatch` fetches all unvisited neighbor nodes (vectors + neighbor lists) and caches them. When any of those neighbors are later popped from the heap, their data is already warm.

The only cold read per search is the entry point (iteration 0), which is a single Get.

### Actual I/O profile per search iteration

```
1. Pop candidate          → cache hit (0µs, was fetched as someone's neighbor)
2. Get neighbor list      → from cached node (0µs)
3. Filter unvisited       → CPU only (~1µs)
4. Batch-fetch neighbors  → 1 pipelined FDB round-trip (~100-300µs) ← THE BOTTLENECK
5. Compute distances      → CPU only (~5µs)
```

Step 4 is the only FDB I/O, and it's already fully pipelined (all Gets fired at once, resolved in sequence). There is nothing to overlap.

### Java does the same thing

Reading `Search.beamSearchLayer()` carefully: `AsyncUtil.whileTrue()` runs a sequential loop. Each iteration calls `fetchNodeIfNotCached` (cache hit) then `fetchNeighborhoodReferences` (batched with `forEach` at `maxNumConcurrentNodeFetches` parallelism). The `CompletableFuture` chains do NOT overlap I/O across iterations — they pipeline reads within a single iteration, which Go's `loadNodeLayerBatch` already does identically.

### What would actually help

The real bottleneck is **one pipelined batch per iteration** (~64 batches × ~0.2ms = ~13ms at 1K vectors). Reducing the number of batches requires an algorithmic change:

- **Multi-candidate batching**: Pop W candidates (all cache hits), combine their unvisited neighbors into one batch, fetch once. Reduces round-trips by W×. But this changes exploration order and may affect recall/results.
- **Cross-transaction cache** (RFC 007 §3.2): Cache hot layer-0 neighborhoods in memory across transactions. Eliminates FDB I/O for frequently-accessed graph regions.

Both diverge from Java's behavior. Given the constraint to stay Java-identical, the current implementation is already optimal for the sequential beam search pattern.
