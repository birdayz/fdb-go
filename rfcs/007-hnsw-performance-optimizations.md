# RFC 007: HNSW Performance Optimizations

## Context

SIFT-1M benchmarks show Go's HNSW achieves excellent recall (0.998@10) but 19x slower QPS than Qdrant and ~150x slower than hnswlib. The bottleneck is sequential FDB reads — each search does 38-58 pipelined round-trips, each ~0.3-0.5ms.

**Current numbers (1K×128D, Ryzen 9 3900X, FDB testcontainer):**
- Insert: 49 vec/sec
- Search: 34 QPS (p50=30ms, p99=33ms)
- Recall@10: 1.000

**Target:** Close the gap with Qdrant (626 QPS) by reducing FDB round-trips and CPU overhead.

---

## Phase 1: Quick Wins (wire-compatible, low complexity)

### 1.1 Persist hnswStorage across operations in same transaction
**Impact: 30-50% for batch insert**

`hnswStorage` (and its node cache) is recreated from scratch for every `Update`/`SearchKNN` call. When inserting N records in a single transaction, all graph data from insert #1 is thrown away before insert #2.

**Fix:** Lift the `hnswStorage` instance to the `vectorIndexMaintainer` level so the cache persists across operations within the same transaction.

### 1.2 Binary-insert instead of sort.Slice in searchLayerMulti
**Impact: 40x on sort code (~1-2% wall-clock)**

`sort.Slice` on the results slice (up to ef=200 elements) runs after every candidate insertion. Binary search for position + `copy()` is O(log n + n) vs O(n log n) per insertion.

### 1.3 Cache parsed node data, not raw bytes
**Impact: 15-20% wall-clock**

The node cache stores raw FDB bytes and re-parses them via `tuple.Unpack` on every access (including cache hits). Storing the parsed `(vecBytes, neighbors)` struct directly eliminates ~50% of `parseNodeValue` calls and ~1000 allocations per search.

### 1.4 Snapshot reads for search
**Impact: HIGH under concurrent load (eliminates search transaction conflicts)**

HNSW search is read-only and approximate. Using `tx.Snapshot()` avoids adding read conflict ranges. Already proven pattern in the codebase (`store_state_cache.go`, `range_set.go`). One-line change: `graph.Search(m.tx.Snapshot(), ...)`.

### 1.5 Avoid visited-set string allocation
**Impact: 5-8% wall-clock**

`string(nbPK.Pack())` allocates twice per neighbor check (~3000 calls per search). Options:
- Cache Pack() result on `hnswCandidate` struct (reuse for storage lookups)
- For single-int64 PKs: use `map[int64]bool` fast path (zero allocations)

### 1.6 Delete repair candidate sampling (efRepair)
**Impact: MEDIUM for delete on large graphs**

Java's `shouldUseSecondaryCandidateForRepair()` samples neighbors-of-neighbors probabilistically, limiting candidates to ~efRepair=64. Go fetches ALL neighbors-of-neighbors (potentially 16×32=512 nodes).

**Fix:** Add `EfRepair int` to `HNSWConfig` (default 64), sample secondary candidates.

### 1.7 Dual priority queues (max-heap for results)
**Impact: LOW-MEDIUM (cleaner algorithm, slight CPU reduction)**

Java uses a min-heap for candidates + max-heap for results (pre-allocated to efSearch+1). Go uses a min-heap for candidates + sorted slice for results. The max-heap avoids full re-sort.

### 1.8 Parallel existence + access info fetch on insert
**Impact: LOW (saves 1 round-trip per insert)**

Java uses `thenCombine` to fetch access info AND check node existence in parallel. Go does them sequentially. Fix: fire both `tx.Get()` calls before resolving either.

### 1.9 Pool/reuse float64 buffers
**Impact: 10-12% (GC pressure reduction)**

Every `deserializeVector` allocates a fresh `[]float64` of 128 elements. A scratch buffer on `hnswGraph` (single-threaded per transaction) eliminates 300 allocations per search.

---

## Phase 2: Java-Aligned Optimizations (moderate complexity)

### 2.1 Inlining Storage Adapter for upper layers
**Impact: HIGH (5-10% search latency reduction)**

Java supports two storage layouts:
- **Compact** (layer 0): `(layer, pk) → (kind, vector, [neighborPKs])`. Reading a node's neighbors requires N additional gets to fetch their vectors.
- **Inlining** (layers > 0): `(layer, pk, neighborPK) → neighborVector`. One range scan gets all neighbors WITH their vectors. No extra gets needed.

For upper layers (typically 4-62 nodes), a single `GetRange` replaces N individual `Get` calls. This is Java's `InliningStorageAdapter`.

**Wire compatibility:** Safe. Inlining is a Go-internal storage choice for upper layers. Java and Go only need to agree on layer 0 format for cross-language reads. Upper layer format can differ since each side builds its own graph structure. The `Config.useInlining` option controls this.

**Trade-off:** Higher write amplification (vectors stored per-edge). Acceptable for upper layers which have few edges and change rarely.

### 2.2 GetRange for entire upper layer (greedy descent)
**Impact: saves ~5 round-trips (1-2.5ms)**

