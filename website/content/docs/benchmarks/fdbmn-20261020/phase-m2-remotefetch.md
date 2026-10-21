---
title: "Phase M2: Remote fetch (3-node cluster)"
description: "Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH on master across 9 storage servers, with the 2026-10-14 cohort for comparison."
weight: 2
---

`USE_REMOTE_FETCH` (`getMappedRange`) against `SCAN_AND_FETCH` on master
(`fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`), from one dedicated client host
to three FoundationDB 7.3.77 server hosts (`double` redundancy, `ssd-2`,
9 storage servers). Both methods run in one master binary per benchmark. This
is **phase M2 (`M2rl` and `M2skip`)** of [fdbmn-20261020](../).

## Cases and shard placement

- **`M2rl` (`BenchmarkIndexFetch`):** small (unsplit) and split (100,000-byte
  padding, two chunks) records × `limit1`, `rows10`, `rows1000` ×
  forward/reverse: 12 cases.
- **`M2skip` (`BenchmarkPlanSkip`):** Skip 3 × `rows10`, `rows1000` ×
  forward/reverse: 4 cases.
- Fixtures: 1,000 Orders records, `VALUE` index `price_idx`, no record
  versions. Store prefixes are 19 bytes (`M2rl`) and 22 bytes (`M2skip`).
- **Placement:** `f` is the fraction of fetched records whose storage-server
  team is disjoint from the index shard's team. `f = 1` means disjoint storage
  processes, not disjoint machines: with two replicas on three machines, any two
  teams share a machine.
  - Placement attempt 3 was selected by the preregistered rule (the first
    attempt where every cell passes, otherwise attempt 3).
  - Small and planskip stores: `f = 1` on every cell.
  - Split store: `f = 1` on every cell except `rows1000`, which is partially
    colocated at `f = 0.820`.
- Data distribution stayed off for all of M2. Placement receipts taken before
  and after each eligible case matched.

## Exclusions

Four of 16 cases are excluded whole, each by the pre-arm client CPU gate
(15% on any single benchmark CPU, 5% aggregate):

| Phase | Case | Observed |
|---|---|---|
| `M2rl` | split-limit1-reverse | 15.2% max per CPU |
| `M2rl` | split-rows10-forward | 15.8% max per CPU |
| `M2rl` | split-rows1000-forward | 15.2% max per CPU |
| `M2skip` | skip3-rows10-forward | 23.2% max per CPU, 6.78% aggregate |

