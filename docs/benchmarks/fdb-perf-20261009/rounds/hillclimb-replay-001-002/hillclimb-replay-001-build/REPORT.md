# Replay hillclimb001 — experimental, NOT promotion-ready

## Candidate and authority

Root#421 authorized scratch plain-Set replay optimization, short matched hillclimb and allocation-component profiles on the existing VM. Owner explicitly requested unfrozen hillclimbing and no regressions. No active/ready worktree edits, provisioning, deletion or baseline replacement occurred.

The matched Go baseline is the read-order WIP export `/tmp/astra-hillclimb-readorder-base/client.tar.gz` SHA256 `5dc3a6fa61806b8ab5ecbcb4dd4c94bd9e279b894d8293461517b316635c5b17`, manifest `c8aed1c8956b4155c0232917bd5aeca252c35913f0e74dc0d900c87cf4a3a401` (757 files). **It is not the previous ownership campaign baseline.** Root reported wholefast101(17actual84cached) and wholefull152(51actual101cached), but explicitly disclosed pending watch-view/conflict mismatch and quadratic COW corrections. These results cannot approve that base's correctness or describe later root changes.

Scratch delta `replay-001.patch` SHA256 `e09c60513f3d90fbebadba5f4000a9a8771fcc005acbb575c8a06352d524ebd8`; candidate manifest `6a714846bc5ff68f8ebe6d7d61c235b7a3a4bc3df14e77c24d5e375973e65c73`. Only `ryw.go` and existing Bazel-owned `ryw_coalesce_test.go` changed. On an all-plain-Set validated snapshot, clone its mutation vector, stable-sort by byte key only if unordered, retain the last equal key. Borrow immutable owned operand bytes until unchanged `captureCommit`; never read live RYW. Mixed mutations fall back to existing replay. Validation, versionstamp/RYW-disabled carveouts, size gate and conflict processing are unchanged.

