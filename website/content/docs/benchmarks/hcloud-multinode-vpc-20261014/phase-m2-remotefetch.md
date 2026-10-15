---
title: "Phase M2: Remote Fetch (3-Node VPC)"
description: "Record Layer USE_REMOTE_FETCH vs SCAN_AND_FETCH across 9 storage servers on a 3-node VPC cluster with verified shard placement."
weight: 2
---

`USE_REMOTE_FETCH` (`getMappedRange`) against `SCAN_AND_FETCH` on the final
tree (`267a41434a81723b07e7150869d702636a1b91a9`) across a **4-host VPC
topology**: one dedicated client VM and three dedicated
FoundationDB 7.3.77 server VMs (`double` redundancy, `ssd-2`
storage engine, 9 storage servers). This is **phase M2 (`M2rl` and `M2skip`)**
of [hcloud-multinode-vpc-20261014](../).

## Shard Placement and Qualification (Gate P0)

- Before any M2 timing, `placementprobe_cgo` (H5, reading `\xff/dataDistributionMode`,
  `\xff/moveKeysLock/Owner`, `\xff/moveKeysLock/Write`, `\xff/keyServers/`, and
  transactional storage addresses via `libfdb_c` at snapshot read version V1 and
  rechecking stability at V2) and the ballast tool (H6) were qualified on the
  live cluster:
  - Probing with data distribution on was refused.
  - The unmodified scratch control store passed `integrity_ok = true` and all
    30 Python provenance/key mutation controls (`ok = true`).
  - All three physical on-cluster defects (`delete-chunk`, `stray-suffix`,
    `version-key`) failed `integrity_ok` with the expected error messages.
- Three placement attempts (`256 MiB`, `512 MiB`, `1024 MiB` ballast) were run
  following the preregistered rule (`protocol.json` `placement.mechanism`):
  settle for 120 s with `moving_data` in-flight and queued bytes at 0, freeze
  data distribution (`datadistribution off`), clear only the ballast range and
  verify it empty, and take the canonical CGo placement probe and receipt.