All twelve other cases have their complete six-block population and a clean R4
telemetry reconciliation. No case was rerun.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure M2.1: Time per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH</strong><span>Below 1.0× means USE_REMOTE_FETCH is faster. 3-node cluster, 9 storage servers, master.</span></figcaption>
<p class="bench-summary">USE_REMOTE_FETCH is slower on every 1,000-row scan (1.152–1.220 cross-server, 2.304 on the partially colocated f=0.820 split case), on split-rows10-reverse (1.326) and on skip3-rows10-reverse (1.169). It is faster on both one-row small fetches (0.976, 0.974). Four cases are excluded by the pre-arm client CPU gate.</p>
<svg class="bench-svg" viewBox="0 0 1430 438" role="img" aria-label="Figure M2.1: Time per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH">
<line class="grid-line" x1="275.0" y1="36" x2="275.0" y2="412"/>
<text class="axis-text" x="275.0" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="275.0" y="427" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="544.0" y1="36" x2="544.0" y2="412"/>
<text class="axis-text" x="544.0" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="544.0" y="427" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="797.7" y1="36" x2="797.7" y2="412"/>
<text class="axis-text" x="797.7" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="797.7" y="427" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1066.7" y1="36" x2="1066.7" y2="412"/>
<text class="axis-text" x="1066.7" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1066.7" y="427" text-anchor="middle">2.0×</text>
<line class="grid-line" x1="1235.0" y1="36" x2="1235.0" y2="412"/>
<text class="axis-text" x="1235.0" y="30" text-anchor="middle">2.5×</text>
<text class="axis-text" x="1235.0" y="427" text-anchor="middle">2.5×</text>
<text class="axis-text" x="536.0" y="17" text-anchor="end">← USE_REMOTE_FETCH faster</text>
<text class="axis-text" x="552.0" y="17" text-anchor="start">SCAN_AND_FETCH faster →</text>
<line class="section-divider" x1="8" y1="48" x2="1422" y2="48"/>
<text class="section-title" x="10" y="63">M2rl: BenchmarkIndexFetch</text>
<text class="row-label" x="263" y="81.0" text-anchor="end">small-limit1-forward [f=1.000]</text>
<g class="ci-win"><title>small-limit1-forward [f=1.000]: 0.976× [0.967, 0.986]</title><line x1="518.7" y1="77.0" x2="533.4" y2="77.0" stroke-width="2.2"/><line x1="518.7" y1="73.0" x2="518.7" y2="81.0" stroke-width="1.8"/><line x1="533.4" y1="73.0" x2="533.4" y2="81.0" stroke-width="1.8"/><circle cx="525.7" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1247" y="81.0">0.976× [0.967, 0.986]</text>
<text class="row-label" x="263" y="101.0" text-anchor="end">small-limit1-reverse [f=1.000]</text>
<g class="ci-win"><title>small-limit1-reverse [f=1.000]: 0.974× [0.970, 0.978]</title><line x1="521.0" y1="97.0" x2="527.2" y2="97.0" stroke-width="2.2"/><line x1="521.0" y1="93.0" x2="521.0" y2="101.0" stroke-width="1.8"/><line x1="527.2" y1="93.0" x2="527.2" y2="101.0" stroke-width="1.8"/><circle cx="524.1" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1247" y="101.0">0.974× [0.970, 0.978]</text>
<text class="row-label" x="263" y="121.0" text-anchor="end">small-rows10-forward [f=1.000]</text>
<g class="ci-reg"><title>small-rows10-forward [f=1.000]: 1.018× [1.007, 1.029]</title><line x1="549.2" y1="117.0" x2="565.5" y2="117.0" stroke-width="2.2"/><line x1="549.2" y1="113.0" x2="549.2" y2="121.0" stroke-width="1.8"/><line x1="565.5" y1="113.0" x2="565.5" y2="121.0" stroke-width="1.8"/><circle cx="557.4" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1247" y="121.0">1.018× [1.007, 1.029]</text>
<text class="row-label" x="263" y="141.0" text-anchor="end">small-rows10-reverse [f=1.000]</text>
<g class="ci-neutral"><title>small-rows10-reverse [f=1.000]: 0.995× [0.984, 1.007]</title><line x1="531.8" y1="137.0" x2="549.2" y2="137.0" stroke-width="2.2"/><line x1="531.8" y1="133.0" x2="531.8" y2="141.0" stroke-width="1.8"/><line x1="549.2" y1="133.0" x2="549.2" y2="141.0" stroke-width="1.8"/><circle cx="540.2" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1247" y="141.0">0.995× [0.984, 1.007]</text>
<text class="row-label" x="263" y="161.0" text-anchor="end">small-rows1000-forward [f=1.000]</text>
<g class="ci-reg"><title>small-rows1000-forward [f=1.000]: 1.213× [1.171, 1.260]</title><line x1="663.0" y1="157.0" x2="718.3" y2="157.0" stroke-width="2.2"/><line x1="663.0" y1="153.0" x2="663.0" y2="161.0" stroke-width="1.8"/><line x1="718.3" y1="153.0" x2="718.3" y2="161.0" stroke-width="1.8"/><circle cx="689.6" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1247" y="161.0">1.213× [1.171, 1.260]</text>
<text class="row-label" x="263" y="181.0" text-anchor="end">small-rows1000-reverse [f=1.000]</text>
<g class="ci-reg"><title>small-rows1000-reverse [f=1.000]: 1.213× [1.184, 1.242]</title><line x1="671.4" y1="177.0" x2="707.4" y2="177.0" stroke-width="2.2"/><line x1="671.4" y1="173.0" x2="671.4" y2="181.0" stroke-width="1.8"/><line x1="707.4" y1="173.0" x2="707.4" y2="181.0" stroke-width="1.8"/><circle cx="689.6" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1247" y="181.0">1.213× [1.184, 1.242]</text>
<text class="row-label" x="263" y="201.0" text-anchor="end">split-limit1-forward [f=1.000]</text>
<g class="ci-neutral"><title>split-limit1-forward [f=1.000]: 1.009× [0.992, 1.030]</title><line x1="537.9" y1="197.0" x2="566.3" y2="197.0" stroke-width="2.2"/><line x1="537.9" y1="193.0" x2="537.9" y2="201.0" stroke-width="1.8"/><line x1="566.3" y1="193.0" x2="566.3" y2="201.0" stroke-width="1.8"/><circle cx="550.7" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1247" y="201.0">1.009× [0.992, 1.030]</text>
<text class="row-label" x="263" y="221.0" text-anchor="end">split-limit1-reverse [f=1.000]</text>
<text class="axis-text" x="285" y="221.0">N/A (excluded)</text>
<text class="row-label" x="263" y="241.0" text-anchor="end">split-rows10-forward [f=1.000]</text>
<text class="axis-text" x="285" y="241.0">N/A (excluded)</text>
<text class="row-label" x="263" y="261.0" text-anchor="end">split-rows10-reverse [f=1.000]</text>
<g class="ci-reg"><title>split-rows10-reverse [f=1.000]: 1.326× [1.292, 1.365]</title><line x1="737.2" y1="257.0" x2="778.6" y2="257.0" stroke-width="2.2"/><line x1="737.2" y1="253.0" x2="737.2" y2="261.0" stroke-width="1.8"/><line x1="778.6" y1="253.0" x2="778.6" y2="261.0" stroke-width="1.8"/><circle cx="756.8" cy="257.0" r="3.8"/></g>
<text class="val-text" x="1247" y="261.0">1.326× [1.292, 1.365]</text>
<text class="row-label" x="263" y="281.0" text-anchor="end">split-rows1000-forward [f=0.820]</text>
<text class="axis-text" x="285" y="281.0">N/A (excluded)</text>
<text class="row-label" x="263" y="301.0" text-anchor="end">split-rows1000-reverse [f=0.820]</text>
<g class="ci-reg"><title>split-rows1000-reverse [f=0.820]: 2.304× [2.247, 2.360]</title><line x1="1154.5" y1="297.0" x2="1191.5" y2="297.0" stroke-width="2.2"/><line x1="1154.5" y1="293.0" x2="1154.5" y2="301.0" stroke-width="1.8"/><line x1="1191.5" y1="293.0" x2="1191.5" y2="301.0" stroke-width="1.8"/><circle cx="1173.4" cy="297.0" r="3.8"/></g>
<text class="val-text" x="1247" y="301.0">2.304× [2.247, 2.360]</text>
<line class="section-divider" x1="8" y1="310" x2="1422" y2="310"/>
<text class="section-title" x="10" y="325">M2skip: BenchmarkPlanSkip</text>
<text class="row-label" x="263" y="343.0" text-anchor="end">skip3-rows10-forward [f=1.000]</text>
<text class="axis-text" x="285" y="343.0">N/A (excluded)</text>
<text class="row-label" x="263" y="363.0" text-anchor="end">skip3-rows10-reverse [f=1.000]</text>
<g class="ci-reg"><title>skip3-rows10-reverse [f=1.000]: 1.169× [1.151, 1.185]</title><line x1="650.0" y1="359.0" x2="672.0" y2="359.0" stroke-width="2.2"/><line x1="650.0" y1="355.0" x2="650.0" y2="363.0" stroke-width="1.8"/><line x1="672.0" y1="355.0" x2="672.0" y2="363.0" stroke-width="1.8"/><circle cx="661.7" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1247" y="363.0">1.169× [1.151, 1.185]</text>
<text class="row-label" x="263" y="383.0" text-anchor="end">skip3-rows1000-forward [f=1.000]</text>
<g class="ci-reg"><title>skip3-rows1000-forward [f=1.000]: 1.220× [1.198, 1.243]</title><line x1="680.2" y1="379.0" x2="708.0" y2="379.0" stroke-width="2.2"/><line x1="680.2" y1="375.0" x2="680.2" y2="383.0" stroke-width="1.8"/><line x1="708.0" y1="375.0" x2="708.0" y2="383.0" stroke-width="1.8"/><circle cx="693.9" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1247" y="383.0">1.220× [1.198, 1.243]</text>
<text class="row-label" x="263" y="403.0" text-anchor="end">skip3-rows1000-reverse [f=1.000]</text>
<g class="ci-reg"><title>skip3-rows1000-reverse [f=1.000]: 1.152× [1.110, 1.195]</title><line x1="622.7" y1="399.0" x2="678.3" y2="399.0" stroke-width="2.2"/><line x1="622.7" y1="395.0" x2="622.7" y2="403.0" stroke-width="1.8"/><line x1="678.3" y1="395.0" x2="678.3" y2="403.0" stroke-width="1.8"/><circle cx="650.7" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1247" y="403.0">1.152× [1.110, 1.195]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M2.2: Client allocations per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH</strong><span>Below 1.0× means USE_REMOTE_FETCH allocates fewer Go objects.</span></figcaption>
<p class="bench-summary">USE_REMOTE_FETCH allocates fewer Go objects in 10 of 12 eligible cases, down to 0.454. The one-row small fetches allocate more (1.080, 1.090).</p>
<svg class="bench-svg" viewBox="0 0 1352 438" role="img" aria-label="Figure M2.2: Client allocations per operation, USE_REMOTE_FETCH / SCAN_AND_FETCH">
<line class="grid-line" x1="197.0" y1="36" x2="197.0" y2="412"/>
<text class="axis-text" x="197.0" y="30" text-anchor="middle">0.35×</text>
<text class="axis-text" x="197.0" y="427" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="474.9" y1="36" x2="474.9" y2="412"/>
<text class="axis-text" x="474.9" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="474.9" y="427" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="737.1" y1="36" x2="737.1" y2="412"/>
<text class="axis-text" x="737.1" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="737.1" y="427" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="1014.9" y1="36" x2="1014.9" y2="412"/>
<text class="axis-text" x="1014.9" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="1014.9" y="427" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1157.0" y1="36" x2="1157.0" y2="412"/>
<text class="axis-text" x="1157.0" y="30" text-anchor="middle">1.2×</text>
<text class="axis-text" x="1157.0" y="427" text-anchor="middle">1.2×</text>
<text class="axis-text" x="1006.9" y="17" text-anchor="end">← USE_REMOTE_FETCH fewer allocs</text>
<text class="axis-text" x="1022.9" y="17" text-anchor="start">SCAN_AND_FETCH fewer allocs →</text>
<line class="section-divider" x1="8" y1="48" x2="1344" y2="48"/>
<text class="section-title" x="10" y="63">M2rl: client Go allocs/op</text>
<text class="row-label" x="185" y="81.0" text-anchor="end">small-limit1-forward</text>
<g class="ci-reg"><title>small-limit1-forward: 1.080× [1.080, 1.080]</title><line x1="1074.9" y1="77.0" x2="1074.9" y2="77.0" stroke-width="2.2"/><line x1="1074.9" y1="73.0" x2="1074.9" y2="81.0" stroke-width="1.8"/><line x1="1074.9" y1="73.0" x2="1074.9" y2="81.0" stroke-width="1.8"/><circle cx="1074.9" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1169" y="81.0">1.080× [1.080, 1.080]</text>
<text class="row-label" x="185" y="101.0" text-anchor="end">small-limit1-reverse</text>
<g class="ci-reg"><title>small-limit1-reverse: 1.090× [1.090, 1.090]</title><line x1="1082.1" y1="97.0" x2="1082.1" y2="97.0" stroke-width="2.2"/><line x1="1082.1" y1="93.0" x2="1082.1" y2="101.0" stroke-width="1.8"/><line x1="1082.1" y1="93.0" x2="1082.1" y2="101.0" stroke-width="1.8"/><circle cx="1082.1" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1169" y="101.0">1.090× [1.090, 1.090]</text>
<text class="row-label" x="185" y="121.0" text-anchor="end">small-rows10-forward</text>
<g class="ci-win"><title>small-rows10-forward: 0.637× [0.636, 0.637]</title><line x1="662.3" y1="117.0" x2="663.6" y2="117.0" stroke-width="2.2"/><line x1="662.3" y1="113.0" x2="662.3" y2="121.0" stroke-width="1.8"/><line x1="663.6" y1="113.0" x2="663.6" y2="121.0" stroke-width="1.8"/><circle cx="663.6" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1169" y="121.0">0.637× [0.636, 0.637]</text>
<text class="row-label" x="185" y="141.0" text-anchor="end">small-rows10-reverse</text>
<g class="ci-win"><title>small-rows10-reverse: 0.636× [0.636, 0.637]</title><line x1="662.3" y1="137.0" x2="663.6" y2="137.0" stroke-width="2.2"/><line x1="662.3" y1="133.0" x2="662.3" y2="141.0" stroke-width="1.8"/><line x1="663.6" y1="133.0" x2="663.6" y2="141.0" stroke-width="1.8"/><circle cx="662.3" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1169" y="141.0">0.636× [0.636, 0.637]</text>
<text class="row-label" x="185" y="161.0" text-anchor="end">small-rows1000-forward</text>
<g class="ci-win"><title>small-rows1000-forward: 0.481× [0.481, 0.481]</title><line x1="444.7" y1="157.0" x2="444.7" y2="157.0" stroke-width="2.2"/><line x1="444.7" y1="153.0" x2="444.7" y2="161.0" stroke-width="1.8"/><line x1="444.7" y1="153.0" x2="444.7" y2="161.0" stroke-width="1.8"/><circle cx="444.7" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1169" y="161.0">0.481× [0.481, 0.481]</text>
<text class="row-label" x="185" y="181.0" text-anchor="end">small-rows1000-reverse</text>
<g class="ci-win"><title>small-rows1000-reverse: 0.481× [0.481, 0.481]</title><line x1="444.7" y1="177.0" x2="444.7" y2="177.0" stroke-width="2.2"/><line x1="444.7" y1="173.0" x2="444.7" y2="181.0" stroke-width="1.8"/><line x1="444.7" y1="173.0" x2="444.7" y2="181.0" stroke-width="1.8"/><circle cx="444.7" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1169" y="181.0">0.481× [0.481, 0.481]</text>
<text class="row-label" x="185" y="201.0" text-anchor="end">split-limit1-forward</text>
<g class="ci-win"><title>split-limit1-forward: 0.986× [0.983, 0.988]</title><line x1="1001.6" y1="197.0" x2="1005.5" y2="197.0" stroke-width="2.2"/><line x1="1001.6" y1="193.0" x2="1001.6" y2="201.0" stroke-width="1.8"/><line x1="1005.5" y1="193.0" x2="1005.5" y2="201.0" stroke-width="1.8"/><circle cx="1004.0" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1169" y="201.0">0.986× [0.983, 0.988]</text>
<text class="row-label" x="185" y="221.0" text-anchor="end">split-limit1-reverse</text>
<text class="axis-text" x="207" y="221.0">N/A (excluded)</text>
<text class="row-label" x="185" y="241.0" text-anchor="end">split-rows10-forward</text>
<text class="axis-text" x="207" y="241.0">N/A (excluded)</text>
<text class="row-label" x="185" y="261.0" text-anchor="end">split-rows10-reverse</text>
<g class="ci-win"><title>split-rows10-reverse: 0.547× [0.547, 0.548]</title><line x1="544.9" y1="257.0" x2="546.3" y2="257.0" stroke-width="2.2"/><line x1="544.9" y1="253.0" x2="544.9" y2="261.0" stroke-width="1.8"/><line x1="546.3" y1="253.0" x2="546.3" y2="261.0" stroke-width="1.8"/><circle cx="544.9" cy="257.0" r="3.8"/></g>
<text class="val-text" x="1169" y="261.0">0.547× [0.547, 0.548]</text>
<text class="row-label" x="185" y="281.0" text-anchor="end">split-rows1000-forward</text>
<text class="axis-text" x="207" y="281.0">N/A (excluded)</text>
<text class="row-label" x="185" y="301.0" text-anchor="end">split-rows1000-reverse</text>
<g class="ci-win"><title>split-rows1000-reverse: 0.454× [0.454, 0.454]</title><line x1="399.7" y1="297.0" x2="399.7" y2="297.0" stroke-width="2.2"/><line x1="399.7" y1="293.0" x2="399.7" y2="301.0" stroke-width="1.8"/><line x1="399.7" y1="293.0" x2="399.7" y2="301.0" stroke-width="1.8"/><circle cx="399.7" cy="297.0" r="3.8"/></g>
<text class="val-text" x="1169" y="301.0">0.454× [0.454, 0.454]</text>
<line class="section-divider" x1="8" y1="310" x2="1344" y2="310"/>
<text class="section-title" x="10" y="325">M2skip: client Go allocs/op</text>
<text class="row-label" x="185" y="343.0" text-anchor="end">skip3-rows10-forward</text>
<text class="axis-text" x="207" y="343.0">N/A (excluded)</text>
<text class="row-label" x="185" y="363.0" text-anchor="end">skip3-rows10-reverse</text>
<g class="ci-win"><title>skip3-rows10-reverse: 0.841× [0.841, 0.842]</title><line x1="880.0" y1="359.0" x2="881.0" y2="359.0" stroke-width="2.2"/><line x1="880.0" y1="355.0" x2="880.0" y2="363.0" stroke-width="1.8"/><line x1="881.0" y1="355.0" x2="881.0" y2="363.0" stroke-width="1.8"/><circle cx="880.0" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1169" y="363.0">0.841× [0.841, 0.842]</text>
<text class="row-label" x="185" y="383.0" text-anchor="end">skip3-rows1000-forward</text>
<g class="ci-win"><title>skip3-rows1000-forward: 0.583× [0.583, 0.584]</title><line x1="594.6" y1="379.0" x2="595.9" y2="379.0" stroke-width="2.2"/><line x1="594.6" y1="375.0" x2="594.6" y2="383.0" stroke-width="1.8"/><line x1="595.9" y1="375.0" x2="595.9" y2="383.0" stroke-width="1.8"/><circle cx="594.6" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1169" y="383.0">0.583× [0.583, 0.584]</text>
<text class="row-label" x="185" y="403.0" text-anchor="end">skip3-rows1000-reverse</text>
<g class="ci-win"><title>skip3-rows1000-reverse: 0.583× [0.583, 0.583]</title><line x1="594.6" y1="399.0" x2="594.6" y2="399.0" stroke-width="2.2"/><line x1="594.6" y1="395.0" x2="594.6" y2="403.0" stroke-width="1.8"/><line x1="594.6" y1="395.0" x2="594.6" y2="403.0" stroke-width="1.8"/><circle cx="594.6" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1169" y="403.0">0.583× [0.583, 0.583]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI below 1.0×</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI includes 1.0× (inconclusive)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI above 1.0×</span></div>
</figure>

