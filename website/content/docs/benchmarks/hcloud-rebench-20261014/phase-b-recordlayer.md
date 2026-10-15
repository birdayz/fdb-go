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
<svg class="bench-svg" viewBox="0 0 860 724" role="img" aria-label="Figure B.1: Record Layer Allocation Optimization Ratios — Final / Before (log₂ scale, 95% CI)">
<line class="grid-line" x1="215.0" y1="28" x2="215.0" y2="694"/>
<text class="axis-text" x="215.0" y="22" text-anchor="middle">0.60×</text>
<text class="axis-text" x="215.0" y="709" text-anchor="middle">0.60×</text>
<line class="grid-line" x1="337.1" y1="28" x2="337.1" y2="694"/>
<text class="axis-text" x="337.1" y="22" text-anchor="middle">0.70×</text>
<text class="axis-text" x="337.1" y="709" text-anchor="middle">0.70×</text>
<line class="grid-line" x1="442.8" y1="28" x2="442.8" y2="694"/>
<text class="axis-text" x="442.8" y="22" text-anchor="middle">0.80×</text>
<text class="axis-text" x="442.8" y="709" text-anchor="middle">0.80×</text>
<line class="grid-line" x1="536.1" y1="28" x2="536.1" y2="694"/>
<text class="axis-text" x="536.1" y="22" text-anchor="middle">0.90×</text>
<text class="axis-text" x="536.1" y="709" text-anchor="middle">0.90×</text>
<line class="parity-line" x1="619.5" y1="28" x2="619.5" y2="694"/>
<text class="axis-text" x="619.5" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="619.5" y="709" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="695.0" y1="28" x2="695.0" y2="694"/>
<text class="axis-text" x="695.0" y="22" text-anchor="middle">1.1×</text>
<text class="axis-text" x="695.0" y="709" text-anchor="middle">1.1×</text>
<text class="axis-text" x="611.5" y="9" text-anchor="end">← Final lower</text>
<text class="axis-text" x="627.5" y="9" text-anchor="start">Before lower →</text>
<line class="section-divider" x1="8" y1="40" x2="852" y2="40"/>
<text class="section-title" x="10" y="55">Mean Time (ns/op): Final / Before</text>
<text class="row-label" x="203" y="79.2" text-anchor="end">SaveRecord</text>
<g class="ci-neutral"><title>SaveRecord: 0.981× [0.954, 1.015]</title><line x1="582.2" y1="75.2" x2="631.3" y2="75.2" stroke-width="2.2"/><line x1="582.2" y1="71.2" x2="582.2" y2="79.2" stroke-width="1.8"/><line x1="631.3" y1="71.2" x2="631.3" y2="79.2" stroke-width="1.8"/><circle cx="604.3" cy="75.2" r="3.8"/></g>
<text class="val-text" x="707" y="79.2">0.981× [0.954, 1.015]</text>
<text class="row-label" x="203" y="103.2" text-anchor="end">SaveRecordWithIndex</text>
<g class="ci-neutral"><title>SaveRecordWithIndex: 0.998× [0.965, 1.040]</title><line x1="591.3" y1="99.2" x2="650.6" y2="99.2" stroke-width="2.2"/><line x1="591.3" y1="95.2" x2="591.3" y2="103.2" stroke-width="1.8"/><line x1="650.6" y1="95.2" x2="650.6" y2="103.2" stroke-width="1.8"/><circle cx="617.9" cy="99.2" r="3.8"/></g>
<text class="val-text" x="707" y="103.2">0.998× [0.965, 1.040]</text>
<text class="row-label" x="203" y="127.2" text-anchor="end">SaveSplitRecord</text>
<g class="ci-neutral"><title>SaveSplitRecord: 0.966× [0.907, 1.031]</title><line x1="542.2" y1="123.2" x2="643.7" y2="123.2" stroke-width="2.2"/><line x1="542.2" y1="119.2" x2="542.2" y2="127.2" stroke-width="1.8"/><line x1="643.7" y1="119.2" x2="643.7" y2="127.2" stroke-width="1.8"/><circle cx="592.1" cy="123.2" r="3.8"/></g>
<text class="val-text" x="707" y="127.2">0.966× [0.907, 1.031]</text>
<text class="row-label" x="203" y="151.2" text-anchor="end">LoadRecord</text>
<g class="ci-win"><title>LoadRecord: 0.978× [0.958, 0.994]</title><line x1="585.5" y1="147.2" x2="614.8" y2="147.2" stroke-width="2.2"/><line x1="585.5" y1="143.2" x2="585.5" y2="151.2" stroke-width="1.8"/><line x1="614.8" y1="143.2" x2="614.8" y2="151.2" stroke-width="1.8"/><circle cx="601.9" cy="147.2" r="3.8"/></g>
<text class="val-text" x="707" y="151.2">0.978× [0.958, 0.994]</text>
<text class="row-label" x="203" y="175.2" text-anchor="end">ScanRecords</text>
<g class="ci-neutral"><title>ScanRecords: 0.978× [0.948, 1.005]</title><line x1="577.2" y1="171.2" x2="623.5" y2="171.2" stroke-width="2.2"/><line x1="577.2" y1="167.2" x2="577.2" y2="175.2" stroke-width="1.8"/><line x1="623.5" y1="167.2" x2="623.5" y2="175.2" stroke-width="1.8"/><circle cx="601.9" cy="171.2" r="3.8"/></g>
<text class="val-text" x="707" y="175.2">0.978× [0.948, 1.005]</text>
<text class="row-label" x="203" y="199.2" text-anchor="end">ScanRecordsSplitVersioned</text>
<g class="ci-win"><title>ScanRecordsSplitVersioned: 0.976× [0.957, 0.996]</title><line x1="584.7" y1="195.2" x2="616.3" y2="195.2" stroke-width="2.2"/><line x1="584.7" y1="191.2" x2="584.7" y2="199.2" stroke-width="1.8"/><line x1="616.3" y1="191.2" x2="616.3" y2="199.2" stroke-width="1.8"/><circle cx="600.3" cy="195.2" r="3.8"/></g>
<text class="val-text" x="707" y="199.2">0.976× [0.957, 0.996]</text>
<text class="row-label" x="203" y="223.2" text-anchor="end">ScanIndexRecords/serial</text>
<g class="ci-neutral"><title>ScanIndexRecords/serial: 1.015× [0.987, 1.052]</title><line x1="609.2" y1="219.2" x2="659.7" y2="219.2" stroke-width="2.2"/><line x1="609.2" y1="215.2" x2="609.2" y2="223.2" stroke-width="1.8"/><line x1="659.7" y1="215.2" x2="659.7" y2="223.2" stroke-width="1.8"/><circle cx="631.3" cy="219.2" r="3.8"/></g>
<text class="val-text" x="707" y="223.2">1.015× [0.987, 1.052]</text>
<text class="row-label" x="203" y="247.2" text-anchor="end">ScanIndexRecords/pipelined</text>
<g class="ci-win"><title>ScanIndexRecords/pipelined: 0.972× [0.952, 0.995]</title><line x1="580.6" y1="243.2" x2="615.6" y2="243.2" stroke-width="2.2"/><line x1="580.6" y1="239.2" x2="580.6" y2="247.2" stroke-width="1.8"/><line x1="615.6" y1="239.2" x2="615.6" y2="247.2" stroke-width="1.8"/><circle cx="597.0" cy="243.2" r="3.8"/></g>
<text class="val-text" x="707" y="247.2">0.972× [0.952, 0.995]</text>
<line class="section-divider" x1="8" y1="258" x2="852" y2="258"/>
<text class="section-title" x="10" y="273">Go Bytes (B/op): Final / Before</text>
<text class="row-label" x="203" y="297.2" text-anchor="end">SaveRecord</text>
<g class="ci-win"><title>SaveRecord: 0.902× [0.900, 0.904]</title><line x1="536.1" y1="293.2" x2="539.6" y2="293.2" stroke-width="2.2"/><line x1="536.1" y1="289.2" x2="536.1" y2="297.2" stroke-width="1.8"/><line x1="539.6" y1="289.2" x2="539.6" y2="297.2" stroke-width="1.8"/><circle cx="537.8" cy="293.2" r="3.8"/></g>
<text class="val-text" x="707" y="297.2">0.902× [0.900, 0.904]</text>
<text class="row-label" x="203" y="321.2" text-anchor="end">SaveRecordWithIndex</text>
<g class="ci-win"><title>SaveRecordWithIndex: 0.910× [0.908, 0.912]</title><line x1="543.1" y1="317.2" x2="546.6" y2="317.2" stroke-width="2.2"/><line x1="543.1" y1="313.2" x2="543.1" y2="321.2" stroke-width="1.8"/><line x1="546.6" y1="313.2" x2="546.6" y2="321.2" stroke-width="1.8"/><circle cx="544.8" cy="317.2" r="3.8"/></g>
<text class="val-text" x="707" y="321.2">0.910× [0.908, 0.912]</text>
<text class="row-label" x="203" y="345.2" text-anchor="end">SaveSplitRecord</text>
<g class="ci-neutral"><title>SaveSplitRecord: 0.998× [0.993, 1.004]</title><line x1="614.0" y1="341.2" x2="622.7" y2="341.2" stroke-width="2.2"/><line x1="614.0" y1="337.2" x2="614.0" y2="345.2" stroke-width="1.8"/><line x1="622.7" y1="337.2" x2="622.7" y2="345.2" stroke-width="1.8"/><circle cx="617.9" cy="341.2" r="3.8"/></g>
<text class="val-text" x="707" y="345.2">0.998× [0.993, 1.004]</text>
<text class="row-label" x="203" y="369.2" text-anchor="end">LoadRecord</text>
<g class="ci-win"><title>LoadRecord: 0.855× [0.854, 0.856]</title><line x1="494.5" y1="365.2" x2="496.4" y2="365.2" stroke-width="2.2"/><line x1="494.5" y1="361.2" x2="494.5" y2="369.2" stroke-width="1.8"/><line x1="496.4" y1="361.2" x2="496.4" y2="369.2" stroke-width="1.8"/><circle cx="495.5" cy="365.2" r="3.8"/></g>
<text class="val-text" x="707" y="369.2">0.855× [0.854, 0.856]</text>
<text class="row-label" x="203" y="393.2" text-anchor="end">ScanRecords</text>
<g class="ci-win"><title>ScanRecords: 0.859× [0.859, 0.860]</title><line x1="499.2" y1="389.2" x2="500.1" y2="389.2" stroke-width="2.2"/><line x1="499.2" y1="385.2" x2="499.2" y2="393.2" stroke-width="1.8"/><line x1="500.1" y1="385.2" x2="500.1" y2="393.2" stroke-width="1.8"/><circle cx="499.2" cy="389.2" r="3.8"/></g>
<text class="val-text" x="707" y="393.2">0.859× [0.859, 0.860]</text>
<text class="row-label" x="203" y="417.2" text-anchor="end">ScanRecordsSplitVersioned</text>
<g class="ci-win"><title>ScanRecordsSplitVersioned: 0.812× [0.812, 0.812]</title><line x1="454.6" y1="413.2" x2="454.6" y2="413.2" stroke-width="2.2"/><line x1="454.6" y1="409.2" x2="454.6" y2="417.2" stroke-width="1.8"/><line x1="454.6" y1="409.2" x2="454.6" y2="417.2" stroke-width="1.8"/><circle cx="454.6" cy="413.2" r="3.8"/></g>
<text class="val-text" x="707" y="417.2">0.812× [0.812, 0.812]</text>
<text class="row-label" x="203" y="441.2" text-anchor="end">ScanIndexRecords/serial</text>
<g class="ci-win"><title>ScanIndexRecords/serial: 0.757× [0.757, 0.758]</title><line x1="399.1" y1="437.2" x2="400.1" y2="437.2" stroke-width="2.2"/><line x1="399.1" y1="433.2" x2="399.1" y2="441.2" stroke-width="1.8"/><line x1="400.1" y1="433.2" x2="400.1" y2="441.2" stroke-width="1.8"/><circle cx="399.1" cy="437.2" r="3.8"/></g>
<text class="val-text" x="707" y="441.2">0.757× [0.757, 0.758]</text>
<text class="row-label" x="203" y="465.2" text-anchor="end">ScanIndexRecords/pipelined</text>
<g class="ci-win"><title>ScanIndexRecords/pipelined: 0.749× [0.749, 0.749]</title><line x1="390.7" y1="461.2" x2="390.7" y2="461.2" stroke-width="2.2"/><line x1="390.7" y1="457.2" x2="390.7" y2="465.2" stroke-width="1.8"/><line x1="390.7" y1="457.2" x2="390.7" y2="465.2" stroke-width="1.8"/><circle cx="390.7" cy="461.2" r="3.8"/></g>
<text class="val-text" x="707" y="465.2">0.749× [0.749, 0.749]</text>
<line class="section-divider" x1="8" y1="476" x2="852" y2="476"/>
<text class="section-title" x="10" y="491">Go Allocations (allocs/op): Final / Before</text>
<text class="row-label" x="203" y="515.2" text-anchor="end">SaveRecord</text>
<g class="ci-win"><title>SaveRecord: 0.859× [0.857, 0.861]</title><line x1="497.3" y1="511.2" x2="501.0" y2="511.2" stroke-width="2.2"/><line x1="497.3" y1="507.2" x2="497.3" y2="515.2" stroke-width="1.8"/><line x1="501.0" y1="507.2" x2="501.0" y2="515.2" stroke-width="1.8"/><circle cx="499.2" cy="511.2" r="3.8"/></g>
<text class="val-text" x="707" y="515.2">0.859× [0.857, 0.861]</text>
<text class="row-label" x="203" y="539.2" text-anchor="end">SaveRecordWithIndex</text>
<g class="ci-win"><title>SaveRecordWithIndex: 0.866× [0.866, 0.867]</title><line x1="505.6" y1="535.2" x2="506.5" y2="535.2" stroke-width="2.2"/><line x1="505.6" y1="531.2" x2="505.6" y2="539.2" stroke-width="1.8"/><line x1="506.5" y1="531.2" x2="506.5" y2="539.2" stroke-width="1.8"/><circle cx="505.6" cy="535.2" r="3.8"/></g>
<text class="val-text" x="707" y="539.2">0.866× [0.866, 0.867]</text>
<text class="row-label" x="203" y="563.2" text-anchor="end">SaveSplitRecord</text>
<g class="ci-win"><title>SaveSplitRecord: 0.888× [0.887, 0.889]</title><line x1="524.6" y1="559.2" x2="526.4" y2="559.2" stroke-width="2.2"/><line x1="524.6" y1="555.2" x2="524.6" y2="563.2" stroke-width="1.8"/><line x1="526.4" y1="555.2" x2="526.4" y2="563.2" stroke-width="1.8"/><circle cx="525.5" cy="559.2" r="3.8"/></g>
<text class="val-text" x="707" y="563.2">0.888× [0.887, 0.889]</text>
<text class="row-label" x="203" y="587.2" text-anchor="end">LoadRecord</text>
<g class="ci-win"><title>LoadRecord: 0.793× [0.793, 0.793]</title><line x1="435.9" y1="583.2" x2="435.9" y2="583.2" stroke-width="2.2"/><line x1="435.9" y1="579.2" x2="435.9" y2="587.2" stroke-width="1.8"/><line x1="435.9" y1="579.2" x2="435.9" y2="587.2" stroke-width="1.8"/><circle cx="435.9" cy="583.2" r="3.8"/></g>
<text class="val-text" x="707" y="587.2">0.793× [0.793, 0.793]</text>
<text class="row-label" x="203" y="611.2" text-anchor="end">ScanRecords</text>
<g class="ci-win"><title>ScanRecords: 0.828× [0.828, 0.828]</title><line x1="470.1" y1="607.2" x2="470.1" y2="607.2" stroke-width="2.2"/><line x1="470.1" y1="603.2" x2="470.1" y2="611.2" stroke-width="1.8"/><line x1="470.1" y1="603.2" x2="470.1" y2="611.2" stroke-width="1.8"/><circle cx="470.1" cy="607.2" r="3.8"/></g>
<text class="val-text" x="707" y="611.2">0.828× [0.828, 0.828]</text>
<text class="row-label" x="203" y="635.2" text-anchor="end">ScanRecordsSplitVersioned</text>
<g class="ci-win"><title>ScanRecordsSplitVersioned: 0.816× [0.816, 0.816]</title><line x1="458.5" y1="631.2" x2="458.5" y2="631.2" stroke-width="2.2"/><line x1="458.5" y1="627.2" x2="458.5" y2="635.2" stroke-width="1.8"/><line x1="458.5" y1="627.2" x2="458.5" y2="635.2" stroke-width="1.8"/><circle cx="458.5" cy="631.2" r="3.8"/></g>
<text class="val-text" x="707" y="635.2">0.816× [0.816, 0.816]</text>
<text class="row-label" x="203" y="659.2" text-anchor="end">ScanIndexRecords/serial</text>
<g class="ci-win"><title>ScanIndexRecords/serial: 0.651× [0.651, 0.651]</title><line x1="279.6" y1="655.2" x2="279.6" y2="655.2" stroke-width="2.2"/><line x1="279.6" y1="651.2" x2="279.6" y2="659.2" stroke-width="1.8"/><line x1="279.6" y1="651.2" x2="279.6" y2="659.2" stroke-width="1.8"/><circle cx="279.6" cy="655.2" r="3.8"/></g>
<text class="val-text" x="707" y="659.2">0.651× [0.651, 0.651]</text>
<text class="row-label" x="203" y="683.2" text-anchor="end">ScanIndexRecords/pipelined</text>
<g class="ci-win"><title>ScanIndexRecords/pipelined: 0.627× [0.626, 0.627]</title><line x1="248.6" y1="679.2" x2="249.9" y2="679.2" stroke-width="2.2"/><line x1="248.6" y1="675.2" x2="248.6" y2="683.2" stroke-width="1.8"/><line x1="249.9" y1="675.2" x2="249.9" y2="683.2" stroke-width="1.8"/><circle cx="249.9" cy="679.2" r="3.8"/></g>
<text class="val-text" x="707" y="683.2">0.627× [0.626, 0.627]</text>
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
