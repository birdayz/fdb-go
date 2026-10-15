---
title: "Phase A: Client (1 worker)"
description: "Pure-Go client vs CGo (libfdb_c 7.3.77) and vs pre-allocation pure-Go across 11 workloads at 1 closed-loop worker."
weight: 1
---

Pure-Go client at the final tree against its predecessor and against Apple's
Go binding over `libfdb_c` 7.3.77. One closed-loop worker, 11 workloads,
counterbalanced. Part of the [hcloud-rebench-20261014](../) campaign,
which lists the other phases.

## Arms

| Arm | Client | Source |
|---|---|---|
| final pure-Go | `pkg/fdbgo` | `267a41434a81723b07e7150869d702636a1b91a9` |
| before pure-Go | `pkg/fdbgo` before the allocation work (after read-path parity) | `3824f14654edae8431b0085de8ebf83f5a460a08` |
| CGo | Apple's Go binding over `libfdb_c` 7.3.77, built from the final tree | `267a41434…`, native library 7.3.77 |

Executables: `//tools/clientbench/cmd/clientbench:clientbench_go` (per tree) and
`:clientbench_cgo`, harness `d94b4c61377d173b7c6884fde426a227dab0f0bb`
(`tools/clientbench`, `docs/benchmarks/campaign-harness.md`).

## Environment

- Hetzner `ccx43`, fsn1: 16 dedicated vCPUs (AMD EPYC Milan, 8 cores × 2 SMT
  threads, one NUMA node), 64 GiB. Guest CPU groups by sibling pairs:
  housekeeping/runner 0–1, `fdbserver` 2–7, client 8–15. Guest affinity is not
  physical-host isolation.
- One `fdbserver` 7.3.77 process, single redundancy, `ssd-2` engine, API 730.
  The client ran in a network namespace with no default route, reaching the
  server over a private veth pair on the same host. No artificial delay.
- Go 1.26.9, `GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`, empty `GODEBUG`.
- Every run's quiescence gate, CPU-steal/OOM/server-health telemetry and pinned
  input hashes passed. All 11 workload campaigns completed; none was rerun.

## Design

- Per workload: 6 balanced blocks, each running all three arms once in a
  preregistered order (seed 641001; every arm order appears once). One fresh
  process per run: 200 warmup then 3,000 measured transactions, so 18,000
  measured transactions per arm and workload, 594,000 in total.
- Workloads: `grv` (read version only), `get100`/`get1000`/`get10000` (one
  point read of a 100 B/1 kB/10 kB value), `read1000` (point read in a
  read-only transaction), `range10`/`range100` (one range read of 10/100 rows),
  `set100` (one 100 B write and commit), `ryw` (write then read the same key
  in a transaction that is never committed: no server round trip),
  `batchset50`/`batchset50x1000` (50 writes of 100 B/1 kB and one commit).
- Estimands: per-run statistics; the paired effect is the geometric mean of the
  six within-block ratios, with a nominal 95% percentile bootstrap over blocks
  (20,000 resamples, seed 391706). Absolute values are medians of the six
  run-level statistics. No outliers were removed.
- CPU scopes. **Shutdown process CPU (E−A)** is the process's user+system CPU
  from after binary identity hashing to after measurement, finalizer drain,
  database close and native network stop, divided by measured transactions. It
  includes startup, fixture, warmup and validation, so it charges each client
  for work the measured window can hide (deferred GC, native threads). Go
  bytes/mallocs per transaction count the Go heap only; the native `libfdb_c`
  heap is unobserved, so they are not comparable with the CGo arm.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure A.1: Client ratios at 1 worker, final pure-Go / CGo (log₂ scale, 95% CI)</strong> — 6 paired blocks × 3,000 tx/run (18,000 tx/arm/workload). Below 1.0× means Pure-Go is faster / lower CPU.</figcaption>
