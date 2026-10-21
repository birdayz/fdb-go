---
title: Benchmarks
description: "Preregistered FoundationDB benchmark campaigns on dedicated single-node and 4-node VPC infrastructure: methodology, confidence intervals, and retained negative results."
weight: 3
---

Three benchmark campaigns on dedicated Hetzner hosts, each with its protocol sealed
before timing. Results are paired-block ratios with 95% bootstrap confidence
intervals. Negative, aborted, and inconclusive cases are reported. Older
measurements are in [`docs/benchmarks/`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/README.md).

{{< cards >}}
  {{< card link="fdbmn-20261020" title="4-node VPC campaign, master (fdbmn-20261020)" icon="server" subtitle="The 4-node VPC phases repeated on master fa8725f1d: master vs old Go (267a41434) vs CGo at 1, 8, and 64 workers, and USE_REMOTE_FETCH vs SCAN_AND_FETCH on master." >}}
  {{< card link="hcloud-multinode-vpc-20261014" title="4-node VPC campaign (hcloud-multinode-vpc-20261014)" icon="server" subtitle="1 dedicated client VM + 3 FDB 7.3.77 server VMs (double ssd-2, 9 storage servers) over an isolated VPC: pure-Go vs CGo at 1–64 workers and USE_REMOTE_FETCH vs SCAN_AND_FETCH under sealed shard placement." >}}
  {{< card link="hcloud-rebench-20261014" title="Single-node 16-vCPU campaign (hcloud-rebench-20261014)" icon="chip" subtitle="8 phases on a dedicated 16-vCPU host: client (1w & 8w), Record Layer, mapped remote fetch (forward, reverse, Skip, synthetic 0.5 ms RTT), and SQL." >}}
{{< /cards >}}

## Reading the ratios

Ratios are `numerator / denominator`. Below `1.0` means the numerator is faster
or allocates less. Each phase page shows the 95% confidence intervals and the
absolute medians.

## Campaigns

| Campaign | Topology & Storage | Phases & Comparisons (`numerator / denominator`) |
|---|---|---|
| **[fdbmn-20261020](fdbmn-20261020/)** | **4 × `ccx43`** (16 vCPU / 64 GiB each) on a private VPC; 1 client host + 3 FDB 7.3.77 server hosts (`double` `ssd-2`, 17 processes, 9 storage servers) | • **[Phase M1](fdbmn-20261020/phase-m1-client/):** `master / old Go`, `master / CGo` and `old Go / CGo` at `1, 8, 64` workers<br>• **[Phase M2](fdbmn-20261020/phase-m2-remotefetch/):** `USE_REMOTE_FETCH / SCAN_AND_FETCH` on master (`M2rl`, `M2skip`); `split` `rows1000` `f = 0.820`, other cells `f = 1` |
| **[hcloud-multinode-vpc-20261014](hcloud-multinode-vpc-20261014/)** | **4 × `ccx43`** (16 vCPU / 64 GiB each) on an isolated `/24` VPC (`0.33–0.41 ms` p50 ping); 1 client host + 3 FDB 7.3.77 server hosts (`double` `ssd-2`, 17 processes, 9 storage servers) | • **[Phase M1](hcloud-multinode-vpc-20261014/phase-m1-client/):** `final pure-Go / CGo` at `1, 8, 32, 64` workers<br>• **[Phase M2](hcloud-multinode-vpc-20261014/phase-m2-remotefetch/):** `USE_REMOTE_FETCH / SCAN_AND_FETCH` (`M2rl`, `M2skip`) with transactional CGo shard-placement receipts (`small` and `planskip` 100% process-disjoint `cross-server`; `split` `limit1` `cross-server`, `rows10` `f = 0.0000`, `rows1000` `f = 0.6960`) |
| **[hcloud-rebench-20261014](hcloud-rebench-20261014/)** | **1 × `ccx43`** (16 vCPU / 64 GiB), private veth pair (`0.039 ms` p50 ping); 1 FDB 7.3.77 server (`single` `ssd-2` for client, `memory` for Record Layer / SQL) | • **[Phase A](hcloud-rebench-20261014/phase-a-client/) & [Phase A2](hcloud-rebench-20261014/phase-a2-client-8w/):** `final pure-Go / CGo` and `final / before` at `1w` (11 workloads) and `8w` (3 workloads)<br>• **[Phase B](hcloud-rebench-20261014/phase-b-recordlayer/) & [Phase D1](hcloud-rebench-20261014/phase-d1-sql/):** Record Layer & SQL `final / before` and `final / legacy`<br>• **[Phase C1](hcloud-rebench-20261014/phase-c1-remotefetch/), [C2](hcloud-rebench-20261014/phase-c2-reverse-skip/), [C3](hcloud-rebench-20261014/phase-c3-synthetic-rtt/), [D2](hcloud-rebench-20261014/phase-d2-sql-remotefetch/):** `USE_REMOTE_FETCH / SCAN_AND_FETCH` |

