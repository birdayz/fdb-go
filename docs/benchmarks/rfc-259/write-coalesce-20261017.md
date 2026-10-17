# Connection write delay

2026-10-17. Each connection now waits before writing a batch, as libfdb_c's
`connectionWriter` does: `delayJittered(max(MIN_COALESCE_DELAY, MAX_COALESCE_DELAY -
(now - lastWriteTime)))`, then writes everything queued for the connection, without
another wait until the queue is empty (FlowTransport.actor.cpp:704-739).
`lastWriteTime` is stamped before each socket write. The knobs are 10 and 20 µs (flow/Knobs.cpp:211-212);
the jitter is 0.9–1.1 (flow.h `delayJittered`, Knobs.cpp:54-55). `WithCoalesceDelay`
on `client` and `fdb` sets the bounds; the default is libfdb_c's, and `0, 0` restores
the earlier behaviour, where an idle connection writes a frame on its sender's goroutine.

This is RFC-259 Part B without that RFC's idle fast path: every batch waits, including
after an idle period, as in libfdb_c.

## Setup

- AMD Ryzen 9 3900X, Linux 7.2. FoundationDB 7.3.77 run natively as three processes
  (`stateless`, `transaction`, `storage`), `configure new single memory`, pinned to
  CPUs 8/20, 9/21 and 10/22. The cluster was restarted before these campaigns, so
  absolute costs are not comparable with earlier reports.