<div class="bench-summary"><ul class="bench-summary-list"><li><strong>Mean latency (final/CGo):</strong> Pure-Go is faster with 95% CI &lt; 1.000 on 10 of 11 workloads (0.189× on client-only ryw, 0.515× on grv, 0.613×–0.931× on reads/commits); range100 (0.846× [0.713, 1.024]) is unresolved.</li><li><strong>p99 latency (final/CGo):</strong> Lower on 8 of 11 workloads (0.245×–0.942×); range100 (0.861× [0.755, 1.008]), set100 (0.939× [0.845, 1.020]), and batchset50 (0.989× [0.913, 1.126]) are unresolved.</li><li><strong>Shutdown CPU (E−A, final/CGo):</strong> Lower on 7 of 11 workloads (0.228×–0.887×); get100 (0.948× [0.817, 1.118]), get10000 (0.959× [0.884, 1.046]), range10 (0.969× [0.834, 1.122]), and range100 (1.101× [0.968, 1.269]) are unresolved.</li></ul></div>
<svg class="bench-svg" viewBox="0 0 1320 800" role="img" aria-label="Figure A.1: Client ratios at 1 worker, final pure-Go / CGo (log₂ scale, 95% CI)">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="774"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.15×</text>
<text class="axis-text" x="165.0" y="789" text-anchor="middle">0.15×</text>
<line class="grid-line" x1="384.6" y1="36" x2="384.6" y2="774"/>
<text class="axis-text" x="384.6" y="30" text-anchor="middle">0.25×</text>
<text class="axis-text" x="384.6" y="789" text-anchor="middle">0.25×</text>
<line class="grid-line" x1="682.5" y1="36" x2="682.5" y2="774"/>
<text class="axis-text" x="682.5" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="682.5" y="789" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="827.1" y1="36" x2="827.1" y2="774"/>
<text class="axis-text" x="827.1" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="827.1" y="789" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="980.4" y1="36" x2="980.4" y2="774"/>
<text class="axis-text" x="980.4" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="980.4" y="789" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="774"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">1.4×</text>
<text class="axis-text" x="1125.0" y="789" text-anchor="middle">1.4×</text>
<text class="axis-text" x="972.4" y="17" text-anchor="end">← Pure-Go faster / lower CPU</text>
<text class="axis-text" x="988.4" y="17" text-anchor="start">CGo faster / lower CPU →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">Mean latency: final pure-Go / CGo</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.515× [0.484, 0.545]</title><line x1="668.5" y1="77.0" x2="719.5" y2="77.0" stroke-width="2.2"/><line x1="668.5" y1="73.0" x2="668.5" y2="81.0" stroke-width="1.8"/><line x1="719.5" y1="73.0" x2="719.5" y2="81.0" stroke-width="1.8"/><circle cx="695.2" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.515× [0.484, 0.545]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.651× [0.594, 0.717]</title><line x1="756.5" y1="97.0" x2="837.4" y2="97.0" stroke-width="2.2"/><line x1="756.5" y1="93.0" x2="756.5" y2="101.0" stroke-width="1.8"/><line x1="837.4" y1="93.0" x2="837.4" y2="101.0" stroke-width="1.8"/><circle cx="795.9" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.651× [0.594, 0.717]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.637× [0.598, 0.698]</title><line x1="759.4" y1="117.0" x2="825.9" y2="117.0" stroke-width="2.2"/><line x1="759.4" y1="113.0" x2="759.4" y2="121.0" stroke-width="1.8"/><line x1="825.9" y1="113.0" x2="825.9" y2="121.0" stroke-width="1.8"/><circle cx="786.5" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.637× [0.598, 0.698]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.637× [0.585, 0.700]</title><line x1="749.9" y1="137.0" x2="827.1" y2="137.0" stroke-width="2.2"/><line x1="749.9" y1="133.0" x2="749.9" y2="141.0" stroke-width="1.8"/><line x1="827.1" y1="133.0" x2="827.1" y2="141.0" stroke-width="1.8"/><circle cx="786.5" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.637× [0.585, 0.700]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.613× [0.583, 0.647]</title><line x1="748.5" y1="157.0" x2="793.2" y2="157.0" stroke-width="2.2"/><line x1="748.5" y1="153.0" x2="748.5" y2="161.0" stroke-width="1.8"/><line x1="793.2" y1="153.0" x2="793.2" y2="161.0" stroke-width="1.8"/><circle cx="770.0" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1137" y="161.0">0.613× [0.583, 0.647]</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.721× [0.618, 0.842]</title><line x1="773.5" y1="177.0" x2="906.5" y2="177.0" stroke-width="2.2"/><line x1="773.5" y1="173.0" x2="773.5" y2="181.0" stroke-width="1.8"/><line x1="906.5" y1="173.0" x2="906.5" y2="181.0" stroke-width="1.8"/><circle cx="839.8" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1137" y="181.0">0.721× [0.618, 0.842]</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.846× [0.713, 1.024]</title><line x1="835.0" y1="197.0" x2="990.6" y2="197.0" stroke-width="2.2"/><line x1="835.0" y1="193.0" x2="835.0" y2="201.0" stroke-width="1.8"/><line x1="990.6" y1="193.0" x2="990.6" y2="201.0" stroke-width="1.8"/><circle cx="908.5" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1137" y="201.0">0.846× [0.713, 1.024]</text>
<text class="row-label" x="153" y="221.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.897× [0.873, 0.920]</title><line x1="922.0" y1="217.0" x2="944.5" y2="217.0" stroke-width="2.2"/><line x1="922.0" y1="213.0" x2="922.0" y2="221.0" stroke-width="1.8"/><line x1="944.5" y1="213.0" x2="944.5" y2="221.0" stroke-width="1.8"/><circle cx="933.7" cy="217.0" r="3.8"/></g>
<text class="val-text" x="1137" y="221.0">0.897× [0.873, 0.920]</text>
<text class="row-label" x="153" y="241.0" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.189× [0.161, 0.219]</title><line x1="195.4" y1="237.0" x2="327.7" y2="237.0" stroke-width="2.2"/><line x1="195.4" y1="233.0" x2="195.4" y2="241.0" stroke-width="1.8"/><line x1="327.7" y1="233.0" x2="327.7" y2="241.0" stroke-width="1.8"/><circle cx="264.3" cy="237.0" r="3.8"/></g>
<text class="val-text" x="1137" y="241.0">0.189× [0.161, 0.219]</text>
<text class="row-label" x="153" y="261.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.931× [0.904, 0.950]</title><line x1="937.0" y1="257.0" x2="958.3" y2="257.0" stroke-width="2.2"/><line x1="937.0" y1="253.0" x2="937.0" y2="261.0" stroke-width="1.8"/><line x1="958.3" y1="253.0" x2="958.3" y2="261.0" stroke-width="1.8"/><circle cx="949.7" cy="257.0" r="3.8"/></g>
<text class="val-text" x="1137" y="261.0">0.931× [0.904, 0.950]</text>
<text class="row-label" x="153" y="281.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.918× [0.898, 0.937]</title><line x1="934.1" y1="277.0" x2="952.4" y2="277.0" stroke-width="2.2"/><line x1="934.1" y1="273.0" x2="934.1" y2="281.0" stroke-width="1.8"/><line x1="952.4" y1="273.0" x2="952.4" y2="281.0" stroke-width="1.8"/><circle cx="943.6" cy="277.0" r="3.8"/></g>
<text class="val-text" x="1137" y="281.0">0.918× [0.898, 0.937]</text>
<line class="section-divider" x1="8" y1="290" x2="1312" y2="290"/>
<text class="section-title" x="10" y="305">p99 latency: final pure-Go / CGo</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.592× [0.566, 0.625]</title><line x1="735.8" y1="319.0" x2="778.4" y2="319.0" stroke-width="2.2"/><line x1="735.8" y1="315.0" x2="735.8" y2="323.0" stroke-width="1.8"/><line x1="778.4" y1="315.0" x2="778.4" y2="323.0" stroke-width="1.8"/><circle cx="755.1" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">0.592× [0.566, 0.625]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.675× [0.613, 0.743]</title><line x1="770.0" y1="339.0" x2="852.7" y2="339.0" stroke-width="2.2"/><line x1="770.0" y1="335.0" x2="770.0" y2="343.0" stroke-width="1.8"/><line x1="852.7" y1="335.0" x2="852.7" y2="343.0" stroke-width="1.8"/><circle cx="811.5" cy="339.0" r="3.8"/></g>
<text class="val-text" x="1137" y="343.0">0.675× [0.613, 0.743]</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.679× [0.627, 0.743]</title><line x1="779.7" y1="359.0" x2="852.7" y2="359.0" stroke-width="2.2"/><line x1="779.7" y1="355.0" x2="779.7" y2="363.0" stroke-width="1.8"/><line x1="852.7" y1="355.0" x2="852.7" y2="363.0" stroke-width="1.8"/><circle cx="814.0" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1137" y="363.0">0.679× [0.627, 0.743]</text>
<text class="row-label" x="153" y="383.0" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.725× [0.668, 0.776]</title><line x1="807.0" y1="379.0" x2="871.4" y2="379.0" stroke-width="2.2"/><line x1="807.0" y1="375.0" x2="807.0" y2="383.0" stroke-width="1.8"/><line x1="871.4" y1="375.0" x2="871.4" y2="383.0" stroke-width="1.8"/><circle cx="842.2" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1137" y="383.0">0.725× [0.668, 0.776]</text>
<text class="row-label" x="153" y="403.0" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.648× [0.596, 0.701]</title><line x1="758.0" y1="399.0" x2="827.7" y2="399.0" stroke-width="2.2"/><line x1="758.0" y1="395.0" x2="758.0" y2="403.0" stroke-width="1.8"/><line x1="827.7" y1="395.0" x2="827.7" y2="403.0" stroke-width="1.8"/><circle cx="793.9" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1137" y="403.0">0.648× [0.596, 0.701]</text>
<text class="row-label" x="153" y="423.0" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.666× [0.628, 0.701]</title><line x1="780.4" y1="419.0" x2="827.7" y2="419.0" stroke-width="2.2"/><line x1="780.4" y1="415.0" x2="780.4" y2="423.0" stroke-width="1.8"/><line x1="827.7" y1="415.0" x2="827.7" y2="423.0" stroke-width="1.8"/><circle cx="805.7" cy="419.0" r="3.8"/></g>
<text class="val-text" x="1137" y="423.0">0.666× [0.628, 0.701]</text>
<text class="row-label" x="153" y="443.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.861× [0.755, 1.008]</title><line x1="859.6" y1="439.0" x2="983.8" y2="439.0" stroke-width="2.2"/><line x1="859.6" y1="435.0" x2="859.6" y2="443.0" stroke-width="1.8"/><line x1="983.8" y1="435.0" x2="983.8" y2="443.0" stroke-width="1.8"/><circle cx="916.1" cy="439.0" r="3.8"/></g>
<text class="val-text" x="1137" y="443.0">0.861× [0.755, 1.008]</text>
<text class="row-label" x="153" y="463.0" text-anchor="end">set100</text>
<g class="ci-neutral"><title>set100: 0.939× [0.845, 1.020]</title><line x1="908.0" y1="459.0" x2="988.9" y2="459.0" stroke-width="2.2"/><line x1="908.0" y1="455.0" x2="908.0" y2="463.0" stroke-width="1.8"/><line x1="988.9" y1="455.0" x2="988.9" y2="463.0" stroke-width="1.8"/><circle cx="953.3" cy="459.0" r="3.8"/></g>
<text class="val-text" x="1137" y="463.0">0.939× [0.845, 1.020]</text>
<text class="row-label" x="153" y="483.0" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.245× [0.214, 0.284]</title><line x1="317.7" y1="479.0" x2="439.4" y2="479.0" stroke-width="2.2"/><line x1="317.7" y1="475.0" x2="317.7" y2="483.0" stroke-width="1.8"/><line x1="439.4" y1="475.0" x2="439.4" y2="483.0" stroke-width="1.8"/><circle cx="375.9" cy="479.0" r="3.8"/></g>
<text class="val-text" x="1137" y="483.0">0.245× [0.214, 0.284]</text>
<text class="row-label" x="153" y="503.0" text-anchor="end">batchset50</text>
<g class="ci-neutral"><title>batchset50: 0.989× [0.913, 1.126]</title><line x1="941.3" y1="499.0" x2="1031.4" y2="499.0" stroke-width="2.2"/><line x1="941.3" y1="495.0" x2="941.3" y2="503.0" stroke-width="1.8"/><line x1="1031.4" y1="495.0" x2="1031.4" y2="503.0" stroke-width="1.8"/><circle cx="975.6" cy="499.0" r="3.8"/></g>
<text class="val-text" x="1137" y="503.0">0.989× [0.913, 1.126]</text>
<text class="row-label" x="153" y="523.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.942× [0.909, 0.979]</title><line x1="939.4" y1="519.0" x2="971.3" y2="519.0" stroke-width="2.2"/><line x1="939.4" y1="515.0" x2="939.4" y2="523.0" stroke-width="1.8"/><line x1="971.3" y1="515.0" x2="971.3" y2="523.0" stroke-width="1.8"/><circle cx="954.7" cy="519.0" r="3.8"/></g>
<text class="val-text" x="1137" y="523.0">0.942× [0.909, 0.979]</text>
<line class="section-divider" x1="8" y1="532" x2="1312" y2="532"/>
<text class="section-title" x="10" y="547">Shutdown process CPU (E−A): final pure-Go / CGo</text>
<text class="row-label" x="153" y="565.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.659× [0.621, 0.692]</title><line x1="775.6" y1="561.0" x2="822.1" y2="561.0" stroke-width="2.2"/><line x1="775.6" y1="557.0" x2="775.6" y2="565.0" stroke-width="1.8"/><line x1="822.1" y1="557.0" x2="822.1" y2="565.0" stroke-width="1.8"/><circle cx="801.1" cy="561.0" r="3.8"/></g>
<text class="val-text" x="1137" y="565.0">0.659× [0.621, 0.692]</text>
<text class="row-label" x="153" y="585.0" text-anchor="end">get100</text>
<g class="ci-neutral"><title>get100: 0.948× [0.817, 1.118]</title><line x1="893.5" y1="581.0" x2="1028.3" y2="581.0" stroke-width="2.2"/><line x1="893.5" y1="577.0" x2="893.5" y2="585.0" stroke-width="1.8"/><line x1="1028.3" y1="577.0" x2="1028.3" y2="585.0" stroke-width="1.8"/><circle cx="957.4" cy="581.0" r="3.8"/></g>
<text class="val-text" x="1137" y="585.0">0.948× [0.817, 1.118]</text>
<text class="row-label" x="153" y="605.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.858× [0.810, 0.925]</title><line x1="889.8" y1="601.0" x2="946.9" y2="601.0" stroke-width="2.2"/><line x1="889.8" y1="597.0" x2="889.8" y2="605.0" stroke-width="1.8"/><line x1="946.9" y1="597.0" x2="946.9" y2="605.0" stroke-width="1.8"/><circle cx="914.6" cy="601.0" r="3.8"/></g>
<text class="val-text" x="1137" y="605.0">0.858× [0.810, 0.925]</text>
<text class="row-label" x="153" y="625.0" text-anchor="end">get10000</text>
<g class="ci-neutral"><title>get10000: 0.959× [0.884, 1.046]</title><line x1="927.4" y1="621.0" x2="999.7" y2="621.0" stroke-width="2.2"/><line x1="927.4" y1="617.0" x2="927.4" y2="625.0" stroke-width="1.8"/><line x1="999.7" y1="617.0" x2="999.7" y2="625.0" stroke-width="1.8"/><circle cx="962.4" cy="621.0" r="3.8"/></g>
<text class="val-text" x="1137" y="625.0">0.959× [0.884, 1.046]</text>
<text class="row-label" x="153" y="645.0" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.775× [0.735, 0.815]</title><line x1="848.1" y1="641.0" x2="892.5" y2="641.0" stroke-width="2.2"/><line x1="848.1" y1="637.0" x2="848.1" y2="645.0" stroke-width="1.8"/><line x1="892.5" y1="637.0" x2="892.5" y2="645.0" stroke-width="1.8"/><circle cx="870.8" cy="641.0" r="3.8"/></g>
<text class="val-text" x="1137" y="645.0">0.775× [0.735, 0.815]</text>
<text class="row-label" x="153" y="665.0" text-anchor="end">range10</text>
<g class="ci-neutral"><title>range10: 0.969× [0.834, 1.122]</title><line x1="902.4" y1="661.0" x2="1029.9" y2="661.0" stroke-width="2.2"/><line x1="902.4" y1="657.0" x2="902.4" y2="665.0" stroke-width="1.8"/><line x1="1029.9" y1="657.0" x2="1029.9" y2="665.0" stroke-width="1.8"/><circle cx="966.8" cy="661.0" r="3.8"/></g>
<text class="val-text" x="1137" y="665.0">0.969× [0.834, 1.122]</text>
<text class="row-label" x="153" y="685.0" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 1.101× [0.968, 1.269]</title><line x1="966.4" y1="681.0" x2="1082.8" y2="681.0" stroke-width="2.2"/><line x1="966.4" y1="677.0" x2="966.4" y2="685.0" stroke-width="1.8"/><line x1="1082.8" y1="677.0" x2="1082.8" y2="685.0" stroke-width="1.8"/><circle cx="1021.7" cy="681.0" r="3.8"/></g>
<text class="val-text" x="1137" y="685.0">1.101× [0.968, 1.269]</text>
<text class="row-label" x="153" y="705.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.869× [0.841, 0.899]</title><line x1="906.0" y1="701.0" x2="934.6" y2="701.0" stroke-width="2.2"/><line x1="906.0" y1="697.0" x2="906.0" y2="705.0" stroke-width="1.8"/><line x1="934.6" y1="697.0" x2="934.6" y2="705.0" stroke-width="1.8"/><circle cx="920.0" cy="701.0" r="3.8"/></g>
<text class="val-text" x="1137" y="705.0">0.869× [0.841, 0.899]</text>
<text class="row-label" x="153" y="725.0" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.228× [0.201, 0.256]</title><line x1="290.8" y1="721.0" x2="394.7" y2="721.0" stroke-width="2.2"/><line x1="290.8" y1="717.0" x2="290.8" y2="725.0" stroke-width="1.8"/><line x1="394.7" y1="717.0" x2="394.7" y2="725.0" stroke-width="1.8"/><circle cx="345.0" cy="721.0" r="3.8"/></g>
<text class="val-text" x="1137" y="725.0">0.228× [0.201, 0.256]</text>
<text class="row-label" x="153" y="745.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.863× [0.854, 0.873]</title><line x1="912.6" y1="741.0" x2="922.0" y2="741.0" stroke-width="2.2"/><line x1="912.6" y1="737.0" x2="912.6" y2="745.0" stroke-width="1.8"/><line x1="922.0" y1="737.0" x2="922.0" y2="745.0" stroke-width="1.8"/><circle cx="917.1" cy="741.0" r="3.8"/></g>
<text class="val-text" x="1137" y="745.0">0.863× [0.854, 0.873]</text>
<text class="row-label" x="153" y="765.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.887× [0.864, 0.922]</title><line x1="917.6" y1="761.0" x2="945.5" y2="761.0" stroke-width="2.2"/><line x1="917.6" y1="757.0" x2="917.6" y2="765.0" stroke-width="1.8"/><line x1="945.5" y1="757.0" x2="945.5" y2="765.0" stroke-width="1.8"/><circle cx="928.8" cy="761.0" r="3.8"/></g>
<text class="val-text" x="1137" y="765.0">0.887× [0.864, 0.922]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure A.2: Allocation ratios, final (267a4143) / before (3824f146) pure-Go (log₂ scale, 95% CI)</strong> — Go heap bytes and mallocs per transaction (6 paired blocks). Below 1.0× means fewer allocations in Final.</figcaption>
<div class="bench-summary"><ul class="bench-summary-list"><li><strong>Allocations (final/before):</strong> Go mallocs/tx drop across all 11 workloads (0.797×–0.973×, largest on range10 0.797× and range100 0.798×) and Go bytes/tx drop across all 11 (0.824×–0.998×, largest on range100 0.824× and range10 0.844×).</li><li><strong>Latency &amp; CPU (final/before):</strong> All 11 mean-latency 95% CIs span 1.000 (0.984×–1.042×, unresolved); only range100 resolves lower p99 (0.951× [0.916, 0.998]) and lower E−A CPU (0.952× [0.937, 0.971]).</li></ul></div>
<svg class="bench-svg" viewBox="0 0 1320 558" role="img" aria-label="Figure A.2: Allocation ratios, final (267a4143) / before (3824f146) pure-Go (log₂ scale, 95% CI)">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="532"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="165.0" y="547" text-anchor="middle">0.7×</text>
<line class="grid-line" x1="448.6" y1="36" x2="448.6" y2="532"/>
<text class="axis-text" x="448.6" y="30" text-anchor="middle">0.8×</text>
<text class="axis-text" x="448.6" y="547" text-anchor="middle">0.8×</text>
<line class="grid-line" x1="698.8" y1="36" x2="698.8" y2="532"/>
<text class="axis-text" x="698.8" y="30" text-anchor="middle">0.9×</text>
<text class="axis-text" x="698.8" y="547" text-anchor="middle">0.9×</text>
<line class="parity-line" x1="922.6" y1="36" x2="922.6" y2="532"/>
<text class="axis-text" x="922.6" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="922.6" y="547" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="532"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">1.1×</text>
<text class="axis-text" x="1125.0" y="547" text-anchor="middle">1.1×</text>
<text class="axis-text" x="914.6" y="17" text-anchor="end">← Final allocates less</text>
<text class="axis-text" x="930.6" y="17" text-anchor="start">Before allocates less →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">Go bytes/tx: final / before</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.993× [0.992, 0.994]</title><line x1="905.5" y1="77.0" x2="909.8" y2="77.0" stroke-width="2.2"/><line x1="905.5" y1="73.0" x2="905.5" y2="81.0" stroke-width="1.8"/><line x1="909.8" y1="73.0" x2="909.8" y2="81.0" stroke-width="1.8"/><circle cx="907.6" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.993× [0.992, 0.994]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.929× [0.928, 0.930]</title><line x1="763.9" y1="97.0" x2="768.4" y2="97.0" stroke-width="2.2"/><line x1="763.9" y1="93.0" x2="763.9" y2="101.0" stroke-width="1.8"/><line x1="768.4" y1="93.0" x2="768.4" y2="101.0" stroke-width="1.8"/><circle cx="766.1" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">0.929× [0.928, 0.930]</text>
<text class="row-label" x="153" y="121.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.944× [0.943, 0.944]</title><line x1="797.9" y1="117.0" x2="800.2" y2="117.0" stroke-width="2.2"/><line x1="797.9" y1="113.0" x2="797.9" y2="121.0" stroke-width="1.8"/><line x1="800.2" y1="113.0" x2="800.2" y2="121.0" stroke-width="1.8"/><circle cx="800.2" cy="117.0" r="3.8"/></g>
<text class="val-text" x="1137" y="121.0">0.944× [0.943, 0.944]</text>
<text class="row-label" x="153" y="141.0" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.982× [0.982, 0.983]</title><line x1="884.0" y1="137.0" x2="886.1" y2="137.0" stroke-width="2.2"/><line x1="884.0" y1="133.0" x2="884.0" y2="141.0" stroke-width="1.8"/><line x1="886.1" y1="133.0" x2="886.1" y2="141.0" stroke-width="1.8"/><circle cx="884.0" cy="137.0" r="3.8"/></g>
<text class="val-text" x="1137" y="141.0">0.982× [0.982, 0.983]</text>
<text class="row-label" x="153" y="161.0" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.997× [0.996, 0.998]</title><line x1="914.1" y1="157.0" x2="918.3" y2="157.0" stroke-width="2.2"/><line x1="914.1" y1="153.0" x2="914.1" y2="161.0" stroke-width="1.8"/><line x1="918.3" y1="153.0" x2="918.3" y2="161.0" stroke-width="1.8"/><circle cx="916.2" cy="157.0" r="3.8"/></g>
<text class="val-text" x="1137" y="161.0">0.997× [0.996, 0.998]</text>
<text class="row-label" x="153" y="181.0" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.844× [0.844, 0.845]</title><line x1="562.3" y1="177.0" x2="564.8" y2="177.0" stroke-width="2.2"/><line x1="562.3" y1="173.0" x2="562.3" y2="181.0" stroke-width="1.8"/><line x1="564.8" y1="173.0" x2="564.8" y2="181.0" stroke-width="1.8"/><circle cx="562.3" cy="177.0" r="3.8"/></g>
<text class="val-text" x="1137" y="181.0">0.844× [0.844, 0.845]</text>
<text class="row-label" x="153" y="201.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.824× [0.824, 0.825]</title><line x1="511.4" y1="197.0" x2="514.0" y2="197.0" stroke-width="2.2"/><line x1="511.4" y1="193.0" x2="511.4" y2="201.0" stroke-width="1.8"/><line x1="514.0" y1="193.0" x2="514.0" y2="201.0" stroke-width="1.8"/><circle cx="511.4" cy="197.0" r="3.8"/></g>
<text class="val-text" x="1137" y="201.0">0.824× [0.824, 0.825]</text>
<text class="row-label" x="153" y="221.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.956× [0.954, 0.957]</title><line x1="822.5" y1="217.0" x2="829.2" y2="217.0" stroke-width="2.2"/><line x1="822.5" y1="213.0" x2="822.5" y2="221.0" stroke-width="1.8"/><line x1="829.2" y1="213.0" x2="829.2" y2="221.0" stroke-width="1.8"/><circle cx="827.0" cy="217.0" r="3.8"/></g>
<text class="val-text" x="1137" y="221.0">0.956× [0.954, 0.957]</text>
<text class="row-label" x="153" y="241.0" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.995× [0.995, 0.995]</title><line x1="911.9" y1="237.0" x2="911.9" y2="237.0" stroke-width="2.2"/><line x1="911.9" y1="233.0" x2="911.9" y2="241.0" stroke-width="1.8"/><line x1="911.9" y1="233.0" x2="911.9" y2="241.0" stroke-width="1.8"/><circle cx="911.9" cy="237.0" r="3.8"/></g>
<text class="val-text" x="1137" y="241.0">0.995× [0.995, 0.995]</text>
<text class="row-label" x="153" y="261.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.993× [0.992, 0.994]</title><line x1="905.5" y1="257.0" x2="909.8" y2="257.0" stroke-width="2.2"/><line x1="905.5" y1="253.0" x2="905.5" y2="261.0" stroke-width="1.8"/><line x1="909.8" y1="253.0" x2="909.8" y2="261.0" stroke-width="1.8"/><circle cx="907.6" cy="257.0" r="3.8"/></g>
<text class="val-text" x="1137" y="261.0">0.993× [0.992, 0.994]</text>
<text class="row-label" x="153" y="281.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.998× [0.998, 0.998]</title><line x1="918.3" y1="277.0" x2="918.3" y2="277.0" stroke-width="2.2"/><line x1="918.3" y1="273.0" x2="918.3" y2="281.0" stroke-width="1.8"/><line x1="918.3" y1="273.0" x2="918.3" y2="281.0" stroke-width="1.8"/><circle cx="918.3" cy="277.0" r="3.8"/></g>
<text class="val-text" x="1137" y="281.0">0.998× [0.998, 0.998]</text>
<line class="section-divider" x1="8" y1="290" x2="1312" y2="290"/>
<text class="section-title" x="10" y="305">Go mallocs/tx: final / before</text>
<text class="row-label" x="153" y="323.0" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.950× [0.950, 0.950]</title><line x1="813.6" y1="319.0" x2="813.6" y2="319.0" stroke-width="2.2"/><line x1="813.6" y1="315.0" x2="813.6" y2="323.0" stroke-width="1.8"/><line x1="813.6" y1="315.0" x2="813.6" y2="323.0" stroke-width="1.8"/><circle cx="813.6" cy="319.0" r="3.8"/></g>
<text class="val-text" x="1137" y="323.0">0.950× [0.950, 0.950]</text>
<text class="row-label" x="153" y="343.0" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.874× [0.873, 0.874]</title><line x1="634.1" y1="339.0" x2="636.5" y2="339.0" stroke-width="2.2"/><line x1="634.1" y1="335.0" x2="634.1" y2="343.0" stroke-width="1.8"/><line x1="636.5" y1="335.0" x2="636.5" y2="343.0" stroke-width="1.8"/><circle cx="636.5" cy="339.0" r="3.8"/></g>
<text class="val-text" x="1137" y="343.0">0.874× [0.873, 0.874]</text>
<text class="row-label" x="153" y="363.0" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.874× [0.873, 0.874]</title><line x1="634.1" y1="359.0" x2="636.5" y2="359.0" stroke-width="2.2"/><line x1="634.1" y1="355.0" x2="634.1" y2="363.0" stroke-width="1.8"/><line x1="636.5" y1="355.0" x2="636.5" y2="363.0" stroke-width="1.8"/><circle cx="636.5" cy="359.0" r="3.8"/></g>
<text class="val-text" x="1137" y="363.0">0.874× [0.873, 0.874]</text>
<text class="row-label" x="153" y="383.0" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.874× [0.874, 0.875]</title><line x1="636.5" y1="379.0" x2="638.9" y2="379.0" stroke-width="2.2"/><line x1="636.5" y1="375.0" x2="636.5" y2="383.0" stroke-width="1.8"/><line x1="638.9" y1="375.0" x2="638.9" y2="383.0" stroke-width="1.8"/><circle cx="636.5" cy="379.0" r="3.8"/></g>
<text class="val-text" x="1137" y="383.0">0.874× [0.874, 0.875]</text>
<text class="row-label" x="153" y="403.0" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.947× [0.947, 0.948]</title><line x1="806.9" y1="399.0" x2="809.1" y2="399.0" stroke-width="2.2"/><line x1="806.9" y1="395.0" x2="806.9" y2="403.0" stroke-width="1.8"/><line x1="809.1" y1="395.0" x2="809.1" y2="403.0" stroke-width="1.8"/><circle cx="806.9" cy="399.0" r="3.8"/></g>
<text class="val-text" x="1137" y="403.0">0.947× [0.947, 0.948]</text>
<text class="row-label" x="153" y="423.0" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.797× [0.796, 0.797]</title><line x1="438.0" y1="419.0" x2="440.6" y2="419.0" stroke-width="2.2"/><line x1="438.0" y1="415.0" x2="438.0" y2="423.0" stroke-width="1.8"/><line x1="440.6" y1="415.0" x2="440.6" y2="423.0" stroke-width="1.8"/><circle cx="440.6" cy="419.0" r="3.8"/></g>
<text class="val-text" x="1137" y="423.0">0.797× [0.796, 0.797]</text>
<text class="row-label" x="153" y="443.0" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.798× [0.798, 0.798]</title><line x1="443.3" y1="439.0" x2="443.3" y2="439.0" stroke-width="2.2"/><line x1="443.3" y1="435.0" x2="443.3" y2="443.0" stroke-width="1.8"/><line x1="443.3" y1="435.0" x2="443.3" y2="443.0" stroke-width="1.8"/><circle cx="443.3" cy="439.0" r="3.8"/></g>
<text class="val-text" x="1137" y="443.0">0.798× [0.798, 0.798]</text>
<text class="row-label" x="153" y="463.0" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.919× [0.918, 0.919]</title><line x1="740.8" y1="459.0" x2="743.2" y2="459.0" stroke-width="2.2"/><line x1="740.8" y1="455.0" x2="740.8" y2="463.0" stroke-width="1.8"/><line x1="743.2" y1="455.0" x2="743.2" y2="463.0" stroke-width="1.8"/><circle cx="743.2" cy="459.0" r="3.8"/></g>
<text class="val-text" x="1137" y="463.0">0.919× [0.918, 0.919]</text>
<text class="row-label" x="153" y="483.0" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.950× [0.950, 0.950]</title><line x1="813.6" y1="479.0" x2="813.6" y2="479.0" stroke-width="2.2"/><line x1="813.6" y1="475.0" x2="813.6" y2="483.0" stroke-width="1.8"/><line x1="813.6" y1="475.0" x2="813.6" y2="483.0" stroke-width="1.8"/><circle cx="813.6" cy="479.0" r="3.8"/></g>
<text class="val-text" x="1137" y="483.0">0.950× [0.950, 0.950]</text>
<text class="row-label" x="153" y="503.0" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.973× [0.973, 0.973]</title><line x1="864.4" y1="499.0" x2="864.4" y2="499.0" stroke-width="2.2"/><line x1="864.4" y1="495.0" x2="864.4" y2="503.0" stroke-width="1.8"/><line x1="864.4" y1="495.0" x2="864.4" y2="503.0" stroke-width="1.8"/><circle cx="864.4" cy="499.0" r="3.8"/></g>
<text class="val-text" x="1137" y="503.0">0.973× [0.973, 0.973]</text>
<text class="row-label" x="153" y="523.0" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.973× [0.973, 0.973]</title><line x1="864.4" y1="519.0" x2="864.4" y2="519.0" stroke-width="2.2"/><line x1="864.4" y1="515.0" x2="864.4" y2="523.0" stroke-width="1.8"/><line x1="864.4" y1="515.0" x2="864.4" y2="523.0" stroke-width="1.8"/><circle cx="864.4" cy="519.0" r="3.8"/></g>
<text class="val-text" x="1137" y="523.0">0.973× [0.973, 0.973]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

