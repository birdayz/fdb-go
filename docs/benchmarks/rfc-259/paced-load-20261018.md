# Paced load: server cost at parity

2026-10-18. clientbench's closed loop starts every worker's next transaction as soon as
the previous one finishes, so workers tend to move in rounds. `-rate` (operations per
second) and `-seed` instead schedule operation *i* at a pseudo-random Poisson arrival
time: evenly spread on average, random in detail, identical for one seed, so both
clients get the same scheduled offsets. This is bounded, per-worker paced load, not a
fully open loop: `-workers` caps operations in flight, operation *i* runs on worker
*i* mod workers (writes stay per-worker, so persisted validation is unchanged), and a
worker still busy at an operation's scheduled time starts it late. Each sample records
its scheduled start (`scheduled_offset_ns`); latency is measured from the actual start,
so it excludes that lag. Workers wait on a timerfd each: `time.Sleep` cannot wake within
a millisecond while the Go side is idle, which would bunch the starts of the CGo arm.
`summary.json` reports how closely the actual starts followed the schedule (`pacing`).

## Setup

- AMD Ryzen 9 3900X, Linux 7.2. FoundationDB 7.3.77, three native processes
  (`stateless`, `transaction`, `storage`, `single memory`) pinned to CPUs 8/20, 9/21 and
  10/22, restarted before these runs. clientbench on CPUs 0–7 with `GOMAXPROCS=8`,
  `GOGC=100`, `GOMEMLIMIT=off`, Go 1.26.9; drivers on CPUs 12–15, SMT siblings of client
  CPUs 0–3. No builds, tests or reviewer work ran during measurements.
- Both binaries were built from this change on master `203560a9e` (Go `b1134193…0a9a`,
  CGo with libfdb_c 7.3.77 `0a05a0fa…42b`), with the write delay at its default.
- [`servercost.py`](../../../tools/clientbench/scripts/servercost.py)
  `--no-syscall-count`: server cycles, instructions and task-clock only, short and long
  runs, per operation completed (warmup included), 64 workers.

## Results

[50 configurations](results-paced-load.jsonl): five alternating Go/CGo pairs per row,
the same seed for both clients in a pair. Medians per operation; "Abs diff" is Go − CGo
server cycles per operation.

| Workload | Offered/s | Server cycles Go / CGo | Go ÷ CGo | Paired ratios | Abs diff | Client CPU µs Go / CGo | Mean latency µs Go / CGo |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: |
| `set100` | 1,000 | 981,090 / 995,503 | 0.99 | 1.00 0.99 0.99 0.98 0.98 | −14,413 | 135.2 / 89.8 | 9,814 / 9,832 |
| `set100` | 2,000 | 679,708 / 678,338 | 1.00 | 1.03 1.04 0.97 1.02 0.97 | +1,370 | 122.3 / 79.7 | 9,970 / 9,841 |
| `set100` | 4,000 | 426,834 / 426,160 | 1.00 | 1.03 1.02 0.99 0.99 1.00 | +673 | 106.7 / 70.6 | 9,863 / 9,954 |
| `get100` | 10,000 | 256,423 / 271,881 | 0.94 | 0.96 0.93 0.95 0.93 0.96 | −15,458 | 89.0 / 86.0 | 327 / 424 |
| `get100` | 30,000 | 129,622 / 124,076 | 1.04 | 1.04 1.02 1.05 1.06 1.05 | +5,546 | 58.2 / 56.5 | 340 / 554 |

Short/long iterations: `set100` 2,000/12,000, 4,000/24,000 and 8,000/48,000;
`get100` 20,000/120,000 and 60,000/360,000, all with 5% warmup. Both clients achieved
the offered rate within 0.4% in every row.

Pacing fidelity, medians of the long runs (lag = actual − scheduled start; gap CV is the
coefficient of variation of gaps between consecutive actual starts, 1.0 for the
schedule itself):

