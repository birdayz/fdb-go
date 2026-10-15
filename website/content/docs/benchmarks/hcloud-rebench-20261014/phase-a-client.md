---
title: "Phase A: Client (1 Worker)"
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
<figcaption><strong>Figure A.1: Single-Node 1-Worker Client Ratios — Final Pure-Go / CGo (log₂ scale, 95% CI)</strong> — 6 paired blocks × 3,000 tx/run (18,000 tx/arm/workload). Below 1.0× means Pure-Go is faster / lower CPU.</figcaption>
<svg class="bench-svg" viewBox="0 0 890 940" role="img" aria-label="Figure A.1: Single-Node 1-Worker Client Ratios — Final Pure-Go / CGo (log₂ scale, 95% CI)">
<line class="grid-line" x1="230.0" y1="28" x2="230.0" y2="910"/>
<text class="axis-text" x="230.0" y="22" text-anchor="middle">0.15×</text>
<text class="axis-text" x="230.0" y="925" text-anchor="middle">0.15×</text>
<line class="grid-line" x1="339.8" y1="28" x2="339.8" y2="910"/>
<text class="axis-text" x="339.8" y="22" text-anchor="middle">0.25×</text>
<text class="axis-text" x="339.8" y="925" text-anchor="middle">0.25×</text>
<line class="grid-line" x1="488.7" y1="28" x2="488.7" y2="910"/>
<text class="axis-text" x="488.7" y="22" text-anchor="middle">0.5×</text>
<text class="axis-text" x="488.7" y="925" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="561.0" y1="28" x2="561.0" y2="910"/>
<text class="axis-text" x="561.0" y="22" text-anchor="middle">0.7×</text>
<text class="axis-text" x="561.0" y="925" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="637.7" y1="28" x2="637.7" y2="910"/>
<text class="axis-text" x="637.7" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="637.7" y="925" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="710.0" y1="28" x2="710.0" y2="910"/>
<text class="axis-text" x="710.0" y="22" text-anchor="middle">1.4×</text>
<text class="axis-text" x="710.0" y="925" text-anchor="middle">1.4×</text>
<text class="axis-text" x="629.7" y="9" text-anchor="end">← Pure-Go faster / lower CPU</text>
<text class="axis-text" x="645.7" y="9" text-anchor="start">CGo faster / lower CPU →</text>
<line class="section-divider" x1="8" y1="40" x2="882" y2="40"/>
<text class="section-title" x="10" y="55">Mean Latency Ratio: Final Pure-Go / CGo</text>
<text class="row-label" x="218" y="79.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.515× [0.484, 0.545]</title><line x1="481.7" y1="75.2" x2="507.3" y2="75.2" stroke-width="2.2"/><line x1="481.7" y1="71.2" x2="481.7" y2="79.2" stroke-width="1.8"/><line x1="507.3" y1="71.2" x2="507.3" y2="79.2" stroke-width="1.8"/><circle cx="495.1" cy="75.2" r="3.8"/></g>
<text class="val-text" x="722" y="79.2">0.515× [0.484, 0.545]</text>
<text class="row-label" x="218" y="103.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.651× [0.594, 0.717]</title><line x1="525.8" y1="99.2" x2="566.2" y2="99.2" stroke-width="2.2"/><line x1="525.8" y1="95.2" x2="525.8" y2="103.2" stroke-width="1.8"/><line x1="566.2" y1="95.2" x2="566.2" y2="103.2" stroke-width="1.8"/><circle cx="545.4" cy="99.2" r="3.8"/></g>
<text class="val-text" x="722" y="103.2">0.651× [0.594, 0.717]</text>
<text class="row-label" x="218" y="127.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.637× [0.598, 0.698]</title><line x1="527.2" y1="123.2" x2="560.4" y2="123.2" stroke-width="2.2"/><line x1="527.2" y1="119.2" x2="527.2" y2="127.2" stroke-width="1.8"/><line x1="560.4" y1="119.2" x2="560.4" y2="127.2" stroke-width="1.8"/><circle cx="540.8" cy="123.2" r="3.8"/></g>
<text class="val-text" x="722" y="127.2">0.637× [0.598, 0.698]</text>
<text class="row-label" x="218" y="151.2" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.637× [0.585, 0.700]</title><line x1="522.5" y1="147.2" x2="561.0" y2="147.2" stroke-width="2.2"/><line x1="522.5" y1="143.2" x2="522.5" y2="151.2" stroke-width="1.8"/><line x1="561.0" y1="143.2" x2="561.0" y2="151.2" stroke-width="1.8"/><circle cx="540.8" cy="147.2" r="3.8"/></g>
<text class="val-text" x="722" y="151.2">0.637× [0.585, 0.700]</text>
<text class="row-label" x="218" y="175.2" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.613× [0.583, 0.647]</title><line x1="521.7" y1="171.2" x2="544.1" y2="171.2" stroke-width="2.2"/><line x1="521.7" y1="167.2" x2="521.7" y2="175.2" stroke-width="1.8"/><line x1="544.1" y1="167.2" x2="544.1" y2="175.2" stroke-width="1.8"/><circle cx="532.5" cy="171.2" r="3.8"/></g>
<text class="val-text" x="722" y="175.2">0.613× [0.583, 0.647]</text>
<text class="row-label" x="218" y="199.2" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.721× [0.618, 0.842]</title><line x1="534.3" y1="195.2" x2="600.7" y2="195.2" stroke-width="2.2"/><line x1="534.3" y1="191.2" x2="534.3" y2="199.2" stroke-width="1.8"/><line x1="600.7" y1="191.2" x2="600.7" y2="199.2" stroke-width="1.8"/><circle cx="567.4" cy="195.2" r="3.8"/></g>
<text class="val-text" x="722" y="199.2">0.721× [0.618, 0.842]</text>
<text class="row-label" x="218" y="223.2" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.846× [0.713, 1.024]</title><line x1="565.0" y1="219.2" x2="642.8" y2="219.2" stroke-width="2.2"/><line x1="565.0" y1="215.2" x2="565.0" y2="223.2" stroke-width="1.8"/><line x1="642.8" y1="215.2" x2="642.8" y2="223.2" stroke-width="1.8"/><circle cx="601.8" cy="219.2" r="3.8"/></g>
<text class="val-text" x="722" y="223.2">0.846× [0.713, 1.024]</text>
<text class="row-label" x="218" y="247.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.897× [0.873, 0.920]</title><line x1="608.5" y1="243.2" x2="619.8" y2="243.2" stroke-width="2.2"/><line x1="608.5" y1="239.2" x2="608.5" y2="247.2" stroke-width="1.8"/><line x1="619.8" y1="239.2" x2="619.8" y2="247.2" stroke-width="1.8"/><circle cx="614.3" cy="243.2" r="3.8"/></g>
<text class="val-text" x="722" y="247.2">0.897× [0.873, 0.920]</text>
<text class="row-label" x="218" y="271.2" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.189× [0.161, 0.219]</title><line x1="245.2" y1="267.2" x2="311.3" y2="267.2" stroke-width="2.2"/><line x1="245.2" y1="263.2" x2="245.2" y2="271.2" stroke-width="1.8"/><line x1="311.3" y1="263.2" x2="311.3" y2="271.2" stroke-width="1.8"/><circle cx="279.7" cy="267.2" r="3.8"/></g>
<text class="val-text" x="722" y="271.2">0.189× [0.161, 0.219]</text>
<text class="row-label" x="218" y="295.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.931× [0.904, 0.950]</title><line x1="616.0" y1="291.2" x2="626.7" y2="291.2" stroke-width="2.2"/><line x1="616.0" y1="287.2" x2="616.0" y2="295.2" stroke-width="1.8"/><line x1="626.7" y1="287.2" x2="626.7" y2="295.2" stroke-width="1.8"/><circle cx="622.3" cy="291.2" r="3.8"/></g>
<text class="val-text" x="722" y="295.2">0.931× [0.904, 0.950]</text>
<text class="row-label" x="218" y="319.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.918× [0.898, 0.937]</title><line x1="614.6" y1="315.2" x2="623.7" y2="315.2" stroke-width="2.2"/><line x1="614.6" y1="311.2" x2="614.6" y2="319.2" stroke-width="1.8"/><line x1="623.7" y1="311.2" x2="623.7" y2="319.2" stroke-width="1.8"/><circle cx="619.3" cy="315.2" r="3.8"/></g>
<text class="val-text" x="722" y="319.2">0.918× [0.898, 0.937]</text>
<line class="section-divider" x1="8" y1="330" x2="882" y2="330"/>
<text class="section-title" x="10" y="345">p99 Latency Ratio: Final Pure-Go / CGo</text>
<text class="row-label" x="218" y="369.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.592× [0.566, 0.625]</title><line x1="515.4" y1="365.2" x2="536.7" y2="365.2" stroke-width="2.2"/><line x1="515.4" y1="361.2" x2="515.4" y2="369.2" stroke-width="1.8"/><line x1="536.7" y1="361.2" x2="536.7" y2="369.2" stroke-width="1.8"/><circle cx="525.0" cy="365.2" r="3.8"/></g>
<text class="val-text" x="722" y="369.2">0.592× [0.566, 0.625]</text>
<text class="row-label" x="218" y="393.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.675× [0.613, 0.743]</title><line x1="532.5" y1="389.2" x2="573.9" y2="389.2" stroke-width="2.2"/><line x1="532.5" y1="385.2" x2="532.5" y2="393.2" stroke-width="1.8"/><line x1="573.9" y1="385.2" x2="573.9" y2="393.2" stroke-width="1.8"/><circle cx="553.2" cy="389.2" r="3.8"/></g>
<text class="val-text" x="722" y="393.2">0.675× [0.613, 0.743]</text>
<text class="row-label" x="218" y="417.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.679× [0.627, 0.743]</title><line x1="537.4" y1="413.2" x2="573.9" y2="413.2" stroke-width="2.2"/><line x1="537.4" y1="409.2" x2="537.4" y2="417.2" stroke-width="1.8"/><line x1="573.9" y1="409.2" x2="573.9" y2="417.2" stroke-width="1.8"/><circle cx="554.5" cy="413.2" r="3.8"/></g>
<text class="val-text" x="722" y="417.2">0.679× [0.627, 0.743]</text>
<text class="row-label" x="218" y="441.2" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.725× [0.668, 0.776]</title><line x1="551.0" y1="437.2" x2="583.2" y2="437.2" stroke-width="2.2"/><line x1="551.0" y1="433.2" x2="551.0" y2="441.2" stroke-width="1.8"/><line x1="583.2" y1="433.2" x2="583.2" y2="441.2" stroke-width="1.8"/><circle cx="568.6" cy="437.2" r="3.8"/></g>
<text class="val-text" x="722" y="441.2">0.725× [0.668, 0.776]</text>
<text class="row-label" x="218" y="465.2" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.648× [0.596, 0.701]</title><line x1="526.5" y1="461.2" x2="561.3" y2="461.2" stroke-width="2.2"/><line x1="526.5" y1="457.2" x2="526.5" y2="465.2" stroke-width="1.8"/><line x1="561.3" y1="457.2" x2="561.3" y2="465.2" stroke-width="1.8"/><circle cx="544.5" cy="461.2" r="3.8"/></g>
<text class="val-text" x="722" y="465.2">0.648× [0.596, 0.701]</text>
<text class="row-label" x="218" y="489.2" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.666× [0.628, 0.701]</title><line x1="537.7" y1="485.2" x2="561.3" y2="485.2" stroke-width="2.2"/><line x1="537.7" y1="481.2" x2="537.7" y2="489.2" stroke-width="1.8"/><line x1="561.3" y1="481.2" x2="561.3" y2="489.2" stroke-width="1.8"/><circle cx="550.3" cy="485.2" r="3.8"/></g>
<text class="val-text" x="722" y="489.2">0.666× [0.628, 0.701]</text>
<text class="row-label" x="218" y="513.2" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 0.861× [0.755, 1.008]</title><line x1="577.3" y1="509.2" x2="639.4" y2="509.2" stroke-width="2.2"/><line x1="577.3" y1="505.2" x2="577.3" y2="513.2" stroke-width="1.8"/><line x1="639.4" y1="505.2" x2="639.4" y2="513.2" stroke-width="1.8"/><circle cx="605.5" cy="509.2" r="3.8"/></g>
<text class="val-text" x="722" y="513.2">0.861× [0.755, 1.008]</text>
<text class="row-label" x="218" y="537.2" text-anchor="end">set100</text>
<g class="ci-neutral"><title>set100: 0.939× [0.845, 1.020]</title><line x1="601.5" y1="533.2" x2="641.9" y2="533.2" stroke-width="2.2"/><line x1="601.5" y1="529.2" x2="601.5" y2="537.2" stroke-width="1.8"/><line x1="641.9" y1="529.2" x2="641.9" y2="537.2" stroke-width="1.8"/><circle cx="624.2" cy="533.2" r="3.8"/></g>
<text class="val-text" x="722" y="537.2">0.939× [0.845, 1.020]</text>
<text class="row-label" x="218" y="561.2" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.245× [0.214, 0.284]</title><line x1="306.4" y1="557.2" x2="367.2" y2="557.2" stroke-width="2.2"/><line x1="306.4" y1="553.2" x2="306.4" y2="561.2" stroke-width="1.8"/><line x1="367.2" y1="553.2" x2="367.2" y2="561.2" stroke-width="1.8"/><circle cx="335.4" cy="557.2" r="3.8"/></g>
<text class="val-text" x="722" y="561.2">0.245× [0.214, 0.284]</text>
<text class="row-label" x="218" y="585.2" text-anchor="end">batchset50</text>
<g class="ci-neutral"><title>batchset50: 0.989× [0.913, 1.126]</title><line x1="618.1" y1="581.2" x2="663.2" y2="581.2" stroke-width="2.2"/><line x1="618.1" y1="577.2" x2="618.1" y2="585.2" stroke-width="1.8"/><line x1="663.2" y1="577.2" x2="663.2" y2="585.2" stroke-width="1.8"/><circle cx="635.3" cy="581.2" r="3.8"/></g>
<text class="val-text" x="722" y="585.2">0.989× [0.913, 1.126]</text>
<text class="row-label" x="218" y="609.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.942× [0.909, 0.979]</title><line x1="617.2" y1="605.2" x2="633.1" y2="605.2" stroke-width="2.2"/><line x1="617.2" y1="601.2" x2="617.2" y2="609.2" stroke-width="1.8"/><line x1="633.1" y1="601.2" x2="633.1" y2="609.2" stroke-width="1.8"/><circle cx="624.9" cy="605.2" r="3.8"/></g>
<text class="val-text" x="722" y="609.2">0.942× [0.909, 0.979]</text>
<line class="section-divider" x1="8" y1="620" x2="882" y2="620"/>
<text class="section-title" x="10" y="635">Shutdown Process CPU (E−A) Ratio: Final Pure-Go / CGo</text>
<text class="row-label" x="218" y="659.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.659× [0.621, 0.692]</title><line x1="535.3" y1="655.2" x2="558.6" y2="655.2" stroke-width="2.2"/><line x1="535.3" y1="651.2" x2="535.3" y2="659.2" stroke-width="1.8"/><line x1="558.6" y1="651.2" x2="558.6" y2="659.2" stroke-width="1.8"/><circle cx="548.1" cy="655.2" r="3.8"/></g>
<text class="val-text" x="722" y="659.2">0.659× [0.621, 0.692]</text>
<text class="row-label" x="218" y="683.2" text-anchor="end">get100</text>
<g class="ci-neutral"><title>get100: 0.948× [0.817, 1.118]</title><line x1="594.3" y1="679.2" x2="661.7" y2="679.2" stroke-width="2.2"/><line x1="594.3" y1="675.2" x2="594.3" y2="683.2" stroke-width="1.8"/><line x1="661.7" y1="675.2" x2="661.7" y2="683.2" stroke-width="1.8"/><circle cx="626.2" cy="679.2" r="3.8"/></g>
<text class="val-text" x="722" y="683.2">0.948× [0.817, 1.118]</text>
<text class="row-label" x="218" y="707.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.858× [0.810, 0.925]</title><line x1="592.4" y1="703.2" x2="620.9" y2="703.2" stroke-width="2.2"/><line x1="592.4" y1="699.2" x2="592.4" y2="707.2" stroke-width="1.8"/><line x1="620.9" y1="699.2" x2="620.9" y2="707.2" stroke-width="1.8"/><circle cx="604.8" cy="703.2" r="3.8"/></g>
<text class="val-text" x="722" y="707.2">0.858× [0.810, 0.925]</text>
<text class="row-label" x="218" y="731.2" text-anchor="end">get10000</text>
<g class="ci-neutral"><title>get10000: 0.959× [0.884, 1.046]</title><line x1="611.2" y1="727.2" x2="647.4" y2="727.2" stroke-width="2.2"/><line x1="611.2" y1="723.2" x2="611.2" y2="731.2" stroke-width="1.8"/><line x1="647.4" y1="723.2" x2="647.4" y2="731.2" stroke-width="1.8"/><circle cx="628.7" cy="727.2" r="3.8"/></g>
<text class="val-text" x="722" y="731.2">0.959× [0.884, 1.046]</text>
<text class="row-label" x="218" y="755.2" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.775× [0.735, 0.815]</title><line x1="571.5" y1="751.2" x2="593.7" y2="751.2" stroke-width="2.2"/><line x1="571.5" y1="747.2" x2="571.5" y2="755.2" stroke-width="1.8"/><line x1="593.7" y1="747.2" x2="593.7" y2="755.2" stroke-width="1.8"/><circle cx="582.9" cy="751.2" r="3.8"/></g>
<text class="val-text" x="722" y="755.2">0.775× [0.735, 0.815]</text>
<text class="row-label" x="218" y="779.2" text-anchor="end">range10</text>
<g class="ci-neutral"><title>range10: 0.969× [0.834, 1.122]</title><line x1="598.7" y1="775.2" x2="662.4" y2="775.2" stroke-width="2.2"/><line x1="598.7" y1="771.2" x2="598.7" y2="779.2" stroke-width="1.8"/><line x1="662.4" y1="771.2" x2="662.4" y2="779.2" stroke-width="1.8"/><circle cx="630.9" cy="775.2" r="3.8"/></g>
<text class="val-text" x="722" y="779.2">0.969× [0.834, 1.122]</text>
<text class="row-label" x="218" y="803.2" text-anchor="end">range100</text>
<g class="ci-neutral"><title>range100: 1.101× [0.968, 1.269]</title><line x1="630.7" y1="799.2" x2="688.9" y2="799.2" stroke-width="2.2"/><line x1="630.7" y1="795.2" x2="630.7" y2="803.2" stroke-width="1.8"/><line x1="688.9" y1="795.2" x2="688.9" y2="803.2" stroke-width="1.8"/><circle cx="658.4" cy="799.2" r="3.8"/></g>
<text class="val-text" x="722" y="803.2">1.101× [0.968, 1.269]</text>
<text class="row-label" x="218" y="827.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.869× [0.841, 0.899]</title><line x1="600.5" y1="823.2" x2="614.8" y2="823.2" stroke-width="2.2"/><line x1="600.5" y1="819.2" x2="600.5" y2="827.2" stroke-width="1.8"/><line x1="614.8" y1="819.2" x2="614.8" y2="827.2" stroke-width="1.8"/><circle cx="607.5" cy="823.2" r="3.8"/></g>
<text class="val-text" x="722" y="827.2">0.869× [0.841, 0.899]</text>
<text class="row-label" x="218" y="851.2" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.228× [0.201, 0.256]</title><line x1="292.9" y1="847.2" x2="344.9" y2="847.2" stroke-width="2.2"/><line x1="292.9" y1="843.2" x2="292.9" y2="851.2" stroke-width="1.8"/><line x1="344.9" y1="843.2" x2="344.9" y2="851.2" stroke-width="1.8"/><circle cx="320.0" cy="847.2" r="3.8"/></g>
<text class="val-text" x="722" y="851.2">0.228× [0.201, 0.256]</text>
<text class="row-label" x="218" y="875.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.863× [0.854, 0.873]</title><line x1="603.8" y1="871.2" x2="608.5" y2="871.2" stroke-width="2.2"/><line x1="603.8" y1="867.2" x2="603.8" y2="875.2" stroke-width="1.8"/><line x1="608.5" y1="867.2" x2="608.5" y2="875.2" stroke-width="1.8"/><circle cx="606.0" cy="871.2" r="3.8"/></g>
<text class="val-text" x="722" y="875.2">0.863× [0.854, 0.873]</text>
<text class="row-label" x="218" y="899.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.887× [0.864, 0.922]</title><line x1="606.3" y1="895.2" x2="620.2" y2="895.2" stroke-width="2.2"/><line x1="606.3" y1="891.2" x2="606.3" y2="899.2" stroke-width="1.8"/><line x1="620.2" y1="891.2" x2="620.2" y2="899.2" stroke-width="1.8"/><circle cx="611.9" cy="895.2" r="3.8"/></g>
<text class="val-text" x="722" y="899.2">0.887× [0.864, 0.922]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

