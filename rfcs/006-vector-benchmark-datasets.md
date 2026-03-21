# RFC 006: Realistic Vector Benchmark Datasets

## Problem

Current VECTOR/HNSW benchmarks use random vectors (`rand.NormFloat64()`). Random high-dimensional vectors have poor locality structure — distances are nearly uniform, making HNSW traversal artificially easy. Real-world vector distributions have clusters, outliers, and varying density. Our benchmarks don't reflect production recall/latency characteristics.

## Proposal

Use industry-standard ANN benchmark datasets for realistic evaluation. Two options:

### Option A: ANN Benchmarks (ann-benchmarks.com)

Standard datasets used by all ANN papers/libraries:

| Dataset | Vectors | Dims | Metric | Size | Source |
|---|---|---|---|---|---|
| **SIFT-1M** | 1,000,000 | 128 | L2 | 512MB | [fvecs format](http://corpus-texmex.irisa.fr/) |
| **GIST-1M** | 1,000,000 | 960 | L2 | 3.6GB | Same source |
| **GloVe-1.2M** | 1,183,514 | 200 | Angular | 900MB | [Stanford NLP](https://nlp.stanford.edu/projects/glove/) |
| **Fashion-MNIST** | 60,000 | 784 | L2 | 180MB | [Zalando](https://github.com/zalandoresearch/fashion-mnist) |
| **NYT-256** | 290,000 | 256 | Angular | 280MB | ann-benchmarks.com |

**SIFT-1M** is the gold standard — every ANN paper reports numbers on it. 128D L2 matches our default configuration.

### Option B: HuggingFace Embedding Datasets

Real embedding vectors from production models:

| Dataset | Vectors | Dims | Model | Source |
|---|---|---|---|---|
| **mteb/stsbenchmark** | 8,628 | 768 | sentence-transformers | HuggingFace |
| **Cohere/wikipedia-22-12-en-embeddings** | 35M | 768 | Cohere embed | HuggingFace |
| **sentence-transformers/all-MiniLM-L6-v2** | varies | 384 | MiniLM | HuggingFace |

These have realistic cluster structure from actual language models. 768D is production-standard.

### Recommendation

**Start with SIFT-1M** (Option A):
1. Well-understood baseline — published results from every ANN library (FAISS, Annoy, ScaNN, etc.)
2. 128D matches our default `VECTOR_BENCH_DIMS=128`
3. L2 metric matches our default `VectorMetricEuclidean`
4. Includes pre-computed ground-truth kNN for recall measurement
5. Small enough to run in CI (512MB download, feasible for testcontainer)

**Add GloVe-1.2M or Cohere embeddings later** for angular/cosine metric evaluation.

## Implementation Plan

### 1. Dataset loader (`pkg/recordlayer/testdata/`)

```go
// LoadSIFTVectors loads vectors from the standard fvecs binary format.
// Format: [dim (int32)] [vec0_f32...] [dim] [vec1_f32...] ...
func LoadSIFTVectors(path string) ([][]float64, error)

// LoadSIFTGroundTruth loads pre-computed kNN from ivecs format.
func LoadSIFTGroundTruth(path string) ([][]int, error)
```

### 2. Download script

```sh
#!/bin/bash
# scripts/download-sift.sh
curl -O http://corpus-texmex.irisa.fr/ftp/sift.tar.gz
tar xzf sift.tar.gz -C pkg/recordlayer/testdata/
# Creates: sift_base.fvecs (1M vectors), sift_query.fvecs (10K queries),
#          sift_groundtruth.ivecs (10K x 100 ground truth)
```

### 3. Benchmark integration

```go
func BenchmarkVectorSIFT(b *testing.B) {
    vectors := LoadSIFTVectors("testdata/sift_base.fvecs")
    queries := LoadSIFTVectors("testdata/sift_query.fvecs")
    groundTruth := LoadSIFTGroundTruth("testdata/sift_groundtruth.ivecs")

    // Insert first N vectors (parameterized)
    // Search with queries
    // Measure recall@1, recall@10, recall@100 vs ground truth
    // Report ops/sec, latency percentiles
}
```

### 4. Reporting format

```
=== SIFT-1M BENCHMARK (N=10000, k=10, ef=64) ===
  Recall@1:  0.95
  Recall@10: 0.92
  Recall@100: 0.85
  QPS:       14.2
  p50:       68ms
  p99:       95ms
  Build:     48 vec/sec (3.5 min for 10K)
```

## Success Criteria

- Recall@10 ≥ 0.90 on SIFT-1M (first 10K vectors)
- Recall@10 ≥ 0.85 on SIFT-1M (first 100K vectors)
- Results comparable to published HNSW numbers (adjusting for FDB overhead)

## Deferred

- Billion-scale datasets (DEEP-1B, SPACEV-1B) — requires real FDB cluster
- GPU-accelerated distance computation
- Quantized recall benchmarks (RaBitQ on SIFT)
