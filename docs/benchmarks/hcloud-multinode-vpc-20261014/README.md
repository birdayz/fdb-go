# hcloud-multinode-vpc-20261014

A preregistered multi-node benchmark campaign across **four dedicated 16-vCPU
cloud hosts on an isolated private VPC**, measuring:

1. **M2 (`M2rl` and `M2skip`):** Record Layer `USE_REMOTE_FETCH`
   (`getMappedRange`) against `SCAN_AND_FETCH` when the client and three
   FoundationDB server machines communicate over a real VPC and index shards and
   record shards are placed across 9 storage servers.
2. **M1 (`M1w1`, `M1w8`, `M1w32`, `M1w64`):** the final pure-Go client
   (`pkg/fdbgo`) against Apple's Go binding over `libfdb_c` 7.3.77 at 1, 8, 32,
   and 64 closed-loop workers against a 3-node `double`-redundancy `ssd-2`
   cluster.

every phase's plan and shard-placement labels were sealed before timing, every
run was executed once in a counterbalanced block order, 1 Hz server telemetry
was collected on all three server hosts and reconciled with conservative clock
bounds (`R4`), and all adverse or omitted cases are retained and reported.
Intervals below use the protocol's preregistered bootstrap seed `492817`
(20,000 resamples; initial exploratory runs with seed `391706` are retained in
the campaign archive).

## Topology and environment

- **Four Hetzner `ccx43` hosts** in `fsn1`: 16 dedicated vCPUs (AMD EPYC Milan,
  8 cores × 2 SMT threads, one NUMA node, `2k,2k+1` sibling pairs verified on
  all four hosts) and 64 GiB RAM per machine.
- **Isolated `/24` private VPC** with host-level `nftables` firewall
  rules and Hetzner Cloud Firewall rules. Unloaded 100-packet client-to-node
  VPC ping medians (`p50` / `p99`): node1 `0.374 / 0.971 ms`, node2 `0.412 /
  1.400 ms`, node3 `0.328 / 1.390 ms` (0% packet loss).
- **Client host:** runs **zero `fdbserver` processes**.
  Housekeeping/runner pinned to CPUs `0–1`, idle sentinel CPUs `2–7`, benchmark
  client pinned to CPUs `8–15` (`GOMAXPROCS=8`, `GOGC=100`, `GOMEMLIMIT=off`,
  Go 1.26.9).
- **Three FoundationDB 7.3.77 server hosts:** 17 `fdbserver`
  processes total, each pinned to its own disjoint SMT sibling pair (`CPUs 0–1`
  reserved for housekeeping and the 1 Hz `node_sampler.py`), configured as
  `double` redundancy `ssd-2` with `commit_proxies=1`, `grv_proxies=1`,
  `resolvers=1`, `logs=2`, and **9 storage servers** (3 per node):
  - `node1`: `4500` stateless (`cluster_controller`, `master`,
    `data_distributor`, `ratekeeper`, `consistency_scan`, `coordinator`) on
    CPUs `2–3`, `4501` `commit_proxy` on `4–5`, `4502` `grv_proxy` on `6–7`,
    `4503` `resolution` on `8–9`, `4504–4506` `storage` on `10–11`, `12–13`,
    `14–15`.
  - `node2` and `node3`: `4500` `coordinator` on `2–3`, `4501` `transaction`
    (tLog) on `4–5`, `4502–4504` `storage` on `6–7`, `8–9`, `10–11`.
- **Tree and binaries:** final tree `267a41434a81723b07e7150869d702636a1b91a9`
  with the frozen benchmark harness `d94b4c61377d173b7c6884fde426a227dab0f0bb`
  ([campaign-harness.md](../campaign-harness.md)), reusing the exact compiled
  binaries verified in [`hcloud-rebench-20261014`](../hcloud-rebench-20261014/README.md)
  (`reuse_only` scope). The campaign tooling is in
  [`tools/benchcampaign/multinode`](../../../tools/benchcampaign/multinode/README.md).

## Phases

| Phase | Question | Report |
|---|---|---|
| M2 (`M2rl`, `M2skip`) | Multi-node VPC `USE_REMOTE_FETCH` vs `SCAN_AND_FETCH` across 9 storage servers (`double` `ssd-2`) | [phase-m2-remotefetch.md](phase-m2-remotefetch.md) |
| M1 (`M1w1`, `M1w8`, `M1w32`, `M1w64`) | Multi-node VPC pure-Go vs CGo (`libfdb_c` 7.3.77) at 1, 8, 32, and 64 workers | [phase-m1-client.md](phase-m1-client.md) |

## Headline findings

