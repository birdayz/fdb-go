# Phase M1: client latency over a real VPC to a 3-node cluster (1, 8, 32, and 64 workers)

Final pure-Go client (`pkg/fdbgo`, `267a41434a81723b07e7150869d702636a1b91a9`)
against Apple's Go binding over `libfdb_c` 7.3.77 (`CGo`) across four
closed-loop concurrency phases (`M1w1` = 1 worker, `M1w8` = 8 workers, `M1w32` =
32 workers, `M1w64` = 64 workers) over an isolated `/24` private VPC against a
3-node `double`-redundancy `ssd-2` FoundationDB 7.3.77 cluster (17 server
processes, 9 storage servers). This is **phase M1** of
[hcloud-multinode-vpc-20261014](README.md).

## Design and scope

- After `M2skip` completed, data distribution was re-enabled (`datadistribution
  on`), the cluster settled to `moving_data.in_flight_bytes == 0` and
  `in_queue_bytes == 0`, `check-roles` passed, and a 1-case `R1+R3+R4`
  diagnostic (`M1w1/grv`, 2 blocks, 32 ops) reconciled **1/1 clean** before
  timed M1 execution began.
- Sealed manifest: `plans/client/manifest.json` (SHA
  `cbbcc5481cebb1df6823f6b817d7fb49f7c783242b15709a3d97e0c1565b1735`), 6
  counterbalanced blocks per workload (all permutations of `final` and `cgo`),
  executed in the preregistered phase order `M1w1 → M1w64 → M1w8 → M1w32`:
  - `M1w1` (1 worker): 3,000 measured + 200 warmup tx/run (18,000 measured
    tx/arm/workload).
  - `M1w8` (8 workers): 12,000 measured + 800 warmup tx/run (72,000 measured
    tx/arm/workload).
  - `M1w32` (32 workers): 24,000 measured + 1,600 warmup tx/run (144,000
    measured tx/arm/workload).
  - `M1w64` (64 workers): 32,000 measured + 1,600 warmup tx/run (192,000
    measured tx/arm/workload).
- **Claim scope:** worker levels ran in a fixed phase sequence (`M1w1`,
  `M1w64`, `M1w8`, `M1w32`) with different per-level total transaction counts.
  Primary estimands are **within-worker-level paired block ratios**
  (`final / CGo`); cross-worker comparisons are descriptive only.
- **Metric scopes (same as single-node [Phase A](../hcloud-rebench-20261014/phase-a-client.md)):**
  Shutdown process CPU (`E−A`) is user+system CPU from after binary identity
  hashing through measurement, finalizer drain, database close, and native
  network stop, divided by measured transactions. Go bytes and mallocs per
  transaction count the Go heap only and exclude the unobserved native
  `libfdb_c` heap.
- **R4 reconciliation and retained adverse cases:**
  - In `M1w1`, all **7/7 workloads** completed and reconciled **clean** in R4.
  - In `M1w8`, `M1w32`, and `M1w64`, the first five workloads (`grv`, `get100`,
    `get1000`, `range100`, `set100`) completed and reconciled **clean** in R4
    (5/7 clean per phase).
  - On the sixth workload (`batchset50`, 50 writes per transaction across 8,
    32, or 64 concurrent workers), the heavy multi-worker write burst pushed the
    FDB transaction-log queue (`cluster.qos.worst_queue_bytes_log_server`) past
    the preregistered 100 MB post-run gate (`113.7 MB` in `M1w8`, `193.4 MB` in
    `M1w32`, `141.9 MB` in `M1w64`). `R1` (`client_campaign_mn.py`) aborted
    `batchset50` as `environment-aborted` (`R4` verdict: `contaminated`), and
    the phase driver conservatively stopped the phase before `batchset50x1000`
    (retained as unexecuted / `R4` verdict: `unverifiable`, rather than a
    measured negative). No partial ratios are computed for those two cases at
    8, 32, or 64 workers.

## Results: Final pure-Go / CGo

Geometric mean of six paired block ratios, with nominal 95% block-bootstrap
intervals (20,000 resamples, preregistered seed 492817; initial exploratory
analysis with seed 391706 is retained in the campaign archive). Below 1 means
pure-Go is faster or uses less CPU.

