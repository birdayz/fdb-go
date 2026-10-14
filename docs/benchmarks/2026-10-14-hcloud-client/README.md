# 2026-10-14: client latency on a dedicated 16-vCPU host (phase A)

Pure-Go client at the final tree against its predecessor and against Apple's
Go binding over `libfdb_c` 7.3.77. One closed-loop worker, 11 workloads,
counterbalanced. This is **phase A** of the hcloud rebench campaign; the Record
Layer results are in [phase B](../2026-10-14-hcloud-recordlayer/README.md), the
remote-fetch results in [phase C1](../2026-10-14-hcloud-remotefetch/README.md),
and the remaining phases are reported separately when complete.

## Arms

| Arm | Client | Source |
|---|---|---|
| final pure-Go | `pkg/fdbgo` | `267a41434a81723b07e7150869d702636a1b91a9` |
| before pure-Go | `pkg/fdbgo` before the allocation work (after read-path parity) | `3824f14654edae8431b0085de8ebf83f5a460a08` |
| CGo | Apple's Go binding over `libfdb_c` 7.3.77, built from the final tree | `267a41434…`, native library 7.3.77 |

Executables: `//tools/clientbench/cmd/clientbench:clientbench_go` (per tree) and
`:clientbench_cgo`, harness `d94b4c61377d173b7c6884fde426a227dab0f0bb`
(`tools/clientbench`, `docs/benchmarks/campaign-harness.md`).

## Environment

- Hetzner `ccx43`, fsn1: 16 dedicated vCPUs (AMD EPYC Milan, 8 cores × 2 SMT
  threads, one NUMA node), 64 GiB. Guest CPU groups by sibling pairs:
  housekeeping/runner 0–1, `fdbserver` 2–7, client 8–15. Guest affinity is not
  physical-host isolation.
- One `fdbserver` 7.3.77 process, single redundancy, `ssd-2` engine, API 730.
  The client ran in a network namespace with no default route, reaching the
  server over a private veth pair on the same host. No artificial delay.
- Go 1.26.9, `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG`.
- Every run's quiescence gate, CPU-steal/OOM/server-health telemetry and pinned
  input hashes passed. All 11 workload campaigns completed; none was rerun.

## Design

- Per workload: 6 balanced blocks, each running all three arms once in a
  preregistered order (seed 641001; every arm order appears once). One fresh
  process per run: 200 warmup then 3,000 measured transactions, so 18,000
  measured transactions per arm and workload, 594,000 in total.
- Workloads: `grv` (read version only), `get100`/`get1000`/`get10000` (one
  point read of a 100 B/1 kB/10 kB value), `read1000` (point read in a
  read-only transaction), `range10`/`range100` (one range read of 10/100 rows),
  `set100` (one 100 B write and commit), `ryw` (write then read the same key
  in a transaction that is never committed: no server round trip),
  `batchset50`/`batchset50x1000` (50 writes of 100 B/1 kB and one commit).
- Estimands: per-run statistics; the paired effect is the geometric mean of the
  six within-block ratios, with a nominal 95% percentile bootstrap over blocks
  (20,000 resamples, seed 391706). Absolute values are medians of the six
  run-level statistics. No outliers were removed.
- CPU scopes. **Shutdown process CPU (E−A)** is the process's user+system CPU
  from after binary identity hashing to after measurement, finalizer drain,
  database close and native network stop, divided by measured transactions. It
  includes startup, fixture, warmup and validation, so it charges each client
  for work the measured window can hide (deferred GC, native threads). Go
  bytes/mallocs per transaction count the Go heap only; the native `libfdb_c`
  heap is unobserved, so they are not comparable with the CGo arm.

## Results

### Final pure-Go vs CGo

