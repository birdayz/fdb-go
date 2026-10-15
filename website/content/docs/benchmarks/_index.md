---
title: Benchmarks
description: "Preregistered FoundationDB benchmark campaigns on dedicated single-node and 4-node VPC infrastructure: methodology, confidence intervals, and retained negative results."
weight: 3
---

Every benchmark campaign in `fdb-go` is run under a **sealed pre-timing
protocol** with counterbalanced block schedules, pinned binary and runtime
SHA-256 digests, 1 Hz host and cluster telemetry, and nominal 95%
paired-block bootstrap intervals (`20,000` resamples). Negative results and
contaminated or unexecuted cases are retained and reported in full alongside
positive results.

{{< cards >}}
  {{< card link="hcloud-multinode-vpc-20261014" title="4-Node VPC Campaign (hcloud-multinode-vpc-20261014)" icon="server" subtitle="1 dedicated client VM + 3 FDB 7.3.77 server VMs (double ssd-2, 9 storage servers) over an isolated VPC: pure-Go vs CGo at 1–64 workers and cross-server USE_REMOTE_FETCH vs SCAN_AND_FETCH." >}}
  {{< card link="hcloud-rebench-20261014" title="Single-Node 16-vCPU Campaign (hcloud-rebench-20261014)" icon="chip" subtitle="8 phases on a dedicated 16-vCPU host: client (1w & 8w), Record Layer, mapped remote fetch (forward, reverse, Skip, synthetic 0.5 ms RTT), and SQL." >}}
{{< /cards >}}

## Campaign Comparison at a Glance

| Campaign | Topology & Storage | Key Scope | Headline Facts |
|---|---|---|---|
| **[hcloud-multinode-vpc-20261014](hcloud-multinode-vpc-20261014/)** | **4 × `ccx43`** (16 vCPU / 64 GiB each) on `10.88.0.0/24` VPC (`0.33–0.41 ms` p50 ping); 1 client host + 3 FDB 7.3.77 server hosts (`double` `ssd-2`, 17 processes, 9 storage servers) | **M1:** `pkg/fdbgo` vs `libfdb_c` 7.3.77 at `1, 8, 32, 64` workers<br>**M2:** `USE_REMOTE_FETCH` vs `SCAN_AND_FETCH` with verified cross-server shard placement | • **Client (`M1w1`, 1w):** pure-Go faster on all 7 workloads (`0.579–0.954×` mean latency, `0.570–0.748×` `E−A` CPU).<br>• **Client (`8w, 32w, 64w`):** pure-Go faster on `get100` (`0.888×, 0.770×, 0.676×`), `get1000` (`0.942×, 0.715×, 0.632×`), and `set100` (`0.980×, 0.938×, 0.887×`), but uses **more `E−A` process CPU** (`1.291–2.162×`), and at 32w regresses `range100` p99 (`1.247×`) and `grv` mean (`1.041×`).<br>• **Remote fetch (`M2`):** 1,000-row scans on 3-node `ssd-2` are **23–133% slower** with `USE_REMOTE_FETCH` (`1.226–2.326×`), confirming `SCAN_AND_FETCH` must remain the default. |
| **[hcloud-rebench-20261014](hcloud-rebench-20261014/)** | **1 × `ccx43`** (16 vCPU / 64 GiB), private veth namespace (`0.039 ms` p50 ping); 1 FDB 7.3.77 server process (`single` `ssd-2` for client, `memory` for Record Layer / SQL) | **A / A2:** Client 1w (11 workloads) & 8w (3 workloads), `final` vs `before` & `CGo`<br>**B:** Record Layer across `legacy`, `before`, `final`<br>**C1 / C2 / C3 / D1 / D2:** Remote fetch & SQL | • **Client (`A`, 1w):** pure-Go `0.515–0.931×` of CGo mean latency on round-trip ops, `0.189×` on in-memory `ryw`.<br>• **Client (`A2`, 8w):** pure-Go faster on `range100` mean (`0.663×`) and `batchset50` (`0.817×`), unresolved on `get100` (`0.991×`), higher `E−A` CPU (`1.150–1.638×`).<br>• **Allocation work (`final` vs `before`):** cut Go `allocs/op` by `3–20%` (client) and `11–37%` (Record Layer).<br>• **Single-server `memory` remote fetch (`C1–C3`):** faster on 1,000 small records (`0.341×` forward, `0.337×` reverse, `0.224×` at +0.5 ms synthetic RTT), but **5–15% slower** on 1,000 two-chunk split records (`1.150×, 1.146×, 1.053×`). |

## Methodology & Honesty Rules

1. **Preregistered designs and sealed schedules:** Every phase's arm orders
   (Latin-square / Williams-square blocks), sample counts, environment
   thresholds, and SHA-256 binary/config pins are sealed before the first timed
   run.
2. **Paired block-bootstrap inference:** Within each block, all arms run once
   in a fresh OS process. Point estimates are geometric means of within-block
   ratios; uncertainty is reported as nominal 95% percentile bootstrap
   intervals over whole blocks (`20,000` resamples). No outliers are removed.
3. **Strict separation of facts from hypotheses:** Tables report only measured
   quantities (`mean`, `p50`, `p99`, `ns/op`, `B/op`, `allocs/op`, and
   `E−A` shutdown process CPU). Architectural explanations that were not
   directly isolated by a profiler or wire counter are explicitly labeled as
   untested hypotheses.
4. **Retained negative and adverse outcomes:** Unfavorable ratios (such as
   `USE_REMOTE_FETCH` regressing 1,000-row scans on 3-node `ssd-2` and 1,000
   split-record scans on single-node `memory`, or pure-Go using higher multi-worker
   process CPU) and environmental gate aborts (such as 8–64 worker `batchset50`
   exceeding the 100 MB tLog queue ceiling) are preserved and documented, never
   hidden or selectively rerun.
5. **Reproducible tooling:** All harnesses, plan sealers, 1 Hz telemetry
   samplers, multi-host clock-bounded telemetry reconcilers (`R4`), and
   `libfdb_c` transactional shard-placement probes (`H5`/`H6`) are in the
   repository under [`tools/clientbench`](https://github.com/birdayz/fdb-go/tree/master/tools/clientbench)
   and [`tools/benchcampaign`](https://github.com/birdayz/fdb-go/tree/master/tools/benchcampaign),
   with benchmark source documentation in
   [`docs/benchmarks/campaign-harness.md`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md).
