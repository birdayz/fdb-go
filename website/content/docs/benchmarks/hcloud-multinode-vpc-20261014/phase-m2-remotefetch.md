---
title: "Phase M2: Remote Fetch (3-Node VPC)"
description: "Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH across 9 storage servers on a 3-node VPC cluster with verified shard placement."
weight: 2
---

`USE_REMOTE_FETCH` (`getMappedRange`) against `SCAN_AND_FETCH` on the final
tree (`267a41434a81723b07e7150869d702636a1b91a9`) across a **4-host VPC
topology**: one dedicated client VM (`10.88.0.10`) and three dedicated
FoundationDB 7.3.77 server VMs (`10.88.0.11–13`, `double` redundancy, `ssd-2`
storage engine, 9 storage servers). This is **phase M2 (`M2rl` and `M2skip`)**
of [hcloud-multinode-vpc-20261014](../).

## Shard Placement and Qualification (Gate P0)

- Before any M2 timing, `placementprobe_cgo` (H5, reading `\xff/dataDistributionMode`,
  `\xff/moveKeysLock/Owner`, `\xff/moveKeysLock/Write`, `\xff/keyServers/`, and
  transactional storage addresses via `libfdb_c` at snapshot read version V1 and
  rechecking stability at V2) and the ballast tool (H6) were qualified on the
  live cluster:
  - Probing with data distribution on was refused.
  - The unmodified scratch control store passed `integrity_ok = true` and all
    30 Python provenance/key mutation controls (`ok = true`).
  - All three physical on-cluster defects (`delete-chunk`, `stray-suffix`,
    `version-key`) failed `integrity_ok` with the expected error messages.
- Three placement attempts (`256 MiB`, `512 MiB`, `1024 MiB` ballast) were run
  following the preregistered rule (`protocol.json` `placement.mechanism`):
  settle for 120 s with `moving_data` in-flight and queued bytes at 0, freeze
  data distribution (`datadistribution off`), clear only the ballast range and
  verify it empty, and take the canonical CGo placement probe and receipt.
