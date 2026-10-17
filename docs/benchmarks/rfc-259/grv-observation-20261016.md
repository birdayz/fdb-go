# GRV observation timing: three-process local measurements

2026-10-16. The candidate measures the adaptive GRV latency after reply publication,
with cooperative yields at 20-recipient boundaries and once before the successful
sample. The RPC-only histogram and MVCC request timestamp stay unchanged.

C++ 7.3.77 appends the timing promise after transaction promises
(`NativeAPI.actor.cpp:7380–7387`), broadcasts in groups of 20
(`fdbrpc/include/fdbrpc/genericactors.actor.h:284–292`), and samples after
`timeReply`'s `delay(0)` (`flow/include/flow/genericactors.actor.h:2074–2079`).
Go's `runtime.Gosched` does not reproduce Flow's priorities or guarantee that every
recipient has completed extraction; this comparison tests the Go placement change,
not scheduler equivalence.

## Setup and populations

- Same local Ryzen 9 3900X host and native FDB 7.3.77 `single memory` cluster,
  with three processes: stateless on CPUs 8/20, transaction on 9/21, storage on 10/22.
  Unlike the earlier one-process results in this directory, roles use separate connections.
- Clients on CPUs 0–7, `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`.
  Driver/control work on CPUs 12–15. No concurrent builds, tests, or profiling.
- All three clientbench binaries report Go 1.26.9. Reference uses libfdb_c 7.3.77.
- Go baseline: `f377e16d926fd6a6d3c2b666d415e8cad71a960d`.
  Candidate: that tree with the GRV observation/publication change only, not the
  scratch allocation or transport-coalescing prototypes. Candidate `grv.go` SHA-256:
  `961767fa7951514ae7b51e81f1b40a822027099021117cc446ed90cdbb56180d`.
- Binary SHA-256, respectively baseline, candidate, unchanged CGo reference:
  `94c0c227809d98d3b175e402321912e8c7a93889f4b3b9ba1bc7c85895755ffa`,
  `78b3894b06198269c52dfc4796fd4b50960c4eb4b893339bb1f480afd90bf687`,
  `82d6dd5a779586b1a7799b1eae315048eb7ff34be4652bca33fc23b1f2dbb12c`.
  The retained reference binary's exact build revision was not recorded here;
  its digest and toolchain identify the artifact used throughout these campaigns.
- Baseline, candidate and CGo run sequentially in interleaved, rotated order.
  [Main campaign](results-grv-observe.jsonl): 210 rows, 13 workload/worker cells.
  [64-worker completion](results-grv-observe-64.jsonl): 45 rows, three cells.
  Every cell has five repetitions per client except `get100/32`, which has ten.
- [Write confirmation](results-grv-write-confirm.jsonl): 20 alternating baseline/candidate
  pairs plus four CGo anchors, 44 rows. All 299 configurations completed successfully.
  Each row represents a separate short and long invocation, not one invocation.

`tools/clientbench/scripts/servercost.py` measures all three server PIDs with
`perf stat`. Server cost is `(long counters − short counters) / (long transactions −
short transactions)`, including warmup transactions. Client CPU and latency come
from the long invocation. Budgets are recorded in every raw row; reads use the
standard short/long budgets, while writes use shorter short runs to reduce subtraction
between different convoy modes. This reduces, but does not eliminate, that confound.

## Server cycles and latency

Medians across repetitions. Cycles are thousands per transaction. Latency is each
long run's mean transaction latency, in microseconds. This is not a multi-node result.