- Per protocol, attempt 3 was sealed before timing (`plans/testingb/manifest.json`
  SHA `4ada3920f92da4489fe4037ad5dd3440cec59957fc4ccdcba81b792b7a45f54a`):
  - **`benchfetch/small`:** `integrity_ok = true`, `passed = true`, **100%
    `cross-server`** (server-disjoint fraction `1.0000`) on every cell.
  - **`benchfetch/planskip`:** `integrity_ok = true`, `passed = true`, **100%
    `cross-server`** (server-disjoint fraction `1.0000`) on every cell.
  - **`benchfetch/split`:** `integrity_ok = true`, 1 index shard and 7 record
    shards, `shared_shards = 0`: `limit1-forward` and `limit1-reverse` are
    **`cross-server`** (`1/1`), `rows10` is **`partially colocated (f =
    0.0000)`** (`0/10`), and `rows1000` is **`partially colocated (f =
    0.6960)`** (`696/1000` rows on storage-server teams disjoint from the index
    shard's team).
  - With `double` replication across three machines, any two 2-replica storage
    teams share at least one physical machine by pigeonhole; `cross-server`
    therefore means **storage-process (address) disjoint**, not machine
    disjoint.
- Before the first process and after the last process of every M2 case, `R2`
  (`bench_runner_mn.py`) took a fresh CGo placement receipt and verified via
  `placement_receipt.compare` that shard boundaries, storage teams, and
  disjointness fractions remained identical (`equal = true`).
- R4 telemetry reconciliation (`reconcile_telemetry.py`) across `node1..node3`
  and `client` reported **12/12 clean cases** in `M2rl` (144 processes, 432
  samples) and **4/4 clean cases** in `M2skip` (48 processes, 144 samples), with
  0 contaminated and 0 unverifiable cases.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure M2.1: Mean Time (ns/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH (log₂ scale, 95% CI)</strong> — 3-node VPC double ssd-2 cluster (9 storage servers). Below 1.0× means USE_REMOTE_FETCH is faster.</figcaption>
<p class="bench-summary"><strong>Summary:</strong> USE_REMOTE_FETCH (getMappedRange) is slower than SCAN_AND_FETCH on 9 of 12 M2rl cases and 2 of 4 M2skip cases on the 3-node ssd-2 cluster. It wins big on skip3-rows10-reverse (0.158×, 6.3× faster: 1.70 ms vs 10.74 ms) where ordinary scan pays sequential client key-selector round-trips, and wins modestly on split-rows10 (0.854×–0.873×) and small-limit1-forward (0.979×). It loses on every 1,000-row scan: 1.226×–1.255× on small-rows1000, 1.324×–1.360× on skip3-rows1000, and 2.300×–2.326× slower (14.1 ms vs 6.1 ms) on split-rows1000 [f = 0.6960], where the index storage server fans out secondary GetKeyValues RPCs across 7 record shards for 3-chunk split records.</p>
<svg class="bench-svg" viewBox="0 0 1454 438" role="img" aria-label="Figure M2.1: Mean Time (ns/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH (log₂ scale, 95% CI)">
<line class="grid-line" x1="299.0" y1="36" x2="299.0" y2="412"/>
<text class="axis-text" x="299.0" y="30" text-anchor="middle">0.15×</text>
<text class="axis-text" x="299.0" y="427" text-anchor="middle">0.15×</text>
<line class="grid-line" x1="473.3" y1="36" x2="473.3" y2="412"/>
<text class="axis-text" x="473.3" y="30" text-anchor="middle">0.25×</text>
<text class="axis-text" x="473.3" y="427" text-anchor="middle">0.25×</text>
<line class="grid-line" x1="709.8" y1="36" x2="709.8" y2="412"/>
<text class="axis-text" x="709.8" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="709.8" y="427" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="824.6" y1="36" x2="824.6" y2="412"/>
<text class="axis-text" x="824.6" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="824.6" y="427" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="946.3" y1="36" x2="946.3" y2="412"/>
<text class="axis-text" x="946.3" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="946.3" y="427" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1061.2" y1="36" x2="1061.2" y2="412"/>
<text class="axis-text" x="1061.2" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="1061.2" y="427" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="1182.9" y1="36" x2="1182.9" y2="412"/>
<text class="axis-text" x="1182.9" y="30" text-anchor="middle">2.0×</text>
<text class="axis-text" x="1182.9" y="427" text-anchor="middle">2.0×</text>
<line class="grid-line" x1="1259.0" y1="36" x2="1259.0" y2="412"/>
<text class="axis-text" x="1259.0" y="30" text-anchor="middle">2.5×</text>
<text class="axis-text" x="1259.0" y="427" text-anchor="middle">2.5×</text>
<text class="axis-text" x="938.3" y="17" text-anchor="end">← Remote fetch faster</text>
<text class="axis-text" x="954.3" y="17" text-anchor="start">SCAN_AND_FETCH faster →</text>
<line class="section-divider" x1="8" y1="48" x2="1446" y2="48"/>
<text class="section-title" x="10" y="63">M2rl: BenchmarkIndexFetch (3-node double ssd-2)</text>
<text class="row-label" x="287" y="81.0" text-anchor="end">small-limit1-forward [cross-srv]</text>
<g class="ci-win"><title>small-limit1-forward [cross-srv]: 0.979× [0.964, 0.995]</title><line x1="933.8" y1="77.0" x2="944.6" y2="77.0" stroke-width="2.2"/><line x1="933.8" y1="73.0" x2="933.8" y2="81.0" stroke-width="1.8"/><line x1="944.6" y1="73.0" x2="944.6" y2="81.0" stroke-width="1.8"/><circle cx="939.1" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1271" y="81.0">0.979× [0.964, 0.995]</text>
<text class="row-label" x="287" y="101.0" text-anchor="end">small-limit1-reverse [cross-srv]</text>
<g class="ci-neutral"><title>small-limit1-reverse [cross-srv]: 0.985× [0.969, 1.003]</title><line x1="935.6" y1="97.0" x2="947.4" y2="97.0" stroke-width="2.2"/><line x1="935.6" y1="93.0" x2="935.6" y2="101.0" stroke-width="1.8"/><line x1="947.4" y1="93.0" x2="947.4" y2="101.0" stroke-width="1.8"/><circle cx="941.2" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1271" y="101.0">0.985× [0.969, 1.003]</text>
<text class="row-label" x="287" y="121.0" text-anchor="end">small-rows10-forward [cross-srv]</text>
<g class="ci-reg"><title>small-rows10-forward [cross-srv]: 1.031× [1.018, 1.042]</title><line x1="952.4" y1="117.0" x2="960.4" y2="117.0" stroke-width="2.2"/><line x1="952.4" y1="113.0" x2="952.4" y2="121.0" stroke-width="1.8"/><line x1="960.4" y1="113.0" x2="960.4" y2="121.0" stroke-width="1.8"/><circle cx="956.8" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1271" y="121.0">1.031× [1.018, 1.042]</text>
<text class="row-label" x="287" y="141.0" text-anchor="end">small-rows10-reverse [cross-srv]</text>
<g class="ci-reg"><title>small-rows10-reverse [cross-srv]: 1.026× [1.013, 1.039]</title><line x1="950.7" y1="137.0" x2="959.4" y2="137.0" stroke-width="2.2"/><line x1="950.7" y1="133.0" x2="950.7" y2="141.0" stroke-width="1.8"/><line x1="959.4" y1="133.0" x2="959.4" y2="141.0" stroke-width="1.8"/><circle cx="955.1" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1271" y="141.0">1.026× [1.013, 1.039]</text>
<text class="row-label" x="287" y="161.0" text-anchor="end">small-rows1000-forward [cross-srv]</text>
<g class="ci-reg"><title>small-rows1000-forward [cross-srv]: 1.226× [1.175, 1.280]</title><line x1="1001.4" y1="157.0" x2="1030.6" y2="157.0" stroke-width="2.2"/><line x1="1001.4" y1="153.0" x2="1001.4" y2="161.0" stroke-width="1.8"/><line x1="1030.6" y1="153.0" x2="1030.6" y2="161.0" stroke-width="1.8"/><circle cx="1015.9" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1271" y="161.0">1.226× [1.175, 1.280]</text>
<text class="row-label" x="287" y="181.0" text-anchor="end">small-rows1000-reverse [cross-srv]</text>
<g class="ci-reg"><title>small-rows1000-reverse [cross-srv]: 1.255× [1.217, 1.293]</title><line x1="1013.4" y1="177.0" x2="1034.0" y2="177.0" stroke-width="2.2"/><line x1="1013.4" y1="173.0" x2="1013.4" y2="181.0" stroke-width="1.8"/><line x1="1034.0" y1="173.0" x2="1034.0" y2="181.0" stroke-width="1.8"/><circle cx="1023.8" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1271" y="181.0">1.255× [1.217, 1.293]</text>
<text class="row-label" x="287" y="201.0" text-anchor="end">split-limit1-forward [cross-srv]</text>
<g class="ci-reg"><title>split-limit1-forward [cross-srv]: 1.110× [1.087, 1.131]</title><line x1="974.8" y1="197.0" x2="988.3" y2="197.0" stroke-width="2.2"/><line x1="974.8" y1="193.0" x2="974.8" y2="201.0" stroke-width="1.8"/><line x1="988.3" y1="193.0" x2="988.3" y2="201.0" stroke-width="1.8"/><circle cx="982.0" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1271" y="201.0">1.110× [1.087, 1.131]</text>
<text class="row-label" x="287" y="221.0" text-anchor="end">split-limit1-reverse [cross-srv]</text>
<g class="ci-reg"><title>split-limit1-reverse [cross-srv]: 1.114× [1.095, 1.138]</title><line x1="977.3" y1="217.0" x2="990.5" y2="217.0" stroke-width="2.2"/><line x1="977.3" y1="213.0" x2="977.3" y2="221.0" stroke-width="1.8"/><line x1="990.5" y1="213.0" x2="990.5" y2="221.0" stroke-width="1.8"/><circle cx="983.2" cy="217.0" r="3.8"/></g>
<text class="val-text" x="1271" y="221.0">1.114× [1.095, 1.138]</text>
<text class="row-label" x="287" y="241.0" text-anchor="end">split-rows10-forward [f = 0.0000]</text>
<g class="ci-win"><title>split-rows10-forward [f = 0.0000]: 0.873× [0.829, 0.920]</title><line x1="882.3" y1="237.0" x2="917.9" y2="237.0" stroke-width="2.2"/><line x1="882.3" y1="233.0" x2="882.3" y2="241.0" stroke-width="1.8"/><line x1="917.9" y1="233.0" x2="917.9" y2="241.0" stroke-width="1.8"/><circle cx="900.0" cy="237.0" r="3.8"/></g>
<text class="val-text" x="1271" y="241.0">0.873× [0.829, 0.920]</text>
<text class="row-label" x="287" y="261.0" text-anchor="end">split-rows10-reverse [f = 0.0000]</text>
<g class="ci-win"><title>split-rows10-reverse [f = 0.0000]: 0.854× [0.823, 0.886]</title><line x1="879.9" y1="257.0" x2="905.0" y2="257.0" stroke-width="2.2"/><line x1="879.9" y1="253.0" x2="879.9" y2="261.0" stroke-width="1.8"/><line x1="905.0" y1="253.0" x2="905.0" y2="261.0" stroke-width="1.8"/><circle cx="892.5" cy="257.0" r="3.8"/></g>
<text class="val-text" x="1271" y="261.0">0.854× [0.823, 0.886]</text>
<text class="row-label" x="287" y="281.0" text-anchor="end">split-rows1000-forward [f = 0.6960]</text>
<g class="ci-reg"><title>split-rows1000-forward [f = 0.6960]: 2.326× [2.254, 2.397]</title><line x1="1223.7" y1="277.0" x2="1244.6" y2="277.0" stroke-width="2.2"/><line x1="1223.7" y1="273.0" x2="1223.7" y2="281.0" stroke-width="1.8"/><line x1="1244.6" y1="273.0" x2="1244.6" y2="281.0" stroke-width="1.8"/><circle cx="1234.4" cy="277.0" r="3.8"/></g>
<text class="val-text" x="1271" y="281.0">2.326× [2.254, 2.397]</text>
<text class="row-label" x="287" y="301.0" text-anchor="end">split-rows1000-reverse [f = 0.6960]</text>
<g class="ci-reg"><title>split-rows1000-reverse [f = 0.6960]: 2.300× [2.206, 2.384]</title><line x1="1216.3" y1="297.0" x2="1242.8" y2="297.0" stroke-width="2.2"/><line x1="1216.3" y1="293.0" x2="1216.3" y2="301.0" stroke-width="1.8"/><line x1="1242.8" y1="293.0" x2="1242.8" y2="301.0" stroke-width="1.8"/><circle cx="1230.5" cy="297.0" r="3.8"/></g>
<text class="val-text" x="1271" y="301.0">2.300× [2.206, 2.384]</text>
<line class="section-divider" x1="8" y1="310" x2="1446" y2="310"/>
<text class="section-title" x="10" y="325">M2skip: BenchmarkPlanSkip (3-node double ssd-2)</text>
<text class="row-label" x="287" y="343.0" text-anchor="end">skip3-rows10-forward [cross-srv]</text>
<g class="ci-neutral"><title>skip3-rows10-forward [cross-srv]: 1.155× [0.991, 1.345]</title><line x1="943.3" y1="339.0" x2="1047.5" y2="339.0" stroke-width="2.2"/><line x1="943.3" y1="335.0" x2="943.3" y2="343.0" stroke-width="1.8"/><line x1="1047.5" y1="335.0" x2="1047.5" y2="343.0" stroke-width="1.8"/><circle cx="995.5" cy="339.0" r="3.8"/></g>
<text class="val-text" x="1271" y="343.0">1.155× [0.991, 1.345]</text>
<text class="row-label" x="287" y="363.0" text-anchor="end">skip3-rows10-reverse [cross-srv]</text>
<g class="ci-win"><title>skip3-rows10-reverse [cross-srv]: 0.158× [0.155, 0.160]</title><line x1="310.2" y1="359.0" x2="321.0" y2="359.0" stroke-width="2.2"/><line x1="310.2" y1="355.0" x2="310.2" y2="363.0" stroke-width="1.8"/><line x1="321.0" y1="355.0" x2="321.0" y2="363.0" stroke-width="1.8"/><circle cx="316.7" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1271" y="363.0">0.158× [0.155, 0.160]</text>
<text class="row-label" x="287" y="383.0" text-anchor="end">skip3-rows1000-forward [cross-srv]</text>
<g class="ci-reg"><title>skip3-rows1000-forward [cross-srv]: 1.360× [1.295, 1.414]</title><line x1="1034.6" y1="379.0" x2="1064.5" y2="379.0" stroke-width="2.2"/><line x1="1034.6" y1="375.0" x2="1034.6" y2="383.0" stroke-width="1.8"/><line x1="1064.5" y1="375.0" x2="1064.5" y2="383.0" stroke-width="1.8"/><circle cx="1051.3" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1271" y="383.0">1.360× [1.295, 1.414]</text>
<text class="row-label" x="287" y="403.0" text-anchor="end">skip3-rows1000-reverse [cross-srv]</text>
<g class="ci-reg"><title>skip3-rows1000-reverse [cross-srv]: 1.324× [1.250, 1.407]</title><line x1="1022.5" y1="399.0" x2="1062.9" y2="399.0" stroke-width="2.2"/><line x1="1022.5" y1="395.0" x2="1022.5" y2="403.0" stroke-width="1.8"/><line x1="1062.9" y1="395.0" x2="1062.9" y2="403.0" stroke-width="1.8"/><circle cx="1042.1" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1271" y="403.0">1.324× [1.250, 1.407]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M2.2: Client Go Allocations (allocs/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH</strong> — Below 1.0× means USE_REMOTE_FETCH allocates fewer Go heap objects on the client.</figcaption>
<p class="bench-summary"><strong>Summary:</strong> Client Go heap allocations drop by 21–60% (0.398×–0.790×) on all 14 multi-row cases and by 5–6% (0.942×–0.952×) on split-limit1 because one getMappedRange RPC replaces per-record client fetch RPCs; only small-limit1 allocates 4–5% more (1.040×–1.049×, +1 alloc/op). Cutting client allocations does not prevent the 1.23×–2.33× wall-time regressions on 1,000-row scans when storage-server fan-out dominates latency.</p>
<svg class="bench-svg" viewBox="0 0 1352 438" role="img" aria-label="Figure M2.2: Client Go Allocations (allocs/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH">
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
<text class="axis-text" x="1006.9" y="17" text-anchor="end">← Remote fetch fewer allocs</text>
<text class="axis-text" x="1022.9" y="17" text-anchor="start">SCAN_AND_FETCH fewer allocs →</text>
<line class="section-divider" x1="8" y1="48" x2="1344" y2="48"/>
<text class="section-title" x="10" y="63">M2rl: Client Go allocs/op Ratio</text>
<text class="row-label" x="185" y="81.0" text-anchor="end">small-limit1-forward</text>
<g class="ci-reg"><title>small-limit1-forward: 1.040× [1.038, 1.041]</title><line x1="1044.0" y1="77.0" x2="1046.3" y2="77.0" stroke-width="2.2"/><line x1="1044.0" y1="73.0" x2="1044.0" y2="81.0" stroke-width="1.8"/><line x1="1046.3" y1="73.0" x2="1046.3" y2="81.0" stroke-width="1.8"/><circle cx="1045.5" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1169" y="81.0">1.040× [1.038, 1.041]</text>
<text class="row-label" x="185" y="101.0" text-anchor="end">small-limit1-reverse</text>
<g class="ci-reg"><title>small-limit1-reverse: 1.049× [1.047, 1.052]</title><line x1="1050.7" y1="97.0" x2="1054.4" y2="97.0" stroke-width="2.2"/><line x1="1050.7" y1="93.0" x2="1050.7" y2="101.0" stroke-width="1.8"/><line x1="1054.4" y1="93.0" x2="1054.4" y2="101.0" stroke-width="1.8"/><circle cx="1052.2" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1169" y="101.0">1.049× [1.047, 1.052]</text>
<text class="row-label" x="185" y="121.0" text-anchor="end">small-rows10-forward</text>
<g class="ci-win"><title>small-rows10-forward: 0.596× [0.596, 0.597]</title><line x1="611.7" y1="117.0" x2="613.0" y2="117.0" stroke-width="2.2"/><line x1="611.7" y1="113.0" x2="611.7" y2="121.0" stroke-width="1.8"/><line x1="613.0" y1="113.0" x2="613.0" y2="121.0" stroke-width="1.8"/><circle cx="611.7" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1169" y="121.0">0.596× [0.596, 0.597]</text>
<text class="row-label" x="185" y="141.0" text-anchor="end">small-rows10-reverse</text>
<g class="ci-win"><title>small-rows10-reverse: 0.595× [0.595, 0.595]</title><line x1="610.4" y1="137.0" x2="610.4" y2="137.0" stroke-width="2.2"/><line x1="610.4" y1="133.0" x2="610.4" y2="141.0" stroke-width="1.8"/><line x1="610.4" y1="133.0" x2="610.4" y2="141.0" stroke-width="1.8"/><circle cx="610.4" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1169" y="141.0">0.595× [0.595, 0.595]</text>
<text class="row-label" x="185" y="161.0" text-anchor="end">small-rows1000-forward</text>
<g class="ci-win"><title>small-rows1000-forward: 0.431× [0.431, 0.431]</title><line x1="359.2" y1="157.0" x2="359.2" y2="157.0" stroke-width="2.2"/><line x1="359.2" y1="153.0" x2="359.2" y2="161.0" stroke-width="1.8"/><line x1="359.2" y1="153.0" x2="359.2" y2="161.0" stroke-width="1.8"/><circle cx="359.2" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1169" y="161.0">0.431× [0.431, 0.431]</text>
<text class="row-label" x="185" y="181.0" text-anchor="end">small-rows1000-reverse</text>
<g class="ci-win"><title>small-rows1000-reverse: 0.430× [0.430, 0.430]</title><line x1="357.4" y1="177.0" x2="357.4" y2="177.0" stroke-width="2.2"/><line x1="357.4" y1="173.0" x2="357.4" y2="181.0" stroke-width="1.8"/><line x1="357.4" y1="173.0" x2="357.4" y2="181.0" stroke-width="1.8"/><circle cx="357.4" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1169" y="181.0">0.430× [0.430, 0.430]</text>
<text class="row-label" x="185" y="201.0" text-anchor="end">split-limit1-forward</text>
<g class="ci-win"><title>split-limit1-forward: 0.942× [0.940, 0.943]</title><line x1="966.7" y1="197.0" x2="969.2" y2="197.0" stroke-width="2.2"/><line x1="966.7" y1="193.0" x2="966.7" y2="201.0" stroke-width="1.8"/><line x1="969.2" y1="193.0" x2="969.2" y2="201.0" stroke-width="1.8"/><circle cx="968.4" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1169" y="201.0">0.942× [0.940, 0.943]</text>
<text class="row-label" x="185" y="221.0" text-anchor="end">split-limit1-reverse</text>
<g class="ci-win"><title>split-limit1-reverse: 0.952× [0.951, 0.953]</title><line x1="975.8" y1="217.0" x2="977.4" y2="217.0" stroke-width="2.2"/><line x1="975.8" y1="213.0" x2="975.8" y2="221.0" stroke-width="1.8"/><line x1="977.4" y1="213.0" x2="977.4" y2="221.0" stroke-width="1.8"/><circle cx="976.6" cy="217.0" r="3.8"/></g>
<text class="val-text" x="1169" y="221.0">0.952× [0.951, 0.953]</text>
<text class="row-label" x="185" y="241.0" text-anchor="end">split-rows10-forward</text>
<g class="ci-win"><title>split-rows10-forward: 0.505× [0.504, 0.505]</title><line x1="481.1" y1="237.0" x2="482.6" y2="237.0" stroke-width="2.2"/><line x1="481.1" y1="233.0" x2="481.1" y2="241.0" stroke-width="1.8"/><line x1="482.6" y1="233.0" x2="482.6" y2="241.0" stroke-width="1.8"/><circle cx="482.6" cy="237.0" r="3.8"/></g>
<text class="val-text" x="1169" y="241.0">0.505× [0.504, 0.505]</text>
<text class="row-label" x="185" y="261.0" text-anchor="end">split-rows10-reverse</text>
<g class="ci-win"><title>split-rows10-reverse: 0.504× [0.504, 0.505]</title><line x1="481.1" y1="257.0" x2="482.6" y2="257.0" stroke-width="2.2"/><line x1="481.1" y1="253.0" x2="481.1" y2="261.0" stroke-width="1.8"/><line x1="482.6" y1="253.0" x2="482.6" y2="261.0" stroke-width="1.8"/><circle cx="481.1" cy="257.0" r="3.8"/></g>
<text class="val-text" x="1169" y="261.0">0.504× [0.504, 0.505]</text>
<text class="row-label" x="185" y="281.0" text-anchor="end">split-rows1000-forward</text>
<g class="ci-win"><title>split-rows1000-forward: 0.399× [0.399, 0.399]</title><line x1="299.1" y1="277.0" x2="299.1" y2="277.0" stroke-width="2.2"/><line x1="299.1" y1="273.0" x2="299.1" y2="281.0" stroke-width="1.8"/><line x1="299.1" y1="273.0" x2="299.1" y2="281.0" stroke-width="1.8"/><circle cx="299.1" cy="277.0" r="3.8"/></g>
<text class="val-text" x="1169" y="281.0">0.399× [0.399, 0.399]</text>
<text class="row-label" x="185" y="301.0" text-anchor="end">split-rows1000-reverse</text>
<g class="ci-win"><title>split-rows1000-reverse: 0.398× [0.398, 0.398]</title><line x1="297.1" y1="297.0" x2="297.1" y2="297.0" stroke-width="2.2"/><line x1="297.1" y1="293.0" x2="297.1" y2="301.0" stroke-width="1.8"/><line x1="297.1" y1="293.0" x2="297.1" y2="301.0" stroke-width="1.8"/><circle cx="297.1" cy="297.0" r="3.8"/></g>
<text class="val-text" x="1169" y="301.0">0.398× [0.398, 0.398]</text>
<line class="section-divider" x1="8" y1="310" x2="1344" y2="310"/>
<text class="section-title" x="10" y="325">M2skip: Client Go allocs/op Ratio</text>
<text class="row-label" x="185" y="343.0" text-anchor="end">skip3-rows10-forward</text>
<g class="ci-win"><title>skip3-rows10-forward: 0.790× [0.790, 0.790]</title><line x1="831.3" y1="339.0" x2="831.3" y2="339.0" stroke-width="2.2"/><line x1="831.3" y1="335.0" x2="831.3" y2="343.0" stroke-width="1.8"/><line x1="831.3" y1="335.0" x2="831.3" y2="343.0" stroke-width="1.8"/><circle cx="831.3" cy="339.0" r="3.8"/></g>
<text class="val-text" x="1169" y="343.0">0.790× [0.790, 0.790]</text>
<text class="row-label" x="185" y="363.0" text-anchor="end">skip3-rows10-reverse</text>
<g class="ci-win"><title>skip3-rows10-reverse: 0.709× [0.709, 0.709]</title><line x1="747.0" y1="359.0" x2="747.0" y2="359.0" stroke-width="2.2"/><line x1="747.0" y1="355.0" x2="747.0" y2="363.0" stroke-width="1.8"/><line x1="747.0" y1="355.0" x2="747.0" y2="363.0" stroke-width="1.8"/><circle cx="747.0" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1169" y="363.0">0.709× [0.709, 0.709]</text>
<text class="row-label" x="185" y="383.0" text-anchor="end">skip3-rows1000-forward</text>
<g class="ci-win"><title>skip3-rows1000-forward: 0.529× [0.529, 0.529]</title><line x1="518.8" y1="379.0" x2="518.8" y2="379.0" stroke-width="2.2"/><line x1="518.8" y1="375.0" x2="518.8" y2="383.0" stroke-width="1.8"/><line x1="518.8" y1="375.0" x2="518.8" y2="383.0" stroke-width="1.8"/><circle cx="518.8" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1169" y="383.0">0.529× [0.529, 0.529]</text>
<text class="row-label" x="185" y="403.0" text-anchor="end">skip3-rows1000-reverse</text>
<g class="ci-win"><title>skip3-rows1000-reverse: 0.527× [0.527, 0.527]</title><line x1="515.9" y1="399.0" x2="515.9" y2="399.0" stroke-width="2.2"/><line x1="515.9" y1="395.0" x2="515.9" y2="403.0" stroke-width="1.8"/><line x1="515.9" y1="395.0" x2="515.9" y2="403.0" stroke-width="1.8"/><circle cx="515.9" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1169" y="403.0">0.527× [0.527, 0.527]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

### `M2rl`: `BenchmarkIndexFetch` (`final-remote / final-ordinary`)

Geometric mean of six paired block ratios (median of 3 repeats per process,
`3s` benchtime), with nominal 95% block-bootstrap intervals (20,000 resamples,
preregistered seed 492817; initial exploratory analysis with seed 391706 is
retained in the campaign archive). Below 1 means `USE_REMOTE_FETCH` is faster
or allocates less.

| Case | Sealed placement label | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| small-limit1-forward | cross-server | 0.979 [0.964, 0.995] | 0.957 [0.956, 0.958] | 1.040 [1.038, 1.041] |
| small-limit1-reverse | cross-server | 0.985 [0.969, 1.003] | 0.958 [0.957, 0.959] | 1.049 [1.047, 1.052] |
| small-rows10-forward | cross-server | **1.031 [1.018, 1.042]** | 0.759 [0.759, 0.760] | 0.596 [0.596, 0.597] |
| small-rows10-reverse | cross-server | **1.026 [1.013, 1.039]** | 0.755 [0.755, 0.756] | 0.595 [0.595, 0.595] |
| small-rows1000-forward | cross-server | **1.226 [1.175, 1.280]** | 0.532 [0.531, 0.534] | 0.431 [0.431, 0.431] |
| small-rows1000-reverse | cross-server | **1.255 [1.217, 1.293]** | 0.527 [0.526, 0.528] | 0.430 [0.430, 0.430] |
| split-limit1-forward | cross-server | **1.110 [1.087, 1.131]** | 0.715 [0.715, 0.716] | 0.942 [0.940, 0.943] |
| split-limit1-reverse | cross-server | **1.114 [1.095, 1.138]** | 0.715 [0.715, 0.715] | 0.952 [0.951, 0.953] |
| split-rows10-forward | partially colocated (f = 0.0000) | 0.873 [0.829, 0.920] | 0.695 [0.695, 0.695] | 0.505 [0.504, 0.505] |
| split-rows10-reverse | partially colocated (f = 0.0000) | 0.854 [0.823, 0.886] | 0.695 [0.695, 0.695] | 0.504 [0.504, 0.505] |
| split-rows1000-forward | partially colocated (f = 0.6960) | **2.326 [2.254, 2.397]** | 0.692 [0.692, 0.692] | 0.399 [0.399, 0.399] |
| split-rows1000-reverse | partially colocated (f = 0.6960) | **2.300 [2.206, 2.384]** | 0.693 [0.692, 0.693] | 0.398 [0.398, 0.398] |

### `M2skip`: `BenchmarkPlanSkip` (`final-remote / final-ordinary`)

| Case | Sealed placement label | Mean ns/op | Go B/op | Go allocs/op |
|---|---|---:|---:|---:|
| skip3-rows10-forward | cross-server | 1.155 [0.991, 1.345] | 0.689 [0.688, 0.689] | 0.790 [0.790, 0.790] |
| skip3-rows10-reverse | cross-server | **0.158 [0.155, 0.160]** | 0.590 [0.589, 0.590] | 0.709 [0.709, 0.709] |
| skip3-rows1000-forward | cross-server | **1.360 [1.295, 1.414]** | 0.599 [0.598, 0.600] | 0.529 [0.529, 0.529] |
| skip3-rows1000-reverse | cross-server | **1.324 [1.250, 1.407]** | 0.594 [0.593, 0.595] | 0.527 [0.527, 0.527] |

All `rows/op` and `prefix-B` ratios in both `M2rl` and `M2skip` were **1.000
[1.000, 1.000]** (`prefix-B = 19` in `M2rl`, `22` in `M2skip`).

### Absolute values

Medians of the six block medians per arm.

| Phase | Case | Placement label | Ordinary ns/op (ms) | Remote ns/op (ms) | Ordinary B/op | Remote B/op | Ordinary allocs/op | Remote allocs/op |
|---|---|---|---:|---:|---:|---:|---:|---:|
| M2rl | small-limit1-forward | cross-server | 2.69 | 2.62 | 17,620 | 16,852 | 222 | 231 |
| M2rl | small-limit1-reverse | cross-server | 2.71 | 2.66 | 17,668 | 16,930 | 223 | 234 |
| M2rl | small-rows10-forward | cross-server | 3.22 | 3.33 | 92,274 | 70,033 | 696 | 415 |
| M2rl | small-rows10-reverse | cross-server | 3.22 | 3.33 | 92,784 | 70,044 | 697 | 415 |
| M2rl | small-rows1000-forward | cross-server | 95.10 | 117.08 | 4,497,080 | 2,393,973 | 52,669 | 22,677 |
| M2rl | small-rows1000-reverse | cross-server | 96.39 | 118.95 | 4,544,538 | 2,393,438 | 52,664 | 22,669 |
| M2rl | split-limit1-forward | cross-server | 3.76 | 4.20 | 475,736 | 340,354 | 263 | 248 |
| M2rl | split-limit1-reverse | cross-server | 3.96 | 4.41 | 475,836 | 340,297 | 263 | 250 |
| M2rl | split-rows10-forward | partially colocated (f = 0.0000) | 6.83 | 6.00 | 4,651,958 | 3,232,746 | 1,050 | 530 |
| M2rl | split-rows10-reverse | partially colocated (f = 0.0000) | 6.81 | 5.77 | 4,652,330 | 3,233,112 | 1,051 | 530 |
| M2rl | split-rows1000-forward | partially colocated (f = 0.6960) | 316.38 | 742.51 | 457,265,229 | 316,465,896 | 79,120 | 31,564 |
| M2rl | split-rows1000-reverse | partially colocated (f = 0.6960) | 327.62 | 768.95 | 457,308,020 | 316,621,142 | 79,125 | 31,476 |
| M2skip | skip3-rows10-forward | cross-server | 3.69 | 4.89 | 186,950 | 128,733 | 1,175 | 928 |
| M2skip | skip3-rows10-reverse | cross-server | 31.13 | 4.87 | 218,190 | 128,648 | 1,307 | 927 |
| M2skip | skip3-rows1000-forward | cross-server | 98.67 | 138.19 | 5,380,260 | 3,224,836 | 67,030 | 35,456 |
| M2skip | skip3-rows1000-reverse | cross-server | 101.06 | 133.24 | 5,433,262 | 3,226,728 | 67,278 | 35,446 |

## Interpretation

- **1,000-row scans on the 3-node `double` `ssd-2` cluster:**
  - On this 3-node `ssd-2` cluster (which differs from the single-node campaign
    simultaneously in host topology, network, `ssd-2` vs `memory` storage
    engine, `double` vs `single` redundancy, and storage-server count, so
    differences between the two campaigns are descriptive and cannot be
    attributed to one factor alone), every 1,000-row scan — both the
    `cross-server` cases (`small-rows1000`, `skip3-rows1000`) and the
    `partially colocated (f = 0.6960)` case (`split-rows1000`) — was **23% to
    133% slower** with `USE_REMOTE_FETCH` than with `SCAN_AND_FETCH`.
  - *Untested hypothesis:* in `SCAN_AND_FETCH`, the client pipelines
    `GetRange` point-range reads directly to the storage servers holding the
    primary records (with client-side load balancing across the two `double`
    replicas). In `USE_REMOTE_FETCH`, the storage server holding the index
    shard issues secondary-to-primary lookups to the storage processes holding
    the primary records (which for process-disjoint teams may be on another
    process on the same host or across the VPC, as per-lookup routing was not
    traced) and serializes the combined `MappedKeyValue` batches back through
    the index storage server; on 1,000-row and 100 MB split-record scans on
    `ssd-2`, that path may bottleneck compared with direct client-pipelined
    fetches.
- **Where `USE_REMOTE_FETCH` won:**
  - `skip3-rows10-reverse` (`0.158 [0.155, 0.160]`, 4.87 ms vs 31.13 ms):
    dramatically faster with remote fetch.
  - `split-rows10` (`0.873` forward, `0.854` reverse, where the 10 target
    records happened to be colocated on the same storage team as the index
    shard, `f = 0.0000`) and `small-limit1` (`0.979` forward).
  - Client-side Go heap allocations (`B/op` and `allocs/op`) were lower with
    `USE_REMOTE_FETCH` on every multi-row case (`B/op` `0.527–0.759`,
    `allocs/op` `0.398–0.790`).
- **Implication for defaults:** these multi-node VPC measurements show that
  `USE_REMOTE_FETCH` is not a universal win and regressed 1,000-row scans in
  the measured configurations (`1.226–2.326×`), supporting retaining
  `SCAN_AND_FETCH` as the default for the measured workloads and conditions.