At upper layers with few nodes (layer 2: ~4 nodes, layer 1: ~62 nodes), a `GetRange` on the entire layer prefix fetches everything in one round-trip:
```go
prefix := dataSubspace.Pack(Tuple{layer})
allNodes := tx.GetRange(PrefixRange(prefix), RangeOptions{Mode: StreamingModeWantAll})
```

This collapses greedy descent from ~7 round-trips to ~2 (one per layer). For layer 1 with ~62 nodes: reads ~62 × 1.2 KB = ~74 KB in one shot.

Can be combined with 2.1 (inlining) for maximum effect, or used standalone.

### 2.3 Concurrent multi-layer deletion
**Impact: LOW**

Java deletes from multiple layers in parallel (parallelism=2). Go deletes sequentially. With typical 2-4 layers per node, the benefit is small.

### 2.4 Ring search + outward traversal iterator
**Impact: Feature gap (not performance)**

Java's `kNearestNeighborsRingSearch()` and `OutwardTraversalIterator` enable cursor-based pagination of vector search results ordered by distance. Required for integration with the record layer's cursor/continuation infrastructure for `BY_DISTANCE` scan type with proper pagination.

---

## Phase 3: Advanced Optimizations (high complexity)

### 3.1 RaBitQ full transform pipeline
**Impact: MEDIUM-HIGH (recall quality with quantization)**

Java's full RaBitQ pipeline includes:
- **FHT-KAC rotation** (fast Hadamard transform): decorrelates vector dimensions for better quantization
- **Centroid subtraction**: centers vectors around the dataset mean
- **Centroid bootstrapping**: samples vectors during initial inserts, computes centroid after threshold, transitions from identity to full transform

Go has the quantizer/estimator but not the transform. Without rotation/centroid, RaBitQ distance estimates are lower quality.

**Complexity:** HIGH — requires FFT library, matrix operations, sampled vector infrastructure, transform transition logic.

### 3.2 Cross-transaction entry point cache with versionstamp invalidation
**Impact: MODERATE (saves 1-3 round-trips per search)**

Cache access info (entry point) and top-layer nodes in memory across transactions. Invalidate via `\xff/metadataVersion` or a per-graph versionstamp key. Pattern already proven in `MetaDataVersionStampStoreStateCache`.

### 3.3 2-hop prefetch at layer 0
**Impact: 20-30% of round-trips reduced, but significant read waste**

Speculatively fire reads for neighbors-of-neighbors while computing distances on current neighbors. Trades bandwidth for latency. With M=16, prefetches ~256 nodes per iteration — many wasted.

### 3.4 Inline neighbor vectors (RaBitQ only)
**Impact: 5-8x read reduction at layer 0**

Store neighbor vectors within the node value: `(kind, vector, [(neighborPK, neighborVec), ...])`. Eliminates the batch read that follows every neighbor list fetch.

Only viable with RaBitQ (105 bytes per vector vs 1025 for float64). With M=16 + RaBitQ: node value ~2.9 KB (safe). With float64: ~17.6 KB (approaches 100KB FDB limit).

**Wire compatibility: breaks Java interop.** Only viable as a Go-only optimization.

---

## Phase 4: Not Worth Doing

| Optimization | Why Not |
|---|---|
| SIMD distance computation | <1% wall-clock (I/O dominates by 1000x) |
| Goroutine-parallel distance | Zero benefit, goroutine overhead > computation |
| Little-endian vector storage | Breaks wire compat for <1% gain |
| Consolidate all layers into one KV | Breaks Java compat, worse write amplification |
| FDB atomic neighbor append | Not feasible (pruning requires read-modify-write) |
| Read/write listener instrumentation | Observability, not performance |

---

## Priority Order

| Priority | Item | Est. Speedup | Effort | Wire Compat |
|---|---|---|---|---|
| **P0** | 1.1 Persist hnswStorage cache | 30-50% batch insert | Low | Safe |
| **P0** | 1.3 Cache parsed data, not raw bytes | 15-20% | Low | Safe |
| **P0** | 1.4 Snapshot reads for search | HIGH concurrent | 1 line | Safe |
| **P1** | 1.2 Binary-insert sort | 1-2% | Very low | Safe |
| **P1** | 1.5 Avoid visited-set alloc | 5-8% | Low | Safe |
| **P1** | 1.6 efRepair sampling | MEDIUM delete | Low | Safe |
| **P1** | 1.9 Pool float64 buffers | 10-12% | Low | Safe |
| **P2** | 2.1 Inlining storage (upper layers) | 5-10% search | HIGH | Safe (Go-only) |
| **P2** | 2.2 GetRange upper layers | 1-2.5ms | Low | Safe |
| **P2** | 2.4 Ring search + outward iterator | Feature | Medium | Safe |
| **P3** | 3.1 RaBitQ full transform | Recall quality | HIGH | Safe |
| **P3** | 3.2 Cross-tx entry point cache | 1-3 round-trips | Medium | Safe |

**Estimated cumulative impact of P0+P1:** ~40-60% improvement on search, ~50-80% on batch insert.
**Estimated cumulative impact of P0+P1+P2:** ~60-80% on search, bringing QPS from ~34 to ~55-60.

The remaining gap to Qdrant (626 QPS) is inherent to FDB's storage model (network round-trips per read) vs Qdrant's in-memory/mmap approach. Closing that gap requires caching hot graph data across transactions (P3) or moving to a hybrid architecture.
