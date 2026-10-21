# fdbmn-20261020

A preregistered repeat of the
[2026-10-14 multi-node campaign](../hcloud-multinode-vpc-20261014/README.md) on
master `fa8725f1d`, run on 2026-10-20 on four dedicated 16-vCPU cloud hosts on a
private VPC: one client host and three FoundationDB 7.3.77 server hosts.

1. **M1 (`M1w1`, `M1w8`, `M1w64`):** three client arms at 1, 8 and 64
   closed-loop workers: **master** (`fa8725f1d`), **old Go** (`267a41434`, the
   final pure-Go tree of the 2026-10-14 campaign) and **CGo** (Apple's Go
   binding over `libfdb_c` 7.3.77). `M1w32` was dropped for budget before
   measurement.
2. **M2 (`M2rl`, `M2skip`):** Record Layer `USE_REMOTE_FETCH`
   (`getMappedRange`) against `SCAN_AND_FETCH` on master, with shard placement
   across 9 storage servers sealed before timing.

## Key findings

Ratios are numerator / denominator: below 1.0 means the numerator has lower
latency, CPU or allocation. Brackets are nominal 95% paired-block bootstrap
intervals. The ratios describe what was measured; none of them identifies a
cause.

- **Master vs old Go.** At 8 and 64 workers master has higher get and set
  latency than old Go, together with substantially lower client CPU on get,
  set and grv and fewer Go allocations per transaction.
  - At 64 workers, p50 is **1.348 [1.308, 1.395]** for get100,
    **1.298 [1.254, 1.331]** for get1000 and **1.283 [1.273, 1.294]** for
    set100. Set100 lifecycle CPU (E−A) per transaction is
    **0.586 [0.572, 0.599]** and Go mallocs per transaction
    **0.561 [0.561, 0.561]**.
  - At 8 workers, p50 is **1.113 [1.097, 1.128]** for get100,
    **1.154 [1.139, 1.172]** for get1000 and **1.137 [1.099, 1.168]** for
    set100. Set100 E−A CPU is **0.553 [0.542, 0.565]**.
  - E−A CPU is lower for every 8- and 64-worker get, set and grv cell
    (0.553–0.817). Go mallocs per transaction are lower in all 14 eligible
    cells (0.555–0.811).
  - Range100 at 64 workers has p50 **1.435 [1.099, 2.378]** and E−A CPU
    **1.180 [0.999, 1.567]**, both with wide, unstable intervals. At 8 workers
    range100 p50 is **1.112 [1.096, 1.129]** and E−A CPU
    **0.865 [0.801, 0.977]**.
- **Master vs libfdb_c.** In every measured get, set and range cell, master's
  mean, p50 and p99 latency is below CGo's or its interval includes 1.0.
  - At 64 workers, p50 is **0.934 [0.911, 0.966]** for get100,
    **0.770 [0.730, 0.821]** for get1000 and **0.893 [0.887, 0.899]** for
    set100.
  - At 8 workers, get100 p50 is **0.977 [0.961, 0.992]**. Get1000
    (**1.003 [0.986, 1.021]**) and set100 (**1.013 [0.978, 1.047]**) are
    unresolved, which is not evidence of equivalence.
  - The one cell where master is above CGo is grv at 8 workers: p50
    **1.025 [1.007, 1.045]** and mean **1.021 [1.002, 1.042]**.
- **Master is closer to libfdb_c than old Go was**, in both latency and
  concurrent CPU.
  - At 64 workers, old Go/CGo p50 is 0.693 (get100), 0.593 (get1000) and
    0.696 (set100), against master/CGo 0.934, 0.770 and 0.893.
  - Set100 E−A CPU relative to CGo is 2.075 (8 workers) and 2.132 (64 workers)
    for old Go, and **1.148 [1.119, 1.179]** and **1.249 [1.213, 1.284]** for
    master.
- **At 1 worker** master has 4.6–9.4% higher mean latency than old Go
  (1.046–1.094) and uses more E−A CPU (1.125–1.292), with fewer allocations.
  It stays faster than CGo: p50 0.636–0.697.
- **Remote fetch.** `USE_REMOTE_FETCH` is slower than `SCAN_AND_FETCH` on every
  1,000-row scan: 1.152–1.220 on the cross-server cases and 2.304 on the
  partially colocated split case (`f = 0.820`). It is also slower on
  `split-rows10-reverse` (1.326) and on reverse Skip 3 of 10 rows
  (**1.169 [1.151, 1.185]**), while allocating fewer Go bytes. The 2026-10-14
  reverse Skip result was 0.158; that comparison across cohorts is
  descriptive. One-row small fetches are about
  2–3% faster remotely.