- clientbench with Go 1.26.9, `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, on CPUs 0–7.
  Drivers ran on CPUs 12–15, SMT siblings of client CPUs 0–3: shared-host results, not
  full isolation. No builds, tests, CI or reviewer work ran during the measurements.
- [`tools/clientbench/scripts/wirediff.py`](../../../tools/clientbench/scripts/wirediff.py):
  short and long runs per configuration. Server cycles and syscalls by role
  (`perf stat --per-thread`), client cycles and client TCP segments are (long − short)
  per extra transaction. Segments are the last `ss` counters of each client socket,
  sampled every 100 ms, so sends after the last sample are missed; they compare
  configurations, they are not exact totals. Request counters are the servers' trace-log
  totals (`GrvProxyMetrics`, `ProxyMetrics`, `StorageMetrics`) around the long run, per
  measured transaction; latency and throughput are the long run's.
- Iterations (short/long): 2,000/6,000 at 1 worker for reads, 6,000/18,000 at 8,
  15,000/45,000 at 32; `set100` 300/1,200, 1,000/6,000 and 1,000/15,000.
- Binary SHA-256: master `590aa97e3` `c567fac1…24db`; final source `669d35e2…11b7`;
  CGo reference (libfdb_c 7.3.77, Go 1.26.9) `82d6dd5a…2b12c`. The CGo reference's exact
  source revision was not recorded.

## Master against libfdb_c

[48 configurations](results-wire-master-vs-cgo.jsonl), three alternating runs per client.
Medians per transaction, Go / CGo:

| Workload | Server cycles | Server receive calls | Client segments | GRV requests | Storage queries | Commit batches |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `get100/8` | 97,924 / 130,140 | 1.61 / 2.06 | 0.850 / 0.561 | 0.159 / 0.256 | 1.009 / 1.008 | – |
| `get100/32` | 60,227 / 43,674 | 1.00 / 0.65 | 0.454 / 0.165 | 0.083 / 0.075 | 1.003 / 1.003 | – |
| `range100/8` | 165,122 / 156,659 | 1.81 / 1.64 | 0.887 / 0.397 | 0.212 / 0.198 | 1.010 / 1.008 | – |
| `range100/32` | 117,159 / 100,787 | 1.01 / 0.69 | 0.676 / 0.192 | 0.112 / 0.080 | 1.003 / 1.003 | – |
| `grv/8` | 47,508 / 65,920 | 0.76 / 1.07 | 0.119 / 0.220 | 0.135 / 0.179 | – | – |
| `grv/32` | 15,938 / 23,599 | 0.25 / 0.37 | 0.045 / 0.073 | 0.044 / 0.060 | – | – |
| `set100/8` | 652,175 / 376,703 | 3.40 / 1.98 | 0.879 / 0.273 | 0.281 / 0.148 | – | 0.191 / 0.126 |
| `set100/32` | 214,438 / 116,289 | 1.33 / 0.72 | 0.455 / 0.105 | 0.105 / 0.063 | – | 0.063 / 0.032 |

Both clients send the same storage queries and commits per transaction; GRV
requests differ. Go sends 1.5–4.3× libfdb_c's TCP segments on reads and writes, and
server receive calls follow. On `grv` Go sends fewer and costs less. Commit batches are
the batches the commit proxy forms (`commitBatchIn`, CommitProxyServer.actor.cpp:2547-2558),
not client requests. Throughput differs (`get100/32`: 43k transactions per second for
libfdb_c, 75k for Go), so per-transaction costs are compared at different loads.

## The write delay

[81 configurations](results-write-coalesce.jsonl) with the final source: one binary,
the delay selected with clientbench's `-coalesce-delay` (`off` is `0,0`), three runs per
arm in rotating order. Medians per transaction:

| Workload | Server cycles off / default / CGo | Default ÷ CGo | Off ÷ CGo | Client segments off / default / CGo | Mean latency µs off / default / CGo | Client cycles off / default / CGo |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `get100/1` | 501,395 / 504,054 / 507,706 | 0.99 | 0.99 | 1.826 / 2.052 / 2.043 | 221 / 279 / 404 | 358,166 / 597,590 / 670,779 |
| `get100/8` | 97,121 / 75,756 / 128,374 | 0.59 | 0.76 | 0.831 / 0.395 / 0.548 | 347 / 402 / 427 | 204,449 / 203,619 / 265,172 |
| `get100/32` | 59,331 / 38,888 / 44,234 | 0.88 | 1.34 | 0.483 / 0.180 / 0.169 | 419 / 488 / 750 | 139,615 / 146,819 / 163,196 |
| `range100/32` | 118,791 / 112,618 / 101,933 | 1.10 | 1.17 | 0.636 / 0.265 / 0.192 | 700 / 750 / 1,159 | 259,828 / 256,456 / 261,566 |
| `grv/1` | 372,871 / 373,379 / 378,672 | 0.99 | 0.98 | 1.161 / 0.989 / 1.013 | 152 / 181 / 286 | 230,470 / 348,692 / 414,755 |
| `grv/8` | 49,467 / 50,425 / 66,143 | 0.76 | 0.75 | 0.145 / 0.135 / 0.208 | 260 / 293 / 297 | 94,165 / 105,448 / 144,214 |
| `set100/1` | 2,958,114 / 3,001,479 / 3,218,079 | 0.93 | 0.92 | 2.034 / 2.042 / 2.013 | 6,487 / 6,457 / 6,497 | 573,382 / 735,212 / 855,310 |
| `set100/8` | 696,562 / 710,892 / 404,967 | 1.76 | 1.72 | 0.882 / 0.548 / 0.285 | 9,966 / 9,579 / 6,581 | 332,848 / 346,536 / 242,918 |
| `set100/32` | 221,943 / 216,182 / 184,736 | 1.17 | 1.20 | 0.451 / 0.232 / 0.157 | 11,080 / 10,960 / 9,989 | 195,553 / 196,178 / 161,063 |

Per-run default/off server-cycle ratios: `get100/8` 0.838, 0.766, 0.792; `get100/32`
0.653, 0.655, 0.690; `range100/32` 0.953, 0.955, 0.934; `grv/8` 0.915, 1.120, 1.046;
`set100/8` 0.971, 0.905, 1.689; `set100/32` 0.974, 0.954, 0.996. Three runs per arm
establish the read effect, not the size of small differences.

- **Reads:** the delay halves client segments and brings `get100` below libfdb_c's
  server cost (0.59 at 8 workers, 0.88 at 32, from 0.76 and 1.34). At 32 workers GRV
  requests also fall (0.081 → 0.057 per transaction), so not all of the gain is fewer
  socket writes. `range100` at 32 workers improves only from 1.17 to 1.10, still with
  1.4× libfdb_c's segments.
- **Writes are bimodal, for both clients.** A closed-loop round either reaches the commit
  proxy as one batch or splits. Unsplit runs form about 0.126 commit batches per
  transaction at 8 workers and 0.032 at 32, and cost about 370–420k and 110–125k server
  cycles per transaction, whichever client sent them (Go's unsplit 8-worker run in the
  master comparison: 366k cycles, 0.126 batches). Split runs form about 0.2 and 0.06 and
  cost roughly twice as much. In this campaign none of Go's 12 write runs at 8 and 32
  workers stayed unsplit (the nearest, `off-r2` at 8 workers: 481k cycles, 0.152), and
  libfdb_c split in all three 32-worker runs, which is why `set100/32` shows 1.17. In the
  earlier campaigns libfdb_c stayed unsplit in 10 of 12 such runs, Go in 2 of 18. The write delay does not
  change how often Go splits; that gap is separate (TODO.md §0, "Write gap").
- **Latency:** the wait adds to every request. Against `off`, mean latency rises 26% for
  `get100` at 1 worker (221 → 279 µs), 19% for `grv` at 1 worker, 13–16% for `grv` and
  `get100` at 8–32 workers and 7% for `range100/32`. It stays below libfdb_c's in every
  read configuration.
- **Client CPU:** at 1 worker the timer wake and the hand-off to the writer cost 28–67%
  more client cycles than `off`, still below libfdb_c's. At 8–32 workers the change is
  between −1% and +12%.

## Earlier runs

The [pre-review build](results-write-coalesce-prereview.jsonl) (`efe2ac03…f2f7`, before
the review added per-write stamping and teardown wake-up) gave the same read results in
81 configurations: `get100` default ÷ CGo 0.61 at 8 workers and 0.82 at 32, `range100/32`
1.13. An earlier prototype (`9b68f740…be153`) without the option gave the same direction
in [48 configurations](results-write-coalesce-prototype.jsonl) against master.

## Remaining transport difference

Go's writer buffers 64 KiB and its `bufio.Writer` may write a large batch in several
calls; libfdb_c writes a buffer sequence of up to 128 KiB per call
(`MAX_PACKET_SEND_BYTES`, flow/Knobs.cpp:225; Net2.actor.cpp:485-490). The delay applies
only on Linux, where a sub-millisecond timer exists; elsewhere, or if the timer cannot be
created, frames are written at once.

## Not covered

No multi-node rerun, no 64-worker configurations and no `range100` at 8 workers with
the delay.