Geometric mean of six paired block ratios (median of three `testing.B` repeats
per process, 3 s benchtime), with nominal 95% block-bootstrap intervals
(20,000 resamples, preregistered seed 492817). Below 1 means
`USE_REMOTE_FETCH` is faster or allocates less. The metric is mean `ns/op`, not
a latency percentile.

### `M2rl`: `BenchmarkIndexFetch` (`USE_REMOTE_FETCH / SCAN_AND_FETCH`)

| Case | Placement | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| small-limit1-forward | cross-server (f = 1.000) | 0.976 [0.967, 0.986] | 0.977 [0.976, 0.978] | 1.080 [1.080, 1.080] |
| small-limit1-reverse | cross-server (f = 1.000) | 0.974 [0.970, 0.978] | 0.976 [0.975, 0.977] | 1.090 [1.090, 1.090] |
| small-rows10-forward | cross-server (f = 1.000) | 1.018 [1.007, 1.029] | 0.788 [0.787, 0.788] | 0.637 [0.636, 0.637] |
| small-rows10-reverse | cross-server (f = 1.000) | 0.995 [0.984, 1.007] | 0.783 [0.782, 0.784] | 0.636 [0.636, 0.637] |
| small-rows1000-forward | cross-server (f = 1.000) | 1.213 [1.171, 1.260] | 0.565 [0.564, 0.566] | 0.481 [0.481, 0.481] |
| small-rows1000-reverse | cross-server (f = 1.000) | 1.213 [1.184, 1.242] | 0.559 [0.559, 0.560] | 0.481 [0.481, 0.481] |
| split-limit1-forward | cross-server (f = 1.000) | 1.009 [0.992, 1.030] | 0.716 [0.715, 0.716] | 0.986 [0.983, 0.988] |
| split-limit1-reverse | cross-server (f = 1.000) | N/A (pre-arm client CPU gate) | — | — |
| split-rows10-forward | cross-server (f = 1.000) | N/A (pre-arm client CPU gate) | — | — |
| split-rows10-reverse | cross-server (f = 1.000) | 1.326 [1.292, 1.365] | 0.698 [0.698, 0.699] | 0.547 [0.547, 0.548] |
| split-rows1000-forward | partially colocated (f = 0.820) | N/A (pre-arm client CPU gate) | — | — |
| split-rows1000-reverse | partially colocated (f = 0.820) | 2.304 [2.247, 2.360] | 0.693 [0.693, 0.693] | 0.454 [0.454, 0.454] |