### Final pure-Go vs CGo

Ratio final/CGo (below 1 means pure-Go is faster or cheaper), nominal 95% interval.

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx |
|---|---:|---:|---:|
| grv | 0.515 [0.484, 0.545] | 0.592 [0.566, 0.625] | 0.659 [0.621, 0.692] |
| get100 | 0.651 [0.594, 0.717] | 0.675 [0.613, 0.743] | 0.948 [0.817, 1.118] |
| get1000 | 0.637 [0.598, 0.698] | 0.679 [0.627, 0.743] | 0.858 [0.810, 0.925] |
| get10000 | 0.637 [0.585, 0.700] | 0.725 [0.668, 0.776] | 0.959 [0.884, 1.046] |
| read1000 | 0.613 [0.583, 0.647] | 0.648 [0.596, 0.701] | 0.775 [0.735, 0.815] |
| range10 | 0.721 [0.618, 0.842] | 0.666 [0.628, 0.701] | 0.969 [0.834, 1.122] |
| range100 | 0.846 [0.713, 1.024] | 0.861 [0.755, 1.008] | 1.101 [0.968, 1.269] |
| set100 | 0.897 [0.873, 0.920] | 0.939 [0.845, 1.020] | 0.869 [0.841, 0.899] |
| ryw | 0.189 [0.161, 0.219] | 0.245 [0.214, 0.284] | 0.228 [0.201, 0.256] |
| batchset50 | 0.931 [0.904, 0.950] | 0.989 [0.913, 1.126] | 0.863 [0.854, 0.873] |
| batchset50x1000 | 0.918 [0.898, 0.937] | 0.942 [0.909, 0.979] | 0.887 [0.864, 0.922] |