### `M1w1` (1 worker, 18,000 tx/arm per workload — 7/7 clean)

| Workload | Mean latency | p50 latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|---:|
| grv | 0.579 [0.568, 0.591] | 0.580 [0.571, 0.591] | 0.498 [0.408, 0.601] | 0.570 [0.551, 0.588] |
| get100 | 0.702 [0.690, 0.712] | 0.698 [0.691, 0.703] | 0.865 [0.734, 1.037] | 0.660 [0.648, 0.671] |
| get1000 | 0.673 [0.656, 0.691] | 0.662 [0.652, 0.673] | 0.705 [0.654, 0.779] | 0.629 [0.603, 0.653] |
| range100 | 0.633 [0.579, 0.672] | 0.627 [0.567, 0.665] | 0.715 [0.595, 0.861] | 0.747 [0.731, 0.761] |
| set100 | 0.886 [0.878, 0.894] | 0.894 [0.882, 0.906] | 0.794 [0.756, 0.837] | 0.688 [0.677, 0.700] |
| batchset50 | 0.920 [0.914, 0.926] | 0.921 [0.914, 0.928] | 0.913 [0.857, 0.975] | 0.667 [0.659, 0.673] |
| batchset50x1000 | 0.954 [0.949, 0.958] | 0.953 [0.951, 0.956] | 0.957 [0.924, 0.990] | 0.748 [0.729, 0.770] |

### `M1w8` (8 workers, 72,000 tx/arm per workload — 5/7 clean)

| Workload | Mean latency | p50 latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|---:|
| grv | 0.996 [0.949, 1.038] | 1.137 [1.116, 1.159] | 1.101 [0.869, 1.457] | 0.904 [0.878, 0.931] |
| get100 | 0.888 [0.866, 0.907] | 0.889 [0.874, 0.904] | 0.815 [0.741, 0.882] | 1.291 [1.264, 1.331] |
| get1000 | 0.942 [0.921, 0.961] | 0.946 [0.923, 0.971] | 0.931 [0.859, 0.993] | 1.362 [1.342, 1.382] |
| range100 | 0.868 [0.846, 0.891] | 0.882 [0.856, 0.909] | 0.925 [0.861, 0.987] | 1.544 [1.476, 1.651] |
| set100 | 0.980 [0.966, 0.993] | 0.998 [0.986, 1.009] | 0.916 [0.860, 0.976] | 2.142 [2.123, 2.160] |
| batchset50 | N/A (R4 `contaminated`: log queue 113.7 MB > 100 MB gate) | — | — | — |
| batchset50x1000 | N/A (R4 `unverifiable`: unexecuted after `batchset50` abort) | — | — | — |

### `M1w32` (32 workers, 144,000 tx/arm per workload — 5/7 clean)

| Workload | Mean latency | p50 latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|---:|
| grv | 1.041 [1.007, 1.081] | 1.143 [1.112, 1.177] | 0.871 [0.754, 1.017] | 0.765 [0.745, 0.779] |
| get100 | 0.770 [0.750, 0.793] | 0.777 [0.763, 0.793] | 0.815 [0.679, 0.944] | 1.368 [1.334, 1.411] |
| get1000 | 0.715 [0.688, 0.747] | 0.739 [0.719, 0.764] | 0.481 [0.423, 0.551] | 1.537 [1.494, 1.575] |
| range100 | 0.869 [0.753, 1.148] | 0.851 [0.736, 1.130] | 1.247 [1.048, 1.644] | 1.723 [1.616, 1.921] |
| set100 | 0.938 [0.931, 0.946] | 0.959 [0.954, 0.964] | 0.904 [0.856, 0.948] | 2.162 [2.108, 2.216] |
| batchset50 | N/A (R4 `contaminated`: log queue 193.4 MB > 100 MB gate) | — | — | — |
| batchset50x1000 | N/A (R4 `unverifiable`: unexecuted after `batchset50` abort) | — | — | — |