### `M2skip`: `BenchmarkPlanSkip` (`USE_REMOTE_FETCH / SCAN_AND_FETCH`)

| Case | Placement | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| skip3-rows10-forward | cross-server (f = 1.000) | N/A (pre-arm client CPU gate) | — | — |
| skip3-rows10-reverse | cross-server (f = 1.000) | 1.169 [1.151, 1.185] | 0.899 [0.899, 0.899] | 0.841 [0.841, 0.842] |
| skip3-rows1000-forward | cross-server (f = 1.000) | 1.220 [1.198, 1.243] | 0.634 [0.634, 0.635] | 0.583 [0.583, 0.584] |
| skip3-rows1000-reverse | cross-server (f = 1.000) | 1.152 [1.110, 1.195] | 0.629 [0.628, 0.630] | 0.583 [0.583, 0.583] |

All `rows/op` and `prefix-B` ratios in both phases are 1.000 [1.000, 1.000];
they are audit quantities, not results.

- Every 1,000-row scan is slower with `USE_REMOTE_FETCH`: small rows1000 by
  about 21% in both directions, Skip rows1000 by 15–22%, and the partially
  colocated split rows1000 reverse at 2.304.
- On small rows1000, `USE_REMOTE_FETCH` allocates about 44% fewer Go bytes and
  52% fewer Go objects per operation.
