---
title: "Phase A2: Client (8 Workers)"
description: "Pure-Go client vs CGo and vs pre-allocation pure-Go at 8 concurrent closed-loop workers."
weight: 2
---

Pure-Go client at the final tree against its predecessor and against Apple's
Go binding over `libfdb_c` 7.3.77, at **8 concurrent closed-loop workers** on a
dedicated 16-vCPU host (`ssd-2` engine). This is **phase A2** of
[hcloud-rebench-20261014](../).

## Arms

| Arm | Client | Source |
|---|---|---|
| final pure-Go | `pkg/fdbgo` | `267a41434a81723b07e7150869d702636a1b91a9` |
| before pure-Go | `pkg/fdbgo` before the allocation work (after read-path parity) | `3824f14654edae8431b0085de8ebf83f5a460a08` |
| CGo | Apple's Go binding over `libfdb_c` 7.3.77, built from the final tree | `267a41434…`, native library 7.3.77 |

## Design and Scope

- Per workload (`get100`, `range100`, `batchset50`): 6 balanced blocks, each
  running all three arms once in a preregistered order (seed 641601; every arm
  order appears once).
- Each run spawns **8 closed-loop workers** in a fresh process: 1,600 warmup
  operations (200 per worker) followed by 24,000 measured operations (3,000 per
  worker), giving **144,000 measured transactions per arm and workload**
  (1,296,000 measured transactions across the phase).
- All 54 scheduled runs completed with zero errors and passed all pre-arm and
  during-arm quiescence, CPU-steal, OOM, server-health, and pin checks.
- **Metric scopes (same as [Phase A](../phase-a-client/)):** Shutdown process
  CPU (E−A) is user+system CPU from after binary identity hashing to after
  measurement, finalizer drain, database close, and native network stop,
  divided by measured transactions; it includes startup, fixture, warmup, and
  validation. Go bytes and mallocs per transaction measure the Go heap only and
  exclude the unobserved native `libfdb_c` heap.
- **Within-phase interpretation:** A2 ran on a fresh `ssd-2` cluster after the
  memory-engine phases; its cluster status reported `tenant_mode: disabled`,
  whereas phase A's initial SSD cluster reported `optional_experimental`
  (neither phase used tenants). Because phase A (1 worker) and phase A2 (8
  workers) ran in separate phases rather than counterbalanced worker blocks,
  the primary estimands here are **within-8-worker arm contrasts**.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure A2.1: Single-Node 8-Worker Client Ratios (log₂ scale, 95% block-bootstrap CI)</strong> — 6 paired blocks × 24,000 tx/run (144,000 tx/arm/workload). Below 1.0× favors Final Pure-Go.</figcaption>
