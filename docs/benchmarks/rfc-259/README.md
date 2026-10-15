# RFC-259 prototype measurements

Server CPU per transaction for the batching prototype behind
[RFC-259](../../../rfcs/259-request-batching-parity.md), against the Go client at
base commit `36685ebfa` and libfdb_c 7.3.77. One local server, so the numbers show the
mechanism; the 3-node gap that motivates the RFC is in TODO.md §0.

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

## Not reproduced here

The `set100` commit-batch counts in RFC-259 come from the server's own trace-log metrics
(`ProxyMetrics.CommitBatchIn` and `TxnCommitIn` deltas around a run) and from client
event traces recorded with the prototype's `FDBGO_EXP_TRACE`. Those raw files were not
kept; the RFC states them as observations, not as table results.