| Workload | Workers | Base cycles | Candidate cycles | CGo cycles | Candidate/base | Base latency | Candidate latency | CGo latency |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| get100 | 1 | 503.4 | 501.3 | 513.6 | −0.4% | 190.6 | 188.0 | 316.3 |
| get100 | 8 | 133.1 | 110.6 | 121.2 | −16.9% | 286.1 | 293.7 | 356.9 |
| get100 | 32 | 65.5 | 63.6 | 43.7 | −3.0% | 367.3 | 373.0 | 708.3 |
| get100 | 64 | 43.2 | 41.2 | 30.7 | −4.4% | 499.5 | 510.1 | 1227.8 |
| grv | 1 | 381.2 | 384.6 | 380.2 | +0.9% | 131.5 | 132.4 | 228.7 |
| grv | 8 | 53.8 | 47.7 | 63.1 | −11.2% | 217.9 | 223.4 | 246.7 |
| grv | 32 | 21.6 | 18.5 | 22.6 | −14.3% | 234.6 | 239.9 | 350.6 |
| grv | 64 | 12.6 | 11.6 | 12.1 | −7.9% | 252.5 | 265.0 | 564.3 |
| range100 | 1 | 554.1 | 557.1 | 558.1 | +0.5% | 215.0 | 218.0 | 343.4 |
| range100 | 8 | 189.4 | 182.6 | 140.5 | −3.6% | 396.2 | 403.7 | 543.1 |
| range100 | 32 | 117.7 | 116.4 | 100.9 | −1.1% | 643.7 | 641.5 | 1133.4 |
| range100 | 64 | 107.3 | 104.0 | 89.3 | −3.1% | 1115.9 | 1125.6 | 1871.8 |
| set100 | 1 | 3076.5 | 3160.5 | 3075.7 | +2.7% | 6480.7 | 6500.2 | 6496.0 |
| set100 | 8 | 457.1 | 646.4 | 471.7 | +41.4% | 9919.1 | 13895.9 | 10809.0 |
| set100 | 32 | 258.7 | 255.7 | 143.6 | −1.2% | 17816.1 | 18236.1 | 10568.1 |
| set100 | 64 | 144.1 | 145.6 | 111.3 | +1.1% | 11653.5 | 11400.3 | 10917.2 |

Across the five 1-worker runs per client, arithmetic means of mean latency change by
−0.9% (`get100`), +0.3% (`grv`), +0.6% (`range100`) and +0.5% (`set100`), all below
the 5% regression limit in this session.

## The apparent 8-worker write regression did not reproduce

The main campaign's five write samples per client had strongly different convoy-mode
populations. Do not omit its apparent 41.4% increase, but do not treat that small median
comparison as a stable effect either. The follow-up alternated the Go pair's order in
20 blocks, using short=500 and long=6000 for each configuration:

| Metric | Base median | Candidate median | Paired geometric-mean ratio | 95% paired bootstrap interval |
| --- | ---: | ---: | ---: | ---: |
| Server cycles/tx | 488,109 | 472,335 | 0.887 | 0.757–1.047 |
| Server syscalls/tx | 24.53 | 23.23 | 0.871 | 0.737–1.035 |
| Client CPU, µs/tx | 64.65 | 63.19 | 0.972 | 0.925–1.023 |
| Mean latency, µs | 10,040 | 9,730 | 0.905 | 0.776–1.055 |

Bootstrap: reset Python `random.Random(20261016)` for each metric. For each of
10,000 replicates, draw 20 pair indices with 20 calls to `rng.randrange(20)`, calculate
`exp(mean(log(candidate/base)))`, sort, and take indices 249 and 9749. The ratio of
arithmetic mean server costs is 0.870, with paired
bootstrap interval 0.738–1.039. The four CGo anchors have median 444,810 cycles/tx;
they are not a matched 20-pair CGo population. The data do not establish a write speedup
or exact equivalence, but do not reproduce the initial large regression.

## Remaining gap

The server-CPU goal is **not complete**. Candidate/CGo median cycles remain 1.45× and
1.34× for `get100` at 32/64 workers, 1.30×/1.15×/1.16× for `range100` at 8/32/64,
and 1.78×/1.31× for `set100` at 32/64. Writes remain multimodal, and no multi-node
confirmation was run. Benchmark success does not establish correctness or authorize
merging the candidate.
