# pr3-r8-batchset50

Absolute values are medians of six run-level statistics; ratios are geometric means of six paired within-block ratios, NOT ratios of these medians. µs per transaction operation. old=published9377c0863aef7ecdee250cdc323e40ebf69d5e46, new=master590145e3161c8dc5cda26b62a95d578b3bdafd3b+PR3patchf02c03072178a7a1bfd4703e58a7401afdfcba1ccee3273ed5f721306d0c2128, cgo=immutable native7.3.77.

|Workload|Arm|Mean µs/op|p99 µs/op|CPU µs/op|
|---|---|---:|---:|---:|
|batchset50|cgo|2059.369|3860.198|831.965|
|batchset50|old|2095.948|3730.364|889.866|
|batchset50|new|1997.083|3483.171|879.212|

|Workload|Ratio|Mean|p99|CPU/op|
|---|---|---:|---:|---:|
|batchset50|new/old|0.942714|0.930274|0.981686|
|batchset50|new/cgo|0.944841|0.917028|1.033915|
|batchset50|old/cgo|1.002257|0.985761|1.053204|

Six paired exploratory run blocks; geometric mean within-block ratios, no confirmatory/noninferiority/global-speed/capacity claim. Native internal counters unobserved, not zero. Go MemStats excludes C++ heap, so no Go/CGo allocation ratios. Pooled tails descriptive. Lifetime RSS includes setup, not measured-phase peak. Source/binary identity verified per run; rawchecks do not approve candidate semantics.