## Results

### Master vs old Go and CGo (2026-10-20)

- At 8 and 64 workers, master (`fa8725f1d`) has higher get and set latency
  than old Go (`267a41434`), up to `1.348×` p50 (get100, 64 workers). Its
  lifecycle CPU on get, set and grv is lower in every cell (down to `0.553×`),
  and it makes fewer Go allocations in every eligible cell. Range100 at
  64 workers has wide, unstable intervals (`1.435×` p50, `1.180×` CPU).
- In every measured get, set and range cell, master's latency is below
  libfdb_c 7.3.77's or the interval includes `1.0×`. The one cell above CGo is
  `grv` at 8 workers (`1.025×` p50, `1.021×` mean).
- `USE_REMOTE_FETCH` is slower than `SCAN_AND_FETCH` on every 1,000-row scan:
  `1.152×` to `1.220×` cross-server and `2.304×` on the partially colocated
  split case (`f = 0.820`).

### Pure-Go client vs CGo (`libfdb_c` 7.3.77), 2026-10-14

- At 1 worker, pure-Go has lower mean latency on 17 of 18 workloads across both
  campaigns. The remaining one is inconclusive.
- At 8–64 workers on the 3-node cluster, pure-Go still has lower mean latency on
  11 of 15 cells, but uses more process CPU on most of them, up to `2.162×`.
- One result favors CGo on mean latency: `grv` at 32 workers (`1.041×`). On p99,
  `range100` at 32 workers is `1.247×`.
- `batchset50` at 8–64 workers has no results: the server's log queue exceeded
  the 100 MB gate.

### `USE_REMOTE_FETCH` vs `SCAN_AND_FETCH`, 2026-10-14

- On the 3-node cluster, `USE_REMOTE_FETCH` is slower on every 1,000-row scan
  (`1.226×` to `2.326×`). `SCAN_AND_FETCH` stays the default.
- On a single in-memory server it is faster in most cases, including SQL index
  scans. Those single-server results did not hold for 1,000-row scans on the
  cluster.

### Allocation work (`final` vs `before`), 2026-10-14

- Go allocations dropped 3–20% per transaction in the client and 11–37% per
  operation in the Record Layer.
- Record Layer time improved 2–3% on 3 of 8 benchmarks. Client mean latency did
  not change measurably.

## Reproducibility and artifacts

- Harnesses, telemetry samplers, and shard-placement probes are in
  [`tools/clientbench`](https://github.com/birdayz/fdb-go/tree/master/tools/clientbench)
  and [`tools/benchcampaign`](https://github.com/birdayz/fdb-go/tree/master/tools/benchcampaign).
  The method is described in
  [`campaign-harness.md`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md).
- Raw per-transaction data and telemetry are archived off-repository with SHA-256
  manifests and are not published. Every number on these pages comes from the
  archived reports.
