---
title: "Phase C1: Remote fetch (forward)"
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
<figcaption><strong>Figure C1.1: Forward scans, USE_REMOTE_FETCH / SCAN_AND_FETCH</strong><span>Below 1.0× favors USE_REMOTE_FETCH. Single in-memory server.</span></figcaption>
<p class="bench-summary">USE_REMOTE_FETCH is faster in 5 of 6 cases, down to 0.341×, and slower on split-all-1000 (1.150×).</p>
<svg class="bench-svg" viewBox="0 0 1329 500" role="img" aria-label="Figure C1.1: Forward scans, USE_REMOTE_FETCH / SCAN_AND_FETCH">
<line class="grid-line" x1="174.0" y1="36" x2="174.0" y2="474"/>
<text class="axis-text" x="174.0" y="30" text-anchor="middle">0.3×</text>
<text class="axis-text" x="174.0" y="489" text-anchor="middle">0.3×</text>
<line class="grid-line" x1="508.4" y1="36" x2="508.4" y2="474"/>
<text class="axis-text" x="508.4" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="508.4" y="489" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="728.7" y1="36" x2="728.7" y2="474"/>
<text class="axis-text" x="728.7" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="728.7" y="489" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="962.2" y1="36" x2="962.2" y2="474"/>
<text class="axis-text" x="962.2" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="962.2" y="489" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1134.0" y1="36" x2="1134.0" y2="474"/>
<text class="axis-text" x="1134.0" y="30" text-anchor="middle">1.3×</text>
<text class="axis-text" x="1134.0" y="489" text-anchor="middle">1.3×</text>
<text class="axis-text" x="954.2" y="17" text-anchor="end">← USE_REMOTE_FETCH lower</text>
<text class="axis-text" x="970.2" y="17" text-anchor="start">SCAN_AND_FETCH lower →</text>
<line class="section-divider" x1="8" y1="48" x2="1321" y2="48"/>
<text class="section-title" x="10" y="63">Mean time (ns/op): final-remote / final-ordinary</text>
<text class="row-label" x="162" y="81.0" text-anchor="end">small-limit-1-broad</text>
<g class="ci-win"><title>small-limit-1-broad: 0.838× [0.795, 0.900]</title><line x1="812.0" y1="77.0" x2="893.3" y2="77.0" stroke-width="2.2"/><line x1="812.0" y1="73.0" x2="812.0" y2="81.0" stroke-width="1.8"/><line x1="893.3" y1="73.0" x2="893.3" y2="81.0" stroke-width="1.8"/><circle cx="846.5" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1146" y="81.0">0.838× [0.795, 0.900]</text>
<text class="row-label" x="162" y="101.0" text-anchor="end">split-limit-1-broad</text>
<g class="ci-win"><title>split-limit-1-broad: 0.707× [0.685, 0.735]</title><line x1="714.5" y1="97.0" x2="760.7" y2="97.0" stroke-width="2.2"/><line x1="714.5" y1="93.0" x2="714.5" y2="101.0" stroke-width="1.8"/><line x1="760.7" y1="93.0" x2="760.7" y2="101.0" stroke-width="1.8"/><circle cx="735.2" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1146" y="101.0">0.707× [0.685, 0.735]</text>
<text class="row-label" x="162" y="121.0" text-anchor="end">small-exact-10</text>
<g class="ci-win"><title>small-exact-10: 0.733× [0.724, 0.743]</title><line x1="750.8" y1="117.0" x2="767.7" y2="117.0" stroke-width="2.2"/><line x1="750.8" y1="113.0" x2="750.8" y2="121.0" stroke-width="1.8"/><line x1="767.7" y1="113.0" x2="767.7" y2="121.0" stroke-width="1.8"/><circle cx="758.9" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1146" y="121.0">0.733× [0.724, 0.743]</text>
<text class="row-label" x="162" y="141.0" text-anchor="end">split-exact-10</text>
<g class="ci-win"><title>split-exact-10: 0.872× [0.843, 0.904]</title><line x1="850.4" y1="137.0" x2="896.2" y2="137.0" stroke-width="2.2"/><line x1="850.4" y1="133.0" x2="850.4" y2="141.0" stroke-width="1.8"/><line x1="896.2" y1="133.0" x2="896.2" y2="141.0" stroke-width="1.8"/><circle cx="872.6" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1146" y="141.0">0.872× [0.843, 0.904]</text>
<text class="row-label" x="162" y="161.0" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.341× [0.321, 0.356]</title><line x1="218.3" y1="157.0" x2="286.0" y2="157.0" stroke-width="2.2"/><line x1="218.3" y1="153.0" x2="218.3" y2="161.0" stroke-width="1.8"/><line x1="286.0" y1="153.0" x2="286.0" y2="161.0" stroke-width="1.8"/><circle cx="257.9" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1146" y="161.0">0.341× [0.321, 0.356]</text>
<text class="row-label" x="162" y="181.0" text-anchor="end">split-all-1000</text>
<g class="ci-reg"><title>split-all-1000: 1.150× [1.129, 1.181]</title><line x1="1041.7" y1="177.0" x2="1071.1" y2="177.0" stroke-width="2.2"/><line x1="1041.7" y1="173.0" x2="1041.7" y2="181.0" stroke-width="1.8"/><line x1="1071.1" y1="173.0" x2="1071.1" y2="181.0" stroke-width="1.8"/><circle cx="1053.7" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1146" y="181.0">1.150× [1.129, 1.181]</text>
<line class="section-divider" x1="8" y1="190" x2="1321" y2="190"/>
<text class="section-title" x="10" y="205">Go bytes (B/op): final-remote / final-ordinary</text>
<text class="row-label" x="162" y="223.0" text-anchor="end">small-limit-1-broad</text>
<g class="ci-win"><title>small-limit-1-broad: 0.960× [0.960, 0.961]</title><line x1="935.5" y1="219.0" x2="936.2" y2="219.0" stroke-width="2.2"/><line x1="935.5" y1="215.0" x2="935.5" y2="223.0" stroke-width="1.8"/><line x1="936.2" y1="215.0" x2="936.2" y2="223.0" stroke-width="1.8"/><circle cx="935.5" cy="219.0" r="3.8"/></g>
<text class="val-text" x="1146" y="223.0">0.960× [0.960, 0.961]</text>
<text class="row-label" x="162" y="243.0" text-anchor="end">split-limit-1-broad</text>
<g class="ci-win"><title>split-limit-1-broad: 0.715× [0.715, 0.715]</title><line x1="742.6" y1="239.0" x2="742.6" y2="239.0" stroke-width="2.2"/><line x1="742.6" y1="235.0" x2="742.6" y2="243.0" stroke-width="1.8"/><line x1="742.6" y1="235.0" x2="742.6" y2="243.0" stroke-width="1.8"/><circle cx="742.6" cy="239.0" r="3.8"/></g>
<text class="val-text" x="1146" y="243.0">0.715× [0.715, 0.715]</text>
<text class="row-label" x="162" y="263.0" text-anchor="end">small-exact-10</text>
<g class="ci-win"><title>small-exact-10: 0.751× [0.750, 0.751]</title><line x1="773.9" y1="259.0" x2="774.8" y2="259.0" stroke-width="2.2"/><line x1="773.9" y1="255.0" x2="773.9" y2="263.0" stroke-width="1.8"/><line x1="774.8" y1="255.0" x2="774.8" y2="263.0" stroke-width="1.8"/><circle cx="774.8" cy="259.0" r="3.8"/></g>
<text class="val-text" x="1146" y="263.0">0.751× [0.750, 0.751]</text>
<text class="row-label" x="162" y="283.0" text-anchor="end">split-exact-10</text>
<g class="ci-win"><title>split-exact-10: 0.695× [0.695, 0.695]</title><line x1="724.0" y1="279.0" x2="724.0" y2="279.0" stroke-width="2.2"/><line x1="724.0" y1="275.0" x2="724.0" y2="283.0" stroke-width="1.8"/><line x1="724.0" y1="275.0" x2="724.0" y2="283.0" stroke-width="1.8"/><circle cx="724.0" cy="279.0" r="3.8"/></g>
<text class="val-text" x="1146" y="283.0">0.695× [0.695, 0.695]</text>
<text class="row-label" x="162" y="303.0" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.512× [0.512, 0.512]</title><line x1="524.0" y1="299.0" x2="524.0" y2="299.0" stroke-width="2.2"/><line x1="524.0" y1="295.0" x2="524.0" y2="303.0" stroke-width="1.8"/><line x1="524.0" y1="295.0" x2="524.0" y2="303.0" stroke-width="1.8"/><circle cx="524.0" cy="299.0" r="3.8"/></g>
<text class="val-text" x="1146" y="303.0">0.512× [0.512, 0.512]</text>
<text class="row-label" x="162" y="323.0" text-anchor="end">split-all-1000</text>
<g class="ci-win"><title>split-all-1000: 0.692× [0.692, 0.692]</title><line x1="721.2" y1="319.0" x2="721.2" y2="319.0" stroke-width="2.2"/><line x1="721.2" y1="315.0" x2="721.2" y2="323.0" stroke-width="1.8"/><line x1="721.2" y1="315.0" x2="721.2" y2="323.0" stroke-width="1.8"/><circle cx="721.2" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1146" y="323.0">0.692× [0.692, 0.692]</text>
<line class="section-divider" x1="8" y1="332" x2="1321" y2="332"/>
<text class="section-title" x="10" y="347">Go allocations (allocs/op): final-remote / final-ordinary</text>
<text class="row-label" x="162" y="365.0" text-anchor="end">small-limit-1-broad</text>
<g class="ci-reg"><title>small-limit-1-broad: 1.046× [1.046, 1.046]</title><line x1="991.7" y1="361.0" x2="991.7" y2="361.0" stroke-width="2.2"/><line x1="991.7" y1="357.0" x2="991.7" y2="365.0" stroke-width="1.8"/><line x1="991.7" y1="357.0" x2="991.7" y2="365.0" stroke-width="1.8"/><circle cx="991.7" cy="361.0" r="3.8"/></g>
<text class="val-text" x="1146" y="365.0">1.046× [1.046, 1.046]</text>
<text class="row-label" x="162" y="385.0" text-anchor="end">split-limit-1-broad</text>
<g class="ci-win"><title>split-limit-1-broad: 0.942× [0.940, 0.944]</title><line x1="921.7" y1="381.0" x2="924.5" y2="381.0" stroke-width="2.2"/><line x1="921.7" y1="377.0" x2="921.7" y2="385.0" stroke-width="1.8"/><line x1="924.5" y1="377.0" x2="924.5" y2="385.0" stroke-width="1.8"/><circle cx="923.1" cy="381.0" r="3.8"/></g>
<text class="val-text" x="1146" y="385.0">0.942× [0.940, 0.944]</text>
<text class="row-label" x="162" y="405.0" text-anchor="end">small-exact-10</text>
<g class="ci-win"><title>small-exact-10: 0.600× [0.600, 0.600]</title><line x1="627.8" y1="401.0" x2="627.8" y2="401.0" stroke-width="2.2"/><line x1="627.8" y1="397.0" x2="627.8" y2="405.0" stroke-width="1.8"/><line x1="627.8" y1="397.0" x2="627.8" y2="405.0" stroke-width="1.8"/><circle cx="627.8" cy="401.0" r="3.8"/></g>
<text class="val-text" x="1146" y="405.0">0.600× [0.600, 0.600]</text>
<text class="row-label" x="162" y="425.0" text-anchor="end">split-exact-10</text>
<g class="ci-win"><title>split-exact-10: 0.515× [0.514, 0.516]</title><line x1="526.5" y1="421.0" x2="529.1" y2="421.0" stroke-width="2.2"/><line x1="526.5" y1="417.0" x2="526.5" y2="425.0" stroke-width="1.8"/><line x1="529.1" y1="417.0" x2="529.1" y2="425.0" stroke-width="1.8"/><circle cx="527.8" cy="421.0" r="3.8"/></g>
<text class="val-text" x="1146" y="425.0">0.515× [0.514, 0.516]</text>
<text class="row-label" x="162" y="445.0" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.436× [0.436, 0.436]</title><line x1="418.8" y1="441.0" x2="418.8" y2="441.0" stroke-width="2.2"/><line x1="418.8" y1="437.0" x2="418.8" y2="445.0" stroke-width="1.8"/><line x1="418.8" y1="437.0" x2="418.8" y2="445.0" stroke-width="1.8"/><circle cx="418.8" cy="441.0" r="3.8"/></g>
<text class="val-text" x="1146" y="445.0">0.436× [0.436, 0.436]</text>
<text class="row-label" x="162" y="465.0" text-anchor="end">split-all-1000</text>
<g class="ci-win"><title>split-all-1000: 0.404× [0.404, 0.404]</title><line x1="368.9" y1="461.0" x2="368.9" y2="461.0" stroke-width="2.2"/><line x1="368.9" y1="457.0" x2="368.9" y2="465.0" stroke-width="1.8"/><line x1="368.9" y1="457.0" x2="368.9" y2="465.0" stroke-width="1.8"/><circle cx="368.9" cy="461.0" r="3.8"/></g>
<text class="val-text" x="1146" y="465.0">0.404× [0.404, 0.404]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
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
