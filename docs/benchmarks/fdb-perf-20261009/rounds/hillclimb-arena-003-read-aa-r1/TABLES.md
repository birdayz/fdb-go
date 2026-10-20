# hillclimb-arena-003-read-aa-r1

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published affef937, new=affef937+arena003, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|534.313|826.345|367.683|
|read1000|old|435.727|636.265|450.667|
|read1000|old-aa|440.908|644.066|470.374|
|read1000|new|433.333|708.560|459.734|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|1.007650|1.112447|1.031251|
|read1000|old-aa/old|1.045012|1.030772|1.051955|
|read1000|new/cgo|0.809053|0.849121|1.313323|
|read1000|old/cgo|0.802911|0.763292|1.273524|
|read1000|old-aa/cgo|0.839051|0.786779|1.339689|

Six partially counterbalanced exploratory run blocks (NOT all24 orders): pairwise precedence3/3 and positions1or2 appearances; old-aa is the exact old binary; A/A does not prove zero patch cost; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
