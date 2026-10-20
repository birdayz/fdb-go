# R10: PR5 direct idle writer

Perf-lead requested two prespecified timed campaigns and separate old/new perf-stat and strace attribution. PR1/PR2/PR3/PR4 are NOT stacked.

Old is the unchanged binary compiled from `9377c0863aef7ecdee250cdc323e40ebf69d5e46`, SHA256 `4d236cbfc53cea32211b5042a86e0e14dc006f8896a1204b27aee3921fb17e71`. R8's exact Git equality evidence for `pkg/fdbgo tools` and separately `go.mod go.sum MODULE.bazel .bazelrc` versus master `590145e3161c8dc5cda26b62a95d578b3bdafd3b` is reused, with a fresh remote recursive comparison of the source subtrees and module files. Old keeps its genuine9377 compiled-source identity, not a relabeled master SHA.

New is built from the same full Git master590145 export, SHA256 `501a847a6fc4eab40bad4ca487076bfc6c46d02e2982ae7765c0c8143bb08dc5`, plus patch SHA256 `4a413f5d64d829b87e681bd9202a5bfa1fd48c1e74b5c3c68ec4ea88e3ef6063` (patch commit `582bc92e414ae5d12dc6edad8d0de29465e7c7dd`: SendFrame/SendFrameContext callers write directly when writeLoop is idle and no frame is queued). NEW binary SHA256 `f1eedbd71df888fbf6be8bb9226a18ec4daa4cc250a593057567e9f6b602d180`. Parent/new populations are6865/6866files; exactly transport/BUILD.bazel, transport/conn.go, transport/conn_direct_write_test.go, transport/conn_lifecycle_test.go and transport/write_context.go differ. Full file populations/content are checked before/after build and timings. Only NEW was built, VM-only, with unchanged frozenV6 harness SHA256 `3d2504ae0db14fad14f0384287377998231cf049310c8289d6e7245d3428e0d0`, Go1.26.9/CGO_ENABLED=0. Its15 standalone harness tests passed. Root/perf-lead own parent/client correctness, Bazel and final-review approval; benchmark checks do not establish those.

CGo is unchanged: binary SHA256 `cd762ea429647784378270613f404573ba598a5049f422e055a887f7746ecbdb`, API730/libfdb_c7.3.77 SHA256 `e4ad7fed7b7580bf5f7b38f0a4279d37e698f69d534182e5de0b8beeef6cd1b1`.

## Prespecified timed design

- read1000:5000measured/200warmup,workers1. Fresh ReadTransact/Get exact1000B value, no commit.
- batchset50:2500measured/200warmup,workers1. Fifty distinct changing100B Sets+commit/transaction.
- Each has six paired blocks covering all six old/new/CGo permutations, independent fixed seeds790106/790206, fresh equal68-byte r10r/r10b prefixes and its own180-second cap. Counts and seeds fixed before any execution; no adaptive repeats.
- Same frozen runner/analyzer, Go1.26.9, FDB7.3.77, GOMAXPROCS8/GOGC100/GOMEMLIMIT off, runnerCPUs0–1/server2–7/clients8–15;2s cooldown+1s quiescence gate and1Hz telemetry. No build/profiler/workload overlaps timed runs.
- Every raw measured/warmup row, final persisted state, success/attempt/counter/config/runtime identity and quiescence gate is validated. Native internal counters are unobserved, not zero.

Absolute values are medians of six run-level statistics. Ratios are geometric means of six within-block ratios, NOT ratios of medians. Nominal95% percentile intervals use10000whole-three-arm-block resamples,seed790707,common resampled indices across metrics/comparisons. CPU is measured-phase getrusage user+system per completed transaction; user/system reported separately. All per-block CPU ratios retained. Six exploratory blocks on one continuously running VM/server do not establish statistical independence, CI coverage, noninferiority, global-native or capacity claims; no multiplicity correction. GoMemStats/GC excludes C++heap.

## Separate attribution

After both timed campaigns: exactly one old then one new read5000/200 process under perf stat(context-switches,task-clock), then exactly one old then one new under strace -f -c(futex/write and full syscall table). Fresh68-byte r10p/r10d prefixes, same affinities/options/quiescence gates; each diagnostic process bounded180s. The instruments never overlap each other or timed campaigns. No CGo diagnostic control requested.

All diagnostic counters are process-lifetime INCLUDING startup, executable hashing, setup, warmup, measurement, persisted validation and shutdown. Dividing by5000 is whole-process counts per measured transaction, NOT measured-phase counters. Failed syscall counts and perf running percentages are retained. perf task-clock is NOT interchangeable with the measured-phase getrusage metric; instrumented timing does not enter campaign conclusions. One old/new pair per instrument is attribution, not a counterbalanced or independently replicated performance result.

## Newly identified CPU limitation

FrozenV6 measures windowCPU only; it does not drain Go finalizers and C++ network-thread cleanup around measurement. Warmup cleanup can enter the window and late measured-transaction cleanup can fall after it. Cross-client CPU ratios therefore are provisional window observations, not complete-lifecycle/native-capacity claims. Each workload includes CPU-SCOPE.md, pair-adjacent GoGC counts and window-cpu-with-gc.csv. The requested separate lifecycle-accounting harness/campaign will quantify the difference; no frozenV6 code is changed here. Write-only Go also lacks the C++ causal-read-risky GRV flag in this exact baseline; that pre-existing difference is disclosed in batch results, not quietly fixed.