Ratio final/CGo (below 1 means pure-Go is faster or cheaper), nominal 95% interval.

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|
| grv | 0.515 [0.484, 0.545] | 0.592 [0.566, 0.625] | 0.659 [0.621, 0.692] |
| get100 | 0.651 [0.594, 0.717] | 0.675 [0.613, 0.743] | 0.948 [0.817, 1.118] |
| get1000 | 0.637 [0.598, 0.698] | 0.679 [0.627, 0.743] | 0.858 [0.810, 0.925] |
| get10000 | 0.637 [0.585, 0.700] | 0.725 [0.668, 0.776] | 0.959 [0.884, 1.046] |
| read1000 | 0.613 [0.583, 0.647] | 0.648 [0.596, 0.701] | 0.775 [0.735, 0.815] |
| range10 | 0.721 [0.618, 0.842] | 0.666 [0.628, 0.701] | 0.969 [0.834, 1.122] |
| range100 | 0.846 [0.713, 1.024] | 0.861 [0.755, 1.008] | 1.101 [0.968, 1.269] |
| set100 | 0.897 [0.873, 0.920] | 0.939 [0.845, 1.020] | 0.869 [0.841, 0.899] |
| ryw | 0.189 [0.161, 0.219] | 0.245 [0.214, 0.284] | 0.228 [0.201, 0.256] |
| batchset50 | 0.931 [0.904, 0.950] | 0.989 [0.913, 1.126] | 0.863 [0.854, 0.873] |
| batchset50x1000 | 0.918 [0.898, 0.937] | 0.942 [0.909, 0.979] | 0.887 [0.864, 0.922] |

### Final vs before pure-Go: the allocation work

Ratio final/before.

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx | Go bytes/tx | Go mallocs/tx |
|---|---:|---:|---:|---:|---:|
| grv | 1.025 [0.970, 1.079] | 1.025 [0.978, 1.079] | 1.027 [0.981, 1.063] | 0.993 [0.992, 0.994] | 0.950 [0.950, 0.950] |
| get100 | 1.021 [0.973, 1.093] | 0.960 [0.896, 1.028] | 1.002 [0.975, 1.038] | 0.929 [0.928, 0.930] | 0.874 [0.873, 0.874] |
| get1000 | 1.017 [0.989, 1.047] | 1.015 [0.922, 1.096] | 1.000 [0.976, 1.026] | 0.944 [0.943, 0.944] | 0.874 [0.873, 0.874] |
| get10000 | 0.984 [0.918, 1.055] | 0.977 [0.931, 1.023] | 1.004 [0.986, 1.022] | 0.982 [0.982, 0.983] | 0.874 [0.874, 0.875] |
| read1000 | 1.042 [0.998, 1.087] | 1.032 [0.932, 1.143] | 1.007 [0.956, 1.050] | 0.997 [0.996, 0.998] | 0.947 [0.947, 0.948] |
| range10 | 0.987 [0.915, 1.081] | 0.953 [0.898, 1.018] | 0.978 [0.939, 1.022] | 0.844 [0.844, 0.845] | 0.797 [0.796, 0.797] |
| range100 | 1.039 [0.977, 1.158] | 0.951 [0.916, 0.998] | 0.952 [0.937, 0.971] | 0.824 [0.824, 0.825] | 0.798 [0.798, 0.798] |
| set100 | 1.019 [0.995, 1.037] | 0.994 [0.881, 1.131] | 1.009 [0.987, 1.028] | 0.956 [0.954, 0.957] | 0.919 [0.918, 0.919] |
| ryw | 1.021 [0.967, 1.082] | 1.016 [0.926, 1.097] | 1.011 [0.947, 1.076] | 0.995 [0.995, 0.995] | 0.950 [0.950, 0.950] |
| batchset50 | 1.000 [0.980, 1.019] | 1.012 [0.955, 1.083] | 0.990 [0.969, 1.016] | 0.993 [0.992, 0.994] | 0.973 [0.973, 0.973] |
| batchset50x1000 | 1.011 [0.991, 1.029] | 0.992 [0.961, 1.025] | 1.002 [0.981, 1.024] | 0.998 [0.998, 0.998] | 0.973 [0.973, 0.973] |

### Absolute values

Medians of six run-level statistics per arm.

