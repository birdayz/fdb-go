# Paced load: writes at parity, reads not

2026-10-18. clientbench's closed loop starts every worker's next transaction as soon
as the previous one finishes. Workers therefore move in rounds, and a round's commits
reach the commit proxy together or, after a disturbance, as two groups that stay apart.
That bunching is a property of the benchmark, not of applications. `-rate` (operations
per second) and `-seed` instead start operation *i* at a pseudo-random Poisson arrival
time: evenly spread on average, random in detail, identical for one seed. Both clients
get the same schedule. `-workers` caps the operations in flight; operation *i* still
runs on worker *i* mod workers, which starts it at once if it is late, and each sample
records its scheduled start (`scheduled_offset_ns`).

## Setup

- AMD Ryzen 9 3900X, Linux 7.2. FoundationDB 7.3.77, three native processes
  (`stateless`, `transaction`, `storage`, `single memory`) pinned to CPUs 8/20, 9/21 and
  10/22, restarted before these runs. clientbench on CPUs 0–7 with `GOMAXPROCS=8`,
  `GOGC=100`, `GOMEMLIMIT=off`, Go 1.26.9; drivers on CPUs 12–15, SMT siblings of client
  CPUs 0–3. No builds, tests or reviewer work ran during measurements.
- Both paced binaries were built from this change on master `203560a9e`
  (Go `58fbcde8…b59b`, CGo with libfdb_c 7.3.77 `10a986cb…0ca0`), with the write delay
  at its default.
- [`servercost.py`](../../../tools/clientbench/scripts/servercost.py)
  `--no-syscall-count`: server cycles, instructions and task-clock only, short and long
  runs, per operation completed (warmup included). The campaign used an equivalent
  scratch copy of this mode; the committed script reproduces it (`get100` at 10,000/s:
  205.7k and 172.7k server cycles against the campaign's 205.0k and 174.3k medians).

## Paced runs

[50 configurations](results-paced-load.jsonl): five alternating Go/CGo pairs per row,
64 workers, the same seed for both clients in a pair. Medians per operation:

| Workload | Offered/s | Achieved/s Go / CGo | Server cycles Go / CGo | Go ÷ CGo | Paired ratios | Client CPU µs Go / CGo | Mean latency µs Go / CGo |
| --- | ---: | ---: | ---: | ---: | --- | ---: | ---: |
| `set100` | 1,000 | 998 / 998 | 876,875 / 921,368 | 0.95 | 0.95 0.96 0.91 1.01 0.99 | 128.6 / 83.6 | 9,596 / 9,826 |
| `set100` | 2,000 | 2,007 / 2,007 | 569,138 / 574,212 | 0.99 | 0.99 1.03 0.97 1.02 0.96 | 98.0 / 64.8 | 9,900 / 9,676 |
| `set100` | 4,000 | 3,996 / 3,996 | 328,437 / 326,314 | 1.01 | 1.01 1.08 0.98 1.05 0.99 | 69.5 / 47.9 | 9,973 / 9,807 |
| `get100` | 10,000 | 10,018 / 10,018 | 204,971 / 174,314 | 1.18 | 1.22 1.16 1.18 1.15 1.18 | 72.4 / 67.0 | 345 / 439 |
| `get100` | 30,000 | 30,046 / 30,046 | 131,497 / 96,979 | 1.36 | 1.36 1.37 1.34 1.38 1.34 | 50.8 / 47.6 | 342 / 558 |

Short/long iterations: `set100` 2,000/12,000, 4,000/24,000 and 8,000/48,000;
`get100` 20,000/120,000 and 60,000/360,000, all with 5% warmup.

- **Writes are at parity.** With scattered arrivals Go costs the servers the same as
  libfdb_c at every rate, with the same latency.
- **Reads are not.** At the same offered load Go costs the servers 1.18–1.36×
  libfdb_c, while answering sooner (342–345 µs against 439–558 µs).
- **Client CPU** is higher for Go on writes (1.45–1.54×) and slightly higher on reads.

## The closed-loop write gap

The same cycles-only method on the closed loop ([40 configurations](results-closed-loop-cycles.jsonl),
ten alternating pairs, binaries `669d35e2…11b7` and the earlier CGo artifact
`82d6dd5a…2b12c`) reproduces the gap reported before:

| `set100` | Server cycles Go / CGo | Go ÷ CGo | Mean latency µs Go / CGo | Runs under 8 ms Go / CGo |
| --- | ---: | ---: | ---: | ---: |
| 8 workers | 563,053 / 422,901 | 1.33 | 9,711 / 6,627 | 4 / 8 of 10 |
| 32 workers | 222,644 / 145,869 | 1.53 | 11,350 / 8,658 | 0 / 4 of 10 |

How often a run splits also depends on instrumentation.
[Single 8-worker runs](results-split-instrumentation.txt) (6,000 iterations, latency
above 8 ms counted as split) split as follows:

| Instrumentation | Go | CGo |
| --- | ---: | ---: |
| none (two batches) | 5 of 16 | 7 of 16 |
| `perf stat` cycles and `raw_syscalls` on the servers | 3 of 6 | 3 of 6 |
| per-thread syscall tracepoints on the servers | 5 of 6 | 3 of 6 |
| syscall tracepoints on the client | 5 of 6 | 2 of 6 |
| none, `GOGC=off` (Go only) | 2 of 10 | – |

The samples are small, but they show the split rate is not a stable property of either
client: it moves with run shape and instrumentation, and per-syscall tracepoints raise
Go's. The closed-loop write gap measures how often lockstep rounds stay together. The
paced runs measure the cost of the requests themselves.

## Not covered

No multi-node runs, one host, five pairs per paced row. Paced `range100` and `grv` and
rates above 4,000/s for `set100` are not measured.
