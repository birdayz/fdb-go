# hillclimb-arena-003-read-heap

PROFILED DIAGNOSTIC OBSERVATIONS, not unprofiled performance results. CPU/heap profiling perturbs these runs. Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|575.760|877.538|374.381|
|read1000|old|434.361|652.761|450.724|
|read1000|new|443.539|689.465|469.588|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.045325|1.016391|1.062074|
|read1000|new/cgo|0.828766|0.817565|1.340654|
|read1000|old/cgo|0.792831|0.804381|1.262298|

Profiled attribution diagnostic only, not an unprofiled rerun/nonregression proof. CPU profiles cover measurement; heap profiles are sampled lifetime (include setup and warmup), native C++ heap unobserved. Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