### Final vs before pure-Go: the allocation work

Ratio final/before.

| Workload | Mean latency | p99 latency | Shutdown process CPU (E−A) per tx | Go bytes/tx | Go mallocs/tx |
|---|---:|---:|---:|---:|---:|
| grv | 1.025 [0.970, 1.079] | 1.025 [0.978, 1.079] | 1.027 [0.981, 1.063] | 0.993 [0.992, 0.994] | 0.950 [0.950, 0.950] |
| get100 | 1.021 [0.973, 1.093] | 0.960 [0.896, 1.028] | 1.002 [0.975, 1.038] | 0.929 [0.928, 0.930] | 0.874 [0.873, 0.874] |
| get1000 | 1.017 [0.989, 1.047] | 1.015 [0.922, 1.096] | 1.000 [0.976, 1.026] | 0.944 [0.943, 0.944] | 0.874 [0.873, 0.874] |
| get10000 | 0.984 [0.918, 1.055] | 0.977 [0.931, 1.023] | 1.004 [0.986, 1.022] | 0.982 [0.982, 0.983] | 0.874 [0.874, 0.875] |
| read1000 | 1.042 [0.998, 1.087] | 1.032 [0.932, 1.143] | 1.007 [0.956, 1.050] | 0.997 [0.996, 0.998] | 0.947 [0.947, 0.948] |
| range10 | 0.987 [0.915, 1.081] | 0.953 [0.898, 1.018] | 0.978 [0.939, 1.022] | 0.844 [0.844, 0.845] | 0.797 [0.796, 0.797] |
| range100 | 1.039 [0.977, 1.158] | 0.951 [0.916, 0.998] | 0.952 [0.937, 0.971] | 0.824 [0.824, 0.825] | 0.798 [0.798, 0.798] |
| set100 | 1.019 [0.995, 1.037] | 0.994 [0.881, 1.131] | 1.009 [0.987, 1.028] | 0.956 [0.954, 0.957] | 0.919 [0.918, 0.919] |
| ryw | 1.021 [0.967, 1.082] | 1.016 [0.926, 1.097] | 1.011 [0.947, 1.076] | 0.995 [0.995, 0.995] | 0.950 [0.950, 0.950] |
| batchset50 | 1.000 [0.980, 1.019] | 1.012 [0.955, 1.083] | 0.990 [0.969, 1.016] | 0.993 [0.992, 0.994] | 0.973 [0.973, 0.973] |
| batchset50x1000 | 1.011 [0.991, 1.029] | 0.992 [0.961, 1.025] | 1.002 [0.981, 1.024] | 0.998 [0.998, 0.998] | 0.973 [0.973, 0.973] |