Before writing code, actual C++ tag7.3.77 was read: `WriteMap.cpp:125–137` (Set replaces a readable key's stack), `ReadYourWrites.actor.cpp:2020–2070` (key-ordered mutation emission).

Four main-module tests added: manually expected binary/empty/duplicate/ordering/snapshot-suffix cases; rejection of five representative non-Set types at every position; 1,000 deterministic all-Set cases versus old RYW replay plus size equality; and <=1 allocation for sorted50×1000B coalescing (intended dispatch-revert sentinel). **These added main-module tests have not been executed by the measurer.** Root owns their Bazel/revert/race/differential validation. Remote standalone harness tests passed15/15 for each Go arm; those are not evidence that the four new tests passed. At report time, parent test/review results had not been received. Reviewer-agent creation hit the50-agent cap; existing hn-client/hn-codehealth reviewers were notified read-only.

Binaries:
- baseline `candidate-hc01-base`: `a5beceafe01412f92adc4e4e335a63047bfb68918ec5924e4815a7f88ec21e14`
- replay `candidate-hc01-replay`: `81218e25f8500d22bfa1378fb91ef302bb4f58a67da110a48f01acbbd7a7c9a5`
- immutable native `candidate-v6-cgo`: `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`

All use immutable V6 harness archive `3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`. CompilerGo1.26.9, native/server7.3.77API730. All757 source hashes verified before and after builds, along with prior binary/runtime hashes.

## Matched batch campaign

`hillclimb-replay-001`:36/36 runs complete/validated, 09:33:51–09:36:04UTC on2026-10-10. Six paired blocks per workload, all six baseline/replay/native orders,100 measured40 warmup,workers1. Unique68Bprefixes/88Bkeys across both50×100B and50×1000B workloads. Exact final persisted bytes checked outside timing. No measured/warmup errors or retries; all gates passed. Per Go arm/workload:600GRV replies/readversions and600started/completed commits, cache/connection/retry counters0. Native counters remain unobserved.

Ratios below are geometric means of six paired **run-level** ratios, not pooled-operation ratios; lower is better.

| Workload | Comparison | Mean | p50 | p99 | CPU/tx | Go B/tx | Go allocs/tx |
|---|---|---:|---:|---:|---:|---:|---:|
|50×100B|replay/base|0.987362|0.995038|0.971902|0.913156|0.767715|0.654758|
|50×1000B|replay/base|0.973723|0.984666|1.042997|0.947443|0.787788|0.651733|
|50×100B|replay/native|0.994615|0.988397|1.115824|1.292648|not comparable total heap|not comparable total heap|
|50×1000B|replay/native|0.981079|0.987970|1.013624|1.066762|not comparable total heap|not comparable total heap|

CPU improved in6/6 small and5/6 large pairs; allocated bytes and allocation counts improved in6/6 for both. Mean latency improved only3/6 small and4/6 large pairs. Large p99 was worse in4/6 pairs (ratio range0.865621–1.369917): **not an across-the-board latency win**.

Run-median resources base→replay:
- Small:1,190.990→1,080.955µsCPU/tx;114,070.96→87,663.20B/tx;465.99→305.51allocs/tx. Measured GC cycles summed over six runs33→29.
- Large:1,298.390→1,236.300µsCPU/tx;352,283.04→276,757.88B/tx;465.44→303.435allocs/tx. GC12→7.

## Broader controls — open regression signals

`hillclimb-replay-001-controls`:48/48 runs complete/validated,09:40:06–09:42:47UTC. Six paired blocks for each of `read1000`, `get1000`, `range100`, `set100`, baseline/replay only (3AB/3BA per workload),100 measured40warmup. Same binary/source/options/environment. Native was not repeated here; no native-control claim. All raw/persisted checks passed, errors/retries/cachehits0. Each arm/workload600GRVs; only set100 records600write commits, others0native commits.

|Control|Mean replay/base|p50|p99|CPU/op|Go B/op|CPU pairs below1|
|---|---:|---:|---:|---:|---:|---:|
|read1000|1.031127|1.028331|0.981659|1.022755|0.991020|3/6|
|get1000|0.972850|1.002683|0.826865|1.018733|0.975190|4/6|
|range100|1.023577|0.999009|1.087273|1.041489|0.999736|1/6|
|set100|1.052323|1.059971|1.073515|0.992310|0.858879|4/6|

Range CPU was worse5/6 pairs and set100 mean worse4/6. These short exploratory results are **not a non-regression proof**, and are not waved away as noise. Longer matched range/set controls are required before promotion; the wide pair ranges are retained in `control-validation.json`. Single-Set allocation savings are consistent6/6 (median8,949.52→7,739.28B/op), but do not override the latency signals.

## Allocation component diagnostics

Four separate heap-only diagnostics, each2,000 measured40warmup, direct client PID sampling, same88Bkeys, 30s/process and90s global budget. All4 validated, 09:36:44–09:37:17UTC; total execution including initial gate about36s. Both Go variants×both batch sizes. No CPU profiles. These are diagnostic executions, **not paired latency comparisons**. Heap alloc_space is sampled process-lifetime allocation, including fixture construction/warmup; it is not precisely measured-phase allocation. No profiler ran during either timed campaign.

Actual sampled `coalesceCommitMutations` cumulative allocation:
- Small base42.57MB→replay5.01MB.
- Large base159.71MB→replay4.01MB.
- Candidate path resolves to **only `coalescePlainSetMutations`→`slices.Clone`**, not RYW map rebuilding/materialization. This confirms the intended target was removed on these inputs, rather than inferring from a callsite.

Diagnostic measured bytes/tx base→replay:small107,548.056→80,848.896;large343,669.752→271,741.252. Do not mix these diagnostic values with the100-op timing-arm estimates: retained fixture sizes and GC regimes differ.

Remaining large-candidate profile components (739.54MB total sampled lifetime allocation):
- `conflictBufAlloc`:182.93MB cumulative24.74%:69.10MB at pool.Get (`transaction.go:2996`, ownMutationLocked caller),113.83MB at growing-array make (`:3014`, write-conflict caller). This is now **actual profile evidence** for both32KiB pool acquisition and64KiB growth, not just the earlier static cap proof.
- `captureCommit`:139.37MB18.85%, separate owning snapshot arena/vectors.
- Initial `rywCache.set`:130.66MB17.67%, all attributed to stateSet;103.10MB value-copy line349,27.56MB map/string line355.
- Fixture payload construction:216.81MB29.32%, outside measured operations; do not charge it to commit.

The next allocation target is safely retiring/reusing source arenas only after every alias is dead, or eliminating another redundant copy with an explicit ownership proof. **Never pool the old32KiB array early:** existing mutation/conflict refs still alias it during growth. This experiment did not change any ownership allocation path.

## Additional unpriced algorithmic risk — do not promote001 blindly

The candidate's unordered all-Set path stable-sorts the **full** mutation log. For large unsorted, duplicate-heavy logs, this can add sorting work where the prior map replay collapsed to few distinct keys in linear time before sorting. Current batch fixtures are ascending distinct keys, and the added1,000-case oracle tests semantics, not this performance tradeoff. This is a real new algorithmic risk. Before keeping the optimization, either price unordered/duplicate-heavy controls or narrow the specialization to already ordered snapshots and retain old replay otherwise. No002 candidate was built/measured in this report;001 remains immutable. Root was notified before handoff.

## Measurement limits and artifacts

Runner/analyzer unchanged. VM16logical AMD EPYC-Milan/KVM, no physicalhost-isolation claim; FDBCPUs2–7, clients8–15/GOMAXPROCS8, collector0–1, Docker/containerd inactive.2s cooldown+1s per-CPU/group gate before each run,1Hz telemetry,180s timed-process cap. No overlapping remote builds/tests/profiles/workloads. Runtime/binary pins checked.

Batch client/proc telemetry captured a process in29/36 sub-second runs; missing instantaneous samples do not imply zero resource use. All runs have internal measured getrusage/MemStats and host/FDB snapshots. CGo GoMemStats excludes the C++ heap; internal native counters unobserved.

All comparisons are adaptive six-block exploratory results. Pooled tails are descriptive; no confirmatoryCI, noninferiority, capacity or globalnativewin claim. Main-module regression tests, reviewers, known base correctness defects, longer flagged controls and the unordered-log risk remain acceptance gates. Artifacts preserve exact sources/patch/manifests, both new binaries plus native, unchanged harness/runner/analyzer, raw timed and diagnostic samples, profiles and pprof annotated sources. No failed/aborted measured run occurred; every attempted timing/diagnostic run is preserved.
