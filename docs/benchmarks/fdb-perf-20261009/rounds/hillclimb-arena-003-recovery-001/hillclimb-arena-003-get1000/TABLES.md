# hillclimb-arena-003-get1000

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|get1000|cgo|669.578|1042.362|502.485|
|get1000|old|466.397|865.399|523.019|
|get1000|new|459.233|732.970|514.730|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|get1000|new/old|1.003656|0.902840|1.009626|
|get1000|new/cgo|0.700186|0.644083|1.071236|
|get1000|old/cgo|0.697635|0.713396|1.061023|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
