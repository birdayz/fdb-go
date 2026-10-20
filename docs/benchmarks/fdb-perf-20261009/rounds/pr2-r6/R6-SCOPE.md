# R6 — PR2 commit-capture borrowing

Perf-lead requested this source-identifiable comparison, not parent correctness approval. Old is exact `9377c0863aef7ecdee250cdc323e40ebf69d5e46`, reusing R5's OLD binary unchanged. New is old+patch SHA256 `b7736a4a40a159b1c1e75904c28292ee7be0e0f763e89a5a8d9c5d756c876a18` (commit27cb32c5ff156790a8cf19c3aeaf1621e8754040). PR1 is NOT stacked. Exactly five files differ (three client runtime files, regression test, BUILD); source manifests/populations6863old/6864new were verified before/after build and each campaign.

Binaries:
- Old `/opt/fdbperf/bin/candidate-pr1-r5-old`, SHA256 `4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71`.
- New `/opt/fdbperf/bin/candidate-pr2-r6-new`, SHA256 `276282e9bdd6aa9d5796afd785c57c0ee77a520dd203328f04f42e43ed1b91bf`.
- Existing CGo `candidate-v6-cgo`, SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`.

Only NEW was built, remotely with Go1.26.9/frozen V6 harness;15 standalone harness tests passed. These do not execute the new parent regressions or establish borrowing/concurrency correctness. Root owns parent just test/full and final review. No active/ready worktree was edited, no local build ran.

## Design / verification

Two serial unprofiled campaigns, each all six old/new/CGo orders, one worker,200warmup/run. Small50×100B uses2500measured/run; large50×1000B uses2000measured/run, explicitly approved/prespecified before execution. No failed/favorable-result repeats. Each uses original immutable runner,180second cap, fixed affinity/GOMAXPROCS8, equal unique68Bprefixes, unchanged timeout/retry settings.

Small18runs:21:50:54–21:53:32 UTC,158seconds. Large18runs:21:53:37–21:56:18 UTC,161seconds. All36runs passed raw/persistence/counter/quiescence/source/binary checks. No build/profiler overlap. Source/runtimes/prior binaries remain unchanged.

## Findings

Full absolute mean/p99/CPU/user/system/B/tx/allocs/tx/GC tables and per-block ratios are in each campaign's `R6-TABLES.md`, `r6-detailed.json`, `r6-per-run.csv`, `r6-per-block.csv`. Absolute values are medians of six run statistics; ratios are paired within-block geometric means, NOT ratios of medians.

|Workload|New/old mean|p99|CPU|User|System|CPU pairs lower|Go B/tx ratio|Go allocs/tx ratio|
|---|---:|---:|---:|---:|---:|---:|---:|---:|
|50×100B|1.003520|1.000807|1.008998|0.990829|1.032069|3/6|0.769153|0.996377|
|50×1000B|0.973700|1.045277|0.920064|0.883443|0.953617|6/6|0.757578|0.996414|

Allocation reduction is observed in all six blocks of each workload: small B/tx median80436.670→61865.930;large271083.508→205354.912. Small GoGC cycles48→37 over15000transactions/arm;large18→12 over12000transactions/arm. This does not translate into a demonstrated small-write CPU improvement: paired CPU is+0.90%. Large CPU falls7.99% in all six pairs, but paired p99 rises4.53%; retain that adverse tail observation.

Paired new/CGo mean/p99/CPU: small0.952870/0.936712/1.031448;large0.919068/1.004037/0.951449. Paired old/CGo:small0.949528/0.935957/1.022250;large0.943893/0.960546/1.034111. New large CPU is below native in4/6pairs, not all; small remains above native in5/6. No global native win/noninferiority/capacity claim. Six blocks are exploratory and native internal counters remain unobserved; GoMemStats/GC exclude C++heap, so no Go/CGo allocation ratios are valid.

Archive contains all36rawruns, allthree binaries, fulloldGitexport (SHA4066d418c44eab9324de9d474c722b0d6026469391eacb0c336e595af2418f36),frozenV6harness,patch,source/manifests,build and execution scripts. Existing OLD remains available for subsequent authorized experiments.
