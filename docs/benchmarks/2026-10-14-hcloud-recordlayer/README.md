# 2026-10-14: Record Layer on a dedicated 16-vCPU host (phase B)

Eight `pkg/recordlayer` benchmarks in three trees, counterbalanced. This
separates two changes: the read-path parity work (record loads switched to
Java's whole-record range reads) and the allocation work that followed it.

## Arms

| Arm | Source | What it contains |
|---|---|---|
| legacy | `9a9a448670a748e4ea721772ba96d70ccc4210a0` | The tree behind the published 2026-10-12 numbers |
| before | `3824f14654edae8431b0085de8ebf83f5a460a08` | legacy plus the read-path parity change (#888); nothing else in between changes code |
| final | `267a41434a81723b07e7150869d702636a1b91a9` | before plus the allocation work and everything merged since |

All three trees ran byte-identical benchmark source
(`pkg/recordlayer/benchmark_test.go`, harness
`d94b4c61377d173b7c6884fde426a227dab0f0bb`; see
[campaign-harness.md](../campaign-harness.md)), so differences come from the
production code. `final/before` is the allocation work plus later changes;
`final/legacy` is both changes together.

## Environment and design

- The host and CPU groups are those of the
  [phase A client run](../2026-10-14-hcloud-client/README.md): Hetzner `ccx43`,
  `fdbserver` on CPUs 2–7, the benchmark process on 8–15 in a network namespace
  reaching the server over a veth pair. Here the single `fdbserver` 7.3.77
  process used the `memory` engine, single redundancy.
- Per case: 6 balanced blocks over the three arms (seed 641101, every order
  once). One fresh process per arm and block, running the benchmark three times
  (`-test.count=3`): read benchmarks for 3 s each, write benchmarks for exactly
  1,000 operations (`SaveRecord`, `SaveRecordWithIndex`, `SaveSplitRecord`
  insert IDs 0–999 into a freshly cleared subspace). 18 samples per arm and
  case, 432 in total. The cluster's user keys were cleared before every
  process.
- Estimand: the geometric mean over blocks of each block's arm ratio, using
  each process's median of its three repeats, with a nominal 95% percentile
  bootstrap over blocks (20,000 resamples, seed 391706). Absolute values are
  the median over blocks of those process medians.
- `ns/op` is testing.B's mean time per operation, not a percentile; `B/op` and
  `allocs/op` are Go heap allocations per operation.
- Workloads: `SaveRecord` and `SaveRecordWithIndex` save one small Order per
  transaction (with a VALUE index for the latter); `SaveSplitRecord` saves one
  ~250 kB record (three chunks); `LoadRecord` loads one record;
  `ScanRecords`/`ScanRecordsSplitVersioned` scan 100 records (the latter in the
  SQL default layout: split records with inline versions);
  `ScanIndexRecords/serial` and `/pipelined` scan 1,000 records through a value
  index, loading each record serially or with the production pipeline.

## Results

### Final vs before: the allocation work

Ratio final/before, nominal 95% interval.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| SaveRecord | 0.981 [0.954, 1.015] | 0.902 [0.900, 0.904] | 0.859 [0.857, 0.861] |
| SaveRecordWithIndex | 0.998 [0.965, 1.040] | 0.910 [0.908, 0.912] | 0.866 [0.866, 0.867] |
| SaveSplitRecord | 0.966 [0.907, 1.031] | 0.998 [0.993, 1.004] | 0.888 [0.887, 0.889] |
| LoadRecord | 0.978 [0.958, 0.994] | 0.855 [0.854, 0.856] | 0.793 [0.793, 0.793] |
| ScanRecords | 0.978 [0.948, 1.005] | 0.859 [0.859, 0.860] | 0.828 [0.828, 0.828] |
| ScanRecordsSplitVersioned | 0.976 [0.957, 0.996] | 0.812 [0.812, 0.812] | 0.816 [0.816, 0.816] |
| ScanIndexRecords/serial | 1.015 [0.987, 1.052] | 0.757 [0.757, 0.758] | 0.651 [0.651, 0.651] |
| ScanIndexRecords/pipelined | 0.972 [0.952, 0.995] | 0.749 [0.749, 0.749] | 0.627 [0.626, 0.627] |

### Final vs legacy: both changes together

Ratio final/legacy, nominal 95% interval. The legacy tree predates the read-path parity change.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| SaveRecord | 1.057 [0.996, 1.174] | 0.970 [0.967, 0.972] | 0.928 [0.928, 0.930] |
| SaveRecordWithIndex | 1.002 [0.982, 1.030] | 0.973 [0.972, 0.974] | 0.927 [0.926, 0.929] |
| SaveSplitRecord | 1.023 [0.996, 1.049] | 1.009 [1.004, 1.013] | 0.873 [0.872, 0.874] |
| LoadRecord | 1.138 [1.119, 1.157] | 0.995 [0.993, 0.996] | 0.917 [0.917, 0.917] |
| ScanRecords | 1.000 [0.975, 1.026] | 0.859 [0.859, 0.860] | 0.828 [0.828, 0.828] |
| ScanRecordsSplitVersioned | 0.990 [0.972, 1.015] | 0.812 [0.811, 0.812] | 0.816 [0.816, 0.816] |
| ScanIndexRecords/serial | 1.477 [1.433, 1.541] | 1.309 [1.308, 1.310] | 1.186 [1.186, 1.186] |
| ScanIndexRecords/pipelined | 0.647 [0.633, 0.665] | 1.219 [1.219, 1.219] | 1.074 [1.074, 1.074] |

### Absolute values

Median over blocks of each process's median of its three repeats.

| Case | Arm | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| SaveRecord | legacy | 2.44 ms | 18,753 | 249 |
| SaveRecord | before | 2.55 ms | 20,148 | 269 |
| SaveRecord | final | 2.46 ms | 18,162 | 231 |
| SaveRecordWithIndex | legacy | 2.60 ms | 22,331 | 299 |
| SaveRecordWithIndex | before | 2.58 ms | 23,894 | 320 |
| SaveRecordWithIndex | final | 2.60 ms | 21,728 | 277 |
| SaveSplitRecord | legacy | 4.30 ms | 1,564,351 | 334 |
| SaveSplitRecord | before | 4.47 ms | 1,580,698 | 329 |
| SaveSplitRecord | final | 4.30 ms | 1,578,096 | 292 |
| LoadRecord | legacy | 660.5 µs | 12,949 | 180 |
| LoadRecord | before | 766.7 µs | 15,060 | 208 |
| LoadRecord | final | 749.9 µs | 12,882 | 165 |
| ScanRecords | legacy | 1.33 ms | 107,746 | 1,435 |
| ScanRecords | before | 1.36 ms | 107,767 | 1,435 |
| ScanRecords | final | 1.33 ms | 92,600 | 1,188 |
| ScanRecordsSplitVersioned | legacy | 1.80 ms | 175,364 | 1,977 |
| ScanRecordsSplitVersioned | before | 1.81 ms | 175,349 | 1,977 |
| ScanRecordsSplitVersioned | final | 1.77 ms | 142,334 | 1,613 |
| ScanIndexRecords/serial | legacy | 171.29 ms | 3,365,490 | 41,250 |
| ScanIndexRecords/serial | before | 249.38 ms | 5,816,518 | 75,140 |
| ScanIndexRecords/serial | final | 246.65 ms | 4,403,421 | 48,917 |
| ScanIndexRecords/pipelined | legacy | 66.30 ms | 3,947,452 | 47,400 |
| ScanIndexRecords/pipelined | before | 44.02 ms | 6,425,942 | 81,278 |
| ScanIndexRecords/pipelined | final | 42.74 ms | 4,811,544 | 50,922 |

## Interpretation

Measured:

- **The allocation work cut allocations in every case.** `allocs/op`
  fell 11–37% from before to final, most in the index scans
  (`ScanIndexRecords/pipelined` 0.627, `/serial` 0.651) and `LoadRecord`
  (0.793). `B/op` fell 9–25% except for `SaveSplitRecord`, whose ~1.6 MB per
  operation is dominated by the record itself (0.998).
- **Time changed little.** Only `LoadRecord` (0.978),
  `ScanRecordsSplitVersioned` (0.976) and `ScanIndexRecords/pipelined` (0.972)
  have intervals entirely below 1, about 2–3% faster. The other five include 1.
- **The parity change dominates the index scans' time.** Compared with
  legacy, final is 48% slower on the serial index scan (1.477) and 14% slower
  on `LoadRecord` (1.138), but 35% faster on the pipelined index scan (0.647).
  The absolute medians put the step between legacy and before, i.e. on the
  parity change: serial 171 → 249 ms, `LoadRecord` 661 → 767 µs, pipelined
  66 → 44 ms. Those legacy-to-before figures are ratios of medians, not paired
  estimates with intervals.
- **Allocation work recovered most of the parity change's allocations but not
  its time.** On the serial index scan, parity raised `allocs/op` from 41,250
  to 75,140 and the allocation work brought it to 48,917, while time stayed at
  about 247 ms.

Not explained by this data:

- The pipelined index scan got 34% faster with the parity change here, while
  the 2026-10-12 parity report measured +3% for the same change on a
  workstation (FoundationDB in Docker, no network namespace). A plausible
  difference is that whole-record range reads overlap better under this
  environment's round-trip time, but that was not measured or profiled.
- Why `LoadRecord` is still 14% slower than legacy after the allocation work.
  The parity report attributes the extra work to reading and validating the
  whole record subspace instead of a point read; this run did not profile it.

## Limits

- Same single host and veth path as phase A, one `fdbserver` process,
  single redundancy, `memory` engine: no replication, no network hop between
  storage servers, no disk. Each benchmark runs one operation at a time.
- testing.B reports means; there are no latency percentiles here.
- Six blocks give limited resolution; intervals are nominal, per case and
  metric, with no multiplicity correction and no aggregate across cases.
  `SaveRecord`'s final/legacy interval is wide because one block's ratio was
  1.36; its leave-one-block-out estimates range 1.005–1.073.
- **Case labels in the aggregate CSV were reconstructed.** A loop variable in
  the campaign runner shadowed the case key, so the runner wrote a CPU field
  name as every row's case in `metrics.csv` (fixed in
  `tools/benchcampaign/singlenode/bench_runner.py`, with a regression test).
  The per-run directories kept the correct case, and the analyzer reparses the
  raw benchmark output. The 432 labels were restored from the pinned plan in a
  separate working copy; no measurement was repeated or changed.
