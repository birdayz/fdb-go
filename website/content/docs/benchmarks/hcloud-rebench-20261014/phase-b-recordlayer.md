---
title: "Phase B: Record Layer"
description: "Record Layer benchmarks across legacy, before-allocation, and final trees."
weight: 3
---

Part of the [hcloud-rebench-20261014](../) campaign.

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
[campaign-harness.md](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)), so differences come from the
production code. `final/before` is the allocation work plus later changes;
`final/legacy` is both changes together.

## Environment and design

- The host and CPU groups are those of the
  [phase A client run](../phase-a-client/): Hetzner `ccx43`,
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

<figure class="bench-figure">
<figcaption><strong>Figure B.1: Record Layer Allocation Optimization Ratios — Final / Before (log₂ scale, 95% CI)</strong> — 6 paired blocks × 3 repeats on memory engine. Below 1.0× means Final is faster or allocates less.</figcaption>
<svg class="bench-svg" viewBox="0 0 1383 620" role="img" aria-label="Figure B.1: Record Layer Allocation Optimization Ratios — Final / Before (log₂ scale, 95% CI)">
<line class="grid-line" x1="228.0" y1="36" x2="228.0" y2="594"/>
<text class="axis-text" x="228.0" y="30" text-anchor="middle">0.6×</text>
<text class="axis-text" x="228.0" y="609" text-anchor="middle">0.6×</text>
<line class="grid-line" x1="472.1" y1="36" x2="472.1" y2="594"/>
<text class="axis-text" x="472.1" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="472.1" y="609" text-anchor="middle">0.7×</text>
<line class="grid-line" x1="683.6" y1="36" x2="683.6" y2="594"/>
<text class="axis-text" x="683.6" y="30" text-anchor="middle">0.8×</text>
<text class="axis-text" x="683.6" y="609" text-anchor="middle">0.8×</text>
<line class="grid-line" x1="870.2" y1="36" x2="870.2" y2="594"/>
<text class="axis-text" x="870.2" y="30" text-anchor="middle">0.9×</text>
<text class="axis-text" x="870.2" y="609" text-anchor="middle">0.9×</text>
<line class="parity-line" x1="1037.0" y1="36" x2="1037.0" y2="594"/>
<text class="axis-text" x="1037.0" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="1037.0" y="609" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1188.0" y1="36" x2="1188.0" y2="594"/>
<text class="axis-text" x="1188.0" y="30" text-anchor="middle">1.1×</text>
<text class="axis-text" x="1188.0" y="609" text-anchor="middle">1.1×</text>
<text class="axis-text" x="1029.0" y="17" text-anchor="end">← Final lower</text>
<text class="axis-text" x="1045.0" y="17" text-anchor="start">Before lower →</text>
<line class="section-divider" x1="8" y1="48" x2="1375" y2="48"/>
<text class="section-title" x="10" y="63">Mean Time (ns/op): Final / Before</text>
<text class="row-label" x="216" y="81.0" text-anchor="end">SaveRecord</text>
<g class="ci-neutral"><title>SaveRecord: 0.981× [0.954, 1.015]</title><line x1="962.5" y1="77.0" x2="1060.6" y2="77.0" stroke-width="2.2"/><line x1="962.5" y1="73.0" x2="962.5" y2="81.0" stroke-width="1.8"/><line x1="1060.6" y1="73.0" x2="1060.6" y2="81.0" stroke-width="1.8"/><circle cx="1006.7" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1200" y="81.0">0.981× [0.954, 1.015]</text>
<text class="row-label" x="216" y="101.0" text-anchor="end">SaveRecordWithIndex</text>
<g class="ci-neutral"><title>SaveRecordWithIndex: 0.998× [0.965, 1.040]</title><line x1="980.6" y1="97.0" x2="1099.2" y2="97.0" stroke-width="2.2"/><line x1="980.6" y1="93.0" x2="980.6" y2="101.0" stroke-width="1.8"/><line x1="1099.2" y1="93.0" x2="1099.2" y2="101.0" stroke-width="1.8"/><circle cx="1033.9" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1200" y="101.0">0.998× [0.965, 1.040]</text>
<text class="row-label" x="216" y="121.0" text-anchor="end">SaveSplitRecord</text>
<g class="ci-neutral"><title>SaveSplitRecord: 0.966× [0.907, 1.031]</title><line x1="882.4" y1="117.0" x2="1085.4" y2="117.0" stroke-width="2.2"/><line x1="882.4" y1="113.0" x2="882.4" y2="121.0" stroke-width="1.8"/><line x1="1085.4" y1="113.0" x2="1085.4" y2="121.0" stroke-width="1.8"/><circle cx="982.3" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1200" y="121.0">0.966× [0.907, 1.031]</text>
<text class="row-label" x="216" y="141.0" text-anchor="end">LoadRecord</text>
<g class="ci-win"><title>LoadRecord: 0.978× [0.958, 0.994]</title><line x1="969.1" y1="137.0" x2="1027.5" y2="137.0" stroke-width="2.2"/><line x1="969.1" y1="133.0" x2="969.1" y2="141.0" stroke-width="1.8"/><line x1="1027.5" y1="133.0" x2="1027.5" y2="141.0" stroke-width="1.8"/><circle cx="1001.8" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1200" y="141.0">0.978× [0.958, 0.994]</text>
<text class="row-label" x="216" y="161.0" text-anchor="end">ScanRecords</text>
<g class="ci-neutral"><title>ScanRecords: 0.978× [0.948, 1.005]</title><line x1="952.5" y1="157.0" x2="1044.9" y2="157.0" stroke-width="2.2"/><line x1="952.5" y1="153.0" x2="952.5" y2="161.0" stroke-width="1.8"/><line x1="1044.9" y1="153.0" x2="1044.9" y2="161.0" stroke-width="1.8"/><circle cx="1001.8" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1200" y="161.0">0.978× [0.948, 1.005]</text>
<text class="row-label" x="216" y="181.0" text-anchor="end">ScanRecordsSplitVersioned</text>
<g class="ci-win"><title>ScanRecordsSplitVersioned: 0.976× [0.957, 0.996]</title><line x1="967.4" y1="177.0" x2="1030.7" y2="177.0" stroke-width="2.2"/><line x1="967.4" y1="173.0" x2="967.4" y2="181.0" stroke-width="1.8"/><line x1="1030.7" y1="173.0" x2="1030.7" y2="181.0" stroke-width="1.8"/><circle cx="998.6" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1200" y="181.0">0.976× [0.957, 0.996]</text>
<text class="row-label" x="216" y="201.0" text-anchor="end">ScanIndexRecords/serial</text>
<g class="ci-neutral"><title>ScanIndexRecords/serial: 1.015× [0.987, 1.052]</title><line x1="1016.3" y1="197.0" x2="1117.3" y2="197.0" stroke-width="2.2"/><line x1="1016.3" y1="193.0" x2="1016.3" y2="201.0" stroke-width="1.8"/><line x1="1117.3" y1="193.0" x2="1117.3" y2="201.0" stroke-width="1.8"/><circle cx="1060.6" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1200" y="201.0">1.015× [0.987, 1.052]</text>
<text class="row-label" x="216" y="221.0" text-anchor="end">ScanIndexRecords/pipelined</text>
<g class="ci-win"><title>ScanIndexRecords/pipelined: 0.972× [0.952, 0.995]</title><line x1="959.1" y1="217.0" x2="1029.1" y2="217.0" stroke-width="2.2"/><line x1="959.1" y1="213.0" x2="959.1" y2="221.0" stroke-width="1.8"/><line x1="1029.1" y1="213.0" x2="1029.1" y2="221.0" stroke-width="1.8"/><circle cx="992.1" cy="217.0" r="3.8"/></g>
<text class="val-text" x="1200" y="221.0">0.972× [0.952, 0.995]</text>
<line class="section-divider" x1="8" y1="230" x2="1375" y2="230"/>
<text class="section-title" x="10" y="245">Go Bytes (B/op): Final / Before</text>
<text class="row-label" x="216" y="263.0" text-anchor="end">SaveRecord</text>
<g class="ci-win"><title>SaveRecord: 0.902× [0.900, 0.904]</title><line x1="870.2" y1="259.0" x2="877.2" y2="259.0" stroke-width="2.2"/><line x1="870.2" y1="255.0" x2="870.2" y2="263.0" stroke-width="1.8"/><line x1="877.2" y1="255.0" x2="877.2" y2="263.0" stroke-width="1.8"/><circle cx="873.7" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1200" y="263.0">0.902× [0.900, 0.904]</text>
<text class="row-label" x="216" y="283.0" text-anchor="end">SaveRecordWithIndex</text>
<g class="ci-win"><title>SaveRecordWithIndex: 0.910× [0.908, 0.912]</title><line x1="884.2" y1="279.0" x2="891.2" y2="279.0" stroke-width="2.2"/><line x1="884.2" y1="275.0" x2="884.2" y2="283.0" stroke-width="1.8"/><line x1="891.2" y1="275.0" x2="891.2" y2="283.0" stroke-width="1.8"/><circle cx="887.7" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1200" y="283.0">0.910× [0.908, 0.912]</text>
<text class="row-label" x="216" y="303.0" text-anchor="end">SaveSplitRecord</text>
<g class="ci-neutral"><title>SaveSplitRecord: 0.998× [0.993, 1.004]</title><line x1="1025.9" y1="299.0" x2="1043.4" y2="299.0" stroke-width="2.2"/><line x1="1025.9" y1="295.0" x2="1025.9" y2="303.0" stroke-width="1.8"/><line x1="1043.4" y1="295.0" x2="1043.4" y2="303.0" stroke-width="1.8"/><circle cx="1033.9" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1200" y="303.0">0.998× [0.993, 1.004]</text>
<text class="row-label" x="216" y="323.0" text-anchor="end">LoadRecord</text>
<g class="ci-win"><title>LoadRecord: 0.855× [0.854, 0.856]</title><line x1="787.1" y1="319.0" x2="790.8" y2="319.0" stroke-width="2.2"/><line x1="787.1" y1="315.0" x2="787.1" y2="323.0" stroke-width="1.8"/><line x1="790.8" y1="315.0" x2="790.8" y2="323.0" stroke-width="1.8"/><circle cx="788.9" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1200" y="323.0">0.855× [0.854, 0.856]</text>
<text class="row-label" x="216" y="343.0" text-anchor="end">ScanRecords</text>
<g class="ci-win"><title>ScanRecords: 0.859× [0.859, 0.860]</title><line x1="796.3" y1="339.0" x2="798.2" y2="339.0" stroke-width="2.2"/><line x1="796.3" y1="335.0" x2="796.3" y2="343.0" stroke-width="1.8"/><line x1="798.2" y1="335.0" x2="798.2" y2="343.0" stroke-width="1.8"/><circle cx="796.3" cy="339.0" r="3.8"/></g>
<text class="val-text" x="1200" y="343.0">0.859× [0.859, 0.860]</text>
<text class="row-label" x="216" y="363.0" text-anchor="end">ScanRecordsSplitVersioned</text>
<g class="ci-win"><title>ScanRecordsSplitVersioned: 0.812× [0.812, 0.812]</title><line x1="707.2" y1="359.0" x2="707.2" y2="359.0" stroke-width="2.2"/><line x1="707.2" y1="355.0" x2="707.2" y2="363.0" stroke-width="1.8"/><line x1="707.2" y1="355.0" x2="707.2" y2="363.0" stroke-width="1.8"/><circle cx="707.2" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1200" y="363.0">0.812× [0.812, 0.812]</text>
<text class="row-label" x="216" y="383.0" text-anchor="end">ScanIndexRecords/serial</text>
<g class="ci-win"><title>ScanIndexRecords/serial: 0.757× [0.757, 0.758]</title><line x1="596.1" y1="379.0" x2="598.2" y2="379.0" stroke-width="2.2"/><line x1="596.1" y1="375.0" x2="596.1" y2="383.0" stroke-width="1.8"/><line x1="598.2" y1="375.0" x2="598.2" y2="383.0" stroke-width="1.8"/><circle cx="596.1" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1200" y="383.0">0.757× [0.757, 0.758]</text>
<text class="row-label" x="216" y="403.0" text-anchor="end">ScanIndexRecords/pipelined</text>
<g class="ci-win"><title>ScanIndexRecords/pipelined: 0.749× [0.749, 0.749]</title><line x1="579.3" y1="399.0" x2="579.3" y2="399.0" stroke-width="2.2"/><line x1="579.3" y1="395.0" x2="579.3" y2="403.0" stroke-width="1.8"/><line x1="579.3" y1="395.0" x2="579.3" y2="403.0" stroke-width="1.8"/><circle cx="579.3" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1200" y="403.0">0.749× [0.749, 0.749]</text>
<line class="section-divider" x1="8" y1="412" x2="1375" y2="412"/>
<text class="section-title" x="10" y="427">Go Allocations (allocs/op): Final / Before</text>
<text class="row-label" x="216" y="445.0" text-anchor="end">SaveRecord</text>
<g class="ci-win"><title>SaveRecord: 0.859× [0.857, 0.861]</title><line x1="792.6" y1="441.0" x2="800.0" y2="441.0" stroke-width="2.2"/><line x1="792.6" y1="437.0" x2="792.6" y2="445.0" stroke-width="1.8"/><line x1="800.0" y1="437.0" x2="800.0" y2="445.0" stroke-width="1.8"/><circle cx="796.3" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1200" y="445.0">0.859× [0.857, 0.861]</text>
<text class="row-label" x="216" y="465.0" text-anchor="end">SaveRecordWithIndex</text>
<g class="ci-win"><title>SaveRecordWithIndex: 0.866× [0.866, 0.867]</title><line x1="809.2" y1="461.0" x2="811.0" y2="461.0" stroke-width="2.2"/><line x1="809.2" y1="457.0" x2="809.2" y2="465.0" stroke-width="1.8"/><line x1="811.0" y1="457.0" x2="811.0" y2="465.0" stroke-width="1.8"/><circle cx="809.2" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1200" y="465.0">0.866× [0.866, 0.867]</text>
<text class="row-label" x="216" y="485.0" text-anchor="end">SaveSplitRecord</text>
<g class="ci-win"><title>SaveSplitRecord: 0.888× [0.887, 0.889]</title><line x1="847.1" y1="481.0" x2="850.7" y2="481.0" stroke-width="2.2"/><line x1="847.1" y1="477.0" x2="847.1" y2="485.0" stroke-width="1.8"/><line x1="850.7" y1="477.0" x2="850.7" y2="485.0" stroke-width="1.8"/><circle cx="848.9" cy="481.0" r="3.8"/></g>
<text class="val-text" x="1200" y="485.0">0.888× [0.887, 0.889]</text>
<text class="row-label" x="216" y="505.0" text-anchor="end">LoadRecord</text>
<g class="ci-win"><title>LoadRecord: 0.793× [0.793, 0.793]</title><line x1="669.7" y1="501.0" x2="669.7" y2="501.0" stroke-width="2.2"/><line x1="669.7" y1="497.0" x2="669.7" y2="505.0" stroke-width="1.8"/><line x1="669.7" y1="497.0" x2="669.7" y2="505.0" stroke-width="1.8"/><circle cx="669.7" cy="501.0" r="3.8"/></g>
<text class="val-text" x="1200" y="505.0">0.793× [0.793, 0.793]</text>
<text class="row-label" x="216" y="525.0" text-anchor="end">ScanRecords</text>
<g class="ci-win"><title>ScanRecords: 0.828× [0.828, 0.828]</title><line x1="738.1" y1="521.0" x2="738.1" y2="521.0" stroke-width="2.2"/><line x1="738.1" y1="517.0" x2="738.1" y2="525.0" stroke-width="1.8"/><line x1="738.1" y1="517.0" x2="738.1" y2="525.0" stroke-width="1.8"/><circle cx="738.1" cy="521.0" r="3.8"/></g>
<text class="val-text" x="1200" y="525.0">0.828× [0.828, 0.828]</text>
<text class="row-label" x="216" y="545.0" text-anchor="end">ScanRecordsSplitVersioned</text>
<g class="ci-win"><title>ScanRecordsSplitVersioned: 0.816× [0.816, 0.816]</title><line x1="715.0" y1="541.0" x2="715.0" y2="541.0" stroke-width="2.2"/><line x1="715.0" y1="537.0" x2="715.0" y2="545.0" stroke-width="1.8"/><line x1="715.0" y1="537.0" x2="715.0" y2="545.0" stroke-width="1.8"/><circle cx="715.0" cy="541.0" r="3.8"/></g>
<text class="val-text" x="1200" y="545.0">0.816× [0.816, 0.816]</text>
<text class="row-label" x="216" y="565.0" text-anchor="end">ScanIndexRecords/serial</text>
<g class="ci-win"><title>ScanIndexRecords/serial: 0.651× [0.651, 0.651]</title><line x1="357.2" y1="561.0" x2="357.2" y2="561.0" stroke-width="2.2"/><line x1="357.2" y1="557.0" x2="357.2" y2="565.0" stroke-width="1.8"/><line x1="357.2" y1="557.0" x2="357.2" y2="565.0" stroke-width="1.8"/><circle cx="357.2" cy="561.0" r="3.8"/></g>
<text class="val-text" x="1200" y="565.0">0.651× [0.651, 0.651]</text>
<text class="row-label" x="216" y="585.0" text-anchor="end">ScanIndexRecords/pipelined</text>
<g class="ci-win"><title>ScanIndexRecords/pipelined: 0.627× [0.626, 0.627]</title><line x1="295.2" y1="581.0" x2="297.7" y2="581.0" stroke-width="2.2"/><line x1="295.2" y1="577.0" x2="295.2" y2="585.0" stroke-width="1.8"/><line x1="297.7" y1="577.0" x2="297.7" y2="585.0" stroke-width="1.8"/><circle cx="297.7" cy="581.0" r="3.8"/></g>
<text class="val-text" x="1200" y="585.0">0.627× [0.626, 0.627]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

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