| Workload | Arm | Transactions | Mean µs | p50 µs | p99 µs | Shutdown CPU (E−A) µs/tx | Go bytes/tx | Go mallocs/tx |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| grv | final pure-Go | 18000 | 217.5 | 206.3 | 354.0 | 224.9 | 3856 | 38.1 |
| grv | before pure-Go | 18000 | 210.1 | 200.2 | 333.5 | 218.9 | 3880 | 40.1 |
| grv | CGo | 18000 | 422.6 | 410.3 | 590.9 | 338.9 | 424 | 10.0 |
| get100 | final pure-Go | 18000 | 375.5 | 368.2 | 548.2 | 424.6 | 6382 | 69.2 |
| get100 | before pure-Go | 18000 | 370.2 | 361.5 | 569.6 | 424.7 | 6873 | 79.2 |
| get100 | CGo | 18000 | 594.2 | 581.7 | 856.5 | 481.5 | 664 | 16.0 |
| get1000 | final pure-Go | 18000 | 401.8 | 391.7 | 633.0 | 455.5 | 8254 | 69.2 |
| get1000 | before pure-Go | 18000 | 394.9 | 388.2 | 619.7 | 447.8 | 8747 | 79.2 |
| get1000 | CGo | 18000 | 652.1 | 650.7 | 937.6 | 544.2 | 1583 | 16.0 |
| get10000 | final pure-Go | 18000 | 433.6 | 425.6 | 820.4 | 520.6 | 26701 | 69.5 |
| get10000 | before pure-Go | 18000 | 428.4 | 416.6 | 832.5 | 515.6 | 27177 | 79.4 |
| get10000 | CGo | 18000 | 682.4 | 676.6 | 1142.6 | 551.7 | 10796 | 16.0 |
| read1000 | final pure-Go | 18000 | 366.6 | 356.8 | 565.1 | 357.6 | 7805 | 54.1 |
| read1000 | before pure-Go | 18000 | 354.7 | 345.1 | 531.3 | 346.9 | 7832 | 57.1 |
| read1000 | CGo | 18000 | 596.0 | 583.4 | 819.7 | 460.3 | 1543 | 13.0 |
| range10 | final pure-Go | 18000 | 435.6 | 437.8 | 643.3 | 474.6 | 10503 | 74.3 |
| range10 | before pure-Go | 18000 | 456.2 | 449.9 | 682.0 | 490.5 | 12444 | 93.2 |
| range10 | CGo | 18000 | 635.5 | 629.5 | 993.9 | 502.1 | 3169 | 44.0 |
| range100 | final pure-Go | 18000 | 557.7 | 536.0 | 1103.4 | 629.5 | 50356 | 74.8 |
| range100 | before pure-Go | 18000 | 560.2 | 535.6 | 1160.8 | 653.4 | 61101 | 93.8 |
| range100 | CGo | 18000 | 729.3 | 790.6 | 1362.7 | 577.7 | 25675 | 224.0 |
| set100 | final pure-Go | 18000 | 1920.2 | 1883.1 | 2844.0 | 566.8 | 10294 | 101.3 |
| set100 | before pure-Go | 18000 | 1876.4 | 1862.5 | 2863.0 | 556.2 | 10775 | 110.2 |
| set100 | CGo | 18000 | 2140.3 | 2099.9 | 3068.7 | 661.0 | 440 | 10.0 |
| ryw | final pure-Go | 18000 | 9.7 | 5.9 | 51.6 | 23.5 | 4564 | 38.0 |
| ryw | before pure-Go | 18000 | 9.7 | 5.9 | 52.1 | 23.6 | 4587 | 40.0 |
| ryw | CGo | 18000 | 49.4 | 32.4 | 213.7 | 100.6 | 648 | 14.0 |
| batchset50 | final pure-Go | 18000 | 2368.3 | 2365.7 | 4008.6 | 881.6 | 65691 | 321.3 |
| batchset50 | before pure-Go | 18000 | 2349.1 | 2354.7 | 4181.9 | 892.8 | 66160 | 330.3 |
| batchset50 | CGo | 18000 | 2541.1 | 2575.7 | 4358.0 | 1018.1 | 1616 | 59.0 |
| batchset50x1000 | final pure-Go | 18000 | 2726.1 | 2662.7 | 4880.5 | 1278.7 | 209165 | 322.2 |
| batchset50x1000 | before pure-Go | 18000 | 2702.7 | 2626.5 | 4797.4 | 1275.9 | 209549 | 331.2 |
| batchset50x1000 | CGo | 18000 | 2974.0 | 2902.0 | 5141.6 | 1452.9 | 1616 | 59.0 |

