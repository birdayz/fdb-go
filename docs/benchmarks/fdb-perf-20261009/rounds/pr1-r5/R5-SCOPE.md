# R5 — PR1 keyrange memcmp comparison

Perf-lead R5/root786 authorized a source-identifiable performance experiment. Parent-module correctness/Bazel/final reviews are separate root-owned gates; this measurement agent did not run them or change an active worktree.

Old: exact Git commit `9377c0863aef7ecdee250cdc323e40ebf69d5e46`, full Git archive SHA256 `4066d418c44eab9324de9d474c722b0d6026469391eacb0c336e595af2418f36`. New: that source plus `pr1-keyrange-memeq.patch`, SHA256 `33f1ed2062a8f412f0ee291af16379f41e32aa7edee5b4274d1db1f7fc7b96ef` (patch commit dc069960e74a3613bbd49c6bb49391e6e91c4321). Exactly three files differ: runtime keyrangeref_custom.go, its new regression/fuzz test, and BUILD.bazel. Frozen V6 harness was substituted unchanged in both new scratch exports.

VM-only Go1.26.9 standalone builds passed15 harness tests per arm. These do not run or approve the added parent tests. Full assembled source populations/hashes (6863old/6864new files) match before/after builds and measurement. Binaries:
- OLD, retained for R6: `/opt/fdbperf/bin/candidate-pr1-r5-old`, SHA256 `4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71`.
- NEW: `/opt/fdbperf/bin/candidate-pr1-r5-new`, SHA256 `8a55ef604332cdede715a86036db0a7ccc94724b61779f0de0070a8f7b90a12c`.
- Existing CGo: SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`.

## Design / result

Batchset50 (50×100B Sets+Commit),2500 measured transactions+200warmup/run,workers1,all six old/new/CGo orders;18runs/45000measured transactions.2500was prespecified before execution: R3's12runs at3000took122s, projecting183s for18runs, above180cap. There was no failed/favorable-result rerun. Original immutable runner and analyzer were used. Timing lasted160s,21:43:52–21:46:32 UTC. All18runs passed raw/persistence/counter/quiescence/source/binary checks; no build/profiler overlap.

`pr1-r5-batchset50/R5-TABLES.md` gives absolute median-run mean/p99/CPU/user/system, paired geometric ratios, all per-block ratios and per-run GC/allocation data. New/old paired ratios are mean1.039208,p990.987230,CPU0.997384,user0.973697,system1.027339. Total CPU is essentially flat (-0.26%,lower3/6), with user CPU -2.63%,system +2.73%,and mean latency adverse +3.92%. This is not demonstrated whole-process CPU improvement or noninferiority. GC counts are48old/48new over15000transactions/arm,0.0032cycles/tx; CGo Go-GC0 excludes native memory management.

Native comparisons are scoped to this campaign: new/CGo paired mean0.929302,p990.837800,CPU0.997856;old/CGo0.894241/0.848637/1.000473. Do not substitute ratio-of-medians or combine old baselines from other source/time campaigns. Six blocks are exploratory, not global-native/capacity/noninferiority evidence. Native internal counters remain unobserved and Go MemStats exclude C++ heap.

This archive contains full old Git export, patch, frozen harness, full source manifests/build identities, all three binaries, scripts/plans and complete raw data. Rebuild by extracting the Git export, replacing tools/clientbench with the included harness, then optionally applying the patch; build-r5.sh records the exact commands and standalone scope.
