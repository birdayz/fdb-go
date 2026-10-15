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
