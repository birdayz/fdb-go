---
title: Benchmarks
description: "Preregistered FoundationDB benchmark campaigns on dedicated single-node and 4-node VPC infrastructure: methodology, confidence intervals, and retained negative results."
weight: 3
---

Both campaigns ran on dedicated hosts under **protocols sealed before timing**:
counterbalanced block schedules, pinned binary and runtime SHA-256 digests, 1 Hz
host and process telemetry with cluster health and role checks, and nominal 95%
paired-block bootstrap intervals (`20,000` resamples). Negative results and
contaminated or unexecuted cases are reported alongside positive results. Older
sequential and change-specific measurements are in
[`docs/benchmarks/`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/README.md).

{{< cards >}}
  {{< card link="hcloud-multinode-vpc-20261014" title="4-node VPC campaign (hcloud-multinode-vpc-20261014)" icon="server" subtitle="1 dedicated client VM + 3 FDB 7.3.77 server VMs (double ssd-2, 9 storage servers) over an isolated VPC: pure-Go vs CGo at 1–64 workers and USE_REMOTE_FETCH vs SCAN_AND_FETCH under sealed shard placement." >}}
  {{< card link="hcloud-rebench-20261014" title="Single-node 16-vCPU campaign (hcloud-rebench-20261014)" icon="chip" subtitle="8 phases on a dedicated 16-vCPU host: client (1w & 8w), Record Layer, mapped remote fetch (forward, reverse, Skip, synthetic 0.5 ms RTT), and SQL." >}}
{{< /cards >}}

## Reading the ratios

- Every ratio is **`numerator / denominator`** (geometric mean of within-block
  ratios across 6 or 8 counterbalanced blocks); **below `1.000` means the
  numerator is faster or allocates less**.
- Point estimates are summary indicators only; consult the linked phase reports
  for the **nominal 95% paired-block bootstrap intervals** (`[low, high]`) and
  absolute medians.

## Campaigns

| Campaign | Topology & Storage | Phases & Comparisons (`numerator / denominator`) |
|---|---|---|
| **[hcloud-multinode-vpc-20261014](hcloud-multinode-vpc-20261014/)** | **4 × `ccx43`** (16 vCPU / 64 GiB each) on an isolated `/24` VPC (`0.33–0.41 ms` p50 ping); 1 client host + 3 FDB 7.3.77 server hosts (`double` `ssd-2`, 17 processes, 9 storage servers) | • **[Phase M1](hcloud-multinode-vpc-20261014/phase-m1-client/):** `final pure-Go / CGo` at `1, 8, 32, 64` workers<br>• **[Phase M2](hcloud-multinode-vpc-20261014/phase-m2-remotefetch/):** `USE_REMOTE_FETCH / SCAN_AND_FETCH` (`M2rl`, `M2skip`) with transactional CGo shard-placement receipts (`small` and `planskip` 100% process-disjoint `cross-server`; `split` `limit1` `cross-server`, `rows10` `f = 0.0000`, `rows1000` `f = 0.6960`) |
| **[hcloud-rebench-20261014](hcloud-rebench-20261014/)** | **1 × `ccx43`** (16 vCPU / 64 GiB), private veth pair (`0.039 ms` p50 ping); 1 FDB 7.3.77 server (`single` `ssd-2` for client, `memory` for Record Layer / SQL) | • **[Phase A](hcloud-rebench-20261014/phase-a-client/) & [Phase A2](hcloud-rebench-20261014/phase-a2-client-8w/):** `final pure-Go / CGo` and `final / before` at `1w` (11 workloads) and `8w` (3 workloads)<br>• **[Phase B](hcloud-rebench-20261014/phase-b-recordlayer/) & [Phase D1](hcloud-rebench-20261014/phase-d1-sql/):** Record Layer & SQL `final / before` and `final / legacy`<br>• **[Phase C1](hcloud-rebench-20261014/phase-c1-remotefetch/), [C2](hcloud-rebench-20261014/phase-c2-reverse-skip/), [C3](hcloud-rebench-20261014/phase-c3-synthetic-rtt/), [D2](hcloud-rebench-20261014/phase-d2-sql-remotefetch/):** `USE_REMOTE_FETCH / SCAN_AND_FETCH` |

## Results

### Pure-Go client vs CGo (`libfdb_c` 7.3.77)

- **1 worker (`M1w1` 4-node VPC and `Phase A` single-node):**
  - On the 4-node VPC (`M1w1`, 7/7 clean), pure-Go had lower mean latency
    (`0.579–0.954×`), lower p99 latency on 6 of 7 workloads (`get100` p99
    `0.865 [0.734, 1.037]` overlaps 1), and lower shutdown process CPU (`E−A`,
    `0.570–0.748×`) on every workload.
  - On single-node (`Phase A`, 11/11 clean), pure-Go took `0.515–0.931×` of CGo
    mean latency on round-trip workloads (`range100` interval `0.846 [0.713,
    1.024]` overlaps 1) and `0.189×` on client-only `ryw`.