### `M1w64` (64 workers, 192,000 tx/arm per workload — 5/7 clean)

| Workload | Mean latency | p50 latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|---:|
| grv | 1.029 [0.990, 1.069] | 1.090 [1.064, 1.121] | 0.960 [0.757, 1.190] | 0.736 [0.721, 0.752] |
| get100 | 0.676 [0.644, 0.710] | 0.682 [0.649, 0.721] | 0.679 [0.601, 0.776] | 1.429 [1.385, 1.478] |
| get1000 | 0.632 [0.601, 0.669] | 0.665 [0.628, 0.710] | 0.385 [0.336, 0.438] | 1.608 [1.509, 1.705] |
| range100 | 0.554 [0.389, 0.673] | 0.532 [0.369, 0.653] | 0.732 [0.506, 0.912] | 1.069 [0.923, 1.180] |
| set100 | 0.887 [0.869, 0.897] | 0.884 [0.871, 0.893] | 0.872 [0.815, 0.927] | 1.981 [1.935, 2.021] |
| batchset50 | N/A (R4 `contaminated`: log queue 141.9 MB > 100 MB gate) | — | — | — |
| batchset50x1000 | N/A (R4 `unverifiable`: unexecuted after `batchset50` abort) | — | — | — |

## Absolute values

Medians of six run-level statistics per arm.

