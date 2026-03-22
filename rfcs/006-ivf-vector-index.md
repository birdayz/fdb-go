# RFC 006: IVF Vector Index on FDB (Hierarchical IVF + RaBitQ)

## Status: WIP

## Motivation

RFC 005 documents the HNSW vector index — a port of Java Record Layer's `VectorIndexMaintainer`. HNSW works but has a fundamental performance problem on networked KV stores: **O(ef) sequential FDB round-trips per query** (68 round-trips at ef=64 for 1K vectors, 96% of latency is layer-0 beam search). See RFC 005 for detailed measurements.

IVF (Inverted File Index) replaces the sequential graph walk with **2 parallel range reads**, regardless of dataset size. This exploits FDB's strongest primitive — range reads — instead of fighting it with random graph traversal.

| | HNSW (ef=64) | IVF (nprobe=10) |
|---|---|---|
| Sequential round-trips | 68 | 2 |
| At 0.3ms/RT (testcontainer) | 18ms | 0.6ms |
| At 1ms/RT (K8s same-AZ) | 68ms | 2ms |
| Scales with N | O(sqrt(N)) more hops | O(1) reads |
| Maintenance | None | Periodic split/merge |
| Insert cost | Expensive (graph walk) | Cheap (append) |

**Build in Go first, port to Java later.** This is a differentiator, not a compatibility item. Ships as `IndexType_VECTOR_IVF` alongside the existing HNSW `IndexType_VECTOR`.

## Prior art

### The C-SPANN blueprint (CockroachDB, 2025)

CockroachDB built exactly what we need on a distributed transactional KV store with the same constraints as FDB:

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

## How IVF works

```
Build time:
  Run k-means on all vectors → N centroids (e.g. 200 for 100K vectors)
  Assign each vector to its nearest centroid
  Store: partition_id → [vector1, vector2, ...]

Query time:
  1. Read all centroids (1 range read, ~25KB for 200 x 128D)
  2. Find the nprobe closest centroids (CPU, microseconds)
  3. Read those nprobe partitions in parallel (nprobe range reads, 1 round-trip)
  4. Scan vectors, compute distances, return top-k (CPU, microseconds)

  Total: 2 round-trips. Done.
```

No graph. No sequential hops. Just partition lookup + scan.

## Architecture

### Why RaBitQ over PQ

| Property | Product Quantization (PQ) | RaBitQ |
|---|---|---|
| Training required | Yes — k-means codebook on sample data | **No** — random orthogonal matrix from seed |
| Codebook storage | Per-index codebook (4-16 KB) | Single seed (8 bytes) |
| Streaming inserts | Codebook degrades as distribution shifts | **Each vector quantized independently** |
| Implementation complexity | ~1000 LOC (k-means + lookup tables) | **~200 LOC** (normalize, rotate, sign-quantize) |
| Compression (768-dim) | ~96 bytes (32x) | **~100 bytes (31x)** |
| Distance computation | Lookup table + sum | **popcount(XOR) + correction** |
| Provable error bound | No | **Yes** — O(1/sqrt(D)) |
| Industry trend | Legacy (FAISS default) | **Converging** (CockroachDB, Weaviate, Elasticsearch, LanceDB) |

RaBitQ wins on every axis that matters for a transactional KV store: no training, no codebook staleness, trivially incremental, simpler implementation. We already have a working RaBitQ implementation in `pkg/rabitq/` (extracted from the HNSW work).

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
6. If partition now exceeds size threshold → split in-place (see Maintenance)

**Delete** (2 reads, 2 clears):
1. Read `REVERSE[primaryKey]` → get partitionID
2. Scan `PARTITION[partitionID]` for entry matching primaryKey
3. Clear partition entry + reverse map entry

**Update**: delete old + insert new. Partition assignment may change if vector changed significantly.

All operations fit comfortably in a normal record save transaction.

