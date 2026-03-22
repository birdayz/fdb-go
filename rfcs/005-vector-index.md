# RFC 005: HNSW Vector Index — Java-Compatible Implementation & Analysis

## Status: Implemented

## Summary

Port of Java Record Layer's HNSW-based `VectorIndexMaintainer` to Go, with full wire compatibility. This RFC documents the implementation, measured performance characteristics, and tradeoff analysis that motivates RFC 006 (IVF alternative).

Apple published the official HNSW design document in [PR #3997](https://github.com/FoundationDB/fdb-record-layer/pull/3997) (2026-03-10), available at [foundationdb.github.io](https://foundationdb.github.io/fdb-record-layer/architecture/vector-index-design.html). The feature is marked `@API(API.Status.EXPERIMENTAL)`.

## Implementation

Full Java wire compatibility achieved:
- Compact and inlining node storage formats
- RaBitQ quantization via `pkg/rabitq/` (extracted into separate package matching Java's `fdb-extensions` architecture)
- FHT-KAC rotation transform
- PK trimming, VectorIndexScanContinuation protobuf continuation tokens
- IndexEntry format: Key=(prefix..., trimmedPK...), Value=(vectorBytes|nil)
- HNSW config parsing (M, MMax, MMax0, EfConstruction) from index options
- 13 cross-language conformance tests including Java kNN search

## How HNSW works on FDB

HNSW is a hierarchy of sparse-to-dense graphs. Upper layers have few nodes (express routing), layer 0 has every vector. Search descends from the top layer to layer 0, then does a beam search at layer 0 to find the k nearest neighbors.

```
Layer 2:   A ---- B                          (~4 nodes, preloaded in 1 range read)
Layer 1:   A ---- B ---- C ---- D            (~60 nodes, preloaded in 1 range read)
Layer 0:   A-B-C-D-E-F-G-H-I-J-K-...        (all N nodes, beam search with ef iterations)
```

Each beam search iteration at layer 0:
1. Pop closest unvisited candidate from priority queue
2. **Read its ~M=16 neighbors from FDB** (1 batch round-trip, ~0.3ms)
3. Compute distances to each neighbor (CPU, microseconds)
4. Push good candidates onto queue
5. Repeat until `ef` candidates explored

The iteration count equals `ef` because each iteration explores one candidate. **Each iteration is a sequential FDB round-trip** — you can't prefetch because the next read depends on which candidate the priority queue yields.

## Measured round-trip counts (1K vectors, 128D, RaBitQ)

```
ef    FDB round-trips              breakdown                    latency   recall@10
      (sequential)
─────────────────────────────────────────────────────────────────────────────────────
10    16.6            2 point + 12.6 batch + 2 range             6.6ms    0.68
16    22.4            2 point + 18.4 batch + 2 range             8.3ms    0.80
32    37.0            2 point + 33.0 batch + 2 range            12.9ms    0.90
64    68.0            2 point + 64.0 batch + 2 range            18.1ms    0.93
200   ~200            2 point + ~196 batch + 2 range            29.3ms    0.95
```

- **2 point reads** = loadAccessInfo + 1 existence check (fixed overhead)
- **2 range reads** = 2 upper layer preloads (fixed, cached for greedy descent)
- **N batch reads** = layer 0 beam search, one per iteration, scales linearly with ef
- **~0.27-0.39ms per round-trip** on FDB testcontainer (Docker local network)

## Latency breakdown

```
Phase                    Time         %
─────────────────────────────────────────
Access info (1 Get)      0.08ms      0.5%
Preload 2 upper layers   0.47ms      2.9%
Greedy descent (cached)  0.04ms      0.3%
Layer 0 beam search     15.71ms     96.4%   ← the bottleneck
─────────────────────────────────────────
Total (in-tx)           16.30ms
Tx/store overhead        1.86ms
```

96% of time is in the layer 0 beam search. CPU distance computation is negligible (~1μs per vector with RaBitQ). **The latency is almost entirely FDB network round-trips.**

## The ef tradeoff

`ef` (exploration factor) controls recall vs latency. Higher ef explores more of the graph, finding more true nearest neighbors but requiring more FDB round-trips.

- **ef < k is meaningless** — you can't find 10 neighbors by exploring fewer than 10 candidates
- **ef = k** — minimum viable, low recall (~68% for k=10)
- **ef = 4*k** — decent recall (~90%), the practical sweet spot
- **ef > 8*k** — diminishing returns, recall plateaus due to RaBitQ's approximate distances

Recall plateaus around 0.94-0.95 regardless of ef because RaBitQ's distance estimates have a noise floor — some true neighbors are never explored because their approximate distance is overestimated. The only way past this ceiling is exact (non-quantized) distance computation or higher-bit RaBitQ (4-bit instead of 1-bit).

## Throughput vs latency

FDB's design philosophy: **throughput over latency**. Scale horizontally, accept per-request latency, compensate with massive concurrency.

```
Sequential:    53 QPS, 18ms p50
10 readers:   167 QPS, 60ms p50
```

Snapshot reads don't conflict. More readers = linearly more QPS. On a real cluster with 100 concurrent readers: ~1000+ QPS, each at 18ms. This is fine for many use cases (background ranking, batch processing, recommendation feeds). Problematic only for latency-sensitive paths (autocomplete, real-time search).

## Real-world latency projections

The testcontainer measures ~0.3ms/RT (Docker local networking). Real deployments have higher network latency:

| Environment | ~ms/RT | ef=64 latency | ef=32 latency |
|---|---|---|---|
| Testcontainer (Docker) | 0.3 | 18ms | 13ms |
| Same-AZ K8s pods | 0.5-1.0 | 34-68ms | 19-37ms |
| Cross-AZ | 1-2 | 68-136ms | 37-74ms |

## HNSW's strength: zero maintenance

HNSW has no partitions, no centroids, no background maintenance. Every insert/delete updates the graph locally. No global rebalancing, no retraining. For workloads with high write rates and moderate query latency requirements, HNSW's operational simplicity is valuable.

## When to use HNSW vs IVF

| Use case | Recommendation |
|---|---|
| Java interop required | **HNSW** — only option with wire compatibility |
| < 10K vectors | **HNSW** — 18ms is fine, zero operational overhead |
| Latency-tolerant (>50ms OK) | **HNSW** — simpler, scale throughput via concurrency |
| Latency-sensitive (<10ms) | **IVF** (RFC 006) — 2 round-trips vs 68 |
| > 100K vectors | **IVF** (RFC 006) — HNSW latency grows with sqrt(N) |
| Zero maintenance budget | **HNSW** — IVF needs periodic split/merge |

See **RFC 006** for the IVF alternative design.
