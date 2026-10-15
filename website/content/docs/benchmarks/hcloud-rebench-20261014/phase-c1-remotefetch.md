---
title: "Phase C1: Remote Fetch (Forward)"
description: "Forward USE_REMOTE_FETCH vs SCAN_AND_FETCH across pr2 and final trees on small and two-chunk split records."
weight: 4
---

Part of the [hcloud-rebench-20261014](../) campaign.

`BenchmarkIndexFetch` in two trees and two fetch methods, counterbalanced.
`USE_REMOTE_FETCH` asks the storage server to resolve each index entry to its
record (a mapped range read); `SCAN_AND_FETCH` scans the index and loads each
record from the client. The question: what does remote fetch buy over the
ordinary path, both in the tree that introduced it and in the final tree?

## Arms

| Arm | Source | Fetch method |
|---|---|---|
| pr2-ordinary | `5052212f520d1f888429d0bbcc749b657e27f39e` (first remote-fetch tree) | `SCAN_AND_FETCH` |
| pr2-remote | `5052212f520d1f888429d0bbcc749b657e27f39e` | `USE_REMOTE_FETCH` |
| final-ordinary | `267a41434a81723b07e7150869d702636a1b91a9` | `SCAN_AND_FETCH` |
| final-remote | `267a41434a81723b07e7150869d702636a1b91a9` | `USE_REMOTE_FETCH` |