### Absolute values

Medians of six run-level statistics per arm.

| Workload | Arm | Transactions | Mean µs | p50 µs | p99 µs | Shutdown CPU (E−A) µs/tx | Go bytes/tx | Go mallocs/tx |
|---|---|---:|---:|---:|---:|---:|---:|---:|
| grv | final pure-Go | 18000 | 217.5 | 206.3 | 354.0 | 224.9 | 3856 | 38.1 |
| grv | before pure-Go | 18000 | 210.1 | 200.2 | 333.5 | 218.9 | 3880 | 40.1 |
| grv | CGo | 18000 | 422.6 | 410.3 | 590.9 | 338.9 | 424 | 10.0 |
| get100 | final pure-Go | 18000 | 375.5 | 368.2 | 548.2 | 424.6 | 6382 | 69.2 |
| get100 | before pure-Go | 18000 | 370.2 | 361.5 | 569.6 | 424.7 | 6873 | 79.2 |
| get100 | CGo | 18000 | 594.2 | 581.7 | 856.5 | 481.5 | 664 | 16.0 |
| get1000 | final pure-Go | 18000 | 401.8 | 391.7 | 633.0 | 455.5 | 8254 | 69.2 |
| get1000 | before pure-Go | 18000 | 394.9 | 388.2 | 619.7 | 447.8 | 8747 | 79.2 |
| get1000 | CGo | 18000 | 652.1 | 650.7 | 937.6 | 544.2 | 1583 | 16.0 |
| get10000 | final pure-Go | 18000 | 433.6 | 425.6 | 820.4 | 520.6 | 26701 | 69.5 |
| get10000 | before pure-Go | 18000 | 428.4 | 416.6 | 832.5 | 515.6 | 27177 | 79.4 |
| get10000 | CGo | 18000 | 682.4 | 676.6 | 1142.6 | 551.7 | 10796 | 16.0 |
| read1000 | final pure-Go | 18000 | 366.6 | 356.8 | 565.1 | 357.6 | 7805 | 54.1 |
| read1000 | before pure-Go | 18000 | 354.7 | 345.1 | 531.3 | 346.9 | 7832 | 57.1 |
| read1000 | CGo | 18000 | 596.0 | 583.4 | 819.7 | 460.3 | 1543 | 13.0 |
| range10 | final pure-Go | 18000 | 435.6 | 437.8 | 643.3 | 474.6 | 10503 | 74.3 |
| range10 | before pure-Go | 18000 | 456.2 | 449.9 | 682.0 | 490.5 | 12444 | 93.2 |
| range10 | CGo | 18000 | 635.5 | 629.5 | 993.9 | 502.1 | 3169 | 44.0 |
| range100 | final pure-Go | 18000 | 557.7 | 536.0 | 1103.4 | 629.5 | 50356 | 74.8 |
| range100 | before pure-Go | 18000 | 560.2 | 535.6 | 1160.8 | 653.4 | 61101 | 93.8 |
| range100 | CGo | 18000 | 729.3 | 790.6 | 1362.7 | 577.7 | 25675 | 224.0 |
| set100 | final pure-Go | 18000 | 1920.2 | 1883.1 | 2844.0 | 566.8 | 10294 | 101.3 |
| set100 | before pure-Go | 18000 | 1876.4 | 1862.5 | 2863.0 | 556.2 | 10775 | 110.2 |
| set100 | CGo | 18000 | 2140.3 | 2099.9 | 3068.7 | 661.0 | 440 | 10.0 |
| ryw | final pure-Go | 18000 | 9.7 | 5.9 | 51.6 | 23.5 | 4564 | 38.0 |
| ryw | before pure-Go | 18000 | 9.7 | 5.9 | 52.1 | 23.6 | 4587 | 40.0 |
| ryw | CGo | 18000 | 49.4 | 32.4 | 213.7 | 100.6 | 648 | 14.0 |
| batchset50 | final pure-Go | 18000 | 2368.3 | 2365.7 | 4008.6 | 881.6 | 65691 | 321.3 |
| batchset50 | before pure-Go | 18000 | 2349.1 | 2354.7 | 4181.9 | 892.8 | 66160 | 330.3 |
| batchset50 | CGo | 18000 | 2541.1 | 2575.7 | 4358.0 | 1018.1 | 1616 | 59.0 |
| batchset50x1000 | final pure-Go | 18000 | 2726.1 | 2662.7 | 4880.5 | 1278.7 | 209165 | 322.2 |
| batchset50x1000 | before pure-Go | 18000 | 2702.7 | 2626.5 | 4797.4 | 1275.9 | 209549 | 331.2 |
| batchset50x1000 | CGo | 18000 | 2974.0 | 2902.0 | 5141.6 | 1452.9 | 1616 | 59.0 |

