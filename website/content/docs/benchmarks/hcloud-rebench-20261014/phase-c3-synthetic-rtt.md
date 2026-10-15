---
title: "Phase C3: Remote Fetch (0.5 ms Synthetic RTT)"
description: "Sensitivity of USE_REMOTE_FETCH vs SCAN_AND_FETCH to 0.5 ms nominal synthetic veth RTT."
weight: 6
---

`USE_REMOTE_FETCH` against `SCAN_AND_FETCH` on the final tree (`267a41434…`)
with a synthetic 250 µs one-way `tc netem` delay on each direction of the
private veth pair (0.5 ms nominal added RTT). This is **phase C3** of
[hcloud-rebench-20261014](../) — a sensitivity check on the same host,
not a multi-host or physical-network benchmark.

## Environment and Network Verification

- Same 16-vCPU `ccx43` host and `memory` storage engine as phase C1, configured
  fresh after phase A2.
- Root `netem` qdisc installed on both `fdb-bench-host` (server-to-client
  egress) and `fdb-bench-peer` (client-to-server egress): `limit 100000 delay
  250us` (kernel JSON `0.000249984` s per direction; 0% loss, 0 jitter, 0
  reordering).
- Pre-phase 100-packet unloaded ping from the client namespace to the host
  veth endpoint: **p50 0.597 ms, p99 0.628 ms** (min 0.534 ms, max 0.629 ms),
  0% packet loss, 0 qdisc drops before and after.
- Post-phase restoration: both root qdiscs deleted (`noqueue` verified), 0
  drops during the phase, and a 100-packet zero-delay ping confirmed restoration
  at **p50 0.039 ms, p99 0.043 ms**.

## Design

- Two full-scan cases (`all-1000` rows drained from `price_idx` on 1,000 `Order`
  records): `small-all-1000` (unsplit, 1 KV per record) and `split-all-1000`
  (100,000-byte padding, 2 KVs per record).
- 6 balanced blocks × 2 arms (`final-ordinary` = `SCAN_AND_FETCH`,
  `final-remote` = strict `USE_REMOTE_FETCH`), 3 `testing.B` repeats (`3s`
  benchtime) per process: **24 processes, 72 samples**.

## Results

