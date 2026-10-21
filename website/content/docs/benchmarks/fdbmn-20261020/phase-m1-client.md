---
title: "Phase M1: Client (1, 8, 64 workers)"
description: "Master vs old Go vs CGo client latency, lifecycle CPU, and Go allocations at 1, 8, and 64 closed-loop workers over a real VPC to a 3-node double ssd-2 cluster."
weight: 1
---

Three client arms over a private VPC against a 3-node `double`-redundancy
`ssd-2` FoundationDB 7.3.77 cluster (17 server processes, 9 storage servers),
in three closed-loop concurrency phases: `M1w1` (1 worker), `M1w8`
(8 workers) and `M1w64` (64 workers). This is **phase M1** of
[fdbmn-20261020](../).

| Arm | Tree | Client |
|---|---|---|
| master | `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc` | Pure-Go client (`pkg/fdbgo`) |
| old Go | `267a41434a81723b07e7150869d702636a1b91a9` | Pure-Go client, the final tree of the [2026-10-14 campaign](/docs/benchmarks/hcloud-multinode-vpc-20261014/phase-m1-client/) |
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

<figure class="bench-figure">
<figcaption><strong>Figure M1.1: p50 latency, master / old Go</strong><span>Below 1.0× means master has lower p50.</span></figcaption>
<p class="bench-summary">At 8 and 64 workers master has higher p50 than old Go on every get, range and set workload, from 1.112 to 1.435; grv p50 is lower (0.941 and 0.923). At 1 worker master p50 is 1.049–1.091.</p>
<svg class="bench-svg" viewBox="0 0 1320 560" role="img" aria-label="Figure M1.1: p50 latency, master / old Go">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="534"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="165.0" y="549" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="434.0" y1="36" x2="434.0" y2="534"/>
<text class="axis-text" x="434.0" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="434.0" y="549" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="687.7" y1="36" x2="687.7" y2="534"/>
<text class="axis-text" x="687.7" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="687.7" y="549" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="956.7" y1="36" x2="956.7" y2="534"/>
<text class="axis-text" x="956.7" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="956.7" y="549" text-anchor="middle">2.0×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="534"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.5×</text>
<text class="axis-text" x="1125.0" y="549" text-anchor="middle">2.5×</text>
<text class="axis-text" x="426.0" y="17" text-anchor="end">← Master lower</text>
<text class="axis-text" x="442.0" y="17" text-anchor="start">Old Go lower →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">M1w1 (1 worker, 4/7 clean)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 1.049× [1.006, 1.080]</title><line x1="438.5" y1="77.0" x2="492.0" y2="77.0" stroke-width="2.2"/><line x1="438.5" y1="73.0" x2="438.5" y2="81.0" stroke-width="1.8"/><line x1="492.0" y1="73.0" x2="492.0" y2="81.0" stroke-width="1.8"/><circle cx="470.1" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">1.049× [1.006, 1.080]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.088× [1.053, 1.128]</title><line x1="472.9" y1="97.0" x2="524.8" y2="97.0" stroke-width="2.2"/><line x1="472.9" y1="93.0" x2="472.9" y2="101.0" stroke-width="1.8"/><line x1="524.8" y1="93.0" x2="524.8" y2="101.0" stroke-width="1.8"/><circle cx="497.6" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">1.088× [1.053, 1.128]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.091× [1.082, 1.099]</title><line x1="493.4" y1="117.0" x2="505.2" y2="117.0" stroke-width="2.2"/><line x1="493.4" y1="113.0" x2="493.4" y2="121.0" stroke-width="1.8"/><line x1="505.2" y1="113.0" x2="505.2" y2="121.0" stroke-width="1.8"/><circle cx="499.7" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">1.091× [1.082, 1.099]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.080× [1.064, 1.092]</title><line x1="480.8" y1="137.0" x2="500.4" y2="137.0" stroke-width="2.2"/><line x1="480.8" y1="133.0" x2="480.8" y2="141.0" stroke-width="1.8"/><line x1="500.4" y1="133.0" x2="500.4" y2="141.0" stroke-width="1.8"/><circle cx="492.0" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">1.080× [1.064, 1.092]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">set100</text>
<text class="axis-text" x="175" y="161.0">N/A (excluded)</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="181.0">N/A (excluded)</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="201.0">N/A (excluded)</text>
<line class="section-divider" x1="8" y1="210" x2="1312" y2="210"/>
<text class="section-title" x="10" y="225">M1w8 (8 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.941× [0.930, 0.954]</title><line x1="379.3" y1="239.0" x2="398.5" y2="239.0" stroke-width="2.2"/><line x1="379.3" y1="235.0" x2="379.3" y2="243.0" stroke-width="1.8"/><line x1="398.5" y1="235.0" x2="398.5" y2="243.0" stroke-width="1.8"/><circle cx="388.1" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">0.941× [0.930, 0.954]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.113× [1.097, 1.128]</title><line x1="503.8" y1="259.0" x2="524.8" y2="259.0" stroke-width="2.2"/><line x1="503.8" y1="255.0" x2="503.8" y2="263.0" stroke-width="1.8"/><line x1="524.8" y1="255.0" x2="524.8" y2="263.0" stroke-width="1.8"/><circle cx="514.7" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">1.113× [1.097, 1.128]</text>
<text class="row-label" x="153" y="283.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.154× [1.139, 1.172]</title><line x1="532.1" y1="279.0" x2="553.7" y2="279.0" stroke-width="2.2"/><line x1="532.1" y1="275.0" x2="532.1" y2="283.0" stroke-width="1.8"/><line x1="553.7" y1="275.0" x2="553.7" y2="283.0" stroke-width="1.8"/><circle cx="542.0" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1137" y="283.0">1.154× [1.139, 1.172]</text>
<text class="row-label" x="153" y="303.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.112× [1.096, 1.129]</title><line x1="503.1" y1="299.0" x2="525.5" y2="299.0" stroke-width="2.2"/><line x1="503.1" y1="295.0" x2="503.1" y2="303.0" stroke-width="1.8"/><line x1="525.5" y1="295.0" x2="525.5" y2="303.0" stroke-width="1.8"/><circle cx="514.0" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1137" y="303.0">1.112× [1.096, 1.129]</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 1.137× [1.099, 1.168]</title><line x1="505.2" y1="319.0" x2="551.1" y2="319.0" stroke-width="2.2"/><line x1="505.2" y1="315.0" x2="505.2" y2="323.0" stroke-width="1.8"/><line x1="551.1" y1="315.0" x2="551.1" y2="323.0" stroke-width="1.8"/><circle cx="530.8" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">1.137× [1.099, 1.168]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="343.0">N/A (excluded)</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="363.0">N/A (excluded)</text>
<line class="section-divider" x1="8" y1="372" x2="1312" y2="372"/>
<text class="section-title" x="10" y="387">M1w64 (64 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="405.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.923× [0.907, 0.938]</title><line x1="360.4" y1="401.0" x2="385.7" y2="401.0" stroke-width="2.2"/><line x1="360.4" y1="397.0" x2="360.4" y2="405.0" stroke-width="1.8"/><line x1="385.7" y1="397.0" x2="385.7" y2="405.0" stroke-width="1.8"/><circle cx="373.6" cy="401.0" r="3.8"/></g>
<text class="val-text" x="1137" y="405.0">0.923× [0.907, 0.938]</text>
<text class="row-label" x="153" y="425.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.348× [1.308, 1.395]</title><line x1="636.5" y1="421.0" x2="685.0" y2="421.0" stroke-width="2.2"/><line x1="636.5" y1="417.0" x2="636.5" y2="425.0" stroke-width="1.8"/><line x1="685.0" y1="417.0" x2="685.0" y2="425.0" stroke-width="1.8"/><circle cx="659.2" cy="421.0" r="3.8"/></g>
<text class="val-text" x="1137" y="425.0">1.348× [1.308, 1.395]</text>
<text class="row-label" x="153" y="445.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.298× [1.254, 1.331]</title><line x1="604.7" y1="441.0" x2="649.6" y2="441.0" stroke-width="2.2"/><line x1="604.7" y1="437.0" x2="604.7" y2="445.0" stroke-width="1.8"/><line x1="649.6" y1="437.0" x2="649.6" y2="445.0" stroke-width="1.8"/><circle cx="630.7" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1137" y="445.0">1.298× [1.254, 1.331]</text>
<text class="row-label" x="153" y="465.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.435× [1.099, 2.378]</title><line x1="505.2" y1="461.0" x2="1087.3" y2="461.0" stroke-width="2.2"/><line x1="505.2" y1="457.0" x2="505.2" y2="465.0" stroke-width="1.8"/><line x1="1087.3" y1="457.0" x2="1087.3" y2="465.0" stroke-width="1.8"/><circle cx="706.4" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1137" y="465.0">1.435× [1.099, 2.378]</text>
<text class="row-label" x="153" y="485.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 1.283× [1.273, 1.294]</title><line x1="616.0" y1="481.0" x2="628.4" y2="481.0" stroke-width="2.2"/><line x1="616.0" y1="477.0" x2="616.0" y2="485.0" stroke-width="1.8"/><line x1="628.4" y1="477.0" x2="628.4" y2="485.0" stroke-width="1.8"/><circle cx="621.9" cy="481.0" r="3.8"/></g>
<text class="val-text" x="1137" y="485.0">1.283× [1.273, 1.294]</text>
<text class="row-label" x="153" y="505.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="505.0">N/A (excluded)</text>
<text class="row-label" x="153" y="525.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="525.0">N/A (excluded)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.2: p50 latency, master / CGo</strong><span>Below 1.0× means master has lower p50 than libfdb_c 7.3.77.</span></figcaption>
<p class="bench-summary">Master p50 is below CGo at 1 worker (0.636–0.697) and at 64 workers for get100, get1000 and set100 (0.770–0.934). At 8 workers the get1000 and set100 intervals include 1.0, and grv is above it (1.025).</p>
<svg class="bench-svg" viewBox="0 0 1320 420" role="img" aria-label="Figure M1.2: p50 latency, master / CGo">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="394"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="165.0" y="409" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="398.0" y1="36" x2="398.0" y2="394"/>
<text class="axis-text" x="398.0" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="398.0" y="409" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="645.0" y1="36" x2="645.0" y2="394"/>
<text class="axis-text" x="645.0" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="645.0" y="409" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="878.0" y1="36" x2="878.0" y2="394"/>
<text class="axis-text" x="878.0" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="878.0" y="409" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="394"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1125.0" y="409" text-anchor="middle">2.0×</text>
<text class="axis-text" x="637.0" y="17" text-anchor="end">← Master lower</text>
<text class="axis-text" x="653.0" y="17" text-anchor="start">CGo lower →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">M1w1 (1 worker, 4/7 clean)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.636× [0.616, 0.649]</title><line x1="309.5" y1="77.0" x2="345.6" y2="77.0" stroke-width="2.2"/><line x1="309.5" y1="73.0" x2="309.5" y2="81.0" stroke-width="1.8"/><line x1="345.6" y1="73.0" x2="345.6" y2="81.0" stroke-width="1.8"/><circle cx="331.6" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.636× [0.616, 0.649]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.675× [0.661, 0.690]</title><line x1="358.3" y1="97.0" x2="388.0" y2="97.0" stroke-width="2.2"/><line x1="358.3" y1="93.0" x2="358.3" y2="101.0" stroke-width="1.8"/><line x1="388.0" y1="93.0" x2="388.0" y2="101.0" stroke-width="1.8"/><circle cx="372.8" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.675× [0.661, 0.690]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.673× [0.666, 0.680]</title><line x1="363.5" y1="117.0" x2="377.9" y2="117.0" stroke-width="2.2"/><line x1="363.5" y1="113.0" x2="363.5" y2="121.0" stroke-width="1.8"/><line x1="377.9" y1="113.0" x2="377.9" y2="121.0" stroke-width="1.8"/><circle cx="370.8" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.673× [0.666, 0.680]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.697× [0.636, 0.738]</title><line x1="331.6" y1="137.0" x2="434.6" y2="137.0" stroke-width="2.2"/><line x1="331.6" y1="133.0" x2="331.6" y2="141.0" stroke-width="1.8"/><line x1="434.6" y1="133.0" x2="434.6" y2="141.0" stroke-width="1.8"/><circle cx="395.0" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.697× [0.636, 0.738]</text>
<line class="section-divider" x1="8" y1="150" x2="1312" y2="150"/>
<text class="section-title" x="10" y="165">M1w8 (8 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="183.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 1.025× [1.007, 1.045]</title><line x1="649.8" y1="179.0" x2="675.5" y2="179.0" stroke-width="2.2"/><line x1="649.8" y1="175.0" x2="649.8" y2="183.0" stroke-width="1.8"/><line x1="675.5" y1="175.0" x2="675.5" y2="183.0" stroke-width="1.8"/><circle cx="662.1" cy="179.0" r="3.8"/></g>
<text class="val-text" x="1137" y="183.0">1.025× [1.007, 1.045]</text>
<text class="row-label" x="153" y="203.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.977× [0.961, 0.992]</title><line x1="617.5" y1="199.0" x2="639.4" y2="199.0" stroke-width="2.2"/><line x1="617.5" y1="195.0" x2="617.5" y2="203.0" stroke-width="1.8"/><line x1="639.4" y1="195.0" x2="639.4" y2="203.0" stroke-width="1.8"/><circle cx="628.9" cy="199.0" r="3.8"/></g>
<text class="val-text" x="1137" y="203.0">0.977× [0.961, 0.992]</text>
<text class="row-label" x="153" y="223.0" text-anchor="end">get1000</text>
<g class="ci-neutral"><title>get1000: 1.003× [0.986, 1.021]</title><line x1="635.2" y1="219.0" x2="659.4" y2="219.0" stroke-width="2.2"/><line x1="635.2" y1="215.0" x2="635.2" y2="223.0" stroke-width="1.8"/><line x1="659.4" y1="215.0" x2="659.4" y2="223.0" stroke-width="1.8"/><circle cx="647.1" cy="219.0" r="3.8"/></g>
<text class="val-text" x="1137" y="223.0">1.003× [0.986, 1.021]</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.966× [0.949, 0.983]</title><line x1="608.8" y1="239.0" x2="633.1" y2="239.0" stroke-width="2.2"/><line x1="608.8" y1="235.0" x2="608.8" y2="243.0" stroke-width="1.8"/><line x1="633.1" y1="235.0" x2="633.1" y2="243.0" stroke-width="1.8"/><circle cx="621.0" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">0.966× [0.949, 0.983]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">set100</text>
<g class="ci-neutral"><title>set100: 1.013× [0.978, 1.047]</title><line x1="629.6" y1="259.0" x2="676.8" y2="259.0" stroke-width="2.2"/><line x1="629.6" y1="255.0" x2="629.6" y2="263.0" stroke-width="1.8"/><line x1="676.8" y1="255.0" x2="676.8" y2="263.0" stroke-width="1.8"/><circle cx="653.9" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">1.013× [0.978, 1.047]</text>
<line class="section-divider" x1="8" y1="272" x2="1312" y2="272"/>
<text class="section-title" x="10" y="287">M1w64 (64 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="305.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 1.001× [0.970, 1.040]</title><line x1="623.9" y1="301.0" x2="672.2" y2="301.0" stroke-width="2.2"/><line x1="623.9" y1="297.0" x2="623.9" y2="305.0" stroke-width="1.8"/><line x1="672.2" y1="297.0" x2="672.2" y2="305.0" stroke-width="1.8"/><circle cx="645.7" cy="301.0" r="3.8"/></g>
<text class="val-text" x="1137" y="305.0">1.001× [0.970, 1.040]</text>
<text class="row-label" x="153" y="325.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.934× [0.911, 0.966]</title><line x1="580.5" y1="321.0" x2="621.0" y2="321.0" stroke-width="2.2"/><line x1="580.5" y1="317.0" x2="580.5" y2="325.0" stroke-width="1.8"/><line x1="621.0" y1="317.0" x2="621.0" y2="325.0" stroke-width="1.8"/><circle cx="597.7" cy="321.0" r="3.8"/></g>
<text class="val-text" x="1137" y="325.0">0.934× [0.911, 0.966]</text>
<text class="row-label" x="153" y="345.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.770× [0.730, 0.821]</title><line x1="427.1" y1="341.0" x2="508.4" y2="341.0" stroke-width="2.2"/><line x1="427.1" y1="337.0" x2="427.1" y2="345.0" stroke-width="1.8"/><line x1="508.4" y1="337.0" x2="508.4" y2="345.0" stroke-width="1.8"/><circle cx="464.0" cy="341.0" r="3.8"/></g>
<text class="val-text" x="1137" y="345.0">0.770× [0.730, 0.821]</text>
<text class="row-label" x="153" y="365.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.867× [0.656, 1.480]</title><line x1="353.0" y1="361.0" x2="916.5" y2="361.0" stroke-width="2.2"/><line x1="353.0" y1="357.0" x2="353.0" y2="365.0" stroke-width="1.8"/><line x1="916.5" y1="357.0" x2="916.5" y2="365.0" stroke-width="1.8"/><circle cx="546.2" cy="361.0" r="3.8"/></g>
<text class="val-text" x="1137" y="365.0">0.867× [0.656, 1.480]</text>
<text class="row-label" x="153" y="385.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.893× [0.887, 0.899]</title><line x1="562.0" y1="381.0" x2="571.3" y2="381.0" stroke-width="2.2"/><line x1="562.0" y1="377.0" x2="562.0" y2="385.0" stroke-width="1.8"/><line x1="571.3" y1="377.0" x2="571.3" y2="385.0" stroke-width="1.8"/><circle cx="566.6" cy="381.0" r="3.8"/></g>
<text class="val-text" x="1137" y="385.0">0.893× [0.887, 0.899]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.3: Lifecycle CPU (E−A) per transaction</strong><span>Below 1.0× means master uses less process CPU. Includes startup, warmup, validation, and shutdown.</span></figcaption>
<p class="bench-summary">Master uses less CPU than old Go in every 8- and 64-worker cell except range100 at 64 workers, down to 0.553 (set100, 8 workers), and more at 1 worker (1.125–1.292). Against CGo it uses more on get, range and set at 8 and 64 workers (1.037–1.354) and less on grv.</p>
<svg class="bench-svg" viewBox="0 0 1320 906" role="img" aria-label="Figure M1.3: Lifecycle CPU (E−A) per transaction">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="880"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="165.0" y="895" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="398.0" y1="36" x2="398.0" y2="880"/>
<text class="axis-text" x="398.0" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="398.0" y="895" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="645.0" y1="36" x2="645.0" y2="880"/>
<text class="axis-text" x="645.0" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="645.0" y="895" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="878.0" y1="36" x2="878.0" y2="880"/>
<text class="axis-text" x="878.0" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="878.0" y="895" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="880"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1125.0" y="895" text-anchor="middle">2.0×</text>
<text class="axis-text" x="637.0" y="17" text-anchor="end">← Master less CPU</text>
<text class="axis-text" x="653.0" y="17" text-anchor="start">Denominator less CPU →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">master / old Go: M1w1 (1 worker, 4/7 clean)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 1.292× [1.255, 1.330]</title><line x1="802.3" y1="77.0" x2="842.5" y2="77.0" stroke-width="2.2"/><line x1="802.3" y1="73.0" x2="802.3" y2="81.0" stroke-width="1.8"/><line x1="842.5" y1="73.0" x2="842.5" y2="81.0" stroke-width="1.8"/><circle cx="822.4" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">1.292× [1.255, 1.330]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.190× [1.166, 1.211]</title><line x1="751.4" y1="97.0" x2="777.6" y2="97.0" stroke-width="2.2"/><line x1="751.4" y1="93.0" x2="751.4" y2="101.0" stroke-width="1.8"/><line x1="777.6" y1="93.0" x2="777.6" y2="101.0" stroke-width="1.8"/><circle cx="765.5" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">1.190× [1.166, 1.211]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.186× [1.166, 1.205]</title><line x1="751.4" y1="117.0" x2="774.1" y2="117.0" stroke-width="2.2"/><line x1="751.4" y1="113.0" x2="751.4" y2="121.0" stroke-width="1.8"/><line x1="774.1" y1="113.0" x2="774.1" y2="121.0" stroke-width="1.8"/><circle cx="763.1" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">1.186× [1.166, 1.205]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.125× [1.108, 1.138]</title><line x1="716.0" y1="137.0" x2="734.5" y2="137.0" stroke-width="2.2"/><line x1="716.0" y1="133.0" x2="716.0" y2="141.0" stroke-width="1.8"/><line x1="734.5" y1="133.0" x2="734.5" y2="141.0" stroke-width="1.8"/><circle cx="726.6" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">1.125× [1.108, 1.138]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">set100</text>
<text class="axis-text" x="175" y="161.0">N/A (excluded)</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="181.0">N/A (excluded)</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="201.0">N/A (excluded)</text>
<line class="section-divider" x1="8" y1="210" x2="1312" y2="210"/>
<text class="section-title" x="10" y="225">master / old Go: M1w8 (8 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.817× [0.792, 0.843]</title><line x1="483.5" y1="239.0" x2="526.7" y2="239.0" stroke-width="2.2"/><line x1="483.5" y1="235.0" x2="483.5" y2="243.0" stroke-width="1.8"/><line x1="526.7" y1="235.0" x2="526.7" y2="243.0" stroke-width="1.8"/><circle cx="505.0" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">0.817× [0.792, 0.843]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.762× [0.750, 0.774]</title><line x1="445.8" y1="259.0" x2="467.6" y2="259.0" stroke-width="2.2"/><line x1="445.8" y1="255.0" x2="445.8" y2="263.0" stroke-width="1.8"/><line x1="467.6" y1="255.0" x2="467.6" y2="263.0" stroke-width="1.8"/><circle cx="456.8" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">0.762× [0.750, 0.774]</text>
<text class="row-label" x="153" y="283.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.762× [0.745, 0.780]</title><line x1="441.1" y1="279.0" x2="472.9" y2="279.0" stroke-width="2.2"/><line x1="441.1" y1="275.0" x2="441.1" y2="283.0" stroke-width="1.8"/><line x1="472.9" y1="275.0" x2="472.9" y2="283.0" stroke-width="1.8"/><circle cx="456.8" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1137" y="283.0">0.762× [0.745, 0.780]</text>
<text class="row-label" x="153" y="303.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.865× [0.801, 0.977]</title><line x1="491.3" y1="299.0" x2="628.9" y2="299.0" stroke-width="2.2"/><line x1="491.3" y1="295.0" x2="491.3" y2="303.0" stroke-width="1.8"/><line x1="628.9" y1="295.0" x2="628.9" y2="303.0" stroke-width="1.8"/><circle cx="544.6" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1137" y="303.0">0.865× [0.801, 0.977]</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.553× [0.542, 0.565]</title><line x1="220.9" y1="319.0" x2="249.6" y2="319.0" stroke-width="2.2"/><line x1="220.9" y1="315.0" x2="220.9" y2="323.0" stroke-width="1.8"/><line x1="249.6" y1="315.0" x2="249.6" y2="323.0" stroke-width="1.8"/><circle cx="234.8" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">0.553× [0.542, 0.565]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="343.0">N/A (excluded)</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="363.0">N/A (excluded)</text>
<line class="section-divider" x1="8" y1="372" x2="1312" y2="372"/>
<text class="section-title" x="10" y="387">master / old Go: M1w64 (64 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="405.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.707× [0.692, 0.720]</title><line x1="390.0" y1="401.0" x2="417.5" y2="401.0" stroke-width="2.2"/><line x1="390.0" y1="397.0" x2="390.0" y2="405.0" stroke-width="1.8"/><line x1="417.5" y1="397.0" x2="417.5" y2="405.0" stroke-width="1.8"/><circle cx="404.9" cy="401.0" r="3.8"/></g>
<text class="val-text" x="1137" y="405.0">0.707× [0.692, 0.720]</text>
<text class="row-label" x="153" y="425.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.738× [0.719, 0.759]</title><line x1="416.6" y1="421.0" x2="454.0" y2="421.0" stroke-width="2.2"/><line x1="416.6" y1="417.0" x2="416.6" y2="425.0" stroke-width="1.8"/><line x1="454.0" y1="417.0" x2="454.0" y2="425.0" stroke-width="1.8"/><circle cx="434.6" cy="421.0" r="3.8"/></g>
<text class="val-text" x="1137" y="425.0">0.738× [0.719, 0.759]</text>
<text class="row-label" x="153" y="445.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.742× [0.716, 0.771]</title><line x1="413.7" y1="441.0" x2="464.9" y2="441.0" stroke-width="2.2"/><line x1="413.7" y1="437.0" x2="413.7" y2="445.0" stroke-width="1.8"/><line x1="464.9" y1="437.0" x2="464.9" y2="445.0" stroke-width="1.8"/><circle cx="438.4" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1137" y="445.0">0.742× [0.716, 0.771]</text>
<text class="row-label" x="153" y="465.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 1.180× [0.999, 1.567]</title><line x1="644.3" y1="461.0" x2="956.0" y2="461.0" stroke-width="2.2"/><line x1="644.3" y1="457.0" x2="644.3" y2="465.0" stroke-width="1.8"/><line x1="956.0" y1="457.0" x2="956.0" y2="465.0" stroke-width="1.8"/><circle cx="759.6" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1137" y="465.0">1.180× [0.999, 1.567]</text>
<text class="row-label" x="153" y="485.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.586× [0.572, 0.599]</title><line x1="258.2" y1="481.0" x2="290.1" y2="481.0" stroke-width="2.2"/><line x1="258.2" y1="477.0" x2="258.2" y2="485.0" stroke-width="1.8"/><line x1="290.1" y1="477.0" x2="290.1" y2="485.0" stroke-width="1.8"/><circle cx="274.9" cy="481.0" r="3.8"/></g>
<text class="val-text" x="1137" y="485.0">0.586× [0.572, 0.599]</text>
<text class="row-label" x="153" y="505.0" text-anchor="end">batchset50</text>
<text class="axis-text" x="175" y="505.0">N/A (excluded)</text>
<text class="row-label" x="153" y="525.0" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="175" y="525.0">N/A (excluded)</text>
<line class="section-divider" x1="8" y1="534" x2="1312" y2="534"/>
<text class="section-title" x="10" y="549">master / CGo: M1w1 (1 worker, 4/7 clean)</text>
<text class="row-label" x="153" y="567.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.783× [0.768, 0.797]</title><line x1="462.2" y1="563.0" x2="487.9" y2="563.0" stroke-width="2.2"/><line x1="462.2" y1="559.0" x2="462.2" y2="567.0" stroke-width="1.8"/><line x1="487.9" y1="559.0" x2="487.9" y2="567.0" stroke-width="1.8"/><circle cx="475.6" cy="563.0" r="3.8"/></g>
<text class="val-text" x="1137" y="567.0">0.783× [0.768, 0.797]</text>
<text class="row-label" x="153" y="587.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.806× [0.779, 0.833]</title><line x1="472.1" y1="583.0" x2="518.5" y2="583.0" stroke-width="2.2"/><line x1="472.1" y1="579.0" x2="472.1" y2="587.0" stroke-width="1.8"/><line x1="518.5" y1="579.0" x2="518.5" y2="587.0" stroke-width="1.8"/><circle cx="495.6" cy="583.0" r="3.8"/></g>
<text class="val-text" x="1137" y="587.0">0.806× [0.779, 0.833]</text>
<text class="row-label" x="153" y="607.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.800× [0.783, 0.821]</title><line x1="475.6" y1="603.0" x2="508.4" y2="603.0" stroke-width="2.2"/><line x1="475.6" y1="599.0" x2="475.6" y2="607.0" stroke-width="1.8"/><line x1="508.4" y1="599.0" x2="508.4" y2="607.0" stroke-width="1.8"/><circle cx="490.5" cy="603.0" r="3.8"/></g>
<text class="val-text" x="1137" y="607.0">0.800× [0.783, 0.821]</text>
<text class="row-label" x="153" y="627.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.908× [0.893, 0.921]</title><line x1="566.6" y1="623.0" x2="588.0" y2="623.0" stroke-width="2.2"/><line x1="566.6" y1="619.0" x2="566.6" y2="627.0" stroke-width="1.8"/><line x1="588.0" y1="619.0" x2="588.0" y2="627.0" stroke-width="1.8"/><circle cx="578.2" cy="623.0" r="3.8"/></g>
<text class="val-text" x="1137" y="627.0">0.908× [0.893, 0.921]</text>
<line class="section-divider" x1="8" y1="636" x2="1312" y2="636"/>
<text class="section-title" x="10" y="651">master / CGo: M1w8 (8 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="669.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.728× [0.715, 0.740]</title><line x1="412.7" y1="665.0" x2="436.5" y2="665.0" stroke-width="2.2"/><line x1="412.7" y1="661.0" x2="412.7" y2="669.0" stroke-width="1.8"/><line x1="436.5" y1="661.0" x2="436.5" y2="669.0" stroke-width="1.8"/><circle cx="425.2" cy="665.0" r="3.8"/></g>
<text class="val-text" x="1137" y="669.0">0.728× [0.715, 0.740]</text>
<text class="row-label" x="153" y="689.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.037× [1.027, 1.046]</title><line x1="663.4" y1="685.0" x2="676.1" y2="685.0" stroke-width="2.2"/><line x1="663.4" y1="681.0" x2="663.4" y2="689.0" stroke-width="1.8"/><line x1="676.1" y1="681.0" x2="676.1" y2="689.0" stroke-width="1.8"/><circle cx="670.2" cy="685.0" r="3.8"/></g>
<text class="val-text" x="1137" y="689.0">1.037× [1.027, 1.046]</text>
<text class="row-label" x="153" y="709.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.133× [1.096, 1.182]</title><line x1="708.5" y1="705.0" x2="760.8" y2="705.0" stroke-width="2.2"/><line x1="708.5" y1="701.0" x2="708.5" y2="709.0" stroke-width="1.8"/><line x1="760.8" y1="701.0" x2="760.8" y2="709.0" stroke-width="1.8"/><circle cx="731.5" cy="705.0" r="3.8"/></g>
<text class="val-text" x="1137" y="709.0">1.133× [1.096, 1.182]</text>
<text class="row-label" x="153" y="729.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.354× [1.268, 1.513]</title><line x1="809.4" y1="725.0" x2="931.8" y2="725.0" stroke-width="2.2"/><line x1="809.4" y1="721.0" x2="809.4" y2="729.0" stroke-width="1.8"/><line x1="931.8" y1="721.0" x2="931.8" y2="729.0" stroke-width="1.8"/><circle cx="854.9" cy="725.0" r="3.8"/></g>
<text class="val-text" x="1137" y="729.0">1.354× [1.268, 1.513]</text>
<text class="row-label" x="153" y="749.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 1.148× [1.119, 1.179]</title><line x1="722.9" y1="745.0" x2="759.0" y2="745.0" stroke-width="2.2"/><line x1="722.9" y1="741.0" x2="722.9" y2="749.0" stroke-width="1.8"/><line x1="759.0" y1="741.0" x2="759.0" y2="749.0" stroke-width="1.8"/><circle cx="740.6" cy="745.0" r="3.8"/></g>
<text class="val-text" x="1137" y="749.0">1.148× [1.119, 1.179]</text>
<line class="section-divider" x1="8" y1="758" x2="1312" y2="758"/>
<text class="section-title" x="10" y="773">master / CGo: M1w64 (64 workers, 5/7 clean)</text>
<text class="row-label" x="153" y="791.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.565× [0.550, 0.581]</title><line x1="231.0" y1="787.0" x2="269.0" y2="787.0" stroke-width="2.2"/><line x1="231.0" y1="783.0" x2="231.0" y2="791.0" stroke-width="1.8"/><line x1="269.0" y1="783.0" x2="269.0" y2="791.0" stroke-width="1.8"/><circle cx="249.6" cy="787.0" r="3.8"/></g>
<text class="val-text" x="1137" y="791.0">0.565× [0.550, 0.581]</text>
<text class="row-label" x="153" y="811.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.135× [1.082, 1.203]</title><line x1="699.6" y1="807.0" x2="773.0" y2="807.0" stroke-width="2.2"/><line x1="699.6" y1="803.0" x2="699.6" y2="811.0" stroke-width="1.8"/><line x1="773.0" y1="803.0" x2="773.0" y2="811.0" stroke-width="1.8"/><circle cx="732.7" cy="807.0" r="3.8"/></g>
<text class="val-text" x="1137" y="811.0">1.135× [1.082, 1.203]</text>
<text class="row-label" x="153" y="831.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.156× [1.111, 1.197]</title><line x1="717.9" y1="827.0" x2="769.5" y2="827.0" stroke-width="2.2"/><line x1="717.9" y1="823.0" x2="717.9" y2="831.0" stroke-width="1.8"/><line x1="769.5" y1="823.0" x2="769.5" y2="831.0" stroke-width="1.8"/><circle cx="745.4" cy="827.0" r="3.8"/></g>
<text class="val-text" x="1137" y="831.0">1.156× [1.111, 1.197]</text>
<text class="row-label" x="153" y="851.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.275× [1.061, 1.731]</title><line x1="686.0" y1="847.0" x2="1025.0" y2="847.0" stroke-width="2.2"/><line x1="686.0" y1="843.0" x2="686.0" y2="851.0" stroke-width="1.8"/><line x1="1025.0" y1="843.0" x2="1025.0" y2="851.0" stroke-width="1.8"/><circle cx="813.2" cy="847.0" r="3.8"/></g>
<text class="val-text" x="1137" y="851.0">1.275× [1.061, 1.731]</text>
<text class="row-label" x="153" y="871.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 1.249× [1.213, 1.284]</title><line x1="778.7" y1="867.0" x2="818.1" y2="867.0" stroke-width="2.2"/><line x1="778.7" y1="863.0" x2="778.7" y2="871.0" stroke-width="1.8"/><line x1="818.1" y1="863.0" x2="818.1" y2="871.0" stroke-width="1.8"/><circle cx="799.0" cy="867.0" r="3.8"/></g>
<text class="val-text" x="1137" y="871.0">1.249× [1.213, 1.284]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.4: p50 and p99 latency at 8 and 64 workers</strong><span>Bar is p50, whisker is p99, in µs per transaction (medians of six runs).</span></figcaption>
<p class="bench-summary">Each worker count ran as a separate phase with its own transaction count, so compare arms within a count.</p>
<svg class="bench-svg" viewBox="0 0 1355 496" role="img" aria-label="Figure M1.4: p50 and p99 latency at 8 and 64 workers">
<line class="grid-line" x1="165.0" y1="22" x2="165.0" y2="472"/>
<text class="axis-text" x="165.0" y="487" text-anchor="middle">0 µs</text>
<line class="grid-line" x1="302.1" y1="22" x2="302.1" y2="472"/>
<text class="axis-text" x="302.1" y="487" text-anchor="middle">2000 µs</text>
<line class="grid-line" x1="439.3" y1="22" x2="439.3" y2="472"/>
<text class="axis-text" x="439.3" y="487" text-anchor="middle">4000 µs</text>
<line class="grid-line" x1="576.4" y1="22" x2="576.4" y2="472"/>
<text class="axis-text" x="576.4" y="487" text-anchor="middle">6000 µs</text>
<line class="grid-line" x1="713.6" y1="22" x2="713.6" y2="472"/>
<text class="axis-text" x="713.6" y="487" text-anchor="middle">8000 µs</text>
<line class="grid-line" x1="850.7" y1="22" x2="850.7" y2="472"/>
<text class="axis-text" x="850.7" y="487" text-anchor="middle">10000 µs</text>
<line class="grid-line" x1="987.9" y1="22" x2="987.9" y2="472"/>
<text class="axis-text" x="987.9" y="487" text-anchor="middle">12000 µs</text>
<line class="grid-line" x1="1125.0" y1="22" x2="1125.0" y2="472"/>
<text class="axis-text" x="1125.0" y="487" text-anchor="middle">14000 µs</text>
<text class="row-label" x="153" y="50.0" text-anchor="end">M1w8 grv</text>
<g class="ci-win"><title>M1w8 grv master: p50 1745.5 µs, p99 2733.4 µs</title><rect class="bar-go" x="165" y="32" width="119.7" height="9" rx="2"/><line x1="284.7" y1="36.5" x2="352.4" y2="36.5" stroke-width="1.6"/><line x1="352.4" y1="32.5" x2="352.4" y2="40.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="41.0">master 1746 / p99 2733 µs</text>
<g class="ci-neutral"><title>M1w8 grv old Go: p50 1848.9 µs, p99 2937.6 µs</title><rect class="bar-before" x="165" y="44" width="126.8" height="9" rx="2"/><line x1="291.8" y1="48.5" x2="366.4" y2="48.5" stroke-width="1.6"/><line x1="366.4" y1="44.5" x2="366.4" y2="52.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="53.0">old Go 1849 / p99 2938 µs</text>
<g class="ci-reg"><title>M1w8 grv CGo: p50 1704.8 µs, p99 2890.4 µs</title><rect class="bar-cgo" x="165" y="56" width="116.9" height="9" rx="2"/><line x1="281.9" y1="60.5" x2="363.2" y2="60.5" stroke-width="1.6"/><line x1="363.2" y1="56.5" x2="363.2" y2="64.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="65.0">CGo 1705 / p99 2890 µs</text>
<text class="row-label" x="153" y="94.0" text-anchor="end">M1w8 get100</text>
<g class="ci-win"><title>M1w8 get100 master: p50 2317.0 µs, p99 3663.5 µs</title><rect class="bar-go" x="165" y="76" width="158.9" height="9" rx="2"/><line x1="323.9" y1="80.5" x2="416.2" y2="80.5" stroke-width="1.6"/><line x1="416.2" y1="76.5" x2="416.2" y2="84.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="85.0">master 2317 / p99 3664 µs</text>
<g class="ci-neutral"><title>M1w8 get100 old Go: p50 2058.8 µs, p99 3597.4 µs</title><rect class="bar-before" x="165" y="88" width="141.2" height="9" rx="2"/><line x1="306.2" y1="92.5" x2="411.7" y2="92.5" stroke-width="1.6"/><line x1="411.7" y1="88.5" x2="411.7" y2="96.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="97.0">old Go 2059 / p99 3597 µs</text>
<g class="ci-reg"><title>M1w8 get100 CGo: p50 2354.6 µs, p99 3978.9 µs</title><rect class="bar-cgo" x="165" y="100" width="161.5" height="9" rx="2"/><line x1="326.5" y1="104.5" x2="437.8" y2="104.5" stroke-width="1.6"/><line x1="437.8" y1="100.5" x2="437.8" y2="108.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="109.0">CGo 2355 / p99 3979 µs</text>
<text class="row-label" x="153" y="138.0" text-anchor="end">M1w8 get1000</text>
<g class="ci-win"><title>M1w8 get1000 master: p50 2400.3 µs, p99 3954.8 µs</title><rect class="bar-go" x="165" y="120" width="164.6" height="9" rx="2"/><line x1="329.6" y1="124.5" x2="436.2" y2="124.5" stroke-width="1.6"/><line x1="436.2" y1="120.5" x2="436.2" y2="128.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="129.0">master 2400 / p99 3955 µs</text>
<g class="ci-neutral"><title>M1w8 get1000 old Go: p50 2078.9 µs, p99 3287.3 µs</title><rect class="bar-before" x="165" y="132" width="142.6" height="9" rx="2"/><line x1="307.6" y1="136.5" x2="390.4" y2="136.5" stroke-width="1.6"/><line x1="390.4" y1="132.5" x2="390.4" y2="140.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="141.0">old Go 2079 / p99 3287 µs</text>
<g class="ci-reg"><title>M1w8 get1000 CGo: p50 2413.0 µs, p99 4166.0 µs</title><rect class="bar-cgo" x="165" y="144" width="165.5" height="9" rx="2"/><line x1="330.5" y1="148.5" x2="450.7" y2="148.5" stroke-width="1.6"/><line x1="450.7" y1="144.5" x2="450.7" y2="152.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="153.0">CGo 2413 / p99 4166 µs</text>
<text class="row-label" x="153" y="182.0" text-anchor="end">M1w8 range100</text>
<g class="ci-win"><title>M1w8 range100 master: p50 2911.2 µs, p99 4832.3 µs</title><rect class="bar-go" x="165" y="164" width="199.6" height="9" rx="2"/><line x1="364.6" y1="168.5" x2="496.4" y2="168.5" stroke-width="1.6"/><line x1="496.4" y1="164.5" x2="496.4" y2="172.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="173.0">master 2911 / p99 4832 µs</text>
<g class="ci-neutral"><title>M1w8 range100 old Go: p50 2638.5 µs, p99 4681.3 µs</title><rect class="bar-before" x="165" y="176" width="180.9" height="9" rx="2"/><line x1="345.9" y1="180.5" x2="486.0" y2="180.5" stroke-width="1.6"/><line x1="486.0" y1="176.5" x2="486.0" y2="184.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="185.0">old Go 2638 / p99 4681 µs</text>
<g class="ci-reg"><title>M1w8 range100 CGo: p50 3020.2 µs, p99 4714.7 µs</title><rect class="bar-cgo" x="165" y="188" width="207.1" height="9" rx="2"/><line x1="372.1" y1="192.5" x2="488.3" y2="192.5" stroke-width="1.6"/><line x1="488.3" y1="188.5" x2="488.3" y2="196.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="197.0">CGo 3020 / p99 4715 µs</text>
<text class="row-label" x="153" y="226.0" text-anchor="end">M1w8 set100</text>
<g class="ci-win"><title>M1w8 set100 master: p50 4430.7 µs, p99 6440.9 µs</title><rect class="bar-go" x="165" y="208" width="303.8" height="9" rx="2"/><line x1="468.8" y1="212.5" x2="606.7" y2="212.5" stroke-width="1.6"/><line x1="606.7" y1="208.5" x2="606.7" y2="216.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="217.0">master 4431 / p99 6441 µs</text>
<g class="ci-neutral"><title>M1w8 set100 old Go: p50 3844.9 µs, p99 5662.3 µs</title><rect class="bar-before" x="165" y="220" width="263.7" height="9" rx="2"/><line x1="428.7" y1="224.5" x2="553.3" y2="224.5" stroke-width="1.6"/><line x1="553.3" y1="220.5" x2="553.3" y2="228.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="229.0">old Go 3845 / p99 5662 µs</text>
<g class="ci-reg"><title>M1w8 set100 CGo: p50 4357.0 µs, p99 6731.7 µs</title><rect class="bar-cgo" x="165" y="232" width="298.8" height="9" rx="2"/><line x1="463.8" y1="236.5" x2="626.6" y2="236.5" stroke-width="1.6"/><line x1="626.6" y1="232.5" x2="626.6" y2="240.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="241.0">CGo 4357 / p99 6732 µs</text>
<text class="row-label" x="153" y="270.0" text-anchor="end">M1w64 grv</text>
<g class="ci-win"><title>M1w64 grv master: p50 1884.8 µs, p99 3418.3 µs</title><rect class="bar-go" x="165" y="252" width="129.2" height="9" rx="2"/><line x1="294.2" y1="256.5" x2="399.4" y2="256.5" stroke-width="1.6"/><line x1="399.4" y1="252.5" x2="399.4" y2="260.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="261.0">master 1885 / p99 3418 µs</text>
<g class="ci-neutral"><title>M1w64 grv old Go: p50 2054.9 µs, p99 3844.9 µs</title><rect class="bar-before" x="165" y="264" width="140.9" height="9" rx="2"/><line x1="305.9" y1="268.5" x2="428.7" y2="268.5" stroke-width="1.6"/><line x1="428.7" y1="264.5" x2="428.7" y2="272.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="273.0">old Go 2055 / p99 3845 µs</text>
<g class="ci-reg"><title>M1w64 grv CGo: p50 1909.2 µs, p99 3612.3 µs</title><rect class="bar-cgo" x="165" y="276" width="130.9" height="9" rx="2"/><line x1="295.9" y1="280.5" x2="412.7" y2="280.5" stroke-width="1.6"/><line x1="412.7" y1="276.5" x2="412.7" y2="284.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="285.0">CGo 1909 / p99 3612 µs</text>
<text class="row-label" x="153" y="314.0" text-anchor="end">M1w64 get100</text>
<g class="ci-win"><title>M1w64 get100 master: p50 2922.1 µs, p99 4812.1 µs</title><rect class="bar-go" x="165" y="296" width="200.4" height="9" rx="2"/><line x1="365.4" y1="300.5" x2="495.0" y2="300.5" stroke-width="1.6"/><line x1="495.0" y1="296.5" x2="495.0" y2="304.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="305.0">master 2922 / p99 4812 µs</text>
<g class="ci-neutral"><title>M1w64 get100 old Go: p50 2216.8 µs, p99 4230.2 µs</title><rect class="bar-before" x="165" y="308" width="152.0" height="9" rx="2"/><line x1="317.0" y1="312.5" x2="455.1" y2="312.5" stroke-width="1.6"/><line x1="455.1" y1="308.5" x2="455.1" y2="316.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="317.0">old Go 2217 / p99 4230 µs</text>
<g class="ci-reg"><title>M1w64 get100 CGo: p50 3195.9 µs, p99 6104.8 µs</title><rect class="bar-cgo" x="165" y="320" width="219.1" height="9" rx="2"/><line x1="384.1" y1="324.5" x2="583.6" y2="324.5" stroke-width="1.6"/><line x1="583.6" y1="320.5" x2="583.6" y2="328.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="329.0">CGo 3196 / p99 6105 µs</text>
<text class="row-label" x="153" y="358.0" text-anchor="end">M1w64 get1000</text>
<g class="ci-win"><title>M1w64 get1000 master: p50 3039.8 µs, p99 5019.0 µs</title><rect class="bar-go" x="165" y="340" width="208.4" height="9" rx="2"/><line x1="373.4" y1="344.5" x2="509.2" y2="344.5" stroke-width="1.6"/><line x1="509.2" y1="340.5" x2="509.2" y2="348.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="349.0">master 3040 / p99 5019 µs</text>
<g class="ci-neutral"><title>M1w64 get1000 old Go: p50 2350.7 µs, p99 4753.0 µs</title><rect class="bar-before" x="165" y="352" width="161.2" height="9" rx="2"/><line x1="326.2" y1="356.5" x2="490.9" y2="356.5" stroke-width="1.6"/><line x1="490.9" y1="352.5" x2="490.9" y2="360.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="361.0">old Go 2351 / p99 4753 µs</text>
<g class="ci-reg"><title>M1w64 get1000 CGo: p50 3964.5 µs, p99 13230.4 µs</title><rect class="bar-cgo" x="165" y="364" width="271.9" height="9" rx="2"/><line x1="436.9" y1="368.5" x2="1072.2" y2="368.5" stroke-width="1.6"/><line x1="1072.2" y1="364.5" x2="1072.2" y2="372.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="373.0">CGo 3964 / p99 13230 µs</text>
<text class="row-label" x="153" y="402.0" text-anchor="end">M1w64 range100</text>
<g class="ci-win"><title>M1w64 range100 master: p50 3425.4 µs, p99 6721.9 µs</title><rect class="bar-go" x="165" y="384" width="234.9" height="9" rx="2"/><line x1="399.9" y1="388.5" x2="625.9" y2="388.5" stroke-width="1.6"/><line x1="625.9" y1="384.5" x2="625.9" y2="392.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="393.0">master 3425 / p99 6722 µs</text>
<g class="ci-neutral"><title>M1w64 range100 old Go: p50 3101.4 µs, p99 7527.6 µs</title><rect class="bar-before" x="165" y="396" width="212.7" height="9" rx="2"/><line x1="377.7" y1="400.5" x2="681.2" y2="400.5" stroke-width="1.6"/><line x1="681.2" y1="396.5" x2="681.2" y2="404.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="405.0">old Go 3101 / p99 7528 µs</text>
<g class="ci-reg"><title>M1w64 range100 CGo: p50 5130.3 µs, p99 9909.4 µs</title><rect class="bar-cgo" x="165" y="408" width="351.8" height="9" rx="2"/><line x1="516.8" y1="412.5" x2="844.5" y2="412.5" stroke-width="1.6"/><line x1="844.5" y1="408.5" x2="844.5" y2="416.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="417.0">CGo 5130 / p99 9909 µs</text>
<text class="row-label" x="153" y="446.0" text-anchor="end">M1w64 set100</text>
<g class="ci-win"><title>M1w64 set100 master: p50 5383.5 µs, p99 8122.7 µs</title><rect class="bar-go" x="165" y="428" width="369.2" height="9" rx="2"/><line x1="534.2" y1="432.5" x2="722.0" y2="432.5" stroke-width="1.6"/><line x1="722.0" y1="428.5" x2="722.0" y2="436.5" stroke-width="1.8"/></g>
<text class="val-text" x="1135" y="437.0">master 5384 / p99 8123 µs</text>
<g class="ci-neutral"><title>M1w64 set100 old Go: p50 4186.4 µs, p99 7382.2 µs</title><rect class="bar-before" x="165" y="440" width="287.1" height="9" rx="2"/><line x1="452.1" y1="444.5" x2="671.2" y2="444.5" stroke-width="1.6"/><line x1="671.2" y1="440.5" x2="671.2" y2="448.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="449.0">old Go 4186 / p99 7382 µs</text>
<g class="ci-reg"><title>M1w64 set100 CGo: p50 5993.7 µs, p99 9176.0 µs</title><rect class="bar-cgo" x="165" y="452" width="411.0" height="9" rx="2"/><line x1="576.0" y1="456.5" x2="794.2" y2="456.5" stroke-width="1.6"/><line x1="794.2" y1="452.5" x2="794.2" y2="460.5" stroke-width="1.8"/></g>
<text class="axis-text" x="1135" y="461.0">CGo 5994 / p99 9176 µs</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> master</span><span><i class="bench-swatch" style="background:#64748b"></i> old Go</span><span><i class="bench-swatch" style="background:#d97706"></i> CGo</span></div>
</figure>

Geometric mean of six paired block ratios, with nominal 95% block-bootstrap
intervals (20,000 resamples, preregistered seed 492817). Below 1 means the
numerator has lower latency, CPU or allocation.

On Linux, master's pure-Go client coalesces outgoing requests before a write
batch, using the same 10 µs minimum and 20 µs maximum delay settings as
`libfdb_c` 7.3.77. The delay depends on time since the last write and is
jittered. [`fdb.WithCoalesceDelay(minDelay, maxDelay time.Duration)`](https://github.com/birdayz/fdb-go/blob/master/pkg/fdbgo/fdb/database.go)
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
[`analysis/M1-comparisons.csv`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/fdbmn-20261020/analysis/M1-comparisons.csv).

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

## Throughput (post-hoc, exploratory)

<figure class="bench-figure">
<figcaption><strong>Figure M1.5: Throughput, operations per second (post-hoc)</strong><span>Above 1.0× means master completes more operations per second. Post-hoc, not preregistered.</span></figcaption>
<p class="bench-summary">At 8 and 64 workers master completes fewer operations per second than old Go on get, range and set (0.718–0.903), and more on grv at 64 workers (1.057). Against CGo its rate is higher or the interval includes 1.0 in every cell except grv at 8 workers (0.979).</p>
<svg class="bench-svg" viewBox="0 0 1320 766" role="img" aria-label="Figure M1.5: Throughput, operations per second (post-hoc)">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="740"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.4×</text>
<text class="axis-text" x="165.0" y="755" text-anchor="middle">0.4×</text>
<line class="grid-line" x1="498.8" y1="36" x2="498.8" y2="740"/>
<text class="axis-text" x="498.8" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="498.8" y="755" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="711.6" y1="36" x2="711.6" y2="740"/>
<text class="axis-text" x="711.6" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="711.6" y="755" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="912.2" y1="36" x2="912.2" y2="740"/>
<text class="axis-text" x="912.2" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="912.2" y="755" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="740"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1125.0" y="755" text-anchor="middle">2.0×</text>
<text class="axis-text" x="703.6" y="17" text-anchor="end">← Denominator more ops/s</text>
<text class="axis-text" x="719.6" y="17" text-anchor="start">Master more ops/s →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">master / old Go: M1w1 (1 worker)</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 0.956× [0.929, 0.998]</title><line x1="667.6" y1="77.0" x2="710.4" y2="77.0" stroke-width="2.2"/><line x1="667.6" y1="73.0" x2="667.6" y2="81.0" stroke-width="1.8"/><line x1="710.4" y1="73.0" x2="710.4" y2="81.0" stroke-width="1.8"/><circle cx="684.7" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.956× [0.929, 0.998]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 0.914× [0.883, 0.942]</title><line x1="637.3" y1="97.0" x2="675.9" y2="97.0" stroke-width="2.2"/><line x1="637.3" y1="93.0" x2="637.3" y2="101.0" stroke-width="1.8"/><line x1="675.9" y1="93.0" x2="675.9" y2="101.0" stroke-width="1.8"/><circle cx="657.9" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.914× [0.883, 0.942]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 0.919× [0.907, 0.934]</title><line x1="653.3" y1="117.0" x2="670.8" y2="117.0" stroke-width="2.2"/><line x1="653.3" y1="113.0" x2="653.3" y2="121.0" stroke-width="1.8"/><line x1="670.8" y1="113.0" x2="670.8" y2="121.0" stroke-width="1.8"/><circle cx="661.2" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.919× [0.907, 0.934]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 0.929× [0.917, 0.942]</title><line x1="659.9" y1="137.0" x2="675.9" y2="137.0" stroke-width="2.2"/><line x1="659.9" y1="133.0" x2="659.9" y2="141.0" stroke-width="1.8"/><line x1="675.9" y1="133.0" x2="675.9" y2="141.0" stroke-width="1.8"/><circle cx="667.6" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.929× [0.917, 0.942]</text>
<line class="section-divider" x1="8" y1="150" x2="1312" y2="150"/>
<text class="section-title" x="10" y="165">master / old Go: M1w8 (8 workers)</text>
<text class="row-label" x="153" y="183.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 0.984× [0.970, 0.996]</title><line x1="693.4" y1="179.0" x2="709.2" y2="179.0" stroke-width="2.2"/><line x1="693.4" y1="175.0" x2="693.4" y2="183.0" stroke-width="1.8"/><line x1="709.2" y1="175.0" x2="709.2" y2="183.0" stroke-width="1.8"/><circle cx="701.9" cy="179.0" r="3.8"/></g>
<text class="val-text" x="1137" y="183.0">0.984× [0.970, 0.996]</text>
<text class="row-label" x="153" y="203.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 0.903× [0.885, 0.920]</title><line x1="638.7" y1="199.0" x2="661.8" y2="199.0" stroke-width="2.2"/><line x1="638.7" y1="195.0" x2="638.7" y2="203.0" stroke-width="1.8"/><line x1="661.8" y1="195.0" x2="661.8" y2="203.0" stroke-width="1.8"/><circle cx="650.7" cy="199.0" r="3.8"/></g>
<text class="val-text" x="1137" y="203.0">0.903× [0.885, 0.920]</text>
<text class="row-label" x="153" y="223.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 0.875× [0.867, 0.883]</title><line x1="626.4" y1="219.0" x2="637.3" y2="219.0" stroke-width="2.2"/><line x1="626.4" y1="215.0" x2="626.4" y2="223.0" stroke-width="1.8"/><line x1="637.3" y1="215.0" x2="637.3" y2="223.0" stroke-width="1.8"/><circle cx="631.9" cy="219.0" r="3.8"/></g>
<text class="val-text" x="1137" y="223.0">0.875× [0.867, 0.883]</text>
<text class="row-label" x="153" y="243.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 0.897× [0.881, 0.911]</title><line x1="636.0" y1="239.0" x2="656.0" y2="239.0" stroke-width="2.2"/><line x1="636.0" y1="235.0" x2="636.0" y2="243.0" stroke-width="1.8"/><line x1="656.0" y1="235.0" x2="656.0" y2="243.0" stroke-width="1.8"/><circle cx="646.7" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1137" y="243.0">0.897× [0.881, 0.911]</text>
<text class="row-label" x="153" y="263.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 0.854× [0.828, 0.882]</title><line x1="599.0" y1="259.0" x2="636.7" y2="259.0" stroke-width="2.2"/><line x1="599.0" y1="255.0" x2="599.0" y2="263.0" stroke-width="1.8"/><line x1="636.7" y1="255.0" x2="636.7" y2="263.0" stroke-width="1.8"/><circle cx="617.4" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1137" y="263.0">0.854× [0.828, 0.882]</text>
<line class="section-divider" x1="8" y1="272" x2="1312" y2="272"/>
<text class="section-title" x="10" y="287">master / old Go: M1w64 (64 workers)</text>
<text class="row-label" x="153" y="305.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 1.057× [1.036, 1.080]</title><line x1="732.6" y1="301.0" x2="757.5" y2="301.0" stroke-width="2.2"/><line x1="732.6" y1="297.0" x2="732.6" y2="305.0" stroke-width="1.8"/><line x1="757.5" y1="297.0" x2="757.5" y2="305.0" stroke-width="1.8"/><circle cx="744.6" cy="301.0" r="3.8"/></g>
<text class="val-text" x="1137" y="305.0">1.057× [1.036, 1.080]</text>
<text class="row-label" x="153" y="325.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 0.769× [0.745, 0.792]</title><line x1="536.0" y1="321.0" x2="572.5" y2="321.0" stroke-width="2.2"/><line x1="536.0" y1="317.0" x2="536.0" y2="325.0" stroke-width="1.8"/><line x1="572.5" y1="317.0" x2="572.5" y2="325.0" stroke-width="1.8"/><circle cx="554.9" cy="321.0" r="3.8"/></g>
<text class="val-text" x="1137" y="325.0">0.769× [0.745, 0.792]</text>
<text class="row-label" x="153" y="345.0" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 0.798× [0.779, 0.819]</title><line x1="562.6" y1="341.0" x2="592.5" y2="341.0" stroke-width="2.2"/><line x1="562.6" y1="337.0" x2="562.6" y2="345.0" stroke-width="1.8"/><line x1="592.5" y1="337.0" x2="592.5" y2="345.0" stroke-width="1.8"/><circle cx="577.0" cy="341.0" r="3.8"/></g>
<text class="val-text" x="1137" y="345.0">0.798× [0.779, 0.819]</text>
<text class="row-label" x="153" y="365.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 0.718× [0.440, 0.925]</title><line x1="221.9" y1="361.0" x2="665.0" y2="361.0" stroke-width="2.2"/><line x1="221.9" y1="357.0" x2="221.9" y2="365.0" stroke-width="1.8"/><line x1="665.0" y1="357.0" x2="665.0" y2="365.0" stroke-width="1.8"/><circle cx="513.9" cy="361.0" r="3.8"/></g>
<text class="val-text" x="1137" y="365.0">0.718× [0.440, 0.925]</text>
<text class="row-label" x="153" y="385.0" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 0.796× [0.787, 0.804]</title><line x1="568.7" y1="381.0" x2="581.4" y2="381.0" stroke-width="2.2"/><line x1="568.7" y1="377.0" x2="568.7" y2="385.0" stroke-width="1.8"/><line x1="581.4" y1="377.0" x2="581.4" y2="385.0" stroke-width="1.8"/><circle cx="575.5" cy="381.0" r="3.8"/></g>
<text class="val-text" x="1137" y="385.0">0.796× [0.787, 0.804]</text>
<line class="section-divider" x1="8" y1="394" x2="1312" y2="394"/>
<text class="section-title" x="10" y="409">master / CGo: M1w1 (1 worker)</text>
<text class="row-label" x="153" y="427.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 1.553× [1.523, 1.606]</title><line x1="962.5" y1="423.0" x2="994.1" y2="423.0" stroke-width="2.2"/><line x1="962.5" y1="419.0" x2="962.5" y2="427.0" stroke-width="1.8"/><line x1="994.1" y1="419.0" x2="994.1" y2="427.0" stroke-width="1.8"/><circle cx="974.1" cy="423.0" r="3.8"/></g>
<text class="val-text" x="1137" y="427.0">1.553× [1.523, 1.606]</text>
<text class="row-label" x="153" y="447.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 1.468× [1.431, 1.507]</title><line x1="925.3" y1="443.0" x2="956.2" y2="443.0" stroke-width="2.2"/><line x1="925.3" y1="439.0" x2="925.3" y2="447.0" stroke-width="1.8"/><line x1="956.2" y1="439.0" x2="956.2" y2="447.0" stroke-width="1.8"/><circle cx="940.5" cy="443.0" r="3.8"/></g>
<text class="val-text" x="1137" y="447.0">1.468× [1.431, 1.507]</text>
<text class="row-label" x="153" y="467.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 1.478× [1.465, 1.492]</title><line x1="939.3" y1="463.0" x2="950.2" y2="463.0" stroke-width="2.2"/><line x1="939.3" y1="459.0" x2="939.3" y2="467.0" stroke-width="1.8"/><line x1="950.2" y1="459.0" x2="950.2" y2="467.0" stroke-width="1.8"/><circle cx="944.6" cy="463.0" r="3.8"/></g>
<text class="val-text" x="1137" y="467.0">1.478× [1.465, 1.492]</text>
<text class="row-label" x="153" y="487.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 1.412× [1.341, 1.527]</title><line x1="886.6" y1="483.0" x2="964.0" y2="483.0" stroke-width="2.2"/><line x1="886.6" y1="479.0" x2="886.6" y2="487.0" stroke-width="1.8"/><line x1="964.0" y1="479.0" x2="964.0" y2="487.0" stroke-width="1.8"/><circle cx="917.3" cy="483.0" r="3.8"/></g>
<text class="val-text" x="1137" y="487.0">1.412× [1.341, 1.527]</text>
<line class="section-divider" x1="8" y1="496" x2="1312" y2="496"/>
<text class="section-title" x="10" y="511">master / CGo: M1w8 (8 workers)</text>
<text class="row-label" x="153" y="529.0" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 0.979× [0.959, 0.998]</title><line x1="686.6" y1="525.0" x2="710.4" y2="525.0" stroke-width="2.2"/><line x1="686.6" y1="521.0" x2="686.6" y2="529.0" stroke-width="1.8"/><line x1="710.4" y1="521.0" x2="710.4" y2="529.0" stroke-width="1.8"/><circle cx="698.9" cy="525.0" r="3.8"/></g>
<text class="val-text" x="1137" y="529.0">0.979× [0.959, 0.998]</text>
<text class="row-label" x="153" y="549.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 1.027× [1.002, 1.049]</title><line x1="712.7" y1="545.0" x2="740.1" y2="545.0" stroke-width="2.2"/><line x1="712.7" y1="541.0" x2="712.7" y2="549.0" stroke-width="1.8"/><line x1="740.1" y1="541.0" x2="740.1" y2="549.0" stroke-width="1.8"/><circle cx="727.4" cy="545.0" r="3.8"/></g>
<text class="val-text" x="1137" y="549.0">1.027× [1.002, 1.049]</text>
<text class="row-label" x="153" y="569.0" text-anchor="end">get1000</text>
<g class="ci-neutral"><title>get1000: 1.012× [0.985, 1.036]</title><line x1="702.5" y1="565.0" x2="732.6" y2="565.0" stroke-width="2.2"/><line x1="702.5" y1="561.0" x2="702.5" y2="569.0" stroke-width="1.8"/><line x1="732.6" y1="561.0" x2="732.6" y2="569.0" stroke-width="1.8"/><circle cx="718.7" cy="565.0" r="3.8"/></g>
<text class="val-text" x="1137" y="569.0">1.012× [0.985, 1.036]</text>
<text class="row-label" x="153" y="589.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 1.031× [1.010, 1.053]</title><line x1="717.5" y1="585.0" x2="742.4" y2="585.0" stroke-width="2.2"/><line x1="717.5" y1="581.0" x2="717.5" y2="589.0" stroke-width="1.8"/><line x1="742.4" y1="581.0" x2="742.4" y2="589.0" stroke-width="1.8"/><circle cx="729.8" cy="585.0" r="3.8"/></g>
<text class="val-text" x="1137" y="589.0">1.031× [1.010, 1.053]</text>
<text class="row-label" x="153" y="609.0" text-anchor="end">set100</text>
<g class="ci-neutral"><title>set100: 0.990× [0.956, 1.027]</title><line x1="684.7" y1="605.0" x2="727.4" y2="605.0" stroke-width="2.2"/><line x1="684.7" y1="601.0" x2="684.7" y2="609.0" stroke-width="1.8"/><line x1="727.4" y1="601.0" x2="727.4" y2="609.0" stroke-width="1.8"/><circle cx="705.6" cy="605.0" r="3.8"/></g>
<text class="val-text" x="1137" y="609.0">0.990× [0.956, 1.027]</text>
<line class="section-divider" x1="8" y1="618" x2="1312" y2="618"/>
<text class="section-title" x="10" y="633">master / CGo: M1w64 (64 workers)</text>
<text class="row-label" x="153" y="651.0" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 1.028× [0.979, 1.081]</title><line x1="698.9" y1="647.0" x2="758.0" y2="647.0" stroke-width="2.2"/><line x1="698.9" y1="643.0" x2="698.9" y2="651.0" stroke-width="1.8"/><line x1="758.0" y1="643.0" x2="758.0" y2="651.0" stroke-width="1.8"/><circle cx="728.0" cy="647.0" r="3.8"/></g>
<text class="val-text" x="1137" y="651.0">1.028× [0.979, 1.081]</text>
<text class="row-label" x="153" y="671.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 1.140× [1.097, 1.179]</title><line x1="766.8" y1="667.0" x2="809.8" y2="667.0" stroke-width="2.2"/><line x1="766.8" y1="663.0" x2="766.8" y2="671.0" stroke-width="1.8"/><line x1="809.8" y1="663.0" x2="809.8" y2="671.0" stroke-width="1.8"/><circle cx="789.7" cy="667.0" r="3.8"/></g>
<text class="val-text" x="1137" y="671.0">1.140× [1.097, 1.179]</text>
<text class="row-label" x="153" y="691.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 1.368× [1.294, 1.436]</title><line x1="865.3" y1="687.0" x2="927.4" y2="687.0" stroke-width="2.2"/><line x1="865.3" y1="683.0" x2="865.3" y2="691.0" stroke-width="1.8"/><line x1="927.4" y1="683.0" x2="927.4" y2="691.0" stroke-width="1.8"/><circle cx="898.5" cy="687.0" r="3.8"/></g>
<text class="val-text" x="1137" y="691.0">1.368× [1.294, 1.436]</text>
<text class="row-label" x="153" y="711.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 1.158× [0.691, 1.514]</title><line x1="491.1" y1="707.0" x2="958.9" y2="707.0" stroke-width="2.2"/><line x1="491.1" y1="703.0" x2="491.1" y2="711.0" stroke-width="1.8"/><line x1="958.9" y1="703.0" x2="958.9" y2="711.0" stroke-width="1.8"/><circle cx="799.1" cy="707.0" r="3.8"/></g>
<text class="val-text" x="1137" y="711.0">1.158× [0.691, 1.514]</text>
<text class="row-label" x="153" y="731.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 1.113× [1.104, 1.121]</title><line x1="770.6" y1="727.0" x2="779.7" y2="727.0" stroke-width="2.2"/><line x1="770.6" y1="723.0" x2="770.6" y2="731.0" stroke-width="1.8"/><line x1="779.7" y1="723.0" x2="779.7" y2="731.0" stroke-width="1.8"/><circle cx="775.4" cy="727.0" r="3.8"/></g>
<text class="val-text" x="1137" y="731.0">1.113× [1.104, 1.121]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI above 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI below 1.0×</span></div>
</figure>

**Post-hoc and exploratory: throughput was not preregistered.** The rates and
intervals below are derived afterwards from the retained raw data of the same
eligible cases. They do not change any estimate, exclusion or result above.

- **Throughput:** completed operations divided by the measured wall window, in
  operations per second. One operation is one workload transaction. The window
  excludes setup, warmup, validation and shutdown.
- **Busy cores:** client process user+system CPU during the measured window
  divided by its wall time (1.0 = one core busy), including Go and native
  threads but not the FDB servers. This is not the lifecycle E−A CPU reported
  above.
- **Estimates:** absolute ops/s and busy cores are medians of six processes per
  arm. Ratios are geometric means of six within-block throughput ratios, with
  the same bootstrap as above (20,000 resamples, seed 492817). **Above 1 means
  the numerator completes more operations per second.** Ratios are not
  quotients of the displayed medians.
- **Closed loop:** each phase runs a fixed number of workers, so the measured
  rate is 0.973–1.000 (median 0.998) of workers divided by mean latency. The
  rates describe these operating points, not peak or saturated throughput.

#### `M1w1`: throughput

| Workload | Ops/s old Go / master / CGo | master / old Go | master / CGo | old Go / CGo | Busy cores old Go / master / CGo |
|---|---:|---:|---:|---:|---:|
| grv | 989 / 927 / 607 | 0.956 [0.929, 0.998] | 1.553 [1.523, 1.606] | 1.624 [1.600, 1.649] | 0.246 / 0.308 / 0.255 |
| get100 | 712 / 653 / 442 | 0.914 [0.883, 0.942] | 1.468 [1.431, 1.507] | 1.607 [1.572, 1.638] | 0.351 / 0.387 / 0.328 |
| get1000 | 699 / 650 / 437 | 0.919 [0.907, 0.934] | 1.478 [1.465, 1.492] | 1.607 [1.584, 1.637] | 0.353 / 0.397 / 0.338 |
| range100 | 594 / 552 / 403 | 0.929 [0.917, 0.942] | 1.412 [1.341, 1.527] | 1.521 [1.452, 1.647] | 0.415 / 0.442 / 0.353 |

#### `M1w8`: throughput

| Workload | Ops/s old Go / master / CGo | master / old Go | master / CGo | old Go / CGo | Busy cores old Go / master / CGo |
|---|---:|---:|---:|---:|---:|
| grv | 4,589 / 4,511 / 4,598 | 0.984 [0.970, 0.996] | 0.979 [0.959, 0.998] | 0.995 [0.973, 1.011] | 0.477 / 0.404 / 0.563 |
| get100 | 3,784 / 3,397 / 3,306 | 0.903 [0.885, 0.920] | 1.027 [1.002, 1.049] | 1.137 [1.128, 1.146] | 0.894 / 0.642 / 0.601 |
| get1000 | 3,760 / 3,283 / 3,194 | 0.875 [0.867, 0.883] | 1.012 [0.985, 1.036] | 1.157 [1.124, 1.186] | 0.945 / 0.653 / 0.559 |
| range100 | 2,975 / 2,692 / 2,599 | 0.897 [0.881, 0.911] | 1.031 [1.010, 1.053] | 1.149 [1.140, 1.161] | 1.358 / 1.031 / 0.778 |
| set100 | 2,110 / 1,783 / 1,814 | 0.854 [0.828, 0.882] | 0.990 [0.956, 1.027] | 1.159 [1.126, 1.183] | 0.793 / 0.393 / 0.332 |

#### `M1w64`: throughput

| Workload | Ops/s old Go / master / CGo | master / old Go | master / CGo | old Go / CGo | Busy cores old Go / master / CGo |
|---|---:|---:|---:|---:|---:|
| grv | 30,937 / 32,673 / 31,850 | 1.057 [1.036, 1.080] | 1.028 [0.979, 1.081] | 0.973 [0.928, 1.008] | 1.494 / 1.228 / 2.135 |
| get100 | 27,430 / 21,479 / 18,613 | 0.769 [0.745, 0.792] | 1.140 [1.097, 1.179] | 1.481 [1.439, 1.532] | 3.590 / 2.147 / 1.580 |
| get1000 | 25,634 / 20,749 / 15,074 | 0.798 [0.779, 0.819] | 1.368 [1.294, 1.436] | 1.714 [1.620, 1.802] | 3.531 / 2.251 / 1.378 |
| range100 | 18,919 / 17,509 / 11,776 | 0.718 [0.440, 0.925] | 1.158 [0.691, 1.514] | 1.613 [1.569, 1.650] | 3.742 / 3.726 / 2.296 |
| set100 | 14,688 / 11,654 / 10,457 | 0.796 [0.787, 0.804] | 1.113 [1.104, 1.121] | 1.399 [1.385, 1.413] | 2.911 / 1.409 / 0.987 |

Range100 at 64 workers has a wide, unstable interval here as well. Process,
absolute, paired and omission records, all marked post-hoc, are in
[`analysis/M1-throughput.csv`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/fdbmn-20261020/analysis/M1-throughput.csv); formulas and scopes
are in [`analysis/throughput-method.json`](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/fdbmn-20261020/analysis/throughput-method.json).

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
