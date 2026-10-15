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
<svg class="bench-svg" viewBox="0 0 860 506" role="img" aria-label="Figure M2.1: Mean Time (ns/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH (log₂ scale, 95% CI)">
<line class="grid-line" x1="215.0" y1="28" x2="215.0" y2="476"/>
<text class="axis-text" x="215.0" y="22" text-anchor="middle">0.15×</text>
<text class="axis-text" x="215.0" y="491" text-anchor="middle">0.15×</text>
<line class="grid-line" x1="302.2" y1="28" x2="302.2" y2="476"/>
<text class="axis-text" x="302.2" y="22" text-anchor="middle">0.25×</text>
<text class="axis-text" x="302.2" y="491" text-anchor="middle">0.25×</text>
<line class="grid-line" x1="420.4" y1="28" x2="420.4" y2="476"/>
<text class="axis-text" x="420.4" y="22" text-anchor="middle">0.50×</text>
<text class="axis-text" x="420.4" y="491" text-anchor="middle">0.50×</text>
<line class="grid-line" x1="477.8" y1="28" x2="477.8" y2="476"/>
<text class="axis-text" x="477.8" y="22" text-anchor="middle">0.70×</text>
<text class="axis-text" x="477.8" y="491" text-anchor="middle">0.70×</text>
<line class="parity-line" x1="538.7" y1="28" x2="538.7" y2="476"/>
<text class="axis-text" x="538.7" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="538.7" y="491" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="596.1" y1="28" x2="596.1" y2="476"/>
<text class="axis-text" x="596.1" y="22" text-anchor="middle">1.4×</text>
<text class="axis-text" x="596.1" y="491" text-anchor="middle">1.4×</text>
<line class="grid-line" x1="656.9" y1="28" x2="656.9" y2="476"/>
<text class="axis-text" x="656.9" y="22" text-anchor="middle">2.0×</text>
<text class="axis-text" x="656.9" y="491" text-anchor="middle">2.0×</text>
<line class="grid-line" x1="695.0" y1="28" x2="695.0" y2="476"/>
<text class="axis-text" x="695.0" y="22" text-anchor="middle">2.5×</text>
<text class="axis-text" x="695.0" y="491" text-anchor="middle">2.5×</text>
<text class="axis-text" x="530.7" y="9" text-anchor="end">← Remote fetch faster</text>
<text class="axis-text" x="546.7" y="9" text-anchor="start">SCAN_AND_FETCH faster →</text>
<line class="section-divider" x1="8" y1="40" x2="852" y2="40"/>
<text class="section-title" x="10" y="55">M2rl: BenchmarkIndexFetch (3-node double ssd-2)</text>
<text class="row-label" x="203" y="79.2" text-anchor="end">small-limit1-forward [cross-server]</text>
<g class="ci-win"><title>small-limit1-forward [cross-server]: 0.979× [0.964, 0.995]</title><line x1="532.4" y1="75.2" x2="537.8" y2="75.2" stroke-width="2.2"/><line x1="532.4" y1="71.2" x2="532.4" y2="79.2" stroke-width="1.8"/><line x1="537.8" y1="71.2" x2="537.8" y2="79.2" stroke-width="1.8"/><circle cx="535.0" cy="75.2" r="3.8"/></g>
<text class="val-text" x="707" y="79.2">0.979× [0.964, 0.995]</text>
<text class="row-label" x="203" y="103.2" text-anchor="end">small-limit1-reverse [cross-server]</text>
<g class="ci-neutral"><title>small-limit1-reverse [cross-server]: 0.985× [0.969, 1.003]</title><line x1="533.3" y1="99.2" x2="539.2" y2="99.2" stroke-width="2.2"/><line x1="533.3" y1="95.2" x2="533.3" y2="103.2" stroke-width="1.8"/><line x1="539.2" y1="95.2" x2="539.2" y2="103.2" stroke-width="1.8"/><circle cx="536.1" cy="99.2" r="3.8"/></g>
<text class="val-text" x="707" y="103.2">0.985× [0.969, 1.003]</text>
<text class="row-label" x="203" y="127.2" text-anchor="end">small-rows10-forward [cross-server]</text>
<g class="ci-reg"><title>small-rows10-forward [cross-server]: 1.031× [1.018, 1.042]</title><line x1="541.7" y1="123.2" x2="545.7" y2="123.2" stroke-width="2.2"/><line x1="541.7" y1="119.2" x2="541.7" y2="127.2" stroke-width="1.8"/><line x1="545.7" y1="119.2" x2="545.7" y2="127.2" stroke-width="1.8"/><circle cx="543.9" cy="123.2" r="3.8"/></g>
<text class="val-text" x="707" y="127.2">1.031× [1.018, 1.042]</text>
<text class="row-label" x="203" y="151.2" text-anchor="end">small-rows10-reverse [cross-server]</text>
<g class="ci-reg"><title>small-rows10-reverse [cross-server]: 1.026× [1.013, 1.039]</title><line x1="540.9" y1="147.2" x2="545.2" y2="147.2" stroke-width="2.2"/><line x1="540.9" y1="143.2" x2="540.9" y2="151.2" stroke-width="1.8"/><line x1="545.2" y1="143.2" x2="545.2" y2="151.2" stroke-width="1.8"/><circle cx="543.0" cy="147.2" r="3.8"/></g>
<text class="val-text" x="707" y="151.2">1.026× [1.013, 1.039]</text>
<text class="row-label" x="203" y="175.2" text-anchor="end">small-rows1000-forward [cross-server]</text>
<g class="ci-reg"><title>small-rows1000-forward [cross-server]: 1.226× [1.175, 1.280]</title><line x1="566.2" y1="171.2" x2="580.8" y2="171.2" stroke-width="2.2"/><line x1="566.2" y1="167.2" x2="566.2" y2="175.2" stroke-width="1.8"/><line x1="580.8" y1="167.2" x2="580.8" y2="175.2" stroke-width="1.8"/><circle cx="573.4" cy="171.2" r="3.8"/></g>
<text class="val-text" x="707" y="175.2">1.226× [1.175, 1.280]</text>
<text class="row-label" x="203" y="199.2" text-anchor="end">small-rows1000-reverse [cross-server]</text>
<g class="ci-reg"><title>small-rows1000-reverse [cross-server]: 1.255× [1.217, 1.293]</title><line x1="572.2" y1="195.2" x2="582.5" y2="195.2" stroke-width="2.2"/><line x1="572.2" y1="191.2" x2="572.2" y2="199.2" stroke-width="1.8"/><line x1="582.5" y1="191.2" x2="582.5" y2="199.2" stroke-width="1.8"/><circle cx="577.4" cy="195.2" r="3.8"/></g>
<text class="val-text" x="707" y="199.2">1.255× [1.217, 1.293]</text>
<text class="row-label" x="203" y="223.2" text-anchor="end">split-limit1-forward [cross-server]</text>
<g class="ci-reg"><title>split-limit1-forward [cross-server]: 1.110× [1.087, 1.131]</title><line x1="552.9" y1="219.2" x2="559.7" y2="219.2" stroke-width="2.2"/><line x1="552.9" y1="215.2" x2="552.9" y2="223.2" stroke-width="1.8"/><line x1="559.7" y1="215.2" x2="559.7" y2="223.2" stroke-width="1.8"/><circle cx="556.5" cy="219.2" r="3.8"/></g>
<text class="val-text" x="707" y="223.2">1.110× [1.087, 1.131]</text>
<text class="row-label" x="203" y="247.2" text-anchor="end">split-limit1-reverse [cross-server]</text>
<g class="ci-reg"><title>split-limit1-reverse [cross-server]: 1.114× [1.095, 1.138]</title><line x1="554.2" y1="243.2" x2="560.7" y2="243.2" stroke-width="2.2"/><line x1="554.2" y1="239.2" x2="554.2" y2="247.2" stroke-width="1.8"/><line x1="560.7" y1="239.2" x2="560.7" y2="247.2" stroke-width="1.8"/><circle cx="557.1" cy="243.2" r="3.8"/></g>
<text class="val-text" x="707" y="247.2">1.114× [1.095, 1.138]</text>
<text class="row-label" x="203" y="271.2" text-anchor="end">split-rows10-forward [partially colocated (f = 0.0000)]</text>
<g class="ci-win"><title>split-rows10-forward [partially colocated (f = 0.0000)]: 0.873× [0.829, 0.920]</title><line x1="506.7" y1="267.2" x2="524.4" y2="267.2" stroke-width="2.2"/><line x1="506.7" y1="263.2" x2="506.7" y2="271.2" stroke-width="1.8"/><line x1="524.4" y1="263.2" x2="524.4" y2="271.2" stroke-width="1.8"/><circle cx="515.5" cy="267.2" r="3.8"/></g>
<text class="val-text" x="707" y="271.2">0.873× [0.829, 0.920]</text>
<text class="row-label" x="203" y="295.2" text-anchor="end">split-rows10-reverse [partially colocated (f = 0.0000)]</text>
<g class="ci-win"><title>split-rows10-reverse [partially colocated (f = 0.0000)]: 0.854× [0.823, 0.886]</title><line x1="505.4" y1="291.2" x2="518.0" y2="291.2" stroke-width="2.2"/><line x1="505.4" y1="287.2" x2="505.4" y2="295.2" stroke-width="1.8"/><line x1="518.0" y1="287.2" x2="518.0" y2="295.2" stroke-width="1.8"/><circle cx="511.7" cy="291.2" r="3.8"/></g>
<text class="val-text" x="707" y="295.2">0.854× [0.823, 0.886]</text>
<text class="row-label" x="203" y="319.2" text-anchor="end">split-rows1000-forward [partially colocated (f = 0.6960)]</text>
<g class="ci-reg"><title>split-rows1000-forward [partially colocated (f = 0.6960)]: 2.326× [2.254, 2.397]</title><line x1="677.3" y1="315.2" x2="687.8" y2="315.2" stroke-width="2.2"/><line x1="677.3" y1="311.2" x2="677.3" y2="319.2" stroke-width="1.8"/><line x1="687.8" y1="311.2" x2="687.8" y2="319.2" stroke-width="1.8"/><circle cx="682.7" cy="315.2" r="3.8"/></g>
<text class="val-text" x="707" y="319.2">2.326× [2.254, 2.397]</text>
<text class="row-label" x="203" y="343.2" text-anchor="end">split-rows1000-reverse [partially colocated (f = 0.6960)]</text>
<g class="ci-reg"><title>split-rows1000-reverse [partially colocated (f = 0.6960)]: 2.300× [2.206, 2.384]</title><line x1="673.7" y1="339.2" x2="686.9" y2="339.2" stroke-width="2.2"/><line x1="673.7" y1="335.2" x2="673.7" y2="343.2" stroke-width="1.8"/><line x1="686.9" y1="335.2" x2="686.9" y2="343.2" stroke-width="1.8"/><circle cx="680.8" cy="339.2" r="3.8"/></g>
<text class="val-text" x="707" y="343.2">2.300× [2.206, 2.384]</text>
<line class="section-divider" x1="8" y1="354" x2="852" y2="354"/>
<text class="section-title" x="10" y="369">M2skip: BenchmarkPlanSkip (3-node double ssd-2)</text>
<text class="row-label" x="203" y="393.2" text-anchor="end">skip3-rows10-forward [cross-server]</text>
<g class="ci-neutral"><title>skip3-rows10-forward [cross-server]: 1.155× [0.991, 1.345]</title><line x1="537.1" y1="389.2" x2="589.2" y2="389.2" stroke-width="2.2"/><line x1="537.1" y1="385.2" x2="537.1" y2="393.2" stroke-width="1.8"/><line x1="589.2" y1="385.2" x2="589.2" y2="393.2" stroke-width="1.8"/><circle cx="563.3" cy="389.2" r="3.8"/></g>
<text class="val-text" x="707" y="393.2">1.155× [0.991, 1.345]</text>
<text class="row-label" x="203" y="417.2" text-anchor="end">skip3-rows10-reverse [cross-server]</text>
<g class="ci-win"><title>skip3-rows10-reverse [cross-server]: 0.158× [0.155, 0.160]</title><line x1="220.6" y1="413.2" x2="226.0" y2="413.2" stroke-width="2.2"/><line x1="220.6" y1="409.2" x2="220.6" y2="417.2" stroke-width="1.8"/><line x1="226.0" y1="409.2" x2="226.0" y2="417.2" stroke-width="1.8"/><circle cx="223.9" cy="413.2" r="3.8"/></g>
<text class="val-text" x="707" y="417.2">0.158× [0.155, 0.160]</text>
<text class="row-label" x="203" y="441.2" text-anchor="end">skip3-rows1000-forward [cross-server]</text>
<g class="ci-reg"><title>skip3-rows1000-forward [cross-server]: 1.360× [1.295, 1.414]</title><line x1="582.8" y1="437.2" x2="597.8" y2="437.2" stroke-width="2.2"/><line x1="582.8" y1="433.2" x2="582.8" y2="441.2" stroke-width="1.8"/><line x1="597.8" y1="433.2" x2="597.8" y2="441.2" stroke-width="1.8"/><circle cx="591.1" cy="437.2" r="3.8"/></g>
<text class="val-text" x="707" y="441.2">1.360× [1.295, 1.414]</text>
<text class="row-label" x="203" y="465.2" text-anchor="end">skip3-rows1000-reverse [cross-server]</text>
<g class="ci-reg"><title>skip3-rows1000-reverse [cross-server]: 1.324× [1.250, 1.407]</title><line x1="576.7" y1="461.2" x2="596.9" y2="461.2" stroke-width="2.2"/><line x1="576.7" y1="457.2" x2="576.7" y2="465.2" stroke-width="1.8"/><line x1="596.9" y1="457.2" x2="596.9" y2="465.2" stroke-width="1.8"/><circle cx="586.6" cy="461.2" r="3.8"/></g>
<text class="val-text" x="707" y="465.2">1.324× [1.250, 1.407]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure M2.2: Client Go Allocations (allocs/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH</strong> — Below 1.0× means USE_REMOTE_FETCH allocates fewer Go heap objects on the client.</figcaption>
<svg class="bench-svg" viewBox="0 0 860 506" role="img" aria-label="Figure M2.2: Client Go Allocations (allocs/op) Ratio — USE_REMOTE_FETCH / SCAN_AND_FETCH">
<line class="grid-line" x1="215.0" y1="28" x2="215.0" y2="476"/>
<text class="axis-text" x="215.0" y="22" text-anchor="middle">0.35×</text>
<text class="axis-text" x="215.0" y="491" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="353.9" y1="28" x2="353.9" y2="476"/>
<text class="axis-text" x="353.9" y="22" text-anchor="middle">0.50×</text>
<text class="axis-text" x="353.9" y="491" text-anchor="middle">0.50×</text>
<line class="grid-line" x1="485.0" y1="28" x2="485.0" y2="476"/>
<text class="axis-text" x="485.0" y="22" text-anchor="middle">0.70×</text>
<text class="axis-text" x="485.0" y="491" text-anchor="middle">0.70×</text>
<line class="parity-line" x1="624.0" y1="28" x2="624.0" y2="476"/>
<text class="axis-text" x="624.0" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="624.0" y="491" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="695.0" y1="28" x2="695.0" y2="476"/>
<text class="axis-text" x="695.0" y="22" text-anchor="middle">1.2×</text>
<text class="axis-text" x="695.0" y="491" text-anchor="middle">1.2×</text>
<text class="axis-text" x="616.0" y="9" text-anchor="end">← Remote fetch fewer allocs</text>
<text class="axis-text" x="632.0" y="9" text-anchor="start">SCAN_AND_FETCH fewer allocs →</text>
<line class="section-divider" x1="8" y1="40" x2="852" y2="40"/>
<text class="section-title" x="10" y="55">M2rl: Client Go allocs/op Ratio</text>
<text class="row-label" x="203" y="79.2" text-anchor="end">small-limit1-forward</text>
<g class="ci-reg"><title>small-limit1-forward: 1.040× [1.038, 1.041]</title><line x1="638.5" y1="75.2" x2="639.6" y2="75.2" stroke-width="2.2"/><line x1="638.5" y1="71.2" x2="638.5" y2="79.2" stroke-width="1.8"/><line x1="639.6" y1="71.2" x2="639.6" y2="79.2" stroke-width="1.8"/><circle cx="639.3" cy="75.2" r="3.8"/></g>
<text class="val-text" x="707" y="79.2">1.040× [1.038, 1.041]</text>
<text class="row-label" x="203" y="103.2" text-anchor="end">small-limit1-reverse</text>
<g class="ci-reg"><title>small-limit1-reverse: 1.049× [1.047, 1.052]</title><line x1="641.9" y1="99.2" x2="643.7" y2="99.2" stroke-width="2.2"/><line x1="641.9" y1="95.2" x2="641.9" y2="103.2" stroke-width="1.8"/><line x1="643.7" y1="95.2" x2="643.7" y2="103.2" stroke-width="1.8"/><circle cx="642.6" cy="99.2" r="3.8"/></g>
<text class="val-text" x="707" y="103.2">1.049× [1.047, 1.052]</text>
<text class="row-label" x="203" y="127.2" text-anchor="end">small-rows10-forward</text>
<g class="ci-win"><title>small-rows10-forward: 0.596× [0.596, 0.597]</title><line x1="422.4" y1="123.2" x2="423.0" y2="123.2" stroke-width="2.2"/><line x1="422.4" y1="119.2" x2="422.4" y2="127.2" stroke-width="1.8"/><line x1="423.0" y1="119.2" x2="423.0" y2="127.2" stroke-width="1.8"/><circle cx="422.4" cy="123.2" r="3.8"/></g>
<text class="val-text" x="707" y="127.2">0.596× [0.596, 0.597]</text>
<text class="row-label" x="203" y="151.2" text-anchor="end">small-rows10-reverse</text>
<g class="ci-win"><title>small-rows10-reverse: 0.595× [0.595, 0.595]</title><line x1="421.7" y1="147.2" x2="421.7" y2="147.2" stroke-width="2.2"/><line x1="421.7" y1="143.2" x2="421.7" y2="151.2" stroke-width="1.8"/><line x1="421.7" y1="143.2" x2="421.7" y2="151.2" stroke-width="1.8"/><circle cx="421.7" cy="147.2" r="3.8"/></g>
<text class="val-text" x="707" y="151.2">0.595× [0.595, 0.595]</text>
<text class="row-label" x="203" y="175.2" text-anchor="end">small-rows1000-forward</text>
<g class="ci-win"><title>small-rows1000-forward: 0.431× [0.431, 0.431]</title><line x1="296.1" y1="171.2" x2="296.1" y2="171.2" stroke-width="2.2"/><line x1="296.1" y1="167.2" x2="296.1" y2="175.2" stroke-width="1.8"/><line x1="296.1" y1="167.2" x2="296.1" y2="175.2" stroke-width="1.8"/><circle cx="296.1" cy="171.2" r="3.8"/></g>
<text class="val-text" x="707" y="175.2">0.431× [0.431, 0.431]</text>
<text class="row-label" x="203" y="199.2" text-anchor="end">small-rows1000-reverse</text>
<g class="ci-win"><title>small-rows1000-reverse: 0.430× [0.430, 0.430]</title><line x1="295.2" y1="195.2" x2="295.2" y2="195.2" stroke-width="2.2"/><line x1="295.2" y1="191.2" x2="295.2" y2="199.2" stroke-width="1.8"/><line x1="295.2" y1="191.2" x2="295.2" y2="199.2" stroke-width="1.8"/><circle cx="295.2" cy="195.2" r="3.8"/></g>
<text class="val-text" x="707" y="199.2">0.430× [0.430, 0.430]</text>
<text class="row-label" x="203" y="223.2" text-anchor="end">split-limit1-forward</text>
<g class="ci-win"><title>split-limit1-forward: 0.942× [0.940, 0.943]</title><line x1="599.9" y1="219.2" x2="601.1" y2="219.2" stroke-width="2.2"/><line x1="599.9" y1="215.2" x2="599.9" y2="223.2" stroke-width="1.8"/><line x1="601.1" y1="215.2" x2="601.1" y2="223.2" stroke-width="1.8"/><circle cx="600.7" cy="219.2" r="3.8"/></g>
<text class="val-text" x="707" y="223.2">0.942× [0.940, 0.943]</text>
<text class="row-label" x="203" y="247.2" text-anchor="end">split-limit1-reverse</text>
<g class="ci-win"><title>split-limit1-reverse: 0.952× [0.951, 0.953]</title><line x1="604.4" y1="243.2" x2="605.2" y2="243.2" stroke-width="2.2"/><line x1="604.4" y1="239.2" x2="604.4" y2="247.2" stroke-width="1.8"/><line x1="605.2" y1="239.2" x2="605.2" y2="247.2" stroke-width="1.8"/><circle cx="604.8" cy="243.2" r="3.8"/></g>
<text class="val-text" x="707" y="247.2">0.952× [0.951, 0.953]</text>
<text class="row-label" x="203" y="271.2" text-anchor="end">split-rows10-forward</text>
<g class="ci-win"><title>split-rows10-forward: 0.505× [0.504, 0.505]</title><line x1="357.1" y1="267.2" x2="357.8" y2="267.2" stroke-width="2.2"/><line x1="357.1" y1="263.2" x2="357.1" y2="271.2" stroke-width="1.8"/><line x1="357.8" y1="263.2" x2="357.8" y2="271.2" stroke-width="1.8"/><circle cx="357.8" cy="267.2" r="3.8"/></g>
<text class="val-text" x="707" y="271.2">0.505× [0.504, 0.505]</text>
<text class="row-label" x="203" y="295.2" text-anchor="end">split-rows10-reverse</text>
<g class="ci-win"><title>split-rows10-reverse: 0.504× [0.504, 0.505]</title><line x1="357.1" y1="291.2" x2="357.8" y2="291.2" stroke-width="2.2"/><line x1="357.1" y1="287.2" x2="357.1" y2="295.2" stroke-width="1.8"/><line x1="357.8" y1="287.2" x2="357.8" y2="295.2" stroke-width="1.8"/><circle cx="357.1" cy="291.2" r="3.8"/></g>
<text class="val-text" x="707" y="295.2">0.504× [0.504, 0.505]</text>
<text class="row-label" x="203" y="319.2" text-anchor="end">split-rows1000-forward</text>
<g class="ci-win"><title>split-rows1000-forward: 0.399× [0.399, 0.399]</title><line x1="266.0" y1="315.2" x2="266.0" y2="315.2" stroke-width="2.2"/><line x1="266.0" y1="311.2" x2="266.0" y2="319.2" stroke-width="1.8"/><line x1="266.0" y1="311.2" x2="266.0" y2="319.2" stroke-width="1.8"/><circle cx="266.0" cy="315.2" r="3.8"/></g>
<text class="val-text" x="707" y="319.2">0.399× [0.399, 0.399]</text>
<text class="row-label" x="203" y="343.2" text-anchor="end">split-rows1000-reverse</text>
<g class="ci-win"><title>split-rows1000-reverse: 0.398× [0.398, 0.398]</title><line x1="265.1" y1="339.2" x2="265.1" y2="339.2" stroke-width="2.2"/><line x1="265.1" y1="335.2" x2="265.1" y2="343.2" stroke-width="1.8"/><line x1="265.1" y1="335.2" x2="265.1" y2="343.2" stroke-width="1.8"/><circle cx="265.1" cy="339.2" r="3.8"/></g>
<text class="val-text" x="707" y="343.2">0.398× [0.398, 0.398]</text>
<line class="section-divider" x1="8" y1="354" x2="852" y2="354"/>
<text class="section-title" x="10" y="369">M2skip: Client Go allocs/op Ratio</text>
<text class="row-label" x="203" y="393.2" text-anchor="end">skip3-rows10-forward</text>
<g class="ci-win"><title>skip3-rows10-forward: 0.790× [0.790, 0.790]</title><line x1="532.1" y1="389.2" x2="532.1" y2="389.2" stroke-width="2.2"/><line x1="532.1" y1="385.2" x2="532.1" y2="393.2" stroke-width="1.8"/><line x1="532.1" y1="385.2" x2="532.1" y2="393.2" stroke-width="1.8"/><circle cx="532.1" cy="389.2" r="3.8"/></g>
<text class="val-text" x="707" y="393.2">0.790× [0.790, 0.790]</text>
<text class="row-label" x="203" y="417.2" text-anchor="end">skip3-rows10-reverse</text>
<g class="ci-win"><title>skip3-rows10-reverse: 0.709× [0.709, 0.709]</title><line x1="490.0" y1="413.2" x2="490.0" y2="413.2" stroke-width="2.2"/><line x1="490.0" y1="409.2" x2="490.0" y2="417.2" stroke-width="1.8"/><line x1="490.0" y1="409.2" x2="490.0" y2="417.2" stroke-width="1.8"/><circle cx="490.0" cy="413.2" r="3.8"/></g>
<text class="val-text" x="707" y="417.2">0.709× [0.709, 0.709]</text>
<text class="row-label" x="203" y="441.2" text-anchor="end">skip3-rows1000-forward</text>
<g class="ci-win"><title>skip3-rows1000-forward: 0.529× [0.529, 0.529]</title><line x1="375.9" y1="437.2" x2="375.9" y2="437.2" stroke-width="2.2"/><line x1="375.9" y1="433.2" x2="375.9" y2="441.2" stroke-width="1.8"/><line x1="375.9" y1="433.2" x2="375.9" y2="441.2" stroke-width="1.8"/><circle cx="375.9" cy="437.2" r="3.8"/></g>
<text class="val-text" x="707" y="441.2">0.529× [0.529, 0.529]</text>
<text class="row-label" x="203" y="465.2" text-anchor="end">skip3-rows1000-reverse</text>
<g class="ci-win"><title>skip3-rows1000-reverse: 0.527× [0.527, 0.527]</title><line x1="374.4" y1="461.2" x2="374.4" y2="461.2" stroke-width="2.2"/><line x1="374.4" y1="457.2" x2="374.4" y2="465.2" stroke-width="1.8"/><line x1="374.4" y1="457.2" x2="374.4" y2="465.2" stroke-width="1.8"/><circle cx="374.4" cy="461.2" r="3.8"/></g>
<text class="val-text" x="707" y="465.2">0.527× [0.527, 0.527]</text>
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