<figure class="bench-figure">
<figcaption><strong>Figure C3.1: Remote Fetch vs Ordinary Scan Ratios (log₂ scale, 95% CI)</strong> — 6 paired blocks × 3 repeats on single-node memory engine. Below 1.0× favors USE_REMOTE_FETCH.</figcaption>
<svg class="bench-svg" viewBox="0 0 1320 260" role="img" aria-label="Figure C3.1: Remote Fetch vs Ordinary Scan Ratios (log₂ scale, 95% CI)">
<line class="grid-line" x1="165.0" y1="36" x2="165.0" y2="234"/>
<text class="axis-text" x="165.0" y="30" text-anchor="middle">0.2×</text>
<text class="axis-text" x="165.0" y="249" text-anchor="middle">0.2×</text>
<line class="grid-line" x1="452.0" y1="36" x2="452.0" y2="234"/>
<text class="axis-text" x="452.0" y="30" text-anchor="middle">0.35×</text>
<text class="axis-text" x="452.0" y="249" text-anchor="middle">0.35×</text>
<line class="grid-line" x1="634.9" y1="36" x2="634.9" y2="234"/>
<text class="axis-text" x="634.9" y="30" text-anchor="middle">0.5×</text>
<text class="axis-text" x="634.9" y="249" text-anchor="middle">0.5×</text>
<line class="grid-line" x1="807.5" y1="36" x2="807.5" y2="234"/>
<text class="axis-text" x="807.5" y="30" text-anchor="middle">0.7×</text>
<text class="axis-text" x="807.5" y="249" text-anchor="middle">0.7×</text>
<line class="parity-line" x1="990.4" y1="36" x2="990.4" y2="234"/>
<text class="axis-text" x="990.4" y="30" text-anchor="middle">1.0×</text>
<text class="axis-text" x="990.4" y="249" text-anchor="middle">1.0×</text>
<line class="grid-line" x1="1125.0" y1="36" x2="1125.0" y2="234"/>
<text class="axis-text" x="1125.0" y="30" text-anchor="middle">1.3×</text>
<text class="axis-text" x="1125.0" y="249" text-anchor="middle">1.3×</text>
<text class="axis-text" x="982.4" y="17" text-anchor="end">← USE_REMOTE_FETCH lower</text>
<text class="axis-text" x="998.4" y="17" text-anchor="start">SCAN_AND_FETCH lower →</text>
<line class="section-divider" x1="8" y1="48" x2="1312" y2="48"/>
<text class="section-title" x="10" y="63">Mean Time (ns/op): USE_REMOTE_FETCH / SCAN_AND_FETCH</text>
<text class="row-label" x="153" y="81.0" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.224× [0.222, 0.226]</title><line x1="218.5" y1="77.0" x2="227.7" y2="77.0" stroke-width="2.2"/><line x1="218.5" y1="73.0" x2="218.5" y2="81.0" stroke-width="1.8"/><line x1="227.7" y1="73.0" x2="227.7" y2="81.0" stroke-width="1.8"/><circle cx="223.1" cy="77.0" r="3.8"/></g>
<text class="val-text" x="1137" y="81.0">0.224× [0.222, 0.226]</text>
<text class="row-label" x="153" y="101.0" text-anchor="end">split-all-1000</text>
<g class="ci-reg"><title>split-all-1000: 1.053× [1.008, 1.101]</title><line x1="994.5" y1="97.0" x2="1039.8" y2="97.0" stroke-width="2.2"/><line x1="994.5" y1="93.0" x2="994.5" y2="101.0" stroke-width="1.8"/><line x1="1039.8" y1="93.0" x2="1039.8" y2="101.0" stroke-width="1.8"/><circle cx="1016.9" cy="97.0" r="3.8"/></g>
<text class="val-text" x="1137" y="101.0">1.053× [1.008, 1.101]</text>
<line class="section-divider" x1="8" y1="110" x2="1312" y2="110"/>
<text class="section-title" x="10" y="125">Go Bytes (B/op): USE_REMOTE_FETCH / SCAN_AND_FETCH</text>
<text class="row-label" x="153" y="143.0" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.524× [0.523, 0.524]</title><line x1="658.0" y1="139.0" x2="659.0" y2="139.0" stroke-width="2.2"/><line x1="658.0" y1="135.0" x2="658.0" y2="143.0" stroke-width="1.8"/><line x1="659.0" y1="135.0" x2="659.0" y2="143.0" stroke-width="1.8"/><circle cx="659.0" cy="139.0" r="3.8"/></g>
<text class="val-text" x="1137" y="143.0">0.524× [0.523, 0.524]</text>
<text class="row-label" x="153" y="163.0" text-anchor="end">split-all-1000</text>
<g class="ci-win"><title>split-all-1000: 0.692× [0.692, 0.692]</title><line x1="801.6" y1="159.0" x2="801.6" y2="159.0" stroke-width="2.2"/><line x1="801.6" y1="155.0" x2="801.6" y2="163.0" stroke-width="1.8"/><line x1="801.6" y1="155.0" x2="801.6" y2="163.0" stroke-width="1.8"/><circle cx="801.6" cy="159.0" r="3.8"/></g>
<text class="val-text" x="1137" y="163.0">0.692× [0.692, 0.692]</text>
<line class="section-divider" x1="8" y1="172" x2="1312" y2="172"/>
<text class="section-title" x="10" y="187">Go Allocations (allocs/op): USE_REMOTE_FETCH / SCAN_AND_FETCH</text>
<text class="row-label" x="153" y="205.0" text-anchor="end">small-all-1000</text>
<g class="ci-win"><title>small-all-1000: 0.439× [0.439, 0.439]</title><line x1="568.2" y1="201.0" x2="568.2" y2="201.0" stroke-width="2.2"/><line x1="568.2" y1="197.0" x2="568.2" y2="205.0" stroke-width="1.8"/><line x1="568.2" y1="197.0" x2="568.2" y2="205.0" stroke-width="1.8"/><circle cx="568.2" cy="201.0" r="3.8"/></g>
<text class="val-text" x="1137" y="205.0">0.439× [0.439, 0.439]</text>
<text class="row-label" x="153" y="225.0" text-anchor="end">split-all-1000</text>
<g class="ci-win"><title>split-all-1000: 0.406× [0.406, 0.407]</title><line x1="528.1" y1="221.0" x2="529.4" y2="221.0" stroke-width="2.2"/><line x1="528.1" y1="217.0" x2="528.1" y2="225.0" stroke-width="1.8"/><line x1="529.4" y1="217.0" x2="529.4" y2="225.0" stroke-width="1.8"/><circle cx="528.1" cy="221.0" r="3.8"/></g>
<text class="val-text" x="1137" y="225.0">0.406× [0.406, 0.407]</text>
</svg>
<div class="bench-legend"><span><i class="bench-swatch" style="background:#0d9488"></i> 95% CI entirely &lt; 1.000 (faster / lower)</span><span><i class="bench-swatch" style="background:#64748b"></i> 95% CI overlaps 1.000 (unresolved)</span><span><i class="bench-swatch" style="background:#d97706"></i> 95% CI entirely &gt; 1.000 (slower / higher)</span></div>
</figure>

