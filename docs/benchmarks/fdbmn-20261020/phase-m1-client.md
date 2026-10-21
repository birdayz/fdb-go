# Phase M1: client latency and CPU at 1, 8 and 64 workers

Three client arms over a private VPC against a 3-node `double`-redundancy
`ssd-2` FoundationDB 7.3.77 cluster (17 server processes, 9 storage servers),
in three closed-loop concurrency phases: `M1w1` (1 worker), `M1w8`
(8 workers) and `M1w64` (64 workers). This is **phase M1** of
[fdbmn-20261020](README.md).

| Arm | Tree | Client |
|---|---|---|
| master | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | Pure-Go client (`pkg/fdbgo`) |
| old Go | `267a41434a81723b07e7150869d702636a1b91a9` | Pure-Go client, the final tree of the [2026-10-14 campaign](../hcloud-multinode-vpc-20261014/phase-m1-client.md) |
| CGo | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | Apple's Go binding over hash-pinned `libfdb_c` 7.3.77, API 730 |

## Design and scope

- After M2, data distribution was re-enabled and the cluster held zero data
  movement for 121.474 s, with stable roles and CPU pins, before `M1w1`.
- Phases ran in the preregistered order `M1w1 → M1w64 → M1w8`. `M1w32` was
  dropped for budget before measurement and was never replaced with capacity
  data.
- Each workload runs six blocks, one for each order of the three arms: 18 fresh
  processes per workload. Transaction counts are totals across workers:
  - `M1w1`: 3,000 measured + 200 warmup per run (18,000 measured per arm and
    workload).
  - `M1w8`: 12,000 + 800 per run (72,000 per arm and workload).
  - `M1w64`: 32,000 + 1,600 per run (192,000 per arm and workload).
- Workload order at every worker count: `grv`, `get100`, `get1000`,
  `range100`, `set100`, `batchset50`, `batchset50x1000`. Per-process 180 s
  and per-workload 900 s caps.
- **Claim scope:** each ratio is a within-worker-level paired contrast. Worker
  levels ran in a fixed sequence with different transaction counts, so they are
  not compared with each other.
- **Metric scopes:** lifecycle CPU (E−A) is process user+system CPU from after
  binary identity hashing through measurement, finalizer drain, database close
  and native network stop, divided by measured transactions. Go bytes and
  mallocs per transaction count the Go heap only and exclude the native
  `libfdb_c` heap, so CGo's small Go counts do not describe its memory use.