<div class="bench-summary"><ul class="bench-summary-list"><li><strong>Latency (final/CGo at 8w):</strong> Mean latency is lower for pure-Go on range100 (0.663× [0.440, 0.858]) and batchset50 (0.817× [0.768, 0.851]) and unresolved on get100 (0.991× [0.928, 1.084]). p99 is lower on batchset50 (0.914× [0.816, 0.985]) and unresolved on range100 (0.881× [0.696, 1.033]) and get100 (1.015× [0.938, 1.097]).</li><li><strong>Shutdown CPU (E−A, final/CGo at 8w):</strong> Higher for pure-Go on all 3 workloads: batchset50 1.150× [1.112, 1.188], range100 1.359× [1.138, 1.559], and get100 1.638× [1.603, 1.676].</li><li><strong>Allocations (final/before at 8w):</strong> Go mallocs/tx drop on all 3 workloads (range100 0.796×, get100 0.872×, batchset50 0.973×).</li></ul></div>
<svg class="bench-svg" viewBox="0 0 1320 402" role="img" aria-label="Figure A2.1: Single-Node 8-Worker Client Ratios (log₂ scale, 95% block-bootstrap CI)">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="376"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.4×</text>
<text class="axis-text" x="165.0" y="391" text-anchor="middle">0.4×</text>
<line class="grid-line" x1="434.0" y1="36" x2="434.0" y2="376"/>
<text class="axis-text" x="434.0" y="30" text-anchor="middle">0.6×</text>
<text class="axis-text" x="434.0" y="391" text-anchor="middle">0.6×</text>
<line class="grid-line" x1="624.9" y1="36" x2="624.9" y2="376"/>
<text class="axis-text" x="624.9" y="30" text-anchor="middle">0.8×</text>
<text class="axis-text" x="624.9" y="391" text-anchor="middle">0.8×</text>
<line class="parity-line" x1="772.9" y1="36" x2="772.9" y2="376"/>
<text class="axis-text" x="772.9" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="772.9" y="391" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="947.0" y1="36" x2="947.0" y2="376"/>
<text class="axis-text" x="947.0" y="30" text-anchor="middle">1.3×</text>
<text class="axis-text" x="947.0" y="391" text-anchor="middle">1.3×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="376"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">1.7×</text>
<text class="axis-text" x="1125.0" y="391" text-anchor="middle">1.7×</text>
<text class="axis-text" x="764.9" y="17" text-anchor="end">← Final Pure-Go lower</text>
<text class="axis-text" x="780.9" y="17" text-anchor="start">Denominator lower →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">Mean Latency Ratio (8w): Final Pure-Go / CGo</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">get100</text>
<g class="ci-neutral"><title>get100: 0.991× [0.928, 1.084]</title><line x1="723.4" y1="77.0" x2="826.5" y2="77.0" stroke-width="2.2"/><line x1="723.4" y1="73.0" x2="723.4" y2="81.0" stroke-width="1.8"/><line x1="826.5" y1="73.0" x2="826.5" y2="81.0" stroke-width="1.8"/><circle cx="766.9" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.991× [0.928, 1.084]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.663× [0.440, 0.858]</title><line x1="228.2" y1="97.0" x2="671.3" y2="97.0" stroke-width="2.2"/><line x1="228.2" y1="93.0" x2="228.2" y2="101.0" stroke-width="1.8"/><line x1="671.3" y1="93.0" x2="671.3" y2="101.0" stroke-width="1.8"/><circle cx="500.3" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.663× [0.440, 0.858]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.817× [0.768, 0.851]</title><line x1="597.8" y1="117.0" x2="665.9" y2="117.0" stroke-width="2.2"/><line x1="597.8" y1="113.0" x2="597.8" y2="121.0" stroke-width="1.8"/><line x1="665.9" y1="113.0" x2="665.9" y2="121.0" stroke-width="1.8"/><circle cx="638.8" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.817× [0.768, 0.851]</text>
<line class="section-divider" x1="8" y1="130" x2="1312" y2="130"/>
<text class="section-title" x="10" y="145">p99 Latency Ratio (8w): Final Pure-Go / CGo</text>
<text class="row-label" x="153" y="163.0" text-anchor="end">get100</text>
<g class="ci-neutral"><title>get100: 1.015× [0.938, 1.097]</title><line x1="730.5" y1="159.0" x2="834.4" y2="159.0" stroke-width="2.2"/><line x1="730.5" y1="155.0" x2="730.5" y2="163.0" stroke-width="1.8"/><line x1="834.4" y1="155.0" x2="834.4" y2="163.0" stroke-width="1.8"/><circle cx="782.8" cy="159.0" r="3.8"/></g>
<text class="val-text" x="1137" y="163.0">1.015× [0.938, 1.097]</text>
<text class="row-label" x="153" y="183.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.881× [0.696, 1.033]</title><line x1="532.5" y1="179.0" x2="794.5" y2="179.0" stroke-width="2.2"/><line x1="532.5" y1="175.0" x2="532.5" y2="183.0" stroke-width="1.8"/><line x1="794.5" y1="175.0" x2="794.5" y2="183.0" stroke-width="1.8"/><circle cx="688.9" cy="179.0" r="3.8"/></g>
<text class="val-text" x="1137" y="183.0">0.881× [0.696, 1.033]</text>
<text class="row-label" x="153" y="203.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.914× [0.816, 0.985]</title><line x1="638.0" y1="199.0" x2="762.9" y2="199.0" stroke-width="2.2"/><line x1="638.0" y1="195.0" x2="638.0" y2="203.0" stroke-width="1.8"/><line x1="762.9" y1="195.0" x2="762.9" y2="203.0" stroke-width="1.8"/><circle cx="713.3" cy="199.0" r="3.8"/></g>
<text class="val-text" x="1137" y="203.0">0.914× [0.816, 0.985]</text>
<line class="section-divider" x1="8" y1="212" x2="1312" y2="212"/>
<text class="section-title" x="10" y="227">Shutdown Process CPU (E−A) Ratio (8w): Final Pure-Go / CGo</text>
<text class="row-label" x="153" y="245.0" text-anchor="end">get100</text>
<g class="ci-reg"><title>get100: 1.638× [1.603, 1.676]</title><line x1="1086.0" y1="241.0" x2="1115.6" y2="241.0" stroke-width="2.2"/><line x1="1086.0" y1="237.0" x2="1086.0" y2="245.0" stroke-width="1.8"/><line x1="1115.6" y1="237.0" x2="1115.6" y2="245.0" stroke-width="1.8"/><circle cx="1100.4" cy="241.0" r="3.8"/></g>
<text class="val-text" x="1137" y="245.0">1.638× [1.603, 1.676]</text>
<text class="row-label" x="153" y="265.0" text-anchor="end">range100</text>
<g class="ci-reg"><title>range100: 1.359× [1.138, 1.559]</title><line x1="858.7" y1="261.0" x2="1067.6" y2="261.0" stroke-width="2.2"/><line x1="858.7" y1="257.0" x2="858.7" y2="265.0" stroke-width="1.8"/><line x1="1067.6" y1="257.0" x2="1067.6" y2="265.0" stroke-width="1.8"/><circle cx="976.5" cy="261.0" r="3.8"/></g>
<text class="val-text" x="1137" y="265.0">1.359× [1.138, 1.559]</text>
<text class="row-label" x="153" y="285.0" text-anchor="end">batchset50</text>
<g class="ci-reg"><title>batchset50: 1.150× [1.112, 1.188]</title><line x1="843.4" y1="281.0" x2="887.2" y2="281.0" stroke-width="2.2"/><line x1="843.4" y1="277.0" x2="843.4" y2="285.0" stroke-width="1.8"/><line x1="887.2" y1="277.0" x2="887.2" y2="285.0" stroke-width="1.8"/><circle cx="865.7" cy="281.0" r="3.8"/></g>
<text class="val-text" x="1137" y="285.0">1.150× [1.112, 1.188]</text>
<line class="section-divider" x1="8" y1="294" x2="1312" y2="294"/>
<text class="section-title" x="10" y="309">Go Mallocs/tx Ratio (8w): Final / Before Pure-Go</text>
<text class="row-label" x="153" y="327.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.872× [0.872, 0.872]</title><line x1="682.1" y1="323.0" x2="682.1" y2="323.0" stroke-width="2.2"/><line x1="682.1" y1="319.0" x2="682.1" y2="327.0" stroke-width="1.8"/><line x1="682.1" y1="319.0" x2="682.1" y2="327.0" stroke-width="1.8"/><circle cx="682.1" cy="323.0" r="3.8"/></g>
<text class="val-text" x="1137" y="327.0">0.872× [0.872, 0.872]</text>
<text class="row-label" x="153" y="347.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.796× [0.796, 0.797]</title><line x1="621.6" y1="343.0" x2="622.4" y2="343.0" stroke-width="2.2"/><line x1="621.6" y1="339.0" x2="621.6" y2="347.0" stroke-width="1.8"/><line x1="622.4" y1="339.0" x2="622.4" y2="347.0" stroke-width="1.8"/><circle cx="621.6" cy="343.0" r="3.8"/></g>
<text class="val-text" x="1137" y="347.0">0.796× [0.796, 0.797]</text>
<text class="row-label" x="153" y="367.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.973× [0.973, 0.973]</title><line x1="754.8" y1="363.0" x2="754.8" y2="363.0" stroke-width="2.2"/><line x1="754.8" y1="359.0" x2="754.8" y2="367.0" stroke-width="1.8"/><line x1="754.8" y1="359.0" x2="754.8" y2="367.0" stroke-width="1.8"/><circle cx="754.8" cy="363.0" r="3.8"/></g>
<text class="val-text" x="1137" y="367.0">0.973× [0.973, 0.973]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

