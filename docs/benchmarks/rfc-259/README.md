# RFC-259 prototype measurements

Server CPU per transaction for the batching prototype behind
[RFC-259](../../../rfcs/259-request-batching-parity.md) and for the Part A
implementation, against the Go client at base commit `36685ebfa` and libfdb_c 7.3.77. One local server, so the numbers show the
mechanism; the 3-node gap that motivates the RFC is in TODO.md §0.

The later [GRV observation campaign](grv-observation-20261016.md) uses a three-process
cluster and baseline `f377e16d9`. It includes all four workloads at 1/8/32/64 workers
and a 20-pair write confirmation; its absolute costs are not comparable to the
one-process measurements below. The [scratch follow-up screens](followup-screens-20261016.md)
retain 305 further configurations for allocation, native completion yields and GC
diagnostics; none of the experimental behavior is included in the production candidate.

## Setup

- AMD Ryzen 9 3900X (24 threads), Linux 7.2, one host.
- `fdbserver` 7.3.77 (source `3ea44ce1`), run natively, one process,
  `configure new single memory`, pinned with `taskset -c 16-19`.
- clientbench built from `36685ebfa` with Go 1.26.9 (`clientbench_go`, `clientbench_cgo`
  against libfdb_c 7.3.77), pinned with `taskset -c 0-7`, `GOMAXPROCS=8`, `GOGC=100`,
  `GOMEMLIMIT=off`.
- The prototype is [prototype.patch](prototype.patch) applied to `36685ebfa`. Its
  environment switches select the variants and are not meant for merge:

| Label | Client | Switches |
| --- | --- | --- |
| `go-today` | `clientbench_go` at `36685ebfa` | none |
| `proto-grv` | prototype | `FDBGO_EXP_PRECISE=2` (timerfd sleeps), `FDBGO_EXP_GRV=serial` |
| `proto-grv-write` | prototype | as `proto-grv`, plus `FDBGO_EXP_BUSYWRITE=1`, `FDBGO_EXP_COALESCE_US=10,20` |
| `libfdb_c` | `clientbench_cgo` at `36685ebfa` | none |

The prototype's serial rule differs from the rule RFC-259 adopts: it sends at once when
no GRV is in flight, no other client request awaits a reply, and the previous batch held
at most one request.

## Method

[`tools/clientbench/scripts/servercost.py`](../../../tools/clientbench/scripts/servercost.py)
runs clientbench twice per repetition, with a short and a long iteration count, under
`perf stat -p <fdbserver pid>`. The difference divided by the difference in transactions
is the marginal server cost per transaction. Three repetitions per row:

```sh
servercost.py --binary <clientbench> --cluster-file <fdb.cluster> \
  --workload <grv|get100> --workers <N> --short <S> --long <L> --reps 3 \
  --label <label> [--env FDBGO_EXP_...=...] --out results.jsonl
```

| Workers | `--short` | `--long` |
| ---: | ---: | ---: |
| 1 | 3,000 | 9,000 |
| 2 | 6,000 | 18,000 |
| 4 | 10,000 | 30,000 |
| 8 | 15,000 | 45,000 |
| 32 | 20,000 | 60,000 |

[results.jsonl](results.jsonl) has one line per repetition: server task-clock, cycles,
instructions and syscalls per transaction, and the long run's client CPU, allocations,
GC cycles, throughput and latency.

## Results

Median server cycles per transaction, with mean latency in µs in parentheses.

`grv`:

| Workers | Go today | GRV window | + write coalescing | libfdb_c |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 171.7k (78) | 166.4k (76) | 155.1k (75) | 148.0k (131) |
| 2 | 142.7k (133) | 81.3k (122) | 90.3k (157) | 82.4k (143) |
| 4 | 74.6k (139) | 42.2k (124) | 40.2k (149) | 51.7k (156) |
| 8 | 39.3k (154) | 25.9k (135) | 26.2k (165) | 32.2k (168) |
| 32 | 10.9k (180) | 11.1k (160) | 11.1k (210) | 9.2k (303) |

`get100`:

| Workers | Go today | GRV window | + write coalescing | libfdb_c |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 283.0k (129) | 279.5k (127) | 284.9k (131) | 314.8k (233) |
| 2 | 229.6k (186) | 241.0k (179) | 261.2k (186) | 180.8k (230) |
| 4 | 151.0k (223) | 124.8k (200) | 91.0k (251) | 125.2k (249) |
| 8 | 89.0k (264) | 87.6k (243) | 64.7k (268) | 62.2k (293) |
| 32 | 42.9k (425) | 39.2k (403) | 33.4k (430) | 25.3k (716) |

## Part A implementation

The same method and run lengths on the Part A implementation, built from the
`pkg/fdbgo` tree of the commit that adds `results-part-a.jsonl`
(`git log -- docs/benchmarks/rfc-259/results-part-a.jsonl`), against
`go-today` and libfdb_c measured in the same session; per-run results in
[results-part-a.jsonl](results-part-a.jsonl). Median server cycles per transaction, with
mean latency in µs in parentheses. 1-worker server cycles vary by ±15% between sessions,
since each transaction there carries the server's fixed background work; a repeat of the
1-worker rows gave `get100` 286k/284k (today) against 289k/319k (Part A), and `grv`
169k/134k against 176k/175k, with mean latency within 5% of today in every pair.

`grv`:

| Workers | Go today | Part A | libfdb_c |
| ---: | ---: | ---: | ---: |
| 1 | 140.7k (70) | 154.5k (73) | 191.8k (144) |
| 2 | 156.9k (134) | 86.3k (126) | 86.7k (146) |
| 4 | 75.7k (142) | 44.6k (126) | 49.9k (146) |
| 8 | 38.6k (141) | 24.2k (127) | 31.3k (163) |
| 32 | 11.5k (183) | 10.8k (151) | 8.6k (292) |

`get100`:

| Workers | Go today | Part A | libfdb_c |
| ---: | ---: | ---: | ---: |
| 1 | 282.3k (127) | 289.9k (133) | 288.8k (217) |
| 2 | 255.5k (187) | 185.4k (182) | 201.1k (236) |
| 4 | 139.0k (213) | 114.0k (193) | 125.8k (251) |
| 8 | 90.1k (256) | 78.1k (229) | 65.2k (298) |
| 32 | 39.6k (423) | 39.7k (403) | 25.3k (706) |

## Not reproduced here

The `set100` commit-batch counts in RFC-259 come from the server's own trace-log metrics
(`ProxyMetrics.CommitBatchIn` and `TxnCommitIn` deltas around a run) and from client
event traces recorded with the prototype's `FDBGO_EXP_TRACE`. Those raw files were not
kept; the RFC states them as observations, not as table results.

The controller-side drivers behind these results, with a script-to-result map, are in
[`tools/benchcampaign/campaigns/rfc-259`](../../../tools/benchcampaign/campaigns/rfc-259/README.md).