| Phase | Workload | Arm | Transactions | Mean µs | p50 µs | p99 µs | Shutdown CPU (E−A) µs/tx | Go bytes/tx | Go mallocs/tx |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| M1w1 | grv | final pure-Go | 18000 | 967.1 | 945.8 | 1477.4 | 260.4 | 3830 | 38.1 |
| M1w1 | grv | CGo | 18000 | 1673.5 | 1629.8 | 3133.0 | 451.0 | 424 | 10.0 |
| M1w1 | get100 | final pure-Go | 18000 | 1557.2 | 1509.6 | 3588.2 | 496.4 | 6358 | 69.2 |
| M1w1 | get100 | CGo | 18000 | 2210.4 | 2149.3 | 3967.9 | 749.8 | 664 | 16.0 |
| M1w1 | get1000 | final pure-Go | 18000 | 1494.3 | 1422.3 | 2642.5 | 503.1 | 8227 | 69.2 |
| M1w1 | get1000 | CGo | 18000 | 2223.1 | 2162.8 | 3861.8 | 799.3 | 1583 | 16.0 |
| M1w1 | range100 | final pure-Go | 18000 | 1580.8 | 1528.4 | 3000.0 | 690.5 | 49727 | 75.7 |
| M1w1 | range100 | CGo | 18000 | 2408.4 | 2318.4 | 4244.1 | 923.0 | 24078 | 224.0 |
| M1w1 | set100 | final pure-Go | 18000 | 4234.5 | 4144.4 | 6418.8 | 569.3 | 10226 | 101.4 |
| M1w1 | set100 | CGo | 18000 | 4786.2 | 4641.5 | 7991.7 | 833.1 | 440 | 10.0 |
| M1w1 | batchset50 | final pure-Go | 18000 | 5039.9 | 4927.2 | 7509.2 | 892.9 | 65571 | 321.4 |
| M1w1 | batchset50 | CGo | 18000 | 5481.1 | 5334.2 | 8253.0 | 1333.4 | 1618 | 59.0 |
| M1w1 | batchset50x1000 | final pure-Go | 18000 | 6241.4 | 6140.3 | 9383.0 | 1290.8 | 209046 | 322.3 |
| M1w1 | batchset50x1000 | CGo | 18000 | 6577.5 | 6438.4 | 10055.1 | 1737.1 | 1616 | 59.0 |
| M1w8 | grv | final pure-Go | 72000 | 1693.1 | 1861.0 | 3502.2 | 115.9 | 3549 | 35.8 |
| M1w8 | grv | CGo | 72000 | 1703.9 | 1641.6 | 3292.9 | 127.5 | 424 | 10.0 |
| M1w8 | get100 | final pure-Go | 72000 | 2083.4 | 2008.6 | 3661.8 | 242.1 | 6279 | 67.7 |
| M1w8 | get100 | CGo | 72000 | 2350.6 | 2251.1 | 4492.1 | 186.3 | 664 | 16.0 |
| M1w8 | get1000 | final pure-Go | 72000 | 2201.1 | 2128.3 | 4164.4 | 248.8 | 8141 | 67.7 |
| M1w8 | get1000 | CGo | 72000 | 2349.0 | 2268.8 | 4509.2 | 182.8 | 1577 | 16.0 |
| M1w8 | range100 | final pure-Go | 72000 | 2539.3 | 2518.7 | 4495.4 | 438.4 | 49615 | 74.5 |
| M1w8 | range100 | CGo | 72000 | 2946.1 | 2858.3 | 4943.5 | 291.0 | 24073 | 224.0 |
| M1w8 | set100 | final pure-Go | 72000 | 5007.4 | 4978.9 | 7292.3 | 421.5 | 10250 | 101.1 |
| M1w8 | set100 | CGo | 72000 | 5087.4 | 4989.9 | 7976.9 | 197.2 | 441 | 10.0 |
| M1w32 | grv | final pure-Go | 144000 | 1821.8 | 1911.8 | 3283.8 | 63.5 | 3474 | 34.6 |
| M1w32 | grv | CGo | 144000 | 1781.2 | 1681.2 | 3720.0 | 82.9 | 424 | 10.0 |
| M1w32 | get100 | final pure-Go | 144000 | 2153.0 | 2083.4 | 4115.7 | 153.5 | 6316 | 66.7 |
| M1w32 | get100 | CGo | 144000 | 2803.6 | 2682.8 | 5034.3 | 113.5 | 664 | 16.0 |
| M1w32 | get1000 | final pure-Go | 144000 | 2210.5 | 2145.1 | 3875.4 | 164.0 | 8172 | 66.7 |
| M1w32 | get1000 | CGo | 144000 | 3029.1 | 2887.8 | 7994.1 | 106.0 | 1576 | 16.0 |
| M1w32 | range100 | final pure-Go | 144000 | 2884.3 | 2722.0 | 6618.7 | 353.3 | 49547 | 73.2 |
| M1w32 | range100 | CGo | 144000 | 3792.2 | 3672.2 | 5977.7 | 215.2 | 24072 | 224.0 |
| M1w32 | set100 | final pure-Go | 144000 | 5497.2 | 5474.3 | 8003.5 | 245.2 | 10438 | 99.2 |
| M1w32 | set100 | CGo | 144000 | 5858.0 | 5717.6 | 9033.9 | 113.2 | 440 | 10.0 |
| M1w64 | grv | final pure-Go | 192000 | 2017.9 | 1995.6 | 4237.9 | 54.3 | 3464 | 34.3 |
| M1w64 | grv | CGo | 192000 | 1931.8 | 1802.1 | 4015.5 | 74.3 | 425 | 10.0 |
| M1w64 | get100 | final pure-Go | 192000 | 2229.2 | 2136.3 | 4538.2 | 132.9 | 6315 | 66.5 |
| M1w64 | get100 | CGo | 192000 | 3271.6 | 3173.4 | 6740.7 | 93.3 | 664 | 16.0 |
| M1w64 | get1000 | final pure-Go | 192000 | 2409.9 | 2308.9 | 4544.4 | 142.5 | 8168 | 66.5 |
| M1w64 | get1000 | CGo | 192000 | 3758.9 | 3459.8 | 11686.4 | 87.4 | 1576 | 16.0 |
| M1w64 | range100 | final pure-Go | 192000 | 3033.1 | 2795.8 | 7695.6 | 207.8 | 49532 | 72.8 |
| M1w64 | range100 | CGo | 192000 | 4659.5 | 4440.8 | 8704.0 | 179.1 | 24072 | 224.0 |
| M1w64 | set100 | final pure-Go | 192000 | 5918.8 | 5795.6 | 9591.9 | 203.4 | 10462 | 98.8 |
| M1w64 | set100 | CGo | 192000 | 6659.4 | 6517.0 | 10868.1 | 102.9 | 440 | 10.0 |