- **8, 32, and 64 workers (`M1w8`, `M1w32`, `M1w64` 4-node VPC and `Phase A2`
  single-node):**
  - **Mean latency wins:** on the 4-node VPC, pure-Go had lower mean latency
    with intervals below 1 on `get100` (`0.888×` at 8w, `0.770×` at 32w,
    `0.676×` at 64w), `get1000` (`0.942×, 0.715×, 0.632×`, with 64w p99
    `0.385 [0.336, 0.438]`), `set100` (`0.980×, 0.938×, 0.887×`), and
    `range100` at 8w (`0.868×`) and 64w (`0.554×`).
  - **32-worker regressions / unresolved cases:** at 32 workers on the 4-node
    VPC, `range100` mean overlapped 1 (`0.869 [0.753, 1.148]`) while `range100`
    p99 regressed (`1.247 [1.048, 1.644]`), and `grv` mean was 4.1% slower for
    pure-Go (`1.041 [1.007, 1.081]`). On single-node 8w (`Phase A2`), `get100`
    latency overlapped 1 (`0.991 [0.928, 1.084]`).
  - **Process CPU (`E−A`) split under concurrency:** on the 4-node VPC, pure-Go
    used **less** `E−A` CPU per transaction on `grv` at every worker level
    (`0.904×` at 8w, `0.765×` at 32w, `0.736×` at 64w), had an interval
    overlapping 1 on 64w `range100` (`1.069 [0.923, 1.180]`), and used **more**
    `E−A` CPU (`1.291–2.162×`) on `get100`, `get1000`, `set100`, and 8w/32w
    `range100` (and `1.150–1.638×` across all three single-node `Phase A2`
    workloads).
  - **Retained environmental aborts (`batchset50` at 8, 32, and 64 workers):**
    in `M1w8`, `M1w32`, and `M1w64`, 8–64 concurrent workers committing 50 keys
    per transaction pushed the FDB tLog queue past the preregistered 100 MB
    post-run gate (`113.7–193.4 MB`), so `batchset50` was omitted as
    `contaminated` and the driver conservatively stopped each phase before
    `batchset50x1000` (retained as unexecuted / `unverifiable`).

### Record Layer and SQL: `USE_REMOTE_FETCH` vs `SCAN_AND_FETCH`

- **4-node VPC (`M2rl`, `M2skip` on 3-node `double` `ssd-2`):**
  - **Retained negative result on 1,000-row scans:** every 1,000-row scan was
    **23% to 133% slower** with `USE_REMOTE_FETCH` than with pipelined
    `SCAN_AND_FETCH` (`small-rows1000` `1.226×` forward / `1.255×` reverse;
    `split-rows1000` `2.326×` forward / `2.300×` reverse; `skip3-rows1000`
    `1.360×` forward / `1.324×` reverse), despite `47–60%` fewer client Go
    heap allocations per operation.
  - **Small-scan wins:** `USE_REMOTE_FETCH` was faster on
    `skip3-rows10-reverse` (`0.158 [0.155, 0.160]`, 4.87 ms vs 31.13 ms),
    `split-rows10` (`0.873×` forward, `0.854×` reverse, where `f = 0.0000`),
    and `small-limit1-forward` (`0.979×`).
  - These results support retaining `SCAN_AND_FETCH` as the default for the
    measured workloads and conditions.
- **Single-node `memory` engine (`C1`, `C2`, `C3`, `D2`):**
  - On one `memory`-engine server, `USE_REMOTE_FETCH` was faster on 1,000 small
    records (`0.341×` forward, `0.337×` reverse, `0.224×` under +0.5 ms
    synthetic veth RTT) and SQL `IDX_A` (`0.885×` on 10 rows, `0.536×` on 1,000
    rows, with a disclosed 1-byte store-prefix confound in 3 ordinary runs),
    but **5.3% to 15.0% slower** on 1,000 two-chunk split records (`1.150×`
    forward, `1.146×` reverse, `1.053×` at +0.5 ms RTT).

### Allocation work (`final` vs `before`)

- Across single-node `Phase A`, `Phase A2`, `Phase B`, and `Phase D1`, the
  RFC-258 allocation work reduced Go heap allocations per operation by
  **`3–20%`** in the client and **`11–37%`** in the Record Layer (`5.1%` in SQL
  `PlanCacheHit`).
- In Record Layer `Phase B`, `ns/op` point estimates ranged from `0.966×` to
  `1.015×` (`LoadRecord` `0.978 [0.958, 0.994]`, `ScanRecordsSplitVersioned`
  `0.976 [0.957, 0.996]`, `ScanIndexRecords/pipelined` `0.972 [0.952, 0.995]`,
  with the remaining five intervals overlapping `1.000`). In client `Phase A`
  (1w) and `Phase A2` (8w), mean-latency point estimates ranged from `0.984×` to
  `1.042×` (`read1000` `1.042 [0.998, 1.087]`) in `A` and from `0.995×` to
  `1.069×` (`range100` `1.069 [0.948, 1.206]`, p99 `0.901 [0.829, 0.982]`) in
  `A2`; every mean-latency interval overlapped `1.000` (which does not establish
  equivalence).

## Reproducibility and artifacts

- All benchmark harnesses, plan sealers, 1 Hz node telemetry samplers (`R3`),
  clock-bounded multi-host telemetry reconcilers (`R4`), and transactional
  `libfdb_c` shard-placement probes (`H5`/`H6`) are committed in the repository
  under [`tools/clientbench`](https://github.com/birdayz/fdb-go/tree/master/tools/clientbench)
  and [`tools/benchcampaign`](https://github.com/birdayz/fdb-go/tree/master/tools/benchcampaign),
  with benchmark source documentation in
  [`docs/benchmarks/campaign-harness.md`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)
  and unit/regression tests covering the runners, parsers, placement receipts,
  and reconcilers.
- The multi-gigabyte raw per-transaction CSVs, 1 Hz `/proc` telemetry streams,
  and full `status json` / probe archives are retained off-repository in the
  campaign archives (`hcloud-rebench-20261014` and
  `hcloud-multinode-vpc-20261014`, not currently published for download) with
  SHA-256 manifests; every summary ratio, confidence interval, and absolute
  median in these pages is generated directly from those archived `report.json`,
  `summary.json`, and `absolute.csv` outputs.