### Search (single read txn, 2-8 round-trips)

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
- Centroid comparisons: ~500 total (5 levels x 100 candidates)
- Leaf partition reads: ~3-5 range reads
- Rerank reads: ~10-50 point reads (for top-K)
- **Total: 8-12 FDB reads, most parallelizable**

### Maintenance strategy

IVF partitions drift as data distribution evolves. Centroids become stale, partitions grow unbalanced, recall degrades. Unlike HNSW (which self-maintains per write), IVF needs periodic maintenance.

**Key insight: every maintenance operation fits in a single FDB transaction.** No distributed coordination, no locks, no downtime. The index is always queryable — maintenance just improves quality incrementally.

#### In-place maintenance (foreground, in write transactions)

**Partition split on insert** — when an insert causes a partition to exceed the size threshold (~500 vectors), split it in the same transaction:

1. Read the oversized partition (~500 vectors x 100 bytes = 50KB)
2. Run k-means bisection (K=2, N=500 — converges in <10ms CPU)
3. Write 2 new partitions + update parent centroid node
4. Update reverse map entries for moved vectors
5. Clear old partition
6. Commit

Normal insert: ~0.1ms. Insert that triggers split: ~30ms. Happens every ~500 writes. Variance is high but always under 5s. No background infrastructure needed.

Merges and reassigns are deferred — they're quality improvements, not correctness requirements. A too-small partition wastes a nprobe slot; a misassigned boundary vector reduces recall by ~0.1%. Neither causes wrong results.

#### CLI-driven maintenance (out-of-band, on-demand)

The Record Layer is a library, not a service. The application owns the lifecycle. Maintenance is just another FDB client running transactions — a CLI command, a cron job, or a management API call.

```
$ reclay index maintain my_vector_index --once
Scanning partition health...
  Partition 47: 823 vectors (threshold 500) → splitting
  Partition 12: 3 vectors → merging with partition 13
  2 splits, 1 merge in 3 transactions (0.4s)

$ reclay index repartition my_vector_index
  Sampling 5000 vectors... (txn 1)
  Training 200 centroids... (txn 2)
  Reassigning partitions... (txn 3-89, 12.4s)
  Swapping to new tree... (txn 90)
  Done. Recall improved from 0.87 → 0.95

$ reclay index stats my_vector_index
  Partitions: 203, Vectors: 98,412
  Avg size: 485, Max: 612, Min: 28
  Centroid staleness: 12 days
```

Run it manually when recall drops. Put it in a cron. Or don't — the index still works, just with slightly lower recall.

#### Large operations decompose into small transactions

Operations too big for one 5s transaction decompose naturally:

**Initial centroid training** (OnlineIndexer pattern):
```
Txn 1:   sample 1000 random vectors → temp subspace
Txn 2:   read samples, run k-means, write centroids + empty partitions
Txn 3-N: scan records in chunks, assign each to nearest partition
Final:   mark index READABLE
```

**Full repartition** (rolling rebuild, zero downtime):
```
Txn 1:     create new centroid tree v2 alongside old v1
Txn 2-100: for each old partition, reassign vectors to v2 partitions
Txn 101:   swap v2 to active, clear v1
```

Readers see the old tree until the final swap. If the process crashes at txn 47, resume from where it left off — each transaction left the index in a valid state. This is exactly how OnlineIndexer already works for building new indexes.

#### Impact on concurrent readers and writers

**Readers: zero impact.** Snapshot reads see a consistent point-in-time view. Maintenance transactions are invisible to concurrent readers. A reader mid-query gets old partitions or new partitions, never a mix.

**Writers: near-zero impact.** Conflict only if a user write and maintenance transaction touch the same partition in the same 5s window. With 200 partitions, collision probability is ~0.5% per write. FDB detects the conflict, the loser retries automatically (~20ms). Nobody notices.

