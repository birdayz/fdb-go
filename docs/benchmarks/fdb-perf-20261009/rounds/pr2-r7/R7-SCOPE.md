# R7: PR2 large-value p99 follow-up

Authorized by perf-lead citing root #846 after the R6 large-value new/old p99 point estimate +4.53%. Exactly two additional campaigns, not repeat-until-significant. Both use the actual R6 binaries without rebuilding:

- old: commit `9377c0863aef7ecdee250cdc323e40ebf69d5e46`, binary SHA256 `4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71`.
- new: old + PR2 patch SHA256 `b7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18`, binary SHA256 `276282e9bdd6aa9d5796afd785c57c0ee77a520dd203328f04f42e43ed1b91bf`. PR1 is NOT stacked.
- CGo: immutable frozen V6 wrapper, binary SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`; libfdb_c 7.3.77 SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.

Each campaign uses batchset50x1000, 2,000 measured + 200 warmup transactions/run, one worker, six three-arm blocks covering all six orders. Runner shuffle seeds 787106 and 787206 were frozen before either campaign began. Fresh 68-byte arm prefixes use r7a1/r7b1; all 50 batch keys remain 88 bytes. Each operation is 50 distinct 1,000-byte Sets + commit. Same fixtures, retry/timeout settings, quiescence gates, telemetry, GOMAXPROCS=8 and guest affinities (runner 0-1/server 2-7/clients 8-15) as R6. Each timed campaign has its own 180-second wall cap, with no build/profiler/workload overlap. No new correctness tests or client builds were performed for R7; raw/persistence validation is not candidate semantic approval.

The existing runner and analyzer are unchanged. Existing validate-r6.py is reused for the identical design; its legacy filenames/headings do not change the campaign identities in plan.json. Original R6 artifacts remain unchanged. Whole old/new source populations and contents are compared with the R6 manifests before/after each campaign. Binaries, runtime/configuration, harness archive, runner/analyzer and source/patch identities are pinned and rechecked.

## Analysis frozen in the two plans

All 18 blocks (R6 large-value + both R7 campaigns) are retained. Primary metrics are run-level nearest-rank p99, p99.9, p90, p50 and measured-phase process getrusage CPU/transaction; also report mean and user/system components for context. Estimate geometric means of within-block new/old and new/CGo ratios (old/CGo supplied as context), not ratios of median statistics or pooled quantiles. Nominal 95% percentile intervals use 10,000 resamples of whole three-arm blocks, 18 draws/replicate, fixed seed 787707 and common resampled indices across comparisons/metrics. No operation-level resampling. Absolute values are medians of 18 run-level statistics. Campaign-specific point estimates and every per-block ratio are included.

Tail clustering uses exactly the 20 largest measured latencies/run (top 1%; descending latency, ascending operation index for ties). For each selected observation, ask whether another selected index is within ±20 samples inclusive, excluding itself, with no wraparound or warmup inclusion. Report all 54 per-run fractions and per-arm aggregation; preserve exact selected indices and values. A uniform-random 20-position subset expectation is descriptive context only, not a significance test or a causal model.

## Limits

R6 prompted this follow-up: it is exploratory, not a fresh confirmatory test. Independent shuffle seeds do not establish statistical independence between blocks or campaigns on the same continuously running VM/server, nor physical-host isolation; bootstrap coverage is nominal. No multiplicity correction or noninferiority margin. p99.9 of 2,000 observations is nearest rank 1,998 and particularly unstable (only two observations exceed it). A CI crossing 1 is not equivalence or proof of no regression. Top-tail index clustering does not establish periodicity, shared cause, or harmlessness of any tail change. Native client counters remain unobserved; Go MemStats excludes the C++ heap. CPU is measured-phase getrusage, not whole-process GNU time or perf task-clock. No global/native-capacity claim is supported.
