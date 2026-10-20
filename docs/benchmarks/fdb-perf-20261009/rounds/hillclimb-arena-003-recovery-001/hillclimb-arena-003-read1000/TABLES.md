# hillclimb-arena-003-read1000

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|599.576|888.213|405.038|
|read1000|old|429.124|720.215|444.842|
|read1000|new|439.068|785.750|474.844|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.031315|1.089243|1.110292|
|read1000|new/cgo|0.738830|0.856017|1.248844|
|read1000|old/cgo|0.716396|0.785882|1.124789|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