## Interpretation

What the data shows:

- **Pure-Go was faster than CGo on every workload's mean latency**, with
  intervals below 1 everywhere except `range100` (0.846, interval reaching
  1.024). The gap is largest where the client's own work dominates:
  - `ryw` (0.189): no server round trip at all. It measures transaction
    creation, a buffered write, a read served from the read-your-writes cache,
    and cancellation. Pure-Go took 9.7 µs mean, CGo 49.4 µs.
  - `grv` (0.515): one round trip to the GRV proxy, 217 µs vs 423 µs.
  - Point reads (0.613–0.651): one GRV plus one storage read.
- **The absolute saving is about the same everywhere a round trip occurs, so
  the ratio shrinks as transactions get longer.** CGo's mean minus pure-Go's
  was 0.17–0.25 ms on every workload except `ryw` (`grv` 0.21 ms, `set100`
  0.22 ms, `batchset50x1000` 0.25 ms). Commits take 1.9–3.0 ms in total, so
  the same saving gives ratios of 0.897–0.931 there.
- **Process CPU through shutdown (E−A) favored pure-Go** on `grv` (0.659),
  `read1000` (0.775), `get1000` (0.858), the commits (0.863–0.887) and `ryw`
  (0.228). For `get100`, `get10000`, `range10` and `range100` the intervals
  include 1; `range100`'s point estimate is above 1 (1.101, interval
  0.968–1.269).
- **The allocation work cut Go allocations** (final vs before): mallocs per
  transaction fell 3–20% on every workload, and bytes up to 18% (`range100`
  0.824, `range10` 0.844). Latency did not change measurably: every
  mean-latency interval includes 1. CPU through shutdown was lower only for
  `range100` (0.952, interval 0.937–0.971).

What the design suggests but did not isolate (hypotheses, not measured
causes):

- The CGo arm's extra latency is consistent with its per-call costs: each
  operation crosses the cgo boundary, and each result is delivered by
  `libfdb_c`'s network thread through a future callback to a waiting
  goroutine. The pure-Go client issues and completes requests on goroutines
  over Go's network poller with no thread handoff. `ryw` is the cleanest view
  of this, since no network is involved; no profile attributed the time.
- Pure-Go's narrower margin and higher CPU on `range100` are consistent with
  materializing 100 key-value slices on the Go heap per read (about 50 kB and
  75 mallocs per transaction), which the C++ client keeps in its arena. Go GC
  work is charged to E−A. This was not profiled.

## Limits

- **One host, no physical network.** Server and client shared a host and
  talked over a veth pair, not a NIC, switch and real network path. A remote
  deployment adds network time that both clients pay alike, so it would most
  likely shrink these ratios toward 1; they are not a prediction for that case.
  The multi-node VPC design (`tools/benchcampaign/multinode/`) measures it.
- **One worker, one process.** Batching of read versions and commits,
  load-balancing across replicas, and cgo thread contention appear only under
  concurrency, which phase A does not test.
- **One server process, single redundancy.** No proxy/log/storage split, no
  replication, no cross-shard reads.
- **Scopes.** Latency is closed-loop per transaction, including retries (none
  occurred). E−A CPU includes startup and warmup; the Go heap figures exclude
  native memory. Intervals are nominal and per workload, with no correction for
  multiple comparisons and no aggregate across workloads.

## Reproduction

The harness, plan sealing and adapter are in `tools/clientbench` and
`tools/benchcampaign/singlenode`; `docs/benchmarks/campaign-harness.md`
describes the benchmark sources. The raw per-run artifacts (plans, schedules,
per-transaction samples, telemetry) and the analysis outputs are retained
outside the repository with the campaign's provenance receipts.
