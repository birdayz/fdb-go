# R17 confirmatory performance pre-registration — v1

Status: protocol frozen before any R17 VM work or R17 observations. Perf-lead's STACK-FINAL ACK has been received. Execution is blocked pending the owner's provisioning of a fresh VM and root's authorization to use it. No R17 work may run on the R16 VM. R18 GOMAXPROCS diagnostics are a separate, lower-priority exploratory study and cannot select or modify this protocol.

This document is adjacent to the sealed R16 results. Any change requires a separately named, dated amendment approved before R17 observations; never silently overwrite this registration. No rebase onto moving master, additions to the stack, outcome-based sample-size changes, or replacement of unfavorable runs.

## 1. Frozen implementation and comparator

Base: `7922a2e5a8e3a2d6cf17dfee8b8134181c88b298`.

Stack patch: `${FDB_PERF_HOME}/results/perf-lead-patches/fullstack-on-7922a2e5a.patch`.

Patch SHA256: `d0a6ae57ac1143aa1247ce2461f84f74264825f901a53cb7ab943a0efe83be85`.

NEW is byte-identical to R16 NEW. Exact PR contents, in apply order, as confirmed by perf-lead:

| PR | Commit identity supplied by perf-lead | Change |
|---|---|---|
| #812 | `03ef49533` | keyRangeEqualsKeyAfter memcmp |
| #818 | `a7af793cf` | commit-capture slab borrow |
| #816 | `239e5a8cd` | buffered readLoop |
| #834 | `d7d49c821` | raw-byte liveness count |
| #824 | `b950fc0f1` | lone GRV inline |
| #827 | `275875031` | idle-writer non-blocking direct write |
| #841 | `4fdc0d4c2` | lone pipelined Get via direct write |
| #828 | `5bb36ed8f` | commit GRV CAUSAL_READ_RISKY |
| #832 | `485425301` | lease on stack |
| #843 | `8d36879e7` | reply-error classification without allocating |

The full base and patch hash, not short commit labels, are the exact source identity. Read-CPU changes are #816, #824, #827, #841, #843, plus the nominally neutral #834. No further read-CPU change goes into R17. The CAUSAL_READ_RISKY correction is an explicit part of the stack, not silently equated with the old baseline's behavior.

Comparator: unchanged native FoundationDB 7.3.77/API 730 through the V7 CGo executable. No OLD arm is required; all confirmatory ratios are NEW/CGo.

Frozen artifacts, reused rather than rebuilt to different identities:

- NEW executable SHA256: `93b8d79f05a5b1620c9c51a1e5dd7515943adcda269333a4b2561f5db11e7f2f`.
- CGo executable SHA256: `4c89840d7e8d83d30534747dc0ae6ab2a1fc68b504db5699d9aa6418397cdb1c`.
- libfdb_c SHA256: `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.
- Reviewed V7 harness archive SHA256: `48b9a56e70aa3d6c43c3760a3c8e43e6f3d70e6cd1805757de7f74518f177203`.
- Isolated wrapper archive SHA256: `8f340663085b961230046bf440e1e80df27e24976219fe3b6e3ae6a1193f1a6a`.
- Harness reviewed source revision: `06097ca4609b1b927a9827f4fff5e8cedf358e46`.
- R16 source/provenance archive SHA256: `4701f00b86ce3313a60152e39b34ebc71506fac3f485de222ff9747f67e10957`.

CGo declares its actual original source revision `8ab5c5a4d6ee4858a4eacd6ac58e2a7be85dbead`; it must not be relabeled as built from the R17 base. The archived root dependency/build-pin equivalence diff is empty. Source strings are declarations, not compiled-source proofs; binary hashes and source archives bind provenance.

## 2. Workloads, host, and lifecycle accounting

Use a newly provisioned VM and fresh FDB cluster, not the R16 host, a clone of its live database, or a reset of the old benchmark process. Record owner-provided allocation identity, provisioning time, CPU model/topology, kernel, OS, FDB server/configuration hashes, library identities, cluster identity, and complete command/environment pins before the first measured process. Do not provision or access owner credentials on this agent's initiative.

Fixed Go 1.26.9, FDB 7.3.77/API 730. Client affinity is CPUs 8–15, server CPUs 2–7, runner/analysis CPUs 0–1. GOMAXPROCS=8 in both arms, GOGC=100, GOMEMLIMIT=off, GODEBUG empty. Reject FDB_NETWORK_OPTION_* overrides. No builds, profiling, strace, perf, concurrent experiments, or offline analysis during timing. Use the frozen V7 quiescence/telemetry and source/runtime identity checks; no VM tuning selected from results.

Each fresh process uses one worker and a unique, equal-length 68-byte prefix:

- `read1000`: 5,000 measured transactions, 200 warmup transactions.
- `batchset50`: 2,500 measured transactions, 200 warmup transactions, 50 writes/transaction and the unchanged 100-byte batch value workload.
- Transaction timeout 30,000 ms; callback retry limit 10, but valid confirmatory runs must observe zero callback retries and errors. Internal RPC retries are not directly observed by that count.

V7 accounting is unchanged:

- A = process stats after executable/library identity work, before opening the client.
- B = before the measured workload; C = after the measured workload.
- D = after bounded finalizer fixpoint drain and native FIFO barrier.
- E = after final drain, database close, and completed native network-thread shutdown.
- Window CPU is `(C-B)/N`; shutdown-inclusive CPU is `(E-A)/N`.
- E–A includes client startup, fixture, warmup, validation, CSV, forced GC, and shutdown, and excludes preceding identity hashing and final JSON/process exit. It is not isolated steady-state CPU.
- GC-drain accounting costs can differ by arm. Absolute E/N is retained in raw/absolute tables only, never used in a paired client-cost comparison.

## 3. Fixed sample size and execution order

Collect exactly **160 paired blocks per workload**: 160 NEW and 160 CGo processes for reads, and the same for batches; **640 timing processes total**. One global block contains an adjacent NEW/CGo pair for each workload (four processes), with workload and arm order randomized in advance. Every workload has 80 NEW-first and 80 CGo-first pairs.

Keep the frozen V7 runner's maximum 180-second campaign cap. Implement the long experiment as **80 predeclared serial segments, two global blocks each**. Each segment contains both workloads and both arm orders exactly once per workload, generated by the unchanged runner's balanced-order scheduler. Global block = 2*segment + local block. Segment number is 0–79; its fixed schedule seed is `797001 + segment`. The plan's workload input order is `[read1000,batchset50]`, and arm input order `[new,cgo]`; the frozen scheduler randomizes workload order within each block. All 80 segment plans and their complete schedules must be generated, checked, and SHA256-pinned before the first segment begins. Fresh prefixes distinguish segments. No order or prefix is regenerated in response to outcomes.

Each process retains the existing 2-second cooldown and 1-second quiescence measurement plus runner overhead. Segmentation adds only orchestration boundaries, not data-dependent stopping or new analysis. No comparison tables, confidence intervals, or performance-based decisions are produced until all 160 global blocks are complete. Operational checks may inspect exit status, identity, schema, exact data validation, retries, lifecycle gates, and host-health thresholds only.

## 4. Five frozen primary endpoints

All effects are paired NEW/CGo ratios. For each block and endpoint, take the ratio of that block's run statistics, then take logs. The reported effect is the exponential of the mean of those 160 paired log ratios (geometric mean ratio), not a ratio of medians and not a transaction-level pooled estimate.

1. `batchset50` shutdown-inclusive process CPU/transaction, `(E-A)/N`.
2. `batchset50` measured-window process CPU/transaction, `(C-B)/N`.
3. `read1000` mean completion latency.
4. `read1000` p99 completion latency, using the unchanged V7 sample quantile definition.
5. `read1000` shutdown-inclusive process CPU/transaction, `(E-A)/N`.

All other endpoints (read window/drained CPU, user/system split, batch latency, allocations, RSS, GC counts, throughput) are secondary/descriptive. They cannot replace a primary endpoint that fails, and any later claim about them requires separately identified exploratory wording or a new confirmatory registration. Do not pool R16 pilot observations into R17 inference.

## 5. Frozen confidence intervals and decision rules

Use two-sided **99% percentile intervals per primary endpoint**, corresponding to a Bonferroni allocation of the familywise 5% error budget across the five primary endpoints. Nominal coverage remains conditional on the block-resampling assumptions; the calculation is not proof of independent VM/fleet replication.

Resample the vector of all five endpoint log ratios jointly, preserving their common global-block identity. To address local serial dependence, the primary resampling is a circular moving-block bootstrap with length **8 global blocks**: sample 20 starting indices uniformly with replacement from 0–159, append the eight consecutive modulo-160 indices at each start, and average the resulting 160 logs. Use **100,000** resamples and Python's `random.Random(797707)`; common index draws across all endpoints. Exponentiate each resampled mean. The interval endpoints use the existing V7 nearest-rank empirical quantile convention at 0.005 and 0.995: after sorting M resampled values, use zero-based index `max(0,ceil(p*M)-1)`.

Run a mandatory longer-dependence sensitivity with length **16 global blocks**, ten sampled starts per resample, 100,000 resamples, seed `797708`, and the same quantile convention. Publish both intervals. This additional gate can only prevent a claim, not rescue one. Do not tune bootstrap lengths/seeds from observed results.

A claim of improvement for an endpoint requires **both predeclared intervals wholly below 1**, i.e. each upper bound strictly `< 1`, and every data/host-validity gate passing. An upper bound equal to or above 1 means **not confirmed/inconclusive**, regardless of the point estimate or other metrics. A point estimate above 1 is reported, not hidden. No equivalence/noninferiority claim follows from failure to find a difference. No optional stopping, post-hoc endpoint substitution, selective arm exclusion, or favorable-run replacement is allowed.

If all five pass, the compound claim must still be scoped to these workloads, counts, protocol, software, and fresh VM. If only some pass, name the passing endpoints individually and explicitly report the failed primary endpoints. Do not claim fleet-wide superiority, general capacity, or universal Go-vs-C++ performance from one fresh VM. R17 is held-out confirmation of the selected R16 stack, not a new causal decomposition of its PRs.

## 6. Sample-size rationale (pilot-only, before R17)

R16 NEW/CGo read E–A ratios, in block order:

`[1.1308005361572568, 0.8437083879254602, 0.8320993403330111, 0.9603474007683825, 1.1178754073247472, 0.7841773429038541]`.

Their sample log-standard deviation is **0.15739946062930055** (only six pilot pairs). The target true ratio 0.93 has absolute log effect **0.07257069283483537**. For a two-sided 99% interval and 90% power, the normal approximation is:

`n = ceil(((z_0.995 + z_0.90) * sigma / abs(log(0.93)))^2)`

with `z_0.995=2.5758293035489` and `z_0.90=1.2815515655446008`.

- At the pilot sigma: **70** pairs; balanced/operational rounding gives 72.
- At **1.5 times** the pilot sigma: **158** pairs; round to the frozen **160**.

The allowance addresses uncertainty in a six-pair pilot and moderate dependence/extra variability; it is not a proven 90% power guarantee for arbitrary host drift or the dependence-robust bootstrap gates. The actual precision can be worse on the fresh VM. If the CI crosses 1, accept an inconclusive result instead of adding blocks.

Making read E–A the only primary endpoint **while retaining 99% two-sided confidence** does not reduce its required pair count: the five-endpoint design already uses 99% for each endpoint. A smaller 72-pair design drops the 1.5x sigma allowance, not an existing multiplicity penalty; it must not be advertised as equally protected against pilot uncertainty. Such a design is not selected in v1.

## 7. Pre-experiment cost estimate from measured R16 artifacts

R16 used three-arm blocks. The two-arm costs below are extracted NEW+CGo process-wall sums from those blocks, not falsely described as always-contiguous observed two-arm spans. Runner gaps are measured from adjacent R16 process timestamps, and two gaps are amortized per projected R17 pair.

| Workload | Mean NEW+CGo process wall/pair | Observed process-wall range | Mean inter-run gap | Projected pair including two gaps | 160 pairs |
|---|---:|---:|---:|---:|---:|
| read1000 | 4.863917 s | 4.199908–5.227840 s | 3.079461 s | 11.022840 s | 29.394 min |
| batchset50 | 11.535781 s | 11.139104–11.869146 s | 3.071086 s | 17.677953 s | 47.141 min |

Combined projected timing: **76.535 minutes (~1 hour 17 minutes)**, plus segment startup/identity checks and final analysis/archive work. A roughly two-hour working reservation is reasonable on comparable hardware; owner's provisioning/queue time is additional and unknown. This is far below the 12-hour cutoff, so v1 retains 160 pairs/workload. This estimate is not permission to start without the fresh-VM handoff.

## 8. Validity, failures, and reporting

Before execution, pin the fresh target identity, FDB server/configuration, exact transferred binaries/libraries/harness, all plans/schedules, and acquisition/analysis scripts in an execution manifest and notify root of its hash. Check them again after acquisition. An identity mismatch, nonzero callback retry/error, wrong byte result, failed lifecycle drain, timeout, quiescence/steal failure, missing sample, or incomplete block invalidates confirmatory completion. Preserve every artifact and report the exact failure. Do not silently discard or rerun that block, extend beyond 160, or pool a partial campaign with a later restart. Any new acquisition after a failed attempt requires a separately registered restart decision with the failed attempt disclosed.

No performance outlier trimming. No exclusions based on which arm is faster. Report all 640 process records, all 160 paired ratios per endpoint, raw A–E counters, GC/finalizer rounds, validation/telemetry, randomized order and segment IDs, both predeclared confidence intervals, and every primary decision. Archive exact source/provenance and the pre-registration; verify every archive file hash on the VM and after download. No tracing or profiling data is mixed with confirmatory timing.

Public wording is gated on R17 results and source/review/CI correctness gates separately. Benchmark success is not proof of parent correctness, clean CI, or permission to merge.
