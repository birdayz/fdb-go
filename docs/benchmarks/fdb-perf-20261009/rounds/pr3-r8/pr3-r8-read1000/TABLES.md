# pr3-r8-read1000

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+PR3patchf02c03072178a7a1bfd4703e58a7401afdfcba1ccee3273ed5f721306d0c2128, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|read1000|cgo|587.956|869.499|399.705|
|read1000|old|445.799|620.815|471.538|
|read1000|new|425.796|588.961|445.726|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|read1000|new/old|0.970600|0.936668|0.961294|
|read1000|new/cgo|0.725310|0.685535|1.143040|
|read1000|old/cgo|0.747280|0.731886|1.189064|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
