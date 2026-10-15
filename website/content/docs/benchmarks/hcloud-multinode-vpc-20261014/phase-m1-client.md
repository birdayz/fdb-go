---
title: "Phase M1: Client (1, 8, 32, 64 Workers)"
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

## Design and Scope

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

## Results

<figure class="bench-figure">
<figcaption><strong>Figure M1.1: Mean Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)</strong> — 6 paired blocks per workload, 20,000 resamples (seed 492817). Below 1.0× means Pure-Go is faster.</figcaption>
<svg class="bench-svg" viewBox="0 0 890 846" role="img" aria-label="Figure M1.1: Mean Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)">
<line class="grid-line" x1="230.0" y1="28" x2="230.0" y2="816"/>
<text class="axis-text" x="230.0" y="22" text-anchor="middle">0.35×</text>
<text class="axis-text" x="230.0" y="831" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="328.2" y1="28" x2="328.2" y2="816"/>
<text class="axis-text" x="328.2" y="22" text-anchor="middle">0.5×</text>
<text class="axis-text" x="328.2" y="831" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="420.9" y1="28" x2="420.9" y2="816"/>
<text class="axis-text" x="420.9" y="22" text-anchor="middle">0.7×</text>
<text class="axis-text" x="420.9" y="831" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="519.1" y1="28" x2="519.1" y2="816"/>
<text class="axis-text" x="519.1" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="519.1" y="831" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="611.8" y1="28" x2="611.8" y2="816"/>
<text class="axis-text" x="611.8" y="22" text-anchor="middle">1.4×</text>
<text class="axis-text" x="611.8" y="831" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="710.0" y1="28" x2="710.0" y2="816"/>
<text class="axis-text" x="710.0" y="22" text-anchor="middle">2.0×</text>
<text class="axis-text" x="710.0" y="831" text-anchor="middle">2.0×</text>
<text class="axis-text" x="511.1" y="9" text-anchor="end">← Pure-Go faster</text>
<text class="axis-text" x="527.1" y="9" text-anchor="start">CGo faster →</text>
<line class="section-divider" x1="8" y1="40" x2="882" y2="40"/>
<text class="section-title" x="10" y="55">M1w1 (1 worker — 7/7 clean)</text>
<text class="row-label" x="218" y="79.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.579× [0.568, 0.591]</title><line x1="363.3" y1="75.2" x2="374.3" y2="75.2" stroke-width="2.2"/><line x1="363.3" y1="71.2" x2="363.3" y2="79.2" stroke-width="1.8"/><line x1="374.3" y1="71.2" x2="374.3" y2="79.2" stroke-width="1.8"/><circle cx="368.6" cy="75.2" r="3.8"/></g>
<text class="val-text" x="722" y="79.2">0.579× [0.568, 0.591]</text>
<text class="row-label" x="218" y="103.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.702× [0.690, 0.712]</title><line x1="416.9" y1="99.2" x2="425.6" y2="99.2" stroke-width="2.2"/><line x1="416.9" y1="95.2" x2="416.9" y2="103.2" stroke-width="1.8"/><line x1="425.6" y1="95.2" x2="425.6" y2="103.2" stroke-width="1.8"/><circle cx="421.7" cy="99.2" r="3.8"/></g>
<text class="val-text" x="722" y="103.2">0.702× [0.690, 0.712]</text>
<text class="row-label" x="218" y="127.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.673× [0.656, 0.691]</title><line x1="403.0" y1="123.2" x2="417.3" y2="123.2" stroke-width="2.2"/><line x1="403.0" y1="119.2" x2="403.0" y2="127.2" stroke-width="1.8"/><line x1="417.3" y1="119.2" x2="417.3" y2="127.2" stroke-width="1.8"/><circle cx="410.1" cy="123.2" r="3.8"/></g>
<text class="val-text" x="722" y="127.2">0.673× [0.656, 0.691]</text>
<text class="row-label" x="218" y="151.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.633× [0.579, 0.672]</title><line x1="368.6" y1="147.2" x2="409.6" y2="147.2" stroke-width="2.2"/><line x1="368.6" y1="143.2" x2="368.6" y2="151.2" stroke-width="1.8"/><line x1="409.6" y1="143.2" x2="409.6" y2="151.2" stroke-width="1.8"/><circle cx="393.2" cy="147.2" r="3.8"/></g>
<text class="val-text" x="722" y="151.2">0.633× [0.579, 0.672]</text>
<text class="row-label" x="218" y="175.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.886× [0.878, 0.894]</title><line x1="483.3" y1="171.2" x2="488.3" y2="171.2" stroke-width="2.2"/><line x1="483.3" y1="167.2" x2="483.3" y2="175.2" stroke-width="1.8"/><line x1="488.3" y1="167.2" x2="488.3" y2="175.2" stroke-width="1.8"/><circle cx="485.8" cy="171.2" r="3.8"/></g>
<text class="val-text" x="722" y="175.2">0.886× [0.878, 0.894]</text>
<text class="row-label" x="218" y="199.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.920× [0.914, 0.926]</title><line x1="494.3" y1="195.2" x2="497.9" y2="195.2" stroke-width="2.2"/><line x1="494.3" y1="191.2" x2="494.3" y2="199.2" stroke-width="1.8"/><line x1="497.9" y1="191.2" x2="497.9" y2="199.2" stroke-width="1.8"/><circle cx="496.2" cy="195.2" r="3.8"/></g>
<text class="val-text" x="722" y="199.2">0.920× [0.914, 0.926]</text>
<text class="row-label" x="218" y="223.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.954× [0.949, 0.958]</title><line x1="504.7" y1="219.2" x2="507.3" y2="219.2" stroke-width="2.2"/><line x1="504.7" y1="215.2" x2="504.7" y2="223.2" stroke-width="1.8"/><line x1="507.3" y1="215.2" x2="507.3" y2="223.2" stroke-width="1.8"/><circle cx="506.1" cy="219.2" r="3.8"/></g>
<text class="val-text" x="722" y="223.2">0.954× [0.949, 0.958]</text>
<line class="section-divider" x1="8" y1="234" x2="882" y2="234"/>
<text class="section-title" x="10" y="249">M1w8 (8 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="273.2" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 0.996× [0.949, 1.038]</title><line x1="504.7" y1="269.2" x2="529.4" y2="269.2" stroke-width="2.2"/><line x1="504.7" y1="265.2" x2="504.7" y2="273.2" stroke-width="1.8"/><line x1="529.4" y1="265.2" x2="529.4" y2="273.2" stroke-width="1.8"/><circle cx="518.0" cy="269.2" r="3.8"/></g>
<text class="val-text" x="722" y="273.2">0.996× [0.949, 1.038]</text>
<text class="row-label" x="218" y="297.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.888× [0.866, 0.907]</title><line x1="479.5" y1="293.2" x2="492.2" y2="293.2" stroke-width="2.2"/><line x1="479.5" y1="289.2" x2="479.5" y2="297.2" stroke-width="1.8"/><line x1="492.2" y1="289.2" x2="492.2" y2="297.2" stroke-width="1.8"/><circle cx="486.4" cy="293.2" r="3.8"/></g>
<text class="val-text" x="722" y="297.2">0.888× [0.866, 0.907]</text>
<text class="row-label" x="218" y="321.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.942× [0.921, 0.961]</title><line x1="496.4" y1="317.2" x2="508.2" y2="317.2" stroke-width="2.2"/><line x1="496.4" y1="313.2" x2="496.4" y2="321.2" stroke-width="1.8"/><line x1="508.2" y1="313.2" x2="508.2" y2="321.2" stroke-width="1.8"/><circle cx="502.7" cy="317.2" r="3.8"/></g>
<text class="val-text" x="722" y="321.2">0.942× [0.921, 0.961]</text>
<text class="row-label" x="218" y="345.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.868× [0.846, 0.891]</title><line x1="473.1" y1="341.2" x2="487.3" y2="341.2" stroke-width="2.2"/><line x1="473.1" y1="337.2" x2="473.1" y2="345.2" stroke-width="1.8"/><line x1="487.3" y1="337.2" x2="487.3" y2="345.2" stroke-width="1.8"/><circle cx="480.1" cy="341.2" r="3.8"/></g>
<text class="val-text" x="722" y="345.2">0.868× [0.846, 0.891]</text>
<text class="row-label" x="218" y="369.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.980× [0.966, 0.993]</title><line x1="509.6" y1="365.2" x2="517.2" y2="365.2" stroke-width="2.2"/><line x1="509.6" y1="361.2" x2="509.6" y2="369.2" stroke-width="1.8"/><line x1="517.2" y1="361.2" x2="517.2" y2="369.2" stroke-width="1.8"/><circle cx="513.5" cy="365.2" r="3.8"/></g>
<text class="val-text" x="722" y="369.2">0.980× [0.966, 0.993]</text>
<text class="row-label" x="218" y="393.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="393.2">N/A (omitted)</text>
<text class="row-label" x="218" y="417.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="417.2">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="428" x2="882" y2="428"/>
<text class="section-title" x="10" y="443">M1w32 (32 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="467.2" text-anchor="end">grv</text>
<g class="ci-reg"><title>grv: 1.041× [1.007, 1.081]</title><line x1="521.0" y1="463.2" x2="540.6" y2="463.2" stroke-width="2.2"/><line x1="521.0" y1="459.2" x2="521.0" y2="467.2" stroke-width="1.8"/><line x1="540.6" y1="459.2" x2="540.6" y2="467.2" stroke-width="1.8"/><circle cx="530.2" cy="463.2" r="3.8"/></g>
<text class="val-text" x="722" y="467.2">1.041× [1.007, 1.081]</text>
<text class="row-label" x="218" y="491.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.770× [0.750, 0.793]</title><line x1="439.9" y1="487.2" x2="455.2" y2="487.2" stroke-width="2.2"/><line x1="439.9" y1="483.2" x2="439.9" y2="491.2" stroke-width="1.8"/><line x1="455.2" y1="483.2" x2="455.2" y2="491.2" stroke-width="1.8"/><circle cx="447.1" cy="487.2" r="3.8"/></g>
<text class="val-text" x="722" y="491.2">0.770× [0.750, 0.793]</text>
<text class="row-label" x="218" y="515.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.715× [0.688, 0.747]</title><line x1="416.1" y1="511.2" x2="438.8" y2="511.2" stroke-width="2.2"/><line x1="416.1" y1="507.2" x2="416.1" y2="515.2" stroke-width="1.8"/><line x1="438.8" y1="507.2" x2="438.8" y2="515.2" stroke-width="1.8"/><circle cx="426.7" cy="511.2" r="3.8"/></g>
<text class="val-text" x="722" y="515.2">0.715× [0.688, 0.747]</text>
<text class="row-label" x="218" y="539.2" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.869× [0.753, 1.148]</title><line x1="441.0" y1="535.2" x2="557.1" y2="535.2" stroke-width="2.2"/><line x1="441.0" y1="531.2" x2="441.0" y2="539.2" stroke-width="1.8"/><line x1="557.1" y1="531.2" x2="557.1" y2="539.2" stroke-width="1.8"/><circle cx="480.4" cy="535.2" r="3.8"/></g>
<text class="val-text" x="722" y="539.2">0.869× [0.753, 1.148]</text>
<text class="row-label" x="218" y="563.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.938× [0.931, 0.946]</title><line x1="499.4" y1="559.2" x2="503.8" y2="559.2" stroke-width="2.2"/><line x1="499.4" y1="555.2" x2="499.4" y2="563.2" stroke-width="1.8"/><line x1="503.8" y1="555.2" x2="503.8" y2="563.2" stroke-width="1.8"/><circle cx="501.5" cy="559.2" r="3.8"/></g>
<text class="val-text" x="722" y="563.2">0.938× [0.931, 0.946]</text>
<text class="row-label" x="218" y="587.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="587.2">N/A (omitted)</text>
<text class="row-label" x="218" y="611.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="611.2">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="622" x2="882" y2="622"/>
<text class="section-title" x="10" y="637">M1w64 (64 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="661.2" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 1.029× [0.990, 1.069]</title><line x1="516.3" y1="657.2" x2="537.5" y2="657.2" stroke-width="2.2"/><line x1="516.3" y1="653.2" x2="516.3" y2="661.2" stroke-width="1.8"/><line x1="537.5" y1="653.2" x2="537.5" y2="661.2" stroke-width="1.8"/><circle cx="527.0" cy="657.2" r="3.8"/></g>
<text class="val-text" x="722" y="661.2">1.029× [0.990, 1.069]</text>
<text class="row-label" x="218" y="685.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.676× [0.644, 0.710]</title><line x1="397.9" y1="681.2" x2="424.8" y2="681.2" stroke-width="2.2"/><line x1="397.9" y1="677.2" x2="397.9" y2="685.2" stroke-width="1.8"/><line x1="424.8" y1="677.2" x2="424.8" y2="685.2" stroke-width="1.8"/><circle cx="411.3" cy="681.2" r="3.8"/></g>
<text class="val-text" x="722" y="685.2">0.676× [0.644, 0.710]</text>
<text class="row-label" x="218" y="709.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.632× [0.601, 0.669]</title><line x1="378.9" y1="705.2" x2="408.4" y2="705.2" stroke-width="2.2"/><line x1="378.9" y1="701.2" x2="378.9" y2="709.2" stroke-width="1.8"/><line x1="408.4" y1="701.2" x2="408.4" y2="709.2" stroke-width="1.8"/><circle cx="392.7" cy="705.2" r="3.8"/></g>
<text class="val-text" x="722" y="709.2">0.632× [0.601, 0.669]</text>
<text class="row-label" x="218" y="733.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.554× [0.389, 0.673]</title><line x1="259.1" y1="729.2" x2="410.1" y2="729.2" stroke-width="2.2"/><line x1="259.1" y1="725.2" x2="259.1" y2="733.2" stroke-width="1.8"/><line x1="410.1" y1="725.2" x2="410.1" y2="733.2" stroke-width="1.8"/><circle cx="356.5" cy="729.2" r="3.8"/></g>
<text class="val-text" x="722" y="733.2">0.554× [0.389, 0.673]</text>
<text class="row-label" x="218" y="757.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.887× [0.869, 0.897]</title><line x1="480.4" y1="753.2" x2="489.2" y2="753.2" stroke-width="2.2"/><line x1="480.4" y1="749.2" x2="480.4" y2="757.2" stroke-width="1.8"/><line x1="489.2" y1="749.2" x2="489.2" y2="757.2" stroke-width="1.8"/><circle cx="486.1" cy="753.2" r="3.8"/></g>
<text class="val-text" x="722" y="757.2">0.887× [0.869, 0.897]</text>
<text class="row-label" x="218" y="781.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="781.2">N/A (omitted)</text>
<text class="row-label" x="218" y="805.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="805.2">N/A (omitted)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.2: Tail (p99) Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)</strong> — Run-level p99 transaction latency resampled over paired blocks. Below 1.0× means Pure-Go has lower p99.</figcaption>
<svg class="bench-svg" viewBox="0 0 890 846" role="img" aria-label="Figure M1.2: Tail (p99) Latency Ratio — Final Pure-Go / CGo (log₂ scale, 95% block-bootstrap CI)">
<line class="grid-line" x1="230.0" y1="28" x2="230.0" y2="816"/>
<text class="axis-text" x="230.0" y="22" text-anchor="middle">0.25×</text>
<text class="axis-text" x="230.0" y="831" text-anchor="middle">0.25×</text>
<line class="grid-line" x1="307.7" y1="28" x2="307.7" y2="816"/>
<text class="axis-text" x="307.7" y="22" text-anchor="middle">0.35×</text>
<text class="axis-text" x="307.7" y="831" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="390.0" y1="28" x2="390.0" y2="816"/>
<text class="axis-text" x="390.0" y="22" text-anchor="middle">0.5×</text>
<text class="axis-text" x="390.0" y="831" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="467.7" y1="28" x2="467.7" y2="816"/>
<text class="axis-text" x="467.7" y="22" text-anchor="middle">0.7×</text>
<text class="axis-text" x="467.7" y="831" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="550.0" y1="28" x2="550.0" y2="816"/>
<text class="axis-text" x="550.0" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="550.0" y="831" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="627.7" y1="28" x2="627.7" y2="816"/>
<text class="axis-text" x="627.7" y="22" text-anchor="middle">1.4×</text>
<text class="axis-text" x="627.7" y="831" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="710.0" y1="28" x2="710.0" y2="816"/>
<text class="axis-text" x="710.0" y="22" text-anchor="middle">2.0×</text>
<text class="axis-text" x="710.0" y="831" text-anchor="middle">2.0×</text>
<text class="axis-text" x="542.0" y="9" text-anchor="end">← Pure-Go lower p99</text>
<text class="axis-text" x="558.0" y="9" text-anchor="start">CGo lower p99 →</text>
<line class="section-divider" x1="8" y1="40" x2="882" y2="40"/>
<text class="section-title" x="10" y="55">M1w1 (1 worker — 7/7 clean)</text>
<text class="row-label" x="218" y="79.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.498× [0.408, 0.601]</title><line x1="343.1" y1="75.2" x2="432.5" y2="75.2" stroke-width="2.2"/><line x1="343.1" y1="71.2" x2="343.1" y2="79.2" stroke-width="1.8"/><line x1="432.5" y1="71.2" x2="432.5" y2="79.2" stroke-width="1.8"/><circle cx="389.1" cy="75.2" r="3.8"/></g>
<text class="val-text" x="722" y="79.2">0.498× [0.408, 0.601]</text>
<text class="row-label" x="218" y="103.2" text-anchor="end">get100</text>
<g class="ci-neutral"><title>get100: 0.865× [0.734, 1.037]</title><line x1="478.6" y1="99.2" x2="558.4" y2="99.2" stroke-width="2.2"/><line x1="478.6" y1="95.2" x2="478.6" y2="103.2" stroke-width="1.8"/><line x1="558.4" y1="95.2" x2="558.4" y2="103.2" stroke-width="1.8"/><circle cx="516.5" cy="99.2" r="3.8"/></g>
<text class="val-text" x="722" y="103.2">0.865× [0.734, 1.037]</text>
<text class="row-label" x="218" y="127.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.705× [0.654, 0.779]</title><line x1="452.0" y1="123.2" x2="492.4" y2="123.2" stroke-width="2.2"/><line x1="452.0" y1="119.2" x2="452.0" y2="127.2" stroke-width="1.8"/><line x1="492.4" y1="119.2" x2="492.4" y2="127.2" stroke-width="1.8"/><circle cx="469.3" cy="123.2" r="3.8"/></g>
<text class="val-text" x="722" y="127.2">0.705× [0.654, 0.779]</text>
<text class="row-label" x="218" y="151.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.715× [0.595, 0.861]</title><line x1="430.2" y1="147.2" x2="515.5" y2="147.2" stroke-width="2.2"/><line x1="430.2" y1="143.2" x2="430.2" y2="151.2" stroke-width="1.8"/><line x1="515.5" y1="143.2" x2="515.5" y2="151.2" stroke-width="1.8"/><circle cx="472.6" cy="147.2" r="3.8"/></g>
<text class="val-text" x="722" y="151.2">0.715× [0.595, 0.861]</text>
<text class="row-label" x="218" y="175.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.794× [0.756, 0.837]</title><line x1="485.4" y1="171.2" x2="508.9" y2="171.2" stroke-width="2.2"/><line x1="485.4" y1="167.2" x2="485.4" y2="175.2" stroke-width="1.8"/><line x1="508.9" y1="167.2" x2="508.9" y2="175.2" stroke-width="1.8"/><circle cx="496.8" cy="171.2" r="3.8"/></g>
<text class="val-text" x="722" y="175.2">0.794× [0.756, 0.837]</text>
<text class="row-label" x="218" y="199.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.913× [0.857, 0.975]</title><line x1="514.4" y1="195.2" x2="544.2" y2="195.2" stroke-width="2.2"/><line x1="514.4" y1="191.2" x2="514.4" y2="199.2" stroke-width="1.8"/><line x1="544.2" y1="191.2" x2="544.2" y2="199.2" stroke-width="1.8"/><circle cx="529.0" cy="195.2" r="3.8"/></g>
<text class="val-text" x="722" y="199.2">0.913× [0.857, 0.975]</text>
<text class="row-label" x="218" y="223.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.957× [0.924, 0.990]</title><line x1="531.8" y1="219.2" x2="547.7" y2="219.2" stroke-width="2.2"/><line x1="531.8" y1="215.2" x2="531.8" y2="223.2" stroke-width="1.8"/><line x1="547.7" y1="215.2" x2="547.7" y2="223.2" stroke-width="1.8"/><circle cx="539.9" cy="219.2" r="3.8"/></g>
<text class="val-text" x="722" y="223.2">0.957× [0.924, 0.990]</text>
<line class="section-divider" x1="8" y1="234" x2="882" y2="234"/>
<text class="section-title" x="10" y="249">M1w8 (8 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="273.2" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 1.101× [0.869, 1.457]</title><line x1="517.6" y1="269.2" x2="636.9" y2="269.2" stroke-width="2.2"/><line x1="517.6" y1="265.2" x2="517.6" y2="273.2" stroke-width="1.8"/><line x1="636.9" y1="265.2" x2="636.9" y2="273.2" stroke-width="1.8"/><circle cx="572.2" cy="269.2" r="3.8"/></g>
<text class="val-text" x="722" y="273.2">1.101× [0.869, 1.457]</text>
<text class="row-label" x="218" y="297.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.815× [0.741, 0.882]</title><line x1="480.8" y1="293.2" x2="521.0" y2="293.2" stroke-width="2.2"/><line x1="480.8" y1="289.2" x2="480.8" y2="297.2" stroke-width="1.8"/><line x1="521.0" y1="289.2" x2="521.0" y2="297.2" stroke-width="1.8"/><circle cx="502.8" cy="293.2" r="3.8"/></g>
<text class="val-text" x="722" y="297.2">0.815× [0.741, 0.882]</text>
<text class="row-label" x="218" y="321.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.931× [0.859, 0.993]</title><line x1="514.9" y1="317.2" x2="548.4" y2="317.2" stroke-width="2.2"/><line x1="514.9" y1="313.2" x2="514.9" y2="321.2" stroke-width="1.8"/><line x1="548.4" y1="313.2" x2="548.4" y2="321.2" stroke-width="1.8"/><circle cx="533.5" cy="317.2" r="3.8"/></g>
<text class="val-text" x="722" y="321.2">0.931× [0.859, 0.993]</text>
<text class="row-label" x="218" y="345.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.925× [0.861, 0.987]</title><line x1="515.5" y1="341.2" x2="547.0" y2="341.2" stroke-width="2.2"/><line x1="515.5" y1="337.2" x2="515.5" y2="345.2" stroke-width="1.8"/><line x1="547.0" y1="337.2" x2="547.0" y2="345.2" stroke-width="1.8"/><circle cx="532.0" cy="341.2" r="3.8"/></g>
<text class="val-text" x="722" y="345.2">0.925× [0.861, 0.987]</text>
<text class="row-label" x="218" y="369.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.916× [0.860, 0.976]</title><line x1="515.2" y1="365.2" x2="544.4" y2="365.2" stroke-width="2.2"/><line x1="515.2" y1="361.2" x2="515.2" y2="369.2" stroke-width="1.8"/><line x1="544.4" y1="361.2" x2="544.4" y2="369.2" stroke-width="1.8"/><circle cx="529.7" cy="365.2" r="3.8"/></g>
<text class="val-text" x="722" y="369.2">0.916× [0.860, 0.976]</text>
<text class="row-label" x="218" y="393.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="393.2">N/A (omitted)</text>
<text class="row-label" x="218" y="417.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="417.2">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="428" x2="882" y2="428"/>
<text class="section-title" x="10" y="443">M1w32 (32 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="467.2" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 0.871× [0.754, 1.017]</title><line x1="484.8" y1="463.2" x2="553.9" y2="463.2" stroke-width="2.2"/><line x1="484.8" y1="459.2" x2="484.8" y2="467.2" stroke-width="1.8"/><line x1="553.9" y1="459.2" x2="553.9" y2="467.2" stroke-width="1.8"/><circle cx="518.1" cy="463.2" r="3.8"/></g>
<text class="val-text" x="722" y="467.2">0.871× [0.754, 1.017]</text>
<text class="row-label" x="218" y="491.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.815× [0.679, 0.944]</title><line x1="460.6" y1="487.2" x2="536.7" y2="487.2" stroke-width="2.2"/><line x1="460.6" y1="483.2" x2="460.6" y2="491.2" stroke-width="1.8"/><line x1="536.7" y1="483.2" x2="536.7" y2="491.2" stroke-width="1.8"/><circle cx="502.8" cy="487.2" r="3.8"/></g>
<text class="val-text" x="722" y="491.2">0.815× [0.679, 0.944]</text>
<text class="row-label" x="218" y="515.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.481× [0.423, 0.551]</title><line x1="351.4" y1="511.2" x2="412.4" y2="511.2" stroke-width="2.2"/><line x1="351.4" y1="507.2" x2="351.4" y2="515.2" stroke-width="1.8"/><line x1="412.4" y1="507.2" x2="412.4" y2="515.2" stroke-width="1.8"/><circle cx="381.1" cy="511.2" r="3.8"/></g>
<text class="val-text" x="722" y="515.2">0.481× [0.423, 0.551]</text>
<text class="row-label" x="218" y="539.2" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.247× [1.048, 1.644]</title><line x1="560.8" y1="535.2" x2="664.8" y2="535.2" stroke-width="2.2"/><line x1="560.8" y1="531.2" x2="560.8" y2="539.2" stroke-width="1.8"/><line x1="664.8" y1="531.2" x2="664.8" y2="539.2" stroke-width="1.8"/><circle cx="601.0" cy="535.2" r="3.8"/></g>
<text class="val-text" x="722" y="539.2">1.247× [1.048, 1.644]</text>
<text class="row-label" x="218" y="563.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.904× [0.856, 0.948]</title><line x1="514.1" y1="559.2" x2="537.7" y2="559.2" stroke-width="2.2"/><line x1="514.1" y1="555.2" x2="514.1" y2="563.2" stroke-width="1.8"/><line x1="537.7" y1="555.2" x2="537.7" y2="563.2" stroke-width="1.8"/><circle cx="526.7" cy="559.2" r="3.8"/></g>
<text class="val-text" x="722" y="563.2">0.904× [0.856, 0.948]</text>
<text class="row-label" x="218" y="587.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="587.2">N/A (omitted)</text>
<text class="row-label" x="218" y="611.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="611.2">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="622" x2="882" y2="622"/>
<text class="section-title" x="10" y="637">M1w64 (64 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="661.2" text-anchor="end">grv</text>
<g class="ci-neutral"><title>grv: 0.960× [0.757, 1.190]</title><line x1="485.7" y1="657.2" x2="590.2" y2="657.2" stroke-width="2.2"/><line x1="485.7" y1="653.2" x2="485.7" y2="661.2" stroke-width="1.8"/><line x1="590.2" y1="653.2" x2="590.2" y2="661.2" stroke-width="1.8"/><circle cx="540.6" cy="657.2" r="3.8"/></g>
<text class="val-text" x="722" y="661.2">0.960× [0.757, 1.190]</text>
<text class="row-label" x="218" y="685.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.679× [0.601, 0.776]</title><line x1="432.5" y1="681.2" x2="491.5" y2="681.2" stroke-width="2.2"/><line x1="432.5" y1="677.2" x2="432.5" y2="685.2" stroke-width="1.8"/><line x1="491.5" y1="677.2" x2="491.5" y2="685.2" stroke-width="1.8"/><circle cx="460.6" cy="681.2" r="3.8"/></g>
<text class="val-text" x="722" y="685.2">0.679× [0.601, 0.776]</text>
<text class="row-label" x="218" y="709.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.385× [0.336, 0.438]</title><line x1="298.2" y1="705.2" x2="359.4" y2="705.2" stroke-width="2.2"/><line x1="298.2" y1="701.2" x2="298.2" y2="709.2" stroke-width="1.8"/><line x1="359.4" y1="701.2" x2="359.4" y2="709.2" stroke-width="1.8"/><circle cx="329.7" cy="705.2" r="3.8"/></g>
<text class="val-text" x="722" y="709.2">0.385× [0.336, 0.438]</text>
<text class="row-label" x="218" y="733.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.732× [0.506, 0.912]</title><line x1="392.8" y1="729.2" x2="528.7" y2="729.2" stroke-width="2.2"/><line x1="392.8" y1="725.2" x2="392.8" y2="733.2" stroke-width="1.8"/><line x1="528.7" y1="725.2" x2="528.7" y2="733.2" stroke-width="1.8"/><circle cx="478.0" cy="729.2" r="3.8"/></g>
<text class="val-text" x="722" y="733.2">0.732× [0.506, 0.912]</text>
<text class="row-label" x="218" y="757.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.872× [0.815, 0.927]</title><line x1="502.8" y1="753.2" x2="532.5" y2="753.2" stroke-width="2.2"/><line x1="502.8" y1="749.2" x2="502.8" y2="757.2" stroke-width="1.8"/><line x1="532.5" y1="749.2" x2="532.5" y2="757.2" stroke-width="1.8"/><circle cx="518.4" cy="753.2" r="3.8"/></g>
<text class="val-text" x="722" y="757.2">0.872× [0.815, 0.927]</text>
<text class="row-label" x="218" y="781.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="781.2">N/A (omitted)</text>
<text class="row-label" x="218" y="805.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="805.2">N/A (omitted)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.3: Shutdown Process CPU (E−A) Ratio — Final Pure-Go / CGo (log₂ scale, 95% CI)</strong> — User+system process CPU through finalizer drain and network stop per transaction. Below 1.0× means Pure-Go uses less CPU.</figcaption>
<svg class="bench-svg" viewBox="0 0 890 846" role="img" aria-label="Figure M1.3: Shutdown Process CPU (E−A) Ratio — Final Pure-Go / CGo (log₂ scale, 95% CI)">
<line class="grid-line" x1="230.0" y1="28" x2="230.0" y2="816"/>
<text class="axis-text" x="230.0" y="22" text-anchor="middle">0.5×</text>
<text class="axis-text" x="230.0" y="831" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="330.3" y1="28" x2="330.3" y2="816"/>
<text class="axis-text" x="330.3" y="22" text-anchor="middle">0.7×</text>
<text class="axis-text" x="330.3" y="831" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="436.7" y1="28" x2="436.7" y2="816"/>
<text class="axis-text" x="436.7" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="436.7" y="831" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="537.1" y1="28" x2="537.1" y2="816"/>
<text class="axis-text" x="537.1" y="22" text-anchor="middle">1.4×</text>
<text class="axis-text" x="537.1" y="831" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="643.4" y1="28" x2="643.4" y2="816"/>
<text class="axis-text" x="643.4" y="22" text-anchor="middle">2.0×</text>
<text class="axis-text" x="643.4" y="831" text-anchor="middle">2.0×</text>
<line class="grid-line" x1="710.0" y1="28" x2="710.0" y2="816"/>
<text class="axis-text" x="710.0" y="22" text-anchor="middle">2.5×</text>
<text class="axis-text" x="710.0" y="831" text-anchor="middle">2.5×</text>
<text class="axis-text" x="428.7" y="9" text-anchor="end">← Pure-Go less CPU</text>
<text class="axis-text" x="444.7" y="9" text-anchor="start">CGo less CPU →</text>
<line class="section-divider" x1="8" y1="40" x2="882" y2="40"/>
<text class="section-title" x="10" y="55">M1w1 (1 worker — 7/7 clean)</text>
<text class="row-label" x="218" y="79.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.570× [0.551, 0.588]</title><line x1="259.0" y1="75.2" x2="278.4" y2="75.2" stroke-width="2.2"/><line x1="259.0" y1="71.2" x2="259.0" y2="79.2" stroke-width="1.8"/><line x1="278.4" y1="71.2" x2="278.4" y2="79.2" stroke-width="1.8"/><circle cx="269.1" cy="75.2" r="3.8"/></g>
<text class="val-text" x="722" y="79.2">0.570× [0.551, 0.588]</text>
<text class="row-label" x="218" y="103.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.660× [0.648, 0.671]</title><line x1="307.3" y1="99.2" x2="317.7" y2="99.2" stroke-width="2.2"/><line x1="307.3" y1="95.2" x2="307.3" y2="103.2" stroke-width="1.8"/><line x1="317.7" y1="95.2" x2="317.7" y2="103.2" stroke-width="1.8"/><circle cx="312.8" cy="99.2" r="3.8"/></g>
<text class="val-text" x="722" y="103.2">0.660× [0.648, 0.671]</text>
<text class="row-label" x="218" y="127.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.629× [0.603, 0.653]</title><line x1="285.9" y1="123.2" x2="309.6" y2="123.2" stroke-width="2.2"/><line x1="285.9" y1="119.2" x2="285.9" y2="127.2" stroke-width="1.8"/><line x1="309.6" y1="119.2" x2="309.6" y2="127.2" stroke-width="1.8"/><circle cx="298.5" cy="123.2" r="3.8"/></g>
<text class="val-text" x="722" y="127.2">0.629× [0.603, 0.653]</text>
<text class="row-label" x="218" y="151.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.747× [0.731, 0.761]</title><line x1="343.3" y1="147.2" x2="355.3" y2="147.2" stroke-width="2.2"/><line x1="343.3" y1="143.2" x2="343.3" y2="151.2" stroke-width="1.8"/><line x1="355.3" y1="143.2" x2="355.3" y2="151.2" stroke-width="1.8"/><circle cx="349.7" cy="147.2" r="3.8"/></g>
<text class="val-text" x="722" y="151.2">0.747× [0.731, 0.761]</text>
<text class="row-label" x="218" y="175.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.688× [0.677, 0.700]</title><line x1="320.4" y1="171.2" x2="330.3" y2="171.2" stroke-width="2.2"/><line x1="320.4" y1="167.2" x2="320.4" y2="175.2" stroke-width="1.8"/><line x1="330.3" y1="167.2" x2="330.3" y2="175.2" stroke-width="1.8"/><circle cx="325.2" cy="171.2" r="3.8"/></g>
<text class="val-text" x="722" y="175.2">0.688× [0.677, 0.700]</text>
<text class="row-label" x="218" y="199.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.667× [0.659, 0.673]</title><line x1="312.3" y1="195.2" x2="318.6" y2="195.2" stroke-width="2.2"/><line x1="312.3" y1="191.2" x2="312.3" y2="199.2" stroke-width="1.8"/><line x1="318.6" y1="191.2" x2="318.6" y2="199.2" stroke-width="1.8"/><circle cx="315.9" cy="195.2" r="3.8"/></g>
<text class="val-text" x="722" y="199.2">0.667× [0.659, 0.673]</text>
<text class="row-label" x="218" y="223.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.748× [0.729, 0.770]</title><line x1="342.5" y1="219.2" x2="358.8" y2="219.2" stroke-width="2.2"/><line x1="342.5" y1="215.2" x2="342.5" y2="223.2" stroke-width="1.8"/><line x1="358.8" y1="215.2" x2="358.8" y2="223.2" stroke-width="1.8"/><circle cx="350.1" cy="219.2" r="3.8"/></g>
<text class="val-text" x="722" y="223.2">0.748× [0.729, 0.770]</text>
<line class="section-divider" x1="8" y1="234" x2="882" y2="234"/>
<text class="section-title" x="10" y="249">M1w8 (8 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="273.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.904× [0.878, 0.931]</title><line x1="397.9" y1="269.2" x2="415.4" y2="269.2" stroke-width="2.2"/><line x1="397.9" y1="265.2" x2="397.9" y2="273.2" stroke-width="1.8"/><line x1="415.4" y1="265.2" x2="415.4" y2="273.2" stroke-width="1.8"/><circle cx="406.6" cy="269.2" r="3.8"/></g>
<text class="val-text" x="722" y="273.2">0.904× [0.878, 0.931]</text>
<text class="row-label" x="218" y="297.2" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.291× [1.264, 1.331]</title><line x1="506.6" y1="293.2" x2="522.0" y2="293.2" stroke-width="2.2"/><line x1="506.6" y1="289.2" x2="506.6" y2="297.2" stroke-width="1.8"/><line x1="522.0" y1="289.2" x2="522.0" y2="297.2" stroke-width="1.8"/><circle cx="512.9" cy="293.2" r="3.8"/></g>
<text class="val-text" x="722" y="297.2">1.291× [1.264, 1.331]</text>
<text class="row-label" x="218" y="321.2" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.362× [1.342, 1.382]</title><line x1="524.5" y1="317.2" x2="533.2" y2="317.2" stroke-width="2.2"/><line x1="524.5" y1="313.2" x2="524.5" y2="321.2" stroke-width="1.8"/><line x1="533.2" y1="313.2" x2="533.2" y2="321.2" stroke-width="1.8"/><circle cx="528.9" cy="317.2" r="3.8"/></g>
<text class="val-text" x="722" y="321.2">1.362× [1.342, 1.382]</text>
<text class="row-label" x="218" y="345.2" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.544× [1.476, 1.651]</title><line x1="552.8" y1="341.2" x2="586.3" y2="341.2" stroke-width="2.2"/><line x1="552.8" y1="337.2" x2="552.8" y2="345.2" stroke-width="1.8"/><line x1="586.3" y1="337.2" x2="586.3" y2="345.2" stroke-width="1.8"/><circle cx="566.3" cy="341.2" r="3.8"/></g>
<text class="val-text" x="722" y="345.2">1.544× [1.476, 1.651]</text>
<text class="row-label" x="218" y="369.2" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 2.142× [2.123, 2.160]</title><line x1="661.2" y1="365.2" x2="666.4" y2="365.2" stroke-width="2.2"/><line x1="661.2" y1="361.2" x2="661.2" y2="369.2" stroke-width="1.8"/><line x1="666.4" y1="361.2" x2="666.4" y2="369.2" stroke-width="1.8"/><circle cx="663.9" cy="365.2" r="3.8"/></g>
<text class="val-text" x="722" y="369.2">2.142× [2.123, 2.160]</text>
<text class="row-label" x="218" y="393.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="393.2">N/A (omitted)</text>
<text class="row-label" x="218" y="417.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="417.2">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="428" x2="882" y2="428"/>
<text class="section-title" x="10" y="443">M1w32 (32 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="467.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.765× [0.745, 0.779]</title><line x1="348.9" y1="463.2" x2="362.2" y2="463.2" stroke-width="2.2"/><line x1="348.9" y1="459.2" x2="348.9" y2="467.2" stroke-width="1.8"/><line x1="362.2" y1="459.2" x2="362.2" y2="467.2" stroke-width="1.8"/><circle cx="356.8" cy="463.2" r="3.8"/></g>
<text class="val-text" x="722" y="467.2">0.765× [0.745, 0.779]</text>
<text class="row-label" x="218" y="491.2" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.368× [1.334, 1.411]</title><line x1="522.7" y1="487.2" x2="539.4" y2="487.2" stroke-width="2.2"/><line x1="522.7" y1="483.2" x2="522.7" y2="491.2" stroke-width="1.8"/><line x1="539.4" y1="483.2" x2="539.4" y2="491.2" stroke-width="1.8"/><circle cx="530.2" cy="487.2" r="3.8"/></g>
<text class="val-text" x="722" y="491.2">1.368× [1.334, 1.411]</text>
<text class="row-label" x="218" y="515.2" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.537× [1.494, 1.575]</title><line x1="556.5" y1="511.2" x2="572.2" y2="511.2" stroke-width="2.2"/><line x1="556.5" y1="507.2" x2="556.5" y2="515.2" stroke-width="1.8"/><line x1="572.2" y1="507.2" x2="572.2" y2="515.2" stroke-width="1.8"/><circle cx="564.9" cy="511.2" r="3.8"/></g>
<text class="val-text" x="722" y="515.2">1.537× [1.494, 1.575]</text>
<text class="row-label" x="218" y="539.2" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.723× [1.616, 1.921]</title><line x1="579.9" y1="535.2" x2="631.4" y2="535.2" stroke-width="2.2"/><line x1="579.9" y1="531.2" x2="579.9" y2="539.2" stroke-width="1.8"/><line x1="631.4" y1="531.2" x2="631.4" y2="539.2" stroke-width="1.8"/><circle cx="599.0" cy="535.2" r="3.8"/></g>
<text class="val-text" x="722" y="539.2">1.723× [1.616, 1.921]</text>
<text class="row-label" x="218" y="563.2" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 2.162× [2.108, 2.216]</title><line x1="659.1" y1="559.2" x2="674.0" y2="559.2" stroke-width="2.2"/><line x1="659.1" y1="555.2" x2="659.1" y2="563.2" stroke-width="1.8"/><line x1="674.0" y1="555.2" x2="674.0" y2="563.2" stroke-width="1.8"/><circle cx="666.7" cy="559.2" r="3.8"/></g>
<text class="val-text" x="722" y="563.2">2.162× [2.108, 2.216]</text>
<text class="row-label" x="218" y="587.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="587.2">N/A (omitted)</text>
<text class="row-label" x="218" y="611.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="611.2">N/A (omitted)</text>
<line class="section-divider" x1="8" y1="622" x2="882" y2="622"/>
<text class="section-title" x="10" y="637">M1w64 (64 workers — 5/7 clean)</text>
<text class="row-label" x="218" y="661.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.736× [0.721, 0.752]</title><line x1="339.2" y1="657.2" x2="351.7" y2="657.2" stroke-width="2.2"/><line x1="339.2" y1="653.2" x2="339.2" y2="661.2" stroke-width="1.8"/><line x1="351.7" y1="653.2" x2="351.7" y2="661.2" stroke-width="1.8"/><circle cx="345.3" cy="657.2" r="3.8"/></g>
<text class="val-text" x="722" y="661.2">0.736× [0.721, 0.752]</text>
<text class="row-label" x="218" y="685.2" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.429× [1.385, 1.478]</title><line x1="533.9" y1="681.2" x2="553.2" y2="681.2" stroke-width="2.2"/><line x1="533.9" y1="677.2" x2="533.9" y2="685.2" stroke-width="1.8"/><line x1="553.2" y1="677.2" x2="553.2" y2="685.2" stroke-width="1.8"/><circle cx="543.2" cy="681.2" r="3.8"/></g>
<text class="val-text" x="722" y="685.2">1.429× [1.385, 1.478]</text>
<text class="row-label" x="218" y="709.2" text-anchor="end">get1000</text>
<g class="ci-reg"><title>get1000: 1.608× [1.509, 1.705]</title><line x1="559.4" y1="705.2" x2="595.9" y2="705.2" stroke-width="2.2"/><line x1="559.4" y1="701.2" x2="559.4" y2="709.2" stroke-width="1.8"/><line x1="595.9" y1="701.2" x2="595.9" y2="709.2" stroke-width="1.8"/><circle cx="578.4" cy="705.2" r="3.8"/></g>
<text class="val-text" x="722" y="709.2">1.608× [1.509, 1.705]</text>
<text class="row-label" x="218" y="733.2" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 1.069× [0.923, 1.180]</title><line x1="412.8" y1="729.2" x2="486.1" y2="729.2" stroke-width="2.2"/><line x1="412.8" y1="725.2" x2="412.8" y2="733.2" stroke-width="1.8"/><line x1="486.1" y1="725.2" x2="486.1" y2="733.2" stroke-width="1.8"/><circle cx="456.6" cy="729.2" r="3.8"/></g>
<text class="val-text" x="722" y="733.2">1.069× [0.923, 1.180]</text>
<text class="row-label" x="218" y="757.2" text-anchor="end">set100</text>
<g class="ci-reg"><title>set100: 1.981× [1.935, 2.021]</title><line x1="633.6" y1="753.2" x2="646.6" y2="753.2" stroke-width="2.2"/><line x1="633.6" y1="749.2" x2="633.6" y2="757.2" stroke-width="1.8"/><line x1="646.6" y1="749.2" x2="646.6" y2="757.2" stroke-width="1.8"/><circle cx="640.6" cy="753.2" r="3.8"/></g>
<text class="val-text" x="722" y="757.2">1.981× [1.935, 2.021]</text>
<text class="row-label" x="218" y="781.2" text-anchor="end">batchset50</text>
<text class="axis-text" x="240" y="781.2">N/A (omitted)</text>
<text class="row-label" x="218" y="805.2" text-anchor="end">batchset50x1000</text>
<text class="axis-text" x="240" y="805.2">N/A (omitted)</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M1.4: Absolute Mean &amp; p99 Latency at 1 Worker (M1w1) and 64 Workers (M1w64)</strong> — Medians of 6 run-level statistics in µs/transaction (solid bar = mean, whisker = p99).</figcaption>
<svg class="bench-svg" viewBox="0 0 860 468" role="img" aria-label="Figure M1.4: Absolute Mean &amp; p99 Latency at 1 Worker (M1w1) and 64 Workers (M1w64)">
<line class="grid-line" x1="195.0" y1="24" x2="195.0" y2="442"/>
<text class="axis-text" x="195.0" y="457" text-anchor="middle">0 µs</text>
<line class="grid-line" x1="275.0" y1="24" x2="275.0" y2="442"/>
<text class="axis-text" x="275.0" y="457" text-anchor="middle">2000 µs</text>
<line class="grid-line" x1="355.0" y1="24" x2="355.0" y2="442"/>
<text class="axis-text" x="355.0" y="457" text-anchor="middle">4000 µs</text>
<line class="grid-line" x1="435.0" y1="24" x2="435.0" y2="442"/>
<text class="axis-text" x="435.0" y="457" text-anchor="middle">6000 µs</text>
<line class="grid-line" x1="515.0" y1="24" x2="515.0" y2="442"/>
<text class="axis-text" x="515.0" y="457" text-anchor="middle">8000 µs</text>
<line class="grid-line" x1="595.0" y1="24" x2="595.0" y2="442"/>
<text class="axis-text" x="595.0" y="457" text-anchor="middle">10000 µs</text>
<line class="grid-line" x1="675.0" y1="24" x2="675.0" y2="442"/>
<text class="axis-text" x="675.0" y="457" text-anchor="middle">12000 µs</text>
<text class="row-label" x="183" y="48.0" text-anchor="end">M1w1 grv</text>
<g class="ci-win"><title>M1w1 grv Pure-Go: mean 967.1 µs, p99 1477.4 µs</title><rect class="bar-go" x="195" y="34" width="38.7" height="10" rx="2"/><line x1="233.7" y1="39" x2="254.1" y2="39" stroke-width="1.6"/><line x1="254.1" y1="35" x2="254.1" y2="43" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 grv CGo: mean 1673.5 µs, p99 3133.0 µs</title><rect class="bar-cgo" x="195" y="46" width="66.9" height="10" rx="2"/><line x1="261.9" y1="51" x2="320.3" y2="51" stroke-width="1.6"/><line x1="320.3" y1="47" x2="320.3" y2="55" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="43.0">Go 967 / p99 1477 µs</text><text class="axis-text" x="685" y="55.0">CGo 1674 / p99 3133 µs</text>
<text class="row-label" x="183" y="82.0" text-anchor="end">M1w1 get100</text>
<g class="ci-win"><title>M1w1 get100 Pure-Go: mean 1557.2 µs, p99 3588.2 µs</title><rect class="bar-go" x="195" y="68" width="62.3" height="10" rx="2"/><line x1="257.3" y1="73" x2="338.5" y2="73" stroke-width="1.6"/><line x1="338.5" y1="69" x2="338.5" y2="77" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 get100 CGo: mean 2210.4 µs, p99 3967.9 µs</title><rect class="bar-cgo" x="195" y="80" width="88.4" height="10" rx="2"/><line x1="283.4" y1="85" x2="353.7" y2="85" stroke-width="1.6"/><line x1="353.7" y1="81" x2="353.7" y2="89" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="77.0">Go 1557 / p99 3588 µs</text><text class="axis-text" x="685" y="89.0">CGo 2210 / p99 3968 µs</text>
<text class="row-label" x="183" y="116.0" text-anchor="end">M1w1 get1000</text>
<g class="ci-win"><title>M1w1 get1000 Pure-Go: mean 1494.3 µs, p99 2642.5 µs</title><rect class="bar-go" x="195" y="102" width="59.8" height="10" rx="2"/><line x1="254.8" y1="107" x2="300.7" y2="107" stroke-width="1.6"/><line x1="300.7" y1="103" x2="300.7" y2="111" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 get1000 CGo: mean 2223.1 µs, p99 3861.8 µs</title><rect class="bar-cgo" x="195" y="114" width="88.9" height="10" rx="2"/><line x1="283.9" y1="119" x2="349.5" y2="119" stroke-width="1.6"/><line x1="349.5" y1="115" x2="349.5" y2="123" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="111.0">Go 1494 / p99 2642 µs</text><text class="axis-text" x="685" y="123.0">CGo 2223 / p99 3862 µs</text>
<text class="row-label" x="183" y="150.0" text-anchor="end">M1w1 range100</text>
<g class="ci-win"><title>M1w1 range100 Pure-Go: mean 1580.8 µs, p99 3000.0 µs</title><rect class="bar-go" x="195" y="136" width="63.2" height="10" rx="2"/><line x1="258.2" y1="141" x2="315.0" y2="141" stroke-width="1.6"/><line x1="315.0" y1="137" x2="315.0" y2="145" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 range100 CGo: mean 2408.4 µs, p99 4244.1 µs</title><rect class="bar-cgo" x="195" y="148" width="96.3" height="10" rx="2"/><line x1="291.3" y1="153" x2="364.8" y2="153" stroke-width="1.6"/><line x1="364.8" y1="149" x2="364.8" y2="157" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="145.0">Go 1581 / p99 3000 µs</text><text class="axis-text" x="685" y="157.0">CGo 2408 / p99 4244 µs</text>
<text class="row-label" x="183" y="184.0" text-anchor="end">M1w1 set100</text>
<g class="ci-win"><title>M1w1 set100 Pure-Go: mean 4234.5 µs, p99 6418.8 µs</title><rect class="bar-go" x="195" y="170" width="169.4" height="10" rx="2"/><line x1="364.4" y1="175" x2="451.8" y2="175" stroke-width="1.6"/><line x1="451.8" y1="171" x2="451.8" y2="179" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 set100 CGo: mean 4786.2 µs, p99 7991.7 µs</title><rect class="bar-cgo" x="195" y="182" width="191.4" height="10" rx="2"/><line x1="386.4" y1="187" x2="514.7" y2="187" stroke-width="1.6"/><line x1="514.7" y1="183" x2="514.7" y2="191" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="179.0">Go 4234 / p99 6419 µs</text><text class="axis-text" x="685" y="191.0">CGo 4786 / p99 7992 µs</text>
<text class="row-label" x="183" y="218.0" text-anchor="end">M1w1 batchset50</text>
<g class="ci-win"><title>M1w1 batchset50 Pure-Go: mean 5039.9 µs, p99 7509.2 µs</title><rect class="bar-go" x="195" y="204" width="201.6" height="10" rx="2"/><line x1="396.6" y1="209" x2="495.4" y2="209" stroke-width="1.6"/><line x1="495.4" y1="205" x2="495.4" y2="213" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 batchset50 CGo: mean 5481.1 µs, p99 8253.0 µs</title><rect class="bar-cgo" x="195" y="216" width="219.2" height="10" rx="2"/><line x1="414.2" y1="221" x2="525.1" y2="221" stroke-width="1.6"/><line x1="525.1" y1="217" x2="525.1" y2="225" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="213.0">Go 5040 / p99 7509 µs</text><text class="axis-text" x="685" y="225.0">CGo 5481 / p99 8253 µs</text>
<text class="row-label" x="183" y="252.0" text-anchor="end">M1w1 batchset50x1000</text>
<g class="ci-win"><title>M1w1 batchset50x1000 Pure-Go: mean 6241.4 µs, p99 9383.0 µs</title><rect class="bar-go" x="195" y="238" width="249.7" height="10" rx="2"/><line x1="444.7" y1="243" x2="570.3" y2="243" stroke-width="1.6"/><line x1="570.3" y1="239" x2="570.3" y2="247" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w1 batchset50x1000 CGo: mean 6577.5 µs, p99 10055.1 µs</title><rect class="bar-cgo" x="195" y="250" width="263.1" height="10" rx="2"/><line x1="458.1" y1="255" x2="597.2" y2="255" stroke-width="1.6"/><line x1="597.2" y1="251" x2="597.2" y2="259" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="247.0">Go 6241 / p99 9383 µs</text><text class="axis-text" x="685" y="259.0">CGo 6578 / p99 10055 µs</text>
<text class="row-label" x="183" y="286.0" text-anchor="end">M1w64 grv</text>
<g class="ci-win"><title>M1w64 grv Pure-Go: mean 2017.9 µs, p99 4237.9 µs</title><rect class="bar-go" x="195" y="272" width="80.7" height="10" rx="2"/><line x1="275.7" y1="277" x2="364.5" y2="277" stroke-width="1.6"/><line x1="364.5" y1="273" x2="364.5" y2="281" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 grv CGo: mean 1931.8 µs, p99 4015.5 µs</title><rect class="bar-cgo" x="195" y="284" width="77.3" height="10" rx="2"/><line x1="272.3" y1="289" x2="355.6" y2="289" stroke-width="1.6"/><line x1="355.6" y1="285" x2="355.6" y2="293" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="281.0">Go 2018 / p99 4238 µs</text><text class="axis-text" x="685" y="293.0">CGo 1932 / p99 4016 µs</text>
<text class="row-label" x="183" y="320.0" text-anchor="end">M1w64 get100</text>
<g class="ci-win"><title>M1w64 get100 Pure-Go: mean 2229.2 µs, p99 4538.2 µs</title><rect class="bar-go" x="195" y="306" width="89.2" height="10" rx="2"/><line x1="284.2" y1="311" x2="376.5" y2="311" stroke-width="1.6"/><line x1="376.5" y1="307" x2="376.5" y2="315" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 get100 CGo: mean 3271.6 µs, p99 6740.7 µs</title><rect class="bar-cgo" x="195" y="318" width="130.9" height="10" rx="2"/><line x1="325.9" y1="323" x2="464.6" y2="323" stroke-width="1.6"/><line x1="464.6" y1="319" x2="464.6" y2="327" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="315.0">Go 2229 / p99 4538 µs</text><text class="axis-text" x="685" y="327.0">CGo 3272 / p99 6741 µs</text>
<text class="row-label" x="183" y="354.0" text-anchor="end">M1w64 get1000</text>
<g class="ci-win"><title>M1w64 get1000 Pure-Go: mean 2409.9 µs, p99 4544.4 µs</title><rect class="bar-go" x="195" y="340" width="96.4" height="10" rx="2"/><line x1="291.4" y1="345" x2="376.8" y2="345" stroke-width="1.6"/><line x1="376.8" y1="341" x2="376.8" y2="349" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 get1000 CGo: mean 3758.9 µs, p99 11686.4 µs</title><rect class="bar-cgo" x="195" y="352" width="150.4" height="10" rx="2"/><line x1="345.4" y1="357" x2="662.5" y2="357" stroke-width="1.6"/><line x1="662.5" y1="353" x2="662.5" y2="361" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="349.0">Go 2410 / p99 4544 µs</text><text class="axis-text" x="685" y="361.0">CGo 3759 / p99 11686 µs</text>
<text class="row-label" x="183" y="388.0" text-anchor="end">M1w64 range100</text>
<g class="ci-win"><title>M1w64 range100 Pure-Go: mean 3033.1 µs, p99 7695.6 µs</title><rect class="bar-go" x="195" y="374" width="121.3" height="10" rx="2"/><line x1="316.3" y1="379" x2="502.8" y2="379" stroke-width="1.6"/><line x1="502.8" y1="375" x2="502.8" y2="383" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 range100 CGo: mean 4659.5 µs, p99 8704.0 µs</title><rect class="bar-cgo" x="195" y="386" width="186.4" height="10" rx="2"/><line x1="381.4" y1="391" x2="543.2" y2="391" stroke-width="1.6"/><line x1="543.2" y1="387" x2="543.2" y2="395" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="383.0">Go 3033 / p99 7696 µs</text><text class="axis-text" x="685" y="395.0">CGo 4660 / p99 8704 µs</text>
<text class="row-label" x="183" y="422.0" text-anchor="end">M1w64 set100</text>
<g class="ci-win"><title>M1w64 set100 Pure-Go: mean 5918.8 µs, p99 9591.9 µs</title><rect class="bar-go" x="195" y="408" width="236.8" height="10" rx="2"/><line x1="431.8" y1="413" x2="578.7" y2="413" stroke-width="1.6"/><line x1="578.7" y1="409" x2="578.7" y2="417" stroke-width="1.8"/></g>
<g class="ci-reg"><title>M1w64 set100 CGo: mean 6659.4 µs, p99 10868.1 µs</title><rect class="bar-cgo" x="195" y="420" width="266.4" height="10" rx="2"/><line x1="461.4" y1="425" x2="629.7" y2="425" stroke-width="1.6"/><line x1="629.7" y1="421" x2="629.7" y2="429" stroke-width="1.8"/></g>
<text class="val-text" x="685" y="417.0">Go 5919 / p99 9592 µs</text><text class="axis-text" x="685" y="429.0">CGo 6659 / p99 10868 µs</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> Solid bar = Pure-Go mean latency; whisker = Pure-Go p99</span><span><i class="bench-swatch" style="background:#d97706"></i> Solid bar = CGo (libfdb_c 7.3.77) mean latency; whisker = CGo p99</span></div>
</figure>: Final pure-Go / CGo

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

## Absolute Values

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