- Per protocol, attempt 3 was sealed before timing (`plans/testingb/manifest.json`
  SHA `4ada3920f92da4489fe4037ad5dd3440cec59957fc4ccdcba81b792b7a45f54a`):
  - **`benchfetch/small`:** `integrity_ok = true`, `passed = true`, **100%
    `cross-server`** (server-disjoint fraction `1.0000`) on every cell.
  - **`benchfetch/planskip`:** `integrity_ok = true`, `passed = true`, **100%
    `cross-server`** (server-disjoint fraction `1.0000`) on every cell.
  - **`benchfetch/split`:** `integrity_ok = true`, 1 index shard and 7 record
    shards, `shared_shards = 0`: `limit1-forward` and `limit1-reverse` are
    **`cross-server`** (`1/1`), `rows10` is **`partially colocated (f =
    0.0000)`** (`0/10`), and `rows1000` is **`partially colocated (f =
    0.6960)`** (`696/1000` rows on storage-server teams disjoint from the index
    shard's team).
  - With `double` replication across three machines, any two 2-replica storage
    teams share at least one physical machine by pigeonhole; `cross-server`
    therefore means **storage-process (address) disjoint**, not machine
    disjoint.
- Before the first process and after the last process of every M2 case, `R2`
  (`bench_runner_mn.py`) took a fresh CGo placement receipt and verified via
  `placement_receipt.compare` that shard boundaries, storage teams, and
  disjointness fractions remained identical (`equal = true`).
- R4 telemetry reconciliation (`reconcile_telemetry.py`) across `node1..node3`
  and `client` reported **12/12 clean cases** in `M2rl` (144 processes, 432
  samples) and **4/4 clean cases** in `M2skip` (48 processes, 144 samples), with
  0 contaminated and 0 unverifiable cases.

## Results

### `M2rl`: `BenchmarkIndexFetch` (`final-remote / final-ordinary`)

Geometric mean of six paired block ratios (median of 3 repeats per process,
`3s` benchtime), with nominal 95% block-bootstrap intervals (20,000 resamples,
preregistered seed 492817; initial exploratory analysis with seed 391706 is
retained in `analysis-seed391706/`). Below 1 means `USE_REMOTE_FETCH` is faster
or allocates less.

| Case | Sealed placement label | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| small-limit1-forward | cross-server | 0.979 [0.964, 0.995] | 0.957 [0.956, 0.958] | 1.040 [1.038, 1.041] |
| small-limit1-reverse | cross-server | 0.985 [0.969, 1.003] | 0.958 [0.957, 0.959] | 1.049 [1.047, 1.052] |
| small-rows10-forward | cross-server | **1.031 [1.018, 1.042]** | 0.759 [0.759, 0.760] | 0.596 [0.596, 0.597] |
| small-rows10-reverse | cross-server | **1.026 [1.013, 1.039]** | 0.755 [0.755, 0.756] | 0.595 [0.595, 0.595] |
| small-rows1000-forward | cross-server | **1.226 [1.175, 1.280]** | 0.532 [0.531, 0.534] | 0.431 [0.431, 0.431] |
| small-rows1000-reverse | cross-server | **1.255 [1.217, 1.293]** | 0.527 [0.526, 0.528] | 0.430 [0.430, 0.430] |
| split-limit1-forward | cross-server | **1.110 [1.087, 1.131]** | 0.715 [0.715, 0.716] | 0.942 [0.940, 0.943] |
| split-limit1-reverse | cross-server | **1.114 [1.095, 1.138]** | 0.715 [0.715, 0.715] | 0.952 [0.951, 0.953] |
| split-rows10-forward | partially colocated (f = 0.0000) | 0.873 [0.829, 0.920] | 0.695 [0.695, 0.695] | 0.505 [0.504, 0.505] |
| split-rows10-reverse | partially colocated (f = 0.0000) | 0.854 [0.823, 0.886] | 0.695 [0.695, 0.695] | 0.504 [0.504, 0.505] |
| split-rows1000-forward | partially colocated (f = 0.6960) | **2.326 [2.254, 2.397]** | 0.692 [0.692, 0.692] | 0.399 [0.399, 0.399] |
| split-rows1000-reverse | partially colocated (f = 0.6960) | **2.300 [2.206, 2.384]** | 0.693 [0.692, 0.693] | 0.398 [0.398, 0.398] |

### `M2skip`: `BenchmarkPlanSkip` (`final-remote / final-ordinary`)

| Case | Sealed placement label | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| skip3-rows10-forward | cross-server | 1.155 [0.991, 1.345] | 0.689 [0.688, 0.689] | 0.790 [0.790, 0.790] |
| skip3-rows10-reverse | cross-server | **0.158 [0.155, 0.160]** | 0.590 [0.589, 0.590] | 0.709 [0.709, 0.709] |
| skip3-rows1000-forward | cross-server | **1.360 [1.295, 1.414]** | 0.599 [0.598, 0.600] | 0.529 [0.529, 0.529] |
| skip3-rows1000-reverse | cross-server | **1.324 [1.250, 1.407]** | 0.594 [0.593, 0.595] | 0.527 [0.527, 0.527] |

All `rows/op` and `prefix-B` ratios in both `M2rl` and `M2skip` were **1.000
[1.000, 1.000]** (`prefix-B = 19` in `M2rl`, `22` in `M2skip`).

### Absolute values

Medians of the six block medians per arm.

| Phase | Case | Placement label | Ordinary ns/op (ms) | Remote ns/op (ms) | Ordinary B/op | Remote B/op | Ordinary allocs/op | Remote allocs/op |
|---|---|---|---:|---:|---:|---:|---:|---:|
| M2rl | small-limit1-forward | cross-server | 2.69 | 2.62 | 17,620 | 16,852 | 222 | 231 |
| M2rl | small-limit1-reverse | cross-server | 2.71 | 2.66 | 17,668 | 16,930 | 223 | 234 |
| M2rl | small-rows10-forward | cross-server | 3.22 | 3.33 | 92,274 | 70,033 | 696 | 415 |
| M2rl | small-rows10-reverse | cross-server | 3.22 | 3.33 | 92,784 | 70,044 | 697 | 415 |
| M2rl | small-rows1000-forward | cross-server | 95.10 | 117.08 | 4,497,080 | 2,393,973 | 52,669 | 22,677 |
| M2rl | small-rows1000-reverse | cross-server | 96.39 | 118.95 | 4,544,538 | 2,393,438 | 52,664 | 22,669 |
| M2rl | split-limit1-forward | cross-server | 3.76 | 4.20 | 475,736 | 340,354 | 263 | 248 |
| M2rl | split-limit1-reverse | cross-server | 3.96 | 4.41 | 475,836 | 340,297 | 263 | 250 |
| M2rl | split-rows10-forward | partially colocated (f = 0.0000) | 6.83 | 6.00 | 4,651,958 | 3,232,746 | 1,050 | 530 |
| M2rl | split-rows10-reverse | partially colocated (f = 0.0000) | 6.81 | 5.77 | 4,652,330 | 3,233,112 | 1,051 | 530 |
| M2rl | split-rows1000-forward | partially colocated (f = 0.6960) | 316.38 | 742.51 | 457,265,229 | 316,465,896 | 79,120 | 31,564 |
| M2rl | split-rows1000-reverse | partially colocated (f = 0.6960) | 327.62 | 768.95 | 457,308,020 | 316,621,142 | 79,125 | 31,476 |
| M2skip | skip3-rows10-forward | cross-server | 3.69 | 4.89 | 186,950 | 128,733 | 1,175 | 928 |
| M2skip | skip3-rows10-reverse | cross-server | 31.13 | 4.87 | 218,190 | 128,648 | 1,307 | 927 |
| M2skip | skip3-rows1000-forward | cross-server | 98.67 | 138.19 | 5,380,260 | 3,224,836 | 67,030 | 35,456 |
| M2skip | skip3-rows1000-reverse | cross-server | 101.06 | 133.24 | 5,433,262 | 3,226,728 | 67,278 | 35,446 |

## Interpretation

- **1,000-row scans on the 3-node `double` `ssd-2` cluster:**
  - On this 3-node `ssd-2` cluster (which differs from the single-node campaign
    simultaneously in host topology, network, `ssd-2` vs `memory` storage
    engine, `double` vs `single` redundancy, and storage-server count, so
    differences between the two campaigns are descriptive and cannot be
    attributed to one factor alone), every 1,000-row scan — both the
    `cross-server` cases (`small-rows1000`, `skip3-rows1000`) and the
    `partially colocated (f = 0.6960)` case (`split-rows1000`) — was **23% to
    133% slower** with `USE_REMOTE_FETCH` than with `SCAN_AND_FETCH`.
  - *Untested hypothesis:* in `SCAN_AND_FETCH`, the client pipelines
    `GetRange` point-range reads directly to the storage servers holding the
    primary records (with client-side load balancing across the two `double`
    replicas). In `USE_REMOTE_FETCH`, the storage server holding the index
    shard issues secondary-to-primary lookups to the storage processes holding
    the primary records (which for process-disjoint teams may be on another
    process on the same host or across the VPC, as per-lookup routing was not
    traced) and serializes the combined `MappedKeyValue` batches back through
    the index storage server; on 1,000-row and 100 MB split-record scans on
    `ssd-2`, that path may bottleneck compared with direct client-pipelined
    fetches.
- **Where `USE_REMOTE_FETCH` won:**
  - `skip3-rows10-reverse` (`0.158 [0.156, 0.160]`, 4.87 ms vs 31.13 ms):
    dramatically faster with remote fetch.
  - `split-rows10` (`0.873` forward, `0.854` reverse, where the 10 target
    records happened to be colocated on the same storage team as the index
    shard, `f = 0.0000`) and `small-limit1` (`0.979` forward).
  - Client-side Go heap allocations (`B/op` and `allocs/op`) were lower with
    `USE_REMOTE_FETCH` on every multi-row case (`B/op` `0.527–0.759`,
    `allocs/op` `0.398–0.790`).
- **Implication for defaults:** these multi-node VPC measurements confirm that
  `USE_REMOTE_FETCH` is not a universal win and can regress large scans
  substantially (`1.23–2.33×` on 1,000 rows), so keeping `SCAN_AND_FETCH` as the
  default and exposing `USE_REMOTE_FETCH` as an explicit opt-in remains the
  right design.
