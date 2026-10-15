---
title: "Phase M1: Client (1, 8, 32, 64 workers)"
description: "Pure-Go vs CGo client latency and process CPU across 1, 8, 32, and 64 closed-loop workers over a real VPC to a 3-node double ssd-2 cluster."
weight: 1
---

Final pure-Go client (`pkg/fdbgo`, `267a41434a81723b07e7150869d702636a1b91a9`)
against Apple's Go binding over `libfdb_c` 7.3.77 (`CGo`) across four
closed-loop concurrency phases (`M1w1` = 1 worker, `M1w8` = 8 workers, `M1w32` =
32 workers, `M1w64` = 64 workers) over an isolated `/24` private VPC against a
3-node `double`-redundancy `ssd-2` FoundationDB 7.3.77 cluster (17 server
processes, 9 storage servers). This is **phase M1** of
[hcloud-multinode-vpc-20261014](../).

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
- **Metric scopes (same as single-node [Phase A](/docs/benchmarks/hcloud-rebench-20261014/phase-a-client/)):**
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

<figure class="bench-figure">
<figcaption><strong>Figure M1.1: Mean latency, pure-Go / CGo</strong><span>Below 1.0× means pure-Go is faster.</span></figcaption>
<p class="bench-summary">Pure-Go has lower mean latency in 18 of 22 cells, down to 0.554×. CGo is faster in one: grv at 32 workers (1.041×). Three cells are inconclusive. There are no results for batchset50 and batchset50x1000 at 8–64 workers: the server&#x27;s log queue exceeded the 100 MB gate during batchset50 and the run stopped.</p>
<svg class="bench-svg" viewBox="0 0 1320 722" role="img" aria-label="Figure M1.1: Mean latency, pure-Go / CGo">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="696"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.35×</text>
<text class="axis-text" x="165.0" y="711" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="361.5" y1="36" x2="361.5" y2="696"/>
<text class="axis-text" x="361.5" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="361.5" y="711" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="546.8" y1="36" x2="546.8" y2="696"/>
<text class="axis-text" x="546.8" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="546.8" y="711" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="743.2" y1="36" x2="743.2" y2="696"/>
<text class="axis-text" x="743.2" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="743.2" y="711" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="928.5" y1="36" x2="928.5" y2="696"/>
<text class="axis-text" x="928.5" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="928.5" y="711" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="696"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1125.0" y="711" text-anchor="middle">2.0×</text>
<text class="axis-text" x="735.2" y="17" text-anchor="end">← Pure-Go faster</text>
<text class="axis-text" x="751.2" y="17" text-anchor="start">CGo faster →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">M1w1 (1 worker — 7/7 clean)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.579× [0.568, 0.591]</title><line x1="431.7" y1="77.0" x2="453.5" y2="77.0" stroke-width="2.2"/><line x1="431.7" y1="73.0" x2="431.7" y2="81.0" stroke-width="1.8"/><line x1="453.5" y1="73.0" x2="453.5" y2="81.0" stroke-width="1.8"/><circle cx="442.2" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.579× [0.568, 0.591]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.702× [0.690, 0.712]</title><line x1="538.8" y1="97.0" x2="556.1" y2="97.0" stroke-width="2.2"/><line x1="538.8" y1="93.0" x2="538.8" y2="101.0" stroke-width="1.8"/><line x1="556.1" y1="93.0" x2="556.1" y2="101.0" stroke-width="1.8"/><circle cx="548.3" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.702× [0.690, 0.712]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.673× [0.656, 0.691]</title><line x1="511.0" y1="117.0" x2="539.6" y2="117.0" stroke-width="2.2"/><line x1="511.0" y1="113.0" x2="511.0" y2="121.0" stroke-width="1.8"/><line x1="539.6" y1="113.0" x2="539.6" y2="121.0" stroke-width="1.8"/><circle cx="525.1" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.673× [0.656, 0.691]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.633× [0.579, 0.672]</title><line x1="442.2" y1="137.0" x2="524.3" y2="137.0" stroke-width="2.2"/><line x1="442.2" y1="133.0" x2="442.2" y2="141.0" stroke-width="1.8"/><line x1="524.3" y1="133.0" x2="524.3" y2="141.0" stroke-width="1.8"/><circle cx="491.4" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.633× [0.579, 0.672]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.886× [0.878, 0.894]</title><line x1="671.6" y1="157.0" x2="681.5" y2="157.0" stroke-width="2.2"/><line x1="671.6" y1="153.0" x2="671.6" y2="161.0" stroke-width="1.8"/><line x1="681.5" y1="153.0" x2="681.5" y2="161.0" stroke-width="1.8"/><circle cx="676.6" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1137" y="161.0">0.886× [0.878, 0.894]</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.920× [0.914, 0.926]</title><line x1="693.7" y1="177.0" x2="700.9" y2="177.0" stroke-width="2.2"/><line x1="693.7" y1="173.0" x2="693.7" y2="181.0" stroke-width="1.8"/><line x1="700.9" y1="173.0" x2="700.9" y2="181.0" stroke-width="1.8"/><circle cx="697.3" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1137" y="181.0">0.920× [0.914, 0.926]</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.954× [0.949, 0.958]</title><line x1="714.4" y1="197.0" x2="719.6" y2="197.0" stroke-width="2.2"/><line x1="714.4" y1="193.0" x2="714.4" y2="201.0" stroke-width="1.8"/><line x1="719.6" y1="193.0" x2="719.6" y2="201.0" stroke-width="1.8"/><circle cx="717.3" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1137" y="201.0">0.954× [0.949, 0.958]</text>
<line class="section-divider" x1="8" y1="210" x2="1312" y2="210"/>
<text class="section-title" x="10" y="225">M1w8 (8 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 0.996× [0.949, 1.038]</title><line x1="714.4" y1="239.0" x2="763.8" y2="239.0" stroke-width="2.2"/><line x1="714.4" y1="235.0" x2="714.4" y2="243.0" stroke-width="1.8"/><line x1="763.8" y1="235.0" x2="763.8" y2="243.0" stroke-width="1.8"/><circle cx="741.0" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">0.996× [0.949, 1.038]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.888× [0.866, 0.907]</title><line x1="664.0" y1="259.0" x2="689.5" y2="259.0" stroke-width="2.2"/><line x1="664.0" y1="255.0" x2="664.0" y2="263.0" stroke-width="1.8"/><line x1="689.5" y1="255.0" x2="689.5" y2="263.0" stroke-width="1.8"/><circle cx="677.8" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">0.888× [0.866, 0.907]</text>
<text class="row-label" x="153" y="283.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.942× [0.921, 0.961]</title><line x1="697.9" y1="279.0" x2="721.3" y2="279.0" stroke-width="2.2"/><line x1="697.9" y1="275.0" x2="697.9" y2="283.0" stroke-width="1.8"/><line x1="721.3" y1="275.0" x2="721.3" y2="283.0" stroke-width="1.8"/><circle cx="710.3" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1137" y="283.0">0.942× [0.921, 0.961]</text>
<text class="row-label" x="153" y="303.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.868× [0.846, 0.891]</title><line x1="651.1" y1="299.0" x2="679.7" y2="299.0" stroke-width="2.2"/><line x1="651.1" y1="295.0" x2="651.1" y2="303.0" stroke-width="1.8"/><line x1="679.7" y1="295.0" x2="679.7" y2="303.0" stroke-width="1.8"/><circle cx="665.3" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1137" y="303.0">0.868× [0.846, 0.891]</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.980× [0.966, 0.993]</title><line x1="724.2" y1="319.0" x2="739.4" y2="319.0" stroke-width="2.2"/><line x1="724.2" y1="315.0" x2="724.2" y2="323.0" stroke-width="1.8"/><line x1="739.4" y1="315.0" x2="739.4" y2="323.0" stroke-width="1.8"/><circle cx="732.1" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">0.980× [0.966, 0.993]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="343.0">N/A (omitted)</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="363.0">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="372" x2="1312" y2="372"/>
<text class="section-title" x="10" y="387">M1w32 (32 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="405.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 1.041× [1.007, 1.081]</title><line x1="747.1" y1="401.0" x2="786.1" y2="401.0" stroke-width="2.2"/><line x1="747.1" y1="397.0" x2="747.1" y2="405.0" stroke-width="1.8"/><line x1="786.1" y1="397.0" x2="786.1" y2="405.0" stroke-width="1.8"/><circle cx="765.4" cy="401.0" r="3.8"/></g>
<text class="val-text" x="1137" y="405.0">1.041× [1.007, 1.081]</text>
<text class="row-label" x="153" y="425.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.770× [0.750, 0.793]</title><line x1="584.8" y1="421.0" x2="615.5" y2="421.0" stroke-width="2.2"/><line x1="584.8" y1="417.0" x2="584.8" y2="425.0" stroke-width="1.8"/><line x1="615.5" y1="417.0" x2="615.5" y2="425.0" stroke-width="1.8"/><circle cx="599.3" cy="421.0" r="3.8"/></g>
<text class="val-text" x="1137" y="425.0">0.770× [0.750, 0.793]</text>
<text class="row-label" x="153" y="445.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.715× [0.688, 0.747]</title><line x1="537.3" y1="441.0" x2="582.6" y2="441.0" stroke-width="2.2"/><line x1="537.3" y1="437.0" x2="537.3" y2="445.0" stroke-width="1.8"/><line x1="582.6" y1="437.0" x2="582.6" y2="445.0" stroke-width="1.8"/><circle cx="558.5" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1137" y="445.0">0.715× [0.688, 0.747]</text>
<text class="row-label" x="153" y="465.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.869× [0.753, 1.148]</title><line x1="587.0" y1="461.0" x2="819.2" y2="461.0" stroke-width="2.2"/><line x1="587.0" y1="457.0" x2="587.0" y2="465.0" stroke-width="1.8"/><line x1="819.2" y1="457.0" x2="819.2" y2="465.0" stroke-width="1.8"/><circle cx="665.9" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1137" y="465.0">0.869× [0.753, 1.148]</text>
<text class="row-label" x="153" y="485.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.938× [0.931, 0.946]</title><line x1="703.8" y1="481.0" x2="712.6" y2="481.0" stroke-width="2.2"/><line x1="703.8" y1="477.0" x2="703.8" y2="485.0" stroke-width="1.8"/><line x1="712.6" y1="477.0" x2="712.6" y2="485.0" stroke-width="1.8"/><circle cx="708.0" cy="481.0" r="3.8"/></g>
<text class="val-text" x="1137" y="485.0">0.938× [0.931, 0.946]</text>
<text class="row-label" x="153" y="505.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="505.0">N/A (omitted)</text>
<text class="row-label" x="153" y="525.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="525.0">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="534" x2="1312" y2="534"/>
<text class="section-title" x="10" y="549">M1w64 (64 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="567.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 1.029× [0.990, 1.069]</title><line x1="737.7" y1="563.0" x2="780.0" y2="563.0" stroke-width="2.2"/><line x1="737.7" y1="559.0" x2="737.7" y2="567.0" stroke-width="1.8"/><line x1="780.0" y1="559.0" x2="780.0" y2="567.0" stroke-width="1.8"/><circle cx="759.0" cy="563.0" r="3.8"/></g>
<text class="val-text" x="1137" y="567.0">1.029× [0.990, 1.069]</text>
<text class="row-label" x="153" y="587.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.676× [0.644, 0.710]</title><line x1="500.8" y1="583.0" x2="554.6" y2="583.0" stroke-width="2.2"/><line x1="500.8" y1="579.0" x2="500.8" y2="587.0" stroke-width="1.8"/><line x1="554.6" y1="579.0" x2="554.6" y2="587.0" stroke-width="1.8"/><circle cx="527.6" cy="583.0" r="3.8"/></g>
<text class="val-text" x="1137" y="587.0">0.676× [0.644, 0.710]</text>
<text class="row-label" x="153" y="607.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.632× [0.601, 0.669]</title><line x1="462.8" y1="603.0" x2="521.8" y2="603.0" stroke-width="2.2"/><line x1="462.8" y1="599.0" x2="462.8" y2="607.0" stroke-width="1.8"/><line x1="521.8" y1="599.0" x2="521.8" y2="607.0" stroke-width="1.8"/><circle cx="490.5" cy="603.0" r="3.8"/></g>
<text class="val-text" x="1137" y="607.0">0.632× [0.601, 0.669]</text>
<text class="row-label" x="153" y="627.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.554× [0.389, 0.673]</title><line x1="223.2" y1="623.0" x2="525.1" y2="623.0" stroke-width="2.2"/><line x1="223.2" y1="619.0" x2="223.2" y2="627.0" stroke-width="1.8"/><line x1="525.1" y1="619.0" x2="525.1" y2="627.0" stroke-width="1.8"/><circle cx="417.9" cy="623.0" r="3.8"/></g>
<text class="val-text" x="1137" y="627.0">0.554× [0.389, 0.673]</text>
<text class="row-label" x="153" y="647.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.887× [0.869, 0.897]</title><line x1="665.9" y1="643.0" x2="683.4" y2="643.0" stroke-width="2.2"/><line x1="665.9" y1="639.0" x2="665.9" y2="647.0" stroke-width="1.8"/><line x1="683.4" y1="639.0" x2="683.4" y2="647.0" stroke-width="1.8"/><circle cx="677.2" cy="643.0" r="3.8"/></g>
<text class="val-text" x="1137" y="647.0">0.887× [0.869, 0.897]</text>
<text class="row-label" x="153" y="667.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="667.0">N/A (omitted)</text>
<text class="row-label" x="153" y="687.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="687.0">N/A (omitted)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.2: p99 latency, pure-Go / CGo</strong><span>Below 1.0× means pure-Go has lower p99.</span></figcaption>
<p class="bench-summary">Pure-Go has lower p99 latency in 17 of 22 cells, down to 0.385× (get1000, 64 workers). CGo is lower in one: range100 at 32 workers (1.247×). Four cells are inconclusive.</p>
<svg class="bench-svg" viewBox="0 0 1320 722" role="img" aria-label="Figure M1.2: p99 latency, pure-Go / CGo">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="696"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.25×</text>
<text class="axis-text" x="165.0" y="711" text-anchor="middle">0.25×</text>
<line class="grid-line" x1="320.3" y1="36" x2="320.3" y2="696"/>
<text class="axis-text" x="320.3" y="30" text-anchor="middle">0.35×</text>
<text class="axis-text" x="320.3" y="711" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="485.0" y1="36" x2="485.0" y2="696"/>
<text class="axis-text" x="485.0" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="485.0" y="711" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="640.3" y1="36" x2="640.3" y2="696"/>
<text class="axis-text" x="640.3" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="640.3" y="711" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="805.0" y1="36" x2="805.0" y2="696"/>
<text class="axis-text" x="805.0" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="805.0" y="711" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="960.3" y1="36" x2="960.3" y2="696"/>
<text class="axis-text" x="960.3" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="960.3" y="711" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="696"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1125.0" y="711" text-anchor="middle">2.0×</text>
<text class="axis-text" x="797.0" y="17" text-anchor="end">← Pure-Go lower p99</text>
<text class="axis-text" x="813.0" y="17" text-anchor="start">CGo lower p99 →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">M1w1 (1 worker — 7/7 clean)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.498× [0.408, 0.601]</title><line x1="391.1" y1="77.0" x2="569.9" y2="77.0" stroke-width="2.2"/><line x1="391.1" y1="73.0" x2="391.1" y2="81.0" stroke-width="1.8"/><line x1="569.9" y1="73.0" x2="569.9" y2="81.0" stroke-width="1.8"/><circle cx="483.1" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.498× [0.408, 0.601]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-neutral"><title>get100: 0.865× [0.734, 1.037]</title><line x1="662.2" y1="97.0" x2="821.8" y2="97.0" stroke-width="2.2"/><line x1="662.2" y1="93.0" x2="662.2" y2="101.0" stroke-width="1.8"/><line x1="821.8" y1="93.0" x2="821.8" y2="101.0" stroke-width="1.8"/><circle cx="738.0" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.865× [0.734, 1.037]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.705× [0.654, 0.779]</title><line x1="609.0" y1="117.0" x2="689.7" y2="117.0" stroke-width="2.2"/><line x1="609.0" y1="113.0" x2="609.0" y2="121.0" stroke-width="1.8"/><line x1="689.7" y1="113.0" x2="689.7" y2="121.0" stroke-width="1.8"/><circle cx="643.6" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.705× [0.654, 0.779]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.715× [0.595, 0.861]</title><line x1="565.3" y1="137.0" x2="735.9" y2="137.0" stroke-width="2.2"/><line x1="565.3" y1="133.0" x2="565.3" y2="141.0" stroke-width="1.8"/><line x1="735.9" y1="133.0" x2="735.9" y2="141.0" stroke-width="1.8"/><circle cx="650.1" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.715× [0.595, 0.861]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.794× [0.756, 0.837]</title><line x1="675.9" y1="157.0" x2="722.9" y2="157.0" stroke-width="2.2"/><line x1="675.9" y1="153.0" x2="675.9" y2="161.0" stroke-width="1.8"/><line x1="722.9" y1="153.0" x2="722.9" y2="161.0" stroke-width="1.8"/><circle cx="698.5" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1137" y="161.0">0.794× [0.756, 0.837]</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.913× [0.857, 0.975]</title><line x1="733.8" y1="177.0" x2="793.3" y2="177.0" stroke-width="2.2"/><line x1="733.8" y1="173.0" x2="733.8" y2="181.0" stroke-width="1.8"/><line x1="793.3" y1="173.0" x2="793.3" y2="181.0" stroke-width="1.8"/><circle cx="763.0" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1137" y="181.0">0.913× [0.857, 0.975]</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.957× [0.924, 0.990]</title><line x1="768.5" y1="197.0" x2="800.4" y2="197.0" stroke-width="2.2"/><line x1="768.5" y1="193.0" x2="768.5" y2="201.0" stroke-width="1.8"/><line x1="800.4" y1="193.0" x2="800.4" y2="201.0" stroke-width="1.8"/><circle cx="784.7" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1137" y="201.0">0.957× [0.924, 0.990]</text>
<line class="section-divider" x1="8" y1="210" x2="1312" y2="210"/>
<text class="section-title" x="10" y="225">M1w8 (8 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 1.101× [0.869, 1.457]</title><line x1="740.2" y1="239.0" x2="978.8" y2="239.0" stroke-width="2.2"/><line x1="740.2" y1="235.0" x2="740.2" y2="243.0" stroke-width="1.8"/><line x1="978.8" y1="235.0" x2="978.8" y2="243.0" stroke-width="1.8"/><circle cx="849.4" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">1.101× [0.869, 1.457]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.815× [0.741, 0.882]</title><line x1="666.6" y1="259.0" x2="747.0" y2="259.0" stroke-width="2.2"/><line x1="666.6" y1="255.0" x2="666.6" y2="263.0" stroke-width="1.8"/><line x1="747.0" y1="255.0" x2="747.0" y2="263.0" stroke-width="1.8"/><circle cx="710.6" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">0.815× [0.741, 0.882]</text>
<text class="row-label" x="153" y="283.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.931× [0.859, 0.993]</title><line x1="734.8" y1="279.0" x2="801.8" y2="279.0" stroke-width="2.2"/><line x1="734.8" y1="275.0" x2="734.8" y2="283.0" stroke-width="1.8"/><line x1="801.8" y1="275.0" x2="801.8" y2="283.0" stroke-width="1.8"/><circle cx="772.0" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1137" y="283.0">0.931× [0.859, 0.993]</text>
<text class="row-label" x="153" y="303.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.925× [0.861, 0.987]</title><line x1="735.9" y1="299.0" x2="799.0" y2="299.0" stroke-width="2.2"/><line x1="735.9" y1="295.0" x2="735.9" y2="303.0" stroke-width="1.8"/><line x1="799.0" y1="295.0" x2="799.0" y2="303.0" stroke-width="1.8"/><circle cx="769.0" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1137" y="303.0">0.925× [0.861, 0.987]</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.916× [0.860, 0.976]</title><line x1="735.4" y1="319.0" x2="793.8" y2="319.0" stroke-width="2.2"/><line x1="735.4" y1="315.0" x2="735.4" y2="323.0" stroke-width="1.8"/><line x1="793.8" y1="315.0" x2="793.8" y2="323.0" stroke-width="1.8"/><circle cx="764.5" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">0.916× [0.860, 0.976]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="343.0">N/A (omitted)</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="363.0">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="372" x2="1312" y2="372"/>
<text class="section-title" x="10" y="387">M1w32 (32 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="405.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 0.871× [0.754, 1.017]</title><line x1="674.6" y1="401.0" x2="812.8" y2="401.0" stroke-width="2.2"/><line x1="674.6" y1="397.0" x2="674.6" y2="405.0" stroke-width="1.8"/><line x1="812.8" y1="397.0" x2="812.8" y2="405.0" stroke-width="1.8"/><circle cx="741.2" cy="401.0" r="3.8"/></g>
<text class="val-text" x="1137" y="405.0">0.871× [0.754, 1.017]</text>
<text class="row-label" x="153" y="425.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.815× [0.679, 0.944]</title><line x1="626.3" y1="421.0" x2="778.4" y2="421.0" stroke-width="2.2"/><line x1="626.3" y1="417.0" x2="626.3" y2="425.0" stroke-width="1.8"/><line x1="778.4" y1="417.0" x2="778.4" y2="425.0" stroke-width="1.8"/><circle cx="710.6" cy="421.0" r="3.8"/></g>
<text class="val-text" x="1137" y="425.0">0.815× [0.679, 0.944]</text>
<text class="row-label" x="153" y="445.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.481× [0.423, 0.551]</title><line x1="407.8" y1="441.0" x2="529.8" y2="441.0" stroke-width="2.2"/><line x1="407.8" y1="437.0" x2="407.8" y2="445.0" stroke-width="1.8"/><line x1="529.8" y1="437.0" x2="529.8" y2="445.0" stroke-width="1.8"/><circle cx="467.1" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1137" y="445.0">0.481× [0.423, 0.551]</text>
<text class="row-label" x="153" y="465.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.247× [1.048, 1.644]</title><line x1="826.6" y1="461.0" x2="1034.5" y2="461.0" stroke-width="2.2"/><line x1="826.6" y1="457.0" x2="826.6" y2="465.0" stroke-width="1.8"/><line x1="1034.5" y1="457.0" x2="1034.5" y2="465.0" stroke-width="1.8"/><circle cx="906.9" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1137" y="465.0">1.247× [1.048, 1.644]</text>
<text class="row-label" x="153" y="485.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.904× [0.856, 0.948]</title><line x1="733.2" y1="481.0" x2="780.3" y2="481.0" stroke-width="2.2"/><line x1="733.2" y1="477.0" x2="733.2" y2="485.0" stroke-width="1.8"/><line x1="780.3" y1="477.0" x2="780.3" y2="485.0" stroke-width="1.8"/><circle cx="758.4" cy="481.0" r="3.8"/></g>
<text class="val-text" x="1137" y="485.0">0.904× [0.856, 0.948]</text>
<text class="row-label" x="153" y="505.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="505.0">N/A (omitted)</text>
<text class="row-label" x="153" y="525.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="525.0">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="534" x2="1312" y2="534"/>
<text class="section-title" x="10" y="549">M1w64 (64 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="567.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 0.960× [0.757, 1.190]</title><line x1="676.5" y1="563.0" x2="885.3" y2="563.0" stroke-width="2.2"/><line x1="676.5" y1="559.0" x2="676.5" y2="567.0" stroke-width="1.8"/><line x1="885.3" y1="559.0" x2="885.3" y2="567.0" stroke-width="1.8"/><circle cx="786.2" cy="563.0" r="3.8"/></g>
<text class="val-text" x="1137" y="567.0">0.960× [0.757, 1.190]</text>
<text class="row-label" x="153" y="587.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.679× [0.601, 0.776]</title><line x1="569.9" y1="583.0" x2="687.9" y2="583.0" stroke-width="2.2"/><line x1="569.9" y1="579.0" x2="569.9" y2="587.0" stroke-width="1.8"/><line x1="687.9" y1="579.0" x2="687.9" y2="587.0" stroke-width="1.8"/><circle cx="626.3" cy="583.0" r="3.8"/></g>
<text class="val-text" x="1137" y="587.0">0.679× [0.601, 0.776]</text>
<text class="row-label" x="153" y="607.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.385× [0.336, 0.438]</title><line x1="301.5" y1="603.0" x2="423.9" y2="603.0" stroke-width="2.2"/><line x1="301.5" y1="599.0" x2="301.5" y2="607.0" stroke-width="1.8"/><line x1="423.9" y1="599.0" x2="423.9" y2="607.0" stroke-width="1.8"/><circle cx="364.3" cy="603.0" r="3.8"/></g>
<text class="val-text" x="1137" y="607.0">0.385× [0.336, 0.438]</text>
<text class="row-label" x="153" y="627.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.732× [0.506, 0.912]</title><line x1="490.5" y1="623.0" x2="762.5" y2="623.0" stroke-width="2.2"/><line x1="490.5" y1="619.0" x2="490.5" y2="627.0" stroke-width="1.8"/><line x1="762.5" y1="619.0" x2="762.5" y2="627.0" stroke-width="1.8"/><circle cx="661.0" cy="623.0" r="3.8"/></g>
<text class="val-text" x="1137" y="627.0">0.732× [0.506, 0.912]</text>
<text class="row-label" x="153" y="647.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.872× [0.815, 0.927]</title><line x1="710.6" y1="643.0" x2="770.0" y2="643.0" stroke-width="2.2"/><line x1="710.6" y1="639.0" x2="710.6" y2="647.0" stroke-width="1.8"/><line x1="770.0" y1="639.0" x2="770.0" y2="647.0" stroke-width="1.8"/><circle cx="741.8" cy="643.0" r="3.8"/></g>
<text class="val-text" x="1137" y="647.0">0.872× [0.815, 0.927]</text>
<text class="row-label" x="153" y="667.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="667.0">N/A (omitted)</text>
<text class="row-label" x="153" y="687.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="687.0">N/A (omitted)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.3: Process CPU per transaction, pure-Go / CGo</strong><span>Below 1.0× means pure-Go uses less CPU. Includes startup, GC, and shutdown (E−A).</span></figcaption>
<p class="bench-summary">At 1 worker, pure-Go uses less CPU on all 7 workloads. At 8–64 workers it uses more CPU in 11 of the 12 cells other than grv, up to 2.162× (set100, 32 workers). grv still uses less.</p>
<svg class="bench-svg" viewBox="0 0 1320 722" role="img" aria-label="Figure M1.3: Process CPU per transaction, pure-Go / CGo">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="696"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="165.0" y="711" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="365.7" y1="36" x2="365.7" y2="696"/>
<text class="axis-text" x="365.7" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="365.7" y="711" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="578.4" y1="36" x2="578.4" y2="696"/>
<text class="axis-text" x="578.4" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="578.4" y="711" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="779.1" y1="36" x2="779.1" y2="696"/>
<text class="axis-text" x="779.1" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="779.1" y="711" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="991.9" y1="36" x2="991.9" y2="696"/>
<text class="axis-text" x="991.9" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="991.9" y="711" text-anchor="middle">2.0×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="696"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.5×</text>
<text class="axis-text" x="1125.0" y="711" text-anchor="middle">2.5×</text>
<text class="axis-text" x="570.4" y="17" text-anchor="end">← Pure-Go less CPU</text>
<text class="axis-text" x="586.4" y="17" text-anchor="start">CGo less CPU →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">M1w1 (1 worker — 7/7 clean)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.570× [0.551, 0.588]</title><line x1="222.9" y1="77.0" x2="261.7" y2="77.0" stroke-width="2.2"/><line x1="222.9" y1="73.0" x2="222.9" y2="81.0" stroke-width="1.8"/><line x1="261.7" y1="73.0" x2="261.7" y2="81.0" stroke-width="1.8"/><circle cx="243.2" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.570× [0.551, 0.588]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.660× [0.648, 0.671]</title><line x1="319.7" y1="97.0" x2="340.5" y2="97.0" stroke-width="2.2"/><line x1="319.7" y1="93.0" x2="319.7" y2="101.0" stroke-width="1.8"/><line x1="340.5" y1="93.0" x2="340.5" y2="101.0" stroke-width="1.8"/><circle cx="330.6" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.660× [0.648, 0.671]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.629× [0.603, 0.653]</title><line x1="276.7" y1="117.0" x2="324.2" y2="117.0" stroke-width="2.2"/><line x1="276.7" y1="113.0" x2="276.7" y2="121.0" stroke-width="1.8"/><line x1="324.2" y1="113.0" x2="324.2" y2="121.0" stroke-width="1.8"/><circle cx="301.9" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.629× [0.603, 0.653]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.747× [0.731, 0.761]</title><line x1="391.5" y1="137.0" x2="415.5" y2="137.0" stroke-width="2.2"/><line x1="391.5" y1="133.0" x2="391.5" y2="141.0" stroke-width="1.8"/><line x1="415.5" y1="133.0" x2="415.5" y2="141.0" stroke-width="1.8"/><circle cx="404.5" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.747× [0.731, 0.761]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.688× [0.677, 0.700]</title><line x1="345.8" y1="157.0" x2="365.7" y2="157.0" stroke-width="2.2"/><line x1="345.8" y1="153.0" x2="345.8" y2="161.0" stroke-width="1.8"/><line x1="365.7" y1="153.0" x2="365.7" y2="161.0" stroke-width="1.8"/><circle cx="355.4" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1137" y="161.0">0.688× [0.677, 0.700]</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.667× [0.659, 0.673]</title><line x1="329.7" y1="177.0" x2="342.2" y2="177.0" stroke-width="2.2"/><line x1="329.7" y1="173.0" x2="329.7" y2="181.0" stroke-width="1.8"/><line x1="342.2" y1="173.0" x2="342.2" y2="181.0" stroke-width="1.8"/><circle cx="336.9" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1137" y="181.0">0.667× [0.659, 0.673]</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.748× [0.729, 0.770]</title><line x1="389.9" y1="197.0" x2="422.6" y2="197.0" stroke-width="2.2"/><line x1="389.9" y1="193.0" x2="389.9" y2="201.0" stroke-width="1.8"/><line x1="422.6" y1="193.0" x2="422.6" y2="201.0" stroke-width="1.8"/><circle cx="405.3" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1137" y="201.0">0.748× [0.729, 0.770]</text>
<line class="section-divider" x1="8" y1="210" x2="1312" y2="210"/>
<text class="section-title" x="10" y="225">M1w8 (8 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.904× [0.878, 0.931]</title><line x1="500.8" y1="239.0" x2="535.8" y2="239.0" stroke-width="2.2"/><line x1="500.8" y1="235.0" x2="500.8" y2="243.0" stroke-width="1.8"/><line x1="535.8" y1="235.0" x2="535.8" y2="243.0" stroke-width="1.8"/><circle cx="518.2" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">0.904× [0.878, 0.931]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.291× [1.264, 1.331]</title><line x1="718.2" y1="259.0" x2="749.0" y2="259.0" stroke-width="2.2"/><line x1="718.2" y1="255.0" x2="718.2" y2="263.0" stroke-width="1.8"/><line x1="749.0" y1="255.0" x2="749.0" y2="263.0" stroke-width="1.8"/><circle cx="730.8" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">1.291× [1.264, 1.331]</text>
<text class="row-label" x="153" y="283.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.362× [1.342, 1.382]</title><line x1="753.9" y1="279.0" x2="771.4" y2="279.0" stroke-width="2.2"/><line x1="753.9" y1="275.0" x2="753.9" y2="283.0" stroke-width="1.8"/><line x1="771.4" y1="275.0" x2="771.4" y2="283.0" stroke-width="1.8"/><circle cx="762.7" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1137" y="283.0">1.362× [1.342, 1.382]</text>
<text class="row-label" x="153" y="303.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.544× [1.476, 1.651]</title><line x1="810.7" y1="299.0" x2="877.5" y2="299.0" stroke-width="2.2"/><line x1="810.7" y1="295.0" x2="810.7" y2="303.0" stroke-width="1.8"/><line x1="877.5" y1="295.0" x2="877.5" y2="303.0" stroke-width="1.8"/><circle cx="837.5" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1137" y="303.0">1.544× [1.476, 1.651]</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 2.142× [2.123, 2.160]</title><line x1="1027.5" y1="319.0" x2="1037.8" y2="319.0" stroke-width="2.2"/><line x1="1027.5" y1="315.0" x2="1027.5" y2="323.0" stroke-width="1.8"/><line x1="1037.8" y1="315.0" x2="1037.8" y2="323.0" stroke-width="1.8"/><circle cx="1032.8" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">2.142× [2.123, 2.160]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="343.0">N/A (omitted)</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="363.0">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="372" x2="1312" y2="372"/>
<text class="section-title" x="10" y="387">M1w32 (32 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="405.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.765× [0.745, 0.779]</title><line x1="402.9" y1="401.0" x2="429.5" y2="401.0" stroke-width="2.2"/><line x1="402.9" y1="397.0" x2="402.9" y2="405.0" stroke-width="1.8"/><line x1="429.5" y1="397.0" x2="429.5" y2="405.0" stroke-width="1.8"/><circle cx="418.7" cy="401.0" r="3.8"/></g>
<text class="val-text" x="1137" y="405.0">0.765× [0.745, 0.779]</text>
<text class="row-label" x="153" y="425.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.368× [1.334, 1.411]</title><line x1="750.3" y1="421.0" x2="783.8" y2="421.0" stroke-width="2.2"/><line x1="750.3" y1="417.0" x2="750.3" y2="425.0" stroke-width="1.8"/><line x1="783.8" y1="417.0" x2="783.8" y2="425.0" stroke-width="1.8"/><circle cx="765.4" cy="421.0" r="3.8"/></g>
<text class="val-text" x="1137" y="425.0">1.368× [1.334, 1.411]</text>
<text class="row-label" x="153" y="445.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.537× [1.494, 1.575]</title><line x1="817.9" y1="441.0" x2="849.4" y2="441.0" stroke-width="2.2"/><line x1="817.9" y1="437.0" x2="817.9" y2="445.0" stroke-width="1.8"/><line x1="849.4" y1="437.0" x2="849.4" y2="445.0" stroke-width="1.8"/><circle cx="834.8" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1137" y="445.0">1.537× [1.494, 1.575]</text>
<text class="row-label" x="153" y="465.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.723× [1.616, 1.921]</title><line x1="864.7" y1="461.0" x2="967.9" y2="461.0" stroke-width="2.2"/><line x1="864.7" y1="457.0" x2="864.7" y2="465.0" stroke-width="1.8"/><line x1="967.9" y1="457.0" x2="967.9" y2="465.0" stroke-width="1.8"/><circle cx="903.0" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1137" y="465.0">1.723× [1.616, 1.921]</text>
<text class="row-label" x="153" y="485.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 2.162× [2.108, 2.216]</title><line x1="1023.3" y1="481.0" x2="1053.1" y2="481.0" stroke-width="2.2"/><line x1="1023.3" y1="477.0" x2="1023.3" y2="485.0" stroke-width="1.8"/><line x1="1053.1" y1="477.0" x2="1053.1" y2="485.0" stroke-width="1.8"/><circle cx="1038.4" cy="481.0" r="3.8"/></g>
<text class="val-text" x="1137" y="485.0">2.162× [2.108, 2.216]</text>
<text class="row-label" x="153" y="505.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="505.0">N/A (omitted)</text>
<text class="row-label" x="153" y="525.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="525.0">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="534" x2="1312" y2="534"/>
<text class="section-title" x="10" y="549">M1w64 (64 workers — 5/7 clean)</text>
<text class="row-label" x="153" y="567.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.736× [0.721, 0.752]</title><line x1="383.3" y1="563.0" x2="408.4" y2="563.0" stroke-width="2.2"/><line x1="383.3" y1="559.0" x2="383.3" y2="567.0" stroke-width="1.8"/><line x1="408.4" y1="559.0" x2="408.4" y2="567.0" stroke-width="1.8"/><circle cx="395.6" cy="563.0" r="3.8"/></g>
<text class="val-text" x="1137" y="567.0">0.736× [0.721, 0.752]</text>
<text class="row-label" x="153" y="587.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.429× [1.385, 1.478]</title><line x1="772.7" y1="583.0" x2="811.5" y2="583.0" stroke-width="2.2"/><line x1="772.7" y1="579.0" x2="772.7" y2="587.0" stroke-width="1.8"/><line x1="811.5" y1="579.0" x2="811.5" y2="587.0" stroke-width="1.8"/><circle cx="791.4" cy="583.0" r="3.8"/></g>
<text class="val-text" x="1137" y="587.0">1.429× [1.385, 1.478]</text>
<text class="row-label" x="153" y="607.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.608× [1.509, 1.705]</title><line x1="823.9" y1="603.0" x2="896.7" y2="603.0" stroke-width="2.2"/><line x1="823.9" y1="599.0" x2="823.9" y2="607.0" stroke-width="1.8"/><line x1="896.7" y1="599.0" x2="896.7" y2="607.0" stroke-width="1.8"/><circle cx="861.8" cy="603.0" r="3.8"/></g>
<text class="val-text" x="1137" y="607.0">1.608× [1.509, 1.705]</text>
<text class="row-label" x="153" y="627.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 1.069× [0.923, 1.180]</title><line x1="530.7" y1="623.0" x2="677.2" y2="623.0" stroke-width="2.2"/><line x1="530.7" y1="619.0" x2="530.7" y2="627.0" stroke-width="1.8"/><line x1="677.2" y1="619.0" x2="677.2" y2="627.0" stroke-width="1.8"/><circle cx="618.2" cy="623.0" r="3.8"/></g>
<text class="val-text" x="1137" y="627.0">1.069× [0.923, 1.180]</text>
<text class="row-label" x="153" y="647.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 1.981× [1.935, 2.021]</title><line x1="972.2" y1="643.0" x2="998.1" y2="643.0" stroke-width="2.2"/><line x1="972.2" y1="639.0" x2="972.2" y2="647.0" stroke-width="1.8"/><line x1="998.1" y1="639.0" x2="998.1" y2="647.0" stroke-width="1.8"/><circle cx="986.2" cy="643.0" r="3.8"/></g>
<text class="val-text" x="1137" y="647.0">1.981× [1.935, 2.021]</text>
<text class="row-label" x="153" y="667.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="667.0">N/A (omitted)</text>
<text class="row-label" x="153" y="687.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="687.0">N/A (omitted)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.4: Mean and p99 latency at 1 and 64 workers</strong><span>Bar is the mean, whisker is p99, in µs per transaction.</span></figcaption>
<p class="bench-summary">Mean latency is about 1–7 ms per transaction for both clients at both worker counts. Each worker count ran as a separate phase, so compare within a count.</p>
<svg class="bench-svg" viewBox="0 0 1347 392" role="img" aria-label="Figure M1.4: Mean and p99 latency at 1 and 64 workers">
<line class="grid-line" x1="182.0" y1="22" x2="182.0" y2="368"/>
<text class="axis-text" x="182.0" y="383" text-anchor="middle">0 µs</text>
<line class="grid-line" x1="342.0" y1="22" x2="342.0" y2="368"/>
<text class="axis-text" x="342.0" y="383" text-anchor="middle">2000 µs</text>
<line class="grid-line" x1="502.0" y1="22" x2="502.0" y2="368"/>
<text class="axis-text" x="502.0" y="383" text-anchor="middle">4000 µs</text>
<line class="grid-line" x1="662.0" y1="22" x2="662.0" y2="368"/>
<text class="axis-text" x="662.0" y="383" text-anchor="middle">6000 µs</text>
<line class="grid-line" x1="822.0" y1="22" x2="822.0" y2="368"/>
<text class="axis-text" x="822.0" y="383" text-anchor="middle">8000 µs</text>
<line class="grid-line" x1="982.0" y1="22" x2="982.0" y2="368"/>
<text class="axis-text" x="982.0" y="383" text-anchor="middle">10000 µs</text>
<line class="grid-line" x1="1142.0" y1="22" x2="1142.0" y2="368"/>
<text class="axis-text" x="1142.0" y="383" text-anchor="middle">12000 µs</text>
<text class="row-label" x="170" y="46.0" text-anchor="end">M1w1 grv</text>
<g class="ci-win"><title>M1w1 grv Pure-Go: mean 967.1 µs, p99 1477.4 µs</title><rect class="bar-go" x="182" y="32" width="77.4" height="10" rx="2"/><line x1="259.4" y1="37" x2="300.2" y2="37" stroke-width="1.6"/><line x1="300.2" y1="33" x2="300.2" y2="41" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 grv CGo: mean 1673.5 µs, p99 3133.0 µs</title><rect class="bar-cgo" x="182" y="44" width="133.9" height="10" rx="2"/><line x1="315.9" y1="49" x2="432.6" y2="49" stroke-width="1.6"/><line x1="432.6" y1="45" x2="432.6" y2="53" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="41.0">Go 967 / p99 1477 µs</text><text class="axis-text" x="1152" y="53.0">CGo 1674 / p99 3133 µs</text>
<text class="row-label" x="170" y="74.0" text-anchor="end">M1w1 get100</text>
<g class="ci-win"><title>M1w1 get100 Pure-Go: mean 1557.2 µs, p99 3588.2 µs</title><rect class="bar-go" x="182" y="60" width="124.6" height="10" rx="2"/><line x1="306.6" y1="65" x2="469.1" y2="65" stroke-width="1.6"/><line x1="469.1" y1="61" x2="469.1" y2="69" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 get100 CGo: mean 2210.4 µs, p99 3967.9 µs</title><rect class="bar-cgo" x="182" y="72" width="176.8" height="10" rx="2"/><line x1="358.8" y1="77" x2="499.4" y2="77" stroke-width="1.6"/><line x1="499.4" y1="73" x2="499.4" y2="81" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="69.0">Go 1557 / p99 3588 µs</text><text class="axis-text" x="1152" y="81.0">CGo 2210 / p99 3968 µs</text>
<text class="row-label" x="170" y="102.0" text-anchor="end">M1w1 get1000</text>
<g class="ci-win"><title>M1w1 get1000 Pure-Go: mean 1494.3 µs, p99 2642.5 µs</title><rect class="bar-go" x="182" y="88" width="119.5" height="10" rx="2"/><line x1="301.5" y1="93" x2="393.4" y2="93" stroke-width="1.6"/><line x1="393.4" y1="89" x2="393.4" y2="97" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 get1000 CGo: mean 2223.1 µs, p99 3861.8 µs</title><rect class="bar-cgo" x="182" y="100" width="177.8" height="10" rx="2"/><line x1="359.8" y1="105" x2="490.9" y2="105" stroke-width="1.6"/><line x1="490.9" y1="101" x2="490.9" y2="109" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="97.0">Go 1494 / p99 2642 µs</text><text class="axis-text" x="1152" y="109.0">CGo 2223 / p99 3862 µs</text>
<text class="row-label" x="170" y="130.0" text-anchor="end">M1w1 range100</text>
<g class="ci-win"><title>M1w1 range100 Pure-Go: mean 1580.8 µs, p99 3000.0 µs</title><rect class="bar-go" x="182" y="116" width="126.5" height="10" rx="2"/><line x1="308.5" y1="121" x2="422.0" y2="121" stroke-width="1.6"/><line x1="422.0" y1="117" x2="422.0" y2="125" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 range100 CGo: mean 2408.4 µs, p99 4244.1 µs</title><rect class="bar-cgo" x="182" y="128" width="192.7" height="10" rx="2"/><line x1="374.7" y1="133" x2="521.5" y2="133" stroke-width="1.6"/><line x1="521.5" y1="129" x2="521.5" y2="137" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="125.0">Go 1581 / p99 3000 µs</text><text class="axis-text" x="1152" y="137.0">CGo 2408 / p99 4244 µs</text>
<text class="row-label" x="170" y="158.0" text-anchor="end">M1w1 set100</text>
<g class="ci-win"><title>M1w1 set100 Pure-Go: mean 4234.5 µs, p99 6418.8 µs</title><rect class="bar-go" x="182" y="144" width="338.8" height="10" rx="2"/><line x1="520.8" y1="149" x2="695.5" y2="149" stroke-width="1.6"/><line x1="695.5" y1="145" x2="695.5" y2="153" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 set100 CGo: mean 4786.2 µs, p99 7991.7 µs</title><rect class="bar-cgo" x="182" y="156" width="382.9" height="10" rx="2"/><line x1="564.9" y1="161" x2="821.3" y2="161" stroke-width="1.6"/><line x1="821.3" y1="157" x2="821.3" y2="165" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="153.0">Go 4234 / p99 6419 µs</text><text class="axis-text" x="1152" y="165.0">CGo 4786 / p99 7992 µs</text>
<text class="row-label" x="170" y="186.0" text-anchor="end">M1w1 batchset50</text>
<g class="ci-win"><title>M1w1 batchset50 Pure-Go: mean 5039.9 µs, p99 7509.2 µs</title><rect class="bar-go" x="182" y="172" width="403.2" height="10" rx="2"/><line x1="585.2" y1="177" x2="782.7" y2="177" stroke-width="1.6"/><line x1="782.7" y1="173" x2="782.7" y2="181" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 batchset50 CGo: mean 5481.1 µs, p99 8253.0 µs</title><rect class="bar-cgo" x="182" y="184" width="438.5" height="10" rx="2"/><line x1="620.5" y1="189" x2="842.2" y2="189" stroke-width="1.6"/><line x1="842.2" y1="185" x2="842.2" y2="193" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="181.0">Go 5040 / p99 7509 µs</text><text class="axis-text" x="1152" y="193.0">CGo 5481 / p99 8253 µs</text>
<text class="row-label" x="170" y="214.0" text-anchor="end">M1w1 batchset50x1000</text>
<g class="ci-win"><title>M1w1 batchset50x1000 Pure-Go: mean 6241.4 µs, p99 9383.0 µs</title><rect class="bar-go" x="182" y="200" width="499.3" height="10" rx="2"/><line x1="681.3" y1="205" x2="932.6" y2="205" stroke-width="1.6"/><line x1="932.6" y1="201" x2="932.6" y2="209" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 batchset50x1000 CGo: mean 6577.5 µs, p99 10055.1 µs</title><rect class="bar-cgo" x="182" y="212" width="526.2" height="10" rx="2"/><line x1="708.2" y1="217" x2="986.4" y2="217" stroke-width="1.6"/><line x1="986.4" y1="213" x2="986.4" y2="221" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="209.0">Go 6241 / p99 9383 µs</text><text class="axis-text" x="1152" y="221.0">CGo 6578 / p99 10055 µs</text>
<text class="row-label" x="170" y="242.0" text-anchor="end">M1w64 grv</text>
<g class="ci-win"><title>M1w64 grv Pure-Go: mean 2017.9 µs, p99 4237.9 µs</title><rect class="bar-go" x="182" y="228" width="161.4" height="10" rx="2"/><line x1="343.4" y1="233" x2="521.0" y2="233" stroke-width="1.6"/><line x1="521.0" y1="229" x2="521.0" y2="237" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 grv CGo: mean 1931.8 µs, p99 4015.5 µs</title><rect class="bar-cgo" x="182" y="240" width="154.5" height="10" rx="2"/><line x1="336.5" y1="245" x2="503.2" y2="245" stroke-width="1.6"/><line x1="503.2" y1="241" x2="503.2" y2="249" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="237.0">Go 2018 / p99 4238 µs</text><text class="axis-text" x="1152" y="249.0">CGo 1932 / p99 4016 µs</text>
<text class="row-label" x="170" y="270.0" text-anchor="end">M1w64 get100</text>
<g class="ci-win"><title>M1w64 get100 Pure-Go: mean 2229.2 µs, p99 4538.2 µs</title><rect class="bar-go" x="182" y="256" width="178.3" height="10" rx="2"/><line x1="360.3" y1="261" x2="545.1" y2="261" stroke-width="1.6"/><line x1="545.1" y1="257" x2="545.1" y2="265" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 get100 CGo: mean 3271.6 µs, p99 6740.7 µs</title><rect class="bar-cgo" x="182" y="268" width="261.7" height="10" rx="2"/><line x1="443.7" y1="273" x2="721.3" y2="273" stroke-width="1.6"/><line x1="721.3" y1="269" x2="721.3" y2="277" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="265.0">Go 2229 / p99 4538 µs</text><text class="axis-text" x="1152" y="277.0">CGo 3272 / p99 6741 µs</text>
<text class="row-label" x="170" y="298.0" text-anchor="end">M1w64 get1000</text>
<g class="ci-win"><title>M1w64 get1000 Pure-Go: mean 2409.9 µs, p99 4544.4 µs</title><rect class="bar-go" x="182" y="284" width="192.8" height="10" rx="2"/><line x1="374.8" y1="289" x2="545.6" y2="289" stroke-width="1.6"/><line x1="545.6" y1="285" x2="545.6" y2="293" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 get1000 CGo: mean 3758.9 µs, p99 11686.4 µs</title><rect class="bar-cgo" x="182" y="296" width="300.7" height="10" rx="2"/><line x1="482.7" y1="301" x2="1116.9" y2="301" stroke-width="1.6"/><line x1="1116.9" y1="297" x2="1116.9" y2="305" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="293.0">Go 2410 / p99 4544 µs</text><text class="axis-text" x="1152" y="305.0">CGo 3759 / p99 11686 µs</text>
<text class="row-label" x="170" y="326.0" text-anchor="end">M1w64 range100</text>
<g class="ci-win"><title>M1w64 range100 Pure-Go: mean 3033.1 µs, p99 7695.6 µs</title><rect class="bar-go" x="182" y="312" width="242.6" height="10" rx="2"/><line x1="424.6" y1="317" x2="797.6" y2="317" stroke-width="1.6"/><line x1="797.6" y1="313" x2="797.6" y2="321" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 range100 CGo: mean 4659.5 µs, p99 8704.0 µs</title><rect class="bar-cgo" x="182" y="324" width="372.8" height="10" rx="2"/><line x1="554.8" y1="329" x2="878.3" y2="329" stroke-width="1.6"/><line x1="878.3" y1="325" x2="878.3" y2="333" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="321.0">Go 3033 / p99 7696 µs</text><text class="axis-text" x="1152" y="333.0">CGo 4660 / p99 8704 µs</text>
<text class="row-label" x="170" y="354.0" text-anchor="end">M1w64 set100</text>
<g class="ci-win"><title>M1w64 set100 Pure-Go: mean 5918.8 µs, p99 9591.9 µs</title><rect class="bar-go" x="182" y="340" width="473.5" height="10" rx="2"/><line x1="655.5" y1="345" x2="949.4" y2="345" stroke-width="1.6"/><line x1="949.4" y1="341" x2="949.4" y2="349" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 set100 CGo: mean 6659.4 µs, p99 10868.1 µs</title><rect class="bar-cgo" x="182" y="352" width="532.8" height="10" rx="2"/><line x1="714.8" y1="357" x2="1051.4" y2="357" stroke-width="1.6"/><line x1="1051.4" y1="353" x2="1051.4" y2="361" stroke-width="1.8"/></g>
<text class="val-text" x="1152" y="349.0">Go 5919 / p99 9592 µs</text><text class="axis-text" x="1152" y="361.0">CGo 6659 / p99 10868 µs</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> Pure-Go</span><span><i class="bench-swatch" style="background:#d97706"></i> CGo (libfdb_c 7.3.77)</span></div>
</figure>

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