- **Coverage.** 26 of the 44 planned cases are eligible. Seven are excluded by
  an environment gate, four were not reached after a gate stopped their phase,
  and seven (`M1w32`) were not run. No case was rerun. See
  [Exclusions](#exclusions).

On Linux, master's pure-Go client coalesces outgoing requests before a write
batch, using the same 10 µs minimum and 20 µs maximum delay settings as
`libfdb_c` 7.3.77. The delay depends on time since the last write and is
jittered. [`fdb.WithCoalesceDelay(minDelay, maxDelay time.Duration)`](../../../pkg/fdbgo/fdb/database.go)
changes those settings.

| Phase | Question | Report |
|---|---|---|
| M1 (`M1w1`, `M1w8`, `M1w64`) | Master vs old Go vs CGo (`libfdb_c` 7.3.77) at 1, 8 and 64 workers | [Phase M1](phase-m1-client.md) |
| M2 (`M2rl`, `M2skip`) | `USE_REMOTE_FETCH` vs `SCAN_AND_FETCH` on master across 9 storage servers | [Phase M2](phase-m2-remotefetch.md) |

## Topology and environment

- **Four Hetzner `ccx43` hosts** in `fsn1`: 16 dedicated vCPUs (AMD EPYC Milan)
  and 64 GiB RAM each, on one isolated private VPC with no added network delay.
  Fresh hosts, not the 2026-10-14 machines.
- **Client host:** runs no `fdbserver`. Housekeeping on CPUs 0–1, CPUs 2–7
  idle, benchmark client on CPUs 8–15 (`GOMAXPROCS=8`, `GOGC=100`,
  `GOMEMLIMIT=off`, Go 1.26.9).
- **Three FoundationDB 7.3.77 server hosts** (API 730): `double` redundancy,
  `ssd-2`, 17 `fdbserver` processes, 9 storage servers (3 per host) and
  3 coordinators. Each process is pinned to its own SMT sibling pair; CPUs 0–1
  are reserved for housekeeping and 1 Hz telemetry.
  - `node1`: stateless/coordinator, commit proxy, GRV proxy, resolver and three
    storage servers.
  - `node2` and `node3`: coordinator, transaction log and three storage
    servers each.
- **Trees and binaries:** master `fa8725f1dcf6ff3ac66825ab6e3e819efbc0abcc`,
  old Go `267a41434a81723b07e7150869d702636a1b91a9`, and CGo built from the
  master tree against hash-pinned `libfdb_c` 7.3.77. All client arms were
  built from the same frozen harness (`d94b4c613` plus the #948 lifecycle-drain
  fix; see [Method](#method)).

## Method

- **Order:** placement (P0), then `M2rl`, `M2skip`, data distribution on and
  settled (121.474 s of zero data movement), then `M1w1`, `M1w64`, `M1w8`.
- **M1:** each workload runs six blocks, one for each order of the three arms,
  for 18 fresh processes per workload. Transactions per run, totalled across
  workers: 3,000 at 1 worker, 12,000 at 8 and 32,000 at 64, after warmup.
- **M2:** one master binary per benchmark runs both methods. Six balanced
  blocks, three `testing.B` repeats per process, 3 s benchtime. The metric is
  mean `ns/op`, not a latency percentile.
- **Estimates:** the geometric mean of six paired block ratios, with a nominal
  95% percentile bootstrap over whole blocks (20,000 resamples, seed 492817).
  All three M1 contrasts are estimated directly, never as quotients of other
  ratios. There is no multiplicity correction and no equivalence claim. Worker
  levels ran as separate phases with different transaction counts, so they are
  not compared with each other.
- **Lifecycle CPU (E−A):** process user+system CPU from after binary identity
  hashing to after database close and native network stop, divided by measured
  transactions. It includes startup, warmup, validation and shutdown; it is not
  steady-state transaction CPU. Go allocation metrics exclude the native
  `libfdb_c` heap.
- **Gates:** before each arm, client and server CPU and quiescence gates; after
  each arm, server gates including a 100 MB transaction-log queue limit; after
  each phase, telemetry reconciliation (R4) over all four hosts. A case that
  fails any gate is excluded whole, with no partial ratios.
- **Harness:** all three client arms are built from the frozen harness
  `d94b4c613` plus the #948 lifecycle-drain fix and its matching validator
  change, declared before any timing. The frozen drain check rejects master's
  completed cleanups as outstanding. Workloads, timing, pacing, CPU accounting
  and estimators are those of the frozen harness.

## Exclusions

| Phase | Planned | Clean | Excluded by a gate | Not reached / not run | Reason |
|---|---:|---:|---:|---:|---|
| `M2rl` | 12 | 9 | 3 | 0 | Pre-arm client CPU gate: `split-limit1-reverse`, `split-rows10-forward`, `split-rows1000-forward` |
| `M2skip` | 4 | 3 | 1 | 0 | Pre-arm client CPU gate: `skip3-rows10-forward` |
| `M1w1` | 7 | 4 | 1 | 2 | Pre-arm client CPU gate rejected set100's first arm; the stop-on-failure policy then ended the phase before both batch cases |
| `M1w64` | 7 | 5 | 1 | 1 | Post-arm transaction-log queue gate on batchset50 (426,278,385 bytes); batchset50x1000 not reached |
| `M1w8` | 7 | 5 | 1 | 1 | Post-arm transaction-log queue gate on batchset50 (118,497,454 bytes); batchset50x1000 not reached |
| `M1w32` | 7 | — | — | 7 | Dropped for budget before measurement |

- **Pre-arm CPU gate.** The client gate limits any single benchmark CPU to 15%
  busy and the aggregate to 5%. The failing windows measured 15.2–15.8% (three
  `M2rl` cases), 23.2% with 6.78% aggregate (`M2skip`) and 17.2% (`M1w1`
  set100), as user+system time with no steal. By their recorded timing, all
  five windows overlap a 10 s controller status poll over SSH.
- **Controller polling.** From `M1w8` on, the controller reused one SSH
  connection and polled every 60 s instead of 10 s. Hosts, workloads, timing
  and gates were unchanged. `M1w8` had no CPU-gate exclusions.
- **No re-attempts.** No case was rerun, shortened or replaced with
  qualification data. Capacity and diagnostic runs never enter a ratio.
- **Batch cases.** The first master batchset50 arm at 64 workers completed
  32,000 of 32,000 commits with no errors; the log-queue gate still excludes the
  whole comparison, and that single arm is not reported as a throughput result.

## Changes from the 2026-10-14 campaign

- **Arms.** Three client arms instead of two, so each M1 workload has 18
  processes instead of 12. Old Go is the earlier campaign's final pure-Go tree.
  CGo is built from the master tree.
- **Harness.** The #948 lifecycle-drain amendment applies to all M1 arms. M2
  benchmark sources are unchanged.
- **Coverage.** No 32-worker phase; no 1-worker write cases; 12 of 16 M2 cases
  instead of 16.
- **Placement.** Fresh hosts, freshly seeded data and different shard and
  replica layouts. `split-rows10-reverse` moved from `f = 0` to `f = 1` and
  `split-rows1000-reverse` from `f = 0.696` to `f = 0.820`; see
  [Phase M2](phase-m2-remotefetch.md#comparison-with-the-2026-10-14-campaign).
- **Same revision in both cohorts.** Old Go is the revision the earlier campaign
  measured as its final tree, so its ratio to CGo can be compared across the two
  campaigns as a description of cohort variation; see
  [Phase M1](phase-m1-client.md#comparison-with-the-2026-10-14-campaign).
  Cross-campaign comparisons are descriptive only: no blocks are pooled and no
  cross-campaign interval is computed.

<!-- website: end of published content -->

## Protocol and records

| Path | Content |
|---|---|
| [RESULTS.md](RESULTS.md) | Detailed analysis: every contrast, coverage, historical joins, operational findings, verification and regeneration. |
| [PROTOCOL.md](PROTOCOL.md), [protocol.json](protocol.json), [orders.json](orders.json) | Preregistered protocol, phases, seeds and counterbalanced orders. |
| [REPRODUCIBILITY.md](REPRODUCIBILITY.md) | Source trees, frozen harness `d94b4c613` and runtime identities. |
| [RUN-DAY.md](RUN-DAY.md) | Run-day checklist written during preparation. |
| [EXECUTION-NOTES.md](EXECUTION-NOTES.md) | Execution log: pre-measurement findings and corrections, per-phase outcomes, decisions. |
| [decisions/](decisions/) | Post-run decision records: no A1 re-attempt (`no-A1-decision.json`), the pre-arm CPU burst attribution, and the operator-side polling deviation from M1w8 on (`polling-ops60-*.json`). |
| [build-check/](build-check/) | Build verification; the `#948` lifecycle-drain deviation patch applied to the frozen harness (`cleanup-compat-*.patch`, amendment record with per-file sha256). |
| [placement/](placement/) | Shard-placement P0 freeze, resume and live-seal procedures, and their sha256 lists. |
| [ANALYSIS-ADAPTER.md](ANALYSIS-ADAPTER.md) | Offline analysis adapter and its controls. |
| [analysis/](analysis/) | Public aggregate outputs: M1 and M2 comparisons, absolutes and historical joins (CSV), coverage and method. |
| [MANIFEST.sha256](MANIFEST.sha256) | sha256 of the preparation documents and command files. |

### Known corrections

The protocol is not edited for these, because its bytes are a hashed input (the
in-tree `protocol.json` differs from the sealed file only by the sanitization
recorded in `docs/perf/IMPORT-PROVENANCE.tsv`, which also holds the sealed
file's sha256). Reviews found:

- `protocol.json:603` (`server_pre_arm`) explains the 60 s storage durability-lag
  gate with "(FDB 7.3.77 TARGET_DURABILITY_LAG_VERSIONS = 5 s normal storage write
  batching cycle)". That is wrong. In FoundationDB 7.3.77 the knob is a
  ratekeeper limit, passed into its normal and batch limits
  (`fdbserver/Ratekeeper.actor.cpp:719-735`), not the storage batching interval.
  `fdbclient/ServerKnobs.cpp:905` sets it to `350e6` versions, 350 s at
  `VERSIONS_PER_SECOND` = `1e6` (`:36`); the batch variant
  `TARGET_DURABILITY_LAG_VERSIONS_BATCH` is `150e6` (`:907`). The storage commit
  interval is `STORAGE_COMMIT_INTERVAL` = `0.5` s (`:1010`). The 60 s gate itself
  is protocol policy and is unaffected.
- `protocol.json:571` (`versioning_finding`) states that every data-distribution
  transaction that changes the shard map calls `checkMoveKeysLock`. In 7.3.77 that
  holds only for the default `SHARD_ENCODE_LOCATION_METADATA` = `false`
  (`fdbclient/ServerKnobs.cpp:190`). With that optional feature on,
  `cleanUpSingleShardDataMove` (`fdbserver/MoveKeys.actor.cpp:630-702`, which
  asserts the knob at `:636`) runs its own transaction: it rewrites `keyServers`
  (`:689-690`) and commits (`:702`) without `checkMoveKeysLock`. `startMoveShards`
  calls it (`:1749-1752`) and then retries, so the caller's lock check at `:1626`
  does not cover it; the path is selected only when the feature is enabled
  (`rawStartMovement`, `:3102-3126`). This is a correction to the protocol's
  claim about 7.3.77, not a claim about the measurements.

### Frozen tools

The campaign's tools are in
[`tools/benchcampaign/campaigns/fdbmn-20261020`](../../../tools/benchcampaign/campaigns/fdbmn-20261020/).
The campaign executes its own pinned copies; the in-tree copies are records.

| Tool | Pinned sha256 | In-tree copy |
|---|---|---|
| `execution/collect_final.py` (collector) | `8ebe4b4224c3ecf858414203cad8ff7b2256b6b55224b9d2a78fedfa99631217` | byte-identical |
| `execution/analyze_campaign.py` (analysis adapter) | `700fd3c18a319ea6738c00652846313797a4f4bfbcf50e4b3aee37e720a70289` | two default paths written as `${FDB_PERF_HOME}` |
| `execution/test_analyze_campaign.py` | `1d622f864dfa0fb3785af9f08d6e609bee4cdd059edc0b34b449dc8cad5fb967` | byte-identical |

`verify_frozen.sh` checks all three; for the adapter it restores the original
paths from `FDB_PERF_HOME` before hashing. The multi-node and single-node tooling
the campaign deployed is identical to `tools/benchcampaign/{multinode,singlenode}`
at `fa8725f1d`. Later in-tree changes to those tools (strict JSON parsing of hcloud
lists, `inventory.py`, `--protect-file`, test skips replaced by fixture assertions)
postdate the campaign, so the in-tree tools now differ from what was deployed.

From M1w8 on, the phase controller ran as `execution/run_phase_ops60.py` with
`remote_ops60.py`: the operator-side polling deviation in
[decisions/](decisions/), which reuses one SSH connection and polls every 60 s
instead of 10 s; the protocol and the frozen harness are unchanged.
`execution/enable_dd_for_m1.py` is the M2→M1 data-distribution boundary step.
`//tools/benchcampaign/campaigns/fdbmn-20261020:fdbmn_20261020_test` runs the
adapter's and phase controller's offline controls.

Cloud resource IDs, the resource-label suffix and the names and address ranges
of unrelated resources the campaign protected are removed; see
[docs/perf/README.md](../../perf/README.md#paths-in-imported-documents).