- One-row small fetches are 2–3% faster with `USE_REMOTE_FETCH` (0.976 and
  0.974).

### Absolute values

Medians of each process's three `testing.B` means, per arm.

| Phase | Case | Ordinary ms | Remote ms | Ordinary B/op | Remote B/op | Ordinary allocs/op | Remote allocs/op |
|---|---|---:|---:|---:|---:|---:|---:|
| M2rl | small-limit1-forward | 3.165 | 3.087 | 15,980 | 15,618 | 187 | 202 |
| M2rl | small-limit1-reverse | 3.196 | 3.106 | 16,054 | 15,655 | 188 | 205 |
| M2rl | small-rows10-forward | 3.654 | 3.718 | 87,286 | 68,748 | 606 | 386 |
| M2rl | small-rows10-reverse | 3.737 | 3.702 | 87,758 | 68,706 | 607 | 386 |
| M2rl | small-rows1000-forward | 102.749 | 123.646 | 4,207,936 | 2,377,137 | 46,972 | 22,590 |
| M2rl | small-rows1000-reverse | 101.429 | 122.551 | 4,257,716 | 2,380,672 | 46,970 | 22,582 |
| M2rl | split-limit1-forward | 4.428 | 4.448 | 472,976 | 338,430 | 221 | 218 |
| M2rl | split-rows10-reverse | 7.983 | 10.625 | 4,645,208 | 3,244,086 | 912 | 500 |
| M2rl | split-rows1000-reverse | 359.495 | 831.216 | 456,772,766 | 316,346,125 | 67,952 | 30,850 |
| M2skip | skip3-rows10-reverse | 4.498 | 5.259 | 140,950 | 126,702 | 1,054 | 887 |
| M2skip | skip3-rows1000-forward | 103.323 | 126.712 | 5,033,941 | 3,192,446 | 60,608 | 35,362 |
| M2skip | skip3-rows1000-reverse | 109.655 | 126.707 | 5,082,649 | 3,196,061 | 60,608 | 35,353 |

