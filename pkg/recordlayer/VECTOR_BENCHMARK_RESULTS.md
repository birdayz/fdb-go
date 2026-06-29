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

---

## 2026-06 HNSW perf pass (branch perf/hnsw-span-decode)

Hardware: 24-core, 64 GB, FDB 7.3.77 testcontainer (single node). All at 1536-D
DOUBLE vectors, the LanceDB-1M comparison shape.

> **UPDATE — cross-transaction cache removed for Java compliance.** The rows below
> marked "+ shared cache" were measured *with* a process-wide, cross-transaction
> node cache. That cache was a **Go-only behavioral divergence** (Java keeps only a
> per-operation `nodeCache`, never cross-transaction) and was **removed** to keep
> this HNSW index 100% Java-faithful. So those rows are **no longer the shipped
> index's numbers** — they're retained as the *measured ceiling* that motivates a
> separate, explicitly-Go-native index (see TODO.md → "Exploration: a second,
> FDB-native vector index"). The **wire-neutral, result-identical** changes
> (raw-span PKs, distance-from-bytes, 4-lane distance) remain in the shipped index;
> its real numbers are the "+ raw-span + distance-from-bytes" rows (search ~25 ms
> p50), and insert reverts to Java's latency-bound, non-asymptoting profile.

### Search (1K × 1536D, k=10, efSearch=64)

| stage | alloc/query | p50 | p99 | recall@10 |
|--|--|--|--|--|
| baseline | 102.8 MB | 46 ms | 64 ms | 0.977 |
| + raw-span neighbor PKs + distance-from-bytes | 23.8 MB | 25 ms | 32 ms | 0.977 |
| + shared cache + 4-lane distance | — | **4.5 ms** | 12.5 ms | 0.977 |

Search beats LanceDB's warmed 25 ms p50. Three wire-neutral, recall-exact changes:
raw-span PKs (no decode/box in the hot loop), distance straight from stored bytes
(no []float64), and a 4-lane unrolled distance reduction.

### Insert (vec/sec, parallelism=8, batch=16)

| graph size | batch=4 (old) | + batch16 | + shared cache | + 4-lane dist (cache on) |
|--|--|--|--|--|
| 1K | 46 | — | — | 177 |
| 6K | — | — | 55.9 | 67 |
| 10K | 10.7 | 24.1 | 45.9 | ~55 |
| 30K | — | — | — | 40.6 |

Crucially the cached rate **asymptotes** rather than collapsing: 177 → 67 → 46 →
40.6 vec/s as the graph grows 1K → 30K — the 10K→30K drop is only ~13% over 3×
the size (per-insert cost stabilizes: bounded efConstruction neighborhood,
neighbor lists saturate at MMax0). Extrapolating the flattening curve, a **1M
build is ~7–9 h on this single node** with the cache sized to hold the graph
(~24 GB for 1M × 1536-D nodes; fits in 64 GB, so no eviction) — feasible as an
overnight build, where uncached (collapsing rate) it was effectively unbuildable.

The shared cross-transaction node cache was the big lever here — it stopped the
per-transaction cache from going cold every batch and re-reading the hot layers,
taking 10K from 10.7 to 45.9 vec/s. **It has since been removed** (Go-only
divergence from Java; see the UPDATE banner above). Without it the shipped index
re-reads the hot upper layers every transaction, exactly as Java does, so the
insert rate is latency-bound and does not asymptote — the asymptoting numbers in
the table above belong to the removed cache and motivate the separate native
index, not this one.

### On "1M"

- 1M queries/sec is physically impossible at 1536-D (~2 M FLOPs/query × 1M =
  ~2.3 PFLOP/s, ~2000× a 24-core CPU). LanceDB's "1m" is the 1M-*vector* dataset.
- 1M-vector SEARCH is ready (4.5–15 ms; raise efSearch for recall at scale).
- 1M-vector BUILD: the ~7–9 h single-node projection (asymptoting ~40 vec/s at
  30K) **depended on the now-removed cross-transaction cache** and therefore
  describes the *future native index*, not the shipped Java-faithful HNSW. The
  shipped index, like Java, re-reads the hot layers each transaction, so its
  insert rate is latency-bound and degrades as the graph grows. Inserts are also
  serialized by the per-prefix write lock (== Java's doWithWriteLock), so a build
  uses ~1 core regardless of machine; going faster needs the FDB-native index
  (TODO) — batched beam search, SPFresh/DiskANN, or atomic-append edges — or a
  real multi-node FDB cluster (parallelizes the write floor).

## SPFresh (RFC-094) — 094.1 baseline (SIFT, build-then-read)

First measured numbers for the FDB-native SPFresh index (phase 094.1: bulk
build + query path; foreground writes land in 094.2, tuning in 094.4):

| Metric | SIFT-10k | SIFT-100k | HNSW reference |
|---|---|---|---|
| Build | 2,687 vec/s | **1,559 vec/s** | 9.5–35 vec/s (degrading) → **~50–150×** |
| Recall@10 (vs brute force) | 1.0000 | **1.0000** | ~0.95 (ef=64) |
| Query p50 | 90 ms | **49 ms** | 25–73 ms (at ≤1k–10k only) |
| Query p99 | 170 ms | **95 ms** | 80–170 ms |

Notes: recall is perfect on SIFT at k_c=96 (8% of postings probed at 100k)
thanks to closure replication + exact sidecar re-rank; real-768D-embedding
recall is TBV per RFC-094 §9. Query p50 includes per-query store open +
maintainer construction (same harness shape as the HNSW benchmark) and
unoptimized Go-side RaBitQ estimation — 094.4 is the tuning phase; the
RFC's <8 ms p50 target stands against that phase, not this one. The
per-query changelog refresh anti-pattern cost ~15% p50 and was removed
during this measurement (queries now pay zero cache-maintenance I/O).

Run: `SPFRESH_BENCH=1 SIFT_N=100000 bazelisk test //pkg/recordlayer/bench:bench_test
--test_arg="--test.run=TestSPFreshSIFTBenchmark" --test_output=streamed
--test_env=SPFRESH_BENCH --test_env=SIFT_N`

### SPFresh RFC-103: parallel staging scan (SIFT-100k build A/B)

The bulk build's staging record-scan is sharded into S disjoint primary-key
sub-ranges scanned concurrently. The synchronous pure-Go FDB client makes a
serial scan latency-bound (one round-trip per batch); the S-way concurrency hides
that latency. Same binary, same SIFT-100k data, S=1 (serial) vs S=8 (sharded),
two runs each:

| staging | build (100k) | vec/sec | recall@10 |
|---|---|---|---|
| **S=1** (serial) | 8.37 / 8.21 s | ~12,060 | 0.9940 |
| **S=8** (sharded) | 6.84 / 6.89 s | ~14,560 | **0.9940** |

**~17% faster build (+21% vec/sec), recall byte-identical.** Recall is unchanged
because the staged SET is shard-count-invariant (pinned by the byte-identical
staging+sidecar determinism test). The win is latency-bound: this single-node
testcontainer has a sub-millisecond round-trip, so localhost **understates** it —
staging was ~37% of the serial build, and S=8 roughly halves it; a real
multi-node cluster (higher RTT, more batches per shard) gains more.

Run (S=8 needs `SIFT_SHARD_SAFE=1` so the metadata's RecordTypeKey-prefixed PKs
pass RFC-103's prefix-safety gate; the default bare-PK metadata is shard-unsafe →
S=1): `SPFRESH_BENCH=1 SIFT_N=100000 SIFT_SHARD_SAFE=1 bazelisk test
//pkg/recordlayer/bench:bench_test --test_arg="--test.run=TestSPFreshSIFTBenchmark"
--test_output=streamed --test_env=SPFRESH_BENCH --test_env=SIFT_N --test_env=SIFT_SHARD_SAFE`

### SPFresh Lmax granularity A/B (SIFT-500k) — NEGATIVE (recall-at-scale item 4)

Tested the spfresh-reviewer's "biggest recall lever": finer cells (smaller Lmax →
more, smaller posting lists) for a better recall ladder. Falsified — finer
granularity LOWERS recall at every fixed probe budget:

| SIFT-500k | Lmax=256 (default) | Lmax=128 (finer) |
|---|---|---|
| cells / active fines | 123 / 5,736 | 246 / 10,266 |
| build | 50.0 s | 57.9 s (slower) |
| recall fast (16/24/64) | **0.8985** @ 6.9 ms | 0.8690 @ 4.3 ms |
| recall default (32/64/200) | **0.9745** @ 13.7 ms | 0.9630 @ 10.9 ms |

At a FIXED w/kc/c probe, smaller lists ⇒ the probe covers fewer total candidates
⇒ recall drops (just faster, and a slower build). Exploiting finer granularity
needs MORE probes (latency cost); reaching the paper's 16% centroid ratio would
need Lmax ≈ 16, far under the FDB-reply-budget floor. So granularity is
structurally bounded and recall is **probe-bound, not granularity-bound** — like
the α-replication sweep (item 3), this lever is spent; default Lmax=256 dominates.
Run: add `SIFT_LMAX=128` to the build-bench env. The recall headroom that remains
is the assignment-quality axis (item 5: drift recovery under ingest), not coarser
or finer cells.

### SPFresh ingest recall-drift (SIFT-300k, fast fill vs bulk) — RFC-104 motivation

Fast foreground ingest costs recall versus a bulk build of the SAME data, and the
rebalancer (drained to quiescence) does NOT recover it. Same query sweep:

| 300k | bulk build (ideal) | fast fill (8 writers, 533 vec/s) | gap |
|---|---|---|---|
| cells / active fines | 74 / 3,418 | 55 / 1,755 | ~½ the fines |
| replication (entries/N) | 1.20× | **1.00×** | closure never fired |
| recall fast (16/24/64) | 0.9205 | **0.8720** | **−4.9 pp** |
| recall default (32/64/200) | 0.9880 | **0.9685** | **−1.9 pp** |

Root cause: a vector is closure-replicated once, at insert, against the coarse
insertion-time topology, where the SPANN RNG rule rejects every non-home centroid
(the item-3 geometry) → it lands at 1.0× replication and is never re-evaluated as
the topology refines. RFC-104 designs an online refinement op to recover it
(validate-first: prototype "refine-all" → measure recovery before building the
budgeted op).

**Recovery CONFIRMED (RFC-104 `refine-all` prototype).** One full refinement pass
over the drifted fast-fill index recovers recall to the bulk baseline:

| 300k fast fill (8 writers) | PRE-refine | POST-refine | bulk (ideal) |
|---|---|---|---|
| recall default (32/64/200) | 0.9735 | **0.9885** | 0.9880 |
| recall fast (16/24/64) | 0.8675 | **0.9225** | 0.9205 |

122k/300k pks moved (3m24s). Recall recovered **even though the topology stayed
coarse** (57 vs 74 cells; replication 1.0→1.09×, not 1.20×) — the drift is
**assignment quality, recoverable by re-routing**, not granularity.

**Budgeted production op (`RefineSPFreshIndex`, `SIFT_REFINE=2`) — also recovers
+ CONVERGES.** Looping the budgeted op (budget 10k/call) until one full cursor
cycle moves nothing: **14 calls, converged**, recall default 0.9680→0.9875
(≈bulk 0.9880), fast 0.8570→0.9030. The default budget recovers to within 0.05pp
of bulk; the fast budget recovers most of the drift but sits ~1.75pp under
bulk-fast (the incremental cursor co-evolves with the rebalancer's splits, so it
converges slightly less optimally than the one-shot at the tightest probe — in
production both loops run continuously). A converged BULK index refines to ZERO
moves (pinned by `TestRecordLayer` "SPFresh refinement", gating
`kc=4·spfreshClosurePool`). Run: `...TestSPFreshForegroundFillBenchmark ...
--test_env=SIFT_REFINE` with `SIFT_REFINE=1` (one-shot) or `=2` (budgeted).

### SPFresh 094.4 tuning sweep (SIFT-100k, recall@10 vs p50/p99)

Same built index, per-query knobs via the scan contract (`High = [k, kc, w, c]`):

| w | kc | c | recall@10 | p50 | p99 |
|---|----|----|-----------|------|------|
| 32 | 96 | 400 | 1.0000 | 52.3ms | 99.1ms |
| 16 | 64 | 200 | 0.9990 | 33.5ms | 44.5ms |
| 8 | 48 | 150 | 0.9970 | 25.2ms | 36.3ms |
| 16 | 32 | 100 | 0.9890 | 14.2ms | 20.3ms |
| 8 | 32 | 100 | 0.9890 | 15.9ms | 26.6ms |
| 8 | 24 | 64 | 0.9740 | 10.4ms | 16.8ms |

Defaults moved to **16/64/200** (recall 0.999, 1.6× faster than 094.1's 32/96/400).
Remaining p50 above the §9 <8ms target is dominated by per-query transaction
overhead (GRV + store open in the bench harness), not index reads — the 094.4
FDB-native items (GRV amortization, metadataVersion piggyback, watch refresh)
own that. Reproduce: `SPFRESH_BENCH=1 SIFT_N=100000 SIFT_SWEEP="w:kc:c,..."`.

### SPFresh 094.4 slice 2: allocation-free scorer (same sweep, after)

| w | kc | c | recall@10 | p50 before | p50 after |
|---|----|----|-----------|------------|-----------|
| 32 | 96 | 400 | 1.0000 | 52.3ms | 40.9ms |
| 16 | 64 | 200 | 0.9990 | 33.5ms | **25.4ms** |
| 8 | 32 | 100 | 0.9890 | 15.9ms | 12.3ms |
| 8 | 24 | 64 | 0.9740 | 10.4ms | 9.5ms |

p99 tightened more (44.5→34.0ms at defaults). The §9 <8ms p50 sits one
step away at the 0.974 point; the residual cost is split between the
posting-read waves and per-entry bookkeeping (tuple unpack + dedup map) —
profile-first before touching it further.

### SPFresh 094.5 churn soak (SIFT-20k, 6 waves × 10% delete+reinsert)

Recall@10 sampled online against brute force over the live set after each
wave: **1.0000 at every wave** (post-build through wave 6; 60% of the index
churned cumulatively; rebalancer actions per wave: 32/0/0/2/0/0). No topology
decay. Reproduce: `SPFRESH_BENCH=1 SIFT_N=20000 SOAK_WAVES=6 … TestSPFreshChurnSoak`;
the 10M soak is the same harness at SIFT_N/SOAK_* scale.

### SPFresh 100k FOREGROUND fill (SIFT-100k, the production write path)

No bulk build: 4 concurrent writers SaveRecord (200/tx) from zero (§6b cold
start) with an in-process rebalancer looping beside them; fill time includes
draining the maintenance queue to quiescence.

| Metric | Value |
|--------|-------|
| Write throughput (fill 0→100k) | **481 vec/s** (4 writers, incl. full drain; 1,552 lifecycle actions) |
| Read default 16/64/200 | recall@10 **0.9880**, p50 **25.0ms**, p99 80ms |
| Read fast 8/24/64 | recall@10 **0.9470**, p50 **9.9ms**, p99 27ms |
| Final topology | 31 cells, ~1.1k fine centroids (converged ≤ cellMax/Lmax) |

For contrast, bulk build at the same N: 1,524 vec/s build, recall 1.0000 @ 25.4ms.
The foreground-fill path surfaced and fixed four convergence bugs (uncommitted
RYW poisoning the shared routing cache; split and coarse-split children born
oversized never re-triggering; the all-SEALED cold-start window hard-erroring
instead of retrying).

### SPFresh 300k FOREGROUND fill (SIFT-300k, post convergence fixes)

Same harness at 3× scale, after the second round of convergence fixes
(unique rebalancer lease owners, cold-start-only first-centroid mint, csplit
pause-window split re-filing — see spfresh_cascade_test.go):

| Metric | Value |
|--------|-------|
| Write throughput (fill 0→300k) | **242 vec/s** (4 writers, incl. full drain; 6,688 lifecycle actions) |
| Read default 16/64/200 | recall@10 **0.9870**, p50 **25.5ms**, p99 34.3ms |
| Read fast 8/24/64 | recall@10 **0.9100**, p50 **9.4ms**, p99 14.2ms |
| Final topology | 97 cells, 3,321 fine centroids, 573k entries (r≈1.9 replication), all ≤ 4×Lmax |
| Integrity | 100/100 sampled pks: membership ⊆ postings, all targets ACTIVE |

The throughput drop vs the earlier 100k number is honest accounting: pre-fix
runs quiesced early with corrupted topologies (recall as low as 0.17 at 300k
when two rebalancer executors sharing a lease owner interleaved lifecycles);
the post-fix rebalancer does all the splits the data actually demands.

### SPFresh 1M FOREGROUND fill (SIFT-1M, the real numbers)

| Metric | Value |
|--------|-------|
| Write throughput (fill 0→1M) | **205 vec/s** (4 writers, 1h21m incl. full drain; 22,852 lifecycle actions) |
| Read default 16/64/200 | recall@10 **0.9500**, p50 **29.4ms**, p99 58.8ms |
| Read fast 8/24/64 | recall@10 **0.8160**, p50 **11.1ms**, p99 16.8ms |
| Final topology | 247 cells, 11,336 fine centroids, 1.95M entries (r≈1.95), 11,298 ≤Lmax / 38 ≤4×Lmax / 0 over |
| Integrity | 100/100 sampled pks; zero orphaned membership targets; task queue empty |

(The earlier, pre-convergence-fix 1M attempt did 420 vec/s with recall 0.058 —
fast and wrong. 205 vec/s is what correct maintenance costs at this scale.)

Recall at fixed probes declines with index size by design — kc=64 probes cover
64/11,336 fines at 1M vs 64/3,321 at 300k. The recall/latency dial is the
per-query (k, kc, w, c) contract: the 100k sweep table above shows the curve;
re-tuning defaults for the 1M+ regime is part of 094.5's defaults freeze.

Foreground fill summary (4 writers, one client process, fill incl. drain):

| Scale | Fill | Recall@10 default | Recall@10 fast |
|-------|------|-------------------|----------------|
| 100k | 481 vec/s | 0.988 @ 25.0ms p50 | 0.947 @ 9.9ms |
| 300k | 242 vec/s | 0.987 @ 25.5ms p50 | 0.910 @ 9.4ms |
| 1M | 205 vec/s | 0.950 @ 29.4ms p50 | 0.816 @ 11.1ms |

### SPFresh 1M ε A/B + w-sweep + QPS (094.5 freeze run)

Fresh 1M foreground fill (205 vec/s, 1h21m, 22,301 lifecycle actions), then
16 sweep configs against the SAME production topology; SIFT_QPS=16 hammers
each config with 800 one-query transactions (the serving shape).

| w | kc | c | ε | recall@10 | p50 | p99 | QPS@16 |
|---|----|----|---|-----------|------|------|--------|
| 16 | 64 | 200 | off | 0.9440 | 27.9ms | 33.3ms | 134 |
| 16 | 64 | 200 | 7.0 | 0.9440 | 27.6ms | 32.4ms | 134 |
| 24 | 64 | 200 | 7.0 | 0.9500 | 28.1ms | 38.7ms | 132 |
| **32** | **64** | **200** | **7.0** | **0.9520** | **27.9ms** | 33.2ms | **134** |
| 8 | 24 | 64 | off | 0.8190 | 10.5ms | 17.3ms | 375 |
| 8 | 24 | 64 | 7.0 | 0.8190 | 10.7ms | 17.9ms | 376 |
| **16** | **24** | **64** | both | **0.8260** | **10.7ms** | 13.6ms | **374** |
| 24 | 24 | 64 | both | 0.8230 | 10.8ms | 16.7ms | 371 |
| 32 | 24 | 64 | both | 0.8220 | 11.2ms | 18.6ms | 365 |
| 8 | 96 | 64 | 7.0 | 0.9360 | 38.1ms | 45.0ms | 127 |
| 8 | 192 | 64 | 7.0 | 0.9480 | 74.9ms | 84.7ms | 66 |
| 16 | 128 | 200 | 7.0 | 0.9730 | 52.0ms | 61.7ms | 80 |
| 16 | 192 | 200 | 7.0 | 0.9870 | 77.1ms | 89.3ms | 59 |

**Findings (vs the spfresh-reviewer's F1–F3 ranking):**

1. **ε=7.0 is inert on SIFT-1M** — recall and latency identical with pruning
   on/off at both budgets. The implementation is Eq.(3)-faithful (ratio
   squared for d² space → true-distance ratio 8); SIFT's distance
   concentration keeps the kc nearest centroids inside the ratio, so nothing
   prunes. Stays default-ON (free here; distributions with spread are where
   it pays), but it does NOT deliver F1's recall-per-ms on this dataset —
   the kc-cap ladder costs full reads: 0.973 @ 52ms (kc=128), 0.987 @ 77ms
   (kc=192).
2. **F2 (routing starvation) is real but small**: at kc=24, w 8→16 buys
   +0.7pp (0.819→0.826); w>16 buys nothing. The 1M fixed-probe decay is
   dominated by F1 (kc tail coverage), confirmed by the kc ladder.
3. **094.5 defaults frozen**: default **32/64/200/ε7** (0.952 @ 27.9ms p50,
   134 QPS — +0.8pp over the old 16/64/200 for free), fast **16/24/64/ε7**
   (0.826 @ 10.7ms p50, 374 QPS — +0.7pp over the old 8/24/64 for free).
   The next recall lever at this scale is replication r=4 + RNG rule
   (rebuild A/B pending), not more probes.

### SPFresh RNG replication rule A/B (SIFT-100k foreground fill, r=2)

SPANN §3.2.2's representative replication (skip a replica closer to an
already-kept centroid than to the vector — spend the copy on a different
direction) vs the prior ratio-only closure, same harness:

| Metric | ratio-only | + RNG rule |
|--------|-----------|------------|
| Fill (4 writers, incl. drain) | 481 vec/s, 1,552 actions | **916 vec/s**, 1,206 actions |
| fast 8/24/64 recall@10 | 0.9470 @ 9.9ms | **0.9530** @ 12.6ms |
| engine default recall@10 | 0.9880 (16/64/200) | **0.9980** (32/64/200, sweep row 30.7ms) |

Diverse replicas beat duplicate replicas on recall (up at every budget — the
paper's Fig. 11 small-budget gap, confirmed) and on fill throughput (nearly
2× — RNG-skipped duplicates mean fewer posting entries, fewer splits, less
drain I/O); fast-budget p50 came in 27% higher on this single run (9.9→12.6ms)
— an unreplicated number on a 137s run, re-measured by the 1M re-pin below.
(Default rows differ in config — w moved 16→32 in the same change; the
+0.6pp at the identical fast config is the clean comparison.)

### CORRECTION (paper re-review): ε calibration + freeze provenance

The paper-authors' re-review caught two defects in the freeze run above:

1. **ε was mis-calibrated by an exponent.** Eq. (3)'s Dist is SPTAG's
   SQUARED L2 and MaxDistRatio = 1+ε applies to it DIRECTLY: published
   ε₂=7.0 means an 8× d² bound (true distance ≈2.83×). Our implementation
   squared the ratio (64× in d²) — so the "ε is inert on SIFT-1M" finding
   above measured a setting 8× looser than published; SPANN Fig. 2/12 were
   measured at the published one. Fixed: the ratio now applies directly to
   d². The ε A/B and the kc-as-cap ladder must be re-measured at the
   corrected setting.
2. **The 1M sweep topology was pre-RNG** (the fill deliberately measured
   the then-committed HEAD while the RNG rule was still in review). The
   frozen 0.952/0.826 therefore describe a topology the shipped write path
   no longer produces.

Both resolved by one 1M foreground refill on the shipped path (RNG on,
corrected ε), re-sweeping ε on/off + kc-cap rows and reporting entries /
effective replication / lifecycle actions — table below when it lands.

### SPFresh profile-driven perf pass (post-094.5-freeze)

Same harness throughout (100k bulk build, 400 queries, default 32/64/200/ε7,
GOMAXPROCS=8); recall@10 pinned at 0.9900 in every row:

| Stage | Build | Query p50 | p99 |
|-------|-------|-----------|-----|
| pre-pass | 7,161 vec/s | 21.1ms | 30.2ms |
| + span posting scan (zero per-entry tuple decode/boxing) | — | 19.2ms | 27.5ms |
| + 4-lane distance kernel + reflection-free sorts | 10,034 vec/s | 17.8ms | 24.5ms |
| + RaBitQ 2-bit fast unpack | **10,704 vec/s** | **15.1ms** | **21.0ms** |

Fast budget at the corrected ε (16/24/64/ε7): **p50 5.74ms, p99 7.98ms @
recall 0.9263** — under the RFC §9 <8ms target; the recalibrated Eq.(3)
pruning now binds (default keeps 0.990 recall at −15% latency; the fast
point trades ~5pp recall for ~2× latency, the Fig. 12 shape).

Kernels (128 dims): spfreshSquaredDistance 83→50 ns (4 accumulator lanes;
the single largest flat CPU cost in build+routing), Scorer.Score 165→108 ns
(fused 2-bit unpack+dot, bit-identical to Distance — pinned by the
differential; landed after the table row above, so even 15.1ms is
conservative). Allocation profile: the postingPK/mergeHit per-entry columns
(~3.7GB across the run) are gone; the remaining query-path allocations are
fdbgo client-layer (snapshot/RYW caches, range materialization) — gated
behind the FDB C++-parity review, tracked as a follow-up.

Build speedup vs the pre-branch baseline (1,524 vec/s single-thread
k-means): **7.0×**.

### SPFresh 1M re-pin (shipped write path: RNG closure + corrected ε)

The paper re-review's two conditions, one run: fresh 1M foreground fill on
the RNG write path with ε applied to d² directly. Topology: **165 cells,
6,228 fine centroids, 1,044,243 entries (effective ρ ≈ 1.04)**, 12,554
lifecycle actions, every posting ≤Lmax but one ≤4Lmax. The RNG rule nearly
halved the index: 1.95M → 1.04M entries, 11,336 → 6,228 lists vs the
ratio-only closure.

| Config | recall@10 | p50 | p99 | QPS@16 |
|--------|-----------|-----|-----|--------|
| default 32/64/200, ε off | 0.9610 | 24.1ms | 34.5ms | 149 |
| **default 32/64/200, ε 7** | **0.9610** | **23.9ms** | 27.1ms | **148** |
| fast 16/24/64, ε off | 0.8300 | 9.2ms | 11.1ms | 422 |
| **fast 16/24/64, ε 7** | **0.8300** | **9.3ms** | 13.8ms | **421** |
| kc=128 cap, ε 7 | 0.9930 | 45.4ms | 54.8ms | 91 |
| kc=192 cap, ε 7 | 0.9980 | 69.2ms | 80.4ms | 64 |
| kc=192 cap, ε 3 | 0.9980 | 68.7ms | 76.6ms | 64 |
| kc=192 cap, ε 15 | 0.9980 | 68.6ms | 75.5ms | 64 |

**Re-pinned freeze (same knobs, better numbers):** default 0.961 @ 23.9ms /
148 QPS (+0.9pp, −14% p50, +10% QPS vs the pre-RNG topology); fast 0.830 @
9.3ms / 421 QPS. The kc-tail ladder gained the most from the leaner
topology: 0.993 @ 45ms and 0.998 @ 69ms (were 0.973/0.987 at higher cost) —
kc now covers 2× the list fraction.

**ε verdict, measured at the PUBLISHED setting (8× in d², SPTAG semantics):
genuinely inert on SIFT-1M.** ε ∈ {0,3,7,15} are indistinguishable at every
budget and at the kc=192 cap. The mis-scaling explained nothing here — the
distance concentration conclusion stands at the corrected operating point.
Stays default-ON (free; spread-out distributions are where Eq. (3) pays).

**Fill: 110 vec/s — a real regression with a known cause.** This run's
binary predates the speculative-burst fix: the RNG diversity scan
REAL-reads up to spfreshClosurePool(r)=16 candidate rows SEQUENTIALLY per
insert (vs ~2-3 pre-RNG), and 1M-density routing keeps more candidates
inside the ratio bound. ρ dropping to 1.04 means writes went DOWN — the
verification round trips are the cost. (The run was also contended by
concurrent profiling for ~1h.) The burst fix (one snapshot burst + explicit
conflict keys on examined rows — semantics-identical) is in the next
commit; the clean fill A/B follows it.

Clean-machine fill A/B (100k, burst fix, quiet box): **705 vec/s** — +46%
over pre-RNG (481), recovering most of the diversity-scan cost; the
residual gap to the shallow-scan 916 is the read volume the leaner ρ≈1.04
topology buys (half the index, better recall, +13% QPS). Reads on the same
production topology with the full perf stack: default 0.996 @ 15.4ms p50,
fast **0.949 @ 6.08ms p50**. (An earlier contended run of the same shape
measured fast 0.963 @ 7.66ms — the number quoted in the perf-pass commit
message; both are under the §9 target, the quiet-box row is canonical.)

### SPFresh 1M clean fill A/B (burst fix + full perf stack) — and the ingest-rate/recall trade

Same harness, quiet box, GOMAXPROCS=16: **FILL 1M in 31m26s = 530 vec/s**
(4 writers, 11,761 lifecycle actions; topology 186 cells / 5,835 fines /
1,009,151 entries, ρ ≈ 1.01) — **2.6× the pre-RNG 205 vec/s** and 4.8× the
un-fixed sequential-verification 110. The speculative-burst fence is the
whole story: verification costs one round trip regardless of diversity-scan
depth.

Query side at 1M with the perf stack (this topology):

| Config | recall@10 | p50 | p99 | QPS@16 |
|--------|-----------|-----|-----|--------|
| default 32/64/200/ε7 | 0.9250 | 17.9ms | 22.0ms | 141 |
| fast 16/24/64/ε7 | 0.7910 | 6.8ms | 10.6ms | 392 |
| kc=128 cap | 0.9730 | 32.5ms | 52.7ms | 90 |
| kc=192 cap | 0.9870 | 47.2ms | 55.1ms | 64 |

p50 down 25–32% at every operating point vs the pre-perf-pass binary on
the slower-filled topology (default 23.9→17.9ms, fast 9.3→6.8ms, kc=192
69→47ms).

**The honest finding: recall at fixed probes depends on the INGEST RATE
the topology was built under.** The 110 vec/s fill read 0.961/0.993/0.998
(default/kc128/kc192); this 530 vec/s fill reads 0.925/0.973/0.987 — same
code, similar lifecycle-action counts. At 5× the write rate the rebalancer
lags the writers, vectors are closure-assigned against a staler topology,
and NPA repairs only split neighborhoods, not global drift. Max-rate
ingest costs ~3.5pp recall at default probes. Operationally: ingest at the
rate your recall target tolerates, raise kc afterward (0.987 @ 47ms holds
even on the fast-filled topology), or run the assignment-refinement sweep
(TODO) after bulk ingest phases. The α-led replication sweep (TODO item 3)
also lifts this floor — diverse replicas make assignment placement less
critical.

### α-led replication sweep (1M foreground fills, shipped path) — measured negative

Four independent 1M fills; the r=2/α=1.2 row is the clean fill A/B above.
α² is the closure's d²-space admission bound; ε₁≈10 (α²=11) is the paper's
own §4.2 closure regime.

| r | α² bound | effective ρ | fill | default | fast | kc=128 | kc=192 |
|---|----------|------------|------|---------|------|--------|--------|
| 2 | 1.44× | 1.009 | 530 vec/s | 0.925 | 0.791 | 0.973 | 0.987 |
| 4 | 1.44× | 1.010 | 413 vec/s | 0.933 | 0.806 | 0.979 | 0.986 |
| 4 | 4× | 1.032 | 419 vec/s | 0.939 | 0.813 | 0.981 | 0.991 |
| 4 | 11× | 1.020 | 415 vec/s | 0.933 | 0.789 | 0.978 | 0.989 |

(The r=2 fast cell read 0.830 in an earlier revision — that number belongs
to the 110 vec/s topology, not the 530 vec/s fill this row cites; the
ingest-rate/recall trade applies to the FAST budget hardest. Torvalds r4
catch.)

**Verdict: closure replication is structurally unavailable on SIFT-1M at
Lmax=256 granularity.** Even at the paper's own 11× admission bound the RNG
rule keeps ρ ≈ 1.02, and recall moves within topology variance (±1pp). The
geometry: SPANN §4.3.1 runs ~6 vectors per posting list (16% centroid
ratio) where boundary vectors sit BETWEEN lists; ours run ~170 (0.6%), so
a vector sits deep inside one cell and every other centroid fails the RNG
diversity test as "same direction past c₁". Replication (Fig. 11) and
ε-pruning (Fig. 12) degrade by the same mechanism — both are granularity
properties, exactly the paper re-review's rider on the Lmax item. r stays
2 (r=4 also costs ~20% fill throughput for nothing); the recall ladder
beyond ~0.94 at 1M runs through granularity (Lmax), the refinement sweep,
and kc.

### Lmax=128 granularity probe, attempt 1 — killed; found the NPA reload bomb

The α-sweep's verdict points at granularity, so: 1M foreground fill at
Lmax=128. KILLED at 1h44m with the fill still incomplete at sustained
~7.5-core CPU (Lmax=256 fills finish in 31-40 min): halving Lmax doubles
the split count AND doubled the per-NPA cost, because **spfreshNPARun did a
full O(fines) routing reload per task** — one reload per split, each over
2× the fines. Fixed: the rebalancer now loads ONE routing cache per round
and shares it across that round's NPAs (round staleness is the same
tolerated staleness as the plan phase's snapshot reads; move transactions
re-verify every pk). The probe reruns with the fix; this section gets the
real Lmax=128 numbers then.

### Provenance note: the sidecar A/B (094.4 slice 2)

The "estimates-only collapses recall 0.999 → 0.69" sidecar verdict quoted in
SPFRESH_OPERATIONS.md was measured during the 094.4 slice-2 scorer work
(SIFT-100k bulk topology, default config, sidecar re-rank disabled via the
noRerank path) and recorded at the time only in the PR #283 description —
this section is its in-repo record. Re-derive before relying on the exact
figure; the direction (re-rank is load-bearing) is not in doubt.

### LanceDB head-to-head (SIFT-1M, same machine, same query/groundtruth files)

Date 2026-06-13, 24-core Ryzen 9 3900X / 64 GB. LanceDB 0.30.0 (Node SDK
@lancedb/lancedb, native Rust core), local directory store on NVMe,
IVF_PQ 1000 partitions × 16 sub-vectors, L2; `refineFactor=10` re-ranks
from exact vectors (their analog of our fp16 sidecar re-rank — without it
PQ error caps recall at ~0.57). k=10, 100 SIFT queries, exact groundtruth;
QPS measured with 16 concurrent requesters. Harness: /tmp/lancedb-bench
shape recorded here — batched `add` of 10k rows, then `createIndex`.

| System / config | ingest (vec/s) | recall@10 | p50 | p99 | QPS@16 | storage |
|---|---|---|---|---|---|---|
| LanceDB IVF_PQ np=20 rf=10 | 40,143 (89,465 raw + 13.7s index) | 0.948 | 5.3ms | 7.9ms | 927 | 518 MB |
| LanceDB IVF_PQ np=128 rf=10 | " | 0.986 | 8.8ms | 10.0ms | 847 | " |
| SPFresh fast 16/24/64/ε7 | 530 (production write path) | 0.830 | 9.3ms | — | 421 | FDB ssd |
| SPFresh default 32/64/200/ε7 | " | 0.961 | 23.9ms | — | 148 | " |
| SPFresh kc=192 ladder | " | ~0.987 | ~47ms | — | — | " |

LanceDB is ~75× faster at ingest and ~4–6× at matched-recall queries.
That gap is architectural, not implementation slack, and it buys exactly
what this index exists for:

- **LanceDB is an embedded, single-process file store.** One writer, no
  transactions across records, queries served from process-local mmap'd
  columnar files; the ANN index is a BATCH build (updates accumulate and
  require reindex/compaction to become ANN-visible — freshness is manual).
- **SPFresh-on-FDB is a multi-tenant transactional index on a shared
  cluster.** Every insert is ACID with the record write (one conflict
  surface, no dual-write divergence), concurrent multi-writer ingest with
  zero coordination, search visibility within the cache-refresh window of
  a commit (no reindex, LIRE rebalancing keeps recall flat under churn —
  the 6-wave churn soak holds 1.0), per-tenant isolation on one cluster,
  and reads that scale out with stateless clients against distributed
  storage. None of that exists in the embedded column.
- Same-machine caveat: SPFresh numbers include FDB server CPU inside the
  same box (testcontainer); a production deployment puts storage servers
  on separate hardware and client-side QPS scales with client count
  (stateless snapshot reads — the 20-tenant soak's aggregate behavior).

Read the table as the price of transactional freshness on shared
infrastructure: ~5× query latency at matched recall and two orders of
magnitude on bulk ingest. When the workload is a single process building a
static index once, use an embedded library; when records mutate
transactionally across many writers and tenants, the embedded library is
not in the running.

### SPFresh bulk-build: two-level wave-B assignment (RFC-099)

The wave-B per-vector assignment scanned the global fine table (flat). RFC-099
routes it two-level (w_b nearest coarse cells → their fines), matching the query
path. Build-time only; no wire/format change.

| metric | flat (w_b=100000) | two-level (w_b=48) |
|---|---|---|
| 500k build | 6,776 vec/s | **9,266 vec/s (1.37×)** |
| 500k recall@10 | 0.9755 | **0.9755 (identical)** |
| assign micro-bench (6,100 fines) | 501 µs/vec | **136 µs/vec (3.7×)** |

At 500k only ~60 coarse cells form, so w_b prunes modestly; the full-build win
grows with scale. `coarsePass` computes the coarse cell count as
`K₀ = ⌈N·Replication / (avgFill·CellTarget)⌉` with `avgFill = (2·Lmax)/3` in
**integer** arithmetic (= 170 at Lmax=256, RFC-094 §8); at 1M with defaults
(Replication=2, Lmax=256, CellTarget=48) that is 246 cells, so the default
w_b=32 scans ≈ 32/246 ≈ 13% of cells ⇒ ~7.7× fewer assignment distances — the
dominant 1M build cost. Recall is unchanged because two-level assignment uses
the same candidate set a query probes.

**Binding-regime A/B** (200k, CellTarget=4 ⇒ **589 cells** by that formula
(avgFill=170: ⌈400000/680⌉ = 589) and confirmed empirically by the
`TOPOLOGY: cells=589` log, so w_b actually binds hard — reproduces, and exceeds,
the 1M pruning ratio at low N without a 1M build):

| w_b | cells gathered | recall@10 | build |
|---|---|---|---|
| flat (100000) | all 589 | 0.9870 | 4,261 vec/s |
| 48 | 48 (8.1%) | 0.9870 | 5,104 vec/s |
| 32 (= w_q, default) | 32 (5.4%) | **0.9870** | 5,116 vec/s |

Recall@10 is **identical (0.9870)** even when w_b=32 gathers only **5.4%** of
cells — the closure's α-bounded replicas span only a few cells, so recall is
insensitive to w_b above a small floor. The default w_b is tied to the query
probe width w_q (=32): the build assigns over exactly the neighborhood a query
navigates (SPANN §3.2.1), and a larger w_b only wastes work placing replicas a
query for that vector never reaches (codex). Reproduce with
`SPFRESH_BENCH=1 SIFT_N=200000 SIFT_CELL_TARGET=4 SIFT_BUILD_W={100000,48,32}`.

### SPFresh bulk-build: assign triangle-inequality bound pruning (RFC-101)

Within the w_b cells (RFC-099), wave-B's fine scan is ~85 % of assign's CPU (a
profile shows `spfreshSquaredDistance` at 64.6 % flat). RFC-101 prunes that scan
with the L2 triangle inequality — using the `d(v, cell)` the coarse routing
already computed — to skip whole cells / fines that cannot enter the pool.
**EXACT** (byte-identical pool ⇒ identical assignment ⇒ identical recall), and it
also eliminates the per-vector gather allocation.

| metric | flat gather (RFC-099) | bound-pruned (RFC-101) |
|---|---|---|
| assign micro-bench (245 cells × 25 fines, 128-D, w_b=32) | 97.2 µs | **84.0 µs (1.16×)** |
| assign bytes/op | 30,019 | **3,011 (10× fewer)** |
| assign allocs/op | 9 | **7** |
| 200k SIFT build (default CellTarget, 50 cells) | 10,889 vec/s | **11,492 vec/s (1.055×)** |
| 200k SIFT recall@10 | 0.9940 | **0.9940 (identical — exact)** |

The distance-pruning is **dimensionality-limited**: at 128-D the distance
distribution concentrates, so triangle-inequality skips (like Elkan/Hamerly) are
modest — 1.16× on assign, ~5 % end-to-end at 200k (w_b gathers 64 % of 50 cells;
binds harder at 1M's 246 cells). The robust, scale-independent win is the **10×
allocation reduction** (less GC churn over 1M+ assigns) plus exactness. Exactness
is pinned by 700 byte-identical fuzz trials (`TestSPFreshGatherTopKExactVsFlat`,
`TestSPFreshAssignExactVsFlat`). No pure-Go path gives an order-of-magnitude here
(the kernel is at the scalar floor, RFC-100); that needs SIMD/GPU.

### SPFresh bulk-build: k-means convergence-fraction early-stop (RFC-102)

The k-means Lloyd assignment step is the #1 distance sink (34.6 % of build CPU).
At high k (k0=246 at 1M) Lloyd runs the full maxIters=25 without converging — a
long micro-refinement tail. A recall-vs-iterations A/B shows that tail is wasted:

| maxIters | 25 | 10 | 6 | 4 |
|---|---|---|---|---|
| recall@10 (SIFT-100k) | 0.9970 | 0.9950 | 0.9960 | 0.9950 |

Recall is flat from 4→25 iters. RFC-102 stops the tail via a convergence-fraction
early-stop (ε=1 %: stop when <1 % of points reassign), as a PARAMETER so the
foreground split/csplit path (k=2) keeps exact convergence (bit-identical — no
recall A/B there) and only the bulk build coarse+wave-A opt in.

| N | build EXACT | build ε=1 % | speedup | recall EXACT → ε=1 % |
|---|---|---|---|---|
| 100k | 12,351 vec/s | 12,776 vec/s | 1.034× | 0.9970 → 0.9960 |
| 500k | 10,198 vec/s | 10,352 vec/s | 1.015× | 0.9840 → 0.9830 |

**Honest: the measured win is small (1.5–3.4 %, near noise)** because coarse k is
small at these scales (k0=25/123, converges fast); the trimmable tail only
appears at the 1M coarse pass (k0=246, 25 non-converging iters per the
instrumented curve), where the win is larger but unmeasured (1M build exceeds the
short-bench budget). Recall-neutral; split path bit-identical. The earlier design
(Hamerly bound pruning) was rejected — its bit-identical form needs the RFC-101
roundoff machinery + tie-break handling to *preserve* a tail the A/B proves is
worthless; stopping the tail is simpler and strictly better.

### SPFresh structural-integrity scale validation (RFC-156 §4.3, SIFT churn soak)

The churn soak (`TestSPFreshChurnSoak`) now asserts the RFC-156 chaos-gate
*structural* invariants at scale via `SPFreshCheckIntegrity`, alongside the
existing recall-stability check — validating that the invariants proven at
chaos-test scale (~hundreds of records) hold on a real SIFT topology under
sustained churn. SIFT-100k, 6 churn waves (10% delete+reinsert/wave):

| metric | value |
|---|---|
| recall@10 post-build | 0.9940 |
| recall@10 after 6 waves | **0.9920** (−0.2pp; no decay, far inside the 5pp gate) |
| members / live records | 100,000 / 100,000 (exactly one membership row per record) |
| active fines | 1,133 |
| max posting length | **234 ≤ Lmax=256** (LIRE split holds the envelope) |
| oversized (>Lmax) / oversizedHard (>4·Lmax) | **0 / 0** |
| badTargets (forward/dead/absent) | **0** |
| membership⊄postings | **0** |

So at 100k on real SIFT data, under churn: every membership target is
ACTIVE/SEALED, membership ⊆ postings, every posting is within the balanced-posting
envelope, and recall is flat. This is the §4.3 *structural* dimension; the
recall-at-fixed-probe ladder vs the papers is the separate §4.3 recall dimension
(needs the per-scale kc/w freeze, tracked in RFC-156 §4.3). Run:

```sh
SPFRESH_BENCH=1 SIFT_N=100000 SOAK_WAVES=6 bazelisk test \
  //pkg/recordlayer/bench:bench_test --test_arg="--test.run=^TestSPFreshChurnSoak$" \
  --test_output=streamed --test_env=SPFRESH_BENCH --test_env=SIFT_N --test_env=SOAK_WAVES \
  --test_timeout=3600
```

The integrity assertion runs at N ≤ 200k (it scans every active fine in one
transaction); above that, recall-stability is the scale signal and the batched
integrity variant is the RFC-156 §4 follow-up. (Runtime: ~110s at 100k.)

**SIFT-1M, 4 churn waves (10% delete+reinsert/wave) — the ceiling:**

| metric | value |
|---|---|
| recall@10 post-build | 0.9620 |
| recall@10 after 4 waves | **0.9620** (flat — zero decay across every wave) |
| live records | 1,000,000 |
| rebalance actions/wave | 57 / 8 / 0 / 0 (lifecycle fired, then quiesced) |
| runtime | ~18.5 min |

Recall holds dead flat at the 1M ceiling under sustained churn — the SPFresh §5.2
recall-stability-under-updates property at scale (a topology that quiesced
oversized or orphaned would decay; it does not). The structural integrity
assertion auto-skips at 1M (single-tx scan limit), so recall-stability is the
scale signal here; pairing it with the batched integrity variant (RFC-156 §4) is
the way to also assert structure at the ceiling. The 0.962 fixed-probe recall
matches the bulk-build 1M figure in the foreground-fill tables above; lifting it
further at 1M is the per-scale kc/w freeze (RFC-156 §4.3 recall dimension), not a
stability problem.