- **Harness:** all three arms use the identical amended harness archive
  (`d94b4c613` plus the #948 lifecycle-drain fix and matching validator).
  Workloads, timing windows, pacing, CPU accounting and estimators are those of
  the frozen harness.
- **Polling change:** from `M1w8` on, the controller reused one SSH connection
  and polled every 60 s instead of 10 s. `M1w64` ran entirely under the original
  controller.

## Exclusions

- **`M1w1`:** 4 of 7 workloads clean. Before set100's first arm, the client
  pre-arm gate measured 17.17% busy on one CPU, above the 15% limit (aggregate
  4.01%, no steal). The arm was not launched, and the preregistered
  stop-on-failure policy ended the phase before `batchset50` and
  `batchset50x1000`. All three write comparisons are N/A.
- **`M1w64`:** 5 of 7 clean. After the first master `batchset50` arm, the
  server's transaction-log queue was 426,278,385 bytes, above the 100 MB gate.
  That arm completed 32,000 of 32,000 commits with no errors or retries and
  passed validation; the whole comparison is still excluded. `batchset50x1000`
  was not reached.
- **`M1w8`:** 5 of 7 clean. The log queue was 118,497,454 bytes after the first
  master `batchset50` arm. `batchset50x1000` was not reached.
- No case was rerun or partially salvaged.

## Results

Geometric mean of six paired block ratios, with nominal 95% block-bootstrap
intervals (20,000 resamples, preregistered seed 492817). Below 1 means the
numerator has lower latency, CPU or allocation.

On Linux, master's pure-Go client coalesces outgoing requests before a write
batch, using the same 10 µs minimum and 20 µs maximum delay settings as
`libfdb_c` 7.3.77. The delay depends on time since the last write and is
jittered. [`fdb.WithCoalesceDelay(minDelay, maxDelay time.Duration)`](../../../pkg/fdbgo/fdb/database.go)
changes those settings.

### `M1w1` (1 worker, 18,000 tx/arm per workload — 4/7 clean)

#### `M1w1`: master / old Go

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx | Go bytes per tx | Go mallocs per tx |
|---|---:|---:|---:|---:|---:|---:|
| grv | 1.046 [1.002, 1.076] | 1.049 [1.006, 1.080] | 1.073 [0.927, 1.245] | 1.292 [1.255, 1.330] | 0.879 [0.878, 0.881] | 0.790 [0.790, 0.791] |
| get100 | 1.094 [1.062, 1.133] | 1.088 [1.053, 1.128] | 1.108 [0.984, 1.242] | 1.190 [1.166, 1.211] | 0.924 [0.924, 0.925] | 0.785 [0.785, 0.785] |
| get1000 | 1.088 [1.071, 1.103] | 1.091 [1.082, 1.099] | 1.121 [0.965, 1.302] | 1.186 [1.166, 1.205] | 0.941 [0.941, 0.942] | 0.785 [0.785, 0.785] |
| range100 | 1.077 [1.062, 1.091] | 1.080 [1.064, 1.092] | 1.106 [1.021, 1.202] | 1.125 [1.108, 1.138] | 0.891 [0.891, 0.892] | 0.797 [0.796, 0.797] |
| set100 | N/A (pre-arm client CPU gate) | — | — | — | — | — |
| batchset50 | N/A (not reached) | — | — | — | — | — |
| batchset50x1000 | N/A (not reached) | — | — | — | — | — |

#### `M1w1`: master / CGo

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx |
|---|---:|---:|---:|---:|
| grv | 0.644 [0.622, 0.657] | 0.636 [0.616, 0.649] | 0.713 [0.675, 0.762] | 0.783 [0.768, 0.797] |
| get100 | 0.681 [0.664, 0.699] | 0.675 [0.661, 0.690] | 0.797 [0.735, 0.902] | 0.806 [0.779, 0.833] |
| get1000 | 0.677 [0.671, 0.683] | 0.673 [0.666, 0.680] | 0.756 [0.718, 0.801] | 0.800 [0.783, 0.821] |
| range100 | 0.708 [0.655, 0.745] | 0.697 [0.636, 0.738] | 0.795 [0.715, 0.881] | 0.908 [0.893, 0.921] |

#### `M1w1`: old Go / CGo

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx |
|---|---:|---:|---:|---:|
| grv | 0.615 [0.606, 0.625] | 0.607 [0.600, 0.613] | 0.665 [0.572, 0.759] | 0.606 [0.581, 0.629] |
| get100 | 0.622 [0.610, 0.636] | 0.621 [0.608, 0.634] | 0.720 [0.633, 0.827] | 0.678 [0.666, 0.689] |
| get1000 | 0.622 [0.611, 0.631] | 0.617 [0.608, 0.626] | 0.674 [0.596, 0.769] | 0.674 [0.667, 0.683] |
| range100 | 0.658 [0.607, 0.689] | 0.645 [0.587, 0.679] | 0.719 [0.634, 0.817] | 0.807 [0.788, 0.826] |

### `M1w8` (8 workers, 72,000 tx/arm per workload — 5/7 clean)

#### `M1w8`: master / old Go

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx | Go bytes per tx | Go mallocs per tx |
|---|---:|---:|---:|---:|---:|---:|
| grv | 1.034 [1.023, 1.047] | 0.941 [0.930, 0.954] | 0.945 [0.895, 1.001] | 0.817 [0.792, 0.843] | 0.837 [0.836, 0.839] | 0.777 [0.776, 0.777] |
| get100 | 1.110 [1.090, 1.133] | 1.113 [1.097, 1.128] | 1.067 [0.919, 1.244] | 0.762 [0.750, 0.774] | 0.870 [0.869, 0.870] | 0.766 [0.766, 0.766] |
| get1000 | 1.147 [1.136, 1.157] | 1.154 [1.139, 1.172] | 1.161 [1.024, 1.286] | 0.762 [0.745, 0.780] | 0.900 [0.899, 0.901] | 0.767 [0.767, 0.767] |
| range100 | 1.118 [1.101, 1.139] | 1.112 [1.096, 1.129] | 1.056 [0.950, 1.191] | 0.865 [0.801, 0.977] | 0.894 [0.894, 0.895] | 0.809 [0.805, 0.815] |
| set100 | 1.178 [1.138, 1.217] | 1.137 [1.099, 1.168] | 1.109 [1.047, 1.163] | 0.553 [0.542, 0.565] | 0.629 [0.628, 0.630] | 0.555 [0.554, 0.555] |
| batchset50 | N/A (log queue 118,497,454 bytes > 100 MB gate) | — | — | — | — | — |
| batchset50x1000 | N/A (not reached) | — | — | — | — | — |

#### `M1w8`: master / CGo

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx |
|---|---:|---:|---:|---:|
| grv | 1.021 [1.002, 1.042] | 1.025 [1.007, 1.045] | 0.996 [0.865, 1.157] | 0.728 [0.715, 0.740] |
| get100 | 0.974 [0.953, 0.998] | 0.977 [0.961, 0.992] | 0.972 [0.866, 1.108] | 1.037 [1.027, 1.046] |
| get1000 | 0.988 [0.966, 1.017] | 1.003 [0.986, 1.021] | 0.924 [0.784, 1.083] | 1.133 [1.096, 1.182] |
| range100 | 0.968 [0.949, 0.988] | 0.966 [0.949, 0.983] | 1.083 [0.983, 1.207] | 1.354 [1.268, 1.513] |
| set100 | 1.010 [0.974, 1.046] | 1.013 [0.978, 1.047] | 0.936 [0.867, 1.000] | 1.148 [1.119, 1.179] |

The set100 p99 upper bound rounds to 1.000; unrounded it is 0.99954.

#### `M1w8`: old Go / CGo

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx |
|---|---:|---:|---:|---:|
| grv | 0.987 [0.970, 1.010] | 1.090 [1.069, 1.117] | 1.054 [0.896, 1.236] | 0.891 [0.856, 0.924] |
| get100 | 0.877 [0.871, 0.884] | 0.878 [0.873, 0.882] | 0.911 [0.853, 0.993] | 1.361 [1.336, 1.384] |
| get1000 | 0.861 [0.840, 0.887] | 0.869 [0.849, 0.890] | 0.796 [0.687, 0.926] | 1.488 [1.445, 1.537] |
| range100 | 0.866 [0.857, 0.874] | 0.869 [0.861, 0.876] | 1.025 [0.975, 1.076] | 1.566 [1.541, 1.595] |
| set100 | 0.858 [0.840, 0.883] | 0.891 [0.870, 0.921] | 0.844 [0.803, 0.879] | 2.075 [2.033, 2.106] |

### `M1w64` (64 workers, 192,000 tx/arm per workload — 5/7 clean)

#### `M1w64`: master / old Go

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx | Go bytes per tx | Go mallocs per tx |
|---|---:|---:|---:|---:|---:|---:|
| grv | 0.956 [0.937, 0.974] | 0.923 [0.907, 0.938] | 0.923 [0.813, 1.036] | 0.707 [0.692, 0.720] | 0.831 [0.830, 0.831] | 0.790 [0.790, 0.790] |
| get100 | 1.310 [1.273, 1.354] | 1.348 [1.308, 1.395] | 1.125 [1.057, 1.198] | 0.738 [0.719, 0.759] | 0.850 [0.850, 0.851] | 0.771 [0.771, 0.771] |
| get1000 | 1.261 [1.231, 1.290] | 1.298 [1.254, 1.331] | 1.077 [1.002, 1.150] | 0.742 [0.716, 0.771] | 0.887 [0.886, 0.888] | 0.772 [0.772, 0.772] |
| range100 | 1.393 [1.079, 2.279] | 1.435 [1.099, 2.378] | 1.134 [0.875, 1.860] | 1.180 [0.999, 1.567] | 0.901 [0.893, 0.918] | 0.811 [0.806, 0.820] |
| set100 | 1.265 [1.253, 1.277] | 1.283 [1.273, 1.294] | 1.110 [1.055, 1.190] | 0.586 [0.572, 0.599] | 0.612 [0.612, 0.612] | 0.561 [0.561, 0.561] |
| batchset50 | N/A (log queue 426,278,385 bytes > 100 MB gate) | — | — | — | — | — |
| batchset50x1000 | N/A (not reached) | — | — | — | — | — |

Range100 at 64 workers is unstable: its master/old intervals are wide in every
metric. Per-order, half-split, moving-block and leave-one-block-out
diagnostics for every estimate are in
[`analysis/M1-comparisons.csv`](analysis/M1-comparisons.csv).

#### `M1w64`: master / CGo

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx |
|---|---:|---:|---:|---:|
| grv | 0.975 [0.927, 1.024] | 1.001 [0.970, 1.040] | 0.948 [0.772, 1.131] | 0.565 [0.550, 0.581] |
| get100 | 0.877 [0.849, 0.910] | 0.934 [0.911, 0.966] | 0.773 [0.693, 0.853] | 1.135 [1.082, 1.203] |
| get1000 | 0.728 [0.693, 0.770] | 0.770 [0.730, 0.821] | 0.395 [0.336, 0.494] | 1.156 [1.111, 1.197] |
| range100 | 0.858 [0.655, 1.438] | 0.867 [0.656, 1.480] | 0.869 [0.665, 1.453] | 1.275 [1.061, 1.731] |
| set100 | 0.902 [0.896, 0.909] | 0.893 [0.887, 0.899] | 0.900 [0.835, 0.989] | 1.249 [1.213, 1.284] |

#### `M1w64`: old Go / CGo

| Workload | Mean latency | p50 latency | p99 latency | E−A CPU per tx |
|---|---:|---:|---:|---:|
| grv | 1.020 [0.983, 1.070] | 1.085 [1.059, 1.128] | 1.028 [0.893, 1.186] | 0.799 [0.776, 0.825] |
| get100 | 0.670 [0.647, 0.689] | 0.693 [0.673, 0.713] | 0.688 [0.614, 0.770] | 1.537 [1.471, 1.605] |
| get1000 | 0.577 [0.549, 0.610] | 0.593 [0.563, 0.630] | 0.367 [0.308, 0.446] | 1.558 [1.526, 1.600] |
| range100 | 0.616 [0.602, 0.633] | 0.604 [0.591, 0.623] | 0.767 [0.745, 0.790] | 1.081 [1.060, 1.106] |
| set100 | 0.713 [0.707, 0.719] | 0.696 [0.689, 0.703] | 0.810 [0.784, 0.836] | 2.132 [2.088, 2.183] |

p99.9 is in the detailed outputs but is not reported here: six blocks are too
few for it.

## Absolute values

Medians of six run-level statistics per arm. These are not quantiles of pooled
transactions.

| Phase | Workload | Arm | Transactions | Mean µs | p50 µs | p99 µs | E−A CPU µs/tx | Go bytes/tx | Go mallocs/tx |
|---|---|---|---:|---:|---:|---:|---:|---:|---:|
| M1w1 | grv | old Go | 18000 | 1010.6 | 984.3 | 1577.8 | 288.1 | 3,834 | 38.1 |
| M1w1 | grv | master | 18000 | 1078.3 | 1049.6 | 1835.6 | 371.7 | 3,370 | 30.1 |
| M1w1 | grv | CGo | 18000 | 1645.9 | 1620.6 | 2504.6 | 465.3 | 424 | 10.0 |
| M1w1 | get100 | old Go | 18000 | 1404.7 | 1369.5 | 2529.5 | 550.6 | 6,367 | 69.2 |
| M1w1 | get100 | master | 18000 | 1531.2 | 1491.4 | 2690.6 | 665.1 | 5,887 | 54.3 |
| M1w1 | get100 | CGo | 18000 | 2262.2 | 2218.2 | 3586.7 | 816.6 | 664 | 16.0 |
| M1w1 | get1000 | old Go | 18000 | 1429.5 | 1384.3 | 2374.5 | 570.7 | 8,234 | 69.2 |
| M1w1 | get1000 | master | 18000 | 1539.1 | 1502.6 | 2785.0 | 675.4 | 7,750 | 54.3 |
| M1w1 | get1000 | CGo | 18000 | 2286.7 | 2238.5 | 3670.4 | 848.8 | 1,583 | 16.0 |
| M1w1 | range100 | old Go | 18000 | 1684.3 | 1622.4 | 2915.8 | 772.9 | 50,285 | 75.7 |
| M1w1 | range100 | master | 18000 | 1812.1 | 1755.6 | 3243.3 | 865.7 | 44,835 | 60.3 |
| M1w1 | range100 | CGo | 18000 | 2482.6 | 2420.7 | 3911.9 | 950.1 | 25,676 | 224.0 |
| M1w8 | grv | old Go | 72000 | 1706.3 | 1848.9 | 2937.6 | 127.5 | 3,550 | 35.8 |
| M1w8 | grv | master | 72000 | 1772.1 | 1745.5 | 2733.4 | 103.1 | 2,973 | 27.8 |
| M1w8 | grv | CGo | 72000 | 1738.8 | 1704.8 | 2890.4 | 142.0 | 425 | 10.0 |
| M1w8 | get100 | old Go | 72000 | 2109.8 | 2058.8 | 3597.4 | 278.3 | 6,265 | 67.7 |
| M1w8 | get100 | master | 72000 | 2354.0 | 2317.0 | 3663.5 | 211.3 | 5,449 | 51.9 |
| M1w8 | get100 | CGo | 72000 | 2418.2 | 2354.6 | 3978.9 | 204.3 | 664 | 16.0 |
| M1w8 | get1000 | old Go | 72000 | 2117.9 | 2078.9 | 3287.3 | 291.8 | 8,129 | 67.7 |
| M1w8 | get1000 | master | 72000 | 2436.3 | 2400.3 | 3954.8 | 220.6 | 7,319 | 51.9 |
| M1w8 | get1000 | CGo | 72000 | 2503.2 | 2413.0 | 4166.0 | 197.8 | 1,577 | 16.0 |
| M1w8 | range100 | old Go | 72000 | 2675.9 | 2638.5 | 4681.3 | 511.0 | 50,147 | 74.5 |
| M1w8 | range100 | master | 72000 | 2965.1 | 2911.2 | 4832.3 | 422.1 | 44,849 | 60.0 |
| M1w8 | range100 | CGo | 72000 | 3074.6 | 3020.2 | 4714.7 | 328.2 | 25,673 | 224.0 |
| M1w8 | set100 | old Go | 72000 | 3769.2 | 3844.9 | 5662.3 | 436.3 | 10,295 | 100.8 |
| M1w8 | set100 | master | 72000 | 4485.2 | 4430.7 | 6440.9 | 236.3 | 6,479 | 55.9 |
| M1w8 | set100 | CGo | 72000 | 4409.2 | 4357.0 | 6731.7 | 208.1 | 441 | 10.0 |
| M1w64 | grv | old Go | 192000 | 2035.0 | 2054.9 | 3844.9 | 64.9 | 3,467 | 34.3 |
| M1w64 | grv | master | 192000 | 1946.0 | 1884.8 | 3418.3 | 45.7 | 2,879 | 27.1 |
| M1w64 | grv | CGo | 192000 | 1996.7 | 1909.2 | 3612.3 | 80.2 | 424 | 10.0 |
| M1w64 | get100 | old Go | 192000 | 2302.2 | 2216.8 | 4230.2 | 153.2 | 6,317 | 66.5 |
| M1w64 | get100 | master | 192000 | 2961.3 | 2922.1 | 4812.1 | 111.3 | 5,371 | 51.3 |
| M1w64 | get100 | CGo | 192000 | 3416.5 | 3195.9 | 6104.8 | 99.0 | 664 | 16.0 |
| M1w64 | get1000 | old Go | 192000 | 2463.7 | 2350.7 | 4753.0 | 161.3 | 8,170 | 66.5 |
| M1w64 | get1000 | master | 192000 | 3062.2 | 3039.8 | 5019.0 | 120.5 | 7,250 | 51.4 |
| M1w64 | get1000 | CGo | 192000 | 4228.2 | 3964.5 | 13230.4 | 103.5 | 1,576 | 16.0 |
| M1w64 | range100 | old Go | 192000 | 3336.1 | 3101.4 | 7527.6 | 226.4 | 50,068 | 72.8 |
| M1w64 | range100 | master | 192000 | 3597.4 | 3425.4 | 6721.9 | 237.6 | 44,704 | 58.7 |
| M1w64 | range100 | CGo | 192000 | 5402.1 | 5130.3 | 9909.4 | 210.2 | 25,672 | 224.0 |
| M1w64 | set100 | old Go | 192000 | 4303.9 | 4186.4 | 7382.2 | 232.2 | 10,491 | 98.8 |
| M1w64 | set100 | master | 192000 | 5470.9 | 5383.5 | 8122.7 | 134.7 | 6,419 | 55.4 |
| M1w64 | set100 | CGo | 192000 | 6055.6 | 5993.7 | 9176.0 | 107.8 | 440 | 10.0 |

## Comparison with the 2026-10-14 campaign

Old Go is the revision the 2026-10-14 campaign measured as its final pure-Go
tree. Its ratio to each campaign's own CGo arm shows how much the two cohorts
differ before any code change is considered: fresh hosts, a three-arm schedule,
the harness amendment and different times. Blocks are never pooled across
campaigns, and no cross-campaign interval or ratio of ratios is computed.

### Old Go / CGo in both campaigns

| Case | p50, 2026-10-14 | p50, 2026-10-20 | p99, 2026-10-14 | p99, 2026-10-20 | E−A CPU, 2026-10-14 | E−A CPU, 2026-10-20 |
|---|---:|---:|---:|---:|---:|---:|
| w1/grv | 0.580 [0.571, 0.591] | 0.607 [0.600, 0.613] | 0.498 [0.408, 0.601] | 0.665 [0.572, 0.759] | 0.570 [0.551, 0.588] | 0.606 [0.581, 0.629] |
| w1/get100 | 0.698 [0.691, 0.703] | 0.621 [0.608, 0.634] | 0.865 [0.734, 1.037] | 0.720 [0.633, 0.827] | 0.660 [0.648, 0.671] | 0.678 [0.666, 0.689] |
| w1/get1000 | 0.662 [0.652, 0.673] | 0.617 [0.608, 0.626] | 0.705 [0.654, 0.779] | 0.674 [0.596, 0.769] | 0.629 [0.603, 0.653] | 0.674 [0.667, 0.683] |
| w1/range100 | 0.627 [0.567, 0.665] | 0.645 [0.587, 0.679] | 0.715 [0.595, 0.861] | 0.719 [0.634, 0.817] | 0.747 [0.731, 0.761] | 0.807 [0.788, 0.826] |
| w8/grv | 1.137 [1.116, 1.159] | 1.090 [1.069, 1.117] | 1.101 [0.869, 1.457] | 1.054 [0.896, 1.236] | 0.904 [0.878, 0.931] | 0.891 [0.856, 0.924] |
| w8/get100 | 0.889 [0.874, 0.904] | 0.878 [0.873, 0.882] | 0.815 [0.741, 0.882] | 0.911 [0.853, 0.993] | 1.291 [1.264, 1.331] | 1.361 [1.336, 1.384] |
| w8/get1000 | 0.946 [0.923, 0.971] | 0.869 [0.849, 0.890] | 0.931 [0.859, 0.993] | 0.796 [0.687, 0.926] | 1.362 [1.342, 1.382] | 1.488 [1.445, 1.537] |
| w8/range100 | 0.882 [0.856, 0.909] | 0.869 [0.861, 0.876] | 0.925 [0.861, 0.987] | 1.025 [0.975, 1.076] | 1.544 [1.476, 1.651] | 1.566 [1.541, 1.595] |
| w8/set100 | 0.998 [0.986, 1.009] | 0.891 [0.870, 0.921] | 0.916 [0.860, 0.976] | 0.844 [0.803, 0.879] | 2.142 [2.123, 2.160] | 2.075 [2.033, 2.106] |
| w64/grv | 1.090 [1.064, 1.121] | 1.085 [1.059, 1.128] | 0.960 [0.757, 1.190] | 1.028 [0.893, 1.186] | 0.736 [0.721, 0.752] | 0.799 [0.776, 0.825] |
| w64/get100 | 0.682 [0.649, 0.721] | 0.693 [0.673, 0.713] | 0.679 [0.601, 0.776] | 0.688 [0.614, 0.770] | 1.429 [1.385, 1.478] | 1.537 [1.471, 1.605] |
| w64/get1000 | 0.665 [0.628, 0.710] | 0.593 [0.563, 0.630] | 0.385 [0.336, 0.438] | 0.367 [0.308, 0.446] | 1.608 [1.509, 1.705] | 1.558 [1.526, 1.600] |
| w64/range100 | 0.532 [0.369, 0.653] | 0.604 [0.591, 0.623] | 0.732 [0.506, 0.912] | 0.767 [0.745, 0.790] | 1.069 [0.923, 1.180] | 1.081 [1.060, 1.106] |
| w64/set100 | 0.884 [0.871, 0.893] | 0.696 [0.689, 0.703] | 0.872 [0.815, 0.927] | 0.810 [0.784, 0.836] | 1.981 [1.935, 2.021] | 2.132 [2.088, 2.183] |

- The same revision's CPU ratio to CGo is similar in both cohorts. For set100,
  old Go/CGo E−A CPU was 2.142 (8 workers) and 1.981 (64 workers) on
  2026-10-14, and is 2.075 and 2.132 now. Master/CGo is 1.148 and 1.249. That
  describes a smaller Go-versus-CGo CPU gap; the reported comparison remains
  the directly paired master/old contrast.
- Latency ratios to CGo vary more between cohorts, notably set100 p50 at
  64 workers (0.884 then, 0.696 now). Cross-campaign differences in absolute
  latency are not attributed to code.