**During full repartition** (90 transactions over ~15s): each maintenance transaction touches 2-3 partitions. With 200 partitions and 100 concurrent writers, ~1-2 conflicts per maintenance transaction. Total repartition takes 15s instead of 12s. Writers see an extra retry once per few hundred writes.

#### Maintenance operations reference

| Operation | Trigger | Fits in 1 txn? | Impact on queries |
|---|---|---|---|
| **Split** | Partition > 500 vectors | Yes (~30ms) | In-place on insert, or CLI |
| **Merge** | Partition < 50 vectors | Yes (~5ms) | CLI only, deferred |
| **Reassign boundary** | After split/merge | Yes (batch of 20) | CLI only, improves recall |
| **Full repartition** | Centroid staleness | No — decompose into N txns | CLI, zero downtime |
| **Initial training** | New index build | No — OnlineIndexer pattern | WRITE_ONLY until done |

#### Adaptive scheduling (Ada-IVF-inspired)

Track per-partition "temperature" (insert count since last maintenance) as an FDB atomic counter. The CLI `maintain` command reads temperatures and prioritizes:
- Hot + oversized → split immediately
- Cold + small → merge eventually
- 80% of updates affect cold partitions — skip them

### Multi-tenancy

FDB has native tenant isolation — tenant prefixes are enforced at the storage layer. This gives us multi-tenancy for free at the data isolation level. The architecture question is about index structure sharing.

**Recommended: tiered approach** (Qdrant-inspired + Curator-inspired)

| Tenant size | Index strategy | Why |
|---|---|---|
| < 1K vectors | **Flat brute-force scan** | Linear scan of <1K vectors is faster than any index. No maintenance overhead. |
| 1K - 100K vectors | **CrackIVF lazy indexing** | Start with flat scan, build partitions lazily as queries arrive. Perfect for many cold tenants. |
| 100K - 10M vectors | **Per-tenant partition tree** | Full hierarchical IVF within tenant subspace. 2-3 tree levels. |
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

Search data per query (nprobe=5 leaf partitions x 500 vectors):
- 1-bit: 5 x 500 x 112B = **274 KB** — trivial for FDB
- 4-bit: 5 x 500 x 400B = **977 KB** — still fine

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
- RaBitQ 1-bit quantization (reuse `pkg/rabitq/` from HNSW work)
- Flat centroid list (no tree — works for <1M vectors)
- Insert/delete in foreground transactions
- In-place partition split on insert when oversized
- `ScanIndexByVector` with brute-force centroid selection + partition scan + reranking
- OnlineIndexer support for initial build (sample → train centroids → assign vectors)
- Unit tests + benchmarks vs HNSW

### Phase 2: Hierarchical tree + CLI maintenance

- Hierarchical K-means tree (multi-level centroids)
- CLI `maintain` command (split/merge/reassign in independent transactions)
- CLI `repartition` command (rolling rebuild, zero downtime)
- CLI `stats` command (partition health dashboard)
- Centroid caching with versionstamp invalidation
- Adaptive maintenance scheduling (Ada-IVF temperature tracking)
- Extended RaBitQ (2-bit, 4-bit options)
- Benchmarks at 1M, 10M, 100M scale

### Phase 3: Multi-tenant optimization

- Tiered index strategy (flat → lazy → full tree based on tenant size)
- CrackIVF lazy indexing for small/cold tenants
- Shared centroid tree option (Curator approach)
- Tier promotion/demotion
- Per-tenant metrics and monitoring

### Phase 4: Advanced features

- Filtered search (attribute filtering + vector similarity)
- Matryoshka truncation for multi-resolution search
- Hybrid search (vector + TEXT index composition)
- Distance metrics: cosine, dot product, L2
- Index-time dimensionality reduction

## Open questions

1. **RaBitQ recall at 768+ dimensions.** Theory says error is O(1/sqrt(D)), so higher dims = better. CockroachDB confirmed this works. But we should benchmark recall@10 vs exact search at various bit widths.

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