- **M2 (`USE_REMOTE_FETCH` vs `SCAN_AND_FETCH` on a 3-node `double` `ssd-2`
  cluster):**
  - On this 3-node `double`-redundancy `ssd-2` cluster (which differs from the
    single-node campaign simultaneously in host topology, network, `ssd-2` vs
    `memory` storage engine, `double` vs `single` redundancy, and storage-server
    count), **1,000-row scans were slower with `USE_REMOTE_FETCH` than with
    pipelined `SCAN_AND_FETCH`**:
    - `small-rows1000` (100% `cross-server`): **1.226 [1.175, 1.280]** forward
      (117.1 ms vs 95.1 ms) and **1.255 [1.217, 1.293]** reverse (119.0 ms vs
      96.4 ms), despite cutting client Go allocations by 57% (`allocs/op`
      0.431).
    - `split-rows1000` (`partially colocated (f = 0.6960)`): **2.326 [2.254,
      2.397]** forward (742.5 ms vs 316.4 ms) and **2.300 [2.206, 2.384]**
      reverse (769.0 ms vs 327.6 ms).
    - `skip3-rows1000` (100% `cross-server`): **1.360 [1.295, 1.414]** forward
      and **1.324 [1.250, 1.407]** reverse.
  - On small result sets, `USE_REMOTE_FETCH` was faster or comparable in
    specific shapes:
    - `skip3-rows10-reverse` (`cross-server`): **0.158 [0.155, 0.160]** (4.87
      ms vs 31.13 ms, a 6.4× speedup).
    - `split-rows10` (`partially colocated (f = 0.0000)`): **0.873 [0.829,
      0.920]** forward and **0.854 [0.823, 0.886]** reverse.
    - `small-limit1` (`cross-server`): **0.979 [0.964, 0.995]** forward and
      **0.985 [0.969, 1.003]** reverse.
    - `small-rows10` (`cross-server`): **1.031 [1.018, 1.042]** forward and
      **1.026 [1.013, 1.039]** reverse; `split-limit1` (`cross-server`):
      **1.110 [1.087, 1.131]** forward and **1.114 [1.095, 1.138]** reverse.
  - All 16 cases across `M2rl` (432 samples) and `M2skip` (144 samples) passed
    before/after placement stability checks and reconciled **100% clean** in
    R4. These results support retaining `SCAN_AND_FETCH` as the default for the
    measured workloads and conditions.
- **M1 (`final pure-Go` vs `CGo` over a real VPC to a 3-node `double` `ssd-2`
  cluster):**
  - **At 1 worker (`M1w1`, 7/7 clean):** pure-Go beat CGo on mean latency, p99
    latency (6 of 7 intervals below 1; `get100` p99 interval `0.865 [0.734,
    1.037]` includes 1), and shutdown process CPU (`E−A`) across every workload:
    `grv` **0.579**, `get100` **0.702**, `get1000` **0.673**, `range100`
    **0.633**, `set100` **0.886**, `batchset50` **0.920**, and `batchset50x1000`
    **0.954** (`E−A` CPU ratios **0.570–0.748**; `batchset50` `0.667`).
  - **At 8, 32, and 64 workers (`M1w8`, `M1w32`, `M1w64`, 5/7 clean per
    phase; within-phase arm contrasts only, as worker levels ran in a fixed
    phase sequence with different transaction counts):**
    - On point reads (`get100`, `get1000`) and single-key commits (`set100`),
      pure-Go had lower mean latency than CGo with nominal 95% intervals below
      1 at all three worker levels (`get100` **0.888 [0.866, 0.907]** at 8w,
      **0.770 [0.750, 0.793]** at 32w, **0.676 [0.644, 0.710]** at 64w;
      `get1000` **0.942 [0.921, 0.961]** at 8w, **0.715 [0.688, 0.747]** at
      32w, **0.632 [0.601, 0.669]** at 64w, with 64w `get1000` p99 **0.385
      [0.336, 0.438]**; `set100` **0.980 [0.966, 0.993]** at 8w, **0.938
      [0.931, 0.946]** at 32w, **0.887 [0.869, 0.897]** at 64w).
    - On `range100`, pure-Go was faster at 8w (mean **0.868 [0.846, 0.891]**,
      p99 **0.925 [0.861, 0.987]**) and 64w (mean **0.554 [0.389, 0.673]**, p99
      **0.732 [0.506, 0.912]**), whereas at 32w the mean interval included 1
      (**0.869 [0.753, 1.148]**) and p99 was slower for pure-Go (**1.247
      [1.048, 1.644]**).
    - On `grv` under concurrency, pure-Go and CGo had unresolved mean latency
      differences at 8w (**0.996 [0.949, 1.038]**) and 64w (**1.029 [0.990,
      1.069]**) and a 4.1% higher mean for pure-Go at 32w (**1.041 [1.007,
      1.081]**), while pure-Go used **less** shutdown process CPU (`E−A`) per
      transaction than CGo at every worker level (**0.904** at 8w, **0.765** at
      32w, **0.736** at 64w).
    - On `get100`, `get1000`, `range100`, and `set100` at 8, 32, and 64
      workers, pure-Go used more shutdown process CPU (`E−A`) per transaction
      than CGo (`1.291–2.162×` across those cases, except 64w `range100` where
      the interval included 1 at `1.069 [0.923, 1.180]`).
    - At 8, 32, and 64 workers, `batchset50` (50 writes per commit across 8–64
      concurrent workers) pushed the FDB tLog queue past the preregistered 100
      MB `adapter-status-after` ceiling (`113.7 MB`, `193.4 MB`, `141.9 MB`),
      so `R1` and `R4` omitted `batchset50` (`contaminated`), and the phase
      driver conservatively stopped the phase before `batchset50x1000` (retained
      as unexecuted / R4 `unverifiable`, rather than a measured negative).