Both trees ran byte-identical benchmark source
(`pkg/recordlayer/benchfetch`, harness
`d94b4c61377d173b7c6884fde426a227dab0f0bb`; see
[campaign-harness.md](https://github.com/birdayz/fdb-go/blob/master/docs/benchmarks/campaign-harness.md)).

## Environment and design

- Host, CPU groups and network path as in
  [phase A](../phase-a-client/) and
  [phase B](../phase-b-recordlayer/): Hetzner `ccx43`, one
  `fdbserver` 7.3.77 process on CPUs 2–7 with the `memory` engine and single
  redundancy, the benchmark process on CPUs 8–15 in a network namespace
  reaching the server over a veth pair.
- Fixture: 1,000 records behind a value index, seeded and read back before any
  timer. `small` records are one key-value each; `split` records carry a
  100,000-byte payload and are stored as two chunks. Every measured operation's
  result (IDs, order, fields, payload checksum, terminal reason) is checked
  with the timer stopped, and an instrumented run before and after the loop
  asserts the path: remote fetch with no index scan or record load, or an
  index scan with record loads for `SCAN_AND_FETCH`.
- Cases (all forward scans):
  - `limit-1-broad`: a scan over the whole index that stops after one row;
  - `exact-10`: IDs 100–109;
  - `all-1000`: all 1,000 records.
- Per case: 8 blocks over the four arms in a Williams-square order (seed
  641201), so each arm precedes each other arm equally often. One fresh process
  per arm and block, three repeats each (`-test.count=3`, 3 s per repeat): 24
  samples per arm and case, 192 processes and 576 samples in total.
- Estimand and bootstrap as in phase B: geometric mean over blocks of each
  block's ratio of process medians, nominal 95% percentile bootstrap over
  blocks (20,000 resamples, seed 391706). Absolute values are the median over
  blocks of the process medians. `ns/op` is testing.B's mean, not a percentile.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure C1.1: Forward Remote Fetch vs Ordinary Scan — Final Tree (log₂ scale, 95% CI)</strong> — 8 Williams-square blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.</figcaption>
<svg class="bench-svg" viewBox="0 0 860 580" role="img" aria-label="Figure C1.1: Forward Remote Fetch vs Ordinary Scan — Final Tree (log₂ scale, 95% CI)">
<line class="grid-line" x1="215.0" y1="28" x2="215.0" y2="550"/>
<text class="axis-text" x="215.0" y="22" text-anchor="middle">0.30×</text>
<text class="axis-text" x="215.0" y="565" text-anchor="middle">0.30×</text>
<line class="grid-line" x1="382.2" y1="28" x2="382.2" y2="550"/>
<text class="axis-text" x="382.2" y="22" text-anchor="middle">0.50×</text>
<text class="axis-text" x="382.2" y="565" text-anchor="middle">0.50×</text>
<line class="grid-line" x1="492.4" y1="28" x2="492.4" y2="550"/>
<text class="axis-text" x="492.4" y="22" text-anchor="middle">0.70×</text>
<text class="axis-text" x="492.4" y="565" text-anchor="middle">0.70×</text>
<line class="parity-line" x1="609.1" y1="28" x2="609.1" y2="550"/>
<text class="axis-text" x="609.1" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="609.1" y="565" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="695.0" y1="28" x2="695.0" y2="550"/>
<text class="axis-text" x="695.0" y="22" text-anchor="middle">1.3×</text>
<text class="axis-text" x="695.0" y="565" text-anchor="middle">1.3×</text>
<text class="axis-text" x="601.1" y="9" text-anchor="end">← USE_REMOTE_FETCH lower</text>
<text class="axis-text" x="617.1" y="9" text-anchor="start">SCAN_AND_FETCH lower →</text>
<line class="section-divider" x1="8" y1="40" x2="852" y2="40"/>
<text class="section-title" x="10" y="55">Mean Time (ns/op): final-remote / final-ordinary</text>
<text class="row-label" x="203" y="79.2" text-anchor="end">small-limit-1-broad</text>
<g class="ci-win"><title>small-limit-1-broad: 0.838× [0.795, 0.900]</title><line x1="534.0" y1="75.2" x2="574.6" y2="75.2" stroke-width="2.2"/><line x1="534.0" y1="71.2" x2="534.0" y2="79.2" stroke-width="1.8"/><line x1="574.6" y1="71.2" x2="574.6" y2="79.2" stroke-width="1.8"/><circle cx="551.3" cy="75.2" r="3.8"/></g>
<text class="val-text" x="707" y="79.2">0.838× [0.795, 0.900]</text>
<text class="row-label" x="203" y="103.2" text-anchor="end">split-limit-1-broad</text>
<g class="ci-win"><title>split-limit-1-broad: 0.707× [0.685, 0.735]</title><line x1="485.3" y1="99.2" x2="508.3" y2="99.2" stroke-width="2.2"/><line x1="485.3" y1="95.2" x2="485.3" y2="103.2" stroke-width="1.8"/><line x1="508.3" y1="95.2" x2="508.3" y2="103.2" stroke-width="1.8"/><circle cx="495.6" cy="99.2" r="3.8"/></g>
<text class="val-text" x="707" y="103.2">0.707× [0.685, 0.735]</text>
<text class="row-label" x="203" y="127.2" text-anchor="end">small-exact-10</text>
<g class="ci-win"><title>small-exact-10: 0.733× [0.724, 0.743]</title><line x1="503.4" y1="123.2" x2="511.9" y2="123.2" stroke-width="2.2"/><line x1="503.4" y1="119.2" x2="503.4" y2="127.2" stroke-width="1.8"/><line x1="511.9" y1="119.2" x2="511.9" y2="127.2" stroke-width="1.8"/><circle cx="507.4" cy="123.2" r="3.8"/></g>
<text class="val-text" x="707" y="127.2">0.733× [0.724, 0.743]</text>
<text class="row-label" x="203" y="151.2" text-anchor="end">split-exact-10</text>
<g class="ci-win"><title>split-exact-10: 0.872× [0.843, 0.904]</title><line x1="553.2" y1="147.2" x2="576.1" y2="147.2" stroke-width="2.2"/><line x1="553.2" y1="143.2" x2="553.2" y2="151.2" stroke-width="1.8"/><line x1="576.1" y1="143.2" x2="576.1" y2="151.2" stroke-width="1.8"/><circle cx="564.3" cy="147.2" r="3.8"/></g>
<text class="val-text" x="707" y="151.2">0.872× [0.843, 0.904]</text>
<text class="row-label" x="203" y="175.2" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.341× [0.321, 0.356]</title><line x1="237.1" y1="171.2" x2="271.0" y2="171.2" stroke-width="2.2"/><line x1="237.1" y1="167.2" x2="237.1" y2="175.2" stroke-width="1.8"/><line x1="271.0" y1="167.2" x2="271.0" y2="175.2" stroke-width="1.8"/><circle cx="256.9" cy="171.2" r="3.8"/></g>
<text class="val-text" x="707" y="175.2">0.341× [0.321, 0.356]</text>
<text class="row-label" x="203" y="199.2" text-anchor="end">split-all-1000</text>
<g class="ci-reg"><title>split-all-1000: 1.150× [1.129, 1.181]</title><line x1="648.8" y1="195.2" x2="663.6" y2="195.2" stroke-width="2.2"/><line x1="648.8" y1="191.2" x2="648.8" y2="199.2" stroke-width="1.8"/><line x1="663.6" y1="191.2" x2="663.6" y2="199.2" stroke-width="1.8"/><circle cx="654.9" cy="195.2" r="3.8"/></g>
<text class="val-text" x="707" y="199.2">1.150× [1.129, 1.181]</text>
<line class="section-divider" x1="8" y1="210" x2="852" y2="210"/>
<text class="section-title" x="10" y="225">Go Bytes (B/op): final-remote / final-ordinary</text>
<text class="row-label" x="203" y="249.2" text-anchor="end">small-limit-1-broad</text>
<g class="ci-win"><title>small-limit-1-broad: 0.960× [0.960, 0.961]</title><line x1="595.8" y1="245.2" x2="596.1" y2="245.2" stroke-width="2.2"/><line x1="595.8" y1="241.2" x2="595.8" y2="249.2" stroke-width="1.8"/><line x1="596.1" y1="241.2" x2="596.1" y2="249.2" stroke-width="1.8"/><circle cx="595.8" cy="245.2" r="3.8"/></g>
<text class="val-text" x="707" y="249.2">0.960× [0.960, 0.961]</text>
<text class="row-label" x="203" y="273.2" text-anchor="end">split-limit-1-broad</text>
<g class="ci-win"><title>split-limit-1-broad: 0.715× [0.715, 0.715]</title><line x1="499.3" y1="269.2" x2="499.3" y2="269.2" stroke-width="2.2"/><line x1="499.3" y1="265.2" x2="499.3" y2="273.2" stroke-width="1.8"/><line x1="499.3" y1="265.2" x2="499.3" y2="273.2" stroke-width="1.8"/><circle cx="499.3" cy="269.2" r="3.8"/></g>
<text class="val-text" x="707" y="273.2">0.715× [0.715, 0.715]</text>
<text class="row-label" x="203" y="297.2" text-anchor="end">small-exact-10</text>
<g class="ci-win"><title>small-exact-10: 0.751× [0.750, 0.751]</title><line x1="514.9" y1="293.2" x2="515.4" y2="293.2" stroke-width="2.2"/><line x1="514.9" y1="289.2" x2="514.9" y2="297.2" stroke-width="1.8"/><line x1="515.4" y1="289.2" x2="515.4" y2="297.2" stroke-width="1.8"/><circle cx="515.4" cy="293.2" r="3.8"/></g>
<text class="val-text" x="707" y="297.2">0.751× [0.750, 0.751]</text>
<text class="row-label" x="203" y="321.2" text-anchor="end">split-exact-10</text>
<g class="ci-win"><title>split-exact-10: 0.695× [0.695, 0.695]</title><line x1="490.0" y1="317.2" x2="490.0" y2="317.2" stroke-width="2.2"/><line x1="490.0" y1="313.2" x2="490.0" y2="321.2" stroke-width="1.8"/><line x1="490.0" y1="313.2" x2="490.0" y2="321.2" stroke-width="1.8"/><circle cx="490.0" cy="317.2" r="3.8"/></g>
<text class="val-text" x="707" y="321.2">0.695× [0.695, 0.695]</text>
<text class="row-label" x="203" y="345.2" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.512× [0.512, 0.512]</title><line x1="390.0" y1="341.2" x2="390.0" y2="341.2" stroke-width="2.2"/><line x1="390.0" y1="337.2" x2="390.0" y2="345.2" stroke-width="1.8"/><line x1="390.0" y1="337.2" x2="390.0" y2="345.2" stroke-width="1.8"/><circle cx="390.0" cy="341.2" r="3.8"/></g>
<text class="val-text" x="707" y="345.2">0.512× [0.512, 0.512]</text>
<text class="row-label" x="203" y="369.2" text-anchor="end">split-all-1000</text>
<g class="ci-win"><title>split-all-1000: 0.692× [0.692, 0.692]</title><line x1="488.6" y1="365.2" x2="488.6" y2="365.2" stroke-width="2.2"/><line x1="488.6" y1="361.2" x2="488.6" y2="369.2" stroke-width="1.8"/><line x1="488.6" y1="361.2" x2="488.6" y2="369.2" stroke-width="1.8"/><circle cx="488.6" cy="365.2" r="3.8"/></g>
<text class="val-text" x="707" y="369.2">0.692× [0.692, 0.692]</text>
<line class="section-divider" x1="8" y1="380" x2="852" y2="380"/>
<text class="section-title" x="10" y="395">Go Allocations (allocs/op): final-remote / final-ordinary</text>
<text class="row-label" x="203" y="419.2" text-anchor="end">small-limit-1-broad</text>
<g class="ci-reg"><title>small-limit-1-broad: 1.046× [1.046, 1.046]</title><line x1="623.8" y1="415.2" x2="623.8" y2="415.2" stroke-width="2.2"/><line x1="623.8" y1="411.2" x2="623.8" y2="419.2" stroke-width="1.8"/><line x1="623.8" y1="411.2" x2="623.8" y2="419.2" stroke-width="1.8"/><circle cx="623.8" cy="415.2" r="3.8"/></g>
<text class="val-text" x="707" y="419.2">1.046× [1.046, 1.046]</text>
<text class="row-label" x="203" y="443.2" text-anchor="end">split-limit-1-broad</text>
<g class="ci-win"><title>split-limit-1-broad: 0.942× [0.940, 0.944]</title><line x1="588.9" y1="439.2" x2="590.3" y2="439.2" stroke-width="2.2"/><line x1="588.9" y1="435.2" x2="588.9" y2="443.2" stroke-width="1.8"/><line x1="590.3" y1="435.2" x2="590.3" y2="443.2" stroke-width="1.8"/><circle cx="589.6" cy="439.2" r="3.8"/></g>
<text class="val-text" x="707" y="443.2">0.942× [0.940, 0.944]</text>
<text class="row-label" x="203" y="467.2" text-anchor="end">small-exact-10</text>
<g class="ci-win"><title>small-exact-10: 0.600× [0.600, 0.600]</title><line x1="441.9" y1="463.2" x2="441.9" y2="463.2" stroke-width="2.2"/><line x1="441.9" y1="459.2" x2="441.9" y2="467.2" stroke-width="1.8"/><line x1="441.9" y1="459.2" x2="441.9" y2="467.2" stroke-width="1.8"/><circle cx="441.9" cy="463.2" r="3.8"/></g>
<text class="val-text" x="707" y="467.2">0.600× [0.600, 0.600]</text>
<text class="row-label" x="203" y="491.2" text-anchor="end">split-exact-10</text>
<g class="ci-win"><title>split-exact-10: 0.515× [0.514, 0.516]</title><line x1="391.3" y1="487.2" x2="392.5" y2="487.2" stroke-width="2.2"/><line x1="391.3" y1="483.2" x2="391.3" y2="491.2" stroke-width="1.8"/><line x1="392.5" y1="483.2" x2="392.5" y2="491.2" stroke-width="1.8"/><circle cx="391.9" cy="487.2" r="3.8"/></g>
<text class="val-text" x="707" y="491.2">0.515× [0.514, 0.516]</text>
<text class="row-label" x="203" y="515.2" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.436× [0.436, 0.436]</title><line x1="337.4" y1="511.2" x2="337.4" y2="511.2" stroke-width="2.2"/><line x1="337.4" y1="507.2" x2="337.4" y2="515.2" stroke-width="1.8"/><line x1="337.4" y1="507.2" x2="337.4" y2="515.2" stroke-width="1.8"/><circle cx="337.4" cy="511.2" r="3.8"/></g>
<text class="val-text" x="707" y="515.2">0.436× [0.436, 0.436]</text>
<text class="row-label" x="203" y="539.2" text-anchor="end">split-all-1000</text>
<g class="ci-win"><title>split-all-1000: 0.404× [0.404, 0.404]</title><line x1="312.4" y1="535.2" x2="312.4" y2="535.2" stroke-width="2.2"/><line x1="312.4" y1="531.2" x2="312.4" y2="539.2" stroke-width="1.8"/><line x1="312.4" y1="531.2" x2="312.4" y2="539.2" stroke-width="1.8"/><circle cx="312.4" cy="535.2" r="3.8"/></g>
<text class="val-text" x="707" y="539.2">0.404× [0.404, 0.404]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

Ratios with nominal 95% intervals. Below 1 means the numerator arm is faster or
allocates less.

### Remote fetch vs ordinary fetch, final tree

final-remote / final-ordinary.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| small-limit-1-broad | 0.838 [0.795, 0.900] | 0.960 [0.960, 0.961] | 1.046 [1.046, 1.046] |
| split-limit-1-broad | 0.707 [0.685, 0.735] | 0.715 [0.715, 0.715] | 0.942 [0.940, 0.944] |
| small-exact-10 | 0.733 [0.724, 0.743] | 0.751 [0.750, 0.751] | 0.600 [0.600, 0.600] |
| split-exact-10 | 0.872 [0.843, 0.904] | 0.695 [0.695, 0.695] | 0.515 [0.514, 0.516] |
| small-all-1000 | 0.341 [0.321, 0.356] | 0.512 [0.512, 0.512] | 0.436 [0.436, 0.436] |
| split-all-1000 | 1.150 [1.129, 1.181] | 0.692 [0.692, 0.692] | 0.404 [0.404, 0.404] |

### Remote fetch vs ordinary fetch, first remote-fetch tree

pr2-remote / pr2-ordinary.

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| small-limit-1-broad | 0.839 [0.810, 0.873] | 0.992 [0.992, 0.993] | 1.096 [1.096, 1.096] |
| split-limit-1-broad | 0.749 [0.721, 0.792] | 0.716 [0.716, 0.717] | 0.995 [0.993, 0.997] |
| small-exact-10 | 0.757 [0.746, 0.771] | 0.830 [0.829, 0.831] | 0.808 [0.808, 0.809] |
| split-exact-10 | 0.900 [0.863, 0.941] | 0.697 [0.697, 0.697] | 0.682 [0.681, 0.683] |
| small-all-1000 | 0.436 [0.423, 0.447] | 0.665 [0.665, 0.666] | 0.707 [0.707, 0.707] |
| split-all-1000 | 1.213 [1.182, 1.243] | 0.693 [0.693, 0.693] | 0.628 [0.628, 0.628] |

### Final vs first remote-fetch tree, per method

final-remote / pr2-remote:

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| small-limit-1-broad | 1.006 [0.978, 1.038] | 0.967 [0.966, 0.968] | 0.954 [0.954, 0.954] |
| split-limit-1-broad | 0.944 [0.891, 0.996] | 0.998 [0.997, 0.998] | 0.945 [0.944, 0.947] |
| small-exact-10 | 0.975 [0.962, 0.987] | 0.905 [0.905, 0.906] | 0.743 [0.742, 0.743] |
| split-exact-10 | 1.005 [0.952, 1.058] | 0.998 [0.997, 0.998] | 0.755 [0.754, 0.756] |
| small-all-1000 | 0.799 [0.770, 0.824] | 0.769 [0.769, 0.769] | 0.617 [0.617, 0.617] |
| split-all-1000 | 0.953 [0.932, 0.973] | 0.998 [0.997, 0.998] | 0.643 [0.643, 0.644] |

final-ordinary / pr2-ordinary (control: the ordinary path):

| Case | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| small-limit-1-broad | 1.006 [0.925, 1.091] | 0.999 [0.999, 1.000] | 1.000 [1.000, 1.000] |
| split-limit-1-broad | 1.002 [0.978, 1.025] | 1.000 [0.999, 1.000] | 0.999 [0.997, 1.000] |
| small-exact-10 | 1.007 [0.997, 1.019] | 1.001 [1.000, 1.002] | 1.001 [1.000, 1.001] |
| split-exact-10 | 1.037 [1.013, 1.060] | 1.000 [1.000, 1.000] | 1.000 [0.999, 1.000] |
| small-all-1000 | 1.024 [1.010, 1.040] | 1.000 [0.999, 1.000] | 1.000 [1.000, 1.000] |
| split-all-1000 | 1.005 [0.966, 1.049] | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] |

### Absolute values

Median over blocks of each process's median of its three repeats.

| Case | pr2-ordinary | pr2-remote | final-ordinary | final-remote |
|---|---:|---:|---:|---:|
| small-limit-1-broad | 992 µs | 816 µs | 1.01 ms | 824 µs |
| split-limit-1-broad | 1.71 ms | 1.26 ms | 1.71 ms | 1.19 ms |
| small-exact-10 | 1.37 ms | 1.04 ms | 1.38 ms | 1.01 ms |
| split-exact-10 | 4.05 ms | 3.59 ms | 4.26 ms | 3.69 ms |
| small-all-1000 | 43.1 ms | 18.9 ms | 44.0 ms | 15.3 ms |
| split-all-1000 | 162 ms | 196 ms | 163 ms | 188 ms |

Final tree, allocations per operation:

| Case | B/op ordinary | B/op remote | allocs/op ordinary | allocs/op remote |
|---|---:|---:|---:|---:|
| small-limit-1-broad | 17,670 | 16,972 | 219 | 229 |
| split-limit-1-broad | 478,972 | 342,347 | 265 | 249 |
| small-exact-10 | 93,684 | 70,348 | 690 | 414 |
| split-exact-10 | 4,652,112 | 3,233,806 | 1,032 | 532 |
| small-all-1000 | 4,562,622 | 2,335,138 | 51,966 | 22,664 |
| split-all-1000 | 457,420,264 | 316,337,938 | 77,736 | 31,401 |

## Interpretation

Measured:

- **Remote fetch is faster in five of six cases**, in both trees. In the final
  tree it takes 0.71–0.87 of the ordinary path's time for the one-row and
  ten-row scans and 0.34 for the 1,000 small records (44.0 → 15.3 ms).
- **One retained negative result: 1,000 split records.** Remote fetch is 15%
  slower in the final tree (1.150 [1.129, 1.181]; 163 → 188 ms) and 21%
  slower in the first tree (1.213), although it allocates 31% fewer bytes and
  60% fewer objects. Every one of the 8 block ratios is above 1.11.
- **The final tree improved remote fetch itself.** final-remote/pr2-remote has
  `allocs/op` 0.62–0.95 in every case and time 0.80 [0.77, 0.82] for 1,000
  small records and 0.95 [0.93, 0.97] for 1,000 split records. In the one-row
  and ten-row cases time is 0.94–1.01; only `split-limit-1-broad`
  (0.944 [0.891, 0.996]) and `small-exact-10` (0.975 [0.962, 0.987]) have
  intervals below 1.
- **The ordinary path did not change between the trees** in bytes or
  allocations (all ratios within 0.1% of 1). Its time ratios are 1.002–1.037;
  `split-exact-10` (1.037 [1.013, 1.060]) and `small-all-1000`
  (1.024 [1.010, 1.040]) have intervals above 1. With identical allocations,
  this run does not show where those 2–4% come from.

Hypotheses, not tested here:

- For 1,000 split records, the storage server reads and returns about 100 MB
  per operation itself; on one `fdbserver` process that serial server-side
  work may cost more than the client-side pipelined loads it replaces. The
  allocation ratios show the client does less work; server CPU was not
  profiled.
- The benefit measured here excludes inter-host and physical-network latency:
  client and server share a host and talk over a veth pair. Remote fetch saves client round trips, so
  its effect over a real network is likely different. The multi-node campaign
  (`tools/benchcampaign/multinode`) is designed to measure that.

## Limits

- Single host, one `fdbserver` process, single redundancy, `memory` engine: no
  replication, no network hop, no disk. One operation at a time.
- testing.B reports means; no latency percentiles.
- Eight blocks give limited resolution; intervals are nominal, per case and
  metric, with no multiplicity correction and no aggregate across cases.
  Numerator-first and denominator-first blocks agree in direction for every
  remote/ordinary time ratio; the largest spread is pr2 `split-exact-10`
  (0.947 vs 0.855).
- Case labels in the aggregate CSV were reconstructed from the pinned plan and
  the unchanged per-run logs, for the same runner bug described in
  [phase B](phase-b-recordlayer.md#limits); 576 labels were
  restored in a separate working copy and no measurement was repeated or
  changed.