### Final pure-Go vs CGo (8 workers)

Ratio final/CGo (below 1 means pure-Go is faster or cheaper), nominal 95%
paired-block bootstrap interval (20,000 resamples, seed 391706).

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|
| get100 | 0.991 [0.928, 1.084] | 1.015 [0.938, 1.097] | 1.638 [1.603, 1.676] |
| range100 | 0.663 [0.440, 0.858] | 0.881 [0.696, 1.033] | 1.359 [1.138, 1.559] |
| batchset50 | 0.817 [0.768, 0.851] | 0.914 [0.816, 0.985] | 1.150 [1.112, 1.188] |

### Final vs before pure-Go (8 workers): the allocation work

Ratio final/before.

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx | Go bytes/tx | Go mallocs/tx |
|---|---:|---:|---:|---:|---:|
| get100 | 1.007 [0.997, 1.018] | 1.028 [0.960, 1.090] | 0.972 [0.959, 0.986] | 0.929 [0.929, 0.929] | 0.872 [0.872, 0.872] |
| range100 | 1.069 [0.948, 1.206] | 0.901 [0.829, 0.982] | 1.041 [0.926, 1.171] | 0.824 [0.824, 0.825] | 0.796 [0.796, 0.797] |
| batchset50 | 0.995 [0.950, 1.022] | 0.995 [0.941, 1.045] | 0.991 [0.971, 1.010] | 0.993 [0.993, 0.994] | 0.973 [0.973, 0.973] |

