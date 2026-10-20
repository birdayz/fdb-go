# hillclimb-arena-003-read-cpu

PROFILED DIAGNOSTIC OBSERVATIONS, not unprofiled performance results. CPU/heap profiling perturbs these runs. Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|563.639|858.543|388.163|
|read1000|old|443.356|688.315|449.539|
|read1000|new|442.294|653.051|474.007|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.005298|0.979884|1.026848|
|read1000|new/cgo|0.778505|0.767609|1.248272|
|read1000|old/cgo|0.774401|0.783367|1.215635|

Profiled attribution diagnostic only, not an unprofiled rerun/nonregression proof. CPU profiles cover measurement; heap profiles are sampled lifetime (include setup and warmup), native C++ heap unobserved. Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
