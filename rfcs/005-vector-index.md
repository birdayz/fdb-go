# RFC 005: Billion-Scale Vector Index on FDB (Hierarchical IVF + RaBitQ)

## Status: WIP

## Motivation

Vector similarity search is table stakes for modern data infrastructure. Every database is adding it. FDB Record Layer has none — Java added an HNSW-based VECTOR index (`VectorIndexMaintainer`), but HNSW scales poorly on networked KV stores (O(M·logN) sequential random reads per insert/query, each a network round-trip).

We can do better: a **partition-tree index** that is fully transactional, lives entirely in FDB, scales to billions of vectors with multi-tenancy, and requires no external services. This exploits FDB's strongest primitive — range reads — instead of fighting it with random graph traversal.

**Build in Go first, port to Java later.** This is a differentiator, not a compatibility item.

## Prior art survey

### Why not graph-based (HNSW / DiskANN)?

Graph traversal = sequential random reads with data-dependent access patterns. Each hop reads a neighbor list, computes distances, then decides which node to visit next. You can't prefetch because the next read depends on the current computation.

| Approach | Reads per query | Per-read latency on FDB | Total | Verdict |
|---|---|---|---|---|
| HNSW | 50-200 hops | ~0.5ms | 25-100ms | Too slow |
| DiskANN/Vamana | 50-100 hops | ~0.5ms | 25-50ms | Marginal |
| Partition tree | 5-8 range reads | ~0.5ms | 3-5ms | **Viable** |

Systems that hit this wall and switched to partitions: **Turbopuffer** (explicitly rejected HNSW for object storage), **CockroachDB** (C-SPANN over their KV layer), **PlanetScale** (SPANN over InnoDB).

Even Java Record Layer's own `VectorIndexMaintainer` uses HNSW with each node as a single KV pair — functional but will hit scaling walls at billion scale on a networked KV store.

### The C-SPANN blueprint (CockroachDB, 2025)

CockroachDB built exactly what we need on a distributed transactional KV store with the same constraints as FDB. Their architecture:

- **Hierarchical K-means tree** with fanout ~100
- Each partition = dozens to hundreds of vectors as contiguous KV rows
- **RaBitQ quantization** (~94% compression, 1 bit per dimension)
- Tree depth: 3 levels for 1M vectors, 5 levels for 10B vectors
- **Search**: 5-8 FDB reads total (tree traversal + partition scans)
- **Insert**: append to nearest partition (1 write). Split if oversized (1 txn).
- No in-memory graph. No full rebuild. Background fixups via regular transactions.

### Key papers and systems

