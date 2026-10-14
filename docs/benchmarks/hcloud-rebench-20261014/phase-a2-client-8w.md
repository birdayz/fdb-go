# Phase A2: client latency at 8 closed-loop workers

Pure-Go client at the final tree against its predecessor and against Apple's
Go binding over `libfdb_c` 7.3.77, at **8 concurrent closed-loop workers** on a
dedicated 16-vCPU host (`ssd-2` engine). This is **phase A2** of
[hcloud-rebench-20261014](README.md).

## Arms

| Arm | Client | Source |
|---|---|---|
| final pure-Go | `pkg/fdbgo` | `267a41434a81723b07e7150869d702636a1b91a9` |
| before pure-Go | `pkg/fdbgo` before the allocation work (after read-path parity) | `3824f14654edae8431b0085de8ebf83f5a460a08` |
| CGo | Apple's Go binding over `libfdb_c` 7.3.77, built from the final tree | `267a41434…`, native library 7.3.77 |

## Design and Scope

- Per workload (`get100`, `range100`, `batchset50`): 6 balanced blocks, each
  running all three arms once in a preregistered order (seed 641601; every arm
  order appears once).
- Each run spawns **8 closed-loop workers** in a fresh process: 1,600 warmup
  operations (200 per worker) followed by 24,000 measured operations (3,000 per
  worker), giving **144,000 measured transactions per arm and workload**
  (1,296,000 measured transactions across the phase).
- All 54 scheduled runs completed with zero errors and passed all pre-arm and
  during-arm quiescence, CPU-steal, OOM, server-health, and pin checks.
- **Metric scopes (same as [Phase A](phase-a-client.md)):** Shutdown process
  CPU (E−A) is user+system CPU from after binary identity hashing to after
  measurement, finalizer drain, database close, and native network stop,
  divided by measured transactions; it includes startup, fixture, warmup, and
  validation. Go bytes and mallocs per transaction measure the Go heap only and
  exclude the unobserved native `libfdb_c` heap.
- **Within-phase interpretation:** A2 ran on a fresh `ssd-2` cluster after the
  memory-engine phases; its cluster status reported `tenant_mode: disabled`,
  whereas phase A's initial SSD cluster reported `optional_experimental`
  (neither phase used tenants). Because phase A (1 worker) and phase A2 (8
  workers) ran in separate phases rather than counterbalanced worker blocks,
  the primary estimands here are **within-8-worker arm contrasts**.

## Results

### Final pure-Go vs CGo (8 workers)

Ratio final/CGo (below 1 means pure-Go is faster or cheaper), nominal 95%
paired-block bootstrap interval (20,000 resamples, seed 391706).

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|
| get100 | 0.991 [0.928, 1.084] | 1.015 [0.938, 1.097] | 1.638 [1.603, 1.676] |
| range100 | 0.663 [0.440, 0.858] | 0.881 [0.696, 1.033] | 1.359 [1.138, 1.559] |
| batchset50 | 0.817 [0.768, 0.851] | 0.914 [0.816, 0.985] | 1.150 [1.112, 1.188] |

### Final vs before pure-Go (8 workers): the allocation work

Ratio final/before.

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx | Go bytes/tx | Go mallocs/tx |
|---|---:|---:|---:|---:|---:|
| get100 | 1.007 [0.997, 1.018] | 1.028 [0.960, 1.090] | 0.972 [0.959, 0.986] | 0.929 [0.929, 0.929] | 0.872 [0.872, 0.872] |
| range100 | 1.069 [0.948, 1.206] | 0.901 [0.829, 0.982] | 1.041 [0.926, 1.171] | 0.824 [0.824, 0.825] | 0.796 [0.796, 0.797] |
| batchset50 | 0.995 [0.950, 1.022] | 0.995 [0.941, 1.045] | 0.991 [0.971, 1.010] | 0.993 [0.993, 0.994] | 0.973 [0.973, 0.973] |

### Absolute values (8 workers)

Medians of six run-level statistics per arm.

| Workload | Arm | Transactions | Mean µs | p50 µs | p99 µs | Shutdown CPU (E−A) µs/tx | Go bytes/tx | Go mallocs/tx |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| get100 | final pure-Go | 144000 | 549.8 | 536.4 | 1065.7 | 202.5 | 6385 | 68.4 |
| get100 | before pure-Go | 144000 | 544.0 | 532.4 | 1019.7 | 208.0 | 6873 | 78.5 |
| get100 | CGo | 144000 | 566.7 | 533.5 | 1091.6 | 124.0 | 664 | 16.0 |
| range100 | final pure-Go | 144000 | 615.6 | 550.2 | 1941.7 | 312.2 | 50147 | 73.9 |
| range100 | before pure-Go | 144000 | 563.4 | 486.2 | 2127.8 | 292.1 | 60829 | 92.8 |
| range100 | CGo | 144000 | 808.5 | 734.6 | 2072.8 | 220.3 | 25672 | 224.0 |
| batchset50 | final pure-Go | 144000 | 4080.2 | 3860.7 | 7941.3 | 684.0 | 73922 | 321.0 |
| batchset50 | before pure-Go | 144000 | 4067.0 | 3925.7 | 7784.8 | 689.3 | 74415 | 330.0 |
| batchset50 | CGo | 144000 | 4904.5 | 4882.2 | 8262.7 | 589.0 | 1616 | 59.0 |

## Interpretation

- **Latency at 8 workers:**
  - On `range100`, pure-Go had lower mean latency than CGo (0.663 [0.440, 0.858], 615.6 µs vs 808.5 µs), while the p99 interval included 1 (0.881 [0.696, 1.033]).
  - On `batchset50`, pure-Go had lower mean latency (0.817 [0.768, 0.851], 4080.2 µs vs 4904.5 µs) and lower p99 latency (0.914 [0.816, 0.985]).
  - On `get100`, the nominal latency intervals include 1 (mean 0.991 [0.928,
    1.084], p99 1.015 [0.938, 1.097]); no clear difference is resolved in this
    bounded sample, and this design does not test equivalence.
- **Process CPU at 8 workers:** unlike at 1 worker, **shutdown process CPU (E−A) per transaction was higher for pure-Go than for CGo on all three workloads at 8 workers**: 1.638 [1.603, 1.676] on `get100`, 1.359 [1.138, 1.559] on `range100`, and 1.150 [1.112, 1.188] on `batchset50`.
  - *Untested hypothesis:* `libfdb_c` multiplexes all 8 Go workers onto its single C++ network thread (`GOMAXPROCS=8` on the Go side vs one native network thread), whereas the pure-Go client runs concurrent goroutines, serialization, and Go GC across up to 8 OS threads. Without CPU profiles, the exact split between Go runtime scheduling, GC, and socket I/O is unmeasured.
- **Final vs before pure-Go:** Go bytes and mallocs per transaction fell by the same proportions as in phase A (`get100` −7.1% B/tx, −12.8% mallocs/tx; `range100` −17.6% B/tx, −20.4% mallocs/tx; `batchset50` −0.7% B/tx, −2.7% mallocs/tx), with `get100` shutdown CPU 0.972 [0.959, 0.986] and `range100` p99 0.901 [0.829, 0.982].