## Interpretation

What the data shows:

- **Pure-Go was faster than CGo on every workload's mean latency**, with
  intervals below 1 everywhere except `range100` (0.846, interval reaching
  1.024). The gap is largest where the client's own work dominates:
  - `ryw` (0.189): no server round trip at all. It measures transaction
    creation, a buffered write, a read served from the read-your-writes cache,
    and cancellation. Pure-Go took 9.7 µs mean, CGo 49.4 µs.
  - `grv` (0.515): one round trip to the GRV proxy, 217 µs vs 423 µs.
  - Point reads (0.613–0.651): one GRV plus one storage read.
- **The absolute saving is about the same everywhere a round trip occurs, so
  the ratio shrinks as transactions get longer.** CGo's mean minus pure-Go's
  was 0.17–0.25 ms on every workload except `ryw` (`grv` 0.21 ms, `set100`
  0.22 ms, `batchset50x1000` 0.25 ms). Commits take 1.9–3.0 ms in total, so
  the same saving gives ratios of 0.897–0.931 there.
- **Process CPU through shutdown (E−A) favored pure-Go** on `grv` (0.659),
  `read1000` (0.775), `get1000` (0.858), the commits (0.863–0.887) and `ryw`
  (0.228). For `get100`, `get10000`, `range10` and `range100` the intervals
  include 1; `range100`'s point estimate is above 1 (1.101, interval
  0.968–1.269).
