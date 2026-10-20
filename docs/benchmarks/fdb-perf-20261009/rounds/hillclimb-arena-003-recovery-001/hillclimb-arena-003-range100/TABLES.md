# hillclimb-arena-003-range100

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|range100|cgo|856.176|1668.892|641.707|
|range100|old|615.947|1288.179|736.490|
|range100|new|617.080|1303.574|739.286|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|range100|new/old|0.996801|1.046184|0.992613|
|range100|new/cgo|0.758956|0.833696|1.209222|
|range100|old/cgo|0.761392|0.796893|1.218221|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