## Comparison with the 2026-10-14 campaign

The 2026-10-14 M2 ran both methods on old Go (`267a41434`); this campaign runs
both on master. No old-versus-master M2 arm was measured in either campaign,
so the table compares two cohorts and is descriptive only: no blocks are
pooled and no cross-campaign interval is computed.

| Case | f, 2026-10-14 → 2026-10-20 | Remote/ordinary ns/op, 2026-10-14 | Remote/ordinary ns/op, 2026-10-20 |
|---|---|---:|---:|
| M2rl/small-limit1-forward | 1.000 → 1.000 | 0.979 [0.964, 0.995] | 0.976 [0.967, 0.986] |
| M2rl/small-limit1-reverse | 1.000 → 1.000 | 0.985 [0.969, 1.003] | 0.974 [0.970, 0.978] |
| M2rl/small-rows10-forward | 1.000 → 1.000 | 1.031 [1.018, 1.042] | 1.018 [1.007, 1.029] |
| M2rl/small-rows10-reverse | 1.000 → 1.000 | 1.026 [1.013, 1.039] | 0.995 [0.984, 1.007] |
| M2rl/small-rows1000-forward | 1.000 → 1.000 | 1.226 [1.175, 1.280] | 1.213 [1.171, 1.260] |
| M2rl/small-rows1000-reverse | 1.000 → 1.000 | 1.255 [1.217, 1.293] | 1.213 [1.184, 1.242] |
| M2rl/split-limit1-forward | 1.000 → 1.000 | 1.110 [1.087, 1.131] | 1.009 [0.992, 1.030] |
| M2rl/split-rows10-reverse | 0.000 → 1.000 | 0.854 [0.823, 0.886] | 1.326 [1.292, 1.365] |
| M2rl/split-rows1000-reverse | 0.696 → 0.820 | 2.300 [2.206, 2.384] | 2.304 [2.247, 2.360] |
| M2skip/skip3-rows10-reverse | 1.000 → 1.000 | 0.158 [0.155, 0.160] | 1.169 [1.151, 1.185] |
| M2skip/skip3-rows1000-forward | 1.000 → 1.000 | 1.360 [1.295, 1.414] | 1.220 [1.198, 1.243] |
| M2skip/skip3-rows1000-reverse | 1.000 → 1.000 | 1.324 [1.250, 1.407] | 1.152 [1.110, 1.195] |

- **Placement changed for two cases.** `split-rows10-reverse` moved from
  `f = 0` to `f = 1` and `split-rows1000-reverse` from `f = 0.696` to
  `f = 0.820` (seven record shards then, six now). Their ratios are not a
  comparison of code.
- **Reverse Skip 10 rows** is labelled cross-server in both cohorts, but the
  physical teams differ: index and record teams both occupied the node1/node2
  pair on 2026-10-14, and span node1/node3 and node2/node3 now. Absolute
  ordinary/remote times were 31.127 ms / 4.873 ms then and 4.498 ms / 5.259 ms
  now.
- **Matched properties:** the same frozen fixture schema, key schema and store
  prefixes; the same 12-case and 4-case geometry, method orders, repeats and
  benchtime; `GOMAXPROCS=8` on CPUs 8–15. Key counts match (2,000 small,
  3,000 split and 2,000 planskip keys); serialized value bytes were not
  compared.
- **Different:** fresh hosts, freshly seeded data, different shard and replica
  layouts in all three stores, and the production revision (old Go then, master
  now).
