# R17 pre-registration addendum v2 — owner-authorized server rebuild

This addendum is recorded before any R17 observations. It amends only the fresh-environment acquisition requirement and records the explicitly requested OS image. All endpoint definitions, sample sizes, randomization seeds, CPU/lifecycle scopes, confidence intervals, decision rules, failure rules, and fixed software/source/binary identities in `R17-preregistration-v1.md` remain unchanged.

Original registration SHA256: `2c395126fc090504c21ae7fb4e1ca513aa1727c6082d55e488af851c9c616788`.

## Owner authorization and exact freshness evidence

Root request #1415 relays the owner's decision: reuse the existing paid 16-core instance, rebuild it through the Hetzner rebuild action using fresh `ubuntu-24.04`, record the action ID and timestamps, run R17 and then R18, and delete the VM when authorized performance work is complete. This replaces v1's requirement to provision a different VM allocation. It does **not** establish independent physical-host replication.

- Provider: Hetzner Cloud.
- Server ID: **<server-id>**.
- Server name: **fdb-perf-20261009**.
- Server type: **ccx43**, 16 vCPUs, 64 GiB advertised memory.
- IPv4: **<vm-public-ip>** (unchanged instance address).
- Rebuild action ID: **<action-id>**.
- Provider action command: **rebuild_server**.
- Provider action started: **2026-10-11T14:58:59Z**.
- Provider action finished successfully: **2026-10-11T14:59:15Z**.
- Requested/resolved image: **ubuntu-24.04**, image ID **<image-id>**, x86.
- Guest reports **Ubuntu 24.04.4 LTS**, rather than R16's Ubuntu 26.04.1 LTS.
- Fresh guest boot ID: **271f7a88-6992-4828-a7f8-fdf37a67a159**.
- Guest boot time: **2026-10-11 14:59:22 UTC**.
- Cloud-init reports `status: done` and the rebuild marker is present.
- Before restoring any benchmark artifacts, `/opt/fdbperf` and `/var/lib/foundationdb` were both verified absent. A new empty FDB data directory and new cluster will be created; no old database data is restored.
- The virtual CPU model remains AMD EPYC-Milan, CPUs 0–15, 8 cores/2 threads per core as exposed to the guest. This topology and the fixed runner/server/client CPU sets do not establish physical-host isolation.

Raw sanitized provider responses, image selection, pre-rebuild metadata and fresh-guest observations are under `work-fullstack-r17/rebuild/`. The initial POST was submitted exactly once. Its local observer stopped because it expected the action name `rebuild`, whereas the API returned `rebuild_server`; a separate read-only GET observer confirmed the original action's successful completion. No second rebuild was requested. SSH strict host-key verification remained enabled using the existing per-server host identity. API credentials and SSH private keys are not result artifacts and must never be copied into the measurement archive.

## Frozen stack: exact R16 patch, not newly merged master

The ten selected PRs have subsequently merged, but R17 **does not switch to a new master commit**. It uses the exact source intervention pre-registered in v1:

- Base: `7922a2e5a8e3a2d6cf17dfee8b8134181c88b298`.
- Patch SHA256: `d0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85`.
- PRs #812, #818, #816, #834, #824, #827, #841, #828, #832, #843, in the recorded apply order; no additions or rebase.
- NEW executable SHA256: `93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f`.
- Native CGo executable SHA256: `4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c`.
- libfdb_c SHA256: `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.
- V7 harness archive SHA256: `48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203`.
- FDB server SHA256, preserved from R16: `179df8ec7292ad060fb7bd60a771ff4a8dd3989a8115638356c73feea4fc2c5f`.

Archived executables, runtime files and source/provenance archives are transferred without rebuilding or changing their contents. Historical R16 raw results may be restored as archival files only; they are not added to R17 observations or FDB data. Native CGo continues to declare its actual original source revision `8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead`, not the Go candidate's source revision. The subsequent repository `libfdbc` build-tag fix does not replace these frozen executables.

## Unchanged acquisition and claims

R17 remains 160 paired blocks per workload, 640 serial timing processes, split into 80 predeclared two-block segments with seeds 797001–797080; each segment has both workloads and both arm orders. `read1000` remains 5000 measured/200 warmup; `batchset50` remains 2500/200; one worker and GOMAXPROCS=8 in both arms. Fixed CPU sets remain runner 0–1, server 2–7, clients 8–15.

The five primary endpoints and joint circular moving-block bootstrap gates remain exactly v1: lengths 8 and 16, 100000 resamples each, fixed seeds 797707/797708, nearest-rank two-sided 99% intervals; both upper bounds must be strictly below 1 for an endpoint-specific claim. No R16 pooling, optional stopping, outcome-based exclusions, relaxed validation, additional measurements to rescue a CI, or new runtime telemetry rejection thresholds are introduced.

The new guest/kernel/libc environment is explicitly part of this held-out experiment. Any result or public wording must say **reimaged existing instance with a fresh cluster**, not a new independent VM/host allocation. Successful R17 results remain conditional on the registered workloads, software, guest and sampling assumptions; no fleet-wide or general-capacity claim follows.