### Final remote / final ordinary (0.5 ms nominal added RTT)

Geometric mean of six paired block ratios, with nominal 95% block-bootstrap
intervals (20,000 resamples, seed 391706). Lower is better.

| Case | Mean ns/op | Go B/op | Go allocs/op | rows/op | prefix-B |
|---|---:|---:|---:|---:|---:|
| small-all-1000 | 0.224 [0.222, 0.226] | 0.524 [0.523, 0.524] | 0.439 [0.439, 0.439] | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] |
| split-all-1000 | **1.053 [1.008, 1.101]** | 0.692 [0.692, 0.692] | 0.406 [0.406, 0.407] | 1.000 [1.000, 1.000] | 1.000 [1.000, 1.000] |

### Absolute values

Medians of the six block medians (and raw repeat min/max across all 18 samples
per arm).

| Case | Arm | Median ns/op (ms) | Min–Max ns/op (ms) | Median B/op | Median allocs/op | prefix-B |
|---|---|---:|---:|---:|---:|---:|
| small-all-1000 | final-ordinary | 107,960,391 (108.0 ms) | 105.4–109.1 ms | 4,457,916 | 51,667 | 19 |
| small-all-1000 | final-remote | 24,030,265 (24.0 ms) | 23.4–25.6 ms | 2,333,757 | 22,662 | 19 |
| split-all-1000 | final-ordinary | 266,416,081 (266.4 ms) | 256.2–301.1 ms | 457,249,481 | 77,262 | 19 |
| split-all-1000 | final-remote | 276,869,638 (276.9 ms) | 258.4–324.3 ms | 316,338,172 | 31,401 | 19 |

## Interpretation and Limits

- **Small unsplit records (`small-all-1000`):** under 0.5 ms added client-server
  RTT, `USE_REMOTE_FETCH` took **0.224 [0.222, 0.226]** of `SCAN_AND_FETCH`'s
  time (24.0 ms vs 108.0 ms, a 4.5× speedup). As a descriptive comparison
  across separate phases (not a controlled causal RTT delta), phase C1's ratio
  at zero added delay was 0.341. `SCAN_AND_FETCH` issues client-driven
  primary-key lookups after scanning the index, whereas `USE_REMOTE_FETCH`
  opens one logical mapped scan (`remote_openings = 1`) and materializes the
  primary records on the server side (wire RPC batch counts are not directly
  instrumented).
- **Two-chunk split records (`split-all-1000`, retained negative result):**
  even with 0.5 ms added client-server RTT, `USE_REMOTE_FETCH` remained
  **5.3% slower** than `SCAN_AND_FETCH` on this single-server setup (**1.053
  [1.008, 1.101]**, 276.9 ms vs 266.4 ms; 4 of 6 block ratios above 1;
  descriptively 1.150 in phase C1 at zero added delay).
- **Why this is not a multi-host result:** in this single-process cluster, the
  storage server holding the index entry and the storage server holding the
  primary record are the **same `fdbserver` process**, so `getMappedRange`'s
  internal secondary-to-primary lookup never traverses the `netem` veth link.
  In a multi-node cluster where index and record shards sit on different
  machines, both client-to-storage and storage-to-storage hops cross the
  network; the multi-node VPC campaign (`tools/benchcampaign/multinode/`) is
  designed to measure that topology directly.