<figure class="bench-figure">
<figcaption><strong>Figure A.2: Allocation Reductions — Final / Before Pure-Go (3824f146 → 267a4143)</strong> — Go heap bytes and mallocs per transaction (6 paired blocks). Below 1.0× means fewer allocations in Final.</figcaption>
<svg class="bench-svg" viewBox="0 0 890 650" role="img" aria-label="Figure A.2: Allocation Reductions — Final / Before Pure-Go (3824f146 → 267a4143)">
<line class="grid-line" x1="230.0" y1="28" x2="230.0" y2="620"/>
<text class="axis-text" x="230.0" y="22" text-anchor="middle">0.7×</text>
<text class="axis-text" x="230.0" y="635" text-anchor="middle">0.7×</text>
<line class="grid-line" x1="371.8" y1="28" x2="371.8" y2="620"/>
<text class="axis-text" x="371.8" y="22" text-anchor="middle">0.8×</text>
<text class="axis-text" x="371.8" y="635" text-anchor="middle">0.8×</text>
<line class="grid-line" x1="496.9" y1="28" x2="496.9" y2="620"/>
<text class="axis-text" x="496.9" y="22" text-anchor="middle">0.9×</text>
<text class="axis-text" x="496.9" y="635" text-anchor="middle">0.9×</text>
<line class="parity-line" x1="608.8" y1="28" x2="608.8" y2="620"/>
<text class="axis-text" x="608.8" y="22" text-anchor="middle">1.0×</text>
<text class="axis-text" x="608.8" y="635" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="710.0" y1="28" x2="710.0" y2="620"/>
<text class="axis-text" x="710.0" y="22" text-anchor="middle">1.1×</text>
<text class="axis-text" x="710.0" y="635" text-anchor="middle">1.1×</text>
<text class="axis-text" x="600.8" y="9" text-anchor="end">← Final allocates less</text>
<text class="axis-text" x="616.8" y="9" text-anchor="start">Before allocates less →</text>
<line class="section-divider" x1="8" y1="40" x2="882" y2="40"/>
<text class="section-title" x="10" y="55">Go Bytes/tx Ratio: Final / Before Pure-Go</text>
<text class="row-label" x="218" y="79.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.993× [0.992, 0.994]</title><line x1="600.3" y1="75.2" x2="602.4" y2="75.2" stroke-width="2.2"/><line x1="600.3" y1="71.2" x2="600.3" y2="79.2" stroke-width="1.8"/><line x1="602.4" y1="71.2" x2="602.4" y2="79.2" stroke-width="1.8"/><circle cx="601.3" cy="75.2" r="3.8"/></g>
<text class="val-text" x="722" y="79.2">0.993× [0.992, 0.994]</text>
<text class="row-label" x="218" y="103.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.929× [0.928, 0.930]</title><line x1="529.4" y1="99.2" x2="531.7" y2="99.2" stroke-width="2.2"/><line x1="529.4" y1="95.2" x2="529.4" y2="103.2" stroke-width="1.8"/><line x1="531.7" y1="95.2" x2="531.7" y2="103.2" stroke-width="1.8"/><circle cx="530.6" cy="99.2" r="3.8"/></g>
<text class="val-text" x="722" y="103.2">0.929× [0.928, 0.930]</text>
<text class="row-label" x="218" y="127.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.944× [0.943, 0.944]</title><line x1="546.5" y1="123.2" x2="547.6" y2="123.2" stroke-width="2.2"/><line x1="546.5" y1="119.2" x2="546.5" y2="127.2" stroke-width="1.8"/><line x1="547.6" y1="119.2" x2="547.6" y2="127.2" stroke-width="1.8"/><circle cx="547.6" cy="123.2" r="3.8"/></g>
<text class="val-text" x="722" y="127.2">0.944× [0.943, 0.944]</text>
<text class="row-label" x="218" y="151.2" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.982× [0.982, 0.983]</title><line x1="589.5" y1="147.2" x2="590.6" y2="147.2" stroke-width="2.2"/><line x1="589.5" y1="143.2" x2="589.5" y2="151.2" stroke-width="1.8"/><line x1="590.6" y1="143.2" x2="590.6" y2="151.2" stroke-width="1.8"/><circle cx="589.5" cy="147.2" r="3.8"/></g>
<text class="val-text" x="722" y="151.2">0.982× [0.982, 0.983]</text>
<text class="row-label" x="218" y="175.2" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.997× [0.996, 0.998]</title><line x1="604.5" y1="171.2" x2="606.7" y2="171.2" stroke-width="2.2"/><line x1="604.5" y1="167.2" x2="604.5" y2="175.2" stroke-width="1.8"/><line x1="606.7" y1="167.2" x2="606.7" y2="175.2" stroke-width="1.8"/><circle cx="605.6" cy="171.2" r="3.8"/></g>
<text class="val-text" x="722" y="175.2">0.997× [0.996, 0.998]</text>
<text class="row-label" x="218" y="199.2" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.844× [0.844, 0.845]</title><line x1="428.7" y1="195.2" x2="429.9" y2="195.2" stroke-width="2.2"/><line x1="428.7" y1="191.2" x2="428.7" y2="199.2" stroke-width="1.8"/><line x1="429.9" y1="191.2" x2="429.9" y2="199.2" stroke-width="1.8"/><circle cx="428.7" cy="195.2" r="3.8"/></g>
<text class="val-text" x="722" y="199.2">0.844× [0.844, 0.845]</text>
<text class="row-label" x="218" y="223.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.824× [0.824, 0.825]</title><line x1="403.2" y1="219.2" x2="404.5" y2="219.2" stroke-width="2.2"/><line x1="403.2" y1="215.2" x2="403.2" y2="223.2" stroke-width="1.8"/><line x1="404.5" y1="215.2" x2="404.5" y2="223.2" stroke-width="1.8"/><circle cx="403.2" cy="219.2" r="3.8"/></g>
<text class="val-text" x="722" y="223.2">0.824× [0.824, 0.825]</text>
<text class="row-label" x="218" y="247.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.956× [0.954, 0.957]</title><line x1="558.8" y1="243.2" x2="562.1" y2="243.2" stroke-width="2.2"/><line x1="558.8" y1="239.2" x2="558.8" y2="247.2" stroke-width="1.8"/><line x1="562.1" y1="239.2" x2="562.1" y2="247.2" stroke-width="1.8"/><circle cx="561.0" cy="243.2" r="3.8"/></g>
<text class="val-text" x="722" y="247.2">0.956× [0.954, 0.957]</text>
<text class="row-label" x="218" y="271.2" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.995× [0.995, 0.995]</title><line x1="603.5" y1="267.2" x2="603.5" y2="267.2" stroke-width="2.2"/><line x1="603.5" y1="263.2" x2="603.5" y2="271.2" stroke-width="1.8"/><line x1="603.5" y1="263.2" x2="603.5" y2="271.2" stroke-width="1.8"/><circle cx="603.5" cy="267.2" r="3.8"/></g>
<text class="val-text" x="722" y="271.2">0.995× [0.995, 0.995]</text>
<text class="row-label" x="218" y="295.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.993× [0.992, 0.994]</title><line x1="600.3" y1="291.2" x2="602.4" y2="291.2" stroke-width="2.2"/><line x1="600.3" y1="287.2" x2="600.3" y2="295.2" stroke-width="1.8"/><line x1="602.4" y1="287.2" x2="602.4" y2="295.2" stroke-width="1.8"/><circle cx="601.3" cy="291.2" r="3.8"/></g>
<text class="val-text" x="722" y="295.2">0.993× [0.992, 0.994]</text>
<text class="row-label" x="218" y="319.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.998× [0.998, 0.998]</title><line x1="606.7" y1="315.2" x2="606.7" y2="315.2" stroke-width="2.2"/><line x1="606.7" y1="311.2" x2="606.7" y2="319.2" stroke-width="1.8"/><line x1="606.7" y1="311.2" x2="606.7" y2="319.2" stroke-width="1.8"/><circle cx="606.7" cy="315.2" r="3.8"/></g>
<text class="val-text" x="722" y="319.2">0.998× [0.998, 0.998]</text>
<line class="section-divider" x1="8" y1="330" x2="882" y2="330"/>
<text class="section-title" x="10" y="345">Go Mallocs/tx Ratio: Final / Before Pure-Go</text>
<text class="row-label" x="218" y="369.2" text-anchor="end">grv</text>
<g class="ci-win"><title>grv: 0.950× [0.950, 0.950]</title><line x1="554.3" y1="365.2" x2="554.3" y2="365.2" stroke-width="2.2"/><line x1="554.3" y1="361.2" x2="554.3" y2="369.2" stroke-width="1.8"/><line x1="554.3" y1="361.2" x2="554.3" y2="369.2" stroke-width="1.8"/><circle cx="554.3" cy="365.2" r="3.8"/></g>
<text class="val-text" x="722" y="369.2">0.950× [0.950, 0.950]</text>
<text class="row-label" x="218" y="393.2" text-anchor="end">get100</text>
<g class="ci-win"><title>get100: 0.874× [0.873, 0.874]</title><line x1="464.5" y1="389.2" x2="465.8" y2="389.2" stroke-width="2.2"/><line x1="464.5" y1="385.2" x2="464.5" y2="393.2" stroke-width="1.8"/><line x1="465.8" y1="385.2" x2="465.8" y2="393.2" stroke-width="1.8"/><circle cx="465.8" cy="389.2" r="3.8"/></g>
<text class="val-text" x="722" y="393.2">0.874× [0.873, 0.874]</text>
<text class="row-label" x="218" y="417.2" text-anchor="end">get1000</text>
<g class="ci-win"><title>get1000: 0.874× [0.873, 0.874]</title><line x1="464.5" y1="413.2" x2="465.8" y2="413.2" stroke-width="2.2"/><line x1="464.5" y1="409.2" x2="464.5" y2="417.2" stroke-width="1.8"/><line x1="465.8" y1="409.2" x2="465.8" y2="417.2" stroke-width="1.8"/><circle cx="465.8" cy="413.2" r="3.8"/></g>
<text class="val-text" x="722" y="417.2">0.874× [0.873, 0.874]</text>
<text class="row-label" x="218" y="441.2" text-anchor="end">get10000</text>
<g class="ci-win"><title>get10000: 0.874× [0.874, 0.875]</title><line x1="465.8" y1="437.2" x2="467.0" y2="437.2" stroke-width="2.2"/><line x1="465.8" y1="433.2" x2="465.8" y2="441.2" stroke-width="1.8"/><line x1="467.0" y1="433.2" x2="467.0" y2="441.2" stroke-width="1.8"/><circle cx="465.8" cy="437.2" r="3.8"/></g>
<text class="val-text" x="722" y="441.2">0.874× [0.874, 0.875]</text>
<text class="row-label" x="218" y="465.2" text-anchor="end">read1000</text>
<g class="ci-win"><title>read1000: 0.947× [0.947, 0.948]</title><line x1="551.0" y1="461.2" x2="552.1" y2="461.2" stroke-width="2.2"/><line x1="551.0" y1="457.2" x2="551.0" y2="465.2" stroke-width="1.8"/><line x1="552.1" y1="457.2" x2="552.1" y2="465.2" stroke-width="1.8"/><circle cx="551.0" cy="461.2" r="3.8"/></g>
<text class="val-text" x="722" y="465.2">0.947× [0.947, 0.948]</text>
<text class="row-label" x="218" y="489.2" text-anchor="end">range10</text>
<g class="ci-win"><title>range10: 0.797× [0.796, 0.797]</title><line x1="366.5" y1="485.2" x2="367.8" y2="485.2" stroke-width="2.2"/><line x1="366.5" y1="481.2" x2="366.5" y2="489.2" stroke-width="1.8"/><line x1="367.8" y1="481.2" x2="367.8" y2="489.2" stroke-width="1.8"/><circle cx="367.8" cy="485.2" r="3.8"/></g>
<text class="val-text" x="722" y="489.2">0.797× [0.796, 0.797]</text>
<text class="row-label" x="218" y="513.2" text-anchor="end">range100</text>
<g class="ci-win"><title>range100: 0.798× [0.798, 0.798]</title><line x1="369.1" y1="509.2" x2="369.1" y2="509.2" stroke-width="2.2"/><line x1="369.1" y1="505.2" x2="369.1" y2="513.2" stroke-width="1.8"/><line x1="369.1" y1="505.2" x2="369.1" y2="513.2" stroke-width="1.8"/><circle cx="369.1" cy="509.2" r="3.8"/></g>
<text class="val-text" x="722" y="513.2">0.798× [0.798, 0.798]</text>
<text class="row-label" x="218" y="537.2" text-anchor="end">set100</text>
<g class="ci-win"><title>set100: 0.919× [0.918, 0.919]</title><line x1="517.9" y1="533.2" x2="519.1" y2="533.2" stroke-width="2.2"/><line x1="517.9" y1="529.2" x2="517.9" y2="537.2" stroke-width="1.8"/><line x1="519.1" y1="529.2" x2="519.1" y2="537.2" stroke-width="1.8"/><circle cx="519.1" cy="533.2" r="3.8"/></g>
<text class="val-text" x="722" y="537.2">0.919× [0.918, 0.919]</text>
<text class="row-label" x="218" y="561.2" text-anchor="end">ryw</text>
<g class="ci-win"><title>ryw: 0.950× [0.950, 0.950]</title><line x1="554.3" y1="557.2" x2="554.3" y2="557.2" stroke-width="2.2"/><line x1="554.3" y1="553.2" x2="554.3" y2="561.2" stroke-width="1.8"/><line x1="554.3" y1="553.2" x2="554.3" y2="561.2" stroke-width="1.8"/><circle cx="554.3" cy="557.2" r="3.8"/></g>
<text class="val-text" x="722" y="561.2">0.950× [0.950, 0.950]</text>
<text class="row-label" x="218" y="585.2" text-anchor="end">batchset50</text>
<g class="ci-win"><title>batchset50: 0.973× [0.973, 0.973]</title><line x1="579.7" y1="581.2" x2="579.7" y2="581.2" stroke-width="2.2"/><line x1="579.7" y1="577.2" x2="579.7" y2="585.2" stroke-width="1.8"/><line x1="579.7" y1="577.2" x2="579.7" y2="585.2" stroke-width="1.8"/><circle cx="579.7" cy="581.2" r="3.8"/></g>
<text class="val-text" x="722" y="585.2">0.973× [0.973, 0.973]</text>
<text class="row-label" x="218" y="609.2" text-anchor="end">batchset50x1000</text>
<g class="ci-win"><title>batchset50x1000: 0.973× [0.973, 0.973]</title><line x1="579.7" y1="605.2" x2="579.7" y2="605.2" stroke-width="2.2"/><line x1="579.7" y1="601.2" x2="579.7" y2="609.2" stroke-width="1.8"/><line x1="579.7" y1="601.2" x2="579.7" y2="609.2" stroke-width="1.8"/><circle cx="579.7" cy="605.2" r="3.8"/></g>
<text class="val-text" x="722" y="609.2">0.973× [0.973, 0.973]</text>
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
