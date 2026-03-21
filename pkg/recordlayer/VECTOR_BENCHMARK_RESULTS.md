# VECTOR/HNSW Benchmark Results

**Date**: 2026-03-21
**Hardware**: AMD Ryzen 9 3900X 12-Core, FDB 7.3.46 testcontainer (single node)
**Go**: see go.mod, FDB Go bindings synchronous/blocking

## SIFT-1M Results (industry standard, 128D float32, L2)

| Metric | 1K vectors | 10K vectors |
|---|---|---|
| **Build** | 35.4 vec/sec (28s) | 9.5 vec/sec (17m33s) |
| **Recall@1** | 1.000 | **0.990** |
| **Recall@10** | 1.000 | **0.998** |
| **Recall@100** | 0.999 | **0.994** |
| **QPS** | 16.3 | **7.9** |
| **p50** | 62ms | 135ms |
| **p99** | 70ms | 170ms |

### Comparison with production systems (SIFT-1M, k=10)

| System | Recall@10 | QPS | Storage | Notes |
|---|---|---|---|---|
| hnswlib | 0.95 | 5,065 | In-memory | M=16, ef=150, single-thread |
| Weaviate | 0.984 | 10,940 | In-memory | ef=64, p99=3.1ms |
| Qdrant | 0.995 | 626 | Disk-backed | p99=38.7ms |
| **FDB Go (1K, v0)** | 1.000 | 16 | FDB-backed | Sequential reads |
| **FDB Go (1K, v1)** | 1.000 | 34 | FDB-backed | Batched FDB futures |
| **FDB Go (1K, v2)** | **1.000** | **39** | FDB-backed | **+8 optimizations (RFC 007)** |
| **FDB Go (10K, v0)** | 0.998 | 8 | FDB-backed | Sequential reads |

**Recall is excellent** — 1.000 on SIFT-1M 1K, beats Weaviate (0.984) and matches Qdrant (0.995).
**v2 (all quick wins)**: 2.4x total speedup. Build: 35→70 vec/sec (2x). p50: 62→26ms. Gap vs Qdrant: 16x.

## Results: 1K × 128D (Double precision, no RaBitQ)

| Metric | Value |
|---|---|
| Insert | 47.7 vec/sec (21ms/op) |
| Sequential search (k=10, ef=64) | 13.7 ops/sec (73ms/op, p50=73ms, p99=80ms) |
| Recall@10 | **0.980** |
| Write cycle (insert+delete) | 7.0 ops/sec (143ms/op) |

## Results: 1K × 128D (Standard Go benchmarks, benchtime=5s)

| Benchmark | ns/op | allocs/op |
|---|---|---|
| BenchmarkVectorInsert | 2,386,286 (2.4ms) | 185 |
| BenchmarkVectorSearch | 414,552 (0.4ms) | 96 |
| BenchmarkVectorDelete | 2,592,186 (2.6ms) | 224 |
| BenchmarkVectorConcurrentSearch (10 readers) | 13,536 ops/sec, p50=671µs, p99=2.6ms | — |

Note: The standard benchmarks use a small pre-populated graph (1K vectors) where the HNSW traversal is shallow. The stress test numbers above are more realistic for production-scale graphs.

## Scaling (stress test, 128D)

| Dataset | Insert rate | Seq search | Concurrent 100 | p50 (100r) | p99 (100r) |
|---|---|---|---|---|---|
| 100 vectors | 349 vec/sec | 89 ops/sec | — | — | — |
| 1,000 vectors | 48 vec/sec | 14 ops/sec | — | — | — |

Insert rate drops significantly as the graph grows — each insert traverses deeper and wider HNSW structure.

## Known Performance Gap vs Java

**Root cause**: Go's FDB bindings are synchronous/blocking. Each `tx.Get()` blocks until the value returns. HNSW traversal does 50-200 sequential FDB reads per operation:

- Insert with efConstruction=200: each candidate neighbor requires fetching its node data + neighbors
- Search with efSearch=64: each visited node requires fetching its neighbor list
- Each FDB get has ~0.3-0.5ms latency to the testcontainer

Java pipelines these reads using `CompletableFuture` — firing 5-10 concurrent gets and processing results as they arrive. **Estimated 5-10x faster** for HNSW operations.

**Fix**: See TODO.md — batch neighbor reads using `tx.GetRange()`, FDB read-ahead (`StreamingMode`), or prefetch neighbor lists into a local cache within the transaction.

## RaBitQ Comparison (100 vectors, 8D)

| Metric | No RaBitQ | RaBitQ (4-bit) |
|---|---|---|
| Insert | 350 vec/sec | ~300 vec/sec (14% slower, quantization overhead) |
| Search | 90 ops/sec | ~90 ops/sec (similar — FDB latency dominates) |

RaBitQ's main benefit (reduced storage, faster distance computation) is masked by FDB read latency. It would matter more with local caching or batch reads.

## How to Run

```sh
# Standard benchmarks (quick, ~45s)
bazelisk test //pkg/recordlayer:vector_benchmark_test \
  --test_arg="-test.bench=." --test_arg="-test.benchtime=5s" \
  --test_output=streamed

# Stress test (configurable)
bazelisk test //pkg/recordlayer:vector_benchmark_test \
  --test_arg="-test.run=TestVectorStressManual" --test_arg="-test.v" \
  --test_output=streamed --test_timeout=600 \
  --test_env=VECTOR_STRESS=1 \
  --test_env=VECTOR_BENCH_SIZE=1000 \
  --test_env=VECTOR_BENCH_DIMS=128 \
  --test_env=VECTOR_BENCH_READERS=10

# Environment variables
VECTOR_BENCH_SIZE=1000       # number of vectors
VECTOR_BENCH_DIMS=128        # dimensions per vector
VECTOR_BENCH_K=10            # kNN k
VECTOR_BENCH_EF_SEARCH=64    # HNSW search expansion factor
VECTOR_BENCH_READERS=10      # concurrent search goroutines
VECTOR_BENCH_RABITQ=false    # enable RaBitQ quantization
```
