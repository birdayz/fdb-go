# RFC 006: Realistic Vector Benchmark Datasets

## Problem

Current VECTOR/HNSW benchmarks use random vectors (`rand.NormFloat64()`). Random high-dimensional vectors have poor locality structure — distances are nearly uniform, making HNSW traversal artificially easy. Real-world vector distributions have clusters, outliers, and varying density. Our benchmarks don't reflect production recall/latency characteristics.

## Industry Standard: ann-benchmarks.com

The definitive ANN benchmark suite ([ann-benchmarks.com](http://ann-benchmarks.com/), [github.com/erikbern/ann-benchmarks](https://github.com/erikbern/ann-benchmarks)) evaluates 38 implementations across 14 datasets. Standard metric: Recall@k vs QPS.

### Datasets

| Dataset | Vectors | Dims | Metric | Download |
|---|---|---|---|---|
| **SIFT-1M** | 1,000,000 | 128 | L2 | [corpus-texmex.irisa.fr](http://corpus-texmex.irisa.fr/) |
| GIST-1M | 1,000,000 | 960 | L2 | Same |
| GloVe-25/50/100/200 | 1,183,514 | 25-200 | Angular | [Stanford NLP](https://nlp.stanford.edu/projects/glove/) |
| Fashion-MNIST | 60,000 | 784 | L2 | [Zalando](https://github.com/zalandoresearch/fashion-mnist) |
| NYTimes | 290,000 | 256 | Angular | ann-benchmarks.com |
| DEEP1B (subset) | 9,990,000 | 96 | Angular | ann-benchmarks.com |

### Published HNSW results on SIFT-1M (k=10)

From ann-benchmarks.com (hnswlib, M=16, efConstruction=500):

| Recall@10 | QPS (single thread) |
|---|---|
| 0.80 | ~12,177 |
| 0.93 | ~6,669 |
| 0.96 | ~5,065 |
| 0.98 | ~3,349 |
| 0.99 | ~2,964 |
| 0.999 | ~1,122 |
| 1.000 | ~555 |

### Production HNSW recall baselines

| System | Recall@10 | QPS | Notes |
|---|---|---|---|
| hnswlib (original) | 0.95 | 5,065 | M=16, ef=150, single-thread |
| FAISS HNSW | 0.978 (Recall@1) | — | M=32, ef=64, 20 threads |
| Weaviate | 0.984 | 10,940 | ef=64, p99=3.1ms |
| Qdrant | 0.995 | 626 | p99=38.7ms |
| Pinecone | 0.991 | — | 1M SBERT embeddings |

**Our current result: Recall@10 = 0.980 on 1K random 128D vectors, 14 QPS.** The recall is competitive; the QPS gap is due to sequential FDB reads (see TODO).

## Recommendation: SIFT-1M

**Start with SIFT-1M.** It's the gold standard — every ANN paper, library, and database reports numbers on it.

**Why SIFT-1M:**
- 128D L2 matches our default config
- 10K pre-computed queries with 100-NN ground truth
- 500MB download, feasible for CI/manual tests
- Published baselines from every competitor
- Citation: Jegou et al., "Product quantization for nearest neighbor search" (IEEE TPAMI, 2010)

**Download:**
```sh
# INRIA TEXMEX (canonical source)
curl -O ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz
# HuggingFace mirror
huggingface-cli download qbo-odp/sift1m
```

### File format: fvecs/ivecs

Binary, no header, little-endian. Each vector:
```
[dim: int32LE] [component_0: float32LE] ... [component_{dim-1}: float32LE]
```

| File | Format | Vectors | Dims | Bytes/vec | Total |
|---|---|---|---|---|---|
| `sift_base.fvecs` | float32 | 1,000,000 | 128 | 516 | ~492 MB |
| `sift_query.fvecs` | float32 | 10,000 | 128 | 516 | ~4.9 MB |
| `sift_groundtruth.ivecs` | int32 | 10,000 | 100 | 404 | ~3.9 MB |

### Go loader

```go
func LoadFVecs(path string) ([][]float32, error) {
    f, _ := os.Open(path)
    defer f.Close()
    var vectors [][]float32
    for {
        var dim int32
        if err := binary.Read(f, binary.LittleEndian, &dim); err == io.EOF {
            break
        }
        vec := make([]float32, dim)
        binary.Read(f, binary.LittleEndian, &vec)
        vectors = append(vectors, vec)
    }
    return vectors, nil
}
```

## Phase 2: HuggingFace Embedding Datasets

Real embedding vectors from production models for cosine/inner-product evaluation:

| Dataset | Vectors | Dims | Model | Ground Truth |
|---|---|---|---|---|
| [Cohere/wikipedia-22-12-en-embeddings](https://huggingface.co/datasets/Cohere/wikipedia-22-12-en-embeddings) | 35M | 768 | Cohere embed | No |
| [KShivendu/dbpedia-entities-openai-1M](https://huggingface.co/datasets/KShivendu/dbpedia-entities-openai-1M) | 1M | 1536 | text-embedding-ada-002 | Yes (brute-force) |
| [Qdrant/dbpedia-entities-openai3-text-embedding-3-large-3072-1M](https://huggingface.co/datasets/Qdrant/dbpedia-entities-openai3-text-embedding-3-large-3072-1M) | 1M | 3072 | text-embedding-3-large | Yes |

## Phase 3: Billion-Scale (NeurIPS Big-ANN)

For cluster-grade testing. [big-ann-benchmarks.com](https://big-ann-benchmarks.com/):

| Dataset | Dims | Type | Vectors | Target QPS |
|---|---|---|---|---|
| BIGANN | 128 | uint8 | 1B | 10,000 (T1) |
| DEEP-1B | 96 | float32 | 1B | 10,000 (T1) |
| MS SPACEV-1B | 100 | int8 | 1B | 10,000 (T1) |
| Cohere Wikipedia | 768 | float32 | 35M | NeurIPS 2023 |

Subsets available: 1M, 10M, 100M.

## Implementation Plan

### Phase 1: SIFT-1M integration (immediate)

1. Add `scripts/download-sift.sh` + `testdata/` gitignore
2. `LoadFVecs` / `LoadIVecs` loaders in test utils
3. `BenchmarkVectorSIFT` — parameterized by N (10K default, 100K, 1M)
4. Report: Recall@1/10/100, QPS, p50/p99, build time
5. Compare against published hnswlib/FAISS numbers

### Phase 2: GloVe angular (after Phase 1)

6. GloVe-100 (angular/cosine metric) — validates cosine distance path
7. Recall comparison against ann-benchmarks.com published results

### Phase 3: Production embeddings (after Phase 2)

8. DBPedia OpenAI 1M (1536D) — high-dimensional production vectors
9. Cohere Wikipedia 35M subset — scale test

## Success Criteria

| Dataset | Recall@10 Target | Notes |
|---|---|---|
| SIFT-1M (10K subset) | ≥ 0.92 | ef=64, M=16 |
| SIFT-1M (100K subset) | ≥ 0.90 | Same params |
| SIFT-1M (1M full) | ≥ 0.85 | With FDB overhead |
| GloVe-100 (angular) | ≥ 0.85 | Cosine metric |

QPS targets deferred until sequential FDB reads are optimized (TODO HIGH).