- **The allocation work cut Go allocations** (final vs before): mallocs per
  transaction fell 3–20% on every workload, and bytes up to 18% (`range100`
  0.824, `range10` 0.844). Latency did not change measurably: every
  mean-latency interval includes 1. CPU through shutdown was lower only for
  `range100` (0.952, interval 0.937–0.971).

What the design suggests but did not isolate (hypotheses, not measured
causes):

- The CGo arm's extra latency is consistent with its per-call costs: each
  operation crosses the cgo boundary, and each result is delivered by
  `libfdb_c`'s network thread through a future callback to a waiting
  goroutine. The pure-Go client issues and completes requests on goroutines
  over Go's network poller with no thread handoff. `ryw` is the cleanest view
  of this, since no network is involved; no profile attributed the time.
- Pure-Go's narrower margin and higher CPU on `range100` are consistent with
  materializing 100 key-value slices on the Go heap per read (about 50 kB and
  75 mallocs per transaction), which the C++ client keeps in its arena. Go GC
  work is charged to E−A. This was not profiled.

## Limits

- **One host, no physical network.** Server and client shared a host and
  talked over a veth pair, not a NIC, switch and real network path. A remote
  deployment adds network time that both clients pay alike, so it would most
  likely shrink these ratios toward 1; they are not a prediction for that case.
  The multi-node VPC design (`tools/benchcampaign/multinode/`) measures it.
- **One worker, one process.** Batching of read versions and commits,
  load-balancing across replicas, and cgo thread contention appear only under
  concurrency, which phase A does not test.
- **One server process, single redundancy.** No proxy/log/storage split, no
  replication, no cross-shard reads.
- **Scopes.** Latency is closed-loop per transaction, including retries (none
  occurred). E−A CPU includes startup and warmup; the Go heap figures exclude
  native memory. Intervals are nominal and per workload, with no correction for
  multiple comparisons and no aggregate across workloads.

## Reproduction

The harness, plan sealing and adapter are in `tools/clientbench` and
`tools/benchcampaign/singlenode`; `docs/benchmarks/campaign-harness.md`
describes the benchmark sources. The raw per-run artifacts (plans, schedules,
per-transaction samples, telemetry) and the analysis outputs are retained
outside the repository with the campaign's provenance receipts.