### Absolute values (8 workers)

Medians of six run-level statistics per arm.

| Workload | Arm | Transactions | Mean µs | p50 µs | p99 µs | Shutdown CPU (E−A) µs/tx | Go bytes/tx | Go mallocs/tx |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| get100 | final pure-Go | 144000 | 549.8 | 536.4 | 1065.7 | 202.5 | 6385 | 68.4 |
| get100 | before pure-Go | 144000 | 544.0 | 532.4 | 1019.7 | 208.0 | 6873 | 78.5 |
| get100 | CGo | 144000 | 566.7 | 533.5 | 1091.6 | 124.0 | 664 | 16.0 |
| range100 | final pure-Go | 144000 | 615.6 | 550.2 | 1941.7 | 312.2 | 50147 | 73.9 |
| range100 | before pure-Go | 144000 | 563.4 | 486.2 | 2127.8 | 292.1 | 60829 | 92.8 |
| range100 | CGo | 144000 | 808.5 | 734.6 | 2072.8 | 220.3 | 25672 | 224.0 |
| batchset50 | final pure-Go | 144000 | 4080.2 | 3860.7 | 7941.3 | 684.0 | 73922 | 321.0 |
| batchset50 | before pure-Go | 144000 | 4067.0 | 3925.7 | 7784.8 | 689.3 | 74415 | 330.0 |
| batchset50 | CGo | 144000 | 4904.5 | 4882.2 | 8262.7 | 589.0 | 1616 | 59.0 |

## Interpretation

- **Latency at 8 workers:**
  - On `range100`, pure-Go had lower mean latency than CGo (0.663 [0.440, 0.858], 615.6 µs vs 808.5 µs), while the p99 interval included 1 (0.881 [0.696, 1.033]).
  - On `batchset50`, pure-Go had lower mean latency (0.817 [0.768, 0.851], 4080.2 µs vs 4904.5 µs) and lower p99 latency (0.914 [0.816, 0.985]).
  - On `get100`, the nominal latency intervals include 1 (mean 0.991 [0.928,
    1.084], p99 1.015 [0.938, 1.097]); no clear difference is resolved in this
    bounded sample, and this design does not test equivalence.
- **Process CPU at 8 workers:** unlike at 1 worker, **shutdown process CPU (E−A) per transaction was higher for pure-Go than for CGo on all three workloads at 8 workers**: 1.638 [1.603, 1.676] on `get100`, 1.359 [1.138, 1.559] on `range100`, and 1.150 [1.112, 1.188] on `batchset50`.
  - *Untested hypothesis:* `libfdb_c` multiplexes all 8 Go workers onto its single C++ network thread (`GOMAXPROCS=8` on the Go side vs one native network thread), whereas the pure-Go client runs concurrent goroutines, serialization, and Go GC across up to 8 OS threads. Without CPU profiles, the exact split between Go runtime scheduling, GC, and socket I/O is unmeasured.
- **Final vs before pure-Go:** Go bytes and mallocs per transaction fell by the same proportions as in phase A (`get100` −7.1% B/tx, −12.8% mallocs/tx; `range100` −17.6% B/tx, −20.4% mallocs/tx; `batchset50` −0.7% B/tx, −2.7% mallocs/tx), with `get100` shutdown CPU 0.972 [0.959, 0.986] and `range100` p99 0.901 [0.829, 0.982].