| Paper/System | Year | Key contribution | Relevance |
|---|---|---|---|
| **SPANN** (Microsoft, NeurIPS '21) | 2021 | Hierarchical balanced clustering, posting lists on disk. 2x faster than DiskANN at 90% recall. | Foundation architecture |
| **SPFresh** (Microsoft, SOSP '23) | 2023 | LIRE rebalancing: split/merge/reassign partitions incrementally. 1% DRAM, <10% CPU vs full rebuild. | Maintenance protocol |
| **RaBitQ** (SIGMOD '24) | 2024 | 1-bit quantization with provable error bounds. No training. ~30x compression. | Quantization strategy |
| **Extended RaBitQ** (SIGMOD '25) | 2025 | 2-6 bit generalization. Asymptotically optimal space-error tradeoff. | Higher-recall option |
| **Curator** (SIGMOD '24) | 2024 | Multi-tenant IVF: shared Global Centroid Tree + per-tenant Bloom filters. | Multi-tenancy design |
| **CrackIVF** (PVLDB '25) | 2025 | Lazy adaptive indexing: start with ~100 partitions, refine as queries arrive. 10-1000x faster cold start. | Small/cold tenant optimization |
| **Ada-IVF** (arXiv Nov '24) | 2024 | Workload-adaptive maintenance: only repartition hot+imbalanced partitions. 2-5x higher update throughput than LIRE. | Smart maintenance scheduling |
| **Quake** (OSDI '25) | 2025 | Multi-level adaptive index. 1.5-38x lower query latency, 4.5-126x lower update latency vs HNSW/DiskANN. | Confirms partition approach dominance |
| **C-SPANN** (CockroachDB '25) | 2025 | SPANN adapted for distributed transactional KV. RaBitQ. Background fixups. | Direct blueprint |
| **Turbopuffer** | 2024 | SPFresh-based centroids on S3. 10B+ vectors. Notion/Cursor/Linear. | Validates partitions on high-latency storage |
| **PlanetScale** | 2025 | SPANN+LIRE on InnoDB B-tree. Handles 6x RAM indexes with 30% overhead. | Validates partitions on traditional DBMS |

## Architecture: Hierarchical IVF + RaBitQ

### Why RaBitQ over PQ

| Property | Product Quantization (PQ) | RaBitQ |
|---|---|---|
| Training required | Yes — k-means codebook on sample data | **No** — random orthogonal matrix from seed |
| Codebook storage | Per-index codebook (4-16 KB) | Single seed (8 bytes) |
| Streaming inserts | Codebook degrades as distribution shifts | **Each vector quantized independently** |
| Implementation complexity | ~1000 LOC (k-means + lookup tables) | **~200 LOC** (normalize, rotate, sign-quantize) |
| Compression (768-dim) | ~96 bytes (32x) | **~100 bytes (31x)** |
| Distance computation | Lookup table + sum | **popcount(XOR) + correction** |
| Provable error bound | No | **Yes** — O(1/√D) |
| Industry trend | Legacy (FAISS default) | **Converging** (CockroachDB, Weaviate, Elasticsearch, LanceDB) |

RaBitQ wins on every axis that matters for a transactional KV store: no training, no codebook staleness, trivially incremental, simpler implementation.

### Subspace layout

```
[tenant][store][IndexKey][indexSubspaceKey]
  [TREE][level][partitionID]                          → centroid vector (float32[]) + child partition IDs
  [PARTITION][partitionID][vectorOrdinal]              → RaBitQ bits + correction floats + primary key
  [REVERSE][primaryKey]                                → partitionID (for deletes/updates)
  [META]                                               → dimension, distance metric, RaBitQ seed, tree depth, partition count
```

Design notes:
- **TREE** subspace stores the hierarchical K-means tree. Level 0 = root. Leaf nodes point to PARTITION IDs.
- **PARTITION** entries are contiguous in key space → FDB range reads fetch an entire partition in one call.
- **REVERSE** map enables O(1) lookup of which partition a vector lives in (needed for delete/update).
- **META** stores index configuration. RaBitQ seed is a uint64 — deterministically generates the random orthogonal rotation matrix.
- All keys under the tenant prefix → FDB tenant isolation is free.

### Foreground operations (in record save/delete txn)

**Insert** (2-3 FDB writes):
1. Evaluate key expression → extract float vector from record
2. Traverse tree to find nearest leaf partition (in-memory cache for upper levels, 1-2 reads for lower levels)
3. RaBitQ-quantize the vector (CPU-only: normalize, rotate by seeded matrix, sign-quantize)
4. Write: `PARTITION[partitionID][nextOrdinal] → quantized + corrections + primaryKey`
5. Write: `REVERSE[primaryKey] → partitionID`

**Delete** (2 reads, 2 clears):
1. Read `REVERSE[primaryKey]` → get partitionID
2. Scan `PARTITION[partitionID]` for entry matching primaryKey
3. Clear partition entry + reverse map entry

**Update**: delete old + insert new. Partition assignment may change if vector changed significantly.

All operations fit comfortably in a normal record save transaction.

### Search (single read txn, 5-8 round-trips)

```
1. Read root partition from cache (0 round-trips if cached, 1 if not)
2. For each tree level (2-4 levels):
   - Compare query to centroids, select top-k children
   - Read next level's centroid partitions (1 range read per level, parallelizable)
3. Read top-nprobe leaf partitions (1-3 range reads, parallelizable)
4. For each leaf partition:
   - Compute RaBitQ approximate distances (popcount + corrections)
   - Collect top-K candidates
5. Rerank: fetch full vectors from primary records for top candidates (1 batch read)
6. Return ranked results with distances
```

With fanout ~100 and 1B vectors:
- Tree depth: 5 levels
- Centroid comparisons: ~500 total (5 levels × 100 candidates)
- Leaf partition reads: ~3-5 range reads
- Rerank reads: ~10-50 point reads (for top-K)
- **Total: 8-12 FDB reads, most parallelizable**

### Background maintenance (LIRE-inspired)

Separate goroutine performing partition health operations in independent transactions:

**Split** (partition exceeds size threshold, e.g., 500 vectors):
1. Read partition (1 range read)
2. Run balanced K-means bisection on full vectors (fetch from records if needed, or use quantized approximations)
3. Create 2 new partitions + update parent centroid node
4. Update reverse map entries for moved vectors
5. Delete old partition

Fits in one 5s txn for partitions of ~500 vectors with RaBitQ compression (~100 KB total).

**Merge** (two small adjacent partitions, e.g., <50 vectors each):
1. Read both partitions + parent
2. Combine into one partition + update parent
3. Single txn

**Reassign** (SPFresh LIRE — fix boundary vectors after split/merge):
1. For each neighbor partition of a recently-split partition
2. Check if any vectors are now closer to the new centroid than their current one
3. Move them in small batches (1 txn per batch)

**Adaptive scheduling** (Ada-IVF-inspired):
- Track per-partition "temperature" (insert count since last maintenance) as an atomic counter
- Only maintain hot + imbalanced partitions
- 80% of updates affect cold partitions — skip them

### Multi-tenancy

FDB has native tenant isolation — tenant prefixes are enforced at the storage layer. This gives us multi-tenancy for free at the data isolation level. The architecture question is about index structure sharing.

**Recommended: tiered approach** (Qdrant-inspired + Curator-inspired)

| Tenant size | Index strategy | Why |
|---|---|---|
| < 1K vectors | **Flat brute-force scan** | Linear scan of <1K vectors is faster than any index. No maintenance overhead. |
| 1K – 100K vectors | **CrackIVF lazy indexing** | Start with flat scan, build partitions lazily as queries arrive. Perfect for many cold tenants. |
| 100K – 10M vectors | **Per-tenant partition tree** | Full hierarchical IVF within tenant subspace. 2-3 tree levels. |
| > 10M vectors | **Per-tenant partition tree + dedicated maintenance** | Full tree, priority background maintenance, larger partition budgets. |

**Shared centroids option** (Curator approach):
- When all tenants use the same embedding model, the vector distribution is similar
- Train a Global Centroid Tree (GCT) once on representative data
- Each tenant's vectors are assigned to GCT partitions
- Per-tenant posting lists within shared partitions: `[tenant][PARTITION][globalPartitionID][vectorOrdinal]`
- Pro: no per-tenant centroid training. Con: centroid quality degrades for tenants with unusual distributions.

**Tier promotion**: when a tenant's vector count crosses a threshold, a background job builds the next tier's index structure (like Qdrant's fallback-shard → dedicated-shard promotion).

### Filtered search

"Find nearest vectors WHERE category = X" — closely related to multi-tenancy.

**Strategy depends on filter selectivity:**

| Selectivity | Strategy |
|---|---|
| < 1% of vectors match | **Pre-filter**: maintain per-attribute posting lists, intersect with vector candidates |
| 1-50% match | **In-search filter**: scan partitions, skip non-matching vectors during distance computation |
| > 50% match | **Post-filter**: full vector search, discard non-matching results |

For the common case of tenant_id filtering, FDB tenant prefixes handle this at the storage layer — no filter logic needed.

For attribute filtering within a tenant, we can leverage existing Record Layer VALUE indexes: scan the attribute index to get candidate PKs, then intersect with vector search results.

## Scale math (realistic embeddings)

| Parameter | 768-dim (OpenAI ada-002) | 1536-dim (OpenAI text-3-large) | 128-dim (lightweight) |
|---|---|---|---|
| Raw vector size | 3,072 B | 6,144 B | 512 B |
| RaBitQ 1-bit + corrections | ~112 B (35x) | ~208 B (30x) | ~32 B (16x) |
| RaBitQ 4-bit (Extended) | ~400 B (8x) | ~780 B (8x) | ~68 B (8x) |
| 1B vectors raw | 2.86 TB | 5.72 TB | 477 GB |
| 1B vectors RaBitQ 1-bit | **~104 GB** | **~194 GB** | **~30 GB** |
| 1B vectors RaBitQ 4-bit | ~373 GB | ~727 GB | ~63 GB |
| Reverse map (1B entries) | ~20 GB | ~20 GB | ~20 GB |
| Tree centroids (in-memory) | ~30 MB | ~60 MB | ~5 MB |
| **Total FDB storage (1-bit)** | **~130 GB** | **~220 GB** | **~55 GB** |

Search data per query (nprobe=5 leaf partitions × 500 vectors):
- 1-bit: 5 × 500 × 112B = **274 KB** — trivial for FDB
- 4-bit: 5 × 500 × 400B = **977 KB** — still fine

## Key expression & API

```go
// Key expression: extract vector field from proto message
VectorExpr("embedding", Dimension(768), DistanceMetric(Euclidean))

// Index definition
index := NewIndex("product_embeddings", IndexType_VECTOR_IVF,
    VectorExpr("embedding", Dimension(768)))
index.SetOption(VectorOptionMaxPartitionSize, "500")
index.SetOption(VectorOptionRaBitQBits, "1")           // 1, 2, or 4
index.SetOption(VectorOptionDistanceMetric, "euclidean") // euclidean, cosine, dot_product

// Search: K nearest neighbors
results, err := store.ScanIndexByVector("product_embeddings", queryVector, topK, scanProps)
// Returns RecordCursor[*FDBIndexedRecord] with distance in IndexEntry value

// Aggregate function: nearest neighbors
fn := IndexAggregateFunction{Name: FunctionNameNearestNeighbors, Operand: vectorExpr}
results, err := store.EvaluateAggregateFunction(fn, queryVector, topK)

// Record function: neighbors of THIS record
fn := IndexRecordFunction{Name: FunctionNameNearestNeighbors, Index: "product_embeddings"}
neighbors, err := store.EvaluateRecordFunction(fn, record)
```

## Implementation phases

### Phase 1: MVP — flat IVF + RaBitQ (single-level)

- `VectorIVFIndexMaintainer` implementing `IndexMaintainer`
- `VectorKeyExpression` for extracting float vectors from proto fields
- RaBitQ 1-bit quantization (normalize → rotate → sign-quantize)
- Flat centroid list (no tree — works for <1M vectors)
- Insert/delete in foreground transactions
- `ScanIndexByVector` with brute-force centroid selection + partition scan + reranking
- OnlineIndexer support for initial build (sample → train centroids → assign vectors)
- Unit tests + conformance tests

### Phase 2: Hierarchical tree + background maintenance

- Hierarchical K-means tree (multi-level centroids)
- Background split/merge/reassign worker (LIRE protocol)
- Centroid caching with versionstamp invalidation
- Adaptive maintenance scheduling (Ada-IVF temperature tracking)
- Extended RaBitQ (2-bit, 4-bit options)
- Benchmarks at 1M, 10M, 100M scale

### Phase 3: Multi-tenant optimization

- Tiered index strategy (flat → lazy → full tree based on tenant size)
- CrackIVF lazy indexing for small/cold tenants
- Shared centroid tree option (Curator approach)
- Tier promotion/demotion background jobs
- Per-tenant metrics and monitoring

### Phase 4: Advanced features

- Filtered search (attribute filtering + vector similarity)
- Matryoshka truncation for multi-resolution search
- Hybrid search (vector + TEXT index composition)
- Distance metrics: cosine, dot product, L2
- Index-time dimensionality reduction

## Open questions

1. **RaBitQ recall at 768+ dimensions.** Theory says error is O(1/√D), so higher dims = better. CockroachDB confirmed this works. But we should benchmark recall@10 vs exact search at various bit widths.

2. **Partition split cost.** Balanced K-means bisection on 500 vectors with 768 dims — how many iterations to converge? Must complete within 5s txn. Likely fast (K=2, N=500), but needs benchmarking.

3. **Optimal partition size.** CockroachDB uses "dozens to hundreds." Turbopuffer uses ~1000. PlanetScale doesn't disclose. Need to benchmark the tradeoff: larger partitions = fewer tree levels but more data per range read.

4. **Concurrent split safety.** Two transactions splitting the same partition = conflict. FDB's optimistic concurrency handles this (one wins, other retries). But we need to ensure the retry is safe.

5. **Initial centroid training without data.** For a brand-new index, how do we get initial centroids? Options: (a) CrackIVF — don't, start flat and partition lazily; (b) buffer first N inserts in a flat list, train when threshold reached; (c) use random projection as initial partitioning (no training needed).

6. **Distance metric support.** Euclidean is simplest for RaBitQ. Cosine = normalize then Euclidean. Dot product requires anisotropic correction (ScaNN insight). Phase 1: Euclidean only.

## Non-goals (for now)

- GPU acceleration — pure CPU RaBitQ popcount is fast enough
- Multi-vector per record — one vector field per index
- Cross-tenant search — each tenant is isolated
- Exact nearest neighbor — this is an approximate index by design
- Wire compatibility with Java's HNSW-based VECTOR index — different algorithm, different wire format

## References

### Core papers
- SPANN (Microsoft, NeurIPS '21): hierarchical balanced clustering on disk
- SPFresh (Microsoft, SOSP '23): LIRE incremental rebalancing protocol
- RaBitQ (SIGMOD '24): 1-bit quantization with provable error bounds
- Extended RaBitQ (SIGMOD '25): 2-6 bit generalization, asymptotically optimal
- Curator (SIGMOD '24): multi-tenant vector indexing with shared centroid trees

### Systems papers
- C-SPANN (CockroachDB '25): partition tree on distributed transactional KV
- Quake (OSDI '25): adaptive multi-level IVF, confirms partition dominance
- Ada-IVF (arXiv Nov '24): workload-adaptive maintenance
- CrackIVF (PVLDB '25): lazy adaptive indexing, 10-1000x faster cold start
- PipeANN (OSDI '25): IO-compute overlap for graph search (motivation for avoiding graphs)
- PlanetScale (Blog '25): SPANN+LIRE on InnoDB
- Turbopuffer (Blog '24): SPFresh centroids on S3, 10B+ vectors

### Production systems evaluated
- CockroachDB: C-SPANN + RaBitQ on KV store (closest analog)
- Turbopuffer: SPFresh on object storage (validates partitions on high-latency storage)
- Pinecone: LSM-based partitions, per-namespace isolation
- Weaviate: HNSW + per-tenant shards, rotational quantization (RaBitQ variant)
- Qdrant: tiered multitenancy (fallback shard → dedicated shard promotion)
- Milvus: partition key for multi-tenancy, IVF-PQ at scale
- Elasticsearch: HNSW + RaBitQ
- LanceDB: IVF + RaBitQ
- FDB community: forum post confirming IVF on FDB with 80-100% recall@10

### Quantization alternatives considered and rejected
- Product Quantization (PQ): requires codebook training, stale under distribution shift
- ScaNN anisotropic PQ: only benefits inner product search, adds training complexity
- QINCo/SAQ neural quantizers: require neural network inference at encode time
- Plain binary quantization: inferior distance estimates vs RaBitQ (no correction factors)