| Workload | Offered/s | Lag p50 µs Go / CGo | Lag p99 µs Go / CGo | Late > 1 ms Go / CGo | Gap CV Go / CGo |
| --- | ---: | ---: | ---: | ---: | ---: |
| `set100` | 1,000 | 9.2 / 7.6 | 21.9 / 17.0 | 0.00% / 0.21% | 1.01 / 1.03 |
| `set100` | 2,000 | 8.4 / 7.0 | 27.6 / 16.6 | 0.04% / 0.00% | 1.01 / 1.00 |
| `set100` | 4,000 | 8.2 / 7.1 | 21,390 / 45,770 | 5.46% / 7.12% | 1.60 / 1.84 |
| `get100` | 10,000 | 7.9 / 7.2 | 24.9 / 27,852 | 0.01% / 1.43% | 1.00 / 2.83 |
| `get100` | 30,000 | 8.6 / 7.6 | 226 / 291,482 | 0.60% / 9.27% | 1.23 / 16.92 |

- **Server cost is at parity** in these rows: 0.94–1.04× libfdb_c, with absolute
  differences of −15k to +6k cycles per operation.
- **Writes** match on server cost and latency at the three tested rates.
- **Reads** cost the same or less at 10,000/s and 4% more at 30,000/s, with lower Go
  latency in both. At 30,000/s and at `set100` 4,000/s the arrivals no longer follow
  the schedule: workers whose previous operation ran long start late, more so for
  libfdb_c (late fraction 9.3% for `get100`, gap CV 16.9). Bunched arrivals batch
  better, so those rows compare different arrival patterns, not only the clients.
- **Client CPU** is 1.51–1.53× libfdb_c on writes and 1.03–1.04× on reads.

## Superseded first campaign

The first paced campaign ([50 configurations](results-paced-load-sleep.jsonl), binaries
`58fbcde8…b59b` and `10a986cb…0ca0`) paced with `time.Sleep` and recorded no pacing
fidelity. It gave `set100` 0.95–1.01× and `get100` 1.18× at 10,000/s and 1.36× at
30,000/s. With precise pacing the `get100` rows are 0.94× and 1.04×: the earlier read gap
is attributable, at least in part, to the CGo arm's `time.Sleep` wake-ups bunching its
arrivals onto millisecond ticks. Use the results above.

## The closed loop

The cycles-only method on the closed loop ([40 configurations](results-closed-loop-cycles.jsonl),
ten alternating pairs, binaries `669d35e2…11b7` and the earlier CGo artifact
`82d6dd5a…2b12c`) reproduces the earlier write gap:

| `set100` | Server cycles Go / CGo | Go ÷ CGo | Mean latency µs Go / CGo | Runs under 8 ms Go / CGo |
| --- | ---: | ---: | ---: | ---: |
| 8 workers | 563,053 / 422,901 | 1.33 | 9,711 / 6,627 | 4 / 8 of 10 |
| 32 workers | 222,644 / 145,869 | 1.53 | 11,350 / 8,658 | 0 / 4 of 10 |

Earlier reports infer, from run-average commit batches, that a closed-loop round reaches
the commit proxy whole or split. In a small screen of
[single 8-worker runs](results-split-instrumentation.txt) (6,000 iterations; latency
above 8 ms counted as split), how often runs split moved with instrumentation:

| Instrumentation | Go | CGo |
| --- | ---: | ---: |
| none (two batches) | 5 of 16 | 7 of 16 |
| `perf stat` cycles and `raw_syscalls` on the servers | 3 of 6 | 3 of 6 |
| per-thread syscall tracepoints on the servers | 5 of 6 | 3 of 6 |
| syscall tracepoints on the client | 5 of 6 | 2 of 6 |
| none, `GOGC=off` (Go only) | 2 of 10 | – |

These samples are small; they associate per-syscall tracepoints with more Go splits and
do not establish a cause. The closed-loop gap remains open (TODO.md §0, "Write gap"):
the paced results show that server cost depends on arrival pattern, not that the
closed-loop result is invalid.

## Not covered

No multi-node runs, one host, five pairs per paced row. Paced `range100` and `grv`, and
rates where arrivals keep to the schedule above 4,000/s for `set100`, are not measured.
